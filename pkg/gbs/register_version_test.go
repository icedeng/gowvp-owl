package gbs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
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

func TestRegisterRejectsInvalidXGBVersionBeforeStateChange(t *testing.T) {
	tests := []struct {
		name    string
		values  []string
		wantErr string
	}{
		{name: "malformed", values: []string{"2011"}, wantErr: "invalid X-GB-Ver"},
		{name: "empty", values: []string{""}, wantErr: "invalid X-GB-Ver"},
		{name: "duplicate", values: []string{"1.0", "2.0"}, wantErr: "multiple X-GB-Ver"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, memory, connection := newRegisterHandlerTestAPI(t, false)
			ctx := newRegisterHandlerTestContext(t, connection, "register-version-"+test.name, 3600)
			ctx.Request.RemoveHeader("X-GB-Ver")
			for _, value := range test.values {
				ctx.Request.AppendHeader(&sip.GenericHeader{HeaderName: "X-GB-Ver", Contents: value})
			}

			api.handlerRegister(ctx)

			payload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 400")
			if !strings.Contains(payload, test.wantErr) || strings.Count(payload, "X-GB-Ver: 3.0") != 1 {
				t.Fatalf("REGISTER invalid-version response = %s", payload)
			}
			if memory.loadOrStoreCalls != 0 || memory.changeCalls != 0 {
				t.Fatalf("invalid version mutated memory: load_or_store=%d change=%d", memory.loadOrStoreCalls, memory.changeCalls)
			}
		})
	}
}

func TestRegisterPreservesSyntacticallyValidUnknownXGBVersion(t *testing.T) {
	api, _, connection := newRegisterHandlerTestAPI(t, false)
	ctx := newRegisterHandlerTestContext(t, connection, "register-version-unknown", 0)
	setRegisterHandlerTestVersion(ctx, "4.0")

	api.handlerRegister(ctx)

	assertRegisterHandlerResponse(t, connection, "SIP/2.0 200 OK")
	if ctx.XGBVerRaw != "4.0" || ctx.XGBVer != "" {
		t.Fatalf("unknown version context = raw %q effective %q", ctx.XGBVerRaw, ctx.XGBVer)
	}
}

func TestSuccessfulRegisterClearsQueryStateDeletionTombstone(t *testing.T) {
	api, _, connection := newRegisterHandlerTestAPI(t, true)
	api.deviceDeletionTombstones.Store(gb10DeviceID, struct{}{})
	api.deviceOfflineTombstones.Store(gb10DeviceID, struct{}{})
	ctx := newRegisterHandlerTestContext(t, connection, "register-clears-query-state-deletion", 3600)

	api.handlerRegister(ctx)

	assertRegisterHandlerResponse(t, connection, "SIP/2.0 200 OK")
	if _, deleted := api.deviceDeletionTombstones.Load(gb10DeviceID); deleted {
		t.Fatal("successful REGISTER retained query-state deletion tombstone")
	}
	if _, offline := api.deviceOfflineTombstones.Load(gb10DeviceID); offline {
		t.Fatal("successful REGISTER retained offline-operation tombstone")
	}
	api.storeQueryState(gb10DeviceID, "DeviceStatus", &DeviceStatusData{Online: "ONLINE"})
	if _, ok := api.GetQueryState(gb10DeviceID); !ok {
		t.Fatal("successful REGISTER did not restore query-state writes")
	}
}

func TestRequestSignalingTransportDistinguishesTLS(t *testing.T) {
	base := newFlowConnection()
	connection := &registerTransportConnection{flowConnection: base, transport: "TLS"}
	ctx := newRegisterHandlerTestContext(t, base, "register-tls-transport", 3600)
	ctx.Request.SetConnection(connection)
	if got := requestSignalingTransport(ctx); got != "tls" {
		t.Fatalf("REGISTER signaling transport = %q", got)
	}
}

type registerTransportConnection struct {
	*flowConnection
	transport string
}

func (*registerTransportConnection) Network() string              { return "tcp" }
func (c *registerTransportConnection) SignalingTransport() string { return c.transport }

func TestRegisterRedirectUsesTrustedServerConfiguration(t *testing.T) {
	cfg := &conf.SIP{
		ID: gb10PlatformID, Domain: "3402000000",
		RegisterRedirect: "sip:" + gb10PlatformID + "@192.0.2.31:5070",
	}
	api := &GB28181API{cfg: cfg}
	request := newFlowRequest(t, newFlowConnection(), sip.MethodRegister, "register-redirect", nil)
	request.RemoveHeader("X-GB-Ver")
	request.AppendHeader(&sip.GenericHeader{HeaderName: "X-GB-Ver", Contents: string(GBVersion30)})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "3600"})
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
			!strings.Contains(text, "192.0.2.31:5070") ||
			!strings.Contains(text, "\r\nExpires: 3600\r\n") ||
			strings.Contains(text, "203.0.113.99") {
			t.Fatalf("REGISTER redirect response = %s", text)
		}
	default:
		t.Fatal("REGISTER redirect response missing")
	}
}

func TestRegisterRedirectRejectsUnsafeRuntimeConfiguration(t *testing.T) {
	for _, redirect := range []string{
		"sip:" + gb10PlatformID + ":secret@192.0.2.31:5070",
		"sip:34020000002000000002@192.0.2.31:5070",
		"sips:" + gb10PlatformID + "@192.0.2.31:5070;transport=tcp",
	} {
		t.Run(redirect, func(t *testing.T) {
			api := &GB28181API{cfg: &conf.SIP{
				ID: gb10PlatformID, Domain: "3402000000", RegisterRedirect: redirect,
			}}
			connection := newFlowConnection()
			request := newFlowRequest(t, connection, sip.MethodRegister, "register-invalid-redirect", nil)
			request.RemoveHeader("X-GB-Ver")
			request.AppendHeader(&sip.GenericHeader{HeaderName: "X-GB-Ver", Contents: string(GBVersion30)})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "3600"})
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction("register-invalid-redirect", connection), DeviceID: gb10DeviceID,
				Source: connection.remote, To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), XGBVer: string(GBVersion30),
			}

			api.handlerRegister(ctx)

			payload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 400 invalid redirect uri")
			if strings.Contains(payload, "\r\nContact:") {
				t.Fatalf("unsafe REGISTER redirect leaked Contact: %s", payload)
			}
		})
	}
}

