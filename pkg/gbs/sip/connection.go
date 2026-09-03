package sip

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"
)

// Packet Packet
type Packet struct {
	reader     *bufio.Reader
	raddr      net.Addr
	bodylength int
	conn       Connection
}

func newPacket(data []byte, raddr net.Addr, conn Connection) Packet {
	slog.Debug("receive new packet,from:", "raddr", addrString(raddr), "data", string(data))
	logTraffic("in", conn.Network(), raddr, conn.LocalAddr(), data)
	return Packet{
		reader:     bufio.NewReader(bytes.NewReader(data)),
		raddr:      raddr,
		bodylength: getBodyLength(data),
		conn:       conn,
	}
}

func (p *Packet) nextLine() (string, error) {
	str, err := p.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	// Trim the newline characters
	str = strings.TrimSuffix(str, "\r\n")
	str = strings.TrimSuffix(str, "\n")
	return str, nil
}

func (p *Packet) bodyLength() int {
	return p.bodylength
}

func (p *Packet) getBody() ([]byte, error) {
	if p.bodyLength() < 1 {
		return []byte{}, nil
	}
	if p.bodylength > maxSIPBodyBytes {
		return nil, fmt.Errorf("SIP body exceeds %d bytes", maxSIPBodyBytes)
	}
	body := make([]byte, p.bodylength)
	if p.bodylength > 0 {
		n, err := io.ReadFull(p.reader, body)
		if err != nil {
			return body[:n], fmt.Errorf("read SIP body: got %d of %d bytes: %w", n, p.bodylength, err)
		}
	}
	return body, nil
}

// Connection Wrapper around net.Conn.
type Connection interface {
	net.Conn
	Network() string
	// String() string
	ReadFrom(buf []byte) (num int, raddr net.Addr, err error)
	WriteTo(buf []byte, raddr net.Addr) (num int, err error)
}

type signalingTransportConnection interface {
	SignalingTransport() string
}

// Connection implementation.
type connection struct {
	baseConn  net.Conn
	laddr     net.Addr
	raddr     net.Addr
	writeGate chan struct{}
	logKey    string
	packet    bool
	transport string
}

func newConnectionWriteGate() chan struct{} {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return gate
}

func NewUDPConnection(baseConn net.Conn) Connection {
	conn := &connection{
		baseConn:  baseConn,
		laddr:     baseConn.LocalAddr(),
		raddr:     baseConn.RemoteAddr(),
		writeGate: newConnectionWriteGate(),
		logKey:    "udp ",
		packet:    true,
		transport: "UDP",
	}
	return conn
}

func NewTCPConnection(baseConn net.Conn) Connection {
	conn := &connection{
		baseConn:  baseConn,
		laddr:     baseConn.LocalAddr(),
		raddr:     baseConn.RemoteAddr(),
		writeGate: newConnectionWriteGate(),
		logKey:    "tcp ",
		transport: "TCP",
	}
	return conn
}

// NewTLSConnection 包装 SIP/TLS 流连接，同时保留底层 Network() 的 TCP 套接字语义。
func NewTLSConnection(baseConn net.Conn) Connection {
	conn := NewTCPConnection(baseConn).(*connection)
	conn.logKey = "tls "
	conn.transport = "TLS"
	return conn
}

// SignalingTransport 返回连接实际承载的 SIP 传输类型。
func SignalingTransport(conn Connection) string {
	if conn == nil {
		return ""
	}
	if value, ok := conn.(signalingTransportConnection); ok {
		return strings.ToUpper(strings.TrimSpace(value.SignalingTransport()))
	}
	network := strings.ToLower(strings.TrimSpace(conn.Network()))
	switch {
	case strings.HasPrefix(network, "tls"):
		return "TLS"
	case strings.HasPrefix(network, "tcp"):
		return "TCP"
	case strings.HasPrefix(network, "udp"):
		return "UDP"
	default:
		return strings.ToUpper(network)
	}
}

func (conn *connection) SignalingTransport() string { return conn.transport }

func (conn *connection) Read(buf []byte) (int, error) {
	var (
		num int
		err error
	)

	num, err = conn.baseConn.Read(buf)
	if err != nil {
		return num, err
		//  NewError(err, conn.logKey, "read", conn.baseConn.LocalAddr().String())
	}
	return num, err
}

func (conn *connection) ReadFrom(buf []byte) (num int, raddr net.Addr, err error) {
	if !conn.packet {
		return 0, nil, fmt.Errorf("%sconnection does not support packet reads", conn.logKey)
	}
	packetConn, ok := conn.baseConn.(net.PacketConn)
	if !ok {
		return 0, nil, fmt.Errorf("%sconnection does not support packet reads", conn.logKey)
	}
	num, raddr, err = packetConn.ReadFrom(buf)
	if err != nil {
		return num, raddr, err
		//  NewError(err, conn.logKey, "readfrom", conn.baseConn.LocalAddr().String(), raddr.String())
	}
	// logrus.Tracef("readFrom %d , %s -> %s \n %s", num, raddr, conn.LocalAddr(), string(buf[:num]))
	return num, raddr, err
}

