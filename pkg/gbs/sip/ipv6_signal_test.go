package sip

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSelectSelfIPPrefersIPv4AndSupportsIPv6Only(t *testing.T) {
	addresses := []net.Addr{
		&net.IPNet{IP: net.ParseIP("2001:db8::20"), Mask: net.CIDRMask(64, 128)},
		&net.IPNet{IP: net.ParseIP("192.0.2.20"), Mask: net.CIDRMask(24, 32)},
	}
	if got, err := selectSelfIP(addresses, 0); err != nil || got.String() != "192.0.2.20" {
		t.Fatalf("dual-stack self IP = %v, %v", got, err)
	}
	if got, err := selectSelfIP(addresses[:1], 0); err != nil || got.String() != "2001:db8::20" {
		t.Fatalf("IPv6-only self IP = %v, %v", got, err)
	}
	if _, err := selectSelfIP(addresses[:1], 4); err == nil {
		t.Fatal("IPv4 selection accepted an IPv6-only address set")
	}
}

func TestSelectSelfIPRejectsUnusableAddresses(t *testing.T) {
	addresses := []net.Addr{
		&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
		&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
		&net.IPNet{IP: net.ParseIP("ff02::1"), Mask: net.CIDRMask(128, 128)},
	}
	if _, err := selectSelfIP(addresses, 0); err == nil {
		t.Fatal("unusable addresses were accepted")
	}
}

func TestIPv6SIPHostSerialization(t *testing.T) {
	port := NewPort(5060)
	uri := &URI{FUser: String{Str: "34020000002000000001"}, FHost: "2001:db8::20", FPort: port}
	if got := uri.String(); got != "sip:34020000002000000001@[2001:db8::20]:5060" {
		t.Fatalf("IPv6 SIP URI = %q", got)
	}
	via := &ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "2001:db8::20", Port: port, Params: NewParams()}
	if got := via.String(); got != "SIP/2.0/UDP [2001:db8::20]:5060" {
		t.Fatalf("IPv6 Via = %q", got)
	}
	if got := via.SentBy(); got != "[2001:db8::20]:5060" {
		t.Fatalf("IPv6 Via sent-by = %q", got)
	}
}

func TestIPv6ViaParsingUsesCanonicalHost(t *testing.T) {
	headers, err := parseViaHeader("Via", "SIP/2.0/UDP [2001:db8::20]:5060;branch=z9hG4bK-ipv6")
	if err != nil {
		t.Fatal(err)
	}
	via, ok := headers[0].(ViaHeader)
	if !ok || len(via) != 1 {
		t.Fatalf("parsed IPv6 Via = %#v", headers)
	}
	if via[0].Host != "2001:db8::20" || via[0].String() != "SIP/2.0/UDP [2001:db8::20]:5060;branch=z9hG4bK-ipv6" {
		t.Fatalf("parsed IPv6 Via hop = host %q, string %q", via[0].Host, via[0].String())
	}
}

func TestIPv4SIPHostSerializationIsUnchanged(t *testing.T) {
	port := NewPort(5060)
	uri := &URI{FUser: String{Str: "34020000002000000001"}, FHost: "192.0.2.20", FPort: port}
	if got := uri.String(); got != "sip:34020000002000000001@192.0.2.20:5060" {
		t.Fatalf("IPv4 SIP URI = %q", got)
	}
	via := &ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "192.0.2.20", Port: port, Params: NewParams()}
	if got := via.String(); got != "SIP/2.0/UDP 192.0.2.20:5060" {
		t.Fatalf("IPv4 Via = %q", got)
	}
}

func TestListenerAdvertiseHostUsesExplicitBoundAddress(t *testing.T) {
	for _, address := range []string{"192.0.2.20", "2001:db8::20"} {
		got, err := listenerAdvertiseHost(net.ParseIP(address), nil)
		if err != nil || got != address {
			t.Fatalf("listener advertise host for %s = %q, %v", address, got, err)
		}
	}
}

func TestFormatSIPHostDoesNotDoubleBracketIPv6(t *testing.T) {
	if got := formatSIPHost("[2001:db8::20]"); got != "[2001:db8::20]" {
		t.Fatalf("bracketed IPv6 host = %q", got)
	}
	if _, err := selectSelfIP(nil, 0); err == nil {
		t.Fatal("empty address set was accepted")
	}
}

func TestRequestFillsViaFromServerAddress(t *testing.T) {
	for _, test := range []struct {
		name, localURI, remoteURI, localIP, remoteIP, wantVia string
	}{
		{"IPv4", "sip:34020000002000000001@192.0.2.20:5060", "sip:34020000001320000001@192.0.2.30:5060", "192.0.2.20", "192.0.2.30", "Via: SIP/2.0/UDP 192.0.2.20:5060;"},
		{"IPv6", "sip:34020000002000000001@[2001:db8::20]:5060", "sip:34020000001320000001@[2001:db8::30]:5060", "2001:db8::20", "2001:db8::30", "Via: SIP/2.0/UDP [2001:db8::20]:5060;"},
	} {
		t.Run(test.name, func(t *testing.T) {
			localURI, err := ParseSipURI(test.localURI)
			if err != nil {
				t.Fatal(err)
			}
			server := NewServer(&Address{URI: &localURI, Params: NewParams()})
			defer server.Close()
			connection := &captureConnection{
				local:  &net.UDPAddr{IP: net.ParseIP(test.localIP), Port: 5060},
				remote: &net.UDPAddr{IP: net.ParseIP(test.remoteIP), Port: 5060},
			}
			server.udpConn = connection
			remoteURI, err := ParseSipURI(test.remoteURI)
			if err != nil {
				t.Fatal(err)
			}
			request := NewRequest("", MethodOptions, &remoteURI, DefaultSipVersion,
				NewHeaderBuilder().SetMethod(MethodOptions).
					SetFrom(&Address{URI: &localURI, Params: NewParams()}).
					SetTo(&Address{URI: &remoteURI, Params: NewParams()}).
					AddVia(&ViaHop{Params: NewParams()}).Build(), nil)
			request.SetConnection(connection)
			request.SetDestination(connection.remote)
			tx, err := server.Request(request)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Close()
			if !strings.Contains(string(connection.payload), test.wantVia) {
				t.Fatalf("%s outbound SIP request = %q", test.name, connection.payload)
			}
		})
	}
}

type captureConnection struct {
	local, remote net.Addr
	payload       []byte
}

func (c *captureConnection) Read([]byte) (int, error)               { return 0, io.EOF }
func (c *captureConnection) Write(payload []byte) (int, error)      { return c.WriteTo(payload, c.remote) }
func (c *captureConnection) Close() error                           { return nil }
func (c *captureConnection) LocalAddr() net.Addr                    { return c.local }
func (c *captureConnection) RemoteAddr() net.Addr                   { return c.remote }
func (c *captureConnection) SetDeadline(time.Time) error            { return nil }
func (c *captureConnection) SetReadDeadline(time.Time) error        { return nil }
func (c *captureConnection) SetWriteDeadline(time.Time) error       { return nil }
func (c *captureConnection) Network() string                        { return "udp" }
func (c *captureConnection) ReadFrom([]byte) (int, net.Addr, error) { return 0, c.remote, io.EOF }
func (c *captureConnection) WriteTo(payload []byte, _ net.Addr) (int, error) {
	c.payload = append([]byte(nil), payload...)
	return len(payload), nil
}
