package gbs

import (
	"context"
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
	if !dialog.expiresAt.IsZero() && time.Now().After(dialog.expiresAt) {
		dialog.response = nil
		dialog.remoteTarget = nil
		dialog.eventValue = ""
	}
	if in.Cancel && dialog.response == nil {
		g.outgoingSubscriptions.Delete(key)
		return fmt.Errorf("subscription does not exist: %s", cmdType)
	}
	if dialog.eventValue == "" {
		dialog.eventValue = buildSubscriptionEventValueForVersion(version, cmdType, targetID)
	}

	var request *sip.Request
	tx, err := g.svr.wrapRequestContext(ctx, target, sip.MethodSubscribe, &sip.ContentTypeXML, body, func(r *sip.Request) {
		request = r
		if dialog.response != nil {
			applyOutgoingSubscriptionDialog(r, dialog)
		}
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: dialog.eventValue})
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: fmt.Sprintf("%d", expires)})
	})
	if err != nil {
		if dialog.response == nil {
			g.outgoingSubscriptions.Delete(key)
		}
		return err
	}
	response, err := sipResponseContext(ctx, tx)
	if err != nil {
		if dialog.response == nil {
			g.outgoingSubscriptions.Delete(key)
		}
		return err
	}
	if in.Cancel {
		g.outgoingSubscriptions.Delete(key)
		return nil
	}
	dialog.response = response
	dialog.expiresAt = time.Now().Add(time.Duration(expires) * time.Second)
	if contact, ok := response.Contact(); ok && contact != nil && contact.Address != nil {
		dialog.remoteTarget = contact.Address.Clone()
	} else if request != nil && request.Recipient() != nil {
		dialog.remoteTarget = request.Recipient().Clone()
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

func applyOutgoingSubscriptionDialog(request *sip.Request, dialog *outgoingSubscriptionDialog) {
	if request == nil || dialog == nil || dialog.response == nil {
		return
	}
	for _, name := range []string{"From", "To", "Call-ID", "CSeq"} {
		request.RemoveHeader(name)
	}
	sip.CopyHeaders("From", dialog.response, request)
	sip.CopyHeaders("To", dialog.response, request)
	sip.CopyHeaders("Call-ID", dialog.response, request)
	if previous, ok := dialog.response.CSeq(); ok && previous != nil {
		request.AppendHeader(&sip.CSeq{SeqNo: previous.SeqNo + 1, MethodName: sip.MethodSubscribe})
	}
	if dialog.remoteTarget != nil {
		request.SetRecipient(dialog.remoteTarget.Clone())
	}
}

// sipNotifyCatalog 处理目录变更通知；应答后通过完整 Catalog 查询收敛本地快照。
func (g *GB28181API) sipNotifyCatalog(ctx *sip.Context) {
	var msg MessageDeviceListResponse
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	if !strings.EqualFold(strings.TrimSpace(msg.CmdType), "Catalog") {
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
	var msg struct {
		CmdType  string `xml:"CmdType"`
		DeviceID string `xml:"DeviceID"`
	}
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err == nil {
		deviceID := strings.TrimSpace(ctx.DeviceID)
		if deviceID == "" {
			deviceID = strings.TrimSpace(msg.DeviceID)
		}
		cmdType := strings.TrimSpace(msg.CmdType)
		if cmdType == "" {
			cmdType = "MobilePosition"
		}
		decoded := g.decodeAndStoreQueryResult(deviceID, cmdType, ctx.Request.Body())
		ctx.String(200, "OK")
		g.persistDecodedQuery(deviceID, cmdType, decoded)
		// 9.11 事件源侧：移动位置事件通知订阅方。
		g.publishEventNotify(cmdType, deviceID, ctx.Request.Body())
		return
	}
	ctx.String(200, "OK")
}
