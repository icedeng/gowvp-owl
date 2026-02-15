package gbs

import (
	"context"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

// messageAlarm 对应 GB28181 报警消息体（MESSAGE/NOTIFY Alarm）。
type messageAlarm struct {
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
		AlarmType   string `xml:"AlarmType"`
		AlarmMethod string `xml:"AlarmMethod"`
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
		if t, err := time.ParseInLocation(layout, value, time.Local); err == nil {
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

	deviceID := strings.TrimSpace(ctx.DeviceID)
	channelID := strings.TrimSpace(msg.DeviceID)
	if channelID == "" {
		// 部分设备可能不上报通道ID，回退为设备ID。
		channelID = deviceID
	}

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
		AlarmDescription: strings.TrimSpace(msg.AlarmDescription),
		AlarmTime:        strings.TrimSpace(msg.AlarmTime),
		Longitude:        strings.TrimSpace(msg.Longitude),
		Latitude:         strings.TrimSpace(msg.Latitude),
		SourceMethod:     sourceMethod,
	}

	// 抽取附录 A.4 扩展对象并做结构化落库。
	if ext := g.decodeAppendixA4Objects(msg.CmdType, ctx.Request.Body()); len(ext) > 0 {
		g.storeAppendixA4State(deviceID, ext)
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

	ctx.String(200, "OK")
}
