package sip

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConnectionCloseHandlesMissingRemoteAddress(t *testing.T) {
	base := &connectionTestConn{
		local:    &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5060},
		closeErr: errors.New("close failed"),
	}
	conn := NewUDPConnection(base)
	err := conn.Close()
	if err == nil || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("Close error = %v", err)
	}
}

func TestConnectionPacketOperationsReturnErrorsForStreamConn(t *testing.T) {
	base, peer := net.Pipe()
	defer base.Close()
	defer peer.Close()
	conn := NewUDPConnection(base)
	if _, _, err := conn.ReadFrom(make([]byte, 1)); err == nil {
		t.Fatal("ReadFrom accepted a stream-only connection")
	}
	if _, err := conn.WriteTo([]byte("x"), &net.UDPAddr{}); err == nil {
		t.Fatal("WriteTo accepted a stream-only connection")
	}
}

func TestTCPConnectionWriteToUsesStreamWrite(t *testing.T) {
	base, peer := net.Pipe()
	defer base.Close()
	defer peer.Close()
	conn := NewTCPConnection(base)
	done := make(chan error, 1)
	go func() {
		buffer := make([]byte, 1)
		_, err := peer.Read(buffer)
		done <- err
	}()
	if _, err := conn.WriteTo([]byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTCPConnectionContextWriteInterruptsBlockedWriteAndClearsDeadline(t *testing.T) {
	base, peer := net.Pipe()
	defer base.Close()
	defer peer.Close()
	conn := NewTCPConnection(base).(*connection)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := conn.writeToContext(ctx, []byte("blocked"), peer.LocalAddr()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked TCP write error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked TCP write took %s", elapsed)
	}

	readDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 1)
		_, err := peer.Read(buffer)
		readDone <- err
	}()
	if _, err := conn.WriteTo([]byte("x"), peer.LocalAddr()); err != nil {
		t.Fatalf("TCP write after context timeout = %v", err)
	}
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
}

func TestTCPConnectionContextWriteCancelsWhileWaitingForWriter(t *testing.T) {
	base, peer := net.Pipe()
	observed := &observedWriteConn{Conn: base, started: make(chan struct{})}
	conn := NewTCPConnection(observed).(*connection)
	defer conn.Close()
	defer peer.Close()

	firstDone := make(chan error, 1)
	go func() {
		_, err := conn.WriteTo([]byte("first blocked write"), peer.LocalAddr())
		firstDone <- err
	}()
	select {
	case <-observed.started:
	case <-time.After(time.Second):
		t.Fatal("first TCP write did not start")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := conn.writeToContext(ctx, []byte("second write"), peer.LocalAddr()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued TCP write error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("queued TCP write took %s", elapsed)
	}

	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first TCP write remained blocked after peer close")
	}
}

func TestTransactionRespondContextInterruptsBlockedTCPWrite(t *testing.T) {
	base, peer := net.Pipe()
	defer base.Close()
	defer peer.Close()
	conn := NewTCPConnection(base)
	request := newSignalDigestTestRequest(t, MethodMessage, nil)
	request.SetConnection(conn)
	request.SetSource(conn.RemoteAddr())
	request.SetDestination(conn.LocalAddr())
	response := NewResponseFromRequest("", request, 200, "OK", nil)
	tx := NewTransaction("blocked-response", conn)
	defer tx.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := tx.RespondContext(ctx, response); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked SIP response error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked SIP response took %s", elapsed)
	}
}

func TestSignalingTransportDistinguishesTLSFromTCP(t *testing.T) {
	for _, test := range []struct {
		name string
		wrap func(net.Conn) Connection
		want string
	}{
		{name: "TCP", wrap: NewTCPConnection, want: "TCP"},
		{name: "TLS", wrap: NewTLSConnection, want: "TLS"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, peer := net.Pipe()
			defer base.Close()
			defer peer.Close()
			if got := SignalingTransport(test.wrap(base)); got != test.want {
				t.Fatalf("signaling transport = %q, want %q", got, test.want)
			}
		})
	}
}

type connectionTestConn struct {
	local    net.Addr
	remote   net.Addr
	closeErr error
}

type observedWriteConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *observedWriteConn) Write(payload []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(payload)
}

func (c *connectionTestConn) Read([]byte) (int, error)         { return 0, errors.New("not implemented") }
func (c *connectionTestConn) Write([]byte) (int, error)        { return 0, errors.New("not implemented") }
func (c *connectionTestConn) Close() error                     { return c.closeErr }
func (c *connectionTestConn) LocalAddr() net.Addr              { return c.local }
func (c *connectionTestConn) RemoteAddr() net.Addr             { return c.remote }
func (c *connectionTestConn) SetDeadline(time.Time) error      { return nil }
func (c *connectionTestConn) SetReadDeadline(time.Time) error  { return nil }
func (c *connectionTestConn) SetWriteDeadline(time.Time) error { return nil }
