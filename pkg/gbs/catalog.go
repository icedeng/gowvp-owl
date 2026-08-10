package gbs

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
	"unicode"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
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
	XMLName  xml.Name   `xml:"Response"`
	CmdType  string     `xml:"CmdType"`
	SN       int        `xml:"SN"`
	DeviceID string     `xml:"DeviceID"`
	SumNum   int        `xml:"SumNum"`
	Item     []Channels `xml:"DeviceList>Item"`
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
	for _, r := range value {
		if !unicode.IsDigit(r) {
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
	if msg.SumNum < 0 {
		ctx.String(200, "OK")
		return
	}

	for index := range msg.Item {
		msg.Item[index].DeviceID = msg.DeviceID
	}
	if g.catalogResponses != nil {
		key := buildMultiResponseKey(ctx.DeviceID, "Catalog", msg.SN)
		g.catalogResponses.Add(key, msg.SumNum, msg.Item)
	}

	// 命中通用查询等待队列（A.2.4 Catalog 查询等待）。
	g.resolvePendingDeviceQuery(ctx.DeviceID, msg.CmdType, msg.SN, "", ctx.Request.Body(), msg.DeviceID)
	// 9.11 事件源侧：目录变化事件推送给订阅方。
	g.publishEventNotify("Catalog", ctx.DeviceID, ctx.Request.Body())

	ctx.String(200, "OK")
}

// QueryCatalog 设备目录查询或订阅请求
// GB/T28181 81 页 A.2.4.3
func (g *GB28181API) QueryCatalog(deviceID string) (err error) {
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
	if !ok || !ipc.IsOnline {
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
	tx, err := g.svr.wrapRequest(ipc, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		g.catalogResponses.Cancel(key)
		return err
	}
	if _, err = sipResponse(tx); err != nil {
		g.catalogResponses.Cancel(key)
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := g.catalogResponses.Wait(ctx, key)
	if len(result.Items) == 0 && !result.Complete {
		return fmt.Errorf("wait Catalog response timeout")
	}
	if !result.Complete {
		g.metrics.catalogPartial.Add(1)
		slog.Warn("Catalog response incomplete", "device_id", deviceID, "received", len(result.Items), "expected", result.Expected)
	}
	g.saveCatalogChannels(deviceID, result.Items)
	return nil
}

func (g *GB28181API) saveCatalogChannels(deviceID string, items []Channels) {
	if len(items) == 0 {
		return
	}
	if device, ok := g.svr.memoryStorer.Load(deviceID); ok {
		for _, item := range items {
			channel := &Channel{ChannelID: item.ChannelID, device: device}
			channel.init(g.cfg.Domain)
			device.Channels.Store(channel.ChannelID, channel)
		}
	}
	out := make([]*ipc.Channel, 0, len(items))
	for _, item := range items {
		out = append(out, &ipc.Channel{
			DeviceID:  deviceID,
			ChannelID: item.ChannelID,
			Name:      item.Name,
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

func (s *Server) wrapRequest(t Targeter, method string, contentType *sip.ContentType, body []byte, opts ...RequestOption) (*sip.Transaction, error) {
	to := t.To()
	conn := t.Conn()
	source := t.Source()

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

	return s.Request(req)
}
