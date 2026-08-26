package sip

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ixugo/goddd/pkg/conc"
)

// TLSListenerOptions 描述 SIP-TLS 监听器及可选客户端证书校验。
type TLSListenerOptions struct {
	CertFile          string
	KeyFile           string
	ClientCAFile      string
	RequireClientCert bool
}

var bufferSize uint16 = 65535 - 20 - 8 // IPv4 max size - IPv4 Header size - UDP Header size

const (
	maxSIPHeaderLineBytes  = 8 << 10
	maxSIPHeaderBytes      = 64 << 10
	maxSIPBodyBytes        = 1 << 20
	sipTCPFrameReadTimeout = 30 * time.Second
)

// Server sip
type Server struct {
	// udpaddr net.Addr
	udpConn Connection

	txs *transacionts

	route conc.Map[string, []HandlerFunc]

	// 全局中间件链，通过 Use() 注册，在所有路由 handler 之前执行
	middlewares []HandlerFunc
	securityMu  sync.RWMutex
	security    RequestSecurityResolver

	port *Port
	host string

	tcpPort     *Port
	tcpListener *net.TCPListener
	tlsListener net.Listener

	tcpaddr net.Addr

	ctx    context.Context
	cancel context.CancelFunc

	closeOnce    sync.Once
	lifecycleMu  sync.Mutex
	listenerWG   sync.WaitGroup
	connectionWG sync.WaitGroup
	requestWG    sync.WaitGroup
	connectionMu sync.Mutex
	connections  map[Connection]struct{}

	from *Address
}

// RequestSecurityResolver 为入向请求解析对端 seed/算法，并返回事务级签名验签器。
type RequestSecurityResolver func(*Request) (MessageSecurity, error)

// NewServer sip server
func NewServer(form *Address) *Server {
	txs := &transacionts{txs: map[string]*Transaction{}, rwm: &sync.RWMutex{}}
	ctx, cancel := context.WithCancel(context.TODO())
	srv := &Server{
		txs:         txs,
		ctx:         ctx,
		cancel:      cancel,
		connections: make(map[Connection]struct{}),
		from:        form,
	}
	if form != nil && form.URI != nil {
		srv.host = form.URI.Host()
		if form.URI.FPort != nil {
			srv.port = form.URI.FPort.Clone()
		}
	}
	return srv
}

// Use 注册全局中间件，所有路由 handler 执行前先经过这些中间件；
// 中间件内调用 ctx.Abort() 可中断后续链路
func (s *Server) Use(middleware ...HandlerFunc) {
	s.middlewares = append(s.middlewares, middleware...)
}

func (s *Server) SetRequestSecurityResolver(resolver RequestSecurityResolver) {
	s.securityMu.Lock()
	s.security = resolver
	s.securityMu.Unlock()
}

func (s *Server) requestSecurityResolver() RequestSecurityResolver {
	s.securityMu.RLock()
	resolver := s.security
	s.securityMu.RUnlock()
	return resolver
}

// SetFrom 热更新 SIP 源地址配置，用于配置变更时无需重启服务
func (s *Server) SetFrom(from *Address) {
	*s.from = *from
}

func (s *Server) addRoute(method string, handler ...HandlerFunc) {
	s.route.Store(strings.ToUpper(method), handler)
}

func (s *Server) Register(handler ...HandlerFunc) {
	s.addRoute(MethodRegister, handler...)
}

func (s *Server) Message(handler ...HandlerFunc) *RouteGroup {
	s.addRoute(MethodMessage, handler...)
	return newRouteGroup(MethodMessage, s, handler...)
}

func (s *Server) Notify(handler ...HandlerFunc) *RouteGroup {
	s.addRoute(MethodNotify, handler...)
	return newRouteGroup(MethodNotify, s, handler...)
}

// Subscribe 注册 SUBSCRIBE 请求处理器。
// 主要用于 9.11 事件源侧订阅流程。
func (s *Server) Subscribe(handler ...HandlerFunc) *RouteGroup {
	s.addRoute(MethodSubscribe, handler...)
	return newRouteGroup(MethodSubscribe, s, handler...)
}

// Handle 注册通用 SIP 方法处理器，用于扩展 INVITE/BYE/ACK 等流程。
func (s *Server) Handle(method string, handler ...HandlerFunc) *RouteGroup {
	s.addRoute(method, handler...)
	return newRouteGroup(method, s, handler...)
}

func (s *Server) getTX(key string) *Transaction {
	return s.txs.getTX(key)
}

