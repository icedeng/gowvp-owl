package owl

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gowvp/owl/internal/core/sms"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/pkg/gbs"
)

// fixedUDP 只接受原媒体节点返回的实际源端点。连接等待配置完成，早到包留在内核有界缓冲区。
type fixedUDP struct {
	*net.UDPConn
	mu        sync.RWMutex
	peer      *net.UDPAddr
	ready     chan struct{}
	once      sync.Once
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *fixedUDP) set(peer *net.UDPAddr) {
	c.mu.Lock()
	c.peer = peer
	c.mu.Unlock()
	c.once.Do(func() { close(c.ready) })
}
func (c *fixedUDP) wait() error {
	select {
	case <-c.ready:
		return nil
	case <-c.closed:
		return net.ErrClosed
	}
}
func (c *fixedUDP) Read(p []byte) (int, error) {
	if err := c.wait(); err != nil {
		return 0, err
	}
	for {
		n, peer, err := c.ReadFromUDP(p)
		if err != nil {
			return n, err
		}
		c.mu.RLock()
		allowed := c.peer != nil && peer.String() == c.peer.String()
		c.mu.RUnlock()
		if allowed {
			return n, nil
		}
	}
}
func (c *fixedUDP) Write(p []byte) (int, error) {
	if err := c.wait(); err != nil {
		return 0, err
	}
	c.mu.RLock()
	peer := c.peer
	c.mu.RUnlock()
	return c.WriteToUDP(p, peer)
}
func (c *fixedUDP) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.UDPConn.Close()
}
func fixed(c *net.UDPConn) *fixedUDP {
	return &fixedUDP{UDPConn: c, ready: make(chan struct{}), closed: make(chan struct{})}
}
func (b *mediaBridge) prepareLocalSender(ctx context.Context, node string, mode int8) error {
	// 路由选择 A 到真实节点的本机地址，避免把 ZLM 的 SDP 公网地址用作内部收发地址。
	route, err := (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort(node, "9"))
	if err != nil {
		return err
	}
	ip := route.LocalAddr().(*net.UDPAddr).IP
	route.Close()
	b.localSendIP = ip.String()
	if mode != 0 {
		b.localListener, err = net.Listen("tcp", net.JoinHostPort(b.localSendIP, "0"))
		return err
	}
	for attempt := 0; attempt < 64; attempt++ {
		rtp, e := net.ListenUDP("udp", &net.UDPAddr{IP: ip})
		if e != nil {
			return e
		}
		port := rtp.LocalAddr().(*net.UDPAddr).Port
		if port == 65535 {
			rtp.Close()
			continue
		}
		rtcp, e := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: port + 1})
		if e != nil {
			rtp.Close()
			continue
		}
		b.rtp = fixed(rtp)
		b.rtcp = fixed(rtcp)
		return nil
	}
	return fmt.Errorf("无法创建本地 RTP/RTCP 接收端点")
}
func (b *mediaBridge) LocalSendEndpoint() (string, int) {
	if b.localListener != nil {
		return b.localSendIP, b.localListener.Addr().(*net.TCPAddr).Port
	}
	if b.rtp != nil {
		return b.localSendIP, b.rtp.LocalAddr().(*net.UDPAddr).Port
	}
	return "", 0
}
func (b *mediaBridge) SetLocalSender(ctx context.Context, rtp, rtcp int) error {
	if b.row.WireMode != "udp" {
		return nil
	}
	if rtp < 1 || rtcp < 1 || rtp > 65535 || rtcp > 65535 {
		return fmt.Errorf("媒体节点未确认实际发送 RTP/RTCP 端口，请更新 ZLM 补丁")
	}
	// PrepareReceive 已保存真实节点端点，发送模式直接从节点快照读取。
	server, err := b.mediaServer()
	if err != nil {
		return err
	}
	b.rtp.(*fixedUDP).set(&net.UDPAddr{IP: net.ParseIP(server.IP), Port: rtp})
	b.rtcp.(*fixedUDP).set(&net.UDPAddr{IP: net.ParseIP(server.IP), Port: rtcp})
	return ctx.Err()
}
func (b *mediaBridge) PublicRTCP() (string, int) {
	if b.lease.PublicRTCP == nil {
		return "", 0
	}
	return b.lease.PublicRTCP.IP, b.lease.PublicRTCP.Port
}
func (b *mediaBridge) ConnectDestination(ctx context.Context, ip string, port int, rtcpIP string, rtcpPort int) error {
	var rtcp *protocol.Endpoint
	if b.row.WireMode == "udp" {
		rtcp = &protocol.Endpoint{IP: rtcpIP, Port: rtcpPort}
	}
	_, err := b.host.host.manager().CallExact(ctx, b.row.GatewayID, b.ref, "media.connect", protocol.MediaConnect{OperationID: uuid.NewString(), SessionID: b.ref.ID, Target: protocol.Endpoint{IP: ip, Port: port}, RTCPTarget: rtcp})
	return err
}
func (b *mediaBridge) acceptLocalSender() {
	defer b.wg.Done()
	listener := b.localListener
	if tcp, ok := listener.(*net.TCPListener); ok {
		_ = tcp.SetDeadline(time.Now().Add(10 * time.Second))
	}
	conn, err := listener.Accept()
	if err != nil {
		b.fail(err)
		return
	}
	server, err := b.mediaServer()
	if err != nil {
		conn.Close()
		b.fail(err)
		return
	}
	host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	if !net.ParseIP(host).Equal(net.ParseIP(server.IP)) {
		conn.Close()
		b.fail(fmt.Errorf("本地媒体发送方不匹配"))
		return
	}
	b.localMu.Lock()
	if b.ctx.Err() != nil {
		b.localMu.Unlock()
		conn.Close()
		return
	}
	b.rtp = conn
	b.wg.Add(2)
	b.localMu.Unlock()
	go b.fromGateway()
	go b.toGateway(conn, protocol.TCPBytes)
}

var _ gbs.ExternalSendLease = (*mediaBridge)(nil)

func (b *mediaBridge) mediaServer() (*sms.MediaServer, error) {
	var server sms.MediaServer
	err := json.Unmarshal([]byte(b.row.MediaNodeSnapshot), &server)
	return &server, err
}
