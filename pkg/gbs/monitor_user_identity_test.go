package gbs

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	testLocalGatewayID   = "34020000002110000001"
	testRemoteGatewayID  = "34030000002110000002"
	testTrustedGatewayID = "34040000002110000003"
	testLocalUserID      = "34020000003000000001"
	testRemoteUserID     = "34030000003000000001"
)

func testMonitorUserIdentityConfig() conf.SIPMonitorUserIdentity {
	return conf.SIPMonitorUserIdentity{
		Enabled:              true,
		Required:             true,
		LocalGatewayID:       testLocalGatewayID,
		RemoteGatewayID:      testRemoteGatewayID,
		LocalUserID:          testLocalUserID,
		LocalOrganization:    "localorg",
		LocalCategory:        "operator",
		LocalRank:            "level1",
		TrustedGatewayIDs:    []string{testTrustedGatewayID},
		AllowedUserIDs:       []string{testRemoteUserID},
		AllowedOrganizations: []string{"remoteorg"},
		AllowedCategories:    []string{"dispatcher"},
		AllowedRanks:         []string{"level2"},
		MaxHops:              8,
	}
}

func TestMonitorUserIdentityGenerateAndForward(t *testing.T) {
	policy, err := newMonitorUserIdentityPolicy(testMonitorUserIdentityConfig())
	if err != nil {
		t.Fatal(err)
	}
	local, err := policy.outgoing(nil)
	if err != nil {
		t.Fatal(err)
	}
	wantLocal := strings.Join([]string{testLocalGatewayID, testLocalUserID, "localorg", "operator", "level1"}, "-")
	if got := local.String(); got != wantLocal {
		t.Fatalf("local Monitor-User-Identity = %q, want %q", got, wantLocal)
	}

	incomingValue := strings.Join([]string{
		testRemoteGatewayID, testTrustedGatewayID, testRemoteUserID, "remoteorg", "dispatcher", "level2",
	}, "-")
	incoming, err := parseMonitorUserIdentity(incomingValue)
	if err != nil {
		t.Fatal(err)
	}
	forwarded, err := policy.outgoing(incoming)
	if err != nil {
		t.Fatal(err)
	}
	wantForwarded := testLocalGatewayID + "-" + incomingValue
	if got := forwarded.String(); got != wantForwarded {
		t.Fatalf("forwarded Monitor-User-Identity = %q, want %q", got, wantForwarded)
	}
	if incoming.String() != incomingValue {
		t.Fatal("outgoing mutated the verified incoming identity")
	}
}

func TestMonitorUserIdentityRejectsInvalidOrUnauthorizedPaths(t *testing.T) {
	policy, err := newMonitorUserIdentityPolicy(testMonitorUserIdentityConfig())
	if err != nil {
		t.Fatal(err)
	}
	valid := []string{testRemoteGatewayID, testTrustedGatewayID, testRemoteUserID, "remoteorg", "dispatcher", "level2"}
	tests := []struct {
		name   string
		mutate func([]string) []string
		want   string
	}{
		{name: "immediate gateway", mutate: func(parts []string) []string { parts[0] = testTrustedGatewayID; return parts }, want: "immediate gateway"},
		{name: "untrusted gateway", mutate: func(parts []string) []string { parts[1] = "34050000002110000004"; return parts }, want: "untrusted gateway"},
		{name: "loop", mutate: func(parts []string) []string { parts[1] = testLocalGatewayID; return parts }, want: "loop"},
		{name: "repeated gateway", mutate: func(parts []string) []string { parts[1] = testRemoteGatewayID; return parts }, want: "repeated gateway"},
		{name: "user", mutate: func(parts []string) []string { parts[2] = "34030000003000000002"; return parts }, want: "user is not allowed"},
		{name: "organization", mutate: func(parts []string) []string { parts[3] = "otherorg"; return parts }, want: "organization is not allowed"},
		{name: "category", mutate: func(parts []string) []string { parts[4] = "viewer"; return parts }, want: "category is not allowed"},
		{name: "rank", mutate: func(parts []string) []string { parts[5] = "level9"; return parts }, want: "rank is not allowed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parts := test.mutate(append([]string(nil), valid...))
			identity, parseErr := parseMonitorUserIdentity(strings.Join(parts, "-"))
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			err := policy.validateInbound(identity)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateInbound() error = %v, want substring %q", err, test.want)
			}
		})
	}

	for _, value := range []string{
		"",
		testRemoteGatewayID + "-" + testRemoteUserID + "-org-category",
		"34030000002000000002-" + testRemoteUserID + "-org-category-rank",
		testRemoteGatewayID + "-34030000002110000001-org-category-rank",
		testRemoteGatewayID + "-" + testRemoteUserID + "-org-category-rank\r\nInjected: value",
		testRemoteGatewayID + "-" + testRemoteUserID + "-" + strings.Repeat("o", 65) + "-category-rank",
	} {
		if _, err := parseMonitorUserIdentity(value); err == nil {
			t.Fatalf("invalid Monitor-User-Identity accepted: %q", value)
		}
	}
}

