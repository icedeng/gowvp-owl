package ipccache_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipccache"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/gowvp/owl/pkg/gbs"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

func TestChangeWaitsForDeviceRegistrationStateLock(t *testing.T) {
	db := openCacheTestDatabase(t)
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

func TestChangeContextHonorsCancellation(t *testing.T) {
	db := openCacheTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_change_context", DeviceID: "34020000001320000012", Type: ipc.TypeGB28181,
		IsOnline: true,
	}
	if err := store.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	cache := ipccache.NewCache(store)
	runtime := &gbs.Device{IsOnline: true}
	cache.LoadOrStore(device.DeviceID, runtime)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	persistentCalled := false
	runtimeCalled := false
	err := cache.ChangeContext(ctx, device.DeviceID, func(current *ipc.Device) error {
		persistentCalled = true
		current.IsOnline = false
		return nil
	}, func(current *gbs.Device) {
		runtimeCalled = true
		current.IsOnline = false
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ChangeContext error = %v, want context.Canceled", err)
	}
	if persistentCalled || runtimeCalled {
		t.Fatalf("canceled change invoked callbacks: persistent=%t runtime=%t", persistentCalled, runtimeCalled)
	}
	if !runtime.IsOnlineNow() {
		t.Fatal("canceled change committed runtime state")
	}
	var stored ipc.Device
	if err := store.Device().Get(t.Context(), &stored, orm.Where("id = ?", device.ID)); err != nil {
		t.Fatal(err)
	}
	if !stored.IsOnline {
		t.Fatal("canceled change committed persistent state")
	}
}

func TestLoadDeviceToMemoryMarksRestoredStreamDeviceAndChannelsOffline(t *testing.T) {
	for _, transport := range []string{"tcp", "tls"} {
		t.Run(transport, func(t *testing.T) {
			testLoadDeviceToMemoryMarksRestoredStreamOffline(t, transport)
		})
	}
}

func TestLoadDeviceToMemoryContextHonorsCancellation(t *testing.T) {
	db := openCacheTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	cache := ipccache.NewCache(store)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := cache.LoadDeviceToMemoryContext(ctx, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadDeviceToMemoryContext error = %v, want context.Canceled", err)
	}
}

func TestLoadDeviceChannelsContextRestoresPersistedChannels(t *testing.T) {
	db := openCacheTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	deviceID := "34020000001320000032"
	channelID := "34020000001320000033"
	channel := &ipc.Channel{
		ID: "GBC_restore_on_register", DeviceID: deviceID, ChannelID: channelID, Type: ipc.TypeGB28181,
	}
	if err := store.Channel().Create(t.Context(), channel); err != nil {
		t.Fatal(err)
	}
	cache := ipccache.NewCache(store)
	runtime := &gbs.Device{Address: "192.0.2.32:5060"}

	if err := cache.LoadDeviceChannelsContext(t.Context(), deviceID, runtime); err != nil {
		t.Fatal(err)
	}
	cache.LoadOrStore(deviceID, runtime)
	if _, ok := cache.GetChannel(deviceID, channelID); !ok {
		t.Fatal("persisted channel was not restored into device runtime")
	}
}

func testLoadDeviceToMemoryMarksRestoredStreamOffline(t *testing.T, transport string) {
	db := openCacheTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_restored_tcp", DeviceID: "34020000001320000002", Type: ipc.TypeGB28181,
		Transport: transport, IsOnline: true,
	}
	channel := &ipc.Channel{
		ID: "GBC_restored_tcp", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000003", Type: ipc.TypeGB28181, IsOnline: true, IsPlaying: true,
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
	if restoredDevice.IsOnline || restoredChannel.IsOnline || restoredChannel.IsPlaying {
		t.Fatalf("restored %s state = device:%v channel online:%v playing:%v, want inactive", transport, restoredDevice.IsOnline, restoredChannel.IsOnline, restoredChannel.IsPlaying)
	}
	if restoredDevice.Ext.GBRegistrationClosed == nil || !*restoredDevice.Ext.GBRegistrationClosed {
		t.Fatalf("restored %s device did not persist closed REGISTER binding: %+v", transport, restoredDevice.Ext)
	}
	if _, ok := cache.Load(device.DeviceID); ok {
		t.Fatalf("restored %s device unexpectedly retained an unusable runtime connection", transport)
	}
}

func TestLoadDeviceToMemoryClearsStalePlayingForRestoredUDPDevice(t *testing.T) {
	db := openCacheTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	closed := false
	now := orm.Now()
	device := &ipc.Device{
		ID: "GB_restored_udp", DeviceID: "34020000001320000012", Type: ipc.TypeGB28181,
		Transport: "udp", Address: "192.0.2.10:5060", IsOnline: true,
		RegisteredAt: now, KeepaliveAt: now, Expires: 3600,
		Ext: ipc.DeviceExt{GBRegistrationClosed: &closed},
	}
	channel := &ipc.Channel{
		ID: "GBC_restored_udp", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000013", Type: ipc.TypeGB28181, IsOnline: true, IsPlaying: true,
	}
	if err := store.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	if err := store.Channel().Create(t.Context(), channel); err != nil {
		t.Fatal(err)
	}
	cache := ipccache.NewCache(store)
	if err := cache.LoadDeviceToMemory(nil); err != nil {
		t.Fatal(err)
	}
	var restored ipc.Channel
	if err := store.Channel().Get(t.Context(), &restored, orm.Where("id = ?", channel.ID)); err != nil {
		t.Fatal(err)
	}
	if !restored.IsOnline || restored.IsPlaying {
		t.Fatalf("restored UDP channel state = online:%v playing:%v, want online idle", restored.IsOnline, restored.IsPlaying)
	}
}

func TestChangeContextClearsPlayingOnlyWhenRegistrationCloses(t *testing.T) {
	for _, test := range []struct {
		name        string
		closed      bool
		wantPlaying bool
	}{
		{name: "DeviceStatus offline keeps active binding state", closed: false, wantPlaying: true},
		{name: "closed registration clears playback", closed: true, wantPlaying: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openCacheTestDatabase(t)
			store := ipcdb.NewDB(db).AutoMigrate(true)
			device := &ipc.Device{
				ID: "GB_change_playing", DeviceID: "34020000001320000022", Type: ipc.TypeGB28181, IsOnline: true,
			}
			channel := &ipc.Channel{
				ID: "GBC_change_playing", DID: device.ID, DeviceID: device.DeviceID,
				ChannelID: "34020000001320000023", Type: ipc.TypeGB28181, IsOnline: true, IsPlaying: true,
			}
			if err := store.Device().Create(t.Context(), device); err != nil {
				t.Fatal(err)
			}
			if err := store.Channel().Create(t.Context(), channel); err != nil {
				t.Fatal(err)
			}
			cache := ipccache.NewCache(store)
			if err := cache.ChangeContext(t.Context(), device.DeviceID, func(current *ipc.Device) error {
				current.IsOnline = false
				current.Ext.GBRegistrationClosed = &test.closed
				return nil
			}, nil); err != nil {
				t.Fatal(err)
			}
			var persisted ipc.Channel
			if err := store.Channel().Get(t.Context(), &persisted, orm.Where("id = ?", channel.ID)); err != nil {
				t.Fatal(err)
			}
			if persisted.IsOnline || persisted.IsPlaying != test.wantPlaying {
				t.Fatalf("offline channel state = online:%v playing:%v, want playing:%v", persisted.IsOnline, persisted.IsPlaying, test.wantPlaying)
			}
		})
	}
}

