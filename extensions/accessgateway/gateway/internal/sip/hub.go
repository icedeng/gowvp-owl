package sip

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"golang.org/x/time/rate"
)

type Flow struct {
	Route           protocol.Route
	Conn            net.Conn
	SubjectType     string
	SubjectID       string
	BindingRevision protocol.Decimal
	Expires         time.Time
	LastSeen        time.Time
	limiter         *rate.Limiter
	writeMu         sync.Mutex
}
type Hub struct {
	mu               sync.Mutex
	ctx              context.Context
	cancel           context.CancelFunc
	udp              *net.UDPConn
	tcp              net.Listener
	flows            map[string]*Flow
	peers            map[string]string
	limit            int
	packetsPerSecond int
	limiter          *rate.Limiter
	packet           func(protocol.Route, []byte) error
	closed           func(string, string)
	wg               sync.WaitGroup
}

func New(parent context.Context, limit, packetsPerSecond int, packet func(protocol.Route, []byte) error, closed func(string, string)) *Hub {
	ctx, cancel := context.WithCancel(parent)
	return &Hub{ctx: ctx, cancel: cancel, limit: limit, packetsPerSecond: packetsPerSecond, limiter: rate.NewLimiter(rate.Limit(packetsPerSecond*8), packetsPerSecond*8), flows: map[string]*Flow{}, peers: map[string]string{}, packet: packet, closed: closed}
}
func (h *Hub) Start(address string) error {
	udpAddr, e := net.ResolveUDPAddr("udp", address)
	if e != nil {
		return e
	}
	h.udp, e = net.ListenUDP("udp", udpAddr)
	if e != nil {
		return e
	}
	h.tcp, e = net.Listen("tcp", address)
	if e != nil {
		h.udp.Close()
		return e
	}
	h.wg.Add(3)
	go h.readUDP()
	go h.acceptTCP()
	go h.sweep()
	return nil
}
func (h *Hub) Close() error {
	h.cancel()
	if h.udp != nil {
		h.udp.Close()
	}
	if h.tcp != nil {
		h.tcp.Close()
	}
	h.Invalidate()
	h.wg.Wait()
	return nil
}
func (h *Hub) Count() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.flows) }
func endpoint(addr net.Addr) protocol.Endpoint {
	host, port, _ := net.SplitHostPort(addr.String())
	n := 0
	fmt.Sscan(port, &n)
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	} else {
		host = ip.String()
	}
	return protocol.Endpoint{IP: host, Port: n}
}
func (h *Hub) newFlow(peer, local net.Addr, transport string, conn net.Conn) (*Flow, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx.Err() != nil {
		return nil, h.ctx.Err()
	}
	if len(h.flows) >= h.limit {
		return nil, protocol.Failure(protocol.Exhausted, "SIP flow limit")
	}
	f := &Flow{Route: protocol.Route{FlowID: uuid.NewString(), Transport: transport, Peer: endpoint(peer), Listener: endpoint(local)}, Conn: conn, LastSeen: time.Now(), Expires: time.Now().Add(60 * time.Second), limiter: rate.NewLimiter(rate.Limit(h.packetsPerSecond), h.packetsPerSecond)}
	h.flows[f.Route.FlowID] = f
	if transport == "udp" {
		h.peers[peer.String()] = f.Route.FlowID
	}
	return f, nil
}
func (h *Hub) readUDP() {
	defer h.wg.Done()
	buf := make([]byte, 65536)
	for {
		n, peer, e := h.udp.ReadFromUDP(buf)
		if e != nil {
			return
		}
		if !h.limiter.Allow() || ValidateDatagram(buf[:n]) != nil {
			continue
		}
		h.mu.Lock()
		f := h.flows[h.peers[peer.String()]]
		h.mu.Unlock()
		if f == nil {
			f, e = h.newFlow(peer, h.udp.LocalAddr(), "udp", nil)
			if e != nil {
				continue
			}
		}
		h.deliver(f, buf[:n])
	}
}
func (h *Hub) acceptTCP() {
	defer h.wg.Done()
	for {
		conn, e := h.tcp.Accept()
		if e != nil {
			return
		}
		if !h.limiter.Allow() {
			conn.Close()
			continue
		}
		f, e := h.newFlow(conn.RemoteAddr(), conn.LocalAddr(), "tcp", conn)
		if e != nil {
			conn.Close()
			continue
		}
		h.wg.Add(1)
		go h.readTCP(f)
	}
}
func (h *Hub) readTCP(f *Flow) {
	defer h.wg.Done()
	defer h.CloseFlow(f.Route.FlowID, "TCP closed")
	reader := bufio.NewReaderSize(f.Conn, 8193)
	for {
		_ = f.Conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		data, e := ReadMessage(reader)
		if e != nil {
			return
		}
		h.deliver(f, data)
	}
}
func (h *Hub) deliver(f *Flow, data []byte) {
	h.mu.Lock()
	if h.flows[f.Route.FlowID] != f || !f.limiter.Allow() {
		h.mu.Unlock()
		return
	}
	f.LastSeen = time.Now()
	route := f.Route
	h.mu.Unlock()
	if e := h.packet(route, data); e != nil {
		if response := unavailable(data); response != nil {
			_ = h.Send(route, response)
		}
	}
}
func (h *Hub) Send(route protocol.Route, data []byte) error {
	h.mu.Lock()
	f := h.flows[route.FlowID]
	if f == nil || f.Route.Transport != route.Transport || f.Route.Peer != route.Peer || time.Now().After(f.Expires) {
		h.mu.Unlock()
		return protocol.Failure(protocol.NotFound, "SIP flow unavailable")
	}
	h.mu.Unlock()
	if len(data) == 0 || len(data) > MaxMessageBytes {
		return protocol.Failure(protocol.InvalidArgument, "invalid SIP size")
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	if f.Conn == nil {
		if e := ValidateDatagram(data); e != nil {
			return e
		}
		peer := net.UDPAddrFromAddrPort(netipEndpoint(f.Route.Peer))
		_, e := h.udp.WriteToUDP(data, peer)
		return e
	}
	_ = f.Conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	for len(data) > 0 {
		n, e := f.Conn.Write(data)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
func (h *Hub) Bind(b protocol.Binding, action string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := h.flows[b.FlowID]
	if f == nil {
		return protocol.Failure(protocol.NotFound, "flow not found")
	}
	current, _ := f.BindingRevision.Int64()
	next, e := b.BindingRevision.Int64()
	if e != nil || next < current || f.SubjectID != "" && (f.SubjectID != b.SubjectID || f.SubjectType != b.SubjectType) {
		return protocol.Failure(protocol.BindingConflict, "flow binding mismatch")
	}
	if action == "refresh" && (f.SubjectID == "" || next != current) {
		return protocol.Failure(protocol.BindingConflict, "refresh requires existing binding")
	}
	if action == "unbind" {
		if next != current || f.SubjectID != b.SubjectID {
			return protocol.Failure(protocol.BindingConflict, "unbind revision mismatch")
		}
		f.SubjectID = ""
		f.SubjectType = ""
		f.Expires = time.Now()
		return nil
	}
	f.SubjectID = b.SubjectID
	f.SubjectType = b.SubjectType
	f.BindingRevision = b.BindingRevision
	revision := b.BindingRevision
	f.Route.BindingRevision = &revision
	f.Expires = time.Now().Add(time.Duration(b.ExpiresInMS) * time.Millisecond)
	return nil
}
func (h *Hub) Authorized(flowID, subjectType, subjectID string) (protocol.Route, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := h.flows[flowID]
	if f == nil || f.SubjectID != subjectID || f.SubjectType != subjectType || time.Now().After(f.Expires) {
		return protocol.Route{}, protocol.Failure(protocol.Forbidden, "authenticated SIP binding required")
	}
	return f.Route, nil
}

// OpenTCP 目标只能由控制器从已认证绑定派生，拒绝任意地址代理。
func (h *Hub) OpenTCP(ctx context.Context, target protocol.Endpoint, subjectType, subjectID string, revision protocol.Decimal) (protocol.Route, error) {
	h.mu.Lock()
	allowed := false
	for _, f := range h.flows {
		if f.SubjectID == subjectID && f.SubjectType == subjectType && f.BindingRevision == revision && f.Route.Peer.IP == target.IP && f.Route.Peer.Port == target.Port && time.Now().Before(f.Expires) {
			allowed = true
			break
		}
	}
	h.mu.Unlock()
	if !allowed {
		return protocol.Route{}, protocol.Failure(protocol.Forbidden, "target is not an authenticated binding")
	}
	conn, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", target.String())
	if e != nil {
		return protocol.Route{}, e
	}
	f, e := h.newFlow(conn.RemoteAddr(), conn.LocalAddr(), "tcp", conn)
	if e != nil {
		conn.Close()
		return protocol.Route{}, e
	}
	if e = h.Bind(protocol.Binding{SubjectType: subjectType, SubjectID: subjectID, FlowID: f.Route.FlowID, BindingRevision: revision, ExpiresInMS: 60000}, "bind"); e != nil {
		h.CloseFlow(f.Route.FlowID, "bind failed")
		return protocol.Route{}, e
	}
	h.wg.Add(1)
	go h.readTCP(f)
	return f.Route, nil
}
func (h *Hub) CloseFlow(id, reason string) {
	h.mu.Lock()
	f := h.flows[id]
	if f != nil {
		delete(h.flows, id)
		if f.Route.Transport == "udp" && h.peers[f.Route.Peer.String()] == id {
			delete(h.peers, f.Route.Peer.String())
		}
	}
	h.mu.Unlock()
	if f != nil {
		if f.Conn != nil {
			f.Conn.Close()
		}
		if h.closed != nil {
			h.closed(id, reason)
		}
	}
}
func (h *Hub) Invalidate() {
	h.mu.Lock()
	ids := make([]string, 0, len(h.flows))
	for id := range h.flows {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	for _, id := range ids {
		h.CloseFlow(id, "control epoch ended")
	}
}
func (h *Hub) sweep() {
	defer h.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case now := <-ticker.C:
			h.mu.Lock()
			expired := []string{}
			for id, f := range h.flows {
				if now.After(f.Expires) {
					expired = append(expired, id)
				}
			}
			h.mu.Unlock()
			for _, id := range expired {
				h.CloseFlow(id, "flow lease expired")
			}
		}
	}
}

func netipEndpoint(e protocol.Endpoint) netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr(e.IP), uint16(e.Port))
}

