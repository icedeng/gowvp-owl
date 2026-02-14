package gbs

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	deviceControlActionCameraControl = "camera_control"
	deviceControlActionTeleBoot      = "tele_boot"
	deviceControlActionRecordStart   = "record_start"
	deviceControlActionRecordStop    = "record_stop"
	deviceControlActionGuardSet      = "guard_set"
	deviceControlActionGuardReset    = "guard_reset"
	deviceControlActionAlarmReset    = "alarm_reset"
	deviceControlActionIFrameSend    = "iframe_send"
	deviceControlActionDragZoomIn    = "drag_zoom_in"
	deviceControlActionDragZoomOut   = "drag_zoom_out"
	deviceControlActionHomePosition  = "home_position"
	deviceControlActionPTZPrecise    = "ptz_precise"
	deviceControlActionFormatSDCard  = "format_sdcard"
)

// DeviceControlInput 是附录 A.2.3 设备控制命令输入。
//
// 说明：
// 1. DeviceID 为设备国标 ID（必填）。
// 2. TargetID 为空时默认对设备下发；需要对通道下发时传通道国标 ID。
// 3. Action 对应 A.2.3.1.3~A.2.3.1.13 的具体控制命令。
type DeviceControlInput struct {
	DeviceID string
	TargetID string
	Action   string
	Timeout  time.Duration

	// CameraControl 参数（A.2.3.1.2）。
	PTZCmd      string
	PTZCmdParam *PTZCmdParam

	// RecordCmd 参数。
	StreamNumber int
	// AlarmCmd 附带参数。
	AlarmMethod string
	AlarmType   string

	// DragZoomIn/DragZoomOut 参数。
	DragZoom *DragZoomParam
	// HomePosition 参数。
	HomePosition *HomePositionParam
	// PTZPreciseCtrl 参数。
	PTZPrecise *PTZPreciseParam
	// FormatSDCard 参数（0 表示全部格式化）。
	SDCardID int
}

// DeviceControlOutput 是设备控制统一返回。
type DeviceControlOutput struct {
	SN       int    `json:"sn"`
	DeviceID string `json:"device_id"`
	TargetID string `json:"target_id"`
	Result   string `json:"result"`
}

// DragZoomParam 对应 DragZoomIn/DragZoomOut 的矩形参数。
type DragZoomParam struct {
	Length    int `json:"length"`
	Width     int `json:"width"`
	MidPointX int `json:"mid_point_x"`
	MidPointY int `json:"mid_point_y"`
	LengthX   int `json:"length_x"`
	LengthY   int `json:"length_y"`
}

// HomePositionParam 对应看守位控制参数。
type HomePositionParam struct {
	Enabled     *int `json:"enabled,omitempty"`
	ResetTime   *int `json:"reset_time,omitempty"`
	PresetIndex *int `json:"preset_index,omitempty"`
}

// PTZPreciseParam 对应 PTZ 精准控制参数。
type PTZPreciseParam struct {
	Pan  *float64 `json:"pan,omitempty"`
	Tilt *float64 `json:"tilt,omitempty"`
	Zoom *float64 `json:"zoom,omitempty"`
}

// PTZCmdParam 对应 A.2.3.1.2 PTZCmdParams 可选参数。
type PTZCmdParam struct {
	PresetName      string `json:"preset_name,omitempty"`
	CruiseTrackName string `json:"cruise_track_name,omitempty"`
}

type deviceControlA23Request struct {
	XMLName  xml.Name `xml:"Control"`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	DeviceID string   `xml:"DeviceID"`

	TeleBoot string `xml:"TeleBoot,omitempty"`

	PTZCmd       string                       `xml:"PTZCmd,omitempty"`
	PTZCmdParams *deviceControlA23PTZCmdParam `xml:"PTZCmdParams,omitempty"`

	RecordCmd    string `xml:"RecordCmd,omitempty"`
	StreamNumber *int   `xml:"StreamNumber,omitempty"`

	GuardCmd string `xml:"GuardCmd,omitempty"`

	AlarmCmd string                `xml:"AlarmCmd,omitempty"`
	Info     *deviceControlA23Info `xml:"Info,omitempty"`

	IFrameCmd string `xml:"IFrameCmd,omitempty"`

	DragZoomIn  *deviceControlA23DragZoom `xml:"DragZoomIn,omitempty"`
	DragZoomOut *deviceControlA23DragZoom `xml:"DragZoomOut,omitempty"`

	HomePosition *deviceControlA23HomePosition `xml:"HomePosition,omitempty"`

	PTZPreciseCtrl *deviceControlA23PTZPrecise `xml:"PTZPreciseCtrl,omitempty"`

	FormatSDCard *int `xml:"FormatSDCard,omitempty"`
}

type deviceControlA23Info struct {
	AlarmMethod string `xml:"AlarmMethod,omitempty"`
	AlarmType   string `xml:"AlarmType,omitempty"`
}

