package sip

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
)

type contentLengthInjectingSecurity struct{}

func (contentLengthInjectingSecurity) Sign(message Message) error {
	message.AppendHeader(&GenericHeader{HeaderName: "l", Contents: "999"})
	return nil
}

func (contentLengthInjectingSecurity) Verify(Message) error { return nil }

type panickingWireHeader struct{}

func (*panickingWireHeader) Name() string          { return "X-Panic" }
func (header *panickingWireHeader) Clone() Header  { return header }
func (*panickingWireHeader) String() string        { panic("invalid custom SIP header") }
func (*panickingWireHeader) Equals(other any) bool { return false }

type panickingCloneHeader struct{}

func (*panickingCloneHeader) Name() string          { return "X-Clone-Panic" }
func (*panickingCloneHeader) Clone() Header         { panic("invalid custom SIP header clone") }
func (*panickingCloneHeader) String() string        { return "X-Clone-Panic: test" }
func (*panickingCloneHeader) Equals(other any) bool { return false }

type panickingMessageSecurity struct{}

func (panickingMessageSecurity) Sign(Message) error { panic("invalid SIP signer") }
func (panickingMessageSecurity) Verify(Message) error {
	return nil
}

type panickingVerifierSecurity struct{}

func (panickingVerifierSecurity) Sign(Message) error { return nil }
func (panickingVerifierSecurity) Verify(Message) error {
	panic("invalid SIP verifier")
}

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

func TestTransactionRequestRejectsMissingRequiredHeadersWithoutPanic(t *testing.T) {
	connection := &captureConnection{
		local:     &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote:    &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		network:   "tcp",
		transport: "TCP",
	}
	request := NewRequest("", MethodOptions, &URI{FHost: "192.0.2.30"}, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodOptions).AddVia(&ViaHop{Host: "192.0.2.20", Params: NewParams()}).Build(), nil)
	request.SetDestination(connection.remote)
	tx := NewTransaction("missing-required-headers", connection)
	defer tx.Close()

	if err := tx.Request(request); err == nil || !strings.Contains(err.Error(), "From") {
		t.Fatalf("missing required header error = %v", err)
	}
	if len(connection.payload) != 0 {
		t.Fatalf("incomplete request was written: %q", connection.payload)
	}

	localURI := &URI{FUser: String{Str: "34020000002000000001"}, FHost: "192.0.2.20"}
	remoteURI := &URI{FUser: String{Str: "34020000001320000001"}, FHost: "192.0.2.30"}
	request = NewRequest("", MethodOptions, remoteURI, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodOptions).
			SetFrom(&Address{URI: localURI, Params: NewParams()}).
			SetTo(&Address{URI: remoteURI, Params: NewParams()}).
			AddVia(&ViaHop{Host: "192.0.2.20", Params: NewParams()}).Build(), nil)
	if err := tx.Request(request); err == nil || !strings.Contains(err.Error(), "destination") {
		t.Fatalf("missing destination error = %v", err)
	}
	if len(connection.payload) != 0 {
		t.Fatalf("request without destination was written: %q", connection.payload)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Request)
		want   string
	}{
		{name: "nil From params", want: "From", mutate: func(request *Request) {
			from, _ := request.From()
			from.Params = nil
		}},
		{name: "nil Via params", want: "Via", mutate: func(request *Request) {
			via, _ := request.ViaHop()
			via.Params = nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection.payload = nil
			request := NewRequest("", MethodOptions, remoteURI, DefaultSipVersion,
				NewHeaderBuilder().SetMethod(MethodOptions).
					SetFrom(&Address{URI: localURI, Params: NewParams()}).
					SetTo(&Address{URI: remoteURI, Params: NewParams()}).
					AddVia(&ViaHop{Host: "192.0.2.20", Params: NewParams()}).Build(), nil)
			request.SetDestination(connection.remote)
			test.mutate(request)
			tx := NewTransaction("invalid-required-header-"+test.name, connection)
			defer tx.Close()
			if err := tx.Request(request); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid required header error = %v, want %s rejection", err, test.want)
			}
			if len(connection.payload) != 0 {
				t.Fatalf("invalid request was written: %q", connection.payload)
			}
		})
	}
}

func TestHeadersBuilderRejectsNilAddressInputsWithoutPanic(t *testing.T) {
	headers := NewHeaderBuilder().
		SetFrom(nil).
		SetFrom(&Address{}).
		SetTo(nil).
		SetTo(&Address{}).
		SetToWithParam(nil).
		SetToWithParam(&Address{}).
		SetContact(nil).
		SetContact(&Address{}).
		AddVia(nil).
		Build()
	request := NewRequest("", MethodOptions, &URI{FHost: "192.0.2.30"}, DefaultSipVersion, headers, nil)
	if len(request.GetHeaders("From")) != 0 || len(request.GetHeaders("To")) != 0 ||
		len(request.GetHeaders("Contact")) != 0 || len(request.GetHeaders("Via")) != 0 {
		t.Fatalf("invalid builder inputs emitted headers: %#v", request.Headers())
	}

	valid := NewHeaderBuilder().SetFrom(&Address{URI: &URI{FHost: "192.0.2.20"}}).Build()
	request = NewRequest("", MethodOptions, &URI{FHost: "192.0.2.30"}, DefaultSipVersion, valid, nil)
	from, ok := request.From()
	if !ok || from == nil || from.Params == nil || dialogHeaderParam(from.Params, "tag") == "" {
		t.Fatalf("From with nil Params was not initialized: %#v", from)
	}
}

