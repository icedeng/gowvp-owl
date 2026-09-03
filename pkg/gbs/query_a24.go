package gbs

import (
	"context"
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	deviceQueryActionCatalog      = "catalog"
	deviceQueryActionDeviceInfo   = "device_info"
	deviceQueryActionDeviceStatus = "device_status"
	// deviceQueryActionAlarm 保留部分厂商接受 MESSAGE/Query/Alarm 并返回
	// Response/Alarm/Result 的兼容流程。四版标准中的 A.2.4 Alarm 查询体用于
	// 9.11 SUBSCRIBE 报警订阅，未定义独立的主动报警列表查询流程。
	deviceQueryActionAlarm             = "alarm"
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
	// Catalog、RecordInfo 查询时间范围（unix 秒）。
	Start              int64
	End                int64
	FilePath           string
	Address            string
	Secrecy            *int
	Type               string
	RecorderID         string
	IndistinctQuery    *int
	StreamNumber       *int
	AlarmMethod        string
	AlarmType          string
	StartAlarmPriority string
	EndAlarmPriority   string
	StartAlarmTime     string
	EndAlarmTime       string
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
	// responseXML 保留同一次 Catalog/ConfigDownload 的有效分包响应；ConfigDownload 供级联逐包转发。
	responseXML []string
}

// ConfigDownloadIncompleteError 表示多配置查询只收到部分标准响应。
type ConfigDownloadIncompleteError struct {
	Received []string
	Missing  []string
}

func (err *ConfigDownloadIncompleteError) Error() string {
	if err == nil {
		return "ConfigDownload response incomplete"
	}
	return fmt.Sprintf("ConfigDownload response incomplete: received %s; missing %s",
		strings.Join(err.Received, "/"), strings.Join(err.Missing, "/"))
}

type pendingQueryWait struct {
	wait              chan *DeviceQueryOutput
	targetID          string
	operation         *pendingDeviceOperation
	autoDelete        bool
	expiryMu          sync.Mutex
	expiryTimer       *time.Timer
	expiryStopped     bool
	mu                sync.Mutex
	expectedConfig    map[string]struct{}
	requestedConfig   map[string]struct{}
	config            *ConfigDownloadState
	configAppendixA4  []AppendixA4Object
	catalogExpected   int
	catalogItems      []Channels
	catalogSeen       map[string]struct{}
	catalogAppendixA4 []AppendixA4Object
	responseXML       []string
}

const automaticQueryResponseTimeout = 30 * time.Second

func (pending *pendingQueryWait) setExpiryTimer(timer *time.Timer) {
	if pending == nil || timer == nil {
		return
	}
	pending.expiryMu.Lock()
	if pending.expiryStopped {
		timer.Stop()
	} else {
		pending.expiryTimer = timer
	}
	pending.expiryMu.Unlock()
}

func (pending *pendingQueryWait) cancel(cause error) {
	if pending == nil {
		return
	}
	pending.expiryMu.Lock()
	pending.expiryStopped = true
	timer := pending.expiryTimer
	pending.expiryTimer = nil
	pending.expiryMu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	if pending.operation != nil {
		pending.operation.Cancel(cause)
	}
}

func (g *GB28181API) expectAutomaticQueryResponse(deviceID, cmdType string, sn int, targetID string) func() {
	cancel, _ := g.tryExpectAutomaticQueryResponse(deviceID, cmdType, sn, targetID)
	return cancel
}

func (g *GB28181API) reserveAutomaticQueryResponse(deviceID, cmdType, targetID string) (int, func()) {
	for {
		sn := g.nextQuerySN()
		if cancel, stored := g.tryExpectAutomaticQueryResponse(deviceID, cmdType, sn, targetID); stored {
			return sn, cancel
		}
	}
}