func TestRegisterRedirectDoesNotInterceptUnregister(t *testing.T) {
	api, _, connection := newRegisterHandlerTestAPI(t, false)
	api.cfg.RegisterRedirect = "sip:" + gb10PlatformID + "@192.0.2.31:5070"
	ctx := newRegisterHandlerTestContext(t, connection, "unregister-no-redirect", 0)
	setRegisterHandlerTestVersion(ctx, string(GBVersion30))

	api.handlerRegister(ctx)

	payload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 200 OK")
	if strings.Contains(payload, "SIP/2.0 302 Moved Temporarily") || strings.Contains(payload, "192.0.2.31:5070") {
		t.Fatalf("unregister was redirected: %s", payload)
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
	request.RemoveHeader("Contact")
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
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "3600"})
	if _, err := parseRegisterExpires(ctx); err == nil || !strings.Contains(err.Error(), "at most one Expires") {
		t.Fatalf("duplicate Expires error = %v", err)
	}
	request.RemoveHeader("Expires")
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "4294967296"})
	if _, err := parseRegisterExpires(ctx); err == nil || !strings.Contains(err.Error(), "invalid REGISTER expires") {
		t.Fatalf("overflow Expires error = %v", err)
	}
}

func TestRegisterExpiryMinimumByVersion(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20, GBVersion30} {
		version := version
		t.Run(version.StandardYear(), func(t *testing.T) {
			api, memory, connection := newRegisterHandlerTestAPI(t, false)
			ctx := newRegisterHandlerTestContext(t, connection, "register-short-"+string(version), minimumStandardRegisterTTL-1)
			setRegisterHandlerTestVersion(ctx, string(version))

			api.handlerRegister(ctx)

			payload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 423 Interval Too Brief")
			if strings.Count(payload, "\r\nMin-Expires: 3600\r\n") != 1 {
				t.Fatalf("REGISTER 423 Min-Expires = %s", payload)
			}
			if registerResponseDate(payload) != "" {
				t.Fatalf("REGISTER 423 unexpectedly included Date: %s", payload)
			}
			if memory.loadOrStoreCalls != 0 || memory.changeCalls != 0 {
				t.Fatalf("short REGISTER mutated memory: load_or_store=%d change=%d", memory.loadOrStoreCalls, memory.changeCalls)
			}
		})
	}

	t.Run("2011 compatibility", func(t *testing.T) {
		api, _, connection := newRegisterHandlerTestAPI(t, true)
		ctx := newRegisterHandlerTestContext(t, connection, "register-short-2011", 60)

		api.handlerRegister(ctx)

		payload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 200 OK")
		if strings.Count(payload, "\r\nExpires: 60\r\n") != 1 {
			t.Fatalf("2011 REGISTER accepted Expires = %s", payload)
		}
	})

	t.Run("stored 2014 profile without version header", func(t *testing.T) {
		api, memory, connection := newRegisterHandlerTestAPI(t, true)
		var device ipc.Device
		if err := api.core.Store().Device().Update(context.Background(), &device, func(current *ipc.Device) error {
			current.Ext.GBManualVersion = string(GBVersion11)
			return nil
		}, orm.Where("device_id=?", gb10DeviceID)); err != nil {
			t.Fatal(err)
		}
		ctx := newRegisterHandlerTestContext(t, connection, "register-short-stored-2014", minimumStandardRegisterTTL-1)
		setRegisterHandlerTestVersion(ctx, "")

		api.handlerRegister(ctx)

		payload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 423 Interval Too Brief")
		if !strings.Contains(payload, "\r\nMin-Expires: 3600\r\n") {
			t.Fatalf("stored 2014 REGISTER 423 = %s", payload)
		}
		if memory.loadOrStoreCalls != 0 || memory.changeCalls != 0 {
			t.Fatalf("stored 2014 short REGISTER mutated memory: load_or_store=%d change=%d", memory.loadOrStoreCalls, memory.changeCalls)
		}
	})
}

func TestRegisterDefaultExpiryIsOneDay(t *testing.T) {
	api, _, connection := newRegisterHandlerTestAPI(t, true)
	ctx := newRegisterHandlerTestContext(t, connection, "register-default-expiry", defaultRegisterExpires)
	ctx.Request.RemoveHeader("Expires")
	setRegisterHandlerTestVersion(ctx, string(GBVersion30))

	api.handlerRegister(ctx)

	payload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 200 OK")
	if strings.Count(payload, "\r\nExpires: 86400\r\n") != 1 {
		t.Fatalf("REGISTER default Expires = %s", payload)
	}
}