func TestTransactionRejectsMalformedOptionalHeadersWithoutPanic(t *testing.T) {
	connection := &captureConnection{
		local:     &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote:    &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		network:   "tcp",
		transport: "TCP",
	}
	localURI := &URI{FUser: String{Str: "34020000002000000001"}, FHost: "192.0.2.20"}
	remoteURI := &URI{FUser: String{Str: "34020000001320000001"}, FHost: "192.0.2.30"}
	newRequest := func() *Request {
		request := NewRequest("", MethodOptions, remoteURI, DefaultSipVersion,
			NewHeaderBuilder().SetMethod(MethodOptions).
				SetFrom(&Address{URI: localURI, Params: NewParams()}).
				SetTo(&Address{URI: remoteURI, Params: NewParams()}).
				AddVia(&ViaHop{Host: "192.0.2.20", Params: NewParams()}).Build(), nil)
		request.SetDestination(connection.remote)
		return request
	}

	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{name: "invalid request method token", mutate: func(request *Request) {
			request.method = "BAD METHOD"
			cseq, _ := request.CSeq()
			cseq.MethodName = "BAD METHOD"
		}},
		{name: "invalid header name token", mutate: func(request *Request) {
			request.AppendHeader(&GenericHeader{HeaderName: "X@Test", Contents: "value"})
		}},
		{name: "non ASCII header name", mutate: func(request *Request) {
			request.AppendHeader(&GenericHeader{HeaderName: "X-测试", Contents: "value"})
		}},
		{name: "nil secondary Via hop", mutate: func(request *Request) {
			via, _ := request.Via()
			request.RemoveHeader("Via")
			request.AppendHeader(ViaHeader{via[0], nil})
		}},
		{name: "nil Contact address", mutate: func(request *Request) {
			request.AppendHeader(&ContactHeader{Params: NewParams()})
		}},
		{name: "nil Route address", mutate: func(request *Request) {
			request.AppendHeader(&RouteHeader{Addresses: []*URI{nil}})
		}},
		{name: "header line injection", mutate: func(request *Request) {
			request.AppendHeader(&GenericHeader{HeaderName: "X-Test", Contents: "ok\r\nInjected: yes"})
		}},
		{name: "panicking custom header", mutate: func(request *Request) {
			request.AppendHeader(&panickingWireHeader{})
		}},
		{name: "panicking custom header clone", mutate: func(request *Request) {
			request.AppendHeader(&panickingCloneHeader{})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connection.payload = nil
			request := newRequest()
			test.mutate(request)
			tx := NewTransaction("malformed-optional-"+test.name, connection)
			defer tx.Close()
			if err := tx.Request(request); err == nil {
				t.Fatal("malformed optional header was accepted")
			}
			if len(connection.payload) != 0 {
				t.Fatalf("malformed request was written: %q", connection.payload)
			}
		})
	}
}

func TestTransactionRespondRejectsMalformedOptionalHeadersWithoutPanic(t *testing.T) {
	connection := &captureConnection{
		local:     &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote:    &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		network:   "tcp",
		transport: "TCP",
	}
	request := NewRequest("", MethodOptions, &URI{FHost: "192.0.2.20"}, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodOptions).
			SetFrom(&Address{URI: &URI{FHost: "192.0.2.30"}, Params: NewParams()}).
			SetTo(&Address{URI: &URI{FHost: "192.0.2.20"}, Params: NewParams()}).
			AddVia(&ViaHop{Host: "192.0.2.30", Params: NewParams()}).Build(), nil)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	responseWithoutDestination := NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	responseWithoutDestination.SetDestination(nil)
	tx := NewTransaction("missing-response-destination", connection)
	tx.beginServerRequest()
	if err := tx.Respond(responseWithoutDestination); err == nil || !strings.Contains(err.Error(), "destination") {
		t.Fatalf("missing response destination error = %v", err)
	}
	tx.Close()
	if len(connection.payload) != 0 {
		t.Fatalf("response without destination was written: %q", connection.payload)
	}

	for _, header := range []Header{
		&RecordRouteHeader{Addresses: []*URI{nil}},
		&ContactHeader{Params: NewParams()},
		&panickingWireHeader{},
		&panickingCloneHeader{},
	} {
		connection.payload = nil
		response := NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
		response.AppendHeader(header)
		tx := NewTransaction("malformed-optional-response", connection)
		tx.beginServerRequest()
		err := tx.Respond(response)
		tx.Close()
		if err == nil {
			t.Fatalf("malformed response header %T was accepted", header)
		}
		if len(connection.payload) != 0 {
			t.Fatalf("malformed response was written: %q", connection.payload)
		}
	}
}

func TestTransactionRejectsPanickingSecurityWithoutNetworkWrite(t *testing.T) {
	connection := &captureConnection{
		local:     &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote:    &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		network:   "tcp",
		transport: "TCP",
	}
	localURI := &URI{FUser: String{Str: "34020000002000000001"}, FHost: "192.0.2.20"}
	remoteURI := &URI{FUser: String{Str: "34020000001320000001"}, FHost: "192.0.2.30"}
	request := NewRequest("", MethodMessage, remoteURI, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodMessage).
			SetFrom(&Address{URI: localURI, Params: NewParams()}).
			SetTo(&Address{URI: remoteURI, Params: NewParams()}).
			AddVia(&ViaHop{Host: "192.0.2.20", Params: NewParams()}).Build(), nil)
	request.SetSource(connection.remote)
	request.SetDestination(connection.remote)

	requestTX := NewTransaction("panicking-request-security", connection)
	requestTX.SetMessageSecurity(panickingMessageSecurity{})
	if err := requestTX.Request(request); err == nil || !strings.Contains(err.Error(), "sign SIP request") {
		t.Fatalf("panicking request signer error = %v", err)
	}
	requestTX.Close()
	if len(connection.payload) != 0 {
		t.Fatalf("request with panicking signer was written: %q", connection.payload)
	}

	response := NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	response.SetDestination(connection.remote)
	responseTX := NewTransaction("panicking-response-security", connection)
	responseTX.beginServerRequest()
	responseTX.SetMessageSecurity(panickingMessageSecurity{})
	if err := responseTX.Respond(response); err == nil || !strings.Contains(err.Error(), "sign SIP response") {
		t.Fatalf("panicking response signer error = %v", err)
	}
	responseTX.Close()
	if len(connection.payload) != 0 {
		t.Fatalf("response with panicking signer was written: %q", connection.payload)
	}
}

