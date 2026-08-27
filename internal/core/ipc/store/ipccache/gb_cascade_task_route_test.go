package ipccache

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"gorm.io/gorm"
)

func TestGBCascadeTaskRoutePersistsBothIndexesAndCleansUp(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
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
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
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

func TestGBCascadeTaskRouteUpdateKeepsIndexesConsistent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
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

func TestDeleteDeviceRemovesGBCascadeTaskRoutes(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
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
