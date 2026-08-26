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

func TestServerRequestRejectsUnsupportedSIPVersionAndInvalidVia(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	request := NewRequest("", MethodMessage, &URI{FHost: "example.com"}, "SIP/1.0",
		NewHeaderBuilder().SetMethod(MethodMessage).AddVia(&ViaHop{Params: NewParams()}).Build(), nil)
	request.SetConnection(NewTCPConnection(client))
	request.SetDestination(&net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060})
	if _, err := server.Request(request); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("unsupported outbound SIP version error = %v", err)
	}

	request.SetSipVersion(DefaultSipVersion)
	via, _ := request.ViaHop()
	via.ProtocolVersion = "1.0"
	if _, err := server.Request(request); err == nil || !strings.Contains(err.Error(), "Via") {
		t.Fatalf("invalid outbound Via error = %v", err)
	}
}
