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
	CmdType        string   `json:"cmd_type"`
	SN             int      `json:"sn"`
	DeviceID       string   `json:"device_id"`
	Result         string   `json:"result,omitempty"`
	Online         string   `json:"online,omitempty"`
	Status         string   `json:"status,omitempty"`
	DeviceTime     string   `json:"device_time,omitempty"`
	Encode         string   `json:"encode,omitempty"`
	Record         string   `json:"record,omitempty"`
	FaultDeviceIDs []string `json:"fault_device_ids,omitempty"`
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
	default:
		return nil
	}
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
	data       any
	appendixA4 []AppendixA4Object
}

func (g *GB28181API) decodeAndStoreQueryResult(deviceID, cmdType string, body []byte) decodedDeviceQuery {
	cmd := strings.TrimSpace(cmdType)
	deviceID = strings.TrimSpace(deviceID)
	if cmd == "" || len(body) == 0 || deviceID == "" {
		return decodedDeviceQuery{}
	}
	result := decodedDeviceQuery{appendixA4: g.decodeAppendixA4Objects(cmd, body)}
	if len(result.appendixA4) > 0 {
		g.storeAppendixA4State(deviceID, result.appendixA4)
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
	if result.data != nil {
		g.storeQueryState(deviceID, cmd, result.data)
	}
	return result
}

func (g *GB28181API) persistDecodedQuery(deviceID, cmdType string, result decodedDeviceQuery) {
	if cmdType == "DeviceStatus" {
		if status, ok := result.data.(*DeviceStatusData); ok {
			if strings.EqualFold(status.Result, "OK") && status.DeviceID == strings.TrimSpace(deviceID) {
				g.applyDeviceStatus(deviceID, status)
			}
		}
	}
	if len(result.appendixA4) > 0 {
		g.persistAppendixA4Objects(deviceID, result.appendixA4)
	}
}

func (g *GB28181API) storeQueryState(deviceID, cmdType string, data any) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || data == nil {
		return
	}
	g.queryStateMu.Lock()
	defer g.queryStateMu.Unlock()
	state := &QueryState{}
	if v, ok := g.queryStates.Load(deviceID); ok {
		if old, ok := v.(*QueryState); ok && old != nil {
			*state = *old
		}
	}
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
	g.queryStates.Store(deviceID, state)
}

func (g *GB28181API) storeDeviceConfigState(deviceID string, state *DeviceConfigState) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || state == nil {
		return
	}
	g.queryStateMu.Lock()
	defer g.queryStateMu.Unlock()
	curr := &QueryState{}
	if v, ok := g.queryStates.Load(deviceID); ok {
		if old, ok := v.(*QueryState); ok && old != nil {
			*curr = *old
		}
	}
	curr.UpdatedAt = time.Now()
	curr.DeviceConfig = state
	g.queryStates.Store(deviceID, curr)
}

func (g *GB28181API) storeAppendixA4State(deviceID string, objs []AppendixA4Object) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || len(objs) == 0 {
		return
	}
	g.queryStateMu.Lock()
	defer g.queryStateMu.Unlock()
	state := &QueryState{}
	if v, ok := g.queryStates.Load(deviceID); ok {
		if old, ok := v.(*QueryState); ok && old != nil {
			*state = *old
		}
	}
	state.UpdatedAt = time.Now()
	state.AppendixA4 = mergeAppendixA4Objects(state.AppendixA4, objs, 128)
	g.queryStates.Store(deviceID, state)
}

