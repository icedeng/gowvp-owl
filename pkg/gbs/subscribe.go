package gbs

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

type SubscribeInput struct {
	DeviceID string
	TargetID string // 为空时订阅设备本身；级联可指定设备下的共享通道
	Event    string // Alarm/Catalog/MobilePosition/PTZPosition
	Expires  int
	Cancel   bool // true 时发送 Expires: 0 取消订阅

	// Alarm 订阅复用 A.2.4 Alarm 查询过滤参数。
	StartAlarmPriority string
	EndAlarmPriority   string
	AlarmMethod        string
	AlarmType          string
	StartAlarmTime     string
	EndAlarmTime       string
	// MobilePosition 位置上报间隔（秒），0 表示使用设备默认值。
	Interval int
}

// Subscribe 事件订阅（9.11），通过 SUBSCRIBE 发送订阅请求。
func (g *GB28181API) Subscribe(ctx context.Context, in *SubscribeInput) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if in == nil || strings.TrimSpace(in.DeviceID) == "" {
		return ErrDeviceNotExist
	}
	deviceID := strings.TrimSpace(in.DeviceID)
	targetID := strings.TrimSpace(in.TargetID)
	if targetID == "" {
		targetID = deviceID
	}
	ipc, ok := g.svr.memoryStorer.Load(deviceID)
	if !ok || !ipc.IsOnlineNow() {
		return ErrDeviceOffline
	}
	var target Targeter = ipc
	if targetID != deviceID {
		channel, exists := g.svr.memoryStorer.GetChannel(deviceID, targetID)
		if !exists {
			return ErrChannelNotExist
		}
		target = channel
	}
	expires := in.Expires
	if expires < 0 {
		return fmt.Errorf("expires must not be negative")
	}
	if in.Cancel {
		expires = 0
	} else if expires == 0 {
		expires = 3600
	}
	cmdType, ok := normalizeSubscribeCmdType(in.Event)
	if strings.TrimSpace(in.Event) == "" {
		cmdType, ok = "Alarm", true
	}
	if !ok {
		return fmt.Errorf("unsupported subscribe event: %s", in.Event)
	}
	if !in.Cancel {
		switch cmdType {
		case "Catalog":
			if err := g.requireGBFeature(deviceID, "directory_notify", "目录订阅", func(c GBCapabilities) bool {
				return c.DirectoryNotify
			}); err != nil {
				return err
			}
		case "MobilePosition":
			if err := g.requireGBFeature(deviceID, "mobile_position", "移动位置订阅", func(c GBCapabilities) bool {
				return c.MobilePosition
			}); err != nil {
				return err
			}
		case "PTZPosition":
			if err := g.requireGBFeature(deviceID, "ptz_position", "PTZ精准位置变化订阅", func(c GBCapabilities) bool {
				return c.PTZPosition
			}); err != nil {
				return err
			}
		}
	}
	version := g.getDeviceGBProtocolVersion(deviceID)
	var interval *int
	if in.Interval > 0 {
		value := in.Interval
		interval = &value
	}
	requestBody := subscribeEventRequest{
		CmdType:            cmdType,
		SN:                 sip.RandInt(100000, 999999),
		DeviceID:           targetID,
		StartAlarmPriority: strings.TrimSpace(in.StartAlarmPriority),
		EndAlarmPriority:   strings.TrimSpace(in.EndAlarmPriority),
		AlarmMethod:        strings.TrimSpace(in.AlarmMethod),
		AlarmType:          strings.TrimSpace(in.AlarmType),
		StartAlarmTime:     strings.TrimSpace(in.StartAlarmTime),
		EndAlarmTime:       strings.TrimSpace(in.EndAlarmTime),
		Interval:           interval,
	}
	if !in.Cancel {
		if err := validateSubscribeEventRequest(requestBody, cmdType, version); err != nil {
			return err
		}
	}
	body, err := sip.XMLEncode(requestBody)
	if err != nil {
		return err
	}
	key := buildOutgoingSubscriptionKey(deviceID, targetID, cmdType, in)
	loaded, _ := g.outgoingSubscriptions.LoadOrStore(key, &outgoingSubscriptionDialog{})
	dialog := loaded.(*outgoingSubscriptionDialog)
	dialog.mu.Lock()
	defer dialog.mu.Unlock()
	dialog.deviceID = deviceID
	dialog.targetID = targetID
	if !dialog.expiresAt.IsZero() && time.Now().After(dialog.expiresAt) {
		dialog.response = nil
		dialog.eventValue = ""
		dialog.clearPendingNotifyDialog()
	}
	if in.Cancel && dialog.response == nil {
		g.outgoingSubscriptions.CompareAndDelete(key, dialog)
		return fmt.Errorf("subscription does not exist: %s", cmdType)
	}
	if dialog.eventValue == "" {
		dialog.eventValue = buildSubscriptionEventValueForVersion(version, cmdType, targetID)
	}
	previousNotify := dialog.snapshotNotifyDialog()
	var dialogRequest *sip.Request
	if dialog.response != nil {
		dialogRequest, err = sip.NewRequestFromResponseChecked(sip.MethodSubscribe, dialog.response)
		if err != nil {
			dialog.restoreNotifyDialog(previousNotify)
			return err
		}
	}

	tx, err := g.svr.wrapRequestContext(ctx, target, sip.MethodSubscribe, &sip.ContentTypeXML, body, func(r *sip.Request) {
		if dialogRequest != nil {
			applyOutgoingSubscriptionDialog(r, dialogRequest)
		}
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: dialog.eventValue})
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: fmt.Sprintf("%d", expires)})
		if !in.Cancel {
			dialog.setPendingNotifyDialog(r, cmdType, deviceID, targetID, expires)
		}
	})
	if err != nil {
		dialog.restoreNotifyDialog(previousNotify)
		if dialog.response == nil {
			g.outgoingSubscriptions.CompareAndDelete(key, dialog)
		}
		return err
	}
	response, err := sipResponseContext(ctx, tx)
	if err != nil {
		dialog.restoreNotifyDialog(previousNotify)
		if dialog.response == nil {
			g.outgoingSubscriptions.CompareAndDelete(key, dialog)
		}
		return err
	}
	if in.Cancel {
		g.outgoingSubscriptions.CompareAndDelete(key, dialog)
		return nil
	}
	if current, loaded := g.outgoingSubscriptions.Load(key); !loaded || current != dialog {
		return fmt.Errorf("subscription dialog ended before final response")
	}
	dialog.response = response
	dialog.expiresAt = time.Now().Add(time.Duration(expires) * time.Second)
	if err := dialog.confirmNotifyDialog(response, expires); err != nil {
		g.outgoingSubscriptions.CompareAndDelete(key, dialog)
		return err
	}
	return nil
}

