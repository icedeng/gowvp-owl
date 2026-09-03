package gbs

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"

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
	// ExtraInfo 是设备控制消息体尾部的普通文本扩展；2011/2014/2016 编码为 Info，2022 编码为 ExtraInfo。
	ExtraInfo []string

	// CameraControl 参数（A.2.3.1.2）。
	PTZCmd      string
	PTZCmdParam *PTZCmdParam
	PTZSpeed    uint8
	PTZPreset   int
	PTZGroup    uint8
	PTZAux      uint8
	PTZValue    uint16
	// ControlPriority 是 2011 规范性附录 J.6 的 PTZ 控制优先级；2014 修改补充文件继续沿用。
	ControlPriority *int

	// StreamNumber 是 2022 录像控制的可选码流编号；0 为缺省主码流。
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

	IFameCmd  string `xml:"IFameCmd,omitempty"`  // GB/T 28181-2016 标准原文拼写
	IFrameCmd string `xml:"IFrameCmd,omitempty"` // GB/T 28181-2022

	DragZoomIn  *deviceControlA23DragZoom `xml:"DragZoomIn,omitempty"`
	DragZoomOut *deviceControlA23DragZoom `xml:"DragZoomOut,omitempty"`

	HomePosition *deviceControlA23HomePosition `xml:"HomePosition,omitempty"`

	PTZPreciseCtrl *deviceControlA23PTZPrecise `xml:"PTZPreciseCtrl,omitempty"`

	FormatSDCard *int `xml:"FormatSDCard,omitempty"`

	TargetTrack string                    `xml:"TargetTrack,omitempty"`
	DeviceID2   string                    `xml:"DeviceID2,omitempty"`
	TargetArea  *deviceControlA23DragZoom `xml:"TargetArea,omitempty"`

	DeviceUpgrade *deviceUpgradeConfig `xml:"DeviceUpgrade,omitempty"`

	LegacyInfo []string `xml:"-"`
	ExtraInfo  []string `xml:"ExtraInfo,omitempty"`
}

type deviceUpgradeConfig struct {
	Firmware     string `xml:"Firmware"`
	FileURL      string `xml:"FileURL"`
	Manufacturer string `xml:"Manufacturer"`
	SessionID    string `xml:"SessionID,omitempty"`
}

type deviceControlA23Info struct {
	ControlPriority *int   `xml:"ControlPriority,omitempty"`
	AlarmMethod     string `xml:"AlarmMethod,omitempty"`
	AlarmType       string `xml:"AlarmType,omitempty"`
}

type deviceControlA23PTZCmdParam struct {
	PresetName      string `xml:"PresetName,omitempty"`
	CruiseTrackName string `xml:"CruiseTrackName,omitempty"`
}

func validatePTZCmdParams(command []byte, params *deviceControlA23PTZCmdParam) error {
	if params == nil {
		return nil
	}
	presetName := strings.TrimSpace(params.PresetName)
	cruiseTrackName := strings.TrimSpace(params.CruiseTrackName)
	if presetName != "" && cruiseTrackName != "" {
		return fmt.Errorf("PTZCmdParams must contain one command-specific name")
	}
	if presetName != "" && (len(command) != 8 || command[3] != 0x81) {
		return fmt.Errorf("PresetName is only valid for the set-preset PTZCmd")
	}
	if cruiseTrackName != "" && (len(command) != 8 || command[3] < 0x84 || command[3] > 0x88) {
		return fmt.Errorf("CruiseTrackName is only valid for a cruise PTZCmd")
	}
	return nil
}

