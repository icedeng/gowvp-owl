package event_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/event/store/eventdb"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

func alarmInput(at time.Time, channel, delivery, payload string) *event.AddEventInput {
	return &event.AddEventInput{DID: "device", CID: channel, Model: "GB28181", Label: "gb_alarm_5", Score: 1,
		StartedAt: orm.Time{Time: at}, EndedAt: orm.Time{Time: at}, SourceKey: &delivery, Zones: payload}
}

func alarmIdentity() event.AlarmIdentity {
	return event.AlarmIdentity{DeviceCode: "34020000001320000001", ChannelCode: "34020000001320000002", Method: "5", Type: "2", Priority: "1"}
}

func TestAlarmDeduplicatesDualRoutesAndAggregatesAcrossRestart(t *testing.T) {
	db, _, w := workflowDB(t)
	if _, err := w.SavePolicy(t.Context(), event.Policy{DID: "device", Snapshot: true, ClipSeconds: 15, CooldownSeconds: 60}, "admin"); err != nil {
		t.Fatal(err)
	}
	notifier := &countingDispatcher{}
	newCore := func() event.Core { return event.NewCore(eventdb.NewDB(db), notifier).WithWorkflow(w) }
	core := newCore()
	at := time.Now().Truncate(time.Second)
	first, err := core.CreateAlarmAndNotify(t.Context(), alarmInput(at, "channel", "message-1", `{"source_method":"MESSAGE","sn":1}`), alarmIdentity())
	if err != nil {
		t.Fatal(err)
	}
	// 同一时间以不同时区、SN、协议与传输方式重复上报，跨重启仍只记一次。
	core = newCore()
	duplicate, err := core.CreateAlarmAndNotify(t.Context(), alarmInput(at.In(time.FixedZone("device", 8*3600)), "channel", "notify-9", `{"source_method":"NOTIFY","sn":9,"protocol_version":"3.0"}`), alarmIdentity())
	if err != nil || duplicate.ID != first.ID || duplicate.OccurrenceCount != 1 {
		t.Fatal("双路报警未去重", duplicate, err)
	}
	for _, offset := range []time.Duration{2 * time.Second, time.Second, 12 * time.Second} {
		e, err := core.CreateAlarmAndNotify(t.Context(), alarmInput(at.Add(offset), "channel", fmt.Sprint(offset), fmt.Sprintf(`{"offset":%d}`, offset)), alarmIdentity())
		if err != nil || e.ID != first.ID {
			t.Fatal("连续或乱序报警未聚合", e, err)
		}
	}
	saved, err := core.GetEvent(t.Context(), first.ID)
	if err != nil || saved.OccurrenceCount != 4 || !saved.StartedAt.Equal(at) || !saved.EndedAt.Equal(at.Add(12*time.Second)) || saved.Revision != 1 {
		t.Fatal("聚合次数、时间或人工版本错误", saved, err)
	}
	reports, total, err := w.AlarmReports(t.Context(), first.ID, 1, 2)
	if err != nil || total != 4 || len(reports) != 2 || reports[0].Payload != `{"offset":12000000000}` {
		t.Fatal("有效上报明细或分页错误", reports, total, err)
	}
	var tasks int64
	if err := db.Model(&event.EvidenceTask{}).Count(&tasks).Error; err != nil || tasks != 1 || notifier.calls.Load() != 1 {
		t.Fatal("聚合重复取证或通知", tasks, notifier.calls.Load(), err)
	}
	separate, err := core.CreateAlarmAndNotify(t.Context(), alarmInput(at.Add(23*time.Second), "channel", "after-quiet", "new"), alarmIdentity())
	if err != nil || separate.ID == first.ID || separate.OccurrenceCount != 1 {
		t.Fatal("静默超过10秒没有新建事件", separate, err)
	}
	// 已有取证任务时不允许删除；完成后删除必须同步清理上报明细。
	if _, err := core.DeleteEvent(t.Context(), first.ID); err == nil {
		t.Fatal("取证过程中允许删除聚合事件")
	}
	if err := db.Model(&event.EvidenceTask{}).Where("event_id = ?", first.ID).Update("state", "done").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := core.DeleteEvent(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, total, err := w.AlarmReports(t.Context(), first.ID, 1, 20); err != nil || total != 0 {
		t.Fatal("删除后残留上报明细", total, err)
	}
}

func TestAlarmSeparatesIdentityAndClosedEvents(t *testing.T) {
	for _, status := range []string{"resolved", "ignored"} {
		t.Run(status, func(t *testing.T) {
			_, core, w := workflowDB(t)
			at := time.Now().Truncate(time.Second)
			first, err := core.CreateAlarmAndNotify(t.Context(), alarmInput(at, "channel", "first", "first"), alarmIdentity())
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Process(t.Context(), event.ProcessInput{Items: []event.ProcessTarget{{ID: first.ID, Revision: 1}}, Status: "processing"}, "admin"); err != nil {
				t.Fatal(err)
			}
			ongoing, err := core.CreateAlarmAndNotify(t.Context(), alarmInput(at.Add(time.Second), "channel", "ongoing", "ongoing"), alarmIdentity())
			if err != nil || ongoing.ID != first.ID || ongoing.OccurrenceCount != 2 || ongoing.Revision != 2 || ongoing.Status != "processing" {
				t.Fatal("处理中聚合改变了人工处理版本", ongoing, err)
			}
			if err := w.Process(t.Context(), event.ProcessInput{Items: []event.ProcessTarget{{ID: first.ID, Revision: 2}}, Status: status, Note: "已核实"}, "admin"); err != nil {
				t.Fatal(err)
			}
			duplicate, err := core.CreateAlarmAndNotify(t.Context(), alarmInput(at, "channel", "same-notify", "duplicate"), alarmIdentity())
			if err != nil || duplicate.ID != first.ID || duplicate.Status != status {
				t.Fatal("重复上报重新打开已处理事件", duplicate, err)
			}
			seen := map[int64]bool{first.ID: true}
			for i, identity := range []event.AlarmIdentity{alarmIdentity(), alarmIdentity(), alarmIdentity(), alarmIdentity(), alarmIdentity()} {
				channel := "channel"
				switch i {
				case 1:
					channel = "another-channel"
				case 2:
					identity.Type = "6"
				case 3:
					identity.Description = "another rule"
				case 4:
					phase := 2
					identity.EventType = &phase
				}
				e, err := core.CreateAlarmAndNotify(t.Context(), alarmInput(at.Add(2*time.Second), channel, fmt.Sprint("new-", i), "next"), identity)
				if err != nil || seen[e.ID] || e.Status != "pending" || e.OccurrenceCount != 1 {
					t.Fatal("新报警被已结束事件或不同类型吞并", e, err)
				}
				seen[e.ID] = true
			}
		})
	}
}

func TestAlarmConcurrentRetriesAndTransactionRollback(t *testing.T) {
	db, core, _ := workflowDB(t)
	at := time.Now().Truncate(time.Second)
	var group sync.WaitGroup
	for i := range 20 {
		group.Go(func() {
			_, err := core.CreateAlarmAndNotify(t.Context(), alarmInput(at.Add(time.Duration(i%5)*time.Second), "channel", fmt.Sprint(i), "payload"), alarmIdentity())
			if err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	var events []event.Event
	if err := db.Find(&events).Error; err != nil || len(events) != 1 || events[0].OccurrenceCount != 5 {
		t.Fatal("并发聚合或重试计数错误", events, err)
	}
	if err := db.Callback().Create().Before("gorm:create").Register("reject-alarm-report", func(tx *gorm.DB) {
		if tx.Statement.Table == "event_alarm_reports" {
			tx.AddError(fmt.Errorf("report write failed"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Create().Remove("reject-alarm-report") })
	for _, offset := range []time.Duration{6 * time.Second, 30 * time.Second} {
		if _, err := core.CreateAlarmAndNotify(context.Background(), alarmInput(at.Add(offset), "channel", fmt.Sprint(offset), "rejected"), alarmIdentity()); err == nil {
			t.Fatal("明细写入失败仍成功")
		}
	}
	events = nil
	if err := db.Find(&events).Error; err != nil || len(events) != 1 || events[0].OccurrenceCount != 5 {
		t.Fatal("失败事务遗留聚合计数或新事件", events, err)
	}
}
