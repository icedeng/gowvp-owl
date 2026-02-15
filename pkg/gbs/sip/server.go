package sip

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/ixugo/goddd/pkg/conc"
)

var bufferSize uint16 = 65535 - 20 - 8 // IPv4 max size - IPv4 Header size - UDP Header size

// Server sip
type Server struct {
	// udpaddr net.Addr
	udpConn Connection

	txs *transacionts

	route conc.Map[string, []HandlerFunc]

	port *Port
	host net.IP

	tcpPort     *Port
	tcpListener *net.TCPListener
	tlsListener net.Listener

	tcpaddr net.Addr

	ctx    context.Context
	cancel context.CancelFunc

	from *Address
}

// NewServer sip server
func NewServer(form *Address) *Server {
	activeTX = &transacionts{txs: map[string]*Transaction{}, rwm: &sync.RWMutex{}}
	ctx, cancel := context.WithCancel(context.TODO())
	srv := &Server{
		txs:    activeTX,
		ctx:    ctx,
		cancel: cancel,
		from:   form,
	}
	return srv
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
	}
	return tx
}

func (s *Server) UDPConn() Connection {
	return s.udpConn
}

// ListenUDPServer ListenUDPServer
func (s *Server) ListenUDPServer(addr string) {
	udpaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		panic(fmt.Errorf("net.ResolveUDPAddr err[%w]", err))
	}
	s.port = NewPort(udpaddr.Port)
	s.host, err = ResolveSelfIP()
	if err != nil {
		panic(fmt.Errorf("net.ListenUDP resolveip err[%w]", err))
	}
	udp, err := net.ListenUDP("udp", udpaddr)
	if err != nil {
		panic(fmt.Errorf("net.ListenUDP err[%w]", err))
	}
	s.udpConn = NewUDPConnection(udp)
	var (
		raddr net.Addr
		num   int
	)
	buf := make([]byte, bufferSize)
	parser := newParser()
	defer parser.stop()
	go s.handlerListen(parser.out)
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			num, raddr, err = s.udpConn.ReadFrom(buf)
			if err != nil {
				slog.Error("udp.ReadFromUDP", "err", err)
				continue
			}
			parser.in <- newPacket(append([]byte{}, buf[:num]...), raddr, s.udpConn)
		}
	}
}

// ListenTCPServer 启动 TCP 服务器并监听指定地址。
func (s *Server) ListenTCPServer(addr string) {
	// 解析传入的地址为 TCP 地址
	tcpaddr, err := net.ResolveTCPAddr("tcp", addr)
	// 如果解析地址失败，则抛出错误
	if err != nil {
		panic(fmt.Errorf("net.ResolveUDPAddr err[%w]", err))
	}
	// 保存解析后的 TCP 地址到服务器结构体
	s.tcpaddr = tcpaddr
	// 创建新的端口实例并保存到服务器结构体
	s.tcpPort = NewPort(tcpaddr.Port)

	// 创建 TCP 监听器
	tcp, err := net.ListenTCP("tcp", tcpaddr)
	// 如果创建监听器失败，则抛出错误
	if err != nil {
		panic(fmt.Errorf("net.ListenUDP err[%w]", err))
	}
	// 确保在方法退出时关闭 TCP 监听器
	// 当这个关闭时 所有的设备的socket都会被关闭
	// defer tcp.Close()
	// 保存 TCP 监听器到服务器结构体
	s.tcpListener = tcp
	// 无限循环接受连接

	for {
		select {
		case <-s.ctx.Done():
			slog.Info("ListenTCPServer Has Been Exits")
			return
		default:
			conn, err := tcp.AcceptTCP()
			if err != nil {
				slog.Error("net.ListenTCP", "err", err, "addr", addr)
				return
			}
			go s.ProcessTcpConn(conn)
		}
	}
}

// ListenTLSServer 启动 TLS 服务器并监听指定地址。
func (s *Server) ListenTLSServer(addr, certFile, keyFile string) error {
	tcpaddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return fmt.Errorf("net.ResolveTCPAddr err[%w]", err)
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("tls.LoadX509KeyPair err[%w]", err)
	}
	ln, err := tls.Listen("tcp", tcpaddr.String(), &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		return fmt.Errorf("tls.Listen err[%w]", err)
	}

	s.tlsListener = ln
	for {
		select {
		case <-s.ctx.Done():
			slog.Info("ListenTLSServer Has Been Exits")
			return nil
		default:
			conn, err := ln.Accept()
			if err != nil {
				slog.Error("tls.Accept", "err", err, "addr", addr)
				return err
			}
			go s.ProcessTcpConn(conn)
		}
	}
}

