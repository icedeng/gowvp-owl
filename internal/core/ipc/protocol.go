package ipc

import (
	"context"
	"fmt"

	"github.com/gowvp/owl/internal/core/bz"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

// 为协议适配，提供协议会用到的功能
type Adapter struct {
	// deviceStore  DeviceStorer
	// channelStore ChannelStorer
	store Storer
	uni   uniqueid.Core
}

func (g Adapter) DeviceHistory() *DeviceHistoryStore {
	if g.store == nil {
		return nil
	}
	return g.store.DeviceHistory()
}

func GenerateDID(d *Device, uni uniqueid.Core) string {
	if d.IsOnvif() {
		return uni.UniqueID(bz.IDPrefixOnvif)
	}
	return uni.UniqueID(bz.IDPrefixGB)
}

// GenerateChannelID 根据通道类型生成唯一 ID
func GenerateChannelID(c *Channel, uni uniqueid.Core) string {
	switch c.GetType() {
	case TypeOnvif:
		return uni.UniqueID(bz.IDPrefixOnvifChannel)
	case TypeRTMP:
		return uni.UniqueID(bz.IDPrefixRTMP)
	case TypeRTSP:
		return uni.UniqueID(bz.IDPrefixRTSP)
	default:
		if c.IsOnvif() {
			return uni.UniqueID(bz.IDPrefixOnvifChannel)
		}
		return uni.UniqueID(bz.IDPrefixGBChannel)
	}
}

func NewAdapter(store Storer, uni uniqueid.Core) Adapter {
	return Adapter{
		store: store,
		uni:   uni,
	}
}

func (g Adapter) Store() Storer {
	return g.store
}

func (g Adapter) ListDevices(ctx context.Context) ([]*Device, error) {
	var devices []*Device
	if _, err := g.store.Device().List(ctx, &devices, web.NewPagerFilterMaxSize()); err != nil {
		return nil, err
	}
	return devices, nil
}

func (g Adapter) GetDeviceByDeviceID(gbDeviceID string) (*Device, error) {
	return g.GetDeviceByDeviceIDContext(context.Background(), gbDeviceID)
}

// GetDeviceByDeviceIDContext 查询设备，不存在时自动建档，并允许调用方取消数据库操作。
func (g Adapter) GetDeviceByDeviceIDContext(ctx context.Context, gbDeviceID string) (*Device, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var d Device
	if err := g.store.Device().Get(ctx, &d, orm.Where("device_id=?", gbDeviceID)); err != nil {
		if !orm.IsErrRecordNotFound(err) {
			return nil, err
		}
		d.init(g.uni.UniqueID(bz.IDPrefixGB), gbDeviceID)
		if err := g.store.Device().Create(ctx, &d); err != nil {
			if !orm.IsDuplicatedKey(err) {
				return nil, err
			}
			// 多实例或并发首次 REGISTER 可能同时建档；唯一键冲突后读取胜出记录并保持幂等。
			d = Device{}
			if err := g.store.Device().Get(ctx, &d, orm.Where("device_id=?", gbDeviceID)); err != nil {
				return nil, err
			}
		}
	}
	return &d, nil
}

func (g Adapter) Logout(deviceID string, changeFn func(*Device)) error {
	return g.UpdateContext(context.Background(), deviceID, changeFn)
}

func (g Adapter) Edit(deviceID string, changeFn func(*Device)) error {
	return g.UpdateContext(context.Background(), deviceID, changeFn)
}

// UpdateContext 更新设备，并允许调用方取消数据库操作。
func (g Adapter) UpdateContext(ctx context.Context, deviceID string, changeFn func(*Device)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var d Device
	if err := g.store.Device().Update(ctx, &d, func(d *Device) error {
		changeFn(d)
		return nil
	}, orm.Where("device_id=?", deviceID)); err != nil {
		return err
	}

	return nil
}

func (g Adapter) Update(deviceID string, changeFn func(*Device)) error {
	return g.UpdateContext(context.Background(), deviceID, changeFn)
}

func (g Adapter) EditPlayingByID(ctx context.Context, id string, playing bool) error {
	var ch Channel
	if err := g.store.Channel().Update(ctx, &ch, func(c *Channel) error {
		c.IsPlaying = playing
		return nil
	}, orm.Where("id=?", id)); err != nil {
		return err
	}
	return nil
}

func (g Adapter) UpdatePlayingByID(ctx context.Context, id string, playing bool) error {
	return g.EditPlayingByID(ctx, id, playing)
}

func (g Adapter) EditPlaying(ctx context.Context, deviceID, channelID string, playing bool) error {
	var ch Channel
	if err := g.store.Channel().Update(ctx, &ch, func(c *Channel) error {
		c.IsPlaying = playing
		return nil
	}, orm.Where("device_id = ? AND channel_id = ?", deviceID, channelID)); err != nil {
		return err
	}
	return nil
}

func (g Adapter) UpdatePlaying(ctx context.Context, deviceID, channelID string, playing bool) error {
	return g.EditPlaying(ctx, deviceID, channelID, playing)
}

// SaveChannels 保存通道列表（增量更新 + 删除多余通道）
//
// 策略说明：
// 1. 批量查询现有通道（减少数据库查询）
// 2. 对比更新：存在则更新，不存在则新增
// 3. 删除多余：不在上报列表中的通道标记为离线或删除
// 4. 使用事务保证数据一致性
func (g Adapter) SaveChannels(channels []*Channel) error {
	if len(channels) <= 0 {
		return nil
	}
	return g.SaveChannelSnapshot(context.Background(), channels[0].DeviceID, channels)
}

// SaveChannelSnapshot 原子保存设备上报的完整通道快照；空列表表示设备当前没有通道。
func (g Adapter) SaveChannelSnapshot(ctx context.Context, deviceID string, channels []*Channel) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if g.store == nil {
		return fmt.Errorf("save channel snapshot: store is unavailable")
	}
	if deviceID == "" {
		return fmt.Errorf("save channel snapshot: device ID is empty")
	}
	seen := make(map[string]struct{}, len(channels))
	for index, channel := range channels {
		if channel == nil {
			return fmt.Errorf("save channel snapshot: channel %d is nil", index)
		}
		if channel.DeviceID != deviceID {
			return fmt.Errorf("save channel snapshot: channel %q belongs to device %q, want %q", channel.ChannelID, channel.DeviceID, deviceID)
		}
		if channel.ChannelID == "" {
			return fmt.Errorf("save channel snapshot: channel %d ID is empty", index)
		}
		if _, ok := seen[channel.ChannelID]; ok {
			return fmt.Errorf("save channel snapshot: duplicate channel ID %q", channel.ChannelID)
		}
		seen[channel.ChannelID] = struct{}{}
	}

	type stagedChannelID struct {
		channel   *Channel
		id        string
		did       string
		generated bool
	}
	// 唯一 ID 存储使用独立连接；先在事务外为新增通道预留 ID，避免 SQLite 等数据库发生自锁。
	var preexistingChannels []*Channel
	if _, err := g.store.Channel().List(ctx, &preexistingChannels,
		web.NewPagerFilterMaxSize(), orm.Where("device_id = ?", deviceID)); err != nil {
		return fmt.Errorf("prepare channel snapshot for device %q: %w", deviceID, err)
	}
	preexistingMap := make(map[string]struct{}, len(preexistingChannels))
	for _, channel := range preexistingChannels {
		preexistingMap[channel.ChannelID] = struct{}{}
	}
	preparedNew := make(map[string]stagedChannelID, len(channels))
	for _, channel := range channels {
		if _, ok := preexistingMap[channel.ChannelID]; ok {
			continue
		}
		id := channel.ID
		generated := id == ""
		if generated {
			id = GenerateChannelID(channel, g.uni)
			if id == "" || id == "unknown" {
				for _, prepared := range preparedNew {
					if prepared.generated {
						_ = g.uni.UndoUniqueID(prepared.id)
					}
				}
				return fmt.Errorf("generate ID for channel %q", channel.ChannelID)
			}
		}
		preparedNew[channel.ChannelID] = stagedChannelID{
			channel: channel, id: id, generated: generated,
		}
	}
	cleanupGeneratedIDs := func(used map[string]struct{}) {
		for channelID, staged := range preparedNew {
			if !staged.generated {
				continue
			}
			if used != nil {
				if _, ok := used[channelID]; ok {
					continue
				}
			}
			_ = g.uni.UndoUniqueID(staged.id)
		}
	}
	stagedIDs := make([]stagedChannelID, 0, len(channels))
	usedPreparedIDs := make(map[string]struct{}, len(preparedNew))
	err := g.store.Channel().Session(ctx, func(tx *gorm.DB) error {
		var device Device
		if err := tx.WithContext(ctx).
			Where("device_id = ? OR id = ?", deviceID, deviceID).
			First(&device).Error; err != nil {
			return fmt.Errorf("load device %q: %w", deviceID, err)
		}

		var existingChannels []*Channel
		if err := tx.WithContext(ctx).
			Where("device_id = ?", deviceID).
			Find(&existingChannels).Error; err != nil {
			return fmt.Errorf("list channels for device %q: %w", deviceID, err)
		}
		existingMap := make(map[string]*Channel, len(existingChannels))
		for _, channel := range existingChannels {
			existingMap[channel.ChannelID] = channel
		}

		currentChannelIDs := make([]string, 0, len(channels))
		for _, reported := range channels {
			currentChannelIDs = append(currentChannelIDs, reported.ChannelID)
			if existing, ok := existingMap[reported.ChannelID]; ok {
				// 只覆盖设备上报字段，保留用户配置和其他扩展属性。
				// 通道明确离线时已不存在可继续恢复的媒体会话，必须同时清除播放状态；
				// 在线通道继续保留当前播放状态，避免目录刷新中断正常直播。
				existing.Name = reported.Name
				existing.IsOnline = reported.IsOnline
				if !reported.IsOnline {
					existing.IsPlaying = false
				}
				existing.PTZType = reported.PTZType
				existing.Ext.Manufacturer = reported.Ext.Manufacturer
				existing.Ext.Firmware = reported.Ext.Firmware
				existing.Ext.GBVersion = reported.Ext.GBVersion
				existing.Ext.Model = reported.Ext.Model
				existing.Ext.GBCatalog = reported.Ext.GBCatalog
				if err := tx.WithContext(ctx).
					Model(existing).
					Select("name", "is_online", "is_playing", "ptztype", "ext").
					Updates(existing).Error; err != nil {
					return fmt.Errorf("update channel %q: %w", reported.ChannelID, err)
				}
				continue
			}

			prepared, ok := preparedNew[reported.ChannelID]
			if !ok {
				return fmt.Errorf("channel %q disappeared while preparing snapshot", reported.ChannelID)
			}
			created := *reported
			created.ID = prepared.id
			created.DID = device.ID
			if err := tx.WithContext(ctx).Create(&created).Error; err != nil {
				return fmt.Errorf("create channel %q: %w", reported.ChannelID, err)
			}
			usedPreparedIDs[reported.ChannelID] = struct{}{}
			stagedIDs = append(stagedIDs, stagedChannelID{
				channel: reported, id: created.ID, did: created.DID, generated: prepared.generated,
			})
		}

		offline := tx.WithContext(ctx).Model(new(Channel)).Where("device_id = ?", deviceID)
		if len(currentChannelIDs) > 0 {
			offline = offline.Where("channel_id NOT IN ?", currentChannelIDs)
		}
		if err := offline.Updates(map[string]any{"is_online": false, "is_playing": false}).Error; err != nil {
			return fmt.Errorf("mark missing channels offline for device %q: %w", deviceID, err)
		}
		if err := tx.WithContext(ctx).
			Model(new(Device)).
			Where("id = ?", device.ID).
			UpdateColumn("channels", len(channels)).Error; err != nil {
			return fmt.Errorf("update channel count for device %q: %w", deviceID, err)
		}
		return nil
	})
	if err != nil {
		cleanupGeneratedIDs(nil)
		return fmt.Errorf("save channel snapshot for device %q: %w", deviceID, err)
	}
	cleanupGeneratedIDs(usedPreparedIDs)
	for _, staged := range stagedIDs {
		staged.channel.ID = staged.id
		staged.channel.DID = staged.did
	}
	return nil
}

// FindDevices 获取所有设备
func (g Adapter) FindDevices(ctx context.Context) ([]*Device, error) {
	var devices []*Device
	if _, err := g.store.Device().List(ctx, &devices, web.NewPagerFilterMaxSize()); err != nil {
		return nil, err
	}
	return devices, nil
}

func (g Adapter) GetChannel(ctx context.Context, id string) (*Channel, error) {
	var ch Channel
	if err := g.store.Channel().Get(ctx, &ch, orm.Where("id=?", id)); err != nil {
		return nil, err
	}
	return &ch, nil
}

func (g Adapter) GetDevice(ctx context.Context, id string) (*Device, error) {
	var dev Device
	if err := g.store.Device().Get(ctx, &dev, orm.Where("id=?", id)); err != nil {
		return nil, err
	}
	return &dev, nil
}
