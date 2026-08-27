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
	Start        int64
	End          int64
	Type         string
	StreamNumber *int
	AlarmMethod  string
	AlarmType    string
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
	// responseXML 保留同一次 ConfigDownload 的分包响应，仅供级联逐包转发。
	responseXML []string
}

type pendingQueryWait struct {
	wait           chan *DeviceQueryOutput
	targetID       string
	mu             sync.Mutex
	expectedConfig map[string]struct{}
	config         *ConfigDownloadState
	responseXML    []string
}

type genericDeviceQueryResponse struct {
	XMLName  xml.Name
	CmdType  string `xml:"CmdType"`
	SN       int    `xml:"SN"`
	DeviceID string `xml:"DeviceID"`
	Result   string `xml:"Result"`
	Online   string `xml:"Online"`
	Status   string `xml:"Status"`
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
		records, err := g.QueryRecordList(ctx, recordQueryInputFromDeviceQuery(deviceID, targetID, in))
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
		CmdType:  gbQueryCmdTypeForVersion(cmdType, g.getDeviceGBProtocolVersion(deviceID)),
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
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: targetID}
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

func recordQueryInputFromDeviceQuery(deviceID, targetID string, in *DeviceQueryInput) *RecordQueryInput {
	return &RecordQueryInput{
		DeviceID:     deviceID,
		ChannelID:    targetID,
		Start:        in.Start,
		End:          in.End,
		Timeout:      in.Timeout,
		Type:         in.Type,
		StreamNumber: in.StreamNumber,
		AlarmMethod:  in.AlarmMethod,
		AlarmType:    in.AlarmType,
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
	version := g.getDeviceGBProtocolVersion(deviceID)
	if !configTypeSupported(version, name) {
		return fmt.Errorf("unsupported config_type: %s", name)
	}
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
	return nil
}

func configTypeSupported(version GBProtocolVersion, name string) bool {
	switch version {
	case GBVersion11:
		switch strings.TrimSpace(name) {
		case "BasicParam", "VideoParamOpt", "VideoParamConfig", "AudioParamOpt", "AudioParamConfig", "SVACEncodeConfig", "SVACDecodeConfig":
			return true
		}
	case GBVersion20:
		switch strings.TrimSpace(name) {
		case "BasicParam", "VideoParamOpt", "SVACEncodeConfig", "SVACDecodeConfig":
			return true
		}
	case GBVersion30:
		switch strings.TrimSpace(name) {
		case "BasicParam", "VideoParamOpt", "SVACEncodeConfig", "SVACDecodeConfig", "VideoParamAttribute", "VideoRecordPlan",
			"VideoAlarmRecord", "PictureMask", "FrameMirror", "AlarmReport", "OSDConfig", "SnapShotConfig":
			return true
		}
	}
	return false
}

func deviceConfigSectionSupported(version GBProtocolVersion, name string) bool {
	switch version {
	case GBVersion11:
		switch strings.TrimSpace(name) {
		case "BasicParam", "VideoParamConfig", "AudioParamConfig", "SVACEncodeConfig", "SVACDecodeConfig":
			return true
		}
	case GBVersion20:
		switch strings.TrimSpace(name) {
		case "BasicParam", "SVACEncodeConfig", "SVACDecodeConfig":
			return true
		}
	case GBVersion30:
		switch strings.TrimSpace(name) {
		case "BasicParam", "SVACEncodeConfig", "SVACDecodeConfig", "VideoParamAttribute", "VideoRecordPlan",
			"VideoAlarmRecord", "PictureMask", "FrameMirror", "AlarmReport", "OSDConfig", "SnapShotConfig":
			return true
		}
	}
	return false
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

func gbQueryCmdTypeForVersion(value string, version GBProtocolVersion) string {
	value = canonicalGBQueryCmdType(value)
	if value == "PresetQuery" && version == GBVersion11 {
		return "PersetQuery"
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
		if expected := strings.TrimSpace(pending.targetID); expected != "" && expected != strings.TrimSpace(out.DeviceID) {
			continue
		}
		if out.CmdType == "ConfigDownload" {
			pending.mu.Lock()
			tracking := len(pending.expectedConfig) > 0
			if tracking {
				if strings.EqualFold(out.Result, "OK") {
					state, _ := out.Data.(*ConfigDownloadState)
					contributed := false
					for _, configType := range configDownloadStateTypes(state) {
						if _, expected := pending.expectedConfig[configType]; expected {
							delete(pending.expectedConfig, configType)
							contributed = true
						}
					}
					if contributed {
						if pending.config == nil {
							pending.config = &ConfigDownloadState{}
						}
						mergeConfigDownloadState(pending.config, state)
						pending.responseXML = append(pending.responseXML, out.XML)
					}
					out.Data = pending.config
				} else {
					pending.responseXML = append(pending.responseXML, out.XML)
				}
			}
			complete := !strings.EqualFold(out.Result, "OK") || len(pending.expectedConfig) == 0
			if complete {
				out.responseXML = append([]string(nil), pending.responseXML...)
			}
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
	if err := validateGenericQueryPayload(g.getDeviceGBProtocolVersion(ctx.DeviceID), msg.CmdType, ctx.Request.Body()); err != nil {
		ctx.String(400, err.Error())
		return
	}
	deviceID := strings.TrimSpace(ctx.DeviceID)
	decoded := g.decodeAndStoreQueryResult(deviceID, msg.CmdType, ctx.Request.Body())
	if strings.EqualFold(ctx.Request.Method(), sip.MethodMessage) {
		g.resolvePendingDeviceQueryResult(ctx.DeviceID, msg.CmdType, msg.SN, msg.Result, ctx.Request.Body(), msg.DeviceID, decoded)
	}
	ctx.String(200, "OK")
	g.persistDecodedQuery(deviceID, msg.CmdType, decoded)
	if strings.EqualFold(ctx.Request.Method(), sip.MethodNotify) {
		// 9.11 事件源侧：只转发真实事件通知，查询应答不得触发订阅事件。
		g.publishEventNotify(msg.CmdType, deviceID, ctx.Request.Body())
	}
}

func (g *GB28181API) validateGenericDeviceQueryResponse(ctx *sip.Context, msg genericDeviceQueryResponse) error {
	if ctx == nil || strings.TrimSpace(ctx.DeviceID) == "" {
		return fmt.Errorf("query response requires authenticated device")
	}
	if ctx.Request == nil {
		return fmt.Errorf("query response requires SIP request")
	}
	switch {
	case strings.EqualFold(ctx.Request.Method(), sip.MethodMessage) && msg.XMLName.Local != "Response":
		return fmt.Errorf("MESSAGE query response root must be Response")
	case strings.EqualFold(ctx.Request.Method(), sip.MethodNotify) && msg.XMLName.Local != "Notify":
		return fmt.Errorf("NOTIFY query event root must be Notify")
	case !strings.EqualFold(ctx.Request.Method(), sip.MethodMessage) && !strings.EqualFold(ctx.Request.Method(), sip.MethodNotify):
		return fmt.Errorf("query response requires MESSAGE or NOTIFY")
	}
	if msg.SN <= 0 || !isGBDeviceIdentifier(msg.DeviceID) {
		return fmt.Errorf("query response requires positive SN and DeviceID")
	}
	minimum, ok := genericQueryResponseMinimumVersion(msg.CmdType)
	if strings.EqualFold(ctx.Request.Method(), sip.MethodNotify) {
		minimum, ok = genericQueryNotificationMinimumVersion(msg.CmdType)
	}
	if !ok {
		return fmt.Errorf("unsupported query response command: %s", msg.CmdType)
	}
	version := g.getDeviceGBProtocolVersion(ctx.DeviceID)
	if !version.AtLeast(minimum) {
		return fmt.Errorf("%s requires %s or later", msg.CmdType, minimum.StandardName())
	}
	if msg.CmdType == "DeviceStatus" {
		if !isGBResultValue(msg.Result) ||
			!equalFoldAny(strings.TrimSpace(msg.Online), "ONLINE", "OFFLINE") ||
			!isGBResultValue(msg.Status) {
			return fmt.Errorf("DeviceStatus requires valid Result, Online and Status")
		}
	}
	if err := g.validateAuthenticatedResponseTarget(ctx, msg.DeviceID); err != nil {
		return err
	}
	if strings.EqualFold(ctx.Request.Method(), sip.MethodMessage) && g.pendingDeviceQueryTargetMismatch(ctx.DeviceID, msg.CmdType, msg.SN, msg.DeviceID) {
		return fmt.Errorf("query response target mismatch")
	}
	return nil
}

func genericQueryNotificationMinimumVersion(cmdType string) (GBProtocolVersion, bool) {
	switch cmdType {
	case "PTZPosition":
		return GBVersion30, true
	default:
		return "", false
	}
}

func (g *GB28181API) pendingDeviceQueryTargetMismatch(deviceID, cmdType string, sn int, targetID string) bool {
	if g == nil || sn <= 0 {
		return false
	}
	value, ok := g.pendingDeviceQuery.Load(buildPendingQueryKey(deviceID, canonicalGBQueryCmdType(cmdType), sn))
	if !ok {
		return false
	}
	pending, ok := value.(*pendingQueryWait)
	if !ok || pending == nil {
		return false
	}
	expected := strings.TrimSpace(pending.targetID)
	return expected != "" && expected != strings.TrimSpace(targetID)
}

func (g *GB28181API) validateAuthenticatedResponseTarget(ctx *sip.Context, targetID string) error {
	if ctx == nil || strings.TrimSpace(ctx.DeviceID) == "" {
		return fmt.Errorf("response requires authenticated device")
	}
	targetID = strings.TrimSpace(targetID)
	if targetID == strings.TrimSpace(ctx.DeviceID) {
		return nil
	}
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return fmt.Errorf("response target mismatch")
	}
	if _, ok := g.svr.memoryStorer.GetChannel(ctx.DeviceID, targetID); !ok {
		return fmt.Errorf("response target mismatch")
	}
	return nil
}

func isGBResultValue(value string) bool {
	return equalFoldAny(strings.TrimSpace(value), "OK", "ERROR")
}

func equalFoldAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.EqualFold(value, candidate) {
			return true
		}
	}
	return false
}

func genericQueryResponseMinimumVersion(cmdType string) (GBProtocolVersion, bool) {
	switch cmdType {
	case "DeviceInfo", "DeviceStatus":
		return GBVersion10, true
	case "PresetQuery", "ConfigDownload", "DeviceConfig":
		return GBVersion11, true
	case "HomePositionQuery", "MobilePosition":
		return GBVersion20, true
	case "CruiseTrackListQuery", "CruiseTrackQuery", "PTZPosition", "SDCardStatus":
		return GBVersion30, true
	default:
		return "", false
	}
}
