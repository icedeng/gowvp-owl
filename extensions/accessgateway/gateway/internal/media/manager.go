package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/lease"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/sip"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
	"golang.org/x/time/rate"
)

type Budget struct {
	mu            sync.Mutex
	toA, toDevice *rate.Limiter
}

func NewBudget(a, b int64) *Budget { v := &Budget{}; v.Configure(a, b); return v }
func (b *Budget) Configure(a, c int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.toA = rate.NewLimiter(rate.Limit(a)/8, 1<<20)
	b.toDevice = rate.NewLimiter(rate.Limit(c)/8, 1<<20)
}
func (b *Budget) Allow(toA bool, n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if toA {
		return b.toA.AllowN(time.Now(), n)
	}
	return b.toDevice.AllowN(time.Now(), n)
}

type Manager struct {
	Counters *transport.Counters
	mu       sync.Mutex
	ctx      context.Context
	registry *lease.Registry
	hub      *sip.Hub
	ports    *Ports
	budget   *Budget
	config   protocol.EffectiveConfig
	revision protocol.Decimal
	event    func(string, any)
	draining bool
}
type Session struct {
	mu         sync.Mutex
	manager    *Manager
	ctx        context.Context
	cancel     context.CancelFunc
	spec       protocol.MediaSpec
	lease      protocol.Lease
	ref        protocol.ResourceRef
	owner      lease.Owner
	ports      *allocation
	binary     *transport.Binary
	attached   chan struct{}
	remote     net.Conn
	udpPeer    *net.UDPAddr
	rtcpPeer   *net.UDPAddr
	allowed    map[netip.Addr]bool
	wg         sync.WaitGroup
	failed     sync.Once
	connected  bool
	firstMedia chan struct{}
	opened     time.Time
	firstAt    *time.Time
	firstOnce  sync.Once
	config     protocol.EffectiveConfig
}

