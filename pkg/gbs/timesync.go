package gbs

import (
	"context"
	"encoding/xml"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

type TimeSyncInput struct {
	DeviceID string
}

// SyncTime 通过厂商扩展 DeviceControl(Time) 主动校时。
// GB/T 28181 四版本的标准校时由成功 REGISTER 的 200 OK Date 或 NTP 完成；保留此接口兼容已有调用方。
func (g *GB28181API) SyncTime(ctx context.Context, in *TimeSyncInput) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if in == nil || in.DeviceID == "" {
		return ErrDeviceNotExist
	}
	ipc, ok := g.svr.memoryStorer.Load(in.DeviceID)
	if !ok || !ipc.IsOnlineNow() {
		return ErrDeviceOffline
	}
	type timeSyncReq struct {
		CmdType  string `xml:"CmdType"`
		SN       int    `xml:"SN"`
		DeviceID string `xml:"DeviceID"`
		Time     string `xml:"Time"`
	}
	body, err := sip.XMLEncode(struct {
		XMLName xml.Name `xml:"Control"`
		timeSyncReq
	}{
		XMLName: xml.Name{Local: "Control"},
		timeSyncReq: timeSyncReq{
			CmdType:  "DeviceControl",
			SN:       g.nextControlSN(),
			DeviceID: in.DeviceID,
			// 厂商扩展沿用国标 Date 的北京时间格式。
			Time: sip.FormatGBTime(time.Now(), "2006-01-02T15:04:05.000"),
		},
	})
	if err != nil {
		return err
	}
	operation, releaseOperation := g.trackPendingDeviceRequest(ctx, in.DeviceID, in.DeviceID)
	defer releaseOperation()
	requestCtx := operation.Context(ctx)
	tx, err := g.svr.wrapRequestContext(requestCtx, ipc, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		return operation.ErrorOr(err)
	}
	_, err = sipResponseContext(requestCtx, tx)
	if err != nil {
		return operation.ErrorOr(err)
	}
	if !operation.Deliver(func() {}) {
		return operation.Cause()
	}
	return nil
}
