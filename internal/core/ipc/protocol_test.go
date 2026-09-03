package ipc_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/domain/uniqueid/store/uniqueiddb"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

func TestAddGBDeviceRejectsInvalidRTPStreamModeBeforeConversion(t *testing.T) {
	var core ipc.Core
	for _, streamMode := range []int{-1, 3, 256} {
		_, err := core.AddDevice(t.Context(), &ipc.AddDeviceInput{
			Type: ipc.TypeGB28181, DeviceID: "34020000001320000001", StreamMode: streamMode,
		})
		if err == nil || !strings.Contains(err.Error(), "invalid RTP stream mode") {
			t.Fatalf("stream mode %d error = %v", streamMode, err)
		}
	}
}

func TestEditGBDeviceRejectsInvalidRTPStreamModeBeforeConversion(t *testing.T) {
	db := openIPCTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_invalid_stream_mode", DeviceID: "34020000001320000001", Type: ipc.TypeGB28181,
	}
	if err := store.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	core := ipc.NewCore(store, uniqueid.Core{}, map[string]ipc.Protocoler{})
	for _, streamMode := range []int{-1, 3, 256} {
		_, err := core.EditDevice(t.Context(), &ipc.EditDeviceInput{
			DeviceID: device.DeviceID, StreamMode: streamMode,
		}, device.ID)
		if err == nil || !strings.Contains(err.Error(), "invalid RTP stream mode") {
			t.Fatalf("stream mode %d error = %v", streamMode, err)
		}
	}
}

func TestAdapterSaveChannelSnapshotRollsBackOnCommitFailure(t *testing.T) {
	db := openIPCTestDatabase(t)
	base := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_catalog_rollback", DeviceID: "34020000001320000001",
		Type: ipc.TypeGB28181, Channels: 1,
	}
	existing := &ipc.Channel{
		ID: "GBC_catalog_existing", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000002", Name: "existing", Type: ipc.TypeGB28181, IsOnline: true,
	}
	if err := base.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	if err := base.Channel().Create(t.Context(), existing); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected Catalog transaction failure")
	channelStore := &failCommitChannelStore{ChannelStorer: base.Channel(), err: injected}
	store := &failCommitStore{Storer: base, channel: channelStore}
	uni := uniqueid.NewCore(uniqueiddb.NewDB(db).AutoMigrate(true), 5)
	adapter := ipc.NewAdapter(store, uni)

	err := adapter.SaveChannelSnapshot(t.Context(), device.DeviceID, []*ipc.Channel{
		{DeviceID: device.DeviceID, ChannelID: existing.ChannelID, Name: "changed", Type: ipc.TypeGB28181},
		{DeviceID: device.DeviceID, ChannelID: "34020000001320000003", Name: "new", Type: ipc.TypeGB28181, IsOnline: true},
	})
	if !errors.Is(err, injected) {
		t.Fatalf("SaveChannelSnapshot error = %v, want %v", err, injected)
	}

	var persistedDevice ipc.Device
	if err := base.Device().Get(t.Context(), &persistedDevice, orm.Where("id = ?", device.ID)); err != nil {
		t.Fatal(err)
	}
	if persistedDevice.Channels != 1 {
		t.Fatalf("device channel count = %d, want rolled back value 1", persistedDevice.Channels)
	}
	var persistedExisting ipc.Channel
	if err := base.Channel().Get(t.Context(), &persistedExisting, orm.Where("id = ?", existing.ID)); err != nil {
		t.Fatal(err)
	}
	if persistedExisting.Name != existing.Name || !persistedExisting.IsOnline {
		t.Fatalf("existing channel after rollback = %+v", persistedExisting)
	}
	var persistedNew ipc.Channel
	if err := base.Channel().Get(t.Context(), &persistedNew, orm.Where("channel_id = ?", "34020000001320000003")); !orm.IsErrRecordNotFound(err) {
		t.Fatalf("new channel lookup after rollback = %v, channel = %+v", err, persistedNew)
	}
}

