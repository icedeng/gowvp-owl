package gbs

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
)

// QueryState 保存设备最近一次结构化查询/状态结果。
//
// 说明：
// 1. 用于补齐 9.5/9.6 的结构化语义。
// 2. 为上层接口提供可复用的解析结果，避免重复 XML 解析。
type QueryState struct {
	ownerDeviceID   string
	UpdatedAt       time.Time            `json:"updated_at"`
	DeviceStatus    *DeviceStatusData    `json:"device_status,omitempty"`
	Presets         []PresetItemData     `json:"presets,omitempty"`
	HomePosition    *HomePositionData    `json:"home_position,omitempty"`
	CruiseTracks    []CruiseTrackData    `json:"cruise_tracks,omitempty"`
	CruiseTrack     *CruiseTrackData     `json:"cruise_track,omitempty"`
	PTZPosition     *PTZPositionData     `json:"ptz_position,omitempty"`
	SDCards         []SDCardItemData     `json:"sd_cards,omitempty"`
	MobilePosition  *MobilePositionData  `json:"mobile_position,omitempty"`
	MobilePositions []MobilePositionData `json:"mobile_positions,omitempty"`
	VideoUpload     *VideoUploadData     `json:"video_upload,omitempty"`
	ConfigDownload  *ConfigDownloadState `json:"config_download,omitempty"`
	DeviceConfig    *DeviceConfigState   `json:"device_config,omitempty"`
	// AppendixA4 保存附录 A.4 扩展对象结构化快照。
	AppendixA4 []AppendixA4Object `json:"appendix_a4,omitempty"`
}

const (
	queryStateTTL        = 7 * 24 * time.Hour
	maxQueryStateEntries = 4096
)

type queryStateCleanupEntry struct {
	deviceID string
	state    *QueryState
}

// DeviceStatusData 对应 DeviceStatus 查询结果。
type DeviceStatusData struct {
	CmdType        string                  `json:"cmd_type"`
	SN             int                     `json:"sn"`
	DeviceID       string                  `json:"device_id"`
	Result         string                  `json:"result,omitempty"`
	Online         string                  `json:"online,omitempty"`
	Status         string                  `json:"status,omitempty"`
	Reason         string                  `json:"reason,omitempty"`
	DeviceTime     string                  `json:"device_time,omitempty"`
	Encode         string                  `json:"encode,omitempty"`
	Record         string                  `json:"record,omitempty"`
	FaultDeviceIDs []string                `json:"fault_device_ids,omitempty"`
	AlarmStatuses  []DeviceAlarmStatusData `json:"alarm_statuses,omitempty"`
	Info           []string                `json:"info,omitempty"`
	ExtraInfo      []string                `json:"extra_info,omitempty"`
}

// DeviceAlarmStatusData 对应 DeviceStatus Alarmstatus 中的单个报警设备状态。
type DeviceAlarmStatusData struct {
	DeviceID   string `json:"device_id,omitempty"`
	DutyStatus string `json:"duty_status,omitempty"`
}

// PresetItemData 是预置位查询条目。
type PresetItemData struct {
	PresetID   string `json:"preset_id"`
	PresetName string `json:"preset_name"`
}

// HomePositionData 是看守位查询结果。
type HomePositionData struct {
	Enabled     *int `json:"enabled,omitempty"`
	ResetTime   *int `json:"reset_time,omitempty"`
	PresetIndex *int `json:"preset_index,omitempty"`
}

// CruiseTrackData 是巡航轨迹列表或详情。
type CruiseTrackData struct {
	Number int               `json:"number"`
	Name   string            `json:"name,omitempty"`
	Points []CruisePointData `json:"points,omitempty"`
}

// CruisePointData 是巡航轨迹中的预置位、停留时间和速度。
type CruisePointData struct {
	PresetIndex int `json:"preset_index"`
	StayTime    int `json:"stay_time"`
	Speed       int `json:"speed"`
}

// PTZPositionData 是 PTZ 精准状态结果。
type PTZPositionData struct {
	Pan                  *float64 `json:"pan,omitempty"`
	Tilt                 *float64 `json:"tilt,omitempty"`
	Zoom                 *float64 `json:"zoom,omitempty"`
	HorizontalFieldAngle *float64 `json:"horizontal_field_angle,omitempty"`
	VerticalFieldAngle   *float64 `json:"vertical_field_angle,omitempty"`
	MaxViewDistance      *float64 `json:"max_view_distance,omitempty"`
}

// SDCardItemData 是存储卡状态条目。
type SDCardItemData struct {
	ID             int    `json:"id"`
	HddName        string `json:"hdd_name,omitempty"`
	Status         string `json:"status,omitempty"`
	FormatProgress *int   `json:"format_progress,omitempty"`
	Capacity       *int   `json:"capacity,omitempty"`
	FreeSpace      *int   `json:"free_space,omitempty"`
}

// MobilePositionData 是移动位置状态。
type MobilePositionData struct {
	DeviceID    string   `json:"device_id,omitempty"`
	Time        string   `json:"time,omitempty"`
	CaptureTime string   `json:"capture_time,omitempty"`
	Longitude   *float64 `json:"longitude,omitempty"`
	Latitude    *float64 `json:"latitude,omitempty"`
	Speed       *float64 `json:"speed,omitempty"`
	Direction   *float64 `json:"direction,omitempty"`
	Altitude    *float64 `json:"altitude,omitempty"`
	Height      *float64 `json:"height,omitempty"`
}

// VideoUploadData 是 2022 A.2.5.8 设备实时视音频回传通知。
type VideoUploadData struct {
	Time      string   `json:"time"`
	Longitude *float64 `json:"longitude,omitempty"`
	Latitude  *float64 `json:"latitude,omitempty"`
}

type videoUploadNotifyXML struct {
	XMLName   xml.Name `xml:"Notify"`
	CmdType   string   `xml:"CmdType"`
	SN        int      `xml:"SN"`
	DeviceID  string   `xml:"DeviceID"`
	Time      string   `xml:"Time"`
	Longitude *float64 `xml:"Longitude"`
	Latitude  *float64 `xml:"Latitude"`
}

// ConfigDownloadState 是配置查询结果快照。
type ConfigDownloadState struct {
	CmdType             string               `json:"cmd_type"`
	SN                  int                  `json:"sn"`
	DeviceID            string               `json:"device_id"`
	Result              string               `json:"result,omitempty"`
	BasicParam          *BasicParam          `json:"basic_param,omitempty"`
	VideoParamOpt       *VideoParamOpt       `json:"video_param_opt,omitempty"`
	VideoParamConfig    *VideoParamConfig    `json:"video_param_config,omitempty"`
	AudioParamOpt       *AudioParamOpt       `json:"audio_param_opt,omitempty"`
	AudioParamConfig    *AudioParamConfig    `json:"audio_param_config,omitempty"`
	SVACEncodeConfig    *SVACEncodeConfig    `json:"svac_encode_config,omitempty"`
	SVACDecodeConfig    *SVACDecodeConfig    `json:"svac_decode_config,omitempty"`
	VideoParamAttribute *VideoParamAttribute `json:"video_param_attribute,omitempty"`
	VideoRecordPlan     *VideoRecordPlan     `json:"video_record_plan,omitempty"`
	VideoAlarmRecord    *VideoAlarmRecord    `json:"video_alarm_record,omitempty"`
	PictureMask         *PictureMask         `json:"picture_mask,omitempty"`
	FrameMirror         *FrameMirror         `json:"frame_mirror,omitempty"`
	AlarmReport         *AlarmReport         `json:"alarm_report,omitempty"`
	OSDConfig           *OSDConfig           `json:"osd_config,omitempty"`
	SnapShot            *SnapShot            `json:"snapshot,omitempty"`
	RawXML              string               `json:"raw_xml,omitempty"`
}

// DeviceConfigState 是设备配置应答快照。
type DeviceConfigState struct {
	CmdType  string    `json:"cmd_type"`
	SN       int       `json:"sn"`
	DeviceID string    `json:"device_id"`
	Result   string    `json:"result,omitempty"`
	SnapShot *SnapShot `json:"snapshot,omitempty"`
	RawXML   string    `json:"raw_xml,omitempty"`
}