func (g *GB28181API) tryExpectAutomaticQueryResponse(deviceID, cmdType string, sn int, targetID string) (func(), bool) {
	operation := newPendingDeviceOperation(context.Background(), deviceID, targetID)
	pending := &pendingQueryWait{
		targetID:   strings.TrimSpace(targetID),
		operation:  operation,
		autoDelete: true,
	}
	if canonicalGBQueryCmdType(cmdType) == CMDTypeConfigDownload {
		pending.expectedConfig = map[string]struct{}{basicParam: {}}
		pending.requestedConfig = map[string]struct{}{basicParam: {}}
	}
	key := buildPendingQueryKey(deviceID, canonicalGBQueryCmdType(cmdType), sn)
	if _, loaded := g.pendingDeviceQuery.LoadOrStore(key, pending); loaded {
		operation.Cancel(nil)
		return func() {}, false
	}
	pending.setExpiryTimer(time.AfterFunc(automaticQueryResponseTimeout, func() {
		g.pendingDeviceQuery.CompareAndDelete(key, pending)
		pending.cancel(context.DeadlineExceeded)
	}))
	return func() {
		g.pendingDeviceQuery.CompareAndDelete(key, pending)
		pending.cancel(context.Canceled)
	}, true
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
	XMLName            xml.Name `xml:"Query"`
	CmdType            string   `xml:"CmdType"`
	SN                 int      `xml:"SN"`
	DeviceID           string   `xml:"DeviceID"`
	StartAlarmPriority string   `xml:"StartAlarmPriority,omitempty"`
	EndAlarmPriority   string   `xml:"EndAlarmPriority,omitempty"`
	AlarmMethod        string   `xml:"AlarmMethod,omitempty"`
	AlarmType          string   `xml:"AlarmType,omitempty"`
	StartAlarmTime     string   `xml:"StartAlarmTime,omitempty"`
	EndAlarmTime       string   `xml:"EndAlarmTime,omitempty"`
	ConfigType         string   `xml:"ConfigType,omitempty"`
	Interval           *int     `xml:"Interval,omitempty"`
	Number             *int     `xml:"Number,omitempty"`
	StartTime          string   `xml:"StartTime,omitempty"`
	EndTime            string   `xml:"EndTime,omitempty"`
}

