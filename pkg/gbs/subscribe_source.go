package gbs

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	defaultSubscribeExpires               = 3600
	defaultCascadeCatalogSubscribeExpires = 600
	defaultProbationSubscribeRetryDelay   = 5 * time.Second
	eventNotifyTaskTimeout                = 32 * time.Second // RFC 3261 Timer F：64*T1，默认 T1=500ms。
	eventNotifyMaxAttempts                = 3
	eventNotifyRetryBaseDelay             = time.Second
	eventNotifyRetryMaxDelay              = 8 * time.Second
	eventNotifyQueueMaxBatches            = 128
	eventNotifyQueueMaxBytes              = 4 << 20
	outgoingUnsubscribeNotifyWait         = 32 * time.Second // RFC 6665 Timer N: 64*T1，默认 T1=500ms。
	outgoingSubscriptionNotifyContextKey  = "gb.outgoing_subscription_notify"
	outgoingSubscriptionCommitContextKey  = "gb.outgoing_subscription_commit"
)

var (
	errInvalidSubscriptionState     = errors.New("invalid Subscription-State")
	errInvalidSubscriptionEvent     = errors.New("invalid Event")
	errInvalidSubscriptionDialog    = errors.New("invalid subscription dialog")
	errUnsupportedSubscriptionEvent = errors.New("unsupported Event package")
	errStaleEventNotifyDispatch     = errors.New("stale event NOTIFY dispatch")
)

func subscriptionExpiredAt(now, expiresAt time.Time) bool {
	return !now.Before(expiresAt)
}

// subscribeEventRequest 是 9.11 SUBSCRIBE 订阅体。
type subscribeEventRequest struct {
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
	// StartTime/EndTime 兼容 2011 附录 J.18 示例与 A.2.4 Schema 的字段名不一致。
	StartTime string `xml:"StartTime,omitempty"`
	EndTime   string `xml:"EndTime,omitempty"`
	Interval  *int   `xml:"Interval,omitempty"`
}

type eventSubscriptionFilter struct {
	StartAlarmPriority string
	EndAlarmPriority   string
	AlarmMethod        string
	AlarmType          string
	StartAlarmTime     string
	EndAlarmTime       string
	CatalogStartTime   string
	CatalogEndTime     string
}

type cascadeDownstreamSubscription struct {
	Input           SubscribeInput
	Refs            int
	Identity        *monitorUserIdentity
	LocalGatewayID  string
	RetryAt         time.Time
	RetryBlocked    bool
	RetryGeneration uint64
}

type keyedOperationLock struct {
	mutex cancelableMutex
	refs  int
}

type eventNotifyBatch struct {
	key         any
	cascade     *cascadeWorker
	dialogCSeq  uint32
	cmdType     string
	deviceID    string
	payloads    [][]byte
	byteCount   int
	nextPayload int
	attempts    int
}

type eventNotifyDispatchExpectation struct {
	cascade    *cascadeWorker
	dialogCSeq uint32
}

// eventSubscription 保存事件源侧订阅会话。
type eventSubscription struct {
	mu               sync.Mutex
	catalogMu        sync.Mutex
	notifyMu         sync.Mutex
	notifyDispatchMu sync.Mutex
	notifyQueue      []eventNotifyBatch
	notifyQueueBytes int
	notifyWorker     bool
	notifyOverloaded bool
	// notifyOverloadDialogCSeq 将过载状态绑定到触发它的远端 SUBSCRIBE 对话代次。
	notifyOverloadDialogCSeq uint32

	Key           string
	CmdType       string
	DeviceID      string
	OwnerDeviceID string

	ExpiresAt time.Time

	To     *sip.Address
	Source net.Addr
	Conn   sip.Connection

	GBVersion      string
	Event          string
	DialogRequest  *sip.Request
	Response       *sip.Response
	Contact        *sip.Address
	DialogCallID   string
	RemoteTag      string
	LocalTag       string
	RemoteCSeq     uint32
	CSeq           uint32
	Cascade        *cascadeWorker
	Identity       *monitorUserIdentity
	LocalGatewayID string
	Filter         eventSubscriptionFilter
	Interval       int

	// DownstreamKeys 记录本级为该上级订阅自动建立的下级订阅，用于续订、退订和超时释放。
	DownstreamKeys  []string
	CatalogSnapshot map[string]cascadeCatalogItem
}

// nextEventSubscriptionCSeqLocked 在持有 sub.mu 时校验下一个本端 NOTIFY 序号，
// 但不提交状态。请求的本地构造与安全头校验完成后才能真正推进序号。
func nextEventSubscriptionCSeqLocked(sub *eventSubscription) (uint32, error) {
	if sub == nil {
		return 0, fmt.Errorf("subscription is unavailable")
	}
	next, err := sip.NextCSeq(sub.CSeq)
	if err != nil {
		return 0, fmt.Errorf("subscription NOTIFY CSeq: %w", err)
	}
	return next, nil
}

// reserveEventSubscriptionCSeqLocked 在持有 sub.mu 时预留下一个本端 NOTIFY
// 序号。达到 SIP 上界后必须让订阅对话失败并由续订流程重建，不能回绕。
func reserveEventSubscriptionCSeqLocked(sub *eventSubscription) (uint32, error) {
	next, err := nextEventSubscriptionCSeqLocked(sub)
	if err != nil {
		return 0, err
	}
	sub.CSeq = next
	return next, nil
}

func cloneEventSubscriptionDialogRequest(request *sip.Request) (cloned *sip.Request, err error) {
	if request == nil {
		return nil, fmt.Errorf("subscription dialog request is unavailable")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			cloned = nil
			err = fmt.Errorf("clone subscription dialog request: %v", recovered)
		}
	}()
	message := request.Clone()
	cloned, _ = message.(*sip.Request)
	if cloned == nil {
		return nil, fmt.Errorf("clone subscription dialog request failed")
	}
	return cloned, nil
}

type outgoingSubscriptionDialog struct {
	mu             sync.Mutex
	response       *sip.Response
	requestBody    []byte
	eventValue     string
	deviceID       string
	targetID       string
	identity       *monitorUserIdentity
	localGatewayID string
	expiresAt      time.Time
	expires        int
	refreshAt      time.Time
	refreshing     bool
	autoRefresh    bool
	refreshInput   SubscribeInput
	cancelPending  atomic.Bool

	// notifyOperationMu 串行化同一订阅对话的 NOTIFY 校验、应答和状态提交，
	// 防止较大 CSeq 先提交后让已经返回 200 的较小 CSeq 业务事件被丢弃。
	notifyOperationMu sync.Mutex

	// notifyMu 保护 NOTIFY 对话快照。它与 mu 分离，避免设备在 SUBSCRIBE 最终响应前
	// 先发送首个 NOTIFY 时，因 Subscribe 正在等待响应而形成互锁。
	notifyMu sync.Mutex
	notify   outgoingSubscriptionNotifyDialog
}

type outgoingSubscriptionNotifyDialog struct {
	callID            string
	localTag          string
	remoteTag         string
	routeRequest      *sip.Request
	event             string
	cmdType           string
	deviceID          string
	targetID          string
	expiresAt         time.Time
	reportedExpiresAt time.Time
	cseq              uint32
}

func (d *outgoingSubscriptionDialog) snapshotNotifyDialog() outgoingSubscriptionNotifyDialog {
	if d == nil {
		return outgoingSubscriptionNotifyDialog{}
	}
	d.notifyMu.Lock()
	defer d.notifyMu.Unlock()
	return d.notify
}

func (d *outgoingSubscriptionDialog) restoreNotifyDialog(snapshot outgoingSubscriptionNotifyDialog) {
	if d == nil {
		return
	}
	d.notifyMu.Lock()
	if d.notify.callID == snapshot.callID && d.notify.localTag == snapshot.localTag {
		if snapshot.remoteTag == "" {
			snapshot.remoteTag = d.notify.remoteTag
		}
		if d.notify.cseq > snapshot.cseq {
			snapshot.cseq = d.notify.cseq
			snapshot.routeRequest = d.notify.routeRequest
			if snapshot.reportedExpiresAt.IsZero() ||
				!d.notify.reportedExpiresAt.IsZero() && d.notify.reportedExpiresAt.Before(snapshot.reportedExpiresAt) {
				snapshot.reportedExpiresAt = d.notify.reportedExpiresAt
			}
			if !snapshot.reportedExpiresAt.IsZero() &&
				(snapshot.expiresAt.IsZero() || snapshot.reportedExpiresAt.Before(snapshot.expiresAt)) {
				snapshot.expiresAt = snapshot.reportedExpiresAt
			}
		}
	}
	d.notify = snapshot
	d.notifyMu.Unlock()
}

// restoreNotifyDialogLocked 在持有 d.mu 时恢复 NOTIFY 快照，并把等待期间合法 NOTIFY
// 报告的更短期限同步到外层刷新状态。只允许缩短，不能把临时候选期限提交为正式期限。
func (d *outgoingSubscriptionDialog) restoreNotifyDialogLocked(snapshot outgoingSubscriptionNotifyDialog, now time.Time) {
	if d == nil {
		return
	}
	d.restoreNotifyDialog(snapshot)
	notifyExpiresAt := d.snapshotNotifyDialog().expiresAt
	if d.response == nil || notifyExpiresAt.IsZero() ||
		!d.expiresAt.IsZero() && !notifyExpiresAt.Before(d.expiresAt) {
		return
	}
	d.expiresAt = notifyExpiresAt
	d.expires = subscriptionRemainingSeconds(now, notifyExpiresAt)
	d.refreshAt = outgoingSubscriptionRefreshAtDeadline(now, notifyExpiresAt)
	d.refreshing = false
}

func (d *outgoingSubscriptionDialog) clearPendingNotifyDialog() {
	if d == nil {
		return
	}
	d.notifyMu.Lock()
	d.notify = outgoingSubscriptionNotifyDialog{}
	d.notifyMu.Unlock()
}

func (d *outgoingSubscriptionDialog) setPendingNotifyDialog(request *sip.Request, cmdType, deviceID, targetID string, expires int) {
	if d == nil || request == nil {
		return
	}
	callID, ok := request.CallID()
	if !ok || callID == nil {
		return
	}
	d.notifyMu.Lock()
	remoteTag := ""
	var cseq uint32
	var routeRequest *sip.Request
	if d.notify.callID == normalizeCallID(callID) && d.notify.localTag == sipRequestFromTag(request) {
		remoteTag = d.notify.remoteTag
		cseq = d.notify.cseq
		routeRequest = d.notify.routeRequest
	}
	d.notify = outgoingSubscriptionNotifyDialog{
		callID:       normalizeCallID(callID),
		localTag:     sipRequestFromTag(request),
		remoteTag:    remoteTag,
		routeRequest: routeRequest,
		event:        strings.TrimSpace(d.eventValue),
		cmdType:      strings.TrimSpace(cmdType),
		deviceID:     strings.TrimSpace(deviceID),
		targetID:     strings.TrimSpace(targetID),
		expiresAt:    time.Now().Add(time.Duration(expires) * time.Second),
		cseq:         cseq,
	}
	d.notifyMu.Unlock()
}

func (d *outgoingSubscriptionDialog) confirmNotifyDialog(response *sip.Response, expires int) error {
	if d == nil || response == nil {
		return fmt.Errorf("invalid subscription response dialog")
	}
	callID, ok := response.CallID()
	if !ok || callID == nil {
		return fmt.Errorf("subscription response missing Call-ID")
	}
	remoteTag := sipResponseToTag(response)
	localTag := sipResponseFromTag(response)
	if remoteTag == "" || localTag == "" {
		return fmt.Errorf("subscription response missing dialog tag")
	}
	d.notifyMu.Lock()
	defer d.notifyMu.Unlock()
	if d.notify.callID == "" || d.notify.callID != normalizeCallID(callID) || d.notify.localTag != localTag {
		return fmt.Errorf("subscription response dialog mismatch")
	}
	if d.notify.remoteTag != "" && d.notify.remoteTag != remoteTag {
		return fmt.Errorf("subscription response remote tag mismatch")
	}
	d.notify.remoteTag = remoteTag
	expiresAt := time.Now().Add(time.Duration(expires) * time.Second)
	if !d.notify.reportedExpiresAt.IsZero() && d.notify.reportedExpiresAt.Before(expiresAt) {
		expiresAt = d.notify.reportedExpiresAt
	}
	d.notify.expiresAt = expiresAt
	return nil
}

func nextOutgoingSubscriptionNotifyRoute(current, incoming *sip.Request) (*sip.Request, error) {
	if incoming == nil {
		return nil, fmt.Errorf("NOTIFY request is unavailable")
	}
	contacts := incoming.GetHeaders("Contact")
	if len(contacts) > 1 || current == nil && len(contacts) != 1 {
		return nil, fmt.Errorf("NOTIFY must contain exactly one Contact when establishing a dialog")
	}
	if len(contacts) == 1 {
		contact, ok := contacts[0].(*sip.ContactHeader)
		if !ok || contact == nil || contact.Address == nil || strings.TrimSpace(contact.Address.Host()) == "" {
			return nil, fmt.Errorf("NOTIFY Contact is invalid")
		}
	}
	if current == nil {
		route, ok := incoming.Clone().(*sip.Request)
		if !ok || route == nil {
			return nil, fmt.Errorf("clone NOTIFY dialog route")
		}
		cseq, ok := incoming.CSeq()
		if !ok || cseq == nil || cseq.SeqNo == 0 {
			return nil, fmt.Errorf("NOTIFY CSeq is invalid")
		}
		response := sip.NewResponseFromRequest("", incoming, http.StatusOK, "OK", nil)
		if _, err := sip.NewRequestFromServerDialogChecked(sip.MethodSubscribe, incoming, response, cseq.SeqNo); err != nil {
			return nil, err
		}
		return route, nil
	}
	if len(contacts) == 0 {
		return current, nil
	}
	route, ok := current.Clone().(*sip.Request)
	if !ok || route == nil {
		return nil, fmt.Errorf("clone existing NOTIFY dialog route")
	}
	// NOTIFY 是 target-refresh 请求：Contact 可更新远端目标，但既有路由集不能改变。
	route.RemoveHeader("Contact")
	sip.CopyHeaders("Contact", incoming, route)
	route.SetConnection(incoming.GetConnection())
	route.SetSource(incoming.Source())
	route.SetDestination(incoming.Destination())
	return route, nil
}

// subscriptionTarget 适配 wrapRequest 的 Targeter。
type subscriptionTarget struct {
	to        *sip.Address
	source    net.Addr
	conn      sip.Connection
	gbVersion string
}

func (t *subscriptionTarget) To() *sip.Address {
	return t.to
}

func (t *subscriptionTarget) Source() net.Addr {
	return t.source
}

func (t *subscriptionTarget) Conn() sip.Connection {
	return t.conn
}

func (t *subscriptionTarget) GBVersion() string {
	return t.gbVersion
}

func (g *GB28181API) inboundEventSubscriptionVersion(ctx *sip.Context, cascade *cascadeWorker) GBProtocolVersion {
	if cascade != nil {
		return cascade.protocolVersion()
	}
	if g != nil && g.svr != nil && g.svr.memoryStorer != nil {
		if device, ok := g.svr.memoryStorer.Load(ctx.DeviceID); ok {
			if device != nil {
				if version, valid := ParseGBProtocolVersion(device.GBVersion()); valid {
					return version
				}
			}
			return GBVersion10
		}
	}
	// 无运行态时保留请求头回退，供独立协议处理和现有纯单元测试使用。
	if version, ok := ParseGBProtocolVersion(ctx.XGBVer); ok {
		return version
	}
	return GBVersion10
}

