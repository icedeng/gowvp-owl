package sip

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProcessTCPConnFramesCaseInsensitiveAndCompactContentLength(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &localURI, Params: NewParams()})
	defer server.Close()
	bodies := make(chan string, 2)
	server.Message().Handle("Keepalive", func(ctx *Context) {
		bodies <- string(ctx.Request.Body())
	})

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	go server.ProcessTcpConn(serverConn)

	firstBody := "<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>34020000001320000001</DeviceID><Status>OK</Status></Notify>"
	secondBody := "<Notify><CmdType> Keepalive </CmdType><SN>2</SN><DeviceID>34020000001320000001</DeviceID><Status>OK</Status></Notify>"
	first := tcpTestRequest("tcp-case-insensitive", 1, "content-length", firstBody)
	second := tcpTestRequest("tcp-compact", 2, "l", secondBody)
	if _, err := clientConn.Write([]byte(first + second)); err != nil {
		t.Fatal(err)
	}

	gotBodies := make(map[string]bool, 2)
	for range 2 {
		select {
		case got := <-bodies:
			gotBodies[got] = true
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for TCP SIP bodies")
		}
	}
	if !gotBodies[firstBody] || !gotBodies[secondBody] || len(gotBodies) != 2 {
		t.Fatalf("TCP SIP bodies = %#v", gotBodies)
	}
}

func TestProcessTCPConnValidatesMANSCDPContentTypeBeforeRouting(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	body := "<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>34020000001320000001</DeviceID><Status>OK</Status></Notify>"
	tests := []struct {
		name        string
		contentType string
		wantStatus  string
		wantHandled bool
	}{
		{name: "valid parameterized type", contentType: "Content-Type: application/manscdp+xml; charset=UTF-8\r\n", wantStatus: "SIP/2.0 200", wantHandled: true},
		{name: "missing type", wantStatus: "SIP/2.0 400"},
		{name: "wrong type", contentType: "Content-Type: application/sdp\r\n", wantStatus: "SIP/2.0 400"},
		{name: "duplicate type", contentType: "Content-Type: Application/MANSCDP+xml\r\nContent-Type: Application/MANSCDP+xml\r\n", wantStatus: "SIP/2.0 400"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer(&Address{URI: &localURI, Params: NewParams()})
			defer server.Close()
			handled := make(chan struct{}, 1)
			server.Message().Handle("Keepalive", func(ctx *Context) {
				handled <- struct{}{}
				ctx.String(200, "OK")
			})

			serverPipe, clientConn := net.Pipe()
			defer clientConn.Close()
			serverConn := &sipTestTCPConn{
				Conn: serverPipe, local: &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
				remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
			}
			go server.ProcessTcpConn(serverConn)
			request := fmt.Sprintf("MESSAGE sip:34020000002000000001@127.0.0.1 SIP/2.0\r\n"+
				"Via: SIP/2.0/TCP 127.0.0.1:5061;branch=z9hG4bK-content-type-%d\r\n"+
				"From: <sip:34020000001320000001@127.0.0.1>;tag=content-type-%d\r\n"+
				"To: <sip:34020000002000000001@127.0.0.1>\r\n"+
				"Call-ID: content-type-%d\r\nCSeq: 1 MESSAGE\r\nMax-Forwards: 70\r\n%sContent-Length: %d\r\n\r\n%s",
				index, index, index, test.contentType, len(body), body)
			if _, err := clientConn.Write([]byte(request)); err != nil {
				t.Fatal(err)
			}
			if err := clientConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			status, err := bufio.NewReader(clientConn).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(status, test.wantStatus) {
				t.Fatalf("content type response = %q, want %q", status, test.wantStatus)
			}
			select {
			case <-handled:
				if !test.wantHandled {
					t.Fatal("handler ran for invalid Content-Type")
				}
			default:
				if test.wantHandled {
					t.Fatal("handler did not run for valid Content-Type")
				}
			}
		})
	}
}