// DeviceQuery 执行附录 A.2.4 查询命令。MobilePosition 以 SIP 200 确认请求，
// 后续位置数据按标准通过 Notify 上报；其他查询继续等待业务响应。
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
	version := g.getDeviceGBProtocolVersion(deviceID)

	configType := strings.TrimSpace(in.ConfigType)
	cmdType, err := g.resolveDeviceQueryCmdType(deviceID, action, configType)
	if err != nil {
		return nil, err
	}
	if cmdType == "Catalog" && classifyGBCatalogItem(targetID) == GBCatalogItemAdministrative {
		if !version.AtLeast(GBVersion11) {
			return nil, fmt.Errorf("administrative Catalog target requires GB/T 28181-2014 or later")
		}
		if err := g.requireGBFeature(deviceID, "catalog_extension", "行政区划 Catalog 查询", func(c GBCapabilities) bool {
			return c.CatalogExtension
		}); err != nil {
			return nil, err
		}
	}
	if cmdType == "MobilePosition" && in.Interval < 0 {
		return nil, fmt.Errorf("mobile_position interval must not be negative")
	}
	if cmdType == "RecordInfo" {
		version := g.getDeviceGBProtocolVersion(deviceID)
		if version.AtLeast(GBVersion20) && (in.Start <= 0 || in.End <= in.Start) ||
			!version.AtLeast(GBVersion20) && in.Start > 0 && in.End > 0 && in.End <= in.Start {
			return nil, fmt.Errorf("record_info requires valid start/end")
		}
		records, result, err := g.queryRecordListResult(ctx, recordQueryInputFromDeviceQuery(deviceID, targetID, in))
		if err != nil && records == nil {
			return nil, err
		}
		out := &DeviceQueryOutput{
			SN:          result.SN,
			CmdType:     "RecordInfo",
			DeviceID:    targetID,
			Data:        records,
			AppendixA4:  cloneAppendixA4Objects(result.AppendixA4),
			responseXML: append([]string(nil), result.ResponseXML...),
		}
		if len(result.ResponseXML) > 0 {
			out.XML = result.ResponseXML[0]
		}
		if err == nil {
			out.Result = "OK"
		}
		return out, err
	}

	req := genericDeviceQueryRequest{
		CmdType:  gbQueryCmdTypeForVersion(cmdType, version),
		DeviceID: targetID,
	}
	if cmdType == "Alarm" {
		if err := applyAlarmQueryFilters(version, &req, in); err != nil {
			return nil, err
		}
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
	if cmdType == "Catalog" {
		if in.Start < 0 || in.End < 0 || in.Start > 0 && in.End > 0 && in.End <= in.Start {
			return nil, fmt.Errorf("catalog requires valid start/end")
		}
		if in.Start > 0 {
			req.StartTime = sip.FormatGBTime(time.Unix(in.Start, 0), "2006-01-02T15:04:05")
		}
		if in.End > 0 {
			req.EndTime = sip.FormatGBTime(time.Unix(in.End, 0), "2006-01-02T15:04:05")
		}
	}

	var target Targeter = ipc
	if targetID != deviceID {
		alarmCenterTarget := cmdType == "Alarm" && len(targetID) == 10 && isNumericIdentifier(targetID) && strings.HasPrefix(deviceID, targetID)
		if !alarmCenterTarget && (cmdType != "Catalog" || classifyGBCatalogItem(targetID) != GBCatalogItemAdministrative) {
			ch, ok := g.svr.memoryStorer.GetChannel(deviceID, targetID)
			if !ok {
				return nil, ErrChannelNotExist
			}
			target = ch
		}
	}

	sn := g.nextQuerySN()
	req.SN = sn
	body, err := sip.XMLEncode(req)
	if err != nil {
		return nil, err
	}

	var pending *pendingQueryWait
	waitKey := ""
	operation, releaseOperation := g.trackPendingDeviceRequest(ctx, deviceID, targetID)
	defer releaseOperation()
	if deviceQueryWaitsForBusinessResponse(cmdType) {
		for {
			waitKey = buildPendingQueryKey(deviceID, cmdType, sn)
			pending = &pendingQueryWait{
				wait:      make(chan *DeviceQueryOutput, 1),
				targetID:  targetID,
				operation: operation,
			}
			if cmdType == "ConfigDownload" {
				pending.expectedConfig = make(map[string]struct{})
				pending.requestedConfig = make(map[string]struct{})
				for _, item := range strings.Split(req.ConfigType, "/") {
					pending.expectedConfig[item] = struct{}{}
					pending.requestedConfig[item] = struct{}{}
				}
			}
			if _, loaded := g.pendingDeviceQuery.LoadOrStore(waitKey, pending); !loaded {
				break
			}
			sn = g.nextQuerySN()
			req.SN = sn
			body, err = sip.XMLEncode(req)
			if err != nil {
				return nil, err
			}
		}
		defer g.pendingDeviceQuery.CompareAndDelete(waitKey, pending)
		defer pending.operation.Cancel(nil)
	}

	requestCtx := operation.Context(ctx)
	tx, err := g.svr.wrapRequestContext(requestCtx, target, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		return nil, operation.ErrorOr(err)
	}
	if _, err = sipResponseContext(requestCtx, tx); err != nil {
		return nil, operation.ErrorOr(err)
	}
	if !deviceQueryWaitsForBusinessResponse(cmdType) {
		out := &DeviceQueryOutput{
			SN: sn, CmdType: cmdType, DeviceID: targetID,
		}
		if !operation.Deliver(func() {}) {
			return nil, operation.Cause()
		}
		return out, nil
	}

	timer := time.NewTimer(in.Timeout)
	defer timer.Stop()

	select {
	case out := <-pending.wait:
		return out, nil
	case <-g.serviceDone():
		return nil, ErrServiceStopped
	case <-operation.Done():
		return nil, operation.Cause()
	case <-timer.C:
		switch cmdType {
		case "Catalog":
			if out, err := catalogPartialResult(sn, targetID, pending); out != nil || err != nil {
				return out, err
			}
		case "ConfigDownload":
			if out, err := configDownloadPartialResult(sn, targetID, pending); out != nil || err != nil {
				return out, err
			}
		}
		return nil, fmt.Errorf("wait query response timeout")
	}
}

