// Package transfer 处理任务限定的 HTTPS/裸 TCP 转送，不暴露任意 URL 代理。
package transfer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/lease"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/media"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/sip"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/state"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
)

type Manager struct {
	Counters   *transport.Counters
	mu         sync.Mutex
	ctx        context.Context
	registry   *lease.Registry
	hub        *sip.Hub
	ports      *media.Ports
	budget     *media.Budget
	store      *state.Store
	config     protocol.EffectiveConfig
	revision   protocol.Decimal
	event      func(string, any)
	tasks      map[string]*Session
	maxRecords int
	draining   bool
}
type Session struct {
	mu         sync.Mutex
	manager    *Manager
	owner      lease.Owner
	ctx        context.Context
	cancel     context.CancelFunc
	spec       protocol.TransferSpec
	ref        protocol.ResourceRef
	publicHash [32]byte
	binary     *transport.Binary
	ready      chan struct{}
	commitDone chan struct{}
	committed  bool
	eof        bool
	busy       bool
	phase      string
	bytes      int64
	digest     hash.Hash
	sha        string
	listener   net.Listener
	remote     net.Conn
	release    func() error
	publicTCP  *protocol.Endpoint
	allowed    map[netip.Addr]bool
	failed     sync.Once
	wg         sync.WaitGroup
	expires    time.Time
}

