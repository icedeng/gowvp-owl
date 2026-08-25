package gbs

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"sync"
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
	deviceQueryActionCruiseTrackList   = "cruise_track_list"
	deviceQueryActionCruiseTrack       = "cruise_track"
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
	// CruiseTrackQuery 轨迹编号；标准当前定义 0、1 两条轨迹。
	Number int
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
	// AppendixA4 为附录 A.4 扩展对象结构化结果。
	AppendixA4 []AppendixA4Object `json:"appendix_a4,omitempty"`
}

type pendingQueryWait struct {
	wait           chan *DeviceQueryOutput
	mu             sync.Mutex
	expectedConfig map[string]struct{}
	config         *ConfigDownloadState
}

type genericDeviceQueryResponse struct {
	XMLName  xml.Name
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
	Number     *int     `xml:"Number,omitempty"`
}

// DeviceQuery 执行附录 A.2.4 查询命令，并等待设备响应。
func (g *GB28181API) DeviceQuery(ctx context.Context, in *DeviceQueryInput) (*DeviceQueryOutput, error) {
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
		records, err := g.QueryRecordList(ctx, &RecordQueryInput{
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
		canonical, _ := normalizeConfigTypes(configType)
		req.ConfigType = canonical
	}
	if cmdType == "MobilePosition" && in.Interval > 0 {
		interval := in.Interval
		req.Interval = &interval
	}
	if cmdType == "CruiseTrackQuery" {
		if in.Number < 0 || in.Number > 1 {
			return nil, fmt.Errorf("cruise track number must be 0 or 1")
		}
		number := in.Number
		req.Number = &number
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
	if cmdType == "ConfigDownload" {
		pending.expectedConfig = make(map[string]struct{})
		for _, item := range strings.Split(req.ConfigType, "/") {
			pending.expectedConfig[item] = struct{}{}
		}
	}
	g.pendingDeviceQuery.Store(waitKey, pending)
	defer g.pendingDeviceQuery.Delete(waitKey)

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
	case out := <-pending.wait:
		return out, nil
	case <-g.serviceDone():
		return nil, ErrServiceStopped
	case <-ctx.Done():
		return nil, ctx.Err()
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
	case "cruise_track_list_query", "cruise_list", "cruise_track_listquery":
		return deviceQueryActionCruiseTrackList
	case "cruise_track_query", "cruise_query", "cruise_trackquery":
		return deviceQueryActionCruiseTrack
	default:
		return a
	}
}

func (g *GB28181API) resolveDeviceQueryCmdType(deviceID, action, configType string) (string, error) {
	switch action {
	case deviceQueryActionCatalog:
		return "Catalog", nil
	case deviceQueryActionBroadcast:
		if err := g.requireGBFeature(deviceID, "voice_broadcast", "语音广播查询", func(c GBCapabilities) bool {
			return c.VoiceBroadcast
		}); err != nil {
			return "", err
		}
		return "Broadcast", nil
	case deviceQueryActionDeviceInfo:
		return "DeviceInfo", nil
	case deviceQueryActionDeviceStatus:
		return "DeviceStatus", nil
	case deviceQueryActionRecordInfo:
		return "RecordInfo", nil
	case deviceQueryActionPresetQuery:
		if err := g.requireGBFeature(deviceID, "preset_query", "预置位查询", func(c GBCapabilities) bool {
			return c.PresetQuery
		}); err != nil {
			return "", err
		}
		return "PresetQuery", nil
	case deviceQueryActionHomePositionQuery:
		if err := g.requireGBFeature(deviceID, "home_position", "看守位查询(HomePositionQuery)", func(c GBCapabilities) bool {
			return c.HomePosition
		}); err != nil {
			return "", err
		}
		return "HomePositionQuery", nil
	case deviceQueryActionCruiseTrackList:
		if err := g.requireGBFeature(deviceID, "cruise_track_query", "巡航轨迹列表查询", func(c GBCapabilities) bool {
			return c.CruiseTrackQuery
		}); err != nil {
			return "", err
		}
		return "CruiseTrackListQuery", nil
	case deviceQueryActionCruiseTrack:
		if err := g.requireGBFeature(deviceID, "cruise_track_query", "巡航轨迹查询", func(c GBCapabilities) bool {
			return c.CruiseTrackQuery
		}); err != nil {
			return "", err
		}
		return "CruiseTrackQuery", nil
	case deviceQueryActionPTZPosition:
		if err := g.requireGBFeature(deviceID, "ptz_position", "PTZ精准状态查询", func(c GBCapabilities) bool {
			return c.PTZPosition
		}); err != nil {
			return "", err
		}
		return "PTZPosition", nil
	case deviceQueryActionSDCardStatus:
		if err := g.requireGBFeature(deviceID, "sdcard", "存储卡状态查询", func(c GBCapabilities) bool {
			return c.SDCard
		}); err != nil {
			return "", err
		}
		return "SDCardStatus", nil
	case deviceQueryActionConfigDownload:
		if configType == "" {
			return "", fmt.Errorf("config_type is required for config_download")
		}
		canonical, ok := normalizeConfigTypes(configType)
		if !ok {
			return "", fmt.Errorf("unsupported config_type: %s", configType)
		}
		for _, item := range strings.Split(canonical, "/") {
			if err := g.requireConfigTypeVersion(deviceID, item); err != nil {
				return "", err
			}
		}
		return "ConfigDownload", nil
	case deviceQueryActionMobilePosition:
		if err := g.requireGBFeature(deviceID, "mobile_position", "移动位置查询", func(c GBCapabilities) bool {
			return c.MobilePosition
		}); err != nil {
			return "", err
		}
		return "MobilePosition", nil
	default:
		return "", fmt.Errorf("unsupported device query action: %s", action)
	}
}

func (g *GB28181API) requireConfigTypeVersion(deviceID, configType string) error {
	name := strings.TrimSpace(configType)
	switch name {
	case "BasicParam", "VideoParamOpt", "VideoParamConfig", "AudioParamOpt", "AudioParamConfig",
		"SVACEncodeConfig", "SVACDecodeConfig":
		return g.requireGBFeature(deviceID, "config_query", "配置查询("+name+")", func(c GBCapabilities) bool {
			return c.ConfigQuery
		})
	case "VideoParamAttribute", "VideoRecordPlan",
		"VideoAlarmRecord", "PictureMask", "FrameMirror", "AlarmReport", "OSDConfig", "SnapShotConfig":
		if err := g.requireGBFeature(deviceID, "config_query", "配置查询("+name+")", func(c GBCapabilities) bool {
			return c.ConfigQuery
		}); err != nil {
			return err
		}
		if name == "SnapShotConfig" {
			if err := g.requireGBFeature(deviceID, "snapshot", "配置查询(SnapShot)", func(c GBCapabilities) bool {
				return c.Snapshot
			}); err != nil {
				return err
			}
		}
		return g.requireGBVersionAtLeast(deviceID, gbVersion2022, "配置查询("+name+")")
	default:
		return fmt.Errorf("unsupported config_type: %s", name)
	}
}

func normalizeConfigType(configType string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(configType))
	key = strings.NewReplacer("_", "", "-", "", " ", "").Replace(key)
	switch key {
	case "basicparam":
		return "BasicParam", true
	case "videoparamopt":
		return "VideoParamOpt", true
	case "videoparamconfig":
		return "VideoParamConfig", true
	case "audioparamopt":
		return "AudioParamOpt", true
	case "audioparamconfig":
		return "AudioParamConfig", true
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
	case "snapshot", "snapshotconfig":
		return "SnapShotConfig", true
	default:
		return "", false
	}
}

