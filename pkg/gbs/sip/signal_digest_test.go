package sip

import (
	"context"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSignalDigestSignsAndVerifiesRequestAndResponse(t *testing.T) {
	now := time.Date(2024, 4, 1, 4, 5, 6, 0, time.UTC)
	security, err := NewSignalDigestSecurity(SignalDigestOptions{
		Seed: "shared-seed", Algorithm: "MD5", Encoding: SignalDigestEncodingBase64,
		Required: true, Window: 10 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := newSignalDigestTestRequest(t, MethodMessage, []byte("<Query><CmdType>Catalog</CmdType></Query>"))
	if err := security.Sign(request); err != nil {
		t.Fatal(err)
	}
	if got := request.GetHeaders("Date"); len(got) != 1 || got[0].String() != "Date: 2024-04-01T12:05:06" {
		t.Fatalf("signed Date = %#v", got)
	}
	if got := request.GetHeaders("Note"); len(got) != 1 || !strings.Contains(got[0].String(), "algorithm=MD5") {
		t.Fatalf("signed Note = %#v", got)
	}
	if err := security.Verify(request); err != nil {
		t.Fatalf("signed request verification failed: %v", err)
	}

	response := NewResponseFromRequest("", request, 200, "OK", []byte("result"))
	if err := security.Sign(response); err != nil {
		t.Fatal(err)
	}
	if err := security.Verify(response); err != nil {
		t.Fatalf("signed response verification failed: %v", err)
	}
}

func TestSignalDigestSM3KnownVectorAndRoundTrip(t *testing.T) {
	newHash, err := signalDigestHashFactory("SM3")
	if err != nil {
		t.Fatal(err)
	}
	hasher := newHash()
	_, _ = hasher.Write([]byte("abc"))
	if got := hex.EncodeToString(hasher.Sum(nil)); got != "66c7f0f462eeedd9d1f2d46bdc10e4e24167c4875cf2f7a2297da02b8f4ba8e0" {
		t.Fatalf("SM3(abc) = %s", got)
	}

	now := time.Date(2024, 4, 1, 4, 5, 6, 0, time.UTC)
	security, err := NewSignalDigestSecurity(SignalDigestOptions{
		Seed: "shared-seed", Algorithm: "SM3", Encoding: SignalDigestEncodingBase64,
		Required: true, Window: 10 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := newSignalDigestTestRequest(t, MethodMessage, []byte("SM3 payload"))
	if err := security.Sign(request); err != nil {
		t.Fatal(err)
	}
	if note := request.GetHeaders("Note"); len(note) != 1 || !strings.Contains(note[0].String(), "algorithm=SM3") {
		t.Fatalf("SM3 Note = %#v", note)
	}
	if err := security.Verify(request); err != nil {
		t.Fatalf("SM3 signed request rejected: %v", err)
	}
}

func TestSignalDigestRejectsTamperingAndExpiredDate(t *testing.T) {
	signedAt := time.Date(2024, 4, 1, 4, 5, 6, 0, time.UTC)
	signer, err := NewSignalDigestSecurity(SignalDigestOptions{
		Seed: "shared-seed", Algorithm: "SHA-256", Required: true, Now: func() time.Time { return signedAt },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := newSignalDigestTestRequest(t, MethodInvite, []byte("original"))
	if err := signer.Sign(request); err != nil {
		t.Fatal(err)
	}
	request.SetBody([]byte("tampered"), true)
	if err := signer.Verify(request); err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("tampered body result = %v", err)
	}
	request.SetBody([]byte("original"), true)

	verifier, err := NewSignalDigestSecurity(SignalDigestOptions{
		Seed: "shared-seed", Algorithm: "SHA-256", Required: true, Window: 10 * time.Minute,
		Now: func() time.Time { return signedAt.Add(11 * time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(request); err == nil || !strings.Contains(err.Error(), "window") {
		t.Fatalf("expired Date result = %v", err)
	}
}

func TestSignalDigestOptionalAndLegacyHexCompatibility(t *testing.T) {
	now := time.Date(2024, 4, 1, 4, 5, 6, 0, time.UTC)
	optional, err := NewSignalDigestSecurity(SignalDigestOptions{
		Seed: "shared-seed", Algorithm: "MD5", Required: false, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	unsigned := newSignalDigestTestRequest(t, MethodMessage, nil)
	if err := optional.Verify(unsigned); err != nil {
		t.Fatalf("optional unsigned message rejected: %v", err)
	}

	required, err := NewSignalDigestSecurity(SignalDigestOptions{
		Seed: "shared-seed", Algorithm: "MD5", Required: true, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := required.Verify(unsigned); err == nil {
		t.Fatal("required unsigned message accepted")
	}

	hexSigner, err := NewSignalDigestSecurity(SignalDigestOptions{
		Seed: "shared-seed", Algorithm: "MD5", Encoding: SignalDigestEncodingHex,
		Required: true, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy := newSignalDigestTestRequest(t, MethodMessage, []byte("legacy"))
	if err := hexSigner.Sign(legacy); err != nil {
		t.Fatal(err)
	}
	compat, err := NewSignalDigestSecurity(SignalDigestOptions{
		Seed: "shared-seed", Algorithm: "MD5", Encoding: SignalDigestEncodingBase64,
		Required: true, AcceptLegacyHex: true, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := compat.Verify(legacy); err != nil {
		t.Fatalf("legacy hex Note rejected: %v", err)
	}
}

func TestSignalDigestExemptsRegister(t *testing.T) {
	security, err := NewSignalDigestSecurity(SignalDigestOptions{Seed: "shared-seed", Required: true})
	if err != nil {
		t.Fatal(err)
	}
	request := newSignalDigestTestRequest(t, MethodRegister, nil)
	if err := security.Sign(request); err != nil {
		t.Fatal(err)
	}
	if len(request.GetHeaders("Date")) != 0 || len(request.GetHeaders("Note")) != 0 {
		t.Fatal("REGISTER was signal-digest signed")
	}
	if err := security.Verify(request); err != nil {
		t.Fatalf("REGISTER verification failed: %v", err)
	}
}

func TestRequestWithSecuritySignsBeforeWrite(t *testing.T) {
	now := time.Date(2024, 4, 1, 4, 5, 6, 0, time.UTC)
	security, err := NewSignalDigestSecurity(SignalDigestOptions{
		Seed: "shared-seed", Required: true, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	connection := NewTCPConnection(&sipTestTCPConn{
		Conn:   client,
		local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41000},
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
	})
	serverURI, err := ParseSipURI("sip:34020000002000000001@3402000000")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	request := newSignalDigestTestRequest(t, MethodMessage, []byte("payload"))
	request.SetConnection(connection)
	request.SetSource(connection.LocalAddr())
	request.SetDestination(connection.RemoteAddr())
	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, requestErr := server.RequestWithSecurity(request, security)
		done <- result{err: requestErr}
	}()
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 8192)
	n, err := peer.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if result := <-done; result.err != nil {
		t.Fatal(result.err)
	}
	payload := string(buffer[:n])
	if !strings.Contains(payload, "Date: 2024-04-01T12:05:06") || !strings.Contains(payload, "Note: Digest nonce=") {
		t.Fatalf("secured request payload = %s", payload)
	}
}

func TestTransactionSignalDigestAcceptsValidAndDiscardsInvalidResponses(t *testing.T) {
	now := time.Date(2024, 4, 1, 4, 5, 6, 0, time.UTC)
	security, err := NewSignalDigestSecurity(SignalDigestOptions{
		Seed: "shared-seed", Required: true, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := newSignalDigestTestRequest(t, MethodMessage, []byte("payload"))
	tx := &Transaction{key: "signal-digest-response", resp: make(chan *Response, 2), active: make(chan int, 2)}
	tx.SetMessageSecurity(security)

	unsigned := NewResponseFromRequest("", request, 200, "OK", nil)
	invalidDone := make(chan struct{})
	go func() {
		tx.receiveResponse(unsigned)
		close(invalidDone)
	}()
	select {
	case <-invalidDone:
	case <-time.After(time.Second):
		t.Fatal("invalid response verification blocked the transaction")
	}
	if len(tx.resp) != 0 {
		t.Fatal("unsigned response was delivered in Required mode")
	}

	valid := NewResponseFromRequest("", request, 200, "OK", nil)
	if err := security.Sign(valid); err != nil {
		t.Fatal(err)
	}
	tx.receiveResponse(valid)
	select {
	case got := <-tx.resp:
		if got != valid {
			t.Fatalf("delivered response = %p, want %p", got, valid)
		}
	case <-time.After(time.Second):
		t.Fatal("valid signed response was not delivered")
	}

	tampered := NewResponseFromRequest("", request, 200, "OK", []byte("original"))
	if err := security.Sign(tampered); err != nil {
		t.Fatal(err)
	}
	tampered.SetBody([]byte("tampered"), true)
	tx.receiveResponse(tampered)
	if len(tx.resp) != 0 {
		t.Fatal("tampered signed response was delivered")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if response, err := tx.GetResponseContext(waitCtx); err != context.DeadlineExceeded || response != nil {
		t.Fatalf("invalid response wait = response:%v err:%v", response, err)
	}
}

func TestTransactionGetResponseContextSkipsProvisionalResponse(t *testing.T) {
	tx := &Transaction{key: "context-response", resp: make(chan *Response, 2), active: make(chan int, 2)}
	provisional := NewResponse("", DefaultSipVersion, http.StatusContinue, "Trying", nil, nil)
	final := NewResponse("", DefaultSipVersion, http.StatusOK, "OK", nil, nil)
	tx.resp <- provisional
	tx.resp <- final
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := tx.GetResponseContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if response != final {
		t.Fatalf("final response = %p, want %p", response, final)
	}
}

func TestRequestSecurityResolverRejectsUnsignedRequestWithSignedResponse(t *testing.T) {
	now := time.Date(2024, 4, 1, 4, 5, 6, 0, time.UTC)
	security, err := NewSignalDigestSecurity(SignalDigestOptions{
		Seed: "shared-seed", Required: true, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	connection := NewTCPConnection(&sipTestTCPConn{
		Conn:   client,
		local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41001},
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
	})
	serverURI, err := ParseSipURI("sip:34020000002000000001@3402000000")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	server.SetRequestSecurityResolver(func(*Request) (MessageSecurity, error) { return security, nil })
	request := newSignalDigestTestRequest(t, MethodOptions, nil)
	request.SetConnection(connection)
	request.SetSource(connection.RemoteAddr())
	request.SetDestination(connection.LocalAddr())
	go server.handlerRequest(request)
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 8192)
	n, err := peer.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(buffer[:n])
	if !strings.Contains(payload, "SIP/2.0 403 invalid request security") ||
		!strings.Contains(payload, "Date: 2024-04-01T12:05:06") || !strings.Contains(payload, "Note: Digest nonce=") {
		t.Fatalf("secured rejection payload = %s", payload)
	}
}

func TestTransactionRespondDoesNotMutateCachedResponse(t *testing.T) {
	now := time.Date(2024, 4, 1, 4, 5, 6, 0, time.UTC)
	security, err := NewSignalDigestSecurity(SignalDigestOptions{
		Seed: "shared-seed", Required: true, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	connection := NewTCPConnection(&sipTestTCPConn{
		Conn: client, local: &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41001},
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
	})
	request := newSignalDigestTestRequest(t, MethodOptions, nil)
	request.SetSource(connection.RemoteAddr())
	request.SetDestination(connection.LocalAddr())
	response := NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	tx := NewTransaction("cached-response", connection)
	defer tx.Close()
	tx.SetMessageSecurity(security)

	done := make(chan error, 1)
	go func() { done <- tx.Respond(response) }()
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 8192)
	n, err := peer.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	payload := string(buffer[:n])
	if !strings.Contains(payload, "Date: 2024-04-01T12:05:06") || !strings.Contains(payload, "Note: Digest nonce=") {
		t.Fatalf("signed response payload = %s", payload)
	}
	if len(response.GetHeaders("Date")) != 0 || len(response.GetHeaders("Note")) != 0 {
		t.Fatalf("cached response was mutated: %s", response.String())
	}
}

func newSignalDigestTestRequest(t *testing.T, method string, body []byte) *Request {
	t.Helper()
	fromURI, err := ParseSipURI("sip:34020000002000000001@3402000000")
	if err != nil {
		t.Fatal(err)
	}
	toURI, err := ParseSipURI("sip:34020000001320000001@3402000000")
	if err != nil {
		t.Fatal(err)
	}
	from := &Address{URI: &fromURI, Params: NewParams()}
	to := &Address{URI: &toURI, Params: NewParams()}
	headers := NewHeaderBuilder().SetFrom(from).SetToWithParam(to).SetMethod(method).AddVia(&ViaHop{
		Host: "192.0.2.1", Port: NewPort(5060), Transport: "UDP",
		Params: NewParams().Add("branch", String{Str: GenerateBranch()}),
	}).Build()
	return NewRequest("signal-digest-call-id", method, &toURI, DefaultSipVersion, headers, body)
}
