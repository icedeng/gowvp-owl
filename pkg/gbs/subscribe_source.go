package gbs

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const defaultSubscribeExpires = 3600

// subscribeEventRequest 是 9.11 SUBSCRIBE 订阅体。
type subscribeEventRequest struct {
	CmdType  string `xml:"CmdType"`
	SN       int    `xml:"SN"`
	DeviceID string `xml:"DeviceID"`
}

// eventSubscription 保存事件源侧订阅会话。
type eventSubscription struct {
	Key      string
	CmdType  string
	DeviceID string

	ExpiresAt time.Time

	To     *sip.Address
	Source net.Addr
	Conn   sip.Connection

	GBVersion string
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

	cmdType := strings.TrimSpace(req.CmdType)
	if !isSupportedSubscribeCmdType(cmdType) {
		ctx.String(400, "unsupported subscribe cmd_type")
		return
	}

	expires, err := parseSubscribeExpires(ctx.GetHeader("Expires"))
	if err != nil {
		ctx.String(400, err.Error())
		return
	}
	deviceID := strings.TrimSpace(req.DeviceID)
	if deviceID == "" {
		deviceID = "*"
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
	if expires == 0 {
		// Expires=0 为退订。
		g.eventSubscribers.Delete(key)
		ctx.String(200, "OK")
		return
	}

	sub := &eventSubscription{
		Key:       key,
		CmdType:   cmdType,
		DeviceID:  deviceID,
		ExpiresAt: time.Now().Add(time.Duration(expires) * time.Second),
		To:        targetAddr.Clone(),
		Source:    ctx.Source,
		Conn:      ctx.Request.GetConnection(),
		GBVersion: ctx.XGBVer,
	}
	g.eventSubscribers.Store(key, sub)
	ctx.String(200, "OK")
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
		if now.After(sub.ExpiresAt) {
			g.eventSubscribers.Delete(key)
			return true
		}
		if !strings.EqualFold(sub.CmdType, cmdType) {
			return true
		}
		if sub.DeviceID != "*" && sub.DeviceID != "" && sub.DeviceID != deviceID {
			return true
		}
		if err := g.sendEventNotify(sub, cmdType, body); err != nil {
			// 这里不删除订阅，避免临时网络抖动导致订阅丢失。
			slog.Warn("send event notify failed", "cmdType", cmdType, "deviceID", deviceID, "err", err)
		}
		return true
	})
}

func (g *GB28181API) sendEventNotify(sub *eventSubscription, _ string, body []byte) error {
	target := &subscriptionTarget{
		to:        sub.To,
		source:    sub.Source,
		conn:      sub.Conn,
		gbVersion: sub.GBVersion,
	}
	expires := int(time.Until(sub.ExpiresAt).Seconds())
	state := "active"
	if expires <= 0 {
		state = "terminated;reason=timeout"
	} else {
		state = fmt.Sprintf("active;expires=%d", expires)
	}
	tx, err := g.svr.wrapRequest(target, sip.MethodNotify, &sip.ContentTypeXML, body, func(r *sip.Request) {
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: state})
	})
	if err != nil {
		return err
	}
	_, err = sipResponse(tx)
	return err
}

// startEventSubscriberCleaner 定时清理过期订阅，避免无事件期间缓存积累。
func (g *GB28181API) startEventSubscriberCleaner() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		g.eventSubscribers.Range(func(key, value any) bool {
			sub, ok := value.(*eventSubscription)
			if !ok || sub == nil || now.After(sub.ExpiresAt) {
				g.eventSubscribers.Delete(key)
			}
			return true
		})
	}
}
