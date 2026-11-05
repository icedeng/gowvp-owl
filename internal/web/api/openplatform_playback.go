package api

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	op "github.com/gowvp/owl/internal/core/openplatform"
	"github.com/gowvp/owl/pkg/gbs"
	"github.com/gowvp/owl/pkg/zlm"
)

var _ op.PlaybackHost = openHost{}

func (h openHost) PreparePlayback(ctx context.Context, row op.Playback) (op.Source, error) {
	ch, err := h.uc.GB28181API.ipc.GetChannel(ctx, row.ChannelID)
	if err != nil {
		return op.Source{}, op.ErrNotFound
	}
	ms, err := h.uc.SMSAPI.smsCore.GetMediaServer(ctx, channelMediaServerID(ch))
	if err != nil {
		return op.Source{}, op.ErrUnavailable
	}
	if ms.Disabled {
		return op.Source{}, op.ErrUnavailable
	}
	if ms.Type != "" && !strings.EqualFold(ms.Type, "zlm") {
		return op.Source{}, op.Fail(400, "UNSUPPORTED_MEDIA_SERVER", "设备回放支持ZLM节点")
	}
	return op.Source{MediaServerID: ms.ID, App: "rtp", Stream: gbs.ApplicationPlaybackStreamID(row.ID), BaseURL: "http://" + net.JoinHostPort(ms.IP, strconv.Itoa(ms.Ports.HTTP))}, nil
}

func playbackChannel(row op.Playback) *ipc.Channel {
	return &ipc.Channel{ID: row.ChannelID, DeviceID: row.DeviceCode, ChannelID: row.ChannelCode}
}

func (h openHost) StartPlayback(ctx context.Context, row op.Playback) error {
	svr, err := h.uc.GB28181API.gbServer()
	if err != nil {
		return err
	}
	// 启动前核对原通道归属；停止与控制只使用创建时的编码，避免通道重建后串到新设备。
	ch, err := h.uc.GB28181API.ipc.GetChannel(ctx, row.ChannelID)
	if err != nil {
		return err
	}
	dev, err := h.uc.GB28181API.ipc.GetDevice(ctx, ch.DID)
	if err != nil {
		return err
	}
	if dev.DeviceID != row.DeviceCode || ch.ChannelID != row.ChannelCode {
		return op.ErrConflict
	}
	ms, err := h.uc.SMSAPI.smsCore.GetMediaServer(ctx, row.Source.MediaServerID)
	if err != nil {
		return err
	}
	if err = svr.StartApplicationPlayback(ctx, row.ID, gbs.HistoryInput{Channel: playbackChannel(row), SMS: ms, StreamMode: dev.StreamMode, StartAt: time.Unix(row.StartAt, 0), EndAt: time.Unix(row.EndAt, 0), RecordType: &row.RecordType}); err != nil {
		return err
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			info, err := h.uc.SMSAPI.smsCore.GetMediaInfoContext(ctx, ms, row.Source.App, row.Source.Stream)
			if err == nil && len(info) > 0 {
				return nil
			}
		}
	}
}

func (h openHost) StopPlayback(ctx context.Context, row op.Playback) error {
	svr, sipErr := h.uc.GB28181API.gbServer()
	if sipErr == nil {
		sipErr = svr.StopApplicationPlayback(ctx, row.ID, gbs.StopHistoryInput{Channel: playbackChannel(row)})
	}
	// 进程重启后没有SIP对话，仍按原媒体节点关闭本会话唯一的RTP接收端。
	ms, err := h.uc.SMSAPI.smsCore.GetMediaServer(ctx, row.Source.MediaServerID)
	if err != nil {
		return errors.Join(sipErr, err)
	}
	_, err = h.uc.SMSAPI.smsCore.CloseRTPServerContext(ctx, ms, zlm.CloseRTPServerRequest{StreamID: gbs.ApplicationPlaybackStreamID(row.ID)})
	return errors.Join(sipErr, err)
}

func (h openHost) ControlPlayback(ctx context.Context, row op.Playback, in op.PlaybackControl) error {
	svr, err := h.uc.GB28181API.gbServer()
	if err != nil {
		return err
	}
	return svr.ControlApplicationPlayback(ctx, row.ID, gbs.ControlHistoryInput{Channel: playbackChannel(row), Action: in.Action, Scale: in.Scale, SeekAt: in.SeekAt})
}

func (h openHost) PlaybackActive(row op.Playback) bool {
	svr, err := h.uc.GB28181API.gbServer()
	return err == nil && svr.ApplicationPlaybackActive(row.ID)
}
