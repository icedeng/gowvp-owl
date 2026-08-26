package gbs

import (
	"context"
	"encoding/xml"
	"fmt"
	"math"
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
	deviceControlActionTargetTrack   = "target_track"
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
	// TargetTrack 参数（2022 A.2.3.1.14）。
	TargetTrack *TargetTrackParam
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

// TargetTrackParam 对应全景摄像机目标自动/手动跟踪参数。
type TargetTrackParam struct {
	Mode       string         `json:"mode"`
	DeviceID2  string         `json:"device_id2,omitempty"`
	TargetArea *DragZoomParam `json:"target_area,omitempty"`
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

	TargetTrack string                    `xml:"TargetTrack,omitempty"`
	DeviceID2   string                    `xml:"DeviceID2,omitempty"`
	TargetArea  *deviceControlA23DragZoom `xml:"TargetArea,omitempty"`

	DeviceUpgrade *deviceUpgradeConfig `xml:"DeviceUpgrade,omitempty"`
}

type deviceUpgradeConfig struct {
	Firmware     string `xml:"Firmware"`
	FileURL      string `xml:"FileURL"`
	Manufacturer string `xml:"Manufacturer"`
	SessionID    string `xml:"SessionID,omitempty"`
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
func (g *GB28181API) DeviceControl(ctx context.Context, in *DeviceControlInput) (*DeviceControlOutput, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in == nil || strings.TrimSpace(in.DeviceID) == "" {
		return nil, ErrDeviceNotExist
	}
	deviceID := strings.TrimSpace(in.DeviceID)
	ipc, ok := g.svr.memoryStorer.Load(deviceID)
	if !ok || !ipc.IsOnlineNow() {
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
	pending := &pendingDeviceControl{wait: make(chan *deviceControlResponse, 1), targetID: targetID}
	g.pendingDeviceControl.Store(waitKey, pending)
	defer g.pendingDeviceControl.Delete(waitKey)

	tx, err := g.svr.wrapRequestContext(ctx, target, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		return nil, err
	}
	if _, err = sipResponseContext(ctx, tx); err != nil {
		return nil, err
	}

	timer := time.NewTimer(in.Timeout)
	defer timer.Stop()

	select {
	case resp := <-pending.wait:
		result := strings.ToUpper(strings.TrimSpace(resp.Result))
		if result != ptzResultOK {
			return nil, fmt.Errorf("device control failed: %s", resp.Result)
		}
		return &DeviceControlOutput{
			SN:       sn,
			DeviceID: deviceID,
			TargetID: targetID,
			Result:   result,
		}, nil
	case <-g.serviceDone():
		return nil, ErrServiceStopped
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		// 统一返回更明确的中文错误，避免调用方误以为命令未发送。
		return nil, fmt.Errorf("%s", ptzTimeoutErrorMessage)
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
	case "target_tracking", "track_target":
		return deviceControlActionTargetTrack
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
			presetName := strings.TrimSpace(in.PTZCmdParam.PresetName)
			cruiseTrackName := strings.TrimSpace(in.PTZCmdParam.CruiseTrackName)
			if presetName != "" || cruiseTrackName != "" {
				if err := g.requireGBVersionAtLeast(deviceID, gbVersion2022, "云台控制附加参数"); err != nil {
					return err
				}
				if len(cruiseTrackName) > 32 {
					return fmt.Errorf("cruise_track_name must not exceed 32 bytes")
				}
				req.PTZCmdParams = &deviceControlA23PTZCmdParam{
					PresetName:      presetName,
					CruiseTrackName: cruiseTrackName,
				}
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
		if streamNo != 0 || g.getDeviceGBProtocolVersion(deviceID).AtLeast(GBVersion20) {
			if err := g.requireGBVersionAtLeast(deviceID, gbVersion2016, "指定录像码流"); err != nil {
				return err
			}
			req.StreamNumber = &streamNo
		}
	case deviceControlActionRecordStop:
		req.RecordCmd = "StopRecord"
		streamNo := in.StreamNumber
		if streamNo < 0 || streamNo > 2 {
			return fmt.Errorf("stream_number must be in [0,2]")
		}
		if streamNo != 0 || g.getDeviceGBProtocolVersion(deviceID).AtLeast(GBVersion20) {
			if err := g.requireGBVersionAtLeast(deviceID, gbVersion2016, "指定录像码流"); err != nil {
				return err
			}
			req.StreamNumber = &streamNo
		}
	case deviceControlActionGuardSet:
		req.GuardCmd = "SetGuard"
	case deviceControlActionGuardReset:
		req.GuardCmd = "ResetGuard"
	case deviceControlActionAlarmReset:
		req.AlarmCmd = "ResetAlarm"
		alarmMethod := strings.TrimSpace(in.AlarmMethod)
		alarmType := strings.TrimSpace(in.AlarmType)
		if alarmMethod != "" || alarmType != "" {
			if err := g.requireGBVersionAtLeast(deviceID, gbVersion2016, "报警复位扩展参数"); err != nil {
				return err
			}
			if g.getDeviceGBProtocolVersion(deviceID).AtLeast(GBVersion30) {
				if err := validateAlarmResetInfo(alarmMethod, alarmType); err != nil {
					return err
				}
			}
			req.Info = &deviceControlA23Info{
				AlarmMethod: alarmMethod,
				AlarmType:   alarmType,
			}
		}
	case deviceControlActionIFrameSend:
		if err := g.requireGBFeature(deviceID, "iframe_control", "强制关键帧", func(c GBCapabilities) bool {
			return c.IFrameControl
		}); err != nil {
			return err
		}
		req.IFrameCmd = "Send"
	case deviceControlActionDragZoomIn:
		if err := g.requireGBFeature(deviceID, "drag_zoom_control", "拉框放大", func(c GBCapabilities) bool {
			return c.DragZoomControl
		}); err != nil {
			return err
		}
		if in.DragZoom == nil {
			return fmt.Errorf("drag_zoom_in requires drag_zoom params")
		}
		if err := validateDeviceControlDragZoom(in.DragZoom); err != nil {
			return fmt.Errorf("drag_zoom_in: %w", err)
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
		if err := g.requireGBFeature(deviceID, "drag_zoom_control", "拉框缩小", func(c GBCapabilities) bool {
			return c.DragZoomControl
		}); err != nil {
			return err
		}
		if in.DragZoom == nil {
			return fmt.Errorf("drag_zoom_out requires drag_zoom params")
		}
		if err := validateDeviceControlDragZoom(in.DragZoom); err != nil {
			return fmt.Errorf("drag_zoom_out: %w", err)
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
		if err := g.requireGBFeature(deviceID, "home_position", "看守位控制(HomePosition)", func(c GBCapabilities) bool {
			return c.HomePosition
		}); err != nil {
			return err
		}
		home := &deviceControlA23HomePosition{}
		enabled := 1
		if in.HomePosition != nil && in.HomePosition.Enabled != nil {
			enabled = *in.HomePosition.Enabled
		}
		if enabled != 0 && enabled != 1 {
			return fmt.Errorf("home_position enabled must be 0 or 1")
		}
		home.Enabled = &enabled
		if in.HomePosition != nil && in.HomePosition.ResetTime != nil {
			v := *in.HomePosition.ResetTime
			if v < 0 {
				return fmt.Errorf("home_position reset_time must be >= 0")
			}
			home.ResetTime = &v
		}
		if in.HomePosition != nil && in.HomePosition.PresetIndex != nil {
			v := *in.HomePosition.PresetIndex
			if v < 0 || v > 255 {
				return fmt.Errorf("home_position preset_index must be in [0,255]")
			}
			home.PresetIndex = &v
		}
		req.HomePosition = home
	case deviceControlActionPTZPrecise:
		if err := g.requireGBFeature(deviceID, "ptz_position", "PTZ精准控制", func(c GBCapabilities) bool {
			return c.PTZPosition
		}); err != nil {
			return err
		}
		if in.PTZPrecise == nil {
			return fmt.Errorf("ptz_precise requires ptz_precise params")
		}
		if in.PTZPrecise.Pan == nil && in.PTZPrecise.Tilt == nil && in.PTZPrecise.Zoom == nil {
			return fmt.Errorf("ptz_precise requires at least one of pan/tilt/zoom")
		}
		if in.PTZPrecise.Pan != nil && (!validFiniteRange(*in.PTZPrecise.Pan, 0, 360)) {
			return fmt.Errorf("ptz_precise pan must be in [0,360]")
		}
		if in.PTZPrecise.Tilt != nil && !validFinite(*in.PTZPrecise.Tilt) {
			return fmt.Errorf("ptz_precise tilt must be finite")
		}
		if in.PTZPrecise.Zoom != nil && !validFinite(*in.PTZPrecise.Zoom) {
			return fmt.Errorf("ptz_precise zoom must be finite")
		}
		req.PTZPreciseCtrl = &deviceControlA23PTZPrecise{
			Pan:  in.PTZPrecise.Pan,
			Tilt: in.PTZPrecise.Tilt,
			Zoom: in.PTZPrecise.Zoom,
		}
	case deviceControlActionFormatSDCard:
		if err := g.requireGBFeature(deviceID, "sdcard", "存储卡格式化控制", func(c GBCapabilities) bool {
			return c.SDCard
		}); err != nil {
			return err
		}
		if in.SDCardID < 0 {
			return fmt.Errorf("sdcard_id must be >= 0")
		}
		sd := in.SDCardID
		req.FormatSDCard = &sd
	case deviceControlActionTargetTrack:
		if err := g.requireGBFeature(deviceID, "target_track", "目标跟踪控制(TargetTrack)", func(c GBCapabilities) bool {
			return c.TargetTrack
		}); err != nil {
			return err
		}
		if in.TargetTrack == nil {
			return fmt.Errorf("target_track requires target_track params")
		}
		var mode string
		switch strings.ToLower(strings.TrimSpace(in.TargetTrack.Mode)) {
		case "auto":
			mode = "Auto"
		case "manual":
			mode = "Manual"
		case "stop":
			mode = "Stop"
		default:
			return fmt.Errorf("target_track mode must be Auto, Manual or Stop")
		}
		req.TargetTrack = mode
		req.DeviceID2 = strings.TrimSpace(in.TargetTrack.DeviceID2)
		if req.DeviceID2 != "" && !isGBDeviceIdentifier(req.DeviceID2) {
			return fmt.Errorf("target_track device_id2 must be 20 digits")
		}
		if mode == "Manual" && in.TargetTrack.TargetArea == nil {
			return fmt.Errorf("manual target_track requires target_area")
		}
		if in.TargetTrack.TargetArea != nil {
			area := in.TargetTrack.TargetArea
			if err := validateDeviceControlDragZoom(area); err != nil {
				return fmt.Errorf("invalid target_area: %w", err)
			}
			req.TargetArea = &deviceControlA23DragZoom{
				Length: area.Length, Width: area.Width, MidPointX: area.MidPointX, MidPointY: area.MidPointY,
				LengthX: area.LengthX, LengthY: area.LengthY,
			}
		}
	default:
		return fmt.Errorf("unsupported device control action: %s", action)
	}
	return nil
}

func validFiniteRange(value, minimum, maximum float64) bool {
	return validFinite(value) && value >= minimum && value <= maximum
}

func validFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validateDeviceControlDragZoom(value *DragZoomParam) error {
	if value == nil || value.Length <= 0 || value.Width <= 0 || value.LengthX <= 0 || value.LengthY <= 0 {
		return fmt.Errorf("dimensions must be positive")
	}
	if value.MidPointX < 0 || value.MidPointX > value.Length || value.MidPointY < 0 || value.MidPointY > value.Width {
		return fmt.Errorf("midpoint must be within the playback window")
	}
	return nil
}

func validateCascadeDragZoom(value *deviceControlA23DragZoom) error {
	if value == nil {
		return fmt.Errorf("DragZoom parameters are required")
	}
	return validateDeviceControlDragZoom(&DragZoomParam{
		Length: value.Length, Width: value.Width, MidPointX: value.MidPointX, MidPointY: value.MidPointY,
		LengthX: value.LengthX, LengthY: value.LengthY,
	})
}

func validateAlarmResetInfo(method, alarmType string) error {
	seen := make(map[string]struct{})
	for _, value := range strings.Split(method, "/") {
		if len(value) != 1 || value[0] < '0' || value[0] > '7' {
			return fmt.Errorf("alarm_method must be 0 or a '/'-separated combination of 1..7")
		}
		if _, ok := seen[value]; ok {
			return fmt.Errorf("alarm_method must not contain duplicate values")
		}
		seen[value] = struct{}{}
	}
	if len(seen) > 1 {
		if _, ok := seen["0"]; ok {
			return fmt.Errorf("alarm_method 0 cannot be combined with other values")
		}
	}
	if alarmType == "" {
		return nil
	}
	if len(seen) != 1 {
		return fmt.Errorf("alarm_type requires a single alarm_method of 2, 5, or 6")
	}
	maximum := 0
	switch method {
	case "2":
		maximum = 5
	case "5":
		maximum = 13
	case "6":
		maximum = 2
	default:
		return fmt.Errorf("alarm_type requires alarm_method 2, 5, or 6")
	}
	for value := 1; value <= maximum; value++ {
		if alarmType == fmt.Sprintf("%d", value) {
			return nil
		}
	}
	return fmt.Errorf("alarm_type is invalid for alarm_method %s", method)
}