func TestMonitorUserIdentityFourProtocolProfiles(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		t.Run(version, func(t *testing.T) {
			platform, err := normalizeCascadePlatform(conf.SIPUpstream{
				Name: "identity-" + version, Enabled: true, ServerID: gb10PlatformID,
				Host: "192.0.2.30", Port: 5060, LocalID: gb10DeviceID, LocalHost: "192.0.2.20",
				Version: version, MonitorUserIdentity: testMonitorUserIdentityConfig(),
			}, conf.SIP{ID: gb10DeviceID, Domain: "3402000000", Host: "192.0.2.20", Port: 5060}, "")
			if err != nil {
				t.Fatal(err)
			}
			worker := newCascadeWorker(nil, platform)
			request := worker.newKeepaliveRequest([]byte("<Notify/>"))
			if err := worker.platform.monitorUserIdentity.apply(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			value, present, err := monitorUserIdentityHeader(request)
			if err != nil || !present || !strings.HasPrefix(value, testLocalGatewayID+"-") {
				t.Fatalf("version %s identity header = %q, present=%v, err=%v", version, value, present, err)
			}
			if headers := request.GetHeaders(monitorUserIdentityHeaderName); len(headers) != 1 {
				t.Fatalf("version %s identity header count = %d", version, len(headers))
			}
		})
	}
}

func TestMonitorUserIdentityMiddlewareAndDownstreamForwarding(t *testing.T) {
	policy, err := newMonitorUserIdentityPolicy(testMonitorUserIdentityConfig())
	if err != nil {
		t.Fatal(err)
	}
	connection := newFlowConnection()
	worker := &cascadeWorker{platform: cascadePlatform{
		serverID: gb10PlatformID, remote: connection.remote, monitorUserIdentity: policy,
	}}
	worker.status.Registered = true
	manager := NewCascadeManager(nil)
	manager.items["identity"] = worker
	api := &GB28181API{svr: &Server{cascade: manager}}

	value := strings.Join([]string{
		testRemoteGatewayID, testTrustedGatewayID, testRemoteUserID, "remoteorg", "dispatcher", "level2",
	}, "-")
	request := newFlowRequest(t, connection, sip.MethodMessage, "identity-middleware", nil)
	request.SetSource(connection.remote)
	request.AppendHeader(&sip.GenericHeader{HeaderName: monitorUserIdentityHeaderName, Contents: value})
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("identity-middleware", connection),
		DeviceID: gb10PlatformID, Source: connection.remote,
	}
	api.sipMonitorUserIdentityMiddleware(ctx)
	if ctx.IsAborted() {
		t.Fatal("valid Monitor-User-Identity was rejected")
	}
	identityCtx := monitorUserIdentityContext(ctx)
	forwardedRequest := sip.NewRequest("", sip.MethodMessage, &sip.URI{FHost: "downstream.example"}, sip.DefaultSipVersion, nil, nil)
	if err := applyForwardedMonitorUserIdentity(identityCtx, forwardedRequest); err != nil {
		t.Fatal(err)
	}
	forwardedValue, present, err := monitorUserIdentityHeader(forwardedRequest)
	if err != nil || !present || forwardedValue != testLocalGatewayID+"-"+value {
		t.Fatalf("downstream identity = %q, present=%v, err=%v", forwardedValue, present, err)
	}

	missing := newFlowRequest(t, connection, sip.MethodMessage, "identity-missing", nil)
	missing.SetSource(connection.remote)
	missingCtx := &sip.Context{
		Request: missing, Tx: sip.NewTransaction("identity-missing", connection),
		DeviceID: gb10PlatformID, Source: connection.remote,
	}
	api.sipMonitorUserIdentityMiddleware(missingCtx)
	if !missingCtx.IsAborted() {
		t.Fatal("Required policy accepted a missing Monitor-User-Identity")
	}

	duplicate := newFlowRequest(t, connection, sip.MethodMessage, "identity-duplicate", nil)
	duplicate.SetSource(connection.remote)
	duplicate.AppendHeader(&sip.GenericHeader{HeaderName: monitorUserIdentityHeaderName, Contents: value})
	duplicate.AppendHeader(&sip.GenericHeader{HeaderName: monitorUserIdentityHeaderName, Contents: value})
	if _, _, err := monitorUserIdentityHeader(duplicate); err == nil {
		t.Fatal("duplicate Monitor-User-Identity headers accepted")
	}
}