func normalizeConfigTypes(value string) (string, bool) {
	parts := strings.Split(value, "/")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		canonical, ok := normalizeConfigType(part)
		if !ok {
			return "", false
		}
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		out = append(out, canonical)
	}
	if len(out) == 0 {
		return "", false
	}
	return strings.Join(out, "/"), true
}

func buildPendingQueryKey(deviceID, cmdType string, sn int) string {
	return fmt.Sprintf("%s:%s:%d", strings.TrimSpace(deviceID), strings.ToUpper(strings.TrimSpace(cmdType)), sn)
}

func canonicalGBQueryCmdType(value string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "PersetQuery") {
		return "PresetQuery"
	}
	return value
}

func (g *GB28181API) resolvePendingDeviceQueryResult(deviceID, cmdType string, sn int, result string, body []byte, targetID string, decoded decodedDeviceQuery) {
	cmdType = canonicalGBQueryCmdType(cmdType)
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
		out.Data = cloneDeviceQueryData(decoded.data)
		out.AppendixA4 = cloneAppendixA4Objects(decoded.appendixA4)
		pending := v.(*pendingQueryWait)
		if out.CmdType == "ConfigDownload" && (out.Result == "" || strings.EqualFold(out.Result, "OK")) {
			state, _ := out.Data.(*ConfigDownloadState)
			pending.mu.Lock()
			tracking := len(pending.expectedConfig) > 0
			if tracking && pending.config == nil {
				pending.config = &ConfigDownloadState{}
			}
			if tracking {
				mergeConfigDownloadState(pending.config, state)
				for _, configType := range configDownloadStateTypes(state) {
					delete(pending.expectedConfig, configType)
				}
				out.Data = pending.config
			}
			complete := len(pending.expectedConfig) == 0
			pending.mu.Unlock()
			if tracking && !complete {
				return
			}
		}
		select {
		case pending.wait <- out:
		default:
		}
		return
	}
}