func TestLoadDeviceToMemoryIgnoresNonGBDevices(t *testing.T) {
	db := openCacheTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	devices := []*ipc.Device{
		{
			ID: "RTSP_restored", DeviceID: "rtsp-restored-device", Type: ipc.TypeRTSP,
			Transport: "tcp", Address: "192.0.2.30:554", IsOnline: true,
		},
		{
			ID: "RTMP_restored", DeviceID: "rtmp-restored-device", Type: ipc.TypeRTMP,
			Transport: "udp", Address: "192.0.2.31:1935", IsOnline: true,
		},
	}
	for _, device := range devices {
		if err := store.Device().Create(t.Context(), device); err != nil {
			t.Fatal(err)
		}
	}
	channel := &ipc.Channel{
		ID: "RTSPC_restored", DID: devices[0].ID, DeviceID: devices[0].DeviceID,
		ChannelID: "rtsp-restored-channel", Type: ipc.TypeRTSP, IsOnline: true,
	}
	if err := store.Channel().Create(t.Context(), channel); err != nil {
		t.Fatal(err)
	}
	cache := ipccache.NewCache(store)

	if err := cache.LoadDeviceToMemory(nil); err != nil {
		t.Fatal(err)
	}
	for _, device := range devices {
		if _, ok := cache.Load(device.DeviceID); ok {
			t.Fatalf("non-GB device %s was restored into GB28181 runtime memory", device.Type)
		}
		var stored ipc.Device
		if err := store.Device().Get(t.Context(), &stored, orm.Where("id = ?", device.ID)); err != nil {
			t.Fatal(err)
		}
		if !stored.IsOnline {
			t.Fatalf("non-GB device %s was marked offline during GB28181 restore", device.Type)
		}
	}
	var storedChannel ipc.Channel
	if err := store.Channel().Get(t.Context(), &storedChannel, orm.Where("id = ?", channel.ID)); err != nil {
		t.Fatal(err)
	}
	if !storedChannel.IsOnline {
		t.Fatal("non-GB channel was marked offline during GB28181 restore")
	}
}