func (s *Server) Close() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.udpConn != nil {
		s.udpConn.Close()
		s.udpConn = nil
	}
	if s.tcpListener != nil {
		s.tcpListener.Close()
		s.tcpListener = nil
	}
	if s.tlsListener != nil {
		s.tlsListener.Close()
		s.tlsListener = nil
	}
}

// ProcessTcpConn 处理传入的 TCP 连接。
func (s *Server) ProcessTcpConn(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	c := NewTCPConnection(conn)

	parser := newParser()
	defer parser.stop()
	go s.handlerListen(parser.out)

	for {
		var buffer bytes.Buffer
		bodyLen := 0
		for {
			// 读取一行数据，以 '\n' 为结束符
			line, err := reader.ReadBytes('\n')
			if err != nil {
				// logrus.Debugln("tcp conn read error:", err)
				return
			}
			buffer.Write(line)
			if len(line) <= 2 {
				if bodyLen <= 0 {
					break
				}

				bodyBuf := make([]byte, bodyLen)
				n, err := io.ReadFull(reader, bodyBuf)
				if err != nil || n != bodyLen {
					slog.Error(`error while read full`, "err", err)
				}
				buffer.Write(bodyBuf)
				break
			}

			if strings.Contains(string(line), "Content-Length") {
				s := strings.Split(string(line), ":")
				value := strings.Trim(s[len(s)-1], " \r\n")
				bodyLen, err = strconv.Atoi(value)
				if err != nil {
					slog.Error("parse Content-Length failed")
					break
				}
			}
		}

		parser.in <- newPacket(buffer.Bytes(), conn.RemoteAddr(), c)
	}
}

func (s *Server) handlerListen(msgs chan Message) {
	var msg Message
	for {
		msg = <-msgs
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
	tx := s.mustTX(msg)
	// logrus.Traceln("receive request from:", msg.Source(), ",method:", msg.Method(), "txKey:", tx.key, "message: \n", msg.String())

	key := msg.Method()
	if key == MethodMessage || key == MethodNotify {

		if l, ok := msg.ContentLength(); !ok || l.Equals(0) {
			_ = tx.Respond(NewResponseFromRequest("", msg, http.StatusBadRequest, "empty body", nil))
			return
		}
		body := msg.Body()
		var parsed MessageReceive
		if err := XMLDecode(body, &parsed); err != nil {
			_ = tx.Respond(NewResponseFromRequest("", msg, http.StatusBadRequest, "invalid xml", nil))
			return
		}
		key += "-" + parsed.CmdType
	}
	handlers, ok := s.route.Load(strings.ToUpper(key))
	if !ok {
		slog.Debug("not found handler func", "method", msg.Method(), "msg", msg.String())
		go handlerMethodNotAllowed(msg, tx)
		return
	}

	ctx := newContext(msg, tx)
	ctx.handlers = handlers
	ctx.From = s.from
	ctx.svr = s
	go ctx.Next()
}

func (s *Server) handlerResponse(msg *Response) {
	tx := s.getTX(getTXKey(msg))
	if tx == nil {
		// logrus.Infoln("not found tx. receive response from:", msg.Source(), "message: \n", msg.String())
	} else {
		// logrus.Traceln("receive response from:", msg.Source(), "txKey:", tx.key, "message: \n", msg.String())
		tx.receiveResponse(msg)
	}
}

// Request Request
func (s *Server) Request(req *Request) (*Transaction, error) {
	viaHop, ok := req.ViaHop()
	if !ok {
		return nil, fmt.Errorf("missing required 'Via' header")
	}
	viaHop.Host = s.host.String()
	viaHop.Port = s.port
	if viaHop.Params == nil {
		viaHop.Params = NewParams().Add("branch", String{Str: GenerateBranch()})
	}
	if !viaHop.Params.Has("rport") {
		viaHop.Params.Add("rport", nil)
	}

	tx := s.mustTX(req)
	return tx, tx.Request(req)
}

func handlerMethodNotAllowed(req *Request, tx *Transaction) {
	resp := NewResponseFromRequest("", req, http.StatusMethodNotAllowed, http.StatusText(http.StatusMethodNotAllowed), []byte{})
	tx.Respond(resp)
}