func TestAdapterSaveChannelSnapshotCommitsCompleteAndEmptySnapshots(t *testing.T) {
	db := openIPCTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_catalog_commit", DeviceID: "34020000001320000011",
		Type: ipc.TypeGB28181, Channels: 2,
	}
	existing := &ipc.Channel{
		ID: "GBC_catalog_commit_existing", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000012", Name: "old", Type: ipc.TypeGB28181,
		IsOnline: true, IsPlaying: true,
		Ext: ipc.DeviceExt{Manufacturer: "old vendor", EnabledAI: true, RecordMode: "ai"},
	}
	missing := &ipc.Channel{
		ID: "GBC_catalog_commit_missing", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000013", Name: "missing", Type: ipc.TypeGB28181, IsOnline: true, IsPlaying: true,
	}
	if err := store.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	for _, channel := range []*ipc.Channel{existing, missing} {
		if err := store.Channel().Create(t.Context(), channel); err != nil {
			t.Fatal(err)
		}
	}
	uni := uniqueid.NewCore(uniqueiddb.NewDB(db).AutoMigrate(true), 5)
	adapter := ipc.NewAdapter(store, uni)
	newChannel := &ipc.Channel{
		DeviceID: device.DeviceID, ChannelID: "34020000001320000014",
		Name: "new", Type: ipc.TypeGB28181, IsOnline: true,
	}
	if err := adapter.SaveChannelSnapshot(t.Context(), device.DeviceID, []*ipc.Channel{
		{
			DeviceID: device.DeviceID, ChannelID: existing.ChannelID,
			Name: "updated", Type: ipc.TypeGB28181, IsOnline: false,
			Ext: ipc.DeviceExt{Manufacturer: "new vendor"},
		},
		newChannel,
	}); err != nil {
		t.Fatal(err)
	}
	if newChannel.ID == "" || newChannel.DID != device.ID {
		t.Fatalf("new channel identity = id:%q did:%q", newChannel.ID, newChannel.DID)
	}

	var persistedDevice ipc.Device
	if err := store.Device().Get(t.Context(), &persistedDevice, orm.Where("id = ?", device.ID)); err != nil {
		t.Fatal(err)
	}
	if persistedDevice.Channels != 2 {
		t.Fatalf("device channel count = %d, want 2", persistedDevice.Channels)
	}
	var persistedExisting ipc.Channel
	if err := store.Channel().Get(t.Context(), &persistedExisting, orm.Where("id = ?", existing.ID)); err != nil {
		t.Fatal(err)
	}
	if persistedExisting.Name != "updated" || persistedExisting.IsOnline || persistedExisting.IsPlaying {
		t.Fatalf("updated channel = %+v", persistedExisting)
	}
	if persistedExisting.Ext.Manufacturer != "new vendor" || !persistedExisting.Ext.EnabledAI || persistedExisting.Ext.RecordMode != "ai" {
		t.Fatalf("updated channel extensions = %+v", persistedExisting.Ext)
	}
	var persistedMissing ipc.Channel
	if err := store.Channel().Get(t.Context(), &persistedMissing, orm.Where("id = ?", missing.ID)); err != nil {
		t.Fatal(err)
	}
	if persistedMissing.IsOnline || persistedMissing.IsPlaying {
		t.Fatalf("missing channel retained active state: %+v", persistedMissing)
	}
	var persistedNew ipc.Channel
	if err := store.Channel().Get(t.Context(), &persistedNew, orm.Where("id = ?", newChannel.ID)); err != nil {
		t.Fatal(err)
	}
	if persistedNew.DID != device.ID || persistedNew.ChannelID != newChannel.ChannelID || !persistedNew.IsOnline {
		t.Fatalf("created channel = %+v", persistedNew)
	}

	if err := adapter.SaveChannelSnapshot(t.Context(), device.DeviceID, nil); err != nil {
		t.Fatal(err)
	}
	persistedDevice = ipc.Device{}
	if err := store.Device().Get(t.Context(), &persistedDevice, orm.Where("id = ?", device.ID)); err != nil {
		t.Fatal(err)
	}
	if persistedDevice.Channels != 0 {
		t.Fatalf("empty snapshot device channel count = %d, want 0", persistedDevice.Channels)
	}
	var online []*ipc.Channel
	if _, err := store.Channel().List(t.Context(), &online, web.NewPagerFilterMaxSize(),
		orm.Where("device_id = ? AND is_online = ?", device.DeviceID, true)); err != nil {
		t.Fatal(err)
	}
	if len(online) != 0 {
		t.Fatalf("empty snapshot retained online channels: %+v", online)
	}
}

