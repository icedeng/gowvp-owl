package gbs

import (
	"encoding/xml"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
	// "github.com/panjjo/gosip/db"
)

// MessageNotify 心跳包xml结构
type MessageNotify struct {
	CmdType  string `xml:"CmdType"`
	SN       int    `xml:"SN"`
	DeviceID string `xml:"DeviceID"`
	Status   string `xml:"Status"`
	Info     string `xml:"Info"`
}

func (g *GB28181API) sipMessageKeepalive(ctx *sip.Context) {
	var msg MessageNotify
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.Log.Error("Message Unmarshal xml err", "err", err)
		ctx.String(400, ErrXMLDecode.Error())
		return
	}

	// 程序重启时会丢内存，收到 keepalive 时，补上
	// 并未补充到
	g.svr.memoryStorer.LoadOrStore(ctx.DeviceID, &Device{
		conn:   ctx.Request.GetConnection(),
		source: ctx.Source,
		to:     ctx.To,
	})

	if err := g.svr.memoryStorer.Change(ctx.DeviceID, func(d *ipc.Device) error {
		d.KeepaliveAt = orm.Now()
		d.IsOnline = msg.Status == "OK" || msg.Status == "ON"
		d.Address = ctx.Source.String()
		d.Transport = ctx.Source.Network()
		if ctx.XGBVer != "" {
			d.Ext.GBVersion = ctx.XGBVer
		}
		return nil
	}, func(d *Device) {
		d.conn = ctx.Request.GetConnection()
		d.source = ctx.Source
		d.to = ctx.To
		if ctx.XGBVer != "" {
			d.gbVersion = ctx.XGBVer
		}
	}); err != nil {
		ctx.Log.Error("keepalive", "err", err)
	}

	// 9.6 状态信息报送：将心跳状态同步为结构化设备状态并推送订阅者。
	status := &DeviceStatusData{
		CmdType:  "DeviceStatus",
		SN:       msg.SN,
		DeviceID: ctx.DeviceID,
		Status:   msg.Status,
	}
	if msg.Status == "OK" || msg.Status == "ON" {
		status.Online = "ONLINE"
	} else {
		status.Online = "OFFLINE"
	}
	g.storeQueryState(ctx.DeviceID, "DeviceStatus", status)
	if body, err := sip.XMLEncode(struct {
		XMLName xml.Name `xml:"Notify"`
		*DeviceStatusData
	}{
		XMLName:          xml.Name{Local: "Notify"},
		DeviceStatusData: status,
	}); err == nil {
		g.publishEventNotify("DeviceStatus", ctx.DeviceID, body)
	}

	ctx.String(200, "OK")
}
