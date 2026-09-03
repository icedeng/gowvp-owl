package gbs

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
)

// QueryDeviceInfo 设备信息查询请求
// GB/T28181 81 页 A.2.4.4
func (g *GB28181API) QueryDeviceInfo(ctx *sip.Context) {
	if g == nil || g.svr == nil || ctx == nil {
		return
	}
	requestCtx := monitorUserIdentityContextWithParent(g.serviceContext(), ctx)
	if err := g.QueryDeviceInfoContext(requestCtx, ctx.DeviceID); err != nil {
		ctx.Log.Error("sipDeviceInfo", "err", err)
	}
}

func (g *GB28181API) QueryDeviceInfoContext(ctx context.Context, deviceID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if g == nil || g.svr == nil {
		return fmt.Errorf("GB28181 service is unavailable")
	}
	target := g.svr.dialogTarget(deviceID, "")
	if target == nil {
		return ErrDeviceNotExist
	}
	sn, cancelExpectation := g.reserveAutomaticQueryResponse(deviceID, "DeviceInfo", deviceID)
	operation, releaseOperation := g.trackPendingDeviceRequest(ctx, deviceID, deviceID)
	defer releaseOperation()
	requestCtx := operation.Context(ctx)
	tx, err := g.svr.wrapRequestContext(requestCtx, target, sip.MethodMessage, &sip.ContentTypeXML, sip.GetDeviceInfoXMLWithSN(deviceID, sn))
	if err != nil {
		cancelExpectation()
		return operation.ErrorOr(err)
	}
	if _, err := sipResponseContext(requestCtx, tx); err != nil {
		cancelExpectation()
		return operation.ErrorOr(err)
	}
	return nil
}

// MessageDeviceInfoResponse 设备信息查询应答结构
type MessageDeviceInfoResponse struct {
	XMLName         xml.Name
	CmdType         string             `xml:"CmdType"`
	SN              int                `xml:"SN"`
	DeviceID        string             `xml:"DeviceID"`   // 目标设备的编码(必选)
	DeviceName      string             `xml:"DeviceName"` // 目标设备的名称(2014+ 可选)
	HasDeviceName   bool               `xml:"-"`
	Manufacturer    string             `xml:"Manufacturer"` // 设备生产商(可选)
	HasManufacturer bool               `xml:"-"`
	Model           string             `xml:"Model"` // 设备型号(可选)
	HasModel        bool               `xml:"-"`
	Firmware        string             `xml:"Firmware"` // 设备固件版本(可选)
	HasFirmware     bool               `xml:"-"`
	Result          string             `xml:"Result"`    // 査询结果(必选)
	Channel         *int               `xml:"Channel"`   // 视频输入通道数(可选)
	MaxCamera       *int               `xml:"MaxCamera"` // 标准示例兼容字段
	MaxAlarm        *int               `xml:"MaxAlarm"`  // 标准示例兼容字段
	Info            []versionedInfoXML `xml:"Info"`
	ExtraInfo       []string           `xml:"ExtraInfo"`
}