func TestRegisterRequiresSingleMatchingContact(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*sip.Request)
		want   string
	}{
		{
			name: "missing",
			mutate: func(request *sip.Request) {
				request.RemoveHeader("Contact")
			},
			want: "exactly one Contact",
		},
		{
			name: "duplicate",
			mutate: func(request *sip.Request) {
				contact, _ := request.Contact()
				request.AppendHeader(contact.Clone())
			},
			want: "exactly one Contact",
		},
		{
			name: "invalid type",
			mutate: func(request *sip.Request) {
				request.RemoveHeader("Contact")
				request.AppendHeader(&sip.GenericHeader{HeaderName: "Contact", Contents: "invalid"})
			},
			want: "Contact header is invalid",
		},
		{
			name: "missing uri",
			mutate: func(request *sip.Request) {
				request.RemoveHeader("Contact")
				request.AppendHeader(&sip.ContactHeader{})
			},
			want: "Contact header is invalid",
		},
		{
			name: "mismatched device",
			mutate: func(request *sip.Request) {
				request.RemoveHeader("Contact")
				contact := mustFlowAddress(t, "sip:34020000001320000009@192.0.2.10:5060")
				request.AppendHeader(&sip.ContactHeader{Address: contact.URI, Params: contact.Params})
			},
			want: "does not match From device id",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, memory, connection := newRegisterHandlerTestAPI(t, false)
			ctx := newRegisterHandlerTestContext(t, connection, "register-contact-"+test.name, 3600)
			test.mutate(ctx.Request)

			api.handlerRegister(ctx)

			payload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 400")
			if !strings.Contains(payload, test.want) {
				t.Fatalf("REGISTER Contact response = %s, want %q", payload, test.want)
			}
			if memory.loadOrStoreCalls != 0 || memory.changeCalls != 0 {
				t.Fatalf("invalid Contact mutated memory: load_or_store=%d change=%d", memory.loadOrStoreCalls, memory.changeCalls)
			}
		})
	}
}

func TestRegisterSuccessResponseIncludesBinding(t *testing.T) {
	request := newFlowRequest(t, newFlowConnection(), sip.MethodRegister, "register-success-binding", nil)
	ctx := &sip.Context{Request: request}
	api := &GB28181API{}
	response := api.newRegisterSuccessResponse(ctx, 3600, "2026-08-29T10:20:30.123")
	text := response.String()

	if strings.Count(text, "\r\nContact:") != 1 || !strings.Contains(text, "sip:"+gb10DeviceID+"@3402000000") {
		t.Fatalf("REGISTER success Contact = %s", text)
	}
	if strings.Count(text, "\r\nExpires: 3600\r\n") != 1 {
		t.Fatalf("REGISTER success Expires = %s", text)
	}
	if strings.Count(text, "\r\nDate: 2026-08-29T10:20:30.123\r\n") != 1 {
		t.Fatalf("REGISTER success Date = %s", text)
	}
}

func TestRegisterStateChangeFailureReturnsServerError(t *testing.T) {
	api, memory, connection := newRegisterHandlerTestAPI(t, true)
	memory.changeErr = errors.New("update failed")
	ctx := newRegisterHandlerTestContext(t, connection, "register-state-failure", 3600)

	api.handlerRegister(ctx)

	assertRegisterHandlerResponse(t, connection, "SIP/2.0 500 server db error")
	metrics := api.metrics.Snapshot()
	if metrics.RegisterSuccess != 0 || metrics.RegisterFailures != 1 {
		t.Fatalf("REGISTER metrics = success:%d failures:%d", metrics.RegisterSuccess, metrics.RegisterFailures)
	}
	if memory.changeCalls != 1 {
		t.Fatalf("state changes = %d, want 1", memory.changeCalls)
	}
}