func catalogPartialResult(sn int, targetID string, pending *pendingQueryWait) (*DeviceQueryOutput, error) {
	if pending == nil {
		return nil, nil
	}
	pending.mu.Lock()
	defer pending.mu.Unlock()
	if pending.catalogExpected < 0 || len(pending.responseXML) == 0 {
		return nil, nil
	}
	responseXML := append([]string(nil), pending.responseXML...)
	received := len(pending.catalogItems)
	out := &DeviceQueryOutput{
		SN:          sn,
		CmdType:     "Catalog",
		DeviceID:    strings.TrimSpace(targetID),
		XML:         responseXML[0],
		Data:        cloneCatalogChannels(pending.catalogItems),
		AppendixA4:  cloneAppendixA4Objects(pending.catalogAppendixA4),
		responseXML: responseXML,
	}
	if received >= pending.catalogExpected {
		return out, nil
	}
	return out, &CatalogIncompleteError{Received: received, Expected: pending.catalogExpected}
}

func configDownloadPartialResult(sn int, targetID string, pending *pendingQueryWait) (*DeviceQueryOutput, error) {
	if pending == nil {
		return nil, nil
	}
	pending.mu.Lock()
	defer pending.mu.Unlock()
	if pending.config == nil || len(pending.responseXML) == 0 {
		return nil, nil
	}
	received := configDownloadStateTypes(pending.config)
	if len(received) == 0 {
		return nil, nil
	}
	missing := make([]string, 0, len(pending.expectedConfig))
	for configType := range pending.expectedConfig {
		missing = append(missing, configType)
	}
	sort.Strings(missing)
	responseXML := append([]string(nil), pending.responseXML...)
	out := &DeviceQueryOutput{
		SN:          sn,
		CmdType:     "ConfigDownload",
		DeviceID:    strings.TrimSpace(targetID),
		Result:      "OK",
		XML:         responseXML[0],
		Data:        cloneConfigDownloadState(pending.config),
		AppendixA4:  cloneAppendixA4Objects(pending.configAppendixA4),
		responseXML: responseXML,
	}
	if len(missing) == 0 {
		return out, nil
	}
	return out, &ConfigDownloadIncompleteError{
		Received: append([]string(nil), received...),
		Missing:  missing,
	}
}

func deviceQueryWaitsForBusinessResponse(cmdType string) bool {
	return !strings.EqualFold(strings.TrimSpace(cmdType), "MobilePosition")
}

func recordQueryInputFromDeviceQuery(deviceID, targetID string, in *DeviceQueryInput) *RecordQueryInput {
	return &RecordQueryInput{
		DeviceID:        deviceID,
		ChannelID:       targetID,
		Start:           in.Start,
		End:             in.End,
		Timeout:         in.Timeout,
		FilePath:        in.FilePath,
		Address:         in.Address,
		Secrecy:         in.Secrecy,
		Type:            in.Type,
		RecorderID:      in.RecorderID,
		IndistinctQuery: in.IndistinctQuery,
		StreamNumber:    in.StreamNumber,
		AlarmMethod:     in.AlarmMethod,
		AlarmType:       in.AlarmType,
	}
}