func (s *Server) mustTX(msg *Request) *Transaction {
	key := getTXKey(msg)
	tx := s.txs.getTX(key)

	if tx == nil {
		if msg.conn.Network() == "udp" {
			tx = s.txs.newTX(key, s.udpConn)
		} else {
			tx = s.txs.newTX(key, msg.conn)
		}
	} else if msg.conn != nil && msg.conn.Network() == "tcp" {
		// 同一对话可能在 TCP 断线后通过新连接继续，事务必须跟随当前连接。
		tx.setConnection(msg.conn)
	}
	return tx
}

func (s *Server) UDPConn() Connection {
	return s.udpConn
}

// ListenUDPServer ListenUDPServer
func (s *Server) ListenUDPServer(addr string) error {
	if err := s.bindUDP(addr); err != nil {
		return err
	}
	if !s.beginListener() {
		_ = s.udpConn.Close()
		return fmt.Errorf("SIP server is closed")
	}
	defer s.listenerWG.Done()
	return s.serveUDP(s.udpConn)
}

// StartUDPServer 同步完成端口绑定，成功后在后台处理 UDP 报文。
func (s *Server) StartUDPServer(addr string) error {
	if err := s.bindUDP(addr); err != nil {
		return err
	}
	if !s.beginListener() {
		_ = s.udpConn.Close()
		return fmt.Errorf("SIP server is closed")
	}
	conn := s.udpConn
	go func() {
		defer s.listenerWG.Done()
		if err := s.serveUDP(conn); err != nil {
			slog.Error("SIP UDP server stopped", "addr", addr, "err", err)
		}
	}()
	return nil
}

func (s *Server) bindUDP(addr string) error {
	udpaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("net.ResolveUDPAddr err[%w]", err)
	}
	udp, err := net.ListenUDP("udp", udpaddr)
	if err != nil {
		return fmt.Errorf("net.ListenUDP err[%w]", err)
	}
	localAddr, ok := udp.LocalAddr().(*net.UDPAddr)
	if !ok {
		_ = udp.Close()
		return fmt.Errorf("SIP UDP listener returned unexpected local address %T", udp.LocalAddr())
	}
	s.port = NewPort(localAddr.Port)
	if s.host == "" {
		s.host, err = listenerAdvertiseHost(localAddr.IP, udpaddr.IP)
		if err != nil {
			_ = udp.Close()
			return fmt.Errorf("resolve SIP UDP self IP: %w", err)
		}
	}
	s.udpConn = NewUDPConnection(udp)
	return nil
}

func listenerAdvertiseHost(localIP, requestedIP net.IP) (string, error) {
	if localIP != nil && !localIP.IsUnspecified() {
		return localIP.String(), nil
	}
	family := 0
	if requestedIP != nil {
		if requestedIP.To4() != nil {
			family = 4
		} else if requestedIP.To16() != nil {
			family = 6
		}
	}
	ip, err := resolveSelfIPForFamily(family)
	if err != nil {
		return "", err
	}
	return ip.String(), nil
}

func (s *Server) serveUDP(conn Connection) error {
	var (
		raddr net.Addr
		num   int
		err   error
	)
	buf := make([]byte, bufferSize)
	parser := newParser()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		s.handlerListen(parser.out)
	}()
	defer func() {
		parser.stop()
		<-handlerDone
	}()
	for {
		select {
		case <-s.ctx.Done():
			return nil
		default:
			num, raddr, err = conn.ReadFrom(buf)
			if err != nil {
				if s.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
					return nil
				}
				slog.Warn("SIP UDP read failed", "err", err)
				continue
			}
			parser.in <- newPacket(append([]byte{}, buf[:num]...), raddr, conn)
		}
	}
}

// ListenTCPServer 启动 TCP 服务器并监听指定地址。
func (s *Server) ListenTCPServer(addr string) error {
	if err := s.bindTCP(addr); err != nil {
		return err
	}
	if !s.beginListener() {
		_ = s.tcpListener.Close()
		return fmt.Errorf("SIP server is closed")
	}
	defer s.listenerWG.Done()
	return s.serveTCP(s.tcpListener, addr)
}

// StartTCPServer 同步完成端口绑定，成功后在后台接受 TCP 连接。
func (s *Server) StartTCPServer(addr string) error {
	if err := s.bindTCP(addr); err != nil {
		return err
	}
	if !s.beginListener() {
		_ = s.tcpListener.Close()
		return fmt.Errorf("SIP server is closed")
	}
	listener := s.tcpListener
	go func() {
		defer s.listenerWG.Done()
		if err := s.serveTCP(listener, addr); err != nil {
			slog.Error("SIP TCP server stopped", "addr", addr, "err", err)
		}
	}()
	return nil
}

