package protocol

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

const testID = "b8f73992-d91b-4b63-bd2e-7ce6497e8d25"

func helloJSON() string {
	return `{"version":1,"type":"hello","kind":"request","request_id":"` + testID + `","gateway_id":"b1","platform_id":"a","generation":null,"connection_epoch":null,"owner_epoch":"7","payload":{"client_instance_id":"` + testID + `","supported_versions":[1],"required_capabilities":["sip_udp"],"last_applied_revision":"0"}}`
}
func TestStrictEnvelopeBoundaries(t *testing.T) {
	if _, _, err := DecodeEnvelope([]byte(helloJSON())); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"duplicate envelope":    strings.Replace(helloJSON(), `"version":1`, `"version":1,"version":1`, 1),
		"duplicate payload":     strings.Replace(helloJSON(), `"supported_versions":[1]`, `"supported_versions":[1],"supported_versions":[1]`, 1),
		"unknown payload":       strings.Replace(helloJSON(), `"supported_versions":[1]`, `"supported_versions":[1],"hidden":true`, 1),
		"missing payload field": strings.Replace(helloJSON(), `"supported_versions":[1],`, "", 1),
		"integer precision":     strings.Replace(helloJSON(), `"owner_epoch":"7"`, `"owner_epoch":7`, 1),
		"leading zero":          strings.Replace(helloJSON(), `"owner_epoch":"7"`, `"owner_epoch":"07"`, 1),
		"overflow":              strings.Replace(helloJSON(), `"owner_epoch":"7"`, `"owner_epoch":"9223372036854775808"`, 1),
		"wrong epoch":           strings.Replace(helloJSON(), `"generation":null`, `"generation":"`+testID+`"`, 1),
		"wrong kind":            strings.Replace(helloJSON(), `"kind":"request"`, `"kind":"event"`, 1),
		"null array":            strings.Replace(helloJSON(), `"supported_versions":[1]`, `"supported_versions":null`, 1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, e := DecodeEnvelope([]byte(data)); e == nil {
				t.Fatal("accepted invalid envelope")
			}
		})
	}
}
func TestJCSVectors(t *testing.T) {
	var value any
	d := json.NewDecoder(strings.NewReader(`{"numbers":[333333333.33333329,1E30,4.50,2e-3,0.000000000000000000000000001,-0],"string":"€$\u000f\nA'B\"\\\"/","literals":[null,true,false]}`))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		t.Fatal(err)
	}
	actual, err := Canonical(value)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27,0],"string":"€$\u000f\nA'B\"\\\"/"}`
	if string(actual) != expected {
		t.Fatalf("got %s", actual)
	}
	actual, err = Canonical(map[string]any{"\ue000": 1, "😀": 2, "\r": 3})
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != `{"\r":3,"😀":2,"":1}` {
		t.Fatalf("UTF16 sorting: %s", actual)
	}
}
func TestInvalidUnicode(t *testing.T) {
	for _, data := range []string{`{"x":"\ud800"}`, `{"x":"\udfff"}`, `{"x":"\ud800x"}`, "{\"x\":\"\xff\"}"} {
		var v map[string]any
		if err := DecodeStrict([]byte(data), &v); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
}
func TestConfigAndBusinessHash(t *testing.T) {
	c := DefaultEffectiveConfig()
	c.PublicIP = "203.0.113.1"
	c.SIPPort = 15060
	c.MediaPortStart = 30000
	c.MediaPortEnd = 30199
	c.SourceCIDRs = []string{"203.0.113.0/24", "192.0.2.0/24"}
	h1, e := ConfigHash(c)
	if e != nil {
		t.Fatal(e)
	}
	c.SourceCIDRs[0], c.SourceCIDRs[1] = c.SourceCIDRs[1], c.SourceCIDRs[0]
	h2, _ := ConfigHash(c)
	if h1 != h2 {
		t.Fatal("CIDR set hash depends on order")
	}
	a, _ := BusinessHash(map[string]any{"operation_id": "one", "source_ips": []string{"203.0.113.1", "203.0.113.2"}})
	b, _ := BusinessHash(map[string]any{"operation_id": "two", "source_ips": []string{"203.0.113.2", "203.0.113.1"}})
	if a != b {
		t.Fatal("operation identity included in business hash")
	}
	c.LeaseTTLMS = 59999
	if ValidateConfig(c) == nil {
		t.Fatal("accepted lease shorter than heartbeat timeout+interval")
	}
}
func TestBinarySequenceEOFAndFraming(t *testing.T) {
	packet := make([]byte, 12)
	packet[0] = 0x80
	packet[1] = 96
	binary.BigEndian.PutUint32(packet[8:], 1234)
	ssrc := "0000001234"
	state := BinaryState{WireMode: "tcp_passive", Direction: "device_to_a", ToA: true}
	if e := state.Accept(BinaryMessage{Kind: TCPBytes, Sequence: 2, Payload: packet}); e == nil {
		t.Fatal("accepted skipped sequence")
	}
	if e := state.Accept(BinaryMessage{Kind: TCPBytes, Sequence: 1, Payload: packet}); e != nil {
		t.Fatal(e)
	}
	if e := state.Accept(BinaryMessage{Kind: EOF, Sequence: 2}); e != nil {
		t.Fatal(e)
	}
	if e := state.Accept(BinaryMessage{Kind: TCPBytes, Sequence: 3, Payload: packet}); e == nil {
		t.Fatal("accepted bytes after EOF")
	}
	frame := append([]byte{0, 12}, packet...)
	parser := RTPFramer{SSRC: &ssrc, AllowRTP: true}
	for i, b := range frame {
		out, e := parser.Feed([]byte{b})
		if e != nil {
			t.Fatal(e)
		}
		if i < len(frame)-1 && len(out) > 0 {
			t.Fatal("forwarded unvalidated partial frame")
		}
		if i == len(frame)-1 && string(out) != string(frame) {
			t.Fatal("changed RFC4571 frame")
		}
	}
	parser = RTPFramer{SSRC: &ssrc, AllowRTP: true}
	frame[len(frame)-1]++
	if _, e := parser.Feed(frame); e == nil {
		t.Fatal("accepted wrong SSRC")
	}
	udp := BinaryState{WireMode: "udp", Direction: "device_to_a", ToA: true}
	if e := udp.Accept(BinaryMessage{Kind: EOF, Sequence: 1}); e == nil {
		t.Fatal("accepted UDP EOF")
	}
}

func TestPublicGatewayAddressPolicy(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "0.1.2.3", "192.0.2.1", "198.18.0.1", "::1", "::ffff:127.0.0.1", "fc00::1", "64:ff9b::a00:1", "2002:7f00:1::1", "2001:db8::1"} {
		if ValidPublicIP(ip) {
			t.Fatalf("accepted non-public gateway address %s", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !ValidPublicIP(ip) {
			t.Fatalf("rejected public IP %s", ip)
		}
	}
}
