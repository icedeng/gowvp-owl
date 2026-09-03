package gbs

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestAccessControlRejectsUnknownDeviceBeforeBusinessRoutes(t *testing.T) {
	const unknownDeviceID = "34020000001320000009"
	adapter, _, _ := newCascadeMediaCore(t)
	memory := &flowMemory{persistent: &ipc.Device{DeviceID: unknownDeviceID}}
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodMessage, "access-unknown", nil)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("access-unknown-tx", connection),
		DeviceID: unknownDeviceID, Source: connection.remote, Log: slog.Default(),
	}

	api.sipAccessControlMiddleware(ctx)
	if !ctx.IsAborted() {
		t.Fatal("unknown device request was not aborted")
	}
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 403") {
			t.Fatalf("unknown device response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("unknown device response timeout")
	}
}

func TestAccessControlRejectsPersistedDeviceMissingFromMemory(t *testing.T) {
	adapter, persisted, _ := newCascadeMediaCore(t)
	memory := &flowMemory{persistent: persisted}
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodMessage, "access-persisted", nil)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("access-persisted-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote, Log: slog.Default(),
	}

	api.sipAccessControlMiddleware(ctx)
	if !ctx.IsAborted() {
		t.Fatal("unregistered persisted device request was not aborted")
	}
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 403") {
			t.Fatalf("unregistered persisted device response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("unregistered persisted device response timeout")
	}
}

func TestAccessControlRejectsInactiveRegistration(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name         string
		online       bool
		registeredAt time.Time
		expires      int
		closed       bool
	}{
		{name: "expired", online: true, registeredAt: now.Add(-time.Minute), expires: 10},
		{name: "expires_at_boundary", online: true, registeredAt: now.Add(-10 * time.Second), expires: 10},
		{name: "registration_closed", online: true, registeredAt: now, expires: 3600, closed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.UpdateRuntime(func(device *Device) {
				device.IsOnline = test.online
				device.LastRegisterAt = test.registeredAt
				device.Expires = test.expires
				device.registrationClosed = test.closed
			})
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			connection := newFlowConnection()
			request := newFlowRequest(t, connection, sip.MethodMessage, "access-inactive-"+test.name, nil)
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction("access-inactive-"+test.name+"-tx", connection),
				DeviceID: gb10DeviceID, Source: connection.remote, Log: slog.Default(),
			}

			api.sipAccessControlMiddleware(ctx)

			if !ctx.IsAborted() {
				t.Fatal("inactive registration request was not aborted")
			}
			select {
			case payload := <-connection.writes:
				if !strings.Contains(string(payload), "SIP/2.0 403") {
					t.Fatalf("inactive registration response = %s", payload)
				}
			case <-time.After(time.Second):
				t.Fatal("inactive registration response timeout")
			}
		})
	}
}

func TestAccessControlAllowsOfflineStatusWithinActiveRegistration(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = false
		device.LastRegisterAt = time.Now()
		device.Expires = 3600
	})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodMessage, "access-status-offline", nil)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("access-status-offline-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote, Log: slog.Default(),
	}

	api.sipAccessControlMiddleware(ctx)

	if ctx.IsAborted() {
		t.Fatal("valid REGISTER binding with DeviceStatus OFFLINE was rejected")
	}
}

func TestAccessControlUsesRegisteredDeviceVersion(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.LastRegisterAt = time.Now()
		device.Expires = 3600
	})
	memory.runtime.setGBVersion(GBVersion11)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodMessage, "access-device-version", nil)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("access-device-version-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote, Log: slog.Default(),
		XGBVer: string(GBVersion30), XGBVerRaw: string(GBVersion30),
	}

	api.sipAccessControlMiddleware(ctx)
	if ctx.IsAborted() {
		t.Fatal("registered device request was aborted")
	}
	if ctx.XGBVer != string(GBVersion11) {
		t.Fatalf("effective request version = %q; want registered %q", ctx.XGBVer, GBVersion11)
	}
	if ctx.XGBVerRaw != string(GBVersion30) {
		t.Fatalf("raw request version = %q; want diagnostic value %q", ctx.XGBVerRaw, GBVersion30)
	}
}

func TestStrictSourceCheckFailsClosedAndCanonicalizesIPs(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Address = "192.0.2.10:5060"
	api := &GB28181API{
		cfg: &conf.SIP{StrictSourceCheck: true},
		svr: &Server{memoryStorer: memory},
	}
	ctx := &sip.Context{DeviceID: gb10DeviceID}

	if err := api.checkSourceAddress(ctx); err == nil || !strings.Contains(err.Error(), "source ip") {
		t.Fatalf("missing request source result = %v", err)
	}

	ctx.Source = &testAddr{network: "udp", value: "192.0.2.10:5060"}
	memory.runtime.Address = ""
	if err := api.checkSourceAddress(ctx); err == nil || !strings.Contains(err.Error(), "registered source ip") {
		t.Fatalf("missing registered source result = %v", err)
	}

	memory.runtime.Address = "[2001:db8::1]:5060"
	ctx.Source = &testAddr{network: "udp", value: "[2001:0db8:0:0:0:0:0:1]:6070"}
	if err := api.checkSourceAddress(ctx); err != nil {
		t.Fatalf("equivalent IPv6 source was rejected: %v", err)
	}
}

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

func TestMessageDigestPreservesPasswordWhitespace(t *testing.T) {
	const password = " secret "
	for _, test := range []struct {
		name           string
		devicePassword string
		globalPassword string
	}{
		{name: "device credential", devicePassword: password},
		{name: "global fallback", globalPassword: password},
	} {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.Password = test.devicePassword
			api := &GB28181API{
				cfg: &conf.SIP{
					ID: gb10PlatformID, Password: test.globalPassword, RequireMessageAuth: true,
				},
				svr: &Server{memoryStorer: memory},
			}
			connection := newFlowConnection()
			request := newFlowRequest(t, connection, sip.MethodMessage, "message-password-whitespace", nil)
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction("message-password-whitespace", connection),
				DeviceID: gb10DeviceID, Source: connection.remote,
			}
			nonce := api.issueMessageNonce(gb10DeviceID, parseAddressIP(addrString(connection.remote)))
			auth := sip.AuthFromValue(fmt.Sprintf(
				`Digest realm="%s",nonce="%s",algorithm=MD5,qop="auth"`, api.cfg.GetDomain(), nonce,
			)).
				SetUsername(gb10DeviceID).
				SetPassword(password).
				SetMethod(sip.MethodMessage).
				SetURI(request.Recipient().String()).
				SetClientNonce("00000001", "client-nonce")
			if _, err := auth.CalcResponseChecked(); err != nil {
				t.Fatal(err)
			}
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Authorization", Contents: auth.String()})

			if err := api.checkDigestAuth(ctx); err != nil {
				t.Fatalf("MESSAGE Digest changed password whitespace: %v", err)
			}
		})
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
