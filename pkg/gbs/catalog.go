package gbs

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
)

type GBCatalogItemKind = string

const (
	GBCatalogItemUnknown             GBCatalogItemKind = "unknown"
	GBCatalogItemAdministrative      GBCatalogItemKind = "administrative"
	GBCatalogItemSystem              GBCatalogItemKind = "system"
	GBCatalogItemBusinessGroup       GBCatalogItemKind = "business_group"
	GBCatalogItemVirtualOrganization GBCatalogItemKind = "virtual_organization"
	GBCatalogItemDevice              GBCatalogItemKind = "device"
)

// MessageDeviceListResponse 设备明细列表返回结构
type MessageDeviceListResponse struct {
	XMLName   xml.Name
	CmdType   string     `xml:"CmdType"`
	SN        int        `xml:"SN"`
	DeviceID  string     `xml:"DeviceID"`
	SumNum    int        `xml:"SumNum"`
	HasSumNum bool       `xml:"-"`
	Item      []Channels `xml:"-"`
	ListNum   *int       `xml:"-"`
	HasList   bool       `xml:"-"`
}

func (m *MessageDeviceListResponse) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	var value struct {
		CmdType  string `xml:"CmdType"`
		SN       int    `xml:"SN"`
		DeviceID string `xml:"DeviceID"`
		SumNum   *int   `xml:"SumNum"`
		List     *struct {
			Num  *int       `xml:"Num,attr"`
			Item []Channels `xml:"Item"`
		} `xml:"DeviceList"`
	}
	if err := decoder.DecodeElement(&value, &start); err != nil {
		return err
	}
	*m = MessageDeviceListResponse{
		XMLName: start.Name, CmdType: value.CmdType, SN: value.SN, DeviceID: value.DeviceID,
	}
	if value.SumNum != nil {
		m.SumNum, m.HasSumNum = *value.SumNum, true
	}
	if value.List != nil {
		m.HasList, m.ListNum, m.Item = true, value.List.Num, value.List.Item
	}
	return nil
}

func classifyGBCatalogItem(deviceID string) GBCatalogItemKind {
	deviceID = strings.TrimSpace(deviceID)
	if !allDecimalDigits(deviceID) {
		return GBCatalogItemUnknown
	}
	switch len(deviceID) {
	case 2, 4, 6, 8:
		return GBCatalogItemAdministrative
	case 20:
		typeCode := deviceID[10:13]
		switch typeCode {
		case "200":
			return GBCatalogItemSystem
		case "215":
			return GBCatalogItemBusinessGroup
		case "216":
			return GBCatalogItemVirtualOrganization
		default:
			return GBCatalogItemDevice
		}
	default:
		return GBCatalogItemUnknown
	}
}

func allDecimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func catalogChannelExt(item Channels) ipc.DeviceExt {
	return ipc.DeviceExt{
		Manufacturer: item.Manufacturer,
		Model:        item.Model,
		GBCatalog: &ipc.GBCatalogExt{
			Kind:            classifyGBCatalogItem(item.ChannelID),
			Owner:           item.Owner,
			CivilCode:       item.CivilCode,
			Block:           item.Block,
			Address:         item.Address,
			Parental:        item.Parental,
			ParentID:        item.ParentID,
			SafetyWay:       item.SafetyWay,
			RegisterWay:     item.RegisterWay,
			CertNum:         item.CertNum,
			Certifiable:     item.Certifiable,
			ErrCode:         item.ErrCode,
			EndTime:         item.EndTime,
			Secrecy:         item.Secrecy,
			IPAddress:       item.IPAddress,
			Port:            item.Port,
			Password:        item.Password,
			Status:          item.Status,
			Longitude:       item.Longitude,
			Latitude:        item.Latitude,
			PTZType:         item.Info.PTZType,
			PositionType:    item.Info.PositionType,
			RoomType:        item.Info.RoomType,
			UseType:         item.Info.UseType,
			SupplyLightType: item.Info.SupplyLightType,
			DirectionType:   item.Info.DirectionType,
			Resolution:      item.Info.Resolution,
			BusinessGroupID: item.Info.BusinessGroupID,
			RawXML:          "<Item>" + item.RawXML + "</Item>",
			InfoRawXML:      item.Info.RawXML,
		},
	}
}

