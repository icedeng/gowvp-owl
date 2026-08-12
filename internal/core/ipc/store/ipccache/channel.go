package ipccache

import (
	"context"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

var _ ipc.ChannelStorer = &Channel{}

type Channel Cache

// Session implements ipc.ChannelStorer.
func (c *Channel) Session(ctx context.Context, changeFns ...func(*gorm.DB) error) error {
	return c.Storer.Channel().Session(ctx, changeFns...)
}

// Create implements ipc.ChannelStorer.
func (c *Channel) Create(ctx context.Context, ch *ipc.Channel) error {
	if err := c.Storer.Channel().Create(ctx, ch); err != nil {
		return err
	}
	dev, ok := c.devices.Load(ch.DeviceID)
	if ok {
		dev.LoadChannels(ch)
	}
	return nil
}

// BatchEdit implements ipc.ChannelStorer.
func (c *Channel) BatchEdit(ctx context.Context, field string, value any, opts ...orm.QueryOption) error {
	return c.Storer.Channel().BatchEdit(ctx, field, value, opts...)
}

// Delete implements ipc.ChannelStorer.
func (c *Channel) Delete(ctx context.Context, ch *ipc.Channel, opts ...orm.QueryOption) error {
	return c.Storer.Channel().Delete(ctx, ch, opts...)
}

// Update implements ipc.ChannelStorer.
func (c *Channel) Update(ctx context.Context, ch *ipc.Channel, changeFn func(*ipc.Channel) error, opts ...orm.QueryOption) error {
	return c.Storer.Channel().Update(ctx, ch, changeFn, opts...)
}

// EditGB28181Config implements ipc.ChannelStorer.
func (c *Channel) EditGB28181Config(ctx context.Context, ch *ipc.Channel) error {
	return c.Storer.Channel().EditGB28181Config(ctx, ch)
}

// List implements ipc.ChannelStorer.
func (c *Channel) List(ctx context.Context, chs *[]*ipc.Channel, pager orm.Pager, opts ...orm.QueryOption) (int64, error) {
	return c.Storer.Channel().List(ctx, chs, pager, opts...)
}

// Get implements ipc.ChannelStorer.
func (c *Channel) Get(ctx context.Context, ch *ipc.Channel, opts ...orm.QueryOption) error {
	return c.Storer.Channel().Get(ctx, ch, opts...)
}
