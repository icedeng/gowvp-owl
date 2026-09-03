package gbs

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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
	// Catalog 订阅按设备目录查询定义使用的新增时间范围。
	StartTime string
	EndTime   string
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
	expires := in.Expires
	if expires < 0 {
		return fmt.Errorf("expires must not be negative")
	}
	cmdType, ok := normalizeSubscribeCmdType(in.Event)
	if strings.TrimSpace(in.Event) == "" {
		cmdType, ok = "Alarm", true
	}
	if !ok {
		return fmt.Errorf("unsupported subscribe event: %s", in.Event)
	}
	if in.Interval < 0 {
		return fmt.Errorf("subscription interval must not be negative")
	}
	key := buildOutgoingSubscriptionKey(deviceID, targetID, cmdType, in)
	key += monitorUserIdentitySubscriptionKey(ctx)
	manualSubscription := !isCascadeSubscribeContext(ctx)
	if manualSubscription && !manualSubscriptionOperationAlreadyHeld(ctx) {
		unlock, err := g.lockEventSubscriptionOperation(ctx, manualSubscriptionOperationKey(key))
		if err != nil {
			return err
		}
		defer unlock()
	}
	manualIntentManaged := manualSubscription && g.manualSubscriptionPersistenceAvailable()
	manualIntentDeleted := false
	if in.Cancel && manualIntentManaged {
		managed, existed, err := g.deleteManualSubscriptionIntent(ctx, key, deviceID)
		if err != nil {
			return err
		}
		manualIntentManaged = managed
		manualIntentDeleted = existed
	}
	ipc, ok := g.svr.memoryStorer.Load(deviceID)
	if !ok || !ipc.IsOnlineNow() {
		if manualIntentDeleted {
			g.forgetManualSubscriptionDialog(key)
			return nil
		}
		return ErrDeviceOffline
	}
	var target Targeter = ipc
	if targetID != deviceID {
		channel, exists := g.svr.memoryStorer.GetChannel(deviceID, targetID)
		if !exists {
			if manualIntentDeleted {
				g.forgetManualSubscriptionDialog(key)
				return nil
			}
			return ErrChannelNotExist
		}
		target = channel
	}
	version := g.getDeviceGBProtocolVersion(deviceID)
	if in.Cancel {
		expires = 0
	} else if expires == 0 {
		expires = defaultOutgoingSubscribeExpires(version, cmdType)
	}
	if err := validateOutgoingSubscribeExpires(expires); err != nil {
		return err
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
	method, err := formatAlarmMethodFilter(version, in.AlarmMethod)
	if err != nil {
		return fmt.Errorf("invalid AlarmMethod: %w", err)
	}
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
		AlarmMethod:        method,
		AlarmType:          strings.TrimSpace(in.AlarmType),
		StartAlarmTime:     strings.TrimSpace(in.StartAlarmTime),
		EndAlarmTime:       strings.TrimSpace(in.EndAlarmTime),
		StartTime:          strings.TrimSpace(in.StartTime),
		EndTime:            strings.TrimSpace(in.EndTime),
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
	loaded, _ := g.outgoingSubscriptions.LoadOrStore(key, &outgoingSubscriptionDialog{})
	dialog := loaded.(*outgoingSubscriptionDialog)
	dialog.mu.Lock()
	defer dialog.mu.Unlock()
	dialog.deviceID = deviceID
	dialog.targetID = targetID
	dialog.identity = monitorUserIdentityFromContext(ctx)
	dialog.localGatewayID, _ = ctx.Value(monitorUserIdentityGatewayContextKey{}).(string)
	dialog.localGatewayID = strings.TrimSpace(dialog.localGatewayID)
	if !dialog.expiresAt.IsZero() && subscriptionExpiredAt(time.Now(), dialog.expiresAt) ||
		dialog.cancelPending.Load() && !in.Cancel {
		dialog.response = nil
		dialog.requestBody = nil
		dialog.eventValue = ""
		dialog.expires = 0
		dialog.refreshAt = time.Time{}
		dialog.refreshing = false
		dialog.autoRefresh = false
		dialog.refreshInput = SubscribeInput{}
		dialog.cancelPending.Store(false)
		dialog.clearPendingNotifyDialog()
	}
	if in.Cancel && dialog.response == nil {
		g.compareAndDeleteOutgoingSubscription(key, dialog)
		if manualIntentDeleted {
			return nil
		}
		return fmt.Errorf("subscription does not exist: %s", cmdType)
	}
	if !in.Cancel {
		intentInput := normalizedManualSubscriptionInput(*in, deviceID, targetID, cmdType, expires)
		managed, persistErr := g.saveManualSubscriptionIntent(ctx, key, intentInput, dialog.identity, dialog.localGatewayID)
		manualIntentManaged = managed
		if persistErr != nil {
			if dialog.response == nil {
				g.compareAndDeleteOutgoingSubscription(key, dialog)
			}
			return persistErr
		}
	}
	if dialog.eventValue == "" {
		// 设备侧传统订阅沿用 presence；Catalog;id=num 仅用于 2014+ 域间目录订阅。
		dialog.eventValue = "presence"
	}
	previousNotify := dialog.snapshotNotifyDialog()
	dialogResponse := dialog.response
	previousCancelPending := dialog.cancelPending.Load()
	cancelDeadline := time.Time{}
	if in.Cancel {
		cancelDeadline = time.Now().Add(outgoingUnsubscribeNotifyWait)
		dialog.cancelPending.Store(true)
		dialog.notifyMu.Lock()
		dialog.notify.expiresAt = cancelDeadline
		dialog.notifyMu.Unlock()
	}
	restoreCancelState := func() {
		if in.Cancel {
			if manualIntentDeleted {
				dialog.autoRefresh = false
				dialog.cancelPending.Store(true)
				return
			}
			dialog.cancelPending.Store(previousCancelPending)
		}
	}

	operation, releaseOperation := g.trackPendingDeviceRequest(ctx, deviceID, targetID)
	defer releaseOperation()
	requestCtx := operation.Context(ctx)
	prepareSubscription := func(r *sip.Request) error {
		if err := applyOutgoingSubscriptionNotifyRoute(r, previousNotify.routeRequest); err != nil {
			return err
		}
		applyOutgoingSubscriptionPayload(g.svr, target, r, body, dialog.eventValue, expires)
		if !in.Cancel {
			dialog.setPendingNotifyDialog(r, cmdType, deviceID, targetID, expires)
		}
		return nil
	}
	var tx *sip.Transaction
	if dialogResponse != nil {
		tx, err = g.svr.requestFromResponsePreparedContext(requestCtx, target, sip.MethodSubscribe, dialogResponse, prepareSubscription)
	} else {
		tx, err = g.svr.wrapRequestContext(requestCtx, target, sip.MethodSubscribe, &sip.ContentTypeXML, body, func(r *sip.Request) {
			r.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: dialog.eventValue})
			r.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: fmt.Sprintf("%d", expires)})
			if !in.Cancel {
				dialog.setPendingNotifyDialog(r, cmdType, deviceID, targetID, expires)
			}
		})
	}
	if err != nil {
		dialog.restoreNotifyDialogLocked(previousNotify, time.Now())
		restoreCancelState()
		if dialog.response == nil {
			g.compareAndDeleteOutgoingSubscription(key, dialog)
		}
		return operation.ErrorOr(err)
	}
	response, err := sipResponseContextAccepted(requestCtx, tx, func(status int) bool {
		return status >= 200 && status < 300
	})
	if err != nil {
		dialog.restoreNotifyDialogLocked(previousNotify, time.Now())
		restoreCancelState()
		if dialog.response == nil || subscribeFailureTerminatesDialog(version, response) {
			g.compareAndDeleteOutgoingSubscription(key, dialog)
		}
		return operation.ErrorOr(err)
	}
	if !operation.Deliver(func() {}) {
		dialog.restoreNotifyDialogLocked(previousNotify, time.Now())
		restoreCancelState()
		if dialog.response == nil {
			g.compareAndDeleteOutgoingSubscription(key, dialog)
		}
		return operation.Cause()
	}
	if err := validateSubscribeBusinessResponse(response, requestBody, dialog.eventValue, version); err != nil {
		dialog.restoreNotifyDialogLocked(previousNotify, time.Now())
		restoreCancelState()
		if dialog.response == nil {
			g.compareAndDeleteOutgoingSubscription(key, dialog)
		}
		return err
	}
	if in.Cancel {
		if current, loaded := g.outgoingSubscriptions.Load(key); loaded && current == dialog {
			cancelDeadline = time.Now().Add(outgoingUnsubscribeNotifyWait)
			dialog.response = response
			dialog.expires = 0
			dialog.expiresAt = cancelDeadline
			dialog.refreshAt = time.Time{}
			dialog.refreshing = false
			dialog.autoRefresh = false
			dialog.refreshInput = SubscribeInput{}
			dialog.notifyMu.Lock()
			dialog.notify.expiresAt = cancelDeadline
			dialog.notifyMu.Unlock()
		}
		return nil
	}
	if current, loaded := g.outgoingSubscriptions.Load(key); !loaded || current != dialog {
		return fmt.Errorf("subscription dialog ended before final response")
	}
	negotiatedExpires, err := negotiatedSubscribeExpires(response, expires)
	if err != nil {
		dialog.restoreNotifyDialogLocked(previousNotify, time.Now())
		if dialog.response == nil {
			g.compareAndDeleteOutgoingSubscription(key, dialog)
		}
		return err
	}
	now := time.Now()
	dialog.response = response
	dialog.requestBody = append(dialog.requestBody[:0], body...)
	dialog.expires = negotiatedExpires
	dialog.expiresAt = now.Add(time.Duration(negotiatedExpires) * time.Second)
	dialog.refreshAt = outgoingSubscriptionRefreshAt(now, negotiatedExpires)
	dialog.refreshing = false
	dialog.autoRefresh = !isCascadeSubscribeContext(ctx)
	dialog.refreshInput = *in
	dialog.refreshInput.DeviceID = deviceID
	dialog.refreshInput.TargetID = targetID
	dialog.refreshInput.Event = outgoingSubscriptionEventName(cmdType)
	dialog.refreshInput.Expires = negotiatedExpires
	dialog.refreshInput.Cancel = false
	dialog.cancelPending.Store(false)
	if err := dialog.confirmNotifyDialog(response, negotiatedExpires); err != nil {
		g.compareAndDeleteOutgoingSubscription(key, dialog)
		return err
	}
	notifyExpiresAt := dialog.snapshotNotifyDialog().expiresAt
	if !notifyExpiresAt.IsZero() && notifyExpiresAt.Before(dialog.expiresAt) {
		dialog.expiresAt = notifyExpiresAt
		dialog.refreshAt = outgoingSubscriptionRefreshAtDeadline(now, notifyExpiresAt)
	}
	if manualIntentManaged {
		if err := g.confirmManualSubscriptionIntent(g.taskPersistenceContext(), key, deviceID, dialog.refreshAt); err != nil {
			slog.Warn("confirm manual subscription persistence failed", "device_id", deviceID, "target_id", targetID, "event", cmdType, "err", err)
		}
	}
	return nil
}

