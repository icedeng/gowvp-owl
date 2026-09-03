package ipccache

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	_ gbs.MemoryStorer = &Cache{}
	_ ipc.Storer       = &Cache{}
)

type Cache struct {
	ipc.Storer

	devices *conc.Map[string, *gbs.Device]
}

func (c *Cache) DeviceHistory() *ipc.DeviceHistoryStore {
	return c.Storer.DeviceHistory()
}

// LoadOrStore implements gbs.MemoryStorer.
func (c *Cache) LoadOrStore(deviceID string, value *gbs.Device) {
	c.devices.LoadOrStore(deviceID, value)
}

func (c *Cache) Device() ipc.DeviceStorer {
	return (*Device)(c)
}

func (c *Cache) Channel() ipc.ChannelStorer {
	return (*Channel)(c)
}

func NewCache(store ipc.Storer) *Cache {
	return &Cache{
		Storer:  store,
		devices: &conc.Map[string, *gbs.Device]{},
	}
}

// LoadDeviceToMemory implements gbs.MemoryStorer.
func (c *Cache) LoadDeviceToMemory(conn sip.Connection) error {
	return c.LoadDeviceToMemoryContext(context.Background(), conn)
}

// LoadDeviceToMemoryContext 恢复设备与通道，并允许应用启动取消数据库操作。
func (c *Cache) LoadDeviceToMemoryContext(ctx context.Context, conn sip.Connection) error {
	if ctx == nil {
		ctx = context.Background()
	}
	devices := make([]*ipc.Device, 0, 100)
	_, err := c.Storer.Device().List(ctx, &devices, web.NewPagerFilterMaxSize(), orm.Where("type != ?", ipc.TypeOnvif))
	if err != nil {
		return fmt.Errorf("list GB28181 devices: %w", err)
	}

	for _, d := range devices {
		if d == nil {
			continue
		}
		// 空类型是历史 GB28181 记录；显式 RTSP/RTMP 等设备不得进入国标运行态，
		// 更不能因其传输字段为 TCP/TLS 而被国标恢复流程错误置离线。
		if deviceType := strings.TrimSpace(d.Type); deviceType != "" && !strings.EqualFold(deviceType, ipc.TypeGB28181) {
			continue
		}
		// 媒体会话不跨进程恢复；无论 UDP/TCP/TLS，启动时都不能保留旧进程的播放中标记。
		if err := c.Storer.Channel().BatchEdit(ctx, "is_playing", false, orm.Where("device_id = ?", d.GetGB28181DeviceID())); err != nil {
			return fmt.Errorf("clear restored GB28181 channel playing state for %s: %w", d.GetGB28181DeviceID(), err)
		}
		if transport := strings.ToLower(d.Transport); transport == "tcp" || transport == "tls" {
			// 通知相关设备/通道离线
			if err := c.ChangeContext(ctx, d.GetGB28181DeviceID(), func(d *ipc.Device) error {
				d.IsOnline = false
				closed := true
				d.Ext.GBRegistrationClosed = &closed
				return nil
			}, func(d *gbs.Device) {
				d.IsOnline = false
			}); err != nil {
				return fmt.Errorf("mark restored TCP device %s offline: %w", d.GetGB28181DeviceID(), err)
			}
			continue
		}

		dev := gbs.NewDevice(conn, d)
		if dev != nil {
			if err := dev.CheckConnection(); err != nil {
				slog.Warn("检查设备连接失败", "err", err, "username", d.GetGB28181DeviceID(), "to", dev.To())
				continue
			}

			slog.Debug("load device to memory", "username", d.GetGB28181DeviceID(), "to", dev.To())
			if err := c.LoadDeviceChannelsContext(ctx, d.GetGB28181DeviceID(), dev); err != nil {
				return err
			}
			c.devices.Store(d.GetGB28181DeviceID(), dev)
		}
	}
	return nil
}