func TestHandlerRequestRecoversPanickingSecurityExtension(t *testing.T) {
	newRequest := func(t *testing.T, connection *serverTransactionCaptureConnection, version string) *Request {
		t.Helper()
		localURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
		if err != nil {
			t.Fatal(err)
		}
		remoteURI, err := ParseSipURI("sip:34020000001320000001@192.0.2.30:5060")
		if err != nil {
			t.Fatal(err)
		}
		request := NewRequest("", MethodOptions, &localURI, version,
			NewHeaderBuilder().SetMethod(MethodOptions).
				SetFrom(&Address{URI: &remoteURI, Params: NewParams()}).
				SetTo(&Address{URI: &localURI, Params: NewParams()}).
				AddVia(&ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "192.0.2.30", Port: NewPort(5060), Params: NewParams()}).Build(), nil)
		request.SetConnection(connection)
		request.SetSource(connection.remote)
		request.SetDestination(connection.local)
		return request
	}

	t.Run("verifier", func(t *testing.T) {
		server := NewServer(&Address{})
		defer server.Close()
		connection := newServerTransactionCaptureConnection()
		server.udpConn = connection
		server.SetRequestSecurityResolver(func(*Request) (MessageSecurity, error) {
			return panickingVerifierSecurity{}, nil
		})

		server.handlerRequest(newRequest(t, connection, DefaultSipVersion))
		response := string(connection.waitPayload(t))
		if !strings.HasPrefix(response, "SIP/2.0 403 invalid request security\r\n") {
			t.Fatalf("panicking verifier response = %q", response)
		}
	})

	t.Run("malformed request response signer", func(t *testing.T) {
		server := NewServer(&Address{})
		defer server.Close()
		connection := newServerTransactionCaptureConnection()
		server.udpConn = connection
		server.SetRequestSecurityResolver(func(*Request) (MessageSecurity, error) {
			return panickingMessageSecurity{}, nil
		})

		server.handlerRequest(newRequest(t, connection, "SIP/1.0"))
		response := string(connection.waitPayload(t))
		if !strings.HasPrefix(response, "SIP/2.0 505 Version Not Supported\r\n") {
			t.Fatalf("panicking error response signer payload = %q", response)
		}
	})

	t.Run("security resolver", func(t *testing.T) {
		server := NewServer(&Address{})
		defer server.Close()
		connection := newServerTransactionCaptureConnection()
		server.udpConn = connection
		server.SetRequestSecurityResolver(func(*Request) (MessageSecurity, error) {
			panic("invalid SIP security resolver")
		})

		server.handlerRequest(newRequest(t, connection, DefaultSipVersion))
		response := string(connection.waitPayload(t))
		if !strings.HasPrefix(response, "SIP/2.0 403 request security unavailable\r\n") {
			t.Fatalf("panicking security resolver response = %q", response)
		}
	})
}

func TestServerRequestRejectsOutOfRangeCSeqBeforeWrite(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	connection := &captureConnection{
		local:     &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote:    &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		network:   "tcp",
		transport: "TCP",
	}
	remoteURI, err := ParseSipURI("sip:34020000001320000001@192.0.2.30:5060")
	if err != nil {
		t.Fatal(err)
	}
	request := NewRequest("", MethodMessage, &URI{FHost: "192.0.2.30"}, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodMessage).SetSeqNo(uint(maxCseq+1)).
			SetFrom(&Address{URI: &serverURI, Params: NewParams()}).
			SetTo(&Address{URI: &remoteURI, Params: NewParams()}).
			AddVia(&ViaHop{Params: NewParams()}).Build(), nil)
	request.SetConnection(connection)
	request.SetDestination(connection.remote)

	if _, err := server.Request(request); err == nil || !strings.Contains(err.Error(), "CSeq") {
		t.Fatalf("out-of-range CSeq error = %v", err)
	}
	if len(connection.payload) != 0 {
		t.Fatalf("out-of-range CSeq was written: %q", connection.payload)
	}
}

