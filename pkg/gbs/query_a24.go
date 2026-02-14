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
	deviceQueryActionCatalog           = "catalog"
	deviceQueryActionBroadcast         = "broadcast"
	deviceQueryActionDeviceInfo        = "device_info"
	deviceQueryActionDeviceStatus      = "device_status"
	deviceQueryActionRecordInfo        = "record_info"
	deviceQueryActionPresetQuery       = "preset_query"
	deviceQueryActionHomePositionQuery = "home_position_query"
	deviceQueryActionPTZPosition       = "ptz_position"
	deviceQueryActionSDCardStatus      = "sdcard_status"
	deviceQueryActionConfigDownload    = "config_download"
	deviceQueryActionMobilePosition    = "mobile_position"
)

// DeviceQueryInput 是附录 A.2.4 设备查询命令输入。
//
// 说明：
// 1. DeviceID 为设备国标 ID（必填）。
// 2. TargetID 为空时默认查询设备本身；查询通道时传通道国标 ID。
// 3. Action 为统一查询动作名，内部映射到 CmdType。
type DeviceQueryInput struct {
	DeviceID string
	TargetID string
	Action   string
	Timeout  time.Duration

	// ConfigDownload 查询参数。
	ConfigType string
	// MobilePosition 查询参数（秒）。
	Interval int
	// RecordInfo 查询参数（unix 秒）。
	Start int64
	End   int64
}

// DeviceQueryOutput 是统一查询返回。
// XML 字段保留设备原始响应，便于上层按厂商差异继续解析。
type DeviceQueryOutput struct {
	SN       int    `json:"sn"`
	CmdType  string `json:"cmd_type"`
	DeviceID string `json:"device_id"`
	Result   string `json:"result,omitempty"`
	XML      string `json:"xml"`
	Data     any    `json:"data,omitempty"`
}

type pendingQueryWait struct {
	wait chan *DeviceQueryOutput
}

type genericDeviceQueryResponse struct {
	CmdType  string `xml:"CmdType"`
	SN       int    `xml:"SN"`
	DeviceID string `xml:"DeviceID"`
	Result   string `xml:"Result"`
}

type genericDeviceQueryRequest struct {
	XMLName    xml.Name `xml:"Query"`
	CmdType    string   `xml:"CmdType"`
	SN         int      `xml:"SN"`
	DeviceID   string   `xml:"DeviceID"`
	ConfigType string   `xml:"ConfigType,omitempty"`
	Interval   *int     `xml:"Interval,omitempty"`
}

// DeviceQuery 执行附录 A.2.4 查询命令，并等待设备响应。
func (g *GB28181API) DeviceQuery(_ context.Context, in *DeviceQueryInput) (*DeviceQueryOutput, error) {
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
	action := normalizeDeviceQueryAction(in.Action)

	configType := strings.TrimSpace(in.ConfigType)
	cmdType, err := g.resolveDeviceQueryCmdType(deviceID, action, configType)
	if err != nil {
		return nil, err
	}
	if cmdType == "RecordInfo" {
		if targetID == "" || targetID == deviceID {
			return nil, fmt.Errorf("record_info requires target channel id")
		}
		if in.Start <= 0 || in.End <= in.Start {
			return nil, fmt.Errorf("record_info requires valid start/end")
		}
		records, err := g.QueryRecordList(context.Background(), &RecordQueryInput{
			DeviceID:  deviceID,
			ChannelID: targetID,
			Start:     in.Start,
			End:       in.End,
			Timeout:   in.Timeout,
		})
		if err != nil {
			return nil, err
		}
		return &DeviceQueryOutput{
			SN:       g.nextQuerySN(),
			CmdType:  "RecordInfo",
			DeviceID: targetID,
			Result:   "OK",
			Data:     records,
		}, nil
	}

	sn := g.nextQuerySN()
	req := genericDeviceQueryRequest{
		CmdType:  cmdType,
		SN:       sn,
		DeviceID: targetID,
	}
	if cmdType == "ConfigDownload" {
		canonical, _ := normalizeConfigType(configType)
		req.ConfigType = canonical
	}
	if cmdType == "MobilePosition" && in.Interval > 0 {
		interval := in.Interval
		req.Interval = &interval
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

	waitKey := buildPendingQueryKey(deviceID, cmdType, sn)
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	g.pendingDeviceQuery.Store(waitKey, pending)
	defer g.pendingDeviceQuery.Delete(waitKey)

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
	case out := <-pending.wait:
		return out, nil
	case <-timer.C:
		return nil, fmt.Errorf("wait query response timeout")
	}
}

func normalizeDeviceQueryAction(action string) string {
	a := strings.ToLower(strings.TrimSpace(action))
	a = strings.ReplaceAll(a, "-", "_")
	switch a {
	case "status", "device_status_query":
		return deviceQueryActionDeviceStatus
	case "file_query", "record_query", "recordinfo", "record_info_query":
		return deviceQueryActionRecordInfo
	case "ptz_precise_status", "ptz_position_query":
		return deviceQueryActionPTZPosition
	case "sd_card_status":
		return deviceQueryActionSDCardStatus
	default:
		return a
	}
}