func (m *MessageDeviceInfoResponse) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	var value struct {
		CmdType      string             `xml:"CmdType"`
		SN           int                `xml:"SN"`
		DeviceID     string             `xml:"DeviceID"`
		DeviceName   *string            `xml:"DeviceName"`
		Manufacturer *string            `xml:"Manufacturer"`
		Model        *string            `xml:"Model"`
		Firmware     *string            `xml:"Firmware"`
		Result       string             `xml:"Result"`
		Channel      *int               `xml:"Channel"`
		MaxCamera    *int               `xml:"MaxCamera"`
		MaxAlarm     *int               `xml:"MaxAlarm"`
		Info         []versionedInfoXML `xml:"Info"`
		ExtraInfo    []string           `xml:"ExtraInfo"`
	}
	if err := decoder.DecodeElement(&value, &start); err != nil {
		return err
	}
	*m = MessageDeviceInfoResponse{
		XMLName: start.Name, CmdType: value.CmdType, SN: value.SN, DeviceID: value.DeviceID,
		Result:  value.Result,
		Channel: value.Channel, MaxCamera: value.MaxCamera, MaxAlarm: value.MaxAlarm,
		Info: value.Info, ExtraInfo: value.ExtraInfo,
	}
	if value.DeviceName != nil {
		m.DeviceName, m.HasDeviceName = *value.DeviceName, true
	}
	if value.Manufacturer != nil {
		m.Manufacturer, m.HasManufacturer = *value.Manufacturer, true
	}
	if value.Model != nil {
		m.Model, m.HasModel = *value.Model, true
	}
	if value.Firmware != nil {
		m.Firmware, m.HasFirmware = *value.Firmware, true
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
	if err := validateDeviceInfoResponseStructure(ctx.Request.Body(), version); err != nil {
		ctx.String(400, err.Error())
		return
	}
	if msg.HasDeviceName && !version.AtLeast(GBVersion11) {
		ctx.String(400, "DeviceInfo DeviceName requires protocol 1.1")
		return
	}
	if err := validateVersionedInfo(version, "DeviceInfo", msg.Info, msg.ExtraInfo); err != nil {
		ctx.String(400, err.Error())
		return
	}
	for _, count := range []*int{msg.Channel, msg.MaxCamera, msg.MaxAlarm} {
		if count != nil && *count < 0 {
			ctx.String(400, "DeviceInfo channel counts must not be negative")
			return
		}
	}
	extended, err := g.validateAndDecodeAppendixA4(ctx.DeviceID, msg.CmdType, ctx.Request.Body())
	if err != nil {
		ctx.String(400, err.Error())
		return
	}
	if _, ok := g.pendingDeviceQueryExpectedTarget(ctx.DeviceID, msg.CmdType, msg.SN); !ok {
		ctx.Log.Warn("ignore unassociated DeviceInfo response", "sn", msg.SN, "target_id", msg.DeviceID)
		ctx.String(200, "OK")
		return
	}
	isChannelResponse := msg.DeviceID != ctx.DeviceID

	// 为什么: Result 非 OK 代表设备端查询失败，可选字段可能为空或旧值，不应覆盖数据库，避免清空已有厂商/型号等信息。
	if !strings.EqualFold(msg.Result, "OK") {
		ctx.Log.Warn("sipMessageDeviceInfo result not ok", "result", msg.Result, "sn", msg.SN)
		if err := ctx.RespondString(200, "OK"); err != nil {
			ctx.Log.Error("respond DeviceInfo", "err", err, "sn", msg.SN, "target_id", msg.DeviceID)
			return
		}
		unlockCommit, err := g.lockAdmittedInboundDeviceStateCommit(ctx)
		if err != nil {
			return
		}
		defer unlockCommit()
		g.resolvePendingDeviceQueryResult(ctx.DeviceID, msg.CmdType, msg.SN, msg.Result, ctx.Request.Body(), msg.DeviceID, decodedDeviceQuery{})
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
			return g.core.Store().Channel().Update(g.serviceContext(), &channel, func(item *ipc.Channel) error {
				if msg.HasDeviceName {
					item.Name = msg.DeviceName
					item.Ext.Name = msg.DeviceName
				}
				if msg.HasFirmware {
					item.Ext.Firmware = msg.Firmware
				}
				if msg.HasManufacturer {
					item.Ext.Manufacturer = msg.Manufacturer
				}
				if msg.HasModel {
					item.Ext.Model = msg.Model
				}
				return nil
			}, orm.Where("device_id = ? AND channel_id = ?", ctx.DeviceID, msg.DeviceID))
		}
	} else {
		persist = func() error {
			return g.core.UpdateContext(g.serviceContext(), ctx.DeviceID, func(d *ipc.Device) {
				// 可选字段缺省时保留旧值；显式空元素属于设备返回的新值，应允许清除旧元数据。
				if msg.HasFirmware {
					d.Ext.Firmware = msg.Firmware
				}
				if msg.HasManufacturer {
					d.Ext.Manufacturer = msg.Manufacturer
				}
				if msg.HasModel {
					d.Ext.Model = msg.Model
				}
				if msg.HasDeviceName {
					d.Ext.Name = msg.DeviceName
				}
			})
		}
	}

	// 命中通用查询等待队列（A.2.4 DeviceInfo 查询等待）。
	stateDeviceID := firstNonEmpty(msg.DeviceID, ctx.DeviceID)
	decoded := g.decodeDeviceQueryResult(stateDeviceID, msg.CmdType, ctx.Request.Body(), extended)
	if err := ctx.RespondString(200, "OK"); err != nil {
		ctx.Log.Error("respond DeviceInfo", "err", err, "sn", msg.SN, "target_id", msg.DeviceID)
		return
	}
	unlockCommit, err := g.lockAdmittedInboundDeviceStateCommit(ctx)
	if err != nil {
		return
	}
	defer unlockCommit()
	g.commitDecodedQueryStateForOwnerLocked(ctx.DeviceID, stateDeviceID, msg.CmdType, decoded)
	g.resolvePendingDeviceQueryResult(ctx.DeviceID, msg.CmdType, msg.SN, msg.Result, ctx.Request.Body(), msg.DeviceID, decoded)
	if err := persist(); err != nil {
		ctx.Log.Error("persist DeviceInfo", "err", err, "target_id", msg.DeviceID)
	}
	// 运行态按实际目标隔离；附录 A.4 持久化仍归属于已鉴权的父设备。
	// 不能把通道编码当作设备表主键，否则扩展对象会静默丢失。
	g.persistDecodedQuery(ctx.DeviceID, msg.CmdType, decoded)
	// 9.11 事件源侧：设备信息变化通知。
	g.publishEventNotify(msg.CmdType, ctx.DeviceID, ctx.Request.Body())
}