func buildOutgoingSubscriptionKey(deviceID, targetID, cmdType string, in *SubscribeInput) string {
	values := []string{
		strings.TrimSpace(deviceID), strings.TrimSpace(targetID), strings.ToUpper(strings.TrimSpace(cmdType)),
	}
	if in != nil {
		values = append(values,
			strings.TrimSpace(in.StartAlarmPriority), strings.TrimSpace(in.EndAlarmPriority),
			strings.TrimSpace(in.AlarmMethod), strings.TrimSpace(in.AlarmType),
			strings.TrimSpace(in.StartAlarmTime), strings.TrimSpace(in.EndAlarmTime),
			fmt.Sprintf("%d", in.Interval),
		)
	}
	return strings.Join(values, "|")
}

func applyOutgoingSubscriptionDialog(request, dialogRequest *sip.Request) {
	if request == nil || dialogRequest == nil || dialogRequest.Recipient() == nil {
		return
	}
	for _, name := range []string{"Via", "Route", "From", "To", "Call-ID", "CSeq"} {
		request.RemoveHeader(name)
	}
	for _, name := range []string{"Via", "Route", "From", "To", "Call-ID", "CSeq"} {
		sip.CopyHeaders(name, dialogRequest, request)
	}
	request.SetRecipient(dialogRequest.Recipient().Clone())
	if dialogRequest.Destination() != nil {
		request.SetDestination(dialogRequest.Destination())
	}
}

func applyServerSubscriptionDialog(request, dialogRequest *sip.Request) {
	if request == nil || dialogRequest == nil || dialogRequest.Recipient() == nil {
		return
	}
	for _, name := range []string{"Route", "From", "To", "Call-ID", "CSeq"} {
		request.RemoveHeader(name)
		sip.CopyHeaders(name, dialogRequest, request)
	}
	request.SetRecipient(dialogRequest.Recipient().Clone())
	if dialogRequest.Destination() != nil {
		request.SetDestination(dialogRequest.Destination())
	}
}