func TestProgrammaticMessagesCanonicalizeContentLength(t *testing.T) {
	request := NewRequest("", MethodOptions, &URI{FHost: "192.0.2.30"}, DefaultSipVersion, nil, nil)
	requestLengths := request.GetHeaders("Content-Length")
	if len(requestLengths) != 1 {
		t.Fatalf("empty request Content-Length count = %d, want 1", len(requestLengths))
	}
	if length, ok := requestLengths[0].(*ContentLength); !ok || length == nil || *length != 0 {
		t.Fatalf("empty request Content-Length = %#v, want 0", requestLengths[0])
	}

	response := NewResponse("", DefaultSipVersion, http.StatusOK, "OK", nil, nil)
	responseLengths := response.GetHeaders("Content-Length")
	if len(responseLengths) != 1 {
		t.Fatalf("empty response Content-Length count = %d, want 1", len(responseLengths))
	}
	if length, ok := responseLengths[0].(*ContentLength); !ok || length == nil || *length != 0 {
		t.Fatalf("empty response Content-Length = %#v, want 0", responseLengths[0])
	}

	stale := ContentLength(99)
	duplicate := ContentLength(100)
	request = NewRequest("", MethodMessage, &URI{FHost: "192.0.2.30"}, DefaultSipVersion,
		[]Header{&stale, &duplicate}, []byte("test"))
	requestLengths = request.GetHeaders("Content-Length")
	if len(requestLengths) != 1 {
		t.Fatalf("body request Content-Length count = %d, want 1", len(requestLengths))
	}
	if length, ok := requestLengths[0].(*ContentLength); !ok || length == nil || *length != 4 {
		t.Fatalf("body request Content-Length = %#v, want 4", requestLengths[0])
	}
}

func TestTransactionCanonicalizesContentLengthBeforeWrite(t *testing.T) {
	connection := &captureConnection{
		local:     &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote:    &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		network:   "tcp",
		transport: "TCP",
	}
	localURI := &URI{FUser: String{Str: "34020000002000000001"}, FHost: "192.0.2.20"}
	remoteURI := &URI{FUser: String{Str: "34020000001320000001"}, FHost: "192.0.2.30"}
	request := NewRequest("", MethodMessage, &URI{FHost: "192.0.2.30"}, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodMessage).SetSeqNo(1).
			SetFrom(&Address{URI: localURI, Params: NewParams()}).
			SetTo(&Address{URI: remoteURI, Params: NewParams()}).
			AddVia(&ViaHop{Host: "192.0.2.20", Params: NewParams()}).Build(), []byte("test"))
	request.SetDestination(connection.remote)
	stale := ContentLength(99)
	request.AppendHeader(&stale)
	request.AppendHeader(&GenericHeader{HeaderName: "l", Contents: "100"})

	tx := NewTransaction("outbound-content-length", connection)
	defer tx.Close()
	if err := tx.Request(request); err != nil {
		t.Fatal(err)
	}
	wire := string(connection.payload)
	if count := strings.Count(strings.ToLower(wire), "\r\ncontent-length:") +
		strings.Count(strings.ToLower(wire), "\r\nl:"); count != 1 {
		t.Fatalf("wire Content-Length count = %d, want 1:\n%s", count, wire)
	}
	if !strings.Contains(wire, "\r\nContent-Length: 4\r\n") {
		t.Fatalf("wire Content-Length is not accurate:\n%s", wire)
	}

	connection.payload = nil
	request.SetSource(connection.remote)
	response := NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	response.AppendHeader(&stale)
	response.AppendHeader(&GenericHeader{HeaderName: "l", Contents: "100"})
	responseTX := NewTransaction("outbound-response-content-length", connection)
	defer responseTX.Close()
	responseTX.beginServerRequest()
	if err := responseTX.Respond(response); err != nil {
		t.Fatal(err)
	}
	wire = string(connection.payload)
	if count := strings.Count(strings.ToLower(wire), "\r\ncontent-length:") +
		strings.Count(strings.ToLower(wire), "\r\nl:"); count != 1 {
		t.Fatalf("response wire Content-Length count = %d, want 1:\n%s", count, wire)
	}
	if !strings.Contains(wire, "\r\nContent-Length: 0\r\n") {
		t.Fatalf("response wire Content-Length is not accurate:\n%s", wire)
	}
}

func TestTransactionRejectsContentLengthMutationBySecurity(t *testing.T) {
	connection := &captureConnection{
		local:     &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote:    &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		network:   "tcp",
		transport: "TCP",
	}
	localURI := &URI{FUser: String{Str: "34020000002000000001"}, FHost: "192.0.2.20"}
	remoteURI := &URI{FUser: String{Str: "34020000001320000001"}, FHost: "192.0.2.30"}
	request := NewRequest("", MethodMessage, remoteURI, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodMessage).SetSeqNo(1).
			SetFrom(&Address{URI: localURI, Params: NewParams()}).
			SetTo(&Address{URI: remoteURI, Params: NewParams()}).
			AddVia(&ViaHop{Host: "192.0.2.20", Params: NewParams()}).Build(), nil)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)

	t.Run("request", func(t *testing.T) {
		connection.payload = nil
		tx := NewTransaction("request-security-content-length", connection)
		defer tx.Close()
		tx.SetMessageSecurity(contentLengthInjectingSecurity{})
		if err := tx.Request(request); err == nil || !strings.Contains(err.Error(), "Content-Length") {
			t.Fatalf("request error = %v, want Content-Length rejection", err)
		}
		if len(connection.payload) != 0 {
			t.Fatalf("invalid request was written: %q", connection.payload)
		}
	})

	t.Run("response", func(t *testing.T) {
		connection.payload = nil
		response := NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
		tx := NewTransaction("response-security-content-length", connection)
		defer tx.Close()
		tx.beginServerRequest()
		tx.SetMessageSecurity(contentLengthInjectingSecurity{})
		if err := tx.Respond(response); err == nil || !strings.Contains(err.Error(), "Content-Length") {
			t.Fatalf("response error = %v, want Content-Length rejection", err)
		}
		if len(connection.payload) != 0 {
			t.Fatalf("invalid response was written: %q", connection.payload)
		}
	})
}