func New(ctx context.Context, registry *lease.Registry, hub *sip.Hub, ports *media.Ports, budget *media.Budget, store *state.Store, c protocol.EffectiveConfig, revision protocol.Decimal, maxRecords int, event func(string, any)) *Manager {
	return &Manager{ctx: ctx, registry: registry, hub: hub, ports: ports, budget: budget, store: store, config: c, revision: revision, maxRecords: maxRecords, event: event, tasks: map[string]*Session{}}
}
func (m *Manager) Configure(c protocol.EffectiveConfig, revision protocol.Decimal) {
	m.mu.Lock()
	m.config = c
	m.revision = revision
	m.mu.Unlock()
}
func (m *Manager) Drain(value bool) { m.mu.Lock(); m.draining = value; m.mu.Unlock() }
func (m *Manager) Open(owner lease.Owner, request protocol.TransferOpen) (protocol.TransferOpened, error) {
	if e := protocol.ValidateTransfer(request.Spec); e != nil {
		return protocol.TransferOpened{}, e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.draining {
		return protocol.TransferOpened{}, protocol.Failure(protocol.GatewayDraining, "gateway is draining")
	}
	if m.registry.Count("transfer") >= m.config.MaxTransferSessions {
		return protocol.TransferOpened{}, protocol.Failure(protocol.Exhausted, "transfer session quota")
	}
	persisted := m.store.Snapshot()
	count := 0
	for _, v := range persisted.Transfers {
		if time.Now().Before(v.ExpiresAt) {
			count++
		}
	}
	if count >= m.maxRecords {
		return protocol.TransferOpened{}, protocol.Failure(protocol.Exhausted, "transfer receipt storage budget")
	}
	if request.Spec.Kind != "raw_download" && m.config.TaskPublicBaseURL == nil {
		return protocol.TransferOpened{}, protocol.Failure(protocol.CapabilityUnsupported, "device HTTPS task listener is not configured")
	}
	subjectType := request.Spec.SubjectType
	if subjectType == "" {
		subjectType = "device"
	}
	route, e := m.hub.SubjectRoute(subjectType, request.Spec.SubjectID)
	if e != nil {
		return protocol.TransferOpened{}, e
	}
	allowed, e := media.AllowedSources(request.Spec.SourceIPs, route.Peer.IP, m.config.SourceCIDRs)
	if e != nil {
		return protocol.TransferOpened{}, e
	}
	taskKey := owner.PlatformID + "/" + request.Spec.TaskID
	for key, v := range m.tasks {
		v.mu.Lock()
		expired := time.Now().After(v.expires.Add(24 * time.Hour))
		v.mu.Unlock()
		if expired {
			delete(m.tasks, key)
		}
	}
	if _, exists := m.tasks[taskKey]; exists {
		return protocol.TransferOpened{}, protocol.Failure(protocol.BindingConflict, "task already has a transfer")
	}
	ctx, cancel := context.WithDeadline(m.ctx, time.Now().Add(time.Duration(request.Spec.TTLMS)*time.Millisecond))
	s := &Session{manager: m, owner: owner, ctx: ctx, cancel: cancel, spec: request.Spec, ready: make(chan struct{}), commitDone: make(chan struct{}), digest: sha256.New(), phase: "allocated", allowed: allowed, expires: time.Now().Add(time.Duration(request.Spec.TTLMS) * time.Millisecond)}
	s.ref = protocol.ResourceRef{ResourceType: "transfer", ID: uuid.NewString(), Generation: owner.Generation, ConnectionEpoch: owner.Epoch}
	// 在任何 socket/票据创建前为最终回执保留空间，关闭时只替换这一有界记录。
	if err := m.saveReceipt(state.TransferResult{PlatformID: owner.PlatformID, TaskID: s.spec.TaskID, TransferID: s.ref.ID, Ref: s.ref, Received: "0", Sent: "0", Phase: "allocated", State: "allocated", ExpiresAt: s.expires.Add(24 * time.Hour), CleanupReserve: strings.Repeat(" ", 8192)}); err != nil {
		cancel()
		return protocol.TransferOpened{}, err
	}
	published := false
	defer func() {
		if !published {
			_ = s.close()
		}
	}()
	var publicURL *string
	if request.Spec.Kind == "raw_download" {
		listener, port, release, e := m.ports.ReserveTCP(m.config)
		if e != nil {
			cancel()
			return protocol.TransferOpened{}, e
		}
		s.listener = listener
		s.release = release
		s.publicTCP = &protocol.Endpoint{IP: m.config.PublicIP, Port: port}
	} else {
		tokenBytes := make([]byte, 32)
		if _, e := rand.Read(tokenBytes); e != nil {
			cancel()
			return protocol.TransferOpened{}, e
		}
		token := base64.RawURLEncoding.EncodeToString(tokenBytes)
		s.publicHash = sha256.Sum256([]byte(token))
		url := strings.TrimRight(*m.config.TaskPublicBaseURL, "/") + "/tasks/" + request.Spec.TaskID + "/" + token
		publicURL = &url
	}
	revision := protocol.Decimal("0")
	if route.BindingRevision != nil {
		revision = *route.BindingRevision
	}
	ref, e := m.registry.Add(lease.Resource{Ref: s.ref, Owner: owner, OperationID: request.OperationID, SubjectType: subjectType, SubjectID: request.Spec.SubjectID, BindingRevision: revision, ConfigRevision: m.revision, TTL: time.Duration(m.config.LeaseTTLMS) * time.Millisecond, HardDeadline: s.expires, Close: s.close, Value: s})
	if e != nil {
		cancel()
		return protocol.TransferOpened{}, e
	}
	s.ref = ref
	m.tasks[taskKey] = s
	published = true
	if request.Spec.Kind == "raw_download" && request.Spec.WireMode == "tcp_passive" {
		s.wg.Add(1)
		go s.acceptTCP()
	}
	return protocol.TransferOpened{TransferID: ref.ID, PublicURL: publicURL, PublicTCP: s.publicTCP, ExpiresInMS: request.Spec.TTLMS, MaxBytes: request.Spec.MaxBytes}, nil
}
func (m *Manager) Attach(owner lease.Owner, id string, value any, conn *websocket.Conn) error {
	s, ok := value.(*Session)
	if !ok || s.owner != owner {
		return protocol.Failure(protocol.Forbidden, "wrong transfer resource")
	}
	s.mu.Lock()
	if s.binary != nil || s.ctx.Err() != nil {
		s.mu.Unlock()
		return protocol.Failure(protocol.NotFound, "transfer unavailable")
	}
	s.binary = transport.NewBinary(s.ctx, conn, "", s.spec.Direction, true, false, m.Counters)
	if e := m.registry.Attached(id); e != nil {
		s.binary.Close()
		s.mu.Unlock()
		return e
	}
	s.phase = "attached"
	close(s.ready)
	s.mu.Unlock()
	m.event("transfer.ready", protocol.TransferReady{TransferID: id})
	return nil
}
func (m *Manager) Connect(ctx context.Context, owner lease.Owner, request protocol.TransferConnect) error {
	value, e := m.registry.Get(owner, request.TransferID)
	if e != nil {
		return e
	}
	s, ok := value.(*Session)
	if !ok {
		return protocol.Failure(protocol.Forbidden, "wrong transfer resource")
	}
	if e = request.Target.Validate(); e != nil {
		return e
	}
	ip := netip.MustParseAddr(request.Target.IP).Unmap()
	if !s.allowed[ip] {
		return protocol.Failure(protocol.Forbidden, "transfer target outside source policy")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spec.Kind != "raw_download" || s.spec.WireMode != "tcp_active" || s.remote != nil || s.ctx.Err() != nil {
		return protocol.Failure(protocol.InvalidArgument, "transfer is not awaiting active TCP connection")
	}
	s.listener.Close()
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	d := net.Dialer{LocalAddr: &net.TCPAddr{Port: s.publicTCP.Port}}
	conn, e := d.DialContext(dialCtx, "tcp", request.Target.String())
	if e != nil {
		return protocol.Failure(protocol.DeviceUnreachable, "raw download device connection failed")
	}
	s.remote = conn
	s.wg.Add(1)
	go s.readRaw(conn)
	return nil
}
func (s *Session) waitReady() (*transport.Binary, error) {
	select {
	case <-s.ready:
		s.mu.Lock()
		b := s.binary
		s.mu.Unlock()
		return b, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}
func (s *Session) acceptTCP() {
	defer s.wg.Done()
	for {
		conn, e := s.listener.Accept()
		if e != nil {
			return
		}
		host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		ip, e := netip.ParseAddr(host)
		if e != nil || !s.allowed[ip.Unmap()] {
			conn.Close()
			continue
		}
		s.mu.Lock()
		if s.ctx.Err() != nil {
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.remote = conn
		s.mu.Unlock()
		s.listener.Close()
		s.forwardRaw(conn)
		return
	}
}
func (s *Session) readRaw(conn net.Conn) { defer s.wg.Done(); s.forwardRaw(conn) }
func (s *Session) forwardRaw(conn net.Conn) {
	if s.spec.Direction == "a_to_device" {
		s.writeRaw(conn)
		return
	}
	b, e := s.waitReady()
	if e != nil {
		return
	}
	buffer := make([]byte, 65536)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, e := conn.Read(buffer)
		if n > 0 {
			if err := s.sendBytes(b, buffer[:n], true); err != nil {
				s.fail(err)
				return
			}
		}
		if e != nil {
			if errors.Is(e, io.EOF) {
				if err := s.finishBytes(b); err != nil {
					s.fail(err)
				}
				return
			}
			if s.ctx.Err() == nil {
				s.fail(e)
			}
			return
		}
	}
}
func (s *Session) sendBytes(b *transport.Binary, data []byte, toA bool) error {
	s.mu.Lock()
	limit, _ := s.spec.MaxBytes.Int64()
	if s.eof || int64(len(data)) > limit-s.bytes {
		s.mu.Unlock()
		return protocol.Failure(protocol.TransferLimit, "transfer byte limit exceeded")
	}
	s.mu.Unlock()
	if !s.manager.budget.Allow(toA, len(data)) {
		return protocol.Failure(protocol.MediaBackpressure, "gateway transfer bandwidth budget exceeded")
	}
	if e := b.Send(s.ctx, protocol.FileBytes, data); e != nil {
		return e
	}
	s.mu.Lock()
	s.bytes += int64(len(data))
	_, _ = s.digest.Write(data)
	s.phase = "receiving"
	s.mu.Unlock()
	s.manager.registry.AddBytes(s.ref.ID, toA, len(data))
	s.manager.registry.Receiving(s.ref.ID)
	return nil
}
func (s *Session) finishBytes(b *transport.Binary) error {
	s.mu.Lock()
	s.sha = hex.EncodeToString(s.digest.Sum(nil))
	s.eof = true
	s.phase = "waiting_commit"
	expected := s.spec.ExpectedSHA256
	actual := s.sha
	s.mu.Unlock()
	if expected != nil && *expected != actual {
		return protocol.Failure(protocol.InvalidArgument, "transfer checksum mismatch")
	}
	if e := b.Send(s.ctx, protocol.EOF, nil); e != nil {
		return e
	}
	s.manager.registry.EOF(s.ref.ID, true)
	return nil
}
func (m *Manager) Finish(owner lease.Owner, request protocol.TransferFinish) (protocol.TransferFinished, error) {
	// 已提交回执可跨控制代次只读核对；finish 仅允许当前资源代次。
	value, e := m.registry.Get(owner, request.TransferID)
	if e != nil {
		receipt, queryErr := m.Query(owner.PlatformID, request.TransferID)
		if queryErr == nil && receipt.Ref.Generation == owner.Generation && receipt.Ref.ConnectionEpoch == owner.Epoch && receipt.Committed && receipt.SHA256 != nil && *receipt.SHA256 == request.SHA256 && (receipt.BytesReceived == request.ReceivedBytes || receipt.BytesSent == request.ReceivedBytes) {
			return protocol.TransferFinished{TransferID: request.TransferID, Committed: true}, nil
		}
		return protocol.TransferFinished{}, e
	}
	s, ok := value.(*Session)
	if !ok {
		return protocol.TransferFinished{}, protocol.Failure(protocol.Forbidden, "wrong transfer resource")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.eof || s.phase != "waiting_commit" || protocol.Number(s.bytes) != request.ReceivedBytes || s.sha != request.SHA256 {
		return protocol.TransferFinished{}, protocol.Failure(protocol.BindingConflict, "transfer has not reached verified EOF")
	}
	hash := s.sha
	received, sent := protocol.Decimal("0"), protocol.Decimal("0")
	if s.spec.Direction == "device_to_a" {
		received = protocol.Number(s.bytes)
	} else {
		sent = protocol.Number(s.bytes)
	}
	record := state.TransferResult{PlatformID: owner.PlatformID, TaskID: s.spec.TaskID, TransferID: s.ref.ID, Ref: s.ref, Received: received, Sent: sent, SHA256: &hash, Committed: true, Phase: "completed", State: "closed", ExpiresAt: time.Now().Add(24 * time.Hour)}
	if e = m.saveReceipt(record); e != nil {
		return protocol.TransferFinished{}, e
	}
	s.committed = true
	s.phase = "completed"
	close(s.commitDone)
	go func() {
		m.event("transfer.completed", protocol.TransferCompleted{TransferID: s.ref.ID, Committed: true, Bytes: protocol.Number(s.bytes), SHA256: hash})
		_, _ = m.registry.CloseRef(owner.PlatformID, s.ref)
	}()
	return protocol.TransferFinished{TransferID: request.TransferID, Committed: true}, nil
}
func (m *Manager) saveReceipt(record state.TransferResult) error {
	return m.store.Update(func(d *state.Data) error {
		kept := make([]state.TransferResult, 0, len(d.Transfers)+1)
		found := false
		for _, v := range d.Transfers {
			if time.Now().After(v.ExpiresAt) {
				continue
			}
			if v.TransferID == record.TransferID && v.PlatformID == record.PlatformID {
				kept = append(kept, record)
				found = true
			} else {
				kept = append(kept, v)
			}
		}
		if !found {
			if len(kept) >= m.maxRecords {
				return protocol.Failure(protocol.Exhausted, "transfer receipt storage budget")
			}
			kept = append(kept, record)
		}
		d.Transfers = kept
		return nil
	})
}
func (m *Manager) Query(platform, id string) (protocol.TransferQueryResult, error) {
	m.mu.Lock()
	var s *Session
	for _, v := range m.tasks {
		if v.ref.ID == id && v.owner.PlatformID == platform {
			s = v
			break
		}
	}
	m.mu.Unlock()
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		var hash *string
		if s.sha != "" {
			value := s.sha
			hash = &value
		}
		received, sent := protocol.Decimal("0"), protocol.Decimal("0")
		if s.spec.Direction == "device_to_a" {
			received = protocol.Number(s.bytes)
		} else {
			sent = protocol.Number(s.bytes)
		}
		state := "receiving"
		if s.phase == "allocated" || s.phase == "attached" {
			state = s.phase
		}
		if s.committed || s.ctx.Err() != nil {
			state = "closed"
		}
		return protocol.TransferQueryResult{TransferID: id, Ref: s.ref, Phase: s.phase, State: state, BytesReceived: received, BytesSent: sent, SHA256: hash, Committed: s.committed}, nil
	}
	for _, record := range m.store.Snapshot().Transfers {
		if record.PlatformID == platform && record.TransferID == id && time.Now().Before(record.ExpiresAt) {
			return protocol.TransferQueryResult{TransferID: id, Ref: record.Ref, Phase: record.Phase, State: record.State, BytesReceived: record.Received, BytesSent: record.Sent, SHA256: record.SHA256, Committed: record.Committed}, nil
		}
	}
	return protocol.TransferQueryResult{}, protocol.Failure(protocol.NotFound, "transfer receipt not found")
}
func (s *Session) fail(err error) {
	s.failed.Do(func() {
		go func() {
			code := protocol.IOError
			var failure *protocol.Error
			if errors.As(err, &failure) {
				code = failure.Code
			}
			s.manager.event("transfer.failed", protocol.TransferFailed{TransferID: s.ref.ID, Error: *protocol.Failure(code, "transfer failed")})
			_, _ = s.manager.registry.CloseRef(s.owner.PlatformID, s.ref)
		}()
	})
}
func (s *Session) close() error {
	s.cancel()
	s.mu.Lock()
	conn, b, listener := s.remote, s.binary, s.listener
	committed := s.committed
	s.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
	if listener != nil {
		listener.Close()
	}
	if b != nil {
		b.Close()
	}
	s.wg.Wait()
	if s.release != nil {
		if e := s.release(); e != nil {
			return e
		}
	}
	if !committed {
		s.mu.Lock()
		s.phase = "failed"
		var hash *string
		if s.sha != "" {
			value := s.sha
			hash = &value
		}
		received, sent := protocol.Decimal("0"), protocol.Decimal("0")
		if s.spec.Direction == "device_to_a" {
			received = protocol.Number(s.bytes)
		} else {
			sent = protocol.Number(s.bytes)
		}
		record := state.TransferResult{PlatformID: s.owner.PlatformID, TaskID: s.spec.TaskID, TransferID: s.ref.ID, Ref: s.ref, Received: received, Sent: sent, SHA256: hash, Committed: false, Phase: "failed", State: "closed", ExpiresAt: time.Now().Add(24 * time.Hour)}
		s.mu.Unlock()
		if e := s.manager.saveReceipt(record); e != nil {
			return e
		}
	}
	s.mu.Lock()
	s.binary = nil
	s.remote = nil
	s.mu.Unlock()
	return nil
}

// TaskHandler 独立于 WSS mTLS，仅按任务凭据认证；不记录原始路径。
func (m *Manager) TaskHandler() http.Handler { return http.HandlerFunc(m.taskHTTP) }
func (m *Manager) taskHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "tasks" || !protocol.ValidID(parts[1]) || len(parts[2]) != 43 || r.URL.RawQuery != "" || r.URL.RawPath != "" {
		http.NotFound(w, r)
		return
	}
	digest := sha256.Sum256([]byte(parts[2]))
	m.mu.Lock()
	var s *Session
	for _, task := range m.tasks {
		if task.spec.TaskID == parts[1] {
			s = task
			break
		}
	}
	m.mu.Unlock()
	if s == nil || subtle.ConstantTimeCompare(digest[:], s.publicHash[:]) != 1 {
		http.NotFound(w, r)
		return
	}
	method := http.MethodPost
	if s.spec.Kind == "upgrade_download" {
		method = http.MethodGet
	}
	if r.Method != method || s.spec.Kind == "raw_download" {
		w.Header().Set("Allow", method)
		http.Error(w, "method not allowed", 405)
		return
	}
	s.mu.Lock()
	if s.committed {
		size, hash := s.bytes, s.sha
		s.mu.Unlock()
		if method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"committed":true,"bytes":"%d","sha256":"%s"}`, size, hash)
		} else {
			http.Error(w, "transfer already consumed", 410)
		}
		return
	}
	if s.ctx.Err() != nil || time.Now().After(s.expires) {
		s.mu.Unlock()
		http.Error(w, "task expired", 410)
		return
	}
	if s.busy {
		s.mu.Unlock()
		http.Error(w, "task in progress", 409)
		return
	}
	select {
	case <-s.ready:
	default:
		s.mu.Unlock()
		http.Error(w, "task not ready", 503)
		return
	}
	s.busy = true
	s.mu.Unlock()
	if method == http.MethodPost {
		s.upload(w, r)
	} else {
		s.download(w, r)
	}
}
func (s *Session) upload(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	stop := context.AfterFunc(s.ctx, func() { r.Body.Close() })
	defer stop()
	limit, _ := s.spec.MaxBytes.Int64()
	// multipart 的边界和小字段不计入文件配额，但整个请求仍有独立上限。
	mediaType, params, parseErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	multipartBody := parseErr == nil && strings.EqualFold(mediaType, "multipart/form-data")
	bodyLimit := limit
	if multipartBody {
		bodyLimit += 64 << 10
	}
	if r.ContentLength > bodyLimit {
		http.Error(w, "task size exceeded", 413)
		s.fail(protocol.Failure(protocol.TransferLimit, "upload too large"))
		return
	}
	if !multipartBody && r.Header.Get("Content-Type") != "" && (parseErr != nil || !strings.EqualFold(mediaType, s.spec.ContentType)) {
		http.Error(w, "content type mismatch", 415)
		s.fail(protocol.Failure(protocol.InvalidArgument, "content type mismatch"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)
	var source io.Reader = r.Body
	if multipartBody {
		reader := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := reader.NextPart()
			if err != nil {
				http.Error(w, "multipart file part missing or invalid", 400)
				s.fail(protocol.Failure(protocol.InvalidArgument, "multipart file part missing or invalid"))
				return
			}
			if part.FileName() != "" || strings.EqualFold(part.FormName(), "file") {
				partType := part.Header.Get("Content-Type")
				if partType != "" && partType != "application/octet-stream" && !strings.EqualFold(partType, s.spec.ContentType) {
					part.Close()
					http.Error(w, "file content type mismatch", 415)
					s.fail(protocol.Failure(protocol.InvalidArgument, "file content type mismatch"))
					return
				}
				source = part
				break
			}
			if _, err = io.Copy(io.Discard, part); err != nil {
				http.Error(w, "invalid multipart field", 400)
				s.fail(err)
				return
			}
			part.Close()
		}
	}
	b, e := s.waitReady()
	if e != nil {
		http.Error(w, "task unavailable", 503)
		return
	}
	buffer := make([]byte, 65536)
	for {
		n, e := source.Read(buffer)
		if n > 0 {
			if sendErr := s.sendBytes(b, buffer[:n], true); sendErr != nil {
				e = sendErr
				status := 502
				var pe *protocol.Error
				if errors.As(e, &pe) && pe.Code == protocol.TransferLimit {
					status = 413
				}
				http.Error(w, "upload failed", status)
				s.fail(e)
				return
			}
		}
		if e != nil {
			if errors.Is(e, io.EOF) {
				break
			}
			http.Error(w, "upload interrupted", 400)
			s.fail(e)
			return
		}
	}
	if e = s.finishBytes(b); e != nil {
		http.Error(w, "upload validation failed", 422)
		s.fail(e)
		return
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-s.commitDone:
	case <-timer.C:
		http.Error(w, "storage confirmation timed out", 504)
		s.fail(protocol.Failure(protocol.UpstreamTimeout, "upload commit timeout"))
		return
	case <-s.ctx.Done():
		s.mu.Lock()
		committed := s.committed
		s.mu.Unlock()
		if !committed {
			http.Error(w, "storage failed", 502)
			return
		}
	case <-r.Context().Done():
		return
	}
	s.mu.Lock()
	size, hash := s.bytes, s.sha
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"committed":true,"bytes":"%d","sha256":"%s"}`, size, hash)
}
func (s *Session) download(w http.ResponseWriter, r *http.Request) {
	length, _ := s.spec.ContentLength.Int64()
	offset, _ := s.spec.RangeOffset.Int64()
	count, _ := s.spec.RangeLength.Int64()
	full := offset == 0 && count == length
	expectedRange := fmt.Sprintf("bytes=%d-%d", offset, offset+count-1)
	if r.Header.Get("Range") != "" && r.Header.Get("Range") != expectedRange || !full && r.Header.Get("Range") != expectedRange {
		http.Error(w, "range not authorized", 416)
		s.fail(protocol.Failure(protocol.InvalidArgument, "unauthorized range"))
		return
	}
	b, e := s.waitReady()
	if e != nil {
		http.Error(w, "task unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", s.spec.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(count, 10))
	w.Header().Set("ETag", `"`+*s.spec.SHA256+`"`)
	w.Header().Set("Cache-Control", "no-store")
	if !full {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+count-1, length))
		w.WriteHeader(206)
	}
	for {
		message, e := b.Read(s.ctx)
		if e != nil {
			s.fail(e)
			return
		}
		if message.Kind == protocol.EOF {
			s.mu.Lock()
			size := s.bytes
			s.sha = hex.EncodeToString(s.digest.Sum(nil))
			hash := s.sha
			valid := size == count && (s.spec.ExpectedSHA256 == nil || *s.spec.ExpectedSHA256 == hash) && (!full || *s.spec.SHA256 == hash)
			if valid {
				s.eof = true
				s.phase = "waiting_commit"
			}
			s.mu.Unlock()
			if !valid {
				s.fail(protocol.Failure(protocol.InvalidArgument, "download length/checksum mismatch"))
				return
			}
			if e = http.NewResponseController(w).Flush(); e != nil {
				s.fail(e)
				return
			}
			s.manager.registry.EOF(s.ref.ID, true)
			s.manager.event("transfer.sent", protocol.TransferSent{TransferID: s.ref.ID, Bytes: protocol.Number(size)})
			return
		}
		s.mu.Lock()
		size := s.bytes
		s.mu.Unlock()
		if message.Kind != protocol.FileBytes || int64(len(message.Payload)) > count-size || !s.manager.budget.Allow(false, len(message.Payload)) {
			s.fail(protocol.Failure(protocol.TransferLimit, "download size/bandwidth exceeded"))
			return
		}
		n, e := w.Write(message.Payload)
		if e != nil || n != len(message.Payload) {
			if e == nil {
				e = io.ErrShortWrite
			}
			s.fail(e)
			return
		}
		s.mu.Lock()
		s.bytes += int64(n)
		s.digest.Write(message.Payload)
		s.phase = "forwarding"
		s.mu.Unlock()
		s.manager.registry.AddBytes(s.ref.ID, false, n)
	}
}