func supportsPTZControlPriority(version GBProtocolVersion) bool {
	return version == GBVersion10 || version == GBVersion11
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

// DeviceControl 执行附录 A.2.3 设备控制命令；仅对标准规定的有应答命令等待设备 Response。
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

	version := g.getDeviceGBProtocolVersion(deviceID)
	req := deviceControlA23Request{CmdType: ptzCmdTypeDeviceControl, DeviceID: targetID}
	setDeviceControlTextInfoForVersion(&req, version, in.ExtraInfo)
	if err := validateDeviceControlExtraInfo(deviceControlTextInfo(&req)); err != nil {
		return nil, err
	}
	if err := g.fillDeviceControlRequest(deviceID, action, in, &req); err != nil {
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
	if err := g.validateTargetTrackChannels(deviceID, targetID, &req); err != nil {
		return nil, err
	}

	requiresBusinessResponse := deviceControlRequiresBusinessResponse(&req)
	operation, releaseOperation := g.trackPendingDeviceRequest(ctx, deviceID, targetID)
	defer releaseOperation()
	var (
		sn      int
		waitKey string
		pending *pendingDeviceControl
	)
	if requiresBusinessResponse {
		sn, waitKey, pending = g.reservePendingDeviceControl(deviceID, targetID, operation)
		defer g.pendingDeviceControl.CompareAndDelete(waitKey, pending)
		defer pending.operation.Cancel(nil)
	} else {
		sn = g.nextControlSN()
	}
	req.SN = sn
	body, err := encodeDeviceControlRequest(&req, version)
	if err != nil {
		return nil, err
	}

	requestCtx := operation.Context(ctx)
	tx, err := g.svr.wrapRequestContext(requestCtx, target, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		return nil, operation.ErrorOr(err)
	}
	if _, err = sipResponseContext(requestCtx, tx); err != nil {
		return nil, operation.ErrorOr(err)
	}
	if !requiresBusinessResponse {
		out := &DeviceControlOutput{
			SN:       sn,
			DeviceID: deviceID,
			TargetID: targetID,
			Result:   ptzResultOK,
		}
		if !operation.Deliver(func() {}) {
			return nil, operation.Cause()
		}
		return out, nil
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
	case <-operation.Done():
		return nil, operation.Cause()
	case <-timer.C:
		// 统一返回更明确的中文错误，避免调用方误以为命令未发送。
		return nil, fmt.Errorf("%s", ptzTimeoutErrorMessage)
	}
}

func (g *GB28181API) reservePendingDeviceControl(deviceID, targetID string, operation *pendingDeviceOperation) (int, string, *pendingDeviceControl) {
	for {
		sn := g.nextControlSN()
		key := fmt.Sprintf("%s:%d", strings.TrimSpace(deviceID), sn)
		pending := &pendingDeviceControl{
			wait:      make(chan *deviceControlResponse, 1),
			targetID:  strings.TrimSpace(targetID),
			operation: operation,
		}
		if _, loaded := g.pendingDeviceControl.LoadOrStore(key, pending); !loaded {
			return sn, key, pending
		}
	}
}

func validateDeviceControlExtraInfo(values []string, versions ...GBProtocolVersion) error {
	if len(values) == 0 {
		return nil
	}
	for _, value := range values {
		if utf8.RuneCountInString(value) > 1024 {
			return fmt.Errorf("DeviceControl text extension exceeds 1024 characters")
		}
	}
	return nil
}

func deviceControlTextInfo(request *deviceControlA23Request) []string {
	if request == nil {
		return nil
	}
	values := make([]string, 0, len(request.LegacyInfo)+len(request.ExtraInfo))
	values = append(values, request.LegacyInfo...)
	values = append(values, request.ExtraInfo...)
	return values
}

func setDeviceControlTextInfoForVersion(request *deviceControlA23Request, version GBProtocolVersion, values []string) {
	if request == nil {
		return
	}
	request.LegacyInfo = nil
	request.ExtraInfo = nil
	if version == GBVersion30 {
		request.ExtraInfo = append([]string(nil), values...)
		return
	}
	request.LegacyInfo = append([]string(nil), values...)
}

func decodeDeviceControlRequest(body []byte, request *deviceControlA23Request) error {
	if request == nil {
		return fmt.Errorf("DeviceControl request is nil")
	}
	if err := sip.XMLDecode(body, request); err != nil {
		return err
	}
	request.Info = nil
	request.LegacyInfo = nil
	decoder := sip.NewGBXMLDecoder(body)
	depth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if depth == 1 && value.Name.Local == "Info" {
				tokens, readErr := readCascadeXMLElement(decoder, value)
				if readErr != nil {
					return readErr
				}
				var encoded bytes.Buffer
				encoder := xml.NewEncoder(&encoded)
				for _, infoToken := range tokens {
					if encodeErr := encoder.EncodeToken(infoToken); encodeErr != nil {
						return encodeErr
					}
				}
				if flushErr := encoder.Flush(); flushErr != nil {
					return flushErr
				}
				structured := false
				for _, infoToken := range tokens[1 : len(tokens)-1] {
					if _, ok := infoToken.(xml.StartElement); ok {
						structured = true
						break
					}
				}
				if structured {
					if request.Info != nil {
						return fmt.Errorf("DeviceControl contains multiple structured Info elements")
					}
					var info deviceControlA23Info
					if decodeErr := sip.XMLDecode(encoded.Bytes(), &info); decodeErr != nil {
						return decodeErr
					}
					request.Info = &info
				} else {
					var text string
					if decodeErr := sip.XMLDecode(encoded.Bytes(), &text); decodeErr != nil {
						return decodeErr
					}
					request.LegacyInfo = append(request.LegacyInfo, text)
				}
				continue
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
}

func encodeDeviceControlRequest(request *deviceControlA23Request, version GBProtocolVersion) ([]byte, error) {
	if request == nil {
		return nil, fmt.Errorf("DeviceControl request is nil")
	}
	copyRequest := *request
	values := deviceControlTextInfo(&copyRequest)
	setDeviceControlTextInfoForVersion(&copyRequest, version, values)
	if version == GBVersion30 {
		// encoding/xml 会因 omitempty 丢弃切片中的空字符串；扩展项允许为空，改为在根节点尾部显式写出。
		copyRequest.ExtraInfo = nil
	}
	body, err := sip.XMLEncode(copyRequest)
	if err != nil {
		return body, err
	}
	if version == GBVersion30 && len(values) == 0 {
		return body, nil
	}
	decoder := sip.NewGBXMLDecoder(body)
	var output bytes.Buffer
	encoder := xml.NewEncoder(&output)
	depth := 0
	structuredInfo := make([]xml.Token, 0)
	for {
		token, decodeErr := decoder.Token()
		if decodeErr == io.EOF {
			break
		}
		if decodeErr != nil {
			return nil, decodeErr
		}
		if start, ok := token.(xml.StartElement); ok && version != GBVersion30 && depth == 1 && start.Name.Local == "Info" {
			tokens, readErr := readCascadeXMLElement(decoder, start)
			if readErr != nil {
				return nil, readErr
			}
			structuredInfo = append(structuredInfo, tokens...)
			continue
		}
		if _, ok := token.(xml.EndElement); ok && depth == 1 {
			if version != GBVersion30 {
				for _, infoToken := range structuredInfo {
					if encodeErr := encoder.EncodeToken(infoToken); encodeErr != nil {
						return nil, encodeErr
					}
				}
			}
			name := "Info"
			if version == GBVersion30 {
				name = "ExtraInfo"
			}
			for _, value := range values {
				if encodeErr := encoder.EncodeElement(value, xml.StartElement{Name: xml.Name{Local: name}}); encodeErr != nil {
					return nil, encodeErr
				}
			}
		}
		if _, ok := token.(xml.StartElement); ok {
			depth++
		} else if _, ok := token.(xml.EndElement); ok {
			depth--
		}
		if encodeErr := encoder.EncodeToken(token); encodeErr != nil {
			return nil, encodeErr
		}
	}
	if err := encoder.Flush(); err != nil {
		return nil, err
	}
	return sip.EncodeGBXMLDocument(output.Bytes())
}

// validateTargetTrackChannels 校验 2022 目标跟踪的球机通道及可选全景通道属于同一设备。
func (g *GB28181API) validateTargetTrackChannels(deviceID, targetID string, request *deviceControlA23Request) error {
	if request == nil || strings.TrimSpace(request.TargetTrack) == "" {
		return nil
	}
	deviceID = strings.TrimSpace(deviceID)
	targetID = strings.TrimSpace(targetID)
	if targetID == "" || targetID == deviceID {
		return fmt.Errorf("target_track requires a ball camera channel target")
	}
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return ErrChannelNotExist
	}
	if _, ok := g.svr.memoryStorer.GetChannel(deviceID, targetID); !ok {
		return ErrChannelNotExist
	}
	panoramaID := strings.TrimSpace(request.DeviceID2)
	if panoramaID == "" {
		return nil
	}
	if panoramaID == targetID {
		return fmt.Errorf("target_track device_id2 must differ from the ball camera channel")
	}
	if _, ok := g.svr.memoryStorer.GetChannel(deviceID, panoramaID); !ok {
		return fmt.Errorf("target_track device_id2 must reference a channel of the target device")
	}
	return nil
}

// deviceControlRequiresBusinessResponse 对应各版 9.3 的有/无应答命令矩阵。
// 明确列出的无应答控制只以 SIP 200 OK 确认接收；未知或后续新增控制默认要求业务应答，避免静默误判成功。
func deviceControlRequiresBusinessResponse(request *deviceControlA23Request) bool {
	if request == nil {
		return false
	}
	return !(strings.TrimSpace(request.PTZCmd) != "" ||
		strings.TrimSpace(request.TeleBoot) != "" ||
		strings.TrimSpace(request.IFameCmd) != "" ||
		strings.TrimSpace(request.IFrameCmd) != "" ||
		request.DragZoomIn != nil ||
		request.DragZoomOut != nil ||
		request.PTZPreciseCtrl != nil ||
		request.FormatSDCard != nil ||
		strings.TrimSpace(request.TargetTrack) != "")
}

func deviceControlIFrameCommand(request *deviceControlA23Request) (string, bool, error) {
	if request == nil {
		return "", false, nil
	}
	legacy := strings.TrimSpace(request.IFameCmd)
	current := strings.TrimSpace(request.IFrameCmd)
	if legacy != "" && current != "" {
		return "", false, fmt.Errorf("DeviceControl must not contain both IFameCmd and IFrameCmd")
	}
	if legacy != "" {
		return legacy, true, nil
	}
	if current != "" {
		return current, true, nil
	}
	return "", false, nil
}

func validateDeviceControlIFrameCommand(request *deviceControlA23Request, version GBProtocolVersion) (string, bool, error) {
	command, present, err := deviceControlIFrameCommand(request)
	if err != nil || !present {
		return command, present, err
	}
	if !version.Capabilities().IFrameControl {
		return "", false, fmt.Errorf("force-I-frame control is not supported by protocol %s", version)
	}
	switch version {
	case GBVersion20:
		if strings.TrimSpace(request.IFameCmd) == "" {
			return "", false, fmt.Errorf("protocol 2.0 force-I-frame control requires IFameCmd")
		}
	case GBVersion30:
		if strings.TrimSpace(request.IFrameCmd) == "" {
			return "", false, fmt.Errorf("protocol 3.0 force-I-frame control requires IFrameCmd")
		}
	default:
		return "", false, fmt.Errorf("force-I-frame control is not supported by protocol %s", version)
	}
	if !strings.EqualFold(command, "Send") {
		return "", false, fmt.Errorf("unsupported force-I-frame command")
	}
	return command, true, nil
}

func setDeviceControlIFrameCommand(request *deviceControlA23Request, version GBProtocolVersion, command string) error {
	if request == nil {
		return fmt.Errorf("DeviceControl request is nil")
	}
	request.IFameCmd = ""
	request.IFrameCmd = ""
	switch version {
	case GBVersion20:
		request.IFameCmd = command
	case GBVersion30:
		request.IFrameCmd = command
	default:
		return fmt.Errorf("force-I-frame control is not supported by protocol %s", version)
	}
	return nil
}

func (g *GB28181API) setDeviceControlRecordCommand(deviceID, command string, streamNumber int, request *deviceControlA23Request) error {
	if streamNumber < 0 {
		return fmt.Errorf("stream_number must be >= 0")
	}
	request.RecordCmd = command
	if streamNumber == 0 {
		return nil
	}
	if g.getDeviceGBProtocolVersion(deviceID) != GBVersion30 {
		return fmt.Errorf("stream_number is only supported by GB/T 28181-2022")
	}
	request.StreamNumber = &streamNumber
	return nil
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
	case "iframe", "iframe_cmd", "ifame_cmd":
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
		ptzCmd := strings.TrimSpace(in.PTZCmd)
		if ptzCmd == "" {
			return fmt.Errorf("camera_control requires ptz_cmd")
		}
		if len(ptzCmd) == 16 {
			if _, err := parsePTZCommand(ptzCmd); err != nil {
				return err
			}
			req.PTZCmd = strings.ToUpper(ptzCmd)
		} else {
			encoded, err := encodePTZCommand(&PTZInput{
				Action: PTZAction(ptzCmd), Speed: in.PTZSpeed, Preset: in.PTZPreset,
				Group: in.PTZGroup, Aux: in.PTZAux, Value: in.PTZValue,
			})
			if err != nil {
				return err
			}
			req.PTZCmd = encoded
		}
		if in.PTZCmdParam != nil {
			presetNamePresent := strings.TrimSpace(in.PTZCmdParam.PresetName) != ""
			cruiseTrackNamePresent := strings.TrimSpace(in.PTZCmdParam.CruiseTrackName) != ""
			if presetNamePresent || cruiseTrackNamePresent {
				if err := g.requireGBVersionAtLeast(deviceID, gbVersion2022, "云台控制附加参数"); err != nil {
					return err
				}
				if len(in.PTZCmdParam.CruiseTrackName) > 32 {
					return fmt.Errorf("cruise_track_name must not exceed 32 bytes")
				}
				params := &deviceControlA23PTZCmdParam{
					PresetName:      in.PTZCmdParam.PresetName,
					CruiseTrackName: in.PTZCmdParam.CruiseTrackName,
				}
				command, err := parsePTZCommand(req.PTZCmd)
				if err != nil {
					return err
				}
				if err := validatePTZCmdParams(command, params); err != nil {
					return err
				}
				req.PTZCmdParams = params
			}
		}
		if in.ControlPriority != nil {
			version := g.getDeviceGBProtocolVersion(deviceID)
			if !supportsPTZControlPriority(version) {
				return fmt.Errorf("control_priority is only supported by GB/T 28181-2011/2014")
			}
			priority := *in.ControlPriority
			req.Info = &deviceControlA23Info{ControlPriority: &priority}
		}
	case deviceControlActionTeleBoot:
		req.TeleBoot = "Boot"
	case deviceControlActionRecordStart:
		if err := g.setDeviceControlRecordCommand(deviceID, "Record", in.StreamNumber, req); err != nil {
			return err
		}
	case deviceControlActionRecordStop:
		if err := g.setDeviceControlRecordCommand(deviceID, "StopRecord", in.StreamNumber, req); err != nil {
			return err
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
		if err := setDeviceControlIFrameCommand(req, g.getDeviceGBProtocolVersion(deviceID), "Send"); err != nil {
			return err
		}
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
			// A.2.3.1.10 只声明 ResetTime 为 integer，未定义非负范围。
			home.ResetTime = &v
		}
		if in.HomePosition != nil && in.HomePosition.PresetIndex != nil {
			v := *in.HomePosition.PresetIndex
			if v < 0 || v > 255 {
				return fmt.Errorf("home_position preset_index must be in [0,255]")
			}
			home.PresetIndex = &v
		}
		if enabled == 0 && (home.ResetTime != nil || home.PresetIndex != nil) {
			return fmt.Errorf("home_position reset_time and preset_index require enabled=1")
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
		if in.PTZPrecise.Tilt != nil && !validFiniteRange(*in.PTZPrecise.Tilt, -30, 90) {
			return fmt.Errorf("ptz_precise tilt must be in [-30,90]")
		}
		if in.PTZPrecise.Zoom != nil && (!validFinite(*in.PTZPrecise.Zoom) || *in.PTZPrecise.Zoom < 1) {
			return fmt.Errorf("ptz_precise zoom must be finite and >= 1.0")
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
		if mode != "Manual" && in.TargetTrack.TargetArea != nil {
			return fmt.Errorf("target_area is only valid for manual target_track")
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
	if err := validateAlarmTypeForMethod(GBVersion30, method, alarmType); err != nil {
		return fmt.Errorf("alarm_type is invalid for alarm_method %s", method)
	}
	return nil
}
