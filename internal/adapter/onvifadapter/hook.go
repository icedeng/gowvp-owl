package onvifadapter

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/pkg/orm"
)

// OnStreamChanged implements ipc.Protocoler.
// ONVIF 协议的 stream 就是 channel.ID，app 固定为 live
func (a *Adapter) OnStreamChanged(ctx context.Context, app, stream string) error {
	var ch ipc.Channel
	if err := a.adapter.Store().Channel().Get(ctx, &ch, orm.Where("id=?", stream)); err != nil {
		return err
	}
	if err := a.adapter.UpdatePlayingByID(ctx, ch.ID, false); err != nil {
		slog.ErrorContext(ctx, "编辑播放状态失败", "err", err)
	}
	return nil
}

func (a *Adapter) OnStreamNotFound(ctx context.Context, app, stream string) error {
	var ch ipc.Channel
	if err := a.adapter.Store().Channel().Get(ctx, &ch, orm.Where("id=?", stream)); err != nil {
		return err
	}

	onvifDev, ok := a.devices.Load(ch.DeviceID)
	if !ok {
		return fmt.Errorf("ONVIF 设备未初始化")
	}

	streamURI, err := a.getStreamURI(ctx, onvifDev, ch.ChannelID)
	if err != nil {
		return err
	}
	svr, err := a.sms.GetMediaServer(ctx, sms.DefaultMediaServerID)
	if err != nil {
		return err
	}

	_, err = a.sms.CreateStreamProxy(svr, sms.AddStreamProxyRequest{
		App:    app,
		Stream: stream,
		URL:    streamURI,
	})
	if err == nil {
		if err := a.adapter.UpdatePlaying(ctx, ch.DeviceID, ch.ChannelID, true); err != nil {
			slog.ErrorContext(ctx, "编辑播放状态失败", "err", err)
		}
	}
	return err
}
