package ipccache_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipccache"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/gowvp/owl/pkg/gbs"
	"gorm.io/gorm"
)

func TestChangeWaitsForDeviceRegistrationStateLock(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{ID: "GB_cache", DeviceID: "34020000001320000001", Type: ipc.TypeGB28181}
	if err := store.Device().Create(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	cache := ipccache.NewCache(store)
	runtime := &gbs.Device{IsOnline: true}
	cache.LoadOrStore(device.DeviceID, runtime)

	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- runtime.SerializeRegistrationState(func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	changeEntered := make(chan struct{})
	changeStarted := make(chan struct{})
	changeDone := make(chan error, 1)
	go func() {
		close(changeStarted)
		changeDone <- cache.Change(device.DeviceID, func(current *ipc.Device) error {
			close(changeEntered)
			current.IsOnline = false
			return nil
		}, nil)
	}()
	<-changeStarted
	select {
	case <-changeEntered:
		t.Fatal("cache change entered while registration state lock was held")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-changeEntered:
	case <-time.After(time.Second):
		t.Fatal("cache change did not enter after registration state lock was released")
	}
	if err := <-changeDone; err != nil {
		t.Fatal(err)
	}
}