// sipSubscribeEvent 处理事件源侧 SUBSCRIBE 请求。
func (g *GB28181API) sipSubscribeEvent(ctx *sip.Context) {
	if g.serviceStopped() {
		ctx.String(503, ErrServiceStopped.Error())
		return
	}
	var req subscribeEventRequest
	if len(ctx.Request.Body()) == 0 {
		ctx.String(400, "empty subscribe body")
		return
	}
	if err := sip.XMLDecode(ctx.Request.Body(), &req); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	cmdType := strings.TrimSpace(req.CmdType)
	if normalized, ok := normalizeSubscribeCmdType(cmdType); ok {
		cmdType = normalized
	} else {
		ctx.String(400, "unsupported subscribe cmd_type")
		return
	}
	if err := validateSubscribeEventRequestStructure(ctx.Request.Body(), cmdType); err != nil {
		ctx.String(400, err.Error())
		return
	}
	if cmdType == "Alarm" {
		if strings.TrimSpace(req.StartAlarmTime) == "" {
			req.StartAlarmTime = strings.TrimSpace(req.StartTime)
		}
		if strings.TrimSpace(req.EndAlarmTime) == "" {
			req.EndAlarmTime = strings.TrimSpace(req.EndTime)
		}
	}

	deviceID := strings.TrimSpace(req.DeviceID)
	var cascade *cascadeWorker
	if value, exists := ctx.Get(cascadeWorkerContextKey); exists {
		cascade, _ = value.(*cascadeWorker)
		allowed := cascade != nil && cascadeSubscriptionTargetAllowed(cascade.platform, cmdType, deviceID)
		if !allowed && cascade != nil && cmdType == "Catalog" && cascade.protocolVersion().AtLeast(GBVersion11) {
			lookupParent := monitorUserIdentityContextWithParent(g.initializedServiceContext(), ctx)
			lookupCtx, cancel := context.WithTimeout(lookupParent, 5*time.Second)
			visible, lookupErr := g.cascadeCatalogTargetVisible(lookupCtx, cascade.platform, cascade.protocolVersion(), deviceID)
			cancel()
			if lookupErr != nil {
				ctx.String(500, "load cascade Catalog target failed")
				return
			}
			allowed = visible
		}
		if !allowed {
			ctx.String(404, "cascade target not found")
			return
		}
	}
	expiresHeaders := ctx.Request.GetHeaders("Expires")
	if len(expiresHeaders) > 1 {
		ctx.String(400, "duplicate expires header")
		return
	}
	expires, err := parseSubscribeExpiresForProfile(ctx.GetHeader("Expires"), cmdType, cascade)
	if err != nil {
		ctx.String(400, err.Error())
		return
	}
	if cascade != nil && expires != 0 {
		switch cmdType {
		case "Catalog", "Alarm":
		case "MobilePosition":
			if !cascade.protocolVersion().Capabilities().MobilePosition {
				ctx.String(400, "MobilePosition subscription is not supported by cascade protocol version")
				return
			}
		case "PTZPosition":
			if !cascade.protocolVersion().Capabilities().PTZPosition {
				ctx.String(400, "PTZPosition subscription is not supported by cascade protocol version")
				return
			}
		default:
			ctx.String(400, "unsupported cascade subscription")
			return
		}
	}
	subscriptionVersion := g.inboundEventSubscriptionVersion(ctx, cascade)
	if err := validateSubscribeEventEnvelope(req, cmdType); err != nil {
		ctx.String(400, err.Error())
		return
	}
	if expires != 0 {
		if err := validateSubscribeEventRequest(req, cmdType, subscriptionVersion); err != nil {
			ctx.String(400, err.Error())
			return
		}
	}
	eventHeaders := ctx.Request.GetHeaders("Event")
	if len(eventHeaders) != 1 || eventHeaders[0] == nil {
		ctx.String(400, "subscribe request must contain exactly one Event header")
		return
	}
	rawEventValue := ctx.GetHeader("Event")
	eventValue, eventID, err := parseSubscriptionEvent(rawEventValue)
	if err != nil {
		ctx.String(400, err.Error())
		return
	}
	if eventValue == "" {
		ctx.String(400, "missing event header")
		return
	}
	if err := validateSubscriptionEventHeader(eventValue, cmdType, eventID); err != nil {
		ctx.String(400, err.Error())
		return
	}
	if strings.EqualFold(cmdType, "Catalog") {
		if subscriptionVersion == GBVersion10 && !strings.EqualFold(strings.TrimSpace(rawEventValue), "presence") {
			ctx.String(400, "GB/T 28181-2011 Catalog Event must use presence")
			return
		}
		if cascade != nil && subscriptionVersion.AtLeast(GBVersion11) {
			if err := validateInterdomainCatalogEventHeader(rawEventValue); err != nil {
				ctx.String(400, err.Error())
				return
			}
		}
	}

	targetAddr := ctx.To
	if contact, ok := ctx.Request.Contact(); ok && contact != nil && contact.Address != nil {
		targetAddr = &sip.Address{
			DisplayName: contact.DisplayName,
			URI:         contact.Address.Clone(),
			Params:      contact.Params,
		}
	}
	if targetAddr == nil || targetAddr.URI == nil {
		ctx.String(400, "invalid subscribe target")
		return
	}

	dialog, err := parseSubscribeDialog(ctx)
	if err != nil {
		ctx.String(400, err.Error())
		return
	}
	key := buildEventSubscriptionKey(subscriptionOwnerKey(ctx, cascade), dialog.callID, dialog.fromTag, cmdType, deviceID)
	identityCtx := monitorUserIdentityContextWithParent(g.serviceContext(), ctx)
	var ownerOperation *pendingDeviceOperation
	if cascade == nil {
		unlockAdmission, err := g.lockAdmittedInboundDeviceStateCommit(ctx)
		if err != nil {
			ctx.String(http.StatusForbidden, err.Error())
			return
		}
		unlockAdmission()
		var releaseOwnerOperation func()
		ownerOperation, releaseOwnerOperation = g.trackPendingDeviceRequest(identityCtx, ctx.DeviceID, deviceID)
		defer releaseOwnerOperation()
		identityCtx = ownerOperation.Context(identityCtx)
		if identityCtx.Err() != nil {
			ctx.String(403, ownerOperation.Cause().Error())
			return
		}
	}
	unlockSubscription, err := g.lockEventSubscriptionOperation(identityCtx, key)
	if err != nil {
		ctx.String(503, err.Error())
		return
	}
	defer unlockSubscription()
	var existing *eventSubscription
	if value, loaded := g.eventSubscribers.Load(key); loaded {
		existing, _ = value.(*eventSubscription)
		if existing == nil {
			g.eventSubscribers.CompareAndDelete(key, value)
		}
	}
	if err := validateInboundSubscribeDialog(existing, dialog); err != nil {
		ctx.String(481, err.Error())
		return
	}
	if existing != nil {
		existing.mu.Lock()
		existingEvent := existing.Event
		existing.mu.Unlock()
		if !subscriptionEventHeadersMatch(existingEvent, rawEventValue) {
			ctx.String(481, "subscription Event header does not match dialog")
			return
		}
	}
	if expires == 0 {
		// Expires=0 为退订。
		if existing == nil {
			ctx.String(481, "subscription dialog does not exist")
			return
		}
		if _, _, err := g.respondSubscribeOK(ctx, req, eventValue, expires, cascade, subscriptionVersion); err != nil {
			ctx.Log.Error("respond SUBSCRIBE cancel", "err", err, "cmd_type", cmdType, "target_id", deviceID)
			return
		}
		var unlockCommit func()
		if ownerOperation != nil {
			unlockCommit, err = g.lockAdmittedInboundDeviceStateCommit(ctx)
			if err != nil {
				return
			}
		}
		var downstreamKeys []string
		commitCancel := func() {
			if !g.eventSubscribers.CompareAndDelete(key, existing) {
				return
			}
			existing.mu.Lock()
			existing.ExpiresAt = time.Now()
			existing.DialogRequest = nil
			existing.Response = nil
			downstreamKeys = append([]string(nil), existing.DownstreamKeys...)
			existing.DownstreamKeys = nil
			existing.mu.Unlock()
		}
		if ownerOperation != nil && !ownerOperation.Deliver(commitCancel) {
			unlockCommit()
			return
		}
		if ownerOperation == nil {
			commitCancel()
		}
		if unlockCommit != nil {
			unlockCommit()
		}
		if _, exists := g.eventSubscribers.Load(key); exists {
			ctx.Log.Error("commit SUBSCRIBE cancel", "err", "subscription dialog changed", "cmd_type", cmdType, "target_id", deviceID)
			return
		}
		g.releaseCascadeDownstreamSubscriptions(identityCtx, downstreamKeys)
		return
	}
	var previousKeys []string
	if existing != nil {
		existing.mu.Lock()
		previousKeys = append(previousKeys, existing.DownstreamKeys...)
		existing.mu.Unlock()
	}
	desired, err := g.desiredCascadeDownstreamSubscriptions(identityCtx, cascade, req, cmdType, deviceID, expires)
	if err != nil {
		ctx.String(502, err.Error())
		return
	}
	previousDownstream := g.snapshotCascadeDownstreamSubscriptions(previousKeys)
	downstreamKeys, err := g.syncCascadeDownstreamSubscriptions(identityCtx, previousKeys, desired)
	if err != nil {
		ctx.String(502, err.Error())
		return
	}

	sub := &eventSubscription{
		Key:      key,
		CmdType:  cmdType,
		DeviceID: deviceID,
		OwnerDeviceID: func() string {
			if cascade == nil {
				return strings.TrimSpace(ctx.DeviceID)
			}
			return ""
		}(),
		ExpiresAt: time.Now().Add(time.Duration(expires) * time.Second),
		To:        targetAddr.Clone(),
		Source:    ctx.Source,
		Conn:      ctx.Request.GetConnection(),
		GBVersion: string(subscriptionVersion),
		Event:     eventValue,
		DialogRequest: func() *sip.Request {
			if existing != nil {
				existing.mu.Lock()
				defer existing.mu.Unlock()
				return refreshInboundSubscriptionDialog(existing.DialogRequest, ctx.Request)
			}
			return refreshInboundSubscriptionDialog(nil, ctx.Request)
		}(),
		Cascade:  cascade,
		Identity: monitorUserIdentityFromContext(monitorUserIdentityContext(ctx)),
		LocalGatewayID: func() string {
			if cascade != nil && cascade.platform.monitorUserIdentity != nil {
				return cascade.platform.monitorUserIdentity.localGatewayID
			}
			return ""
		}(),
		Filter:         subscriptionFilterFromRequest(req),
		Interval:       subscribeRequestInterval(req),
		DownstreamKeys: downstreamKeys,
	}
	response, contact, err := g.respondSubscribeOK(ctx, req, eventValue, expires, cascade, subscriptionVersion)
	if err != nil {
		g.rollbackCascadeDownstreamSubscriptionSync(identityCtx, downstreamKeys, previousDownstream)
		ctx.Log.Error("respond SUBSCRIBE", "err", err, "cmd_type", cmdType, "target_id", deviceID)
		return
	}
	sub.Response = response
	sub.Contact = contact
	sub.DialogCallID = dialog.callID
	sub.RemoteTag = dialog.fromTag
	sub.LocalTag = sipResponseToTag(response)
	sub.RemoteCSeq = dialog.remoteCSeq
	var unlockCommit func()
	if ownerOperation != nil {
		unlockCommit, err = g.lockAdmittedInboundDeviceStateCommit(ctx)
		if err != nil {
			g.rollbackCascadeDownstreamSubscriptionSync(identityCtx, downstreamKeys, previousDownstream)
			return
		}
	}
	initial := true
	commitSubscription := func() {
		if actual, loaded := g.eventSubscribers.LoadOrStore(key, sub); loaded {
			if existing, ok := actual.(*eventSubscription); ok && existing != nil {
				newRemoteCSeq := sub.RemoteCSeq
				existing.mu.Lock()
				existing.CmdType = sub.CmdType
				existing.DeviceID = sub.DeviceID
				existing.OwnerDeviceID = sub.OwnerDeviceID
				existing.ExpiresAt = sub.ExpiresAt
				existing.To = sub.To
				existing.Source = sub.Source
				existing.Conn = sub.Conn
				existing.GBVersion = sub.GBVersion
				existing.Event = sub.Event
				existing.DialogRequest = sub.DialogRequest
				existing.Response = sub.Response
				existing.Contact = sub.Contact
				existing.DialogCallID = sub.DialogCallID
				existing.RemoteTag = sub.RemoteTag
				existing.LocalTag = sub.LocalTag
				existing.RemoteCSeq = sub.RemoteCSeq
				existing.Cascade = sub.Cascade
				existing.Identity = sub.Identity.clone()
				existing.LocalGatewayID = sub.LocalGatewayID
				existing.Filter = sub.Filter
				existing.Interval = sub.Interval
				existing.DownstreamKeys = append(existing.DownstreamKeys[:0], sub.DownstreamKeys...)
				existing.mu.Unlock()
				resetStaleEventNotifyOverload(existing, newRemoteCSeq)
				sub = existing
				initial = false
			} else {
				g.eventSubscribers.Store(key, sub)
			}
		}
	}
	if ownerOperation != nil && !ownerOperation.Deliver(commitSubscription) {
		unlockCommit()
		g.rollbackCascadeDownstreamSubscriptionSync(identityCtx, downstreamKeys, previousDownstream)
		return
	}
	if ownerOperation == nil {
		commitSubscription()
	}
	if unlockCommit != nil {
		unlockCommit()
	}
	if g.serviceStopped() {
		if g.eventSubscribers.CompareAndDelete(key, sub) {
			sub.mu.Lock()
			downstreamKeys := append([]string(nil), sub.DownstreamKeys...)
			sub.mu.Unlock()
			g.releaseCascadeDownstreamSubscriptions(identityCtx, downstreamKeys)
		}
		return
	}
	if initial && shouldSendCascadeInitialCatalogNotify(cascade, cmdType) {
		g.startCascadeLifecycleTask(context.Background(), cascade, func(taskCtx context.Context) {
			initialCtx, cancel := context.WithTimeout(taskCtx, 30*time.Second)
			defer cancel()
			if err := g.sendCascadeInitialCatalogNotify(initialCtx, sub); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("send initial cascade Catalog NOTIFY failed", "upstream", cascade.platform.name, "err", err)
			}
		})
	} else if initial && cascade != nil && strings.EqualFold(cmdType, "Catalog") {
		seedCtx, cancel := context.WithTimeout(identityCtx, 5*time.Second)
		if err := g.seedCascadeCatalogSnapshot(seedCtx, sub); err != nil {
			slog.Warn("seed cascade Catalog subscription snapshot failed", "upstream", cascade.platform.name, "err", err)
		}
		cancel()
	}
}

// sipNotifySubscriptionState 校验 RFC 3265/6665 订阅对话。
// GB/T 28181-2016 附录 P 允许 terminated NOTIFY 消息体为空，此时仍应返回 200 OK。
func (g *GB28181API) sipNotifySubscriptionState(ctx *sip.Context) {
	if len(ctx.Request.Body()) == 0 {
		if !strings.EqualFold(subscriptionStateName(ctx.GetHeader("Subscription-State")), "terminated") {
			ctx.String(400, "empty notify body")
			ctx.Abort()
			return
		}
		_, validatedAt, unlock, err := g.lockValidatedOutgoingSubscriptionNotify(ctx.DeviceID, ctx.Request, "")
		if err != nil {
			respondOutgoingSubscriptionNotifyError(ctx, err)
			ctx.Abort()
			return
		}
		defer unlock()
		if err = ctx.RespondString(200, "OK"); err != nil {
			slog.Error("respond empty terminated NOTIFY failed", "device_id", ctx.DeviceID, "err", err)
			ctx.Abort()
			return
		}
		matchedKey, err := g.validateOutgoingSubscriptionNotifyModeAt(true, validatedAt, ctx.DeviceID, ctx.Request, "")
		if err != nil {
			slog.Error("commit empty terminated NOTIFY failed", "device_id", ctx.DeviceID, "err", err)
			ctx.Abort()
			return
		}
		ctx.Set(outgoingSubscriptionNotifyContextKey, matchedKey)
		ctx.Abort()
		return
	}
	var envelope struct {
		CmdType    string `xml:"CmdType"`
		DeviceID   string `xml:"DeviceID"`
		DeviceList struct {
			Item []struct {
				DeviceID string `xml:"DeviceID"`
			} `xml:"Item"`
		} `xml:"DeviceList"`
	}
	if err := sip.XMLDecode(ctx.Request.Body(), &envelope); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		ctx.Abort()
		return
	}
	cmdType, ok := normalizeSubscribeCmdType(envelope.CmdType)
	if !ok {
		// 独立业务 NOTIFY 不属于 SUBSCRIBE/NOTIFY 对话，继续交给专用 handler。
		ctx.Next()
		return
	}
	targetIDs := []string{envelope.DeviceID}
	if strings.EqualFold(cmdType, "MobilePosition") {
		for _, item := range envelope.DeviceList.Item {
			targetIDs = append(targetIDs, item.DeviceID)
		}
	}
	targetIDs = uniqueEventTargetIDs(targetIDs)
	matchedKey, validatedAt, unlock, err := g.lockValidatedOutgoingSubscriptionNotify(ctx.DeviceID, ctx.Request, cmdType, targetIDs...)
	if err != nil {
		respondOutgoingSubscriptionNotifyError(ctx, err)
		ctx.Abort()
		return
	}
	defer unlock()
	ctx.Set(outgoingSubscriptionNotifyContextKey, matchedKey)
	ctx.Set(outgoingSubscriptionCommitContextKey, &outgoingSubscriptionNotifyCommit{
		deviceID:    ctx.DeviceID,
		request:     ctx.Request,
		cmdType:     cmdType,
		targetIDs:   append([]string(nil), targetIDs...),
		validatedAt: validatedAt,
	})
	ctx.Next()
}

func (g *GB28181API) lockValidatedOutgoingSubscriptionNotify(deviceID string, request *sip.Request, cmdType string, targetIDs ...string) (any, time.Time, func(), error) {
	matchedKey, err := g.validateOutgoingSubscriptionNotifyMode(false, deviceID, request, cmdType, targetIDs...)
	if err != nil {
		return nil, time.Time{}, nil, err
	}
	value, loaded := g.outgoingSubscriptions.Load(matchedKey)
	dialog, ok := value.(*outgoingSubscriptionDialog)
	if !loaded || !ok || dialog == nil {
		return nil, time.Time{}, nil, fmt.Errorf("subscription dialog changed")
	}
	dialog.notifyOperationMu.Lock()
	current, loaded := g.outgoingSubscriptions.Load(matchedKey)
	if !loaded || current != dialog {
		dialog.notifyOperationMu.Unlock()
		return nil, time.Time{}, nil, fmt.Errorf("subscription dialog changed")
	}
	validatedAt := time.Now()
	validatedKey, err := g.validateOutgoingSubscriptionNotifyModeAt(false, validatedAt, deviceID, request, cmdType, targetIDs...)
	if err != nil || validatedKey != matchedKey {
		dialog.notifyOperationMu.Unlock()
		if err != nil {
			return nil, time.Time{}, nil, err
		}
		return nil, time.Time{}, nil, fmt.Errorf("subscription dialog changed")
	}
	return matchedKey, validatedAt, dialog.notifyOperationMu.Unlock, nil
}

func (g *GB28181API) compareAndDeleteOutgoingSubscription(key any, dialog *outgoingSubscriptionDialog) bool {
	if g == nil || dialog == nil {
		return false
	}
	dialog.notifyOperationMu.Lock()
	deleted := g.outgoingSubscriptions.CompareAndDelete(key, dialog)
	dialog.notifyOperationMu.Unlock()
	return deleted
}

func (g *GB28181API) loadAndDeleteOutgoingSubscription(key any) (any, bool) {
	if g == nil {
		return nil, false
	}
	for {
		value, loaded := g.outgoingSubscriptions.Load(key)
		if !loaded {
			return nil, false
		}
		dialog, ok := value.(*outgoingSubscriptionDialog)
		if !ok || dialog == nil {
			if g.outgoingSubscriptions.CompareAndDelete(key, value) {
				return value, true
			}
			continue
		}
		if g.compareAndDeleteOutgoingSubscription(key, dialog) {
			return dialog, true
		}
	}
}

func respondOutgoingSubscriptionNotifyError(ctx *sip.Context, err error) {
	switch {
	case errors.Is(err, errInvalidSubscriptionState):
		ctx.String(400, errInvalidSubscriptionState.Error())
	case errors.Is(err, errInvalidSubscriptionEvent):
		ctx.String(400, errInvalidSubscriptionEvent.Error())
	case errors.Is(err, errInvalidSubscriptionDialog):
		ctx.String(400, errInvalidSubscriptionDialog.Error())
	case errors.Is(err, errUnsupportedSubscriptionEvent):
		ctx.String(489, "Bad Event")
	default:
		ctx.String(481, "subscription dialog does not exist")
	}
}

type subscriptionStateValue struct {
	name       string
	reason     string
	expires    *time.Duration
	retryAfter *time.Duration
}

type outgoingSubscriptionNotifyCommit struct {
	deviceID    string
	request     *sip.Request
	cmdType     string
	targetIDs   []string
	validatedAt time.Time
	committed   bool
}

