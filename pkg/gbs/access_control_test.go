package gbs

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestMessageDigestUsesDerivedDomainAndBindsRequestURI(t *testing.T) {
	const password = "secret"
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Password = password
	api := &GB28181API{
		cfg: &conf.SIP{ID: gb10PlatformID, RequireMessageAuth: true},
		svr: &Server{memoryStorer: memory},
	}
	connection := newFlowConnection()

	newContext := func(callID string) (*sip.Context, *sip.Request) {
		request := newFlowRequest(t, connection, sip.MethodMessage, callID, nil)
		return &sip.Context{
			Request: request, Tx: sip.NewTransaction(callID, connection),
			DeviceID: gb10DeviceID, Source: connection.remote,
		}, request
	}
	newAuthorization := func(realm, uri, nonce, nc, clientPassword string) *sip.GenericHeader {
		auth := sip.AuthFromValue(fmt.Sprintf(`Digest realm="%s",nonce="%s",algorithm=MD5,qop="auth"`, realm, nonce)).
			SetUsername(gb10DeviceID).
			SetPassword(clientPassword).
			SetMethod(sip.MethodMessage).
			SetURI(uri).
			SetClientNonce(nc, "client-nonce")
		if _, err := auth.CalcResponseChecked(); err != nil {
			t.Fatal(err)
		}
		return &sip.GenericHeader{HeaderName: "Authorization", Contents: auth.String()}
	}

	ctx, request := newContext("message-derived-domain")
	nonce := api.issueMessageNonce(gb10DeviceID, parseAddressIP(addrString(connection.remote)))
	request.AppendHeader(newAuthorization(api.cfg.GetDomain(), request.Recipient().String(), nonce, "00000001", password))
	if err := api.checkDigestAuth(ctx); err != nil {
		t.Fatalf("MESSAGE Digest using derived domain rejected: %v", err)
	}
	if err := api.checkDigestAuth(ctx); err != nil {
		t.Fatalf("exact MESSAGE retransmission rejected: %v", err)
	}

	replayCtx, replayRequest := newContext("message-replay")
	replayRequest.AppendHeader(newAuthorization(api.cfg.GetDomain(), replayRequest.Recipient().String(), nonce, "00000001", password))
	if err := api.checkDigestAuth(replayCtx); err == nil || !strings.Contains(err.Error(), "replay") {
		t.Fatalf("MESSAGE Digest replay result = %v", err)
	}
	nextCtx, nextRequest := newContext("message-next-nc")
	nextRequest.AppendHeader(newAuthorization(api.cfg.GetDomain(), nextRequest.Recipient().String(), nonce, "00000002", password))
	if err := api.checkDigestAuth(nextCtx); err != nil {
		t.Fatalf("MESSAGE Digest next nc rejected: %v", err)
	}

	for _, test := range []struct {
		name  string
		realm string
		uri   string
	}{
		{name: "realm", realm: "other-realm", uri: request.Recipient().String()},
		{name: "URI", realm: api.cfg.GetDomain(), uri: "sip:34020000002000000002@3402000000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			badCtx, badRequest := newContext("message-bad-" + test.name)
			badRequest.AppendHeader(newAuthorization(test.realm, test.uri, nonce, "00000003", password))
			if err := api.checkDigestAuth(badCtx); err == nil {
				t.Fatal("mismatched Digest identity accepted")
			}
		})
	}

	unissuedCtx, unissuedRequest := newContext("message-unissued")
	unissuedRequest.AppendHeader(newAuthorization(api.cfg.GetDomain(), unissuedRequest.Recipient().String(), "not-issued", "00000001", password))
	if err := api.checkDigestAuth(unissuedCtx); err == nil || !strings.Contains(err.Error(), "not issued") {
		t.Fatalf("unissued MESSAGE nonce result = %v", err)
	}

	wrongPasswordCtx, wrongPasswordRequest := newContext("message-wrong-password")
	wrongPasswordNonce := api.issueMessageNonce(gb10DeviceID, parseAddressIP(addrString(connection.remote)))
	wrongPasswordRequest.AppendHeader(newAuthorization(api.cfg.GetDomain(), wrongPasswordRequest.Recipient().String(), wrongPasswordNonce, "00000001", "wrong"))
	if err := api.checkDigestAuth(wrongPasswordCtx); err == nil || !strings.Contains(err.Error(), "auth failed") {
		t.Fatalf("wrong MESSAGE Digest password result = %v", err)
	}

	legacyCtx, legacyRequest := newContext("message-legacy")
	legacyNonce := api.issueMessageNonce(gb10DeviceID, parseAddressIP(addrString(connection.remote)))
	legacy := sip.AuthFromValue(fmt.Sprintf(`Digest realm="%s",nonce="%s",algorithm=MD5`, api.cfg.GetDomain(), legacyNonce)).
		SetUsername(gb10DeviceID).
		SetPassword(password).
		SetMethod(sip.MethodMessage).
		SetURI(legacyRequest.Recipient().String())
	if _, err := legacy.CalcResponseChecked(); err != nil {
		t.Fatal(err)
	}
	legacyHeader := &sip.GenericHeader{HeaderName: "Authorization", Contents: legacy.String()}
	legacyRequest.AppendHeader(legacyHeader)
	if err := api.checkDigestAuth(legacyCtx); err != nil {
		t.Fatalf("legacy MESSAGE Digest rejected: %v", err)
	}
	if err := api.checkDigestAuth(legacyCtx); err != nil {
		t.Fatalf("legacy MESSAGE retransmission rejected: %v", err)
	}
	legacyReplayCtx, legacyReplayRequest := newContext("message-legacy-replay")
	legacyReplayRequest.AppendHeader(legacyHeader)
	if err := api.checkDigestAuth(legacyReplayCtx); err == nil || !strings.Contains(err.Error(), "replay") {
		t.Fatalf("legacy MESSAGE replay result = %v", err)
	}

	challengeCtx, _ := newContext("message-challenge")
	api.sipAccessControlMiddleware(challengeCtx)
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), `realm="3402000000"`) {
			t.Fatalf("MESSAGE challenge did not use derived domain: %s", payload)
		}
	default:
		t.Fatal("MESSAGE challenge response missing")
	}
}

func TestMessageNonceBindsDeviceSourceAndExpiry(t *testing.T) {
	api := &GB28181API{}
	nonce := api.issueMessageNonce(gb10DeviceID, "192.0.2.10")
	if err := api.validateMessageNonce(nonce, gb10DeviceID, "192.0.2.10"); err != nil {
		t.Fatalf("valid MESSAGE nonce rejected: %v", err)
	}
	if err := api.validateMessageNonce(nonce, "34020000001320000002", "192.0.2.10"); err == nil || !strings.Contains(err.Error(), "device") {
		t.Fatalf("cross-device MESSAGE nonce result = %v", err)
	}
	if err := api.validateMessageNonce(nonce, gb10DeviceID, "192.0.2.11"); err == nil || !strings.Contains(err.Error(), "source") {
		t.Fatalf("cross-source MESSAGE nonce result = %v", err)
	}

	api.messageNonceMu.Lock()
	state := api.messageNonces[nonce]
	state.Expires = time.Now().Add(-time.Second)
	api.messageNonces[nonce] = state
	api.messageNonceMu.Unlock()
	if err := api.validateMessageNonce(nonce, gb10DeviceID, "192.0.2.10"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired MESSAGE nonce result = %v", err)
	}
}
