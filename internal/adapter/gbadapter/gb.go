package gbadapter

import (
	"context"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs"
)

var _ ipc.Protocoler = (*Adapter)(nil)

type Adapter struct {
	adapter ipc.Adapter
	gbs     *gbs.Server
	smsCore sms.Core
}

// DeleteDevice implements ipc.Protocoler.
func (a *Adapter) DeleteDevice(ctx context.Context, device *ipc.Device) error {
	return nil
}

func NewAdapter(adapter ipc.Adapter, gbs *gbs.Server, smsCore sms.Core) *Adapter {
	return &Adapter{adapter: adapter, gbs: gbs, smsCore: smsCore}
}

// InitDevice implements ipc.Protocoler.
func (a *Adapter) InitDevice(ctx context.Context, device *ipc.Device) error {
	return nil
}

// OnStreamChanged implements ipc.Protocoler.
// 流注销时停止播放并更新播放状态（仅在 regist=false 时由 zlm_webhook 调用）
// GB28181 协议的 stream 就是 channel.ID，app 固定为 rtp
func (a *Adapter) OnStreamChanged(ctx context.Context, app, stream string) error {
	ch, err := a.adapter.GetChannel(ctx, stream)
	if err != nil {
		return err
	}
	// 更新播放状态为 false
	if err := a.adapter.EditPlayingByID(ctx, ch.ID, false); err != nil {
		return err
	}
	return a.gbs.StopPlay(ctx, &gbs.StopPlayInput{Channel: ch})
}

// OnStreamNotFound implements ipc.Protocoler.
func (a *Adapter) OnStreamNotFound(ctx context.Context, app string, stream string) error {
	ch, err := a.adapter.GetChannel(ctx, stream)
	if err != nil {
		return err
	}

	dev, err := a.adapter.GetDevice(ctx, ch.DID)
	if err != nil {
		return err
	}

	svr, err := a.smsCore.GetMediaServer(ctx, sms.DefaultMediaServerID)
	if err != nil {
		return err
	}

	return a.gbs.Play(&gbs.PlayInput{
		Channel:    ch,
		StreamMode: dev.StreamMode,
		SMS:        svr,
	})
}

// QueryCatalog implements ipc.Protocoler.
func (a *Adapter) QueryCatalog(ctx context.Context, device *ipc.Device) error {
	return a.gbs.QueryCatalog(device.DeviceID)
}

// StartPlay implements ipc.Protocoler.
func (a *Adapter) StartPlay(ctx context.Context, device *ipc.Device, channel *ipc.Channel) (*ipc.PlayResponse, error) {
	svr, err := a.smsCore.GetMediaServer(ctx, sms.DefaultMediaServerID)
	if err != nil {
		return nil, err
	}
	if err := a.gbs.Play(&gbs.PlayInput{
		Channel:    channel,
		StreamMode: device.StreamMode,
		SMS:        svr,
	}); err != nil {
		return nil, err
	}
	return &ipc.PlayResponse{
		Stream: channel.ID,
	}, nil
}

// StopPlay implements ipc.Protocoler.
func (a *Adapter) StopPlay(ctx context.Context, device *ipc.Device, channel *ipc.Channel) error {
	_ = device
	return a.gbs.StopPlay(ctx, &gbs.StopPlayInput{Channel: channel})
}

// ValidateDevice implements ipc.Protocoler.
func (a *Adapter) ValidateDevice(ctx context.Context, device *ipc.Device) error {
	return nil
}

func (a *Adapter) PTZControl(ctx context.Context, device *ipc.Device, channel *ipc.Channel, in *ipc.PTZControlInput) error {
	_, err := a.gbs.PTZ(ctx, &gbs.PTZInput{
		DeviceID:  device.DeviceID,
		ChannelID: channel.ChannelID,
		Action:    gbs.PTZAction(in.Action),
		Speed:     in.Speed,
		Timeout:   time.Duration(in.Timeout) * time.Second,
		Preset:    in.Preset,
		Group:     in.Group,
		Aux:       in.Aux,
		Value:     in.Value,
	})
	return err
}