type deviceControlA23PTZCmdParam struct {
	PresetName      string `xml:"PresetName,omitempty"`
	CruiseTrackName string `xml:"CruiseTrackName,omitempty"`
}

type deviceControlA23DragZoom struct {
	Length    int `xml:"Length"`
	Width     int `xml:"Width"`
	MidPointX int `xml:"MidPointX"`
	MidPointY int `xml:"MidPointY"`
	LengthX   int `xml:"LengthX"`
	LengthY   int `xml:"LengthY"`
}

type deviceControlA23HomePosition struct {
	Enabled     *int `xml:"Enabled,omitempty"`
	ResetTime   *int `xml:"ResetTime,omitempty"`
	PresetIndex *int `xml:"PresetIndex,omitempty"`
}

type deviceControlA23PTZPrecise struct {
	Pan  *float64 `xml:"Pan,omitempty"`
	Tilt *float64 `xml:"Tilt,omitempty"`
	Zoom *float64 `xml:"Zoom,omitempty"`
}

// DeviceControl 执行附录 A.2.3 设备控制命令，并等待设备 Response。
func (g *GB28181API) DeviceControl(_ context.Context, in *DeviceControlInput) (*DeviceControlOutput, error) {
	if in == nil || strings.TrimSpace(in.DeviceID) == "" {
		return nil, ErrDeviceNotExist
	}
	deviceID := strings.TrimSpace(in.DeviceID)
	ipc, ok := g.svr.memoryStorer.Load(deviceID)
	if !ok || !ipc.IsOnline {
		return nil, ErrDeviceOffline
	}

	if in.Timeout <= 0 {
		in.Timeout = 6 * time.Second
	}
	targetID := strings.TrimSpace(in.TargetID)
	if targetID == "" {
		targetID = deviceID
	}
	action := normalizeDeviceControlAction(in.Action)
	sn := g.nextControlSN()

	req := deviceControlA23Request{
		CmdType:  ptzCmdTypeDeviceControl,
		SN:       sn,
		DeviceID: targetID,
	}
	if err := g.fillDeviceControlRequest(deviceID, action, in, &req); err != nil {
		return nil, err
	}

	body, err := sip.XMLEncode(req)
	if err != nil {
		return nil, err
	}

	var target Targeter = ipc
	if targetID != deviceID {
		ch, ok := g.svr.memoryStorer.GetChannel(deviceID, targetID)
		if !ok {
			return nil, ErrChannelNotExist
		}
		target = ch
	}

	waitKey := fmt.Sprintf("%s:%d", deviceID, sn)
	pending := &pendingDeviceControl{wait: make(chan *deviceControlResponse, 1)}
	g.pendingDeviceControl.Store(waitKey, pending)
	defer g.pendingDeviceControl.Delete(waitKey)

	tx, err := g.svr.wrapRequest(target, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		return nil, err
	}
	if _, err = sipResponse(tx); err != nil {
		return nil, err
	}

	timer := time.NewTimer(in.Timeout)
	defer timer.Stop()

	select {
	case resp := <-pending.wait:
		result := strings.ToUpper(strings.TrimSpace(resp.Result))
		if result == "" {
			result = ptzResultOK
		}
		if result != ptzResultOK {
			return nil, fmt.Errorf("device control failed: %s", resp.Result)
		}
		return &DeviceControlOutput{
			SN:       sn,
			DeviceID: deviceID,
			TargetID: targetID,
			Result:   result,
		}, nil
	case <-timer.C:
		return nil, fmt.Errorf("wait device control response timeout")
	}
}

func normalizeDeviceControlAction(action string) string {
	a := strings.ToLower(strings.TrimSpace(action))
	a = strings.ReplaceAll(a, "-", "_")
	switch a {
	case "camera", "camera_ctrl", "ptz_cmd":
		return deviceControlActionCameraControl
	case "teleboot", "reboot":
		return deviceControlActionTeleBoot
	case "record", "record_on":
		return deviceControlActionRecordStart
	case "stop_record", "record_off":
		return deviceControlActionRecordStop
	case "set_guard", "guard_on", "arm":
		return deviceControlActionGuardSet
	case "reset_guard", "guard_off", "disarm":
		return deviceControlActionGuardReset
	case "reset_alarm":
		return deviceControlActionAlarmReset
	case "iframe", "iframe_cmd":
		return deviceControlActionIFrameSend
	case "ptz_precise_ctrl":
		return deviceControlActionPTZPrecise
	case "format_sd_card":
		return deviceControlActionFormatSDCard
	default:
		return a
	}
}