func (h *Hub) SubjectRoute(subjectType, subjectID string) (protocol.Route, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var chosen *Flow
	for _, f := range h.flows {
		if f.SubjectType == subjectType && f.SubjectID == subjectID && time.Now().Before(f.Expires) && (chosen == nil || f.LastSeen.After(chosen.LastSeen)) {
			chosen = f
		}
	}
	if chosen == nil {
		return protocol.Route{}, protocol.Failure(protocol.Forbidden, "authenticated subject binding required")
	}
	return chosen.Route, nil
}

// OpenPlatform 只接受 A 已保存平台配置产生的显式授权，且目标必须是公网字面 IP。
// 设备升级继续走 OpenTCP 的现有绑定校验，不共享此入口。
func (h *Hub) OpenPlatform(ctx context.Context, request protocol.FlowOpen) (protocol.Route, error) {
	if request.SubjectType != "platform" || !request.ConfiguredPlatform || !protocol.ValidPublicIP(request.Target.IP) {
		return protocol.Route{}, protocol.Failure(protocol.Forbidden, "configured public platform required")
	}
	h.mu.Lock()
	for _, f := range h.flows {
		if f.SubjectType == "platform" && f.SubjectID == request.SubjectID && f.BindingRevision == request.BindingRevision && f.Route.Transport == request.Transport && f.Route.Peer == request.Target && time.Now().Before(f.Expires) {
			route := f.Route
			h.mu.Unlock()
			return route, nil
		}
	}
	h.mu.Unlock()
	var f *Flow
	var err error
	if request.Transport == "udp" {
		h.mu.Lock()
		f = h.flows[h.peers[request.Target.String()]]
		h.mu.Unlock()
		if f == nil {
			f, err = h.newFlow(net.UDPAddrFromAddrPort(netipEndpoint(request.Target)), h.udp.LocalAddr(), "udp", nil)
		}
	} else {
		var conn net.Conn
		conn, err = (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", request.Target.String())
		if err == nil {
			f, err = h.newFlow(conn.RemoteAddr(), conn.LocalAddr(), "tcp", conn)
			if err != nil {
				conn.Close()
			}
		}
	}
	if err != nil {
		return protocol.Route{}, err
	}
	if err = h.Bind(protocol.Binding{SubjectType: "platform", SubjectID: request.SubjectID, FlowID: f.Route.FlowID, BindingRevision: request.BindingRevision, ExpiresInMS: 90000}, "bind"); err != nil {
		return protocol.Route{}, err
	}
	if request.Transport == "tcp" {
		h.wg.Add(1)
		go h.readTCP(f)
	}
	return f.Route, nil
}
