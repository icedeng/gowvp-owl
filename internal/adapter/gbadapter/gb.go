package gbadapter

import (
	"context"
	"strings"
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
	if a == nil || a.gbs == nil || device == nil {
		return nil
	}
	return a.gbs.CleanupDevice(ctx, device.GetGB28181DeviceID())
}

func NewAdapter(adapter ipc.Adapter, gbs *gbs.Server, smsCore sms.Core) *Adapter {
	return &Adapter{adapter: adapter, gbs: gbs, smsCore: smsCore}
}

// InitDevice implements ipc.Protocoler.
func (a *Adapter) InitDevice(ctx context.Context, device *ipc.Device) error {
	a.gbs.RefreshDeviceVersion(device)
	return nil
}

// OnStreamChanged implements ipc.Protocoler.
// 流注销时停止播放并更新播放状态（仅在 regist=false 时由 zlm_webhook 调用）
// stream 可能是普通 channel.ID，也可能是指定路径级联使用的独立流 ID。
func (a *Adapter) OnStreamChanged(ctx context.Context, app, stream string) error {
	_ = app
	return a.gbs.OnMediaStreamChanged(ctx, stream, false, "stream_unregistered")
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

	return a.gbs.PlayContext(ctx, &gbs.PlayInput{
		Channel:    ch,
		StreamMode: dev.StreamMode,
		SMS:        svr,
	})
}

// QueryCatalog implements ipc.Protocoler.
func (a *Adapter) QueryCatalog(ctx context.Context, device *ipc.Device) error {
	return a.gbs.QueryCatalogContext(ctx, device.DeviceID)
}

