package gbs

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestRegisterResponseIncludesPlatformVersion(t *testing.T) {
	deviceURI, err := sip.ParseSipURI("sip:34020000001320000001@3402000000")
	if err != nil {
		t.Fatal(err)
	}
	serverURI, err := sip.ParseSipURI("sip:34020000002000000001@3402000000")
	if err != nil {
		t.Fatal(err)
	}
	from := &sip.Address{URI: &deviceURI, Params: sip.NewParams()}
	to := &sip.Address{URI: &serverURI, Params: sip.NewParams()}
	hb := sip.NewHeaderBuilder().SetFrom(from).SetToWithParam(to).SetMethod(sip.MethodRegister).AddVia(&sip.ViaHop{
		Host:      "192.0.2.10",
		Port:      sip.NewPort(5060),
		Transport: "UDP",
		Params:    sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
	})
	req := sip.NewRequest("", sip.MethodRegister, &serverURI, sip.DefaultSipVersion, hb.Build(), nil)
	ctx := &sip.Context{Request: req}

	api := &GB28181API{}
	for _, status := range []int{200, 401, 403} {
		resp := api.newRegisterResponse(ctx, status, "test")
		headers := resp.GetHeaders("X-GB-Ver")
		if len(headers) != 1 || headers[0].String() != "X-GB-Ver: 3.0" {
			t.Fatalf("status %d X-GB-Ver headers = %#v", status, headers)
		}
	}
}

func TestRegisterRedirectUsesTrustedServerConfiguration(t *testing.T) {
	cfg := &conf.SIP{
		ID: gb10PlatformID, Domain: "3402000000",
		RegisterRedirect: "sip:" + gb10PlatformID + "@192.0.2.31:5070",
	}
	api := &GB28181API{cfg: cfg}
	request := newFlowRequest(t, newFlowConnection(), sip.MethodRegister, "register-redirect", nil)
	request.RemoveHeader("X-GB-Ver")
	request.AppendHeader(&sip.GenericHeader{HeaderName: "X-GB-Ver", Contents: string(GBVersion30)})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "X-GB-Redirect", Contents: "sip:" + gb10PlatformID + "@203.0.113.99:5090"})
	connection := newFlowConnection()
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("register-redirect", connection), DeviceID: gb10DeviceID,
		Source: connection.remote, To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), XGBVer: string(GBVersion30),
	}
	api.handlerRegister(ctx)
	select {
	case payload := <-connection.writes:
		text := string(payload)
		if !strings.Contains(text, "SIP/2.0 302 Moved Temporarily") ||
			!strings.Contains(text, "192.0.2.31:5070") || strings.Contains(text, "203.0.113.99") {
			t.Fatalf("REGISTER redirect response = %s", text)
		}
	default:
		t.Fatal("REGISTER redirect response missing")
	}
}

func TestParseRegisterExpires(t *testing.T) {
	conn := newFlowConnection()
	request := newFlowRequest(t, conn, sip.MethodRegister, "register-expires", nil)
	ctx := &sip.Context{Request: request}
	if got, err := parseRegisterExpires(ctx); err != nil || got != defaultRegisterExpires {
		t.Fatalf("default expires = %d, %v", got, err)
	}

	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "3600"})
	if got, err := parseRegisterExpires(ctx); err != nil || got != 3600 {
		t.Fatalf("header expires = %d, %v", got, err)
	}
	request.RemoveHeader("Expires")
	contact := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	contact.Params.Add("expires", sip.String{Str: "7200"})
	request.AppendHeader(&sip.ContactHeader{Address: contact.URI, Params: contact.Params})
	if got, err := parseRegisterExpires(ctx); err != nil || got != 7200 {
		t.Fatalf("Contact expires = %d, %v", got, err)
	}
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "3600"})
	if got, err := parseRegisterExpires(ctx); err != nil || got != 7200 {
		t.Fatalf("Contact expires did not override Expires header: %d, %v", got, err)
	}
	request.RemoveHeader("Contact")
	unregisterContact := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	unregisterContact.Params.Add("expires", sip.String{Str: "0"})
	request.AppendHeader(&sip.ContactHeader{Address: unregisterContact.URI, Params: unregisterContact.Params})
	if got, err := parseRegisterExpires(ctx); err != nil || got != 0 {
		t.Fatalf("Contact expires=0 did not override Expires header: %d, %v", got, err)
	}

	request.RemoveHeader("Contact")
	request.RemoveHeader("Expires")
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "invalid"})
	if _, err := parseRegisterExpires(ctx); err == nil {
		t.Fatal("invalid expires accepted")
	}
}

