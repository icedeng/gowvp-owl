package sip

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
				assertSIPFixtureFraming(t, data)
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

func assertSIPFixtureFraming(t *testing.T, data []byte) {
	t.Helper()
	assertCRLF(t, data)
	header, body, ok := bytes.Cut(data, []byte("\r\n\r\n"))
	if !ok {
		t.Fatal("SIP fixture missing CRLF header/body separator")
	}

	contentLength := -1
	for _, line := range strings.Split(string(header), "\r\n") {
		name, value, found := strings.Cut(line, ":")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			continue
		}
		if contentLength >= 0 {
			t.Fatal("SIP fixture contains duplicate Content-Length")
		}
		length, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || length < 0 {
			t.Fatalf("invalid Content-Length %q", value)
		}
		contentLength = length
	}
	if contentLength < 0 {
		t.Fatal("SIP fixture missing Content-Length")
	}
	if contentLength != len(body) {
		t.Fatalf("Content-Length = %d; body bytes = %d", contentLength, len(body))
	}
}

func assertCRLF(t *testing.T, data []byte) {
	t.Helper()
	for i, b := range data {
		if b == '\n' && (i == 0 || data[i-1] != '\r') {
			t.Fatalf("fixture contains bare LF at byte %d", i)
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
