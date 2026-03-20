package gbs

import (
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	ptzCmdTypeDeviceControl = "DeviceControl"
	ptzResultOK             = "OK"
	ptzResultWeakConfirmed  = "SENT_NO_RESPONSE"
)

const ptzTimeoutErrorMessage = "命令已发送，但设备未返回控制应答"

type PTZAction string

const (
	PTZActionStop         PTZAction = "stop"
	PTZActionLeft         PTZAction = "left"
	PTZActionRight        PTZAction = "right"
	PTZActionUp           PTZAction = "up"
	PTZActionDown         PTZAction = "down"
	PTZActionLeftUp       PTZAction = "left_up"
	PTZActionLeftDown     PTZAction = "left_down"
	PTZActionRightUp      PTZAction = "right_up"
	PTZActionRightDown    PTZAction = "right_down"
	PTZActionZoomIn       PTZAction = "zoom_in"
	PTZActionZoomOut      PTZAction = "zoom_out"
	PTZActionIrisOpen     PTZAction = "iris_open"
	PTZActionIrisClose    PTZAction = "iris_close"
	PTZActionIrisAdd      PTZAction = "iris_add"
	PTZActionIrisSub      PTZAction = "iris_sub"
	PTZActionFocusNear    PTZAction = "focus_near"
	PTZActionFocusFar     PTZAction = "focus_far"
	PTZActionFocusAdd     PTZAction = "focus_add"
	PTZActionFocusSub     PTZAction = "focus_sub"
	PTZActionPresetSet    PTZAction = "preset_set"
	PTZActionPresetCall   PTZAction = "preset_call"
	PTZActionPresetDelete PTZAction = "preset_delete"
	PTZActionCruiseAdd    PTZAction = "cruise_add"
	PTZActionCruiseDel    PTZAction = "cruise_del"
	PTZActionCruiseSpeed  PTZAction = "cruise_speed"
	PTZActionCruiseStay   PTZAction = "cruise_stay"
	PTZActionCruiseStart  PTZAction = "cruise_start"
	PTZActionScanStart    PTZAction = "scan_start"
	PTZActionScanLeft     PTZAction = "scan_left"
	PTZActionScanRight    PTZAction = "scan_right"
	PTZActionScanSpeed    PTZAction = "scan_speed"
	PTZActionAuxOn        PTZAction = "aux_on"
	PTZActionAuxOff       PTZAction = "aux_off"
)

type PTZInput struct {
	DeviceID  string
	ChannelID string
	Action    PTZAction
	Speed     uint8
	Timeout   time.Duration
	Preset    int
	Group     uint8
	Aux       uint8
	Value     uint16
}

type PTZOutput struct {
	SN       int    `json:"sn"`
	DeviceID string `json:"device_id"`
	Channel  string `json:"channel"`
	Result   string `json:"result"`
}

type deviceControlRequest struct {
	XMLName   xml.Name `xml:"Control"`
	CmdType   string   `xml:"CmdType"`
	SN        int      `xml:"SN"`
	DeviceID  string   `xml:"DeviceID"`
	PTZCmd    string   `xml:"PTZCmd,omitempty"`
	ExtraInfo string   `xml:"ExtralInfo,omitempty"`
}

type deviceControlResponse struct {
	XMLName  xml.Name `xml:"Response"`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	DeviceID string   `xml:"DeviceID"`
	Result   string   `xml:"Result"`
}

type pendingDeviceControl struct {
	wait chan *deviceControlResponse
}

func (g *GB28181API) PTZ(in *PTZInput) (*PTZOutput, error) {
	if in == nil || in.ChannelID == "" {
		return nil, errors.New("invalid ptz input")
	}
	ipc, ok := g.svr.memoryStorer.Load(in.DeviceID)
	if !ok || !ipc.IsOnline {
		return nil, ErrDeviceOffline
	}
	ch, ok := g.svr.memoryStorer.GetChannel(in.DeviceID, in.ChannelID)
	if !ok {
		return nil, errors.New("channel offline")
	}

	if in.Speed == 0 {
		in.Speed = 40
	}
	if in.Timeout <= 0 {
		in.Timeout = 6 * time.Second
	}

	sn := g.nextControlSN()
	cmd, err := encodePTZCommand(in)
	if err != nil {
		return nil, err
	}
	req := deviceControlRequest{
		CmdType:  ptzCmdTypeDeviceControl,
		SN:       sn,
		DeviceID: in.ChannelID,
		PTZCmd:   cmd,
	}
	body, err := sip.XMLEncode(req)
	if err != nil {
		return nil, err
	}

	waitKey := fmt.Sprintf("%s:%d", in.DeviceID, sn)
	pending := &pendingDeviceControl{wait: make(chan *deviceControlResponse, 1)}
	g.pendingDeviceControl.Store(waitKey, pending)
	defer g.pendingDeviceControl.Delete(waitKey)

	tx, err := g.svr.wrapRequest(ch, sip.MethodMessage, &sip.ContentTypeXML, body)
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
		return &PTZOutput{
			SN:       sn,
			DeviceID: in.DeviceID,
			Channel:  in.ChannelID,
			Result:   result,
		}, nil
	case <-timer.C:
		if g.cfg != nil && g.cfg.PTZWeakConfirm {
			// 弱确认模式：SIP MESSAGE 已经收到 200 OK，只是设备未回业务 Response。
			// 对部分“执行命令但不回 DeviceControl 应答”的厂商设备，按已发送成功处理。
			return &PTZOutput{
				SN:       sn,
				DeviceID: in.DeviceID,
				Channel:  in.ChannelID,
				Result:   ptzResultWeakConfirmed,
			}, nil
		}
		return nil, fmt.Errorf("%s", ptzTimeoutErrorMessage)
	}
}