// sipMessageCatalog 设备目录信息查询应答
// GB/T28181 90 页 A.2.6.4
func (g *GB28181API) sipMessageCatalog(ctx *sip.Context) {
	var msg MessageDeviceListResponse
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		slog.Error("Message Unmarshal xml", "err", err)
		ctx.String(400, "xml err")
		return
	}
	msg.CmdType = strings.TrimSpace(msg.CmdType)
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	if err := g.validateCatalogEnvelope(ctx, msg, false); err != nil {
		ctx.String(400, err.Error())
		return
	}

	for index := range msg.Item {
		msg.Item[index].DeviceID = msg.DeviceID
		msg.Item[index].ChannelID = strings.TrimSpace(msg.Item[index].ChannelID)
	}
	if g.catalogResponses != nil {
		key := buildMultiResponseKey(ctx.DeviceID, "Catalog", msg.SN)
		g.catalogResponses.Add(key, msg.SumNum, msg.Item)
	}

	// 命中通用查询等待队列（A.2.4 Catalog 查询等待）。
	stateDeviceID := firstNonEmpty(msg.DeviceID, strings.TrimSpace(ctx.DeviceID))
	decoded := g.decodeAndStoreQueryResult(stateDeviceID, msg.CmdType, ctx.Request.Body())
	g.resolvePendingDeviceQueryResult(ctx.DeviceID, msg.CmdType, msg.SN, "", ctx.Request.Body(), msg.DeviceID, decoded)

	ctx.String(200, "OK")
	g.persistDecodedQuery(stateDeviceID, msg.CmdType, decoded)
}

func (g *GB28181API) validateCatalogEnvelope(ctx *sip.Context, msg MessageDeviceListResponse, notification bool) error {
	if msg.XMLName.Local != "Response" && msg.XMLName.Local != "Notify" {
		return fmt.Errorf("Catalog root must be Response or Notify")
	}
	if !strings.EqualFold(strings.TrimSpace(msg.CmdType), "Catalog") {
		return fmt.Errorf("invalid Catalog command")
	}
	if msg.SN <= 0 || !isGBDeviceIdentifier(strings.TrimSpace(msg.DeviceID)) {
		return fmt.Errorf("Catalog requires positive SN and 20-digit DeviceID")
	}
	if !msg.HasSumNum || msg.SumNum < 0 {
		return fmt.Errorf("Catalog SumNum must not be negative")
	}
	if !msg.HasList {
		if msg.SumNum != 0 {
			return fmt.Errorf("Catalog DeviceList is required for non-empty results")
		}
	} else if msg.ListNum == nil || *msg.ListNum < 0 || *msg.ListNum != len(msg.Item) || len(msg.Item) > msg.SumNum {
		return fmt.Errorf("invalid Catalog DeviceList count")
	}
	if notification && msg.HasList && *msg.ListNum != msg.SumNum {
		return fmt.Errorf("Catalog notification count mismatch")
	}
	for _, item := range msg.Item {
		if classifyGBCatalogItem(strings.TrimSpace(item.ChannelID)) == GBCatalogItemUnknown {
			return fmt.Errorf("invalid Catalog item DeviceID")
		}
	}
	if err := g.validateCatalogResponseTarget(ctx, msg.DeviceID); err != nil {
		return err
	}
	if !notification && g.pendingDeviceQueryTargetMismatch(ctx.DeviceID, msg.CmdType, msg.SN, msg.DeviceID) {
		return fmt.Errorf("Catalog response target mismatch")
	}
	if !notification && g.catalogResponses != nil && g.catalogResponses.Has(buildMultiResponseKey(ctx.DeviceID, "Catalog", msg.SN)) && msg.DeviceID != strings.TrimSpace(ctx.DeviceID) {
		return fmt.Errorf("Catalog aggregate target mismatch")
	}
	return nil
}

func (g *GB28181API) validateCatalogResponseTarget(ctx *sip.Context, targetID string) error {
	if err := g.validateAuthenticatedResponseTarget(ctx, targetID); err == nil {
		return nil
	}
	sourceID := ""
	if ctx != nil {
		sourceID = strings.TrimSpace(ctx.DeviceID)
	}
	targetID = strings.TrimSpace(targetID)
	if len(sourceID) != 20 || len(targetID) != 20 || sourceID[:10] != targetID[:10] {
		return fmt.Errorf("Catalog response target mismatch")
	}
	switch classifyGBCatalogItem(targetID) {
	case GBCatalogItemSystem, GBCatalogItemBusinessGroup, GBCatalogItemVirtualOrganization:
		return nil
	default:
		return fmt.Errorf("Catalog response target mismatch")
	}
}

// QueryCatalog 设备目录查询或订阅请求
// GB/T28181 81 页 A.2.4.3
func (g *GB28181API) QueryCatalog(deviceID string) (err error) {
	return g.QueryCatalogContext(context.Background(), deviceID)
}

