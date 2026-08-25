package sip

import (
	"net"
	"strings"
	"testing"
)

func TestStartUDPServerReturnsBindError(t *testing.T) {
	occupied, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	server := NewServer(&Address{})
	defer server.Close()
	if err := server.StartUDPServer(occupied.LocalAddr().String()); err == nil || !strings.Contains(err.Error(), "net.ListenUDP") {
		t.Fatalf("occupied UDP listener error = %v", err)
	}
}

func TestStartTCPServerReturnsBindError(t *testing.T) {
	occupied, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	server := NewServer(&Address{})
	defer server.Close()
	if err := server.StartTCPServer(occupied.Addr().String()); err == nil || !strings.Contains(err.Error(), "net.ListenTCP") {
		t.Fatalf("occupied TCP listener error = %v", err)
	}
}

func TestStartTLSServerReturnsCertificateError(t *testing.T) {
	server := NewServer(&Address{})
	defer server.Close()
	if err := server.StartTLSServer("127.0.0.1:0", "missing.crt", "missing.key"); err == nil || !strings.Contains(err.Error(), "tls.LoadX509KeyPair") {
		t.Fatalf("invalid TLS certificate error = %v", err)
	}
}

func TestStartedUDPAndTCPServersCloseCleanly(t *testing.T) {
	server := NewServer(&Address{})
	if err := server.StartUDPServer("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if err := server.StartTCPServer("127.0.0.1:0"); err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Close()
}