func (s *Server) bindTCP(addr string) error {
	tcpaddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return fmt.Errorf("net.ResolveTCPAddr err[%w]", err)
	}
	s.tcpaddr = tcpaddr
	s.tcpPort = NewPort(tcpaddr.Port)
	tcp, err := net.ListenTCP("tcp", tcpaddr)
	if err != nil {
		return fmt.Errorf("net.ListenTCP err[%w]", err)
	}
	s.tcpListener = tcp
	return nil
}

func (s *Server) serveTCP(listener *net.TCPListener, addr string) error {
	for {
		select {
		case <-s.ctx.Done():
			return nil
		default:
			conn, err := listener.AcceptTCP()
			if err != nil {
				if s.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
					return nil
				}
				return fmt.Errorf("accept SIP TCP connection: %w", err)
			}
			connection := NewTLSConnection(conn)
			if !s.trackConnection(connection) {
				_ = connection.Close()
				continue
			}
			go s.processTrackedTCPConnection(connection)
		}
	}
}

// ListenTLSServer 启动 TLS 服务器并监听指定地址。
func (s *Server) ListenTLSServer(addr, certFile, keyFile string) error {
	return s.ListenTLSServerWithOptions(addr, TLSListenerOptions{CertFile: certFile, KeyFile: keyFile})
}

// ListenTLSServerWithOptions 启动支持可选客户端证书校验的 TLS 监听器。
func (s *Server) ListenTLSServerWithOptions(addr string, options TLSListenerOptions) error {
	if err := s.bindTLS(addr, options); err != nil {
		return err
	}
	if !s.beginListener() {
		_ = s.tlsListener.Close()
		return fmt.Errorf("SIP server is closed")
	}
	defer s.listenerWG.Done()
	return s.serveTLS(s.tlsListener, addr)
}

// StartTLSServer 同步完成证书加载和端口绑定，成功后在后台接受 TLS 连接。
func (s *Server) StartTLSServer(addr, certFile, keyFile string) error {
	return s.StartTLSServerWithOptions(addr, TLSListenerOptions{CertFile: certFile, KeyFile: keyFile})
}

// StartTLSServerWithOptions 在后台启动支持可选客户端证书校验的 TLS 监听器。
func (s *Server) StartTLSServerWithOptions(addr string, options TLSListenerOptions) error {
	if err := s.bindTLS(addr, options); err != nil {
		return err
	}
	if !s.beginListener() {
		_ = s.tlsListener.Close()
		return fmt.Errorf("SIP server is closed")
	}
	listener := s.tlsListener
	go func() {
		defer s.listenerWG.Done()
		if err := s.serveTLS(listener, addr); err != nil {
			slog.Error("SIP TLS server stopped", "addr", addr, "err", err)
		}
	}()
	return nil
}

func (s *Server) bindTLS(addr string, options TLSListenerOptions) error {
	tcpaddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return fmt.Errorf("net.ResolveTCPAddr err[%w]", err)
	}

	tlsConfig, err := newTLSListenerConfig(options)
	if err != nil {
		return err
	}
	ln, err := tls.Listen("tcp", tcpaddr.String(), tlsConfig)
	if err != nil {
		return fmt.Errorf("tls.Listen err[%w]", err)
	}

	s.tlsListener = ln
	return nil
}

func newTLSListenerConfig(options TLSListenerOptions) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(options.CertFile, options.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tls.LoadX509KeyPair err[%w]", err)
	}
	config := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if clientCAFile := strings.TrimSpace(options.ClientCAFile); clientCAFile != "" {
		contents, err := os.ReadFile(clientCAFile)
		if err != nil {
			return nil, fmt.Errorf("read SIP-TLS client CA: %w", err)
		}
		clientCAs := x509.NewCertPool()
		if !clientCAs.AppendCertsFromPEM(contents) {
			return nil, fmt.Errorf("SIP-TLS client CA does not contain a valid certificate")
		}
		config.ClientCAs = clientCAs
		config.ClientAuth = tls.VerifyClientCertIfGiven
	}
	if options.RequireClientCert {
		if config.ClientCAs == nil {
			return nil, fmt.Errorf("SIP-TLS client CA is required when client certificates are mandatory")
		}
		config.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return config, nil
}