// sipNotifyCatalog 处理目录变更通知；应答后通过完整 Catalog 查询收敛本地快照。
func (g *GB28181API) sipNotifyCatalog(ctx *sip.Context) {
	var msg MessageDeviceListResponse
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	msg.CmdType = strings.TrimSpace(msg.CmdType)
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	if err := g.validateCatalogEnvelope(ctx, msg, true); err != nil {
		ctx.String(400, "invalid catalog notify")
		return
	}
	ctx.String(200, "OK")
	g.publishEventNotify("Catalog", ctx.DeviceID, ctx.Request.Body())
	if g.svr == nil || g.svr.memoryStorer == nil {
		return
	}
	deviceID := strings.TrimSpace(ctx.DeviceID)
	g.startLifecycleTask(context.Background(), func(taskCtx context.Context) {
		if err := g.QueryCatalogContext(taskCtx, deviceID); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("refresh Catalog after NOTIFY failed", "device_id", deviceID, "err", err)
		}
	})
}

// sipNotifyMobilePosition 处理位置订阅通知，结构化保存并转发给本级订阅方。
func (g *GB28181API) sipNotifyMobilePosition(ctx *sip.Context) {
	var msg mobilePositionNotify
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	deviceID := strings.TrimSpace(ctx.DeviceID)
	msg.CmdType = strings.TrimSpace(msg.CmdType)
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	position, positions, err := g.validateMobilePositionNotify(ctx, &msg)
	if err != nil {
		ctx.String(400, err.Error())
		return
	}
	extended := g.decodeAppendixA4Objects("MobilePosition", ctx.Request.Body())
	g.storeMobilePositionState(deviceID, position, positions)
	if len(extended) > 0 {
		g.storeAppendixA4State(deviceID, extended)
	}
	ctx.String(200, "OK")
	if len(extended) > 0 {
		g.persistAppendixA4Objects(deviceID, extended)
	}
	// 9.11 事件源侧：移动位置事件通知订阅方。
	g.publishEventNotify("MobilePosition", deviceID, ctx.Request.Body())
}

type mobilePositionNotify struct {
	XMLName    xml.Name
	CmdType    string   `xml:"CmdType"`
	SN         int      `xml:"SN"`
	DeviceID   string   `xml:"DeviceID"`
	Time       string   `xml:"Time"`
	SumNum     *int     `xml:"SumNum"`
	Longitude  *float64 `xml:"Longitude"`
	Latitude   *float64 `xml:"Latitude"`
	Speed      *float64 `xml:"Speed"`
	Direction  *float64 `xml:"Direction"`
	Altitude   *float64 `xml:"Altitude"`
	DeviceList struct {
		XMLName xml.Name
		Num     *int                    `xml:"Num,attr"`
		Item    []mobilePositionItemXML `xml:"Item"`
	} `xml:"DeviceList"`
}

type mobilePositionItemXML struct {
	DeviceID    string   `xml:"DeviceID"`
	CaptureTime string   `xml:"CaptureTime"`
	Longitude   *float64 `xml:"Longitude"`
	Latitude    *float64 `xml:"Latitude"`
	Speed       *float64 `xml:"Speed"`
	Direction   *float64 `xml:"Direction"`
	Altitude    *float64 `xml:"Altitude"`
	Height      *float64 `xml:"Height"`
}

func (g *GB28181API) validateMobilePositionNotify(ctx *sip.Context, msg *mobilePositionNotify) (*MobilePositionData, []MobilePositionData, error) {
	if msg == nil || msg.XMLName.Local != "Notify" || !strings.EqualFold(msg.CmdType, "MobilePosition") || msg.SN <= 0 {
		return nil, nil, fmt.Errorf("invalid MobilePosition envelope")
	}
	if !isGBDeviceIdentifier(msg.DeviceID) {
		return nil, nil, fmt.Errorf("invalid MobilePosition device id")
	}
	if err := g.validateAuthenticatedResponseTarget(ctx, msg.DeviceID); err != nil {
		return nil, nil, err
	}
	if !validGBDateTime(msg.Time) {
		return nil, nil, fmt.Errorf("invalid MobilePosition time")
	}
	version := g.getDeviceGBProtocolVersion(ctx.DeviceID)
	if !version.AtLeast(GBVersion20) {
		return nil, nil, fmt.Errorf("MobilePosition requires GB/T 28181-2016 or later")
	}
	hasBatch := msg.SumNum != nil || msg.DeviceList.XMLName.Local != ""
	if hasBatch {
		if !version.AtLeast(GBVersion30) {
			return nil, nil, fmt.Errorf("batch MobilePosition requires GB/T 28181-2022")
		}
		return g.validateBatchMobilePosition(ctx, msg)
	}
	position := &MobilePositionData{
		DeviceID: msg.DeviceID, Time: strings.TrimSpace(msg.Time), Longitude: msg.Longitude, Latitude: msg.Latitude,
		Speed: msg.Speed, Direction: msg.Direction, Altitude: msg.Altitude,
	}
	if err := validateMobilePositionData(position); err != nil {
		return nil, nil, err
	}
	return position, nil, nil
}

