package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/config"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/lease"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/media"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/sip"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/state"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/transfer"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
)

type prepared struct {
	request       protocol.ConfigPrepare
	expires       time.Time
	requiresDrain bool
}
type cached struct {
	hash       string
	done       chan struct{}
	result     any
	failure    *protocol.Error
	expires    time.Time
	resourceID string
}
type Controller struct {
	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	config     config.Config
	store      *state.Store
	generation string
	epoch      int64
	reserved   bool
	ready      bool
	closed     bool
	peer       *transport.Peer
	owner      lease.Owner
	effective  protocol.EffectiveConfig
	revision   protocol.Decimal
	configHash string
	draining   bool
	prepared   map[string]prepared
	cache      map[string]*cached
	Hub        *sip.Hub
	Registry   *lease.Registry
	Ports      *media.Ports
	Media      *media.Manager
	Budget     *media.Budget
	Transfers  *transfer.Manager
	wg         sync.WaitGroup
	sample     atomic.Int64
	started    time.Time
	counters   transport.Counters
}

func New(parent context.Context, c config.Config, store *state.Store) (*Controller, error) {
	ctx, cancel := context.WithCancel(parent)
	initial, e := c.Initial()
	if e != nil {
		cancel()
		return nil, e
	}
	persisted := store.Snapshot()
	if e := store.Update(func(d *state.Data) error {
		for i := range d.Transfers {
			if !d.Transfers[i].Committed {
				d.Transfers[i].State = "closed"
				d.Transfers[i].Phase = "failed"
			}
			d.Transfers[i].CleanupReserve = ""
		}
		return nil
	}); e != nil {
		cancel()
		return nil, e
	}
	revision := persisted.AppliedRevision
	if persisted.AppliedConfig != nil {
		if e = c.Check(*persisted.AppliedConfig); e != nil {
			cancel()
			return nil, fmt.Errorf("persisted configuration exceeds startup boundaries: %w", e)
		}
		initial = *persisted.AppliedConfig
	}
	hash, e := protocol.ConfigHash(initial)
	if e != nil {
		cancel()
		return nil, e
	}
	if persisted.AppliedConfig != nil && hash != persisted.ConfigSHA256 {
		cancel()
		return nil, fmt.Errorf("persisted configuration hash mismatch")
	}
	v := &Controller{ctx: ctx, cancel: cancel, config: c, store: store, generation: uuid.NewString(), effective: initial, revision: revision, configHash: hash, prepared: map[string]prepared{}, cache: map[string]*cached{}, started: time.Now()}
	v.Registry = lease.New(ctx, store, func(ref protocol.ResourceRef, e error) {
		if ref.ResourceType == "media" {
			code := protocol.IOError
			message := "media lease expired"
			if e != nil {
				message = "media lease cleanup pending"
			}
			v.Event("media.failed", protocol.MediaFailed{SessionID: ref.ID, Error: *protocol.Failure(code, message)})
		}
	})
	v.Hub = sip.New(ctx, c.MaxSIPFlows, c.SIPPacketsPerSecond, v.packet, func(id, reason string) { v.Event("sip.flow.closed", protocol.FlowClosed{FlowID: id, Reason: reason}) })
	v.Ports = media.NewPorts(c.MediaBindIP, store)
	a, _ := initial.MaxToABPS.Int64()
	b, _ := initial.MaxToDeviceBPS.Int64()
	v.Budget = media.NewBudget(a, b)
	v.Media = media.NewManager(ctx, v.Registry, v.Hub, v.Ports, v.Budget, initial, revision, v.Event)
	v.Transfers = transfer.New(ctx, v.Registry, v.Hub, v.Ports, v.Budget, store, initial, revision, c.MaxTransferRecords, v.Event)
	v.Media.Counters = &v.counters
	v.Registry.Counters = &v.counters
	v.Transfers.Counters = &v.counters
	return v, nil
}
func (c *Controller) Start() error {
	if e := c.Hub.Start(c.config.SIPListen); e != nil {
		return e
	}
	c.wg.Add(1)
	go c.observe()
	return nil
}
func (c *Controller) Close() error {
	c.mu.Lock()
	c.closed = true
	p := c.peer
	c.mu.Unlock()
	c.cancel()
	if p != nil {
		p.Close()
	}
	c.Hub.Close()
	e := c.Registry.Close()
	c.wg.Wait()
	return e
}
func (c *Controller) Capabilities() []string {
	caps := []string{"sip_udp", "sip_tcp", "rtp_udp", "rtp_tcp_passive", "rtp_tcp_active", "rtcp_udp", "rtcp_tcp_mux", "playback", "rtp_download", "voice_duplex", "broadcast", "cascade"}
	if c.config.Platform.MaxTransferSessions > 0 {
		caps = append(caps, "raw_tcp_download")
		if c.config.TaskListen != "" && c.config.TaskPublicBaseURL != nil {
			caps = append(caps, "https_transfer")
		}
	}
	return caps
}

