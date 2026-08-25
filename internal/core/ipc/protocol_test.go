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
