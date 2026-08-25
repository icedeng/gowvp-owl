package sip

import (
	"errors"
	"net"
	"strings"
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

type connectionTestConn struct {
	local    net.Addr
	remote   net.Addr
	closeErr error
}

func (c *connectionTestConn) Read([]byte) (int, error)         { return 0, errors.New("not implemented") }
func (c *connectionTestConn) Write([]byte) (int, error)        { return 0, errors.New("not implemented") }
func (c *connectionTestConn) Close() error                     { return c.closeErr }
func (c *connectionTestConn) LocalAddr() net.Addr              { return c.local }
func (c *connectionTestConn) RemoteAddr() net.Addr             { return c.remote }
func (c *connectionTestConn) SetDeadline(time.Time) error      { return nil }
func (c *connectionTestConn) SetReadDeadline(time.Time) error  { return nil }
func (c *connectionTestConn) SetWriteDeadline(time.Time) error { return nil }
