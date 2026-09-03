package gbs

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/annexg"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"golang.org/x/time/rate"
	"gorm.io/gorm"
)

func newAnnexGRuntime(t *testing.T, role annexg.SystemRole, allowInsecure bool) (*annexGService, *gorm.DB) {
	t.Helper()
	return newAnnexGRuntimeWithConfig(t, annexGTestSIPConfig(role, allowInsecure))
}

func newAnnexGRuntimeWithConfig(t *testing.T, cfg conf.SIP) (*annexGService, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:annex-g-"+sip.RandString(12)+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := newAnnexGService(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.close)
	return service, db
}

func TestNewAnnexGServiceContextHonorsCancellation(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	service, err := newAnnexGServiceContext(ctx, annexGTestSIPConfig(annexg.RoleTollgateSystem, true), db)
	if service != nil {
		service.close()
		t.Fatal("canceled Annex G constructor returned a service")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("newAnnexGServiceContext error = %v, want context.Canceled", err)
	}
}

func annexGTestSIPConfig(role annexg.SystemRole, allowInsecure bool) conf.SIP {
	cfg := conf.DefaultConfig().Sip
	cfg.ID = gb10PlatformID
	cfg.Domain = "3402000000"
	cfg.AnnexG = conf.SIPAnnexG{
		Enabled: true, MaxSendRecords: 100,
		Systems: []conf.SIPAnnexGSystem{{
			ID: gb10DeviceID, Role: string(role), Version: "1.0", Password: "secret",
			Address: "192.0.2.10:5060", SourceCIDRs: []string{"192.0.2.10"}, AllowInsecureTransport: allowInsecure,
		}},
	}
	if !allowInsecure {
		cfg.EnableTLS = true
		cfg.AnnexG.Systems[0].Transport = "tls"
	} else {
		cfg.AnnexG.Systems[0].Transport = "udp"
	}
	return cfg
}

func TestAnnexGRuntimePreservesOpaqueSecrets(t *testing.T) {
	cfg := annexGTestSIPConfig(annexg.RoleEmergencyCommandSystem, true)
	cfg.AnnexG.Systems[0].Password = " secret "
	cfg.AnnexG.Systems[0].SignalDigestSeed = " note-seed "
	service, _ := newAnnexGRuntimeWithConfig(t, cfg)
	system := service.systems[gb10DeviceID]
	if system == nil {
		t.Fatal("Annex G system was not loaded")
	}
	if system.password != " secret " || system.signalDigestSeed != " note-seed " {
		t.Fatalf("Annex G secrets changed = password:%q seed:%q", system.password, system.signalDigestSeed)
	}
}

func newAnnexGContext(t *testing.T, connection *flowConnection, body []byte) *sip.Context {
	t.Helper()
	request := newFlowRequest(t, connection, sip.MethodMessage, "annex-g-"+sip.RandString(8), body)
	return &sip.Context{
		Request: request, Tx: sip.NewTransaction("annex-g-tx-"+sip.RandString(8), connection),
		DeviceID: gb10DeviceID, Source: connection.remote, XGBVer: "1.0", XGBVerRaw: "1.0",
		To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3401000000"), Log: slog.Default(),
	}
}

func addAnnexGDigest(t *testing.T, api *GB28181API, ctx *sip.Context, password, nonce, nc string) {
	t.Helper()
	requestURI := ctx.Request.Recipient().String()
	auth := sip.AuthFromValue(fmt.Sprintf(`Digest realm="%s",nonce="%s",algorithm=MD5,qop="auth"`, api.annexG.realm, nonce)).
		SetUsername(gb10DeviceID).
		SetPassword(password).
		SetMethod(sip.MethodMessage).
		SetURI(requestURI).
		SetClientNonce(nc, "annex-g-client")
	if _, err := auth.CalcResponseChecked(); err != nil {
		t.Fatal(err)
	}
	ctx.Request.AppendHeader(&sip.GenericHeader{HeaderName: "Authorization", Contents: auth.String()})
}

type annexGTestTCPConn struct {
	net.Conn
	local  net.Addr
	remote net.Addr
}

func (conn *annexGTestTCPConn) LocalAddr() net.Addr  { return conn.local }
func (conn *annexGTestTCPConn) RemoteAddr() net.Addr { return conn.remote }