func subscriptionStateName(value string) string {
	state, _, _ := strings.Cut(strings.TrimSpace(value), ";")
	return strings.ToLower(strings.TrimSpace(state))
}

func parseSubscriptionState(value string) (subscriptionStateValue, error) {
	segments, err := splitSubscriptionStateSegments(strings.TrimSpace(value))
	if err != nil || len(segments) == 0 {
		return subscriptionStateValue{}, fmt.Errorf("invalid Subscription-State")
	}
	state := subscriptionStateValue{name: strings.ToLower(strings.TrimSpace(segments[0]))}
	if !validSubscriptionStateToken(state.name) {
		return subscriptionStateValue{}, fmt.Errorf("invalid Subscription-State")
	}
	known := make(map[string]struct{}, 3)
	for _, raw := range segments[1:] {
		parameter := strings.TrimSpace(raw)
		if parameter == "" {
			return subscriptionStateValue{}, fmt.Errorf("invalid Subscription-State parameter")
		}
		name, rawValue, hasValue := strings.Cut(parameter, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			return subscriptionStateValue{}, fmt.Errorf("invalid Subscription-State parameter")
		}
		switch name {
		case "expires", "reason", "retry-after":
			if _, duplicate := known[name]; duplicate {
				return subscriptionStateValue{}, fmt.Errorf("duplicate Subscription-State %s parameter", name)
			}
			known[name] = struct{}{}
		}
		switch name {
		case "expires", "retry-after":
			duration, parseErr := parseSubscriptionStateDeltaSeconds(name, rawValue, hasValue)
			if parseErr != nil {
				return subscriptionStateValue{}, parseErr
			}
			if name == "expires" {
				state.expires = duration
			} else {
				state.retryAfter = duration
			}
		case "reason":
			rawValue = strings.TrimSpace(rawValue)
			if !hasValue || !validSubscriptionStateToken(rawValue) {
				return subscriptionStateValue{}, fmt.Errorf("invalid Subscription-State reason parameter")
			}
			state.reason = strings.ToLower(rawValue)
		}
	}
	return state, nil
}

func parseSubscriptionStateDeltaSeconds(name, rawValue string, hasValue bool) (*time.Duration, error) {
	rawValue = strings.TrimSpace(rawValue)
	if !hasValue || rawValue == "" {
		return nil, fmt.Errorf("invalid Subscription-State %s parameter", name)
	}
	for _, char := range rawValue {
		if char < '0' || char > '9' {
			return nil, fmt.Errorf("invalid Subscription-State %s parameter", name)
		}
	}
	seconds, err := strconv.ParseInt(rawValue, 10, 64)
	const maxDurationSeconds = int64(1<<63-1) / int64(time.Second)
	if err != nil || seconds > maxDurationSeconds {
		return nil, fmt.Errorf("invalid Subscription-State %s parameter", name)
	}
	duration := time.Duration(seconds) * time.Second
	return &duration, nil
}

func validSubscriptionStateToken(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			continue
		}
		switch char {
		case '-', '.', '!', '%', '*', '_', '+', '`', '\'', '~':
		default:
			return false
		}
	}
	return true
}

func terminatedSubscriptionRetry(state subscriptionStateValue, version GBProtocolVersion) (bool, time.Duration) {
	switch state.reason {
	case "rejected", "noresource":
		return false, 0
	case "invariant":
		// RFC 6665 新增 invariant，2022 订阅者不得再次订阅；
		// 旧三版引用 RFC 3265，仍按未知扩展原因保持兼容。
		if version.AtLeast(GBVersion30) {
			return false, 0
		}
		if state.retryAfter != nil {
			return true, *state.retryAfter
		}
		return true, 0
	case "deactivated", "timeout":
		// RFC 3265 明确允许立即重订，retry-after 对这两个原因没有语义。
		return true, 0
	case "probation":
		if state.retryAfter != nil {
			return true, *state.retryAfter
		}
		return true, defaultProbationSubscribeRetryDelay
	default:
		// giveup、未知扩展原因或未携带 reason 时均允许重试，但需尊重 retry-after。
		if state.retryAfter != nil {
			return true, *state.retryAfter
		}
		return true, 0
	}
}

func splitSubscriptionStateSegments(value string) ([]string, error) {
	if value == "" {
		return nil, fmt.Errorf("empty Subscription-State")
	}
	segments := make([]string, 0, 4)
	start := 0
	inQuotes := false
	escaped := false
	for index := 0; index < len(value); index++ {
		char := value[index]
		if escaped {
			escaped = false
			continue
		}
		if inQuotes && char == '\\' {
			escaped = true
			continue
		}
		if char == '"' {
			inQuotes = !inQuotes
			continue
		}
		if char == ';' && !inQuotes {
			segments = append(segments, value[start:index])
			start = index + 1
		}
	}
	if inQuotes || escaped {
		return nil, fmt.Errorf("invalid quoted Subscription-State parameter")
	}
	segments = append(segments, value[start:])
	return segments, nil
}

func (g *GB28181API) validateOutgoingSubscriptionNotify(deviceID string, request *sip.Request, cmdType string, targetIDs ...string) (any, error) {
	return g.validateOutgoingSubscriptionNotifyMode(true, deviceID, request, cmdType, targetIDs...)
}

func (g *GB28181API) validateOutgoingSubscriptionNotifyMode(commit bool, deviceID string, request *sip.Request, cmdType string, targetIDs ...string) (any, error) {
	return g.validateOutgoingSubscriptionNotifyModeAt(commit, time.Now(), deviceID, request, cmdType, targetIDs...)
}

func (g *GB28181API) validateOutgoingSubscriptionNotifyModeAt(commit bool, validatedAt time.Time, deviceID string, request *sip.Request, cmdType string, targetIDs ...string) (any, error) {
	if g == nil || request == nil {
		return nil, fmt.Errorf("subscription dialog is unavailable")
	}
	state, err := parseSubscriptionState(firstSingleHeaderValue(request, "Subscription-State"))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidSubscriptionState, err)
	}
	version := g.getDeviceGBProtocolVersion(deviceID)
	if version.AtLeast(GBVersion30) && (state.name == "active" || state.name == "pending") && state.expires == nil {
		return nil, fmt.Errorf("%w: RFC 6665 active or pending state requires expires", errInvalidSubscriptionState)
	}
	eventHeaders := request.GetHeaders("Event")
	if len(eventHeaders) != 1 || eventHeaders[0] == nil {
		return nil, fmt.Errorf("%w: NOTIFY must contain exactly one Event header", errInvalidSubscriptionEvent)
	}
	eventValue := firstSingleHeaderValue(request, "Event")
	parsedEvent, _, err := parseSubscriptionEvent(eventValue)
	if err != nil || strings.TrimSpace(parsedEvent) == "" {
		return nil, fmt.Errorf("%w: malformed Event header", errInvalidSubscriptionEvent)
	}
	eventPackage, _, _ := strings.Cut(parsedEvent, ";")
	if !supportedSubscriptionEventPackage(eventPackage) {
		return nil, fmt.Errorf("%w: %s", errUnsupportedSubscriptionEvent, strings.TrimSpace(eventPackage))
	}
	callID, ok := request.CallID()
	if !ok || callID == nil {
		return nil, fmt.Errorf("missing Call-ID")
	}
	wantedCallID := normalizeCallID(callID)
	wantedFromTag := sipRequestFromTag(request)
	wantedToTag := sipRequestToTag(request)
	if wantedCallID == "" || wantedFromTag == "" || wantedToTag == "" {
		return nil, fmt.Errorf("invalid NOTIFY dialog")
	}
	cseq, ok := request.CSeq()
	if !ok || cseq == nil || cseq.MethodName != sip.MethodNotify || cseq.SeqNo == 0 {
		return nil, fmt.Errorf("invalid NOTIFY CSeq")
	}
	now := time.Now()
	if validatedAt.IsZero() {
		validatedAt = now
	}
	targetIDs = uniqueEventTargetIDs(targetIDs)
	var matchedKey any
	var matchedDialog *outgoingSubscriptionDialog
	var matchedExpiresAt time.Time
	var routeErr error
	g.outgoingSubscriptions.Range(func(key, value any) bool {
		dialog, ok := value.(*outgoingSubscriptionDialog)
		if !ok || dialog == nil {
			return true
		}
		dialog.notifyMu.Lock()
		snapshot := dialog.notify
		matches := snapshot.callID == wantedCallID && snapshot.localTag == wantedToTag &&
			(snapshot.remoteTag == "" || snapshot.remoteTag == wantedFromTag) &&
			strings.EqualFold(snapshot.deviceID, strings.TrimSpace(deviceID)) &&
			subscriptionEventHeadersMatch(snapshot.event, eventValue) &&
			(cmdType == "" || strings.EqualFold(snapshot.cmdType, cmdType)) &&
			subscriptionNotifyTargetsMatch(snapshot, targetIDs) &&
			cseq.SeqNo > snapshot.cseq &&
			(validatedAt.Before(snapshot.expiresAt) || state.name == "terminated")
		var nextRoute *sip.Request
		if matches {
			nextRoute, routeErr = nextOutgoingSubscriptionNotifyRoute(snapshot.routeRequest, request)
			if routeErr != nil {
				dialog.notifyMu.Unlock()
				return false
			}
		}
		if commit && matches && snapshot.remoteTag == "" {
			// RFC 6665 允许首个 NOTIFY 先于 SUBSCRIBE 最终响应，首个合法请求绑定远端 tag。
			dialog.notify.remoteTag = wantedFromTag
		}
		if commit && matches {
			// RFC 6665 4.4.1：路由集由首个合法 NOTIFY 建立；后续 NOTIFY 仅刷新远端目标。
			dialog.notify.routeRequest = nextRoute
		}
		if commit && matches {
			dialog.notify.cseq = cseq.SeqNo
			if state.expires != nil && state.name != "terminated" {
				reportedExpiresAt := validatedAt.Add(*state.expires)
				if snapshot.expiresAt.Before(reportedExpiresAt) {
					reportedExpiresAt = snapshot.expiresAt
				}
				if !snapshot.reportedExpiresAt.IsZero() && snapshot.reportedExpiresAt.Before(reportedExpiresAt) {
					reportedExpiresAt = snapshot.reportedExpiresAt
				}
				dialog.notify.reportedExpiresAt = reportedExpiresAt
				if !dialog.cancelPending.Load() {
					matchedExpiresAt = reportedExpiresAt
					dialog.notify.expiresAt = matchedExpiresAt
				}
			}
		}
		dialog.notifyMu.Unlock()
		if matches {
			matchedKey = key
			matchedDialog = dialog
			return false
		}
		return true
	})
	if routeErr != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidSubscriptionDialog, routeErr)
	}
	if matchedKey == nil || matchedDialog == nil {
		return nil, fmt.Errorf("subscription dialog does not exist")
	}
	if !commit {
		return matchedKey, nil
	}
	current, loaded := g.outgoingSubscriptions.Load(matchedKey)
	if !loaded || current != matchedDialog {
		return nil, fmt.Errorf("subscription dialog changed")
	}
	if !matchedExpiresAt.IsZero() {
		shortenOutgoingSubscriptionExpiry(matchedDialog, matchedExpiresAt, now)
	}
	if state.name == "terminated" {
		retry, delay := terminatedSubscriptionRetry(state, version)
		if !g.terminateOutgoingSubscription(matchedKey, matchedDialog, retry, delay, validatedAt, state) {
			return nil, fmt.Errorf("subscription dialog changed")
		}
		if retry {
			remainingDelay := delay
			if remainingDelay > 0 {
				remainingDelay = time.Until(validatedAt.Add(delay))
				if remainingDelay < 0 {
					remainingDelay = 0
				}
			}
			if !g.startLifecycleTask(context.Background(), func(taskCtx context.Context) {
				g.renewTerminatedCascadeSubscription(matchedKey, matchedDialog, taskCtx, remainingDelay)
			}) {
				return nil, ErrServiceStopped
			}
		}
	}
	return matchedKey, nil
}

func (g *GB28181API) commitOutgoingSubscriptionNotifyState(ctx *sip.Context) error {
	if ctx == nil {
		return fmt.Errorf("subscription context is unavailable")
	}
	value, ok := ctx.Get(outgoingSubscriptionCommitContextKey)
	if !ok {
		// 直接调用业务 handler 的兼容入口没有订阅中间件上下文。
		return nil
	}
	pending, ok := value.(*outgoingSubscriptionNotifyCommit)
	if !ok || pending == nil || pending.request == nil {
		return fmt.Errorf("subscription dialog does not exist")
	}
	if pending.committed {
		return nil
	}
	matchedKey, err := g.validateOutgoingSubscriptionNotifyModeAt(true, pending.validatedAt,
		pending.deviceID, pending.request, pending.cmdType, pending.targetIDs...)
	if err != nil {
		return err
	}
	pending.committed = true
	ctx.Set(outgoingSubscriptionNotifyContextKey, matchedKey)
	return nil
}

// commitOutgoingSubscriptionNotify 在尚未确认 SIP 事务时提交 NOTIFY，并把对话错误映射为 SIP 错误响应。
func (g *GB28181API) commitOutgoingSubscriptionNotify(ctx *sip.Context) bool {
	if err := g.commitOutgoingSubscriptionNotifyState(ctx); err != nil {
		respondOutgoingSubscriptionNotifyError(ctx, err)
		ctx.Abort()
		return false
	}
	return true
}

// commitOutgoingSubscriptionNotifyAfterResponse 用于业务正文已校验且 200 OK 已成功写出的路径。
func (g *GB28181API) commitOutgoingSubscriptionNotifyAfterResponse(ctx *sip.Context) bool {
	if err := g.commitOutgoingSubscriptionNotifyState(ctx); err != nil {
		slog.Error("commit acknowledged NOTIFY failed", "device_id", ctx.DeviceID, "err", err)
		ctx.Abort()
		return false
	}
	return true
}

func (g *GB28181API) terminateOutgoingSubscription(key any, dialog *outgoingSubscriptionDialog, retry bool, delay time.Duration, now time.Time, terminationStates ...subscriptionStateValue) bool {
	if g == nil || dialog == nil {
		return false
	}
	g.cascadeSubscriptionMu.Lock()
	if !g.outgoingSubscriptions.CompareAndDelete(key, dialog) {
		g.cascadeSubscriptionMu.Unlock()
		return false
	}
	if dialog.cancelPending.Load() {
		retry = false
	}
	keyString, ok := key.(string)
	if !ok || keyString == "" {
		g.cascadeSubscriptionMu.Unlock()
		return true
	}
	state := g.cascadeSubscriptions[keyString]
	if state != nil {
		state.RetryBlocked = !retry
		state.RetryAt = time.Time{}
		state.RetryGeneration++
		if retry && delay > 0 {
			state.RetryAt = now.Add(delay)
		}
	}
	g.cascadeSubscriptionMu.Unlock()

	// 人工订阅与级联引用使用同一运行态对话，但只有人工订阅有持久化意图。
	// 把终止策略写回持久状态，避免服务重启或设备重新注册绕过
	// rejected/noresource/invariant 以及 retry-after。
	if len(terminationStates) > 0 {
		if _, persistErr := g.persistManualSubscriptionTermination(keyString, dialog.deviceID, terminationStates[0], retry, delay, now); persistErr != nil {
			slog.Error("persist manual subscription termination failed", "device_id", dialog.deviceID, "key", keyString, "err", persistErr)
		}
	}
	return true
}

func shortenOutgoingSubscriptionExpiry(dialog *outgoingSubscriptionDialog, expiresAt, now time.Time) {
	if dialog == nil || expiresAt.IsZero() {
		return
	}
	// 首个 NOTIFY 可以早于 SUBSCRIBE 最终响应。此时 Subscribe 持有 mu 等待响应，
	// 不能让 NOTIFY 为更新外层计时器而阻塞；最终响应路径会从 notify 快照再次收敛期限。
	if !dialog.mu.TryLock() {
		return
	}
	defer dialog.mu.Unlock()
	if dialog.response == nil || (!dialog.expiresAt.IsZero() && !expiresAt.Before(dialog.expiresAt)) {
		return
	}
	dialog.expiresAt = expiresAt
	dialog.expires = subscriptionRemainingSeconds(now, expiresAt)
	dialog.refreshAt = outgoingSubscriptionRefreshAtDeadline(now, expiresAt)
	dialog.refreshing = false
}

func subscriptionRemainingSeconds(now, expiresAt time.Time) int {
	remaining := expiresAt.Sub(now)
	if remaining <= 0 {
		return 0
	}
	return int((remaining + time.Second - 1) / time.Second)
}

func subscriptionNotifyTargetMatches(dialog outgoingSubscriptionNotifyDialog, targetID string) bool {
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return true
	}
	// 订阅父设备时允许其已知子通道上报，具体所有权仍由业务包络校验；
	// 订阅指定子通道时必须精确匹配，防止同一 NVR 的兄弟通道串话。
	return strings.EqualFold(dialog.targetID, dialog.deviceID) || strings.EqualFold(dialog.targetID, targetID)
}

func subscriptionNotifyTargetsMatch(dialog outgoingSubscriptionNotifyDialog, targetIDs []string) bool {
	if len(targetIDs) == 0 {
		return subscriptionNotifyTargetMatches(dialog, "")
	}
	for _, targetID := range targetIDs {
		if subscriptionNotifyTargetMatches(dialog, targetID) {
			return true
		}
	}
	return false
}

func subscriptionEventHeadersMatch(expected, actual string) bool {
	expectedValue, expectedID, expectedErr := parseSubscriptionEvent(expected)
	actualValue, actualID, actualErr := parseSubscriptionEvent(actual)
	if expectedErr != nil || actualErr != nil || expectedValue == "" || actualValue == "" {
		return false
	}
	expectedName, _, _ := strings.Cut(expectedValue, ";")
	actualName, _, _ := strings.Cut(actualValue, ";")
	return strings.TrimSpace(expectedName) == strings.TrimSpace(actualName) && expectedID == actualID
}

func supportedSubscriptionEventPackage(value string) bool {
	value = strings.TrimSpace(value)
	return value == "presence" || value == "Catalog"
}

func firstSingleHeaderValue(request *sip.Request, name string) string {
	if request == nil {
		return ""
	}
	headers := request.GetHeaders(name)
	if len(headers) != 1 {
		return ""
	}
	value := headers[0].String()
	if _, after, ok := strings.Cut(value, ":"); ok {
		value = after
	}
	return strings.TrimSpace(value)
}

