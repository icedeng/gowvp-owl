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
	"mime"
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

var errSIPTooManyHops = errors.New("request Max-Forwards is zero")

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

func resolveRequestSecurity(resolver RequestSecurityResolver, request *Request) (security MessageSecurity, err error) {
	if resolver == nil {
		return nil, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			security = nil
			err = fmt.Errorf("SIP request security resolver panic: %v", recovered)
		}
	}()
	return resolver(request)
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

func (s *Server) mustServerTX(msg *Request) *Transaction {
	conn := msg.conn
	if conn != nil && conn.Network() == "udp" {
		conn = s.udpConn
	}
	return s.txs.newServerTX(getServerTXKey(msg), conn)
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
			connection := NewTCPConnection(conn)
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
			connection := NewTLSConnection(conn)
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
		// 对端可能在发送最终响应后立即关闭连接；排空已接收帧，避免事务丢响应。
		parser.finish()
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
		// 前一帧读取可能已经把后续完整帧预取到 bufio.Reader；对端随后关闭时，
		// 必须先排空缓存，不能因已关闭连接无法清 deadline 而丢弃后续帧。
		if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
			if reader.Buffered() > 0 {
				message, readErr := readTCPMessage(reader)
				if readErr == nil {
					return message, nil
				}
				return nil, errors.Join(fmt.Errorf("clear SIP/TCP read deadline: %w", err), readErr)
			}
			return nil, io.EOF
		}
		return nil, fmt.Errorf("clear SIP/TCP read deadline: %w", err)
	}
	// 长连接可无限空闲；收到首字节后才启动整帧时限，防止慢速逐字节占用连接。
	if _, err := reader.Peek(1); err != nil {
		return nil, err
	}
	if timeout > 0 {
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			// Peek 已经从连接读到数据；对端此时关闭时，完整帧可能仍在 reader 缓冲区。
			// 对已关闭连接继续排空缓冲区，避免丢弃对端关闭前发送的最终响应。
			if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
				message, readErr := readTCPMessage(reader)
				if readErr == nil {
					return message, nil
				}
				return nil, errors.Join(fmt.Errorf("set SIP/TCP frame deadline: %w", err), readErr)
			}
			return nil, fmt.Errorf("set SIP/TCP frame deadline: %w", err)
		}
		defer func() {
			if clearErr := conn.SetReadDeadline(time.Time{}); clearErr != nil && err != nil {
				err = errors.Join(err, fmt.Errorf("clear SIP/TCP frame deadline: %w", clearErr))
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
	raw := strings.TrimRight(string(line), "\r\n")
	trimmed := strings.TrimSpace(raw)
	if len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\t') && isContentLengthHeaderLine(trimmed) {
		return 0, true, fmt.Errorf("folded Content-Length header is invalid")
	}
	name, value, ok := strings.Cut(trimmed, ":")
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
				if security, resolveErr := resolveRequestSecurity(resolver, msg); resolveErr == nil {
					responseSecurity = security
				}
			}
		}
		statusCode := http.StatusBadRequest
		reason := err.Error()
		switch {
		case !isSupportedSIPVersion(msg.SipVersion()):
			statusCode = http.StatusHTTPVersionNotSupported
			reason = "Version Not Supported"
		case errors.Is(err, errSIPTooManyHops):
			statusCode = 483
			reason = "Too Many Hops"
		}
		response := newInboundResponseFromRequest(msg, statusCode, reason, nil)
		response.SetSipVersion(DefaultSipVersion)
		if responseSecurity != nil {
			if signErr := signOutboundMessage(responseSecurity, response); signErr != nil {
				// 核心字段虽不歧义但仍无法签名时，退化为全新无签名错误响应，避免发送部分签名头。
				response = newInboundResponseFromRequest(msg, statusCode, reason, nil)
				response.SetSipVersion(DefaultSipVersion)
			}
		}
		_ = tx.Respond(response)
		return
	}
	tx := s.mustServerTX(msg)
	if !tx.beginServerRequest() {
		// 若原 handler 仍在处理，允许同源 TCP/TLS 重连接管后续响应；
		// 已完成终态则由 replayServerResponseForRequest 负责重放。
		tx.rebindServerRequestForRequest(msg)
		tx.replayServerResponseForRequest(msg)
		return
	}
	var security MessageSecurity
	if resolver := s.requestSecurityResolver(); resolver != nil {
		resolved, err := resolveRequestSecurity(resolver, msg)
		if err != nil {
			slog.Warn("resolve SIP request security failed", "method", msg.Method(), "err", err)
			_ = tx.Respond(newInboundResponseFromRequest(msg, http.StatusForbidden, "request security unavailable", nil))
			return
		}
		security = resolved
		tx.SetMessageSecurity(resolved)
	}
	if security != nil {
		if err := verifyInboundMessage(security, msg); err != nil {
			slog.Warn("reject SIP request with invalid security proof", "method", msg.Method(), "err", err)
			_ = tx.Respond(newInboundResponseFromRequest(msg, http.StatusForbidden, "invalid request security", nil))
			return
		}
	}
	// logrus.Traceln("receive request from:", msg.Source(), ",method:", msg.Method(), "txKey:", tx.key, "message: \n", msg.String())

	key, err := requestRouteKey(msg)
	if err != nil {
		_ = tx.Respond(newInboundResponseFromRequest(msg, http.StatusBadRequest, err.Error(), nil))
		return
	}
	routeHandlers, ok := s.route.Load(strings.ToUpper(key))
	if !ok {
		slog.Debug("not found handler func", "method", msg.Method(), "msg", msg.String())
		if strings.EqualFold(msg.Method(), MethodMessage) || strings.EqualFold(msg.Method(), MethodNotify) {
			routeHandlers = []HandlerFunc{func(c *Context) {
				_ = c.Tx.Respond(newInboundResponseFromRequest(c.Request, http.StatusBadRequest, http.StatusText(http.StatusBadRequest), nil))
			}}
		} else {
			routeHandlers = []HandlerFunc{func(c *Context) {
				handlerMethodNotAllowed(c.Request, c.Tx)
			}}
		}
	}

	// 全局中间件 + 路由 handler 合并为完整链
	chain := make([]HandlerFunc, 0, len(s.middlewares)+len(routeHandlers))
	chain = append(chain, s.middlewares...)
	chain = append(chain, routeHandlers...)

	ctx, err := newContextChecked(msg, tx)
	if err != nil {
		_ = tx.Respond(newInboundResponseFromRequest(msg, http.StatusBadRequest, err.Error(), nil))
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
	if err := validateInboundHeaderNames(msg); err != nil {
		return fmt.Errorf("request %w", err)
	}
	if !isSIPToken(msg.Method()) {
		return fmt.Errorf("request method is invalid")
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
	if !isSIPToken(cseq.MethodName) {
		return fmt.Errorf("request CSeq method is invalid")
	}
	if err := ValidateCSeq(cseq.SeqNo); err != nil {
		return fmt.Errorf("request %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(cseq.MethodName), strings.TrimSpace(msg.Method())) {
		return fmt.Errorf("request method does not match CSeq method")
	}
	maxForwardsHeaders := msg.GetHeaders("Max-Forwards")
	if len(maxForwardsHeaders) != 1 {
		return fmt.Errorf("request must contain exactly one Max-Forwards header")
	}
	maxForwards, ok := maxForwardsHeaders[0].(*MaxForwards)
	if !ok || maxForwards == nil {
		return fmt.Errorf("request Max-Forwards header is invalid")
	}
	if *maxForwards > 255 {
		// 程序化构造的 Request 不一定经过文本解析，仍需在公共入口执行同一范围校验。
		return fmt.Errorf("request Max-Forwards header exceeds 255")
	}
	if *maxForwards == 0 && !strings.EqualFold(strings.TrimSpace(msg.Method()), MethodOptions) {
		return errSIPTooManyHops
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
	ctx.Tx.beginServerHandler()
	s.lifecycleMu.Unlock()
	go func() {
		defer s.requestWG.Done()
		s.runContextSafely(ctx)
	}()
}

func requestRouteKey(msg *Request) (string, error) {
	// 路由表按大写方法名索引；先规范化方法，再判断是否需要解析 MANSCDP CmdType。
	// 否则混合大小写的 MESSAGE/NOTIFY 会落到只有组中间件的基础路由，正文 handler 不会执行。
	key := strings.ToUpper(strings.TrimSpace(msg.Method()))
	if key == MethodSubscribe {
		if len(msg.Body()) == 0 {
			return key, nil
		}
		if err := validateMANSCDPContentType(msg); err != nil {
			return "", err
		}
		return key, nil
	}
	if key != MethodMessage && key != MethodNotify {
		return key, nil
	}
	if length, ok := msg.ContentLength(); !ok || length == nil || *length == 0 {
		if key == MethodNotify && isTerminatedSubscriptionNotify(msg) {
			return key, nil
		}
		return "", fmt.Errorf("empty body")
	}
	if err := validateMANSCDPContentType(msg); err != nil {
		return "", err
	}
	var parsed MessageReceive
	if err := XMLDecode(msg.Body(), &parsed); err != nil {
		return "", fmt.Errorf("invalid xml")
	}
	cmdType := strings.TrimSpace(parsed.CmdType)
	if cmdType == "" {
		return "", fmt.Errorf("missing CmdType")
	}
	return key + "-" + cmdType, nil
}

func validateMANSCDPContentType(msg *Request) error {
	headers := msg.GetHeaders("Content-Type")
	if len(headers) != 1 {
		return fmt.Errorf("%s body requires exactly one Content-Type", msg.Method())
	}
	contentType, ok := msg.ContentType()
	if !ok || contentType == nil {
		return fmt.Errorf("invalid %s Content-Type", msg.Method())
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(string(*contentType)))
	if err != nil || !strings.EqualFold(mediaType, string(ContentTypeXML)) {
		return fmt.Errorf("%s body requires %s Content-Type", msg.Method(), string(ContentTypeXML))
	}
	return nil
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
	if err := validateInboundHeaderNames(msg); err != nil {
		return fmt.Errorf("response %w", err)
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
	if !isSIPToken(cseq.MethodName) {
		return fmt.Errorf("response CSeq method is invalid")
	}
	if err := ValidateCSeq(cseq.SeqNo); err != nil {
		return fmt.Errorf("response %w", err)
	}
	if msg.StatusCode() >= 200 && strings.EqualFold(strings.TrimSpace(cseq.MethodName), MethodInvite) && dialogHeaderParam(to.Params, "tag") == "" {
		return fmt.Errorf("response INVITE final To tag is invalid")
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

func validateInboundHeaderNames(msg Message) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("SIP header name validation panic: %v", recovered)
		}
	}()
	for index, header := range msg.Headers() {
		if isNilInterfaceValue(header) {
			return fmt.Errorf("SIP header %d is nil", index)
		}
		if malformed, ok := header.(*malformedHeader); ok {
			return fmt.Errorf("SIP header %d is malformed: %s", index, malformed.cause)
		}
		if name := strings.TrimSpace(header.Name()); !isSIPToken(name) {
			return fmt.Errorf("SIP header %d name is invalid", index)
		}
	}
	return nil
}

func isSupportedSIPVersion(version string) bool {
	return strings.EqualFold(strings.TrimSpace(version), DefaultSipVersion)
}

func validateOutboundRequestCSeq(request *Request) error {
	if request == nil {
		return fmt.Errorf("SIP request is nil")
	}
	if len(request.GetHeaders("CSeq")) != 1 {
		return fmt.Errorf("SIP request must contain exactly one CSeq header")
	}
	cseq, ok := request.CSeq()
	if !ok || cseq == nil || !isSIPToken(strings.TrimSpace(cseq.MethodName)) {
		return fmt.Errorf("SIP request CSeq header is invalid")
	}
	if err := ValidateCSeq(cseq.SeqNo); err != nil {
		return fmt.Errorf("SIP request %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(cseq.MethodName), strings.TrimSpace(request.Method())) {
		return fmt.Errorf("SIP request method does not match CSeq method")
	}
	return nil
}

func validateOutboundRequestHeaders(request *Request) error {
	if request == nil {
		return fmt.Errorf("SIP request is nil")
	}
	if !isSupportedSIPVersion(request.SipVersion()) {
		return fmt.Errorf("SIP request version is invalid")
	}
	if request.Destination() == nil {
		return fmt.Errorf("SIP request destination is unavailable")
	}
	if !isSIPToken(strings.TrimSpace(request.Method())) {
		return fmt.Errorf("SIP request method is invalid")
	}
	recipient := request.Recipient()
	if err := validateOutboundURI("request recipient", recipient); err != nil {
		return fmt.Errorf("SIP request recipient is invalid")
	}
	if headers := request.GetHeaders("From"); len(headers) != 1 {
		return fmt.Errorf("SIP request must contain exactly one From header")
	}
	from, ok := request.From()
	if !ok || from == nil || from.Address == nil || strings.TrimSpace(from.Address.Host()) == "" || from.Params == nil {
		return fmt.Errorf("SIP request From header is invalid")
	}
	if headers := request.GetHeaders("To"); len(headers) != 1 {
		return fmt.Errorf("SIP request must contain exactly one To header")
	}
	to, ok := request.To()
	if !ok || to == nil || to.Address == nil || strings.TrimSpace(to.Address.Host()) == "" {
		return fmt.Errorf("SIP request To header is invalid")
	}
	if headers := request.GetHeaders("Call-ID"); len(headers) != 1 {
		return fmt.Errorf("SIP request must contain exactly one Call-ID header")
	}
	callID, ok := request.CallID()
	if !ok || callID == nil || strings.TrimSpace(string(*callID)) == "" {
		return fmt.Errorf("SIP request Call-ID header is invalid")
	}
	if err := validateOutboundRequestCSeq(request); err != nil {
		return err
	}
	maxForwardsHeaders := request.GetHeaders("Max-Forwards")
	if len(maxForwardsHeaders) != 1 {
		return fmt.Errorf("SIP request must contain exactly one Max-Forwards header")
	}
	maxForwards, ok := maxForwardsHeaders[0].(*MaxForwards)
	if !ok || maxForwards == nil || *maxForwards > 255 {
		return fmt.Errorf("SIP request Max-Forwards header is invalid")
	}
	viaHeaders := request.GetHeaders("Via")
	if len(viaHeaders) == 0 {
		return fmt.Errorf("SIP request Via header is invalid")
	}
	for _, header := range viaHeaders {
		via, ok := header.(ViaHeader)
		if !ok || len(via) == 0 {
			return fmt.Errorf("SIP request Via header is invalid")
		}
		for _, hop := range via {
			if !isValidSIPViaHop(hop) || isNilInterfaceValue(hop.Params) {
				return fmt.Errorf("SIP request Via header is invalid")
			}
		}
	}
	return nil
}

func validateOutboundResponseCSeq(response *Response) error {
	if response == nil {
		return fmt.Errorf("SIP response is nil")
	}
	if response.Destination() == nil {
		return fmt.Errorf("SIP response destination is unavailable")
	}
	if !isSupportedSIPVersion(response.SipVersion()) {
		return fmt.Errorf("SIP response version is invalid")
	}
	if response.StatusCode() < 100 || response.StatusCode() > 699 {
		return fmt.Errorf("SIP response status code is invalid")
	}
	// 畸形请求的 400 响应可能无法复制唯一 CSeq；仍允许发送错误响应，但只要
	// 存在程序化 CSeq 就绝不能写出解析器自身会拒绝的数值。
	for _, header := range response.GetHeaders("CSeq") {
		cseq, ok := header.(*CSeq)
		if !ok || cseq == nil {
			continue
		}
		if !isSIPToken(cseq.MethodName) {
			return fmt.Errorf("SIP response CSeq method is invalid")
		}
		if err := ValidateCSeq(cseq.SeqNo); err != nil {
			return fmt.Errorf("SIP response %w", err)
		}
	}
	return nil
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
	ctx, cancel := transactionWriteContext()
	defer cancel()
	return s.RequestWithSecurityContext(ctx, req, security)
}

// RequestWithSecurityContext 在安装事务级安全器后写出请求，并允许 context 中止阻塞的流式写入。
func (s *Server) RequestWithSecurityContext(ctx context.Context, req *Request, security MessageSecurity) (*Transaction, error) {
	return s.requestWithSecurityContext(ctx, req, security, false)
}

// RequestWithSecurityContextOwnedConnection 写出请求，并把当前连接的生命周期交给事务。
// 仅应用于为单次非对话请求主动建立的连接；最终响应、写失败或事务关闭都会释放连接。
func (s *Server) RequestWithSecurityContextOwnedConnection(ctx context.Context, req *Request, security MessageSecurity) (*Transaction, error) {
	return s.requestWithSecurityContext(ctx, req, security, true)
}

// PreparedRequest 保存已完成校验、签名、序列化和事务快照的请求。
// Send 之后只进入连接写入；Close 用于放弃尚未提交 CSeq 的准备结果。
type PreparedRequest struct {
	mu       sync.Mutex
	done     bool
	ctx      context.Context
	server   *Server
	tx       *Transaction
	prepared *preparedTransactionRequest
}

// PrepareRequestWithSecurityContext 完成请求写出前的全部纯本地工作，并暂时锁定服务生命周期。
// 调用方必须恰好调用一次 Send 或 Close。
func (s *Server) PrepareRequestWithSecurityContext(ctx context.Context, req *Request, security MessageSecurity) (*PreparedRequest, error) {
	if s == nil || req == nil {
		return nil, fmt.Errorf("SIP request is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !isSupportedSIPVersion(req.SipVersion()) {
		return nil, fmt.Errorf("unsupported SIP request version")
	}
	if err := validateOutboundRequestCSeq(req); err != nil {
		return nil, err
	}
	s.lifecycleMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.lifecycleMu.Unlock()
		}
	}()
	select {
	case <-s.ctx.Done():
		return nil, fmt.Errorf("SIP server is closed")
	default:
	}
	if err := prepareServerRequestTransport(s, req); err != nil {
		return nil, err
	}
	tx := s.mustTX(req)
	tx.SetMessageSecurity(security)
	prepared, err := tx.prepareRequest(req)
	if err != nil {
		tx.Close()
		return nil, err
	}
	slog.Debug("SIP 最终发送报文", "method", req.Method(), "request", string(prepared.payload))
	locked = false
	return &PreparedRequest{ctx: ctx, server: s, tx: tx, prepared: prepared}, nil
}

func prepareServerRequestTransport(s *Server, req *Request) error {
	if req.GetConnection() == nil {
		return fmt.Errorf("SIP request connection is unavailable")
	}
	if req.Destination() == nil {
		return fmt.Errorf("SIP request destination is unavailable")
	}
	viaHop, ok := req.ViaHop()
	if !ok {
		return fmt.Errorf("missing required 'Via' header")
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
		return fmt.Errorf("invalid SIP Via header")
	}
	if viaHop.Params == nil {
		viaHop.Params = NewParams()
	}
	branchKey, branch, branchCount := sipViaParam(viaHop, "branch")
	if branchCount > 1 {
		return fmt.Errorf("Via header contains multiple branch parameters")
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
	return nil
}

// Send 提交已经准备完成的请求写入。
func (prepared *PreparedRequest) Send() (*Transaction, error) {
	if prepared == nil {
		return nil, fmt.Errorf("prepared SIP request is unavailable")
	}
	prepared.mu.Lock()
	if prepared.done {
		prepared.mu.Unlock()
		return nil, fmt.Errorf("prepared SIP request is already completed")
	}
	prepared.done = true
	prepared.mu.Unlock()
	defer prepared.server.lifecycleMu.Unlock()
	if err := prepared.tx.writePreparedRequestContext(prepared.ctx, prepared.prepared); err != nil {
		prepared.tx.Close()
		return nil, err
	}
	return prepared.tx, nil
}

// Close 放弃尚未写出的准备结果并释放服务生命周期锁。
func (prepared *PreparedRequest) Close() {
	if prepared == nil {
		return
	}
	prepared.mu.Lock()
	if prepared.done {
		prepared.mu.Unlock()
		return
	}
	prepared.done = true
	prepared.mu.Unlock()
	prepared.tx.Close()
	prepared.server.lifecycleMu.Unlock()
}

func (s *Server) requestWithSecurityContext(ctx context.Context, req *Request, security MessageSecurity, ownConnection bool) (*Transaction, error) {
	if s == nil || req == nil {
		return nil, fmt.Errorf("SIP request is unavailable")
	}
	var owned Connection
	attached := false
	if ownConnection {
		owned = req.GetConnection()
		defer func() {
			if !attached && owned != nil {
				_ = owned.Close()
			}
		}()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !isSupportedSIPVersion(req.SipVersion()) {
		return nil, fmt.Errorf("unsupported SIP request version")
	}
	if err := validateOutboundRequestCSeq(req); err != nil {
		return nil, err
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
	if err := prepareOutboundContentLength(req); err != nil {
		return nil, err
	}
	if err := validateOutboundRequestHeaders(req); err != nil {
		return nil, err
	}
	preview, err := serializeOutboundMessage(req)
	if err != nil {
		return nil, err
	}
	slog.Debug("SIP 最终发送报文", "method", req.Method(), "request", string(preview))

	tx := s.mustTX(req)
	tx.SetMessageSecurity(security)
	if owned != nil {
		tx.ownConnection(owned)
		attached = true
	}
	err = tx.RequestContext(ctx, req)
	if owned != nil {
		tx.finishOwnedConnectionWrite()
	}
	if err != nil {
		tx.Close()
		return nil, err
	}
	return tx, nil
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
	resp := newInboundResponseFromRequest(req, http.StatusMethodNotAllowed, http.StatusText(http.StatusMethodNotAllowed), []byte{})
	resp.AppendHeader(defaultAllowMethods.Clone())
	tx.Respond(resp)
}

func newInboundResponseFromRequest(req *Request, status int, reason string, body []byte) *Response {
	resp := NewResponseFromRequest("", req, status, reason, body)
	if req != nil && strings.EqualFold(strings.TrimSpace(req.Method()), MethodRegister) {
		version := XGBVer("3.0")
		resp.AppendHeader(&version)
	}
	return resp
}

func (s *Server) runContextSafely(ctx *Context) {
	if ctx != nil && ctx.Tx != nil {
		defer ctx.Tx.completeServerHandler()
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("recover panic in SIP request handler", s.requestPanicLogArgs(ctx, recovered)...)
			s.ensureRequestFinalResponse(ctx, "panic")
		}
	}()

	ctx.Next()
	s.ensureRequestFinalResponse(ctx, "without final response")
}

func (s *Server) ensureRequestFinalResponse(ctx *Context, cause string) {
	if ctx == nil || ctx.Tx == nil || ctx.Request == nil {
		return
	}
	// ACK 不允许响应；只有 panic 异常退出时才释放接纳标记，正常 ACK 仍保持事务幂等。
	if strings.EqualFold(strings.TrimSpace(ctx.Request.Method()), MethodACK) {
		if cause == "panic" {
			ctx.Tx.allowServerRequestRetry()
		}
		return
	}
	// handler 可能已经成功写出业务响应；保留原终态供事务层重放，不能追加矛盾响应。
	if ctx.Tx.hasServerFinalResponse() {
		return
	}
	// handler 已经尝试提交最终响应但写出失败时，RespondContext 会释放事务接纳状态；
	// 保留该重传恢复语义，不能在返回路径追加一个与业务响应矛盾的 500。
	if ctx.Tx.hasServerFinalAttempt() {
		return
	}
	response := newInboundResponseFromRequest(ctx.Request, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError), nil)
	if err := ctx.Tx.Respond(response); err != nil {
		slog.Error("respond incomplete SIP handler", "err", err, "method", ctx.Request.Method(), "cause", cause)
	}
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
