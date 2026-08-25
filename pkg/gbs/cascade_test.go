package gbs

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func testCascadePlatform(t *testing.T, version string) cascadePlatform {
	t.Helper()
	platform, err := normalizeCascadePlatform(conf.SIPUpstream{
		Name: "provincial", Enabled: true,
		ServerID: gb10PlatformID, Host: "192.0.2.30", Port: 5060,
		Domain: "remote.example", LocalID: gb10DeviceID, LocalDomain: "local.example",
		LocalHost: "192.0.2.20", Password: "cascade-secret",
		Version: version, Expires: 3600, KeepaliveInterval: conf.Duration(30 * time.Second),
	}, conf.SIP{ID: gb10DeviceID, Domain: "local.example", Host: "192.0.2.20", Port: 5060}, "")
	if err != nil {
		t.Fatal(err)
	}
	return platform
}

func testCascadeTCPPlatform(t *testing.T, version string) cascadePlatform {
	t.Helper()
	platform, err := normalizeCascadePlatform(conf.SIPUpstream{
		Name: "provincial-tcp", Enabled: true,
		ServerID: gb10PlatformID, Host: "192.0.2.30", Port: 5060, Transport: "tcp",
		Domain: "remote.example", LocalID: gb10DeviceID, LocalDomain: "local.example",
		LocalHost: "192.0.2.20", Password: "cascade-secret",
		Version: version, Expires: 3600, KeepaliveInterval: conf.Duration(30 * time.Second),
	}, conf.SIP{ID: gb10DeviceID, Domain: "local.example", Host: "192.0.2.20", Port: 5060}, "")
	if err != nil {
		t.Fatal(err)
	}
	return platform
}

func TestCascadeRegisterDigestAndVersionNegotiation(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, "2.0"))
	requests := make([]*sip.Request, 0, 2)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		if len(requests) == 1 {
			response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
			response.AppendHeader(&sip.GenericHeader{
				HeaderName: "WWW-Authenticate",
				Contents:   `Digest realm="3402000000",qop="auth",nonce="cascade-nonce",opaque="registrar-token"`,
			})
			return response, nil
		}
		response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
		version := sip.XGBVer("1.1")
		response.AppendHeader(&version)
		response.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "120"})
		return response, nil
	}

	if err := worker.register(t.Context(), worker.platform.expires); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("REGISTER request count = %d", len(requests))
	}
	firstCallID, _ := requests[0].CallID()
	secondCallID, _ := requests[1].CallID()
	if firstCallID == nil || secondCallID == nil || *firstCallID != *secondCallID {
		t.Fatalf("Digest retry changed Call-ID: %v / %v", firstCallID, secondCallID)
	}
	firstCSeq, _ := requests[0].CSeq()
	secondCSeq, _ := requests[1].CSeq()
	if firstCSeq == nil || secondCSeq == nil || firstCSeq.SeqNo != 1 || secondCSeq.SeqNo != 2 {
		t.Fatalf("REGISTER CSeq = %v / %v", firstCSeq, secondCSeq)
	}
	assertCascadeRegisterAddressing(t, requests[0])
	authorizationHeaders := requests[1].GetHeaders("Authorization")
	if len(authorizationHeaders) != 1 {
		t.Fatal("authenticated REGISTER missing Authorization")
	}
	auth := sip.AuthFromValue(authorizationHeaders[0].String())
	expected := sip.CalcResponse(
		gb10DeviceID, "3402000000", "cascade-secret", sip.MethodRegister,
		requests[1].Recipient().String(), "cascade-nonce", "auth", auth.Get("cnonce"), "00000001",
	)
	if auth.Get("username") != gb10DeviceID || auth.Get("response") != expected || auth.Get("nc") != "00000001" || auth.Get("opaque") != "registrar-token" {
		t.Fatalf("unexpected Digest Authorization: %s", authorizationHeaders[0].String())
	}
	if worker.effective != GBVersion11 {
		t.Fatalf("negotiated version = %s; want 1.1", worker.effective)
	}
	status := worker.snapshot()
	if !status.Registered || status.State != "registered" || status.NegotiatedVersion != "1.1" || status.ExpiresAt.IsZero() || worker.accepted != 120 {
		t.Fatalf("cascade status = %+v", status)
	}
	remaining := time.Until(status.ExpiresAt)
	if remaining < 115*time.Second || remaining > 120*time.Second {
		t.Fatalf("cascade accepted expiry remaining = %s", remaining)
	}
}