func subscribeFailureTerminatesDialog(version GBProtocolVersion, response *sip.Response) bool {
	if response == nil {
		return false
	}
	status := response.StatusCode()
	if !version.AtLeast(GBVersion30) {
		return status == 481
	}
	switch status {
	case 404, 405, 410, 416, 480, 481, 482, 483, 484, 485, 489, 501, 604:
		return true
	default:
		return false
	}
}

// negotiatedSubscribeExpires 读取事件源在成功响应中确认的实际有效期。
// RFC 3265 允许事件源缩短请求时长，但不得延长；部分老设备省略该头时继续沿用请求值。
func negotiatedSubscribeExpires(response *sip.Response, requested int) (int, error) {
	if response == nil || requested <= 0 {
		return 0, fmt.Errorf("invalid subscribe expiration")
	}
	headers := response.GetHeaders("Expires")
	if len(headers) == 0 {
		return requested, nil
	}
	if len(headers) != 1 || headers[0] == nil {
		return 0, fmt.Errorf("subscribe response must contain at most one Expires header")
	}
	value := strings.TrimSpace(headers[0].String())
	if _, after, ok := strings.Cut(value, ":"); ok {
		value = strings.TrimSpace(after)
	}
	expires, err := strconv.Atoi(value)
	if err != nil || expires <= 0 {
		return 0, fmt.Errorf("invalid subscribe response Expires header")
	}
	if expires > requested {
		return 0, fmt.Errorf("subscribe response Expires %d exceeds requested %d", expires, requested)
	}
	return expires, nil
}

