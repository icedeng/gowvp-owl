package gbs

import (
	"context"
	"encoding/xml"
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
	XMLName       xml.Name
	CmdType       string `xml:"CmdType"`
	SN            int    `xml:"SN"`
	DeviceID      string `xml:"DeviceID"`   // 目标设备的编码(必选)
	DeviceName    string `xml:"DeviceName"` // 目标设备的名称(2014+ 可选)
	HasDeviceName bool   `xml:"-"`
	Manufacturer  string `xml:"Manufacturer"` // 设备生产商(可选)
	Model         string `xml:"Model"`        // 设备型号(可选)
	Firmware      string `xml:"Firmware"`     // 设备固件版本(可选)
	Result        string `xml:"Result"`       // 査询结果(必选)
	Channel       *int   `xml:"Channel"`      // 视频输入通道数(可选)
	MaxCamera     *int   `xml:"MaxCamera"`    // 标准示例兼容字段
	MaxAlarm      *int   `xml:"MaxAlarm"`     // 标准示例兼容字段
}

func (m *MessageDeviceInfoResponse) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	var value struct {
		CmdType      string  `xml:"CmdType"`
		SN           int     `xml:"SN"`
		DeviceID     string  `xml:"DeviceID"`
		DeviceName   *string `xml:"DeviceName"`
		Manufacturer string  `xml:"Manufacturer"`
		Model        string  `xml:"Model"`
		Firmware     string  `xml:"Firmware"`
		Result       string  `xml:"Result"`
		Channel      *int    `xml:"Channel"`
		MaxCamera    *int    `xml:"MaxCamera"`
		MaxAlarm     *int    `xml:"MaxAlarm"`
	}
	if err := decoder.DecodeElement(&value, &start); err != nil {
		return err
	}
	*m = MessageDeviceInfoResponse{
		XMLName: start.Name, CmdType: value.CmdType, SN: value.SN, DeviceID: value.DeviceID,
		Manufacturer: value.Manufacturer, Model: value.Model, Firmware: value.Firmware, Result: value.Result,
		Channel: value.Channel, MaxCamera: value.MaxCamera, MaxAlarm: value.MaxAlarm,
	}
	if value.DeviceName != nil {
		m.DeviceName, m.HasDeviceName = *value.DeviceName, true
	}
	return nil
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

	msg.CmdType = strings.TrimSpace(msg.CmdType)
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	msg.Result = strings.TrimSpace(msg.Result)
	ctx.DeviceID = strings.TrimSpace(ctx.DeviceID)
	if msg.XMLName.Local != "Response" || !strings.EqualFold(msg.CmdType, "DeviceInfo") || !isGBResultValue(msg.Result) {
		ctx.String(400, "invalid DeviceInfo response")
		return
	}
	if err := g.validateGenericDeviceQueryResponse(ctx, genericDeviceQueryResponse{
		XMLName: msg.XMLName, CmdType: msg.CmdType, SN: msg.SN, DeviceID: msg.DeviceID, Result: msg.Result,
	}); err != nil {
		ctx.String(400, err.Error())
		return
	}
	version := g.getDeviceGBProtocolVersion(ctx.DeviceID)
	if msg.HasDeviceName && !version.AtLeast(GBVersion11) {
		ctx.String(400, "DeviceInfo DeviceName requires protocol 1.1")
		return
	}
	for _, count := range []*int{msg.Channel, msg.MaxCamera, msg.MaxAlarm} {
		if count != nil && *count < 0 {
			ctx.String(400, "DeviceInfo channel counts must not be negative")
			return
		}
	}
	isChannelResponse := msg.DeviceID != ctx.DeviceID

	// 为什么: Result 非 OK 代表设备端查询失败，可选字段可能为空或旧值，不应覆盖数据库，避免清空已有厂商/型号等信息。
	if !strings.EqualFold(msg.Result, "OK") {
		ctx.Log.Warn("sipMessageDeviceInfo result not ok", "result", msg.Result, "sn", msg.SN)
		stateDeviceID := firstNonEmpty(msg.DeviceID, ctx.DeviceID)
		decoded := g.decodeAndStoreQueryResult(stateDeviceID, msg.CmdType, ctx.Request.Body())
		g.resolvePendingDeviceQueryResult(ctx.DeviceID, msg.CmdType, msg.SN, msg.Result, ctx.Request.Body(), msg.DeviceID, decoded)
		ctx.String(200, "OK")
		g.persistDecodedQuery(stateDeviceID, msg.CmdType, decoded)
		return
	}

	var persist func() error
	if isChannelResponse {
		if g.core.Store() == nil {
			ctx.String(500, ErrDatabase.Error())
			return
		}
		persist = func() error {
			var channel ipc.Channel
			return g.core.Store().Channel().Update(context.Background(), &channel, func(item *ipc.Channel) error {
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
		}
	} else {
		persist = func() error {
			return g.core.Update(ctx.DeviceID, func(d *ipc.Device) {
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
				d.Transport = requestSignalingTransport(ctx)
			})
		}
	}

	// 命中通用查询等待队列（A.2.4 DeviceInfo 查询等待）。
	stateDeviceID := firstNonEmpty(msg.DeviceID, ctx.DeviceID)
	decoded := g.decodeAndStoreQueryResult(stateDeviceID, msg.CmdType, ctx.Request.Body())
	g.resolvePendingDeviceQueryResult(ctx.DeviceID, msg.CmdType, msg.SN, msg.Result, ctx.Request.Body(), msg.DeviceID, decoded)
	ctx.String(200, "OK")
	if err := persist(); err != nil {
		ctx.Log.Error("persist DeviceInfo", "err", err, "target_id", msg.DeviceID)
	}
	g.persistDecodedQuery(stateDeviceID, msg.CmdType, decoded)
	// 9.11 事件源侧：设备信息变化通知。
	g.publishEventNotify(msg.CmdType, ctx.DeviceID, ctx.Request.Body())
}