func applyAlarmQueryFilters(version GBProtocolVersion, request *genericDeviceQueryRequest, in *DeviceQueryInput) error {
	if request == nil || in == nil {
		return fmt.Errorf("alarm query input is unavailable")
	}
	start, err := parseAlarmPriorityFilter(in.StartAlarmPriority)
	if err != nil {
		return fmt.Errorf("invalid StartAlarmPriority: %w", err)
	}
	end, err := parseAlarmPriorityFilter(in.EndAlarmPriority)
	if err != nil {
		return fmt.Errorf("invalid EndAlarmPriority: %w", err)
	}
	if start > 0 && end > 0 && start > end {
		return fmt.Errorf("StartAlarmPriority must not exceed EndAlarmPriority")
	}
	method, err := formatAlarmMethodFilter(version, in.AlarmMethod)
	if err != nil {
		return fmt.Errorf("invalid AlarmMethod: %w", err)
	}
	alarmType := strings.TrimSpace(in.AlarmType)
	if alarmType != "" && !version.AtLeast(GBVersion20) {
		return fmt.Errorf("AlarmType query requires GB/T 28181-2016 or later")
	}
	if alarmType != "" && !alarmTypeMatchesMethods(version, method, alarmType) {
		return fmt.Errorf("AlarmType is invalid for AlarmMethod")
	}
	startTime, hasStart, err := parseSubscriptionTime(in.StartAlarmTime)
	if err != nil {
		return fmt.Errorf("invalid StartAlarmTime: %w", err)
	}
	endTime, hasEnd, err := parseSubscriptionTime(in.EndAlarmTime)
	if err != nil {
		return fmt.Errorf("invalid EndAlarmTime: %w", err)
	}
	if hasStart && hasEnd && endTime.Before(startTime) {
		return fmt.Errorf("StartAlarmTime must not be after EndAlarmTime")
	}
	request.StartAlarmPriority = strings.TrimSpace(in.StartAlarmPriority)
	request.EndAlarmPriority = strings.TrimSpace(in.EndAlarmPriority)
	request.AlarmMethod = method
	request.AlarmType = alarmType
	request.StartAlarmTime = strings.TrimSpace(in.StartAlarmTime)
	request.EndAlarmTime = strings.TrimSpace(in.EndAlarmTime)
	return nil
}