func TestTransactionRespondRejectsOutOfRangeCSeqBeforeWrite(t *testing.T) {
	connection := &captureConnection{
		local:     &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote:    &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		network:   "tcp",
		transport: "TCP",
	}
	target := &URI{FHost: "192.0.2.20"}
	request := NewRequest("", MethodOptions, target, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodOptions).SetSeqNo(uint(maxCseq+1)).Build(), nil)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	response := NewResponseFromRequest("", request, http.StatusBadRequest, "Bad Request", nil)
	tx := NewTransaction("outbound-response-cseq-limit", connection)
	defer tx.Close()
	tx.beginServerRequest()

	if err := tx.Respond(response); err == nil || !strings.Contains(err.Error(), "CSeq") {
		t.Fatalf("out-of-range response CSeq error = %v", err)
	}
	if len(connection.payload) != 0 {
		t.Fatalf("out-of-range response CSeq was written: %q", connection.payload)
	}
}

func TestTransactionRespondRejectsInvalidStartLineBeforeWrite(t *testing.T) {
	connection := &captureConnection{
		local:     &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote:    &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		network:   "tcp",
		transport: "TCP",
	}
	request := NewRequest("", MethodOptions, &URI{FHost: "192.0.2.20"}, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodOptions).
			SetFrom(&Address{URI: &URI{FHost: "192.0.2.30"}, Params: NewParams()}).
			SetTo(&Address{URI: &URI{FHost: "192.0.2.20"}, Params: NewParams()}).
			AddVia(&ViaHop{Host: "192.0.2.30", Params: NewParams()}).Build(), nil)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)

	for _, test := range []struct {
		name   string
		mutate func(*Response)
	}{
		{name: "unsupported SIP version", mutate: func(response *Response) { response.SetSipVersion("SIP/1.0") }},
		{name: "status below range", mutate: func(response *Response) { response.SetStatusCode(99) }},
		{name: "status above range", mutate: func(response *Response) { response.SetStatusCode(700) }},
		{name: "reason line injection", mutate: func(response *Response) { response.SetReason("OK\r\nInjected: yes") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection.payload = nil
			response := NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
			test.mutate(response)
			tx := NewTransaction("invalid-response-start-line-"+test.name, connection)
			tx.beginServerRequest()
			err := tx.Respond(response)
			tx.Close()
			if err == nil {
				t.Fatal("invalid response start line was accepted")
			}
			if len(connection.payload) != 0 {
				t.Fatalf("invalid response was written: %q", connection.payload)
			}
		})
	}
}

func TestInboundMessageTransportMustMatchConnection(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	remoteURI, err := ParseSipURI("sip:34020000001320000001@192.0.2.30:5060")
	if err != nil {
		t.Fatal(err)
	}
	base, peer := net.Pipe()
	defer base.Close()
	defer peer.Close()
	connection := NewTCPConnection(base)
	request := NewRequest("", MethodOptions, &localURI, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodOptions).
			SetFrom(&Address{URI: &remoteURI, Params: NewParams()}).
			SetTo(&Address{URI: &localURI, Params: NewParams()}).
			AddVia(&ViaHop{Host: "192.0.2.30", Port: NewPort(5060), Transport: "UDP", Params: NewParams()}).Build(), nil)
	request.SetConnection(connection)
	if err := validateInboundRequestHeaders(request); err == nil || !strings.Contains(err.Error(), "transport") {
		t.Fatalf("mismatched inbound request transport error = %v", err)
	}
	via, _ := request.ViaHop()
	via.Transport = "TCP"
	if err := validateInboundRequestHeaders(request); err != nil {
		t.Fatalf("matching inbound request transport rejected: %v", err)
	}

	response := NewResponseFromRequest("", request, 200, "OK", nil)
	response.SetConnection(connection)
	via, _ = response.ViaHop()
	via.Transport = "UDP"
	if err := validateInboundResponseHeaders(response); err == nil || !strings.Contains(err.Error(), "transport") {
		t.Fatalf("mismatched inbound response transport error = %v", err)
	}
}

func TestProgrammaticInboundMessagesRejectOutOfRangeCSeq(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	remoteURI, err := ParseSipURI("sip:34020000001320000001@192.0.2.30:5060")
	if err != nil {
		t.Fatal(err)
	}
	connection := &captureConnection{
		local: &net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060}, remote: &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		network: "udp", transport: "UDP",
	}
	request := NewRequest("", MethodOptions, &localURI, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodOptions).SetSeqNo(uint(maxCseq+1)).
			SetFrom(&Address{URI: &remoteURI, Params: NewParams()}).SetTo(&Address{URI: &localURI, Params: NewParams()}).
			AddVia(&ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "192.0.2.30", Port: NewPort(5060), Params: NewParams()}).Build(), nil)
	request.SetConnection(connection)
	if err := validateInboundRequestHeaders(request); err == nil || !strings.Contains(err.Error(), "CSeq") {
		t.Fatalf("programmatic request CSeq error = %v", err)
	}
	response := NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	response.SetConnection(connection)
	if err := validateInboundResponseHeaders(response); err == nil || !strings.Contains(err.Error(), "CSeq") {
		t.Fatalf("programmatic response CSeq error = %v", err)
	}
}