func TestAdapterSaveChannelSnapshotPreservesPlayingForOnlineChannel(t *testing.T) {
	db := openIPCTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{ID: "GB_catalog_online", DeviceID: "34020000001320000021", Type: ipc.TypeGB28181}
	channel := &ipc.Channel{
		ID: "GBC_catalog_online", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000022", Type: ipc.TypeGB28181, IsOnline: true, IsPlaying: true,
	}
	if err := store.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	if err := store.Channel().Create(t.Context(), channel); err != nil {
		t.Fatal(err)
	}
	adapter := ipc.NewAdapter(store, uniqueid.NewCore(uniqueiddb.NewDB(db).AutoMigrate(true), 5))
	if err := adapter.SaveChannelSnapshot(t.Context(), device.DeviceID, []*ipc.Channel{{
		DeviceID: device.DeviceID, ChannelID: channel.ChannelID, Type: ipc.TypeGB28181, IsOnline: true,
	}}); err != nil {
		t.Fatal(err)
	}
	var persisted ipc.Channel
	if err := store.Channel().Get(t.Context(), &persisted, orm.Where("id = ?", channel.ID)); err != nil {
		t.Fatal(err)
	}
	if !persisted.IsOnline || !persisted.IsPlaying {
		t.Fatalf("online Catalog refresh changed active playback state: %+v", persisted)
	}
}

func TestAdapterGetDeviceByDeviceIDRecoversConcurrentCreate(t *testing.T) {
	db := openIPCTestDatabase(t)
	base := ipcdb.NewDB(db).AutoMigrate(true)
	existing := &ipc.Device{ID: "GB_existing", DeviceID: "34020000001320000001", Type: ipc.TypeGB28181}
	if err := base.Device().Create(context.Background(), existing); err != nil {
		t.Fatal(err)
	}
	deviceStore := &concurrentCreateDeviceStore{DeviceStorer: base.Device()}
	store := &concurrentCreateStore{Storer: base, device: deviceStore}
	uni := uniqueid.NewCore(uniqueiddb.NewDB(db).AutoMigrate(true), 5)
	adapter := ipc.NewAdapter(store, uni)

	device, err := adapter.GetDeviceByDeviceID(existing.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if device.ID != existing.ID || device.DeviceID != existing.DeviceID {
		t.Fatalf("resolved device = %+v, want existing record %+v", device, existing)
	}
	if got := deviceStore.createCalls.Load(); got != 1 {
		t.Fatalf("create calls = %d, want 1", got)
	}
}

func TestAdapterGetDeviceByDeviceIDContextHonorsCancellation(t *testing.T) {
	db := openIPCTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	uni := uniqueid.NewCore(uniqueiddb.NewDB(db).AutoMigrate(true), 5)
	adapter := ipc.NewAdapter(store, uni)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.GetDeviceByDeviceIDContext(ctx, "34020000001320000001"); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetDeviceByDeviceIDContext error = %v, want context.Canceled", err)
	}
}