func (s *Server) serveTLS(listener net.Listener, addr string) error {
	for {
		select {
		case <-s.ctx.Done():
			return nil
		default:
			conn, err := listener.Accept()
			if err != nil {
				if s.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
					return nil
				}
				return fmt.Errorf("accept SIP TLS connection: %w", err)
			}
			connection := NewTCPConnection(conn)
			if !s.trackConnection(connection) {
				_ = connection.Close()
				continue
			}
			go s.processTrackedTCPConnection(connection)
		}
	}
}

func (s *Server) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.lifecycleMu.Lock()
		if s.cancel != nil {
			s.cancel()
		}
		s.lifecycleMu.Unlock()
		if s.udpConn != nil {
			_ = s.udpConn.Close()
		}
		if s.tcpListener != nil {
			_ = s.tcpListener.Close()
		}
		if s.tlsListener != nil {
			_ = s.tlsListener.Close()
		}
		s.listenerWG.Wait()
		s.closeActiveConnections()
		s.connectionWG.Wait()
		if s.txs != nil {
			s.txs.close()
		}
		s.requestWG.Wait()
	})
}

func (s *Server) beginListener() bool {
	if s == nil {
		return false
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	select {
	case <-s.ctx.Done():
		return false
	default:
		s.listenerWG.Add(1)
		return true
	}
}

func (s *Server) trackConnection(conn Connection) bool {
	if s == nil || conn == nil {
		return false
	}
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	select {
	case <-s.ctx.Done():
		return false
	default:
	}
	if s.connections == nil {
		s.connections = make(map[Connection]struct{})
	}
	s.connections[conn] = struct{}{}
	s.connectionWG.Add(1)
	return true
}

func (s *Server) untrackConnection(conn Connection) {
	if s == nil || conn == nil {
		return
	}
	s.connectionMu.Lock()
	_, tracked := s.connections[conn]
	if tracked {
		delete(s.connections, conn)
	}
	s.connectionMu.Unlock()
	if tracked {
		s.connectionWG.Done()
	}
}

func (s *Server) closeActiveConnections() {
	s.connectionMu.Lock()
	connections := make([]Connection, 0, len(s.connections))
	for conn := range s.connections {
		connections = append(connections, conn)
	}
	s.connectionMu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

// ProcessTcpConn 处理传入的 TCP 连接。
func (s *Server) ProcessTcpConn(conn net.Conn) {
	s.ProcessTCPConnection(NewTCPConnection(conn))
}

// ProcessTCPConnection 处理已建立的入向或出向 SIP/TCP 连接。
// 调用方把连接所有权交给服务器，读取循环退出时连接会被关闭。
func (s *Server) ProcessTCPConnection(conn Connection) {
	if conn == nil {
		return
	}
	if !s.trackConnection(conn) {
		_ = conn.Close()
		return
	}
	s.processTrackedTCPConnection(conn)
}

func (s *Server) processTrackedTCPConnection(conn Connection) {
	defer s.untrackConnection(conn)
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, maxSIPHeaderLineBytes+1)

	parser := newParser()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		s.handlerListen(parser.out)
	}()
	defer func() {
		parser.stop()
		<-handlerDone
	}()

	for {
		message, err := readTCPMessageWithTimeout(conn, reader, sipTCPFrameReadTimeout)
		if err != nil {
			if err != io.EOF {
				slog.Warn("reject SIP/TCP message", "err", err)
			}
			return
		}
		parser.in <- newPacket(message, conn.RemoteAddr(), conn)
	}
}

func readTCPMessageWithTimeout(conn Connection, reader *bufio.Reader, timeout time.Duration) (message []byte, err error) {
	if conn == nil {
		return nil, fmt.Errorf("SIP/TCP connection is nil")
	}
	if reader == nil {
		return nil, fmt.Errorf("SIP/TCP reader is nil")
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear SIP/TCP read deadline: %w", err)
	}
	// 长连接可无限空闲；收到首字节后才启动整帧时限，防止慢速逐字节占用连接。
	if _, err := reader.Peek(1); err != nil {
		return nil, err
	}
	if timeout > 0 {
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return nil, fmt.Errorf("set SIP/TCP frame deadline: %w", err)
		}
		defer func() {
			clearErr := conn.SetReadDeadline(time.Time{})
			if err == nil && clearErr != nil {
				err = fmt.Errorf("clear SIP/TCP frame deadline: %w", clearErr)
			}
		}()
	}
	return readTCPMessage(reader)
}

