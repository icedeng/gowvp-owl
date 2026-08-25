package sip

import (
	"bufio"
	"fmt"
	"net"
	"strings"
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
	secondBody := "<Notify><CmdType>Keepalive</CmdType><SN>2</SN><DeviceID>34020000001320000001</DeviceID><Status>OK</Status></Notify>"
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
				"To: <sip:34020000002000000001@127.0.0.1>\r\n",
		},
		{
			name: "Via",
			headers: "From: <sip:34020000001320000001@127.0.0.1>;tag=from-missing-via\r\n" +
				"To: <sip:34020000002000000001@127.0.0.1>\r\n",
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
			request := "OPTIONS sip:34020000002000000001@127.0.0.1 SIP/2.0\r\n" + test.headers +
				"Call-ID: missing-" + strings.ToLower(test.name) + "\r\n" +
				"CSeq: 1 OPTIONS\r\nContent-Length: 0\r\n\r\n"
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
		"Content-Type: Application/MANSCDP+xml\r\n"+
		"%s: %d\r\n\r\n%s",
		callID, cseq, callID, cseq, lengthHeader, len(body), body)
}