func (g *GB28181API) renewTerminatedCascadeSubscription(key any, terminated *outgoingSubscriptionDialog, parent context.Context, delays ...time.Duration) {
	if g == nil {
		return
	}
	keyString, ok := key.(string)
	if !ok || keyString == "" {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	if len(delays) > 0 && delays[0] > 0 {
		timer := time.NewTimer(delays[0])
		defer timer.Stop()
		select {
		case <-parent.Done():
			return
		case <-timer.C:
		}
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	unlock, err := g.lockCascadeSubscriptionOperation(ctx, keyString)
	if err != nil {
		return
	}
	defer unlock()
	g.cascadeSubscriptionMu.Lock()
	state := g.cascadeSubscriptions[keyString]
	if state == nil || state.Refs <= 0 {
		g.cascadeSubscriptionMu.Unlock()
		return
	}
	input := state.Input
	identity := state.Identity.clone()
	localGatewayID := state.LocalGatewayID
	retryBlocked := state.RetryBlocked
	retryAt := state.RetryAt
	retryGeneration := state.RetryGeneration
	g.cascadeSubscriptionMu.Unlock()
	if retryBlocked || !retryAt.IsZero() && time.Now().Before(retryAt) {
		return
	}
	if current, loaded := g.outgoingSubscriptions.Load(keyString); loaded && current != terminated {
		return
	}
	ctx = withMonitorUserIdentityRoute(ctx, identity, localGatewayID)
	if err := g.invokeCascadeSubscribe(ctx, &input); err != nil {
		slog.Warn("renew terminated cascade subscription failed", "event", input.Event, "device_id", input.DeviceID, "target_id", input.TargetID, "err", err)
		return
	}
	g.cascadeSubscriptionMu.Lock()
	if g.cascadeSubscriptions[keyString] == state && state.RetryGeneration == retryGeneration {
		state.RetryAt = time.Time{}
		state.RetryBlocked = false
	}
	g.cascadeSubscriptionMu.Unlock()
}

func shouldSendCascadeInitialCatalogNotify(cascade *cascadeWorker, cmdType string) bool {
	return cascade != nil && strings.EqualFold(strings.TrimSpace(cmdType), "Catalog") && cascade.protocolVersion().AtLeast(GBVersion11)
}

func cascadeSubscriptionTargetAllowed(platform cascadePlatform, cmdType, deviceID string) bool {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || deviceID == "*" || deviceID == platform.localID {
		return true
	}
	_, shared := platform.exposedChannelMap[deviceID]
	return shared
}

func (g *GB28181API) respondSubscribeOK(ctx *sip.Context, req subscribeEventRequest, eventValue string, expires int, cascade *cascadeWorker, version GBProtocolVersion) (*sip.Response, *sip.Address, error) {
	var body []byte
	if shouldIncludeSubscribeBusinessResponse(version, req.CmdType, eventValue) {
		body, _ = sip.XMLEncode(struct {
			XMLName  xml.Name `xml:"Response"`
			CmdType  string   `xml:"CmdType"`
			SN       int      `xml:"SN"`
			DeviceID string   `xml:"DeviceID"`
			Result   string   `xml:"Result"`
		}{
			CmdType: req.CmdType, SN: req.SN, DeviceID: req.DeviceID, Result: "OK",
		})
	}
	response := sip.NewResponseFromRequest("", ctx.Request, 200, "OK", body)
	response.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: eventValue})
	response.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: strconv.Itoa(expires)})
	if ctx.XGBVer != "" || cascade != nil {
		header := sip.XGBVer(version)
		response.AppendHeader(&header)
	}
	if len(body) > 0 {
		response.AppendHeader(&sip.ContentTypeXML)
	}
	var contact *sip.Address
	if cascade != nil {
		contact = cascade.contactAddress()
	} else if g != nil && g.svr != nil && g.svr.fromAddress.URI != nil {
		contact = g.svr.fromAddress.Clone()
	}
	if contact != nil && contact.URI != nil {
		response.AppendHeader(&sip.ContactHeader{DisplayName: contact.DisplayName, Address: contact.URI.Clone(), Params: contact.Params.Clone()})
	}
	if err := ctx.Tx.Respond(response); err != nil {
		return response, contact, err
	}
	return response, contact, nil
}

// shouldIncludeSubscribeBusinessResponse 保留旧版传统订阅要求的 MANSCDP 业务应答。
// Alarm 以及 presence Catalog 在 2011/2014/2016 使用业务应答；2014+ 域间 Catalog;id=num
// 流程和 2022 均返回空正文 SIP 200。
func shouldIncludeSubscribeBusinessResponse(version GBProtocolVersion, cmdType, eventValue string) bool {
	cmdType = strings.TrimSpace(cmdType)
	if strings.EqualFold(cmdType, "Alarm") {
		return version != GBVersion30
	}
	return version != GBVersion30 && strings.EqualFold(cmdType, "Catalog") &&
		strings.EqualFold(strings.TrimSpace(eventValue), "presence")
}

// respondEventNotifyOK 按版本返回 9.11 事件/目录通知确认。
// 2011/2014/2016 Alarm 和传统 presence Catalog 携带业务 XML；其余版本/事件返回空 200 OK。
func respondEventNotifyOK(ctx *sip.Context, version GBProtocolVersion, cmdType string, sn int, deviceID string) error {
	if ctx == nil || ctx.Request == nil || ctx.Tx == nil {
		return fmt.Errorf("NOTIFY response context is unavailable")
	}
	var body []byte
	if shouldIncludeSubscribeBusinessResponse(version, cmdType, ctx.GetHeader("Event")) {
		body, _ = sip.XMLEncode(subscribeBusinessResponse{
			CmdType: strings.TrimSpace(cmdType), SN: sn, DeviceID: strings.TrimSpace(deviceID), Result: "OK",
		})
	}
	response := sip.NewResponseFromRequest("", ctx.Request, 200, "OK", body)
	if len(body) > 0 {
		response.AppendHeader(&sip.ContentTypeXML)
	}
	return ctx.Tx.Respond(response)
}

func normalizeSubscribeCmdType(value string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(value))
	key = strings.NewReplacer("_", "", "-", "", " ", "").Replace(key)
	switch key {
	case "alarm":
		return "Alarm", true
	case "catalog":
		return "Catalog", true
	case "mobileposition", "deviceposition":
		return "MobilePosition", true
	case "ptzposition":
		return "PTZPosition", true
	default:
		return "", false
	}
}

func buildSubscriptionEventValue(cmdType, eventID string) string {
	cmdType = strings.TrimSpace(cmdType)
	eventID = strings.TrimSpace(eventID)
	if strings.EqualFold(cmdType, "Catalog") && eventID != "" {
		return "Catalog;id=" + eventID
	}
	return cmdType
}

// buildSubscriptionEventValueForVersion 区分基础事件订阅与 2014 增加的域间目录订阅 Event id 参数。
func buildSubscriptionEventValueForVersion(version GBProtocolVersion, cmdType, eventID string) string {
	parsed, ok := ParseGBProtocolVersion(string(version))
	if !ok {
		parsed = GBVersion10
	}
	if parsed != GBVersion10 && strings.EqualFold(strings.TrimSpace(cmdType), "Catalog") {
		return buildSubscriptionEventValue(cmdType, eventID)
	}
	return "presence"
}

func parseSubscriptionEvent(value string) (eventValue, eventID string, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", nil
	}
	parts := strings.Split(value, ";")
	eventName := strings.TrimSpace(parts[0])
	if eventName == "" {
		return "", "", fmt.Errorf("invalid event header")
	}
	for _, part := range parts[1:] {
		keyValue := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(keyValue) != 2 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(keyValue[0]), "id") {
			eventID = strings.Trim(strings.TrimSpace(keyValue[1]), `"`)
		}
	}
	return buildSubscriptionEventValue(eventName, eventID), eventID, nil
}

func validateSubscriptionEventHeader(eventValue, cmdType, eventID string) error {
	eventName, _, _ := strings.Cut(strings.TrimSpace(eventValue), ";")
	if !strings.EqualFold(strings.TrimSpace(eventName), "presence") && !strings.EqualFold(strings.TrimSpace(eventName), strings.TrimSpace(cmdType)) {
		return fmt.Errorf("event header does not match subscribe cmd_type")
	}
	if strings.TrimSpace(eventID) != "" && !strings.EqualFold(strings.TrimSpace(cmdType), "Catalog") {
		return fmt.Errorf("event id is only valid for Catalog subscriptions")
	}
	return nil
}

func validateInterdomainCatalogEventHeader(eventValue string) error {
	parts := strings.Split(strings.TrimSpace(eventValue), ";")
	if len(parts) != 2 || !strings.EqualFold(strings.TrimSpace(parts[0]), "Catalog") {
		return fmt.Errorf("interdomain Catalog Event must use Catalog;id=num")
	}
	parameter := strings.SplitN(strings.TrimSpace(parts[1]), "=", 2)
	if len(parameter) != 2 || !strings.EqualFold(strings.TrimSpace(parameter[0]), "id") {
		return fmt.Errorf("interdomain Catalog Event must use Catalog;id=num")
	}
	eventID := strings.TrimSpace(parameter[1])
	if !allDecimalDigits(eventID) {
		return fmt.Errorf("interdomain Catalog Event id must be numeric")
	}
	return nil
}

func parseSubscribeExpires(value string) (int, error) {
	if strings.TrimSpace(value) == "" {
		return defaultSubscribeExpires, nil
	}
	// SIP Expires uses delta-seconds and the shared SIP parser represents it as
	// uint32. Keep the inbound SUBSCRIBE path on the same range; accepting a
	// larger value on 64-bit hosts would overflow time.Duration when calculating
	// ExpiresAt and could turn a long-lived subscription into an already-expired
	// one.
	raw := strings.TrimSpace(value)
	parsed, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || parsed > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("invalid expires header")
	}
	return int(parsed), nil
}

func parseSubscribeExpiresForProfile(value, cmdType string, cascade *cascadeWorker) (int, error) {
	if strings.TrimSpace(value) == "" && cascade != nil && strings.EqualFold(strings.TrimSpace(cmdType), "Catalog") && cascade.protocolVersion().AtLeast(GBVersion11) {
		return defaultCascadeCatalogSubscribeExpires, nil
	}
	return parseSubscribeExpires(value)
}

func subscriptionOwnerKey(ctx *sip.Context, cascade *cascadeWorker) string {
	if cascade != nil {
		// 上级名称在同一 CascadeManager 内唯一，且不会随 301/302 重定向地址变化。
		return "cascade:" + strings.TrimSpace(cascade.platform.name)
	}
	if ctx == nil {
		return ""
	}
	return "device:" + strings.TrimSpace(ctx.DeviceID)
}

func buildEventSubscriptionKey(owner, dialogID, fromTag, cmdType, deviceID string) string {
	parts := []string{
		strings.TrimSpace(owner),
		strings.TrimSpace(dialogID),
		strings.TrimSpace(fromTag),
		strings.ToUpper(strings.TrimSpace(cmdType)),
		strings.TrimSpace(deviceID),
	}
	var key strings.Builder
	for _, part := range parts {
		_, _ = fmt.Fprintf(&key, "%d:%s", len(part), part)
	}
	return key.String()
}

type inboundSubscribeDialog struct {
	callID     string
	fromTag    string
	toTag      string
	remoteCSeq uint32
}

func parseSubscribeDialog(ctx *sip.Context) (inboundSubscribeDialog, error) {
	if ctx == nil || ctx.Request == nil {
		return inboundSubscribeDialog{}, fmt.Errorf("invalid SUBSCRIBE dialog")
	}
	callID, ok := ctx.Request.CallID()
	if !ok || callID == nil || normalizeCallID(callID) == "" {
		return inboundSubscribeDialog{}, fmt.Errorf("missing SUBSCRIBE Call-ID")
	}
	fromTag := sipRequestFromTag(ctx.Request)
	if fromTag == "" {
		return inboundSubscribeDialog{}, fmt.Errorf("missing SUBSCRIBE From tag")
	}
	cseq, ok := ctx.Request.CSeq()
	if !ok || cseq == nil || !strings.EqualFold(cseq.MethodName, sip.MethodSubscribe) {
		return inboundSubscribeDialog{}, fmt.Errorf("invalid SUBSCRIBE CSeq")
	}
	return inboundSubscribeDialog{
		callID: normalizeCallID(callID), fromTag: fromTag,
		toTag: sipRequestToTag(ctx.Request), remoteCSeq: cseq.SeqNo,
	}, nil
}

func validateInboundSubscribeDialog(existing *eventSubscription, incoming inboundSubscribeDialog) error {
	if incoming.callID == "" || incoming.fromTag == "" || incoming.remoteCSeq == 0 {
		return fmt.Errorf("invalid SUBSCRIBE dialog")
	}
	if existing == nil {
		if incoming.toTag != "" {
			return fmt.Errorf("subscription dialog does not exist")
		}
		return nil
	}
	existing.mu.Lock()
	callID := existing.DialogCallID
	remoteTag := existing.RemoteTag
	localTag := existing.LocalTag
	remoteCSeq := existing.RemoteCSeq
	existing.mu.Unlock()
	if callID == "" || remoteTag == "" || localTag == "" || remoteCSeq == 0 {
		return fmt.Errorf("subscription dialog is incomplete")
	}
	if incoming.callID != callID || incoming.fromTag != remoteTag || incoming.toTag != localTag {
		return fmt.Errorf("subscription dialog does not match")
	}
	if incoming.remoteCSeq <= remoteCSeq {
		return fmt.Errorf("SUBSCRIBE CSeq is not increasing")
	}
	return nil
}

// refreshInboundSubscriptionDialog 保留初始请求建立的路由集，只刷新对端 Contact 与当前传输路径。
func refreshInboundSubscriptionDialog(initial, current *sip.Request) *sip.Request {
	if current == nil {
		return nil
	}
	if initial == nil {
		clone, _ := current.Clone().(*sip.Request)
		return clone
	}
	clone, _ := initial.Clone().(*sip.Request)
	if clone == nil {
		return nil
	}
	for _, name := range []string{"From", "To", "Call-ID", "CSeq"} {
		clone.RemoveHeader(name)
		sip.CopyHeaders(name, current, clone)
	}
	if contact, ok := current.Contact(); ok && contact != nil && contact.Address != nil {
		clone.RemoveHeader("Contact")
		clone.AppendHeader(contact.Clone())
	}
	clone.SetSource(current.Source())
	clone.SetDestination(current.Destination())
	clone.SetConnection(current.GetConnection())
	return clone
}

// publishEventNotify 向匹配订阅方发送 NOTIFY。
func (g *GB28181API) publishEventNotify(cmdType, deviceID string, body []byte) {
	g.publishEventNotifyForTarget(cmdType, deviceID, deviceID, body)
}

func (g *GB28181API) publishEventNotifyForTarget(cmdType, deviceID, eventTargetID string, body []byte) {
	g.publishEventNotifyForTargets(cmdType, deviceID, []string{eventTargetID}, body)
}

func (g *GB28181API) publishEventNotifyForTargets(cmdType, deviceID string, eventTargetIDs []string, body []byte) {
	cmdType = strings.TrimSpace(cmdType)
	deviceID = strings.TrimSpace(deviceID)
	if cmdType == "" || len(body) == 0 {
		return
	}
	eventTargetIDs = uniqueEventTargetIDs(eventTargetIDs)

	now := time.Now()
	g.eventSubscribers.Range(func(key, value any) bool {
		sub, ok := value.(*eventSubscription)
		if !ok || sub == nil {
			g.eventSubscribers.CompareAndDelete(key, value)
			return true
		}
		sub.mu.Lock()
		expiresAt := sub.ExpiresAt
		cascade := sub.Cascade
		subCmdType := sub.CmdType
		subDeviceID := sub.DeviceID
		filter := sub.Filter
		dialogCSeq := sub.RemoteCSeq
		subGBVersion := sub.GBVersion
		sub.mu.Unlock()
		if subscriptionExpiredAt(now, expiresAt) {
			// 统一由清理器删除并释放下级订阅，避免与并发续订互相覆盖。
			return true
		}
		if strings.EqualFold(cmdType, "Alarm") && !alarmMatchesSubscription(filter, body) {
			return true
		}
		if cascade != nil {
			if !strings.EqualFold(subCmdType, cmdType) {
				return true
			}
			if strings.EqualFold(cmdType, "Catalog") {
				// 目录变化由 sendCascadeCatalogNotify 按共享列表重新构造完整上级视图。
				return true
			}
			if !strings.EqualFold(cmdType, "Alarm") && !strings.EqualFold(cmdType, "MobilePosition") && !strings.EqualFold(cmdType, "PTZPosition") {
				return true
			}
			if strings.EqualFold(cmdType, "MobilePosition") {
				localHint := firstEventTargetID(eventTargetIDs)
				if localHint == "" {
					localHint = deviceID
				}
				outputs, err := rewriteCascadeMobilePositionForVersion(cascade.platform, body, localHint, cascade.protocolVersion(), subDeviceID)
				if err != nil {
					slog.Warn("rewrite cascade MobilePosition version failed", "deviceID", deviceID, "err", err)
					return true
				}
				payloads := make([][]byte, 0, len(outputs))
				for _, output := range outputs {
					payloads = append(payloads, output.body)
				}
				g.startEventNotifyTask(key, sub, cascade, dialogCSeq, cmdType, deviceID, payloads)
				return true
			}
			cascadeSourceBody := body
			if strings.EqualFold(cmdType, "Alarm") {
				var err error
				cascadeSourceBody, err = rewriteCascadeAlarmInfoForVersion(body, cascade.protocolVersion())
				if err != nil {
					slog.Warn("rewrite cascade Alarm version failed", "deviceID", deviceID, "err", err)
					return true
				}
			}
			cascadeBody, exposedID, err := rewriteCascadeEventBodyForDevice(cascade.platform, cascadeSourceBody, deviceID)
			if err != nil {
				slog.Warn("rewrite cascade event notify failed", "cmdType", cmdType, "deviceID", deviceID, "err", err)
				return true
			}
			if exposedID == "" {
				return true
			}
			if subDeviceID != "" && subDeviceID != "*" && subDeviceID != cascade.platform.localID && subDeviceID != exposedID {
				return true
			}
			g.startEventNotifyTask(key, sub, cascade, dialogCSeq, cmdType, deviceID, [][]byte{cascadeBody})
			return true
		}
		if !strings.EqualFold(subCmdType, cmdType) {
			return true
		}
		if !eventSubscriptionMatchesTargets(subDeviceID, deviceID, eventTargetIDs) {
			return true
		}
		payloads := [][]byte{body}
		if strings.EqualFold(cmdType, "MobilePosition") {
			targetVersion, valid := ParseGBProtocolVersion(subGBVersion)
			if !valid {
				slog.Warn("invalid MobilePosition subscription version", "deviceID", deviceID, "target_id", subDeviceID, "version", subGBVersion)
				return true
			}
			var err error
			payloads, err = rewriteLocalMobilePositionForSubscription(body, deviceID, eventTargetIDs, targetVersion, subDeviceID)
			if err != nil {
				slog.Warn("rewrite local MobilePosition version failed", "deviceID", deviceID, "target_id", subDeviceID, "err", err)
				return true
			}
			if len(payloads) == 0 {
				return true
			}
		}
		g.startEventNotifyTask(key, sub, nil, dialogCSeq, cmdType, deviceID, payloads)
		return true
	})
}

