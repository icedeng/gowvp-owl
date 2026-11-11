package sip

import (
	"bufio"
	"bytes"
	"testing"
)

func TestTCPMessageBoundaries(t *testing.T) {
	first := []byte("MESSAGE sip:a@b SIP/2.0\r\nContent-Length: 4\r\n\r\nbody")
	second := []byte("SIP/2.0 200 OK\r\nContent-Length: 0\r\n\r\n")
	r := bufio.NewReader(bytes.NewReader(append(append([]byte{}, first...), second...)))
	a, e := ReadMessage(r)
	if e != nil || !bytes.Equal(a, first) {
		t.Fatal("lost first message", e)
	}
	b, e := ReadMessage(r)
	if e != nil || !bytes.Equal(b, second) {
		t.Fatal("lost second message", e)
	}
	for _, packet := range [][]byte{[]byte("MESSAGE x SIP/2.0\r\nContent-Length: 1\r\nl: 1\r\n\r\nx"), []byte("MESSAGE x SIP/2.0\r\nContent-Length: -1\r\n\r\n"), []byte("MESSAGE x SIP/2.0\r\nContent-Length: 9\r\n\r\nshort"), append(append([]byte{}, second...), second...)} {
		if ValidateDatagram(packet) == nil {
			t.Fatalf("accepted malformed datagram: %q", packet)
		}
	}
}