func TestRegisterStateChangesClearPendingActivityPersistence(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.offlinePersistencePending = true
		device.registrationClosed = true
		device.deviceStatusPersistencePending = true
		device.keepalivePersistencePending = true
	})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	connection := newFlowConnection()
	ctx := &sip.Context{
		Request:  newFlowRequest(t, connection, sip.MethodRegister, "register-clear-pending", nil),
		DeviceID: gb10DeviceID,
		Source:   connection.remote,
		To:       mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
	}
	registeredAt := time.Now()
	if err := api.login(ctx, GBVersion10, nil, func(device *ipc.Device) error {
		device.IsOnline = true
		device.RegisteredAt = orm.Time{Time: registeredAt}
		device.Expires = 3600
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state := memory.runtime.runtimeSnapshot()
	if state.RegistrationClosed || state.OfflinePersistencePending || state.DeviceStatusPersistencePending ||
		state.KeepalivePersistencePending {
		t.Fatalf("REGISTER retained stale pending state: %+v", state)
	}

	memory.runtime.UpdateRuntime(func(device *Device) {
		device.deviceStatusPersistencePending = true
		device.keepalivePersistencePending = true
	})
	if err := api.logout(gb10DeviceID, func(device *ipc.Device) error {
		device.IsOnline = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state = memory.runtime.runtimeSnapshot()
	if !state.RegistrationClosed || state.DeviceStatusPersistencePending || state.KeepalivePersistencePending {
		t.Fatalf("logout retained stale pending state: %+v", state)
	}
}

func TestUnregisterStateChangeFailureReturnsServerError(t *testing.T) {
	api, memory, connection := newRegisterHandlerTestAPI(t, true)
	memory.changeErr = errors.New("update failed")
	ctx := newRegisterHandlerTestContext(t, connection, "unregister-state-failure", 0)

	api.handlerRegister(ctx)

	assertRegisterHandlerResponse(t, connection, "SIP/2.0 500 server db error")
	metrics := api.metrics.Snapshot()
	if metrics.RegisterSuccess != 0 || metrics.RegisterFailures != 1 {
		t.Fatalf("unregister metrics = success:%d failures:%d", metrics.RegisterSuccess, metrics.RegisterFailures)
	}
	if memory.changeCalls != 1 {
		t.Fatalf("state changes = %d, want 1", memory.changeCalls)
	}
}

func TestUnknownDeviceUnregisterIsIdempotentWithoutCreatingDevice(t *testing.T) {
	api, memory, connection := newRegisterHandlerTestAPI(t, false)
	ctx := newRegisterHandlerTestContext(t, connection, "unknown-unregister", 0)

	api.handlerRegister(ctx)

	assertRegisterHandlerResponse(t, connection, "SIP/2.0 200 OK")
	if memory.loadOrStoreCalls != 0 || memory.changeCalls != 0 {
		t.Fatalf("unknown unregister mutated memory: load_or_store=%d change=%d", memory.loadOrStoreCalls, memory.changeCalls)
	}
	var device ipc.Device
	err := api.core.Store().Device().Get(context.Background(), &device, orm.Where("device_id = ?", gb10DeviceID))
	if !orm.IsErrRecordNotFound(err) {
		t.Fatalf("unknown unregister database lookup = %v, device = %+v", err, device)
	}
	metrics := api.metrics.Snapshot()
	if metrics.RegisterSuccess != 1 || metrics.RegisterFailures != 0 {
		t.Fatalf("unknown unregister metrics = success:%d failures:%d", metrics.RegisterSuccess, metrics.RegisterFailures)
	}
}

func TestUnknownDeviceUnregisterDoesNotBypassAuthentication(t *testing.T) {
	api, memory, connection := newRegisterHandlerTestAPI(t, false)
	api.cfg.Password = "secret"
	ctx := newRegisterHandlerTestContext(t, connection, "unknown-unregister-auth", 0)

	api.handlerRegister(ctx)

	payload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 401 Unauthorized")
	if date := registerResponseDate(payload); date != "" {
		t.Fatalf("REGISTER challenge unexpectedly included calibration Date %q", date)
	}
	if memory.loadOrStoreCalls != 0 || memory.changeCalls != 0 {
		t.Fatalf("unauthenticated unregister mutated memory: load_or_store=%d change=%d", memory.loadOrStoreCalls, memory.changeCalls)
	}
	metrics := api.metrics.Snapshot()
	if metrics.RegisterSuccess != 0 || metrics.RegisterFailures != 0 {
		t.Fatalf("unauthenticated unregister metrics = success:%d failures:%d", metrics.RegisterSuccess, metrics.RegisterFailures)
	}
}

func TestSuccessfulRegisterResponseDateAcrossVersions(t *testing.T) {
	versions := []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30}
	for _, version := range versions {
		version := version
		t.Run(version.StandardYear(), func(t *testing.T) {
			api, _, connection := newRegisterHandlerTestAPI(t, false)
			ctx := newRegisterHandlerTestContext(t, connection, "register-date-"+string(version), 0)
			setRegisterHandlerTestVersion(ctx, string(version))

			before := time.Now().In(sip.GBTimeLocation())
			api.handlerRegister(ctx)
			payload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 200 OK")
			after := time.Now().In(sip.GBTimeLocation())

			date := registerResponseDate(payload)
			parsed, err := time.ParseInLocation("2006-01-02T15:04:05.000", date, sip.GBTimeLocation())
			if err != nil {
				t.Fatalf("REGISTER Date %q does not use yyyy-MM-ddTHH:mm:ss.SSS: %v", date, err)
			}
			if parsed.Before(before.Add(-time.Millisecond)) || parsed.After(after.Add(time.Millisecond)) {
				t.Fatalf("REGISTER Date %q is outside Beijing response window [%s, %s]", date, before, after)
			}
		})
	}
}

func TestSuccessfulRegisterStateAcrossVersions(t *testing.T) {
	versions := []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30}
	for _, version := range versions {
		version := version
		t.Run(version.StandardYear(), func(t *testing.T) {
			api, memory, connection := newRegisterHandlerTestAPI(t, true)
			ctx := newRegisterHandlerTestContext(t, connection, "register-state-"+string(version), minimumStandardRegisterTTL)
			setRegisterHandlerTestVersion(ctx, string(version))

			api.handlerRegister(ctx)

			payload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 200 OK")
			if strings.Count(payload, "\r\nX-GB-Ver: 3.0\r\n") != 1 || strings.Count(payload, "\r\nExpires: 3600\r\n") != 1 {
				t.Fatalf("REGISTER success response = %s", payload)
			}
			if !memory.persistent.IsOnline || memory.persistent.Expires != minimumStandardRegisterTTL || memory.persistent.RegisteredAt.Time.IsZero() {
				t.Fatalf("REGISTER state = online:%v expires:%d registered:%v", memory.persistent.IsOnline, memory.persistent.Expires, memory.persistent.RegisteredAt.Time)
			}
			if memory.persistent.Ext.GBDeclaredVersion != string(version) || memory.persistent.Ext.GBEffectiveVersion != string(version) || memory.runtime.GBVersion() != string(version) {
				t.Fatalf("REGISTER version state = declared:%q effective:%q runtime:%q", memory.persistent.Ext.GBDeclaredVersion, memory.persistent.Ext.GBEffectiveVersion, memory.runtime.GBVersion())
			}
			if memory.changeCalls < 1 || memory.loadOrStoreCalls != 1 {
				t.Fatalf("REGISTER state mutations = change:%d load_or_store:%d", memory.changeCalls, memory.loadOrStoreCalls)
			}
			metrics := api.metrics.Snapshot()
			if metrics.RegisterSuccess != 1 || metrics.RegisterFailures != 0 {
				t.Fatalf("REGISTER metrics = success:%d failures:%d", metrics.RegisterSuccess, metrics.RegisterFailures)
			}
		})
	}
}

func TestRegisterOperationLockSerializesOnlySameDevice(t *testing.T) {
	api := &GB28181API{}
	unlockFirst := api.lockRegisterOperation(gb10DeviceID)

	sameEntered := make(chan struct{})
	sameDone := make(chan struct{})
	go func() {
		unlock := api.lockRegisterOperation(gb10DeviceID)
		close(sameEntered)
		unlock()
		close(sameDone)
	}()
	select {
	case <-sameEntered:
		t.Fatal("same-device REGISTER operation was not serialized")
	case <-time.After(50 * time.Millisecond):
	}

	otherDeviceID := "34020000001320000009"
	otherEntered := make(chan struct{})
	go func() {
		unlock := api.lockRegisterOperation(otherDeviceID)
		close(otherEntered)
		unlock()
	}()
	select {
	case <-otherEntered:
	case <-time.After(time.Second):
		t.Fatal("unrelated device REGISTER operation was blocked")
	}

	unlockFirst()
	select {
	case <-sameDone:
	case <-time.After(time.Second):
		t.Fatal("same-device REGISTER operation did not resume")
	}
	api.registerOperationMu.Lock()
	remaining := len(api.registerOperations)
	api.registerOperationMu.Unlock()
	if remaining != 0 {
		t.Fatalf("REGISTER operation locks retained %d entries", remaining)
	}
}

func TestExactUnknownUnregisterRetransmissionReusesSuccess(t *testing.T) {
	api, memory, connection := newRegisterHandlerTestAPI(t, false)
	countingStore := &countingRegisterStore{Storer: api.core.Store()}
	countingStore.device = &countingRegisterDeviceStore{DeviceStorer: countingStore.Storer.Device()}
	api.core = ipc.NewAdapter(countingStore, uniqueid.Core{})
	first := newRegisterHandlerTestContext(t, connection, "unknown-unregister-retransmit", 0)
	secondRequest := first.Request.Clone().(*sip.Request)
	secondRequest.SetConnection(connection)
	secondRequest.SetSource(connection.remote)
	secondRequest.SetDestination(connection.local)
	second := &sip.Context{
		Request: secondRequest, Tx: sip.NewTransaction("unknown-unregister-retransmit-2", connection),
		DeviceID: first.DeviceID, Source: first.Source, To: first.To.Clone(), XGBVer: first.XGBVer,
		XGBVerRaw: first.XGBVerRaw, Log: slog.Default(),
	}

	api.handlerRegister(first)
	firstPayload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 200 OK")
	api.handlerRegister(second)
	secondPayload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 200 OK")

	if got := countingStore.device.getCalls.Load(); got != 1 {
		t.Fatalf("database lookups = %d, want 1 for exact retransmission", got)
	}
	if registerResponseDate(firstPayload) == "" || registerResponseDate(firstPayload) != registerResponseDate(secondPayload) {
		t.Fatalf("retransmitted REGISTER Date differs:\nfirst=%s\nsecond=%s", firstPayload, secondPayload)
	}
	if memory.loadOrStoreCalls != 0 || memory.changeCalls != 0 {
		t.Fatalf("unknown unregister retransmission mutated memory: load_or_store=%d change=%d", memory.loadOrStoreCalls, memory.changeCalls)
	}
}

func TestRegisterStateTransitionInvalidatesEarlierSuccess(t *testing.T) {
	api, memory, connection := newRegisterHandlerTestAPI(t, true)
	register := newRegisterHandlerTestContext(t, connection, "register-before-unregister", 3600)
	registerRetryRequest := register.Request.Clone().(*sip.Request)
	registerRetryRequest.SetConnection(connection)
	registerRetryRequest.SetSource(connection.remote)
	registerRetryRequest.SetDestination(connection.local)

	api.handlerRegister(register)
	assertRegisterHandlerResponse(t, connection, "SIP/2.0 200 OK")
	if memory.changeCalls != 2 {
		t.Fatalf("initial REGISTER state commits = %d, want 2", memory.changeCalls)
	}
	if _, ok := api.loadRegisterResult(registerResultKey(register), time.Now()); !ok {
		t.Fatal("initial REGISTER success was not cached")
	}

	unregister := newRegisterHandlerTestContext(t, connection, "register-before-unregister", 0)
	unregister.Request.RemoveHeader("CSeq")
	unregister.Request.AppendHeader(&sip.CSeq{SeqNo: 2, MethodName: sip.MethodRegister})
	api.handlerRegister(unregister)
	assertRegisterHandlerResponse(t, connection, "SIP/2.0 200 OK")
	if memory.changeCalls != 4 {
		t.Fatalf("unregister state commits = %d, want 4", memory.changeCalls)
	}
	if _, ok := api.loadRegisterResult(registerResultKey(register), time.Now()); ok {
		t.Fatal("unregister retained an earlier REGISTER success")
	}
	if _, ok := api.loadRegisterResult(registerResultKey(unregister), time.Now()); !ok {
		t.Fatal("unregister success was not cached")
	}

	// 迟到的旧 REGISTER 不能重放历史 200，也不能覆盖较新注销状态。
	retry := &sip.Context{
		Request: registerRetryRequest, Tx: sip.NewTransaction("register-before-unregister-retry", connection),
		DeviceID: register.DeviceID, Source: register.Source, To: register.To.Clone(), XGBVer: register.XGBVer,
		XGBVerRaw: register.XGBVerRaw, Log: slog.Default(),
	}
	api.handlerRegister(retry)
	payload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 500 REGISTER CSeq is out of order")
	if !strings.Contains(payload, "\r\nRetry-After: 0\r\n") {
		t.Fatalf("late REGISTER response lacks Retry-After: %s", payload)
	}
	if memory.changeCalls != 5 {
		// 进入带行锁的状态事务后才能与最新持久 CSeq 原子比较；拒绝事务本身会计入一次 Change。
		t.Fatalf("late REGISTER state checks = %d, want 5", memory.changeCalls)
	}
	if memory.runtime.runtimeSnapshot().IsOnline {
		t.Fatal("late REGISTER reopened a newer unregistered binding")
	}
}

func TestRegisterRejectsChangedRequestWithSameCSeq(t *testing.T) {
	api, memory, connection := newRegisterHandlerTestAPI(t, true)
	first := newRegisterHandlerTestContext(t, connection, "register-same-cseq", 3600)
	api.handlerRegister(first)
	assertRegisterHandlerResponse(t, connection, "SIP/2.0 200 OK")

	changed := newRegisterHandlerTestContext(t, connection, "register-same-cseq", 7200)
	api.handlerRegister(changed)
	assertRegisterHandlerResponse(t, connection, "SIP/2.0 500 REGISTER CSeq is out of order")
	if memory.persistent.Expires != 3600 || memory.runtime.runtimeSnapshot().Expires != 3600 {
		t.Fatalf("same-CSeq request changed binding expires: persistent=%d runtime=%d", memory.persistent.Expires, memory.runtime.runtimeSnapshot().Expires)
	}
}

func TestRegisterExactReplaySurvivesProcessLocalCacheLoss(t *testing.T) {
	api, memory, connection := newRegisterHandlerTestAPI(t, true)
	register := newRegisterHandlerTestContext(t, connection, "register-persistent-replay", 3600)
	registerReplayRequest := register.Request.Clone().(*sip.Request)
	registerReplayRequest.SetConnection(connection)
	registerReplayRequest.SetSource(connection.remote)
	registerReplayRequest.SetDestination(connection.local)

	api.handlerRegister(register)
	registerPayload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 200 OK")
	registerDate := registerResponseDate(registerPayload)
	registeredAt := memory.persistent.RegisteredAt.Time
	if memory.persistent.Ext.GBRegisterRequestFingerprint == "" || memory.persistent.Ext.GBRegisterResponseDate != registerDate {
		t.Fatalf("REGISTER persistent replay metadata = %+v, response date = %q", memory.persistent.Ext, registerDate)
	}
	if !memory.persistent.Ext.GBRegisterResponseConfirmed {
		t.Fatal("REGISTER successful response was not persistently confirmed")
	}

	// 模拟进程重启或另一实例没有本地成功缓存。
	api.registerResultMu.Lock()
	api.registerResults = make(map[[sha256.Size]byte]registerResultState)
	api.registerResultMu.Unlock()
	registerReplay := &sip.Context{
		Request: registerReplayRequest, Tx: sip.NewTransaction("register-persistent-replay-retry", connection),
		DeviceID: register.DeviceID, Source: register.Source, To: register.To.Clone(), XGBVer: register.XGBVer,
		XGBVerRaw: register.XGBVerRaw, Log: slog.Default(),
	}
	api.handlerRegister(registerReplay)
	replayPayload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 200 OK")
	if got := registerResponseDate(replayPayload); got != registerDate {
		t.Fatalf("REGISTER replay Date = %q, want %q", got, registerDate)
	}
	if !memory.persistent.RegisteredAt.Time.Equal(registeredAt) {
		t.Fatalf("REGISTER replay refreshed binding time: got %v, want %v", memory.persistent.RegisteredAt.Time, registeredAt)
	}

	unregister := newRegisterHandlerTestContext(t, connection, "register-persistent-replay", 0)
	unregister.Request.RemoveHeader("CSeq")
	unregister.Request.AppendHeader(&sip.CSeq{SeqNo: 2, MethodName: sip.MethodRegister})
	unregisterReplayRequest := unregister.Request.Clone().(*sip.Request)
	unregisterReplayRequest.SetConnection(connection)
	unregisterReplayRequest.SetSource(connection.remote)
	unregisterReplayRequest.SetDestination(connection.local)
	api.handlerRegister(unregister)
	unregisterPayload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 200 OK")
	unregisterDate := registerResponseDate(unregisterPayload)

	api.registerResultMu.Lock()
	api.registerResults = make(map[[sha256.Size]byte]registerResultState)
	api.registerResultMu.Unlock()
	unregisterReplay := &sip.Context{
		Request: unregisterReplayRequest, Tx: sip.NewTransaction("unregister-persistent-replay-retry", connection),
		DeviceID: unregister.DeviceID, Source: unregister.Source, To: unregister.To.Clone(), XGBVer: unregister.XGBVer,
		XGBVerRaw: unregister.XGBVerRaw, Log: slog.Default(),
	}
	api.handlerRegister(unregisterReplay)
	unregisterReplayPayload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 200 OK")
	if got := registerResponseDate(unregisterReplayPayload); got != unregisterDate {
		t.Fatalf("unregister replay Date = %q, want %q", got, unregisterDate)
	}
	if memory.runtime.runtimeSnapshot().IsOnline || !persistedRegistrationClosed(memory.persistent) {
		t.Fatal("unregister replay reopened the closed binding")
	}
	if memory.changeCalls != 6 {
		t.Fatalf("REGISTER/unregister state checks = %d, want 6", memory.changeCalls)
	}
}

type countingRegisterWriteFailureConnection struct {
	*flowConnection
	writes atomic.Int32
}

func (c *countingRegisterWriteFailureConnection) WriteTo([]byte, net.Addr) (int, error) {
	c.writes.Add(1)
	return 0, errors.New("REGISTER response write failed")
}

func TestRegisterPostActionsWaitForSuccessfulSIPOK(t *testing.T) {
	api, memory, connection := newRegisterHandlerTestAPI(t, true)
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	sipServer := sip.NewServer(local)
	defer sipServer.Close()
	api.svr.Server = sipServer
	api.svr.fromAddress = *local

	failing := &countingRegisterWriteFailureConnection{flowConnection: connection}
	ctx := newRegisterHandlerTestContext(t, connection, "register-confirmed-post-actions", 3600)
	ctx.Request.SetConnection(failing)
	ctx.Tx = sip.NewTransaction("register-confirmed-post-actions-failure", failing)
	api.handlerRegister(ctx)

	if got := failing.writes.Load(); got != 1 {
		t.Fatalf("REGISTER write failure triggered %d writes, want only the failed 200 response", got)
	}
	if memory.changeCalls != 1 {
		t.Fatalf("REGISTER committed state %d times, want 1", memory.changeCalls)
	}
	if memory.persistent.Ext.GBRegisterResponseConfirmed {
		t.Fatal("failed REGISTER 200 response was persistently confirmed")
	}
	if _, ok := api.loadRegisterResult(registerResultKey(ctx), time.Now()); ok {
		t.Fatal("failed REGISTER 200 response was cached as a completed request")
	}
	metrics := api.metrics.Snapshot()
	if metrics.RegisterSuccess != 0 {
		t.Fatalf("failed REGISTER 200 response incremented success metric: %d", metrics.RegisterSuccess)
	}

	retryRequest := ctx.Request.Clone().(*sip.Request)
	retryRequest.SetConnection(connection)
	retryRequest.SetSource(connection.remote)
	retryRequest.SetDestination(connection.local)
	retry := &sip.Context{
		Request: retryRequest, Tx: sip.NewTransaction("register-confirmed-post-actions-retry", connection),
		DeviceID: ctx.DeviceID, Source: ctx.Source, To: ctx.To.Clone(), XGBVer: ctx.XGBVer,
		XGBVerRaw: ctx.XGBVerRaw, Log: slog.Default(),
	}
	// 成功确认后的查询不属于本测试；移除 SIP 发送器，使其快速返回且不延长测试。
	api.svr.Server = nil
	api.handlerRegister(retry)
	assertRegisterHandlerResponse(t, connection, "SIP/2.0 200 OK")
	if memory.changeCalls != 3 {
		t.Fatalf("REGISTER retry state commits = %d, want 3", memory.changeCalls)
	}
	if !memory.persistent.Ext.GBRegisterResponseConfirmed {
		t.Fatal("successfully retried REGISTER response was not persistently confirmed")
	}
	if _, ok := api.loadRegisterResult(registerResultKey(retry), time.Now()); !ok {
		t.Fatal("successfully retried REGISTER response was not cached")
	}
	metrics = api.metrics.Snapshot()
	if metrics.RegisterSuccess != 1 {
		t.Fatalf("REGISTER retry success metric = %d, want 1", metrics.RegisterSuccess)
	}
}

func TestRegisterResultKeyChangesWithRequestAndSource(t *testing.T) {
	connection := newFlowConnection()
	ctx := newRegisterHandlerTestContext(t, connection, "register-result-key", 3600)
	base := registerResultKey(ctx)
	changedExpires := newRegisterHandlerTestContext(t, connection, "register-result-key", 7200)
	if base == registerResultKey(changedExpires) {
		t.Fatal("REGISTER result key ignored Expires change")
	}
	changedSource := *ctx
	changedSource.Source = &net.UDPAddr{IP: net.ParseIP("192.0.2.11"), Port: 5060}
	if base == registerResultKey(&changedSource) {
		t.Fatal("REGISTER result key ignored source change")
	}
}

func TestRegisterResultCacheExpiresAndRemainsBounded(t *testing.T) {
	api := &GB28181API{}
	now := time.Now()
	expiredKey := sha256.Sum256([]byte("expired"))
	api.storeRegisterResult(expiredKey, registerResultState{Date: "expired", ExpiresAt: now.Add(-time.Second)}, now)
	if _, ok := api.loadRegisterResult(expiredKey, now); ok {
		t.Fatal("expired REGISTER result remained cached")
	}
	for index := 0; index <= maxRegisterResults; index++ {
		key := sha256.Sum256([]byte(fmt.Sprintf("result-%d", index)))
		api.storeRegisterResult(key, registerResultState{
			Date: fmt.Sprint(index), ExpiresAt: now.Add(time.Duration(index+1) * time.Second),
		}, now)
	}
	api.registerResultMu.Lock()
	size := len(api.registerResults)
	api.registerResultMu.Unlock()
	if size != maxRegisterResults {
		t.Fatalf("REGISTER result cache size = %d, want %d", size, maxRegisterResults)
	}
}

func TestRegisterResultCacheDoesNotOutliveShortBinding(t *testing.T) {
	now := time.Now()
	if got, want := registerResultCacheExpiry(now, 30), now.Add(30*time.Second); !got.Equal(want) {
		t.Fatalf("short REGISTER cache expiry = %v, want %v", got, want)
	}
	if got, want := registerResultCacheExpiry(now, 3600), now.Add(registerResultTTL); !got.Equal(want) {
		t.Fatalf("normal REGISTER cache expiry = %v, want %v", got, want)
	}
	if got, want := registerResultCacheExpiry(now, 0), now.Add(registerResultTTL); !got.Equal(want) {
		t.Fatalf("unregister cache expiry = %v, want %v", got, want)
	}
}

func TestRegisterRestoresPersistedChannelsBeforePostRegisterCatalog(t *testing.T) {
	api, memory, connection := newRegisterHandlerTestAPI(t, true)
	memory.runtime = nil
	memory.persistedChannels = []*ipc.Channel{{
		ID: "GBC_register_restored", DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, Type: ipc.TypeGB28181,
	}}
	ctx := newRegisterHandlerTestContext(t, connection, "register-restores-persisted-channels", 3600)

	api.handlerRegister(ctx)
	assertRegisterHandlerResponse(t, connection, "SIP/2.0 200 OK")

	if memory.loadChannelsCalls != 1 {
		t.Fatalf("REGISTER channel restoration calls = %d, want 1", memory.loadChannelsCalls)
	}
	if _, ok := memory.GetChannel(gb10DeviceID, gb10ChannelID); !ok {
		t.Fatal("successful REGISTER did not restore persisted channel before Catalog refresh")
	}
}

func TestRegisterChannelRestoreFailureDoesNotPublishPartialRuntime(t *testing.T) {
	api, memory, connection := newRegisterHandlerTestAPI(t, true)
	memory.runtime = nil
	memory.loadChannelsErr = errors.New("channel store unavailable")
	ctx := newRegisterHandlerTestContext(t, connection, "register-channel-restore-failure", 3600)

	api.handlerRegister(ctx)
	assertRegisterHandlerResponse(t, connection, "SIP/2.0 500 server db error")

	if memory.loadChannelsCalls != 1 || memory.loadOrStoreCalls != 0 {
		t.Fatalf("failed channel restoration calls = load:%d publish:%d, want 1/0", memory.loadChannelsCalls, memory.loadOrStoreCalls)
	}
	if memory.runtime != nil {
		t.Fatal("failed channel restoration published a partial device runtime")
	}
}

type registerHandlerTestMemory struct {
	*flowMemory
	changeErr         error
	loadChannelsErr   error
	persistedChannels []*ipc.Channel
	loadChannelsCalls int
	loadOrStoreCalls  int
	changeCalls       int
}

func (m *registerHandlerTestMemory) LoadOrStore(deviceID string, device *Device) {
	m.loadOrStoreCalls++
	m.flowMemory.LoadOrStore(deviceID, device)
}

func (m *registerHandlerTestMemory) LoadDeviceChannelsContext(_ context.Context, _ string, device *Device) error {
	m.loadChannelsCalls++
	if m.loadChannelsErr != nil {
		return m.loadChannelsErr
	}
	device.LoadChannels(m.persistedChannels...)
	return nil
}

func (m *registerHandlerTestMemory) Change(deviceID string, persistent func(*ipc.Device) error, runtime func(*Device)) error {
	m.changeCalls++
	if m.changeErr != nil {
		return m.changeErr
	}
	return m.flowMemory.Change(deviceID, persistent, runtime)
}

func newRegisterHandlerTestAPI(t *testing.T, createDevice bool) (*GB28181API, *registerHandlerTestMemory, *flowConnection) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s-%s?mode=memory&cache=shared", t.Name(), sip.RandString(12))))
	if err != nil {
		t.Fatal(err)
	}
	store := ipcdb.NewDB(db).AutoMigrate(true)
	if createDevice {
		device := &ipc.Device{
			ID: "GB_register_device", DeviceID: gb10DeviceID, Type: ipc.TypeGB28181,
			IsOnline: true, RegisteredAt: orm.Now(), KeepaliveAt: orm.Now(),
		}
		if err := db.Create(device).Error; err != nil {
			t.Fatal(err)
		}
	}
	connection := newFlowConnection()
	memory := &registerHandlerTestMemory{flowMemory: newFlowMemory(gb10DeviceID)}
	api := &GB28181API{
		cfg:              &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"},
		core:             ipc.NewAdapter(store, uniqueid.Core{}),
		catalogResponses: newMultiResponseCollector(func(item Channels) string { return item.ChannelID }),
		recordResponses:  newMultiResponseCollector(func(item RecordItem) string { return item.FilePath }),
	}
	api.svr = &Server{memoryStorer: memory}
	return api, memory, connection
}