// GetQueryState 获取设备最新结构化状态。
func (g *GB28181API) GetQueryState(deviceID string) (*QueryState, bool) {
	g.queryStateMu.RLock()
	defer g.queryStateMu.RUnlock()
	v, ok := g.queryStates.Load(strings.TrimSpace(deviceID))
	if !ok {
		return nil, false
	}
	state, ok := v.(*QueryState)
	if !ok || state == nil {
		return nil, false
	}
	return cloneQueryState(state), true
}

func cloneQueryState(state *QueryState) *QueryState {
	if state == nil {
		return nil
	}
	out := *state
	out.DeviceStatus = cloneValue(state.DeviceStatus)
	if out.DeviceStatus != nil {
		out.DeviceStatus.FaultDeviceIDs = append([]string(nil), state.DeviceStatus.FaultDeviceIDs...)
		out.DeviceStatus.AlarmStatuses = append([]DeviceAlarmStatusData(nil), state.DeviceStatus.AlarmStatuses...)
		out.DeviceStatus.Info = append([]string(nil), state.DeviceStatus.Info...)
		out.DeviceStatus.ExtraInfo = append([]string(nil), state.DeviceStatus.ExtraInfo...)
	}
	out.Presets = append([]PresetItemData(nil), state.Presets...)
	out.HomePosition = cloneValue(state.HomePosition)
	if out.HomePosition != nil {
		out.HomePosition.Enabled = cloneValue(state.HomePosition.Enabled)
		out.HomePosition.ResetTime = cloneValue(state.HomePosition.ResetTime)
		out.HomePosition.PresetIndex = cloneValue(state.HomePosition.PresetIndex)
	}
	out.CruiseTracks = cloneCruiseTracks(state.CruiseTracks)
	out.CruiseTrack = cloneCruiseTrack(state.CruiseTrack)
	out.PTZPosition = cloneValue(state.PTZPosition)
	if out.PTZPosition != nil {
		out.PTZPosition.Pan = cloneValue(state.PTZPosition.Pan)
		out.PTZPosition.Tilt = cloneValue(state.PTZPosition.Tilt)
		out.PTZPosition.Zoom = cloneValue(state.PTZPosition.Zoom)
		out.PTZPosition.HorizontalFieldAngle = cloneValue(state.PTZPosition.HorizontalFieldAngle)
		out.PTZPosition.VerticalFieldAngle = cloneValue(state.PTZPosition.VerticalFieldAngle)
		out.PTZPosition.MaxViewDistance = cloneValue(state.PTZPosition.MaxViewDistance)
	}
	out.SDCards = append([]SDCardItemData(nil), state.SDCards...)
	for index := range out.SDCards {
		out.SDCards[index].FormatProgress = cloneValue(state.SDCards[index].FormatProgress)
		out.SDCards[index].Capacity = cloneValue(state.SDCards[index].Capacity)
		out.SDCards[index].FreeSpace = cloneValue(state.SDCards[index].FreeSpace)
	}
	out.MobilePosition = cloneValue(state.MobilePosition)
	if out.MobilePosition != nil {
		out.MobilePosition.Longitude = cloneValue(state.MobilePosition.Longitude)
		out.MobilePosition.Latitude = cloneValue(state.MobilePosition.Latitude)
		out.MobilePosition.Speed = cloneValue(state.MobilePosition.Speed)
		out.MobilePosition.Direction = cloneValue(state.MobilePosition.Direction)
		out.MobilePosition.Altitude = cloneValue(state.MobilePosition.Altitude)
		out.MobilePosition.Height = cloneValue(state.MobilePosition.Height)
	}
	out.MobilePositions = cloneMobilePositions(state.MobilePositions)
	out.VideoUpload = cloneValue(state.VideoUpload)
	if out.VideoUpload != nil {
		out.VideoUpload.Longitude = cloneValue(state.VideoUpload.Longitude)
		out.VideoUpload.Latitude = cloneValue(state.VideoUpload.Latitude)
	}
	out.ConfigDownload = cloneConfigDownloadState(state.ConfigDownload)
	out.DeviceConfig = cloneValue(state.DeviceConfig)
	if out.DeviceConfig != nil {
		out.DeviceConfig.SnapShot = cloneValue(state.DeviceConfig.SnapShot)
	}
	out.AppendixA4 = cloneAppendixA4Objects(state.AppendixA4)
	return &out
}

func cloneMobilePositions(items []MobilePositionData) []MobilePositionData {
	out := append([]MobilePositionData(nil), items...)
	for index := range out {
		out[index].Longitude = cloneValue(items[index].Longitude)
		out[index].Latitude = cloneValue(items[index].Latitude)
		out[index].Speed = cloneValue(items[index].Speed)
		out[index].Direction = cloneValue(items[index].Direction)
		out[index].Altitude = cloneValue(items[index].Altitude)
		out[index].Height = cloneValue(items[index].Height)
	}
	return out
}

func cloneValue[T any](value *T) *T {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneCruiseTracks(tracks []CruiseTrackData) []CruiseTrackData {
	out := append([]CruiseTrackData(nil), tracks...)
	for index := range out {
		out[index].Points = append([]CruisePointData(nil), tracks[index].Points...)
	}
	return out
}

func cloneCruiseTrack(track *CruiseTrackData) *CruiseTrackData {
	out := cloneValue(track)
	if out != nil {
		out.Points = append([]CruisePointData(nil), track.Points...)
	}
	return out
}

func cloneConfigDownloadState(state *ConfigDownloadState) *ConfigDownloadState {
	out := cloneValue(state)
	if out == nil {
		return nil
	}
	out.BasicParam = cloneValue(state.BasicParam)
	out.VideoParamOpt = cloneValue(state.VideoParamOpt)
	out.VideoParamConfig = cloneValue(state.VideoParamConfig)
	out.AudioParamOpt = cloneValue(state.AudioParamOpt)
	out.AudioParamConfig = cloneValue(state.AudioParamConfig)
	if out.VideoParamOpt != nil {
		out.VideoParamOpt.Attributes = append([]xml.Attr(nil), state.VideoParamOpt.Attributes...)
	}
	if out.VideoParamConfig != nil {
		out.VideoParamConfig.Num = cloneValue(state.VideoParamConfig.Num)
		out.VideoParamConfig.Attributes = append([]xml.Attr(nil), state.VideoParamConfig.Attributes...)
	}
	if out.AudioParamOpt != nil {
		out.AudioParamOpt.Attributes = append([]xml.Attr(nil), state.AudioParamOpt.Attributes...)
	}
	if out.AudioParamConfig != nil {
		out.AudioParamConfig.Num = cloneValue(state.AudioParamConfig.Num)
		out.AudioParamConfig.Attributes = append([]xml.Attr(nil), state.AudioParamConfig.Attributes...)
	}
	out.SVACEncodeConfig = cloneValue(state.SVACEncodeConfig)
	out.SVACDecodeConfig = cloneValue(state.SVACDecodeConfig)
	out.VideoParamAttribute = cloneValue(state.VideoParamAttribute)
	out.VideoRecordPlan = cloneValue(state.VideoRecordPlan)
	out.VideoAlarmRecord = cloneValue(state.VideoAlarmRecord)
	out.PictureMask = cloneValue(state.PictureMask)
	out.FrameMirror = cloneValue(state.FrameMirror)
	out.AlarmReport = cloneValue(state.AlarmReport)
	out.OSDConfig = cloneValue(state.OSDConfig)
	out.SnapShot = cloneValue(state.SnapShot)
	return out
}

func cloneAppendixA4Objects(objects []AppendixA4Object) []AppendixA4Object {
	out := append([]AppendixA4Object(nil), objects...)
	for index := range out {
		out[index].Fields = maps.Clone(objects[index].Fields)
	}
	return out
}

func cloneDeviceQueryData(data any) any {
	state := &QueryState{}
	switch value := data.(type) {
	case *DeviceStatusData:
		state.DeviceStatus = value
		return cloneQueryState(state).DeviceStatus
	case []PresetItemData:
		state.Presets = value
		return cloneQueryState(state).Presets
	case *HomePositionData:
		state.HomePosition = value
		return cloneQueryState(state).HomePosition
	case []CruiseTrackData:
		state.CruiseTracks = value
		return cloneQueryState(state).CruiseTracks
	case *CruiseTrackData:
		state.CruiseTrack = value
		return cloneQueryState(state).CruiseTrack
	case *PTZPositionData:
		state.PTZPosition = value
		return cloneQueryState(state).PTZPosition
	case []SDCardItemData:
		state.SDCards = value
		return cloneQueryState(state).SDCards
	case *MobilePositionData:
		state.MobilePosition = value
		return cloneQueryState(state).MobilePosition
	case *VideoUploadData:
		state.VideoUpload = value
		return cloneQueryState(state).VideoUpload
	case *ConfigDownloadState:
		state.ConfigDownload = value
		return cloneQueryState(state).ConfigDownload
	case []Channels:
		return cloneCatalogChannels(value)
	default:
		return nil
	}
}

func cloneCatalogChannels(items []Channels) []Channels {
	out := append([]Channels(nil), items...)
	for index := range out {
		if items[index].addr != nil {
			out[index].addr = items[index].addr.Clone()
		}
	}
	return out
}

func (g *GB28181API) cleanupQueryStates(now time.Time) {
	if g == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-queryStateTTL)
	g.queryStateMu.Lock()
	defer g.queryStateMu.Unlock()
	entries := make([]queryStateCleanupEntry, 0)
	g.queryStates.Range(func(key, value any) bool {
		deviceID, keyOK := key.(string)
		state, stateOK := value.(*QueryState)
		if !keyOK || !stateOK || state == nil || state.UpdatedAt.IsZero() || !state.UpdatedAt.After(cutoff) {
			g.queryStates.CompareAndDelete(key, value)
			return true
		}
		entries = append(entries, queryStateCleanupEntry{deviceID: deviceID, state: state})
		return true
	})
	if len(entries) <= maxQueryStateEntries {
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].state.UpdatedAt.Equal(entries[j].state.UpdatedAt) {
			return entries[i].deviceID < entries[j].deviceID
		}
		return entries[i].state.UpdatedAt.Before(entries[j].state.UpdatedAt)
	})
	for _, entry := range entries[:len(entries)-maxQueryStateEntries] {
		g.queryStates.CompareAndDelete(entry.deviceID, entry.state)
	}
}