func uniqueEventTargetIDs(targets []string) []string {
	result := make([]string, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if _, exists := seen[target]; exists {
			continue
		}
		seen[target] = struct{}{}
		result = append(result, target)
	}
	return result
}

func firstEventTargetID(targets []string) string {
	if len(targets) == 0 {
		return ""
	}
	return targets[0]
}

func eventSubscriptionMatchesTargets(subscriptionTarget, deviceID string, eventTargetIDs []string) bool {
	subscriptionTarget = strings.TrimSpace(subscriptionTarget)
	if subscriptionTarget == "" || subscriptionTarget == "*" || subscriptionTarget == strings.TrimSpace(deviceID) {
		return true
	}
	for _, targetID := range eventTargetIDs {
		if subscriptionTarget == targetID {
			return true
		}
	}
	return false
}

// rewriteLocalMobilePositionForSubscription 复用级联的版本转换和目标过滤规则，
// 但使用恒等编码映射，保证本地订阅方只能收到其订阅目标对应的位置项。
func rewriteLocalMobilePositionForSubscription(body []byte, deviceID string, eventTargetIDs []string, targetVersion GBProtocolVersion, targetID string) ([][]byte, error) {
	deviceID = strings.TrimSpace(deviceID)
	identity := cascadePlatform{
		localID:           deviceID,
		channelIDMap:      make(map[string]string, len(eventTargetIDs)+1),
		exposedChannelMap: make(map[string]string, len(eventTargetIDs)+1),
	}
	addIdentity := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		identity.channelIDMap[id] = id
		if id != deviceID {
			identity.exposedChannelMap[id] = id
		}
	}
	addIdentity(deviceID)
	for _, eventTargetID := range eventTargetIDs {
		addIdentity(eventTargetID)
	}
	localHint := firstEventTargetID(eventTargetIDs)
	if localHint == "" {
		localHint = deviceID
	}
	outputs, err := rewriteCascadeMobilePositionForVersion(identity, body, localHint, targetVersion, strings.TrimSpace(targetID))
	if err != nil {
		return nil, err
	}
	payloads := make([][]byte, 0, len(outputs))
	for _, output := range outputs {
		payloads = append(payloads, output.body)
	}
	return payloads, nil
}

// startEventNotifyTask 将同一订阅的事件放入 FIFO 队列，每个订阅最多运行一个发送 worker。
func (g *GB28181API) startEventNotifyTask(key any, sub *eventSubscription, cascade *cascadeWorker, dialogCSeq uint32, cmdType, deviceID string, bodies [][]byte) {
	if g == nil || sub == nil || len(bodies) == 0 {
		return
	}
	sub.mu.Lock()
	currentCascade := sub.Cascade
	currentDialogCSeq := sub.RemoteCSeq
	sub.mu.Unlock()
	if currentCascade != cascade || currentDialogCSeq != dialogCSeq {
		return
	}
	batch := eventNotifyBatch{
		key: key, cascade: cascade, dialogCSeq: dialogCSeq, cmdType: cmdType, deviceID: deviceID,
		payloads: make([][]byte, 0, len(bodies)),
	}
	for _, body := range bodies {
		if len(body) == 0 {
			continue
		}
		if len(body) > eventNotifyQueueMaxBytes-batch.byteCount {
			g.overloadEventNotifySubscription(key, sub, dialogCSeq, cmdType, deviceID, batch.byteCount+len(body))
			return
		}
		batch.payloads = append(batch.payloads, append([]byte(nil), body...))
		batch.byteCount += len(body)
	}
	if len(batch.payloads) == 0 {
		return
	}

	sub.notifyDispatchMu.Lock()
	if sub.notifyOverloaded {
		sub.notifyDispatchMu.Unlock()
		return
	}
	if len(sub.notifyQueue) >= eventNotifyQueueMaxBatches || batch.byteCount > eventNotifyQueueMaxBytes-sub.notifyQueueBytes {
		queuedBytes := sub.notifyQueueBytes + batch.byteCount
		sub.notifyOverloaded = true
		sub.notifyOverloadDialogCSeq = dialogCSeq
		sub.notifyQueue = nil
		sub.notifyQueueBytes = 0
		sub.notifyDispatchMu.Unlock()
		g.scheduleOverloadedEventSubscriptionRemoval(key, sub, dialogCSeq, cmdType, deviceID, queuedBytes)
		return
	}
	sub.notifyQueue = append(sub.notifyQueue, batch)
	sub.notifyQueueBytes += batch.byteCount
	if sub.notifyWorker {
		sub.notifyDispatchMu.Unlock()
		return
	}
	sub.notifyWorker = true
	sub.notifyDispatchMu.Unlock()

	if g.startLifecycleTask(context.Background(), func(taskCtx context.Context) {
		g.runEventNotifyWorker(taskCtx, sub)
	}) {
		return
	}
	sub.notifyDispatchMu.Lock()
	sub.notifyQueue = nil
	sub.notifyQueueBytes = 0
	sub.notifyWorker = false
	sub.notifyDispatchMu.Unlock()
}

func (g *GB28181API) runEventNotifyWorker(taskCtx context.Context, sub *eventSubscription) {
	for {
		sub.notifyDispatchMu.Lock()
		if taskCtx.Err() != nil || sub.notifyOverloaded || len(sub.notifyQueue) == 0 {
			if taskCtx.Err() != nil || sub.notifyOverloaded {
				sub.notifyQueue = nil
				sub.notifyQueueBytes = 0
			}
			sub.notifyWorker = false
			sub.notifyDispatchMu.Unlock()
			return
		}
		batch := sub.notifyQueue[0]
		sub.notifyQueue[0] = eventNotifyBatch{}
		sub.notifyQueue = sub.notifyQueue[1:]
		sub.notifyQueueBytes -= batch.byteCount
		sub.notifyDispatchMu.Unlock()

		for {
			result, response := g.sendEventNotifyBatch(taskCtx, sub, &batch)
			if result == eventNotifyBatchRetry && g.waitEventNotifyRetry(taskCtx, sub, batch, response) {
				continue
			}
			if result == eventNotifyBatchDone {
				break
			}
			sub.notifyDispatchMu.Lock()
			sub.notifyQueue = nil
			sub.notifyQueueBytes = 0
			sub.notifyWorker = false
			sub.notifyDispatchMu.Unlock()
			return
		}
	}
}

type eventNotifyBatchResult uint8

const (
	eventNotifyBatchDone eventNotifyBatchResult = iota
	eventNotifyBatchRetry
	eventNotifyBatchStop
)

func (g *GB28181API) sendEventNotifyBatch(taskCtx context.Context, sub *eventSubscription, batch *eventNotifyBatch) (eventNotifyBatchResult, *sip.Response) {
	if batch == nil {
		return eventNotifyBatchStop, nil
	}
	batchCtx := taskCtx
	stopWorkerContext := func() {}
	if batch.cascade != nil {
		if !g.cascadeWorkerAvailable(batch.cascade) {
			return eventNotifyBatchStop, nil
		}
		batchCtx, stopWorkerContext = withCascadeWorkerOperation(taskCtx, batch.cascade)
		if !g.cascadeWorkerAvailable(batch.cascade) {
			stopWorkerContext()
			return eventNotifyBatchStop, nil
		}
	}
	defer stopWorkerContext()
	notifyCtx, cancel := context.WithTimeout(batchCtx, eventNotifyTaskTimeout)
	defer cancel()
	for batch.nextPayload < len(batch.payloads) {
		payload := batch.payloads[batch.nextPayload]
		sent, response, err := g.sendCurrentEventNotifyAttemptContext(notifyCtx, batch.key, sub, batch.cascade, batch.dialogCSeq, batch.cmdType, payload)
		if !sent {
			return eventNotifyBatchStop, response
		}
		if err == nil {
			batch.nextPayload++
			batch.attempts = 0
			continue
		}
		if taskCtx.Err() != nil || errors.Is(err, ErrServiceStopped) ||
			!g.eventSubscriptionCurrentForDispatch(batch.key, sub, batch.cascade, batch.dialogCSeq, time.Now()) {
			return eventNotifyBatchStop, response
		}
		if !retryableEventNotifyFailure(response, err) {
			slog.Warn("discard non-retryable event NOTIFY delivery", "cmd_type", batch.cmdType,
				"device_id", batch.deviceID, "err", err)
			batch.nextPayload++
			batch.attempts = 0
			continue
		}
		batch.attempts++
		if batch.attempts < eventNotifyMaxAttempts {
			slog.Warn("retry event NOTIFY delivery", "cmd_type", batch.cmdType, "device_id", batch.deviceID,
				"attempt", batch.attempts, "max_attempts", eventNotifyMaxAttempts, "err", err)
			return eventNotifyBatchRetry, response
		}
		g.detachEventSubscriptionAfterDeliveryExhausted(taskCtx, sub, batch.cascade, batch.dialogCSeq)
		slog.Error("event NOTIFY delivery exhausted", "cmd_type", batch.cmdType, "device_id", batch.deviceID,
			"attempts", batch.attempts, "err", err)
		return eventNotifyBatchStop, response
	}
	return eventNotifyBatchDone, nil
}

func (g *GB28181API) waitEventNotifyRetry(ctx context.Context, sub *eventSubscription, batch eventNotifyBatch, response *sip.Response) bool {
	if ctx == nil || ctx.Err() != nil || sub == nil {
		return false
	}
	delay := eventNotifyRetryDelay(batch.attempts, response)
	if g != nil && g.eventNotifyRetryWait != nil {
		delay = g.eventNotifyRetryWait(batch.attempts, response)
	}
	if delay < 0 {
		delay = 0
	}
	sub.mu.Lock()
	expiresAt := sub.ExpiresAt
	sub.mu.Unlock()
	if remaining := time.Until(expiresAt); remaining <= 0 {
		return false
	} else if delay > remaining {
		delay = remaining
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return g.eventSubscriptionCurrentForDispatch(batch.key, sub, batch.cascade, batch.dialogCSeq, time.Now())
	}
}

func (g *GB28181API) waitEventNotifyDispatchRetry(ctx context.Context, sub *eventSubscription, expectedCascade *cascadeWorker, expectedDialogCSeq uint32, attempt int, response *sip.Response) bool {
	if ctx == nil || ctx.Err() != nil || sub == nil {
		return false
	}
	delay := eventNotifyRetryDelay(attempt, response)
	if g != nil && g.eventNotifyRetryWait != nil {
		delay = g.eventNotifyRetryWait(attempt, response)
	}
	if delay < 0 {
		delay = 0
	}
	sub.mu.Lock()
	expiresAt := sub.ExpiresAt
	sub.mu.Unlock()
	if remaining := time.Until(expiresAt); remaining <= 0 {
		return false
	} else if delay > remaining {
		delay = remaining
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		sub.mu.Lock()
		current := sub.Cascade == expectedCascade && sub.RemoteCSeq == expectedDialogCSeq && !subscriptionExpiredAt(time.Now(), sub.ExpiresAt)
		sub.mu.Unlock()
		return current
	}
}

func eventNotifyRetryDelay(attempt int, response *sip.Response) time.Duration {
	if retryAfter, ok := eventNotifyRetryAfter(response); ok {
		return retryAfter
	}
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 3 {
		shift = 3
	}
	delay := eventNotifyRetryBaseDelay * time.Duration(1<<shift)
	if delay > eventNotifyRetryMaxDelay {
		return eventNotifyRetryMaxDelay
	}
	return delay
}