func newRegisterHandlerTestContext(t *testing.T, connection *flowConnection, callID string, expires int) *sip.Context {
	t.Helper()
	request := newFlowRequest(t, connection, sip.MethodRegister, callID, nil)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: fmt.Sprint(expires)})
	return &sip.Context{
		Request: request, Tx: sip.NewTransaction(callID, connection), DeviceID: gb10DeviceID,
		Source: connection.remote, To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
		XGBVer: string(GBVersion10), XGBVerRaw: string(GBVersion10), Log: slog.Default(),
	}
}

func setRegisterHandlerTestVersion(ctx *sip.Context, version string) {
	ctx.Request.RemoveHeader("X-GB-Ver")
	ctx.XGBVer = ""
	ctx.XGBVerRaw = strings.TrimSpace(version)
	if ctx.XGBVerRaw == "" {
		return
	}
	ctx.Request.AppendHeader(&sip.GenericHeader{HeaderName: "X-GB-Ver", Contents: ctx.XGBVerRaw})
	if parsed, ok := ParseGBProtocolVersion(ctx.XGBVerRaw); ok {
		ctx.XGBVer = string(parsed)
	}
}

func assertRegisterHandlerResponse(t *testing.T, connection *flowConnection, expected string) {
	t.Helper()
	_ = assertRegisterHandlerResponsePayload(t, connection, expected)
}