func outgoingSubscriptionRefreshAt(now time.Time, expires int) time.Time {
	return outgoingSubscriptionRefreshAtDeadline(now, now.Add(time.Duration(expires)*time.Second))
}

func outgoingSubscriptionRefreshAtDeadline(now, expiresAt time.Time) time.Time {
	duration := expiresAt.Sub(now)
	if duration <= 0 {
		return time.Time{}
	}
	lead := duration / 10
	if duration < 10*time.Minute {
		lead = duration / 2
	}
	if lead < 5*time.Second {
		lead = 5 * time.Second
	}
	if lead >= duration {
		lead = duration / 2
	}
	if lead > 5*time.Minute {
		lead = 5 * time.Minute
	}
	return expiresAt.Add(-lead)
}

func defaultOutgoingSubscribeExpires(version GBProtocolVersion, cmdType string) int {
	if version.AtLeast(GBVersion11) && strings.EqualFold(strings.TrimSpace(cmdType), "Catalog") {
		return defaultCascadeCatalogSubscribeExpires
	}
	return defaultSubscribeExpires
}

func validateOutgoingSubscribeExpires(expires int) error {
	if expires < 0 {
		return fmt.Errorf("expires must not be negative")
	}
	// SIP Expires 在本项目中按 uint32 解析；出向请求保持同一范围，
	// 避免本地计时溢出，也避免对端回显后无法由本端解析。
	if uint64(expires) > uint64(^uint32(0)) {
		return fmt.Errorf("expires exceeds SIP delta-seconds range")
	}
	return nil
}