func TestInboundInviteFinalResponseRequiresToTag(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	remoteURI, err := ParseSipURI("sip:34020000001320000001@192.0.2.30:5060")
	if err != nil {
		t.Fatal(err)
	}
	request := NewRequest("", MethodInvite, &remoteURI, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodInvite).
			SetFrom(&Address{URI: &localURI, Params: NewParams()}).
			SetTo(&Address{URI: &remoteURI, Params: NewParams()}).
			AddVia(&ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "192.0.2.20", Port: NewPort(5060), Params: NewParams().Add("branch", String{Str: GenerateBranch()})}).Build(), nil)
	for _, status := range []int{200, 486} {
		response := NewResponseFromRequest("", request, status, "Final", nil)
		to, ok := response.To()
		if !ok || to == nil {
			t.Fatal("response is missing To")
		}
		to.Params = NewParams()
		if err := validateInboundResponseHeaders(response); err == nil || !strings.Contains(err.Error(), "To tag") {
			t.Fatalf("INVITE final response %d without To tag error = %v", status, err)
		}
	}
}

func TestInboundRequestValidatesMaxForwards(t *testing.T) {
	newRequest := func(method string, maxForwards MaxForwards) *Request {
		localURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
		if err != nil {
			t.Fatal(err)
		}
		remoteURI, err := ParseSipURI("sip:34020000001320000001@192.0.2.30:5060")
		if err != nil {
			t.Fatal(err)
		}
		request := NewRequest("", method, &localURI, DefaultSipVersion,
			NewHeaderBuilder().SetMethod(method).
				SetFrom(&Address{URI: &remoteURI, Params: NewParams()}).
				SetTo(&Address{URI: &localURI, Params: NewParams()}).
				AddVia(&ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "192.0.2.30", Port: NewPort(5060), Params: NewParams()}).Build(), nil)
		request.RemoveHeader("Max-Forwards")
		request.AppendHeader(&maxForwards)
		return request
	}

	t.Run("missing", func(t *testing.T) {
		request := newRequest(MethodMessage, defaultMaxForwards)
		request.RemoveHeader("Max-Forwards")
		if err := validateInboundRequestHeaders(request); err == nil || !strings.Contains(err.Error(), "exactly one Max-Forwards") {
			t.Fatalf("missing Max-Forwards error = %v", err)
		}
	})

	t.Run("duplicate", func(t *testing.T) {
		request := newRequest(MethodMessage, defaultMaxForwards)
		duplicate := defaultMaxForwards
		request.AppendHeader(&duplicate)
		if err := validateInboundRequestHeaders(request); err == nil || !strings.Contains(err.Error(), "exactly one Max-Forwards") {
			t.Fatalf("duplicate Max-Forwards error = %v", err)
		}
	})

	t.Run("invalid type", func(t *testing.T) {
		request := newRequest(MethodMessage, defaultMaxForwards)
		request.RemoveHeader("Max-Forwards")
		request.AppendHeader(&GenericHeader{HeaderName: "Max-Forwards", Contents: "70"})
		if err := validateInboundRequestHeaders(request); err == nil || !strings.Contains(err.Error(), "Max-Forwards header is invalid") {
			t.Fatalf("invalid Max-Forwards error = %v", err)
		}
	})

	t.Run("zero message", func(t *testing.T) {
		request := newRequest(MethodMessage, 0)
		if err := validateInboundRequestHeaders(request); !errors.Is(err, errSIPTooManyHops) {
			t.Fatalf("zero MESSAGE Max-Forwards error = %v", err)
		}
	})

	t.Run("zero options", func(t *testing.T) {
		request := newRequest(MethodOptions, 0)
		if err := validateInboundRequestHeaders(request); err != nil {
			t.Fatalf("zero OPTIONS Max-Forwards rejected: %v", err)
		}
	})

	t.Run("maximum", func(t *testing.T) {
		request := newRequest(MethodMessage, 255)
		if err := validateInboundRequestHeaders(request); err != nil {
			t.Fatalf("maximum Max-Forwards rejected: %v", err)
		}
	})

	t.Run("above maximum", func(t *testing.T) {
		request := newRequest(MethodMessage, 256)
		if err := validateInboundRequestHeaders(request); err == nil || !strings.Contains(err.Error(), "exceeds 255") {
			t.Fatalf("out-of-range Max-Forwards error = %v", err)
		}
	})
}

func TestParseMaxForwardsRejectsValuesAboveRFC3261Range(t *testing.T) {
	for _, value := range []string{"255", "0"} {
		headers, err := ParseHeader("Max-Forwards: " + value)
		if err != nil || len(headers) != 1 {
			t.Fatalf("parse Max-Forwards %s = headers:%#v err:%v", value, headers, err)
		}
	}
	for _, value := range []string{"256", "4294967295", "-1"} {
		if _, err := ParseHeader("Max-Forwards: " + value); err == nil {
			t.Fatalf("out-of-range Max-Forwards %s was accepted", value)
		}
	}
}

