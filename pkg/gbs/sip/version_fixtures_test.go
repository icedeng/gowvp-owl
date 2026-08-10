package sip

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGBVersionSIPFixturesParse(t *testing.T) {
	versions := []string{"1.0", "1.1", "2.0", "3.0"}
	files := []string{"register-initial.sip", "register-auth.sip", "register-401.sip", "register-200.sip"}
	for _, version := range versions {
		for _, name := range files {
			t.Run(version+"/"+name, func(t *testing.T) {
				path := filepath.Join("..", "testdata", "gb28181", version, name)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				msg := parseFixtureMessage(t, data)
				if msg.SipVersion() != DefaultSipVersion {
					t.Fatalf("SIP version = %q", msg.SipVersion())
				}
				if len(msg.GetHeaders("X-GB-Ver")) != 1 {
					t.Fatal("fixture must contain exactly one X-GB-Ver")
				}
			})
		}
	}
}

func parseFixtureMessage(t *testing.T, data []byte) Message {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	parser := newParser()
	defer parser.stop()
	packet := newPacket(data, &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060}, NewTCPConnection(server))
	select {
	case parser.in <- packet:
	case <-time.After(time.Second):
		t.Fatal("queue SIP fixture timeout")
	}
	select {
	case msg := <-parser.out:
		return msg
	case <-time.After(time.Second):
		t.Fatal("parse SIP fixture timeout")
		return nil
	}
}