func TestAdapterUpdateContextHonorsCancellation(t *testing.T) {
	db := openIPCTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{ID: "GB_update_context", DeviceID: "34020000001320000013", Type: ipc.TypeGB28181}
	if err := store.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	uni := uniqueid.NewCore(uniqueiddb.NewDB(db).AutoMigrate(true), 5)
	adapter := ipc.NewAdapter(store, uni)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	err := adapter.UpdateContext(ctx, device.DeviceID, func(current *ipc.Device) {
		called = true
		current.Name = "updated"
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("UpdateContext error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("UpdateContext invoked change callback after cancellation")
	}
}

func TestDelDeviceRunsProtocolCleanupBeforeSingleDelete(t *testing.T) {
	db := openIPCTestDatabase(t)
	base := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{ID: "GB_delete", DeviceID: "34020000001320000005", Type: ipc.TypeGB28181}
	channel := &ipc.Channel{
		ID: "GBC_delete", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000006", Type: ipc.TypeGB28181,
	}
	if err := base.Device().Create(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	if err := base.Channel().Create(context.Background(), channel); err != nil {
		t.Fatal(err)
	}
	trackingDevice := &deleteCountingDeviceStore{DeviceStorer: base.Device()}
	store := &concurrentCreateStore{Storer: base, device: trackingDevice}
	protocol := &deleteOrderProtocol{store: store}
	core := ipc.NewCore(store, uniqueid.Core{}, map[string]ipc.Protocoler{ipc.TypeGB28181: protocol})

	deleted, err := core.DelDevice(context.Background(), device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.ID != device.ID || protocol.calls != 1 || !protocol.sawPersistedDevice {
		t.Fatalf("delete result = device:%+v calls:%d saw_persisted:%v", deleted, protocol.calls, protocol.sawPersistedDevice)
	}
	if trackingDevice.deletes != 1 {
		t.Fatalf("device delete calls = %d, want 1", trackingDevice.deletes)
	}
	if trackingDevice.deleted == nil || trackingDevice.deleted.ID != device.ID || trackingDevice.deleted.DeviceID != device.DeviceID {
		t.Fatalf("device delete input = %+v, want resolved device %+v", trackingDevice.deleted, device)
	}
	var remainingDevice ipc.Device
	if err := base.Device().Get(context.Background(), &remainingDevice, orm.Where("id = ?", device.ID)); !orm.IsErrRecordNotFound(err) {
		t.Fatalf("deleted device lookup = %v", err)
	}
	var remainingChannel ipc.Channel
	if err := base.Channel().Get(context.Background(), &remainingChannel, orm.Where("id = ?", channel.ID)); !orm.IsErrRecordNotFound(err) {
		t.Fatalf("deleted channel lookup = %v", err)
	}
}

func TestDelDeviceHoldsProtocolDeleteLockThroughPersistence(t *testing.T) {
	db := openIPCTestDatabase(t)
	base := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{ID: "GB_delete_locked", DeviceID: "34020000001320000007", Type: ipc.TypeGB28181}
	if err := base.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}

	var locked atomic.Bool
	trackingDevice := &deleteLockObservingDeviceStore{DeviceStorer: base.Device(), locked: &locked}
	store := &concurrentCreateStore{Storer: base, device: trackingDevice}
	protocol := &coordinatedDeleteProtocol{
		deleteOrderProtocol: deleteOrderProtocol{store: store},
		locked:              &locked,
	}
	core := ipc.NewCore(store, uniqueid.Core{}, map[string]ipc.Protocoler{ipc.TypeGB28181: protocol})

	if _, err := core.DelDevice(t.Context(), device.ID); err != nil {
		t.Fatal(err)
	}
	if !protocol.cleanupSawLock.Load() {
		t.Fatal("protocol cleanup ran without the device delete lock")
	}
	if !trackingDevice.deleteSawLock.Load() {
		t.Fatal("persistent device deletion ran after the device delete lock was released")
	}
	if locked.Load() {
		t.Fatal("device delete lock was not released")
	}
}

func TestEditDeviceHoldsProtocolLockThroughPersistenceAndInit(t *testing.T) {
	db := openIPCTestDatabase(t)
	base := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_edit_locked", DeviceID: "34020000001320000008", Type: ipc.TypeGB28181,
		Name: "before", Password: "old-password",
	}
	if err := base.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}

	var locked atomic.Bool
	trackingDevice := &editLockObservingDeviceStore{DeviceStorer: base.Device(), locked: &locked}
	store := &concurrentCreateStore{Storer: base, device: trackingDevice}
	protocol := &coordinatedEditProtocol{deleteOrderProtocol: deleteOrderProtocol{store: store}, locked: &locked}
	core := ipc.NewCore(store, uniqueid.Core{}, map[string]ipc.Protocoler{ipc.TypeGB28181: protocol})
	newPassword := "new-password"

	updated, err := core.EditDevice(t.Context(), &ipc.EditDeviceInput{
		DeviceID: device.DeviceID,
		Name:     "after",
		Password: &newPassword,
	}, device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "after" || !trackingDevice.updateSawLock.Load() ||
		!protocol.validateSawLock.Load() || !protocol.editedSawLock.Load() || !protocol.initSawLock.Load() {
		t.Fatalf("coordinated edit = updated:%+v store:%t validate:%t edited:%t init:%t",
			updated, trackingDevice.updateSawLock.Load(), protocol.validateSawLock.Load(),
			protocol.editedSawLock.Load(), protocol.initSawLock.Load())
	}
	if locked.Load() {
		t.Fatal("device edit lock was not released")
	}
}

func TestUpgradeDevicePreservesPlainStringWhitespace(t *testing.T) {
	db := openIPCTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_upgrade_strings", DeviceID: "34020000001320000021", Type: ipc.TypeGB28181,
	}
	channel := &ipc.Channel{
		ID: "GBC_upgrade_strings", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000022", Type: ipc.TypeGB28181,
	}
	if err := store.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	if err := store.Channel().Create(t.Context(), channel); err != nil {
		t.Fatal(err)
	}
	protocol := &deleteOrderProtocol{}
	core := ipc.NewCore(store, uniqueid.Core{}, map[string]ipc.Protocoler{ipc.TypeGB28181: protocol})

	in := &ipc.UpgradeInput{
		Firmware: " V1.2.4 ", FileURL: " https://example.invalid/fw.bin ", Manufacturer: " Vendor ",
		SessionID: " upgrade-session-000000000001 ", Timeout: 3,
	}
	if _, err := core.UpgradeDevice(t.Context(), channel.ID, in); err != nil {
		t.Fatal(err)
	}
	if protocol.upgradeCalls != 1 || protocol.upgradeInput == nil {
		t.Fatalf("upgrade calls = %d, input = %+v", protocol.upgradeCalls, protocol.upgradeInput)
	}
	if protocol.upgradeInput.Firmware != " V1.2.4 " ||
		protocol.upgradeInput.FileURL != " https://example.invalid/fw.bin " ||
		protocol.upgradeInput.Manufacturer != " Vendor " {
		t.Fatalf("upgrade plain strings were normalized: %+v", protocol.upgradeInput)
	}
	if protocol.upgradeInput.SessionID != "upgrade-session-000000000001" || protocol.upgradeInput.ChannelID != channel.ChannelID {
		t.Fatalf("upgrade structured fields = %+v", protocol.upgradeInput)
	}
	if _, err := core.UpgradeDevice(t.Context(), channel.ID, &ipc.UpgradeInput{
		Firmware: " \t ", FileURL: "https://example.invalid/fw.bin", Manufacturer: "Vendor",
	}); err == nil {
		t.Fatal("whitespace-only firmware was accepted")
	}
	if protocol.upgradeCalls != 1 {
		t.Fatalf("invalid upgrade reached protocol %d times", protocol.upgradeCalls)
	}
}

func TestEditDeviceFinalizerUsesLockedPersistedBeforeState(t *testing.T) {
	db := openIPCTestDatabase(t)
	base := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_edit_before_state", DeviceID: "34020000001320000018", Type: ipc.TypeGB28181,
		Name: "camera", Password: "old-password",
	}
	if err := base.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}

	stale := &staleResolveDeviceStore{DeviceStorer: base.Device(), currentPassword: "current-password"}
	store := &concurrentCreateStore{Storer: base, device: stale}
	var locked atomic.Bool
	protocol := &coordinatedEditProtocol{locked: &locked}
	core := ipc.NewCore(store, uniqueid.Core{}, map[string]ipc.Protocoler{ipc.TypeGB28181: protocol})
	password := "old-password"

	if _, err := core.EditDevice(t.Context(), &ipc.EditDeviceInput{
		DeviceID: device.DeviceID,
		Name:     device.Name,
		Password: &password,
	}, device.ID); err != nil {
		t.Fatal(err)
	}
	if protocol.beforePassword != "current-password" || protocol.afterPassword != "old-password" {
		t.Fatalf("edit finalizer states = before:%q after:%q", protocol.beforePassword, protocol.afterPassword)
	}
}

