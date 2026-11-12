package event_test

import (
	"github.com/gowvp/owl/internal/core/event"
	"testing"
	"time"
)

func TestHistoricalAlarmNoEvidenceAndReplayDoesNotDuplicateRealtime(t *testing.T) {
	db, c, w := workflowDB(t)
	if _, err := w.SavePolicy(t.Context(), event.Policy{DID: "device", Snapshot: true, Record: true, ClipSeconds: 15, CooldownSeconds: 60}, "admin"); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Truncate(time.Second)
	first, err := c.CreateAlarmAndNotify(t.Context(), alarmInput(at, "channel", "live", "first"), alarmIdentity())
	if err != nil {
		t.Fatal(err)
	}
	input := alarmInput(at.Add(time.Second), "channel", "live2", "second")
	second, err := c.CreateAlarmAndNotify(t.Context(), input, alarmIdentity())
	if err != nil || second.ID != first.ID {
		t.Fatal(second, err)
	}
	// 第二次实时报警曾被聚合；重连补传也应通过明细语义键去重。
	input.Historical = true
	input.SourceKey = nil
	replay, err := c.CreateAlarmAndNotify(t.Context(), input, alarmIdentity())
	if err != nil || replay.ID != first.ID || replay.OccurrenceCount != 2 {
		t.Fatal(replay, err)
	}
	history := alarmInput(at.Add(2*time.Second), "channel", "backlog", "historical")
	history.Historical = true
	received := time.Now()
	history.ReceivedAt = &received
	saved, err := c.CreateAlarmAndNotify(t.Context(), history, alarmIdentity())
	if err != nil || saved.ID == first.ID || !saved.Historical || saved.ReceivedAt == nil {
		t.Fatal(saved, err)
	}
	var n int64
	if err = db.Model(&event.EvidenceTask{}).Where("event_id = ?", saved.ID).Count(&n).Error; err != nil || n != 0 {
		t.Fatal("历史报警创建了现场取证任务", n, err)
	}
}