func readTCPMessage(reader *bufio.Reader) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("SIP/TCP reader is nil")
	}
	var buffer bytes.Buffer
	bodyLen := 0
	bodyLenSeen := false
	headerBytes := 0
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, fmt.Errorf("SIP header line exceeds %d bytes", maxSIPHeaderLineBytes)
		}
		if err != nil {
			return nil, err
		}
		if len(line) > maxSIPHeaderLineBytes {
			return nil, fmt.Errorf("SIP header line exceeds %d bytes", maxSIPHeaderLineBytes)
		}
		headerBytes += len(line)
		if headerBytes > maxSIPHeaderBytes {
			return nil, fmt.Errorf("SIP headers exceed %d bytes", maxSIPHeaderBytes)
		}
		_, _ = buffer.Write(line)
		if len(line) <= 2 {
			if bodyLen > 0 {
				body := make([]byte, bodyLen)
				if _, err := io.ReadFull(reader, body); err != nil {
					return nil, fmt.Errorf("read SIP body: %w", err)
				}
				_, _ = buffer.Write(body)
			}
			return buffer.Bytes(), nil
		}

		length, found, err := parseTCPContentLength(line)
		if err != nil {
			return nil, err
		}
		if found {
			if bodyLenSeen {
				return nil, fmt.Errorf("multiple Content-Length headers")
			}
			bodyLen = length
			bodyLenSeen = true
		}
	}
}

// parseTCPContentLength 按 SIP 头名称大小写不敏感规则解析消息帧长度，
// 同时兼容 RFC 3261 定义的紧凑头名 l。
func parseTCPContentLength(line []byte) (int, bool, error) {
	name, value, ok := strings.Cut(strings.TrimSpace(string(line)), ":")
	if !ok {
		return 0, false, nil
	}
	name = strings.TrimSpace(name)
	if !strings.EqualFold(name, "Content-Length") && !strings.EqualFold(name, "l") {
		return 0, false, nil
	}
	length, err := strconv.ParseUint(strings.TrimSpace(value), 10, 31)
	if err != nil {
		return 0, true, fmt.Errorf("invalid %s value: %w", name, err)
	}
	if length > maxSIPBodyBytes {
		return 0, true, fmt.Errorf("%s exceeds %d bytes", name, maxSIPBodyBytes)
	}
	return int(length), true, nil
}

func (s *Server) handlerListen(msgs <-chan Message) {
	for msg := range msgs {
		switch tmsg := msg.(type) {
		case *Request:
			req := tmsg

			// 对面向连接传输（TCP/TLS），响应源地址使用当前连接本地地址。
			if req.conn != nil && req.conn.Network() == "tcp" {
				req.SetDestination(req.conn.LocalAddr())
			}

			s.handlerRequest(req)
		case *Response:
			resp := tmsg

			if resp.conn != nil && resp.conn.Network() == "tcp" {
				resp.SetDestination(resp.conn.LocalAddr())
			}
			s.handlerResponse(resp)
		default:
			// logrus.Errorln("undefind msg type,", tmsg, msg.String())
		}
	}
}

