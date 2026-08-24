package gbs

import (
	"context"
	"encoding/xml"
	"log/slog"
	"strings"
	"time"

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
	Info     struct {
		DeviceIDs []string `xml:"DeviceID"`
	} `xml:"Info"`
}

func (g *GB28181API) sipMessageKeepalive(ctx *sip.Context) {
	var msg MessageNotify
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.Log.Error("Message Unmarshal xml err", "err", err)
		ctx.String(400, ErrXMLDecode.Error())
		return
	}

	// 程序重启后内存丢失，收到 keepalive 时补上；首次补载后还需主动加载 Catalog。
	_, alreadyLoaded := g.svr.memoryStorer.Load(ctx.DeviceID)
	g.svr.memoryStorer.LoadOrStore(ctx.DeviceID, &Device{
		conn:   ctx.Request.GetConnection(),
		source: ctx.Source,
		to:     ctx.To,
	})

	effectiveVersion := GBVersion10
	var disabledCapabilities []string
	if err := g.svr.memoryStorer.Change(ctx.DeviceID, func(d *ipc.Device) error {
		d.KeepaliveAt = orm.Now()
		// 兼容省略 Status 的厂商，同时保留显式 OFF/ERROR 状态的语义。
		d.IsOnline = msg.Status == "" || msg.Status == "OK" || msg.Status == "ON"
		d.Address = ctx.Source.String()
		d.Transport = ctx.Source.Network()
		effectiveVersion = applyGBProtocolVersion(&d.Ext, ctx.XGBVer)
		disabledCapabilities = append(disabledCapabilities[:0], d.Ext.GBDisabledCapabilities...)
		return nil
	}, func(d *Device) {
		d.conn = ctx.Request.GetConnection()
		d.source = ctx.Source
		d.to = ctx.To
		d.setGBProfile(effectiveVersion, disabledCapabilities)
	}); err != nil {
		ctx.Log.Error("keepalive", "err", err)
	}
	if history := g.core.DeviceHistory(); history != nil {
		if err := history.Record(context.TODO(), ctx.DeviceID, ipc.DeviceHistoryHeartbeat, ctx.Source.String(), msg.Status, time.Now()); err != nil {
			ctx.Log.Error("持久化设备心跳历史失败", "err", err)
		}
	}

	// 9.6 状态信息报送：将心跳状态同步为结构化设备状态并推送订阅者。
	status := &DeviceStatusData{
		CmdType:        "DeviceStatus",
		SN:             msg.SN,
		DeviceID:       ctx.DeviceID,
		Status:         msg.Status,
		FaultDeviceIDs: normalizeGBIDList(msg.Info.DeviceIDs),
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

	// QueryCatalog 会检查在线状态，因此必须放在 Change(IsOnline=true) 之后。
	if !alreadyLoaded {
		slog.Info("keepalive 触发 Catalog 补载", "device_id", ctx.DeviceID)
		_ = g.QueryCatalog(ctx.DeviceID)
	}

	ctx.String(200, "OK")
}

func normalizeGBIDList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