// StartPlay implements ipc.Protocoler.
func (a *Adapter) StartPlay(ctx context.Context, device *ipc.Device, channel *ipc.Channel) (*ipc.PlayResponse, error) {
	svr, err := a.smsCore.GetMediaServer(ctx, sms.DefaultMediaServerID)
	if err != nil {
		return nil, err
	}
	if err := a.gbs.PlayContext(ctx, &gbs.PlayInput{
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
	disabled, err := gbs.NormalizeGBDisabledCapabilities(device.Ext.GBDisabledCapabilities)
	if err != nil {
		return err
	}
	device.Ext.GBDisabledCapabilities = disabled
	a.gbs.RefreshDeviceVersion(device)
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

func (a *Adapter) Upgrade(ctx context.Context, device *ipc.Device, channel *ipc.Channel, in *ipc.UpgradeInput) (*ipc.UpgradeOutput, error) {
	out, err := a.gbs.Upgrade(ctx, &gbs.UpgradeInput{
		DeviceID:     device.DeviceID,
		ChannelID:    channel.ChannelID,
		Firmware:     in.Firmware,
		FileURL:      in.FileURL,
		Manufacturer: in.Manufacturer,
		SessionID:    in.SessionID,
		Timeout:      time.Duration(in.Timeout) * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return &ipc.UpgradeOutput{
		SN: out.SN, DeviceID: out.DeviceID, ChannelID: out.Channel, SessionID: out.SessionID, Result: out.Result,
	}, nil
}

func (a *Adapter) StartHistory(ctx context.Context, device *ipc.Device, channel *ipc.Channel, in *ipc.HistoryControlInput) error {
	var svr *sms.MediaServer
	if in.Transport != ipc.HistoryTransportDirectTCP {
		var err error
		svr, err = a.smsCore.GetMediaServer(ctx, sms.DefaultMediaServerID)
		if err != nil {
			return err
		}
	}
	mode := "Playback"
	if in.Mode == "download" {
		mode = "Download"
	}
	return a.gbs.StartHistory(ctx, &gbs.HistoryInput{
		Channel:       channel,
		SMS:           svr,
		StreamMode:    device.StreamMode,
		StartAt:       time.Unix(in.StartAt, 0),
		EndAt:         time.Unix(in.EndAt, 0),
		Mode:          mode,
		Transport:     in.Transport,
		DownloadSpeed: in.DownloadSpeed,
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
		DeviceID:           device.DeviceID,
		TargetID:           in.TargetID,
		Event:              in.Event,
		Expires:            in.Expires,
		Cancel:             in.Cancel,
		StartAlarmPriority: in.StartAlarmPriority,
		EndAlarmPriority:   in.EndAlarmPriority,
		AlarmMethod:        in.AlarmMethod,
		AlarmType:          in.AlarmType,
		StartAlarmTime:     in.StartAlarmTime,
		EndAlarmTime:       in.EndAlarmTime,
		Interval:           in.Interval,
	})
}

func (a *Adapter) ProbeOptions(ctx context.Context, device *ipc.Device, in *ipc.OptionsProbeInput) error {
	return a.gbs.ProbeOptions(ctx, &gbs.OptionsProbeInput{
		DeviceID: device.DeviceID,
		Timeout:  time.Duration(in.Timeout) * time.Second,
	})
}

func (a *Adapter) StartVoice(ctx context.Context, device *ipc.Device, channel *ipc.Channel, in *ipc.VoiceControlInput) error {
	mediaServerID := strings.TrimSpace(in.MediaServerID)
	if mediaServerID == "" {
		mediaServerID = sms.DefaultMediaServerID
	}
	svr, err := a.smsCore.GetMediaServer(ctx, mediaServerID)
	if err != nil {
		return err
	}
	mode := "Talk"
	if in.Mode == "broadcast" {
		mode = "Broadcast"
	}
	return a.gbs.StartVoice(ctx, &gbs.VoiceInput{
		Channel:      channel,
		SMS:          svr,
		StreamMode:   device.StreamMode,
		Mode:         mode,
		SourceID:     in.SourceID,
		SourceVHost:  in.SourceVHost,
		SourceApp:    in.SourceApp,
		SourceStream: in.SourceStream,
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
		TargetTrack:  toGBTargetTrack(in.TargetTrack),
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

func toGBTargetTrack(in *ipc.GBTargetTrackInput) *gbs.TargetTrackParam {
	if in == nil {
		return nil
	}
	return &gbs.TargetTrackParam{Mode: in.Mode, DeviceID2: in.DeviceID2, TargetArea: toGBDragZoom(in.TargetArea)}
}

func (a *Adapter) DeviceQuery(ctx context.Context, device *ipc.Device, in *ipc.GBDeviceQueryInput) (*ipc.GBDeviceQueryOutput, error) {
	out, err := a.gbs.DeviceQuery(ctx, &gbs.DeviceQueryInput{
		DeviceID:   device.DeviceID,
		TargetID:   in.TargetID,
		Action:     in.Action,
		Timeout:    time.Duration(in.Timeout) * time.Second,
		ConfigType: in.ConfigType,
		Interval:   in.Interval,
		Number:     in.Number,
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

func (a *Adapter) DeviceConfig(ctx context.Context, device *ipc.Device, in *ipc.GBDeviceConfigInput) (*ipc.GBDeviceConfigOutput, error) {
	state, err := a.gbs.SetDeviceConfig(ctx, toGBDeviceConfigInput(device.DeviceID, in))
	if err != nil {
		return nil, err
	}
	return &ipc.GBDeviceConfigOutput{
		SN:       state.SN,
		CmdType:  state.CmdType,
		DeviceID: state.DeviceID,
		Result:   state.Result,
		RawXML:   state.RawXML,
	}, nil
}

func toGBDeviceConfigInput(deviceID string, in *ipc.GBDeviceConfigInput) *gbs.DeviceConfigInput {
	if in == nil {
		return nil
	}
	input := &gbs.DeviceConfigInput{
		DeviceID: deviceID,
		TargetID: in.TargetID,
		Timeout:  time.Duration(in.Timeout) * time.Second,
	}
	if in.BasicParam != nil {
		input.BasicParam = &gbs.BasicParam{
			Name:              in.BasicParam.Name,
			Expiration:        in.BasicParam.Expiration,
			HeartBeatInterval: in.BasicParam.HeartBeatInterval,
			HeartBeatCount:    in.BasicParam.HeartBeatCount,
		}
	}
	if in.VideoParamConfig != nil {
		input.VideoParamConfig = &gbs.VideoParamConfigWrite{Items: make([]gbs.VideoParamWriteItem, 0, len(in.VideoParamConfig.Items))}
		for _, item := range in.VideoParamConfig.Items {
			input.VideoParamConfig.Items = append(input.VideoParamConfig.Items, gbs.VideoParamWriteItem{
				StreamName: item.StreamName, VideoFormat: item.VideoFormat, Resolution: item.Resolution,
				FrameRate: item.FrameRate, BitRateType: item.BitRateType, VideoBitRate: item.VideoBitRate,
			})
		}
	}
	if in.AudioParamConfig != nil {
		input.AudioParamConfig = &gbs.AudioParamConfigWrite{Items: make([]gbs.AudioParamWriteItem, 0, len(in.AudioParamConfig.Items))}
		for _, item := range in.AudioParamConfig.Items {
			input.AudioParamConfig.Items = append(input.AudioParamConfig.Items, gbs.AudioParamWriteItem{
				StreamName: item.StreamName, AudioFormat: item.AudioFormat,
				AudioBitRate: item.AudioBitRate, SamplingRate: item.SamplingRate,
			})
		}
	}
	if in.SVACEncodeConfig != nil {
		input.SVACEncodeConfig = &gbs.SVACEncodeConfig{InnerXML: in.SVACEncodeConfig.InnerXML}
	}
	if in.SVACDecodeConfig != nil {
		input.SVACDecodeConfig = &gbs.SVACDecodeConfig{InnerXML: in.SVACDecodeConfig.InnerXML}
	}
	if in.VideoParamAttribute != nil {
		input.VideoParamAttribute = &gbs.VideoParamAttribute{InnerXML: in.VideoParamAttribute.InnerXML}
	}
	if in.VideoRecordPlan != nil {
		input.VideoRecordPlan = &gbs.VideoRecordPlan{InnerXML: in.VideoRecordPlan.InnerXML}
	}
	if in.VideoAlarmRecord != nil {
		input.VideoAlarmRecord = &gbs.VideoAlarmRecord{InnerXML: in.VideoAlarmRecord.InnerXML}
	}
	if in.PictureMask != nil {
		input.PictureMask = &gbs.PictureMask{InnerXML: in.PictureMask.InnerXML}
	}
	if in.FrameMirror != nil {
		input.FrameMirror = &gbs.FrameMirror{InnerXML: in.FrameMirror.InnerXML}
	}
	if in.AlarmReport != nil {
		input.AlarmReport = &gbs.AlarmReport{InnerXML: in.AlarmReport.InnerXML}
	}
	if in.OSDConfig != nil {
		input.OSDConfig = &gbs.OSDConfig{InnerXML: in.OSDConfig.InnerXML}
	}
	if in.SnapShotConfig != nil {
		input.SnapShotConfig = &gbs.SnapShot{
			SnapNum: in.SnapShotConfig.SnapNum, Interval: in.SnapShotConfig.Interval,
			UploadURL: in.SnapShotConfig.UploadURL, SessionID: in.SnapShotConfig.SessionID,
		}
	}
	return input
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