func (g *GB28181API) fillDeviceControlRequest(deviceID, action string, in *DeviceControlInput, req *deviceControlA23Request) error {
	switch action {
	case deviceControlActionCameraControl:
		req.PTZCmd = strings.TrimSpace(in.PTZCmd)
		if req.PTZCmd == "" {
			return fmt.Errorf("camera_control requires ptz_cmd")
		}
		if in.PTZCmdParam != nil {
			req.PTZCmdParams = &deviceControlA23PTZCmdParam{
				PresetName:      strings.TrimSpace(in.PTZCmdParam.PresetName),
				CruiseTrackName: strings.TrimSpace(in.PTZCmdParam.CruiseTrackName),
			}
		}
	case deviceControlActionTeleBoot:
		req.TeleBoot = "Boot"
	case deviceControlActionRecordStart:
		req.RecordCmd = "Record"
		streamNo := in.StreamNumber
		if streamNo < 0 || streamNo > 2 {
			return fmt.Errorf("stream_number must be in [0,2]")
		}
		req.StreamNumber = &streamNo
	case deviceControlActionRecordStop:
		req.RecordCmd = "StopRecord"
		streamNo := in.StreamNumber
		if streamNo < 0 || streamNo > 2 {
			return fmt.Errorf("stream_number must be in [0,2]")
		}
		req.StreamNumber = &streamNo
	case deviceControlActionGuardSet:
		req.GuardCmd = "SetGuard"
	case deviceControlActionGuardReset:
		req.GuardCmd = "ResetGuard"
	case deviceControlActionAlarmReset:
		req.AlarmCmd = "ResetAlarm"
		if strings.TrimSpace(in.AlarmMethod) != "" || strings.TrimSpace(in.AlarmType) != "" {
			req.Info = &deviceControlA23Info{
				AlarmMethod: strings.TrimSpace(in.AlarmMethod),
				AlarmType:   strings.TrimSpace(in.AlarmType),
			}
		}
	case deviceControlActionIFrameSend:
		req.IFrameCmd = "Send"
	case deviceControlActionDragZoomIn:
		if in.DragZoom == nil {
			return fmt.Errorf("drag_zoom_in requires drag_zoom params")
		}
		req.DragZoomIn = &deviceControlA23DragZoom{
			Length:    in.DragZoom.Length,
			Width:     in.DragZoom.Width,
			MidPointX: in.DragZoom.MidPointX,
			MidPointY: in.DragZoom.MidPointY,
			LengthX:   in.DragZoom.LengthX,
			LengthY:   in.DragZoom.LengthY,
		}
	case deviceControlActionDragZoomOut:
		if in.DragZoom == nil {
			return fmt.Errorf("drag_zoom_out requires drag_zoom params")
		}
		req.DragZoomOut = &deviceControlA23DragZoom{
			Length:    in.DragZoom.Length,
			Width:     in.DragZoom.Width,
			MidPointX: in.DragZoom.MidPointX,
			MidPointY: in.DragZoom.MidPointY,
			LengthX:   in.DragZoom.LengthX,
			LengthY:   in.DragZoom.LengthY,
		}
	case deviceControlActionHomePosition:
		if err := g.requireGBVersionAtLeast(deviceID, gbVersion2022, "看守位控制(HomePosition)"); err != nil {
			return err
		}
		home := &deviceControlA23HomePosition{}
		enabled := 1
		if in.HomePosition != nil && in.HomePosition.Enabled != nil {
			enabled = *in.HomePosition.Enabled
		}
		home.Enabled = &enabled
		if in.HomePosition != nil && in.HomePosition.ResetTime != nil {
			v := *in.HomePosition.ResetTime
			home.ResetTime = &v
		}
		if in.HomePosition != nil && in.HomePosition.PresetIndex != nil {
			v := *in.HomePosition.PresetIndex
			home.PresetIndex = &v
		}
		req.HomePosition = home
	case deviceControlActionPTZPrecise:
		if err := g.requireGBVersionAtLeast(deviceID, gbVersion2022, "PTZ精准控制"); err != nil {
			return err
		}
		if in.PTZPrecise == nil {
			return fmt.Errorf("ptz_precise requires ptz_precise params")
		}
		if in.PTZPrecise.Pan == nil && in.PTZPrecise.Tilt == nil && in.PTZPrecise.Zoom == nil {
			return fmt.Errorf("ptz_precise requires at least one of pan/tilt/zoom")
		}
		req.PTZPreciseCtrl = &deviceControlA23PTZPrecise{
			Pan:  in.PTZPrecise.Pan,
			Tilt: in.PTZPrecise.Tilt,
			Zoom: in.PTZPrecise.Zoom,
		}
	case deviceControlActionFormatSDCard:
		if err := g.requireGBVersionAtLeast(deviceID, gbVersion2022, "存储卡格式化控制"); err != nil {
			return err
		}
		if in.SDCardID < 0 {
			return fmt.Errorf("sdcard_id must be >= 0")
		}
		sd := in.SDCardID
		req.FormatSDCard = &sd
	default:
		return fmt.Errorf("unsupported device control action: %s", action)
	}
	return nil
}
