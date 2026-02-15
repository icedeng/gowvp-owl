package gbs

import (
	"context"
	"fmt"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
)

// OptionsProbeInput OPTIONS 探测参数。
type OptionsProbeInput struct {
	DeviceID string
	Timeout  time.Duration
}

// sipOptionsGeneric 处理入向 OPTIONS 探测，按 RFC 返回 200。
func (g *GB28181API) sipOptionsGeneric(ctx *sip.Context) {
	resp := sip.NewResponseFromRequest("", ctx.Request, 200, "OK", nil)
	resp.AppendHeader(&sip.GenericHeader{
		HeaderName: "Allow",
		Contents:   "INVITE, ACK, CANCEL, OPTIONS, BYE, MESSAGE, SUBSCRIBE, NOTIFY, INFO",
	})
	_ = ctx.Tx.Respond(resp)
}

// ProbeOptions 对设备发起 OPTIONS 探测。
// 探测成功后会刷新设备 Keepalive 时间，避免被离线扫描误判。
func (g *GB28181API) ProbeOptions(_ context.Context, in *OptionsProbeInput) error {
	if in == nil || in.DeviceID == "" {
		return fmt.Errorf("invalid options probe request")
	}
	ipcDev, ok := g.svr.memoryStorer.Load(in.DeviceID)
	if !ok || !ipcDev.IsOnline {
		return ErrDeviceOffline
	}
	tx, err := g.svr.wrapRequest(ipcDev, sip.MethodOptions, nil, nil)
	if err != nil {
		return err
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	resp, err := sipResponseWithTimeout(tx, timeout)
	if err != nil {
		return err
	}
	if resp.StatusCode() != 200 {
		return fmt.Errorf("options failed: %d %s", resp.StatusCode(), resp.Reason())
	}
	_ = g.svr.memoryStorer.Change(in.DeviceID, func(d *ipc.Device) error {
		d.KeepaliveAt = orm.Now()
		return nil
	}, func(d *Device) {
		d.LastKeepaliveAt = time.Now()
	})
	return nil
}

func sipResponseWithTimeout(tx *sip.Transaction, timeout time.Duration) (*sip.Response, error) {
	ch := make(chan *sip.Response, 1)
	go func() {
		ch <- tx.GetResponse()
	}()
	select {
	case resp := <-ch:
		if resp == nil {
			return nil, sip.NewError(nil, "response timeout", "tx key:", tx.Key())
		}
		return resp, nil
	case <-time.After(timeout):
		return nil, sip.NewError(nil, "response timeout", "tx key:", tx.Key())
	}
}