type subscribeBusinessResponse struct {
	XMLName  xml.Name `xml:"Response"`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	DeviceID string   `xml:"DeviceID"`
	Result   string   `xml:"Result"`
}

// validateSubscribeBusinessResponse 校验旧版订阅响应中携带的 MANSCDP 业务结果。
// 部分存量厂商只返回空 200，继续兼容；一旦携带业务应答，就不能把 ERROR 或错配响应当成订阅成功。
func validateSubscribeBusinessResponse(response *sip.Response, request subscribeEventRequest, eventValue string, version GBProtocolVersion) error {
	if response == nil || !shouldIncludeSubscribeBusinessResponse(version, request.CmdType, eventValue) {
		return nil
	}
	body := response.Body()
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := validateSIPContentType(response, string(sip.ContentTypeXML)); err != nil {
		return fmt.Errorf("invalid subscribe business response Content-Type: %w", err)
	}
	if err := validateMANSCDPStructure(body, "Response", strings.TrimSpace(request.CmdType), broadcastResponseRules); err != nil {
		return fmt.Errorf("invalid subscribe business response: %w", err)
	}
	var result subscribeBusinessResponse
	if err := sip.XMLDecode(body, &result); err != nil {
		return fmt.Errorf("invalid subscribe business response: %w", err)
	}
	if result.XMLName.Local != "Response" || strings.TrimSpace(result.CmdType) != strings.TrimSpace(request.CmdType) ||
		result.SN != request.SN || strings.TrimSpace(result.DeviceID) != strings.TrimSpace(request.DeviceID) {
		return fmt.Errorf("subscribe business response does not match request")
	}
	if strings.ToUpper(strings.TrimSpace(result.Result)) != "OK" {
		return fmt.Errorf("subscribe business response failed: %s", strings.TrimSpace(result.Result))
	}
	return nil
}

