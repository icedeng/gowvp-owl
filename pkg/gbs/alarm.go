package gbs

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

// messageAlarm 对应 GB28181 报警消息体（MESSAGE/NOTIFY Alarm）。
type messageAlarm struct {
	XMLName          xml.Name
	CmdType          string         `xml:"CmdType"`
	SN               int            `xml:"SN"`
	DeviceID         string         `xml:"DeviceID"`
	AlarmPriority    string         `xml:"AlarmPriority"`
	AlarmMethod      string         `xml:"AlarmMethod"`
	AlarmTime        string         `xml:"AlarmTime"`
	AlarmDescription string         `xml:"AlarmDescription"`
	Longitude        string         `xml:"Longitude"`
	Latitude         string         `xml:"Latitude"`
	AlarmType        string         `xml:"AlarmType"`
	Info             []alarmInfoXML `xml:"Info"`
	ExtraInfo        []string       `xml:"ExtraInfo"`

	normalizedAlarmMethod string
	normalizedAlarmType   string
	normalizedEventType   *int
}

type alarmInfoXML struct {
	Content        string `xml:",chardata"`
	AlarmType      string `xml:"AlarmType"`
	AlarmMethod    string `xml:"AlarmMethod"`
	AlarmTypeParam struct {
		EventType *int `xml:"EventType"`
	} `xml:"AlarmTypeParam"`
	Children []a4XMLNode `xml:",any"`
}

type alarmBusinessResponse struct {
	XMLName  xml.Name `xml:"Response"`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	DeviceID string   `xml:"DeviceID"`
	Result   string   `xml:"Result"`
}

