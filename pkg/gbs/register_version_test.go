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

	assertRegisterHandlerResponse(t, connection, "SIP/2.0 401 Unauthorized")
	if memory.loadOrStoreCalls != 0 || memory.changeCalls != 0 {
		t.Fatalf("unauthenticated unregister mutated memory: load_or_store=%d change=%d", memory.loadOrStoreCalls, memory.changeCalls)
	}
	metrics := api.metrics.Snapshot()
	if metrics.RegisterSuccess != 0 || metrics.RegisterFailures != 0 {
		t.Fatalf("unauthenticated unregister metrics = success:%d failures:%d", metrics.RegisterSuccess, metrics.RegisterFailures)
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

type registerHandlerTestMemory struct {
	*flowMemory
	changeErr        error
	loadOrStoreCalls int
	changeCalls      int
}

func (m *registerHandlerTestMemory) LoadOrStore(deviceID string, device *Device) {
	m.loadOrStoreCalls++
	m.flowMemory.LoadOrStore(deviceID, device)
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
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
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
		cfg:  &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"},
		core: ipc.NewAdapter(store, uniqueid.Core{}),
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