// validateEventNotifyBusinessResponse 校验 2011/2014/2016 Alarm 和传统 Catalog NOTIFY 的 200 OK 业务应答。
// 存量设备只返回空 200 时继续兼容；一旦携带正文，就必须与本次通知严格关联且业务结果为 OK。
func validateEventNotifyBusinessResponse(response *sip.Response, requestBody []byte, cmdType, eventValue string, version GBProtocolVersion) error {
	if response == nil || !shouldIncludeSubscribeBusinessResponse(version, cmdType, eventValue) {
		return nil
	}
	body := response.Body()
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := validateSIPContentType(response, string(sip.ContentTypeXML)); err != nil {
		return fmt.Errorf("invalid NOTIFY business response Content-Type: %w", err)
	}
	if err := validateMANSCDPStructure(body, "Response", strings.TrimSpace(cmdType), broadcastResponseRules); err != nil {
		return fmt.Errorf("invalid NOTIFY business response: %w", err)
	}
	var request struct {
		XMLName  xml.Name
		CmdType  string `xml:"CmdType"`
		SN       int    `xml:"SN"`
		DeviceID string `xml:"DeviceID"`
	}
	var result subscribeBusinessResponse
	if err := sip.XMLDecode(requestBody, &request); err != nil {
		return fmt.Errorf("invalid NOTIFY request body: %w", err)
	}
	if err := sip.XMLDecode(body, &result); err != nil {
		return fmt.Errorf("invalid NOTIFY business response: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(result.CmdType), strings.TrimSpace(request.CmdType)) ||
		result.SN != request.SN || strings.TrimSpace(result.DeviceID) != strings.TrimSpace(request.DeviceID) {
		return fmt.Errorf("NOTIFY business response does not match request")
	}
	if !strings.EqualFold(strings.TrimSpace(result.Result), "OK") {
		return fmt.Errorf("NOTIFY business response failed: %s", strings.TrimSpace(result.Result))
	}
	return nil
}

func buildOutgoingSubscriptionKey(deviceID, targetID, cmdType string, in *SubscribeInput) string {
	values := []string{
		strings.TrimSpace(deviceID), strings.TrimSpace(targetID), strings.ToUpper(strings.TrimSpace(cmdType)),
	}
	if in != nil {
		alarmMethod, err := normalizeAlarmMethodFilter(in.AlarmMethod)
		if err != nil {
			alarmMethod = strings.TrimSpace(in.AlarmMethod)
		}
		values = append(values,
			strings.TrimSpace(in.StartAlarmPriority), strings.TrimSpace(in.EndAlarmPriority),
			alarmMethod, strings.TrimSpace(in.AlarmType),
			strings.TrimSpace(in.StartAlarmTime), strings.TrimSpace(in.EndAlarmTime),
			strings.TrimSpace(in.StartTime), strings.TrimSpace(in.EndTime),
			fmt.Sprintf("%d", in.Interval),
		)
	}
	return strings.Join(values, "|")
}

// buildOutgoingSubscriptionRefreshRequest 保留 SUBSCRIBE 响应维护的本地 CSeq，
// 并按 RFC 6665 使用首个合法 NOTIFY 建立的远端目标与路由集。
func buildOutgoingSubscriptionRefreshRequest(response *sip.Response, notify *sip.Request) (*sip.Request, error) {
	if notify == nil {
		return sip.NewRequestFromResponseChecked(sip.MethodSubscribe, response)
	}
	return sip.NewRequestFromResponsePreparedChecked(sip.MethodSubscribe, response, func(request *sip.Request) error {
		return applyOutgoingSubscriptionNotifyRoute(request, notify)
	})
}

func applyOutgoingSubscriptionNotifyRoute(request, notify *sip.Request) error {
	if request == nil || notify == nil {
		return nil
	}
	cseq, ok := request.CSeq()
	if !ok || cseq == nil || cseq.SeqNo == 0 {
		return fmt.Errorf("subscription refresh request is missing CSeq")
	}
	notifyResponse := sip.NewResponseFromRequest("", notify, http.StatusOK, "OK", nil)
	routed, err := sip.NewRequestFromServerDialogChecked(sip.MethodSubscribe, notify, notifyResponse, cseq.SeqNo)
	if err != nil {
		return fmt.Errorf("build subscription route from NOTIFY: %w", err)
	}
	request.RemoveHeader("Route")
	sip.CopyHeaders("Route", routed, request)
	request.SetRecipient(routed.Recipient().Clone())
	sip.CopyRequestRouteState(routed, request)
	if routed.Destination() != nil {
		request.SetDestination(routed.Destination())
	}
	return nil
}

func applyOutgoingSubscriptionPayload(server *Server, target Targeter, request *sip.Request, body []byte, eventValue string, expires int) {
	if request == nil {
		return
	}
	request.SetBody(body, true)
	request.AppendHeader(&sip.ContentTypeXML)
	if server != nil && server.fromAddress.URI != nil {
		contact := &sip.ContactHeader{DisplayName: server.fromAddress.DisplayName, Address: server.fromAddress.URI.Clone()}
		if server.fromAddress.Params != nil {
			contact.Params = server.fromAddress.Params.Clone()
		}
		request.AppendHeader(contact)
	}
	if versioner, ok := target.(gbVersioner); ok {
		version, valid := ParseGBProtocolVersion(versioner.GBVersion())
		if !valid {
			version = GBVersion10
		}
		header := sip.XGBVer(version)
		request.AppendHeader(&header)
	}
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: eventValue})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: fmt.Sprintf("%d", expires)})
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
	sip.CopyRequestRouteState(dialogRequest, request)
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
	sip.CopyRequestRouteState(dialogRequest, request)
	if dialogRequest.Destination() != nil {
		request.SetDestination(dialogRequest.Destination())
	}
}