type decodedDeviceQuery struct {
	data            any
	appendixA4      []AppendixA4Object
	catalogExpected *int
}

func (g *GB28181API) decodeAndStoreQueryResult(deviceID, cmdType string, body []byte, validatedAppendixA4 ...[]AppendixA4Object) decodedDeviceQuery {
	result := g.decodeDeviceQueryResult(deviceID, cmdType, body, validatedAppendixA4...)
	g.commitDecodedQueryState(deviceID, cmdType, result)
	return result
}

func (g *GB28181API) decodeDeviceQueryResult(deviceID, cmdType string, body []byte, validatedAppendixA4 ...[]AppendixA4Object) decodedDeviceQuery {
	cmd := strings.TrimSpace(cmdType)
	deviceID = strings.TrimSpace(deviceID)
	if cmd == "" || len(body) == 0 || deviceID == "" {
		return decodedDeviceQuery{}
	}
	result := decodedDeviceQuery{}
	if len(validatedAppendixA4) > 0 {
		result.appendixA4 = cloneAppendixA4Objects(validatedAppendixA4[0])
	} else if g.getDeviceGBProtocolVersion(deviceID).AtLeast(GBVersion30) {
		result.appendixA4 = g.decodeAppendixA4Objects(cmd, body)
	}
	switch cmd {
	case "DeviceStatus":
		result.data = decodeDeviceStatusData(body)
	case "PresetQuery":
		result.data = decodePresetQueryData(body)
	case "HomePositionQuery":
		result.data = decodeHomePositionData(body)
	case "CruiseTrackListQuery":
		result.data = decodeCruiseTrackListData(body)
	case "CruiseTrackQuery":
		result.data = decodeCruiseTrackData(body)
	case "PTZPosition":
		result.data = decodePTZPositionData(body)
	case "SDCardStatus":
		result.data = decodeSDCardStatusData(body)
	case "MobilePosition":
		result.data = decodeMobilePositionData(body)
	case "VideoUploadNotify":
		result.data = decodeVideoUploadData(body)
	case "ConfigDownload":
		result.data = decodeConfigDownloadState(body)
	default:
		return result
	}
	return result
}

func (g *GB28181API) commitDecodedQueryState(deviceID, cmdType string, result decodedDeviceQuery) {
	g.commitDecodedQueryStateForOwner(deviceID, deviceID, cmdType, result)
}

func (g *GB28181API) commitDecodedQueryStateForOwner(ownerDeviceID, stateDeviceID, cmdType string, result decodedDeviceQuery) {
	g.withQueryStateOwner(ownerDeviceID, func(ownerDeviceID string) {
		g.commitDecodedQueryStateForOwnerLocked(ownerDeviceID, stateDeviceID, cmdType, result)
	})
}

// commitDecodedQueryStateForOwnerLocked 在调用方已持有设备注册操作锁时提交查询状态。
func (g *GB28181API) commitDecodedQueryStateForOwnerLocked(ownerDeviceID, stateDeviceID, cmdType string, result decodedDeviceQuery) {
	if !g.queryStateOwnerWritableLocked(ownerDeviceID) {
		return
	}
	if len(result.appendixA4) > 0 {
		g.storeAppendixA4StateForOwnerLocked(ownerDeviceID, stateDeviceID, result.appendixA4)
	}
	cmdType = strings.TrimSpace(cmdType)
	switch cmdType {
	case "DeviceStatus", "PresetQuery", "HomePositionQuery", "CruiseTrackListQuery", "CruiseTrackQuery",
		"PTZPosition", "SDCardStatus", "MobilePosition", "VideoUploadNotify", "ConfigDownload":
		if result.data != nil {
			g.storeQueryStateForOwnerLocked(ownerDeviceID, stateDeviceID, cmdType, result.data)
		}
	}
}

func (g *GB28181API) persistDecodedQuery(deviceID, cmdType string, result decodedDeviceQuery) {
	g.persistDecodedQueryForBinding(deviceID, cmdType, result, nil)
}

func (g *GB28181API) persistDecodedQueryForBinding(deviceID, cmdType string, result decodedDeviceQuery, expected *inboundRegistrationBinding) {
	if cmdType == "DeviceStatus" {
		if status, ok := result.data.(*DeviceStatusData); ok {
			if strings.EqualFold(status.Result, "OK") && status.DeviceID == strings.TrimSpace(deviceID) {
				if err := g.applyDeviceStatusForBinding(deviceID, status, expected); err != nil {
					slog.Error("persist DeviceStatus failed", "device_id", deviceID, "err", err)
				}
			}
		}
	}
	if len(result.appendixA4) > 0 {
		g.persistAppendixA4Objects(deviceID, result.appendixA4)
	}
}

func (g *GB28181API) storeQueryState(deviceID, cmdType string, data any) {
	g.storeQueryStateForOwner(deviceID, deviceID, cmdType, data)
}

func (g *GB28181API) storeQueryStateForOwner(ownerDeviceID, stateDeviceID, cmdType string, data any) {
	g.withQueryStateOwner(ownerDeviceID, func(ownerDeviceID string) {
		g.storeQueryStateForOwnerLocked(ownerDeviceID, stateDeviceID, cmdType, data)
	})
}

// storeQueryStateForOwnerLocked 在调用方已持有设备注册操作锁时更新目标状态。
func (g *GB28181API) storeQueryStateForOwnerLocked(ownerDeviceID, stateDeviceID, cmdType string, data any) {
	ownerDeviceID = strings.TrimSpace(ownerDeviceID)
	stateDeviceID = strings.TrimSpace(stateDeviceID)
	if ownerDeviceID == "" || stateDeviceID == "" || data == nil || !g.queryStateOwnerWritableLocked(ownerDeviceID) {
		return
	}
	g.queryStateMu.Lock()
	defer g.queryStateMu.Unlock()
	state := g.queryStateForUpdateLocked(ownerDeviceID, stateDeviceID)
	state.UpdatedAt = time.Now()
	switch cmdType {
	case "DeviceStatus":
		if s, ok := data.(*DeviceStatusData); ok {
			state.DeviceStatus = s
		}
	case "PresetQuery":
		if items, ok := data.([]PresetItemData); ok {
			state.Presets = items
		}
	case "HomePositionQuery":
		if s, ok := data.(*HomePositionData); ok {
			state.HomePosition = s
		}
	case "CruiseTrackListQuery":
		if items, ok := data.([]CruiseTrackData); ok {
			state.CruiseTracks = items
		}
	case "CruiseTrackQuery":
		if item, ok := data.(*CruiseTrackData); ok {
			state.CruiseTrack = item
		}
	case "PTZPosition":
		if s, ok := data.(*PTZPositionData); ok {
			state.PTZPosition = s
		}
	case "SDCardStatus":
		if items, ok := data.([]SDCardItemData); ok {
			state.SDCards = items
		}
	case "MobilePosition":
		if s, ok := data.(*MobilePositionData); ok {
			state.MobilePosition = s
		}
	case "VideoUploadNotify":
		if s, ok := data.(*VideoUploadData); ok {
			state.VideoUpload = s
		}
	case "ConfigDownload":
		if s, ok := data.(*ConfigDownloadState); ok {
			merged := &ConfigDownloadState{}
			if state.ConfigDownload != nil {
				*merged = *state.ConfigDownload
			}
			mergeConfigDownloadState(merged, s)
			state.ConfigDownload = merged
		}
	}
	g.queryStates.Store(stateDeviceID, state)
}