func TestProcessTCPConnRejectsMissingRequiredRoutingHeaders(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name    string
		headers string
	}{
		{
			name: "From",
			headers: "Via: SIP/2.0/TCP 127.0.0.1:5061;branch=z9hG4bK-missing-from\r\n" +
				"To: <sip:34020000002000000001@127.0.0.1>\r\n" +
				"Call-ID: missing-from\r\nCSeq: 1 OPTIONS\r\n",
		},
		{
			name: "Via",
			headers: "From: <sip:34020000001320000001@127.0.0.1>;tag=from-missing-via\r\n" +
				"To: <sip:34020000002000000001@127.0.0.1>\r\n" +
				"Call-ID: missing-via\r\nCSeq: 1 OPTIONS\r\n",
		},
		{
			name: "To",
			headers: "Via: SIP/2.0/TCP 127.0.0.1:5061;branch=z9hG4bK-missing-to\r\n" +
				"From: <sip:34020000001320000001@127.0.0.1>;tag=from-missing-to\r\n" +
				"Call-ID: missing-to\r\nCSeq: 1 OPTIONS\r\n",
		},
		{
			name: "Call-ID",
			headers: "Via: SIP/2.0/TCP 127.0.0.1:5061;branch=z9hG4bK-missing-call-id\r\n" +
				"From: <sip:34020000001320000001@127.0.0.1>;tag=from-missing-call-id\r\n" +
				"To: <sip:34020000002000000001@127.0.0.1>\r\nCSeq: 1 OPTIONS\r\n",
		},
		{
			name: "CSeq",
			headers: "Via: SIP/2.0/TCP 127.0.0.1:5061;branch=z9hG4bK-missing-cseq\r\n" +
				"From: <sip:34020000001320000001@127.0.0.1>;tag=from-missing-cseq\r\n" +
				"To: <sip:34020000002000000001@127.0.0.1>\r\nCall-ID: missing-cseq\r\n",
		},
		{
			name: "mismatched CSeq method",
			headers: "Via: SIP/2.0/TCP 127.0.0.1:5061;branch=z9hG4bK-mismatched-cseq\r\n" +
				"From: <sip:34020000001320000001@127.0.0.1>;tag=from-mismatched-cseq\r\n" +
				"To: <sip:34020000002000000001@127.0.0.1>\r\n" +
				"Call-ID: mismatched-cseq\r\nCSeq: 1 BYE\r\n",
		},
		{
			name: "duplicate From",
			headers: "Via: SIP/2.0/TCP 127.0.0.1:5061;branch=z9hG4bK-duplicate-from\r\n" +
				"From: <sip:34020000001320000001@127.0.0.1>;tag=duplicate-from-1\r\n" +
				"From: <sip:34020000001320000002@127.0.0.1>;tag=duplicate-from-2\r\n" +
				"To: <sip:34020000002000000001@127.0.0.1>\r\n" +
				"Call-ID: duplicate-from\r\nCSeq: 1 OPTIONS\r\n",
		},
		{
			name: "duplicate To",
			headers: "Via: SIP/2.0/TCP 127.0.0.1:5061;branch=z9hG4bK-duplicate-to\r\n" +
				"From: <sip:34020000001320000001@127.0.0.1>;tag=duplicate-to\r\n" +
				"To: <sip:34020000002000000001@127.0.0.1>\r\n" +
				"To: <sip:34020000002000000002@127.0.0.1>\r\n" +
				"Call-ID: duplicate-to\r\nCSeq: 1 OPTIONS\r\n",
		},
		{
			name: "duplicate Call-ID",
			headers: "Via: SIP/2.0/TCP 127.0.0.1:5061;branch=z9hG4bK-duplicate-call-id\r\n" +
				"From: <sip:34020000001320000001@127.0.0.1>;tag=duplicate-call-id\r\n" +
				"To: <sip:34020000002000000001@127.0.0.1>\r\n" +
				"Call-ID: duplicate-call-id-1\r\nCall-ID: duplicate-call-id-2\r\nCSeq: 1 OPTIONS\r\n",
		},
		{
			name: "duplicate CSeq",
			headers: "Via: SIP/2.0/TCP 127.0.0.1:5061;branch=z9hG4bK-duplicate-cseq\r\n" +
				"From: <sip:34020000001320000001@127.0.0.1>;tag=duplicate-cseq\r\n" +
				"To: <sip:34020000002000000001@127.0.0.1>\r\n" +
				"Call-ID: duplicate-cseq\r\nCSeq: 1 OPTIONS\r\nCSeq: 2 OPTIONS\r\n",
		},
		{
			name: "malformed Record-Route",
			headers: "Via: SIP/2.0/TCP 127.0.0.1:5061;branch=z9hG4bK-malformed-record-route\r\n" +
				"From: <sip:34020000001320000001@127.0.0.1>;tag=malformed-record-route\r\n" +
				"To: <sip:34020000002000000001@127.0.0.1>\r\n" +
				"Call-ID: malformed-record-route\r\nCSeq: 1 OPTIONS\r\n" +
				"Record-Route: <sip:broken.example.com\r\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer(&Address{URI: &localURI, Params: NewParams()})
			defer server.Close()
			handled := make(chan struct{}, 1)
			server.Handle(MethodOptions, func(*Context) { handled <- struct{}{} })

			serverPipe, clientConn := net.Pipe()
			defer clientConn.Close()
			serverConn := &sipTestTCPConn{
				Conn:   serverPipe,
				local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
				remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
			}
			go server.ProcessTcpConn(serverConn)
			request := "OPTIONS sip:34020000002000000001@127.0.0.1 SIP/2.0\r\n" + test.headers + "Content-Length: 0\r\n\r\n"
			if _, err := clientConn.Write([]byte(request)); err != nil {
				t.Fatal(err)
			}
			if err := clientConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			status, err := bufio.NewReader(clientConn).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(status, "SIP/2.0 400") {
				t.Fatalf("malformed request status = %q", status)
			}
			select {
			case <-handled:
				t.Fatal("handler ran for malformed request")
			default:
			}
		})
	}
}