func assertCascadeRegisterAddressing(t *testing.T, request *sip.Request) {
	t.Helper()
	if got := request.Recipient().String(); got != "sip:"+gb10PlatformID+"@remote.example" {
		t.Fatalf("REGISTER Request-URI = %s", got)
	}
	from, ok := request.From()
	if !ok || from == nil || from.Address == nil || from.Address.String() != "sip:"+gb10DeviceID+"@local.example" {
		t.Fatalf("REGISTER From = %v", from)
	}
	to, ok := request.To()
	if !ok || to == nil || to.Address == nil || to.Address.String() != "sip:"+gb10DeviceID+"@local.example" {
		t.Fatalf("REGISTER To = %v", to)
	}
	contact, ok := request.Contact()
	if !ok || contact == nil || contact.Address == nil || contact.Address.String() != "sip:"+gb10DeviceID+"@192.0.2.20:5060" {
		t.Fatalf("REGISTER Contact = %v", contact)
	}
}

func TestCascadeRegisterSupportsLegacyDigestWithoutQOP(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, "1.0"))
	var authenticated *sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		if len(request.GetHeaders("Authorization")) == 0 {
			response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
			response.AppendHeader(&sip.GenericHeader{HeaderName: "WWW-Authenticate", Contents: `Digest realm="3402000000",nonce="legacy-nonce"`})
			return response, nil
		}
		authenticated = request
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	if err := worker.register(t.Context(), 3600); err != nil {
		t.Fatal(err)
	}
	auth := sip.AuthFromValue(authenticated.GetHeaders("Authorization")[0].String())
	if auth.Get("qop") != "" || auth.Get("cnonce") != "" || auth.Get("nc") != "" {
		t.Fatalf("legacy Digest unexpectedly contains qop fields: %s", authenticated.GetHeaders("Authorization")[0].String())
	}
}

func TestCascadeDigestSupportsSHA256AndRejectsUnsupportedAlgorithm(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, "3.0"))
	request := worker.newRegisterRequest(3600, nil)
	response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
	response.AppendHeader(&sip.GenericHeader{
		HeaderName: "WWW-Authenticate",
		Contents:   `Digest realm="3402000000",nonce="sha256-nonce",algorithm=SHA-256,qop="auth,auth-int"`,
	})
	auth, err := cascadeDigestAuthorization(response, request, gb10DeviceID, "cascade-secret")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := sip.CalcResponseWithAlgorithm(
		"SHA-256", gb10DeviceID, "3402000000", "cascade-secret", sip.MethodRegister,
		request.Recipient().String(), "sha256-nonce", "auth", auth.Get("cnonce"), "00000001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Algorithm() != "SHA-256" || auth.Get("response") != expected || auth.QOP() != "auth" {
		t.Fatalf("SHA-256 Digest Authorization = %s", auth.String())
	}

	unsupported := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
	unsupported.AppendHeader(&sip.GenericHeader{
		HeaderName: "WWW-Authenticate",
		Contents:   `Digest realm="3402000000",nonce="sm3-nonce",algorithm=SM3,qop="auth"`,
	})
	if _, err := cascadeDigestAuthorization(unsupported, request, gb10DeviceID, "cascade-secret"); err == nil {
		t.Fatal("unsupported Digest algorithm accepted")
	}
}