func TestEditGBDeviceRejectsDeviceIDChangeWithoutPersistence(t *testing.T) {
	db := openIPCTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_identity", DeviceID: "34020000001320000008", Type: ipc.TypeGB28181,
		Name: "camera", Password: "secret",
	}
	channel := &ipc.Channel{
		ID: "GBC_identity", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000009", Type: ipc.TypeGB28181,
	}
	if err := store.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	if err := store.Channel().Create(t.Context(), channel); err != nil {
		t.Fatal(err)
	}
	core := ipc.NewCore(store, uniqueid.Core{}, map[string]ipc.Protocoler{})
	password := device.Password

	_, err := core.EditDevice(t.Context(), &ipc.EditDeviceInput{
		DeviceID: "34020000001320000010",
		Name:     device.Name,
		Password: &password,
	}, device.ID)
	if err == nil {
		t.Fatal("GB28181 device identity change was accepted")
	}

	var storedDevice ipc.Device
	if err := store.Device().Get(t.Context(), &storedDevice, orm.Where("id = ?", device.ID)); err != nil {
		t.Fatal(err)
	}
	var storedChannel ipc.Channel
	if err := store.Channel().Get(t.Context(), &storedChannel, orm.Where("id = ?", channel.ID)); err != nil {
		t.Fatal(err)
	}
	if storedDevice.DeviceID != device.DeviceID || storedChannel.DeviceID != device.DeviceID {
		t.Fatalf("rejected identity change persisted split state = device:%s channel:%s", storedDevice.DeviceID, storedChannel.DeviceID)
	}
}

