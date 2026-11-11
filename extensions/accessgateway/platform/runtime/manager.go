// Package runtime 管理每个 B 的独立连接、所有权、代次和运行快照。
package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
)

type DialFunc func(context.Context, string, string, *tls.Config, http.Header) (*websocket.Conn, error)
type Manager struct {
	mu          sync.RWMutex
	config      core.Config
	repo        core.RuntimeRepository
	credentials core.CredentialLoader
	host        core.SignalHost
	instanceID  string
	ctx         context.Context
	cancel      context.CancelFunc
	workers     map[string]*worker
	closed      bool
	wg          sync.WaitGroup
	dial        DialFunc
	connecting  chan struct{}
	reconcileMu sync.Mutex
}
type worker struct {
	mu          sync.RWMutex
	manager     *Manager
	config      core.RuntimeConfig
	snapshot    core.RuntimeSnapshot
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	peer        *transport.Peer
	ownerEpoch  int64
	leases      map[string]protocol.ResourceRef
	ready       map[string]chan struct{}
	reconfigure chan struct{}
}

func New(parent context.Context, config core.Config, repo core.RuntimeRepository, credentials core.CredentialLoader, host core.SignalHost) *Manager {
	ctx, cancel := context.WithCancel(parent)
	return &Manager{config: config, repo: repo, credentials: credentials, host: host, instanceID: uuid.NewString(), ctx: ctx, cancel: cancel, workers: map[string]*worker{}, dial: transport.Dial, connecting: make(chan struct{}, 4)}
}
func (m *Manager) Start() error {
	if !m.config.Enabled {
		return nil
	}
	if err := m.Reconcile(m.ctx); err != nil {
		return err
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
				_ = m.Reconcile(ctx)
				cancel()
			}
		}
	}()
	return nil
}
func (m *Manager) Reconcile(ctx context.Context) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	rows, err := m.repo.RuntimeConfigs(ctx)
	if err != nil {
		return err
	}
	wanted := map[string]core.RuntimeConfig{}
	for _, v := range rows {
		wanted[v.GatewayPK] = v
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return context.Canceled
	}
	stopping := []*worker{}
	for id, w := range m.workers {
		next, exists := wanted[id]
		old := w.configuration()
		select {
		case <-w.done:
			delete(m.workers, id)
			continue
		default:
		}
		if !exists || connectionChanged(old, next) {
			w.cancel()
			stopping = append(stopping, w)
			delete(m.workers, id)
		} else {
			w.mu.Lock()
			w.config = next
			w.mu.Unlock()
			if old.AppliedRevision != next.AppliedRevision || old.Desired.HeartbeatIntervalMS != next.Desired.HeartbeatIntervalMS {
				select {
				case w.reconfigure <- struct{}{}:
				default:
				}
			}
		}
	}
	m.mu.Unlock()
	for _, w := range stopping {
		select {
		case <-w.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return context.Canceled
	}
	for id, row := range wanted {
		if _, exists := m.workers[id]; exists {
			continue
		}
		if len(m.workers) >= m.config.MaxConnections {
			return core.Fail(429, "RESOURCE_EXHAUSTED", "超过 A 的网关连接总额度")
		}
		child, cancel := context.WithCancel(m.ctx)
		w := &worker{manager: m, config: row, ctx: child, cancel: cancel, done: make(chan struct{}), reconfigure: make(chan struct{}, 1), snapshot: core.RuntimeSnapshot{ConnectionState: "disconnected", HealthState: "unknown", Capabilities: []string{}}}
		m.workers[id] = w
		m.wg.Add(1)
		go func() { defer m.wg.Done(); defer close(w.done); w.run() }()
	}
	return nil
}
func connectionChanged(a, b core.RuntimeConfig) bool {
	ptr := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	return a.Desired.URL != b.Desired.URL || ptr(a.Desired.CredentialID) != ptr(b.Desired.CredentialID) || ptr(a.Desired.ProxyRef) != ptr(b.Desired.ProxyRef)
}
func (w *worker) configuration() core.RuntimeConfig {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.config
}
func (m *Manager) Snapshot(id string) core.RuntimeSnapshot {
	m.mu.RLock()
	w := m.workers[id]
	m.mu.RUnlock()
	if w == nil {
		return core.RuntimeSnapshot{ConnectionState: "disconnected", HealthState: "unknown", Capabilities: []string{}}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	snapshot := w.snapshot
	snapshot.Capabilities = slices.Clone(snapshot.Capabilities)
	return snapshot
}
func (m *Manager) Call(ctx context.Context, id, name string, payload any) (any, error) {
	m.mu.RLock()
	w := m.workers[id]
	m.mu.RUnlock()
	if w == nil {
		return nil, core.Fail(503, "GATEWAY_UNAVAILABLE", "网关未连接")
	}
	w.mu.RLock()
	p := w.peer
	w.mu.RUnlock()
	if p == nil {
		return nil, core.Fail(503, "GATEWAY_UNAVAILABLE", "网关未连接")
	}
	reply, err := p.Call(ctx, name, payload)
	return reply.Payload, err
}

func (m *Manager) OwnerLease(id string) (string, int64, bool) {
	m.mu.RLock()
	w := m.workers[id]
	m.mu.RUnlock()
	if w == nil {
		return "", 0, false
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return m.instanceID, w.ownerEpoch, w.ownerEpoch > 0 && w.ctx.Err() == nil
}

// Reconnect 只重建指定网关，调用方必须先完成业务排空。
func (m *Manager) Reconnect(ctx context.Context, id string) error {
	m.mu.RLock()
	w := m.workers[id]
	m.mu.RUnlock()
	if w != nil {
		w.cancel()
		select {
		case <-w.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return m.Reconcile(ctx)
}
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
	return nil
}
func (w *worker) setState(connection, health string, err error) {
	w.mu.Lock()
	w.snapshot.ConnectionState = connection
	w.snapshot.HealthState = health
	if err == nil {
		w.snapshot.Error = nil
	} else {
		var pe *protocol.Error
		if errors.As(err, &pe) {
			copy := *pe
			w.snapshot.Error = &copy
		} else {
			w.snapshot.Error = protocol.Failure(protocol.IOError, err.Error())
		}
	}
	failure := w.snapshot.Error
	last := w.snapshot.LastSeenAt
	epoch := w.ownerEpoch
	row := w.config
	w.mu.Unlock()
	if epoch > 0 {
		ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
		_ = w.manager.repo.RuntimeCheckpoint(ctx, row.GatewayPK, w.manager.instanceID, epoch, last, failure)
		cancel()
	}
}
func (w *worker) run() {
	m := w.manager
	row := w.configuration()
	epoch, err := m.repo.AcquireOwner(w.ctx, row.GatewayPK, m.instanceID)
	if err != nil {
		w.setState("disconnected", "unavailable", err)
		return
	}
	w.mu.Lock()
	w.ownerEpoch = epoch
	w.snapshot.OwnerEpoch = protocol.Number(epoch)
	w.mu.Unlock()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.repo.ReleaseOwner(ctx, row.GatewayPK, m.instanceID, epoch)
	}()
	ownerCtx, ownerCancel := context.WithCancel(w.ctx)
	defer ownerCancel()
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ownerCtx.Done():
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(ownerCtx, 5*time.Second)
				err := m.repo.RenewOwner(ctx, row.GatewayPK, m.instanceID, epoch)
				cancel()
				if err != nil {
					w.setState("disconnected", "unavailable", err)
					w.cancel()
					return
				}
			}
		}
	}()
	defer func() { ownerCancel(); <-renewDone }()
	backoff := time.Second
	for {
		if w.ctx.Err() != nil {
			return
		}
		w.setState("connecting", "unknown", nil)
		err = w.connect()
		if w.ctx.Err() != nil {
			return
		}
		w.setState("reconnecting", "unavailable", err)
		delay := backoff + time.Duration(rand.Int64N(int64(backoff)/3+1))
		var pe *protocol.Error
		var certificateError *tls.CertificateVerificationError
		var unknownCA x509.UnknownAuthorityError
		var hostname x509.HostnameError
		var invalidCert x509.CertificateInvalidError
		if errors.As(err, &certificateError) || errors.As(err, &unknownCA) || errors.As(err, &hostname) || errors.As(err, &invalidCert) || errors.As(err, &pe) && !pe.Retryable {
			delay = 5 * time.Minute
		}
		timer := time.NewTimer(delay)
		select {
		case <-w.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}
func (w *worker) connect() error {
	m := w.manager
	row := w.configuration()
	if row.Desired.CredentialID == nil {
		return protocol.Failure(protocol.Forbidden, "网关尚未配置客户端凭据")
	}
	if row.Desired.ProxyRef != nil {
		return protocol.Failure(protocol.CapabilityUnsupported, "当前未配置受控 HTTP CONNECT 代理适配器")
	}
	refs, err := m.repo.RuntimeCredential(w.ctx, row.GatewayPK, *row.Desired.CredentialID)
	if err != nil {
		return err
	}
	tlsConfig, err := m.credentials.Load(refs.CA, refs.Cert, refs.Key)
	if err != nil {
		return protocol.Failure(protocol.Forbidden, "客户端凭据缺失、过期或无效")
	}
	select {
	case m.connecting <- struct{}{}:
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
	dialCtx, cancel := context.WithTimeout(w.ctx, 15*time.Second)
	conn, err := m.dial(dialCtx, row.Desired.URL, protocol.ControlPath, tlsConfig, nil)
	cancel()
	<-m.connecting
	if err != nil {
		return err
	}
	p := transport.NewPeer(w.ctx, conn, transport.Identity{GatewayID: row.GatewayID, PlatformID: row.Desired.PlatformID, OwnerEpoch: protocol.Number(w.ownerEpoch)})
	defer p.Close()
	w.setState("authenticating", "unknown", nil)
	helloCtx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	reply, err := p.Call(helloCtx, "hello", protocol.Hello{ClientInstanceID: m.instanceID, SupportedVersions: []int{protocol.Version}, RequiredCapabilities: row.Desired.RequiredCapabilities, LastAppliedRevision: protocol.Number(row.AppliedRevision)})
	cancel()
	if err != nil {
		return err
	}
	ack, ok := reply.Payload.(*protocol.HelloAck)
	if !ok {
		return protocol.Failure(protocol.InvalidArgument, "hello 应答类型错误")
	}

	hostCaps, err := m.host.SupportedGatewayCapabilities(w.ctx, row.Desired)
	if err != nil {
		return err
	}
	capabilities := []string{}
	for _, name := range ack.Capabilities {
		if slices.Contains(hostCaps, name) {
			capabilities = append(capabilities, name)
		}
	}
	for _, name := range row.Desired.RequiredCapabilities {
		if !slices.Contains(capabilities, name) {
			return protocol.Failure(protocol.CapabilityUnsupported, "A 媒体驱动或 B 不满足要求的能力")
		}
	}
	id := p.Identity()
	now := time.Now().UTC()
	w.mu.Lock()
	w.peer = p
	w.snapshot.Generation = *id.Generation
	w.snapshot.Epoch = *id.ConnectionEpoch
	w.snapshot.ConnectedAt = &now
	w.snapshot.LastSeenAt = &now
	w.snapshot.Capabilities = capabilities
	w.snapshot.Status = nil
	w.snapshot.Metrics = nil
	w.snapshot.ReceivedAt = nil
	w.snapshot.MetricsReceivedAt = nil
	w.snapshot.StatusReceivedAt = nil
	w.snapshot.RTTMS = nil
	w.leases = map[string]protocol.ResourceRef{}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if w.peer == p {
			w.peer = nil
			w.leases = map[string]protocol.ResourceRef{}
		}
		w.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = m.host.InvalidateIngress(ctx, row.GatewayPK, *id.Generation, *id.ConnectionEpoch)
	}()
	if err = w.recover(p); err != nil {
		return err
	}
	health := "ready"
	hash, err := protocol.ConfigHash(row.Desired.EffectiveConfig)
	if err != nil {
		return err
	}
	remoteHash, err := protocol.ConfigHash(ack.Config)
	if err != nil {
		return err
	}
	if ack.ConfigRevision != protocol.Number(row.ConfigRevision) || hash != remoteHash || ack.CooldownRemainingMS > 0 {
		health = "degraded"
	}
	w.setState("connected", health, nil)
	return w.serve(p, ack)
}
func (w *worker) recover(p *transport.Peer) error {
	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	defer cancel()
	row := w.configuration()
	var cursor *string
	for {
		response, err := p.Call(ctx, "resources.list", protocol.ResourcesList{ResourceType: "all", Cursor: cursor, Size: 200})
		if err != nil {
			return err
		}
		resources := response.Payload.(*protocol.ResourcesResult)
		for _, resource := range resources.Items {
			if resource.Ref.ResourceType == "flow" {
				continue
			}
			reply, err := p.Call(ctx, "resources.close", protocol.ResourceClose{OperationID: uuid.NewString(), Ref: resource.Ref, Reason: "control_recovery"})
			if err != nil {
				return err
			}
			if !reply.Payload.(*protocol.ResourceCloseResult).Closed {
				return protocol.Failure(protocol.UpstreamTimeout, "B 遗留资源仍待回收")
			}
		}
		if resources.NextCursor == nil {
			break
		}
		cursor = resources.NextCursor
	}
	sessions, err := w.manager.repo.RecoverySessions(ctx, row.GatewayPK)
	if err != nil {
		return err
	}
	for _, session := range sessions {
		remote := session.RemoteClosed
		if !remote {
			response, err := p.Call(ctx, "resources.close", protocol.ResourceClose{OperationID: uuid.NewString(), Ref: session.Ref, Reason: "platform_recovery"})
			if err != nil {
				return err
			}
			remote = response.Payload.(*protocol.ResourceCloseResult).Closed
		}
		local := session.LocalClosed
		if !local {
			local, err = w.manager.host.RecoverLocalSession(ctx, session)
		}
		outcome := core.RecoveryOutcome{RemoteClosed: remote, LocalClosed: local, DialogUnavailable: true}
		if err != nil {
			outcome.Error = protocol.Failure(protocol.IOError, "原媒体节点资源身份无法确认或清理失败")
		}
		if saveErr := w.manager.repo.SaveRecovery(ctx, row.GatewayPK, w.manager.instanceID, w.ownerEpoch, session, outcome); saveErr != nil {
			return saveErr
		}
		if !remote || !local {
			return protocol.Failure(protocol.UpstreamTimeout, "遗留会话等待核对，禁止恢复新业务")
		}
	}
	return nil
}