func TestRegisterDigestRequiresIssuedNonceAndRejectsReplay(t *testing.T) {
	const password = "secret"
	cfg := &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"}
	api := &GB28181API{cfg: cfg, registerNonces: make(map[string]registerNonceState)}
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodRegister, "register-auth-1", nil)
	ctx := &sip.Context{Request: request, DeviceID: gb10DeviceID, Source: connection.remote}
	nonce := api.issueRegisterNonce(gb10DeviceID, registerNonceSourceIP(ctx))

	auth := sip.AuthFromValue(fmt.Sprintf(`Digest realm="%s",nonce="%s",algorithm=MD5,qop="auth"`, cfg.Domain, nonce)).
		SetUsername(gb10DeviceID).
		SetPassword(password).
		SetMethod(sip.MethodRegister).
		SetURI(request.Recipient().String()).
		SetClientNonce("00000001", "client-nonce")
	if _, err := auth.CalcResponseChecked(); err != nil {
		t.Fatal(err)
	}
	header := &sip.GenericHeader{HeaderName: "Authorization", Contents: auth.String()}
	if err := api.validateRegisterAuthorization(ctx, header, gb10DeviceID, password); err != nil {
		t.Fatalf("valid REGISTER Digest rejected: %v", err)
	}
	// UDP 重传的 Call-ID/CSeq 和摘要完全相同，应保持幂等。
	if err := api.validateRegisterAuthorization(ctx, header, gb10DeviceID, password); err != nil {
		t.Fatalf("exact REGISTER retransmission rejected: %v", err)
	}

	replayed := newFlowRequest(t, connection, sip.MethodRegister, "register-auth-2", nil)
	replayCtx := &sip.Context{Request: replayed, DeviceID: gb10DeviceID, Source: connection.remote}
	if err := api.validateRegisterAuthorization(replayCtx, header, gb10DeviceID, password); err == nil || !strings.Contains(err.Error(), "replay") {
		t.Fatalf("Digest nonce replay result = %v", err)
	}

	legacyRequest := newFlowRequest(t, connection, sip.MethodRegister, "register-auth-legacy", nil)
	legacyCtx := &sip.Context{Request: legacyRequest, DeviceID: gb10DeviceID, Source: connection.remote}
	legacyNonce := api.issueRegisterNonce(gb10DeviceID, registerNonceSourceIP(legacyCtx))
	legacy := sip.AuthFromValue(fmt.Sprintf(`Digest realm="%s",nonce="%s",algorithm=MD5`, cfg.Domain, legacyNonce)).
		SetUsername(gb10DeviceID).
		SetPassword(password).
		SetMethod(sip.MethodRegister).
		SetURI(legacyRequest.Recipient().String())
	_, _ = legacy.CalcResponseChecked()
	if err := api.validateRegisterAuthorization(legacyCtx, &sip.GenericHeader{HeaderName: "Authorization", Contents: legacy.String()}, gb10DeviceID, password); err != nil {
		t.Fatalf("legacy qop-less REGISTER Digest rejected: %v", err)
	}

	unknown := sip.AuthFromValue(fmt.Sprintf(`Digest realm="%s",nonce="not-issued",algorithm=MD5,qop="auth"`, cfg.Domain)).
		SetUsername(gb10DeviceID).
		SetPassword(password).
		SetMethod(sip.MethodRegister).
		SetURI(request.Recipient().String()).
		SetClientNonce("00000001", "client-nonce")
	_, _ = unknown.CalcResponseChecked()
	if err := api.validateRegisterAuthorization(ctx, &sip.GenericHeader{HeaderName: "Authorization", Contents: unknown.String()}, gb10DeviceID, password); err == nil || !strings.Contains(err.Error(), "not issued") {
		t.Fatalf("unissued Digest nonce result = %v", err)
	}
}

