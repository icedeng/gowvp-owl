package gbs

import (
	"fmt"
	"strings"
	"testing"

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
	newAuthorization := func(realm, uri string) *sip.GenericHeader {
		auth := sip.AuthFromValue(fmt.Sprintf(`Digest realm="%s",nonce="message-nonce",algorithm=MD5,qop="auth"`, realm)).
			SetUsername(gb10DeviceID).
			SetPassword(password).
			SetMethod(sip.MethodMessage).
			SetURI(uri).
			SetClientNonce("00000001", "client-nonce")
		if _, err := auth.CalcResponseChecked(); err != nil {
			t.Fatal(err)
		}
		return &sip.GenericHeader{HeaderName: "Authorization", Contents: auth.String()}
	}

	ctx, request := newContext("message-derived-domain")
	request.AppendHeader(newAuthorization(api.cfg.GetDomain(), request.Recipient().String()))
	if err := api.checkDigestAuth(ctx); err != nil {
		t.Fatalf("MESSAGE Digest using derived domain rejected: %v", err)
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
			badRequest.AppendHeader(newAuthorization(test.realm, test.uri))
			if err := api.checkDigestAuth(badCtx); err == nil {
				t.Fatal("mismatched Digest identity accepted")
			}
		})
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
