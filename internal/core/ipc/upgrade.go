package ipc

import (
	"context"
	"strings"

	"github.com/ixugo/goddd/pkg/reason"
)

// UpgradeDevice 执行设备软件升级，协议实现需自行处理版本兼容与响应等待。
func (c Core) UpgradeDevice(ctx context.Context, channelID string, in *UpgradeInput) (*UpgradeOutput, error) {
	if in == nil {
		return nil, reason.ErrBadRequest.SetMsg("invalid upgrade request")
	}
	ch, err := c.GetChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	dev, err := c.GetDevice(ctx, ch.DID)
	if err != nil {
		return nil, err
	}
	protocol, ok := c.protocols[dev.GetType()]
	if !ok {
		return nil, reason.ErrBadRequest.SetMsg("unsupported protocol")
	}
	up, ok := protocol.(UpgradeCapable)
	if !ok {
		return nil, reason.ErrBadRequest.SetMsg("protocol does not support upgrade")
	}

	in.ChannelID = ch.ChannelID
	in.SessionID = strings.TrimSpace(in.SessionID)
	if strings.TrimSpace(in.Firmware) == "" || strings.TrimSpace(in.FileURL) == "" || strings.TrimSpace(in.Manufacturer) == "" {
		return nil, reason.ErrBadRequest.SetMsg("firmware/file_url/manufacturer are required")
	}
	if in.Timeout <= 0 {
		in.Timeout = 8
	}
	return up.Upgrade(ctx, dev, ch, in)
}
