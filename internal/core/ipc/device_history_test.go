package ipc

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

var deviceHistoryTestDatabaseSequence atomic.Uint64

func newDeviceHistoryTestStore(t *testing.T, cfg DeviceHistoryConfig) *DeviceHistoryStore {
	t.Helper()
	dsn := fmt.Sprintf("file:%s-%d?mode=memory&cache=shared", t.Name(), deviceHistoryTestDatabaseSequence.Add(1))
	db, err := gorm.Open(sqlite.Open(dsn))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close device history test database: %v", err)
		}
	})
	return NewDeviceHistoryStore(db, cfg, true)
}

func TestDeviceHistoryRetainsMaxRecordsAndCalculatesInterval(t *testing.T) {
	store := newDeviceHistoryTestStore(t, DeviceHistoryConfig{MaxRecords: 2, MaxDays: 30})
	base := time.Now().Add(-time.Minute)
	for i := 0; i < 3; i++ {
		if err := store.Record(context.Background(), "34020000001110000001", DeviceHistoryHeartbeat, "udp://127.0.0.1:5060", "OK", base.Add(time.Duration(i)*10*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	items, total, err := store.List(context.Background(), "34020000001110000001", DeviceHistoryHeartbeat, &web.PagerFilter{Page: 1, Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("records = %d/%d, want 2/2", len(items), total)
	}
	if items[0].IntervalSeconds != 10 {
		t.Fatalf("interval = %d, want 10", items[0].IntervalSeconds)
	}
}

func TestDeviceHistoryRetainsMaxDays(t *testing.T) {
	store := newDeviceHistoryTestStore(t, DeviceHistoryConfig{MaxDays: 1})
	if err := store.Record(context.Background(), "34020000001110000002", DeviceHistoryRegister, "tcp://127.0.0.1:5060", "online", time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), "34020000001110000002", DeviceHistoryRegister, "tcp://127.0.0.1:5060", "online", time.Now()); err != nil {
		t.Fatal(err)
	}
	items, total, err := store.List(context.Background(), "34020000001110000002", DeviceHistoryRegister, &web.PagerFilter{Page: 1, Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("records = %d/%d, want 1/1", len(items), total)
	}
}