// persistAppendixA4Objects 将附录 A.4 结构化结果持久化到设备 ext 字段。
func (g *GB28181API) persistAppendixA4Objects(deviceID string, objs []AppendixA4Object) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || len(objs) == 0 {
		return
	}
	var dev ipc.Device
	if err := g.core.Store().Device().Update(context.TODO(), &dev, func(d *ipc.Device) error {
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
	cache := make(map[string]AppendixA4Object, len(base)+len(inc))
	for _, item := range base {
		cache[appendixA4ObjectKey(item)] = item
	}
	for _, item := range inc {
		key := appendixA4ObjectKey(item)
		if old, ok := cache[key]; ok {
			// 取更新的记录，避免旧值覆盖新值。
			if item.UpdatedAt < old.UpdatedAt {
				item.UpdatedAt = old.UpdatedAt
			}
		}
		cache[key] = item
	}
	out := make([]AppendixA4Object, 0, len(cache))
	for _, item := range cache {
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].UpdatedAt == out[j].UpdatedAt {
			return out[i].Type < out[j].Type
		}
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

func (g *GB28181API) applyDeviceStatus(deviceID string, in *DeviceStatusData) {
	if in == nil {
		return
	}
	online := strings.EqualFold(strings.TrimSpace(in.Online), "ONLINE") ||
		strings.EqualFold(strings.TrimSpace(in.Status), "OK") ||
		strings.EqualFold(strings.TrimSpace(in.Status), "ON")
	_ = g.svr.memoryStorer.Change(deviceID, func(d *ipc.Device) error {
		d.IsOnline = online
		d.KeepaliveAt = orm.Now()
		return nil
	}, func(d *Device) {
		d.IsOnline = online
		d.LastKeepaliveAt = time.Now()
	})
}

type queryDeviceStatusXML struct {
	CmdType    string `xml:"CmdType"`
	SN         int    `xml:"SN"`
	DeviceID   string `xml:"DeviceID"`
	Result     string `xml:"Result"`
	Online     string `xml:"Online"`
	Status     string `xml:"Status"`
	DeviceTime string `xml:"DeviceTime"`
	Encode     string `xml:"Encode"`
	Record     string `xml:"Record"`
}

func decodeDeviceStatusData(body []byte) *DeviceStatusData {
	var msg queryDeviceStatusXML
	if err := sip.XMLDecode(body, &msg); err != nil {
		return nil
	}
	return &DeviceStatusData{
		CmdType:    strings.TrimSpace(msg.CmdType),
		SN:         msg.SN,
		DeviceID:   strings.TrimSpace(msg.DeviceID),
		Result:     strings.TrimSpace(msg.Result),
		Online:     strings.TrimSpace(msg.Online),
		Status:     strings.TrimSpace(msg.Status),
		DeviceTime: strings.TrimSpace(msg.DeviceTime),
		Encode:     strings.TrimSpace(msg.Encode),
		Record:     strings.TrimSpace(msg.Record),
	}
}

type presetQueryXML struct {
	SumNum     *int `xml:"SumNum"`
	PresetList *struct {
		Num   *int `xml:"Num,attr"`
		Items []struct {
			PresetID   string `xml:"PresetID"`
			PresetName string `xml:"PresetName"`
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
		out = append(out, PresetItemData{
			PresetID:   strings.TrimSpace(item.PresetID),
			PresetName: strings.TrimSpace(item.PresetName),
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
		out = append(out, CruiseTrackData{Number: *item.Number, Name: strings.TrimSpace(item.Name)})
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
	out := &CruiseTrackData{Number: *msg.Number, Name: strings.TrimSpace(msg.Name)}
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
			ID             *int   `xml:"ID"`
			HddName        string `xml:"HddName"`
			Status         string `xml:"Status"`
			FormatProgress *int   `xml:"FormatProgress"`
			Capacity       *int   `xml:"Capacity"`
			FreeSpace      *int   `xml:"FreeSpace"`
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
		out = append(out, SDCardItemData{
			ID:             *item.ID,
			HddName:        strings.TrimSpace(item.HddName),
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
	case "PresetQuery":
		var msg presetQueryXML
		if err := sip.XMLDecode(body, &msg); err != nil {
			return ErrXMLDecode
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
			if strings.TrimSpace(item.PresetID) == "" || strings.TrimSpace(item.PresetName) == "" {
				return fmt.Errorf("PresetQuery item requires PresetID and PresetName")
			}
		}
	case "HomePositionQuery":
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
		if msg.HomePosition.ResetTime != nil && *msg.HomePosition.ResetTime < 0 {
			return fmt.Errorf("HomePositionQuery ResetTime must not be negative")
		}
		if msg.HomePosition.PresetIndex != nil && (*msg.HomePosition.PresetIndex < 0 || *msg.HomePosition.PresetIndex > 255) {
			return fmt.Errorf("HomePositionQuery PresetIndex must be in [0,255]")
		}
	case "CruiseTrackListQuery":
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
			if *point.PresetIndex < 0 || *point.StayTime < 0 || *point.Speed < 1 || *point.Speed > 15 {
				return fmt.Errorf("CruiseTrackQuery point values are invalid")
			}
		}
	case "PTZPosition":
		var msg ptzPositionQueryXML
		if err := sip.XMLDecode(body, &msg); err != nil {
			return ErrXMLDecode
		}
		values := []*float64{msg.Pan, msg.Tilt, msg.Zoom, msg.HorizontalFieldAngle, msg.VerticalFieldAngle, msg.MaxViewDistance}
		provided := false
		for _, value := range values {
			if value == nil {
				continue
			}
			provided = true
			if math.IsNaN(*value) || math.IsInf(*value, 0) {
				return fmt.Errorf("PTZPosition values must be finite")
			}
		}
		if !provided {
			return fmt.Errorf("PTZPosition requires at least one position value")
		}
	case "SDCardStatus":
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
			if item.ID == nil || strings.TrimSpace(item.HddName) == "" || !equalFoldAny(status, "ok", "formatting", "unformatted", "idle", "error") || item.Capacity == nil || item.FreeSpace == nil {
				return fmt.Errorf("SDCardStatus item requires valid ID, HddName, Status, Capacity and FreeSpace")
			}
			if item.FormatProgress != nil && (*item.FormatProgress < 0 || *item.FormatProgress > 100) {
				return fmt.Errorf("SDCardStatus FormatProgress must be in [0,100]")
			}
			if *item.Capacity < 0 || *item.FreeSpace < 0 || *item.FreeSpace > *item.Capacity {
				return fmt.Errorf("SDCardStatus capacity values are invalid")
			}
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
	if err := g.requireGBVersionAtLeast(ctx.DeviceID, gbVersion2022, "设备实时视音频回传通知(A.2.5.8)"); err != nil {
		ctx.String(400, err.Error())
		return
	}
	var msg videoUploadNotifyXML
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
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
	if msg.Longitude != nil && !validFinite(*msg.Longitude) || msg.Latitude != nil && !validFinite(*msg.Latitude) {
		ctx.String(400, "invalid VideoUploadNotify location")
		return
	}
	if err := g.validateAuthenticatedResponseTarget(ctx, msg.DeviceID); err != nil {
		ctx.String(400, err.Error())
		return
	}
	decoded := g.decodeAndStoreQueryResult(ctx.DeviceID, msg.CmdType, ctx.Request.Body())
	ctx.String(200, "OK")
	g.persistDecodedQuery(ctx.DeviceID, msg.CmdType, decoded)
	g.publishEventNotify(msg.CmdType, ctx.DeviceID, ctx.Request.Body())
	body := append([]byte(nil), ctx.Request.Body()...)
	deviceID := ctx.DeviceID
	g.startLifecycleTask(context.Background(), func(taskCtx context.Context) {
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
	snapshot := msg.SnapShotConfig
	if snapshot == nil {
		snapshot = msg.SnapShot
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