func (s *Server) handlerRequest(msg *Request) {
	if err := validateInboundRequestHeaders(msg); err != nil {
		// ACK 没有对应响应事务，畸形 ACK 只丢弃，不能进入业务状态机。
		if msg == nil || msg.IsAck() {
			return
		}
		// 畸形报文不能复用由 Call-ID/CSeq 派生的正式事务键，否则可能覆盖合法事务的连接或安全器。
		tx := NewTransaction(RandString(16), msg.GetConnection())
		defer tx.Close()
		var responseSecurity MessageSecurity
		if malformedRequestCanBeSigned(msg) {
			if resolver := s.requestSecurityResolver(); resolver != nil {
				if security, resolveErr := resolver(msg); resolveErr == nil {
					responseSecurity = security
				}
			}
		}
		statusCode := http.StatusBadRequest
		reason := err.Error()
		if !isSupportedSIPVersion(msg.SipVersion()) {
			statusCode = http.StatusHTTPVersionNotSupported
			reason = "Version Not Supported"
		}
		response := NewResponseFromRequest("", msg, statusCode, reason, nil)
		response.SetSipVersion(DefaultSipVersion)
		if responseSecurity != nil {
			if signErr := responseSecurity.Sign(response); signErr != nil {
				// 核心字段虽不歧义但仍无法签名时，退化为全新无签名错误响应，避免发送部分签名头。
				response = NewResponseFromRequest("", msg, statusCode, reason, nil)
				response.SetSipVersion(DefaultSipVersion)
			}
		}
		_ = tx.Respond(response)
		return
	}
	tx := s.mustTX(msg)
	var security MessageSecurity
	if resolver := s.requestSecurityResolver(); resolver != nil {
		resolved, err := resolver(msg)
		if err != nil {
			slog.Warn("resolve SIP request security failed", "method", msg.Method(), "err", err)
			_ = tx.Respond(NewResponseFromRequest("", msg, http.StatusForbidden, "request security unavailable", nil))
			return
		}
		security = resolved
		tx.SetMessageSecurity(resolved)
	}
	if security != nil {
		if err := security.Verify(msg); err != nil {
			slog.Warn("reject SIP request with invalid security proof", "method", msg.Method(), "err", err)
			_ = tx.Respond(NewResponseFromRequest("", msg, http.StatusForbidden, "invalid request security", nil))
			return
		}
	}
	// logrus.Traceln("receive request from:", msg.Source(), ",method:", msg.Method(), "txKey:", tx.key, "message: \n", msg.String())

	key, err := requestRouteKey(msg)
	if err != nil {
		_ = tx.Respond(NewResponseFromRequest("", msg, http.StatusBadRequest, err.Error(), nil))
		return
	}
	routeHandlers, ok := s.route.Load(strings.ToUpper(key))
	if !ok {
		slog.Debug("not found handler func", "method", msg.Method(), "msg", msg.String())
		routeHandlers = []HandlerFunc{func(c *Context) {
			handlerMethodNotAllowed(c.Request, c.Tx)
		}}
	}

	// 全局中间件 + 路由 handler 合并为完整链
	chain := make([]HandlerFunc, 0, len(s.middlewares)+len(routeHandlers))
	chain = append(chain, s.middlewares...)
	chain = append(chain, routeHandlers...)

	ctx, err := newContextChecked(msg, tx)
	if err != nil {
		_ = tx.Respond(NewResponseFromRequest("", msg, http.StatusBadRequest, err.Error(), nil))
		return
	}
	ctx.handlers = chain
	ctx.From = s.from
	ctx.svr = s
	s.startRequestContext(ctx)
}

func malformedRequestCanBeSigned(msg *Request) bool {
	if msg == nil || len(msg.GetHeaders("From")) != 1 || len(msg.GetHeaders("To")) != 1 ||
		len(msg.GetHeaders("Call-ID")) != 1 || len(msg.GetHeaders("CSeq")) != 1 {
		return false
	}
	from, fromOK := msg.From()
	to, toOK := msg.To()
	callID, callIDOK := msg.CallID()
	cseq, cseqOK := msg.CSeq()
	return fromOK && from != nil && from.Address != nil && toOK && to != nil && to.Address != nil &&
		callIDOK && callID != nil && strings.TrimSpace(string(*callID)) != "" &&
		cseqOK && cseq != nil && strings.TrimSpace(cseq.MethodName) != ""
}

func validateInboundRequestHeaders(msg *Request) error {
	if msg == nil {
		return fmt.Errorf("request is nil")
	}
	if !isSupportedSIPVersion(msg.SipVersion()) {
		return fmt.Errorf("request SIP version is invalid")
	}
	if headers := msg.GetHeaders("From"); len(headers) != 1 {
		return fmt.Errorf("request must contain exactly one From header")
	}
	from, ok := msg.From()
	if !ok || from == nil || from.Address == nil {
		return fmt.Errorf("request From header is invalid")
	}
	if headers := msg.GetHeaders("To"); len(headers) != 1 {
		return fmt.Errorf("request must contain exactly one To header")
	}
	to, ok := msg.To()
	if !ok || to == nil || to.Address == nil {
		return fmt.Errorf("request To header is invalid")
	}
	if headers := msg.GetHeaders("Call-ID"); len(headers) != 1 {
		return fmt.Errorf("request must contain exactly one Call-ID header")
	}
	callID, ok := msg.CallID()
	if !ok || callID == nil || strings.TrimSpace(string(*callID)) == "" {
		return fmt.Errorf("request Call-ID header is invalid")
	}
	if headers := msg.GetHeaders("CSeq"); len(headers) != 1 {
		return fmt.Errorf("request must contain exactly one CSeq header")
	}
	cseq, ok := msg.CSeq()
	if !ok || cseq == nil || strings.TrimSpace(cseq.MethodName) == "" {
		return fmt.Errorf("request CSeq header is invalid")
	}
	if !strings.EqualFold(strings.TrimSpace(cseq.MethodName), strings.TrimSpace(msg.Method())) {
		return fmt.Errorf("request method does not match CSeq method")
	}
	via, ok := msg.ViaHop()
	if !ok || !isValidSIPViaHop(via) {
		return fmt.Errorf("request Via header is invalid")
	}
	if !messageTransportMatchesConnection(msg) {
		return fmt.Errorf("request Via transport does not match connection")
	}
	return nil
}