func TestSIPTokenValidationAtParserAndProgrammaticBoundaries(t *testing.T) {
	for _, line := range []string{
		"BAD@METHOD sip:34020000002000000001@192.0.2.20 SIP/2.0",
		"测试 sip:34020000002000000001@192.0.2.20 SIP/2.0",
	} {
		if _, _, _, err := ParseRequestLine(line); err == nil {
			t.Fatalf("invalid request method token was accepted: %q", line)
		}
	}
	for _, header := range []string{
		"X@Test: value",
		"X-测试: value",
		"CSeq: 1 BAD@METHOD",
	} {
		if _, err := ParseHeader(header); err == nil {
			t.Fatalf("invalid SIP token was accepted in header %q", header)
		}
	}
	if _, err := ParseHeader("X_Device-Trace: value"); err != nil {
		t.Fatalf("valid extension header rejected: %v", err)
	}

	localURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	remoteURI, err := ParseSipURI("sip:34020000001320000001@192.0.2.30:5060")
	if err != nil {
		t.Fatal(err)
	}
	connection := &captureConnection{
		local: &net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060}, remote: &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		network: "udp", transport: "UDP",
	}
	request := NewRequest("", "BAD@METHOD", &localURI, DefaultSipVersion,
		NewHeaderBuilder().SetMethod("BAD@METHOD").
			SetFrom(&Address{URI: &remoteURI, Params: NewParams()}).
			SetTo(&Address{URI: &localURI, Params: NewParams()}).
			AddVia(&ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "192.0.2.30", Port: NewPort(5060), Params: NewParams()}).Build(), nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	if err := validateInboundRequestHeaders(request); err == nil || !strings.Contains(err.Error(), "method") {
		t.Fatalf("programmatic inbound invalid method error = %v", err)
	}

	validRequest := NewRequest("", MethodOptions, &localURI, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodOptions).
			SetFrom(&Address{URI: &remoteURI, Params: NewParams()}).
			SetTo(&Address{URI: &localURI, Params: NewParams()}).
			AddVia(&ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "192.0.2.30", Port: NewPort(5060), Params: NewParams()}).Build(), nil)
	validRequest.SetConnection(connection)
	validRequest.SetSource(connection.remote)
	validRequest.SetDestination(connection.local)
	validRequest.AppendHeader(&GenericHeader{HeaderName: "X@Test", Contents: "value"})
	if err := validateInboundRequestHeaders(validRequest); err == nil || !strings.Contains(err.Error(), "header") {
		t.Fatalf("programmatic inbound invalid Header name error = %v", err)
	}
	validRequest.RemoveHeader("X@Test")
	response := NewResponseFromRequest("", validRequest, http.StatusOK, "OK", nil)
	response.SetConnection(connection)
	response.SetSource(connection.remote)
	response.SetDestination(connection.local)
	cseq, _ := response.CSeq()
	cseq.MethodName = "BAD@METHOD"
	if err := validateInboundResponseHeaders(response); err == nil || !strings.Contains(err.Error(), "CSeq") {
		t.Fatalf("programmatic inbound response invalid CSeq method error = %v", err)
	}
	if err := validateOutboundResponseCSeq(response); err == nil || !strings.Contains(err.Error(), "CSeq") {
		t.Fatalf("programmatic outbound response invalid CSeq method error = %v", err)
	}
	cseq.MethodName = MethodOptions
	response.AppendHeader(&GenericHeader{HeaderName: "X@Test", Contents: "value"})
	if err := validateInboundResponseHeaders(response); err == nil || !strings.Contains(err.Error(), "header") {
		t.Fatalf("programmatic inbound response invalid Header name error = %v", err)
	}
}

func TestHandlerRequestRespondsTooManyHops(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	remoteURI, err := ParseSipURI("sip:34020000001320000001@192.0.2.30:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &localURI, Params: NewParams()})
	defer server.Close()
	connection := newServerTransactionCaptureConnection()
	request := NewRequest("", MethodMessage, &localURI, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodMessage).
			SetFrom(&Address{URI: &remoteURI, Params: NewParams()}).
			SetTo(&Address{URI: &localURI, Params: NewParams()}).
			AddVia(&ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "192.0.2.30", Port: NewPort(5060), Params: NewParams()}).Build(), nil)
	request.RemoveHeader("Max-Forwards")
	zero := MaxForwards(0)
	request.AppendHeader(&zero)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)

	server.handlerRequest(request)
	response := string(connection.waitPayload(t))
	if !strings.HasPrefix(response, "SIP/2.0 483 Too Many Hops\r\n") {
		t.Fatalf("zero Max-Forwards response = %q", response)
	}
}

func TestHandlerRequestMethodNotAllowedIncludesAllow(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	remoteURI, err := ParseSipURI("sip:34020000001320000001@192.0.2.30:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &localURI, Params: NewParams()})
	defer server.Close()
	connection := newServerTransactionCaptureConnection()
	server.udpConn = connection
	request := NewRequest("", "PRACK", &localURI, DefaultSipVersion,
		NewHeaderBuilder().SetMethod("PRACK").
			SetFrom(&Address{URI: &remoteURI, Params: NewParams()}).
			SetTo(&Address{URI: &localURI, Params: NewParams()}).
			AddVia(&ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "192.0.2.30", Port: NewPort(5060), Params: NewParams()}).Build(), nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)

	server.handlerRequest(request)
	response := string(connection.waitPayload(t))
	if !strings.HasPrefix(response, "SIP/2.0 405 Method Not Allowed\r\n") {
		t.Fatalf("unknown method response = %q", response)
	}
	want := map[string]struct{}{
		MethodInvite: {}, MethodACK: {}, MethodCancel: {}, MethodOptions: {}, MethodBYE: {},
		MethodMessage: {}, MethodRegister: {}, MethodSubscribe: {}, MethodNotify: {}, MethodInfo: {},
	}
	var got map[string]struct{}
	for _, line := range strings.Split(response, "\r\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Allow") {
			continue
		}
		if got != nil {
			t.Fatalf("405 response contains duplicate Allow headers: %q", response)
		}
		got = make(map[string]struct{})
		for _, method := range strings.Split(value, ",") {
			method = strings.TrimSpace(method)
			if method == "" {
				t.Fatalf("405 Allow contains an empty method: %q", line)
			}
			if _, exists := got[method]; exists {
				t.Fatalf("405 Allow contains duplicate method %q", method)
			}
			got[method] = struct{}{}
		}
	}
	if len(got) != len(want) {
		t.Fatalf("405 Allow methods = %#v, want %#v", got, want)
	}
	for method := range want {
		if _, ok := got[method]; !ok {
			t.Errorf("405 Allow missing %s: %#v", method, got)
		}
	}
}