func TestMonitorUserIdentityDoesNotTrustSourcePortOnly(t *testing.T) {
	policy, err := newMonitorUserIdentityPolicy(testMonitorUserIdentityConfig())
	if err != nil {
		t.Fatal(err)
	}
	worker := &cascadeWorker{platform: cascadePlatform{
		serverID:            gb10PlatformID,
		remote:              &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		monitorUserIdentity: policy,
	}}
	worker.status.Registered = true
	manager := NewCascadeManager(nil)
	manager.items["identity"] = worker
	if _, ok := manager.matchRegistered(gb10PlatformID, &net.UDPAddr{IP: net.ParseIP("192.0.2.31"), Port: 5060}); ok {
		t.Fatal("identity policy matched a different source IP")
	}
}

func TestMonitorUserIdentitySeparatesDownstreamSubscriptionOwners(t *testing.T) {
	first := &monitorUserIdentity{
		Gateways: []string{testRemoteGatewayID}, UserID: testRemoteUserID,
		Organization: "remoteorg", Category: "dispatcher", Rank: "level2",
	}
	second := first.clone()
	second.UserID = "34030000003000000002"
	firstContext := withMonitorUserIdentityRoute(context.Background(), first, testLocalGatewayID)
	secondContext := withMonitorUserIdentityRoute(context.Background(), second, testLocalGatewayID)
	firstKey := monitorUserIdentitySubscriptionKey(firstContext)
	secondKey := monitorUserIdentitySubscriptionKey(secondContext)
	if firstKey == "" || secondKey == "" || firstKey == secondKey {
		t.Fatalf("subscription identity keys were not isolated: %q, %q", firstKey, secondKey)
	}
	if got := monitorUserIdentitySubscriptionKey(context.Background()); got != "" {
		t.Fatalf("local subscription unexpectedly received an identity key: %q", got)
	}
}

func TestWrapRequestContextCarriesVerifiedMonitorUserIdentity(t *testing.T) {
	localURI, err := sip.ParseSipURI("sip:" + gb10PlatformID + "@127.0.0.1:5060")
	if err != nil {
		t.Fatal(err)
	}
	remoteURI, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@3402000000")
	if err != nil {
		t.Fatal(err)
	}
	baseConnection := newFlowConnection()
	connection := &tcpFlowConnection{flowConnection: baseConnection}
	cfg := conf.DefaultConfig().Sip
	api := &GB28181API{cfg: &cfg}
	server := &Server{
		Server: sip.NewServer(&sip.Address{URI: &localURI, Params: sip.NewParams()}),
		gb:     api, fromAddress: sip.Address{URI: &localURI, Params: sip.NewParams()},
	}
	api.svr = server
	t.Cleanup(server.Close)
	target := &subscriptionTarget{
		to: &sip.Address{URI: &remoteURI, Params: sip.NewParams()}, source: baseConnection.remote,
		conn: connection, gbVersion: "1.0",
	}
	identity := &monitorUserIdentity{
		Gateways: []string{testRemoteGatewayID}, UserID: testRemoteUserID,
		Organization: "remoteorg", Category: "dispatcher", Rank: "level2",
	}
	ctx := withMonitorUserIdentityRoute(context.Background(), identity, testLocalGatewayID)
	tx, err := server.wrapRequestContext(ctx, target, sip.MethodMessage, &sip.ContentTypeXML, []byte("<Query/>"))
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	select {
	case payload := <-baseConnection.writes:
		want := "Monitor-User-Identity: " + testLocalGatewayID + "-" + identity.String()
		if !strings.Contains(string(payload), want) {
			t.Fatalf("downstream SIP request missing %q: %s", want, payload)
		}
	default:
		t.Fatal("downstream SIP request was not written")
	}
}

