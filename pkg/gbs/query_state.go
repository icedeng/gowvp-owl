package gbs

import (
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
	UpdatedAt      time.Time            `json:"updated_at"`
	DeviceStatus   *DeviceStatusData    `json:"device_status,omitempty"`
	Presets        []PresetItemData     `json:"presets,omitempty"`
	HomePosition   *HomePositionData    `json:"home_position,omitempty"`
	PTZPosition    *PTZPositionData     `json:"ptz_position,omitempty"`
	SDCards        []SDCardItemData     `json:"sd_cards,omitempty"`
	MobilePosition *MobilePositionData  `json:"mobile_position,omitempty"`
	ConfigDownload *ConfigDownloadState `json:"config_download,omitempty"`
	DeviceConfig   *DeviceConfigState   `json:"device_config,omitempty"`
}

// DeviceStatusData 对应 DeviceStatus 查询结果。
type DeviceStatusData struct {
	CmdType    string `json:"cmd_type"`
	SN         int    `json:"sn"`
	DeviceID   string `json:"device_id"`
	Result     string `json:"result,omitempty"`
	Online     string `json:"online,omitempty"`
	Status     string `json:"status,omitempty"`
	DeviceTime string `json:"device_time,omitempty"`
	Encode     string `json:"encode,omitempty"`
	Record     string `json:"record,omitempty"`
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
	Time      string   `json:"time,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
	Latitude  *float64 `json:"latitude,omitempty"`
	Speed     *float64 `json:"speed,omitempty"`
	Direction *float64 `json:"direction,omitempty"`
	Altitude  *float64 `json:"altitude,omitempty"`
}

// ConfigDownloadState 是配置查询结果快照。
type ConfigDownloadState struct {
	CmdType             string               `json:"cmd_type"`
	SN                  int                  `json:"sn"`
	DeviceID            string               `json:"device_id"`
	Result              string               `json:"result,omitempty"`
	BasicParam          *BasicParam          `json:"basic_param,omitempty"`
	VideoParamOpt       *VideoParamOpt       `json:"video_param_opt,omitempty"`
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
}

// DeviceConfigState 是设备配置应答快照。
type DeviceConfigState struct {
	CmdType  string    `json:"cmd_type"`
	SN       int       `json:"sn"`
	DeviceID string    `json:"device_id"`
	Result   string    `json:"result,omitempty"`
	SnapShot *SnapShot `json:"snapshot,omitempty"`
}

// GetQueryState 获取设备最新结构化状态。
func (g *GB28181API) GetQueryState(deviceID string) (*QueryState, bool) {
	v, ok := g.queryStates.Load(strings.TrimSpace(deviceID))
	if !ok {
		return nil, false
	}
	state, ok := v.(*QueryState)
	return state, ok
}

func (g *GB28181API) decodeAndStoreQueryData(deviceID, cmdType string, body []byte) any {
	cmd := strings.TrimSpace(cmdType)
	deviceID = strings.TrimSpace(deviceID)
	if cmd == "" || len(body) == 0 || deviceID == "" {
		return nil
	}
	var data any
	switch cmd {
	case "DeviceStatus":
		data = decodeDeviceStatusData(body)
	case "PresetQuery":
		data = decodePresetQueryData(body)
	case "HomePositionQuery":
		data = decodeHomePositionData(body)
	case "PTZPosition":
		data = decodePTZPositionData(body)
	case "SDCardStatus":
		data = decodeSDCardStatusData(body)
	case "MobilePosition":
		data = decodeMobilePositionData(body)
	case "ConfigDownload":
		data = decodeConfigDownloadState(body)
	default:
		return nil
	}
	if data == nil {
		return nil
	}
	g.storeQueryState(deviceID, cmd, data)
	return data
}

func (g *GB28181API) storeQueryState(deviceID, cmdType string, data any) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || data == nil {
		return
	}
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
			g.applyDeviceStatus(deviceID, s)
		}
	case "PresetQuery":
		if items, ok := data.([]PresetItemData); ok {
			state.Presets = items
		}
	case "HomePositionQuery":
		if s, ok := data.(*HomePositionData); ok {
			state.HomePosition = s
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
	case "ConfigDownload":
		if s, ok := data.(*ConfigDownloadState); ok {
			state.ConfigDownload = s
		}
	}
	g.queryStates.Store(deviceID, state)
}

func (g *GB28181API) storeDeviceConfigState(deviceID string, state *DeviceConfigState) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || state == nil {
		return
	}
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
	PresetList struct {
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
	Items []struct {
		ID             int    `xml:"ID"`
		HddName        string `xml:"HddName"`
		Status         string `xml:"Status"`
		FormatProgress *int   `xml:"FormatProgress"`
		Capacity       *int   `xml:"Capacity"`
		FreeSpace      *int   `xml:"FreeSpace"`
	} `xml:"SDCardStatusInfo>Item"`
}

func decodeSDCardStatusData(body []byte) []SDCardItemData {
	var msg sdCardStatusXML
	if err := sip.XMLDecode(body, &msg); err != nil {
		return nil
	}
	out := make([]SDCardItemData, 0, len(msg.Items))
	for _, item := range msg.Items {
		out = append(out, SDCardItemData{
			ID:             item.ID,
			HddName:        strings.TrimSpace(item.HddName),
			Status:         strings.TrimSpace(item.Status),
			FormatProgress: item.FormatProgress,
			Capacity:       item.Capacity,
			FreeSpace:      item.FreeSpace,
		})
	}
	return out
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

func decodeConfigDownloadState(body []byte) *ConfigDownloadState {
	var msg ConfigDownloadResponse
	if err := sip.XMLDecode(body, &msg); err != nil {
		return nil
	}
	return &ConfigDownloadState{
		CmdType:             strings.TrimSpace(msg.CmdType),
		SN:                  msg.SN,
		DeviceID:            strings.TrimSpace(msg.DeviceID),
		Result:              strings.TrimSpace(msg.Result),
		BasicParam:          msg.BasicParam,
		VideoParamOpt:       msg.VideoParamOpt,
		SVACEncodeConfig:    msg.SVACEncodeConfig,
		SVACDecodeConfig:    msg.SVACDecodeConfig,
		VideoParamAttribute: msg.VideoParamAttribute,
		VideoRecordPlan:     msg.VideoRecordPlan,
		VideoAlarmRecord:    msg.VideoAlarmRecord,
		PictureMask:         msg.PictureMask,
		FrameMirror:         msg.FrameMirror,
		AlarmReport:         msg.AlarmReport,
		OSDConfig:           msg.OSDConfig,
		SnapShot:            msg.SnapShot,
	}
}