func TestHandlerRequestUnsupportedMANSCDPCommandReturnsBadRequest(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	remoteURI, err := ParseSipURI("sip:34020000001320000001@192.0.2.30:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &localURI, Params: NewParams()})
	defer server.Close()

	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		for _, method := range []string{MethodMessage, MethodNotify} {
			t.Run(version+"/"+method, func(t *testing.T) {
				connection := newServerTransactionCaptureConnection()
				server.udpConn = connection
				body := []byte(`<Notify><CmdType>UnsupportedCommand</CmdType><SN>1</SN><DeviceID>34020000001320000001</DeviceID></Notify>`)
				request := NewRequest("", method, &localURI, DefaultSipVersion,
					NewHeaderBuilder().SetMethod(method).
						SetFrom(&Address{URI: &remoteURI, Params: NewParams()}).
						SetTo(&Address{URI: &localURI, Params: NewParams()}).
						SetXGBVerValue(version).
						SetContentType(&ContentTypeXML).
						AddVia(&ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "192.0.2.30", Port: NewPort(5060), Params: NewParams()}).Build(), body)
				request.SetConnection(connection)
				request.SetSource(connection.remote)
				request.SetDestination(connection.local)

				server.handlerRequest(request)
				response := string(connection.waitPayload(t))
				if !strings.HasPrefix(response, "SIP/2.0 400 Bad Request\r\n") {
					t.Fatalf("unsupported %s CmdType response = %q", method, response)
				}
				if strings.Contains(strings.ToLower(response), "\r\nallow:") {
					t.Fatalf("unsupported %s CmdType response advertised SIP methods: %q", method, response)
				}
			})
		}
	}
}

func TestHandlerRequestMixedCaseMANSCDPMethodReachesCommandRoute(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	remoteURI, err := ParseSipURI("sip:34020000001320000001@192.0.2.30:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &localURI, Params: NewParams()})
	defer server.Close()
	handler := func(ctx *Context) {
		ctx.String(http.StatusOK, http.StatusText(http.StatusOK))
	}
	server.Message().Handle("Keepalive", handler)
	server.Notify().Handle("Keepalive", handler)

	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		for _, method := range []string{"MeSsAgE", "NoTiFy"} {
			t.Run(version+"/"+method, func(t *testing.T) {
				connection := newServerTransactionCaptureConnection()
				server.udpConn = connection
				body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>34020000001320000001</DeviceID></Notify>`)
				request := NewRequest("", method, &localURI, DefaultSipVersion,
					NewHeaderBuilder().SetMethod(method).
						SetFrom(&Address{URI: &remoteURI, Params: NewParams()}).
						SetTo(&Address{URI: &localURI, Params: NewParams()}).
						SetXGBVerValue(version).
						SetContentType(&ContentTypeXML).
						AddVia(&ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "192.0.2.30", Port: NewPort(5060), Params: NewParams()}).Build(), body)
				request.SetConnection(connection)
				request.SetSource(connection.remote)
				request.SetDestination(connection.local)

				server.handlerRequest(request)
				response := string(connection.waitPayload(t))
				if !strings.HasPrefix(response, "SIP/2.0 200 OK\r\n") {
					t.Fatalf("mixed-case %s response = %q", method, response)
				}
			})
		}
	}
}

func TestEarlyRegisterErrorResponseIncludesPlatformVersion(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	remoteURI, err := ParseSipURI("sip:34020000001320000001@192.0.2.30:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &localURI, Params: NewParams()})
	defer server.Close()
	connection := newServerTransactionCaptureConnection()
	request := NewRequest("", MethodRegister, &localURI, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodRegister).
			SetFrom(&Address{URI: &remoteURI, Params: NewParams()}).
			SetTo(&Address{URI: &localURI, Params: NewParams()}).
			AddVia(&ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "192.0.2.30", Port: NewPort(5060), Params: NewParams()}).Build(), nil)
	duplicate := defaultMaxForwards
	request.AppendHeader(&duplicate)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)

	server.handlerRequest(request)

	response := string(connection.waitPayload(t))
	if !strings.HasPrefix(response, "SIP/2.0 400 ") || strings.Count(response, "\r\nX-GB-Ver: 3.0\r\n") != 1 {
		t.Fatalf("early REGISTER error response = %q", response)
	}
}

func TestRegisterMiddlewareErrorResponseIncludesPlatformVersion(t *testing.T) {
	localURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	remoteURI, err := ParseSipURI("sip:34020000001320000001@192.0.2.30:5060")
	if err != nil {
		t.Fatal(err)
	}
	request := NewRequest("", MethodRegister, &localURI, DefaultSipVersion,
		NewHeaderBuilder().SetMethod(MethodRegister).
			SetFrom(&Address{URI: &remoteURI, Params: NewParams()}).
			SetTo(&Address{URI: &localURI, Params: NewParams()}).
			AddVia(&ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "192.0.2.30", Port: NewPort(5060), Params: NewParams()}).Build(), nil)
	connection := newServerTransactionCaptureConnection()
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	ctx := &Context{Request: request, Tx: NewTransaction("register-middleware-error", connection)}

	ctx.AbortString(503, "service stopped")

	response := string(connection.waitPayload(t))
	if !strings.HasPrefix(response, "SIP/2.0 503 service stopped\r\n") || strings.Count(response, "\r\nX-GB-Ver: 3.0\r\n") != 1 {
		t.Fatalf("REGISTER middleware error response = %q", response)
	}
}