func normalizeDeviceQueryAction(action string) string {
	a := strings.ToLower(strings.TrimSpace(action))
	a = strings.ReplaceAll(a, "-", "_")
	switch a {
	case "status", "device_status_query":
		return deviceQueryActionDeviceStatus
	case "alarm_query", "query_alarm":
		return deviceQueryActionAlarm
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
	case deviceQueryActionDeviceInfo:
		return "DeviceInfo", nil
	case deviceQueryActionDeviceStatus:
		return "DeviceStatus", nil
	case deviceQueryActionAlarm:
		return "Alarm", nil
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
		if err := g.requireGBFeature(deviceID, "home_position_query", "看守位查询(HomePositionQuery)", func(c GBCapabilities) bool {
			return c.HomePositionQuery
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

func pendingCatalogChunkItems(current int, seen map[string]struct{}, items []Channels) ([]Channels, []string, error) {
	accepted := make([]Channels, 0, len(items))
	keys := make([]string, 0, len(items))
	chunkSeen := make(map[string]struct{}, len(items))
	for _, item := range items {
		key := strings.TrimSpace(item.ChannelID)
		if _, exists := seen[key]; exists {
			continue
		}
		if _, exists := chunkSeen[key]; exists {
			continue
		}
		if current+len(accepted) >= gbMultiResponseMaxCollectedItems {
			return nil, nil, fmt.Errorf("%w: received at least %d, safety limit %d", errMultiResponseItemLimit, current+len(accepted)+1, gbMultiResponseMaxCollectedItems)
		}
		chunkSeen[key] = struct{}{}
		accepted = append(accepted, item)
		keys = append(keys, key)
	}
	return accepted, keys, nil
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
		if out.CmdType == "Catalog" {
			pending.mu.Lock()
			if pending.catalogSeen == nil {
				pending.catalogExpected = -1
				pending.catalogSeen = make(map[string]struct{})
			}
			if decoded.catalogExpected != nil && *decoded.catalogExpected > pending.catalogExpected {
				pending.catalogExpected = *decoded.catalogExpected
			}
			var (
				acceptedItems []Channels
				acceptedKeys  []string
				acceptErr     error
			)
			if items, ok := out.Data.([]Channels); ok {
				acceptedItems, acceptedKeys, acceptErr = pendingCatalogChunkItems(len(pending.catalogItems), pending.catalogSeen, items)
			}
			if acceptErr != nil {
				pending.mu.Unlock()
				pending.operation.Cancel(acceptErr)
				return
			}
			for index, item := range acceptedItems {
				pending.catalogSeen[acceptedKeys[index]] = struct{}{}
				pending.catalogItems = append(pending.catalogItems, item)
			}
			contributed := len(acceptedItems) > 0
			accepted := contributed || pending.catalogExpected == 0 && len(pending.responseXML) == 0
			if accepted {
				pending.responseXML = append(pending.responseXML, out.XML)
				pending.catalogAppendixA4 = append(pending.catalogAppendixA4, cloneAppendixA4Objects(decoded.appendixA4)...)
			}
			complete := pending.catalogExpected >= 0 && len(pending.catalogItems) >= pending.catalogExpected
			out.Data = cloneCatalogChannels(pending.catalogItems)
			out.AppendixA4 = cloneAppendixA4Objects(pending.catalogAppendixA4)
			out.responseXML = append([]string(nil), pending.responseXML...)
			if len(pending.responseXML) > 0 {
				out.XML = pending.responseXML[0]
			}
			pending.mu.Unlock()
			if !complete {
				return
			}
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
						pending.configAppendixA4 = append(pending.configAppendixA4, cloneAppendixA4Objects(decoded.appendixA4)...)
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
				out.AppendixA4 = cloneAppendixA4Objects(pending.configAppendixA4)
			}
			pending.mu.Unlock()
			if tracking && !complete {
				return
			}
		}
		pending.operation.Deliver(func() {
			select {
			case pending.wait <- out:
			default:
			}
		})
		if pending.autoDelete && g.pendingDeviceQuery.CompareAndDelete(key, pending) {
			pending.cancel(nil)
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
	deviceID := strings.TrimSpace(ctx.DeviceID)
	var (
		admittedBinding *inboundRegistrationBinding
		unlockBinding   func()
	)
	defer func() {
		if unlockBinding != nil {
			unlockBinding()
		}
	}()
	if binding, ok := admittedInboundRegistrationBinding(ctx); ok {
		unlockBinding = g.lockRegisterOperation(deviceID)
		if !g.inboundRegistrationBindingMatchesLocked(deviceID, binding) {
			ctx.String(200, "OK")
			return
		}
		admittedBinding = &binding
	}
	if err := g.validateGenericDeviceQueryResponse(ctx, msg); err != nil {
		ctx.String(400, err.Error())
		return
	}
	if err := validateGenericQueryPayload(g.getDeviceGBProtocolVersion(ctx.DeviceID), msg.CmdType, ctx.Request.Body()); err != nil {
		ctx.String(400, err.Error())
		return
	}
	if strings.EqualFold(ctx.Request.Method(), sip.MethodMessage) {
		if _, ok := g.pendingDeviceQueryExpectedTarget(ctx.DeviceID, msg.CmdType, msg.SN); !ok {
			// 查询 Response 必须关联平台当前发出的请求；迟到或无关联响应只确认接收，不能产生状态副作用。
			ctx.Log.Warn("ignore unassociated query response", "cmd_type", msg.CmdType, "sn", msg.SN, "target_id", msg.DeviceID)
			ctx.String(200, "OK")
			return
		}
	}
	extended, err := g.validateAndDecodeAppendixA4(deviceID, msg.CmdType, ctx.Request.Body())
	if err != nil {
		ctx.String(400, err.Error())
		return
	}
	// 查询状态归属于响应中的实际目标。owner 仍是已鉴权的父设备，用于注册
	// 代次和删除门禁；stateDeviceID 可以是设备本身，也可以是其下属通道。
	// 不能把通道级 DeviceStatus/Preset/PTZ 等结果写到父设备键，否则多个
	// 通道会互相覆盖，且 GetQueryState(channelID) 永远无法取得刚收到的结果。
	stateDeviceID := firstNonEmpty(msg.DeviceID, deviceID)
	isNotify := strings.EqualFold(ctx.Request.Method(), sip.MethodNotify)
	decoded := decodedDeviceQuery{}
	// 接受的 Result=ERROR 只结束本次查询。失败报文中的数据不代表一次成功观测，
	// 不能覆盖最近一次成功快照或持久化附录 A.4 对象。PresetQuery 的 Result
	// 是旧版设备兼容字段；其他无 Result 的查询不受影响。
	if !strings.EqualFold(msg.Result, "ERROR") {
		decoded = g.decodeDeviceQueryResult(stateDeviceID, msg.CmdType, ctx.Request.Body(), extended)
	}
	if err := ctx.RespondString(200, "OK"); err != nil {
		ctx.Log.Error("respond device query", "err", err, "cmd_type", msg.CmdType, "sn", msg.SN, "target_id", msg.DeviceID)
		return
	}
	if isNotify && !g.commitOutgoingSubscriptionNotifyAfterResponse(ctx) {
		return
	}
	if unlockBinding != nil {
		g.commitDecodedQueryStateForOwnerLocked(deviceID, stateDeviceID, msg.CmdType, decoded)
	} else {
		g.commitDecodedQueryStateForOwner(deviceID, stateDeviceID, msg.CmdType, decoded)
	}
	if strings.EqualFold(ctx.Request.Method(), sip.MethodMessage) {
		g.resolvePendingDeviceQueryResult(ctx.DeviceID, msg.CmdType, msg.SN, msg.Result, ctx.Request.Body(), msg.DeviceID, decoded)
	}
	if unlockBinding != nil {
		unlockBinding()
		unlockBinding = nil
	}
	g.persistDecodedQueryForBinding(deviceID, msg.CmdType, decoded, admittedBinding)
	if isNotify {
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
	validTarget := isGBDeviceIdentifier(msg.DeviceID)
	if msg.CmdType == "Alarm" {
		validTarget = validAlarmBusinessTargetID(msg.DeviceID)
	}
	if msg.SN <= 0 || !validTarget {
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
	expected, ok := g.pendingDeviceQueryExpectedTarget(deviceID, cmdType, sn)
	if !ok {
		return false
	}
	return expected != "" && expected != strings.TrimSpace(targetID)
}

func (g *GB28181API) pendingDeviceQueryExpectedTarget(deviceID, cmdType string, sn int) (string, bool) {
	if g == nil || sn <= 0 {
		return "", false
	}
	value, ok := g.pendingDeviceQuery.Load(buildPendingQueryKey(deviceID, canonicalGBQueryCmdType(cmdType), sn))
	if !ok {
		return "", false
	}
	pending, ok := value.(*pendingQueryWait)
	if !ok || pending == nil {
		return "", false
	}
	return strings.TrimSpace(pending.targetID), true
}

func (g *GB28181API) validateAuthenticatedResponseTarget(ctx *sip.Context, targetID string) error {
	if ctx == nil || strings.TrimSpace(ctx.DeviceID) == "" {
		return fmt.Errorf("response requires authenticated device")
	}
	targetID = strings.TrimSpace(targetID)
	if targetID == strings.TrimSpace(ctx.DeviceID) {
		return nil
	}
	if len(targetID) == 10 && isNumericIdentifier(targetID) && strings.HasPrefix(strings.TrimSpace(ctx.DeviceID), targetID) {
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
	case "Alarm", "DeviceInfo", "DeviceStatus":
		return GBVersion10, true
	case "PresetQuery", "ConfigDownload", "DeviceConfig":
		return GBVersion11, true
	case "HomePositionQuery", "CruiseTrackListQuery", "CruiseTrackQuery", "PTZPosition", "SDCardStatus":
		return GBVersion30, true
	default:
		return "", false
	}
}