func TestCascadeRegisterOverTCPReusesConnectionForDigest(t *testing.T) {
	platform := testCascadeTCPPlatform(t, "3.0")
	localURI, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	sipServer := sip.NewServer(&sip.Address{URI: &localURI, Params: sip.NewParams()})
	defer sipServer.Close()
	worker := newCascadeWorker(&Server{Server: sipServer}, platform)
	defer worker.closeTCPConnection()

	registrarErr := make(chan error, 1)
	allowRegistrarClose := make(chan struct{})
	dialCalls := 0
	worker.dialTCP = func(_ context.Context, _ string) (net.Conn, error) {
		dialCalls++
		client, registrar := net.Pipe()
		clientConn := &cascadeTestTCPConn{
			Conn:   client,
			local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41000},
			remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		}
		go func() {
			defer registrar.Close()
			reader := bufio.NewReader(registrar)
			for index := 0; index < 3; index++ {
				request, readErr := readCascadeTestTCPMessage(reader)
				if readErr != nil {
					registrarErr <- readErr
					return
				}
				if !strings.Contains(request, "Via: SIP/2.0/TCP") || !strings.Contains(request, ";transport=tcp") {
					registrarErr <- fmt.Errorf("TCP REGISTER missing transport markers: %s", request)
					return
				}
				status, reason := http.StatusUnauthorized, "Unauthorized"
				extra := `WWW-Authenticate: Digest realm="3402000000",qop="auth",nonce="tcp-nonce"`
				if index == 1 {
					if !strings.Contains(request, "Authorization: Digest") {
						registrarErr <- fmt.Errorf("authenticated TCP REGISTER missing Authorization")
						return
					}
					status, reason = http.StatusOK, "OK"
					extra = "Expires: 120\r\nX-GB-Ver: 3.0"
				} else if index == 2 {
					if !strings.HasPrefix(request, "MESSAGE ") || !strings.Contains(request, "<CmdType>Keepalive</CmdType>") {
						registrarErr <- fmt.Errorf("unexpected TCP keepalive request: %s", request)
						return
					}
					status, reason, extra = http.StatusOK, "OK", ""
				}
				if _, writeErr := io.WriteString(registrar, cascadeTestTCPResponse(request, status, reason, extra)); writeErr != nil {
					registrarErr <- writeErr
					return
				}
			}
			registrarErr <- nil
			<-allowRegistrarClose
		}()
		return clientConn, nil
	}

	if err := worker.register(t.Context(), worker.platform.expires); err != nil {
		t.Fatal(err)
	}
	if dialCalls != 1 {
		t.Fatalf("TCP cascade dial calls = %d, want 1", dialCalls)
	}
	if status := worker.snapshot(); !status.Registered || status.Address != "192.0.2.30:5060" || status.NegotiatedVersion != "3.0" {
		t.Fatalf("TCP cascade status = %+v", status)
	}
	manager := NewCascadeManager(nil)
	manager.items[worker.platform.name] = worker
	if matched, ok := manager.matchRegistered(gb10PlatformID, &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}); !ok || matched != worker {
		t.Fatalf("registered TCP upstream source match = %v, %v", matched, ok)
	}
	keepaliveDone := make(chan error, 1)
	go func() { keepaliveDone <- worker.keepalive(t.Context()) }()
	select {
	case err := <-keepaliveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("TCP keepalive timed out")
	}
	if dialCalls != 1 {
		t.Fatalf("TCP keepalive opened another connection: dials=%d", dialCalls)
	}
	if err := <-registrarErr; err != nil {
		t.Fatal(err)
	}
	close(allowRegistrarClose)
}

type cascadeTestTCPConn struct {
	net.Conn
	local  net.Addr
	remote net.Addr
}

func (c *cascadeTestTCPConn) LocalAddr() net.Addr  { return c.local }
func (c *cascadeTestTCPConn) RemoteAddr() net.Addr { return c.remote }

func readCascadeTestTCPMessage(reader *bufio.Reader) (string, error) {
	var message strings.Builder
	bodyLength := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		message.WriteString(line)
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		name, value, found := strings.Cut(trimmed, ":")
		if found && (strings.EqualFold(strings.TrimSpace(name), "Content-Length") || strings.EqualFold(strings.TrimSpace(name), "l")) {
			bodyLength, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return "", err
			}
		}
	}
	if bodyLength > 0 {
		body := make([]byte, bodyLength)
		if _, err := io.ReadFull(reader, body); err != nil {
			return "", err
		}
		message.Write(body)
	}
	return message.String(), nil
}

