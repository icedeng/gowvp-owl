package sip

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
)

// ExternalRoutingConnection 的可信作用域只由进程内传输适配器提供，不能从 SIP Header 构造。
// Network/SignalingTransport 仍表示设备侧 UDP/TCP，禁止把隧道类型用于 SIP 语义。
type ExternalRoutingConnection interface {
	Connection
	RoutingScope() string
	AdvertisedAddr() net.Addr
	OpenSignalingTransport(context.Context, string, net.Addr) (Connection, error)
}

func RoutingScope(conn Connection) string {
	if external, ok := conn.(ExternalRoutingConnection); ok {
		return external.RoutingScope()
	}
	return ""
}
func IsExternal(conn Connection) bool { return RoutingScope(conn) != "" }

func (s *Server) SetRequestIngressGuard(guard func(*Request) error) {
	s.securityMu.Lock()
	s.ingressGuard = guard
	s.securityMu.Unlock()
}
func (s *Server) requestIngressGuard() func(*Request) error {
	s.securityMu.RLock()
	defer s.securityMu.RUnlock()
	return s.ingressGuard
}

// RetireServerReplay 仅由已授权的入口迁移调用；运行中的 handler 仍由原生命周期管理。
func (s *Server) RetireServerReplay(request *Request) {
	if tx := s.txs.getTX(getServerTXKey(request)); tx != nil {
		tx.serverMu.RLock()
		retire := tx.serverResponseFinal && !tx.serverHandlerActive
		tx.serverMu.RUnlock()
		if retire {
			tx.Close()
		}
	}
}

// InjectExternalPacket 接收一条已经由可信传输完成边界校验的完整 SIP 报文。
// 不经过本机 UDP 端口，沿用 parser、入向安全校验、事务和请求关闭链。
func (s *Server) InjectExternalPacket(ctx context.Context, conn ExternalRoutingConnection, raw []byte) error {
	if conn == nil || conn.RoutingScope() == "" || conn.RemoteAddr() == nil || len(raw) == 0 || len(raw) > maxSIPHeaderBytes+maxSIPBodyBytes {
		return fmt.Errorf("invalid external SIP packet")
	}
	if !s.beginListener() {
		return fmt.Errorf("SIP server is closed")
	}
	defer s.listenerWG.Done()
	p := newParser()
	defer p.stop()
	packet := Packet{reader: bufio.NewReader(bytes.NewReader(raw)), bodylength: getBodyLength(raw), raddr: conn.RemoteAddr(), conn: conn}
	select {
	case p.in <- packet:
		p.finish()
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
	for {
		select {
		case msg, ok := <-p.out:
			if !ok {
				return nil
			}
			switch v := msg.(type) {
			case *Request:
				v.SetDestination(conn.LocalAddr())
				s.handlerRequest(v)
			case *Response:
				v.SetDestination(conn.LocalAddr())
				s.handlerResponse(v)
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
}