func configDownloadStateTypes(state *ConfigDownloadState) []string {
	if state == nil {
		return nil
	}
	types := make([]string, 0, 16)
	checks := []struct {
		name string
		on   bool
	}{
		{"BasicParam", state.BasicParam != nil},
		{"VideoParamOpt", state.VideoParamOpt != nil},
		{"VideoParamConfig", state.VideoParamConfig != nil},
		{"AudioParamOpt", state.AudioParamOpt != nil},
		{"AudioParamConfig", state.AudioParamConfig != nil},
		{"SVACEncodeConfig", state.SVACEncodeConfig != nil},
		{"SVACDecodeConfig", state.SVACDecodeConfig != nil},
		{"VideoParamAttribute", state.VideoParamAttribute != nil},
		{"VideoRecordPlan", state.VideoRecordPlan != nil},
		{"VideoAlarmRecord", state.VideoAlarmRecord != nil},
		{"PictureMask", state.PictureMask != nil},
		{"FrameMirror", state.FrameMirror != nil},
		{"AlarmReport", state.AlarmReport != nil},
		{"OSDConfig", state.OSDConfig != nil},
		{"SnapShotConfig", state.SnapShot != nil},
	}
	for _, check := range checks {
		if check.on {
			types = append(types, check.name)
		}
	}
	return types
}

func mergeConfigDownloadState(dst, src *ConfigDownloadState) {
	if dst == nil || src == nil {
		return
	}
	dst.CmdType, dst.SN, dst.DeviceID, dst.Result = src.CmdType, src.SN, src.DeviceID, src.Result
	if src.BasicParam != nil {
		dst.BasicParam = src.BasicParam
	}
	if src.VideoParamOpt != nil {
		dst.VideoParamOpt = src.VideoParamOpt
	}
	if src.VideoParamConfig != nil {
		dst.VideoParamConfig = src.VideoParamConfig
	}
	if src.AudioParamOpt != nil {
		dst.AudioParamOpt = src.AudioParamOpt
	}
	if src.AudioParamConfig != nil {
		dst.AudioParamConfig = src.AudioParamConfig
	}
	if src.SVACEncodeConfig != nil {
		dst.SVACEncodeConfig = src.SVACEncodeConfig
	}
	if src.SVACDecodeConfig != nil {
		dst.SVACDecodeConfig = src.SVACDecodeConfig
	}
	if src.VideoParamAttribute != nil {
		dst.VideoParamAttribute = src.VideoParamAttribute
	}
	if src.VideoRecordPlan != nil {
		dst.VideoRecordPlan = src.VideoRecordPlan
	}
	if src.VideoAlarmRecord != nil {
		dst.VideoAlarmRecord = src.VideoAlarmRecord
	}
	if src.PictureMask != nil {
		dst.PictureMask = src.PictureMask
	}
	if src.FrameMirror != nil {
		dst.FrameMirror = src.FrameMirror
	}
	if src.AlarmReport != nil {
		dst.AlarmReport = src.AlarmReport
	}
	if src.OSDConfig != nil {
		dst.OSDConfig = src.OSDConfig
	}
	if src.SnapShot != nil {
		dst.SnapShot = src.SnapShot
	}
	if src.RawXML != "" {
		dst.RawXML = src.RawXML
	}
}