func cascadeTestTCPResponse(request string, status int, reason, extra string) string {
	header := func(name string) string {
		for line := range strings.SplitSeq(request, "\r\n") {
			key, value, ok := strings.Cut(line, ":")
			if ok && strings.EqualFold(strings.TrimSpace(key), name) {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	to := header("To")
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=tcp-registrar"
	}
	if extra != "" {
		extra += "\r\n"
	}
	return fmt.Sprintf("SIP/2.0 %d %s\r\nVia: %s\r\nFrom: %s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: %s\r\n%sContent-Length: 0\r\n\r\n",
		status, reason, header("Via"), header("From"), to, header("Call-ID"), header("CSeq"), extra)
}

func TestCascadeRegisterFollows2022Redirect(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, "3.0"))
	requests := make([]*sip.Request, 0, 3)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		switch len(requests) {
		case 1:
			response := sip.NewResponseFromRequest("", request, http.StatusMovedPermanently, "Moved Permanently", nil)
			redirectURI, err := sip.ParseSipURI("sip:" + gb10PlatformID + "@192.0.2.31:5070")
			if err != nil {
				t.Fatal(err)
			}
			response.AppendHeader(&sip.ContactHeader{Address: &redirectURI, Params: sip.NewParams()})
			return response, nil
		case 2:
			response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
			response.AppendHeader(&sip.GenericHeader{HeaderName: "WWW-Authenticate", Contents: `Digest realm="3402000000",qop="auth",nonce="redirect-nonce"`})
			return response, nil
		default:
			return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
		}
	}

	if err := worker.register(t.Context(), worker.platform.expires); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 {
		t.Fatalf("redirect REGISTER request count = %d", len(requests))
	}
	for _, request := range requests[1:] {
		if got := request.Recipient().String(); got != "sip:"+gb10PlatformID+"@192.0.2.31:5070" {
			t.Fatalf("redirect REGISTER Request-URI = %s", got)
		}
		if got := request.Destination().String(); got != "192.0.2.31:5070" {
			t.Fatalf("redirect REGISTER destination = %s", got)
		}
	}
	auth := sip.AuthFromValue(requests[2].GetHeaders("Authorization")[0].String())
	if auth.Get("uri") != requests[2].Recipient().String() {
		t.Fatalf("redirect Digest uri = %q; want %q", auth.Get("uri"), requests[2].Recipient().String())
	}
	if status := worker.snapshot(); status.Address != "192.0.2.31:5070" || !status.Registered {
		t.Fatalf("redirect registration status = %+v", status)
	}
	manager := NewCascadeManager(nil)
	manager.items[worker.platform.name] = worker
	if matched, ok := manager.matchRegistered(gb10PlatformID, &net.UDPAddr{IP: net.ParseIP("192.0.2.31"), Port: 5070}); !ok || matched != worker {
		t.Fatalf("redirected upstream source match = %v, %v", matched, ok)
	}
	if _, ok := manager.matchRegistered(gb10PlatformID, worker.platform.remote); ok {
		t.Fatal("configured upstream source remained authorized after redirect")
	}

	var keepalive *sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		keepalive = request
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	if err := worker.keepalive(t.Context()); err != nil {
		t.Fatal(err)
	}
	if keepalive == nil || keepalive.Recipient().String() != "sip:"+gb10PlatformID+"@192.0.2.31:5070" || keepalive.Destination().String() != "192.0.2.31:5070" {
		t.Fatalf("redirect keepalive target = %#v", keepalive)
	}
}

func TestCascadeRegisterRejectsUnsafeRedirect(t *testing.T) {
	tests := []struct {
		name    string
		contact string
	}{
		{name: "different server", contact: "sip:34020000002000009999@192.0.2.31:5070"},
		{name: "sips unsupported", contact: "sips:" + gb10PlatformID + "@192.0.2.31:5071"},
		{name: "transport unsupported", contact: "sip:" + gb10PlatformID + "@192.0.2.31:5070;transport=ws"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worker := newCascadeWorker(nil, testCascadePlatform(t, "3.0"))
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				response := sip.NewResponseFromRequest("", request, http.StatusMovedPermanently, "Moved Permanently", nil)
				redirectURI, err := sip.ParseSipURI(test.contact)
				if err != nil {
					t.Fatal(err)
				}
				response.AppendHeader(&sip.ContactHeader{Address: &redirectURI, Params: sip.NewParams()})
				return response, nil
			}
			if err := worker.register(t.Context(), worker.platform.expires); err == nil || !strings.Contains(strings.ToLower(err.Error()), "redirect") {
				t.Fatalf("unsafe redirect error = %v", err)
			}
		})
	}
}

