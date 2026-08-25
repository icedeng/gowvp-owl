package gbs

import (
	"context"
	"strings"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
)

// QueryDeviceInfo 设备信息查询请求
// GB/T28181 81 页 A.2.4.4
func (g *GB28181API) QueryDeviceInfo(ctx *sip.Context) {
	tx, err := ctx.SendRequest(sip.MethodMessage, sip.GetDeviceInfoXML(ctx.DeviceID))
	if err != nil {
		ctx.Log.Error("sipDeviceInfo", "err", err)
		return
	}
	if _, err := sipResponse(tx); err != nil {
		ctx.Log.Error("sipResponse", "err", err)
		return
	}
}

// MessageDeviceInfoResponse 设备信息查询应答结构
type MessageDeviceInfoResponse struct {
	CmdType      string `xml:"CmdType"`
	SN           int    `xml:"SN"`
	DeviceID     string `xml:"DeviceID"`     // 目标设备的编码(必选)
	DeviceName   string `xml:"DeviceName"`   // 目标设备的名称(可选
	Manufacturer string `xml:"Manufacturer"` // 设备生产商(可选)
	Model        string `xml:"Model"`        // 设备型号(可选)
	Firmware     string `xml:"Firmware"`     // 设备固件版本(可选)
	Result       string `xml:"Result"`       // 査询结果(必选)
}

// isResultOK 判定 DeviceInfo 应答是否成功
// 为什么: 部分厂商不严格按协议返回 Result，可能为空串或大小写不一；空串按成功处理，维持与历史行为兼容。
func (m *MessageDeviceInfoResponse) isResultOK() bool {
	r := strings.TrimSpace(m.Result)
	return r == "" || strings.EqualFold(r, "OK")
}

// sipMessageDeviceInfo 设备信息查询应答
// GB/T28181 91 页 A.2.6.5
func (g *GB28181API) sipMessageDeviceInfo(ctx *sip.Context) {
	var msg MessageDeviceInfoResponse
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.Log.Error("sipMessageDeviceInfo", "err", err, "body", string(ctx.Request.Body()))
		ctx.String(400, ErrXMLDecode.Error())
		return
	}

	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	ctx.DeviceID = strings.TrimSpace(ctx.DeviceID)
	isChannelResponse := false
	// NVR 可代表其子通道返回 DeviceInfo。仅接受当前注册设备内已知通道，防止越权覆盖其他设备。
	if msg.DeviceID != "" && msg.DeviceID != ctx.DeviceID {
		if g.svr != nil && g.svr.memoryStorer != nil {
			_, isChannelResponse = g.svr.memoryStorer.GetChannel(ctx.DeviceID, msg.DeviceID)
		}
	}
	if msg.DeviceID != "" && msg.DeviceID != ctx.DeviceID && !isChannelResponse {
		ctx.Log.Error("sipMessageDeviceInfo device id mismatch", "body_device_id", msg.DeviceID, "ctx_device_id", ctx.DeviceID)
		ctx.String(400, "device id mismatch")
		return
	}

	// 为什么: Result 非 OK 代表设备端查询失败，可选字段可能为空或旧值，不应覆盖数据库，避免清空已有厂商/型号等信息。
	if !msg.isResultOK() {
		ctx.Log.Warn("sipMessageDeviceInfo result not ok", "result", msg.Result, "sn", msg.SN)
		g.resolvePendingDeviceQuery(ctx.DeviceID, msg.CmdType, msg.SN, msg.Result, ctx.Request.Body(), msg.DeviceID)
		ctx.String(200, "OK")
		return
	}

	if isChannelResponse {
		if g.core.Store() == nil {
			ctx.String(500, ErrDatabase.Error())
			return
		}
		var channel ipc.Channel
		err := g.core.Store().Channel().Update(context.Background(), &channel, func(item *ipc.Channel) error {
			if msg.DeviceName != "" {
				item.Name = msg.DeviceName
				item.Ext.Name = msg.DeviceName
			}
			if msg.Firmware != "" {
				item.Ext.Firmware = msg.Firmware
			}
			if msg.Manufacturer != "" {
				item.Ext.Manufacturer = msg.Manufacturer
			}
			if msg.Model != "" {
				item.Ext.Model = msg.Model
			}
			return nil
		}, orm.Where("device_id = ? AND channel_id = ?", ctx.DeviceID, msg.DeviceID))
		if err != nil {
			ctx.Log.Error("update channel DeviceInfo", "err", err, "channel_id", msg.DeviceID)
			ctx.String(500, ErrDatabase.Error())
			return
		}
	} else if err := g.core.Update(ctx.DeviceID, func(d *ipc.Device) {
		// 为什么: 可选字段为空时不覆盖，避免把上一次成功拿到的信息抹成空串。
		if msg.Firmware != "" {
			d.Ext.Firmware = msg.Firmware
		}
		if msg.Manufacturer != "" {
			d.Ext.Manufacturer = msg.Manufacturer
		}
		if msg.Model != "" {
			d.Ext.Model = msg.Model
		}
		if msg.DeviceName != "" {
			d.Ext.Name = msg.DeviceName
		}

		d.Address = ctx.Source.String()
		d.Transport = ctx.Source.Network()
	}); err != nil {
		ctx.Log.Error("Edit", "err", err)
		ctx.String(500, ErrDatabase.Error())
		return
	}

	// 命中通用查询等待队列（A.2.4 DeviceInfo 查询等待）。
	g.resolvePendingDeviceQuery(ctx.DeviceID, msg.CmdType, msg.SN, msg.Result, ctx.Request.Body(), msg.DeviceID)
	ctx.String(200, "OK")
	// 9.11 事件源侧：设备信息变化通知。
	g.publishEventNotify(msg.CmdType, ctx.DeviceID, ctx.Request.Body())
}
