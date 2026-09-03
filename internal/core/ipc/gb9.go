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
	in.Transport = strings.ToLower(strings.TrimSpace(in.Transport))
	if in.Transport != "" && in.Transport != HistoryTransportRTP && in.Transport != HistoryTransportDirectTCP {
		return reason.ErrBadRequest.SetMsg("transport must be rtp/direct_tcp")
	}
	if in.Transport == HistoryTransportDirectTCP && mode != "download" {
		return reason.ErrBadRequest.SetMsg("direct_tcp transport only supports download")
	}
	if in.DownloadSpeed < 0 || (in.DownloadSpeed > 0 && mode != "download") {
		return reason.ErrBadRequest.SetMsg("download_speed must be non-negative and only used with download mode")
	}
	if in.RecordType != nil && (*in.RecordType < 0 || *in.RecordType > 3) {
		return reason.ErrBadRequest.SetMsg("record_type must be between 0 and 3")
	}
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

// SyncTime 执行厂商扩展 DeviceControl(Time) 主动校时。
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

// SubscriptionStates 查询平台向指定设备建立的事件订阅运行态。
func (c Core) SubscriptionStates(ctx context.Context, deviceID string) ([]SubscriptionState, error) {
	dev, err := c.GetDevice(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	p, ok := c.protocols[dev.GetType()]
	if !ok {
		return nil, reason.ErrBadRequest.SetMsg("unsupported protocol")
	}
	provider, ok := p.(SubscriptionStateCapable)
	if !ok {
		return nil, reason.ErrBadRequest.SetMsg("protocol does not expose subscription state")
	}
	states, err := provider.SubscriptionStates(ctx, dev)
	if err != nil {
		return nil, err
	}
	return append([]SubscriptionState(nil), states...), nil
}

// ProbeOptions 发起 OPTIONS 探活（9.2 协议探测）。
func (c Core) ProbeOptions(ctx context.Context, deviceID string, in *OptionsProbeInput) error {
	if in == nil {
		return reason.ErrBadRequest.SetMsg("invalid options probe request")
	}
	dev, err := c.GetDevice(ctx, deviceID)
	if err != nil {
		return err
	}
	p, ok := c.protocols[dev.GetType()]
	if !ok {
		return reason.ErrBadRequest.SetMsg("unsupported protocol")
	}
	s, ok := p.(OptionsProbeCapable)
	if !ok {
		return reason.ErrBadRequest.SetMsg("protocol does not support options probe")
	}
	return s.ProbeOptions(ctx, dev, in)
}

// StartVoice 启动语音会话（9.12），mode=talk/talk_standard/broadcast。
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
	if in.Mode != "talk" && in.Mode != "talk_standard" && in.Mode != "broadcast" {
		return reason.ErrBadRequest.SetMsg("mode must be talk/talk_standard/broadcast")
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
	if in.Mode != "talk" && in.Mode != "talk_standard" && in.Mode != "broadcast" {
		return reason.ErrBadRequest.SetMsg("mode must be talk/talk_standard/broadcast")
	}
	return v.StopVoice(ctx, dev, ch, in)
}