// sipNotifyCatalog 处理目录变更通知；应答后通过完整 Catalog 查询收敛本地快照。
func (g *GB28181API) sipNotifyCatalog(ctx *sip.Context) {
	version := g.getDeviceGBProtocolVersion(ctx.DeviceID)
	if err := validateCatalogStructure(ctx.Request.Body(), version, true); err != nil {
		ctx.String(400, "invalid catalog notify")
		return
	}
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
	if _, err := g.validateAndDecodeAppendixA4(ctx.DeviceID, msg.CmdType, ctx.Request.Body()); err != nil {
		ctx.String(400, err.Error())
		return
	}
	if err := respondEventNotifyOK(ctx, version, msg.CmdType, msg.SN, msg.DeviceID); err != nil {
		ctx.Log.Error("respond Catalog NOTIFY", "err", err, "sn", msg.SN, "target_id", msg.DeviceID)
		return
	}
	unlockCommit, err := g.lockAdmittedInboundDeviceStateCommit(ctx)
	if err != nil {
		return
	}
	defer unlockCommit()
	if !g.commitOutgoingSubscriptionNotifyAfterResponse(ctx) {
		return
	}
	g.publishEventNotify("Catalog", ctx.DeviceID, ctx.Request.Body())
	if g.svr == nil || g.svr.memoryStorer == nil {
		return
	}
	deviceID := strings.TrimSpace(ctx.DeviceID)
	g.scheduleCatalogRefresh(deviceID)
}

// sipNotifyMobilePosition 处理位置订阅通知，结构化保存并转发给本级订阅方。
func (g *GB28181API) sipNotifyMobilePosition(ctx *sip.Context) {
	version := g.getDeviceGBProtocolVersion(ctx.DeviceID)
	if err := validateMobilePositionNotifyStructure(ctx.Request.Body(), version); err != nil {
		ctx.String(400, err.Error())
		return
	}
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
	extended, err := inspectAppendixA4Payload(ctx.Request.Body())
	if err != nil {
		ctx.String(400, err.Error())
		return
	}
	if extended {
		ctx.String(400, "MobilePosition does not support Appendix A.4 extensions")
		return
	}
	if err := ctx.RespondString(200, "OK"); err != nil {
		ctx.Log.Error("respond MobilePosition NOTIFY", "err", err, "sn", msg.SN, "target_id", msg.DeviceID)
		return
	}
	unlockCommit, err := g.lockAdmittedInboundDeviceStateCommit(ctx)
	if err != nil {
		return
	}
	defer unlockCommit()
	if !g.commitOutgoingSubscriptionNotifyAfterResponse(ctx) {
		return
	}
	// 9.11 事件源侧：移动位置事件通知订阅方。
	eventTargetID := deviceID
	if position != nil && strings.TrimSpace(position.DeviceID) != "" {
		eventTargetID = strings.TrimSpace(position.DeviceID)
	}
	if len(positions) > 0 {
		// 2022 批量通知保留设备级聚合快照，同时让每个通道都能按自身编码查询最新位置。
		g.storeMobilePositionStateForOwnerLocked(deviceID, deviceID, position, positions)
		eventTargetIDs := make([]string, 0, len(positions))
		for index := range positions {
			item := positions[index]
			eventTargetIDs = append(eventTargetIDs, item.DeviceID)
			if strings.TrimSpace(item.DeviceID) == deviceID {
				continue
			}
			g.storeMobilePositionStateForOwnerLocked(deviceID, item.DeviceID, &item, nil)
		}
		g.publishEventNotifyForTargets("MobilePosition", deviceID, eventTargetIDs, ctx.Request.Body())
	} else if position != nil {
		g.storeMobilePositionStateForOwnerLocked(deviceID, eventTargetID, position, nil)
		g.publishEventNotifyForTarget("MobilePosition", deviceID, eventTargetID, ctx.Request.Body())
	} else {
		g.storeMobilePositionStateForOwnerLocked(deviceID, deviceID, nil, positions)
		g.publishEventNotifyForTargets("MobilePosition", deviceID, nil, ctx.Request.Body())
	}
	// A.2.4 MobilePosition Query 使用 MESSAGE/Notify，而不是等待 Response。
	g.forwardCascadeMobilePositionQueryNotify(deviceID, ctx.Request.Body())
}