func eventNotifyRetryAfter(response *sip.Response) (time.Duration, bool) {
	if !hasValidRetryAfter(response) {
		return 0, false
	}
	header := response.GetHeaders("Retry-After")[0]
	value := header.String()
	if _, after, ok := strings.Cut(value, ":"); ok {
		value = after
	}
	value = strings.TrimSpace(value)
	end := 0
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	seconds, err := strconv.ParseInt(value[:end], 10, 64)
	if err != nil || seconds < 0 || seconds > int64((365*24*time.Hour)/time.Second) {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

func retryableEventNotifyFailure(response *sip.Response, err error) bool {
	if err == nil || errors.Is(err, ErrServiceStopped) || errors.Is(err, errStaleEventNotifyDispatch) {
		return false
	}
	if response == nil {
		return !errors.Is(err, context.Canceled)
	}
	if hasValidRetryAfter(response) {
		return true
	}
	switch response.StatusCode() {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func (g *GB28181API) detachEventSubscriptionAfterDeliveryExhausted(ctx context.Context, sub *eventSubscription, expectedCascade *cascadeWorker, expectedDialogCSeq uint32) {
	if g == nil || sub == nil {
		return
	}
	expectation := &eventNotifyDispatchExpectation{cascade: expectedCascade, dialogCSeq: expectedDialogCSeq}
	sub.notifyMu.Lock()
	downstreamKeys := g.detachEventSubscriptionAfterNotifyFailureContextExpected(ctx, sub, expectation)
	sub.notifyMu.Unlock()
	g.releaseCascadeDownstreamSubscriptions(ctx, downstreamKeys)
}

func (g *GB28181API) overloadEventNotifySubscription(key any, sub *eventSubscription, dialogCSeq uint32, cmdType, deviceID string, queuedBytes int) {
	sub.notifyDispatchMu.Lock()
	if sub.notifyOverloaded {
		sub.notifyDispatchMu.Unlock()
		return
	}
	sub.notifyOverloaded = true
	sub.notifyOverloadDialogCSeq = dialogCSeq
	sub.notifyQueue = nil
	sub.notifyQueueBytes = 0
	sub.notifyDispatchMu.Unlock()
	g.scheduleOverloadedEventSubscriptionRemoval(key, sub, dialogCSeq, cmdType, deviceID, queuedBytes)
}

func (g *GB28181API) scheduleOverloadedEventSubscriptionRemoval(key any, sub *eventSubscription, dialogCSeq uint32, cmdType, deviceID string, queuedBytes int) {
	slog.Warn("event subscription notify queue overflow", "cmdType", cmdType, "deviceID", deviceID,
		"max_batches", eventNotifyQueueMaxBatches, "max_bytes", eventNotifyQueueMaxBytes, "queued_bytes", queuedBytes)
	g.startLifecycleTask(context.Background(), func(taskCtx context.Context) {
		g.removeOverloadedEventSubscription(taskCtx, key, sub, dialogCSeq)
	})
}

func (g *GB28181API) removeOverloadedEventSubscription(ctx context.Context, key any, sub *eventSubscription, expectedDialogCSeq uint32) {
	keyText, ok := key.(string)
	if !ok || strings.TrimSpace(keyText) == "" || sub == nil {
		return
	}
	unlock, err := g.lockEventSubscriptionOperation(ctx, keyText)
	if err != nil {
		return
	}
	sub.mu.Lock()
	currentDialogCSeq := sub.RemoteCSeq
	sub.mu.Unlock()
	if currentDialogCSeq != expectedDialogCSeq {
		unlock()
		clearEventNotifyOverload(sub, expectedDialogCSeq)
		return
	}
	removed := g.eventSubscribers.CompareAndDelete(key, sub)
	var downstreamKeys []string
	if removed {
		sub.mu.Lock()
		sub.ExpiresAt = time.Now()
		sub.DialogRequest = nil
		sub.Response = nil
		downstreamKeys = append(downstreamKeys, sub.DownstreamKeys...)
		sub.DownstreamKeys = nil
		sub.mu.Unlock()
	}
	unlock()
	if removed {
		g.releaseCascadeDownstreamSubscriptions(ctx, downstreamKeys)
	}
}

func resetStaleEventNotifyOverload(sub *eventSubscription, currentDialogCSeq uint32) {
	if sub == nil {
		return
	}
	sub.notifyDispatchMu.Lock()
	if sub.notifyOverloaded && sub.notifyOverloadDialogCSeq != currentDialogCSeq {
		sub.notifyOverloaded = false
		sub.notifyOverloadDialogCSeq = 0
	}
	sub.notifyDispatchMu.Unlock()
}

func clearEventNotifyOverload(sub *eventSubscription, expectedDialogCSeq uint32) {
	if sub == nil {
		return
	}
	sub.notifyDispatchMu.Lock()
	if sub.notifyOverloaded && sub.notifyOverloadDialogCSeq == expectedDialogCSeq {
		sub.notifyOverloaded = false
		sub.notifyOverloadDialogCSeq = 0
	}
	sub.notifyDispatchMu.Unlock()
}

func (g *GB28181API) eventSubscriptionCurrent(key any, sub *eventSubscription, now time.Time) bool {
	if g == nil || sub == nil {
		return false
	}
	current, exists := g.eventSubscribers.Load(key)
	if !exists || current != sub {
		return false
	}
	sub.mu.Lock()
	expiresAt := sub.ExpiresAt
	sub.mu.Unlock()
	return !subscriptionExpiredAt(now, expiresAt)
}

func (g *GB28181API) eventSubscriptionCurrentForDispatch(key any, sub *eventSubscription, expectedCascade *cascadeWorker, expectedDialogCSeq uint32, now time.Time) bool {
	if g == nil || sub == nil {
		return false
	}
	current, exists := g.eventSubscribers.Load(key)
	if !exists || current != sub {
		return false
	}
	sub.mu.Lock()
	expiresAt := sub.ExpiresAt
	cascade := sub.Cascade
	dialogCSeq := sub.RemoteCSeq
	sub.mu.Unlock()
	return !subscriptionExpiredAt(now, expiresAt) && cascade == expectedCascade && dialogCSeq == expectedDialogCSeq
}

type cascadeMobilePositionOutput struct {
	body      []byte
	exposedID string
}

type cascadeMobilePositionSingleNotify struct {
	XMLName   xml.Name `xml:"Notify"`
	CmdType   string   `xml:"CmdType"`
	SN        int      `xml:"SN"`
	Time      string   `xml:"Time"`
	Longitude *float64 `xml:"Longitude"`
	Latitude  *float64 `xml:"Latitude"`
	Speed     *float64 `xml:"Speed,omitempty"`
	Direction *float64 `xml:"Direction,omitempty"`
	Altitude  *float64 `xml:"Altitude,omitempty"`
}

// rewriteCascadeMobilePositionForVersion 在 2016 单点结构与 2022 批量结构之间转换，且只保留显式共享通道。
func rewriteCascadeMobilePositionForVersion(platform cascadePlatform, body []byte, sourceDeviceID string, targetVersion GBProtocolVersion, targetID string) ([]cascadeMobilePositionOutput, error) {
	if targetVersion != GBVersion20 && targetVersion != GBVersion30 {
		return nil, fmt.Errorf("MobilePosition requires GB/T 28181-2016 or later")
	}
	var msg mobilePositionNotify
	if err := sip.XMLDecode(body, &msg); err != nil {
		return nil, fmt.Errorf("decode cascade MobilePosition: %w", err)
	}
	if msg.XMLName.Local != "Notify" || !strings.EqualFold(strings.TrimSpace(msg.CmdType), "MobilePosition") || msg.SN <= 0 || !validGBDateTime(msg.Time) {
		return nil, fmt.Errorf("invalid cascade MobilePosition envelope")
	}
	if len(msg.Info) > 0 || len(msg.ExtraInfo) > 0 || len(msg.ExtralInfo) > 0 {
		return nil, fmt.Errorf("MobilePosition does not define Info or ExtraInfo")
	}
	extended, err := inspectAppendixA4Payload(body)
	if err != nil {
		return nil, err
	}
	if extended {
		return nil, fmt.Errorf("MobilePosition does not support Appendix A.4 extensions")
	}

	targetID = strings.TrimSpace(targetID)
	localTargetID := platform.exposedChannelMap[targetID]
	specificTarget := targetID != "" && targetID != "*" && targetID != platform.localID
	if specificTarget && localTargetID == "" {
		return nil, nil
	}
	items, batch, err := cascadeMobilePositionItems(platform, &msg, strings.TrimSpace(sourceDeviceID), localTargetID)
	if err != nil {
		return nil, err
	}
	if targetVersion == GBVersion30 {
		for index := range items {
			if items[index].Direction != nil && *items[index].Direction == 360 {
				zero := float64(0)
				items[index].Direction = &zero
			}
		}
	}
	if len(items) == 0 {
		if targetVersion != GBVersion30 || !batch || specificTarget || msg.SumNum == nil || *msg.SumNum != 0 {
			return nil, nil
		}
	}

	if targetVersion == GBVersion20 {
		outputs := make([]cascadeMobilePositionOutput, 0, len(items))
		for _, item := range items {
			notify := cascadeMobilePositionSingleNotify{
				CmdType: "MobilePosition", SN: msg.SN, Time: strings.TrimSpace(item.CaptureTime),
				Longitude: item.Longitude, Latitude: item.Latitude, Speed: item.Speed, Direction: item.Direction, Altitude: item.Altitude,
			}
			encoded, encodeErr := sip.XMLEncode(notify)
			if encodeErr != nil {
				return nil, encodeErr
			}
			outputs = append(outputs, cascadeMobilePositionOutput{body: encoded, exposedID: item.DeviceID})
		}
		return outputs, nil
	}

	deviceID := targetID
	if deviceID == "" || deviceID == "*" {
		deviceID = platform.localID
	}
	if !isGBDeviceIdentifier(deviceID) {
		return nil, fmt.Errorf("invalid cascade MobilePosition target id")
	}
	notify := cascadeSystemMobilePositionNotify{
		CmdType: "MobilePosition", SN: msg.SN, DeviceID: deviceID, Time: strings.TrimSpace(msg.Time), SumNum: len(items),
	}
	notify.DeviceList.Num = len(items)
	notify.DeviceList.Item = append(notify.DeviceList.Item, items...)
	encoded, err := sip.XMLEncode(notify)
	if err != nil {
		return nil, err
	}
	exposedID := deviceID
	if len(items) == 1 {
		exposedID = items[0].DeviceID
	}
	return []cascadeMobilePositionOutput{{body: encoded, exposedID: exposedID}}, nil
}

func cascadeMobilePositionItems(platform cascadePlatform, msg *mobilePositionNotify, sourceDeviceID, localTargetID string) ([]mobilePositionItemXML, bool, error) {
	if msg == nil {
		return nil, false, fmt.Errorf("invalid cascade MobilePosition")
	}
	hasBatch := msg.SumNum != nil || msg.DeviceList.XMLName.Local != ""
	if hasBatch {
		if msg.Longitude != nil || msg.Latitude != nil || msg.Speed != nil || msg.Direction != nil || msg.Altitude != nil || msg.Height != nil {
			return nil, true, fmt.Errorf("batch MobilePosition does not support top-level position fields")
		}
		if msg.SumNum == nil || *msg.SumNum < 0 {
			return nil, true, fmt.Errorf("invalid MobilePosition SumNum")
		}
		if !isGBDeviceIdentifier(strings.TrimSpace(msg.DeviceID)) {
			return nil, true, fmt.Errorf("invalid MobilePosition device id")
		}
		if msg.DeviceList.XMLName.Local == "" {
			if *msg.SumNum == 0 {
				return nil, true, nil
			}
			return nil, true, fmt.Errorf("missing MobilePosition DeviceList")
		}
		if msg.DeviceList.Num != nil && (*msg.DeviceList.Num < 0 || *msg.DeviceList.Num != len(msg.DeviceList.Item)) {
			return nil, true, fmt.Errorf("invalid MobilePosition DeviceList count")
		}
		if len(msg.DeviceList.Item) != *msg.SumNum {
			return nil, true, fmt.Errorf("invalid MobilePosition DeviceList count")
		}
		items := make([]mobilePositionItemXML, 0, len(msg.DeviceList.Item))
		for _, item := range msg.DeviceList.Item {
			localID := strings.TrimSpace(item.DeviceID)
			position := &MobilePositionData{
				DeviceID: localID, CaptureTime: strings.TrimSpace(item.CaptureTime), Time: strings.TrimSpace(item.CaptureTime),
				Longitude: item.Longitude, Latitude: item.Latitude, Speed: item.Speed, Direction: item.Direction, Altitude: item.Altitude, Height: item.Height,
			}
			if !validGBDateTime(position.CaptureTime) {
				return nil, true, fmt.Errorf("invalid MobilePosition capture time")
			}
			if err := validateMobilePositionData(position, GBVersion30); err != nil {
				return nil, true, err
			}
			if localTargetID != "" && localID != localTargetID {
				continue
			}
			exposedID := platform.channelIDMap[localID]
			if exposedID == "" {
				continue
			}
			item.DeviceID = exposedID
			item.CaptureTime = position.CaptureTime
			items = append(items, item)
		}
		return items, true, nil
	}

	localID := ""
	if msg.Height != nil {
		return nil, false, fmt.Errorf("GB/T 28181-2016 MobilePosition does not define Height")
	}
	if candidate := strings.TrimSpace(msg.DeviceID); platform.channelIDMap[candidate] != "" {
		localID = candidate
	}
	if localTargetID != "" {
		if localID != "" && localID != localTargetID {
			return nil, false, nil
		}
		localID = localTargetID
	}
	if localID == "" && platform.channelIDMap[sourceDeviceID] != "" {
		localID = sourceDeviceID
	}
	if localID == "" {
		return nil, false, nil
	}
	position := &MobilePositionData{
		DeviceID: localID, CaptureTime: strings.TrimSpace(msg.Time), Time: strings.TrimSpace(msg.Time),
		Longitude: msg.Longitude, Latitude: msg.Latitude, Speed: msg.Speed, Direction: msg.Direction, Altitude: msg.Altitude,
	}
	if err := validateMobilePositionData(position, GBVersion20); err != nil {
		return nil, false, err
	}
	return []mobilePositionItemXML{{
		DeviceID: platform.channelIDMap[localID], CaptureTime: position.CaptureTime,
		Longitude: msg.Longitude, Latitude: msg.Latitude, Speed: msg.Speed, Direction: msg.Direction, Altitude: msg.Altitude,
	}}, false, nil
}

type cascadeAlarmInfoKind uint8

const (
	cascadeAlarmInfoPlain cascadeAlarmInfoKind = iota
	cascadeAlarmInfoTyped
	cascadeAlarmInfoAppendixA4
	cascadeAlarmInfoTypedAppendixA4
)

// rewriteCascadeAlarmInfoForVersion 按上级协议版本转换报警扩展字段，基础报警字段保持原样。
func rewriteCascadeAlarmInfoForVersion(body []byte, targetVersion GBProtocolVersion) ([]byte, error) {
	if !targetVersion.Valid() {
		targetVersion = GBVersion10
	}
	var alarm messageAlarm
	if err := sip.XMLDecode(body, &alarm); err != nil {
		return nil, fmt.Errorf("decode cascade Alarm: %w", err)
	}
	if alarm.XMLName.Local != "Notify" || !strings.EqualFold(strings.TrimSpace(alarm.CmdType), "Alarm") {
		return nil, fmt.Errorf("invalid cascade Alarm envelope")
	}
	typedAlarmType, typedMethod, _, err := alarmTypedInfo(alarm.Info)
	if err != nil {
		return nil, err
	}
	if typedMethod != "" {
		return nil, fmt.Errorf("cascade Alarm Info.AlarmMethod is not standard")
	}
	method := strings.TrimSpace(alarm.AlarmMethod)
	if strings.TrimSpace(alarm.AlarmType) != "" {
		return nil, fmt.Errorf("cascade Alarm top-level AlarmType is not standard")
	}
	alarmType := typedAlarmType
	dropTypedInfo := !targetVersion.AtLeast(GBVersion20)
	if !dropTypedInfo {
		if targetErr := validateAlarmTypeForMethod(targetVersion, method, alarmType); targetErr != nil {
			if targetVersion != GBVersion20 || validateAlarmTypeForMethod(GBVersion30, method, alarmType) != nil {
				return nil, targetErr
			}
			// 例如 2022 扩展的 AlarmMethod=5/AlarmType=13 不能发送给 2016。
			dropTypedInfo = true
		}
	}
	decoder := sip.NewGBXMLDecoder(body)
	var output bytes.Buffer
	encoder := xml.NewEncoder(&output)
	depth := 0
	for {
		token, decodeErr := decoder.Token()
		if decodeErr == io.EOF {
			break
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("decode cascade Alarm XML: %w", decodeErr)
		}
		start, isStart := token.(xml.StartElement)
		if isStart && depth == 1 {
			name := start.Name.Local
			if strings.EqualFold(name, "Info") || strings.EqualFold(name, "ExtraInfo") || strings.EqualFold(name, "AlarmType") {
				tokens, readErr := readCascadeXMLElement(decoder, start)
				if readErr != nil {
					return nil, readErr
				}
				keep := true
				switch {
				case strings.EqualFold(name, "AlarmType"):
					keep = targetVersion.AtLeast(GBVersion20) && !dropTypedInfo
				case strings.EqualFold(name, "ExtraInfo"):
					kind, classifyErr := classifyCascadeAlarmInfo(tokens)
					if classifyErr != nil {
						return nil, classifyErr
					}
					if kind != cascadeAlarmInfoPlain {
						return nil, fmt.Errorf("cascade Alarm ExtraInfo must be plain text")
					}
					if targetVersion != GBVersion30 {
						// 2011/2014 使用纯文本 Info；2016 允许结构化 Info 后继续携带纯文本 Info。
						renameCascadeXMLElement(tokens, "Info")
					}
				default:
					kind, classifyErr := classifyCascadeAlarmInfo(tokens)
					if classifyErr != nil {
						return nil, classifyErr
					}
					switch kind {
					case cascadeAlarmInfoPlain:
						if targetVersion == GBVersion30 {
							renameCascadeXMLElement(tokens, "ExtraInfo")
						}
					case cascadeAlarmInfoTyped:
						keep = !dropTypedInfo
					case cascadeAlarmInfoAppendixA4:
						keep = targetVersion == GBVersion30
					case cascadeAlarmInfoTypedAppendixA4:
						switch {
						case targetVersion == GBVersion30:
							keep = !dropTypedInfo
						case targetVersion == GBVersion20 && !dropTypedInfo:
							tokens, classifyErr = cascadeAlarmInfoTokensFor2016(tokens)
							if classifyErr != nil {
								return nil, classifyErr
							}
						default:
							keep = false
						}
					}
				}
				if keep {
					for _, elementToken := range tokens {
						if encodeErr := encoder.EncodeToken(elementToken); encodeErr != nil {
							return nil, fmt.Errorf("encode cascade Alarm XML: %w", encodeErr)
						}
					}
				}
				continue
			}
		}
		if isStart {
			depth++
		} else if _, isEnd := token.(xml.EndElement); isEnd {
			depth--
		}
		if err := encoder.EncodeToken(token); err != nil {
			return nil, fmt.Errorf("encode cascade Alarm XML: %w", err)
		}
	}
	if err := encoder.Flush(); err != nil {
		return nil, fmt.Errorf("flush cascade Alarm XML: %w", err)
	}
	var converted messageAlarm
	if err := sip.XMLDecode(output.Bytes(), &converted); err != nil {
		return nil, fmt.Errorf("decode converted cascade Alarm: %w", err)
	}
	if err := validateAlarmInfoVersion(targetVersion, converted.Info, converted.ExtraInfo); err != nil {
		return nil, err
	}
	encoded, err := sip.EncodeGBXMLDocument(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("encode cascade Alarm XML as GB2312: %w", err)
	}
	return encoded, nil
}

func readCascadeXMLElement(decoder *xml.Decoder, start xml.StartElement) ([]xml.Token, error) {
	tokens := []xml.Token{xml.CopyToken(start)}
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("decode cascade Alarm element %s: %w", start.Name.Local, err)
		}
		tokens = append(tokens, xml.CopyToken(token))
		switch token.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return tokens, nil
}

func classifyCascadeAlarmInfo(tokens []xml.Token) (cascadeAlarmInfoKind, error) {
	var encoded bytes.Buffer
	encoder := xml.NewEncoder(&encoded)
	for _, token := range tokens {
		if err := encoder.EncodeToken(token); err != nil {
			return 0, fmt.Errorf("encode cascade Alarm Info: %w", err)
		}
	}
	if err := encoder.Flush(); err != nil {
		return 0, fmt.Errorf("flush cascade Alarm Info: %w", err)
	}
	var info alarmInfoXML
	if err := sip.XMLDecode(encoded.Bytes(), &info); err != nil {
		return 0, fmt.Errorf("decode cascade Alarm Info: %w", err)
	}
	typed := strings.TrimSpace(info.AlarmType) != "" || strings.TrimSpace(info.AlarmMethod) != "" || info.AlarmTypeParam.EventType != nil
	appendixA4 := len(info.Children) > 0
	if appendixA4 && (strings.TrimSpace(info.Content) != "" || !containsAppendixA4Object(info.Children)) {
		return 0, fmt.Errorf("cascade Alarm contains unknown structured Info")
	}
	if typed && appendixA4 {
		return cascadeAlarmInfoTypedAppendixA4, nil
	}
	if typed {
		return cascadeAlarmInfoTyped, nil
	}
	if !appendixA4 {
		return cascadeAlarmInfoPlain, nil
	}
	return cascadeAlarmInfoAppendixA4, nil
}

func cascadeAlarmInfoTokensFor2016(tokens []xml.Token) ([]xml.Token, error) {
	if len(tokens) < 2 {
		return nil, fmt.Errorf("invalid cascade Alarm Info")
	}
	output := []xml.Token{xml.CopyToken(tokens[0])}
	for index := 1; index < len(tokens)-1; {
		start, ok := tokens[index].(xml.StartElement)
		if !ok {
			index++
			continue
		}
		depth := 1
		end := index + 1
		for ; end < len(tokens); end++ {
			switch tokens[end].(type) {
			case xml.StartElement:
				depth++
			case xml.EndElement:
				depth--
				if depth == 0 {
					end++
					goto elementComplete
				}
			}
		}
		return nil, fmt.Errorf("unterminated cascade Alarm Info child %s", start.Name.Local)

	elementComplete:
		if start.Name.Local == "AlarmType" || start.Name.Local == "AlarmTypeParam" {
			for _, token := range tokens[index:end] {
				output = append(output, xml.CopyToken(token))
			}
		}
		index = end
	}
	output = append(output, xml.CopyToken(tokens[len(tokens)-1]))
	return output, nil
}

func renameCascadeXMLElement(tokens []xml.Token, name string) {
	if len(tokens) < 2 {
		return
	}
	if start, ok := tokens[0].(xml.StartElement); ok {
		start.Name.Local = name
		tokens[0] = start
	}
	if end, ok := tokens[len(tokens)-1].(xml.EndElement); ok {
		end.Name.Local = name
		tokens[len(tokens)-1] = end
	}
}

// rewriteCascadeEventBody 只转发显式共享通道的事件，并返回该上级可见的事件源编码。
func rewriteCascadeEventBody(platform cascadePlatform, body []byte) ([]byte, string, error) {
	return rewriteCascadeEventBodyForDevice(platform, body, "")
}

func rewriteCascadeEventBodyForDevice(platform cascadePlatform, body []byte, sourceDeviceID string) ([]byte, string, error) {
	var envelope struct {
		DeviceID string `xml:"DeviceID"`
	}
	if err := sip.XMLDecode(body, &envelope); err != nil {
		return nil, "", fmt.Errorf("decode cascade event envelope: %w", err)
	}
	localDeviceID := strings.TrimSpace(envelope.DeviceID)
	exposedDeviceID := platform.channelIDMap[localDeviceID]
	if exposedDeviceID == "" {
		if localDeviceID != platform.localID {
			return nil, "", nil
		}
		exposedDeviceID = localDeviceID
	}
	mappingPlatform := withCascadeIdentifierMapping(platform, sourceDeviceID, platform.localID)
	decoder := sip.NewGBXMLDecoder(body)
	var output bytes.Buffer
	encoder := xml.NewEncoder(&output)
	stack := make([]xml.Name, 0, 8)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("decode cascade event XML: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			for index := range value.Attr {
				rewritten, rewriteErr := rewriteCascadeIdentifierValue(value.Attr[index].Value, value.Attr[index].Name.Local, mappingPlatform, localDeviceID, exposedDeviceID)
				if rewriteErr != nil {
					return nil, "", rewriteErr
				}
				value.Attr[index].Value = rewritten
			}
			token = value
			stack = append(stack, value.Name)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 0 {
				name := stack[len(stack)-1].Local
				original := string(value)
				var rewritten string
				if strings.EqualFold(name, "ExtraInfo") || strings.EqualFold(name, "ExtralInfo") {
					rewritten, err = rewriteCascadeOpaqueIdentifiers(original, name, mappingPlatform, localDeviceID, exposedDeviceID)
				} else {
					rewritten, err = rewriteCascadeIdentifierValue(original, name, mappingPlatform, localDeviceID, exposedDeviceID)
				}
				if err != nil {
					return nil, "", err
				}
				if len(stack) == 2 && strings.EqualFold(name, "DeviceID") {
					rewritten = exposedDeviceID
				}
				token = xml.CharData([]byte(rewritten))
			}
		}
		if err := encoder.EncodeToken(token); err != nil {
			return nil, "", fmt.Errorf("encode cascade event XML: %w", err)
		}
	}
	if err := encoder.Flush(); err != nil {
		return nil, "", fmt.Errorf("flush cascade event XML: %w", err)
	}
	encoded, err := sip.EncodeGBXMLDocument(output.Bytes())
	if err != nil {
		return nil, "", fmt.Errorf("encode cascade event XML as GB2312: %w", err)
	}
	return encoded, exposedDeviceID, nil
}

func (g *GB28181API) sendEventNotify(sub *eventSubscription, cmdType string, body []byte) error {
	return g.sendEventNotifyContext(context.Background(), sub, cmdType, body)
}

func (g *GB28181API) sendEventNotifyContext(ctx context.Context, sub *eventSubscription, cmdType string, body []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if sub == nil {
		return fmt.Errorf("subscription is unavailable")
	}
	sub.notifyMu.Lock()
	downstreamKeys, err := g.sendEventNotifyLockedContext(ctx, sub, cmdType, body)
	sub.notifyMu.Unlock()
	g.releaseCascadeDownstreamSubscriptions(ctx, downstreamKeys)
	return err
}

// sendCurrentEventNotifyContext 在取得单订阅发送锁后重新确认映射，避免退订或替换后的排队任务补发。
func (g *GB28181API) sendCurrentEventNotifyContext(ctx context.Context, key any, sub *eventSubscription, expectedCascade *cascadeWorker, expectedDialogCSeq uint32, cmdType string, body []byte) (bool, error) {
	sent, _, err := g.sendCurrentEventNotifyAttemptContext(ctx, key, sub, expectedCascade, expectedDialogCSeq, cmdType, body)
	return sent, err
}

func (g *GB28181API) sendCurrentEventNotifyAttemptContext(ctx context.Context, key any, sub *eventSubscription, expectedCascade *cascadeWorker, expectedDialogCSeq uint32, cmdType string, body []byte) (bool, *sip.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sub == nil {
		return false, nil, fmt.Errorf("subscription is unavailable")
	}
	sub.notifyMu.Lock()
	if ctx.Err() != nil || !g.eventSubscriptionCurrentForDispatch(key, sub, expectedCascade, expectedDialogCSeq, time.Now()) {
		err := ctx.Err()
		sub.notifyMu.Unlock()
		return false, nil, err
	}
	downstreamKeys, sent, response, err := g.sendEventNotifyLockedContextForDispatchAttempt(ctx, sub, expectedCascade, expectedDialogCSeq, cmdType, body)
	sub.notifyMu.Unlock()
	g.releaseCascadeDownstreamSubscriptions(ctx, downstreamKeys)
	return sent, response, err
}

// sendEventNotifyForDispatchContext 发送已绑定订阅代次的事件，不要求调用方持有发送锁。
func (g *GB28181API) sendEventNotifyForDispatchContext(ctx context.Context, sub *eventSubscription, expectedCascade *cascadeWorker, expectedDialogCSeq uint32, cmdType string, body []byte) (bool, error) {
	sent, _, err := g.sendEventNotifyForDispatchAttemptContext(ctx, sub, expectedCascade, expectedDialogCSeq, cmdType, body)
	return sent, err
}

func (g *GB28181API) sendEventNotifyForDispatchAttemptContext(ctx context.Context, sub *eventSubscription, expectedCascade *cascadeWorker, expectedDialogCSeq uint32, cmdType string, body []byte) (bool, *sip.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sub == nil {
		return false, nil, fmt.Errorf("subscription is unavailable")
	}
	sub.notifyMu.Lock()
	downstreamKeys, sent, response, err := g.sendEventNotifyLockedContextForDispatchAttempt(ctx, sub, expectedCascade, expectedDialogCSeq, cmdType, body)
	sub.notifyMu.Unlock()
	g.releaseCascadeDownstreamSubscriptions(ctx, downstreamKeys)
	return sent, response, err
}

// sendEventNotifyLockedContextForDispatch 在实际生成请求的状态快照内再次核对订阅代次，
// 关闭预检查后并发续订把旧批次套用到新对话上的窗口。
func (g *GB28181API) sendEventNotifyLockedContextForDispatch(ctx context.Context, sub *eventSubscription, expectedCascade *cascadeWorker, expectedDialogCSeq uint32, cmdType string, body []byte) ([]string, bool, error) {
	downstreamKeys, sent, _, err := g.sendEventNotifyLockedContextForDispatchAttempt(ctx, sub, expectedCascade, expectedDialogCSeq, cmdType, body)
	return downstreamKeys, sent, err
}

func (g *GB28181API) sendEventNotifyLockedContextForDispatchAttempt(ctx context.Context, sub *eventSubscription, expectedCascade *cascadeWorker, expectedDialogCSeq uint32, cmdType string, body []byte) ([]string, bool, *sip.Response, error) {
	expectation := &eventNotifyDispatchExpectation{cascade: expectedCascade, dialogCSeq: expectedDialogCSeq}
	var response *sip.Response
	var err error
	if expectedCascade != nil {
		response, err = g.sendCascadeEventNotifyRequestContextExpected(ctx, sub, cmdType, body, expectation)
	} else {
		response, err = g.sendDirectEventNotifyContextExpected(ctx, sub, cmdType, body, expectation)
	}
	if errors.Is(err, errStaleEventNotifyDispatch) {
		return nil, false, response, nil
	}
	var downstreamKeys []string
	if shouldRemoveEventSubscriptionAfterNotifyFailure(ctx, response, err, sub) && !retryableEventNotifyFailure(response, err) {
		downstreamKeys = g.detachEventSubscriptionAfterNotifyFailureContextExpected(ctx, sub, expectation)
	}
	return downstreamKeys, true, response, err
}

// sendEventNotifyLockedContext 必须在持有 sub.notifyMu 时调用。
func (g *GB28181API) sendEventNotifyLockedContext(ctx context.Context, sub *eventSubscription, cmdType string, body []byte) ([]string, error) {
	var expectation *eventNotifyDispatchExpectation
	var response *sip.Response
	var err error
	for {
		sub.mu.Lock()
		expectation = &eventNotifyDispatchExpectation{cascade: sub.Cascade, dialogCSeq: sub.RemoteCSeq}
		sub.mu.Unlock()
		if expectation.cascade != nil {
			response, err = g.sendCascadeEventNotifyRequestContextExpected(ctx, sub, cmdType, body, expectation)
		} else {
			response, err = g.sendDirectEventNotifyContextExpected(ctx, sub, cmdType, body, expectation)
		}
		if !errors.Is(err, errStaleEventNotifyDispatch) {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	var downstreamKeys []string
	if shouldRemoveEventSubscriptionAfterNotifyFailure(ctx, response, err, sub) {
		downstreamKeys = g.detachEventSubscriptionAfterNotifyFailureContextExpected(ctx, sub, expectation)
	}
	return downstreamKeys, err
}

func (g *GB28181API) sendDirectEventNotifyContext(ctx context.Context, sub *eventSubscription, cmdType string, body []byte) (*sip.Response, error) {
	return g.sendDirectEventNotifyContextExpected(ctx, sub, cmdType, body, nil)
}

func (g *GB28181API) sendDirectEventNotifyContextExpected(ctx context.Context, sub *eventSubscription, cmdType string, body []byte, expectation *eventNotifyDispatchExpectation) (*sip.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sub == nil {
		return nil, fmt.Errorf("subscription is unavailable")
	}
	sub.mu.Lock()
	if expectation != nil && (sub.Cascade != expectation.cascade || sub.RemoteCSeq != expectation.dialogCSeq) {
		sub.mu.Unlock()
		return nil, errStaleEventNotifyDispatch
	}
	if sub.DialogRequest == nil || sub.Response == nil || sub.To == nil || sub.To.URI == nil {
		sub.mu.Unlock()
		return nil, fmt.Errorf("subscription dialog is unavailable")
	}
	to := sub.To.Clone()
	source := sub.Source
	conn := sub.Conn
	gbVersion := sub.GBVersion
	expiresAt := sub.ExpiresAt
	event := sub.Event
	deviceID := sub.DeviceID
	ownerDeviceID := sub.OwnerDeviceID
	dialogRequestSource := sub.DialogRequest
	dialogRequest, cloneErr := cloneEventSubscriptionDialogRequest(dialogRequestSource)
	if cloneErr != nil {
		sub.mu.Unlock()
		return nil, cloneErr
	}
	dialogResponse := sub.Response
	dialogCascade := sub.Cascade
	dialogCSeq := sub.RemoteCSeq
	previousCSeq := sub.CSeq
	cseq, cseqErr := nextEventSubscriptionCSeqLocked(sub)
	if cseqErr != nil {
		sub.mu.Unlock()
		return nil, cseqErr
	}
	sub.mu.Unlock()

	dialogNotify, err := sip.NewRequestFromServerDialogChecked(sip.MethodNotify, dialogRequest, dialogResponse, cseq)
	if err != nil {
		return nil, err
	}
	if to == nil || to.URI == nil || strings.TrimSpace(to.URI.Host()) == "" {
		return nil, fmt.Errorf("subscription target URI is unavailable")
	}
	if source == nil || conn == nil {
		return nil, fmt.Errorf("subscription target transport is unavailable")
	}
	if g == nil || g.svr == nil || g.svr.Server == nil || g.svr.gb == nil {
		return nil, fmt.Errorf("SIP server is unavailable")
	}
	identityProbe := sip.NewRequest("", sip.MethodNotify, to.URI.Clone(), sip.DefaultSipVersion, nil, nil)
	if err := applyForwardedMonitorUserIdentity(ctx, identityProbe); err != nil {
		return nil, err
	}

	var operation *pendingDeviceOperation
	if strings.TrimSpace(ownerDeviceID) != "" {
		var releaseOperation func()
		operation, releaseOperation = g.trackPendingDeviceRequest(ctx, ownerDeviceID, deviceID)
		defer releaseOperation()
		ctx = operation.Context(ctx)
		if ctx.Err() != nil {
			return nil, operation.Cause()
		}
	}

	sub.mu.Lock()
	stale := sub.DialogRequest != dialogRequestSource || sub.Response != dialogResponse ||
		sub.Cascade != dialogCascade || sub.RemoteCSeq != dialogCSeq || sub.CSeq != previousCSeq
	ctxErr := ctx.Err()
	if !stale && ctxErr == nil {
		sub.CSeq = cseq
	}
	sub.mu.Unlock()
	if stale {
		return nil, errStaleEventNotifyDispatch
	}
	if ctxErr != nil {
		if operation != nil {
			return nil, operation.Cause()
		}
		return nil, ctxErr
	}
	target := &subscriptionTarget{
		to:        to,
		source:    source,
		conn:      conn,
		gbVersion: gbVersion,
	}
	expires := int(time.Until(expiresAt).Seconds())
	state := "active"
	if expires <= 0 {
		state = "terminated;reason=timeout"
	} else {
		state = fmt.Sprintf("active;expires=%d", expires)
	}
	eventValue := strings.TrimSpace(event)
	if eventValue == "" {
		version, ok := ParseGBProtocolVersion(gbVersion)
		if !ok {
			version = GBVersion10
		}
		eventValue = buildSubscriptionEventValueForVersion(version, cmdType, deviceID)
	}
	tx, err := g.svr.wrapRequestContext(ctx, target, sip.MethodNotify, &sip.ContentTypeXML, body, func(r *sip.Request) {
		applyServerSubscriptionDialog(r, dialogNotify)
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: eventValue})
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: state})
	})
	if err != nil {
		if operation != nil {
			return nil, operation.ErrorOr(err)
		}
		return nil, err
	}
	response, err := sipResponseContextAccepted(ctx, tx, func(status int) bool {
		return status >= 200 && status < 300
	})
	if err != nil {
		if operation != nil {
			return response, operation.ErrorOr(err)
		}
		return response, err
	}
	if operation != nil && !operation.Deliver(func() {}) {
		return response, operation.Cause()
	}
	version, ok := ParseGBProtocolVersion(gbVersion)
	if !ok {
		version = GBVersion10
	}
	return response, validateEventNotifyBusinessResponse(response, body, cmdType, eventValue, version)
}