// LoadDeviceChannelsContext 将持久化通道装载到尚未发布的设备运行态。
// 设备重新 REGISTER 时复用该能力，消除等待 Catalog 查询期间的播放空窗。
func (c *Cache) LoadDeviceChannelsContext(ctx context.Context, deviceID string, device *gbs.Device) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if device == nil {
		return fmt.Errorf("load GB28181 channels for %s: runtime device is nil", deviceID)
	}
	channels := make([]*ipc.Channel, 0, 8)
	if _, err := c.Storer.Channel().List(ctx, &channels, web.NewPagerFilterMaxSize(), orm.Where("device_id=?", deviceID)); err != nil {
		return fmt.Errorf("list GB28181 channels for %s: %w", deviceID, err)
	}
	device.LoadChannels(channels...)
	return nil
}

// RangeDevices implements gbs.MemoryStorer.
func (c *Cache) RangeDevices(fn func(key string, value *gbs.Device) bool) {
	c.devices.Range(fn)
}

// Change implements gbs.MemoryStorer.
func (c *Cache) Change(deviceID string, changeFn func(*ipc.Device) error, changeFn2 func(*gbs.Device)) error {
	return c.ChangeContext(context.Background(), deviceID, changeFn, changeFn2)
}

// ChangeContext 提交持久态和运行态，并允许服务关闭取消数据库事务。
func (c *Cache) ChangeContext(ctx context.Context, deviceID string, changeFn func(*ipc.Device) error, changeFn2 func(*gbs.Device)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runtime, hasRuntime := c.devices.Load(deviceID)
	change := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var dev ipc.Device
		if err := c.Storer.Device().Session(
			ctx,
			func(tx *gorm.DB) error {
				db := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("device_id = ?", deviceID)
				if err := db.First(&dev).Error; err != nil {
					return err
				}
				if err := changeFn(&dev); err != nil {
					return err
				}
				return tx.Save(&dev).Error
			},
			func(tx *gorm.DB) error {
				if dev.IsOnline {
					return nil
				}
				updates := map[string]any{"is_online": false}
				// REGISTER 注销/超时关闭了媒体恢复所依赖的绑定，清除播放状态；
				// 有效绑定内的 DeviceStatus OFFLINE 仍保留既有播放语义。
				if dev.Ext.GBRegistrationClosed != nil && *dev.Ext.GBRegistrationClosed {
					updates["is_playing"] = false
				}
				if err := tx.Model(new(ipc.Channel)).Where("did = ?", dev.ID).Updates(updates).Error; err != nil {
					return fmt.Errorf("mark channels offline: %w", err)
				}
				return nil
			},
		); err != nil {
			return err
		}

		if !hasRuntime {
			return nil
		}
		runtime.UpdateRuntime(func(current *gbs.Device) {
			current.IsOnline = dev.IsOnline
			current.LastKeepaliveAt = dev.KeepaliveAt.Time
			current.LastRegisterAt = dev.RegisteredAt.Time
			current.Expires = dev.Expires
			current.Password = dev.Password
			current.Address = dev.Address
			gbs.SyncRegistrationBindingRuntime(current, &dev)
			if changeFn2 != nil {
				changeFn2(current)
			}
		})
		return nil
	}

	if hasRuntime {
		return runtime.SerializeRegistrationState(change)
	}
	return change()
}

// GetChannel implements gbs.MemoryStorer.
func (c *Cache) GetChannel(deviceID string, channelID string) (*gbs.Channel, bool) {
	dev, ok := c.devices.Load(deviceID)
	if !ok {
		return nil, false
	}
	return dev.GetChannel(channelID)
}

// Load implements gbs.MemoryStorer.
func (c *Cache) Load(deviceID string) (*gbs.Device, bool) {
	return c.devices.Load(deviceID)
}

// Store implements gbs.MemoryStorer.
func (c *Cache) Store(deviceID string, value *gbs.Device) {
	c.devices.Store(deviceID, value)
}