func (conn *connection) Write(buf []byte) (int, error) {
	if conn.packet {
		return conn.writeLocked(buf)
	}
	if err := conn.lockWrite(context.Background()); err != nil {
		return 0, err
	}
	defer conn.unlockWrite()
	return conn.writeLocked(buf)
}

func (conn *connection) writeLocked(buf []byte) (int, error) {
	var (
		num int
		err error
	)

	num, err = conn.baseConn.Write(buf)
	if err != nil {
		return num, err
		//  NewError(err, conn.logKey, "write", conn.baseConn.LocalAddr().String())
	}
	return num, err
}

func (conn *connection) WriteTo(buf []byte, raddr net.Addr) (num int, err error) {
	if conn.packet {
		return conn.writeToLocked(buf, raddr)
	}
	if err := conn.lockWrite(context.Background()); err != nil {
		return 0, err
	}
	defer conn.unlockWrite()
	return conn.writeToLocked(buf, raddr)
}

func (conn *connection) lockWrite(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.writeGate:
		return nil
	}
}

func (conn *connection) unlockWrite() {
	conn.writeGate <- struct{}{}
}

func (conn *connection) writeToLocked(buf []byte, raddr net.Addr) (num int, err error) {
	if !conn.packet {
		num, err = conn.baseConn.Write(buf)
	} else {
		packetConn, ok := conn.baseConn.(net.PacketConn)
		if !ok {
			return 0, fmt.Errorf("%sconnection does not support packet writes", conn.logKey)
		}
		num, err = packetConn.WriteTo(buf, raddr)
	}
	if err != nil {
		return num, err
		//  NewError(err, conn.logKey, "writeTo", conn.baseConn.LocalAddr().String(), raddr.String())
	}
	// logrus.Tracef("writeTo %d , %s -> %s \n %s", num, conn.baseConn.LocalAddr(), raddr.String(), string(buf[:num]))
	return num, err
}

func (conn *connection) writeToContext(ctx context.Context, buf []byte, raddr net.Addr) (num int, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if conn.packet {
		return conn.writeToLocked(buf, raddr)
	}
	if err := conn.lockWrite(ctx); err != nil {
		return 0, err
	}
	defer conn.unlockWrite()

	deadline := time.Time{}
	if value, ok := ctx.Deadline(); ok {
		deadline = value
	}
	if err := conn.baseConn.SetWriteDeadline(deadline); err != nil {
		return 0, err
	}
	interruptDone := make(chan struct{})
	stopInterrupt := context.AfterFunc(ctx, func() {
		_ = conn.baseConn.SetWriteDeadline(time.Now())
		close(interruptDone)
	})
	num, writeErr := conn.writeToLocked(buf, raddr)
	if !stopInterrupt() {
		<-interruptDone
	}
	clearErr := conn.baseConn.SetWriteDeadline(time.Time{})
	// 对端可能在完整接收请求并回送最终响应后立即关闭连接。此时写入已经成功，
	// 清理已关闭连接的 deadline 失败不应把已送达请求改判为发送失败。
	if writeErr == nil && (errors.Is(clearErr, net.ErrClosed) || errors.Is(clearErr, io.ErrClosedPipe)) {
		clearErr = nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return num, errors.Join(ctxErr, writeErr, clearErr)
	}
	var timeoutErr net.Error
	if !deadline.IsZero() && !time.Now().Before(deadline) && errors.As(writeErr, &timeoutErr) && timeoutErr.Timeout() {
		return num, errors.Join(context.DeadlineExceeded, writeErr, clearErr)
	}
	return num, errors.Join(writeErr, clearErr)
}

func (conn *connection) LocalAddr() net.Addr {
	return conn.baseConn.LocalAddr()
}

func (conn *connection) RemoteAddr() net.Addr {
	return conn.baseConn.RemoteAddr()
}

func (conn *connection) Close() error {
	local := conn.baseConn.LocalAddr()
	remote := conn.baseConn.RemoteAddr()
	err := conn.baseConn.Close()
	if err != nil {
		return NewError(err, conn.logKey, "close", addrString(local), addrString(remote))
	}
	return nil
}

func (conn *connection) Network() string {
	if local := conn.baseConn.LocalAddr(); local != nil {
		return local.Network()
	}
	return ""
}

func (conn *connection) SetDeadline(t time.Time) error {
	return conn.baseConn.SetDeadline(t)
}

func (conn *connection) SetReadDeadline(t time.Time) error {
	return conn.baseConn.SetReadDeadline(t)
}

func (conn *connection) SetWriteDeadline(t time.Time) error {
	return conn.baseConn.SetWriteDeadline(t)
}