func (g *GB28181API) storeDeviceConfigState(deviceID string, state *DeviceConfigState) {
	g.storeDeviceConfigStateForOwner(deviceID, deviceID, state)
}

func (g *GB28181API) storeDeviceConfigStateForOwner(ownerDeviceID, stateDeviceID string, state *DeviceConfigState) {
	g.withQueryStateOwner(ownerDeviceID, func(ownerDeviceID string) {
		g.storeDeviceConfigStateForOwnerLocked(ownerDeviceID, stateDeviceID, state)
	})
}

func (g *GB28181API) storeDeviceConfigStateForOwnerLocked(ownerDeviceID, stateDeviceID string, state *DeviceConfigState) {
	ownerDeviceID = strings.TrimSpace(ownerDeviceID)
	stateDeviceID = strings.TrimSpace(stateDeviceID)
	if ownerDeviceID == "" || stateDeviceID == "" || state == nil || !g.queryStateOwnerWritableLocked(ownerDeviceID) {
		return
	}
	g.queryStateMu.Lock()
	defer g.queryStateMu.Unlock()
	curr := g.queryStateForUpdateLocked(ownerDeviceID, stateDeviceID)
	curr.UpdatedAt = time.Now()
	curr.DeviceConfig = state
	g.queryStates.Store(stateDeviceID, curr)
}

func (g *GB28181API) storeAppendixA4State(deviceID string, objs []AppendixA4Object) {
	g.storeAppendixA4StateForOwner(deviceID, deviceID, objs)
}

func (g *GB28181API) storeAppendixA4StateForOwner(ownerDeviceID, stateDeviceID string, objs []AppendixA4Object) {
	g.withQueryStateOwner(ownerDeviceID, func(ownerDeviceID string) {
		g.storeAppendixA4StateForOwnerLocked(ownerDeviceID, stateDeviceID, objs)
	})
}

func (g *GB28181API) storeAppendixA4StateForOwnerLocked(ownerDeviceID, stateDeviceID string, objs []AppendixA4Object) {
	ownerDeviceID = strings.TrimSpace(ownerDeviceID)
	stateDeviceID = strings.TrimSpace(stateDeviceID)
	if ownerDeviceID == "" || stateDeviceID == "" || len(objs) == 0 || !g.queryStateOwnerWritableLocked(ownerDeviceID) {
		return
	}
	g.queryStateMu.Lock()
	defer g.queryStateMu.Unlock()
	state := g.queryStateForUpdateLocked(ownerDeviceID, stateDeviceID)
	state.UpdatedAt = time.Now()
	state.AppendixA4 = mergeAppendixA4Objects(state.AppendixA4, objs, 128)
	g.queryStates.Store(stateDeviceID, state)
}

func (g *GB28181API) withQueryStateOwner(ownerDeviceID string, commit func(string)) {
	ownerDeviceID = strings.TrimSpace(ownerDeviceID)
	if g == nil || ownerDeviceID == "" || commit == nil {
		return
	}
	if g.svr == nil || g.svr.memoryStorer == nil {
		commit(ownerDeviceID)
		return
	}
	unlock := g.lockRegisterOperation(ownerDeviceID)
	defer unlock()
	commit(ownerDeviceID)
}

// queryStateOwnerWritableLocked 必须在持有 ownerDeviceID 的注册操作锁时用于生产路径。
// 运行时缓存可能短暂缺失；只有明确执行过设备删除，才拒绝迟到状态提交。
func (g *GB28181API) queryStateOwnerWritableLocked(ownerDeviceID string) bool {
	ownerDeviceID = strings.TrimSpace(ownerDeviceID)
	if g == nil || ownerDeviceID == "" {
		return false
	}
	return !g.deviceDeletionActiveLocked(ownerDeviceID)
}

// queryStateForUpdateLocked 返回归属于父设备的目标状态副本；调用方必须持有 queryStateMu。
func (g *GB28181API) queryStateForUpdateLocked(ownerDeviceID, stateDeviceID string) *QueryState {
	state := &QueryState{ownerDeviceID: ownerDeviceID}
	if value, ok := g.queryStates.Load(stateDeviceID); ok {
		if old, valid := value.(*QueryState); valid && old != nil &&
			(old.ownerDeviceID == "" || old.ownerDeviceID == ownerDeviceID) {
			*state = *old
			state.ownerDeviceID = ownerDeviceID
		}
	}
	return state
}

// persistAppendixA4Objects 将附录 A.4 结构化结果持久化到设备 ext 字段。
func (g *GB28181API) persistAppendixA4Objects(deviceID string, objs []AppendixA4Object) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || len(objs) == 0 {
		return
	}
	var dev ipc.Device
	if err := g.core.Store().Device().Update(g.serviceContext(), &dev, func(d *ipc.Device) error {
		exist := fromIPCAppendixA4Objects(d.Ext.GBAppendixA4)
		merged := mergeAppendixA4Objects(exist, objs, 256)
		d.Ext.GBAppendixA4 = toIPCAppendixA4Objects(merged)
		return nil
	}, orm.Where("device_id=?", deviceID)); err != nil {
		slog.Warn("persist appendix a4 failed", "device_id", deviceID, "err", err)
	}
}