func (g *GB28181API) validateBatchMobilePosition(ctx *sip.Context, msg *mobilePositionNotify) (*MobilePositionData, []MobilePositionData, error) {
	if msg.SumNum == nil || *msg.SumNum < 0 {
		return nil, nil, fmt.Errorf("invalid MobilePosition SumNum")
	}
	if msg.DeviceList.XMLName.Local == "" {
		if *msg.SumNum == 0 {
			return nil, []MobilePositionData{}, nil
		}
		return nil, nil, fmt.Errorf("missing MobilePosition DeviceList")
	}
	if msg.DeviceList.Num == nil || *msg.DeviceList.Num < 0 || *msg.DeviceList.Num != len(msg.DeviceList.Item) || len(msg.DeviceList.Item) != *msg.SumNum {
		return nil, nil, fmt.Errorf("invalid MobilePosition DeviceList count")
	}
	positions := make([]MobilePositionData, 0, len(msg.DeviceList.Item))
	for _, item := range msg.DeviceList.Item {
		position := MobilePositionData{
			DeviceID: strings.TrimSpace(item.DeviceID), Time: strings.TrimSpace(item.CaptureTime), CaptureTime: strings.TrimSpace(item.CaptureTime),
			Longitude: item.Longitude, Latitude: item.Latitude, Speed: item.Speed, Direction: item.Direction, Altitude: item.Altitude, Height: item.Height,
		}
		if err := g.validateAuthenticatedResponseTarget(ctx, position.DeviceID); err != nil {
			return nil, nil, err
		}
		if !validGBDateTime(position.CaptureTime) {
			return nil, nil, fmt.Errorf("invalid MobilePosition capture time")
		}
		if err := validateMobilePositionData(&position); err != nil {
			return nil, nil, err
		}
		positions = append(positions, position)
	}
	if len(positions) == 0 {
		return nil, positions, nil
	}
	latest := positions[len(positions)-1]
	return &latest, positions, nil
}

func validateMobilePositionData(position *MobilePositionData) error {
	if position == nil || !isGBDeviceIdentifier(position.DeviceID) || position.Longitude == nil || position.Latitude == nil ||
		!validFiniteRange(*position.Longitude, -180, 180) || !validFiniteRange(*position.Latitude, -90, 90) {
		return fmt.Errorf("invalid MobilePosition coordinates")
	}
	for _, value := range []*float64{position.Speed, position.Altitude, position.Height} {
		if value != nil && !validFinite(*value) {
			return fmt.Errorf("invalid MobilePosition value")
		}
	}
	if position.Direction != nil && (!validFinite(*position.Direction) || *position.Direction < 0 || *position.Direction >= 360) {
		return fmt.Errorf("invalid MobilePosition direction")
	}
	return nil
}

func validGBDateTime(value string) bool {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04:05Z07:00", time.RFC3339} {
		if _, err := sip.ParseGBTime(layout, value); err == nil {
			return true
		}
	}
	return false
}

func (g *GB28181API) storeMobilePositionState(deviceID string, position *MobilePositionData, positions []MobilePositionData) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return
	}
	g.queryStateMu.Lock()
	defer g.queryStateMu.Unlock()
	state := &QueryState{}
	if value, ok := g.queryStates.Load(deviceID); ok {
		if previous, ok := value.(*QueryState); ok && previous != nil {
			*state = *previous
		}
	}
	state.UpdatedAt = time.Now()
	state.MobilePosition = position
	state.MobilePositions = cloneMobilePositions(positions)
	g.queryStates.Store(deviceID, state)
}