// QueryCatalogContext 查询目录，并允许调用方取消 SIP 及分包聚合等待。
func (g *GB28181API) QueryCatalogContext(ctx context.Context, deviceID string) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	g.metrics.catalogRequests.Add(1)
	defer func() {
		if err == nil {
			g.metrics.catalogSuccess.Add(1)
		} else if strings.Contains(err.Error(), "Catalog response timeout") {
			g.metrics.catalogTimeouts.Add(1)
		}
	}()
	slog.Debug("QueryCatalog", "deviceID", deviceID)
	ipc, ok := g.svr.memoryStorer.Load(deviceID)
	if !ok || !ipc.IsOnlineNow() {
		return ErrDeviceOffline
	}

	sn := g.nextQuerySN()
	key := buildMultiResponseKey(deviceID, "Catalog", sn)
	g.catalogResponses.Start(key)
	body, err := sip.XMLEncode(genericDeviceQueryRequest{
		CmdType:  "Catalog",
		SN:       sn,
		DeviceID: deviceID,
	})
	if err != nil {
		g.catalogResponses.Cancel(key)
		return err
	}
	tx, err := g.svr.wrapRequestContext(ctx, ipc, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		g.catalogResponses.Cancel(key)
		return err
	}
	if _, err = sipResponseContext(ctx, tx); err != nil {
		g.catalogResponses.Cancel(key)
		return err
	}
	requestCtx := ctx
	waitCtx, cancel := context.WithTimeout(requestCtx, 10*time.Second)
	defer cancel()
	result := g.catalogResponses.Wait(waitCtx, key)
	if g.serviceStopped() {
		return ErrServiceStopped
	}
	if !result.Complete && requestCtx.Err() != nil {
		return requestCtx.Err()
	}
	if len(result.Items) == 0 && !result.Complete {
		return fmt.Errorf("wait Catalog response timeout")
	}
	if !result.Complete {
		g.metrics.catalogPartial.Add(1)
		slog.Warn("Catalog response incomplete", "device_id", deviceID, "received", len(result.Items), "expected", result.Expected)
	}
	g.persistCatalogResult(deviceID, result)
	return nil
}

// persistCatalogResult 只使用完整目录替换本地快照，避免丢包时把未收到的通道错误标记为离线。
func (g *GB28181API) persistCatalogResult(deviceID string, result multiResponseResult[Channels]) {
	if !result.Complete {
		return
	}
	g.saveCatalogChannels(deviceID, result.Items)
}

func (g *GB28181API) saveCatalogChannels(deviceID string, items []Channels) {
	for index := range items {
		items[index].ChannelID = strings.TrimSpace(items[index].ChannelID)
	}
	cfg := g.configSnapshot()
	domain := ""
	if cfg != nil {
		domain = cfg.GetDomain()
	}
	if device, ok := g.svr.memoryStorer.Load(deviceID); ok {
		channelDomain := domain
		if device.To() != nil && device.To().URI != nil && device.To().URI.Host() != "" {
			channelDomain = device.To().URI.Host()
		}
		// 完整目录先整体校验，再替换运行时快照，避免中途失败造成部分提交。
		for _, item := range items {
			channel := &Channel{ChannelID: item.ChannelID, device: device}
			if err := channel.init(channelDomain); err != nil {
				slog.Warn("reject invalid GB28181 Catalog snapshot", "device_id", deviceID, "channel_id", item.ChannelID, "err", err)
				return
			}
		}
		seen := make(map[string]struct{}, len(items))
		for _, item := range items {
			seen[item.ChannelID] = struct{}{}
			channel := &Channel{ChannelID: item.ChannelID, device: device}
			if err := channel.init(channelDomain); err != nil {
				return
			}
			device.Channels.Store(channel.ChannelID, channel)
		}
		device.Channels.Range(func(channelID string, _ *Channel) bool {
			if _, ok := seen[channelID]; !ok {
				device.Channels.Delete(channelID)
			}
			return true
		})
	}
	if len(items) == 0 {
		if g.core.Store() != nil {
			if err := g.core.Store().Channel().BatchEdit(context.Background(), "is_online", false, orm.Where("device_id = ?", deviceID)); err != nil {
				slog.Error("mark Catalog channels offline", "device_id", deviceID, "err", err)
			}
			var device ipc.Device
			if err := g.core.Store().Device().Update(context.Background(), &device, func(current *ipc.Device) error {
				current.Channels = 0
				return nil
			}, orm.Where("device_id = ?", deviceID)); err != nil {
				slog.Error("reset Catalog channel count", "device_id", deviceID, "err", err)
			}
		}
		g.startLifecycleTask(context.Background(), g.notifyCascadeCatalog)
		return
	}
	out := make([]*ipc.Channel, 0, len(items))
	for _, item := range items {
		out = append(out, &ipc.Channel{
			DeviceID:  deviceID,
			ChannelID: item.ChannelID,
			Name:      item.Name,
			PTZType:   item.Info.PTZType,
			IsOnline:  item.Status == "OK" || item.Status == "ON",
			Ext:       catalogChannelExt(item),
			Type:      ipc.TypeGB28181,
		})
	}
	if g.core.Store() != nil {
		if err := g.core.SaveChannels(out); err != nil {
			slog.Error("SaveChannels", "err", err)
		}
	}
	g.startLifecycleTask(context.Background(), g.notifyCascadeCatalog)
}

