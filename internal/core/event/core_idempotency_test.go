package event_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/event/store/eventdb"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

type countingDispatcher struct {
	calls atomic.Int32
}

func (d *countingDispatcher) Dispatch(context.Context, *event.Event) {
	d.calls.Add(1)
}

func TestCreateEventAndNotifyDeduplicatesExternalSourceKey(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	store := eventdb.NewDB(db).AutoMigrate(true)
	dispatcher := &countingDispatcher{}
	core := event.NewCore(store, dispatcher)
	sourceKey := fmt.Sprintf("gb-alarm-delivery-%d", time.Now().UnixNano())
	input := &event.AddEventInput{
		DID: "device-1", CID: "channel-1", Label: "gb_alarm", Model: "GB28181",
		StartedAt: orm.Time{Time: time.Now()}, EndedAt: orm.Time{Time: time.Now().Add(time.Second)},
		SourceKey: &sourceKey,
	}

	first, err := core.CreateEventAndNotify(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := core.CreateEventAndNotify(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == 0 || second.ID != first.ID {
		t.Fatalf("idempotent event IDs = %d, %d", first.ID, second.ID)
	}
	if dispatcher.calls.Load() != 1 {
		t.Fatalf("event notifications = %d, want 1", dispatcher.calls.Load())
	}
	var count int64
	if err := db.Model(new(event.Event)).Where("source_key = ?", sourceKey).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("events with source key = %d, want 1", count)
	}
}