func (g *GB28181API) sipMessageDeviceControl(ctx *sip.Context) {
	var msg deviceControlResponse
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	if msg.SN <= 0 {
		ctx.String(200, "OK")
		return
	}

	waitKey := fmt.Sprintf("%s:%d", ctx.DeviceID, msg.SN)
	if v, ok := g.pendingDeviceControl.Load(waitKey); ok {
		select {
		case v.(*pendingDeviceControl).wait <- &msg:
		default:
		}
	}
	ctx.String(200, "OK")
}

func encodePTZCommand(in *PTZInput) (string, error) {
	action := PTZAction(strings.ToLower(strings.TrimSpace(string(in.Action))))
	speed := in.Speed
	preset := in.Preset
	group := in.Group
	aux := in.Aux
	value := in.Value

	if group == 0 {
		group = 1
	}
	if value == 0 {
		value = uint16(speed)
	}

	switch action {
	case "ptz_stop":
		action = PTZActionStop
	case "set_preset":
		action = PTZActionPresetSet
	case "goto_preset":
		action = PTZActionPresetCall
	case "del_preset", "remove_preset":
		action = PTZActionPresetDelete
	case "focus_plus":
		action = PTZActionFocusAdd
	case "focus_minus":
		action = PTZActionFocusSub
	case "aperture_add":
		action = PTZActionIrisAdd
	case "aperture_sub":
		action = PTZActionIrisSub
	case "cruise_add":
		action = PTZActionCruiseAdd
	case "cruise_del":
		action = PTZActionCruiseDel
	case "cruise_speed":
		action = PTZActionCruiseSpeed
	case "cruise_stay":
		action = PTZActionCruiseStay
	case "cruise_start":
		action = PTZActionCruiseStart
	case "scan_start":
		action = PTZActionScanStart
	case "scan_left":
		action = PTZActionScanLeft
	case "scan_right":
		action = PTZActionScanRight
	case "scan_speed":
		action = PTZActionScanSpeed
	case "aux_on":
		action = PTZActionAuxOn
	case "aux_off":
		action = PTZActionAuxOff
	}

	if action != PTZActionStop && !isPresetCmd(action) && speed == 0 {
		speed = 40
	}

	cmd := make([]byte, 8)
	cmd[0] = 0xA5
	cmd[1] = 0x0F
	cmd[2] = 0x01

	switch action {
	case PTZActionCruiseAdd:
		if preset < 1 || preset > 255 {
			return "", fmt.Errorf("preset must be in [1,255]")
		}
		cmd[3] = 0x84
		cmd[4] = group
		cmd[5] = byte(preset)
		cmd[6] = 0x00
		return finalizePTZCmd(cmd), nil
	case PTZActionCruiseDel:
		if preset < 1 || preset > 255 {
			return "", fmt.Errorf("preset must be in [1,255]")
		}
		cmd[3] = 0x85
		cmd[4] = group
		cmd[5] = byte(preset)
		cmd[6] = 0x00
		return finalizePTZCmd(cmd), nil
	case PTZActionCruiseSpeed:
		cmd[3] = 0x86
		cmd[4] = group
		set12BitValue(cmd, value)
		return finalizePTZCmd(cmd), nil
	case PTZActionCruiseStay:
		cmd[3] = 0x87
		cmd[4] = group
		set12BitValue(cmd, value)
		return finalizePTZCmd(cmd), nil
	case PTZActionCruiseStart:
		cmd[3] = 0x88
		cmd[4] = group
		cmd[5] = 0x00
		cmd[6] = 0x00
		return finalizePTZCmd(cmd), nil
	case PTZActionScanStart:
		cmd[3] = 0x89
		cmd[4] = group
		cmd[5] = 0x00
		cmd[6] = 0x00
		return finalizePTZCmd(cmd), nil
	case PTZActionScanLeft:
		cmd[3] = 0x89
		cmd[4] = group
		cmd[5] = 0x01
		cmd[6] = 0x00
		return finalizePTZCmd(cmd), nil
	case PTZActionScanRight:
		cmd[3] = 0x89
		cmd[4] = group
		cmd[5] = 0x02
		cmd[6] = 0x00
		return finalizePTZCmd(cmd), nil
	case PTZActionScanSpeed:
		cmd[3] = 0x8A
		cmd[4] = group
		set12BitValue(cmd, value)
		return finalizePTZCmd(cmd), nil
	case PTZActionAuxOn:
		cmd[3] = 0x8C
		cmd[4] = aux
		set12BitValue(cmd, value)
		return finalizePTZCmd(cmd), nil
	case PTZActionAuxOff:
		cmd[3] = 0x8D
		cmd[4] = aux
		set12BitValue(cmd, value)
		return finalizePTZCmd(cmd), nil
	}

	if isFICmd(action) {
		cmd[3] = 0x40
		switch action {
		case PTZActionIrisClose, PTZActionIrisSub:
			cmd[3] |= 0x08 // bit3: iris close
			cmd[5] = speed
		case PTZActionIrisOpen, PTZActionIrisAdd:
			cmd[3] |= 0x04 // bit2: iris open
			cmd[5] = speed
		case PTZActionFocusNear, PTZActionFocusAdd:
			cmd[3] |= 0x02 // bit1: focus near
			cmd[4] = speed
		case PTZActionFocusFar, PTZActionFocusSub:
			cmd[3] |= 0x01 // bit0: focus far
			cmd[4] = speed
		default:
			return "", fmt.Errorf("unsupported fi action: %s", action)
		}
		return finalizePTZCmd(cmd), nil
	}

	if isPresetCmd(action) {
		if preset < 1 || preset > 255 {
			return "", fmt.Errorf("preset must be in [1,255]")
		}
		switch action {
		case PTZActionPresetSet:
			cmd[3] = 0x81
		case PTZActionPresetCall:
			cmd[3] = 0x82
		case PTZActionPresetDelete:
			cmd[3] = 0x83
		default:
			return "", fmt.Errorf("unsupported preset action: %s", action)
		}
		cmd[4] = 0x00
		cmd[5] = byte(preset)
		cmd[6] = 0x00
		return finalizePTZCmd(cmd), nil
	}

	if action == PTZActionStop {
		// A.5 停止示例：控制字节为 0，速度字段为 0
		cmd[3] = 0x00
		cmd[4] = 0x00
		cmd[5] = 0x00
		cmd[6] = 0x00
		return finalizePTZCmd(cmd), nil
	}

	var (
		left, right, up, down uint8
		zoomIn, zoomOut       uint8
	)

	switch action {
	case PTZActionLeft:
		left = 1
	case PTZActionRight:
		right = 1
	case PTZActionUp:
		up = 1
	case PTZActionDown:
		down = 1
	case PTZActionLeftUp:
		left, up = 1, 1
	case PTZActionLeftDown:
		left, down = 1, 1
	case PTZActionRightUp:
		right, up = 1, 1
	case PTZActionRightDown:
		right, down = 1, 1
	case PTZActionZoomIn:
		zoomIn = 1
	case PTZActionZoomOut:
		zoomOut = 1
	default:
		return "", fmt.Errorf("unsupported ptz action: %s", action)
	}

	cmd[3] = ((zoomOut & 0x01) << 6) |
		((zoomIn & 0x01) << 5) |
		((down & 0x01) << 3) |
		((up & 0x01) << 2) |
		((left & 0x01) << 1) |
		(right & 0x01)
	cmd[4] = speed
	cmd[5] = speed
	cmd[6] = speed << 4

	return finalizePTZCmd(cmd), nil
}

func finalizePTZCmd(cmd []byte) string {
	sum := uint16(0)
	for i := 0; i < 7; i++ {
		sum += uint16(cmd[i])
	}
	cmd[7] = byte(sum % 0x100)
	return strings.ToUpper(hex.EncodeToString(cmd))
}

func set12BitValue(cmd []byte, value uint16) {
	if value > 0x0FFF {
		value = 0x0FFF
	}
	cmd[5] = byte(value & 0xFF)
	cmd[6] = byte((value >> 8) << 4)
}

func isFICmd(action PTZAction) bool {
	switch action {
	case PTZActionIrisOpen, PTZActionIrisClose, PTZActionIrisAdd, PTZActionIrisSub,
		PTZActionFocusNear, PTZActionFocusFar, PTZActionFocusAdd, PTZActionFocusSub:
		return true
	default:
		return false
	}
}

func isPresetCmd(action PTZAction) bool {
	switch action {
	case PTZActionPresetSet, PTZActionPresetCall, PTZActionPresetDelete:
		return true
	default:
		return false
	}
}
