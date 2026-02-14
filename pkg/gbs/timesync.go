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

// SyncTime 主动校时（9.10），向设备发送 DeviceControl(Time)。
// 注：注册 200 OK 中已携带 Date，此接口用于主动触发。
func (g *GB28181API) SyncTime(_ context.Context, in *TimeSyncInput) error {
	if in == nil || in.DeviceID == "" {
		return ErrDeviceNotExist
	}
	ipc, ok := g.svr.memoryStorer.Load(in.DeviceID)
	if !ok || !ipc.IsOnline {
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
			// 9.10 采用 XML 时间格式：yyyy-MM-ddTHH:mm:ss.SSS
			Time: time.Now().Format("2006-01-02T15:04:05.000"),
		},
	})
	if err != nil {
		return err
	}
	tx, err := g.svr.wrapRequest(ipc, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		return err
	}
	_, err = sipResponse(tx)
	return err
}