func (g *GB28181API) sendCascadeEventNotify(sub *eventSubscription, cmdType string, body []byte) error {
	return g.sendCascadeEventNotifyContext(context.Background(), sub, cmdType, body)
}

func (g *GB28181API) sendCascadeEventNotifyContext(ctx context.Context, sub *eventSubscription, cmdType string, body []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if sub == nil {
		return fmt.Errorf("subscription is unavailable")
	}
	sub.notifyMu.Lock()
	var expectation *eventNotifyDispatchExpectation
	var response *sip.Response
	var err error
	for {
		sub.mu.Lock()
		expectation = &eventNotifyDispatchExpectation{cascade: sub.Cascade, dialogCSeq: sub.RemoteCSeq}
		sub.mu.Unlock()
		response, err = g.sendCascadeEventNotifyRequestContextExpected(ctx, sub, cmdType, body, expectation)
		if !errors.Is(err, errStaleEventNotifyDispatch) {
			break
		}
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
	}
	var downstreamKeys []string
	if shouldRemoveEventSubscriptionAfterNotifyFailure(ctx, response, err, sub) {
		downstreamKeys = g.detachEventSubscriptionAfterNotifyFailureContextExpected(ctx, sub, expectation)
	}
	sub.notifyMu.Unlock()
	g.releaseCascadeDownstreamSubscriptions(ctx, downstreamKeys)
	return err
}

func (g *GB28181API) sendCascadeEventNotifyRequestContext(ctx context.Context, sub *eventSubscription, cmdType string, body []byte) (*sip.Response, error) {
	return g.sendCascadeEventNotifyRequestContextExpected(ctx, sub, cmdType, body, nil)
}

func (g *GB28181API) sendCascadeEventNotifyRequestContextExpected(ctx context.Context, sub *eventSubscription, cmdType string, body []byte, expectation *eventNotifyDispatchExpectation) (*sip.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sub == nil {
		return nil, fmt.Errorf("subscription is unavailable")
	}
	sub.mu.Lock()
	if expectation != nil && (sub.Cascade != expectation.cascade || sub.RemoteCSeq != expectation.dialogCSeq) {
		sub.mu.Unlock()
		return nil, errStaleEventNotifyDispatch
	}
	if sub.DialogRequest == nil || sub.Response == nil || sub.Cascade == nil {
		sub.mu.Unlock()
		return nil, fmt.Errorf("cascade subscription dialog is unavailable")
	}
	if subscriptionExpiredAt(time.Now(), sub.ExpiresAt) {
		sub.mu.Unlock()
		return nil, fmt.Errorf("cascade subscription has expired")
	}
	dialogRequestSource := sub.DialogRequest
	dialogRequest, cloneErr := cloneEventSubscriptionDialogRequest(dialogRequestSource)
	if cloneErr != nil {
		sub.mu.Unlock()
		return nil, cloneErr
	}
	dialogResponse := sub.Response
	cascade := sub.Cascade
	gbVersion := sub.GBVersion
	event := sub.Event
	deviceID := sub.DeviceID
	expiresAt := sub.ExpiresAt
	identity := sub.Identity.clone()
	dialogCSeq := sub.RemoteCSeq
	previousCSeq := sub.CSeq
	cseq, cseqErr := nextEventSubscriptionCSeqLocked(sub)
	if cseqErr != nil {
		sub.mu.Unlock()
		return nil, cseqErr
	}
	sub.mu.Unlock()

	eventValue := strings.TrimSpace(event)
	if eventValue == "" {
		version, ok := ParseGBProtocolVersion(gbVersion)
		if !ok {
			version = GBVersion10
		}
		eventValue = buildSubscriptionEventValueForVersion(version, cmdType, deviceID)
	}
	expires := int(time.Until(expiresAt).Seconds())
	state := "terminated;reason=timeout"
	if expires > 0 {
		state = fmt.Sprintf("active;expires=%d", expires)
	}
	identityCtx := withMonitorUserIdentity(ctx, identity)
	buildRequest := func(sequence uint32, authHeader string, auth *sip.Authorization) (*sip.Request, error) {
		dialogNotify, err := sip.NewRequestFromServerDialogChecked(sip.MethodNotify, dialogRequest, dialogResponse, sequence)
		if err != nil {
			return nil, err
		}
		request := cascade.newRequest(sip.MethodNotify, &sip.ContentTypeXML, body, nil, sequence, -1, nil)
		applyServerSubscriptionDialog(request, dialogNotify)
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: eventValue})
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: state})
		if auth != nil {
			request.AppendHeader(&sip.GenericHeader{HeaderName: authHeader, Contents: auth.String()})
		}
		if err := cascade.platform.monitorUserIdentity.apply(identityCtx, request); err != nil {
			return nil, err
		}
		return request, nil
	}
	request, err := buildRequest(cseq, "", nil)
	if err != nil {
		return nil, err
	}
	sub.mu.Lock()
	stale := sub.DialogRequest != dialogRequestSource || sub.Response != dialogResponse ||
		sub.Cascade != cascade || sub.RemoteCSeq != dialogCSeq || sub.CSeq != previousCSeq
	ctxErr := ctx.Err()
	expired := subscriptionExpiredAt(time.Now(), sub.ExpiresAt)
	if !stale && ctxErr == nil && !expired {
		sub.CSeq = cseq
	}
	sub.mu.Unlock()
	if stale {
		return nil, errStaleEventNotifyDispatch
	}
	if ctxErr != nil {
		return nil, ctxErr
	}
	if expired {
		return nil, fmt.Errorf("cascade subscription has expired")
	}
	exchangeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopCascadeCancel := func() bool { return false }
	if operationCtx := cascade.operationContext(); operationCtx != nil {
		stopCascadeCancel = context.AfterFunc(operationCtx, cancel)
	}
	defer stopCascadeCancel()
	response, err := cascade.exchange(exchangeCtx, request)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, fmt.Errorf("cascade NOTIFY response is unavailable")
	}
	if (response.StatusCode() == http.StatusUnauthorized || response.StatusCode() == http.StatusProxyAuthRequired) &&
		cascade.platform.password != "" {
		authHeader, auth, authErr := cascadeEventNotifyDigestAuthorization(response, request, cascade.platform.localID, cascade.platform.password)
		if authErr != nil {
			return response, authErr
		}
		sub.mu.Lock()
		stale = sub.DialogRequest != dialogRequestSource || sub.Response != dialogResponse ||
			sub.Cascade != cascade || sub.RemoteCSeq != dialogCSeq
		ctxErr = ctx.Err()
		expired = subscriptionExpiredAt(time.Now(), sub.ExpiresAt)
		retryPreviousCSeq := sub.CSeq
		if !stale && ctxErr == nil && !expired {
			cseq, cseqErr = nextEventSubscriptionCSeqLocked(sub)
		}
		sub.mu.Unlock()
		if stale {
			return response, errStaleEventNotifyDispatch
		}
		if ctxErr != nil {
			return response, ctxErr
		}
		if expired {
			return response, fmt.Errorf("cascade subscription has expired")
		}
		if cseqErr != nil {
			return response, cseqErr
		}
		request, err = buildRequest(cseq, authHeader, auth)
		if err != nil {
			return response, err
		}
		sub.mu.Lock()
		stale = sub.DialogRequest != dialogRequestSource || sub.Response != dialogResponse ||
			sub.Cascade != cascade || sub.RemoteCSeq != dialogCSeq || sub.CSeq != retryPreviousCSeq
		ctxErr = ctx.Err()
		expired = subscriptionExpiredAt(time.Now(), sub.ExpiresAt)
		if !stale && ctxErr == nil && !expired {
			sub.CSeq = cseq
		}
		sub.mu.Unlock()
		if stale {
			return response, errStaleEventNotifyDispatch
		}
		if ctxErr != nil {
			return response, ctxErr
		}
		if expired {
			return response, fmt.Errorf("cascade subscription has expired")
		}
		response, err = cascade.exchange(exchangeCtx, request)
		if err != nil {
			return nil, err
		}
		if response == nil {
			return nil, fmt.Errorf("cascade NOTIFY authentication response is unavailable")
		}
	}
	if response.StatusCode() < 200 || response.StatusCode() >= 300 {
		return response, fmt.Errorf("cascade NOTIFY failed: %d %s", response.StatusCode(), response.Reason())
	}
	version, ok := ParseGBProtocolVersion(gbVersion)
	if !ok {
		version = GBVersion10
	}
	return response, validateEventNotifyBusinessResponse(response, body, cmdType, eventValue, version)
}