func NewManager(ctx context.Context, registry *lease.Registry, hub *sip.Hub, ports *Ports, budget *Budget, c protocol.EffectiveConfig, revision protocol.Decimal, event func(string, any)) *Manager {
	return &Manager{ctx: ctx, registry: registry, hub: hub, ports: ports, budget: budget, config: c, revision: revision, event: event}
}
func (m *Manager) Configure(c protocol.EffectiveConfig, revision protocol.Decimal) {
	m.mu.Lock()
	m.config = c
	m.revision = revision
	m.mu.Unlock()
	a, _ := c.MaxToABPS.Int64()
	b, _ := c.MaxToDeviceBPS.Int64()
	m.budget.Configure(a, b)
}
func (m *Manager) Drain(value bool) { m.mu.Lock(); m.draining = value; m.mu.Unlock() }
func (m *Manager) Open(owner lease.Owner, request protocol.MediaOpen) (protocol.Lease, error) {
	if err := protocol.ValidateMedia(request.Spec); err != nil {
		return protocol.Lease{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.draining {
		return protocol.Lease{}, protocol.Failure(protocol.GatewayDraining, "gateway is draining")
	}
	if request.Spec.ConfigRevision != m.revision {
		return protocol.Lease{}, protocol.Failure(protocol.ConfigConflict, "media config revision mismatch")
	}
	if m.registry.Count("media") >= m.config.MaxMediaSessions {
		return protocol.Lease{}, protocol.Failure(protocol.Exhausted, "media session quota")
	}
	route, e := m.hub.Authorized(request.Spec.FlowID, request.Spec.SubjectType, request.Spec.SubjectID)
	if e != nil {
		return protocol.Lease{}, e
	}
	allowed, e := AllowedSources(request.Spec.SourceIPs, route.Peer.IP, m.config.SourceCIDRs)
	if e != nil {
		return protocol.Lease{}, e
	}
	ports, e := m.ports.Allocate(m.config, request.Spec.WireMode, request.Spec.RTCP)
	if e != nil {
		return protocol.Lease{}, e
	}
	ctx, cancel := context.WithCancel(m.ctx)
	s := &Session{opened: time.Now(), manager: m, ctx: ctx, cancel: cancel, config: m.config, spec: request.Spec, owner: owner, ports: ports, allowed: allowed, attached: make(chan struct{}), firstMedia: make(chan struct{})}
	s.lease = protocol.Lease{State: "allocated", PublicRTP: &protocol.Endpoint{IP: m.config.PublicIP, Port: ports.port}, LeaseTTLMS: m.config.LeaseTTLMS, AttachTTLMS: 30000, ConfigRevision: m.revision, WireMode: request.Spec.WireMode, Direction: request.Spec.Direction}
	if ports.rtcpPort != 0 {
		s.lease.PublicRTCP = &protocol.Endpoint{IP: m.config.PublicIP, Port: ports.rtcpPort}
	}
	rev := protocol.Decimal("0")
	if route.BindingRevision != nil {
		rev = *route.BindingRevision
	}
	ref, e := m.registry.Add(lease.Resource{Ref: protocol.ResourceRef{ResourceType: "media"}, Owner: owner, OperationID: request.OperationID, SubjectType: request.Spec.SubjectType, SubjectID: request.Spec.SubjectID, BindingRevision: rev, ConfigRevision: m.revision, TTL: time.Duration(m.config.LeaseTTLMS) * time.Millisecond, Close: s.close, Value: s})
	if e != nil {
		cancel()
		_ = m.ports.Release(ports, time.Duration(m.config.LeaseTTLMS)*time.Millisecond)
		return protocol.Lease{}, e
	}
	s.ref = ref
	s.lease.SessionID = ref.ID
	if ports.udp != nil {
		s.wg.Add(1)
		go s.readUDP(ports.udp, false)
		if ports.rtcp != nil {
			s.wg.Add(1)
			go s.readUDP(ports.rtcp, true)
		}
	} else if request.Spec.WireMode == "tcp_passive" {
		s.wg.Add(1)
		go s.acceptTCP()
	}
	return s.lease, nil
}
func AllowedSources(requested []string, peer string, cidrs []string) (map[netip.Addr]bool, error) {
	if len(requested) == 0 {
		requested = []string{peer}
	}
	allowed := map[netip.Addr]bool{}
	for _, value := range requested {
		ip, e := netip.ParseAddr(value)
		if e != nil || ip.IsUnspecified() || ip.IsMulticast() || ip.Zone() != "" {
			return nil, protocol.Failure(protocol.Forbidden, "invalid media source")
		}
		ip = ip.Unmap()
		valid := len(cidrs) == 0 && ip.String() == peer
		for _, raw := range cidrs {
			p, e := netip.ParsePrefix(raw)
			if e == nil && p.Contains(ip) {
				valid = true
			}
		}
		if !valid {
			return nil, protocol.Failure(protocol.Forbidden, "media source outside configured policy")
		}
		allowed[ip] = true
	}
	return allowed, nil
}
func (m *Manager) Attach(owner lease.Owner, id string, value any, conn *websocket.Conn) error {
	s, ok := value.(*Session)
	if !ok {
		return protocol.Failure(protocol.Forbidden, "wrong media resource")
	}
	s.mu.Lock()
	if s.ctx.Err() != nil || s.binary != nil {
		s.mu.Unlock()
		return protocol.Failure(protocol.NotFound, "media session unavailable")
	}
	b := transport.NewBinary(s.ctx, conn, s.spec.WireMode, s.spec.Direction, false, false, m.Counters)
	s.binary = b
	if e := m.registry.Attached(id); e != nil {
		s.mu.Unlock()
		b.Close()
		return e
	}
	close(s.attached)
	s.wg.Add(1)
	go s.writeDevice()
	if s.spec.Direction != "a_to_device" {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			timer := time.NewTimer(time.Duration(s.config.MediaFirstPacketTimeoutMS) * time.Millisecond)
			defer timer.Stop()
			select {
			case <-s.firstMedia:
			case <-s.ctx.Done():
			case <-timer.C:
				s.fail(protocol.Failure(protocol.UpstreamTimeout, "device first media timeout"))
			}
		}()
	}
	s.mu.Unlock()
	m.event("media.ready", protocol.MediaReady{SessionID: id})
	return nil
}
func (m *Manager) Connect(ctx context.Context, owner lease.Owner, request protocol.MediaConnect) error {
	value, e := m.registry.Get(owner, request.SessionID)
	if e != nil {
		return e
	}
	s, ok := value.(*Session)
	if !ok {
		return protocol.Failure(protocol.Forbidden, "wrong resource type")
	}
	return s.connect(ctx, request.Target, request.RTCPTarget)
}
func (s *Session) connect(ctx context.Context, target protocol.Endpoint, rtcpTarget *protocol.Endpoint) error {
	if e := target.Validate(); e != nil {
		return e
	}
	ip := netip.MustParseAddr(target.IP).Unmap()
	if !s.allowed[ip] {
		return protocol.Failure(protocol.Forbidden, "active media target outside source policy")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spec.WireMode == "udp" {
		var nextRTCP *net.UDPAddr
		if s.spec.Direction == "device_to_a" || s.ctx.Err() != nil {
			return protocol.Failure(protocol.InvalidArgument, "UDP target configuration requires a sending lease")
		}
		if s.spec.RTCP == "udp_separate" {
			if rtcpTarget == nil {
				return protocol.Failure(protocol.InvalidArgument, "explicit RTCP target required")
			}
			if err := rtcpTarget.Validate(); err != nil {
				return err
			}
			if !s.allowed[netip.MustParseAddr(rtcpTarget.IP).Unmap()] {
				return protocol.Failure(protocol.Forbidden, "RTCP target outside source policy")
			}
			nextRTCP = net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr(rtcpTarget.IP), uint16(rtcpTarget.Port)))
		} else if rtcpTarget != nil {
			return protocol.Failure(protocol.InvalidArgument, "RTCP not negotiated")
		}
		if s.connected {
			if s.udpPeer != nil && s.udpPeer.String() == target.String() && ((s.rtcpPeer == nil && nextRTCP == nil) || (s.rtcpPeer != nil && nextRTCP != nil && s.rtcpPeer.String() == nextRTCP.String())) {
				return nil
			}
			return protocol.Failure(protocol.BindingConflict, "UDP destination already fixed")
		}
		s.rtcpPeer = nextRTCP
		s.udpPeer = net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, uint16(target.Port)))
		s.connected = true
		return nil
	}
	if rtcpTarget != nil {
		return protocol.Failure(protocol.InvalidArgument, "RTCP target is only valid for UDP")
	}
	if s.spec.WireMode != "tcp_active" || s.connected || s.ctx.Err() != nil {
		return protocol.Failure(protocol.InvalidArgument, "media.connect requires an unconnected active TCP lease")
	}
	// 先关闭仅用于预留源端口的监听器，再由内核绑定相同端口发起连接；失败不换端口。
	if s.ports.listener != nil {
		s.ports.listener.Close()
	}
	dialCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.MediaConnectTimeoutMS)*time.Millisecond)
	defer cancel()
	stopCancel := context.AfterFunc(s.ctx, cancel)
	defer stopCancel()
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(s.manager.ports.bindIP), Port: s.ports.port}}
	conn, e := d.DialContext(dialCtx, "tcp", target.String())
	if e != nil {
		return protocol.Failure(protocol.DeviceUnreachable, "device active TCP connection failed")
	}
	s.remote = conn
	s.connected = true
	s.wg.Add(1)
	go s.readTCP(conn)
	return nil
}
func (s *Session) waitBinary() (*transport.Binary, error) {
	select {
	case <-s.attached:
		s.mu.Lock()
		b := s.binary
		s.mu.Unlock()
		if b == nil {
			return nil, context.Canceled
		}
		return b, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}
func (s *Session) readUDP(conn *net.UDPConn, rtcp bool) {
	defer s.wg.Done()
	b, e := s.waitBinary()
	if e != nil {
		return
	}
	buffer := make([]byte, 65536)
	for {
		n, peer, e := conn.ReadFromUDP(buffer)
		if e != nil {
			if s.ctx.Err() == nil {
				s.fail(e)
			}
			return
		}
		ip := peer.AddrPort().Addr().Unmap()
		if !s.allowed[ip] {
			continue
		}
		packet := buffer[:n]
		if rtcp {
			if s.spec.RTCP != "udp_separate" || protocol.ValidateRTCP(packet) != nil {
				continue
			}
		} else {
			if s.spec.Direction == "a_to_device" || protocol.ValidateRTP(packet, s.spec.SSRC) != nil {
				continue
			}
		}
		s.mu.Lock()
		current := s.udpPeer
		if rtcp {
			current = s.rtcpPeer
		}
		if current == nil {
			copyPeer := *peer
			if rtcp {
				s.rtcpPeer = &copyPeer
			} else {
				s.udpPeer = &copyPeer
			}
		} else if current.AddrPort() != peer.AddrPort() {
			s.mu.Unlock()
			continue
		}
		s.mu.Unlock()
		if !s.manager.budget.Allow(true, n) {
			s.fail(protocol.Failure(protocol.MediaBackpressure, "gateway bandwidth budget exceeded"))
			return
		}
		kind := protocol.UDPRTP
		if rtcp {
			kind = protocol.UDPRTCP
		}
		if !rtcp {
			s.markFirstPacket()
		}
		if e = b.Send(s.ctx, kind, packet); e != nil {
			s.fail(e)
			return
		}
		s.manager.registry.AddBytes(s.ref.ID, true, n)

		s.manager.registry.Receiving(s.ref.ID)
	}
}
func (s *Session) acceptTCP() {
	defer s.wg.Done()
	for {
		conn, e := s.ports.listener.Accept()
		if e != nil {
			return
		}
		stopCancel := context.AfterFunc(s.ctx, func() { conn.Close() })
		host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		ip, e := netip.ParseAddr(host)
		if e != nil || !s.allowed[ip.Unmap()] {
			stopCancel()
			conn.Close()
			continue
		}
		if s.spec.Direction != "device_to_a" {
			s.mu.Lock()
			if s.ctx.Err() != nil {
				s.mu.Unlock()
				stopCancel()
				conn.Close()
				return
			}
			s.remote = conn
			s.connected = true
			s.mu.Unlock()
			s.ports.listener.Close()
			s.readTCPWithParser(conn, s.inboundFramer())
			stopCancel()
			return
		}
		// 同一监听器逐个验证首帧，限制半包占用时间；错误来源不会永久占住租约。
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		parser := s.inboundFramer()
		buffer := make([]byte, 65536)
		first := []byte{}
		for len(first) == 0 {
			n, e := conn.Read(buffer)
			if e != nil {
				break
			}
			first, e = parser.Feed(buffer[:n])
			if e != nil {
				break
			}
		}
		if len(first) == 0 {
			stopCancel()
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
		s.connected = true
		s.mu.Unlock()
		s.ports.listener.Close()
		b, e := s.waitBinary()
		if e != nil {
			conn.Close()
			return
		}
		if !s.manager.budget.Allow(true, len(first)) {
			s.fail(protocol.Failure(protocol.MediaBackpressure, "gateway bandwidth budget exceeded"))
			return
		}
		s.markFirstPacket()
		for data := first; len(data) > 0; {
			size := min(len(data), 65536)
			if e = b.Send(s.ctx, protocol.TCPBytes, data[:size]); e != nil {
				s.fail(e)
				return
			}
			data = data[size:]
		}
		s.manager.registry.AddBytes(s.ref.ID, true, len(first))

		s.manager.registry.Receiving(s.ref.ID)
		s.readTCPWithParser(conn, parser)
		stopCancel()
		return
	}
}
func (s *Session) inboundFramer() *protocol.RTPFramer {
	return &protocol.RTPFramer{SSRC: s.spec.SSRC, AllowRTP: s.spec.Direction != "a_to_device", AllowRTCP: s.spec.RTCP == "tcp_mux"}
}
func (s *Session) readTCP(conn net.Conn) {
	defer s.wg.Done()
	s.readTCPWithParser(conn, s.inboundFramer())
}
func (s *Session) readTCPWithParser(conn net.Conn, parser *protocol.RTPFramer) {
	b, e := s.waitBinary()
	if e != nil {
		return
	}
	buffer := make([]byte, 65536)
	for {
		_ = conn.SetReadDeadline(time.Time{})
		n, e := conn.Read(buffer)
		if n > 0 {
			data, parseErr := parser.Feed(buffer[:n])
			if parseErr != nil {
				s.fail(parseErr)
				return
			}
			if len(data) > 0 {
				s.markFirstPacket()
				for len(data) > 0 {
					size := min(len(data), 65536)
					if !s.manager.budget.Allow(true, size) {
						s.fail(protocol.Failure(protocol.MediaBackpressure, "gateway bandwidth budget exceeded"))
						return
					}
					if sendErr := b.Send(s.ctx, protocol.TCPBytes, data[:size]); sendErr != nil {
						s.fail(sendErr)
						return
					}
					s.manager.registry.AddBytes(s.ref.ID, true, size)
					data = data[size:]
				}

				s.manager.registry.Receiving(s.ref.ID)
			}
		}
		if e != nil {
			if errors.Is(e, io.EOF) && parser.Finish() == nil {
				if err := b.Send(s.ctx, protocol.EOF, nil); err != nil {
					s.fail(err)
				}
				s.manager.registry.EOF(s.ref.ID, true)
				return
			}
			if s.ctx.Err() == nil {
				s.fail(e)
			}
			return
		}
	}
}
func (s *Session) writeDevice() {
	defer s.wg.Done()
	b, e := s.waitBinary()
	if e != nil {
		return
	}
	parser := &protocol.RTPFramer{SSRC: s.spec.SendSSRC, AllowRTP: s.spec.Direction != "device_to_a", AllowRTCP: s.spec.RTCP == "tcp_mux"}
	for {
		m, e := b.Read(s.ctx)
		if e != nil {
			if s.ctx.Err() == nil {
				s.fail(e)
			}
			return
		}
		if m.Kind == protocol.EOF {
			s.mu.Lock()
			conn := s.remote
			s.mu.Unlock()
			if e := parser.Finish(); e != nil {
				s.fail(e)
				return
			}
			if tcp, ok := conn.(*net.TCPConn); ok {
				if e := tcp.CloseWrite(); e != nil {
					s.fail(e)
					return
				}
			}
			s.manager.registry.EOF(s.ref.ID, false)
			return
		}
		if !s.manager.budget.Allow(false, len(m.Payload)) {
			s.fail(protocol.Failure(protocol.MediaBackpressure, "gateway outbound bandwidth budget exceeded"))
			return
		}
		if s.spec.WireMode == "udp" {
			s.mu.Lock()
			peer, conn := s.udpPeer, s.ports.udp
			if m.Kind == protocol.UDPRTCP {
				peer, conn = s.rtcpPeer, s.ports.rtcp
			}
			s.mu.Unlock()
			if peer == nil || conn == nil {
				s.fail(protocol.Failure(protocol.DeviceUnreachable, "no validated UDP destination"))
				return
			}
			if m.Kind == protocol.UDPRTP {
				if e := protocol.ValidateRTP(m.Payload, s.spec.SendSSRC); e != nil {
					s.fail(e)
					return
				}
			} else if s.spec.RTCP != "udp_separate" {
				s.fail(protocol.Failure(protocol.InvalidArgument, "RTCP not negotiated"))
				return
			}
			_, e = conn.WriteToUDP(m.Payload, peer)
		} else {
			var data []byte
			data, e = parser.Feed(m.Payload)
			if e == nil && len(data) > 0 {
				var conn net.Conn
				deadline := time.NewTimer(10 * time.Second)
				for conn == nil {
					s.mu.Lock()
					conn = s.remote
					s.mu.Unlock()
					if conn != nil {
						break
					}
					select {
					case <-s.ctx.Done():
						deadline.Stop()
						return
					case <-deadline.C:
						s.fail(protocol.Failure(protocol.DeviceUnreachable, "device TCP connection unavailable"))
						return
					case <-time.After(10 * time.Millisecond):
					}
				}
				deadline.Stop()
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				for len(data) > 0 {
					var n int
					n, e = conn.Write(data)
					if e != nil {
						break
					}
					if n == 0 {
						e = io.ErrShortWrite
						break
					}
					data = data[n:]
				}
			}
		}
		if e != nil {
			s.fail(e)
			return
		}
		s.manager.registry.AddBytes(s.ref.ID, false, len(m.Payload))
	}
}
func (s *Session) fail(err error) {
	s.failed.Do(func() {
		go func() {
			s.manager.event("media.failed", protocol.MediaFailed{SessionID: s.ref.ID, Error: *protocol.Failure(protocol.IOError, err.Error())})
			_, _ = s.manager.registry.CloseRef(s.owner.PlatformID, s.ref)
		}()
	})
}
func (s *Session) close() error {
	s.cancel()
	s.ports.closeSockets()
	s.mu.Lock()
	conn, b := s.remote, s.binary
	s.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
	if b != nil {
		b.Close()
	}
	s.wg.Wait()
	if e := s.manager.ports.Release(s.ports, time.Duration(s.lease.LeaseTTLMS)*time.Millisecond); e != nil {
		return fmt.Errorf("close media ports: %w", e)
	}
	return nil
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

func (m *Manager) Inspect(owner lease.Owner, id string) (protocol.MediaInspection, error) {
	v, err := m.registry.Get(owner, id)
	if err != nil {
		return protocol.MediaInspection{}, err
	}
	s, ok := v.(*Session)
	if !ok {
		return protocol.MediaInspection{}, fmt.Errorf("not a media session")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := protocol.MediaInspection{}
	if s.firstAt != nil {
		n := s.firstAt.Sub(s.opened).Milliseconds()
		out.FirstPacketMS = &n
	}
	return out, nil
}

func (s *Session) markFirstPacket() {
	s.firstOnce.Do(func() { now := time.Now(); s.mu.Lock(); s.firstAt = &now; s.mu.Unlock(); close(s.firstMedia) })
}