func TestProcessTCPConnRejectsUnsupportedSIPVersion(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &localURI, Params: NewParams()})
	defer server.Close()
	serverPipe, clientConn := net.Pipe()
	defer clientConn.Close()
	serverConn := &sipTestTCPConn{
		Conn: serverPipe, local: &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
	}
	go server.ProcessTcpConn(serverConn)
	request := "OPTIONS sip:34020000002000000001@127.0.0.1 SIP/1.0\r\n" +
		"Via: SIP/1.0/TCP 127.0.0.1:5061;branch=z9hG4bK-old-version\r\n" +
		"From: <sip:34020000001320000001@127.0.0.1>;tag=old-version\r\n" +
		"To: <sip:34020000002000000001@127.0.0.1>\r\n" +
		"Call-ID: old-version\r\nCSeq: 1 OPTIONS\r\nContent-Length: 0\r\n\r\n"
	if _, err := clientConn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	if err := clientConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	status, err := bufio.NewReader(clientConn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "SIP/2.0 505 Version Not Supported") {
		t.Fatalf("unsupported SIP version response = %q", status)
	}
}

func TestParserStopClosesHandlerOutput(t *testing.T) {
	parser := newParser()
	handlerDone := make(chan struct{})
	server := NewServer(&Address{})
	defer server.Close()
	go func() {
		server.handlerListen(parser.out)
		close(handlerDone)
	}()

	parser.stop()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("SIP parser handler did not stop after parser shutdown")
	}
}

func TestParserFinishDrainsAcceptedPacket(t *testing.T) {
	parser := newParser()
	message := "SIP/2.0 200 OK\r\n" +
		"Via: SIP/2.0/TCP 192.0.2.10:5060;branch=z9hG4bK-drain\r\n" +
		"From: <sip:34020000001320000001@192.0.2.10>;tag=drain\r\n" +
		"To: <sip:34020000002000000001@192.0.2.20>\r\n" +
		"Call-ID: drain-response\r\n" +
		"CSeq: 1 MESSAGE\r\n" +
		"Content-Length: 0\r\n\r\n"
	base, peer := net.Pipe()
	defer base.Close()
	defer peer.Close()
	connection := NewTCPConnection(base)

	accepted := make(chan struct{})
	go func() {
		parser.in <- newPacket([]byte(message), connection.RemoteAddr(), connection)
		close(accepted)
	}()
	<-accepted
	parser.finish()

	select {
	case parsed, ok := <-parser.out:
		if !ok || parsed == nil {
			t.Fatal("SIP parser discarded an accepted packet while draining")
		}
		response, ok := parsed.(*Response)
		if !ok || response.StatusCode() != 200 {
			t.Fatalf("drained SIP message = %#v", parsed)
		}
	case <-time.After(time.Second):
		t.Fatal("SIP parser did not drain an accepted packet")
	}
	select {
	case _, ok := <-parser.out:
		if ok {
			t.Fatal("SIP parser output remained open after draining")
		}
	case <-time.After(time.Second):
		t.Fatal("SIP parser did not close after draining")
	}
}

