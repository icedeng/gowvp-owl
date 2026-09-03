package ipccache

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

func TestGBTaskStatePersistsAcrossCacheRestartAndCleansUp(t *testing.T) {
	db := openInternalCacheTestDatabase(t)
	base := ipcdb.NewDB(db).AutoMigrate(true)
	first := NewCache(base)
	now := time.Now()
	deviceID := "34020000001320000001"
	currentSession := "snapshot-session-persist-000000001"
	oldSession := "snapshot-session-expired-000000001"
	if err := first.SaveGBTaskState(t.Context(), "snapshot", deviceID, oldSession, []byte(`{"status":"old"}`), now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := first.SaveGBTaskState(t.Context(), "snapshot", deviceID, currentSession, []byte(`{"status":"pending"}`), now); err != nil {
		t.Fatal(err)
	}
	if err := first.SaveGBTaskState(t.Context(), "snapshot", deviceID, currentSession, []byte(`{"status":"accepted"}`), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	restarted := NewCache(ipcdb.NewDB(db))
	records, err := restarted.ListGBTaskStates(t.Context(), "snapshot", 10)
	if err != nil || len(records) != 2 {
		t.Fatalf("listed task states = %d, err=%v", len(records), err)
	}
	payload, ok, err := restarted.LoadGBTaskState(t.Context(), "snapshot", deviceID, currentSession)
	if err != nil || !ok || string(payload) != `{"status":"accepted"}` {
		t.Fatalf("restored task state = %s, %v, %v", payload, ok, err)
	}
	if err := restarted.CleanupGBTaskStates(t.Context(), "snapshot", now.Add(-7*24*time.Hour), 1024); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := restarted.LoadGBTaskState(t.Context(), "snapshot", deviceID, oldSession); err != nil || ok {
		t.Fatalf("expired task state survived cleanup: ok=%v err=%v", ok, err)
	}
}

func TestDeleteDeviceRemovesGBTaskStates(t *testing.T) {
	db := openInternalCacheTestDatabase(t)
	base := ipcdb.NewDB(db).AutoMigrate(true)
	cache := NewCache(base)
	device := &ipc.Device{ID: "GB_task_state", DeviceID: "34020000001320000001", Type: ipc.TypeGB28181}
	if err := base.Device().Create(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	const sessionID = "snapshot-session-delete-000000001"
	if err := cache.SaveGBTaskState(t.Context(), "snapshot", device.DeviceID, sessionID, []byte(`{"status":"pending"}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := cache.Device().Delete(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.LoadGBTaskState(t.Context(), "snapshot", device.DeviceID, sessionID); err != nil || ok {
		t.Fatalf("device task state survived deletion: ok=%v err=%v", ok, err)
	}
}

func TestGBTaskStateCapacityCleanupUsesCurrentOrdering(t *testing.T) {
	db := openInternalCacheTestDatabase(t)
	cache := NewCache(ipcdb.NewDB(db).AutoMigrate(true))
	const (
		kind       = "snapshot"
		deviceID   = "34020000001320000001"
		oldSession = "snapshot-capacity-old-000000001"
		newSession = "snapshot-capacity-new-000000001"
	)
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	if err := cache.SaveGBTaskState(t.Context(), kind, deviceID, oldSession, []byte(`{"status":"old"}`), base); err != nil {
		t.Fatal(err)
	}
	if err := cache.SaveGBTaskState(t.Context(), kind, deviceID, newSession, []byte(`{"status":"new"}`), base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	refreshedAt := base.Add(2 * time.Minute)
	var once sync.Once
	if err := db.Callback().Delete().Before("gorm:delete").Register("test:refresh-gb-task-before-capacity-delete", func(tx *gorm.DB) {
		if tx.Statement.Table != (&ipc.GBTaskStateRecord{}).TableName() {
			return
		}
		once.Do(func() {
			if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).
				Model(new(ipc.GBTaskStateRecord)).
				Where("kind = ? AND device_id = ? AND session_id = ?", kind, deviceID, oldSession).
				UpdateColumns(map[string]any{
					"payload":    `{"status":"refreshed"}`,
					"updated_at": orm.Time{Time: refreshedAt},
				}).Error; err != nil {
				t.Errorf("refresh task state before capacity delete: %v", err)
			}
		})
	}); err != nil {
		t.Fatal(err)
	}

	if err := cache.CleanupGBTaskStates(t.Context(), kind, time.Time{}, 1); err != nil {
		t.Fatal(err)
	}
	payload, ok, err := cache.LoadGBTaskState(t.Context(), kind, deviceID, oldSession)
	if err != nil || !ok || string(payload) != `{"status":"refreshed"}` {
		t.Fatalf("refreshed task state = %s, %v, %v", payload, ok, err)
	}
	if _, ok, err := cache.LoadGBTaskState(t.Context(), kind, deviceID, newSession); err != nil || ok {
		t.Fatalf("older task after concurrent refresh survived capacity cleanup: ok=%v err=%v", ok, err)
	}
}