func messageTransportMatchesConnection(msg Message) bool {
	if msg == nil {
		return false
	}
	transport := SignalingTransport(msg.GetConnection())
	if transport == "" {
		return true
	}
	via, ok := msg.ViaHop()
	return ok && via != nil && strings.EqualFold(strings.TrimSpace(via.Transport), transport)
}

func (s *Server) startRequestContext(ctx *Context) {
	if s == nil || ctx == nil {
		return
	}
	s.lifecycleMu.Lock()
	select {
	case <-s.ctx.Done():
		s.lifecycleMu.Unlock()
		return
	default:
		s.requestWG.Add(1)
	}
	s.lifecycleMu.Unlock()
	go func() {
		defer s.requestWG.Done()
		s.runContextSafely(ctx)
	}()
}

func requestRouteKey(msg *Request) (string, error) {
	key := msg.Method()
	if key != MethodMessage && key != MethodNotify {
		return key, nil
	}
	if length, ok := msg.ContentLength(); !ok || length == nil || *length == 0 {
		if key == MethodNotify && isTerminatedSubscriptionNotify(msg) {
			return key, nil
		}
		return "", fmt.Errorf("empty body")
	}
	var parsed MessageReceive
	if err := XMLDecode(msg.Body(), &parsed); err != nil {
		return "", fmt.Errorf("invalid xml")
	}
	return key + "-" + parsed.CmdType, nil
}

func isTerminatedSubscriptionNotify(msg *Request) bool {
	if msg == nil {
		return false
	}
	for _, header := range msg.GetHeaders("Subscription-State") {
		value := header.String()
		if _, after, ok := strings.Cut(value, ":"); ok {
			value = after
		}
		state, _, _ := strings.Cut(strings.TrimSpace(value), ";")
		if strings.EqualFold(strings.TrimSpace(state), "terminated") {
			return true
		}
	}
	return false
}

func (s *Server) handlerResponse(msg *Response) {
	if err := validateInboundResponseHeaders(msg); err != nil {
		slog.Warn("discard malformed SIP response", "err", err)
		return
	}
	tx := s.getTX(getTXKey(msg))
	if tx == nil {
		// logrus.Infoln("not found tx. receive response from:", msg.Source(), "message: \n", msg.String())
	} else if tx.acceptsResponse(msg) {
		// logrus.Traceln("receive response from:", msg.Source(), "txKey:", tx.key, "message: \n", msg.String())
		tx.receiveResponse(msg)
	}
}

func validateInboundResponseHeaders(msg *Response) error {
	if msg == nil {
		return fmt.Errorf("response is nil")
	}
	if !isSupportedSIPVersion(msg.SipVersion()) {
		return fmt.Errorf("response SIP version is invalid")
	}
	if msg.StatusCode() < 100 || msg.StatusCode() > 699 {
		return fmt.Errorf("response status code is invalid")
	}
	if headers := msg.GetHeaders("From"); len(headers) != 1 {
		return fmt.Errorf("response must contain exactly one From header")
	}
	from, ok := msg.From()
	if !ok || from == nil || from.Address == nil {
		return fmt.Errorf("response From header is invalid")
	}
	if headers := msg.GetHeaders("To"); len(headers) != 1 {
		return fmt.Errorf("response must contain exactly one To header")
	}
	to, ok := msg.To()
	if !ok || to == nil || to.Address == nil {
		return fmt.Errorf("response To header is invalid")
	}
	if headers := msg.GetHeaders("Call-ID"); len(headers) != 1 {
		return fmt.Errorf("response must contain exactly one Call-ID header")
	}
	callID, ok := msg.CallID()
	if !ok || callID == nil || strings.TrimSpace(string(*callID)) == "" {
		return fmt.Errorf("response Call-ID header is invalid")
	}
	if headers := msg.GetHeaders("CSeq"); len(headers) != 1 {
		return fmt.Errorf("response must contain exactly one CSeq header")
	}
	cseq, ok := msg.CSeq()
	if !ok || cseq == nil || strings.TrimSpace(cseq.MethodName) == "" {
		return fmt.Errorf("response CSeq header is invalid")
	}
	via, ok := msg.ViaHop()
	if !ok || !isValidSIPViaHop(via) {
		return fmt.Errorf("response Via header is invalid")
	}
	if !messageTransportMatchesConnection(msg) {
		return fmt.Errorf("response Via transport does not match connection")
	}
	return nil
}