// QueryRecords 通过 GB28181 RecordInfo 查询录像目录，并转换为 IPC 统一返回结构。
func (a *Adapter) QueryRecords(ctx context.Context, device *ipc.Device, channel *ipc.Channel, in *ipc.RecordQueryInput) (*ipc.RecordQueryOutput, error) {
	out, err := a.gbs.QueryRecordList(ctx, &gbs.RecordQueryInput{
		DeviceID:  device.DeviceID,
		ChannelID: channel.ChannelID,
		Start:     in.StartAt,
		End:       in.EndAt,
		Timeout:   time.Duration(in.Timeout) * time.Second,
	})
	if err != nil {
		return nil, err
	}

	ret := &ipc.RecordQueryOutput{
		DayTotal: out.DayTotal,
		TimeNum:  out.TimeNum,
		Data:     make([]ipc.RecordDate, 0, len(out.Data)),
	}
	// 结构转换：gbs.Record* -> ipc.Record*
	for _, day := range out.Data {
		item := ipc.RecordDate{
			Date:  day.Date,
			Items: make([]ipc.RecordSegment, 0, len(day.Items)),
		}
		for _, seg := range day.Items {
			item.Items = append(item.Items, ipc.RecordSegment{
				Start: seg.Start,
				End:   seg.End,
			})
		}
		ret.Data = append(ret.Data, item)
	}
	return ret, nil
}

func (a *Adapter) Upgrade(ctx context.Context, device *ipc.Device, channel *ipc.Channel, in *ipc.UpgradeInput) error {
	_, err := a.gbs.Upgrade(ctx, &gbs.UpgradeInput{
		DeviceID:     device.DeviceID,
		ChannelID:    channel.ChannelID,
		Firmware:     in.Firmware,
		FileURL:      in.FileURL,
		Manufacturer: in.Manufacturer,
		SessionID:    in.SessionID,
		Timeout:      time.Duration(in.Timeout) * time.Second,
	})
	return err
}

func (a *Adapter) StartHistory(ctx context.Context, device *ipc.Device, channel *ipc.Channel, in *ipc.HistoryControlInput) error {
	svr, err := a.smsCore.GetMediaServer(ctx, sms.DefaultMediaServerID)
	if err != nil {
		return err
	}
	mode := "Playback"
	if in.Mode == "download" {
		mode = "Download"
	}
	return a.gbs.StartHistory(ctx, &gbs.HistoryInput{
		Channel:    channel,
		SMS:        svr,
		StreamMode: device.StreamMode,
		StartAt:    time.Unix(in.StartAt, 0),
		EndAt:      time.Unix(in.EndAt, 0),
		Mode:       mode,
	})
}

func (a *Adapter) StopHistory(ctx context.Context, _ *ipc.Device, channel *ipc.Channel, in *ipc.HistoryControlInput) error {
	mode := "Playback"
	if in.Mode == "download" {
		mode = "Download"
	}
	return a.gbs.StopHistory(ctx, &gbs.StopHistoryInput{
		Channel: channel,
		Mode:    mode,
	})
}

func (a *Adapter) ControlHistory(ctx context.Context, _ *ipc.Device, channel *ipc.Channel, in *ipc.HistoryControlInput) error {
	mode := "Playback"
	if in.Mode == "download" {
		mode = "Download"
	}
	return a.gbs.ControlHistory(ctx, &gbs.ControlHistoryInput{
		Channel: channel,
		Mode:    mode,
		Cmd:     in.Cmd,
		Action:  in.Action,
		Scale:   in.Scale,
		SeekAt:  in.SeekAt,
	})
}

func (a *Adapter) SyncTime(ctx context.Context, device *ipc.Device) error {
	return a.gbs.SyncTime(ctx, &gbs.TimeSyncInput{DeviceID: device.DeviceID})
}

func (a *Adapter) Subscribe(ctx context.Context, device *ipc.Device, in *ipc.SubscribeInput) error {
	return a.gbs.Subscribe(ctx, &gbs.SubscribeInput{
		DeviceID: device.DeviceID,
		Event:    in.Event,
		Expires:  in.Expires,
	})
}

func (a *Adapter) ProbeOptions(ctx context.Context, device *ipc.Device, in *ipc.OptionsProbeInput) error {
	return a.gbs.ProbeOptions(ctx, &gbs.OptionsProbeInput{
		DeviceID: device.DeviceID,
		Timeout:  time.Duration(in.Timeout) * time.Second,
	})
}

func (a *Adapter) StartVoice(ctx context.Context, device *ipc.Device, channel *ipc.Channel, in *ipc.VoiceControlInput) error {
	svr, err := a.smsCore.GetMediaServer(ctx, sms.DefaultMediaServerID)
	if err != nil {
		return err
	}
	mode := "Talk"
	if in.Mode == "broadcast" {
		mode = "Broadcast"
	}
	return a.gbs.StartVoice(ctx, &gbs.VoiceInput{
		Channel:    channel,
		SMS:        svr,
		StreamMode: device.StreamMode,
		Mode:       mode,
	})
}