func testECSAlarmBody(t *testing.T) []byte {
	t.Helper()
	body, err := annexg.Encode(annexg.Version2011, &annexg.ECSAlarmNotify{
		CmdType: annexg.CommandECSAlarm, SN: 71,
		AlarmContent: annexg.ECSAlarmRecord{
			AlarmNO: "ecs-71", AlarmTime: "2026-08-27T10:20:30", AlarmPriority: "1",
			AlarmClass: "1", AlarmAddress: "address", AlarmMethod: "2", AlarmTelephone: "110",
			Processor: "processor", SrecipientName: "operator", NsStatus: "open", NCallType: "alarm",
			AlarmInfo: "alarm info",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestAnnexGCommandFromBodyRequiresStandardEnvelope(t *testing.T) {
	tests := []struct {
		name string
		body string
		want annexg.Command
	}{
		{name: "notification", body: `<Notify><CmdType>ECSAlarm</CmdType></Notify>`, want: annexg.CommandECSAlarm},
		{name: "query", body: `<Query><CmdType>ECSAlarmRecordList</CmdType></Query>`, want: annexg.CommandECSAlarmRecordList},
		{name: "response", body: `<Response><CmdType>ConfigDefence</CmdType></Response>`, want: annexg.CommandConfigDefence},
		{name: "standard namespace", body: `<Notify xmlns="http://www.w3.org/namespace/"><CmdType>ECSAlarm</CmdType></Notify>`, want: annexg.CommandECSAlarm},
		{name: "unknown root", body: `<Vendor><CmdType>ECSAlarm</CmdType></Vendor>`},
		{name: "notification command under query", body: `<Query><CmdType>ECSAlarm</CmdType></Query>`},
		{name: "query command under notify", body: `<Notify><CmdType>ECSAlarmRecordList</CmdType></Notify>`},
		{name: "duplicate command", body: `<Notify><CmdType>MPAlarm</CmdType><CmdType>ECSAlarm</CmdType></Notify>`},
		{name: "command whitespace", body: `<Notify><CmdType> ECSAlarm </CmdType></Notify>`},
		{name: "namespaced root", body: `<Notify xmlns="urn:invalid"><CmdType>ECSAlarm</CmdType></Notify>`},
		{name: "root attribute", body: `<Notify vendor="x"><CmdType>ECSAlarm</CmdType></Notify>`},
		{name: "command attribute", body: `<Notify><CmdType vendor="x">ECSAlarm</CmdType></Notify>`},
		{name: "nested command", body: `<Notify><CmdType><Value>ECSAlarm</Value></CmdType></Notify>`},
		{name: "multiple roots", body: `<Notify><CmdType>ECSAlarm</CmdType></Notify><Notify><CmdType>ECSAlarm</CmdType></Notify>`},
		{name: "malformed", body: `<Notify><CmdType>ECSAlarm</Notify>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := annexGCommandFromBody([]byte(test.body)); got != test.want {
				t.Fatalf("annexGCommandFromBody() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAccessControlDoesNotMisclassifyNonAnnexGEnvelope(t *testing.T) {
	service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.LastRegisterAt = time.Now()
		device.Expires = 3600
	})
	api := &GB28181API{
		annexG: service,
		svr:    &Server{memoryStorer: memory},
	}
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodMessage, "ordinary-non-annex-g", []byte(
		`<Vendor><CmdType>ECSAlarm</CmdType></Vendor>`,
	))
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("ordinary-non-annex-g-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote, Log: slog.Default(),
	}

	api.sipAccessControlMiddleware(ctx)

	if ctx.IsAborted() {
		t.Fatal("registered ordinary device was diverted to Annex G access control")
	}
	if _, ok := ctx.Get(annexGSystemContextKey); ok {
		t.Fatal("non-Annex G envelope received Annex G authentication context")
	}
}

func TestAccessControlDoesNotClassifyAnnexGXMLOnSIPNotify(t *testing.T) {
	service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.LastRegisterAt = time.Now()
		device.Expires = 3600
	})
	api := &GB28181API{
		annexG: service,
		svr:    &Server{memoryStorer: memory},
	}
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodNotify, "ordinary-notify", testECSAlarmBody(t))
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("ordinary-notify-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote, Log: slog.Default(),
	}

	api.sipAccessControlMiddleware(ctx)

	if ctx.IsAborted() {
		t.Fatal("SIP NOTIFY was diverted to the MESSAGE-only Annex G access control")
	}
	if _, ok := ctx.Get(annexGSystemContextKey); ok {
		t.Fatal("SIP NOTIFY received Annex G authentication context")
	}
}

func TestAnnexGAccessControl(t *testing.T) {
	body := testECSAlarmBody(t)

	t.Run("challenge and success", func(t *testing.T) {
		service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
		api := &GB28181API{cfg: &conf.SIP{ID: gb10PlatformID, Domain: service.realm}, annexG: service, messageNonces: make(map[string]messageNonceState)}
		connection := newFlowConnection()
		challengeCtx := newAnnexGContext(t, connection, body)
		api.sipAccessControlMiddleware(challengeCtx)
		select {
		case response := <-connection.writes:
			if !strings.Contains(string(response), "401 Unauthorized") || !strings.Contains(string(response), "WWW-Authenticate") {
				t.Fatalf("challenge = %s", response)
			}
		case <-time.After(time.Second):
			t.Fatal("Digest challenge timeout")
		}

		ctx := newAnnexGContext(t, connection, body)
		nonce := api.issueMessageNonce(gb10DeviceID, "192.0.2.10")
		addAnnexGDigest(t, api, ctx, "secret", nonce, "00000001")
		api.sipAccessControlMiddleware(ctx)
		if _, ok := ctx.Get(annexGSystemContextKey); !ok || ctx.IsAborted() {
			t.Fatal("valid Annex G credentials were rejected")
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
		api := &GB28181API{cfg: &conf.SIP{ID: gb10PlatformID, Domain: service.realm}, annexG: service, messageNonces: make(map[string]messageNonceState)}
		connection := newFlowConnection()
		ctx := newAnnexGContext(t, connection, body)
		nonce := api.issueMessageNonce(gb10DeviceID, "192.0.2.10")
		addAnnexGDigest(t, api, ctx, "wrong", nonce, "00000001")
		api.sipAccessControlMiddleware(ctx)
		if !ctx.IsAborted() {
			t.Fatal("wrong Annex G password was accepted")
		}
	})

	t.Run("source and version", func(t *testing.T) {
		service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
		api := &GB28181API{cfg: &conf.SIP{ID: gb10PlatformID, Domain: service.realm}, annexG: service, messageNonces: make(map[string]messageNonceState)}
		for _, test := range []struct {
			name   string
			mutate func(*sip.Context)
		}{
			{name: "source", mutate: func(ctx *sip.Context) { ctx.Source = &testAddr{network: "udp", value: "192.0.2.11:5060"} }},
			{name: "version", mutate: func(ctx *sip.Context) { ctx.XGBVerRaw = "2.0" }},
		} {
			t.Run(test.name, func(t *testing.T) {
				ctx := newAnnexGContext(t, newFlowConnection(), body)
				test.mutate(ctx)
				api.sipAccessControlMiddleware(ctx)
				if !ctx.IsAborted() {
					t.Fatal("invalid Annex G peer was accepted")
				}
			})
		}
	})

	t.Run("TLS required", func(t *testing.T) {
		service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, false)
		api := &GB28181API{cfg: &conf.SIP{ID: gb10PlatformID, Domain: service.realm}, annexG: service, messageNonces: make(map[string]messageNonceState)}
		connection := newFlowConnection()
		ctx := newAnnexGContext(t, connection, body)
		api.sipAccessControlMiddleware(ctx)
		if !ctx.IsAborted() {
			t.Fatal("plaintext Annex G transport was accepted")
		}

		tlsCtx := newAnnexGContext(t, connection, body)
		tlsCtx.Request.SetConnection(&tlsFlowConnection{flowConnection: connection})
		nonce := api.issueMessageNonce(gb10DeviceID, "192.0.2.10")
		addAnnexGDigest(t, api, tlsCtx, "secret", nonce, "00000001")
		api.sipAccessControlMiddleware(tlsCtx)
		if tlsCtx.IsAborted() {
			t.Fatal("authenticated SIP-TLS Annex G transport was rejected")
		}
	})

	t.Run("Digest replay", func(t *testing.T) {
		service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
		api := &GB28181API{cfg: &conf.SIP{ID: gb10PlatformID, Domain: service.realm}, annexG: service, messageNonces: make(map[string]messageNonceState)}
		connection := newFlowConnection()
		nonce := api.issueMessageNonce(gb10DeviceID, "192.0.2.10")
		first := newAnnexGContext(t, connection, body)
		addAnnexGDigest(t, api, first, "secret", nonce, "00000001")
		api.sipAccessControlMiddleware(first)
		if first.IsAborted() {
			t.Fatal("first authenticated request was rejected")
		}
		authorization := first.Request.GetHeaders("Authorization")[0].Clone()
		replay := newAnnexGContext(t, connection, body)
		replay.Request.AppendHeader(authorization)
		api.sipAccessControlMiddleware(replay)
		if !replay.IsAborted() {
			t.Fatal("cross-request Digest replay was accepted")
		}
	})

	t.Run("per-system rate limit", func(t *testing.T) {
		service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
		service.systems[gb10DeviceID].inboundLimiter = rate.NewLimiter(0, 1)
		api := &GB28181API{cfg: &conf.SIP{ID: gb10PlatformID, Domain: service.realm}, annexG: service, messageNonces: make(map[string]messageNonceState)}
		connection := newFlowConnection()
		first := newAnnexGContext(t, connection, body)
		firstNonce := api.issueMessageNonce(gb10DeviceID, "192.0.2.10")
		addAnnexGDigest(t, api, first, "secret", firstNonce, "00000001")
		api.sipAccessControlMiddleware(first)
		if first.IsAborted() {
			t.Fatal("first request within Annex G burst was rejected")
		}

		second := newAnnexGContext(t, connection, body)
		secondNonce := api.issueMessageNonce(gb10DeviceID, "192.0.2.10")
		addAnnexGDigest(t, api, second, "secret", secondNonce, "00000001")
		api.sipAccessControlMiddleware(second)
		if !second.IsAborted() {
			t.Fatal("Annex G request exceeding the per-system limit was accepted")
		}
		select {
		case response := <-connection.writes:
			payload := string(response)
			if !strings.Contains(payload, "503 Service Unavailable") || !strings.Contains(payload, "Retry-After: 1") {
				t.Fatalf("rate limit response = %s", payload)
			}
		case <-time.After(time.Second):
			t.Fatal("Annex G rate limit response timeout")
		}
		metrics := api.metrics.Snapshot()
		if metrics.AnnexGRequests != 2 || metrics.AnnexGAccepted != 1 || metrics.AnnexGRejected != 1 || metrics.AnnexGRateLimited != 1 {
			t.Fatalf("Annex G access metrics = %+v", metrics)
		}
	})
}

func TestAnnexGSignalDigestUsesRegisteredSIPRoute(t *testing.T) {
	cfg := annexGTestSIPConfig(annexg.RoleEmergencyCommandSystem, true)
	cfg.SignalDigest = conf.SIPSignalDigest{
		Enabled: true, Required: true, Seed: "global-note-seed", Algorithm: "MD5",
		Encoding: "base64", Window: conf.Duration(10 * time.Minute),
	}
	cfg.AnnexG.Systems[0].SignalDigestSeed = "annex-g-note-seed"
	service, _ := newAnnexGRuntimeWithConfig(t, cfg)
	responses := make(chan annexg.Message, 1)
	service.send = func(_ *sip.Context, _ *annexGSystem, _ annexg.Version, response annexg.Message) error {
		responses <- response
		return nil
	}
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Password = "ordinary-device-seed"
	localAddress := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.1:5060")
	sipServer := sip.NewServer(localAddress)
	api := &GB28181API{
		cfg: &cfg, annexG: service, lifecycleCtx: context.Background(),
		messageNonces: make(map[string]messageNonceState),
	}
	api.svr = &Server{Server: sipServer, gb: api, fromAddress: *localAddress, memoryStorer: memory}
	sipServer.SetRequestSecurityResolver(api.resolveRequestSecurity)
	registerAnnexGRoutes(sipServer.Message(api.sipAccessControlMiddleware), api.sipAnnexGMessage)

	localRaw, remoteRaw := net.Pipe()
	wrapped := &annexGTestTCPConn{
		Conn:   localRaw,
		local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 5060},
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 41001},
	}
	connection := sip.NewTCPConnection(wrapped)
	go sipServer.ProcessTCPConnection(connection)
	t.Cleanup(func() {
		_ = remoteRaw.Close()
		_ = connection.Close()
		sipServer.Close()
	})

	request := newFlowRequest(t, newFlowConnection(), sip.MethodMessage, "annex-g-registered-route", testECSAlarmBody(t))
	via, _ := request.ViaHop()
	via.Transport = "TCP"
	signer, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
		Seed: "annex-g-note-seed", Algorithm: "MD5", Encoding: "base64", Required: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.Sign(request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(remoteRaw)
	if _, err := io.WriteString(remoteRaw, request.String()); err != nil {
		t.Fatal(err)
	}
	challengeFrame, err := readAnnexGTestSIPFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	challenge := annexGTestSIPHeader(challengeFrame, "WWW-Authenticate")
	nonce := sip.AuthFromValue(challenge).Get("nonce")
	if !strings.HasPrefix(challengeFrame, "SIP/2.0 401 Unauthorized\r\n") || nonce == "" ||
		annexGTestSIPHeader(challengeFrame, "Date") == "" ||
		!strings.Contains(annexGTestSIPHeader(challengeFrame, "Note"), "Digest nonce=") {
		t.Fatalf("Annex G registered route challenge = %q", challengeFrame)
	}

	retry, _ := request.Clone().(*sip.Request)
	cseq, _ := retry.CSeq()
	retry.RemoveHeader("CSeq")
	retry.AppendHeader(&sip.CSeq{SeqNo: cseq.SeqNo + 1, MethodName: sip.MethodMessage})
	retry.RemoveHeader("Via")
	retry.AppendHeader(sip.ViaHeader{&sip.ViaHop{
		ProtocolName: "SIP", ProtocolVersion: "2.0", Host: "192.0.2.10", Port: sip.NewPort(5060), Transport: "TCP",
		Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
	}})
	addAnnexGDigest(t, api, &sip.Context{Request: retry}, "secret", nonce, "00000001")
	if err := signer.Sign(retry); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(remoteRaw, retry.String()); err != nil {
		t.Fatal(err)
	}
	frame, err := readAnnexGTestSIPFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(frame, "SIP/2.0 200 OK\r\n") || annexGTestSIPHeader(frame, "Date") == "" ||
		!strings.Contains(annexGTestSIPHeader(frame, "Note"), "Digest nonce=") {
		t.Fatalf("Annex G registered route response = %q", frame)
	}
	select {
	case response := <-responses:
		result, ok := response.(*annexg.NotificationResponse)
		if !ok || result.CmdType != annexg.CommandECSAlarm || result.SN != 71 || result.Result != annexg.ResultOK {
			t.Fatalf("Annex G registered route business response = %#v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("Annex G registered route did not reach the business handler")
	}
}

func TestAnnexGHandlerAcknowledgesBeforeBusinessResponse(t *testing.T) {
	service, db := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	responses := make(chan annexg.Message, 1)
	service.send = func(_ *sip.Context, _ *annexGSystem, _ annexg.Version, response annexg.Message) error {
		responses <- response
		return nil
	}
	api := &GB28181API{annexG: service, lifecycleCtx: context.Background()}
	connection := newFlowConnection()
	ctx := newAnnexGContext(t, connection, testECSAlarmBody(t))
	ctx.Set(annexGSystemContextKey, service.systems[gb10DeviceID])

	api.sipAnnexGMessage(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200 OK") {
			t.Fatalf("SIP response = %s", response)
		}
	default:
		t.Fatal("SIP 200 was not sent before handler returned")
	}
	select {
	case response := <-responses:
		ack, ok := response.(*annexg.NotificationResponse)
		if !ok || ack.CmdType != annexg.CommandECSAlarm || ack.SN != 71 || ack.Result != annexg.ResultOK {
			t.Fatalf("business response = %#v", response)
		}
	default:
		t.Fatal("Annex G business response was not sent")
	}
	var count int64
	if err := db.Table("gb_annex_g_alarm_records").Where("kind = ?", "ecs").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("stored ECS alarms = %d, want 1", count)
	}
}

func TestAnnexGRequestConsumesOnlyAfterSuccessfulSIPOK(t *testing.T) {
	service, db := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	responses := make(chan annexg.Message, 1)
	service.send = func(_ *sip.Context, _ *annexGSystem, _ annexg.Version, response annexg.Message) error {
		responses <- response
		return nil
	}
	api := &GB28181API{annexG: service, lifecycleCtx: context.Background()}
	base := newFlowConnection()
	connection := &blockingFlowResponseConnection{
		flowConnection: base,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
		writeErr:       errors.New("Annex G SIP OK write failed"),
	}
	ctx := newAnnexGContext(t, base, testECSAlarmBody(t))
	ctx.Request.SetConnection(connection)
	ctx.Tx = sip.NewTransaction("annex-g-request-write-failure", connection)
	ctx.Set(annexGSystemContextKey, service.systems[gb10DeviceID])
	done := make(chan struct{})
	go func() {
		api.sipAnnexGMessage(ctx)
		close(done)
	}()

	select {
	case <-connection.started:
	case <-time.After(time.Second):
		close(connection.release)
		t.Fatal("Annex G SIP OK write did not start")
	}
	var count int64
	if err := db.Table("gb_annex_g_alarm_records").Where("kind = ?", "ecs").Count(&count).Error; err != nil {
		close(connection.release)
		t.Fatal(err)
	}
	if count != 0 {
		close(connection.release)
		t.Fatalf("ECS alarm was stored before SIP OK completed: %d", count)
	}
	select {
	case response := <-responses:
		close(connection.release)
		t.Fatalf("business response was sent before SIP OK completed: %#v", response)
	default:
	}

	close(connection.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Annex G handler did not return after SIP OK write failure")
	}
	if err := db.Table("gb_annex_g_alarm_records").Where("kind = ?", "ecs").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("ECS alarm was stored after SIP OK write failure: %d", count)
	}
	select {
	case response := <-responses:
		t.Fatalf("business response was sent after SIP OK write failure: %#v", response)
	default:
	}
}

func TestAnnexGHandlerUsesRealSIPPathForBusinessResponse(t *testing.T) {
	service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	signalSecurity, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
		Seed: "annex-g-note-seed", Algorithm: "MD5", Encoding: "base64",
	})
	if err != nil {
		t.Fatal(err)
	}
	service.systems[gb10DeviceID].setSignalSecurity(signalSecurity)
	localAddress := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.1:5060")
	sipServer := sip.NewServer(localAddress)
	localRaw, remoteRaw := net.Pipe()
	local := sip.NewTCPConnection(localRaw)
	remoteDone := make(chan error, 1)
	go sipServer.ProcessTCPConnection(local)
	go func() {
		defer remoteRaw.Close()
		reader := bufio.NewReader(remoteRaw)
		ack, err := readAnnexGTestSIPFrame(reader)
		if err != nil {
			remoteDone <- err
			return
		}
		if !strings.HasPrefix(ack, "SIP/2.0 200 OK\r\n") {
			remoteDone <- fmt.Errorf("Annex G SIP acknowledgement = %q", ack)
			return
		}
		businessResponse, err := readAnnexGTestSIPFrame(reader)
		if err != nil {
			remoteDone <- err
			return
		}
		if !strings.HasPrefix(businessResponse, "MESSAGE sip:"+gb10DeviceID+"@3401000000 SIP/2.0\r\n") ||
			annexGTestSIPHeader(businessResponse, "X-GB-Ver") != "1.0" ||
			annexGTestSIPHeader(businessResponse, "Date") == "" ||
			!strings.Contains(annexGTestSIPHeader(businessResponse, "Note"), "Digest nonce=") ||
			!strings.Contains(businessResponse, "<CmdType>ECSAlarm</CmdType>") ||
			!strings.Contains(businessResponse, "<SN>71</SN>") ||
			!strings.Contains(businessResponse, "<Result>OK</Result>") {
			remoteDone <- fmt.Errorf("Annex G business response = %q", businessResponse)
			return
		}
		_, err = io.WriteString(remoteRaw, annexGTestSIPResponse(businessResponse, 200, "OK", ""))
		remoteDone <- err
	}()

	lifecycleCtx, cancel := context.WithCancel(context.Background())
	api := &GB28181API{annexG: service, lifecycleCtx: lifecycleCtx, lifecycleCancel: cancel}
	server := &Server{Server: sipServer, gb: api, fromAddress: *localAddress}
	api.svr = server
	service.send = api.sendAnnexGResponse
	t.Cleanup(func() {
		cancel()
		_ = local.Close()
		sipServer.Close()
	})

	ctx := newAnnexGContext(t, newFlowConnection(), testECSAlarmBody(t))
	ctx.Request.SetConnection(local)
	ctx.Request.SetSource(local.RemoteAddr())
	ctx.Source = local.RemoteAddr()
	ctx.Tx = sip.NewTransaction("annex-g-real-response-"+sip.RandString(8), local)
	ctx.From = localAddress
	ctx.Set(annexGSystemContextKey, service.systems[gb10DeviceID])
	api.sipAnnexGMessage(ctx)

	select {
	case err := <-remoteDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Annex G real SIP business response did not finish")
	}
}

func TestAnnexGHandlerQueriesManagementPlatformRecords(t *testing.T) {
	service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	record := annexg.MPAlarmRecord{
		AlarmNO: "mp-81", AlarmTime: "2026-08-27T10:20:30", DeviceID: "34020000001320000081",
		AlarmPriority: "1", AlarmMethod: "2", OriginalNO: "original-81", OriginalInfo: "original",
		Sender: "sender", Processor: "processor", AlarmLevel: "1", Disposal: "processed", AlarmInfo: "alarm",
	}
	if err := service.store.SaveMPAlarmRecord(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	responseChannel := make(chan annexg.Message, 1)
	service.send = func(_ *sip.Context, _ *annexGSystem, _ annexg.Version, response annexg.Message) error {
		responseChannel <- response
		return nil
	}
	api := &GB28181API{annexG: service, lifecycleCtx: context.Background()}
	body, err := annexg.Encode(annexg.Version2011, &annexg.AlarmRecordQuery{CmdType: annexg.CommandMPAlarmRecordList, SN: 82})
	if err != nil {
		t.Fatal(err)
	}
	ctx := newAnnexGContext(t, newFlowConnection(), body)
	ctx.Set(annexGSystemContextKey, service.systems[gb10DeviceID])
	api.sipAnnexGMessage(ctx)

	select {
	case response := <-responseChannel:
		result, ok := response.(*annexg.MPAlarmRecordListResponse)
		if !ok || result.Result != annexg.ResultOK || result.RealRecordNum != 1 || result.SendRecordNum != 1 ||
			len(result.RecordList.AlarmRecords) != 1 || result.RecordList.AlarmRecords[0].AlarmNO != record.AlarmNO {
			t.Fatalf("query response = %#v", response)
		}
	default:
		t.Fatal("MPAlarmRecordList response was not sent")
	}
}

func TestAnnexGManagementPlatformQueryResponsePersistsRecords(t *testing.T) {
	service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	request := &annexg.AlarmRecordQuery{CmdType: annexg.CommandMPAlarmRecordList, SN: 83}
	record := annexg.MPAlarmRecord{
		AlarmNO: "mp-query-83", AlarmTime: "2026-08-27T10:20:30", DeviceID: "34020000001320000083",
		AlarmPriority: "1", AlarmMethod: "2", OriginalNO: "original-83", OriginalInfo: "original",
		Sender: "sender", Processor: "processor", AlarmLevel: "1", Disposal: "processed", AlarmInfo: "alarm",
	}
	response := &annexg.MPAlarmRecordListResponse{
		CmdType: annexg.CommandMPAlarmRecordList, SN: request.SN, Result: annexg.ResultOK,
		RealRecordNum: 1, SendRecordNum: 1,
		RecordList: annexg.MPAlarmRecordList{AlarmRecords: []annexg.MPAlarmRecord{record}},
	}
	if err := service.persistOutboundResponse(context.Background(), request, response); err != nil {
		t.Fatalf("persist MP query response = %v", err)
	}
	stored, err := service.store.QueryMPAlarmRecords(context.Background(), *request)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RealRecordNum != 1 || stored.SendRecordNum != 1 || len(stored.RecordList.AlarmRecords) != 1 || stored.RecordList.AlarmRecords[0].AlarmNO != record.AlarmNO {
		t.Fatalf("stored MP query response = %#v", stored)
	}
}

type annexGConsumerFunc func(context.Context, annexg.Exchange) (annexg.Message, error)

func (fn annexGConsumerFunc) ConsumeAnnexG(ctx context.Context, exchange annexg.Exchange) (annexg.Message, error) {
	return fn(ctx, exchange)
}

func TestAnnexGHandlerReturnsBusinessErrorAfterConsumptionFailure(t *testing.T) {
	service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	service.adapter.Consumer = annexGConsumerFunc(func(context.Context, annexg.Exchange) (annexg.Message, error) {
		return nil, fmt.Errorf("database unavailable")
	})
	responseChannel := make(chan annexg.Message, 1)
	service.send = func(_ *sip.Context, _ *annexGSystem, _ annexg.Version, response annexg.Message) error {
		responseChannel <- response
		return nil
	}
	api := &GB28181API{annexG: service, lifecycleCtx: context.Background()}
	connection := newFlowConnection()
	ctx := newAnnexGContext(t, connection, testECSAlarmBody(t))
	ctx.Set(annexGSystemContextKey, service.systems[gb10DeviceID])
	api.sipAnnexGMessage(ctx)

	select {
	case sipResponse := <-connection.writes:
		if !strings.Contains(string(sipResponse), "SIP/2.0 200 OK") {
			t.Fatalf("SIP response = %s", sipResponse)
		}
	default:
		t.Fatal("SIP transaction was not acknowledged")
	}
	select {
	case response := <-responseChannel:
		result, ok := response.(*annexg.NotificationResponse)
		if !ok || result.Result != annexg.ResultError || result.CmdType != annexg.CommandECSAlarm || result.SN != 71 {
			t.Fatalf("business error response = %#v", response)
		}
	default:
		t.Fatal("business ERROR response was not sent")
	}
}

func TestAnnexGHandlerRejectsRoleDirectionMismatch(t *testing.T) {
	service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	sent := false
	service.send = func(*sip.Context, *annexGSystem, annexg.Version, annexg.Message) error {
		sent = true
		return nil
	}
	body, err := annexg.Encode(annexg.Version2011, &annexg.TGSAlarmNotify{
		CmdType: annexg.CommandTGSAlarm, SN: 91,
		AlarmContent: annexg.TGSAlarmRecord{
			AlarmTime: "2026-08-27T10:20:30", TollgateID: "34020000001990000001",
			CarPlate: "浙A12345", PlateType: "02", DefenceType: "wanted",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := newFlowConnection()
	ctx := newAnnexGContext(t, connection, body)
	ctx.Set(annexGSystemContextKey, service.systems[gb10DeviceID])
	api := &GB28181API{annexG: service, lifecycleCtx: context.Background()}
	api.sipAnnexGMessage(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 400") {
			t.Fatalf("direction rejection = %s", response)
		}
	default:
		t.Fatal("direction mismatch was not rejected")
	}
	if sent {
		t.Fatal("direction mismatch produced a business response")
	}
}

func TestAnnexGDisabledDoesNotInitializeRuntime(t *testing.T) {
	service, err := newAnnexGService(conf.DefaultConfig().Sip, nil)
	if err != nil || service != nil {
		t.Fatalf("disabled Annex G runtime = %#v, %v; want nil, nil", service, err)
	}
}

func TestBuildAnnexGDigestRetry(t *testing.T) {
	service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	system := service.systems[gb10DeviceID]
	system.realm = "3401000000"
	body := []byte(`<Response><CmdType>ECSAlarm</CmdType><SN>71</SN><Result>OK</Result></Response>`)
	for _, version := range []annexg.Version{annexg.Version2011, annexg.Version2014, annexg.Version2016} {
		for _, challengeStatus := range []struct {
			status          int
			reason          string
			challengeHeader string
			authorizeHeader string
		}{
			{status: 401, reason: "Unauthorized", challengeHeader: "WWW-Authenticate", authorizeHeader: "Authorization"},
			{status: 407, reason: "Proxy Authentication Required", challengeHeader: "Proxy-Authenticate", authorizeHeader: "Proxy-Authorization"},
		} {
			t.Run(string(version)+"/"+strconv.Itoa(challengeStatus.status), func(t *testing.T) {
				system.version = version
				connection := newFlowConnection()
				ctx := newAnnexGContext(t, connection, testECSAlarmBody(t))
				ctx.From = mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
				request, err := buildAnnexGResponseRequest(ctx, version, body)
				if err != nil {
					t.Fatal(err)
				}
				originalCSeq, _ := request.CSeq()
				originalVia, _ := request.ViaHop()
				originalCallID, _ := request.CallID()
				challenge := sip.NewResponseFromRequest("", request, challengeStatus.status, challengeStatus.reason, nil)
				challenge.AppendHeader(&sip.GenericHeader{
					HeaderName: challengeStatus.challengeHeader,
					Contents:   `Digest realm="3401000000",qop="auth",nonce="remote-nonce",algorithm=MD5`,
				})
				retry, err := buildAnnexGDigestRetry(request, challenge, system, system.realm)
				if err != nil {
					t.Fatal(err)
				}
				retryCSeq, ok := retry.CSeq()
				if !ok || retryCSeq.SeqNo != originalCSeq.SeqNo+1 || retryCSeq.MethodName != sip.MethodMessage {
					t.Fatalf("retry CSeq = %#v, original = %#v", retryCSeq, originalCSeq)
				}
				retryVia, ok := retry.ViaHop()
				if !ok || retryVia == nil || originalVia == nil || retryVia.Params.String() == originalVia.Params.String() {
					t.Fatalf("retry Via = %#v, original = %#v", retryVia, originalVia)
				}
				retryCallID, ok := retry.CallID()
				if !ok || retryCallID == nil || originalCallID == nil || *retryCallID != *originalCallID {
					t.Fatalf("retry Call-ID = %#v, original = %#v", retryCallID, originalCallID)
				}
				if retry.Recipient().String() != request.Recipient().String() || string(retry.Body()) != string(body) {
					t.Fatalf("retry changed target or body: target=%s body=%s", retry.Recipient(), retry.Body())
				}
				versionHeaders := retry.GetHeaders("X-GB-Ver")
				if len(versionHeaders) != 1 || !strings.Contains(versionHeaders[0].String(), string(version)) {
					t.Fatalf("retry X-GB-Ver = %v", versionHeaders)
				}
				headers := retry.GetHeaders(challengeStatus.authorizeHeader)
				if len(headers) != 1 {
					t.Fatalf("retry %s headers = %d", challengeStatus.authorizeHeader, len(headers))
				}
				otherHeader := "Authorization"
				if challengeStatus.authorizeHeader == otherHeader {
					otherHeader = "Proxy-Authorization"
				}
				if headers := retry.GetHeaders(otherHeader); len(headers) != 0 {
					t.Fatalf("retry unexpectedly contains %s: %v", otherHeader, headers)
				}
				auth := sip.AuthFromValue(headers[0].String())
				if auth.Get("username") != gb10DeviceID || auth.Get("realm") != system.realm || auth.Get("nonce") != "remote-nonce" ||
					auth.Get("uri") != request.Recipient().String() || auth.QOP() != "auth" || auth.Get("nc") != "00000001" || auth.Get("response") == "" {
					t.Fatalf("retry %s = %s", challengeStatus.authorizeHeader, headers[0].String())
				}
			})
		}
	}
}

func TestBuildAnnexGDigestRetryRejectsInvalidChallenges(t *testing.T) {
	service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	system := service.systems[gb10DeviceID]
	system.realm = "3401000000"
	connection := newFlowConnection()
	ctx := newAnnexGContext(t, connection, testECSAlarmBody(t))
	ctx.From = mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	request, err := buildAnnexGResponseRequest(ctx, annexg.Version2011, []byte(`<Response><CmdType>ECSAlarm</CmdType><SN>71</SN><Result>OK</Result></Response>`))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		status  int
		headers []string
	}{
		{name: "missing challenge", status: 401},
		{name: "duplicate challenge", status: 401, headers: []string{`Digest realm="3401000000",nonce="one"`, `Digest realm="3401000000",nonce="two"`}},
		{name: "missing Digest scheme", status: 401, headers: []string{`realm="3401000000",nonce="remote-nonce"`}},
		{name: "duplicate realm", status: 401, headers: []string{`Digest realm="3401000000",realm="3401000000",nonce="remote-nonce"`}},
		{name: "duplicate nonce", status: 401, headers: []string{`Digest realm="3401000000",nonce="one",nonce="two"`}},
		{name: "duplicate algorithm", status: 401, headers: []string{`Digest realm="3401000000",nonce="remote-nonce",algorithm=MD5,algorithm=MD5`}},
		{name: "duplicate qop", status: 401, headers: []string{`Digest realm="3401000000",nonce="remote-nonce",qop=auth,qop=auth`}},
		{name: "malformed parameter", status: 401, headers: []string{`Digest realm="3401000000",nonce="remote-nonce",broken`}},
		{name: "wrong realm", status: 401, headers: []string{`Digest realm="other",nonce="remote-nonce"`}},
		{name: "unsupported algorithm", status: 401, headers: []string{`Digest realm="3401000000",nonce="remote-nonce",algorithm=SHA-512`}},
		{name: "unsupported qop", status: 401, headers: []string{`Digest realm="3401000000",nonce="remote-nonce",qop=auth-int`}},
		{name: "missing proxy challenge", status: 407},
		{name: "duplicate proxy challenge", status: 407, headers: []string{`Digest realm="3401000000",nonce="one"`, `Digest realm="3401000000",nonce="two"`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reason, headerName := "Unauthorized", "WWW-Authenticate"
			if test.status == 407 {
				reason, headerName = "Proxy Authentication Required", "Proxy-Authenticate"
			}
			challenge := sip.NewResponseFromRequest("", request, test.status, reason, nil)
			for _, value := range test.headers {
				challenge.AppendHeader(&sip.GenericHeader{HeaderName: headerName, Contents: value})
			}
			if _, err := buildAnnexGDigestRetry(request, challenge, system, system.realm); err == nil {
				t.Fatal("invalid Annex G Digest challenge was accepted")
			}
		})
	}
}

func TestBuildAnnexGTargetsAndTLSDefaults(t *testing.T) {
	for _, test := range []struct {
		transport string
		network   string
		encrypted bool
	}{
		{transport: "udp", network: "udp"},
		{transport: "tcp", network: "tcp"},
		{transport: "tls", network: "tls", encrypted: true},
	} {
		t.Run(test.transport, func(t *testing.T) {
			uri, target, host, err := buildAnnexGTarget(gb10DeviceID, "192.0.2.10:5061", test.transport)
			if err != nil {
				t.Fatal(err)
			}
			if target == nil || target.Network() != test.network || host != "192.0.2.10" || uri == nil || uri.FIsEncrypted != test.encrypted {
				t.Fatalf("target = uri:%#v addr:%#v host:%q", uri, target, host)
			}
			transport, exists := uri.FUriParams.Get("transport")
			if test.transport == "udp" {
				if exists {
					t.Fatalf("UDP URI unexpectedly contains transport=%v", transport)
				}
			} else if !exists || transport == nil || transport.String() != test.transport {
				t.Fatalf("URI transport = %v, %v", transport, exists)
			}
		})
	}

	tlsConfig, err := annexGTLSClientConfig(conf.SIPAnnexGSystem{Transport: "tls"}, "annex-g.example")
	if err != nil {
		t.Fatal(err)
	}
	if tlsConfig == nil || tlsConfig.ServerName != "annex-g.example" || tlsConfig.MinVersion == 0 {
		t.Fatalf("TLS config = %#v", tlsConfig)
	}
	if config, err := annexGTLSClientConfig(conf.SIPAnnexGSystem{Transport: "tcp"}, "annex-g.example"); err != nil || config != nil {
		t.Fatalf("TCP TLS config = %#v, %v", config, err)
	}
	if _, err := buildAnnexGSystem(conf.SIPAnnexGSystem{ID: "short", Version: "1.0"}); err == nil {
		t.Fatal("short Annex G system ID was accepted")
	}
}

func TestAnnexGTLSCRLRejectsRevokedServerCertificate(t *testing.T) {
	now := time.Now()
	caKey := generateRegisterCertificateRSAKey(t)
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(500), Subject: pkix.Name{CommonName: "Annex G TLS CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, IsCA: true, BasicConstraintsValid: true,
	}
	caCertificate := createRegisterCertificate(t, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	serverKey := generateRegisterCertificateRSAKey(t)
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(501), Subject: pkix.Name{CommonName: "annex-g.example"},
		DNSNames: []string{"annex-g.example"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverCertificate := createRegisterCertificate(t, serverTemplate, caCertificate, &serverKey.PublicKey, caKey)
	directory := t.TempDir()
	caPath := filepath.Join(directory, "annex-g-ca.pem")
	writeCertificatePEM(t, caPath, caCertificate)
	crlDER, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		SignatureAlgorithm: x509.SHA256WithRSA,
		RevokedCertificateEntries: []x509.RevocationListEntry{{
			SerialNumber: serverCertificate.SerialNumber, RevocationTime: now.Add(-time.Minute),
		}},
		Number: big.NewInt(1), ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Hour),
	}, caCertificate, caKey)
	if err != nil {
		t.Fatal(err)
	}
	crlPath := filepath.Join(directory, "annex-g.crl")
	writePEM(t, crlPath, "X509 CRL", crlDER)

	tlsConfig, err := annexGTLSClientConfig(conf.SIPAnnexGSystem{
		Transport: "tls", TLSCA: caPath, TLSCRL: crlPath, TLSServerName: "annex-g.example",
	}, "annex-g.example")
	if err != nil {
		t.Fatal(err)
	}
	if tlsConfig.VerifyConnection == nil {
		t.Fatal("Annex G TLS CRL verifier was not installed")
	}
	err = tlsConfig.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCertificate}})
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked Annex G TLS server certificate result = %v", err)
	}
	allowedTemplate := *serverTemplate
	allowedTemplate.SerialNumber = big.NewInt(502)
	allowedCertificate := createRegisterCertificate(t, &allowedTemplate, caCertificate, &serverKey.PublicKey, caKey)
	if err := tlsConfig.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{allowedCertificate}}); err != nil {
		t.Fatalf("non-revoked Annex G TLS server certificate result = %v", err)
	}
}

func TestAnnexGTLSCRLRejectsRevokedIntermediateCertificate(t *testing.T) {
	now := time.Now()
	rootKey := generateRegisterCertificateRSAKey(t)
	rootTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(510), Subject: pkix.Name{CommonName: "Annex G Root CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, IsCA: true, BasicConstraintsValid: true,
	}
	rootCertificate := createRegisterCertificate(t, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	intermediateKey := generateRegisterCertificateRSAKey(t)
	intermediateTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(511), Subject: pkix.Name{CommonName: "Annex G Intermediate CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, IsCA: true, BasicConstraintsValid: true,
	}
	intermediateCertificate := createRegisterCertificate(t, intermediateTemplate, rootCertificate, &intermediateKey.PublicKey, rootKey)
	serverKey := generateRegisterCertificateRSAKey(t)
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(512), Subject: pkix.Name{CommonName: "annex-g.example"},
		DNSNames: []string{"annex-g.example"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverCertificate := createRegisterCertificate(t, serverTemplate, intermediateCertificate, &serverKey.PublicKey, intermediateKey)

	directory := t.TempDir()
	caPath := filepath.Join(directory, "annex-g-root.pem")
	writeCertificatePEM(t, caPath, rootCertificate)
	caFile, err := os.OpenFile(caPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(caFile, &pem.Block{Type: "CERTIFICATE", Bytes: intermediateCertificate.Raw}); err != nil {
		_ = caFile.Close()
		t.Fatal(err)
	}
	if err := caFile.Close(); err != nil {
		t.Fatal(err)
	}
	intermediateCRL, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		SignatureAlgorithm: x509.SHA256WithRSA, Number: big.NewInt(2),
		ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Hour),
	}, intermediateCertificate, intermediateKey)
	if err != nil {
		t.Fatal(err)
	}
	rootCRL, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		SignatureAlgorithm: x509.SHA256WithRSA,
		RevokedCertificateEntries: []x509.RevocationListEntry{{
			SerialNumber: intermediateCertificate.SerialNumber, RevocationTime: now.Add(-time.Minute),
		}},
		Number: big.NewInt(3), ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Hour),
	}, rootCertificate, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	crlPath := filepath.Join(directory, "annex-g-chain.crl")
	writePEM(t, crlPath, "X509 CRL", intermediateCRL)
	crlFile, err := os.OpenFile(crlPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(crlFile, &pem.Block{Type: "X509 CRL", Bytes: rootCRL}); err != nil {
		_ = crlFile.Close()
		t.Fatal(err)
	}
	if err := crlFile.Close(); err != nil {
		t.Fatal(err)
	}

	clientConfig, err := annexGTLSClientConfig(conf.SIPAnnexGSystem{
		Transport: "tls", TLSCA: caPath, TLSCRL: crlPath, TLSServerName: "annex-g.example",
	}, "annex-g.example")
	if err != nil {
		t.Fatal(err)
	}
	clientSide, serverSide := net.Pipe()
	clientTLS := tls.Client(clientSide, clientConfig)
	serverTLS := tls.Server(serverSide, &tls.Config{
		MinVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{serverCertificate.Raw, intermediateCertificate.Raw},
			PrivateKey:  serverKey,
			Leaf:        serverCertificate,
		}},
	})
	serverResult := make(chan error, 1)
	go func() { serverResult <- serverTLS.HandshakeContext(t.Context()) }()
	handshakeErr := clientTLS.HandshakeContext(t.Context())
	_ = clientTLS.Close()
	_ = serverTLS.Close()
	<-serverResult
	if handshakeErr == nil || !strings.Contains(handshakeErr.Error(), "serial 511 is revoked") {
		t.Fatalf("revoked Annex G TLS intermediate certificate handshake result = %v", handshakeErr)
	}
}

func newAnnexGOutboundHarness(t *testing.T, role annexg.SystemRole) (*annexGService, *Server, *GB28181API, *gorm.DB, *flowConnection) {
	t.Helper()
	service, db := newAnnexGRuntime(t, role, true)
	system := service.systems[gb10DeviceID]
	targetURI, target, _, err := buildAnnexGTarget(system.id, "192.0.2.10:5060", "tcp")
	if err != nil {
		t.Fatal(err)
	}
	connection := newFlowConnection()
	system.transport = "tcp"
	system.targetURI = targetURI
	system.target = target
	system.conn = connection

	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.1:5060")
	sipServer := sip.NewServer(local)
	lifecycleCtx, cancel := context.WithCancel(context.Background())
	api := &GB28181API{
		annexG: service, lifecycleCtx: lifecycleCtx, lifecycleCancel: cancel,
		lifecycleDone: make(chan struct{}),
	}
	server := &Server{Server: sipServer, gb: api, fromAddress: *local}
	api.svr = server
	t.Cleanup(func() {
		service.close()
		cancel()
		sipServer.Close()
	})
	return service, server, api, db, connection
}

func annexGOutboundResponseSender(t *testing.T, api *GB28181API, service *annexGService, response annexg.Message) func(context.Context, *sip.Server, *annexGSystem, *sip.Request) error {
	t.Helper()
	return func(_ context.Context, _ *sip.Server, system *annexGSystem, request *sip.Request) error {
		if request == nil || request.Method() != sip.MethodMessage || request.GetConnection() == nil || request.Destination() == nil {
			t.Fatalf("outbound SIP request = %#v", request)
		}
		if headers := request.GetHeaders("X-GB-Ver"); len(headers) != 1 || !strings.Contains(headers[0].String(), string(system.version)) {
			t.Fatalf("outbound X-GB-Ver = %#v", headers)
		}
		body, err := annexg.Encode(system.version, response)
		if err != nil {
			t.Fatal(err)
		}
		responseConnection := newFlowConnection()
		ctx := newAnnexGContext(t, responseConnection, body)
		ctx.Set(annexGSystemContextKey, system)
		api.sipAnnexGMessage(ctx)
		select {
		case payload := <-responseConnection.writes:
			if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
				t.Fatalf("business response SIP acknowledgement = %s", payload)
			}
		default:
			t.Fatal("business response was not acknowledged")
		}
		return nil
	}
}

func TestAnnexGPlatformOutboundExchangesAndPersistence(t *testing.T) {
	active := true
	for _, test := range []struct {
		name     string
		role     annexg.SystemRole
		request  annexg.Message
		response annexg.Message
		check    func(*testing.T, *gorm.DB)
	}{
		{
			name: "MP alarm", role: annexg.RoleEmergencyCommandSystem,
			request: &annexg.MPAlarmNotify{CmdType: annexg.CommandMPAlarm, SN: 101, AlarmContent: annexg.MPAlarmRecord{
				AlarmNO: "mp-101", AlarmTime: "2026-08-27T10:20:30", DeviceID: gb10DeviceID,
				AlarmPriority: "1", AlarmMethod: "2", OriginalNO: "original-101", OriginalInfo: "original",
				Sender: "sender", Processor: "processor", AlarmLevel: "1", Disposal: "processed", AlarmInfo: "alarm",
			}},
			response: &annexg.NotificationResponse{CmdType: annexg.CommandMPAlarm, SN: 101, Result: annexg.ResultOK},
			check: func(t *testing.T, db *gorm.DB) {
				var count int64
				if err := db.Table("gb_annex_g_alarm_records").Where("kind = ?", "mp").Count(&count).Error; err != nil || count != 1 {
					t.Fatalf("stored MP alarms = %d, %v", count, err)
				}
			},
		},
		{
			name: "config defence", role: annexg.RoleTollgateSystem,
			request: &annexg.ConfigDefenceNotify{
				CmdType: annexg.CommandConfigDefence, SN: 102, Type: &active,
				TollgateID: "tollgate-102", CarPlate: "浙A12345", PlateType: "02", DefenceType: "wanted", DefenceTime: "2026-08-27T10:20:30",
			},
			response: &annexg.NotificationResponse{CmdType: annexg.CommandConfigDefence, SN: 102, Result: annexg.ResultOK},
			check: func(t *testing.T, db *gorm.DB) {
				var states, audits int64
				stateErr := db.Table("gb_annex_g_defence_states").Count(&states).Error
				auditErr := db.Table("gb_annex_g_defence_audits").Count(&audits).Error
				if stateErr != nil || auditErr != nil || states != 1 || audits != 1 {
					t.Fatalf("defence persistence = states:%d audits:%d errors:%v/%v", states, audits, stateErr, auditErr)
				}
			},
		},
		{
			name: "ECS query", role: annexg.RoleEmergencyCommandSystem,
			request: &annexg.AlarmRecordQuery{CmdType: annexg.CommandECSAlarmRecordList, SN: 103},
			response: &annexg.ECSAlarmRecordListResponse{
				CmdType: annexg.CommandECSAlarmRecordList, SN: 103, Result: annexg.ResultOK, RealRecordNum: 1, SendRecordNum: 1,
				RecordList: annexg.ECSAlarmRecordList{AlarmRecords: []annexg.ECSAlarmRecord{{
					AlarmNO: "ecs-103", AlarmTime: "2026-08-27T10:20:30", AlarmPriority: "1", AlarmClass: "1",
					AlarmAddress: "address", AlarmMethod: "2", AlarmTelephone: "110", Processor: "processor",
					SrecipientName: "operator", NsStatus: "open", NCallType: "alarm", AlarmInfo: "alarm",
				}}},
			},
			check: func(t *testing.T, db *gorm.DB) {
				var count int64
				if err := db.Table("gb_annex_g_alarm_records").Where("kind = ?", "ecs").Count(&count).Error; err != nil || count != 1 {
					t.Fatalf("stored ECS alarms = %d, %v", count, err)
				}
			},
		},
		{
			name: "TGS query", role: annexg.RoleTollgateSystem,
			request: &annexg.AlarmRecordQuery{CmdType: annexg.CommandTGSAlarmRecordList, SN: 104},
			response: &annexg.TGSAlarmRecordListResponse{
				CmdType: annexg.CommandTGSAlarmRecordList, SN: 104, Result: annexg.ResultOK, RealRecordNum: 1, SendRecordNum: 1,
				RecordList: annexg.TGSAlarmRecordList{AlarmRecords: []annexg.TGSAlarmRecord{{
					AlarmTime: "2026-08-27T10:20:30", TollgateID: "tollgate-104", CarPlate: "浙A54321", PlateType: "02", DefenceType: "wanted",
				}}},
			},
			check: func(t *testing.T, db *gorm.DB) {
				var count int64
				if err := db.Table("gb_annex_g_alarm_records").Where("kind = ?", "tgs").Count(&count).Error; err != nil || count != 1 {
					t.Fatalf("stored TGS alarms = %d, %v", count, err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, server, api, db, _ := newAnnexGOutboundHarness(t, test.role)
			service.outbound = annexGOutboundResponseSender(t, api, service, test.response)
			response, err := server.AnnexGExchange(context.Background(), gb10DeviceID, test.request)
			if err != nil {
				t.Fatal(err)
			}
			if response == nil || response.CommandType() != test.request.CommandType() {
				t.Fatalf("business response = %#v", response)
			}
			test.check(t, db)
		})
	}
}

func TestAnnexGRejectsUnassociatedBusinessResponse(t *testing.T) {
	service, _, api, _, _ := newAnnexGOutboundHarness(t, annexg.RoleEmergencyCommandSystem)
	response := &annexg.NotificationResponse{CmdType: annexg.CommandMPAlarm, SN: 999, Result: annexg.ResultOK}
	body, err := annexg.Encode(annexg.Version2011, response)
	if err != nil {
		t.Fatal(err)
	}
	connection := newFlowConnection()
	ctx := newAnnexGContext(t, connection, body)
	ctx.Set(annexGSystemContextKey, service.systems[gb10DeviceID])
	api.sipAnnexGMessage(ctx)
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 409") {
			t.Fatalf("unassociated response rejection = %s", payload)
		}
	default:
		t.Fatal("unassociated response was not rejected")
	}
	if failures := api.metrics.Snapshot().AnnexGBusinessFailures; failures != 1 {
		t.Fatalf("Annex G business failure metric = %d, want 1", failures)
	}
}

func TestAnnexGExchangeTimeoutRetainsPendingForLateResponse(t *testing.T) {
	service, server, _, db, _ := newAnnexGOutboundHarness(t, annexg.RoleEmergencyCommandSystem)
	service.outbound = func(context.Context, *sip.Server, *annexGSystem, *sip.Request) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	request := &annexg.MPAlarmNotify{CmdType: annexg.CommandMPAlarm, SN: 105, AlarmContent: annexg.MPAlarmRecord{
		AlarmNO: "mp-105", AlarmTime: "2026-08-27T10:20:30", DeviceID: gb10DeviceID,
		AlarmPriority: "1", AlarmMethod: "2", OriginalNO: "original-105", OriginalInfo: "original",
		Sender: "sender", Processor: "processor", AlarmLevel: "1", Disposal: "processed", AlarmInfo: "alarm",
	}}
	if _, err := server.AnnexGExchange(ctx, gb10DeviceID, request); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("timeout error = %v", err)
	}
	service.pendingMu.Lock()
	pendingCount := len(service.pending)
	service.pendingMu.Unlock()
	if pendingCount != 1 {
		t.Fatalf("pending exchanges after timeout = %d, want 1", pendingCount)
	}
	var stored int64
	if err := db.Table("gb_annex_g_pending_exchanges").Count(&stored).Error; err != nil || stored != 1 {
		t.Fatalf("stored pending exchanges after timeout = %d, %v", stored, err)
	}
}

func TestAnnexGOutboundFailureRetriesPersistedRequest(t *testing.T) {
	service, server, _, _, _ := newAnnexGOutboundHarness(t, annexg.RoleEmergencyCommandSystem)
	service.bindServer(server)
	errSend := errors.New("Annex G test outbound failed")
	var calls atomic.Int32
	var bodiesMu sync.Mutex
	bodies := make([]string, 0, 2)
	service.outbound = func(_ context.Context, _ *sip.Server, _ *annexGSystem, request *sip.Request) error {
		bodiesMu.Lock()
		bodies = append(bodies, string(request.Body()))
		bodiesMu.Unlock()
		if calls.Add(1) == 1 {
			return errSend
		}
		return nil
	}
	request := &annexg.AlarmRecordQuery{CmdType: annexg.CommandECSAlarmRecordList, SN: 210}
	if _, err := server.AnnexGExchange(context.Background(), gb10DeviceID, request); !errors.Is(err, errSend) {
		t.Fatalf("initial outbound error = %v, want %v", err, errSend)
	}
	key, err := annexGPendingKeyFor(gb10DeviceID, request)
	if err != nil {
		t.Fatal(err)
	}
	service.pendingMu.Lock()
	pending := service.pending[key]
	if pending == nil {
		service.pendingMu.Unlock()
		t.Fatal("failed outbound request was not retained")
	}
	nextSend := pending.nextSend
	needsSend, sending, attempts := pending.needsSend, pending.sending, pending.attempts
	service.pendingMu.Unlock()
	if !needsSend || sending || attempts != 1 || nextSend.IsZero() {
		t.Fatalf("failed outbound retry state = needs:%v sending:%v attempts:%d next:%v", needsSend, sending, attempts, nextSend)
	}

	service.retryPendingRequests(context.Background(), nextSend.Add(-time.Nanosecond))
	if got := calls.Load(); got != 1 {
		t.Fatalf("outbound attempts before backoff = %d, want 1", got)
	}
	service.retryPendingRequests(context.Background(), nextSend)
	if got := calls.Load(); got != 2 {
		t.Fatalf("outbound attempts after backoff = %d, want 2", got)
	}
	bodiesMu.Lock()
	defer bodiesMu.Unlock()
	if len(bodies) != 2 || bodies[0] != bodies[1] || !strings.Contains(bodies[1], "<SN>210</SN>") {
		t.Fatalf("retried Annex G payloads = %#v", bodies)
	}
	service.pendingMu.Lock()
	needsSend, sending = pending.needsSend, pending.sending
	service.pendingMu.Unlock()
	if needsSend || sending {
		t.Fatalf("successful retry state = needs:%v sending:%v", needsSend, sending)
	}
}

func TestAnnexGPendingRequestRetryUsesBoundedDeterministicBatches(t *testing.T) {
	service, server, _, _, _ := newAnnexGOutboundHarness(t, annexg.RoleEmergencyCommandSystem)
	now := time.Now()
	for index := 0; index < annexGOutboundRetryBatch+2; index++ {
		request := &annexg.AlarmRecordQuery{CmdType: annexg.CommandECSAlarmRecordList, SN: 300 + index}
		key, err := annexGPendingKeyFor(gb10DeviceID, request)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := service.registerPending(key, request)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.store.SavePendingExchange(context.Background(), gb10DeviceID, annexg.Version2011, request); err != nil {
			t.Fatal(err)
		}
		service.pendingMu.Lock()
		pending.sending = false
		pending.nextSend = now.Add(-time.Second)
		service.pendingMu.Unlock()
	}
	service.pendingMu.Lock()
	service.server = server
	service.pendingMu.Unlock()
	var sentMu sync.Mutex
	sent := make([]int, 0, annexGOutboundRetryBatch+2)
	service.outbound = func(_ context.Context, _ *sip.Server, system *annexGSystem, request *sip.Request) error {
		message, err := annexg.Decode(system.version, request.Body())
		if err != nil {
			return err
		}
		sn, ok := annexg.MessageSequence(message)
		if !ok {
			return errors.New("retried Annex G request has no SN")
		}
		sentMu.Lock()
		sent = append(sent, sn)
		sentMu.Unlock()
		return nil
	}

	service.retryPendingRequests(context.Background(), now)
	sentMu.Lock()
	firstBatch := append([]int(nil), sent...)
	sentMu.Unlock()
	if len(firstBatch) != annexGOutboundRetryBatch {
		t.Fatalf("first Annex G retry batch = %v, want %d items", firstBatch, annexGOutboundRetryBatch)
	}
	for index, sn := range firstBatch {
		if want := 300 + index; sn != want {
			t.Fatalf("first Annex G retry batch = %v, want deterministic SN order starting at 300", firstBatch)
		}
	}

	service.retryPendingRequests(context.Background(), now)
	sentMu.Lock()
	all := append([]int(nil), sent...)
	sentMu.Unlock()
	if len(all) != annexGOutboundRetryBatch+2 || all[len(all)-2] != 300+annexGOutboundRetryBatch || all[len(all)-1] != 301+annexGOutboundRetryBatch {
		t.Fatalf("all Annex G retry batches = %v", all)
	}
}

func TestAnnexGRestoredPendingRequestRetriesAfterServerBind(t *testing.T) {
	service, db := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	request := &annexg.AlarmRecordQuery{CmdType: annexg.CommandECSAlarmRecordList, SN: 211}
	key, err := annexGPendingKeyFor(gb10DeviceID, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.registerPending(key, request); err != nil {
		t.Fatal(err)
	}
	if err := service.store.SavePendingExchange(context.Background(), gb10DeviceID, annexg.Version2011, request, service.systems[gb10DeviceID].profileFingerprint); err != nil {
		t.Fatal(err)
	}
	service.close()

	restored, err := newAnnexGService(annexGTestSIPConfig(annexg.RoleEmergencyCommandSystem, true), db)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	system := restored.systems[gb10DeviceID]
	targetURI, target, _, err := buildAnnexGTarget(system.id, "192.0.2.10:5060", "tcp")
	if err != nil {
		t.Fatal(err)
	}
	system.transport = "tcp"
	system.targetURI = targetURI
	system.target = target
	system.conn = newFlowConnection()
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.1:5060")
	sipServer := sip.NewServer(local)
	defer sipServer.Close()
	server := &Server{Server: sipServer, fromAddress: *local}
	sent := make(chan string, 1)
	restored.outbound = func(_ context.Context, _ *sip.Server, _ *annexGSystem, request *sip.Request) error {
		sent <- string(request.Body())
		return nil
	}

	restored.retryPendingRequests(context.Background(), time.Now().Add(time.Hour))
	select {
	case payload := <-sent:
		t.Fatalf("restored request sent before server bind: %s", payload)
	default:
	}
	restored.bindServer(server)
	select {
	case payload := <-sent:
		if !strings.Contains(payload, "<SN>211</SN>") {
			t.Fatalf("restored retry payload = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("restored pending request was not retried after server bind")
	}
}

func TestAnnexGRestoredPendingRequestIsDiscardedWhenSystemProfileChanges(t *testing.T) {
	service, db := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	request := &annexg.AlarmRecordQuery{CmdType: annexg.CommandECSAlarmRecordList, SN: 213}
	key, err := annexGPendingKeyFor(gb10DeviceID, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.registerPending(key, request); err != nil {
		t.Fatal(err)
	}
	if err := service.store.SavePendingExchange(context.Background(), gb10DeviceID, annexg.Version2011, request, service.systems[gb10DeviceID].profileFingerprint); err != nil {
		t.Fatal(err)
	}
	service.close()

	changed := annexGTestSIPConfig(annexg.RoleEmergencyCommandSystem, true)
	changed.AnnexG.Systems[0].Address = "192.0.2.99:5060"
	restored, err := newAnnexGService(changed, db)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	restored.pendingMu.Lock()
	count := len(restored.pending)
	restored.pendingMu.Unlock()
	if count != 0 {
		t.Fatalf("stale pending exchange restored after profile change: %d", count)
	}
	var stored int64
	if err := db.Table("gb_annex_g_pending_exchanges").Count(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("stale pending exchange remained persisted after profile change: %d", stored)
	}
}

func TestAnnexGRestoredPendingRequestIsDiscardedForProfileMutations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*conf.SIP)
	}{
		{name: "role", mutate: func(cfg *conf.SIP) {
			cfg.AnnexG.Systems[0].Role = string(annexg.RoleTollgateSystem)
		}},
		{name: "version", mutate: func(cfg *conf.SIP) {
			cfg.AnnexG.Systems[0].Version = "1.1"
		}},
		{name: "source cidr", mutate: func(cfg *conf.SIP) {
			cfg.AnnexG.Systems[0].SourceCIDRs = []string{"192.0.2.99/32"}
		}},
		{name: "password", mutate: func(cfg *conf.SIP) {
			cfg.AnnexG.Systems[0].Password = "changed-secret"
		}},
		{name: "global signal digest seed", mutate: func(cfg *conf.SIP) {
			cfg.SignalDigest.Seed = "changed-global-seed"
		}},
		{name: "system deleted", mutate: func(cfg *conf.SIP) {
			cfg.AnnexG.Systems[0].ID = "34020000002000000003"
		}},
	}
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			service, db := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
			request := &annexg.AlarmRecordQuery{CmdType: annexg.CommandECSAlarmRecordList, SN: 400 + index}
			key, err := annexGPendingKeyFor(gb10DeviceID, request)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.registerPending(key, request); err != nil {
				t.Fatal(err)
			}
			if err := service.store.SavePendingExchange(context.Background(), gb10DeviceID, annexg.Version2011, request, service.systems[gb10DeviceID].profileFingerprint); err != nil {
				t.Fatal(err)
			}
			service.close()

			changed := annexGTestSIPConfig(annexg.RoleEmergencyCommandSystem, true)
			test.mutate(&changed)
			restored, err := newAnnexGService(changed, db)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.close()
			if restored.pendingCount() != 0 {
				t.Fatalf("stale pending exchange restored after %s mutation", test.name)
			}
			var stored int64
			if err := db.Table("gb_annex_g_pending_exchanges").Count(&stored).Error; err != nil {
				t.Fatal(err)
			}
			if stored != 0 {
				t.Fatalf("stale pending exchange remained persisted after %s mutation: %d", test.name, stored)
			}
		})
	}
}

func TestAnnexGResponseRaceStopsRequestRetry(t *testing.T) {
	service, server, _, _, _ := newAnnexGOutboundHarness(t, annexg.RoleEmergencyCommandSystem)
	request := &annexg.AlarmRecordQuery{CmdType: annexg.CommandECSAlarmRecordList, SN: 212}
	key, err := annexGPendingKeyFor(gb10DeviceID, request)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := service.registerPending(key, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.store.SavePendingExchange(context.Background(), gb10DeviceID, annexg.Version2011, request); err != nil {
		t.Fatal(err)
	}
	service.pendingMu.Lock()
	service.server = server
	pending.sending = false
	pending.nextSend = time.Time{}
	service.pendingMu.Unlock()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	service.outbound = func(context.Context, *sip.Server, *annexGSystem, *sip.Request) error {
		calls.Add(1)
		close(started)
		<-release
		return errors.New("Annex G test raced send failed")
	}
	done := make(chan struct{})
	go func() {
		service.retryPendingRequests(context.Background(), time.Now())
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Annex G retry did not start")
	}
	response := &annexg.ECSAlarmRecordListResponse{
		CmdType: annexg.CommandECSAlarmRecordList, SN: 212, Result: annexg.ResultOK,
	}
	claimed, err := service.claimPendingResponse(gb10DeviceID, response)
	if err != nil || claimed != pending {
		t.Fatalf("claim raced response = %#v, %v", claimed, err)
	}
	if !service.setPendingResponse(key, pending, response) {
		t.Fatal("store raced response in pending state failed")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Annex G raced retry did not finish")
	}
	service.retryPendingRequests(context.Background(), time.Now().Add(time.Hour))
	if got := calls.Load(); got != 1 {
		t.Fatalf("outbound attempts after response race = %d, want 1", got)
	}
}

func TestAnnexGCloseDisablesPendingRequestRetry(t *testing.T) {
	service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	request := &annexg.AlarmRecordQuery{CmdType: annexg.CommandECSAlarmRecordList, SN: 213}
	key, err := annexGPendingKeyFor(gb10DeviceID, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.registerPending(key, request); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	service.outbound = func(context.Context, *sip.Server, *annexGSystem, *sip.Request) error {
		calls.Add(1)
		return nil
	}
	service.close()
	service.retryPendingRequests(context.Background(), time.Now().Add(time.Hour))
	if got := calls.Load(); got != 0 {
		t.Fatalf("outbound attempts after close = %d, want 0", got)
	}
}

func TestAnnexGUnsentExchangeKeepsPendingWhenPersistentDiscardFails(t *testing.T) {
	service, server, _, db, _ := newAnnexGOutboundHarness(t, annexg.RoleEmergencyCommandSystem)
	system := service.systems[gb10DeviceID]
	system.conn = nil
	errDial := errors.New("Annex G test dial failed")
	var failDelete atomic.Bool
	system.dialTCP = func(context.Context, string) (net.Conn, error) {
		failDelete.Store(true)
		return nil, errDial
	}
	errDelete := errors.New("Annex G test pending delete failed")
	callbackName := "test:fail_annex_g_pending_delete"
	if err := db.Callback().Delete().Before("gorm:delete").Register(callbackName, func(tx *gorm.DB) {
		if failDelete.Load() && tx.Statement != nil && tx.Statement.Table == "gb_annex_g_pending_exchanges" {
			tx.AddError(errDelete)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Delete().Remove(callbackName) })

	request := &annexg.AlarmRecordQuery{CmdType: annexg.CommandECSAlarmRecordList, SN: 208}
	_, err := server.AnnexGExchange(context.Background(), gb10DeviceID, request)
	if !errors.Is(err, errDial) || !errors.Is(err, errDelete) {
		t.Fatalf("unsent exchange error = %v, want dial and persistent discard errors", err)
	}
	service.pendingMu.Lock()
	pendingCount := len(service.pending)
	service.pendingMu.Unlock()
	if pendingCount != 1 {
		t.Fatalf("pending exchanges after failed persistent discard = %d, want 1", pendingCount)
	}
	var stored int64
	if err := db.Table("gb_annex_g_pending_exchanges").Count(&stored).Error; err != nil || stored != 1 {
		t.Fatalf("stored pending exchanges after failed discard = %d, %v", stored, err)
	}
}

func TestAnnexGCleanupExpiresMemoryAndStoredPendingExchange(t *testing.T) {
	service, db := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	defer service.close()
	request := &annexg.AlarmRecordQuery{CmdType: annexg.CommandECSAlarmRecordList, SN: 204}
	key, err := annexGPendingKeyFor(gb10DeviceID, request)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := service.registerPending(key, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.store.SavePendingExchange(context.Background(), gb10DeviceID, annexg.Version2011, request); err != nil {
		t.Fatal(err)
	}
	if pendingCount := (&Server{gb: &GB28181API{annexG: service}}).Metrics().AnnexGPending; pendingCount != 1 {
		t.Fatalf("pending metric before cleanup = %d, want 1", pendingCount)
	}
	now := time.Now()
	service.pendingMu.Lock()
	pending.expiresAt = now.Add(-time.Second)
	service.pendingMu.Unlock()
	if err := db.Table("gb_annex_g_pending_exchanges").Where("sn = ?", request.SN).
		Update("expires_at", now.Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}

	service.cleanupExpiredPending(context.Background(), now)
	service.pendingMu.Lock()
	pendingCount := len(service.pending)
	service.pendingMu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("pending exchanges after cleanup = %d, want 0", pendingCount)
	}
	select {
	case result := <-pending.result:
		if !errors.Is(result.err, context.DeadlineExceeded) {
			t.Fatalf("expired pending result = %v", result.err)
		}
	default:
		t.Fatal("expired pending waiter was not released")
	}
	var stored int64
	if err := db.Table("gb_annex_g_pending_exchanges").Count(&stored).Error; err != nil || stored != 0 {
		t.Fatalf("stored pending exchanges after cleanup = %d, %v", stored, err)
	}
}

func TestAnnexGPendingExchangeRestoresAfterRestart(t *testing.T) {
	service, server, _, db, _ := newAnnexGOutboundHarness(t, annexg.RoleTollgateSystem)
	service.outbound = func(context.Context, *sip.Server, *annexGSystem, *sip.Request) error { return nil }
	active := true
	request := &annexg.ConfigDefenceNotify{
		CmdType: annexg.CommandConfigDefence, SN: 205, Type: &active,
		TollgateID: "gate-205", CarPlate: "浙A00205", PlateType: "02", DefenceType: "wanted",
		DefenceTime: "2026-08-27T10:20:30",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := server.AnnexGExchange(ctx, gb10DeviceID, request); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("timeout error = %v", err)
	}
	service.close()

	cfg := annexGTestSIPConfig(annexg.RoleTollgateSystem, true)
	restored, err := newAnnexGService(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	restored.pendingMu.Lock()
	restoredCount := len(restored.pending)
	restored.pendingMu.Unlock()
	if restoredCount != 1 {
		t.Fatalf("restored pending exchanges = %d, want 1", restoredCount)
	}

	response := &annexg.NotificationResponse{CmdType: annexg.CommandConfigDefence, SN: 205, Result: annexg.ResultOK}
	body, err := annexg.Encode(annexg.Version2011, response)
	if err != nil {
		t.Fatal(err)
	}
	connection := newFlowConnection()
	requestContext := newAnnexGContext(t, connection, body)
	requestContext.Set(annexGSystemContextKey, restored.systems[gb10DeviceID])
	api := &GB28181API{annexG: restored, lifecycleCtx: context.Background()}
	api.sipAnnexGMessage(requestContext)
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
			t.Fatalf("restored response acknowledgement = %s", payload)
		}
	default:
		t.Fatal("restored response was not acknowledged")
	}
	var pendingRows, defenceRows int64
	if err := db.Table("gb_annex_g_pending_exchanges").Count(&pendingRows).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("gb_annex_g_defence_states").Where("tollgate_id = ?", "gate-205").Count(&defenceRows).Error; err != nil {
		t.Fatal(err)
	}
	if pendingRows != 0 || defenceRows != 1 {
		t.Fatalf("restart completion rows = pending:%d defence:%d", pendingRows, defenceRows)
	}
}

func TestAnnexGStoredResponseReplaysSideEffectsAfterRestart(t *testing.T) {
	service, server, _, db, _ := newAnnexGOutboundHarness(t, annexg.RoleTollgateSystem)
	service.outbound = func(context.Context, *sip.Server, *annexGSystem, *sip.Request) error { return nil }
	active := true
	request := &annexg.ConfigDefenceNotify{
		CmdType: annexg.CommandConfigDefence, SN: 206, Type: &active,
		TollgateID: "gate-206", CarPlate: "浙A00206", PlateType: "02", DefenceType: "wanted",
		DefenceTime: "2026-08-27T10:20:30",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := server.AnnexGExchange(ctx, gb10DeviceID, request); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("timeout error = %v", err)
	}
	response := &annexg.NotificationResponse{CmdType: annexg.CommandConfigDefence, SN: 206, Result: annexg.ResultOK}
	if err := service.store.SavePendingResponse(context.Background(), gb10DeviceID, annexg.Version2011, response); err != nil {
		t.Fatal(err)
	}
	var storedResponse string
	if err := db.Table("gb_annex_g_pending_exchanges").Select("response").Where("sn = ?", 206).Scan(&storedResponse).Error; err != nil || storedResponse == "" {
		t.Fatalf("stored response before restart = %q, %v", storedResponse, err)
	}
	service.close()

	restored, err := newAnnexGService(annexGTestSIPConfig(annexg.RoleTollgateSystem, true), db)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	var pendingRows, defenceRows int64
	if err := db.Table("gb_annex_g_pending_exchanges").Count(&pendingRows).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("gb_annex_g_defence_states").Where("tollgate_id = ?", "gate-206").Count(&defenceRows).Error; err != nil {
		t.Fatal(err)
	}
	if pendingRows != 0 || defenceRows != 1 {
		t.Fatalf("replayed response rows = pending:%d defence:%d", pendingRows, defenceRows)
	}
}

func TestAnnexGStoredResponseReplayUsesBoundedDeterministicBatches(t *testing.T) {
	service, _ := newAnnexGRuntime(t, annexg.RoleEmergencyCommandSystem, true)
	defer service.close()
	const total = annexGStoredResponseBatch + 2
	for sn := total; sn >= 1; sn-- {
		request := &annexg.MPAlarmNotify{CmdType: annexg.CommandMPAlarm, SN: sn, AlarmContent: annexg.MPAlarmRecord{
			AlarmNO: fmt.Sprintf("mp-batch-%d", sn), AlarmTime: "2026-08-27T10:20:30", DeviceID: gb10DeviceID,
			AlarmPriority: "1", AlarmMethod: "2", OriginalNO: "original", OriginalInfo: "original",
			Sender: "sender", Processor: "processor", AlarmLevel: "1", Disposal: "processed", AlarmInfo: "alarm",
		}}
		key, err := annexGPendingKeyFor(gb10DeviceID, request)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := service.registerPending(key, request)
		if err != nil {
			t.Fatal(err)
		}
		response := &annexg.NotificationResponse{CmdType: annexg.CommandMPAlarm, SN: sn, Result: annexg.ResultOK}
		pending.response = response
		pending.delivered = false
		if err := service.store.SavePendingExchange(t.Context(), gb10DeviceID, annexg.Version2011, request); err != nil {
			t.Fatal(err)
		}
		if err := service.store.SavePendingResponse(t.Context(), gb10DeviceID, annexg.Version2011, response); err != nil {
			t.Fatal(err)
		}
	}

	service.retryStoredResponses(t.Context())
	service.pendingMu.Lock()
	remaining := make([]int, 0, total)
	for key := range service.pending {
		remaining = append(remaining, key.sn)
	}
	service.pendingMu.Unlock()
	sort.Ints(remaining)
	if !slices.Equal(remaining, []int{annexGStoredResponseBatch + 1, annexGStoredResponseBatch + 2}) {
		t.Fatalf("remaining response replay SNs = %v", remaining)
	}

	service.retryStoredResponses(t.Context())
	if service.pendingCount() != 0 {
		t.Fatalf("pending responses after second batch = %d, want 0", service.pendingCount())
	}
}

func TestAnnexGResponseIsNotAcknowledgedBeforePersistence(t *testing.T) {
	service, _, api, db, _ := newAnnexGOutboundHarness(t, annexg.RoleEmergencyCommandSystem)
	request := &annexg.MPAlarmNotify{CmdType: annexg.CommandMPAlarm, SN: 207, AlarmContent: annexg.MPAlarmRecord{
		AlarmNO: "mp-207", AlarmTime: "2026-08-27T10:20:30", DeviceID: gb10DeviceID,
		AlarmPriority: "1", AlarmMethod: "2", OriginalNO: "original-207", OriginalInfo: "original",
		Sender: "sender", Processor: "processor", AlarmLevel: "1", Disposal: "processed", AlarmInfo: "alarm",
	}}
	key, err := annexGPendingKeyFor(gb10DeviceID, request)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := service.registerPending(key, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.store.SavePendingExchange(context.Background(), gb10DeviceID, annexg.Version2011, request); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropTable("gb_annex_g_pending_exchanges"); err != nil {
		t.Fatal(err)
	}
	response := &annexg.NotificationResponse{CmdType: annexg.CommandMPAlarm, SN: 207, Result: annexg.ResultOK}
	body, err := annexg.Encode(annexg.Version2011, response)
	if err != nil {
		t.Fatal(err)
	}
	connection := newFlowConnection()
	requestContext := newAnnexGContext(t, connection, body)
	requestContext.Set(annexGSystemContextKey, service.systems[gb10DeviceID])
	api.sipAnnexGMessage(requestContext)
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 503") {
			t.Fatalf("persistence failure response = %s", payload)
		}
	default:
		t.Fatal("persistence failure was not returned")
	}
	service.pendingMu.Lock()
	delivered := pending.delivered
	service.pendingMu.Unlock()
	if delivered {
		t.Fatal("persistence failure left the pending response claimed")
	}
}

func TestAnnexGResponseConsumesOnlyAfterSuccessfulSIPOK(t *testing.T) {
	service, _, api, db, _ := newAnnexGOutboundHarness(t, annexg.RoleTollgateSystem)
	active := true
	request := &annexg.ConfigDefenceNotify{
		CmdType: annexg.CommandConfigDefence, SN: 209, Type: &active,
		TollgateID: "gate-209", CarPlate: "浙A00209", PlateType: "02", DefenceType: "wanted",
		DefenceTime: "2026-08-27T10:20:30",
	}
	key, err := annexGPendingKeyFor(gb10DeviceID, request)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := service.registerPending(key, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.store.SavePendingExchange(context.Background(), gb10DeviceID, annexg.Version2011, request); err != nil {
		t.Fatal(err)
	}
	response := &annexg.NotificationResponse{CmdType: annexg.CommandConfigDefence, SN: 209, Result: annexg.ResultOK}
	body, err := annexg.Encode(annexg.Version2011, response)
	if err != nil {
		t.Fatal(err)
	}
	base := newFlowConnection()
	connection := &blockingFlowResponseConnection{
		flowConnection: base,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
		writeErr:       errors.New("Annex G response SIP OK write failed"),
	}
	ctx := newAnnexGContext(t, base, body)
	ctx.Request.SetConnection(connection)
	ctx.Tx = sip.NewTransaction("annex-g-response-write-failure", connection)
	ctx.Set(annexGSystemContextKey, service.systems[gb10DeviceID])
	done := make(chan struct{})
	go func() {
		api.sipAnnexGMessage(ctx)
		close(done)
	}()

	select {
	case <-connection.started:
	case <-time.After(time.Second):
		close(connection.release)
		t.Fatal("Annex G response SIP OK write did not start")
	}
	var defenceRows int64
	if err := db.Table("gb_annex_g_defence_states").Where("tollgate_id = ?", request.TollgateID).Count(&defenceRows).Error; err != nil {
		close(connection.release)
		t.Fatal(err)
	}
	if defenceRows != 0 {
		close(connection.release)
		t.Fatalf("defence state was stored before SIP OK completed: %d", defenceRows)
	}

	close(connection.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Annex G response handler did not return after SIP OK write failure")
	}
	if err := db.Table("gb_annex_g_defence_states").Where("tollgate_id = ?", request.TollgateID).Count(&defenceRows).Error; err != nil {
		t.Fatal(err)
	}
	if defenceRows != 0 {
		t.Fatalf("defence state was stored after SIP OK write failure: %d", defenceRows)
	}
	service.pendingMu.Lock()
	delivered := pending.delivered
	retained := service.pending[key] == pending && pending.response != nil
	service.pendingMu.Unlock()
	if delivered || !retained {
		t.Fatalf("pending response after SIP OK failure = delivered:%v retained:%v", delivered, retained)
	}
	var pendingRows int64
	if err := db.Table("gb_annex_g_pending_exchanges").Where("sn = ?", request.SN).Count(&pendingRows).Error; err != nil {
		t.Fatal(err)
	}
	if pendingRows != 1 {
		t.Fatalf("persistent pending response rows after SIP OK failure = %d, want 1", pendingRows)
	}

	retryConnection := newFlowConnection()
	retryCtx := newAnnexGContext(t, retryConnection, body)
	retryCtx.Set(annexGSystemContextKey, service.systems[gb10DeviceID])
	api.sipAnnexGMessage(retryCtx)
	select {
	case payload := <-retryConnection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
			t.Fatalf("retried response acknowledgement = %s", payload)
		}
	default:
		t.Fatal("retried Annex G response was not acknowledged")
	}
	if err := db.Table("gb_annex_g_defence_states").Where("tollgate_id = ?", request.TollgateID).Count(&defenceRows).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("gb_annex_g_pending_exchanges").Where("sn = ?", request.SN).Count(&pendingRows).Error; err != nil {
		t.Fatal(err)
	}
	if defenceRows != 1 || pendingRows != 0 {
		t.Fatalf("retried response rows = defence:%d pending:%d", defenceRows, pendingRows)
	}
}

func TestAnnexGTCPConnectionReuseAndClose(t *testing.T) {
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.1:5060")
	sipServer := sip.NewServer(local)
	server := &Server{Server: sipServer, fromAddress: *local}
	defer sipServer.Close()
	var dials atomic.Int32
	var peer net.Conn
	system := &annexGSystem{
		transport: "tcp", target: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
		dialTCP: func(context.Context, string) (net.Conn, error) {
			dials.Add(1)
			localConn, remoteConn := net.Pipe()
			peer = remoteConn
			return localConn, nil
		},
	}
	first, err := system.ensureConnection(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	second, err := system.ensureConnection(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || dials.Load() != 1 {
		t.Fatalf("connection reuse = same:%v dials:%d", first == second, dials.Load())
	}
	system.closeConnection()
	if peer != nil {
		_ = peer.Close()
	}
	if _, err := system.ensureConnection(context.Background(), server); err == nil {
		t.Fatal("closed Annex G system connection was reopened")
	}
}

func TestAnnexGTCPConnectionInvalidationAndPeerCloseRedial(t *testing.T) {
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.1:5060")
	sipServer := sip.NewServer(local)
	server := &Server{Server: sipServer, fromAddress: *local}
	defer sipServer.Close()

	var dials atomic.Int32
	var peersMu sync.Mutex
	var peers []net.Conn
	system := &annexGSystem{
		transport: "tcp", target: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
		dialTCP: func(context.Context, string) (net.Conn, error) {
			dials.Add(1)
			localConn, remoteConn := net.Pipe()
			peersMu.Lock()
			peers = append(peers, remoteConn)
			peersMu.Unlock()
			return localConn, nil
		},
	}
	t.Cleanup(func() {
		system.closeConnection()
		peersMu.Lock()
		defer peersMu.Unlock()
		for _, peer := range peers {
			_ = peer.Close()
		}
	})

	first, err := system.ensureConnection(t.Context(), server)
	if err != nil {
		t.Fatal(err)
	}
	system.invalidateConnection(first)
	second, err := system.ensureConnection(t.Context(), server)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || dials.Load() != 2 {
		t.Fatalf("invalidation redial = same:%v dials:%d", first == second, dials.Load())
	}

	peersMu.Lock()
	secondPeer := peers[1]
	peersMu.Unlock()
	if err := secondPeer.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		system.connMu.Lock()
		cleared := system.conn == nil
		system.connMu.Unlock()
		if cleared {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("peer close did not invalidate cached Annex G connection")
		}
		time.Sleep(time.Millisecond)
	}
	third, err := system.ensureConnection(t.Context(), server)
	if err != nil {
		t.Fatal(err)
	}
	if third == second || dials.Load() != 3 {
		t.Fatalf("peer-close redial = same:%v dials:%d", third == second, dials.Load())
	}
}

func TestAnnexGBusinessResponseTimeoutInvalidatesPersistentConnection(t *testing.T) {
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.1:5060")
	sipServer := sip.NewServer(local)
	server := &Server{Server: sipServer, fromAddress: *local}
	defer sipServer.Close()

	targetURI, target, _, err := buildAnnexGTarget(gb10DeviceID, "192.0.2.10:5060", "tcp")
	if err != nil {
		t.Fatal(err)
	}
	requestRead := make(chan error, 1)
	peerClosed := make(chan error, 1)
	system := &annexGSystem{
		id: gb10DeviceID, version: annexg.Version2011, transport: "tcp",
		targetURI: targetURI, target: target,
		dialTCP: func(context.Context, string) (net.Conn, error) {
			localConn, remoteConn := net.Pipe()
			go func() {
				defer remoteConn.Close()
				if _, readErr := readAnnexGTestSIPFrame(bufio.NewReader(remoteConn)); readErr != nil {
					requestRead <- readErr
					peerClosed <- readErr
					return
				}
				requestRead <- nil
				buffer := make([]byte, 1)
				_, readErr := remoteConn.Read(buffer)
				if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, net.ErrClosed) {
					peerClosed <- readErr
					return
				}
				peerClosed <- nil
			}()
			return &cascadeTestTCPConn{
				Conn:   localConn,
				local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 41501},
				remote: target,
			}, nil
		},
	}
	defer system.closeConnection()
	connection, err := system.ensureConnection(t.Context(), server)
	if err != nil {
		t.Fatal(err)
	}

	inbound := newFlowRequest(t, newFlowConnection(), sip.MethodMessage, "annex-g-business-response-timeout", testECSAlarmBody(t))
	inbound.SetConnection(connection)
	inbound.SetSource(target)
	requestCtx := &sip.Context{
		Request: inbound,
		Source:  target,
		To:      mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060"),
		From:    local,
	}
	lifecycleCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	api := &GB28181API{svr: server, lifecycleCtx: lifecycleCtx}
	response := &annexg.NotificationResponse{
		CmdType: annexg.CommandECSAlarm,
		SN:      301,
		Result:  annexg.ResultOK,
	}
	err = api.sendAnnexGResponse(requestCtx, system, annexg.Version2011, response)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Annex G business response error = %v, want context deadline exceeded", err)
	}
	if err := <-requestRead; err != nil {
		t.Fatal(err)
	}
	system.connMu.Lock()
	cached := system.conn
	system.connMu.Unlock()
	if cached != nil {
		t.Fatal("timed-out Annex G business response connection remained cached")
	}
	select {
	case err := <-peerClosed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed-out Annex G business response connection was not closed")
	}
}

func TestAnnexGResponseTimeoutInvalidatesPersistentConnectionAndRedials(t *testing.T) {
	for _, transport := range []string{"tcp", "tls"} {
		t.Run(transport, func(t *testing.T) {
			local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.1:5060")
			sipServer := sip.NewServer(local)
			server := &Server{Server: sipServer, fromAddress: *local}
			defer sipServer.Close()

			port := 5060
			if transport == "tls" {
				port = 5061
			}
			targetURI, target, _, err := buildAnnexGTarget(gb10DeviceID, net.JoinHostPort("192.0.2.10", strconv.Itoa(port)), transport)
			if err != nil {
				t.Fatal(err)
			}
			firstClosed := make(chan error, 1)
			secondDone := make(chan error, 1)
			allowSecondClose := make(chan struct{})
			var dials atomic.Int32
			openConnection := func() (net.Conn, error) {
				dial := dials.Add(1)
				localConn, remoteConn := net.Pipe()
				switch dial {
				case 1:
					go func() {
						defer remoteConn.Close()
						request, readErr := readAnnexGTestSIPFrame(bufio.NewReader(remoteConn))
						if readErr != nil {
							firstClosed <- readErr
							return
						}
						if !strings.HasPrefix(request, "MESSAGE ") {
							firstClosed <- fmt.Errorf("unexpected first Annex G request: %s", request)
							return
						}
						buffer := make([]byte, 1)
						_, readErr = remoteConn.Read(buffer)
						if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, net.ErrClosed) {
							firstClosed <- fmt.Errorf("timed-out Annex G connection close: %w", readErr)
							return
						}
						firstClosed <- nil
					}()
				case 2:
					go func() {
						defer remoteConn.Close()
						request, readErr := readAnnexGTestSIPFrame(bufio.NewReader(remoteConn))
						if readErr != nil {
							secondDone <- readErr
							return
						}
						_, writeErr := io.WriteString(remoteConn, annexGTestSIPResponse(request, 200, "OK", ""))
						secondDone <- writeErr
						<-allowSecondClose
					}()
				default:
					_ = remoteConn.Close()
					_ = localConn.Close()
					return nil, fmt.Errorf("unexpected Annex G redial %d", dial)
				}
				return &cascadeTestTCPConn{
					Conn:   localConn,
					local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 41300 + int(dial)},
					remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: port},
				}, nil
			}
			system := &annexGSystem{
				id: gb10DeviceID, version: annexg.Version2011, transport: transport,
				targetURI: targetURI, target: target, tlsConfig: &tls.Config{MinVersion: tls.VersionTLS12},
				dialTCP: func(context.Context, string) (net.Conn, error) { return openConnection() },
				dialTLS: func(context.Context, string, *tls.Config) (net.Conn, error) { return openConnection() },
			}
			defer system.closeConnection()
			defer close(allowSecondClose)
			service := &annexGService{localID: gb10PlatformID, realm: "3402000000"}
			body := []byte("<Notify><CmdType>MPAlarm</CmdType></Notify>")

			firstRequest, err := buildAnnexGOutboundRequest(server, service, system, body)
			if err != nil {
				t.Fatal(err)
			}
			firstCtx, cancelFirst := context.WithTimeout(t.Context(), 100*time.Millisecond)
			if err := system.prepareRequestConnection(firstCtx, server, firstRequest); err != nil {
				cancelFirst()
				t.Fatal(err)
			}
			err = sendAnnexGSIPRequest(firstCtx, sipServer, system, firstRequest)
			cancelFirst()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("first Annex G request error = %v, want context deadline exceeded", err)
			}
			system.connMu.Lock()
			cachedAfterTimeout := system.conn
			system.connMu.Unlock()
			if cachedAfterTimeout != nil {
				t.Fatalf("timed-out Annex G %s connection remained cached", transport)
			}
			select {
			case closeErr := <-firstClosed:
				if closeErr != nil {
					t.Fatal(closeErr)
				}
			case <-time.After(time.Second):
				t.Fatalf("timed-out Annex G %s connection was not closed", transport)
			}

			secondRequest, err := buildAnnexGOutboundRequest(server, service, system, body)
			if err != nil {
				t.Fatal(err)
			}
			secondCtx, cancelSecond := context.WithTimeout(t.Context(), time.Second)
			if err := system.prepareRequestConnection(secondCtx, server, secondRequest); err != nil {
				cancelSecond()
				t.Fatal(err)
			}
			err = sendAnnexGSIPRequest(secondCtx, sipServer, system, secondRequest)
			cancelSecond()
			if err != nil {
				t.Fatal(err)
			}
			if dials.Load() != 2 {
				t.Fatalf("Annex G %s dial calls = %d, want 2", transport, dials.Load())
			}
			if err := <-secondDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAnnexGDigestRetryTimeoutInvalidatesPersistentConnection(t *testing.T) {
	for _, transport := range []string{"tcp", "tls"} {
		t.Run(transport, func(t *testing.T) {
			local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.1:5060")
			sipServer := sip.NewServer(local)
			server := &Server{Server: sipServer, fromAddress: *local}
			defer sipServer.Close()

			port := 5060
			if transport == "tls" {
				port = 5061
			}
			targetURI, target, _, err := buildAnnexGTarget(gb10DeviceID, net.JoinHostPort("192.0.2.10", strconv.Itoa(port)), transport)
			if err != nil {
				t.Fatal(err)
			}
			remoteDone := make(chan error, 1)
			var dials atomic.Int32
			openConnection := func() (net.Conn, error) {
				if dial := dials.Add(1); dial != 1 {
					return nil, fmt.Errorf("unexpected Annex G redial %d", dial)
				}
				localConn, remoteConn := net.Pipe()
				go func() {
					defer remoteConn.Close()
					reader := bufio.NewReader(remoteConn)
					first, readErr := readAnnexGTestSIPFrame(reader)
					if readErr != nil {
						remoteDone <- readErr
						return
					}
					challenge := annexGTestSIPResponse(first, 401, "Unauthorized", `WWW-Authenticate: Digest realm="3402000000",qop="auth",nonce="timeout-nonce",algorithm=MD5`)
					if _, writeErr := io.WriteString(remoteConn, challenge); writeErr != nil {
						remoteDone <- writeErr
						return
					}
					retry, readErr := readAnnexGTestSIPFrame(reader)
					if readErr != nil {
						remoteDone <- readErr
						return
					}
					if annexGTestSIPHeader(retry, "Authorization") == "" {
						remoteDone <- errors.New("Annex G Digest retry missing Authorization")
						return
					}
					buffer := make([]byte, 1)
					_, readErr = remoteConn.Read(buffer)
					if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, net.ErrClosed) {
						remoteDone <- fmt.Errorf("timed-out Annex G Digest connection close: %w", readErr)
						return
					}
					remoteDone <- nil
				}()
				return &cascadeTestTCPConn{
					Conn:   localConn,
					local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 41400},
					remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: port},
				}, nil
			}
			system := &annexGSystem{
				id: gb10DeviceID, version: annexg.Version2011, password: "annex-g-secret", realm: "3402000000", transport: transport,
				targetURI: targetURI, target: target, tlsConfig: &tls.Config{MinVersion: tls.VersionTLS12},
				dialTCP: func(context.Context, string) (net.Conn, error) { return openConnection() },
				dialTLS: func(context.Context, string, *tls.Config) (net.Conn, error) { return openConnection() },
			}
			defer system.closeConnection()
			service := &annexGService{localID: gb10PlatformID, realm: "3402000000"}
			request, err := buildAnnexGOutboundRequest(server, service, system, []byte("<Notify><CmdType>MPAlarm</CmdType></Notify>"))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			if err := system.prepareRequestConnection(ctx, server, request); err != nil {
				cancel()
				t.Fatal(err)
			}
			err = sendAnnexGSIPRequest(ctx, sipServer, system, request)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Annex G Digest retry error = %v, want context deadline exceeded", err)
			}
			system.connMu.Lock()
			cachedAfterTimeout := system.conn
			system.connMu.Unlock()
			if cachedAfterTimeout != nil {
				t.Fatalf("timed-out Annex G Digest %s connection remained cached", transport)
			}
			select {
			case remoteErr := <-remoteDone:
				if remoteErr != nil {
					t.Fatal(remoteErr)
				}
			case <-time.After(time.Second):
				t.Fatalf("timed-out Annex G Digest %s connection was not closed", transport)
			}
		})
	}
}

func TestAnnexGWriteTimeoutInvalidatesPersistentConnection(t *testing.T) {
	for _, transport := range []string{"tcp", "tls"} {
		t.Run(transport, func(t *testing.T) {
			local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.1:5060")
			sipServer := sip.NewServer(local)
			server := &Server{Server: sipServer, fromAddress: *local}
			defer sipServer.Close()

			port := 5060
			if transport == "tls" {
				port = 5061
			}
			targetURI, target, _, err := buildAnnexGTarget(gb10DeviceID, net.JoinHostPort("192.0.2.10", strconv.Itoa(port)), transport)
			if err != nil {
				t.Fatal(err)
			}
			localConn, remoteConn := net.Pipe()
			defer remoteConn.Close()
			var dials atomic.Int32
			openConnection := func() (net.Conn, error) {
				dials.Add(1)
				return &cascadeTestTCPConn{
					Conn:   localConn,
					local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 41600},
					remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: port},
				}, nil
			}
			system := &annexGSystem{
				id: gb10DeviceID, version: annexg.Version2011, transport: transport,
				targetURI: targetURI, target: target, tlsConfig: &tls.Config{MinVersion: tls.VersionTLS12},
				dialTCP: func(context.Context, string) (net.Conn, error) { return openConnection() },
				dialTLS: func(context.Context, string, *tls.Config) (net.Conn, error) { return openConnection() },
			}
			defer system.closeConnection()
			request, err := buildAnnexGOutboundRequest(server, &annexGService{localID: gb10PlatformID, realm: "3402000000"}, system,
				[]byte("<Notify><CmdType>MPAlarm</CmdType></Notify>"))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			if err := system.prepareRequestConnection(ctx, server, request); err != nil {
				cancel()
				t.Fatal(err)
			}
			err = sendAnnexGSIPRequest(ctx, sipServer, system, request)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("blocked Annex G %s write error = %v, want context deadline exceeded", transport, err)
			}
			system.connMu.Lock()
			cachedAfterTimeout := system.conn
			system.connMu.Unlock()
			if cachedAfterTimeout != nil {
				t.Fatalf("write-blocked Annex G %s connection remained cached", transport)
			}
			if dials.Load() != 1 {
				t.Fatalf("Annex G %s dial calls = %d, want 1", transport, dials.Load())
			}
		})
	}
}

func readAnnexGTestSIPFrame(reader *bufio.Reader) (string, error) {
	var builder strings.Builder
	contentLength := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		builder.WriteString(line)
		trimmed := strings.TrimRight(line, "\r\n")
		if name, value, ok := strings.Cut(trimmed, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			contentLength, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil || contentLength < 0 {
				return "", fmt.Errorf("invalid Content-Length %q", value)
			}
		}
		if trimmed == "" {
			break
		}
	}
	if contentLength > 0 {
		body := make([]byte, contentLength)
		if _, err := io.ReadFull(reader, body); err != nil {
			return "", err
		}
		builder.Write(body)
	}
	return builder.String(), nil
}

func annexGTestSIPHeader(frame, name string) string {
	for _, line := range strings.Split(frame, "\r\n") {
		headerName, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(headerName), name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func annexGTestSIPResponse(request string, status int, reason, extra string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "SIP/2.0 %d %s\r\n", status, reason)
	for _, name := range []string{"Via", "From", "To", "Call-ID", "CSeq"} {
		fmt.Fprintf(&builder, "%s: %s\r\n", name, annexGTestSIPHeader(request, name))
	}
	if extra != "" {
		builder.WriteString(extra)
		if !strings.HasSuffix(extra, "\r\n") {
			builder.WriteString("\r\n")
		}
	}
	builder.WriteString("Content-Length: 0\r\n\r\n")
	return builder.String()
}

func TestAnnexGOversizedUDPRequestUsesTCP(t *testing.T) {
	localURI, err := sip.ParseSipURI("sip:" + gb10PlatformID + "@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	targetURI, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@192.0.2.10:5060")
	if err != nil {
		t.Fatal(err)
	}
	sipServer := sip.NewServer(&sip.Address{URI: &localURI, Params: sip.NewParams()})
	server := &Server{
		Server: sipServer, fromAddress: sip.Address{URI: &localURI, Params: sip.NewParams()},
	}
	system := &annexGSystem{
		id: gb10DeviceID, version: annexg.Version2011, transport: "udp",
		targetURI: &targetURI, target: &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
	}
	received := make(chan string, 1)
	remoteDone := make(chan error, 1)
	system.dialTCP = func(_ context.Context, address string) (net.Conn, error) {
		if address != "192.0.2.10:5060" {
			return nil, fmt.Errorf("Annex G TCP target = %q", address)
		}
		client, remote := net.Pipe()
		go func() {
			defer remote.Close()
			message, readErr := readAnnexGTestSIPFrame(bufio.NewReader(remote))
			if readErr != nil {
				remoteDone <- readErr
				return
			}
			received <- message
			_, writeErr := io.WriteString(remote, annexGTestSIPResponse(message, 200, "OK", ""))
			remoteDone <- writeErr
		}()
		return &cascadeTestTCPConn{
			Conn:   client,
			local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 42002},
			remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
		}, nil
	}
	t.Cleanup(func() {
		system.closeConnection()
		sipServer.Close()
	})

	request, err := buildAnnexGOutboundRequest(server, &annexGService{localID: gb10PlatformID, realm: "3402000000"}, system, []byte(strings.Repeat("x", 1301)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := system.prepareRequestConnection(ctx, server, request); err != nil {
		t.Fatal(err)
	}
	if err := sendAnnexGSIPRequest(ctx, sipServer, system, request); err != nil {
		t.Fatal(err)
	}
	if err := <-remoteDone; err != nil {
		t.Fatal(err)
	}
	message := <-received
	if !strings.Contains(message, "Via: SIP/2.0/TCP") {
		t.Fatalf("oversized Annex G request Via = %s", message)
	}
	if startLine := strings.SplitN(message, "\r\n", 2)[0]; !strings.Contains(startLine, "transport=tcp") {
		t.Fatalf("oversized Annex G request URI = %q", startLine)
	}
}

func TestSendAnnexGSIPRequestRetriesRemoteDigestChallenge(t *testing.T) {
	for _, version := range []annexg.Version{annexg.Version2011, annexg.Version2014, annexg.Version2016} {
		for _, challengeStatus := range []struct {
			status          int
			reason          string
			challengeHeader string
			authorizeHeader string
		}{
			{status: 401, reason: "Unauthorized", challengeHeader: "WWW-Authenticate", authorizeHeader: "Authorization"},
			{status: 407, reason: "Proxy Authentication Required", challengeHeader: "Proxy-Authenticate", authorizeHeader: "Proxy-Authorization"},
		} {
			t.Run(string(version)+"/"+strconv.Itoa(challengeStatus.status), func(t *testing.T) {
				local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.1:5060")
				sipServer := sip.NewServer(local)
				server := &Server{Server: sipServer, fromAddress: *local}
				remoteDone := make(chan error, 1)
				var remote net.Conn
				system := &annexGSystem{
					id: gb10DeviceID, version: version, password: "remote-secret", realm: "3401000000",
					transport: "tcp", targetURI: mustFlowAddress(t, "sip:"+gb10DeviceID+"@remote.example").URI,
					target: &testAddr{network: "tcp", value: "pipe"},
					dialTCP: func(context.Context, string) (net.Conn, error) {
						localConn, remoteConn := net.Pipe()
						remote = remoteConn
						go func() {
							defer remoteConn.Close()
							reader := bufio.NewReader(remoteConn)
							first, err := readAnnexGTestSIPFrame(reader)
							if err != nil {
								remoteDone <- err
								return
							}
							if annexGTestSIPHeader(first, "Authorization") != "" || annexGTestSIPHeader(first, "Proxy-Authorization") != "" {
								remoteDone <- errors.New("initial Annex G request unexpectedly contained Digest authorization")
								return
							}
							if annexGTestSIPHeader(first, "Date") == "" || !strings.Contains(annexGTestSIPHeader(first, "Note"), "Digest nonce=") {
								remoteDone <- errors.New("initial Annex G request did not contain Date+Note signal Digest")
								return
							}
							challengeHeader := fmt.Sprintf(`%s: Digest realm="3401000000",qop="auth",nonce="remote-nonce",algorithm=MD5`, challengeStatus.challengeHeader)
							challenge := annexGTestSIPResponse(first, challengeStatus.status, challengeStatus.reason, challengeHeader)
							if _, err := io.WriteString(remoteConn, challenge); err != nil {
								remoteDone <- err
								return
							}
							second, err := readAnnexGTestSIPFrame(reader)
							if err != nil {
								remoteDone <- err
								return
							}
							authorization := annexGTestSIPHeader(second, challengeStatus.authorizeHeader)
							if !strings.Contains(authorization, `username="`+gb10DeviceID+`"`) ||
								!strings.Contains(authorization, `realm="3401000000"`) ||
								!strings.Contains(authorization, `response="`) {
								remoteDone <- fmt.Errorf("authenticated request %s = %q", challengeStatus.authorizeHeader, authorization)
								return
							}
							otherHeader := "Authorization"
							if challengeStatus.authorizeHeader == otherHeader {
								otherHeader = "Proxy-Authorization"
							}
							if annexGTestSIPHeader(second, otherHeader) != "" {
								remoteDone <- fmt.Errorf("authenticated request unexpectedly contained %s", otherHeader)
								return
							}
							firstStart, _, _ := strings.Cut(first, "\r\n")
							secondStart, _, _ := strings.Cut(second, "\r\n")
							_, firstBody, _ := strings.Cut(first, "\r\n\r\n")
							_, secondBody, _ := strings.Cut(second, "\r\n\r\n")
							if firstStart != secondStart || firstBody != secondBody ||
								annexGTestSIPHeader(first, "Call-ID") != annexGTestSIPHeader(second, "Call-ID") ||
								annexGTestSIPHeader(second, "X-GB-Ver") != string(version) {
								remoteDone <- errors.New("authenticated Annex G request changed its target, Call-ID, X-GB-Ver, or XML body")
								return
							}
							if annexGTestSIPHeader(first, "Via") == annexGTestSIPHeader(second, "Via") ||
								annexGTestSIPHeader(first, "CSeq") != "1 MESSAGE" || annexGTestSIPHeader(second, "CSeq") != "2 MESSAGE" {
								remoteDone <- errors.New("authenticated Annex G request did not refresh Via and increment CSeq")
								return
							}
							if annexGTestSIPHeader(second, "Date") == "" || !strings.Contains(annexGTestSIPHeader(second, "Note"), "Digest nonce=") {
								remoteDone <- errors.New("authenticated Annex G request did not contain Date+Note signal Digest")
								return
							}
							_, err = io.WriteString(remoteConn, annexGTestSIPResponse(second, 200, "OK", ""))
							remoteDone <- err
						}()
						return localConn, nil
					},
				}
				signalSecurity, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
					Seed: "annex-g-note-seed", Algorithm: "MD5", Encoding: "base64",
				})
				if err != nil {
					t.Fatal(err)
				}
				system.setSignalSecurity(signalSecurity)
				body, err := annexg.Encode(version, &annexg.MPAlarmNotify{CmdType: annexg.CommandMPAlarm, SN: 106, AlarmContent: annexg.MPAlarmRecord{
					AlarmNO: "mp-106", AlarmTime: "2026-08-27T10:20:30", DeviceID: gb10DeviceID,
					AlarmPriority: "1", AlarmMethod: "2", OriginalNO: "original-106", OriginalInfo: "original",
					Sender: "sender", Processor: "processor", AlarmLevel: "1", Disposal: "processed", AlarmInfo: "alarm",
				}})
				if err != nil {
					t.Fatal(err)
				}
				request, err := buildAnnexGOutboundRequest(server, &annexGService{localID: gb10PlatformID, realm: "3402000000"}, system, body)
				if err != nil {
					t.Fatal(err)
				}
				if err := system.prepareRequestConnection(t.Context(), server, request); err != nil {
					t.Fatal(err)
				}
				if via, ok := request.ViaHop(); !ok || via == nil || via.ProtocolName == "" || via.ProtocolVersion == "" || via.Transport == "" || via.Host == "" {
					t.Fatalf("outbound Via before send = %#v", via)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				if err := sendAnnexGSIPRequest(ctx, sipServer, system, request); err != nil {
					cancel()
					t.Fatal(err)
				}
				cancel()
				select {
				case err := <-remoteDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("remote Digest exchange did not finish")
				}
				system.closeConnection()
				if remote != nil {
					_ = remote.Close()
				}
				sipServer.Close()
			})
		}
	}
}