func TestReadTCPMessageKeepsCompleteFrameWhenDeadlineCleanupFails(t *testing.T) {
	message := "SIP/2.0 200 OK\r\nContent-Length: 0\r\n\r\n"
	base, peer := net.Pipe()
	defer base.Close()
	defer peer.Close()
	connection := &deadlineCleanupErrorConnection{Connection: NewTCPConnection(base)}
	reader := bufio.NewReader(connection)
	written := make(chan error, 1)
	go func() {
		_, err := peer.Write([]byte(message))
		written <- err
	}()

	frame, err := readTCPMessageWithTimeout(connection, reader, time.Second)
	if err != nil {
		t.Fatalf("complete SIP frame was discarded: %v", err)
	}
	if string(frame) != message {
		t.Fatalf("SIP frame = %q, want %q", frame, message)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}

func TestReadTCPMessageDrainsBufferedFrameWhenDeadlineSetupFindsClosedPeer(t *testing.T) {
	message := "SIP/2.0 200 OK\r\nContent-Length: 0\r\n\r\n"
	base, peer := net.Pipe()
	defer base.Close()
	connection := &deadlineSetupClosedConnection{Connection: NewTCPConnection(base)}
	reader := bufio.NewReader(connection)
	written := make(chan error, 1)
	go func() {
		_, err := peer.Write([]byte(message))
		_ = peer.Close()
		written <- err
	}()

	frame, err := readTCPMessageWithTimeout(connection, reader, time.Second)
	if err != nil {
		t.Fatalf("buffered SIP frame was discarded: %v", err)
	}
	if string(frame) != message {
		t.Fatalf("SIP frame = %q, want %q", frame, message)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}

func TestProcessTCPConnectionDrainsResponseBeforePeerClose(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	remoteURI, err := ParseSipURI("sip:34020000001320000001@192.0.2.10:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &localURI, Params: NewParams()})
	defer server.Close()
	serverPipe, peer := net.Pipe()
	connection := NewTCPConnection(&sipTestTCPConn{
		Conn:   serverPipe,
		local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
	})
	go server.ProcessTCPConnection(connection)

	callID := CallID("peer-close-response")
	request := NewRequest("", MethodMessage, &remoteURI, DefaultSipVersion,
		NewHeaderBuilder().
			SetFrom(&Address{URI: &localURI, Params: NewParams().Add("tag", String{Str: "peer-close"})}).
			SetTo(&Address{URI: &remoteURI, Params: NewParams()}).
			SetCallID(&callID).
			SetMethod(MethodMessage).
			SetSeqNo(1).
			AddVia(&ViaHop{
				ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "TCP",
				Host: "192.0.2.20", Port: NewPort(5060),
				Params: NewParams().Add("branch", String{Str: "z9hG4bK-peer-close"}),
			}).Build(), nil)
	request.SetConnection(connection)
	request.SetSource(connection.LocalAddr())
	request.SetDestination(connection.RemoteAddr())

	peerDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(peer)
		if _, err := readTCPMessage(reader); err != nil {
			peerDone <- err
			return
		}
		response := "SIP/2.0 200 OK\r\n" +
			"Via: SIP/2.0/TCP 192.0.2.20:5060;branch=z9hG4bK-peer-close\r\n" +
			"From: <sip:34020000002000000001@192.0.2.20:5060>;tag=peer-close\r\n" +
			"To: <sip:34020000001320000001@192.0.2.10:5060>\r\n" +
			"Call-ID: peer-close-response\r\n" +
			"CSeq: 1 MESSAGE\r\n" +
			"Content-Length: 0\r\n\r\n"
		_, err := peer.Write([]byte(response))
		_ = peer.Close()
		peerDone <- err
	}()

	tx, err := server.Request(request)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := tx.GetResponseContext(ctx)
	if err != nil {
		t.Fatalf("response sent before peer close was lost: %v", err)
	}
	if response == nil || response.StatusCode() != 200 {
		t.Fatalf("response before peer close = %#v", response)
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
}

func TestReadTCPMessageDrainsPrefetchedFramesAfterPeerClose(t *testing.T) {
	base, peer := net.Pipe()
	conn := NewTCPConnection(base)
	defer conn.Close()
	first := "MESSAGE sip:first@example.com SIP/2.0\r\nContent-Length: 0\r\n\r\n"
	second := "NOTIFY sip:second@example.com SIP/2.0\r\nContent-Length: 4\r\n\r\ntest"
	writeDone := make(chan error, 1)
	go func() {
		_, err := peer.Write([]byte(first + second))
		_ = peer.Close()
		writeDone <- err
	}()

	reader := bufio.NewReaderSize(conn, maxSIPHeaderLineBytes+1)
	message, err := readTCPMessageWithTimeout(conn, reader, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(message) != first {
		t.Fatalf("first SIP frame = %q", message)
	}
	message, err = readTCPMessageWithTimeout(conn, reader, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(message) != second {
		t.Fatalf("second SIP frame = %q", message)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if _, err := readTCPMessageWithTimeout(conn, reader, time.Second); !errors.Is(err, io.EOF) {
		t.Fatalf("drained closed connection error = %v, want EOF", err)
	}
}

type deadlineCleanupErrorConnection struct {
	Connection
	readDeadlineCalls int
}

type deadlineSetupClosedConnection struct {
	Connection
	readDeadlineCalls int
}

func (c *deadlineSetupClosedConnection) SetReadDeadline(deadline time.Time) error {
	c.readDeadlineCalls++
	if c.readDeadlineCalls == 2 {
		return io.ErrClosedPipe
	}
	return c.Connection.SetReadDeadline(deadline)
}

func (c *deadlineCleanupErrorConnection) SetReadDeadline(deadline time.Time) error {
	c.readDeadlineCalls++
	if c.readDeadlineCalls == 3 {
		return fmt.Errorf("injected deadline cleanup failure")
	}
	return c.Connection.SetReadDeadline(deadline)
}

func TestReadTCPMessageEnforcesResourceLimits(t *testing.T) {
	read := func(input string) ([]byte, error) {
		reader := bufio.NewReaderSize(strings.NewReader(input), maxSIPHeaderLineBytes+1)
		return readTCPMessage(reader)
	}

	body := "hello"
	valid := "MESSAGE sip:test@example.com SIP/2.0\r\nContent-Length: 5\r\n\r\n" + body
	message, err := read(valid)
	if err != nil {
		t.Fatal(err)
	}
	if string(message) != valid {
		t.Fatalf("framed SIP message = %q", message)
	}

	var oversizedHeaders strings.Builder
	oversizedHeaders.WriteString("OPTIONS sip:test@example.com SIP/2.0\r\n")
	for oversizedHeaders.Len() <= maxSIPHeaderBytes {
		oversizedHeaders.WriteString("X-Padding: ")
		oversizedHeaders.WriteString(strings.Repeat("x", 1000))
		oversizedHeaders.WriteString("\r\n")
	}
	oversizedHeaders.WriteString("\r\n")

	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{name: "header line", input: "X-Test: " + strings.Repeat("x", maxSIPHeaderLineBytes) + "\r\n", wantErr: "header line exceeds"},
		{name: "header total", input: oversizedHeaders.String(), wantErr: "headers exceed"},
		{name: "body length", input: fmt.Sprintf("MESSAGE sip:test@example.com SIP/2.0\r\nContent-Length: %d\r\n\r\n", maxSIPBodyBytes+1), wantErr: "exceeds"},
		{name: "duplicate length", input: "MESSAGE sip:test@example.com SIP/2.0\r\nContent-Length: 0\r\nl: 0\r\n\r\n", wantErr: "multiple Content-Length"},
		{name: "folded canonical length", input: "MESSAGE sip:test@example.com SIP/2.0\r\nX-Test: value\r\n Content-Length: 5\r\n\r\nhello", wantErr: "folded Content-Length"},
		{name: "folded compact length", input: "MESSAGE sip:test@example.com SIP/2.0\r\nX-Test: value\r\n\tl: 5\r\n\r\nhello", wantErr: "folded Content-Length"},
		{name: "truncated body", input: "MESSAGE sip:test@example.com SIP/2.0\r\nContent-Length: 5\r\n\r\nabc", wantErr: "read SIP body"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := read(test.input); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("readTCPMessage error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestContentLengthLimitsApplyToTCPAndPacketParser(t *testing.T) {
	overLimit := fmt.Sprintf("%d", maxSIPBodyBytes+1)
	if _, found, err := parseTCPContentLength([]byte("Content-Length: " + overLimit + "\r\n")); !found || err == nil {
		t.Fatalf("oversized TCP Content-Length result = found %v, err %v", found, err)
	}
	if _, err := ParseHeader("Content-Length: " + overLimit); err == nil {
		t.Fatal("oversized parsed Content-Length was accepted")
	}
	packet := Packet{bodylength: maxSIPBodyBytes + 1}
	if _, err := packet.getBody(); err == nil {
		t.Fatal("oversized packet body allocation was accepted")
	}
	truncated := Packet{reader: bufio.NewReader(strings.NewReader("abc")), bodylength: 5}
	if _, err := truncated.getBody(); err == nil || !strings.Contains(err.Error(), "got 3 of 5") {
		t.Fatalf("truncated packet body error = %v", err)
	}
}

func TestPacketParserRejectsMismatchedAndDuplicateContentLength(t *testing.T) {
	parser := newParser()
	defer parser.stop()
	base, peer := net.Pipe()
	defer base.Close()
	defer peer.Close()
	connection := NewTCPConnection(base)
	tests := []string{
		"MESSAGE sip:test@example.com SIP/2.0\r\nContent-Length: 5\r\n\r\nabc",
		"MESSAGE sip:test@example.com SIP/2.0\r\nContent-Length: 0\r\n\r\nabc",
		"MESSAGE sip:test@example.com SIP/2.0\r\nContent-Length: 3\r\nl: 3\r\n\r\nabc",
		"MESSAGE sip:test@example.com SIP/2.0\r\nX-Test: value\r\n Content-Length: 3\r\n\r\nabc",
		"MESSAGE sip:test@example.com SIP/2.0\r\nX-Test: value\r\n\tl: 3\r\n\r\nabc",
	}
	for _, input := range tests {
		parser.in <- newPacket([]byte(input), &net.TCPAddr{}, connection)
		select {
		case message := <-parser.out:
			t.Fatalf("invalid packet was dispatched: %v", message)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestPacketParserRetainsMalformedOptionalHeaderForValidation(t *testing.T) {
	parser := newParser()
	defer parser.stop()
	base, peer := net.Pipe()
	defer base.Close()
	defer peer.Close()
	connection := NewTCPConnection(base)
	input := "OPTIONS sip:34020000002000000001@example.com SIP/2.0\r\n" +
		"Via: SIP/2.0/TCP 192.0.2.10:5060;branch=z9hG4bK-malformed-route\r\n" +
		"From: <sip:34020000001320000001@example.com>;tag=malformed-route\r\n" +
		"To: <sip:34020000002000000001@example.com>\r\n" +
		"Call-ID: malformed-route\r\n" +
		"CSeq: 1 OPTIONS\r\n" +
		"Max-Forwards: 70\r\n" +
		"Record-Route: <sip:broken.example.com\r\n" +
		"Content-Length: 0\r\n\r\n"
	parser.in <- newPacket([]byte(input), &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060}, connection)
	select {
	case message := <-parser.out:
		request, ok := message.(*Request)
		if !ok {
			t.Fatalf("parsed message = %T, want request", message)
		}
		if err := validateInboundRequestHeaders(request); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("malformed optional header validation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("parser did not dispatch malformed request for a 400 response")
	}
}

func TestServerCloseTerminatesActiveTCPConnections(t *testing.T) {
	server := NewServer(&Address{})
	serverPipe, clientPipe := net.Pipe()
	defer clientPipe.Close()
	connection := NewTCPConnection(&sipTestTCPConn{
		Conn:   serverPipe,
		local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
	})
	done := make(chan struct{})
	go func() {
		server.ProcessTCPConnection(connection)
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		server.connectionMu.Lock()
		tracked := len(server.connections) == 1
		server.connectionMu.Unlock()
		if tracked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("TCP connection was not tracked")
		}
		time.Sleep(time.Millisecond)
	}

	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			server.Close()
		}()
	}
	wait.Wait()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("active TCP read loop survived Server.Close")
	}
	server.connectionMu.Lock()
	remaining := len(server.connections)
	server.connectionMu.Unlock()
	if remaining != 0 {
		t.Fatalf("tracked TCP connections after close = %d", remaining)
	}
}

func TestServerCloseWaitsForActiveRequestHandlers(t *testing.T) {
	server := NewServer(&Address{})
	started := make(chan struct{})
	release := make(chan struct{})
	ctx := &Context{handlers: []HandlerFunc{func(*Context) {
		close(started)
		<-release
	}}, index: -1}
	server.startRequestContext(ctx)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request handler did not start")
	}

	closed := make(chan struct{})
	go func() {
		server.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Server.Close returned while a request handler was active")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Server.Close did not finish after the request handler exited")
	}
}

func TestClosedServerRejectsNewRequestHandlers(t *testing.T) {
	server := NewServer(&Address{})
	server.Close()
	called := make(chan struct{}, 1)
	server.startRequestContext(&Context{handlers: []HandlerFunc{func(*Context) { called <- struct{}{} }}, index: -1})
	select {
	case <-called:
		t.Fatal("request handler started after Server.Close")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestClosedServerRejectsOutboundRequests(t *testing.T) {
	server := NewServer(&Address{})
	server.Close()
	serverPipe, clientPipe := net.Pipe()
	defer serverPipe.Close()
	defer clientPipe.Close()
	connection := NewTCPConnection(&sipTestTCPConn{
		Conn:   serverPipe,
		local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
	})
	target := URI{FHost: "192.0.2.10", FPort: NewPort(5060)}
	request := NewRequest("", MethodOptions, &target, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodOptions).AddVia(&ViaHop{Params: NewParams()}).Build(), nil)
	request.SetConnection(connection)
	request.SetDestination(connection.RemoteAddr())
	if _, err := server.Request(request); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed server request error = %v", err)
	}
}

func TestTCPFrameTimeoutStartsAfterFirstByte(t *testing.T) {
	newPipe := func() (Connection, net.Conn) {
		serverPipe, clientPipe := net.Pipe()
		connection := NewTCPConnection(&sipTestTCPConn{
			Conn:   serverPipe,
			local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
			remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
		})
		return connection, clientPipe
	}

	t.Run("idle connection", func(t *testing.T) {
		connection, peer := newPipe()
		defer connection.Close()
		defer peer.Close()
		reader := bufio.NewReaderSize(connection, maxSIPHeaderLineBytes+1)
		result := make(chan error, 1)
		go func() {
			_, err := readTCPMessageWithTimeout(connection, reader, 20*time.Millisecond)
			result <- err
		}()
		time.Sleep(50 * time.Millisecond)
		message := "OPTIONS sip:test@example.com SIP/2.0\r\nContent-Length: 0\r\n\r\n"
		if _, err := peer.Write([]byte(message)); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("idle connection timed out before first byte: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for framed message")
		}
	})

	t.Run("slow frame", func(t *testing.T) {
		connection, peer := newPipe()
		defer connection.Close()
		defer peer.Close()
		reader := bufio.NewReaderSize(connection, maxSIPHeaderLineBytes+1)
		result := make(chan error, 1)
		go func() {
			_, err := readTCPMessageWithTimeout(connection, reader, 20*time.Millisecond)
			result <- err
		}()
		if _, err := peer.Write([]byte("O")); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "timeout") {
				t.Fatalf("slow frame error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("slow frame did not hit read deadline")
		}
	})
}

func TestServerUpdatesExistingTransactionToReconnectedTCPConnection(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &localURI, Params: NewParams()})
	defer server.Close()
	callID := CallID("tcp-reconnect")

	newConnection := func(localPort int) (Connection, net.Conn) {
		client, peer := net.Pipe()
		wrapped := &sipTestTCPConn{
			Conn:   client,
			local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: localPort},
			remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		}
		return NewTCPConnection(wrapped), peer
	}
	first, firstPeer := newConnection(41000)
	defer first.Close()
	defer firstPeer.Close()
	second, secondPeer := newConnection(41001)
	defer second.Close()
	defer secondPeer.Close()

	request := NewRequest("", MethodRegister, &localURI, DefaultSipVersion,
		[]Header{&callID}, nil)
	request.SetConnection(first)
	transaction := server.mustTX(request)
	if transaction.connection() != first {
		t.Fatal("initial TCP transaction did not retain its connection")
	}

	reconnected := request.Clone().(*Request)
	reconnected.SetConnection(second)
	if got := server.mustTX(reconnected); got != transaction {
		t.Fatal("same Call-ID did not reuse transaction")
	}
	if transaction.connection() != second {
		t.Fatal("reconnected TCP transaction kept stale connection")
	}
}

func TestMalformedRequestDoesNotReplaceExistingTransactionConnection(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	remoteURI, err := ParseSipURI("sip:34020000001320000001@127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &localURI, Params: NewParams()})
	defer server.Close()
	newConnection := func(localPort int) (Connection, net.Conn) {
		client, peer := net.Pipe()
		wrapped := &sipTestTCPConn{
			Conn: client, local: &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: localPort},
			remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		}
		return NewTCPConnection(wrapped), peer
	}
	ownerConnection, ownerPeer := newConnection(41000)
	defer ownerConnection.Close()
	defer ownerPeer.Close()
	malformedConnection, malformedPeer := newConnection(41001)
	defer malformedConnection.Close()
	defer malformedPeer.Close()

	callID := CallID("malformed-transaction-isolation")
	from := &Address{URI: &remoteURI, Params: NewParams().Add("tag", String{Str: "owner"})}
	to := &Address{URI: &localURI, Params: NewParams()}
	request := NewRequest("", MethodOptions, &localURI, DefaultSipVersion,
		NewHeaderBuilder().SetFrom(from).SetTo(to).SetCallID(&callID).SetMethod(MethodOptions).SetSeqNo(7).
			AddVia(&ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "TCP", Host: "192.0.2.30", Port: NewPort(5060), Params: NewParams().Add("branch", String{Str: "z9hG4bK-owner"})}).Build(), nil)
	request.SetConnection(ownerConnection)
	request.SetSource(ownerConnection.RemoteAddr())
	request.SetDestination(ownerConnection.LocalAddr())
	owner := server.mustTX(request)

	malformed := request.Clone().(*Request)
	malformed.AppendHeader(&CSeq{SeqNo: 8, MethodName: MethodOptions})
	malformed.SetConnection(malformedConnection)
	malformed.SetSource(malformedConnection.RemoteAddr())
	malformed.SetDestination(malformedConnection.LocalAddr())
	go server.handlerRequest(malformed)
	if err := malformedPeer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	status, err := bufio.NewReader(malformedPeer).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "SIP/2.0 400") {
		t.Fatalf("malformed response status = %q", status)
	}
	if owner.connection() != ownerConnection {
		t.Fatal("malformed request replaced existing transaction connection")
	}
	if server.getTX(getTXKey(request)) != owner {
		t.Fatal("malformed request replaced existing transaction")
	}
}

type sipTestTCPConn struct {
	net.Conn
	local  net.Addr
	remote net.Addr
}

func (c *sipTestTCPConn) LocalAddr() net.Addr  { return c.local }
func (c *sipTestTCPConn) RemoteAddr() net.Addr { return c.remote }

func tcpTestRequest(callID string, cseq int, lengthHeader, body string) string {
	return fmt.Sprintf("MESSAGE sip:34020000002000000001@127.0.0.1 SIP/2.0\r\n"+
		"Via: SIP/2.0/TCP 127.0.0.1:5061;branch=z9hG4bK-%s\r\n"+
		"From: <sip:34020000001320000001@127.0.0.1>;tag=from-%d\r\n"+
		"To: <sip:34020000002000000001@127.0.0.1>\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: %d MESSAGE\r\n"+
		"Max-Forwards: 70\r\n"+
		"Content-Type: Application/MANSCDP+xml\r\n"+
		"%s: %d\r\n\r\n%s",
		callID, cseq, callID, cseq, lengthHeader, len(body), body)
}