func TestCascadeRegisterRedirectSupportsTCPTransport(t *testing.T) {
	request := sip.NewRequest("", sip.MethodRegister, &sip.URI{FHost: "remote.example"}, sip.DefaultSipVersion, nil, nil)
	response := sip.NewResponseFromRequest("", request, http.StatusMovedPermanently, "Moved Permanently", nil)
	redirectURI, err := sip.ParseSipURI("sip:" + gb10PlatformID + "@192.0.2.31:5070;transport=tcp")
	if err != nil {
		t.Fatal(err)
	}
	response.AppendHeader(&sip.ContactHeader{Address: &redirectURI, Params: sip.NewParams()})
	uri, remote, err := cascadeRegisterRedirectTarget(response, gb10PlatformID, "udp")
	if err != nil {
		t.Fatal(err)
	}
	if uri == nil || remote == nil || cascadeTransportForAddr(remote) != "tcp" || remote.String() != "192.0.2.31:5070" {
		t.Fatalf("TCP redirect target = %v / %v", uri, remote)
	}
}

func TestCascadeRegisterRedirectUpdatesRequestTransport(t *testing.T) {
	tests := []struct {
		name          string
		platform      func(*testing.T, string) cascadePlatform
		redirect      string
		wantTransport string
	}{
		{name: "udp to tcp", platform: testCascadePlatform, redirect: "tcp", wantTransport: "TCP"},
		{name: "tcp to udp", platform: testCascadeTCPPlatform, redirect: "udp", wantTransport: "UDP"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worker := newCascadeWorker(nil, test.platform(t, "3.0"))
			requests := make([]*sip.Request, 0, 2)
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				requests = append(requests, request)
				if len(requests) == 1 {
					response := sip.NewResponseFromRequest("", request, http.StatusMovedPermanently, "Moved Permanently", nil)
					uri, err := sip.ParseSipURI("sip:" + gb10PlatformID + "@192.0.2.31:5070;transport=" + test.redirect)
					if err != nil {
						t.Fatal(err)
					}
					response.AppendHeader(&sip.ContactHeader{Address: &uri, Params: sip.NewParams()})
					return response, nil
				}
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}
			if err := worker.register(t.Context(), worker.platform.expires); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 2 {
				t.Fatalf("redirect request count = %d", len(requests))
			}
			via, _ := requests[1].ViaHop()
			contact, _ := requests[1].Contact()
			if via == nil || via.Transport != test.wantTransport || cascadeTransportForAddr(requests[1].Destination()) != strings.ToLower(test.wantTransport) {
				t.Fatalf("redirect transport = via %v destination %v", via, requests[1].Destination())
			}
			hasTCPContact := contact != nil && contact.Address != nil && strings.Contains(strings.ToLower(contact.Address.String()), "transport=tcp")
			if hasTCPContact != (test.wantTransport == "TCP") {
				t.Fatalf("redirect Contact = %v", contact)
			}
		})
	}
}

func TestCascadeKeepaliveUsesNegotiatedVersion(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, "3.0"))
	worker.effective = GBVersion20
	var request *sip.Request
	worker.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		request = in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}
	if err := worker.keepalive(t.Context()); err != nil {
		t.Fatal(err)
	}
	if request == nil || request.Method() != sip.MethodMessage {
		t.Fatalf("Keepalive request = %#v", request)
	}
	from, _ := request.From()
	to, _ := request.To()
	if from == nil || from.Address.String() != "sip:"+gb10DeviceID+"@local.example" || to == nil || to.Address.String() != "sip:"+gb10PlatformID+"@remote.example" {
		t.Fatalf("Keepalive From/To = %v / %v", from, to)
	}
	if got := request.GetHeaders("X-GB-Ver"); len(got) != 1 || !strings.Contains(got[0].String(), "2.0") {
		t.Fatalf("Keepalive X-GB-Ver = %v", got)
	}
	var body MessageNotify
	if err := sip.XMLDecode(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if body.CmdType != "Keepalive" || body.DeviceID != gb10DeviceID || body.Status != "OK" {
		t.Fatalf("Keepalive body = %+v", body)
	}
}