func (g *GB28181API) resolveDeviceQueryCmdType(deviceID, action, configType string) (string, error) {
	switch action {
	case deviceQueryActionCatalog:
		return "Catalog", nil
	case deviceQueryActionBroadcast:
		return "Broadcast", nil
	case deviceQueryActionDeviceInfo:
		return "DeviceInfo", nil
	case deviceQueryActionDeviceStatus:
		return "DeviceStatus", nil
	case deviceQueryActionRecordInfo:
		return "RecordInfo", nil
	case deviceQueryActionPresetQuery:
		return "PresetQuery", nil
	case deviceQueryActionHomePositionQuery:
		if err := g.requireGBVersionAtLeast(deviceID, gbVersion2022, "看守位查询(HomePositionQuery)"); err != nil {
			return "", err
		}
		return "HomePositionQuery", nil
	case deviceQueryActionPTZPosition:
		if err := g.requireGBVersionAtLeast(deviceID, gbVersion2022, "PTZ精准状态查询"); err != nil {
			return "", err
		}
		return "PTZPosition", nil
	case deviceQueryActionSDCardStatus:
		if err := g.requireGBVersionAtLeast(deviceID, gbVersion2022, "存储卡状态查询"); err != nil {
			return "", err
		}
		return "SDCardStatus", nil
	case deviceQueryActionConfigDownload:
		if configType == "" {
			return "", fmt.Errorf("config_type is required for config_download")
		}
		canonical, ok := normalizeConfigType(configType)
		if !ok {
			return "", fmt.Errorf("unsupported config_type: %s", configType)
		}
		if err := g.requireConfigTypeVersion(deviceID, canonical); err != nil {
			return "", err
		}
		return "ConfigDownload", nil
	case deviceQueryActionMobilePosition:
		return "MobilePosition", nil
	default:
		return "", fmt.Errorf("unsupported device query action: %s", action)
	}
}

func (g *GB28181API) requireConfigTypeVersion(deviceID, configType string) error {
	name := strings.TrimSpace(configType)
	switch name {
	case "BasicParam":
		return nil
	case "VideoParamOpt", "SVACEncodeConfig", "SVACDecodeConfig", "VideoParamAttribute", "VideoRecordPlan",
		"VideoAlarmRecord", "PictureMask", "FrameMirror", "AlarmReport", "OSDConfig", "SnapShot":
		return g.requireGBVersionAtLeast(deviceID, gbVersion2022, "配置查询("+name+")")
	default:
		return fmt.Errorf("unsupported config_type: %s", name)
	}
}

func normalizeConfigType(configType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(configType)) {
	case "basicparam":
		return "BasicParam", true
	case "videoparamopt":
		return "VideoParamOpt", true
	case "svacencodeconfig":
		return "SVACEncodeConfig", true
	case "svacdecodeconfig":
		return "SVACDecodeConfig", true
	case "videoparamattribute":
		return "VideoParamAttribute", true
	case "videorecordplan":
		return "VideoRecordPlan", true
	case "videoalarmrecord":
		return "VideoAlarmRecord", true
	case "picturemask":
		return "PictureMask", true
	case "framemirror":
		return "FrameMirror", true
	case "alarmreport":
		return "AlarmReport", true
	case "osdconfig":
		return "OSDConfig", true
	case "snapshot":
		return "SnapShot", true
	default:
		return "", false
	}
}

func buildPendingQueryKey(deviceID, cmdType string, sn int) string {
	return fmt.Sprintf("%s:%s:%d", strings.TrimSpace(deviceID), strings.ToUpper(strings.TrimSpace(cmdType)), sn)
}

func (g *GB28181API) resolvePendingDeviceQuery(deviceID, cmdType string, sn int, result string, body []byte, targetID string) {
	cmdType = strings.TrimSpace(cmdType)
	if sn <= 0 || cmdType == "" {
		return
	}
	keys := []string{buildPendingQueryKey(deviceID, cmdType, sn)}
	if targetID != "" && targetID != deviceID {
		keys = append(keys, buildPendingQueryKey(targetID, cmdType, sn))
	}
	for _, key := range keys {
		v, ok := g.pendingDeviceQuery.Load(key)
		if !ok {
			continue
		}
		out := &DeviceQueryOutput{
			SN:       sn,
			CmdType:  cmdType,
			DeviceID: strings.TrimSpace(targetID),
			Result:   strings.TrimSpace(result),
			XML:      string(append([]byte(nil), body...)),
		}
		if out.DeviceID == "" {
			out.DeviceID = strings.TrimSpace(deviceID)
		}
		out.Data = g.decodeAndStoreQueryData(out.DeviceID, out.CmdType, body)
		select {
		case v.(*pendingQueryWait).wait <- out:
		default:
		}
		return
	}
}

// sipMessageQueryGeneric 处理通用查询响应（A.2.4 补齐项）。
func (g *GB28181API) sipMessageQueryGeneric(ctx *sip.Context) {
	var msg genericDeviceQueryResponse
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	g.resolvePendingDeviceQuery(ctx.DeviceID, msg.CmdType, msg.SN, msg.Result, ctx.Request.Body(), msg.DeviceID)
	deviceID := strings.TrimSpace(ctx.DeviceID)
	if deviceID == "" {
		deviceID = strings.TrimSpace(msg.DeviceID)
	}
	// 解析并落入结构化状态缓存。
	g.decodeAndStoreQueryData(deviceID, msg.CmdType, ctx.Request.Body())
	// 9.11 事件源侧：通用查询类事件通知。
	g.publishEventNotify(msg.CmdType, deviceID, ctx.Request.Body())
	ctx.String(200, "OK")
}
