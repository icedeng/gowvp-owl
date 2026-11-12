package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/lease"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/state"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
)

func operationID(payload any) string {
	v := reflect.ValueOf(payload)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}
	f := v.FieldByName("OperationID")
	if f.IsValid() && f.Kind() == reflect.String {
		return f.String()
	}
	return ""
}
func resourceID(result any) string {
	switch v := result.(type) {
	case protocol.MediaOpenResult:
		return v.Lease.SessionID
	case protocol.TransferOpened:
		return v.TransferID
	}
	return ""
}
func (c *Controller) executeIdempotent(ctx context.Context, owner lease.Owner, in transport.Incoming) (any, *protocol.Error) {
	op := operationID(in.Payload)
	if op == "" {
		v, e := c.execute(ctx, owner, in)
		return v, asError(e)
	}
	hash, e := protocol.BusinessHash(in.Payload)
	if e != nil {
		return nil, protocol.Failure(protocol.InvalidArgument, "cannot normalize request")
	}
	key := strings.Join([]string{owner.PlatformID, owner.Generation, string(owner.Epoch), in.Envelope.Type, op}, "/")
	c.mu.Lock()
	for key, record := range c.cache {
		if time.Now().After(record.expires) {
			select {
			case <-record.done:
				if record.resourceID == "" || !c.Registry.Active(record.resourceID) {
					delete(c.cache, key)
				}
			default:
			}
		}
	}
	if record := c.cache[key]; record != nil {
		if record.hash != hash {
			c.mu.Unlock()
			return nil, protocol.Failure(protocol.IdempotencyConflict, "operation ID reused with different parameters")
		}
		c.mu.Unlock()
		select {
		case <-record.done:
			if in.Envelope.Type == "attachment.issue" {
				v, e := c.execute(ctx, owner, in)
				return v, asError(e)
			}
			return record.result, record.failure
		case <-ctx.Done():
			return nil, asError(ctx.Err())
		}
	}
	if len(c.cache) >= 20000 {
		c.mu.Unlock()
		return nil, protocol.Failure(protocol.Exhausted, "idempotency record budget")
	}
	record := &cached{hash: hash, done: make(chan struct{}), expires: time.Now().Add(10 * time.Minute)}
	if in.Envelope.Type == "config.prepare" {
		record.expires = time.Now().Add(30 * time.Second)
	}
	c.cache[key] = record
	c.mu.Unlock()
	result, err := c.execute(ctx, owner, in)
	failure := asError(err)
	c.mu.Lock()
	record.result = result
	record.failure = failure
	record.resourceID = resourceID(result)
	close(record.done)
	c.mu.Unlock()
	return result, failure
}
func (c *Controller) execute(ctx context.Context, owner lease.Owner, in transport.Incoming) (any, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	switch v := in.Payload.(type) {
	case *protocol.GatewayInfoRequest:
		n, b, e := c.store.AlarmStats()
		return protocol.GatewayInfo{Build: protocol.GatewayBuild, ProtocolVersion: protocol.Version, AlarmPending: n, AlarmBytes: protocol.Number(b), AlarmStoreError: e != nil}, nil
	case *protocol.MediaInspect:
		return c.Media.Inspect(owner, v.SessionID)
	case *protocol.AlarmPull:
		if !c.store.Snapshot().AlarmSpoolEnabled {
			if err := c.store.Update(func(d *state.Data) error { d.AlarmSpoolEnabled = true; return nil }); err != nil {
				return nil, err
			}
		}
		return c.store.PullAlarms(v.After)
	case *protocol.AlarmAck:
		n, err := c.store.AckAlarms(v.IDs)
		return protocol.AlarmAckResult{Removed: n}, err
	case *protocol.ResumeList:
		return c.Hub.ResumeFlows(v.After), nil
	case *protocol.Heartbeat:
		a, ar := c.Registry.Renew(owner, v.RenewSessionIDs)
		b, br := c.Registry.Renew(owner, v.RenewTransferIDs)
		c.mu.Lock()
		drain := "enabled"
		if c.draining {
			drain = "draining"
		}
		c.mu.Unlock()
		return protocol.HeartbeatAck{Traffic: c.Registry.Traffic(owner, append(append([]string{}, a...), b...)), HeartbeatSeq: v.HeartbeatSeq, RenewedSessionIDs: a, RenewedTransferIDs: b, RejectedIDs: append(ar, br...), DrainState: drain, PartIndex: v.PartIndex, PartCount: v.PartCount}, nil
	case *protocol.FlowOpen:
		if v.ConfiguredPlatform {
			route, e := c.Hub.OpenPlatform(ctx, *v)
			return protocol.FlowOpenResult{Route: route}, e
		}
		if v.Transport != "tcp" {
			return nil, protocol.Failure(protocol.InvalidArgument, "device flow requires TCP")
		}
		route, e := c.Hub.OpenTCP(ctx, v.Target, v.SubjectType, v.SubjectID, v.BindingRevision)
		return protocol.FlowOpenResult{Route: route}, e
	case *protocol.FlowClose:
		c.Hub.CloseFlow(v.FlowID, v.Reason)
		return protocol.FlowCloseResult{FlowID: v.FlowID, Closed: true}, nil
	case *protocol.BindingUpdate:
		c.mu.Lock()
		draining := c.draining
		c.mu.Unlock()
		if draining && v.Action == "bind" {
			return nil, protocol.Failure(protocol.GatewayDraining, "new bindings disabled while draining")
		}
		e := c.Hub.Bind(v.Binding, v.Action)
		return protocol.BindingResult{SubjectID: v.Binding.SubjectID, BindingRevision: v.Binding.BindingRevision, Applied: e == nil}, e
	case *protocol.MediaOpen:
		result, e := c.Media.Open(owner, *v)
		return protocol.MediaOpenResult{Lease: result}, e
	case *protocol.MediaConnect:
		e := c.Media.Connect(ctx, owner, *v)
		return protocol.MediaConnectResult{SessionID: v.SessionID, Connected: e == nil}, e
	case *protocol.MediaClose:
		ref := protocol.ResourceRef{ResourceType: "media", ID: v.SessionID, Generation: owner.Generation, ConnectionEpoch: owner.Epoch}
		closed, e := c.Registry.CloseRef(owner.PlatformID, ref)
		return protocol.MediaCloseResult{SessionID: v.SessionID, Closed: closed, CleanupPending: !closed}, e
	case *protocol.TransferOpen:
		return c.Transfers.Open(owner, *v)
	case *protocol.TransferConnect:
		e := c.Transfers.Connect(ctx, owner, *v)
		return protocol.TransferConnected{TransferID: v.TransferID, Connected: e == nil}, e
	case *protocol.TransferClose:
		ref := protocol.ResourceRef{ResourceType: "transfer", ID: v.TransferID, Generation: owner.Generation, ConnectionEpoch: owner.Epoch}
		closed, e := c.Registry.CloseRef(owner.PlatformID, ref)
		return protocol.TransferClosed{TransferID: v.TransferID, Closed: closed, CleanupPending: !closed}, e
	case *protocol.TransferFinish:
		return c.Transfers.Finish(owner, *v)
	case *protocol.TransferQuery:
		return c.Transfers.Query(owner.PlatformID, v.TransferID)
	case *protocol.AttachmentIssue:
		ticket, e := c.Registry.Issue(owner, v.ResourceType, v.ResourceID)
		return protocol.AttachmentResult{Ticket: ticket, ExpiresInMS: 30000}, e
	case *protocol.ResourcesList:
		items := c.Registry.List(owner.PlatformID)
		sort.Slice(items, func(i, j int) bool { return items[i].Ref.ID < items[j].Ref.ID })
		filtered := make([]protocol.ResourceRecord, 0)
		for _, item := range items {
			if (v.ResourceType == "all" || v.ResourceType == item.Ref.ResourceType) && (v.Cursor == nil || item.Ref.ID > *v.Cursor) {
				filtered = append(filtered, item)
			}
		}
		var next *string
		if len(filtered) > v.Size {
			cursor := filtered[v.Size-1].Ref.ID
			next = &cursor
			filtered = filtered[:v.Size]
		}
		return protocol.ResourcesResult{Items: filtered, NextCursor: next}, nil
	case *protocol.ResourceClose:
		closed, e := c.Registry.CloseRef(owner.PlatformID, v.Ref)
		return protocol.ResourceCloseResult{Ref: v.Ref, Closed: closed, CleanupPending: !closed}, e
	case *protocol.ConfigPrepare:
		return c.prepare(*v)
	case *protocol.ConfigApply:
		return c.apply(*v)
	case *protocol.Drain:
		c.mu.Lock()
		c.draining = true
		c.mu.Unlock()
		c.Media.Drain(true)
		c.Transfers.Drain(true)
		return protocol.DrainResult{DrainState: "draining", ActiveResources: c.Registry.Count("all")}, nil
	case *protocol.Resume:
		c.mu.Lock()
		if v.ConfigRevision != c.revision || c.Ports.CooldownMS() > 0 {
			c.mu.Unlock()
			return nil, protocol.Failure(protocol.ConfigConflict, "configuration/cooldown does not permit resume")
		}
		c.draining = false
		c.mu.Unlock()
		c.Media.Drain(false)
		c.Transfers.Drain(false)
		return protocol.ResumeResult{DrainState: "enabled"}, nil
	}
	return nil, protocol.Failure(protocol.CapabilityUnsupported, "request is not supported by this gateway version")
}
func (c *Controller) prepare(request protocol.ConfigPrepare) (protocol.ConfigPrepared, error) {
	if e := c.config.Check(request.Config); e != nil {
		return protocol.ConfigPrepared{}, e
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if request.ExpectedRevision != c.revision {
		return protocol.ConfigPrepared{}, protocol.Failure(protocol.ConfigConflict, "expected config revision mismatch")
	}
	for id, v := range c.prepared {
		if time.Now().After(v.expires) {
			delete(c.prepared, id)
		}
	}
	if len(c.prepared) >= 32 {
		return protocol.ConfigPrepared{}, protocol.Failure(protocol.Exhausted, "too many pending configurations")
	}
	requiresDrain := request.Config.PublicIP != c.effective.PublicIP || request.Config.MediaPortStart != c.effective.MediaPortStart || request.Config.MediaPortEnd != c.effective.MediaPortEnd
	// 来源策略改变也需要排空，活动租约保持原快照。
	left, _ := protocol.Canonical(request.Config.SourceCIDRs)
	right, _ := protocol.Canonical(c.effective.SourceCIDRs)
	requiresDrain = requiresDrain || string(left) != string(right)
	id := uuid.NewString()
	c.prepared[id] = prepared{request: request, expires: time.Now().Add(30 * time.Second), requiresDrain: requiresDrain}
	return protocol.ConfigPrepared{PrepareID: id, PrepareTTLMS: 30000, RequiresDrain: requiresDrain}, nil
}
func (c *Controller) apply(request protocol.ConfigApply) (protocol.ConfigResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if request.DesiredRevision == c.revision {
		if request.ConfigSHA256 != c.configHash {
			return protocol.ConfigResult{}, protocol.Failure(protocol.ConfigConflict, "same revision has different hash")
		}
		return protocol.ConfigResult{AppliedRevision: c.revision, ConfigSHA256: c.configHash, Applied: true}, nil
	}
	value, ok := c.prepared[request.PrepareID]
	if !ok || time.Now().After(value.expires) || value.request.DesiredRevision != request.DesiredRevision || value.request.ConfigSHA256 != request.ConfigSHA256 || value.request.ExpectedRevision != c.revision {
		return protocol.ConfigResult{}, protocol.Failure(protocol.ConfigConflict, "configuration preparation expired or changed")
	}
	if value.requiresDrain && c.Registry.Count("all") > 0 {
		return protocol.ConfigResult{}, protocol.Failure(protocol.GatewayDraining, "active resources must drain before apply")
	}
	next := value.request.Config
	if e := c.store.Update(func(d *state.Data) error {
		d.AppliedConfig = &next
		d.AppliedRevision = request.DesiredRevision
		d.ConfigSHA256 = request.ConfigSHA256
		return nil
	}); e != nil {
		return protocol.ConfigResult{}, e
	}
	c.effective = next
	c.revision = request.DesiredRevision
	c.configHash = request.ConfigSHA256
	delete(c.prepared, request.PrepareID)
	c.Media.Configure(next, request.DesiredRevision)
	c.Transfers.Configure(next, request.DesiredRevision)
	return protocol.ConfigResult{AppliedRevision: c.revision, ConfigSHA256: c.configHash, Applied: true}, nil
}
func (c *Controller) observe() {
	defer c.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	process := processSampler{}
	_, _ = process.sample(time.Now())
	for {
		select {
		case <-c.ctx.Done():
			return
		case now := <-ticker.C:
			c.mu.Lock()
			revision := c.revision
			effective := c.effective
			drain := "enabled"
			if c.draining {
				drain = "draining"
			}
			c.mu.Unlock()
			seq := protocol.Number(c.sample.Add(1))
			c.Event("status.snapshot", protocol.StatusSnapshot{SampleSeq: seq, SampledAt: now.UTC().Format("2006-01-02T15:04:05.000Z"), SIPUDPBound: true, SIPTCPBound: true, MediaReady: c.Ports.CooldownMS() == 0, ConfigRevision: revision, DrainState: drain, PortTotal: effective.MediaPortEnd - effective.MediaPortStart + 1, PortUsed: c.Ports.Used()})
			aa, bb := protocol.Number(c.counters.RX.Load()), protocol.Number(c.counters.TX.Load())
			rx, tx := &aa, &bb
			deviceRX, deviceTX := protocol.Number(c.counters.DeviceRX.Load()), protocol.Number(c.counters.DeviceTX.Load())
			cpu, rss := process.sample(now)
			queue := protocol.Number(c.Registry.QueuedBytes())
			c.Event("metrics.snapshot", protocol.MetricsSnapshot{SampleSeq: seq, SampledAt: now.UTC().Format("2006-01-02T15:04:05.000Z"), UptimeMS: int(time.Since(c.started) / time.Millisecond), CPUPercent: cpu, RSSBytes: rss, QueueBytes: &queue, SIPFlows: c.Hub.Count(), MediaSessions: c.Registry.Count("media"), TransferSessions: c.Registry.Count("transfer"), WSSRXBytes: rx, WSSTXBytes: tx, DeviceRXBytes: &deviceRX, DeviceTXBytes: &deviceTX})
		}
	}
}

// DecodeOperation 用于恢复日志中的共享操作对象，不另维护一份协议字段。
func DecodeOperation(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return fmt.Errorf("missing operation snapshot")
	}
	return protocol.DecodeStrict(raw, dst)
}
