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
		func(tx *gorm.DB) error {
			return tx.Where("device_id = ?", dev.DeviceID).Delete(new(ipc.DeviceHistoryRecord)).Error
		},
		func(tx *gorm.DB) error {
			if !tx.Migrator().HasTable(new(ipc.GBTaskStateRecord)) {
				return nil
			}
			return tx.Where("device_id = ?", dev.DeviceID).Delete(new(ipc.GBTaskStateRecord)).Error
		},
		func(tx *gorm.DB) error {
			if !tx.Migrator().HasTable(new(ipc.GBCascadeTaskRouteRecord)) {
				return nil
			}
			return tx.Where("downstream_device_id = ?", dev.DeviceID).Delete(new(ipc.GBCascadeTaskRouteRecord)).Error
		},
	); err != nil {
		return err
	}

	d.devices.Delete(dev.GetGB28181DeviceID())
	return nil
}

// Update implements ipc.DeviceStorer.
func (d *Device) Update(ctx context.Context, dev *ipc.Device, changeFn func(*ipc.Device) error, opts ...orm.QueryOption) error {
	before := *dev
	if err := d.Storer.Device().Get(ctx, &before, opts...); err != nil {
		return err
	}
	runtime, hasRuntime := d.devices.Load(before.GetGB28181DeviceID())

	update := func() error {
		passwordChanged := false
		if err := d.Storer.Device().Session(
			ctx,
			func(tx *gorm.DB) error {
				db := tx.Clauses(clause.Locking{Strength: "UPDATE"})
				for _, opt := range opts {
					db = opt(db)
				}
				if err := db.First(dev).Error; err != nil {
					return err
				}
				previousPassword := dev.Password
				if err := changeFn(dev); err != nil {
					return err
				}
				passwordChanged = dev.IsGB28181() && dev.Password != previousPassword
				if passwordChanged {
					dev.IsOnline = false
					closed := true
					dev.Ext.GBRegistrationClosed = &closed
				}
				return tx.Save(dev).Error
			},
			func(tx *gorm.DB) error {
				if !passwordChanged {
					return nil
				}
				return tx.Model(new(ipc.Channel)).Where("did = ?", dev.ID).UpdateColumn("is_online", false).Error
			},
		); err != nil {
			return err
		}

		if passwordChanged && hasRuntime {
			runtime.UpdateRuntime(func(current *gbs.Device) {
				current.IsOnline = dev.IsOnline
				current.LastKeepaliveAt = dev.KeepaliveAt.Time
				current.LastRegisterAt = dev.RegisteredAt.Time
				current.Expires = dev.Expires
				current.Password = dev.Password
				current.Address = dev.Address
				gbs.SyncRegistrationBindingRuntime(current, dev)
			})
			slog.InfoContext(ctx, "修改密码，设备离线")
		}
		return nil
	}

	if hasRuntime {
		return runtime.SerializeRegistrationState(update)
	}
	return update()
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