// AlarmEvent 是系统内部统一的报警事件模型。
// DeviceID 使用设备国标编码，ChannelID 使用通道国标编码（无通道时回退为设备编码）。
type AlarmEvent struct {
	DeliveryID       string `json:"delivery_id"`
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

// SetAlarmHandler 注册兼容的无错误报警回调。
func (g *GB28181API) SetAlarmHandler(fn func(context.Context, *AlarmEvent)) {
	if fn == nil {
		g.SetReliableAlarmHandler(nil)
		return
	}
	g.SetReliableAlarmHandler(func(ctx context.Context, event *AlarmEvent) error {
		fn(ctx, event)
		return nil
	})
}

// SetReliableAlarmHandler 注册可报告持久化失败的报警回调。
// 生产存储可用时，协议层会在 SIP 成功确认前写入持久收件箱，并在失败后重放。
func (g *GB28181API) SetReliableAlarmHandler(fn func(context.Context, *AlarmEvent) error) {
	g.alarmHandlerMu.Lock()
	g.alarmHandler = fn
	if fn != nil && g.alarmInboxWake == nil {
		g.alarmInboxWake = make(chan struct{}, 1)
	}
	g.alarmHandlerMu.Unlock()
	if fn != nil {
		g.startAlarmInboxWorker()
		g.signalAlarmInboxWorker()
	}
}

// sipMessageAlarm 处理 MESSAGE Alarm。
func (g *GB28181API) sipMessageAlarm(ctx *sip.Context) {
	var envelope struct {
		XMLName  xml.Name
		CmdType  string `xml:"CmdType"`
		SN       int    `xml:"SN"`
		DeviceID string `xml:"DeviceID"`
	}
	if err := sip.XMLDecode(ctx.Request.Body(), &envelope); err == nil && envelope.XMLName.Local == "Response" &&
		strings.EqualFold(strings.TrimSpace(envelope.CmdType), "Alarm") {
		if _, ok := g.pendingDeviceQueryExpectedTarget(ctx.DeviceID, "Alarm", envelope.SN); ok {
			// Alarm 查询响应与 9.4 报警业务应答使用相同的 Response/Alarm
			// 信封。只要 SN 命中当前查询，就必须交给查询处理器继续校验目标；
			// 不能因为 DeviceID 不匹配而降级为“无关联业务应答”并静默返回 200。
			g.sipMessageQueryGeneric(ctx)
			return
		}
		g.handleLocalAlarmBusinessResponse(ctx)
		return
	}
	g.handleAlarm(ctx, sip.MethodMessage)
}

// sipNotifyAlarm 处理 NOTIFY Alarm。
func (g *GB28181API) sipNotifyAlarm(ctx *sip.Context) {
	g.handleAlarm(ctx, sip.MethodNotify)
}

// handleAlarm 统一解析报警消息，并触发回调与对外通知。
func (g *GB28181API) handleAlarm(ctx *sip.Context, sourceMethod string) {
	version := g.getDeviceGBProtocolVersion(ctx.DeviceID)
	if err := validateAlarmStructure(ctx.Request.Body(), version); err != nil {
		ctx.String(400, err.Error())
		return
	}
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
	ext, err := g.validateAndDecodeAppendixA4(ctx.DeviceID, msg.CmdType, ctx.Request.Body())
	if err != nil {
		ctx.String(400, err.Error())
		return
	}
	deviceID := strings.TrimSpace(ctx.DeviceID)
	channelID := msg.DeviceID

	event := &AlarmEvent{
		CmdType:          msg.CmdType,
		SN:               msg.SN,
		DeviceID:         deviceID,
		ChannelID:        channelID,
		AlarmPriority:    strings.TrimSpace(msg.AlarmPriority),
		AlarmMethod:      msg.normalizedAlarmMethod,
		AlarmType:        msg.normalizedAlarmType,
		EventType:        msg.normalizedEventType,
		AlarmDescription: msg.AlarmDescription,
		AlarmTime:        strings.TrimSpace(msg.AlarmTime),
		Longitude:        strings.TrimSpace(msg.Longitude),
		Latitude:         strings.TrimSpace(msg.Latitude),
		SourceMethod:     sourceMethod,
	}
	event.DeliveryID = alarmDeliveryID(deviceID, sourceMethod, ctx.Request.Body())

	binding, hasBinding := admittedInboundRegistrationBinding(ctx)
	unlockDeviceCommit, err := g.lockAdmittedInboundDeviceStateCommit(ctx)
	if err != nil {
		if errors.Is(err, errInboundDeviceGenerationChanged) {
			// 已确认的旧注册代次请求只做幂等应答，不能写入新代次任务或发送业务消息。
			ctx.String(200, "OK")
			return
		}
		ctx.String(403, err.Error())
		return
	}
	defer unlockDeviceCommit()

	handler := g.alarmHandlerSnapshot()
	inboxPersisted, err := g.persistAlarmInbox(g.serviceContext(), event)
	if err != nil {
		ctx.Log.Error("persist Alarm inbox", "err", err, "sn", msg.SN, "target_id", msg.DeviceID)
		ctx.String(500, "alarm persistence unavailable")
		return
	}

	// 持久收件箱提交后立即确认；事件中心写入、业务回调和订阅方 NOTIFY 不拖延设备响应。
	if sourceMethod == sip.MethodNotify {
		if err := respondEventNotifyOK(ctx, version, msg.CmdType, msg.SN, msg.DeviceID); err != nil {
			ctx.Log.Error("respond Alarm NOTIFY", "err", err, "sn", msg.SN, "target_id", msg.DeviceID)
			return
		}
		if !g.commitOutgoingSubscriptionNotifyAfterResponse(ctx) {
			return
		}
	} else {
		if err := ctx.RespondString(200, "OK"); err != nil {
			ctx.Log.Error("respond Alarm", "err", err, "sn", msg.SN, "target_id", msg.DeviceID)
			return
		}
	}
	if sourceMethod == sip.MethodMessage {
		// 源设备业务应答逐次发送，避免首次应答丢包后设备持续重报。
		g.sendAlarmBusinessResponse(ctx, msg)
	}

	business := func() error {
		// 抽取附录 A.4 扩展对象并在成功确认后更新内存快照。
		if len(ext) > 0 {
			g.storeAppendixA4StateForOwnerLocked(ctx.DeviceID, channelID, ext)
		}
		if sourceMethod == sip.MethodMessage {
			// 9.4 目标侧分发独立于业务回调和 9.11 订阅链路，仅由源设备 MESSAGE 触发。
			if hasBinding {
				g.dispatchAlarmToLocalTargets(deviceID, ctx.Request.Body(), binding)
				g.dispatchAlarmToCascadeTargets(deviceID, ctx.Request.Body(), binding)
			} else {
				g.dispatchAlarmToLocalTargets(deviceID, ctx.Request.Body())
				g.dispatchAlarmToCascadeTargets(deviceID, ctx.Request.Body())
			}
		}
		if len(ext) > 0 {
			g.persistAppendixA4Objects(deviceID, ext)
		}
		notify(notifyAlarm(event))
		// 9.11 事件源侧：报警发生后，向订阅方发送 NOTIFY。
		g.publishEventNotify("Alarm", deviceID, ctx.Request.Body())
		return nil
	}
	businessComplete := false
	businessCommittedInRequest := false
	if inboxPersisted {
		ran, completed, commitErr := g.commitAlarmBusinessOnce(g.serviceContext(), event.DeviceID, event.DeliveryID, business)
		businessComplete = completed
		businessCommittedInRequest = ran && completed
		if commitErr != nil {
			ctx.Log.Error("commit Alarm business", "err", commitErr, "delivery_id", event.DeliveryID, "device_id", event.DeviceID)
			// SIP 已成功确认时优先保证事件不丢失；只有提交前失败才在持久门禁外补执行。
			if !ran {
				if fallbackErr := business(); fallbackErr != nil {
					ctx.Log.Error("fallback Alarm business", "err", fallbackErr, "delivery_id", event.DeliveryID, "device_id", event.DeviceID)
				} else {
					businessComplete = true
					businessCommittedInRequest = true
				}
			}
		}
	} else if err := business(); err != nil {
		ctx.Log.Error("process non-persistent Alarm business", "err", err, "delivery_id", event.DeliveryID, "device_id", event.DeviceID)
	} else {
		businessComplete = true
	}

	if !businessComplete {
		return
	}
	// DeliveryID 提交锁已经释放，回调投递可以安全复用同一同键锁；设备代次锁仍覆盖全部副作用。
	if inboxPersisted {
		if businessCommittedInRequest {
			g.processCommittedAlarmInboxDelivery(event.DeviceID, event.DeliveryID, time.Now())
		} else {
			g.processAlarmInboxDelivery(event.DeviceID, event.DeliveryID)
		}
	} else if handler != nil {
		if err := handler(g.serviceContext(), event); err != nil {
			slog.Warn("process non-persistent Alarm callback failed", "delivery_id", event.DeliveryID, "device_id", event.DeviceID, "err", err)
		}
	}
}

// sendAlarmBusinessResponse 按 9.4 在空正文 SIP 200 之后发送独立的 MESSAGE/Response 业务应答。
// 订阅产生的 NOTIFY Alarm 使用同一 SIP 200 携带旧版业务应答，不进入独立 MESSAGE 流程。
func (g *GB28181API) sendAlarmBusinessResponse(ctx *sip.Context, alarm messageAlarm) {
	if g == nil || ctx == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return
	}
	sourceID := strings.TrimSpace(ctx.DeviceID)
	target, ok := g.svr.memoryStorer.Load(sourceID)
	if !ok || target == nil {
		slog.Warn("send Alarm business response failed", "device_id", sourceID, "sn", alarm.SN, "err", ErrDeviceNotExist)
		return
	}
	body, err := sip.XMLEncode(alarmBusinessResponse{
		CmdType: "Alarm", SN: alarm.SN, DeviceID: strings.TrimSpace(alarm.DeviceID), Result: "OK",
	})
	if err != nil {
		slog.Warn("encode Alarm business response failed", "device_id", sourceID, "sn", alarm.SN, "err", err)
		return
	}
	identityCtx := monitorUserIdentityContext(ctx)
	tx, err := g.svr.wrapRequestContext(identityCtx, target, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		slog.Warn("send Alarm business response failed", "device_id", sourceID, "sn", alarm.SN, "err", err)
		return
	}
	if !g.startLifecycleTask(identityCtx, func(taskCtx context.Context) {
		responseCtx, cancel := context.WithTimeout(taskCtx, 6*time.Second)
		defer cancel()
		if _, err := sipResponseContext(responseCtx, tx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("wait Alarm business response SIP acknowledgement failed", "device_id", sourceID, "sn", alarm.SN, "err", err)
		}
	}) {
		tx.Close()
	}
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
	infoAlarmType, infoMethod, eventType, err := alarmTypedInfo(msg.Info)
	if err != nil {
		return err
	}
	if infoMethod != "" {
		return fmt.Errorf("Alarm Info.AlarmMethod is not standard")
	}
	method := strings.TrimSpace(msg.AlarmMethod)
	if len(method) != 1 || method[0] < '1' || method[0] > '7' {
		return fmt.Errorf("invalid Alarm method")
	}
	if strings.TrimSpace(msg.AlarmType) != "" {
		return fmt.Errorf("top-level AlarmType is not standard")
	}
	alarmType := infoAlarmType
	version := g.getDeviceGBProtocolVersion(ctx.DeviceID)
	if err := validateAlarmInfoVersion(version, msg.Info, msg.ExtraInfo); err != nil {
		return err
	}
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
	msg.normalizedAlarmMethod = method
	msg.normalizedAlarmType = alarmType
	msg.normalizedEventType = eventType
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

func alarmTypedInfo(values []alarmInfoXML) (alarmType, alarmMethod string, eventType *int, err error) {
	for _, value := range values {
		if current := strings.TrimSpace(value.AlarmType); current != "" {
			if alarmType != "" && alarmType != current {
				return "", "", nil, fmt.Errorf("conflicting Alarm Info type")
			}
			alarmType = current
		}
		if current := strings.TrimSpace(value.AlarmMethod); current != "" {
			if alarmMethod != "" && alarmMethod != current {
				return "", "", nil, fmt.Errorf("conflicting Alarm Info method")
			}
			alarmMethod = current
		}
		if current := value.AlarmTypeParam.EventType; current != nil {
			if eventType != nil && *eventType != *current {
				return "", "", nil, fmt.Errorf("conflicting Alarm Info event type")
			}
			copyValue := *current
			eventType = &copyValue
		}
	}
	return alarmType, alarmMethod, eventType, nil
}

func validateAlarmInfoVersion(version GBProtocolVersion, values []alarmInfoXML, extraInfo []string) error {
	typedCount := 0
	plainInfoSeen := false
	for _, value := range values {
		typed := strings.TrimSpace(value.AlarmType) != "" || strings.TrimSpace(value.AlarmMethod) != "" || value.AlarmTypeParam.EventType != nil
		if typed {
			typedCount++
		}
		if !version.AtLeast(GBVersion20) {
			if typed || len(value.Children) > 0 {
				return fmt.Errorf("Alarm structured Info requires %s or later", GBVersion20.StandardName())
			}
			if utf8.RuneCountInString(value.Content) > 1024 {
				return fmt.Errorf("Alarm Info exceeds 1024 characters")
			}
			continue
		}
		if version == GBVersion20 {
			if len(value.Children) > 0 {
				return fmt.Errorf("Alarm Appendix A.4 Info requires %s", GBVersion30.StandardName())
			}
			if typed && plainInfoSeen {
				return fmt.Errorf("Alarm structured Info must precede plain Info in protocol 2.0")
			}
			if !typed {
				plainInfoSeen = true
			}
			if !typed && utf8.RuneCountInString(value.Content) > 1024 {
				return fmt.Errorf("Alarm Info exceeds 1024 characters")
			}
			continue
		}
		if strings.TrimSpace(value.Content) != "" || (!typed && !containsAppendixA4Object(value.Children)) {
			return fmt.Errorf("Alarm plain or unknown Info is not supported by protocol 3.0")
		}
	}
	if typedCount > 1 {
		return fmt.Errorf("Alarm contains duplicate structured Info")
	}
	if version != GBVersion30 && len(extraInfo) > 0 {
		return fmt.Errorf("Alarm ExtraInfo requires protocol 3.0")
	}
	for _, value := range extraInfo {
		if utf8.RuneCountInString(value) > 1024 {
			return fmt.Errorf("Alarm ExtraInfo exceeds 1024 characters")
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
