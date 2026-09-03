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

func TestGBCascadeTaskRoutePersistsBothIndexesAndCleansUp(t *testing.T) {
	db := openInternalCacheTestDatabase(t)
	base := ipcdb.NewDB(db).AutoMigrate(true)
	first := NewCache(base)
	now := time.Now()
	kind := "upgrade"
	platformName := "provincial"
	deviceID := "34020000001320000001"
	downstreamSessionID := "downstream-session-0000000000001"
	exposedID := "34020000001320000911"
	upstreamSessionID := "upstream-session-000000000000001"
	payload := []byte(`{"completed":false}`)
	if err := first.SaveGBCascadeTaskRoute(t.Context(), kind, platformName, deviceID, downstreamSessionID, exposedID, upstreamSessionID, payload, now); err != nil {
		t.Fatal(err)
	}

	restarted := NewCache(ipcdb.NewDB(db))
	for _, load := range []func() ([]byte, bool, error){
		func() ([]byte, bool, error) {
			return restarted.LoadGBCascadeTaskRouteByDownstream(t.Context(), kind, deviceID, downstreamSessionID)
		},
		func() ([]byte, bool, error) {
			return restarted.LoadGBCascadeTaskRouteByUpstream(t.Context(), kind, platformName, exposedID, upstreamSessionID)
		},
	} {
		got, ok, err := load()
		if err != nil || !ok || string(got) != string(payload) {
			t.Fatalf("restored cascade task route = %s, %v, %v", got, ok, err)
		}
	}
	if err := restarted.CleanupGBCascadeTaskRoutes(t.Context(), now.Add(time.Second), 1024); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := restarted.LoadGBCascadeTaskRouteByDownstream(t.Context(), kind, deviceID, downstreamSessionID); err != nil || ok {
		t.Fatalf("expired cascade task route survived cleanup: ok=%v err=%v", ok, err)
	}
}

func TestGBCascadeTaskRouteRejectsConflictingUpstreamOwner(t *testing.T) {
	db := openInternalCacheTestDatabase(t)
	cache := NewCache(ipcdb.NewDB(db).AutoMigrate(true))
	now := time.Now()
	const (
		kind              = "upgrade"
		platformName      = "provincial"
		exposedID         = "34020000001320000911"
		upstreamSessionID = "upstream-session-conflict-00000001"
	)
	if err := cache.SaveGBCascadeTaskRoute(t.Context(), kind, platformName,
		"34020000001320000001", "downstream-session-conflict-00001", exposedID, upstreamSessionID,
		[]byte(`{"route":1}`), now); err != nil {
		t.Fatal(err)
	}
	if err := cache.SaveGBCascadeTaskRoute(t.Context(), kind, platformName,
		"34020000001320000002", "downstream-session-conflict-00002", exposedID, upstreamSessionID,
		[]byte(`{"route":2}`), now); err == nil {
		t.Fatal("same upstream task identity was assigned to two downstream routes")
	}
}