func isSupportedSIPVersion(version string) bool {
	return strings.EqualFold(strings.TrimSpace(version), DefaultSipVersion)
}

func isValidSIPViaHop(via *ViaHop) bool {
	return via != nil && strings.EqualFold(strings.TrimSpace(via.ProtocolName), "SIP") &&
		strings.EqualFold(strings.TrimSpace(via.ProtocolVersion), "2.0") &&
		strings.TrimSpace(via.Transport) != "" && strings.TrimSpace(via.Host) != ""
}

// Request Request
func (s *Server) Request(req *Request) (*Transaction, error) {
	return s.RequestWithSecurity(req, nil)
}

// RequestWithSecurity 在报文写出前安装事务级签名器，避免响应早于验签器安装的竞态。
func (s *Server) RequestWithSecurity(req *Request, security MessageSecurity) (*Transaction, error) {
	if s == nil || req == nil {
		return nil, fmt.Errorf("SIP request is unavailable")
	}
	if !isSupportedSIPVersion(req.SipVersion()) {
		return nil, fmt.Errorf("unsupported SIP request version")
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	select {
	case <-s.ctx.Done():
		return nil, fmt.Errorf("SIP server is closed")
	default:
	}
	if req.GetConnection() == nil {
		return nil, fmt.Errorf("SIP request connection is unavailable")
	}
	if req.Destination() == nil {
		return nil, fmt.Errorf("SIP request destination is unavailable")
	}
	viaHop, ok := req.ViaHop()
	if !ok {
		return nil, fmt.Errorf("missing required 'Via' header")
	}
	if viaHop.Host == "" {
		viaHop.Host = s.host
	}
	if transport := SignalingTransport(req.GetConnection()); transport != "" {
		viaHop.Transport = transport
	}
	if viaHop.Port == nil {
		viaHop.Port = connectionLocalPort(req.GetConnection())
		if viaHop.Port == nil {
			viaHop.Port = s.port
		}
	}
	if !isValidSIPViaHop(viaHop) {
		return nil, fmt.Errorf("invalid SIP Via header")
	}
	if viaHop.Params == nil {
		viaHop.Params = NewParams()
	}
	branchKey, branch, branchCount := sipViaParam(viaHop, "branch")
	if branchCount > 1 {
		return nil, fmt.Errorf("Via header contains multiple branch parameters")
	}
	if branchCount == 0 {
		branchKey = "branch"
	}
	if branch == "" {
		viaHop.Params.Add(branchKey, String{Str: GenerateBranch()})
	}
	if _, _, count := sipViaParam(viaHop, "rport"); count == 0 {
		viaHop.Params.Add("rport", nil)
	}

	slog.Debug("SIP 最终发送报文", "method", req.Method(), "request", req.String())

	tx := s.mustTX(req)
	tx.SetMessageSecurity(security)
	return tx, tx.Request(req)
}

func connectionLocalPort(conn Connection) *Port {
	if conn == nil || conn.LocalAddr() == nil {
		return nil
	}
	switch addr := conn.LocalAddr().(type) {
	case *net.UDPAddr:
		if addr != nil && addr.Port > 0 {
			return NewPort(addr.Port)
		}
	case *net.TCPAddr:
		if addr != nil && addr.Port > 0 {
			return NewPort(addr.Port)
		}
	}
	_, rawPort, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return nil
	}
	value, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || value == 0 {
		return nil
	}
	return NewPort(int(value))
}

func handlerMethodNotAllowed(req *Request, tx *Transaction) {
	resp := NewResponseFromRequest("", req, http.StatusMethodNotAllowed, http.StatusText(http.StatusMethodNotAllowed), []byte{})
	tx.Respond(resp)
}

func (s *Server) runContextSafely(ctx *Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("recover panic in SIP request handler", s.requestPanicLogArgs(ctx, recovered)...)
		}
	}()

	ctx.Next()
}

func (s *Server) requestPanicLogArgs(ctx *Context, recovered any) []any {
	args := []any{
		"panic", recovered,
		"stack", string(debug.Stack()),
	}

	if ctx == nil || ctx.Request == nil {
		return args
	}

	req := ctx.Request
	args = append(args, "method", req.Method())

	if callID, ok := req.CallID(); ok && callID != nil {
		args = append(args, "call_id", string(*callID))
	}

	if from, ok := req.From(); ok && from != nil {
		args = append(args, "from", from.String())
	}

	if remote := req.Source(); remote != nil {
		args = append(args, "remote_addr", remote.String())
	}

	return args
}
