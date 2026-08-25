package sip

import (
	"net"
	"strings"
	"testing"
)

func TestServerRequestRejectsIncompleteTransport(t *testing.T) {
	server := NewServer(&Address{})
	defer server.Close()
	request := NewRequest("", MethodMessage, &URI{FHost: "example.com"}, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodMessage).AddVia(&ViaHop{Params: NewParams()}).Build(), nil)
	if _, err := server.Request(request); err == nil || !strings.Contains(err.Error(), "connection") {
		t.Fatalf("missing connection error = %v", err)
	}

	base, peer := net.Pipe()
	defer base.Close()
	defer peer.Close()
	request.SetConnection(NewTCPConnection(base))
	if _, err := server.Request(request); err == nil || !strings.Contains(err.Error(), "destination") {
		t.Fatalf("missing destination error = %v", err)
	}
}