func TestGBCascadeTaskRouteDeletesByUpstreamIdentity(t *testing.T) {
	db := openInternalCacheTestDatabase(t)
	cache := NewCache(ipcdb.NewDB(db).AutoMigrate(true))
	const (
		kind                = "upgrade"
		platformName        = "upstream-delete-platform"
		deviceID            = "34020000001320000001"
		downstreamSessionID = "downstream-delete-upstream-00001"
		exposedID           = "34020000001320000911"
		upstreamSessionID   = "upstream-delete-session-0000001"
	)
	if err := cache.SaveGBCascadeTaskRoute(
		t.Context(), kind, platformName, deviceID, downstreamSessionID, exposedID, upstreamSessionID,
		[]byte(`{"route":true}`), time.Now(),
	); err != nil {
		t.Fatal(err)
	}
	if err := cache.DeleteGBCascadeTaskRouteByUpstream(t.Context(), kind, platformName, exposedID, upstreamSessionID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.LoadGBCascadeTaskRouteByDownstream(t.Context(), kind, deviceID, downstreamSessionID); err != nil || ok {
		t.Fatalf("route survived upstream deletion through downstream index: found:%v err:%v", ok, err)
	}
	if _, ok, err := cache.LoadGBCascadeTaskRouteByUpstream(t.Context(), kind, platformName, exposedID, upstreamSessionID); err != nil || ok {
		t.Fatalf("route survived upstream deletion: found:%v err:%v", ok, err)
	}
}

func TestGBCascadeTaskRouteUpdateKeepsIndexesConsistent(t *testing.T) {
	db := openInternalCacheTestDatabase(t)
	cache := NewCache(ipcdb.NewDB(db).AutoMigrate(true))
	const (
		kind                = "snapshot"
		platformName        = "provincial"
		deviceID            = "34020000001320000001"
		downstreamSessionID = "downstream-session-update-0000001"
		exposedID           = "34020000001320000911"
		oldUpstreamSession  = "upstream-session-update-old-00001"
		newUpstreamSession  = "upstream-session-update-new-00001"
	)
	if err := cache.SaveGBCascadeTaskRoute(t.Context(), kind, platformName, deviceID, downstreamSessionID,
		exposedID, oldUpstreamSession, []byte(`{"completed":false}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	updated := []byte(`{"completed":true}`)
	if err := cache.SaveGBCascadeTaskRoute(t.Context(), kind, platformName, deviceID, downstreamSessionID,
		exposedID, newUpstreamSession, updated, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.LoadGBCascadeTaskRouteByUpstream(t.Context(), kind, platformName, exposedID, oldUpstreamSession); err != nil || ok {
		t.Fatalf("old upstream index survived route update: ok=%v err=%v", ok, err)
	}
	for _, load := range []func() ([]byte, bool, error){
		func() ([]byte, bool, error) {
			return cache.LoadGBCascadeTaskRouteByDownstream(t.Context(), kind, deviceID, downstreamSessionID)
		},
		func() ([]byte, bool, error) {
			return cache.LoadGBCascadeTaskRouteByUpstream(t.Context(), kind, platformName, exposedID, newUpstreamSession)
		},
	} {
		payload, ok, err := load()
		if err != nil || !ok || string(payload) != string(updated) {
			t.Fatalf("updated route index = %s, %v, %v", payload, ok, err)
		}
	}
}

func TestGBCascadeTaskRouteUpdateRefreshesCleanupTTL(t *testing.T) {
	db := openInternalCacheTestDatabase(t)
	cache := NewCache(ipcdb.NewDB(db).AutoMigrate(true))
	const (
		kind                = "upgrade"
		platformName        = "ttl-platform"
		deviceID            = "34020000001320000001"
		downstreamSessionID = "downstream-session-ttl-update-001"
		exposedID           = "34020000001320000911"
		upstreamSessionID   = "upstream-session-ttl-update-00001"
	)
	createdAt := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
	updatedAt := createdAt.Add(time.Hour)
	if err := cache.SaveGBCascadeTaskRoute(t.Context(), kind, platformName, deviceID, downstreamSessionID,
		exposedID, upstreamSessionID, []byte(`{"completed":false}`), createdAt); err != nil {
		t.Fatal(err)
	}
	updatedPayload := []byte(`{"completed":true}`)
	if err := cache.SaveGBCascadeTaskRoute(t.Context(), kind, platformName, deviceID, downstreamSessionID,
		exposedID, upstreamSessionID, updatedPayload, updatedAt); err != nil {
		t.Fatal(err)
	}

	if err := cache.CleanupGBCascadeTaskRoutes(t.Context(), createdAt.Add(30*time.Minute), 1024); err != nil {
		t.Fatal(err)
	}
	payload, ok, err := cache.LoadGBCascadeTaskRouteByDownstream(t.Context(), kind, deviceID, downstreamSessionID)
	if err != nil || !ok || string(payload) != string(updatedPayload) {
		t.Fatalf("updated cascade route after old cutoff = %s, %v, %v", payload, ok, err)
	}

	if err := cache.CleanupGBCascadeTaskRoutes(t.Context(), updatedAt.Add(time.Second), 1024); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.LoadGBCascadeTaskRouteByDownstream(t.Context(), kind, deviceID, downstreamSessionID); err != nil || ok {
		t.Fatalf("updated cascade route survived new cutoff: ok=%v err=%v", ok, err)
	}
}

func TestGBCascadeTaskRouteCapacityCleanupUsesCurrentOrdering(t *testing.T) {
	db := openInternalCacheTestDatabase(t)
	cache := NewCache(ipcdb.NewDB(db).AutoMigrate(true))
	const (
		kind               = "snapshot"
		platformName       = "capacity-platform"
		oldDeviceID        = "34020000001320000001"
		newDeviceID        = "34020000001320000002"
		oldDownstream      = "downstream-capacity-old-0000001"
		newDownstream      = "downstream-capacity-new-0000001"
		oldExposedID       = "34020000001320000911"
		newExposedID       = "34020000001320000912"
		oldUpstreamSession = "upstream-capacity-old-000000001"
		newUpstreamSession = "upstream-capacity-new-000000001"
	)
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	if err := cache.SaveGBCascadeTaskRoute(t.Context(), kind, platformName, oldDeviceID, oldDownstream,
		oldExposedID, oldUpstreamSession, []byte(`{"route":"old"}`), base); err != nil {
		t.Fatal(err)
	}
	if err := cache.SaveGBCascadeTaskRoute(t.Context(), kind, platformName, newDeviceID, newDownstream,
		newExposedID, newUpstreamSession, []byte(`{"route":"new"}`), base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	refreshedAt := base.Add(2 * time.Minute)
	oldRouteKey := ipc.GBCascadeTaskRouteKey(kind, oldDeviceID, oldDownstream)
	var once sync.Once
	if err := db.Callback().Delete().Before("gorm:delete").Register("test:refresh-gb-cascade-before-capacity-delete", func(tx *gorm.DB) {
		if tx.Statement.Table != (&ipc.GBCascadeTaskRouteRecord{}).TableName() {
			return
		}
		once.Do(func() {
			if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).
				Model(new(ipc.GBCascadeTaskRouteRecord)).
				Where("route_key = ?", oldRouteKey).
				UpdateColumns(map[string]any{
					"payload":    `{"route":"refreshed"}`,
					"updated_at": orm.Time{Time: refreshedAt},
				}).Error; err != nil {
				t.Errorf("refresh cascade route before capacity delete: %v", err)
			}
		})
	}); err != nil {
		t.Fatal(err)
	}

	if err := cache.CleanupGBCascadeTaskRoutes(t.Context(), time.Time{}, 1); err != nil {
		t.Fatal(err)
	}
	payload, ok, err := cache.LoadGBCascadeTaskRouteByDownstream(t.Context(), kind, oldDeviceID, oldDownstream)
	if err != nil || !ok || string(payload) != `{"route":"refreshed"}` {
		t.Fatalf("refreshed cascade route = %s, %v, %v", payload, ok, err)
	}
	if _, ok, err := cache.LoadGBCascadeTaskRouteByDownstream(t.Context(), kind, newDeviceID, newDownstream); err != nil || ok {
		t.Fatalf("older cascade route after concurrent refresh survived capacity cleanup: ok=%v err=%v", ok, err)
	}
}

func TestDeleteDeviceRemovesGBCascadeTaskRoutes(t *testing.T) {
	db := openInternalCacheTestDatabase(t)
	base := ipcdb.NewDB(db).AutoMigrate(true)
	cache := NewCache(base)
	device := &ipc.Device{ID: "GB_cascade_route", DeviceID: "34020000001320000001", Type: ipc.TypeGB28181}
	if err := base.Device().Create(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	const (
		kind                = "upgrade"
		platformName        = "provincial"
		downstreamSessionID = "downstream-session-delete-000001"
		exposedID           = "34020000001320000911"
		upstreamSessionID   = "upstream-session-delete-00000001"
	)
	if err := cache.SaveGBCascadeTaskRoute(t.Context(), kind, platformName, device.DeviceID, downstreamSessionID,
		exposedID, upstreamSessionID, []byte(`{"completed":false}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := cache.Device().Delete(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.LoadGBCascadeTaskRouteByDownstream(t.Context(), kind, device.DeviceID, downstreamSessionID); err != nil || ok {
		t.Fatalf("device cascade task route survived deletion: ok=%v err=%v", ok, err)
	}
	if _, ok, err := cache.LoadGBCascadeTaskRouteByUpstream(t.Context(), kind, platformName, exposedID, upstreamSessionID); err != nil || ok {
		t.Fatalf("device cascade task upstream index survived deletion: ok=%v err=%v", ok, err)
	}
}
