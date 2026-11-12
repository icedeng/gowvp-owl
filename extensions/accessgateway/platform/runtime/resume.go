package runtime

import (
	"context"

	"slices"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
)

func (w *worker) resumeFlows(parent context.Context, p *transport.Peer, ack *protocol.HelloAck) error {
	if !slices.Contains(ack.Capabilities, "sip_resume_v1") {
		return nil
	}
	host, ok := w.manager.host.(interface {
		ResumeFlow(context.Context, core.IngressRoute, protocol.ResumeFlow) error
	})
	if !ok {
		return nil
	}
	after := ""
	for {
		ctx, cancel := context.WithTimeout(parent, 10*time.Second)
		reply, err := p.Call(ctx, "sip.resume.list", protocol.ResumeList{After: after})
		cancel()
		if err != nil {
			return err
		}
		batch := reply.Payload.(*protocol.ResumeFlows)
		for _, f := range batch.Items {
			row := w.configuration()
			s := w.manager.Snapshot(row.GatewayPK)
			route := core.IngressRoute{IngressType: "gateway", GatewayPK: row.GatewayPK, GatewayID: row.GatewayID, Generation: s.Generation, ConnectionEpoch: s.Epoch, OwnerEpoch: s.OwnerEpoch, FlowID: f.Route.FlowID, BindingRevision: f.BindingRevision, ConfigRevision: protocol.Number(row.ConfigRevision), SIPAdvertise: protocol.Endpoint{IP: row.Desired.PublicIP, Port: row.Desired.SIPPort}}
			ctx, cancel := context.WithTimeout(parent, 5*time.Second)
			err := host.ResumeFlow(ctx, route, f)
			cancel()
			// 失败不授予新代次权限，等待设备重新注册；不关闭可能刚被并发注册刷新的 flow。
			_ = err
		}
		if batch.Next == "" {
			return nil
		}
		after = batch.Next
	}
}