type Targeter interface {
	To() *sip.Address
	Conn() sip.Connection
	Source() net.Addr
}

type gbVersioner interface {
	GBVersion() string
}

type RequestOption func(*sip.Request)

func requestTargetSnapshot(target Targeter) (*sip.Address, sip.Connection, net.Addr) {
	switch value := target.(type) {
	case *Device:
		state := value.runtimeSnapshot()
		return state.To, state.Conn, state.Source
	case *Channel:
		if value == nil || value.device == nil {
			return nil, nil, nil
		}
		state := value.device.runtimeSnapshot()
		var to *sip.Address
		if value.to != nil {
			to = value.to.Clone()
		}
		return to, state.Conn, state.Source
	default:
		return target.To(), target.Conn(), target.Source()
	}
}

// prepareDialogRequestTransport 优先复用建立对话的响应路径，仅在旧响应缺少传输元数据时回退注册链路。
func prepareDialogRequestTransport(request *sip.Request, target Targeter) error {
	if request == nil {
		return fmt.Errorf("SIP dialog request is unavailable")
	}
	var connection sip.Connection
	var destination net.Addr
	if target != nil {
		_, connection, destination = requestTargetSnapshot(target)
	}
	// TCP/TLS 注册可能已重连；发送连接取当前运行态，对话 Request-URI/Route 仍由原响应决定。
	if connection != nil && (request.GetConnection() == nil || !strings.EqualFold(request.Transport(), "UDP")) {
		request.SetConnection(connection)
	}
	if request.GetConnection() == nil {
		return fmt.Errorf("SIP dialog connection is unavailable")
	}
	if request.Destination() == nil {
		if destination == nil {
			return fmt.Errorf("SIP dialog destination is unavailable")
		}
		request.SetDestination(destination)
	}
	return nil
}

func (s *Server) wrapRequest(t Targeter, method string, contentType *sip.ContentType, body []byte, opts ...RequestOption) (*sip.Transaction, error) {
	return s.wrapRequestContext(context.Background(), t, method, contentType, body, opts...)
}

func (s *Server) wrapRequestContext(ctx context.Context, t Targeter, method string, contentType *sip.ContentType, body []byte, opts ...RequestOption) (*sip.Transaction, error) {
	if s == nil || s.Server == nil || s.gb == nil {
		return nil, fmt.Errorf("SIP server is unavailable")
	}
	if t == nil {
		return nil, fmt.Errorf("SIP request target is unavailable")
	}
	if s.gb.serviceStopped() {
		return nil, ErrServiceStopped
	}
	to, conn, source := requestTargetSnapshot(t)
	if to == nil || to.URI == nil || strings.TrimSpace(to.URI.Host()) == "" {
		return nil, fmt.Errorf("SIP request target URI is unavailable")
	}
	if conn == nil {
		return nil, fmt.Errorf("SIP request target connection is unavailable")
	}
	if source == nil {
		return nil, fmt.Errorf("SIP request target address is unavailable")
	}

	hb := sip.NewHeaderBuilder().
		SetTo(to).
		SetFrom(&s.fromAddress).
		SetContentType(contentType).
		SetMethod(method).
		SetContact(&s.fromAddress).
		AddVia(&sip.ViaHop{
			Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
		})

	if v, ok := t.(gbVersioner); ok {
		version, valid := ParseGBProtocolVersion(v.GBVersion())
		if !valid {
			// 未知设备使用保守的 1.0 档案，避免发送设备无法识别的扩展。
			version = GBVersion10
		}
		hb.SetXGBVerValue(string(version))
	}

	req := sip.NewRequest("", method, to.URI, sip.DefaultSipVersion, hb.Build(), body)
	req.SetConnection(conn)
	req.SetSource(source)
	req.SetDestination(source)

	for _, opt := range opts {
		opt(req)
	}
	if err := applyForwardedMonitorUserIdentity(ctx, req); err != nil {
		return nil, err
	}

	security, err := s.gb.newSignalDigestSecurity(targetSignalDigestSeed(t))
	if err != nil {
		return nil, err
	}
	return s.RequestWithSecurity(req, security)
}