func TestCascadeUnregisterUsesDigestAndExpiresZero(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, "1.1"))
	worker.updateStatus(func(status *CascadePlatformStatus) {
		status.State = "registered"
		status.Registered = true
	})
	requests := make([]*sip.Request, 0, 2)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		if len(request.GetHeaders("Authorization")) == 0 {
			response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
			response.AppendHeader(&sip.GenericHeader{HeaderName: "WWW-Authenticate", Contents: `Digest realm="3402000000",nonce="logout-nonce"`})
			return response, nil
		}
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}

	worker.unregisterOnStop()
	if len(requests) != 2 {
		t.Fatalf("unregister request count = %d", len(requests))
	}
	for _, request := range requests {
		headers := request.GetHeaders("Expires")
		if len(headers) != 1 || !strings.HasSuffix(headers[0].String(), ": 0") {
			t.Fatalf("unregister Expires = %v", headers)
		}
		assertCascadeRegisterAddressing(t, request)
	}
	if len(requests[1].GetHeaders("Authorization")) != 1 {
		t.Fatal("authenticated unregister missing Authorization")
	}
	status := worker.snapshot()
	if status.State != "stopped" || status.Registered || !status.ExpiresAt.IsZero() {
		t.Fatalf("unregister status = %+v", status)
	}
}

func TestNormalizeCascadePlatformsRejectsUnsafeConfiguration(t *testing.T) {
	local := conf.SIP{ID: gb10DeviceID, Domain: "3402000000", Host: "192.0.2.20", Port: 5060}
	base := conf.SIPUpstream{
		Name: "same", Enabled: true, ServerID: gb10PlatformID,
		Host: "192.0.2.30", LocalID: gb10DeviceID, Version: "1.1",
	}
	if _, err := normalizeCascadePlatforms(local, []conf.SIPUpstream{base, base}, ""); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate upstream error = %v", err)
	}
	invalid := base
	invalid.Version = "9.9"
	if _, err := normalizeCascadePlatforms(local, []conf.SIPUpstream{invalid}, ""); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("invalid version error = %v", err)
	}
	invalid = base
	invalid.ServerID = "not-a-gb-id"
	if _, err := normalizeCascadePlatforms(local, []conf.SIPUpstream{invalid}, ""); err == nil || !strings.Contains(err.Error(), "server_id") {
		t.Fatalf("invalid server ID error = %v", err)
	}
	invalid = base
	invalid.Transport = "sctp"
	if _, err := normalizeCascadePlatforms(local, []conf.SIPUpstream{invalid}, ""); err == nil || !strings.Contains(err.Error(), "transport") {
		t.Fatalf("invalid transport error = %v", err)
	}
	invalid = base
	local.Port = 0
	if _, err := normalizeCascadePlatforms(local, []conf.SIPUpstream{invalid}, ""); err == nil || !strings.Contains(err.Error(), "local SIP port") {
		t.Fatalf("invalid local SIP port error = %v", err)
	}
}

func TestCascadeAcceptedExpiresRejectsInvalidResponse(t *testing.T) {
	response := sip.NewResponse("", sip.DefaultSipVersion, http.StatusOK, "OK", nil, nil)
	response.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "0"})
	if _, err := cascadeAcceptedExpires(response, 3600); err == nil {
		t.Fatal("zero accepted expiry should fail")
	}
}

func TestCascadeManagerSerializesConcurrentApplyAndClose(t *testing.T) {
	manager := NewCascadeManager(nil)
	local := conf.SIP{ID: gb10DeviceID, Domain: "local.example", Host: "192.0.2.20", Port: 5060}
	upstream := conf.SIPUpstream{
		Name: "provincial", Enabled: true, ServerID: gb10PlatformID,
		Domain: "remote.example", Host: "192.0.2.30", Port: 5060,
		LocalDomain: "local.example", Version: "1.1",
	}

	done := make(chan error, 2)
	go func() { done <- manager.Apply(local, []conf.SIPUpstream{upstream}) }()
	go func() { done <- manager.Apply(local, []conf.SIPUpstream{upstream}) }()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent cascade Apply deadlocked")
		}
	}

	closed := make(chan struct{})
	go func() {
		manager.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("cascade Close deadlocked")
	}
}
