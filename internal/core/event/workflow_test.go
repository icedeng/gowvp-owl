package event_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/event/store/eventdb"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

func workflowDB(t *testing.T) (*gorm.DB, event.Core, *event.Workflow) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared&_pragma=foreign_keys(1)", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	sql, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sql.SetMaxOpenConns(1)
	t.Cleanup(func() { sql.Close() })
	store := eventdb.NewDB(db).AutoMigrate(true)
	w := event.NewWorkflow(db)
	core := event.NewCore(store, nil).WithWorkflow(w)
	return db, core, w
}
func makeEvent(t *testing.T, c event.Core, cid string, at time.Time) *event.Event {
	t.Helper()
	key := fmt.Sprintf("%s-%d", cid, time.Now().UnixNano())
	e, err := c.CreateEventAndNotify(t.Context(), &event.AddEventInput{DID: "device", CID: cid, Model: "GB28181", SourceKey: &key, StartedAt: orm.Time{Time: at}, EndedAt: orm.Time{Time: at.Add(time.Second)}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func TestEventWorkflowAuditAndAtomicBatchConflict(t *testing.T) {
	db, c, w := workflowDB(t)
	a := makeEvent(t, c, "channel", time.Now())
	b := makeEvent(t, c, "channel", time.Now())
	if a.Status != "pending" || a.Revision != 1 {
		t.Fatal(a)
	}
	err := w.Process(t.Context(), event.ProcessInput{Items: []event.ProcessTarget{{ID: b.ID, Revision: 1}}, Status: "processing"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	err = w.Process(t.Context(), event.ProcessInput{Items: []event.ProcessTarget{{ID: a.ID, Revision: 1}, {ID: b.ID, Revision: 1}}, Status: "resolved", Note: "核实完成"}, "bob")
	if err == nil {
		t.Fatal("stale batch accepted")
	}
	persisted, err := c.GetEvent(t.Context(), a.ID)
	if err != nil || persisted.Status != "pending" || persisted.Revision != 1 {
		t.Fatal("batch partially committed", persisted, err)
	}
	var count int64
	db.Model(&event.Action{}).Where("event_id = ?", a.ID).Count(&count)
	if count != 0 {
		t.Fatal("audit partially committed")
	}
	if err = w.Process(t.Context(), event.ProcessInput{Items: []event.ProcessTarget{{ID: a.ID, Revision: 1}}, Status: "ignored"}, "bob"); err == nil {
		t.Fatal("empty disposition accepted")
	}
	if err = w.Process(t.Context(), event.ProcessInput{Items: []event.ProcessTarget{{ID: a.ID, Revision: 1}}, Status: "resolved", Note: "现场核实，处置完成"}, "bob"); err != nil {
		t.Fatal(err)
	}
	if err = w.Process(t.Context(), event.ProcessInput{Items: []event.ProcessTarget{{ID: a.ID, Revision: 2}}, Status: "processing"}, "bob"); err == nil {
		t.Fatal("closed event silently reopened")
	}
	if err = w.Process(t.Context(), event.ProcessInput{Items: []event.ProcessTarget{{ID: a.ID, Revision: 2}}, Status: "pending", Note: "复核"}, "alice"); err != nil {
		t.Fatal(err)
	}
	if err = w.Process(t.Context(), event.ProcessInput{Items: []event.ProcessTarget{{ID: b.ID, Revision: 2}}, Status: "processing", Note: "已联系值班人员核实"}, "alice"); err != nil {
		t.Fatal(err)
	}
	notes, err := w.Actions(t.Context(), b.ID)
	if err != nil || len(notes) != 2 || notes[0].Note != "已联系值班人员核实" {
		t.Fatal("same-state note lost", notes, err)
	}
	history, err := w.Actions(t.Context(), a.ID)
	if err != nil || len(history) != 2 || history[0].Actor != "alice" || history[1].Note != "现场核实，处置完成" {
		t.Fatal(history, err)
	}
}
func TestEventEvidencePolicyCooldownAndIdempotence(t *testing.T) {
	db, c, w := workflowDB(t)
	old := makeEvent(t, c, "one", time.Now())
	p, err := w.Policy(t.Context(), "device")
	if err != nil || p.Snapshot || p.Record {
		t.Fatal(p, err)
	}
	p.Snapshot, p.Record = true, true
	p, err = w.SavePolicy(t.Context(), *p, "admin")
	if err != nil {
		t.Fatal(err)
	}
	// 已有事件不追溯创建任务。
	var n int64
	db.Model(&event.EvidenceTask{}).Where("event_id = ?", old.ID).Count(&n)
	if n != 0 {
		t.Fatal("historical event scheduled")
	}
	a := makeEvent(t, c, "one", time.Now())
	b := makeEvent(t, c, "one", time.Now())
	other := makeEvent(t, c, "two", time.Now())
	stale := makeEvent(t, c, "three", time.Now().Add(-time.Hour))
	for _, test := range []struct {
		id    int64
		state string
	}{{a.ID, "queued"}, {b.ID, "skipped"}, {other.ID, "queued"}, {stale.ID, "skipped"}} {
		var task event.EvidenceTask
		if err = db.First(&task, test.id).Error; err != nil || task.State != test.state || task.PolicyRevision != p.Revision {
			t.Fatal(task, err)
		}
	}
	_, err = c.CreateEventAndNotify(t.Context(), &event.AddEventInput{DID: a.DID, CID: a.CID, Model: a.Model, SourceKey: a.SourceKey, StartedAt: a.StartedAt})
	if err != nil {
		t.Fatal(err)
	}
	db.Model(&event.EvidenceTask{}).Where("event_id = ?", a.ID).Count(&n)
	if n != 1 {
		t.Fatal("duplicate evidence", n)
	}
	// 策略修改带版本检查。
	stalePolicy := *p
	p.Record = false
	if _, err = w.SavePolicy(t.Context(), *p, "new-user"); err != nil {
		t.Fatal(err)
	}
	if _, err = w.SavePolicy(t.Context(), stalePolicy, "stale-user"); err == nil {
		t.Fatal("stale policy overwrote configuration")
	}
}
func TestEventEvidenceWorkersPersistIndependentResultsAndStop(t *testing.T) {
	db, c, w := workflowDB(t)
	_, err := w.SavePolicy(t.Context(), event.Policy{DID: "device", Snapshot: true, Record: true, ClipSeconds: 5, CooldownSeconds: 10}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	e := makeEvent(t, c, "channel", time.Now())
	var calls atomic.Int32
	w.SetEvidenceExecutor(func(ctx context.Context, e *event.Event, kind string, _ int) (string, error) {
		calls.Add(1)
		if kind == "video" {
			return "", errors.New("test camera unavailable")
		}
		return "snapshots/test.jpg", nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); w.RunEvidenceWorkers(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var task event.EvidenceTask
		if err = db.First(&task, e.ID).Error; err != nil {
			t.Fatal(err)
		}
		if task.State == "failed" {
			if task.SnapshotState != "succeeded" || task.VideoState != "failed" {
				t.Fatal(task)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	stored, err := c.GetEvent(t.Context(), e.ID)
	if err != nil || stored.ImagePath != "snapshots/test.jpg" || stored.Status != "pending" {
		t.Fatal(stored, err)
	}
	if calls.Load() != 2 {
		t.Fatal("repeated evidence", calls.Load())
	}
}
func TestEventEvidenceEnqueueFailureRollsBackEvent(t *testing.T) {
	db, c, w := workflowDB(t)
	_, err := w.SavePolicy(t.Context(), event.Policy{DID: "device", Snapshot: true, ClipSeconds: 15, CooldownSeconds: 60}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Migrator().DropTable(&event.EvidenceTask{}); err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateEvent(t.Context(), &event.AddEventInput{DID: "device", CID: "one", Model: "GB28181", StartedAt: orm.Now()})
	if err == nil {
		t.Fatal("enqueue failure accepted")
	}
	var count int64
	db.Model(&event.Event{}).Count(&count)
	if count != 0 {
		t.Fatal("half committed event")
	}
}
func TestEventWorkflowNameSearchPaginationAndUpgrade(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	// 模拟旧版数据，升级后默认待处理，附件和来源键不丢失。
	if err = db.Exec("CREATE TABLE events (id integer PRIMARY KEY, did text NOT NULL DEFAULT '', cid text NOT NULL DEFAULT '', started_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP, ended_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP, label text NOT NULL DEFAULT '', score real NOT NULL DEFAULT 0, zones text NOT NULL DEFAULT '', image_path text NOT NULL DEFAULT '', model text NOT NULL DEFAULT '', source_key text, created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP)").Error; err != nil {
		t.Fatal(err)
	}
	db.Exec("INSERT INTO events(id,did,cid,image_path,source_key) VALUES (1,'device','one','old.jpg','old-key')")
	store := eventdb.NewDB(db).AutoMigrate(true)
	c := event.NewCore(store, nil)
	w := event.NewWorkflow(db)
	old, err := c.GetEvent(t.Context(), 1)
	if err != nil || old.Status != "pending" || old.Revision != 1 || old.OccurrenceCount != 1 || old.AggregationKey != nil || old.ImagePath != "old.jpg" || old.SourceKey == nil || *old.SourceKey != "old-key" {
		t.Fatal(old, err)
	}
	db.Exec("CREATE TABLE devices (id text PRIMARY KEY, name text, device_id text, ext text, type text)")
	db.Exec("CREATE TABLE channels (id text PRIMARY KEY,did text,name text,channel_id text)")
	db.Exec("INSERT INTO devices VALUES ('device','大门摄像机','34020000001320000101','{}','GB28181')")
	db.Exec("INSERT INTO channels VALUES ('one','device','大门入口','34020000001320000001')")
	makeEvent(t, c, "one", time.Now())
	for page := 1; page <= 2; page++ {
		rows, total, err := c.ListEvents(t.Context(), &event.FindEventInput{Key: "大门摄像机", Status: "pending", PagerFilter: web.PagerFilter{Page: page, Size: 1}})
		if err != nil || total != 2 || len(rows) != 1 {
			t.Fatal(rows, total, err)
		}
		if err = w.FillNames(t.Context(), rows); err != nil || rows[0].DeviceName != "大门摄像机" || rows[0].ChannelName != "大门入口" {
			t.Fatal(rows, err)
		}
	}
	rows, total, err := c.ListEvents(t.Context(), &event.FindEventInput{Key: "%", PagerFilter: web.PagerFilter{Page: 1, Size: 20}})
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatal("wildcard escaped incorrectly", rows, total, err)
	}
}

func TestEventDeleteRetainsActiveEvidenceAndCleansAttachments(t *testing.T) {
	t.Chdir(t.TempDir())
	db, c, w := workflowDB(t)
	_, err := w.SavePolicy(t.Context(), event.Policy{DID: "device", Snapshot: true, ClipSeconds: 15, CooldownSeconds: 60}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	e := makeEvent(t, c, "one", time.Now())
	if _, err = c.DeleteEvent(t.Context(), e.ID); err == nil {
		t.Fatal("active evidence was deleted")
	}
	if err = db.Model(&event.EvidenceTask{}).Where("event_id = ?", e.ID).Update("state", "failed").Error; err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll("configs/events/clips", 0750); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile("configs/events/clips/test.mp4", []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	db.Model(e).Update("video_path", "clips/test.mp4")
	if err = w.Process(t.Context(), event.ProcessInput{Items: []event.ProcessTarget{{ID: e.ID, Revision: 1}}, Status: "resolved", Note: "确认"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err = c.DeleteEvent(t.Context(), e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat("configs/events/clips/test.mp4"); !os.IsNotExist(err) {
		t.Fatal("video attachment retained", err)
	}
	for _, model := range []any{&event.Action{}, &event.EvidenceTask{}} {
		var count int64
		db.Model(model).Count(&count)
		if count != 0 {
			t.Fatal("orphan metadata", count)
		}
	}
}

func TestEventEvidenceRestartDoesNotReplayInterruptedOrStaleJobs(t *testing.T) {
	db, c, w := workflowDB(t)
	_, err := w.SavePolicy(t.Context(), event.Policy{DID: "device", Snapshot: true, Record: true, ClipSeconds: 5, CooldownSeconds: 10}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	interrupted := makeEvent(t, c, "one", time.Now())
	db.Model(&event.EvidenceTask{}).Where("event_id = ?", interrupted.ID).Updates(map[string]any{"state": "working", "snapshot_state": "working", "video_state": "working"})
	stale := makeEvent(t, c, "two", time.Now())
	db.Model(stale).Update("started_at", orm.Time{Time: time.Now().Add(-time.Hour)})
	var calls atomic.Int32
	w.SetEvidenceExecutor(func(context.Context, *event.Event, string, int) (string, error) { calls.Add(1); return "bad.jpg", nil })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); w.RunEvidenceWorkers(ctx) }()
	deadline := time.Now().Add(4 * time.Second)
	for {
		var task event.EvidenceTask
		db.First(&task, stale.ID)
		if task.State == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stale task not settled")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	var task event.EvidenceTask
	db.First(&task, interrupted.ID)
	if task.State != "failed" || task.SnapshotState != "failed" || task.VideoState != "failed" || calls.Load() != 0 {
		t.Fatal("interrupted/stale work replayed", task, calls.Load())
	}
}
