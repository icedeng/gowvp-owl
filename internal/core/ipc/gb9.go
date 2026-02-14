package ipc

import (
	"context"
	"strings"

	"github.com/ixugo/goddd/pkg/reason"
)

// StartHistory 启动历史会话（回放/下载）。
func (c Core) StartHistory(ctx context.Context, channelID string, in *HistoryControlInput) error {
	if in == nil {
		return reason.ErrBadRequest.SetMsg("invalid history request")
	}
	ch, err := c.GetChannel(ctx, channelID)
	if err != nil {
		return err
	}
	dev, err := c.GetDevice(ctx, ch.DID)
	if err != nil {
		return err
	}
	p, ok := c.protocols[dev.GetType()]
	if !ok {
		return reason.ErrBadRequest.SetMsg("unsupported protocol")
	}
	h, ok := p.(HistoryCapable)
	if !ok {
		return reason.ErrBadRequest.SetMsg("protocol does not support history")
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode != "playback" && mode != "download" {
		return reason.ErrBadRequest.SetMsg("mode must be playback/download")
	}
	if in.StartAt <= 0 || in.EndAt <= in.StartAt {
		return reason.ErrBadRequest.SetMsg("invalid history range")
	}
	in.Mode = mode
	return h.StartHistory(ctx, dev, ch, in)
}

// StopHistory 停止历史会话（回放/下载）。
func (c Core) StopHistory(ctx context.Context, channelID string, in *HistoryControlInput) error {
	if in == nil {
		return reason.ErrBadRequest.SetMsg("invalid history request")
	}
	ch, err := c.GetChannel(ctx, channelID)
	if err != nil {
		return err
	}
	dev, err := c.GetDevice(ctx, ch.DID)
	if err != nil {
		return err
	}
	p, ok := c.protocols[dev.GetType()]
	if !ok {
		return reason.ErrBadRequest.SetMsg("unsupported protocol")
	}
	h, ok := p.(HistoryCapable)
	if !ok {
		return reason.ErrBadRequest.SetMsg("protocol does not support history")
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode != "playback" && mode != "download" {
		return reason.ErrBadRequest.SetMsg("mode must be playback/download")
	}
	in.Mode = mode
	return h.StopHistory(ctx, dev, ch, in)
}

// ControlHistory 下发历史会话控制命令（INFO/MANSRTSP）。
func (c Core) ControlHistory(ctx context.Context, channelID string, in *HistoryControlInput) error {
	if in == nil {
		return reason.ErrBadRequest.SetMsg("invalid history request")
	}
	ch, err := c.GetChannel(ctx, channelID)
	if err != nil {
		return err
	}
	dev, err := c.GetDevice(ctx, ch.DID)
	if err != nil {
		return err
	}
	p, ok := c.protocols[dev.GetType()]
	if !ok {
		return reason.ErrBadRequest.SetMsg("unsupported protocol")
	}
	h, ok := p.(HistoryCapable)
	if !ok {
		return reason.ErrBadRequest.SetMsg("protocol does not support history")
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode != "playback" && mode != "download" {
		return reason.ErrBadRequest.SetMsg("mode must be playback/download")
	}
	in.Mode = mode
	in.Action = strings.ToLower(strings.TrimSpace(in.Action))
	return h.ControlHistory(ctx, dev, ch, in)
}

// SyncTime 执行设备校时（9.10）。
func (c Core) SyncTime(ctx context.Context, deviceID string) error {
	dev, err := c.GetDevice(ctx, deviceID)
	if err != nil {
		return err
	}
	p, ok := c.protocols[dev.GetType()]
	if !ok {
		return reason.ErrBadRequest.SetMsg("unsupported protocol")
	}
	s, ok := p.(TimeSyncCapable)
	if !ok {
		return reason.ErrBadRequest.SetMsg("protocol does not support time sync")
	}
	return s.SyncTime(ctx, dev)
}

// Subscribe 发起事件订阅（9.11）。
func (c Core) Subscribe(ctx context.Context, deviceID string, in *SubscribeInput) error {
	if in == nil {
		return reason.ErrBadRequest.SetMsg("invalid subscribe request")
	}
	dev, err := c.GetDevice(ctx, deviceID)
	if err != nil {
		return err
	}
	p, ok := c.protocols[dev.GetType()]
	if !ok {
		return reason.ErrBadRequest.SetMsg("unsupported protocol")
	}
	s, ok := p.(SubscribeCapable)
	if !ok {
		return reason.ErrBadRequest.SetMsg("protocol does not support subscribe")
	}
	in.Event = strings.TrimSpace(in.Event)
	return s.Subscribe(ctx, dev, in)
}

// StartVoice 启动语音会话（9.12），mode=talk/broadcast。
func (c Core) StartVoice(ctx context.Context, channelID string, in *VoiceControlInput) error {
	if in == nil {
		return reason.ErrBadRequest.SetMsg("invalid voice request")
	}
	ch, err := c.GetChannel(ctx, channelID)
	if err != nil {
		return err
	}
	dev, err := c.GetDevice(ctx, ch.DID)
	if err != nil {
		return err
	}
	p, ok := c.protocols[dev.GetType()]
	if !ok {
		return reason.ErrBadRequest.SetMsg("unsupported protocol")
	}
	v, ok := p.(VoiceCapable)
	if !ok {
		return reason.ErrBadRequest.SetMsg("protocol does not support voice")
	}
	in.Mode = strings.ToLower(strings.TrimSpace(in.Mode))
	if in.Mode != "talk" && in.Mode != "broadcast" {
		return reason.ErrBadRequest.SetMsg("mode must be talk/broadcast")
	}
	return v.StartVoice(ctx, dev, ch, in)
}

// StopVoice 停止语音会话（9.12）。
func (c Core) StopVoice(ctx context.Context, channelID string, in *VoiceControlInput) error {
	if in == nil {
		return reason.ErrBadRequest.SetMsg("invalid voice request")
	}
	ch, err := c.GetChannel(ctx, channelID)
	if err != nil {
		return err
	}
	dev, err := c.GetDevice(ctx, ch.DID)
	if err != nil {
		return err
	}
	p, ok := c.protocols[dev.GetType()]
	if !ok {
		return reason.ErrBadRequest.SetMsg("unsupported protocol")
	}
	v, ok := p.(VoiceCapable)
	if !ok {
		return reason.ErrBadRequest.SetMsg("protocol does not support voice")
	}
	in.Mode = strings.ToLower(strings.TrimSpace(in.Mode))
	if in.Mode != "talk" && in.Mode != "broadcast" {
		return reason.ErrBadRequest.SetMsg("mode must be talk/broadcast")
	}
	return v.StopVoice(ctx, dev, ch, in)
}