func TestPasswordChangePersistsCredentialAndOfflineStateAtomically(t *testing.T) {
	for _, test := range []struct {
		name        string
		newPassword string
	}{
		{name: "replace device password", newPassword: "new-password"},
		{name: "clear device password and fall back to global password"},
	} {
		t.Run(test.name, func(t *testing.T) {
			testPasswordChangePersistsCredentialAndOfflineStateAtomically(t, test.newPassword)
		})
	}
}

func testPasswordChangePersistsCredentialAndOfflineStateAtomically(t *testing.T, newPassword string) {
	db := openCacheTestDatabase(t)
	base := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_password", DeviceID: "34020000001320000004", Type: ipc.TypeGB28181,
		Password: "old-password", IsOnline: true,
	}
	channel := &ipc.Channel{
		ID: "GBC_password", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000009", Type: ipc.TypeGB28181, IsOnline: true,
	}
	if err := base.Device().Create(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	if err := base.Channel().Create(context.Background(), channel); err != nil {
		t.Fatal(err)
	}
	failingDevice := &failSecondDeviceUpdateStore{DeviceStorer: base.Device()}
	store := &overrideDeviceStore{Storer: base, device: failingDevice}
	cache := ipccache.NewCache(store)
	runtime := &gbs.Device{Password: "old-password", IsOnline: true}
	cache.LoadOrStore(device.DeviceID, runtime)

	var updated ipc.Device
	err := cache.Device().Update(context.Background(), &updated, func(current *ipc.Device) error {
		current.Password = newPassword
		return nil
	}, orm.Where("device_id = ?", device.DeviceID))
	if err != nil {
		t.Fatal(err)
	}
	var storedDevice ipc.Device
	if err := base.Device().Get(t.Context(), &storedDevice, orm.Where("id = ?", device.ID)); err != nil {
		t.Fatal(err)
	}
	var storedChannel ipc.Channel
	if err := base.Channel().Get(t.Context(), &storedChannel, orm.Where("id = ?", channel.ID)); err != nil {
		t.Fatal(err)
	}
	if storedDevice.Password != newPassword || storedDevice.IsOnline ||
		storedDevice.Ext.GBRegistrationClosed == nil || !*storedDevice.Ext.GBRegistrationClosed ||
		storedChannel.IsOnline || runtime.PasswordValue() != newPassword || runtime.IsOnlineNow() {
		t.Fatalf("atomic password change = device:%+v channel:%+v runtime_password:%q runtime_online:%v",
			storedDevice, storedChannel, runtime.PasswordValue(), runtime.IsOnlineNow())
	}
}

func TestPasswordChangeRollbackKeepsCredentialAndRegistration(t *testing.T) {
	db := openCacheTestDatabase(t)
	base := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_password_rollback", DeviceID: "34020000001320000010", Type: ipc.TypeGB28181,
		Password: "old-password", IsOnline: true,
	}
	channel := &ipc.Channel{
		ID: "GBC_password_rollback", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000011", Type: ipc.TypeGB28181, IsOnline: true,
	}
	if err := base.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	if err := base.Channel().Create(t.Context(), channel); err != nil {
		t.Fatal(err)
	}
	failingDevice := &failDeviceSessionStore{DeviceStorer: base.Device(), err: errPasswordChangeCommit}
	store := &overrideDeviceStore{Storer: base, device: failingDevice}
	cache := ipccache.NewCache(store)
	runtime := &gbs.Device{Password: "old-password", IsOnline: true}
	cache.LoadOrStore(device.DeviceID, runtime)

	var updated ipc.Device
	err := cache.Device().Update(t.Context(), &updated, func(current *ipc.Device) error {
		current.Password = "new-password"
		return nil
	}, orm.Where("device_id = ?", device.DeviceID))
	if !errors.Is(err, errPasswordChangeCommit) {
		t.Fatalf("password rollback result = %v", err)
	}
	var storedDevice ipc.Device
	if err := base.Device().Get(t.Context(), &storedDevice, orm.Where("id = ?", device.ID)); err != nil {
		t.Fatal(err)
	}
	var storedChannel ipc.Channel
	if err := base.Channel().Get(t.Context(), &storedChannel, orm.Where("id = ?", channel.ID)); err != nil {
		t.Fatal(err)
	}
	if storedDevice.Password != "old-password" || !storedDevice.IsOnline || !storedChannel.IsOnline ||
		runtime.PasswordValue() != "old-password" || !runtime.IsOnlineNow() {
		t.Fatalf("rolled-back password change = device:%+v channel:%+v runtime_password:%q runtime_online:%v",
			storedDevice, storedChannel, runtime.PasswordValue(), runtime.IsOnlineNow())
	}
}

