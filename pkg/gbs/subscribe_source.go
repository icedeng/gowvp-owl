package gbs

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	defaultSubscribeExpires               = 3600
	defaultCascadeCatalogSubscribeExpires = 600
)

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
}

type cascadeDownstreamSubscription struct {
	Input SubscribeInput
	Refs  int
}

// eventSubscription 保存事件源侧订阅会话。
type eventSubscription struct {
	mu        sync.Mutex
	catalogMu sync.Mutex

	Key      string
	CmdType  string
	DeviceID string

	ExpiresAt time.Time

	To     *sip.Address
	Source net.Addr
	Conn   sip.Connection

	GBVersion string
	Event     string
	Response  *sip.Response
	Contact   *sip.Address
	CSeq      uint32
	Cascade   *cascadeWorker
	Filter    eventSubscriptionFilter
	Interval  int

	// DownstreamKeys 记录本级为该上级订阅自动建立的下级订阅，用于续订、退订和超时释放。
	DownstreamKeys  []string
	CatalogSnapshot map[string]cascadeCatalogItem
}

type outgoingSubscriptionDialog struct {
	mu           sync.Mutex
	response     *sip.Response
	remoteTarget *sip.URI
	eventValue   string
	expiresAt    time.Time
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

// sipSubscribeEvent 处理事件源侧 SUBSCRIBE 请求。
func (g *GB28181API) sipSubscribeEvent(ctx *sip.Context) {
	var req subscribeEventRequest
	if len(ctx.Request.Body()) == 0 {
		ctx.String(400, "empty subscribe body")
		return
	}
	if err := sip.XMLDecode(ctx.Request.Body(), &req); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	if strings.TrimSpace(req.StartAlarmTime) == "" {
		req.StartAlarmTime = strings.TrimSpace(req.StartTime)
	}
	if strings.TrimSpace(req.EndAlarmTime) == "" {
		req.EndAlarmTime = strings.TrimSpace(req.EndTime)
	}

	cmdType := strings.TrimSpace(req.CmdType)
	if normalized, ok := normalizeSubscribeCmdType(cmdType); ok {
		cmdType = normalized
	} else if !isSupportedSubscribeCmdType(cmdType) {
		ctx.String(400, "unsupported subscribe cmd_type")
		return
	}

	deviceID := strings.TrimSpace(req.DeviceID)
	if deviceID == "" {
		deviceID = "*"
	}
	var cascade *cascadeWorker
	if value, exists := ctx.Get(cascadeWorkerContextKey); exists {
		cascade, _ = value.(*cascadeWorker)
		if cascade == nil || !cascadeSubscriptionTargetAllowed(cascade.platform, cmdType, deviceID) {
			ctx.String(404, "cascade target not found")
			return
		}
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
	subscriptionVersion := GBVersion10
	if cascade != nil {
		subscriptionVersion = cascade.protocolVersion()
	} else if parsed, ok := ParseGBProtocolVersion(ctx.XGBVer); ok {
		subscriptionVersion = parsed
	} else if g != nil {
		subscriptionVersion = g.getDeviceGBProtocolVersion(ctx.DeviceID)
	}
	if expires != 0 {
		if err := validateSubscribeEventRequest(req, cmdType, subscriptionVersion); err != nil {
			ctx.String(400, err.Error())
			return
		}
	}
	eventValue, eventID, err := parseSubscriptionEvent(ctx.GetHeader("Event"))
	if err != nil {
		ctx.String(400, err.Error())
		return
	}
	if eventValue != "" {
		if err := validateSubscriptionEventHeader(eventValue, cmdType, eventID, deviceID); err != nil {
			ctx.String(400, err.Error())
			return
		}
	}
	if eventValue == "" {
		eventValue = buildSubscriptionEventValueForVersion(subscriptionVersion, cmdType, deviceID)
	}
	if eventID != "" && deviceID != "*" && eventID != deviceID {
		ctx.String(400, "event id does not match DeviceID")
		return
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

	dialogID, fromTag := parseSubscribeDialog(ctx)
	key := buildEventSubscriptionKey(dialogID, fromTag, cmdType, deviceID)
	g.eventSubscriptionMu.Lock()
	defer g.eventSubscriptionMu.Unlock()
	if expires == 0 {
		// Expires=0 为退订。
		if value, loaded := g.eventSubscribers.LoadAndDelete(key); loaded {
			if existing, ok := value.(*eventSubscription); ok && existing != nil {
				existing.mu.Lock()
				existing.ExpiresAt = time.Now()
				downstreamKeys := append([]string(nil), existing.DownstreamKeys...)
				existing.mu.Unlock()
				g.releaseCascadeDownstreamSubscriptions(context.Background(), downstreamKeys)
			}
		}
		g.respondSubscribeOK(ctx, req, eventValue, expires, cascade, subscriptionVersion)
		return
	}
	var previousKeys []string
	if value, loaded := g.eventSubscribers.Load(key); loaded {
		if existing, ok := value.(*eventSubscription); ok && existing != nil {
			existing.mu.Lock()
			previousKeys = append(previousKeys, existing.DownstreamKeys...)
			existing.mu.Unlock()
		}
	}
	desired, err := g.desiredCascadeDownstreamSubscriptions(context.Background(), cascade, req, cmdType, deviceID, expires)
	if err != nil {
		ctx.String(502, err.Error())
		return
	}
	downstreamKeys, err := g.syncCascadeDownstreamSubscriptions(context.Background(), previousKeys, desired)
	if err != nil {
		ctx.String(502, err.Error())
		return
	}

	sub := &eventSubscription{
		Key:            key,
		CmdType:        cmdType,
		DeviceID:       deviceID,
		ExpiresAt:      time.Now().Add(time.Duration(expires) * time.Second),
		To:             targetAddr.Clone(),
		Source:         ctx.Source,
		Conn:           ctx.Request.GetConnection(),
		GBVersion:      ctx.XGBVer,
		Event:          eventValue,
		Cascade:        cascade,
		Filter:         subscriptionFilterFromRequest(req),
		Interval:       subscribeRequestInterval(req),
		DownstreamKeys: downstreamKeys,
	}
	response, contact := g.respondSubscribeOK(ctx, req, eventValue, expires, cascade, subscriptionVersion)
	sub.Response = response
	sub.Contact = contact
	initial := true
	if actual, loaded := g.eventSubscribers.LoadOrStore(key, sub); loaded {
		if existing, ok := actual.(*eventSubscription); ok && existing != nil {
			existing.mu.Lock()
			existing.CmdType = sub.CmdType
			existing.DeviceID = sub.DeviceID
			existing.ExpiresAt = sub.ExpiresAt
			existing.To = sub.To
			existing.Source = sub.Source
			existing.Conn = sub.Conn
			existing.GBVersion = sub.GBVersion
			existing.Event = sub.Event
			existing.Response = sub.Response
			existing.Contact = sub.Contact
			existing.Cascade = sub.Cascade
			existing.Filter = sub.Filter
			existing.Interval = sub.Interval
			existing.DownstreamKeys = append(existing.DownstreamKeys[:0], sub.DownstreamKeys...)
			existing.mu.Unlock()
			sub = existing
			initial = false
		} else {
			g.eventSubscribers.Store(key, sub)
		}
	}
	if initial && shouldSendCascadeInitialCatalogNotify(cascade, cmdType) {
		go func() {
			initialCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := g.sendCascadeInitialCatalogNotify(initialCtx, sub); err != nil {
				slog.Warn("send initial cascade Catalog NOTIFY failed", "upstream", cascade.platform.name, "err", err)
			}
		}()
	} else if initial && cascade != nil && strings.EqualFold(cmdType, "Catalog") {
		seedCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := g.seedCascadeCatalogSnapshot(seedCtx, sub); err != nil {
			slog.Warn("seed cascade Catalog subscription snapshot failed", "upstream", cascade.platform.name, "err", err)
		}
		cancel()
	}
}

// sipNotifySubscriptionState 处理 RFC 3265 主动终止订阅。
// GB/T 28181-2016 附录 P 允许 terminated NOTIFY 消息体为空，此时仍应返回 200 OK。
func (g *GB28181API) sipNotifySubscriptionState(ctx *sip.Context) {
	state := strings.TrimSpace(ctx.GetHeader("Subscription-State"))
	state, _, _ = strings.Cut(state, ";")
	terminated := strings.EqualFold(strings.TrimSpace(state), "terminated")
	if terminated {
		g.removeOutgoingSubscriptionDialog(ctx.Request)
		if len(ctx.Request.Body()) == 0 {
			ctx.String(200, "OK")
			ctx.Abort()
			return
		}
	}
	if len(ctx.Request.Body()) == 0 {
		ctx.String(400, "empty notify body")
		ctx.Abort()
		return
	}
	ctx.Next()
}

func (g *GB28181API) removeOutgoingSubscriptionDialog(request *sip.Request) {
	if g == nil || request == nil {
		return
	}
	callID, ok := request.CallID()
	if !ok || callID == nil {
		return
	}
	wanted := normalizeCallID(callID)
	g.outgoingSubscriptions.Range(func(key, value any) bool {
		dialog, ok := value.(*outgoingSubscriptionDialog)
		if !ok || dialog == nil {
			return true
		}
		dialog.mu.Lock()
		response := dialog.response
		matches := false
		if response != nil {
			if existing, exists := response.CallID(); exists && existing != nil {
				matches = normalizeCallID(existing) == wanted
			}
		}
		dialog.mu.Unlock()
		if matches {
			g.outgoingSubscriptions.Delete(key)
			go g.renewTerminatedCascadeSubscription(key)
		}
		return true
	})
}

func (g *GB28181API) renewTerminatedCascadeSubscription(key any) {
	if g == nil {
		return
	}
	keyString, ok := key.(string)
	if !ok || keyString == "" {
		return
	}
	g.cascadeSubscriptionMu.Lock()
	defer g.cascadeSubscriptionMu.Unlock()
	state := g.cascadeSubscriptions[keyString]
	if state == nil || state.Refs <= 0 {
		return
	}
	input := state.Input
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := g.invokeCascadeSubscribe(ctx, &input); err != nil {
		slog.Warn("renew terminated cascade subscription failed", "event", input.Event, "device_id", input.DeviceID, "target_id", input.TargetID, "err", err)
	}
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

func (g *GB28181API) respondSubscribeOK(ctx *sip.Context, req subscribeEventRequest, eventValue string, expires int, cascade *cascadeWorker, version GBProtocolVersion) (*sip.Response, *sip.Address) {
	var body []byte
	if version == GBVersion10 {
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
	_ = ctx.Tx.Respond(response)
	return response, contact
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
	case "devicestatus":
		return "DeviceStatus", true
	case "recordinfo":
		return "RecordInfo", true
	case "deviceinfo":
		return "DeviceInfo", true
	case "configdownload":
		return "ConfigDownload", true
	case "presetquery":
		return "PresetQuery", true
	case "homepositionquery":
		return "HomePositionQuery", true
	case "sdcardstatus":
		return "SDCardStatus", true
	case "broadcast":
		return "Broadcast", true
	default:
		return "", false
	}
}

func buildSubscriptionEventValue(cmdType, deviceID string) string {
	cmdType = strings.TrimSpace(cmdType)
	deviceID = strings.TrimSpace(deviceID)
	if strings.EqualFold(cmdType, "Catalog") && deviceID != "" && deviceID != "*" {
		return "Catalog;id=" + deviceID
	}
	return cmdType
}

// buildSubscriptionEventValueForVersion 区分基础事件订阅与 2014 增加的域间目录订阅 Event id 参数。
func buildSubscriptionEventValueForVersion(version GBProtocolVersion, cmdType, deviceID string) string {
	parsed, ok := ParseGBProtocolVersion(string(version))
	if !ok {
		parsed = GBVersion10
	}
	if parsed != GBVersion10 && strings.EqualFold(strings.TrimSpace(cmdType), "Catalog") {
		return buildSubscriptionEventValue(cmdType, deviceID)
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

func validateSubscriptionEventHeader(eventValue, cmdType, eventID, deviceID string) error {
	eventName, _, _ := strings.Cut(strings.TrimSpace(eventValue), ";")
	if !strings.EqualFold(strings.TrimSpace(eventName), "presence") && !strings.EqualFold(strings.TrimSpace(eventName), strings.TrimSpace(cmdType)) {
		return fmt.Errorf("event header does not match subscribe cmd_type")
	}
	if strings.TrimSpace(eventID) != "" && !strings.EqualFold(strings.TrimSpace(cmdType), "Catalog") {
		return fmt.Errorf("event id is only valid for Catalog subscriptions")
	}
	if strings.TrimSpace(eventID) != "" && strings.TrimSpace(deviceID) != "" && strings.TrimSpace(deviceID) != "*" && strings.TrimSpace(eventID) != strings.TrimSpace(deviceID) {
		return fmt.Errorf("event id does not match DeviceID")
	}
	return nil
}

func parseSubscribeExpires(value string) (int, error) {
	if strings.TrimSpace(value) == "" {
		return defaultSubscribeExpires, nil
	}
	expires, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid expires header")
	}
	if expires < 0 {
		return 0, fmt.Errorf("invalid expires header")
	}
	return expires, nil
}

func parseSubscribeExpiresForProfile(value, cmdType string, cascade *cascadeWorker) (int, error) {
	if strings.TrimSpace(value) == "" && cascade != nil && strings.EqualFold(strings.TrimSpace(cmdType), "Catalog") && cascade.protocolVersion().AtLeast(GBVersion11) {
		return defaultCascadeCatalogSubscribeExpires, nil
	}
	return parseSubscribeExpires(value)
}

func isSupportedSubscribeCmdType(cmdType string) bool {
	cmd := strings.TrimSpace(cmdType)
	if cmd == "" {
		return false
	}
	switch cmd {
	case "Alarm", "Catalog", "MobilePosition", "PTZPosition",
		"DeviceStatus", "RecordInfo", "DeviceInfo", "ConfigDownload",
		"PresetQuery", "HomePositionQuery", "SDCardStatus", "Broadcast":
		return true
	default:
		// 兼容扩展事件类型：允许 Query/Status 结尾的订阅命令。
		return strings.HasSuffix(cmd, "Query") || strings.HasSuffix(cmd, "Status")
	}
}

func buildEventSubscriptionKey(dialogID, fromTag, cmdType, deviceID string) string {
	return strings.Join([]string{
		strings.TrimSpace(dialogID),
		strings.TrimSpace(fromTag),
		strings.ToUpper(strings.TrimSpace(cmdType)),
		strings.TrimSpace(deviceID),
	}, "|")
}

func parseSubscribeDialog(ctx *sip.Context) (dialogID, fromTag string) {
	if callID, ok := ctx.Request.CallID(); ok && callID != nil {
		dialogID = strings.TrimSpace(string(*callID))
	}
	if from, ok := ctx.Request.From(); ok && from != nil && from.Params != nil {
		if tag, ok := from.Params.Get("tag"); ok && tag != nil {
			fromTag = strings.TrimSpace(tag.String())
		}
	}
	if dialogID == "" && ctx.To != nil && ctx.To.URI != nil {
		// 兜底：缺少 Call-ID 的异常请求，退化为目标 URI 维度。
		dialogID = strings.TrimSpace(ctx.To.URI.String())
	}
	return dialogID, fromTag
}

// publishEventNotify 向匹配订阅方发送 NOTIFY。
func (g *GB28181API) publishEventNotify(cmdType, deviceID string, body []byte) {
	cmdType = strings.TrimSpace(cmdType)
	deviceID = strings.TrimSpace(deviceID)
	if cmdType == "" || len(body) == 0 {
		return
	}

	now := time.Now()
	g.eventSubscribers.Range(func(key, value any) bool {
		sub, ok := value.(*eventSubscription)
		if !ok || sub == nil {
			g.eventSubscribers.Delete(key)
			return true
		}
		sub.mu.Lock()
		expiresAt := sub.ExpiresAt
		cascade := sub.Cascade
		subCmdType := sub.CmdType
		subDeviceID := sub.DeviceID
		filter := sub.Filter
		sub.mu.Unlock()
		if now.After(expiresAt) {
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
			cascadeBody, exposedID, err := rewriteCascadeEventBodyForDevice(cascade.platform, body, deviceID)
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
			if err := g.sendEventNotify(sub, cmdType, cascadeBody); err != nil {
				slog.Warn("send cascade event notify failed", "cmdType", cmdType, "deviceID", deviceID, "err", err)
			}
			return true
		}
		if !strings.EqualFold(subCmdType, cmdType) {
			return true
		}
		if subDeviceID != "*" && subDeviceID != "" && subDeviceID != deviceID {
			return true
		}
		if err := g.sendEventNotify(sub, cmdType, body); err != nil {
			// 这里不删除订阅，避免临时网络抖动导致订阅丢失。
			slog.Warn("send event notify failed", "cmdType", cmdType, "deviceID", deviceID, "err", err)
		}
		return true
	})
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
	decoder := xml.NewDecoder(bytes.NewReader(body))
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
	return output.Bytes(), exposedDeviceID, nil
}

func (g *GB28181API) sendEventNotify(sub *eventSubscription, cmdType string, body []byte) error {
	if sub == nil {
		return fmt.Errorf("subscription is unavailable")
	}
	sub.mu.Lock()
	cascade := sub.Cascade
	sub.mu.Unlock()
	if cascade != nil {
		return g.sendCascadeEventNotify(sub, cmdType, body)
	}
	sub.mu.Lock()
	to := sub.To.Clone()
	source := sub.Source
	conn := sub.Conn
	gbVersion := sub.GBVersion
	expiresAt := sub.ExpiresAt
	event := sub.Event
	deviceID := sub.DeviceID
	sub.mu.Unlock()
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
	tx, err := g.svr.wrapRequest(target, sip.MethodNotify, &sip.ContentTypeXML, body, func(r *sip.Request) {
		eventValue := strings.TrimSpace(event)
		if eventValue == "" {
			version, ok := ParseGBProtocolVersion(gbVersion)
			if !ok {
				version = GBVersion10
			}
			eventValue = buildSubscriptionEventValueForVersion(version, cmdType, deviceID)
		}
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: eventValue})
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: state})
	})
	if err != nil {
		return err
	}
	_, err = sipResponse(tx)
	return err
}

func (g *GB28181API) sendCascadeEventNotify(sub *eventSubscription, cmdType string, body []byte) error {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.Response == nil || sub.Cascade == nil {
		return fmt.Errorf("cascade subscription dialog is unavailable")
	}
	if !time.Now().Before(sub.ExpiresAt) {
		return fmt.Errorf("cascade subscription has expired")
	}
	local, localOK := sub.Response.To()
	remote, remoteOK := sub.Response.From()
	callID, callIDOK := sub.Response.CallID()
	if !localOK || local == nil || local.Address == nil || !remoteOK || remote == nil || remote.Address == nil || !callIDOK || callID == nil || sub.To == nil || sub.To.URI == nil {
		return fmt.Errorf("cascade subscription target is unavailable")
	}
	sub.CSeq++
	if sub.CSeq == 0 {
		sub.CSeq = 1
	}
	localAddress := &sip.Address{DisplayName: local.DisplayName, URI: local.Address.Clone(), Params: local.Params.Clone()}
	remoteAddress := &sip.Address{DisplayName: remote.DisplayName, URI: remote.Address.Clone(), Params: remote.Params.Clone()}
	headers := sip.NewHeaderBuilder().
		SetFrom(localAddress).
		SetToWithParam(remoteAddress).
		SetContentType(&sip.ContentTypeXML).
		SetMethod(sip.MethodNotify).
		SetSeqNo(uint(sub.CSeq)).
		SetCallID(callID).
		SetXGBVerValue(sub.GBVersion).
		AddVia(&sip.ViaHop{
			Host: sub.Cascade.platform.localHost, Port: sip.NewPort(sub.Cascade.platform.localPort), Transport: "UDP",
			Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
		}).Build()
	if sub.Contact != nil && sub.Contact.URI != nil {
		headers = append(headers, &sip.ContactHeader{DisplayName: sub.Contact.DisplayName, Address: sub.Contact.URI.Clone(), Params: sub.Contact.Params.Clone()})
	}
	eventValue := strings.TrimSpace(sub.Event)
	if eventValue == "" {
		version, ok := ParseGBProtocolVersion(sub.GBVersion)
		if !ok {
			version = GBVersion10
		}
		eventValue = buildSubscriptionEventValueForVersion(version, cmdType, sub.DeviceID)
	}
	expires := int(time.Until(sub.ExpiresAt).Seconds())
	state := "terminated;reason=timeout"
	if expires > 0 {
		state = fmt.Sprintf("active;expires=%d", expires)
	}
	headers = append(headers,
		&sip.GenericHeader{HeaderName: "Event", Contents: eventValue},
		&sip.GenericHeader{HeaderName: "Subscription-State", Contents: state},
	)
	request := sip.NewRequest("", sip.MethodNotify, sub.To.URI.Clone(), sip.DefaultSipVersion, headers, body)
	request.SetDestination(sub.Cascade.remoteDestination())
	if g != nil && g.svr != nil && g.svr.UDPConn() != nil {
		request.SetConnection(g.svr.UDPConn())
		request.SetSource(g.svr.UDPConn().LocalAddr())
	}
	response, err := sub.Cascade.exchange(context.Background(), request)
	if err != nil {
		return err
	}
	if response.StatusCode() < 200 || response.StatusCode() >= 300 {
		return fmt.Errorf("cascade NOTIFY failed: %d %s", response.StatusCode(), response.Reason())
	}
	return nil
}

// startEventSubscriberCleaner 定时清理过期订阅，避免无事件期间缓存积累。
func (g *GB28181API) startEventSubscriberCleaner() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-g.lifecycleDone:
			return
		case <-ticker.C:
		}
		now := time.Now()
		g.cleanupEventSubscriptions(now)
		g.outgoingSubscriptions.Range(func(key, value any) bool {
			dialog, ok := value.(*outgoingSubscriptionDialog)
			if !ok || dialog == nil {
				g.outgoingSubscriptions.Delete(key)
				return true
			}
			dialog.mu.Lock()
			expired := !dialog.expiresAt.IsZero() && now.After(dialog.expiresAt)
			dialog.mu.Unlock()
			if expired {
				g.outgoingSubscriptions.Delete(key)
			}
			return true
		})
	}
}

func (g *GB28181API) cleanupEventSubscriptions(now time.Time) {
	g.eventSubscriptionMu.Lock()
	defer g.eventSubscriptionMu.Unlock()
	g.eventSubscribers.Range(func(key, value any) bool {
		sub, ok := value.(*eventSubscription)
		if !ok || sub == nil {
			g.eventSubscribers.Delete(key)
			return true
		}
		sub.mu.Lock()
		expired := now.After(sub.ExpiresAt)
		downstreamKeys := append([]string(nil), sub.DownstreamKeys...)
		sub.mu.Unlock()
		if expired {
			g.eventSubscribers.Delete(key)
			g.releaseCascadeDownstreamSubscriptions(context.Background(), downstreamKeys)
		}
		return true
	})
}
