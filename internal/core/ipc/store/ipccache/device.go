package ipccache

import (
	"context"
	"log/slog"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var _ ipc.DeviceStorer = &Device{}

type Device = Cache

// Create implements ipc.DeviceStorer.
func (d *Device) Create(ctx context.Context, dev *ipc.Device) error {
	if err := d.Storer.Device().Create(ctx, dev); err != nil {
		return err
	}
	d.devices.LoadOrStore(dev.GetGB28181DeviceID(), gbs.NewDevice(nil, dev))
	return nil
}

// Delete implements ipc.DeviceStorer.
func (d *Device) Delete(ctx context.Context, dev *ipc.Device, opts ...orm.QueryOption) error {
	if err := d.Storer.Device().Session(
		ctx,
		func(tx *gorm.DB) error {
			db := tx.Clauses(clause.Returning{})
			for _, fn := range opts {
				db = fn(db)
			}
			return db.Delete(dev).Error
		},
		func(tx *gorm.DB) error {
			return tx.Model(&ipc.Channel{}).Where("did=?", dev.ID).Delete(nil).Error
		},
	); err != nil {
		return err
	}

	d.devices.Delete(dev.GetGB28181DeviceID())
	return nil
}

// Update implements ipc.DeviceStorer.
func (d *Device) Update(ctx context.Context, dev *ipc.Device, changeFn func(*ipc.Device) error, opts ...orm.QueryOption) error {
	if err := d.Storer.Device().Update(ctx, dev, changeFn, opts...); err != nil {
		return err
	}
	dev2, ok := d.devices.Load(dev.GetGB28181DeviceID())
	// TODO: 待重构
	if dev.IsGB28181() && ok {
		// 密码修改，设备需要重新注册
		if dev2.Password != dev.Password && dev.Password != "" {
			slog.InfoContext(ctx, " 修改密码，设备离线")
			d.Change(dev.GetGB28181DeviceID(), func(d *ipc.Device) error {
				d.Password = dev.Password
				d.IsOnline = false
				return nil
			}, func(d *gbs.Device) {
			})
		}
	}

	return nil
}

// List implements ipc.DeviceStorer.
func (d *Device) List(ctx context.Context, devs *[]*ipc.Device, pager orm.Pager, opts ...orm.QueryOption) (int64, error) {
	return d.Storer.Device().List(ctx, devs, pager, opts...)
}

// Get implements ipc.DeviceStorer.
func (d *Device) Get(ctx context.Context, dev *ipc.Device, opts ...orm.QueryOption) error {
	return d.Storer.Device().Get(ctx, dev, opts...)
}

// Session implements ipc.DeviceStorer.
func (d *Device) Session(ctx context.Context, changeFns ...func(*gorm.DB) error) error {
	return d.Storer.Device().Session(ctx, changeFns...)
}