func TestChangeRollsBackDeviceWhenChannelOfflinePersistenceFails(t *testing.T) {
	db := openCacheTestDatabase(t)
	base := ipcdb.NewDB(db).AutoMigrate(true)
	registeredAt := time.Now()
	device := &ipc.Device{
		ID: "GB_channel_offline", DeviceID: "34020000001320000005", Type: ipc.TypeGB28181,
		IsOnline: true, RegisteredAt: orm.Time{Time: registeredAt}, KeepaliveAt: orm.Time{Time: registeredAt}, Expires: 3600,
	}
	channel := &ipc.Channel{
		ID: "GBC_channel_offline", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000006", Type: ipc.TypeGB28181, IsOnline: true,
	}
	if err := base.Device().Create(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	if err := base.Channel().Create(context.Background(), channel); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Update().Before("gorm:update").Register("test:fail_channel_offline", func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" {
			tx.AddError(errBatchEditChannel)
		}
	}); err != nil {
		t.Fatal(err)
	}
	cache := ipccache.NewCache(base)
	runtime := &gbs.Device{IsOnline: true, LastRegisterAt: registeredAt, LastKeepaliveAt: registeredAt, Expires: 3600}
	cache.LoadOrStore(device.DeviceID, runtime)

	err := cache.Change(device.DeviceID, func(current *ipc.Device) error {
		current.IsOnline = false
		return nil
	}, func(current *gbs.Device) {
		current.Expires = 0
		current.IsOnline = false
	})
	if err == nil || !errors.Is(err, errBatchEditChannel) {
		t.Fatalf("channel offline persistence result = %v", err)
	}
	if !runtime.IsOnlineNow() || runtime.Expires != 3600 {
		t.Fatalf("failed channel persistence committed runtime = online:%t expires:%d", runtime.IsOnlineNow(), runtime.Expires)
	}
	var storedDevice ipc.Device
	if err := base.Device().Get(context.Background(), &storedDevice, orm.Where("id = ?", device.ID)); err != nil {
		t.Fatal(err)
	}
	var storedChannel ipc.Channel
	if err := base.Channel().Get(context.Background(), &storedChannel, orm.Where("id = ?", channel.ID)); err != nil {
		t.Fatal(err)
	}
	if !storedDevice.IsOnline || !storedChannel.IsOnline {
		t.Fatalf("failed channel update left partial database state = device:%t channel:%t", storedDevice.IsOnline, storedChannel.IsOnline)
	}
}

var errSecondDeviceUpdate = errors.New("second device update failed")

var errPasswordChangeCommit = errors.New("password change commit failed")

type overrideDeviceStore struct {
	ipc.Storer
	device ipc.DeviceStorer
}

func (s *overrideDeviceStore) Device() ipc.DeviceStorer { return s.device }

type failSecondDeviceUpdateStore struct {
	ipc.DeviceStorer
	updates int
}

type failDeviceSessionStore struct {
	ipc.DeviceStorer
	err error
}

func (s *failDeviceSessionStore) Session(ctx context.Context, changeFns ...func(*gorm.DB) error) error {
	return s.DeviceStorer.Session(ctx, append(changeFns, func(*gorm.DB) error { return s.err })...)
}

var errBatchEditChannel = errors.New("batch edit channel failed")

func (s *failSecondDeviceUpdateStore) Update(ctx context.Context, device *ipc.Device, change func(*ipc.Device) error, opts ...orm.QueryOption) error {
	s.updates++
	if s.updates == 2 {
		return errSecondDeviceUpdate
	}
	return s.DeviceStorer.Update(ctx, device, change, opts...)
}