func TestRegisterDigestBindsNonceAndAlgorithm(t *testing.T) {
	api := &GB28181API{
		cfg:            &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"},
		registerNonces: make(map[string]registerNonceState),
	}
	nonce := api.issueRegisterNonce(gb10DeviceID, "192.0.2.10")
	if err := api.validateRegisterNonce(nonce, gb10DeviceID, "192.0.2.11"); err == nil || !strings.Contains(err.Error(), "source") {
		t.Fatalf("cross-source nonce result = %v", err)
	}
	if err := api.validateRegisterNonce(nonce, "34020000001320000009", "192.0.2.10"); err == nil || !strings.Contains(err.Error(), "device") {
		t.Fatalf("cross-device nonce result = %v", err)
	}

	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodRegister, "register-sha256", nil)
	ctx := &sip.Context{Request: request, DeviceID: gb10DeviceID, Source: connection.remote}
	shaNonce := api.issueRegisterNonce(gb10DeviceID, registerNonceSourceIP(ctx))
	auth := sip.AuthFromValue(fmt.Sprintf(`Digest realm="%s",nonce="%s",algorithm=SHA-256,qop="auth"`, api.cfg.Domain, shaNonce)).
		SetUsername(gb10DeviceID).
		SetPassword("secret").
		SetMethod(sip.MethodRegister).
		SetURI(request.Recipient().String()).
		SetClientNonce("00000001", "client-nonce")
	_, _ = auth.CalcResponseChecked()
	if err := api.validateRegisterAuthorization(ctx, &sip.GenericHeader{HeaderName: "Authorization", Contents: auth.String()}, gb10DeviceID, "secret"); err == nil || !strings.Contains(err.Error(), "algorithm") {
		t.Fatalf("unadvertised REGISTER algorithm result = %v", err)
	}
}

func TestRegisterDigestUsesDomainDerivedFromPlatformID(t *testing.T) {
	const password = "secret"
	cfg := &conf.SIP{ID: gb10PlatformID}
	api := &GB28181API{cfg: cfg, registerNonces: make(map[string]registerNonceState)}
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodRegister, "register-derived-domain", nil)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("register-derived-domain", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
	}

	api.respondRegisterChallenge(ctx)
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), `realm="3402000000"`) {
			t.Fatalf("REGISTER challenge did not use derived domain: %s", payload)
		}
	default:
		t.Fatal("REGISTER challenge response missing")
	}

	nonce := api.issueRegisterNonce(gb10DeviceID, registerNonceSourceIP(ctx))
	auth := sip.AuthFromValue(fmt.Sprintf(`Digest realm="%s",nonce="%s",algorithm=MD5,qop="auth"`, cfg.GetDomain(), nonce)).
		SetUsername(gb10DeviceID).
		SetPassword(password).
		SetMethod(sip.MethodRegister).
		SetURI(request.Recipient().String()).
		SetClientNonce("00000001", "client-nonce")
	if _, err := auth.CalcResponseChecked(); err != nil {
		t.Fatal(err)
	}
	if err := api.validateRegisterAuthorization(ctx, &sip.GenericHeader{HeaderName: "Authorization", Contents: auth.String()}, gb10DeviceID, password); err != nil {
		t.Fatalf("REGISTER Digest using derived domain rejected: %v", err)
	}
}