// sipMessageQueryGeneric 处理通用查询响应（A.2.4 补齐项）。
func (g *GB28181API) sipMessageQueryGeneric(ctx *sip.Context) {
	var msg genericDeviceQueryResponse
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	msg.CmdType = canonicalGBQueryCmdType(msg.CmdType)
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	if err := g.validateGenericDeviceQueryResponse(ctx, msg); err != nil {
		ctx.String(400, err.Error())
		return
	}
	deviceID := strings.TrimSpace(ctx.DeviceID)
	decoded := g.decodeAndStoreQueryResult(deviceID, msg.CmdType, ctx.Request.Body())
	g.resolvePendingDeviceQueryResult(ctx.DeviceID, msg.CmdType, msg.SN, msg.Result, ctx.Request.Body(), msg.DeviceID, decoded)
	ctx.String(200, "OK")
	g.persistDecodedQuery(deviceID, msg.CmdType, decoded)
	// 9.11 事件源侧：通用查询类事件通知。
	g.publishEventNotify(msg.CmdType, deviceID, ctx.Request.Body())
}

func (g *GB28181API) validateGenericDeviceQueryResponse(ctx *sip.Context, msg genericDeviceQueryResponse) error {
	if ctx == nil || strings.TrimSpace(ctx.DeviceID) == "" {
		return fmt.Errorf("query response requires authenticated device")
	}
	if msg.XMLName.Local != "Response" && msg.XMLName.Local != "Notify" {
		return fmt.Errorf("query response root must be Response or Notify")
	}
	if msg.SN <= 0 || msg.DeviceID == "" {
		return fmt.Errorf("query response requires positive SN and DeviceID")
	}
	minimum, ok := genericQueryResponseMinimumVersion(msg.CmdType)
	if !ok {
		return fmt.Errorf("unsupported query response command: %s", msg.CmdType)
	}
	version := g.getDeviceGBProtocolVersion(ctx.DeviceID)
	if !version.AtLeast(minimum) {
		return fmt.Errorf("%s requires %s or later", msg.CmdType, minimum.StandardName())
	}
	if msg.DeviceID == strings.TrimSpace(ctx.DeviceID) {
		return nil
	}
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return fmt.Errorf("query response target mismatch")
	}
	if _, ok := g.svr.memoryStorer.GetChannel(ctx.DeviceID, msg.DeviceID); !ok {
		return fmt.Errorf("query response target mismatch")
	}
	return nil
}

func genericQueryResponseMinimumVersion(cmdType string) (GBProtocolVersion, bool) {
	switch cmdType {
	case "DeviceStatus":
		return GBVersion10, true
	case "PresetQuery", "ConfigDownload", "DeviceConfig":
		return GBVersion11, true
	case "HomePositionQuery", "MobilePosition":
		return GBVersion20, true
	case "CruiseTrackListQuery", "CruiseTrackQuery", "PTZPosition", "SDCardStatus", "VideoUploadNotify":
		return GBVersion30, true
	default:
		return "", false
	}
}
