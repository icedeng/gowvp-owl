package gbs

import (
	"encoding/xml"
	"log/slog"
	"net"

	"github.com/gowvp/owl/pkg/gbs/sip"
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

	for _, d := range msg.Item {
		d.DeviceID = msg.DeviceID
		g.catalog.Write(&sip.CollectorMsg[Channels]{
			Key:   d.DeviceID,
			Data:  &d,
			Total: msg.SumNum,
		})

		// channel := Channels{ChannelID: d.ChannelID, DeviceID: message.DeviceID}
		// if err := db.Get(db.DBClient, &channel); err == nil {
		// 	channel.Active = time.Now().Unix()
		// 	channel.URIStr = fmt.Sprintf("sip:%s@%s", d.ChannelID, _sysinfo.Region)
		// 	channel.Status = transDeviceStatus(d.Status)
		// 	channel.Name = d.Name
		// 	channel.Manufacturer = d.Manufacturer
		// 	channel.Model = d.Model
		// 	channel.Owner = d.Owner
		// 	channel.CivilCode = d.CivilCode
		// 	// Address ip地址
		// 	channel.Address = d.Address
		// 	channel.Parental = d.Parental
		// 	channel.SafetyWay = d.SafetyWay
		// 	channel.RegisterWay = d.RegisterWay
		// 	channel.Secrecy = d.Secrecy
		// 	db.Save(db.DBClient, &channel)
		// 	go notify(notifyChannelsActive(channel))
		// } else {
		// 	// logrus.Infoln("deviceid not found,deviceid:", d.DeviceID, "pdid:", message.DeviceID, "err", err)
		// }
	}

	// 命中通用查询等待队列（A.2.4 Catalog 查询等待）。
	g.resolvePendingDeviceQuery(ctx.DeviceID, msg.CmdType, msg.SN, "", ctx.Request.Body(), msg.DeviceID)
	// 9.11 事件源侧：目录变化事件推送给订阅方。
	g.publishEventNotify("Catalog", ctx.DeviceID, ctx.Request.Body())

	ctx.String(200, "OK")
}

// QueryCatalog 设备目录查询或订阅请求
// GB/T28181 81 页 A.2.4.3
func (g *GB28181API) QueryCatalog(deviceID string) error {
	slog.Debug("QueryCatalog", "deviceID", deviceID)
	ipc, ok := g.svr.memoryStorer.Load(deviceID)
	if !ok || !ipc.IsOnline {
		return ErrDeviceOffline
	}

	_, err := g.svr.wrapRequest(ipc, sip.MethodMessage, &sip.ContentTypeXML, sip.GetCatalogXML(deviceID))
	if err != nil {
		return err
	}

	g.catalog.Run(deviceID)
	g.catalog.Wait(deviceID)
	return nil
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
		switch v.GBVersion() {
		case "2011":
			hb.SetXGBVerValue("1.0")
		case "2016":
			hb.SetXGBVerValue("2.0")
		case "2022":
			hb.SetXGBVerValue("3.0")
		default:
			// 未知版本保持默认 3.0，兼容 2022 设备。
			hb.SetXGBVerValue("3.0")
		}
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
