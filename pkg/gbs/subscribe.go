package gbs

import (
	"context"
	"fmt"
	"strings"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

type SubscribeInput struct {
	DeviceID string
	Event    string // Alarm/Catalog/MobilePosition
	Expires  int
}

// Subscribe 事件订阅（9.11），通过 SUBSCRIBE 发送订阅请求。
func (g *GB28181API) Subscribe(_ context.Context, in *SubscribeInput) error {
	if in == nil || in.DeviceID == "" {
		return ErrDeviceNotExist
	}
	ipc, ok := g.svr.memoryStorer.Load(in.DeviceID)
	if !ok || !ipc.IsOnline {
		return ErrDeviceOffline
	}
	if in.Expires <= 0 {
		in.Expires = 3600
	}
	cmdType := in.Event
	if cmdType == "" {
		cmdType = "Alarm"
	}
	body := fmt.Appendf(nil, `<?xml version="1.0" encoding="GB2312"?>
<Query>
<CmdType>%s</CmdType>
<SN>%d</SN>
<DeviceID>%s</DeviceID>
</Query>
`, cmdType, sip.RandInt(100000, 999999), in.DeviceID)

	tx, err := g.svr.wrapRequest(ipc, sip.MethodSubscribe, &sip.ContentTypeXML, body, func(r *sip.Request) {
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: fmt.Sprintf("%d", in.Expires)})
	})
	if err != nil {
		return err
	}
	_, err = sipResponse(tx)
	return err
}

// sipNotifyCatalog 目录订阅通知，复用目录处理逻辑。
func (g *GB28181API) sipNotifyCatalog(ctx *sip.Context) {
	g.sipMessageCatalog(ctx)
}

// sipNotifyMobilePosition 位置订阅通知，目前仅应答 200，后续可扩展入库。
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
		g.decodeAndStoreQueryData(deviceID, cmdType, ctx.Request.Body())
		// 9.11 事件源侧：移动位置事件通知订阅方。
		g.publishEventNotify(cmdType, deviceID, ctx.Request.Body())
	}
	ctx.String(200, "OK")
}