func assertRegisterHandlerResponsePayload(t *testing.T, connection *flowConnection, expected string) string {
	t.Helper()
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), expected) {
			t.Fatalf("REGISTER response = %s, want %q", payload, expected)
		}
		return string(payload)
	case <-time.After(time.Second):
		t.Fatalf("REGISTER response timeout, want %q", expected)
		return ""
	}
}

func registerResponseDate(payload string) string {
	for _, line := range strings.Split(payload, "\r\n") {
		if strings.HasPrefix(line, "Date: ") {
			return strings.TrimPrefix(line, "Date: ")
		}
	}
	return ""
}

type countingRegisterStore struct {
	ipc.Storer
	device *countingRegisterDeviceStore
}

func (s *countingRegisterStore) Device() ipc.DeviceStorer { return s.device }

type countingRegisterDeviceStore struct {
	ipc.DeviceStorer
	getCalls atomic.Int32
}

func (s *countingRegisterDeviceStore) Get(ctx context.Context, device *ipc.Device, opts ...orm.QueryOption) error {
	s.getCalls.Add(1)
	return s.DeviceStorer.Get(ctx, device, opts...)
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
	modifiedRequest := request.Clone().(*sip.Request)
	modifiedRequest.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "0"})
	modifiedCtx := &sip.Context{Request: modifiedRequest, DeviceID: gb10DeviceID, Source: connection.remote}
	if err := api.validateRegisterAuthorization(modifiedCtx, header, gb10DeviceID, password); err == nil || !strings.Contains(err.Error(), "replay") {
		t.Fatalf("header-modified Digest nonce replay result = %v", err)
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

	wrongNonce := api.issueRegisterNonce(gb10DeviceID, registerNonceSourceIP(ctx))
	wrong := sip.AuthFromValue(fmt.Sprintf(`Digest realm="%s",nonce="%s",algorithm=MD5,qop="auth"`, cfg.GetDomain(), wrongNonce)).
		SetUsername(gb10DeviceID).
		SetPassword("wrong").
		SetMethod(sip.MethodRegister).
		SetURI(request.Recipient().String()).
		SetClientNonce("00000001", "client-nonce")
	if _, err := wrong.CalcResponseChecked(); err != nil {
		t.Fatal(err)
	}
	if err := api.validateRegisterAuthorization(ctx, &sip.GenericHeader{HeaderName: "Authorization", Contents: wrong.String()}, gb10DeviceID, password); err == nil || !strings.Contains(err.Error(), "response mismatch") {
		t.Fatalf("wrong REGISTER Digest password result = %v", err)
	}
}