func TestGBDeviceQueryDelegatesLegacyRecordInfoWithoutTimeRange(t *testing.T) {
	db := openIPCTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_record_query", DeviceID: "34020000001320000001", Type: ipc.TypeGB28181,
		Ext: ipc.DeviceExt{GBVersion: "1.1", GBEffectiveVersion: "1.1"},
	}
	if err := store.Device().Create(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	protocol := &deleteOrderProtocol{}
	core := ipc.NewCore(store, uniqueid.Core{}, map[string]ipc.Protocoler{ipc.TypeGB28181: protocol})

	out, err := core.GBDeviceQuery(t.Context(), device.ID, &ipc.GBDeviceQueryInput{
		Action: "record_info", TargetID: "34020000001320000002",
	})
	if err != nil {
		t.Fatal(err)
	}
	if protocol.queryCalls != 1 || protocol.queryInput == nil {
		t.Fatalf("DeviceQuery calls = %d input = %+v", protocol.queryCalls, protocol.queryInput)
	}
	if protocol.queryInput.Start != 0 || protocol.queryInput.End != 0 || protocol.queryInput.Timeout != 6 {
		t.Fatalf("delegated RecordInfo input = %+v", protocol.queryInput)
	}
	if out == nil || out.CmdType != "RecordInfo" {
		t.Fatalf("DeviceQuery output = %+v", out)
	}

	out, err = core.GBDeviceQuery(t.Context(), device.ID, &ipc.GBDeviceQueryInput{Action: "record_info"})
	if err != nil {
		t.Fatalf("device-target RecordInfo query failed: %v", err)
	}
	if protocol.queryCalls != 2 || protocol.queryInput == nil || protocol.queryInput.TargetID != device.DeviceID {
		t.Fatalf("device-target RecordInfo delegation = calls:%d input:%+v", protocol.queryCalls, protocol.queryInput)
	}
	if out == nil || out.CmdType != "RecordInfo" {
		t.Fatalf("device-target DeviceQuery output = %+v", out)
	}
}

func TestQueryRecordsDelegatesLegacyRequestWithoutTimeRange(t *testing.T) {
	db := openIPCTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_record_api", DeviceID: "34020000001320000001", Type: ipc.TypeGB28181,
		Ext: ipc.DeviceExt{GBVersion: "1.1", GBEffectiveVersion: "1.1"},
	}
	channel := &ipc.Channel{
		ID: "GBC_record_api", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000002", Type: ipc.TypeGB28181,
	}
	if err := store.Device().Create(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	if err := store.Channel().Create(context.Background(), channel); err != nil {
		t.Fatal(err)
	}
	protocol := &deleteOrderProtocol{}
	core := ipc.NewCore(store, uniqueid.Core{}, map[string]ipc.Protocoler{ipc.TypeGB28181: protocol})

	out, err := core.QueryRecords(t.Context(), channel.ID, &ipc.RecordQueryInput{})
	if err != nil {
		t.Fatal(err)
	}
	if protocol.recordCalls != 1 || protocol.recordInput == nil {
		t.Fatalf("QueryRecords calls = %d input = %+v", protocol.recordCalls, protocol.recordInput)
	}
	if protocol.recordInput.StartAt != 0 || protocol.recordInput.EndAt != 0 || protocol.recordInput.Timeout != 10 {
		t.Fatalf("delegated RecordInfo input = %+v", protocol.recordInput)
	}
	if out == nil || out.TimeNum != 1 {
		t.Fatalf("QueryRecords output = %+v", out)
	}
}

