package ipccache_test

import (
	"context"
	"errors"
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

func TestLoadDeviceToMemoryMarksRestoredStreamDeviceAndChannelsOffline(t *testing.T) {
	for _, transport := range []string{"tcp", "tls"} {
		t.Run(transport, func(t *testing.T) {
			testLoadDeviceToMemoryMarksRestoredStreamOffline(t, transport)
		})
	}
}

func testLoadDeviceToMemoryMarksRestoredStreamOffline(t *testing.T, transport string) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_restored_tcp", DeviceID: "34020000001320000002", Type: ipc.TypeGB28181,
		Transport: transport, IsOnline: true,
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
		t.Fatalf("restored %s state = device:%v channel:%v, want both offline", transport, restoredDevice.IsOnline, restoredChannel.IsOnline)
	}
	if _, ok := cache.Load(device.DeviceID); ok {
		t.Fatalf("restored %s device unexpectedly retained an unusable runtime connection", transport)
	}
}

func TestPasswordChangeReturnsRuntimeOfflinePersistenceFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	base := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_password", DeviceID: "34020000001320000004", Type: ipc.TypeGB28181,
		Password: "old-password", IsOnline: true,
	}
	if err := base.Device().Create(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	failingDevice := &failSecondDeviceUpdateStore{DeviceStorer: base.Device()}
	store := &overrideDeviceStore{Storer: base, device: failingDevice}
	cache := ipccache.NewCache(store)
	runtime := &gbs.Device{Password: "old-password", IsOnline: true}
	cache.LoadOrStore(device.DeviceID, runtime)

	var updated ipc.Device
	err = cache.Device().Update(context.Background(), &updated, func(current *ipc.Device) error {
		current.Password = "new-password"
		return nil
	}, orm.Where("device_id = ?", device.DeviceID))
	if err == nil || !errors.Is(err, errSecondDeviceUpdate) {
		t.Fatalf("password change result = %v", err)
	}
	if runtime.PasswordValue() != "old-password" || !runtime.IsOnlineNow() {
		t.Fatalf("failed offline persistence changed runtime = password:%q online:%v", runtime.PasswordValue(), runtime.IsOnlineNow())
	}
}

var errSecondDeviceUpdate = errors.New("second device update failed")

type overrideDeviceStore struct {
	ipc.Storer
	device ipc.DeviceStorer
}

func (s *overrideDeviceStore) Device() ipc.DeviceStorer { return s.device }

type failSecondDeviceUpdateStore struct {
	ipc.DeviceStorer
	updates int
}

func (s *failSecondDeviceUpdateStore) Update(ctx context.Context, device *ipc.Device, change func(*ipc.Device) error, opts ...orm.QueryOption) error {
	s.updates++
	if s.updates == 2 {
		return errSecondDeviceUpdate
	}
	return s.DeviceStorer.Update(ctx, device, change, opts...)
}
