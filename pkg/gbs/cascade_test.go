package gbs

import (
	"context"
	"net"
	"net/http"
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
		{name: "tcp unsupported", contact: "sip:" + gb10PlatformID + "@192.0.2.31:5070;transport=tcp"},
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