func (c *Controller) Probe() protocol.Probe {
	c.mu.Lock()
	defer c.mu.Unlock()
	return protocol.Probe{Version: protocol.Version, GatewayID: c.config.GatewayID, PlatformID: c.config.Platform.ID, Generation: c.generation, ConfigRevision: c.revision, Capabilities: c.Capabilities(), ConfigSHA256: c.configHash, OwnerConnected: c.peer != nil && c.ready, CooldownRemainingMS: c.Ports.CooldownMS()}
}

// Reserve 在 Upgrade 前预留控制所有权，避免两个并发握手都通过冲突检查。
func (c *Controller) Reserve() (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, protocol.Failure(protocol.GatewayDisabled, "gateway shutting down")
	}
	if c.reserved || c.peer != nil {
		return nil, protocol.Failure(protocol.OwnerConflict, "platform already has a control owner")
	}
	c.reserved = true
	var once sync.Once
	return func() { once.Do(func() { c.mu.Lock(); c.reserved = false; c.mu.Unlock() }) }, nil
}
func (c *Controller) ServeControl(conn *websocket.Conn, fingerprint string) {
	defer conn.Close()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.wg.Add(1)
	c.mu.Unlock()
	defer c.wg.Done()
	stopClose := context.AfterFunc(c.ctx, func() { _ = conn.Close() })
	defer stopClose()
	conn.SetReadLimit(protocol.MaxControlBytes)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, data, e := conn.ReadMessage()
	if e != nil || kind != websocket.TextMessage {
		return
	}
	env, payload, e := protocol.DecodeEnvelope(data)
	if e != nil || env.Type != "hello" {
		return
	}
	if env.GatewayID != c.config.GatewayID || env.PlatformID != c.config.Platform.ID {
		c.rejectHello(conn, env, protocol.IdentityMismatch, "gateway/platform identity mismatch")
		return
	}
	hello, ok := payload.(*protocol.Hello)
	if !ok || !slices.Contains(hello.SupportedVersions, protocol.Version) {
		c.rejectHello(conn, env, protocol.CapabilityUnsupported, "no compatible protocol version")
		return
	}
	for _, capability := range hello.RequiredCapabilities {
		if !slices.Contains(c.Capabilities(), capability) {
			c.rejectHello(conn, env, protocol.CapabilityUnsupported, "required capability is unavailable")
			return
		}
	}
	requested, e := env.OwnerEpoch.Int64()
	if e != nil || requested < 1 {
		c.rejectHello(conn, env, protocol.StaleEpoch, "positive owner epoch required")
		return
	}
	c.mu.Lock()
	if c.closed || c.peer != nil || c.epoch == math.MaxInt64 {
		c.mu.Unlock()
		c.rejectHello(conn, env, protocol.OwnerConflict, "control owner already connected or gateway closing")
		return
	}
	persisted := c.store.Snapshot()
	highest, _ := persisted.OwnerEpoch.Int64()
	if requested < highest || requested == highest && hello.ClientInstanceID != persisted.ClientInstanceID {
		c.mu.Unlock()
		c.rejectHello(conn, env, protocol.StaleEpoch, "owner epoch/client instance is fenced")
		return
	}
	if e = c.store.Update(func(d *state.Data) error {
		d.OwnerEpoch = env.OwnerEpoch
		d.ClientInstanceID = hello.ClientInstanceID
		return nil
	}); e != nil {
		c.mu.Unlock()
		c.rejectHello(conn, env, protocol.IOError, "cannot persist owner fencing state")
		return
	}
	c.epoch++
	epoch := protocol.Number(c.epoch)
	generation := c.generation
	owner := lease.Owner{PlatformID: c.config.Platform.ID, Generation: generation, Epoch: epoch, OwnerEpoch: env.OwnerEpoch, Fingerprint: fingerprint}
	identity := transport.Identity{GatewayID: c.config.GatewayID, PlatformID: c.config.Platform.ID, Generation: &generation, ConnectionEpoch: &epoch, OwnerEpoch: env.OwnerEpoch}
	peer := transport.NewPeer(c.ctx, conn, identity, &c.counters)
	c.peer = peer
	c.owner = owner
	c.ready = false
	effective, revision := c.effective, c.revision
	c.mu.Unlock()
	defer func() {
		peer.Close()
		c.mu.Lock()
		if c.peer == peer {
			c.ready = false
			c.peer = nil
			c.owner = lease.Owner{}
		}
		c.mu.Unlock()
		c.Hub.Invalidate()
	}()
	reply := protocol.HelloAck{SelectedVersion: protocol.Version, Capabilities: c.Capabilities(), Config: effective, ConfigRevision: revision, HeartbeatIntervalMS: effective.HeartbeatIntervalMS, HeartbeatTimeoutMS: effective.HeartbeatTimeoutMS, LeaseTTLMS: effective.LeaseTTLMS, CooldownRemainingMS: c.Ports.CooldownMS()}
	if e = peer.Reply(c.ctx, env, reply, nil); e != nil {
		return
	}
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
	workerCtx, stop := context.WithCancel(c.ctx)
	defer stop()
	queue := make(chan transport.Incoming, 32)
	var workers sync.WaitGroup
	workers.Add(4)
	for i := 0; i < 4; i++ {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-workerCtx.Done():
					return
				case in := <-queue:
					c.handle(workerCtx, peer, owner, in)
				}
			}
		}()
	}
	defer func() { stop(); workers.Wait() }()
	// 应用心跳独立于 Ping；没有有效 heartbeat 的连接不能继续持有控制权。
	lastHeartbeat := time.Now()
	for {
		c.mu.Lock()
		timeout := c.effective.HeartbeatTimeoutMS
		c.mu.Unlock()
		nextCtx, cancel := context.WithDeadline(c.ctx, lastHeartbeat.Add(time.Duration(timeout)*time.Millisecond))
		in, e := peer.Next(nextCtx)
		cancel()
		if e != nil {
			return
		}
		if in.Envelope.Type == "heartbeat" {
			lastHeartbeat = time.Now()
			c.handle(c.ctx, peer, owner, in)
			continue
		}
		if in.Envelope.Type == "sip.packet" {
			c.handle(c.ctx, peer, owner, in)
			continue
		}
		select {
		case queue <- in:
		case <-peer.Done():
			return
		default:
			if in.Envelope.Kind == "request" {
				_ = peer.Reply(c.ctx, in.Envelope, nil, protocol.Failure(protocol.Exhausted, "gateway command workers busy"))
			}
		}
	}
}
func (c *Controller) CurrentOwner(fingerprint string) (lease.Owner, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.peer == nil || !c.ready || c.owner.Fingerprint != fingerprint {
		return lease.Owner{}, protocol.Failure(protocol.Forbidden, "active matching control identity required")
	}
	return c.owner, nil
}
func (c *Controller) packet(route protocol.Route, data []byte) error {
	c.counters.DeviceReceived(len(data))
	c.mu.Lock()
	p, ready := c.peer, c.ready
	c.mu.Unlock()
	if p == nil || !ready {
		return protocol.Failure(protocol.GatewayDisabled, "no control owner")
	}
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	return p.Event(ctx, "sip.packet", protocol.SIPPacket{Route: route, RawBase64: base64.StdEncoding.EncodeToString(data)})
}
func (c *Controller) Event(name string, payload any) {
	c.mu.Lock()
	p, ready := c.peer, c.ready
	c.mu.Unlock()
	if p == nil || !ready {
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	_ = p.Event(ctx, name, payload)
}
func asError(err error) *protocol.Error {
	if err == nil {
		return nil
	}
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return protocol.Failure(protocol.UpstreamTimeout, "gateway operation timed out")
	}
	return protocol.Failure(protocol.InternalError, "gateway operation failed")
}
func (c *Controller) handle(ctx context.Context, p *transport.Peer, owner lease.Owner, in transport.Incoming) {
	if in.Envelope.Kind == "event" {
		if packet, ok := in.Payload.(*protocol.SIPPacket); ok {
			if data, e := protocol.DecodeSIP(*packet); e == nil {
				if err := c.Hub.Send(packet.Route, data); err == nil {
					c.counters.DeviceSent(len(data))
				}
			}
		}
		return
	}
	if in.Envelope.Kind != "request" || in.Envelope.Type == "hello" {
		return
	}
	c.mu.Lock()
	valid := c.peer == p && c.ready && c.owner == owner
	c.mu.Unlock()
	if !valid {
		return
	}
	deadline := 5 * time.Second
	if in.Envelope.Type == "media.connect" || in.Envelope.Type == "config.apply" {
		deadline = 10 * time.Second
	}
	opCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	result, failure := c.executeIdempotent(opCtx, owner, in)
	_ = p.Reply(opCtx, in.Envelope, result, failure)
}