type mobilePositionNotify struct {
	XMLName    xml.Name
	CmdType    string      `xml:"CmdType"`
	SN         int         `xml:"SN"`
	DeviceID   string      `xml:"DeviceID"`
	Time       string      `xml:"Time"`
	SumNum     *int        `xml:"SumNum"`
	Longitude  *float64    `xml:"Longitude"`
	Latitude   *float64    `xml:"Latitude"`
	Speed      *float64    `xml:"Speed"`
	Direction  *float64    `xml:"Direction"`
	Altitude   *float64    `xml:"Altitude"`
	Height     *float64    `xml:"Height"`
	Info       []a4XMLNode `xml:"Info"`
	ExtraInfo  []string    `xml:"ExtraInfo,omitempty"`
	ExtralInfo []string    `xml:"ExtralInfo,omitempty"`
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
	if !validGBDateTime(msg.Time) {
		return nil, nil, fmt.Errorf("invalid MobilePosition time")
	}
	version := g.getDeviceGBProtocolVersion(ctx.DeviceID)
	if !version.AtLeast(GBVersion20) {
		return nil, nil, fmt.Errorf("MobilePosition requires GB/T 28181-2016 or later")
	}
	if len(msg.Info) > 0 || len(msg.ExtraInfo) > 0 || len(msg.ExtralInfo) > 0 {
		return nil, nil, fmt.Errorf("MobilePosition does not define Info or ExtraInfo")
	}
	hasBatch := msg.SumNum != nil || msg.DeviceList.XMLName.Local != ""
	if version == GBVersion30 {
		if !isGBDeviceIdentifier(msg.DeviceID) {
			return nil, nil, fmt.Errorf("invalid MobilePosition device id")
		}
		if err := g.validateAuthenticatedResponseTarget(ctx, msg.DeviceID); err != nil {
			return nil, nil, err
		}
		if !hasBatch {
			return nil, nil, fmt.Errorf("GB/T 28181-2022 MobilePosition requires SumNum and DeviceList")
		}
		if msg.Longitude != nil || msg.Latitude != nil || msg.Speed != nil || msg.Direction != nil || msg.Altitude != nil || msg.Height != nil {
			return nil, nil, fmt.Errorf("GB/T 28181-2022 MobilePosition does not support top-level position fields")
		}
		return g.validateBatchMobilePosition(ctx, msg)
	}
	if hasBatch {
		return nil, nil, fmt.Errorf("batch MobilePosition requires GB/T 28181-2022")
	}
	if msg.Height != nil {
		return nil, nil, fmt.Errorf("GB/T 28181-2016 MobilePosition does not define Height")
	}
	targetID := strings.TrimSpace(msg.DeviceID)
	if targetID == "" {
		targetID = g.incomingMobilePositionTarget(ctx)
	}
	if !isGBDeviceIdentifier(targetID) {
		return nil, nil, fmt.Errorf("invalid MobilePosition device id")
	}
	if err := g.validateAuthenticatedResponseTarget(ctx, targetID); err != nil {
		return nil, nil, err
	}
	position := &MobilePositionData{
		DeviceID: targetID, Time: strings.TrimSpace(msg.Time), Longitude: msg.Longitude, Latitude: msg.Latitude,
		Speed: msg.Speed, Direction: msg.Direction, Altitude: msg.Altitude,
	}
	if err := validateMobilePositionData(position, version); err != nil {
		return nil, nil, err
	}
	return position, nil, nil
}