func mergeAppendixA4Objects(base, inc []AppendixA4Object, max int) []AppendixA4Object {
	if max <= 0 {
		max = 128
	}
	positions := make(map[string]int, len(base)+len(inc))
	out := make([]AppendixA4Object, 0, len(base)+len(inc))
	appendOrReplace := func(item AppendixA4Object) {
		key := appendixA4ObjectKey(item)
		if index, ok := positions[key]; ok {
			old := out[index]
			// 取更新的记录，避免旧值覆盖新值；相同对象保留首次出现位置。
			if item.UpdatedAt < old.UpdatedAt {
				item.UpdatedAt = old.UpdatedAt
			}
			out[index] = item
			return
		}
		positions[key] = len(out)
		out = append(out, item)
	}
	for _, item := range base {
		appendOrReplace(item)
	}
	for _, item := range inc {
		appendOrReplace(item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	if len(out) > max {
		out = out[:max]
	}
	return out
}

func toIPCAppendixA4Objects(in []AppendixA4Object) []ipc.GBAppendixA4Object {
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

func fromIPCAppendixA4Objects(in []ipc.GBAppendixA4Object) []AppendixA4Object {
	if len(in) == 0 {
		return nil
	}
	out := make([]AppendixA4Object, 0, len(in))
	for _, item := range in {
		out = append(out, AppendixA4Object{
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

func (g *GB28181API) applyDeviceStatus(deviceID string, in *DeviceStatusData) error {
	return g.applyDeviceStatusForBinding(deviceID, in, nil)
}

func (g *GB28181API) applyDeviceStatusForBinding(deviceID string, in *DeviceStatusData, expected *inboundRegistrationBinding) error {
	if in == nil {
		return nil
	}
	unlockActivity := g.lockRegisterOperation(deviceID)
	defer unlockActivity()
	device, ok := g.svr.memoryStorer.Load(deviceID)
	if !ok || device == nil {
		return nil
	}
	state := device.runtimeSnapshot()
	if expected != nil && (state.Expires != expected.expires || !state.LastRegisterAt.Equal(expected.lastRegisterAt)) {
		return nil
	}
	if state.RegistrationClosed || state.OfflinePersistencePending ||
		registrationBindingExpired(state.LastRegisterAt, state.Expires, time.Now()) {
		return nil
	}
	// Online 表示设备是否在线；Status 只表示设备是否正常工作，二者不能互相替代。
	online := strings.EqualFold(strings.TrimSpace(in.Online), "ONLINE")
	observedAt := time.Now()
	if err := g.persistDeviceStatusState(deviceID, online, observedAt); err != nil {
		device.UpdateRuntime(func(current *Device) {
			if current.registrationClosed || current.offlinePersistencePending ||
				current.Expires != state.Expires || !current.LastRegisterAt.Equal(state.LastRegisterAt) {
				return
			}
			clearPendingKeepaliveLocked(current)
			current.deviceStatusPersistencePending = true
			current.pendingDeviceStatusOnline = online
			current.pendingDeviceStatusAt = observedAt
		})
		return fmt.Errorf("persist DeviceStatus for %s: %w", deviceID, err)
	}
	if online {
		g.deviceOfflineTombstones.Delete(deviceID)
	}
	return nil
}

func (g *GB28181API) persistDeviceStatusState(deviceID string, online bool, observedAt time.Time) error {
	return g.svr.changeMemory(g.serviceContext(), deviceID, func(d *ipc.Device) error {
		d.IsOnline = online
		d.KeepaliveAt = orm.Time{Time: observedAt}
		setPersistedRegistrationClosed(d, false)
		return nil
	}, func(d *Device) {
		d.IsOnline = online
		d.LastKeepaliveAt = observedAt
		d.offlinePersistencePending = false
		clearPendingDeviceStatusLocked(d)
		clearPendingKeepaliveLocked(d)
	})
}

func samePendingDeviceStatus(current, expected deviceRuntimeState) bool {
	return current.DeviceStatusPersistencePending && expected.DeviceStatusPersistencePending &&
		current.PendingDeviceStatusOnline == expected.PendingDeviceStatusOnline &&
		current.PendingDeviceStatusAt.Equal(expected.PendingDeviceStatusAt) &&
		current.Expires == expected.Expires && current.LastRegisterAt.Equal(expected.LastRegisterAt)
}

func (g *GB28181API) retryPendingDeviceStatus(deviceID string, expected deviceRuntimeState) (bool, error) {
	unlockActivity := g.lockRegisterOperation(deviceID)
	defer unlockActivity()
	device, ok := g.svr.memoryStorer.Load(deviceID)
	if !ok || device == nil {
		return false, nil
	}
	current := device.runtimeSnapshot()
	if !samePendingDeviceStatus(current, expected) {
		return false, nil
	}
	if current.RegistrationClosed || current.OfflinePersistencePending ||
		registrationBindingExpired(current.LastRegisterAt, current.Expires, time.Now()) {
		device.UpdateRuntime(func(latest *Device) {
			if latest.deviceStatusPersistencePending &&
				latest.pendingDeviceStatusOnline == current.PendingDeviceStatusOnline &&
				latest.pendingDeviceStatusAt.Equal(current.PendingDeviceStatusAt) &&
				latest.Expires == current.Expires && latest.LastRegisterAt.Equal(current.LastRegisterAt) {
				clearPendingDeviceStatusLocked(latest)
			}
		})
		return false, nil
	}
	if err := g.persistDeviceStatusState(deviceID, current.PendingDeviceStatusOnline, current.PendingDeviceStatusAt); err != nil {
		return false, fmt.Errorf("retry DeviceStatus persistence for %s: %w", deviceID, err)
	}
	return true, nil
}

type queryDeviceStatusXML struct {
	CmdType    string                `xml:"CmdType"`
	SN         int                   `xml:"SN"`
	DeviceID   string                `xml:"DeviceID"`
	Result     string                `xml:"Result"`
	Online     string                `xml:"Online"`
	Status     string                `xml:"Status"`
	Reason     string                `xml:"Reason"`
	DeviceTime *string               `xml:"DeviceTime"`
	Encode     *string               `xml:"Encode"`
	Record     *string               `xml:"Record"`
	Alarm      *deviceAlarmStatusXML `xml:"Alarmstatus"`
	Info       []versionedInfoXML    `xml:"Info"`
	ExtraInfo  []string              `xml:"ExtraInfo"`
}

type versionedInfoXML struct {
	Content  string      `xml:",chardata"`
	Children []a4XMLNode `xml:",any"`
}

type deviceAlarmStatusXML struct {
	Num      *int                       `xml:"Num,attr"`
	LowerNum *int                       `xml:"num,attr"`
	Items    []deviceAlarmStatusItemXML `xml:"Item"`
}

type deviceAlarmStatusItemXML struct {
	DeviceID         *string `xml:"DeviceID"`
	Status           *string `xml:"Status"`
	StatusDutyStatus *string `xml:"StatusDutyStatus"`
	DutyStatus       *string `xml:"DutyStatus"`
}

func decodeDeviceStatusData(body []byte) *DeviceStatusData {
	var msg queryDeviceStatusXML
	if err := sip.XMLDecode(body, &msg); err != nil {
		return nil
	}
	data := &DeviceStatusData{
		CmdType:  strings.TrimSpace(msg.CmdType),
		SN:       msg.SN,
		DeviceID: strings.TrimSpace(msg.DeviceID),
		Result:   strings.TrimSpace(msg.Result),
		Online:   strings.TrimSpace(msg.Online),
		Status:   strings.TrimSpace(msg.Status),
		Reason:   msg.Reason,
	}
	if msg.DeviceTime != nil {
		data.DeviceTime = strings.TrimSpace(*msg.DeviceTime)
	}
	if msg.Encode != nil {
		data.Encode = strings.TrimSpace(*msg.Encode)
	}
	if msg.Record != nil {
		data.Record = strings.TrimSpace(*msg.Record)
	}
	data.Info = collectPlainDeviceStatusInfoValues(msg.Info)
	data.ExtraInfo = cloneDeviceStatusExtensionValues(msg.ExtraInfo)
	if msg.Alarm != nil {
		data.AlarmStatuses = make([]DeviceAlarmStatusData, 0, len(msg.Alarm.Items))
		for _, item := range msg.Alarm.Items {
			deviceID := ""
			if item.DeviceID != nil {
				deviceID = strings.TrimSpace(*item.DeviceID)
			}
			data.AlarmStatuses = append(data.AlarmStatuses, DeviceAlarmStatusData{
				DeviceID:   deviceID,
				DutyStatus: strings.TrimSpace(firstNonNilString(item.DutyStatus, item.StatusDutyStatus, item.Status)),
			})
		}
	}
	return data
}

func collectPlainDeviceStatusInfoValues(values []versionedInfoXML) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if len(value.Children) == 0 {
			out = append(out, value.Content)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneDeviceStatusExtensionValues(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func firstNonNilString(values ...*string) string {
	for _, value := range values {
		if value != nil {
			return *value
		}
	}
	return ""
}

type presetQueryXML struct {
	Result     string `xml:"Result"`
	SumNum     *int   `xml:"SumNum"`
	PresetList *struct {
		Num   *int `xml:"Num,attr"`
		Items []struct {
			PresetID   string  `xml:"PresetID"`
			PresetName *string `xml:"PresetName"`
		} `xml:"Item"`
	} `xml:"PresetList"`
}

func decodePresetQueryData(body []byte) []PresetItemData {
	var msg presetQueryXML
	if err := sip.XMLDecode(body, &msg); err != nil {
		return nil
	}
	if msg.PresetList == nil {
		return nil
	}
	out := make([]PresetItemData, 0, len(msg.PresetList.Items))
	for _, item := range msg.PresetList.Items {
		presetName := ""
		if item.PresetName != nil {
			presetName = *item.PresetName
		}
		out = append(out, PresetItemData{
			PresetID:   item.PresetID,
			PresetName: presetName,
		})
	}
	return out
}

type homePositionQueryXML struct {
	HomePosition *struct {
		Enabled     *int `xml:"Enabled"`
		ResetTime   *int `xml:"ResetTime"`
		PresetIndex *int `xml:"PresetIndex"`
	} `xml:"HomePosition"`
}

func decodeHomePositionData(body []byte) *HomePositionData {
	var msg homePositionQueryXML
	if err := sip.XMLDecode(body, &msg); err != nil || msg.HomePosition == nil {
		return nil
	}
	return &HomePositionData{
		Enabled:     msg.HomePosition.Enabled,
		ResetTime:   msg.HomePosition.ResetTime,
		PresetIndex: msg.HomePosition.PresetIndex,
	}
}

type cruiseTrackListQueryXML struct {
	SumNum          *int `xml:"SumNum"`
	CruiseTrackList *struct {
		Num   *int `xml:"Num,attr"`
		Items []struct {
			Number *int   `xml:"Number"`
			Name   string `xml:"Name"`
		} `xml:"CruiseTrack"`
	} `xml:"CruiseTrackList"`
}

func decodeCruiseTrackListData(body []byte) []CruiseTrackData {
	var msg cruiseTrackListQueryXML
	if err := sip.XMLDecode(body, &msg); err != nil {
		return nil
	}
	if msg.CruiseTrackList == nil {
		return []CruiseTrackData{}
	}
	out := make([]CruiseTrackData, 0, len(msg.CruiseTrackList.Items))
	for _, item := range msg.CruiseTrackList.Items {
		if item.Number == nil {
			return nil
		}
		out = append(out, CruiseTrackData{Number: *item.Number, Name: item.Name})
	}
	return out
}

type cruiseTrackQueryXML struct {
	Number          *int   `xml:"Number"`
	Name            string `xml:"Name"`
	SumNum          *int   `xml:"SumNum"`
	CruisePointList *struct {
		Num    *int `xml:"Num,attr"`
		Points []struct {
			PresetIndex *int `xml:"PresetIndex"`
			StayTime    *int `xml:"StayTime"`
			Speed       *int `xml:"Speed"`
		} `xml:"CruisePoint"`
	} `xml:"CruisePointList"`
}

func decodeCruiseTrackData(body []byte) *CruiseTrackData {
	var msg cruiseTrackQueryXML
	if err := sip.XMLDecode(body, &msg); err != nil {
		return nil
	}
	if msg.Number == nil {
		return nil
	}
	out := &CruiseTrackData{Number: *msg.Number, Name: msg.Name}
	if msg.CruisePointList == nil {
		return out
	}
	for _, point := range msg.CruisePointList.Points {
		if point.PresetIndex == nil || point.StayTime == nil || point.Speed == nil {
			return nil
		}
		out.Points = append(out.Points, CruisePointData{
			PresetIndex: *point.PresetIndex,
			StayTime:    *point.StayTime,
			Speed:       *point.Speed,
		})
	}
	return out
}

type ptzPositionQueryXML struct {
	Pan                  *float64 `xml:"Pan"`
	Tilt                 *float64 `xml:"Tilt"`
	Zoom                 *float64 `xml:"Zoom"`
	HorizontalFieldAngle *float64 `xml:"HorizontalFieldAngle"`
	VerticalFieldAngle   *float64 `xml:"VerticalFieldAngle"`
	MaxViewDistance      *float64 `xml:"MaxViewDistance"`
}

func decodePTZPositionData(body []byte) *PTZPositionData {
	var msg ptzPositionQueryXML
	if err := sip.XMLDecode(body, &msg); err != nil {
		return nil
	}
	if msg.Pan == nil && msg.Tilt == nil && msg.Zoom == nil && msg.HorizontalFieldAngle == nil && msg.VerticalFieldAngle == nil && msg.MaxViewDistance == nil {
		return nil
	}
	return &PTZPositionData{
		Pan:                  msg.Pan,
		Tilt:                 msg.Tilt,
		Zoom:                 msg.Zoom,
		HorizontalFieldAngle: msg.HorizontalFieldAngle,
		VerticalFieldAngle:   msg.VerticalFieldAngle,
		MaxViewDistance:      msg.MaxViewDistance,
	}
}

type sdCardStatusXML struct {
	SumNum           *int `xml:"SumNum"`
	SDCardStatusInfo *struct {
		Num   *int `xml:"Num,attr"`
		Items []struct {
			ID             *int    `xml:"ID"`
			HddName        *string `xml:"HddName"`
			Status         string  `xml:"Status"`
			FormatProgress *int    `xml:"FormatProgress"`
			Capacity       *int    `xml:"Capacity"`
			FreeSpace      *int    `xml:"FreeSpace"`
		} `xml:"Item"`
	} `xml:"SDCardStatusInfo"`
}

func decodeSDCardStatusData(body []byte) []SDCardItemData {
	var msg sdCardStatusXML
	if err := sip.XMLDecode(body, &msg); err != nil {
		return nil
	}
	if msg.SDCardStatusInfo == nil {
		return []SDCardItemData{}
	}
	out := make([]SDCardItemData, 0, len(msg.SDCardStatusInfo.Items))
	for _, item := range msg.SDCardStatusInfo.Items {
		if item.ID == nil {
			return nil
		}
		hddName := ""
		if item.HddName != nil {
			hddName = *item.HddName
		}
		out = append(out, SDCardItemData{
			ID:             *item.ID,
			HddName:        hddName,
			Status:         strings.TrimSpace(item.Status),
			FormatProgress: item.FormatProgress,
			Capacity:       item.Capacity,
			FreeSpace:      item.FreeSpace,
		})
	}
	return out
}

func validateGenericQueryPayload(version GBProtocolVersion, cmdType string, body []byte) error {
	switch cmdType {
	case "Alarm":
		if err := validateAlarmBusinessResponseStructure(body); err != nil {
			return err
		}
		var msg alarmBusinessResponse
		if err := sip.XMLDecode(body, &msg); err != nil {
			return ErrXMLDecode
		}
		if !isGBResultValue(strings.TrimSpace(msg.Result)) {
			return fmt.Errorf("Alarm Result must be OK or ERROR")
		}
	case "DeviceStatus":
		if err := validateDeviceStatusResponseStructure(body, version); err != nil {
			return err
		}
		var msg queryDeviceStatusXML
		if err := sip.XMLDecode(body, &msg); err != nil {
			return ErrXMLDecode
		}
		if msg.Encode != nil && !equalFoldAny(strings.TrimSpace(*msg.Encode), "ON", "OFF") {
			return fmt.Errorf("DeviceStatus Encode must be ON or OFF")
		}
		if msg.Record != nil && !equalFoldAny(strings.TrimSpace(*msg.Record), "ON", "OFF") {
			return fmt.Errorf("DeviceStatus Record must be ON or OFF")
		}
		if msg.DeviceTime != nil && !validGBDateTime(*msg.DeviceTime) {
			return fmt.Errorf("DeviceStatus DeviceTime must be dateTime")
		}
		if err := validateVersionedInfo(version, "DeviceStatus", msg.Info, msg.ExtraInfo); err != nil {
			return err
		}
		if err := validateDeviceAlarmStatus(version, msg.Alarm); err != nil {
			return err
		}
	case "PresetQuery":
		if err := validatePresetQueryResponseStructure(body, version); err != nil {
			return err
		}
		var msg presetQueryXML
		if err := sip.XMLDecode(body, &msg); err != nil {
			return ErrXMLDecode
		}
		if strings.TrimSpace(msg.Result) != "" && !isGBResultValue(msg.Result) {
			return fmt.Errorf("PresetQuery Result must be OK or ERROR")
		}
		if msg.PresetList == nil || msg.PresetList.Num == nil {
			return fmt.Errorf("PresetQuery requires PresetList Num")
		}
		if version.AtLeast(GBVersion30) && msg.SumNum == nil {
			return fmt.Errorf("PresetQuery requires SumNum")
		}
		if msg.SumNum != nil && (*msg.SumNum < 0 || *msg.SumNum != *msg.PresetList.Num) {
			return fmt.Errorf("PresetQuery count mismatch")
		}
		if *msg.PresetList.Num < 0 || *msg.PresetList.Num != len(msg.PresetList.Items) {
			return fmt.Errorf("PresetQuery count mismatch")
		}
		for _, item := range msg.PresetList.Items {
			// PresetID/PresetName 都是必选的普通 string；结构校验负责元素存在性，
			// 不能再附加 Schema 未定义的 minLength 或裁剪空白。
			if item.PresetName == nil {
				return fmt.Errorf("PresetQuery item requires PresetID and PresetName")
			}
		}
	case "HomePositionQuery":
		if err := validateHomePositionQueryResponseStructure(body); err != nil {
			return err
		}
		var msg homePositionQueryXML
		if err := sip.XMLDecode(body, &msg); err != nil {
			return ErrXMLDecode
		}
		if msg.HomePosition == nil {
			return nil
		}
		if msg.HomePosition.Enabled == nil || (*msg.HomePosition.Enabled != 0 && *msg.HomePosition.Enabled != 1) {
			return fmt.Errorf("HomePositionQuery Enabled must be 0 or 1")
		}
		// A.2.6.12 将 ResetTime 声明为 integer，未定义非负范围。
		if msg.HomePosition.PresetIndex != nil && (*msg.HomePosition.PresetIndex < 0 || *msg.HomePosition.PresetIndex > 255) {
			return fmt.Errorf("HomePositionQuery PresetIndex must be in [0,255]")
		}
	case "CruiseTrackListQuery":
		if err := validateCruiseTrackListQueryResponseStructure(body); err != nil {
			return err
		}
		var msg cruiseTrackListQueryXML
		if err := sip.XMLDecode(body, &msg); err != nil {
			return ErrXMLDecode
		}
		if msg.SumNum == nil || *msg.SumNum < 0 {
			return fmt.Errorf("CruiseTrackListQuery requires non-negative SumNum")
		}
		if msg.CruiseTrackList == nil {
			if *msg.SumNum != 0 {
				return fmt.Errorf("CruiseTrackListQuery count mismatch")
			}
			return nil
		}
		if msg.CruiseTrackList.Num == nil || *msg.CruiseTrackList.Num < 0 || *msg.SumNum != *msg.CruiseTrackList.Num || *msg.CruiseTrackList.Num != len(msg.CruiseTrackList.Items) {
			return fmt.Errorf("CruiseTrackListQuery count mismatch")
		}
		for _, item := range msg.CruiseTrackList.Items {
			if item.Number == nil || (*item.Number != 0 && *item.Number != 1) {
				return fmt.Errorf("CruiseTrackListQuery Number must be 0 or 1")
			}
			if len([]byte(item.Name)) > 32 {
				return fmt.Errorf("CruiseTrackListQuery Name exceeds 32 bytes")
			}
		}
	case "CruiseTrackQuery":
		if err := validateCruiseTrackQueryResponseStructure(body); err != nil {
			return err
		}
		var msg cruiseTrackQueryXML
		if err := sip.XMLDecode(body, &msg); err != nil {
			return ErrXMLDecode
		}
		if msg.Number == nil || (*msg.Number != 0 && *msg.Number != 1) {
			return fmt.Errorf("CruiseTrackQuery Number must be 0 or 1")
		}
		if len([]byte(msg.Name)) > 32 {
			return fmt.Errorf("CruiseTrackQuery Name exceeds 32 bytes")
		}
		if msg.SumNum == nil || *msg.SumNum < 0 {
			return fmt.Errorf("CruiseTrackQuery requires non-negative SumNum")
		}
		if msg.CruisePointList == nil {
			if *msg.SumNum != 0 {
				return fmt.Errorf("CruiseTrackQuery count mismatch")
			}
			return nil
		}
		if msg.CruisePointList.Num == nil || *msg.CruisePointList.Num < 0 || *msg.SumNum != *msg.CruisePointList.Num || *msg.CruisePointList.Num != len(msg.CruisePointList.Points) {
			return fmt.Errorf("CruiseTrackQuery count mismatch")
		}
		for _, point := range msg.CruisePointList.Points {
			if point.PresetIndex == nil || point.StayTime == nil || point.Speed == nil {
				return fmt.Errorf("CruiseTrackQuery point requires PresetIndex, StayTime and Speed")
			}
			// A.2.6.14 仅约束云台速度为 1~15；PresetIndex 和 StayTime
			// 在查询应答中只声明为 integer，不能沿用控制指令的字节范围。
			if *point.Speed < 1 || *point.Speed > 15 {
				return fmt.Errorf("CruiseTrackQuery Speed must be in [1,15]")
			}
		}
	case "PTZPosition":
		if err := validatePTZPositionQueryStructure(body); err != nil {
			return err
		}
		var msg ptzPositionQueryXML
		if err := sip.XMLDecode(body, &msg); err != nil {
			return ErrXMLDecode
		}
		values := []*float64{msg.Pan, msg.Tilt, msg.Zoom, msg.HorizontalFieldAngle, msg.VerticalFieldAngle, msg.MaxViewDistance}
		for _, value := range values {
			if value == nil {
				continue
			}
			if math.IsNaN(*value) || math.IsInf(*value, 0) {
				return fmt.Errorf("PTZPosition values must be finite")
			}
		}
	case "SDCardStatus":
		if err := validateSDCardStatusQueryResponseStructure(body); err != nil {
			return err
		}
		var msg sdCardStatusXML
		if err := sip.XMLDecode(body, &msg); err != nil {
			return ErrXMLDecode
		}
		if msg.SumNum == nil || *msg.SumNum < 0 {
			return fmt.Errorf("SDCardStatus requires non-negative SumNum")
		}
		if msg.SDCardStatusInfo == nil {
			if *msg.SumNum != 0 {
				return fmt.Errorf("SDCardStatus count mismatch")
			}
			return nil
		}
		items := msg.SDCardStatusInfo.Items
		if msg.SDCardStatusInfo.Num == nil || *msg.SDCardStatusInfo.Num < 0 || *msg.SumNum != *msg.SDCardStatusInfo.Num || *msg.SDCardStatusInfo.Num != len(items) || len(items) > 8 {
			return fmt.Errorf("SDCardStatus count mismatch")
		}
		for _, item := range items {
			status := strings.ToLower(strings.TrimSpace(item.Status))
			if item.ID == nil || item.HddName == nil || !equalFoldAny(status, "ok", "formatting", "unformatted", "idle", "error") || item.Capacity == nil || item.FreeSpace == nil {
				return fmt.Errorf("SDCardStatus item requires valid ID, HddName, Status, Capacity and FreeSpace")
			}
			if item.FormatProgress != nil && (*item.FormatProgress < 0 || *item.FormatProgress > 100) {
				return fmt.Errorf("SDCardStatus FormatProgress must be in [0,100]")
			}
			// A.2.6.16 只规定 Capacity/FreeSpace 为 integer，未定义数值范围
			// 或二者的大小关系；这里不附加 Schema 之外的约束。
		}
	}
	return nil
}

func containsAppendixA4Object(nodes []a4XMLNode) bool {
	for _, node := range nodes {
		if isAppendixA4Type(node.XMLName.Local) || containsAppendixA4Object(node.Children) {
			return true
		}
	}
	return false
}

func validateVersionedInfo(version GBProtocolVersion, cmdType string, info []versionedInfoXML, extraInfo []string) error {
	if version == GBVersion30 {
		for _, value := range info {
			if len(value.Children) == 0 || strings.TrimSpace(value.Content) != "" || !containsAppendixA4Object(value.Children) {
				return fmt.Errorf("%s plain or unknown Info is not supported by protocol 3.0", cmdType)
			}
		}
	} else {
		if len(extraInfo) > 0 {
			return fmt.Errorf("%s ExtraInfo requires protocol 3.0", cmdType)
		}
		for _, value := range info {
			if len(value.Children) > 0 {
				return fmt.Errorf("%s structured Info requires protocol 3.0", cmdType)
			}
			if utf8.RuneCountInString(value.Content) > 1024 {
				return fmt.Errorf("%s Info exceeds 1024 characters", cmdType)
			}
		}
	}
	for _, value := range extraInfo {
		if utf8.RuneCountInString(value) > 1024 {
			return fmt.Errorf("%s ExtraInfo exceeds 1024 characters", cmdType)
		}
	}
	return nil
}

func validateDeviceAlarmStatus(version GBProtocolVersion, alarm *deviceAlarmStatusXML) error {
	if alarm == nil {
		return nil
	}
	if alarm.Num != nil && alarm.LowerNum != nil {
		return fmt.Errorf("DeviceStatus Alarmstatus has duplicate Num")
	}
	var num *int
	if version == GBVersion20 {
		if alarm.Num != nil {
			return fmt.Errorf("DeviceStatus Alarmstatus count field does not match protocol")
		}
		num = alarm.LowerNum
	} else {
		if alarm.LowerNum != nil {
			return fmt.Errorf("DeviceStatus Alarmstatus count field does not match protocol")
		}
		num = alarm.Num
	}
	if num == nil || *num < 0 || *num != len(alarm.Items) {
		return fmt.Errorf("DeviceStatus Alarmstatus count mismatch")
	}
	for _, item := range alarm.Items {
		if item.DeviceID != nil && !isGBDeviceIdentifier(strings.TrimSpace(*item.DeviceID)) {
			return fmt.Errorf("DeviceStatus Alarmstatus has invalid DeviceID")
		}
		var status *string
		switch version {
		case GBVersion10:
			// 2011 的 A.2.6 Schema 使用 Status，但 9.5.3.3.2 正文和 J.11 示例使用 DutyStatus。
			// 两种标准内写法均兼容，但同一条目不能重复携带。
			if item.StatusDutyStatus != nil || (item.Status != nil && item.DutyStatus != nil) {
				return fmt.Errorf("DeviceStatus Alarmstatus field does not match protocol")
			}
			status = item.DutyStatus
			if status == nil {
				status = item.Status
			}
		case GBVersion11:
			// 2014 修改补充文件将 2011 的状态字段修订为 StatusDutyStatus。
			// 部分存量设备仍沿用 DutyStatus，作为兼容写法接受，但不允许
			// 与修订字段或已删除的 Status 同时出现。
			if item.Status != nil || (item.StatusDutyStatus != nil && item.DutyStatus != nil) {
				return fmt.Errorf("DeviceStatus Alarmstatus field does not match protocol")
			}
			status = item.StatusDutyStatus
			if status == nil {
				status = item.DutyStatus
			}
		default:
			status = item.DutyStatus
			if item.Status != nil || item.StatusDutyStatus != nil {
				return fmt.Errorf("DeviceStatus Alarmstatus field does not match protocol")
			}
		}
		if status != nil && !equalFoldAny(strings.TrimSpace(*status), "ONDUTY", "OFFDUTY", "ALARM") {
			return fmt.Errorf("DeviceStatus Alarmstatus has invalid duty status")
		}
	}
	return nil
}

type mobilePositionXML struct {
	Time      string   `xml:"Time"`
	Longitude *float64 `xml:"Longitude"`
	Latitude  *float64 `xml:"Latitude"`
	Speed     *float64 `xml:"Speed"`
	Direction *float64 `xml:"Direction"`
	Altitude  *float64 `xml:"Altitude"`
}

func decodeMobilePositionData(body []byte) *MobilePositionData {
	var msg mobilePositionXML
	if err := sip.XMLDecode(body, &msg); err != nil {
		return nil
	}
	if strings.TrimSpace(msg.Time) == "" && msg.Longitude == nil && msg.Latitude == nil {
		return nil
	}
	return &MobilePositionData{
		Time:      strings.TrimSpace(msg.Time),
		Longitude: msg.Longitude,
		Latitude:  msg.Latitude,
		Speed:     msg.Speed,
		Direction: msg.Direction,
		Altitude:  msg.Altitude,
	}
}

func decodeVideoUploadData(body []byte) *VideoUploadData {
	var msg struct {
		Time      string   `xml:"Time"`
		Longitude *float64 `xml:"Longitude"`
		Latitude  *float64 `xml:"Latitude"`
	}
	if err := sip.XMLDecode(body, &msg); err != nil || strings.TrimSpace(msg.Time) == "" {
		return nil
	}
	return &VideoUploadData{Time: strings.TrimSpace(msg.Time), Longitude: msg.Longitude, Latitude: msg.Latitude}
}

func (g *GB28181API) sipMessageVideoUploadNotify(ctx *sip.Context) {
	if !requireMessageNotification(ctx, "VideoUploadNotify") {
		return
	}
	if err := g.requireGBVersionAtLeast(ctx.DeviceID, gbVersion2022, "设备实时视音频回传通知(A.2.5.8)"); err != nil {
		ctx.String(400, err.Error())
		return
	}
	var msg videoUploadNotifyXML
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	if err := validateVideoUploadNotifyStructure(ctx.Request.Body()); err != nil {
		ctx.String(400, err.Error())
		return
	}
	msg.CmdType = strings.TrimSpace(msg.CmdType)
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	msg.Time = strings.TrimSpace(msg.Time)
	if msg.XMLName.Local != "Notify" || msg.SN <= 0 || !strings.EqualFold(msg.CmdType, "VideoUploadNotify") || !isGBDeviceIdentifier(msg.DeviceID) {
		ctx.String(400, "invalid VideoUploadNotify notification")
		return
	}
	if _, hasTime, err := parseSubscriptionTime(msg.Time); err != nil || !hasTime {
		ctx.String(400, "invalid VideoUploadNotify time")
		return
	}
	if msg.Longitude != nil && !validFiniteRange(*msg.Longitude, -180, 180) ||
		msg.Latitude != nil && !validFiniteRange(*msg.Latitude, -90, 90) {
		ctx.String(400, "invalid VideoUploadNotify location")
		return
	}
	if err := g.validateAuthenticatedResponseTarget(ctx, msg.DeviceID); err != nil {
		ctx.String(400, err.Error())
		return
	}
	stateDeviceID := firstNonEmpty(msg.DeviceID, strings.TrimSpace(ctx.DeviceID))
	decoded := g.decodeDeviceQueryResult(stateDeviceID, msg.CmdType, ctx.Request.Body())
	binding, hasBinding := admittedInboundRegistrationBinding(ctx)
	body := append([]byte(nil), ctx.Request.Body()...)
	_, queued, err := g.persistVideoUploadOutbox(g.serviceContext(), ctx.DeviceID, body, binding, hasBinding)
	if err != nil {
		ctx.Log.Error("persist VideoUploadNotify outbox", "err", err, "sn", msg.SN, "target_id", msg.DeviceID)
		ctx.String(503, "VideoUploadNotify delivery is unavailable")
		return
	}
	if err := ctx.RespondString(200, "OK"); err != nil {
		ctx.Log.Error("respond VideoUploadNotify", "err", err, "sn", msg.SN, "target_id", msg.DeviceID)
		return
	}
	var unlockCommit func()
	if hasBinding {
		unlockCommit, err = g.lockInboundDeviceStateCommit(ctx.DeviceID, binding)
	} else {
		unlockCommit, err = g.lockInboundDeviceStateCommit(ctx.DeviceID)
	}
	if err != nil {
		return
	}
	g.commitDecodedQueryStateForOwnerLocked(ctx.DeviceID, stateDeviceID, msg.CmdType, decoded)
	g.persistDecodedQuery(ctx.DeviceID, msg.CmdType, decoded)
	unlockCommit()
	if queued {
		g.signalVideoUploadOutboxWorker()
		return
	}
	deviceID := ctx.DeviceID
	g.startLifecycleTask(context.Background(), func(taskCtx context.Context) {
		var unlockForward func()
		var err error
		if hasBinding {
			unlockForward, err = g.lockInboundDeviceStateCommit(deviceID, binding)
		} else {
			unlockForward, err = g.lockInboundDeviceStateCommit(deviceID)
		}
		if err != nil {
			return
		}
		defer unlockForward()
		if err := g.forwardCascadeVideoUploadNotify(taskCtx, deviceID, body); err != nil {
			slog.Warn("forward cascade VideoUploadNotify failed", "device_id", deviceID, "err", err)
		}
	})
}

func decodeConfigDownloadState(body []byte) *ConfigDownloadState {
	var msg ConfigDownloadResponse
	if err := sip.XMLDecode(body, &msg); err != nil {
		return nil
	}
	snapshot := msg.SnapShot
	if snapshot == nil {
		snapshot = msg.SnapShotConfig
	}
	return &ConfigDownloadState{
		CmdType:             strings.TrimSpace(msg.CmdType),
		SN:                  msg.SN,
		DeviceID:            strings.TrimSpace(msg.DeviceID),
		Result:              strings.TrimSpace(msg.Result),
		BasicParam:          msg.BasicParam,
		VideoParamOpt:       msg.VideoParamOpt,
		VideoParamConfig:    msg.VideoParamConfig,
		AudioParamOpt:       msg.AudioParamOpt,
		AudioParamConfig:    msg.AudioParamConfig,
		SVACEncodeConfig:    msg.SVACEncodeConfig,
		SVACDecodeConfig:    msg.SVACDecodeConfig,
		VideoParamAttribute: msg.VideoParamAttribute,
		VideoRecordPlan:     msg.VideoRecordPlan,
		VideoAlarmRecord:    msg.VideoAlarmRecord,
		PictureMask:         msg.PictureMask,
		FrameMirror:         msg.FrameMirror,
		AlarmReport:         msg.AlarmReport,
		OSDConfig:           msg.OSDConfig,
		SnapShot:            snapshot,
		RawXML:              string(body),
	}
}