func TestSendAnnexGSIPRequestDigestRetriesOnlyOnce(t *testing.T) {
	for _, version := range []annexg.Version{annexg.Version2011, annexg.Version2014, annexg.Version2016} {
		for _, challengeStatus := range []struct {
			status int
			reason string
			header string
		}{
			{status: 401, reason: "Unauthorized", header: `WWW-Authenticate: Digest realm="3401000000",nonce="remote-nonce",algorithm=MD5`},
			{status: 407, reason: "Proxy Authentication Required", header: `Proxy-Authenticate: Digest realm="3401000000",nonce="remote-nonce",algorithm=MD5`},
		} {
			t.Run(string(version)+"/"+strconv.Itoa(challengeStatus.status), func(t *testing.T) {
				local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.1:5060")
				sipServer := sip.NewServer(local)
				defer sipServer.Close()
				server := &Server{Server: sipServer, fromAddress: *local}
				remoteDone := make(chan error, 1)
				system := &annexGSystem{
					id: gb10DeviceID, version: version, password: "remote-secret", realm: "3401000000",
					transport: "tcp", targetURI: mustFlowAddress(t, "sip:"+gb10DeviceID+"@remote.example").URI,
					target: &testAddr{network: "tcp", value: "pipe"},
					dialTCP: func(context.Context, string) (net.Conn, error) {
						localConn, remoteConn := net.Pipe()
						go func() {
							defer remoteConn.Close()
							reader := bufio.NewReader(remoteConn)
							first, err := readAnnexGTestSIPFrame(reader)
							if err != nil {
								remoteDone <- err
								return
							}
							challenge := annexGTestSIPResponse(first, challengeStatus.status, challengeStatus.reason, challengeStatus.header)
							if _, err := io.WriteString(remoteConn, challenge); err != nil {
								remoteDone <- err
								return
							}
							second, err := readAnnexGTestSIPFrame(reader)
							if err != nil {
								remoteDone <- err
								return
							}
							if _, err := io.WriteString(remoteConn, annexGTestSIPResponse(second, challengeStatus.status, challengeStatus.reason, challengeStatus.header)); err != nil {
								remoteDone <- err
								return
							}
							if err := remoteConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
								remoteDone <- err
								return
							}
							if third, err := readAnnexGTestSIPFrame(reader); err == nil {
								remoteDone <- fmt.Errorf("unexpected third Annex G Digest request: %s", third)
								return
							} else {
								var timeout net.Error
								if !errors.As(err, &timeout) || !timeout.Timeout() {
									remoteDone <- err
									return
								}
							}
							remoteDone <- nil
						}()
						return localConn, nil
					},
				}
				defer system.closeConnection()
				request, err := buildAnnexGOutboundRequest(server, &annexGService{localID: gb10PlatformID, realm: "3402000000"}, system,
					[]byte(`<Notify><CmdType>MPAlarm</CmdType><SN>106</SN></Notify>`))
				if err != nil {
					t.Fatal(err)
				}
				if err := system.prepareRequestConnection(t.Context(), server, request); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				err = sendAnnexGSIPRequest(ctx, sipServer, system, request)
				cancel()
				if err == nil || !strings.Contains(err.Error(), "authenticated SIP response status") {
					t.Fatalf("repeated Annex G Digest challenge error = %v", err)
				}
				select {
				case err := <-remoteDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("remote repeated Digest challenge check did not finish")
				}
			})
		}
	}
}

type tlsFlowConnection struct {
	*flowConnection
}

func (*tlsFlowConnection) SignalingTransport() string { return "TLS" }

type testAddr struct {
	network string
	value   string
}

func (address *testAddr) Network() string { return address.network }
func (address *testAddr) String() string  { return address.value }