type concurrentCreateStore struct {
	ipc.Storer
	device ipc.DeviceStorer
}

func (s *concurrentCreateStore) Device() ipc.DeviceStorer { return s.device }

type failCommitStore struct {
	ipc.Storer
	channel ipc.ChannelStorer
}

func (s *failCommitStore) Channel() ipc.ChannelStorer { return s.channel }

type failCommitChannelStore struct {
	ipc.ChannelStorer
	err error
}

func (s *failCommitChannelStore) Session(ctx context.Context, changeFns ...func(*gorm.DB) error) error {
	return s.ChannelStorer.Session(ctx, append(changeFns, func(*gorm.DB) error { return s.err })...)
}

type concurrentCreateDeviceStore struct {
	ipc.DeviceStorer
	getCalls    atomic.Int32
	createCalls atomic.Int32
}

type deleteCountingDeviceStore struct {
	ipc.DeviceStorer
	deletes int
	deleted *ipc.Device
}

type deleteLockObservingDeviceStore struct {
	ipc.DeviceStorer
	locked        *atomic.Bool
	deleteSawLock atomic.Bool
}

type editLockObservingDeviceStore struct {
	ipc.DeviceStorer
	locked        *atomic.Bool
	updateSawLock atomic.Bool
}

type staleResolveDeviceStore struct {
	ipc.DeviceStorer
	currentPassword string
	firstGet        atomic.Bool
}

func (s *staleResolveDeviceStore) Get(ctx context.Context, device *ipc.Device, opts ...orm.QueryOption) error {
	if err := s.DeviceStorer.Get(ctx, device, opts...); err != nil {
		return err
	}
	if s.firstGet.CompareAndSwap(false, true) {
		stale := *device
		var current ipc.Device
		if err := s.DeviceStorer.Update(ctx, &current, func(value *ipc.Device) error {
			value.Password = s.currentPassword
			return nil
		}, opts...); err != nil {
			return err
		}
		*device = stale
	}
	return nil
}

func (s *editLockObservingDeviceStore) Update(ctx context.Context, device *ipc.Device, changeFn func(*ipc.Device) error, opts ...orm.QueryOption) error {
	s.updateSawLock.Store(s.locked != nil && s.locked.Load())
	return s.DeviceStorer.Update(ctx, device, changeFn, opts...)
}

func (s *deleteLockObservingDeviceStore) Delete(ctx context.Context, device *ipc.Device, opts ...orm.QueryOption) error {
	s.deleteSawLock.Store(s.locked != nil && s.locked.Load())
	return s.DeviceStorer.Delete(ctx, device, opts...)
}

func (s *deleteCountingDeviceStore) Delete(ctx context.Context, device *ipc.Device, opts ...orm.QueryOption) error {
	s.deletes++
	if device != nil {
		copy := *device
		s.deleted = &copy
	}
	return s.DeviceStorer.Delete(ctx, device, opts...)
}

type deleteOrderProtocol struct {
	store              ipc.Storer
	calls              int
	sawPersistedDevice bool
	queryCalls         int
	queryInput         *ipc.GBDeviceQueryInput
	recordCalls        int
	recordInput        *ipc.RecordQueryInput
	upgradeCalls       int
	upgradeInput       *ipc.UpgradeInput
}

type coordinatedDeleteProtocol struct {
	deleteOrderProtocol
	locked         *atomic.Bool
	cleanupSawLock atomic.Bool
}

type coordinatedEditProtocol struct {
	deleteOrderProtocol
	locked          *atomic.Bool
	validateSawLock atomic.Bool
	editedSawLock   atomic.Bool
	initSawLock     atomic.Bool
	beforePassword  string
	afterPassword   string
}