func TestMonitorUserIdentityTransactionSecurityDecoratesResponses(t *testing.T) {
	policy, err := newMonitorUserIdentityPolicy(testMonitorUserIdentityConfig())
	if err != nil {
		t.Fatal(err)
	}
	security := &monitorUserIdentityMessageSecurity{policy: policy}
	value := strings.Join([]string{
		testRemoteGatewayID, testTrustedGatewayID, testRemoteUserID, "remoteorg", "dispatcher", "level2",
	}, "-")
	request := sip.NewRequest("", sip.MethodMessage, &sip.URI{FHost: "local.example"}, sip.DefaultSipVersion,
		[]sip.Header{&sip.GenericHeader{HeaderName: monitorUserIdentityHeaderName, Contents: value}}, nil)
	if err := security.Verify(request); err != nil {
		t.Fatal(err)
	}
	response := sip.NewResponse("", sip.DefaultSipVersion, 200, "OK", nil, nil)
	if err := security.Sign(response); err != nil {
		t.Fatal(err)
	}
	got, present, err := monitorUserIdentityHeader(response)
	if err != nil || !present || got != testLocalGatewayID+"-"+value {
		t.Fatalf("response Monitor-User-Identity = %q, present=%v, err=%v", got, present, err)
	}

	request.RemoveHeader(monitorUserIdentityHeaderName)
	request.AppendHeader(&sip.GenericHeader{HeaderName: monitorUserIdentityHeaderName, Contents: value + "tampered"})
	if err := security.Verify(request); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed identity after verification result = %v", err)
	}
}

func TestResolveRequestSecurityCombinesIdentityAndSignalDigest(t *testing.T) {
	platform := testCascadePlatform(t, "2.0")
	platform.signalDigestSeed = "upstream-note-seed"
	policy, err := newMonitorUserIdentityPolicy(testMonitorUserIdentityConfig())
	if err != nil {
		t.Fatal(err)
	}
	platform.monitorUserIdentity = policy
	worker := newCascadeWorker(nil, platform)
	worker.updateStatus(func(status *CascadePlatformStatus) { status.Registered = true })
	manager := NewCascadeManager(nil)
	manager.items[platform.name] = worker
	api := &GB28181API{cfg: &conf.SIP{SignalDigest: conf.SIPSignalDigest{
		Enabled: true, Required: true, Seed: "global-note-seed", Algorithm: "MD5",
		Encoding: "base64", Window: conf.Duration(10 * time.Minute),
	}}}
	api.svr = &Server{cascade: manager}

	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodMessage, "combined-security", []byte("payload"))
	upstream := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	if from, ok := request.From(); !ok || from == nil {
		t.Fatal("test request From header is unavailable")
	} else {
		from.Address = upstream.URI
	}
	request.SetSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5090})
	value := strings.Join([]string{
		testRemoteGatewayID, testTrustedGatewayID, testRemoteUserID, "remoteorg", "dispatcher", "level2",
	}, "-")
	request.AppendHeader(&sip.GenericHeader{HeaderName: monitorUserIdentityHeaderName, Contents: value})
	digest, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
		Seed: "upstream-note-seed", Algorithm: "MD5", Encoding: "base64", Required: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := digest.Sign(request); err != nil {
		t.Fatal(err)
	}
	security, err := api.resolveRequestSecurity(request)
	if err != nil || security == nil {
		t.Fatalf("resolve combined request security = %v, %v", security, err)
	}
	if err := security.Verify(request); err != nil {
		t.Fatalf("combined security rejected request: %v", err)
	}
	response := sip.NewResponseFromRequest("", request, 200, "OK", nil)
	if err := security.Sign(response); err != nil {
		t.Fatal(err)
	}
	if _, present, err := monitorUserIdentityHeader(response); err != nil || !present {
		t.Fatalf("combined response identity present=%v err=%v", present, err)
	}
	if len(response.GetHeaders("Date")) != 1 || len(response.GetHeaders("Note")) != 1 {
		t.Fatalf("combined response missing Date/Note: %s", response.String())
	}
}