func cascadeEventNotifyDigestAuthorization(response *sip.Response, request *sip.Request, username, password string) (string, *sip.Authorization, error) {
	header, auth, err := cascadeRequestDigestAuthorization(response, request, username, password)
	if err != nil {
		return "", nil, fmt.Errorf("cascade NOTIFY Digest challenge: %w", err)
	}
	return header, auth, nil
}

func shouldRemoveEventSubscriptionAfterNotifyFailure(ctx context.Context, response *sip.Response, err error, sub *eventSubscription) bool {
	if err == nil {
		return false
	}
	if response != nil {
		if response.StatusCode() >= 200 && response.StatusCode() < 300 {
			return false
		}
		rawVersion := ""
		if sub != nil {
			sub.mu.Lock()
			rawVersion = sub.GBVersion
			sub.mu.Unlock()
		}
		version, ok := ParseGBProtocolVersion(rawVersion)
		if !ok {
			version = GBVersion10
		}
		if version == GBVersion30 {
			return shouldRemoveRFC6665Subscription(response.StatusCode())
		}
		if response.StatusCode() == 481 {
			return true
		}
		if response.StatusCode() == 401 || response.StatusCode() == 407 {
			return false
		}
		if hasValidRetryAfter(response) {
			return false
		}
		return true
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func shouldRemoveRFC6665Subscription(status int) bool {
	switch status {
	case 404, 405, 410, 416, 480, 481, 482, 483, 484, 485, 489, 501, 604:
		return true
	default:
		return false
	}
}

func hasValidRetryAfter(response *sip.Response) bool {
	if response == nil {
		return false
	}
	headers := response.GetHeaders("Retry-After")
	if len(headers) != 1 || headers[0] == nil {
		return false
	}
	value := headers[0].String()
	if _, after, ok := strings.Cut(value, ":"); ok {
		value = after
	}
	return validRetryAfterValue(strings.TrimSpace(value))
}

func validRetryAfterValue(value string) bool {
	index := 0
	for index < len(value) && value[index] >= '0' && value[index] <= '9' {
		index++
	}
	if index == 0 {
		return false
	}
	value = strings.TrimSpace(value[index:])
	if strings.HasPrefix(value, "(") {
		var ok bool
		value, ok = consumeRetryAfterComment(value)
		if !ok {
			return false
		}
		value = strings.TrimSpace(value)
	}
	for value != "" {
		if value[0] != ';' {
			return false
		}
		value = strings.TrimSpace(value[1:])
		nameEnd := 0
		for nameEnd < len(value) && isRetryAfterTokenByte(value[nameEnd]) {
			nameEnd++
		}
		if nameEnd == 0 {
			return false
		}
		name := value[:nameEnd]
		value = strings.TrimSpace(value[nameEnd:])
		if !strings.HasPrefix(value, "=") {
			if strings.EqualFold(name, "duration") {
				return false
			}
			continue
		}
		value = strings.TrimSpace(value[1:])
		paramValue, remaining, ok := consumeRetryAfterParamValue(value)
		if !ok || strings.EqualFold(name, "duration") && !isASCIIDigits(paramValue) {
			return false
		}
		value = strings.TrimSpace(remaining)
	}
	return true
}

func consumeRetryAfterComment(value string) (string, bool) {
	depth := 0
	escaped := false
	for index := 0; index < len(value); index++ {
		char := value[index]
		if escaped {
			escaped = false
			continue
		}
		switch char {
		case '\\':
			escaped = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return value[index+1:], true
			}
		case '\r', '\n':
			return "", false
		}
	}
	return "", false
}

func consumeRetryAfterParamValue(value string) (string, string, bool) {
	if value == "" {
		return "", "", false
	}
	if value[0] == '"' {
		escaped := false
		for index := 1; index < len(value); index++ {
			char := value[index]
			if escaped {
				escaped = false
				continue
			}
			switch char {
			case '\\':
				escaped = true
			case '"':
				return value[:index+1], value[index+1:], true
			case '\r', '\n':
				return "", "", false
			}
		}
		return "", "", false
	}
	index := 0
	for index < len(value) && isRetryAfterParamValueByte(value[index]) {
		index++
	}
	if index == 0 {
		return "", "", false
	}
	return value[:index], value[index:], true
}

func isRetryAfterTokenByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || strings.ContainsRune("-.!%*_+`'~", rune(value))
}

func isRetryAfterParamValueByte(value byte) bool {
	return isRetryAfterTokenByte(value) || strings.ContainsRune(":[]", rune(value))
}

func isASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

// detachEventSubscriptionAfterNotifyFailure 必须在持有 sub.notifyMu 时调用，防止等待中的旧 NOTIFY
// 在订阅被删除后继续使用同一对话发送。
func (g *GB28181API) detachEventSubscriptionAfterNotifyFailure(sub *eventSubscription) []string {
	return g.detachEventSubscriptionAfterNotifyFailureContext(context.Background(), sub)
}

func (g *GB28181API) detachEventSubscriptionAfterNotifyFailureContext(ctx context.Context, sub *eventSubscription) []string {
	return g.detachEventSubscriptionAfterNotifyFailureContextExpected(ctx, sub, nil)
}

func (g *GB28181API) detachEventSubscriptionAfterNotifyFailureContextExpected(ctx context.Context, sub *eventSubscription, expectation *eventNotifyDispatchExpectation) []string {
	if g == nil || sub == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := strings.TrimSpace(sub.Key)
	if key == "" {
		return nil
	}
	unlock, err := g.lockEventSubscriptionOperation(ctx, key)
	if err != nil {
		return nil
	}
	current, loaded := g.eventSubscribers.Load(key)
	if !loaded || current != sub {
		unlock()
		return nil
	}
	sub.mu.Lock()
	if expectation != nil && (sub.Cascade != expectation.cascade || sub.RemoteCSeq != expectation.dialogCSeq) {
		sub.mu.Unlock()
		unlock()
		return nil
	}
	removed := g.eventSubscribers.CompareAndDelete(key, sub)
	var downstreamKeys []string
	if removed {
		sub.ExpiresAt = time.Now()
		sub.DialogRequest = nil
		sub.Response = nil
		downstreamKeys = append(downstreamKeys, sub.DownstreamKeys...)
		sub.DownstreamKeys = nil
	}
	sub.mu.Unlock()
	unlock()
	return downstreamKeys
}

// startEventSubscriberCleaner 定时清理过期订阅，避免无事件期间缓存积累。
func (g *GB28181API) startEventSubscriberCleaner() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	ctx := g.serviceContext()
	for {
		select {
		case <-g.lifecycleDone:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now()
		g.cleanupEventSubscriptionsContext(ctx, now)
		if ctx.Err() != nil {
			return
		}
		g.outgoingSubscriptions.Range(func(key, value any) bool {
			g.cleanupOutgoingSubscription(key, value, now)
			return ctx.Err() == nil
		})
	}
}

func (g *GB28181API) cleanupOutgoingSubscription(key, value any, now time.Time) {
	if g == nil {
		return
	}
	dialog, ok := value.(*outgoingSubscriptionDialog)
	if !ok || dialog == nil {
		g.outgoingSubscriptions.CompareAndDelete(key, value)
		return
	}
	// Subscribe 会在持有 dialog.mu 的网络流程中通过 compareAndDeleteOutgoingSubscription
	// 获取 notifyOperationMu。清理器必须沿用相同锁序，不能先占用 notifyOperationMu 后等待
	// dialog.mu，否则订阅失败收尾与定时清理可形成 ABBA 死锁。
	dialog.mu.Lock()
	dialog.notifyOperationMu.Lock()
	defer dialog.notifyOperationMu.Unlock()
	if current, loaded := g.outgoingSubscriptions.Load(key); !loaded || current != dialog {
		dialog.mu.Unlock()
		return
	}
	keyString, keyOK := key.(string)
	referenced := keyOK && g.hasReferencedCascadeSubscription(keyString)
	expired := !dialog.expiresAt.IsZero() && subscriptionExpiredAt(now, dialog.expiresAt)
	refreshAt := dialog.refreshAt
	cancelPending := dialog.cancelPending.Load()
	autoRefresh := dialog.autoRefresh
	refreshInput := dialog.refreshInput
	identity := dialog.identity.clone()
	localGatewayID := dialog.localGatewayID
	refreshDue := (referenced || autoRefresh) && !cancelPending && !expired && !refreshAt.IsZero() && !now.Before(refreshAt) && !dialog.refreshing
	if refreshDue {
		dialog.refreshing = true
	}
	dialog.mu.Unlock()
	if expired {
		// 清理判断与删除之间可能已由 terminated NOTIFY 或重新订阅替换对话，
		// 只能删除本次检查的旧对象，不能误删新建立的会话。
		if g.outgoingSubscriptions.CompareAndDelete(key, dialog) && !cancelPending {
			switch {
			case referenced:
				g.startLifecycleTask(context.Background(), func(taskCtx context.Context) {
					g.renewTerminatedCascadeSubscription(key, dialog, taskCtx)
				})
			case autoRefresh && keyOK && strings.TrimSpace(refreshInput.DeviceID) != "" && !refreshInput.Cancel:
				g.startLifecycleTask(context.Background(), func(taskCtx context.Context) {
					g.recreateExpiredManualSubscription(refreshInput, identity, localGatewayID, taskCtx)
				})
			}
		}
		return
	}
	refreshTask := func(taskCtx context.Context) {
		if referenced {
			g.renewExpiringCascadeSubscription(keyString, dialog, refreshAt, taskCtx)
			return
		}
		g.renewExpiringManualSubscription(keyString, dialog, refreshAt, taskCtx)
	}
	if refreshDue && !g.startLifecycleTask(context.Background(), refreshTask) {
		dialog.mu.Lock()
		if dialog.refreshAt.Equal(refreshAt) {
			dialog.refreshing = false
		}
		dialog.mu.Unlock()
	}
}

func (g *GB28181API) recreateExpiredManualSubscription(input SubscribeInput, identity *monitorUserIdentity, localGatewayID string, parent context.Context) {
	if g == nil || strings.TrimSpace(input.DeviceID) == "" || input.Cancel {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	ctx = withMonitorUserIdentityRoute(ctx, identity, localGatewayID)
	if err := g.invokeManualSubscribeRefresh(ctx, &input); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("recreate expired device subscription failed", "event", input.Event, "device_id", input.DeviceID, "target_id", input.TargetID, "err", err)
	}
}

func (g *GB28181API) renewExpiringManualSubscription(key string, observed *outgoingSubscriptionDialog, refreshAt time.Time, parent context.Context) {
	if g == nil || observed == nil || strings.TrimSpace(key) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	defer g.finishOutgoingSubscriptionRefresh(observed, refreshAt)
	current, loaded := g.outgoingSubscriptions.Load(key)
	if !loaded || current != observed {
		return
	}
	observed.mu.Lock()
	due := observed.autoRefresh && observed.refreshing && observed.refreshAt.Equal(refreshAt) && !time.Now().Before(observed.refreshAt)
	input := observed.refreshInput
	identity := observed.identity.clone()
	localGatewayID := observed.localGatewayID
	observed.mu.Unlock()
	if !due || strings.TrimSpace(input.DeviceID) == "" || input.Cancel {
		return
	}
	ctx = withMonitorUserIdentityRoute(ctx, identity, localGatewayID)
	if err := g.invokeManualSubscribeRefresh(ctx, &input); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("refresh expiring device subscription failed", "event", input.Event, "device_id", input.DeviceID, "target_id", input.TargetID, "err", err)
	}
}

func (g *GB28181API) invokeManualSubscribeRefresh(ctx context.Context, input *SubscribeInput) error {
	if g.manualSubscribeRefresh != nil {
		return g.manualSubscribeRefresh(ctx, input)
	}
	return g.Subscribe(ctx, input)
}

func (g *GB28181API) hasReferencedCascadeSubscription(key string) bool {
	if g == nil || strings.TrimSpace(key) == "" {
		return false
	}
	g.cascadeSubscriptionMu.Lock()
	state := g.cascadeSubscriptions[key]
	referenced := state != nil && state.Refs > 0
	g.cascadeSubscriptionMu.Unlock()
	return referenced
}

func (g *GB28181API) renewExpiringCascadeSubscription(key string, observed *outgoingSubscriptionDialog, refreshAt time.Time, parent context.Context) {
	if g == nil || observed == nil || strings.TrimSpace(key) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	unlock, err := g.lockCascadeSubscriptionOperation(ctx, key)
	if err != nil {
		g.finishOutgoingSubscriptionRefresh(observed, refreshAt)
		return
	}
	defer unlock()
	defer g.finishOutgoingSubscriptionRefresh(observed, refreshAt)
	current, loaded := g.outgoingSubscriptions.Load(key)
	if !loaded || current != observed {
		return
	}
	observed.mu.Lock()
	due := observed.refreshing && observed.refreshAt.Equal(refreshAt) && !time.Now().Before(observed.refreshAt)
	observed.mu.Unlock()
	if !due {
		return
	}
	g.cascadeSubscriptionMu.Lock()
	state := g.cascadeSubscriptions[key]
	if state == nil || state.Refs <= 0 {
		g.cascadeSubscriptionMu.Unlock()
		return
	}
	input := state.Input
	identity := state.Identity.clone()
	localGatewayID := state.LocalGatewayID
	g.cascadeSubscriptionMu.Unlock()
	ctx = withMonitorUserIdentityRoute(ctx, identity, localGatewayID)
	if err := g.invokeCascadeSubscribe(ctx, &input); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("refresh expiring cascade subscription failed", "event", input.Event, "device_id", input.DeviceID, "target_id", input.TargetID, "err", err)
	}
}

func (g *GB28181API) finishOutgoingSubscriptionRefresh(dialog *outgoingSubscriptionDialog, refreshAt time.Time) {
	if dialog == nil {
		return
	}
	dialog.mu.Lock()
	if dialog.refreshAt.Equal(refreshAt) {
		dialog.refreshing = false
	}
	dialog.mu.Unlock()
}

func (g *GB28181API) cleanupEventSubscriptions(now time.Time) {
	g.cleanupEventSubscriptionsContext(context.Background(), now)
}

func (g *GB28181API) cleanupEventSubscriptionsContext(ctx context.Context, now time.Time) {
	if g == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	g.eventSubscribers.Range(func(rawKey, value any) bool {
		if ctx.Err() != nil {
			return false
		}
		key, ok := rawKey.(string)
		if !ok || strings.TrimSpace(key) == "" {
			g.eventSubscribers.CompareAndDelete(rawKey, value)
			return true
		}
		unlock, err := g.lockEventSubscriptionOperation(ctx, key)
		if err != nil {
			return ctx.Err() == nil
		}
		defer unlock()
		value, exists := g.eventSubscribers.Load(key)
		if !exists {
			return true
		}
		sub, ok := value.(*eventSubscription)
		if !ok || sub == nil {
			g.eventSubscribers.CompareAndDelete(key, value)
			return true
		}
		sub.mu.Lock()
		expired := subscriptionExpiredAt(now, sub.ExpiresAt)
		downstreamKeys := append([]string(nil), sub.DownstreamKeys...)
		sub.mu.Unlock()
		if expired {
			if g.eventSubscribers.CompareAndDelete(key, sub) {
				g.releaseCascadeDownstreamSubscriptions(ctx, downstreamKeys)
			}
		}
		return ctx.Err() == nil
	})
}