// writeRaw 将受控文件流写入已校验的平台 TCP 连接，完成长度与摘要后等待 A 提交。
func (s *Session) writeRaw(conn net.Conn) {
	b, err := s.waitReady()
	if err != nil {
		return
	}
	for {
		msg, err := b.Read(s.ctx)
		if err != nil {
			s.fail(err)
			return
		}
		if msg.Kind == protocol.EOF {
			if tcp, ok := conn.(*net.TCPConn); ok {
				if err = tcp.CloseWrite(); err != nil {
					s.fail(err)
					return
				}
			}
			s.mu.Lock()
			s.eof = true
			s.sha = hex.EncodeToString(s.digest.Sum(nil))
			s.phase = "waiting_commit"
			s.mu.Unlock()
			s.manager.registry.EOF(s.ref.ID, false)
			return
		}
		if msg.Kind != protocol.FileBytes {
			s.fail(protocol.Failure(protocol.InvalidArgument, "expected raw file bytes"))
			return
		}
		s.mu.Lock()
		limit, _ := s.spec.MaxBytes.Int64()
		over := int64(len(msg.Payload)) > limit-s.bytes
		s.mu.Unlock()
		if over {
			s.fail(protocol.Failure(protocol.TransferLimit, "raw transfer size exceeded"))
			return
		}
		if !s.manager.budget.Allow(false, len(msg.Payload)) {
			s.fail(protocol.Failure(protocol.MediaBackpressure, "raw transfer bandwidth exceeded"))
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		data := msg.Payload
		for len(data) > 0 {
			n, e := conn.Write(data)
			if e != nil {
				s.fail(e)
				return
			}
			if n == 0 {
				s.fail(io.ErrShortWrite)
				return
			}
			data = data[n:]
		}
		s.mu.Lock()
		s.bytes += int64(len(msg.Payload))
		_, _ = s.digest.Write(msg.Payload)
		s.phase = "receiving"
		s.mu.Unlock()
		s.manager.registry.AddBytes(s.ref.ID, false, len(msg.Payload))
	}
}

// QueuedBytes 返回当前连接排队/在途的字节数，关闭后不保留虚假积压。
func (s *Session) QueuedBytes() int64 {
	s.mu.Lock()
	b := s.binary
	s.mu.Unlock()
	if b == nil {
		return 0
	}
	return b.QueuedBytes()
}
