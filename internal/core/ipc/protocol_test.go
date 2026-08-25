package ipc_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/domain/uniqueid/store/uniqueiddb"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

func TestAdapterGetDeviceByDeviceIDRecoversConcurrentCreate(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
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

func TestDelDeviceRunsProtocolCleanupBeforeSingleDelete(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
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
	var remainingDevice ipc.Device
	if err := base.Device().Get(context.Background(), &remainingDevice, orm.Where("id = ?", device.ID)); !orm.IsErrRecordNotFound(err) {
		t.Fatalf("deleted device lookup = %v", err)
	}
	var remainingChannel ipc.Channel
	if err := base.Channel().Get(context.Background(), &remainingChannel, orm.Where("id = ?", channel.ID)); !orm.IsErrRecordNotFound(err) {
		t.Fatalf("deleted channel lookup = %v", err)
	}
}

type concurrentCreateStore struct {
	ipc.Storer
	device ipc.DeviceStorer
}

func (s *concurrentCreateStore) Device() ipc.DeviceStorer { return s.device }

type concurrentCreateDeviceStore struct {
	ipc.DeviceStorer
	getCalls    atomic.Int32
	createCalls atomic.Int32
}

type deleteCountingDeviceStore struct {
	ipc.DeviceStorer
	deletes int
}

func (s *deleteCountingDeviceStore) Delete(ctx context.Context, device *ipc.Device, opts ...orm.QueryOption) error {
	s.deletes++
	return s.DeviceStorer.Delete(ctx, device, opts...)
}

type deleteOrderProtocol struct {
	store              ipc.Storer
	calls              int
	sawPersistedDevice bool
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
func (*deleteOrderProtocol) StopPlay(context.Context, *ipc.Device, *ipc.Channel) error {
	return nil
}
func (*deleteOrderProtocol) OnStreamNotFound(context.Context, string, string) error { return nil }
func (*deleteOrderProtocol) OnStreamChanged(context.Context, string, string) error  { return nil }

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
