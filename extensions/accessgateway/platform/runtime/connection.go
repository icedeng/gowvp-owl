package runtime

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
)

func (w *worker) serve(p *transport.Peer, ack *protocol.HelloAck) error {
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	heartbeats := make(chan error, 1)
	go func() { heartbeats <- w.heartbeatLoop(ctx, p, ack) }()
	defer func() { cancel(); <-heartbeats }()
	signals := make(chan transport.Incoming, 64)
	var workers sync.WaitGroup
	workers.Add(4)
	var bytesMu sync.Mutex
	queuedBytes := 0
	for i := 0; i < 4; i++ {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case in := <-signals:
					packet := in.Payload.(*protocol.SIPPacket)
					bytesMu.Lock()
					queuedBytes -= len(packet.RawBase64)
					bytesMu.Unlock()
					_ = w.receiveSignal(ctx, p, in.Envelope, *packet)
				}
			}
		}()
	}
	defer func() { cancel(); workers.Wait() }()
	for {
		nextCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		in, err := p.Next(nextCtx)
		stop()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			select {
			case e := <-heartbeats:
				heartbeats <- e
				return e
			default:
			}
			if err == context.DeadlineExceeded {
				continue
			}
			return err
		}
		now := time.Now().UTC()
		w.mu.Lock()
		w.snapshot.LastSeenAt = &now
		w.mu.Unlock()
		switch value := in.Payload.(type) {
		case *protocol.MediaReady:
			w.attached(value.SessionID)
		case *protocol.TransferReady:
			w.attached(value.TransferID)
		case *protocol.SIPPacket:
			bytesMu.Lock()
			if queuedBytes+len(value.RawBase64) > 8<<20 {
				bytesMu.Unlock()
				return protocol.Failure(protocol.Exhausted, "A 信令队列字节额度已满")
			}
			queuedBytes += len(value.RawBase64)
			bytesMu.Unlock()
			select {
			case signals <- in:
			case <-ctx.Done():
				return ctx.Err()
			default:
				return protocol.Failure(protocol.Exhausted, "A 信令处理队列已满")
			}
		case *protocol.StatusSnapshot:
			w.mu.Lock()
			last := w.snapshot.Status
			if last == nil || decimalGreater(value.SampleSeq, last.SampleSeq) {
				copy := *value
				w.snapshot.Status = &copy
				w.snapshot.StatusReceivedAt = &now
				w.snapshot.ReceivedAt = &now
				row := w.config
				health := "ready"
				if value.ConfigRevision != protocol.Number(row.ConfigRevision) || row.AppliedRevision != row.ConfigRevision || !value.MediaReady || !value.SIPUDPBound || !value.SIPTCPBound {
					health = "degraded"
				}
				w.snapshot.HealthState = health
			}
			w.mu.Unlock()
		case *protocol.MetricsSnapshot:
			accepted := false
			w.mu.Lock()
			last := w.snapshot.Metrics
			if last == nil || decimalGreater(value.SampleSeq, last.SampleSeq) {
				w.snapshot.ToABPS, w.snapshot.ToDeviceBPS = nil, nil
				if last != nil && w.snapshot.MetricsReceivedAt != nil {
					elapsed := now.Sub(*w.snapshot.MetricsReceivedAt)
					if elapsed <= time.Duration(w.config.Desired.HeartbeatTimeoutMS)*time.Millisecond {
						w.snapshot.ToABPS = protocol.BitRate(last.WSSTXBytes, value.WSSTXBytes, elapsed)
						w.snapshot.ToDeviceBPS = protocol.BitRate(last.DeviceTXBytes, value.DeviceTXBytes, elapsed)
					}
				}
				copy := *value
				w.snapshot.Metrics = &copy
				w.snapshot.MetricsReceivedAt = &now
				w.snapshot.ReceivedAt = &now
				accepted = true
			}
			_, _, rtt := p.Stats()
			ms := float64(rtt) / float64(time.Millisecond)
			w.snapshot.RTTMS = &ms
			w.mu.Unlock()
			if accepted {
				if sink, ok := w.manager.repo.(interface {
					RuntimeMetrics(context.Context, string, string, int64, string, protocol.MetricsSnapshot, *protocol.StatusSnapshot, time.Time, core.Metrics, *core.Counts) error
				}); ok {
					w.mu.RLock()
					status := w.snapshot.Status
					generation := w.snapshot.Generation
					rates := core.Metrics{ToABPS: w.snapshot.ToABPS, ToDeviceBPS: w.snapshot.ToDeviceBPS, RTTMS: w.snapshot.RTTMS}
					snapshot := w.snapshot
					w.mu.RUnlock()
					metricCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
					var counts *core.Counts
					if host, ok := w.manager.host.(interface {
						GatewayCounts(context.Context, string, core.RuntimeSnapshot) (core.Counts, error)
					}); ok {
						if value, err := host.GatewayCounts(metricCtx, w.configuration().GatewayPK, snapshot); err == nil {
							counts = &value
						}
					}
					_ = sink.RuntimeMetrics(metricCtx, w.configuration().GatewayPK, w.manager.instanceID, w.ownerEpoch, generation, *value, status, now, rates, counts)
					cancel()
				}
			}
		case *protocol.MediaFailed:
			ref := w.resourceRef("media", value.SessionID)
			_ = w.manager.host.ResourceFailed(ctx, w.configuration().GatewayPK, ref, value.Error)
		case *protocol.TransferFailed:
			ref := w.resourceRef("transfer", value.TransferID)
			_ = w.manager.host.ResourceFailed(ctx, w.configuration().GatewayPK, ref, value.Error)
		case *protocol.FlowClosed:
			_ = w.manager.host.FlowClosed(ctx, w.configuration().GatewayPK, w.resourceRef("flow", value.FlowID))
		}
	}
}
func decimalGreater(a, b protocol.Decimal) bool {
	aa, e1 := a.Int64()
	bb, e2 := b.Int64()
	return e1 == nil && e2 == nil && aa > bb
}
func (w *worker) resourceRef(kind, id string) protocol.ResourceRef {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return protocol.ResourceRef{ResourceType: kind, ID: id, Generation: w.snapshot.Generation, ConnectionEpoch: w.snapshot.Epoch}
}
func (w *worker) receiveSignal(ctx context.Context, p *transport.Peer, env protocol.Envelope, packet protocol.SIPPacket) error {
	raw, err := protocol.DecodeSIP(packet)
	if err != nil {
		return err
	}
	row := w.configuration()
	w.mu.RLock()
	snapshot := w.snapshot
	valid := w.peer == p && snapshot.ConnectionState == "connected" && snapshot.HealthState == "ready"
	w.mu.RUnlock()
	if !valid {
		return core.Fail(503, "GATEWAY_UNAVAILABLE", "网关尚未完成恢复或配置应用")
	}
	bindingRevision := protocol.Decimal("0")
	if packet.Route.BindingRevision != nil {
		bindingRevision = *packet.Route.BindingRevision
	}
	route := core.IngressRoute{IngressType: "gateway", GatewayPK: row.GatewayPK, GatewayID: row.GatewayID, Generation: snapshot.Generation, ConnectionEpoch: snapshot.Epoch, OwnerEpoch: snapshot.OwnerEpoch, FlowID: packet.Route.FlowID, BindingRevision: bindingRevision, SIPAdvertise: protocol.Endpoint{IP: row.Desired.PublicIP, Port: row.Desired.SIPPort}, ConfigRevision: protocol.Number(row.ConfigRevision)}
	return w.manager.host.ReceiveSignal(ctx, core.SignalIngress{Ingress: route, Route: packet.Route, Raw: raw, Send: func(sendCtx context.Context, data []byte) error {
		w.mu.RLock()
		valid := w.peer == p && w.snapshot.Generation == route.Generation && w.snapshot.Epoch == route.ConnectionEpoch
		w.mu.RUnlock()
		if !valid {
			return protocol.Failure(protocol.StaleEpoch, "信令入口已换代")
		}
		return p.Event(sendCtx, "sip.packet", protocol.SIPPacket{Route: packet.Route, RawBase64: base64.StdEncoding.EncodeToString(data)})
	}})
}
func (w *worker) heartbeatLoop(ctx context.Context, p *transport.Peer, ack *protocol.HelloAck) error {
	timer := time.NewTimer(time.Duration(ack.HeartbeatIntervalMS) * time.Millisecond)
	defer timer.Stop()
	var seq int64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.Done():
			return p.Err()
		case <-w.reconfigure:
			timer.Reset(0)
			continue
		case <-timer.C:
			config := w.configuration()
			interval, ttl := ack.HeartbeatIntervalMS, ack.LeaseTTLMS
			if config.AppliedRevision > 0 && config.AppliedRevision == config.ConfigRevision {
				interval = config.Desired.HeartbeatIntervalMS
				ttl = config.Desired.LeaseTTLMS
			}
			timer.Reset(time.Duration(interval) * time.Millisecond)
			if seq == int64(1<<63-1) {
				return fmt.Errorf("heartbeat sequence exhausted")
			}
			seq++
			w.mu.RLock()
			refs := make([]protocol.ResourceRef, 0, len(w.leases))
			for _, ref := range w.leases {
				refs = append(refs, ref)
			}
			w.mu.RUnlock()
			sessionIDs, transferIDs := []string{}, []string{}
			for _, ref := range refs {
				if ref.ResourceType == "media" {
					sessionIDs = append(sessionIDs, ref.ID)
				} else {
					transferIDs = append(transferIDs, ref.ID)
				}
			}
			parts := max(1, (max(len(sessionIDs), len(transferIDs))+499)/500)
			if parts > 64 {
				return protocol.Failure(protocol.Exhausted, "heartbeat partition limit exceeded")
			}
			for part := 0; part < parts; part++ {
				a := sessionIDs[min(part*500, len(sessionIDs)):min((part+1)*500, len(sessionIDs))]
				b := transferIDs[min(part*500, len(transferIDs)):min((part+1)*500, len(transferIDs))]
				request := protocol.Heartbeat{HeartbeatSeq: protocol.Number(seq), RenewSessionIDs: a, RenewTransferIDs: b}
				if parts > 1 {
					request.PartIndex = part
					request.PartCount = parts
				}
				sent := time.Now().UTC()
				callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				reply, err := p.Call(callCtx, "heartbeat", request)
				cancel()
				if err != nil {
					return err
				}
				result := reply.Payload.(*protocol.HeartbeatAck)
				if result.HeartbeatSeq != request.HeartbeatSeq || result.PartIndex != request.PartIndex || result.PartCount != request.PartCount {
					return protocol.Failure(protocol.InvalidArgument, "heartbeat response sequence mismatch")
				}
				expected := map[string]bool{}
				for _, id := range append(append([]string{}, a...), b...) {
					expected[id] = true
				}
				accepted := map[string]bool{}
				for _, id := range append(append([]string{}, result.RenewedSessionIDs...), result.RenewedTransferIDs...) {
					if !expected[id] || accepted[id] {
						return protocol.Failure(protocol.InvalidArgument, "unexpected renewal response")
					}
					accepted[id] = true
					delete(expected, id)
				}
				w.mu.RLock()
				refByID := make(map[string]protocol.ResourceRef, len(w.leases))
				for id, ref := range w.leases {
					refByID[id] = ref
				}
				w.mu.RUnlock()
				for _, id := range result.RejectedIDs {
					if !expected[id] {
						return protocol.Failure(protocol.InvalidArgument, "unexpected rejected renewal")
					}
					delete(expected, id)
					if ref, ok := refByID[id]; ok {
						_ = w.manager.host.ResourceFailed(ctx, w.configuration().GatewayPK, ref, *protocol.Failure(protocol.NotFound, "B 拒绝会话续租"))
					}
				}
				if len(expected) > 0 {
					return protocol.Failure(protocol.UpstreamTimeout, "heartbeat did not confirm all leases")
				}
				sampled := map[string]bool{}
				for _, sample := range result.Traffic {
					ref, found := refByID[sample.Ref.ID]
					if !found || !accepted[sample.Ref.ID] || ref != sample.Ref || sampled[sample.Ref.ID] {
						return protocol.Failure(protocol.InvalidArgument, "unexpected resource traffic")
					}
					sampled[sample.Ref.ID] = true
					for _, n := range []protocol.Decimal{sample.RXBytes, sample.TXBytes, sample.QueueBytes} {
						if _, err := n.Int64(); err != nil {
							return err
						}
					}
				}
				if sink, ok := w.manager.repo.(interface {
					RuntimeSessionTraffic(context.Context, string, string, int64, []protocol.ResourceTraffic, time.Time) error
				}); ok && len(result.Traffic) > 0 {
					if err := sink.RuntimeSessionTraffic(ctx, config.GatewayPK, w.manager.instanceID, w.ownerEpoch, result.Traffic, time.Now().UTC()); err != nil {
						return err
					}
				}
				renewed := []protocol.ResourceRef{}
				for id := range accepted {
					if ref, ok := refByID[id]; ok {
						renewed = append(renewed, ref)
					}
				}
				if len(renewed) > 0 {
					row := w.configuration()
					if err := w.manager.repo.RuntimeLeasesRenewed(ctx, row.GatewayPK, w.manager.instanceID, w.ownerEpoch, renewed, sent, ttl); err != nil {
						return err
					}
				}
				now := time.Now().UTC()
				w.mu.Lock()
				w.snapshot.LastSeenAt = &now
				w.mu.Unlock()
			}
		}
	}
}
func (m *Manager) RegisterLease(gatewayPK string, ref protocol.ResourceRef) (func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.workers[gatewayPK]
	if w == nil {
		return nil, core.Fail(503, "GATEWAY_UNAVAILABLE", "网关没有运行态")
	}
	totalMedia, totalTransfer := 0, 0
	for _, current := range m.workers {
		current.mu.RLock()
		for _, existing := range current.leases {
			if existing.ResourceType == "media" {
				totalMedia++
			} else {
				totalTransfer++
			}
		}
		current.mu.RUnlock()
	}
	if ref.ResourceType == "media" && totalMedia >= m.config.MaxMediaSessions || ref.ResourceType == "transfer" && totalTransfer >= m.config.MaxTransferSessions {
		return nil, core.Fail(429, "RESOURCE_EXHAUSTED", "超过 A 的全局媒体或任务额度")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.peer == nil || w.snapshot.Generation != ref.Generation || w.snapshot.Epoch != ref.ConnectionEpoch || w.snapshot.HealthState != "ready" {
		return nil, protocol.Failure(protocol.StaleEpoch, "媒体租约入口已失效")
	}
	if w.leases == nil {
		w.leases = map[string]protocol.ResourceRef{}
	}
	w.leases[ref.ID] = ref
	var once sync.Once
	return func() {
		once.Do(func() {
			w.mu.Lock()
			if existing, ok := w.leases[ref.ID]; ok && existing == ref {
				delete(w.leases, ref.ID)
			}
			w.mu.Unlock()
		})
	}, nil
}

func (m *Manager) SendSignal(ctx context.Context, gatewayPK, generation string, epoch protocol.Decimal, route protocol.Route, raw []byte) error {
	m.mu.RLock()
	w := m.workers[gatewayPK]
	m.mu.RUnlock()
	if w == nil {
		return protocol.Failure(protocol.StaleEpoch, "网关入口不存在")
	}
	w.mu.RLock()
	p := w.peer
	valid := p != nil && w.snapshot.Generation == generation && w.snapshot.Epoch == epoch
	w.mu.RUnlock()
	if !valid {
		return protocol.Failure(protocol.StaleEpoch, "信令入口已经换代")
	}
	return p.Event(ctx, "sip.packet", protocol.SIPPacket{Route: route, RawBase64: base64.StdEncoding.EncodeToString(raw)})
}
