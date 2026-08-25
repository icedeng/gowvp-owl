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
	"github.com/ixugo/goddd/pkg/orm"
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

func TestLoadDeviceToMemoryMarksRestoredTCPDeviceAndChannelsOffline(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_restored_tcp", DeviceID: "34020000001320000002", Type: ipc.TypeGB28181,
		Transport: "tcp", IsOnline: true,
	}
	channel := &ipc.Channel{
		ID: "GBC_restored_tcp", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000003", Type: ipc.TypeGB28181, IsOnline: true,
	}
	if err := store.Device().Create(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	if err := store.Channel().Create(context.Background(), channel); err != nil {
		t.Fatal(err)
	}
	cache := ipccache.NewCache(store)

	if err := cache.LoadDeviceToMemory(nil); err != nil {
		t.Fatal(err)
	}
	var restoredDevice ipc.Device
	if err := store.Device().Get(context.Background(), &restoredDevice, orm.Where("id = ?", device.ID)); err != nil {
		t.Fatal(err)
	}
	var restoredChannel ipc.Channel
	if err := store.Channel().Get(context.Background(), &restoredChannel, orm.Where("id = ?", channel.ID)); err != nil {
		t.Fatal(err)
	}
	if restoredDevice.IsOnline || restoredChannel.IsOnline {
		t.Fatalf("restored TCP state = device:%v channel:%v, want both offline", restoredDevice.IsOnline, restoredChannel.IsOnline)
	}
	if _, ok := cache.Load(device.DeviceID); ok {
		t.Fatal("restored TCP device unexpectedly retained an unusable runtime connection")
	}
}
