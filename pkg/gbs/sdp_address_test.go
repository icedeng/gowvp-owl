package gbs

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/sms"
)

func TestParseSDPAddress(t *testing.T) {
	tests := []struct {
		name, input, addressType, canonical string
	}{
		{"IPv4", " 192.0.2.20 ", "IP4", "192.0.2.20"},
		{"IPv6", "2001:0db8:0:0::20", "IP6", "2001:db8::20"},
		{"bracketed IPv6", "[2001:db8::20]", "IP6", "2001:db8::20"},
		{"IPv4-mapped IPv6", "::ffff:192.0.2.20", "IP4", "192.0.2.20"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseSDPAddress(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got.Type != test.addressType || got.Canonical != test.canonical || !got.IP.Equal(net.ParseIP(test.canonical)) {
				t.Fatalf("parseSDPAddress(%q) = %+v", test.input, got)
			}
		})
	}
	for _, input := range []string{"", "host.example", "2001:db8::20%en0", "192.0.2.999"} {
		if _, err := parseSDPAddress(input); err == nil {
			t.Fatalf("parseSDPAddress(%q) error = nil", input)
		}
	}
}

func TestValidateSDPConnectionAddressRejectsTypeMismatch(t *testing.T) {
	if err := validateSDPConnectionAddress("IN", "IP6", net.ParseIP("192.0.2.20")); err == nil {
		t.Fatal("IPv4 address with IP6 type was accepted")
	}
	if err := validateSDPConnectionAddress("IN", "IP4", net.ParseIP("2001:db8::20")); err == nil {
		t.Fatal("IPv6 address with IP4 type was accepted")
	}
	if err := validateSDPConnectionAddress("IN", "IP6", net.ParseIP("2001:db8::20")); err != nil {
		t.Fatalf("valid IPv6 connection rejected: %v", err)
	}
}

func TestGetIPAcceptsLiteralIPv4AndIPv6(t *testing.T) {
	tests := map[string]string{
		" 192.0.2.20 ":      "192.0.2.20",
		"2001:0db8:0:0::20": "2001:db8::20",
		"[2001:db8::20]":    "2001:db8::20",
		"::ffff:192.0.2.20": "192.0.2.20",
	}
	for input, want := range tests {
		got, err := GetIP(input)
		if err != nil || got != want {
			t.Fatalf("GetIP(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestBuildGBSDPIPv6AcrossFourProfilesAndModes(t *testing.T) {
	versions := []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30}
	modes := []string{historyModePlay, historyModePlayback, historyModeDownload}
	for _, version := range versions {
		for _, mode := range modes {
			t.Run(string(version)+"/"+mode, func(t *testing.T) {
				input := gbSDPInput{
					Version: version, SessionName: mode, ChannelID: gb10ChannelID,
					IP: "2001:db8::20", Port: 30000, SSRC: "0100000001",
				}
				if mode != historyModePlay {
					input.URI = gb10ChannelID + ":0"
					input.StartAt = time.Unix(1711929600, 0)
					input.EndAt = time.Unix(1711933200, 0)
					input.SSRC = "1100000001"
				}
				body, err := buildGBSDP(input)
				if err != nil {
					t.Fatal(err)
				}
				text := string(body)
				if !strings.Contains(text, "o="+gb10ChannelID+" 0 0 IN IP6 2001:db8::20\r\n") ||
					!strings.Contains(text, "c=IN IP6 2001:DB8::20\r\n") {
					t.Fatalf("IPv6 SDP address mismatch:\n%s", text)
				}
			})
		}
	}
}

func TestBuildIPv6VoiceAndCascadeSDP(t *testing.T) {
	server := &sms.MediaServer{SDPIP: "2001:db8::20"}
	tests := []struct {
		name  string
		build func() ([]byte, error)
	}{
		{"voice", func() ([]byte, error) { return buildVoiceSDP(gb10ChannelID, server.SDPIP, 30000, 0, "0100000001") }},
		{"broadcast answer", func() ([]byte, error) {
			return buildBroadcastSDPAnswer(&broadcastSession{SMS: server, SourceID: gb10ChannelID}, 30000, 96, "PS/90000", "0100000001")
		}},
		{"cascade voice", func() ([]byte, error) {
			return buildCascadeVoiceReceiveSDP(gb10DeviceID, server, GBVersion11, 30000, "0100000001")
		}},
		{"cascade video", func() ([]byte, error) {
			return buildCascadeSDPAnswer(gb10DeviceID, server, &cascadeVideoOffer{Payload: 96, Protocol: "RTP/AVP", IsUDP: true, SSRC: "0100000001"}, 30000)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := test.build()
			if err != nil {
				t.Fatal(err)
			}
			if text := string(body); !strings.Contains(text, " IN IP6 2001:db8::20\r\n") || !strings.Contains(text, "c=IN IP6 2001:DB8::20\r\n") {
				t.Fatalf("IPv6 SDP address mismatch:\n%s", text)
			}
		})
	}
}

func TestParseIPv6InboundMediaSDP(t *testing.T) {
	platform := cascadePlatform{remote: &net.TCPAddr{IP: net.ParseIP("2001:db8::30"), Port: 5060}}
	videoBody := []byte("v=0\r\no=x 0 0 IN IP6 2001:db8::30\r\ns=Play\r\nc=IN IP6 2001:db8::30\r\nt=0 0\r\nm=video 30000 RTP/AVP 96\r\na=recvonly\r\na=rtpmap:96 PS/90000\r\ny=0100000001\r\n")
	video, err := parseCascadeVideoOffer(videoBody, GBVersion10, platform)
	if err != nil {
		t.Fatal(err)
	}
	if !video.RemoteIP.Equal(net.ParseIP("2001:db8::30")) || video.Port != 30000 {
		t.Fatalf("IPv6 cascade offer = %+v", video)
	}

	audioBody := []byte("v=0\r\no=x 0 0 IN IP6 2001:db8::30\r\ns=Play\r\nc=IN IP6 2001:db8::30\r\nt=0 0\r\nm=audio 30002 RTP/AVP 96\r\na=recvonly\r\na=rtpmap:96 PS/90000\r\n")
	audio, err := parseBroadcastSDPOffer(audioBody, GBVersion11)
	if err != nil {
		t.Fatal(err)
	}
	if !audio.RemoteIP.Equal(net.ParseIP("2001:db8::30")) || audio.Port != 30002 {
		t.Fatalf("IPv6 Broadcast offer = %+v", audio)
	}
}