func (g *GB28181API) incomingMobilePositionTarget(ctx *sip.Context) string {
	if ctx != nil {
		if value, ok := ctx.Get(outgoingSubscriptionNotifyContextKey); ok {
			if current, loaded := g.outgoingSubscriptions.Load(value); loaded {
				if dialog, valid := current.(*outgoingSubscriptionDialog); valid && dialog != nil {
					dialog.notifyMu.Lock()
					targetID := strings.TrimSpace(dialog.notify.targetID)
					dialog.notifyMu.Unlock()
					if targetID != "" {
						return targetID
					}
				}
			}
		}
		return strings.TrimSpace(ctx.DeviceID)
	}
	return ""
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
	if msg.DeviceList.Num != nil && (*msg.DeviceList.Num < 0 || *msg.DeviceList.Num != len(msg.DeviceList.Item)) {
		return nil, nil, fmt.Errorf("invalid MobilePosition DeviceList count")
	}
	if len(msg.DeviceList.Item) != *msg.SumNum {
		return nil, nil, fmt.Errorf("invalid MobilePosition DeviceList count")
	}
	positions := make([]MobilePositionData, 0, len(msg.DeviceList.Item))
	var latest *MobilePositionData
	var latestCaptureTime time.Time
	for _, item := range msg.DeviceList.Item {
		position := MobilePositionData{
			DeviceID: strings.TrimSpace(item.DeviceID), Time: strings.TrimSpace(item.CaptureTime), CaptureTime: strings.TrimSpace(item.CaptureTime),
			Longitude: item.Longitude, Latitude: item.Latitude, Speed: item.Speed, Direction: item.Direction, Altitude: item.Altitude, Height: item.Height,
		}
		if err := g.validateAuthenticatedResponseTarget(ctx, position.DeviceID); err != nil {
			return nil, nil, err
		}
		captureTime, err := parseGBDateTime(position.CaptureTime)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid MobilePosition capture time")
		}
		if err := validateMobilePositionData(&position, GBVersion30); err != nil {
			return nil, nil, err
		}
		positions = append(positions, position)
		if latest == nil || !captureTime.Before(latestCaptureTime) {
			latest = &positions[len(positions)-1]
			latestCaptureTime = captureTime
		}
	}
	if latest == nil {
		return nil, positions, nil
	}
	latestCopy := *latest
	return &latestCopy, positions, nil
}

func validateMobilePositionData(position *MobilePositionData, version GBProtocolVersion) error {
	if position == nil || !isGBDeviceIdentifier(position.DeviceID) || position.Longitude == nil || position.Latitude == nil ||
		!validFiniteRange(*position.Longitude, -180, 180) || !validFiniteRange(*position.Latitude, -90, 90) {
		return fmt.Errorf("invalid MobilePosition coordinates")
	}
	for _, value := range []*float64{position.Speed, position.Altitude, position.Height} {
		if value != nil && !validFinite(*value) {
			return fmt.Errorf("invalid MobilePosition value")
		}
	}
	if position.Direction != nil {
		direction := *position.Direction
		if !validFinite(direction) || direction < 0 || version == GBVersion30 && direction >= 360 || version != GBVersion30 && direction > 360 {
			return fmt.Errorf("invalid MobilePosition direction")
		}
	}
	return nil
}

func validGBDateTime(value string) bool {
	_, err := parseGBDateTime(value)
	return err == nil
}

func parseGBDateTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04:05Z07:00", time.RFC3339} {
		if parsed, err := sip.ParseGBTime(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported GB dateTime")
}

func (g *GB28181API) storeMobilePositionState(deviceID string, position *MobilePositionData, positions []MobilePositionData) {
	g.storeMobilePositionStateForOwner(deviceID, deviceID, position, positions)
}

func (g *GB28181API) storeMobilePositionStateForOwner(ownerDeviceID, stateDeviceID string, position *MobilePositionData, positions []MobilePositionData) {
	g.withQueryStateOwner(ownerDeviceID, func(ownerDeviceID string) {
		g.storeMobilePositionStateForOwnerLocked(ownerDeviceID, stateDeviceID, position, positions)
	})
}

func (g *GB28181API) storeMobilePositionStateForOwnerLocked(ownerDeviceID, stateDeviceID string, position *MobilePositionData, positions []MobilePositionData) {
	ownerDeviceID = strings.TrimSpace(ownerDeviceID)
	stateDeviceID = strings.TrimSpace(stateDeviceID)
	if ownerDeviceID == "" || stateDeviceID == "" || !g.queryStateOwnerWritableLocked(ownerDeviceID) {
		return
	}
	g.queryStateMu.Lock()
	defer g.queryStateMu.Unlock()
	state := g.queryStateForUpdateLocked(ownerDeviceID, stateDeviceID)
	state.UpdatedAt = time.Now()
	state.MobilePosition = position
	state.MobilePositions = cloneMobilePositions(positions)
	g.queryStates.Store(stateDeviceID, state)
}
