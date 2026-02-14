package ipc

import (
	"context"

	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/reason"
)

// GetChannelByDeviceChannelID 通过设备国标ID+通道国标ID查询内部通道记录。
// 主要用于协议上报（如报警）向业务模型映射。
func (c Core) GetChannelByDeviceChannelID(ctx context.Context, deviceID, channelID string) (*Channel, error) {
	var out Channel
	if err := c.store.Channel().Get(ctx, &out, orm.Where("device_id=? AND channel_id=?", deviceID, channelID)); err != nil {
		if orm.IsErrRecordNotFound(err) {
			return nil, reason.ErrNotFound.Withf("channel not found device_id[%s] channel_id[%s]", deviceID, channelID)
		}
		return nil, reason.ErrDB.Withf("GetChannelByDeviceChannelID err[%s]", err.Error())
	}
	return &out, nil
}