func (a *Adapter) StopVoice(ctx context.Context, _ *ipc.Device, channel *ipc.Channel, in *ipc.VoiceControlInput) error {
	mode := "Talk"
	if in.Mode == "broadcast" {
		mode = "Broadcast"
	}
	return a.gbs.StopVoice(ctx, &gbs.StopVoiceInput{
		Channel: channel,
		Mode:    mode,
	})
}

func (a *Adapter) DeviceControl(ctx context.Context, device *ipc.Device, in *ipc.GBDeviceControlInput) (*ipc.GBDeviceControlOutput, error) {
	out, err := a.gbs.DeviceControl(ctx, &gbs.DeviceControlInput{
		DeviceID:     device.DeviceID,
		TargetID:     in.TargetID,
		Action:       in.Action,
		Timeout:      time.Duration(in.Timeout) * time.Second,
		PTZCmd:       in.PTZCmd,
		PTZCmdParam:  toGBPTZCmdParam(in.PTZCmdParam),
		StreamNumber: in.StreamNumber,
		AlarmMethod:  in.AlarmMethod,
		AlarmType:    in.AlarmType,
		SDCardID:     in.SDCardID,
		DragZoom:     toGBDragZoom(in.DragZoom),
		HomePosition: toGBHomePosition(in.HomePosition),
		PTZPrecise:   toGBPTZPrecise(in.PTZPrecise),
	})
	if err != nil {
		return nil, err
	}
	return &ipc.GBDeviceControlOutput{
		SN:       out.SN,
		DeviceID: out.DeviceID,
		TargetID: out.TargetID,
		Result:   out.Result,
	}, nil
}

func (a *Adapter) DeviceQuery(ctx context.Context, device *ipc.Device, in *ipc.GBDeviceQueryInput) (*ipc.GBDeviceQueryOutput, error) {
	out, err := a.gbs.DeviceQuery(ctx, &gbs.DeviceQueryInput{
		DeviceID:   device.DeviceID,
		TargetID:   in.TargetID,
		Action:     in.Action,
		Timeout:    time.Duration(in.Timeout) * time.Second,
		ConfigType: in.ConfigType,
		Interval:   in.Interval,
		Start:      in.Start,
		End:        in.End,
	})
	if err != nil {
		return nil, err
	}
	return &ipc.GBDeviceQueryOutput{
		SN:         out.SN,
		CmdType:    out.CmdType,
		DeviceID:   out.DeviceID,
		Result:     out.Result,
		XML:        out.XML,
		Data:       out.Data,
		AppendixA4: toIPCAppendixA4(out.AppendixA4),
	}, nil
}

func toGBDragZoom(in *ipc.GBDragZoomInput) *gbs.DragZoomParam {
	if in == nil {
		return nil
	}
	return &gbs.DragZoomParam{
		Length:    in.Length,
		Width:     in.Width,
		MidPointX: in.MidPointX,
		MidPointY: in.MidPointY,
		LengthX:   in.LengthX,
		LengthY:   in.LengthY,
	}
}

func toGBHomePosition(in *ipc.GBHomePositionInput) *gbs.HomePositionParam {
	if in == nil {
		return nil
	}
	return &gbs.HomePositionParam{
		Enabled:     in.Enabled,
		ResetTime:   in.ResetTime,
		PresetIndex: in.PresetIndex,
	}
}

func toGBPTZPrecise(in *ipc.GBPTZPreciseInput) *gbs.PTZPreciseParam {
	if in == nil {
		return nil
	}
	return &gbs.PTZPreciseParam{
		Pan:  in.Pan,
		Tilt: in.Tilt,
		Zoom: in.Zoom,
	}
}

func toGBPTZCmdParam(in *ipc.GBPTZCmdParamInput) *gbs.PTZCmdParam {
	if in == nil {
		return nil
	}
	return &gbs.PTZCmdParam{
		PresetName:      in.PresetName,
		CruiseTrackName: in.CruiseTrackName,
	}
}

func toIPCAppendixA4(in []gbs.AppendixA4Object) []ipc.GBAppendixA4Object {
	if len(in) == 0 {
		return nil
	}
	out := make([]ipc.GBAppendixA4Object, 0, len(in))
	for _, item := range in {
		out = append(out, ipc.GBAppendixA4Object{
			Type:      item.Type,
			CmdType:   item.CmdType,
			Path:      item.Path,
			Fields:    item.Fields,
			RawXML:    item.RawXML,
			UpdatedAt: item.UpdatedAt,
		})
	}
	return out
}
