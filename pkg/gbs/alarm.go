package gbs

import (
	"context"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

// messageAlarm 对应 GB28181 报警消息体（MESSAGE/NOTIFY Alarm）。
type messageAlarm struct {
	XMLName          xml.Name
	CmdType          string `xml:"CmdType"`
	SN               int    `xml:"SN"`
	DeviceID         string `xml:"DeviceID"`
	AlarmPriority    string `xml:"AlarmPriority"`
	AlarmMethod      string `xml:"AlarmMethod"`
	AlarmTime        string `xml:"AlarmTime"`
	AlarmDescription string `xml:"AlarmDescription"`
	Longitude        string `xml:"Longitude"`
	Latitude         string `xml:"Latitude"`
	AlarmType        string `xml:"AlarmType"`
	Info             struct {
		AlarmType      string `xml:"AlarmType"`
		AlarmMethod    string `xml:"AlarmMethod"`
		AlarmTypeParam struct {
			EventType *int `xml:"EventType"`
		} `xml:"AlarmTypeParam"`
	} `xml:"Info"`
}

// AlarmEvent 是系统内部统一的报警事件模型。
// DeviceID 使用设备国标编码，ChannelID 使用通道国标编码（无通道时回退为设备编码）。
type AlarmEvent struct {
	CmdType          string `json:"cmd_type"`
	SN               int    `json:"sn"`
	DeviceID         string `json:"device_id"`
	ChannelID        string `json:"channel_id"`
	AlarmPriority    string `json:"alarm_priority"`
	AlarmMethod      string `json:"alarm_method"`
	AlarmType        string `json:"alarm_type"`
	EventType        *int   `json:"event_type,omitempty"`
	AlarmDescription string `json:"alarm_description"`
	AlarmTime        string `json:"alarm_time"`
	Longitude        string `json:"longitude"`
	Latitude         string `json:"latitude"`
	SourceMethod     string `json:"source_method"`
}

// ParseTime 将报警时间字符串解析为 time.Time，兼容常见时间格式。
func (e *AlarmEvent) ParseTime() (time.Time, bool) {
	layouts := []string{
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05Z07:00",
		time.RFC3339,
		time.DateTime,
	}
	value := strings.TrimSpace(e.AlarmTime)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range layouts {
		if t, err := sip.ParseGBTime(layout, value); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// SetAlarmHandler 注册报警事件回调，用于将协议层消息桥接到业务层。
func (g *GB28181API) SetAlarmHandler(fn func(context.Context, *AlarmEvent)) {
	g.alarmHandlerMu.Lock()
	g.alarmHandler = fn
	g.alarmHandlerMu.Unlock()
}

// sipMessageAlarm 处理 MESSAGE Alarm。
func (g *GB28181API) sipMessageAlarm(ctx *sip.Context) {
	g.handleAlarm(ctx, sip.MethodMessage)
}

// sipNotifyAlarm 处理 NOTIFY Alarm。
func (g *GB28181API) sipNotifyAlarm(ctx *sip.Context) {
	g.handleAlarm(ctx, sip.MethodNotify)
}

// handleAlarm 统一解析报警消息，并触发回调与对外通知。
func (g *GB28181API) handleAlarm(ctx *sip.Context, sourceMethod string) {
	var msg messageAlarm
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	msg.CmdType = strings.TrimSpace(msg.CmdType)
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	if err := g.validateAlarmEnvelope(ctx, &msg); err != nil {
		ctx.String(400, err.Error())
		return
	}

	deviceID := strings.TrimSpace(ctx.DeviceID)
	channelID := msg.DeviceID

	alarmType := strings.TrimSpace(msg.AlarmType)
	if alarmType == "" {
		// 兼容部分设备把报警类型放在 Info 节点。
		alarmType = strings.TrimSpace(msg.Info.AlarmType)
	}
	alarmMethod := strings.TrimSpace(msg.AlarmMethod)
	if alarmMethod == "" {
		// 兼容部分设备把报警方式放在 Info 节点。
		alarmMethod = strings.TrimSpace(msg.Info.AlarmMethod)
	}

	event := &AlarmEvent{
		CmdType:          msg.CmdType,
		SN:               msg.SN,
		DeviceID:         deviceID,
		ChannelID:        channelID,
		AlarmPriority:    strings.TrimSpace(msg.AlarmPriority),
		AlarmMethod:      alarmMethod,
		AlarmType:        alarmType,
		EventType:        msg.Info.AlarmTypeParam.EventType,
		AlarmDescription: strings.TrimSpace(msg.AlarmDescription),
		AlarmTime:        strings.TrimSpace(msg.AlarmTime),
		Longitude:        strings.TrimSpace(msg.Longitude),
		Latitude:         strings.TrimSpace(msg.Latitude),
		SourceMethod:     sourceMethod,
	}

	// 抽取附录 A.4 扩展对象并先更新内存快照。
	ext := g.decodeAppendixA4Objects(msg.CmdType, ctx.Request.Body())
	if len(ext) > 0 {
		g.storeAppendixA4State(deviceID, ext)
	}
	// 设备报警确认不应被数据库、业务回调或订阅方 NOTIFY 响应拖延。
	ctx.String(200, "OK")
	if len(ext) > 0 {
		g.persistAppendixA4Objects(deviceID, ext)
	}

	g.alarmHandlerMu.RLock()
	handler := g.alarmHandler
	g.alarmHandlerMu.RUnlock()
	if handler != nil {
		handler(context.Background(), event)
	}
	notify(notifyAlarm(event))
	// 9.11 事件源侧：报警发生后，向订阅方发送 NOTIFY。
	g.publishEventNotify("Alarm", deviceID, ctx.Request.Body())
}

func (g *GB28181API) validateAlarmEnvelope(ctx *sip.Context, msg *messageAlarm) error {
	if msg == nil || msg.XMLName.Local != "Notify" || !strings.EqualFold(msg.CmdType, "Alarm") || msg.SN <= 0 {
		return fmt.Errorf("invalid Alarm envelope")
	}
	targetID := strings.TrimSpace(msg.DeviceID)
	if len(targetID) == 10 {
		if !isNumericIdentifier(targetID) || ctx == nil || !strings.HasPrefix(strings.TrimSpace(ctx.DeviceID), targetID) {
			return fmt.Errorf("Alarm center id mismatch")
		}
	} else {
		if !isGBDeviceIdentifier(targetID) {
			return fmt.Errorf("invalid Alarm device id")
		}
		if err := g.validateAuthenticatedResponseTarget(ctx, targetID); err != nil {
			return err
		}
	}
	priority := strings.TrimSpace(msg.AlarmPriority)
	if len(priority) != 1 || priority[0] < '1' || priority[0] > '4' {
		return fmt.Errorf("invalid Alarm priority")
	}
	method := strings.TrimSpace(msg.AlarmMethod)
	infoMethod := strings.TrimSpace(msg.Info.AlarmMethod)
	if method != "" && infoMethod != "" && method != infoMethod {
		return fmt.Errorf("conflicting Alarm method")
	}
	if method == "" {
		method = infoMethod
	}
	if len(method) != 1 || method[0] < '1' || method[0] > '7' {
		return fmt.Errorf("invalid Alarm method")
	}
	alarmType := strings.TrimSpace(msg.AlarmType)
	infoAlarmType := strings.TrimSpace(msg.Info.AlarmType)
	if alarmType != "" && infoAlarmType != "" && alarmType != infoAlarmType {
		return fmt.Errorf("conflicting Alarm type")
	}
	if alarmType == "" {
		alarmType = infoAlarmType
	}
	version := g.getDeviceGBProtocolVersion(ctx.DeviceID)
	eventType := msg.Info.AlarmTypeParam.EventType
	if !version.AtLeast(GBVersion20) && (alarmType != "" || eventType != nil) {
		return fmt.Errorf("Alarm type extension requires %s or later", GBVersion20.StandardName())
	}
	if err := validateAlarmTypeForMethod(version, method, alarmType); err != nil {
		return err
	}
	if eventType != nil {
		if method != "5" || alarmType != "6" || (*eventType != 1 && *eventType != 2) {
			return fmt.Errorf("invalid Alarm EventType")
		}
	}
	if _, ok := (&AlarmEvent{AlarmTime: msg.AlarmTime}).ParseTime(); !ok {
		return fmt.Errorf("invalid Alarm time")
	}
	for field, bounds := range map[string]struct {
		value            string
		minimum, maximum float64
	}{
		"longitude": {value: msg.Longitude, minimum: -180, maximum: 180},
		"latitude":  {value: msg.Latitude, minimum: -90, maximum: 90},
	} {
		value := strings.TrimSpace(bounds.value)
		if value == "" {
			continue
		}
		coordinate, err := strconv.ParseFloat(value, 64)
		if err != nil || !validFiniteRange(coordinate, bounds.minimum, bounds.maximum) {
			return fmt.Errorf("invalid Alarm %s", field)
		}
	}
	return nil
}

func validateAlarmTypeForMethod(version GBProtocolVersion, method, alarmType string) error {
	alarmType = strings.TrimSpace(alarmType)
	if alarmType == "" || !version.AtLeast(GBVersion20) {
		return nil
	}
	maximum := 0
	switch strings.TrimSpace(method) {
	case "2":
		maximum = 5
	case "5":
		maximum = 12
		if version.AtLeast(GBVersion30) {
			maximum = 13
		}
	case "6":
		maximum = 2
	default:
		return fmt.Errorf("AlarmType requires AlarmMethod 2, 5, or 6")
	}
	value, err := strconv.Atoi(alarmType)
	if err != nil || value < 1 || value > maximum {
		return fmt.Errorf("invalid AlarmType for AlarmMethod %s", method)
	}
	return nil
}

func isNumericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}