func (p *coordinatedEditProtocol) LockDeviceEdit(*ipc.Device) func() {
	p.locked.Store(true)
	return func() { p.locked.Store(false) }
}

func (p *coordinatedEditProtocol) ValidateDevice(context.Context, *ipc.Device) error {
	p.validateSawLock.Store(p.locked != nil && p.locked.Load())
	return nil
}

func (p *coordinatedEditProtocol) DeviceEdited(_ context.Context, before, after *ipc.Device) error {
	p.editedSawLock.Store(p.locked != nil && p.locked.Load())
	if before != nil {
		p.beforePassword = before.Password
	}
	if after != nil {
		p.afterPassword = after.Password
	}
	return nil
}

func (p *coordinatedEditProtocol) InitDevice(context.Context, *ipc.Device) error {
	p.initSawLock.Store(p.locked != nil && p.locked.Load())
	return nil
}

func (p *coordinatedDeleteProtocol) LockDeviceDelete(*ipc.Device) func() {
	p.locked.Store(true)
	return func() { p.locked.Store(false) }
}

func (p *coordinatedDeleteProtocol) DeleteDevice(ctx context.Context, device *ipc.Device) error {
	p.cleanupSawLock.Store(p.locked != nil && p.locked.Load())
	return p.deleteOrderProtocol.DeleteDevice(ctx, device)
}

func (p *deleteOrderProtocol) DeleteDevice(ctx context.Context, device *ipc.Device) error {
	p.calls++
	var persisted ipc.Device
	if err := p.store.Device().Get(ctx, &persisted, orm.Where("id = ?", device.ID)); err == nil {
		p.sawPersistedDevice = true
	}
	return nil
}

func (*deleteOrderProtocol) ValidateDevice(context.Context, *ipc.Device) error { return nil }
func (*deleteOrderProtocol) InitDevice(context.Context, *ipc.Device) error     { return nil }
func (*deleteOrderProtocol) QueryCatalog(context.Context, *ipc.Device) error   { return nil }
func (*deleteOrderProtocol) StartPlay(context.Context, *ipc.Device, *ipc.Channel) (*ipc.PlayResponse, error) {
	return nil, nil
}

func (p *deleteOrderProtocol) Upgrade(_ context.Context, _ *ipc.Device, _ *ipc.Channel, in *ipc.UpgradeInput) (*ipc.UpgradeOutput, error) {
	p.upgradeCalls++
	if in != nil {
		copy := *in
		p.upgradeInput = &copy
	}
	return &ipc.UpgradeOutput{ChannelID: in.ChannelID, SessionID: in.SessionID, Result: "OK"}, nil
}
func (*deleteOrderProtocol) StopPlay(context.Context, *ipc.Device, *ipc.Channel) error {
	return nil
}
func (*deleteOrderProtocol) OnStreamNotFound(context.Context, string, string) error { return nil }
func (*deleteOrderProtocol) OnStreamChanged(context.Context, string, string) error  { return nil }

func (p *deleteOrderProtocol) DeviceQuery(_ context.Context, _ *ipc.Device, in *ipc.GBDeviceQueryInput) (*ipc.GBDeviceQueryOutput, error) {
	p.queryCalls++
	copy := *in
	p.queryInput = &copy
	return &ipc.GBDeviceQueryOutput{CmdType: "RecordInfo"}, nil
}

func (p *deleteOrderProtocol) QueryRecords(_ context.Context, _ *ipc.Device, _ *ipc.Channel, in *ipc.RecordQueryInput) (*ipc.RecordQueryOutput, error) {
	p.recordCalls++
	copy := *in
	p.recordInput = &copy
	return &ipc.RecordQueryOutput{TimeNum: 1}, nil
}

func (s *concurrentCreateDeviceStore) Get(ctx context.Context, device *ipc.Device, opts ...orm.QueryOption) error {
	if s.getCalls.Add(1) == 1 {
		return orm.ErrRecordNotFound
	}
	return s.DeviceStorer.Get(ctx, device, opts...)
}

func (s *concurrentCreateDeviceStore) Create(context.Context, *ipc.Device) error {
	s.createCalls.Add(1)
	return orm.ErrDuplicatedKey
}
