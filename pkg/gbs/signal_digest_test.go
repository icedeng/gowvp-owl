package gbs

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/annexg"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
)

type transportSelectionPanickingCloneHeader struct{}

func (*transportSelectionPanickingCloneHeader) Name() string { return "X-Clone-Panic" }
func (*transportSelectionPanickingCloneHeader) Clone() sip.Header {
	panic("transport selection header clone")
}
func (*transportSelectionPanickingCloneHeader) String() string {
	return "X-Clone-Panic: test"
}
func (*transportSelectionPanickingCloneHeader) Equals(any) bool { return false }

type transportSelectionPanickingStringHeader struct{}

func (*transportSelectionPanickingStringHeader) Name() string { return "X-String-Panic" }
func (header *transportSelectionPanickingStringHeader) Clone() sip.Header {
	return header
}
func (*transportSelectionPanickingStringHeader) String() string {
	panic("transport selection header serialization")
}
func (*transportSelectionPanickingStringHeader) Equals(any) bool { return false }

type transportSelectionNoopSecurity struct{}

func (transportSelectionNoopSecurity) Sign(sip.Message) error   { return nil }
func (transportSelectionNoopSecurity) Verify(sip.Message) error { return nil }

type transportSelectionPanickingSecurity struct{}

func (transportSelectionPanickingSecurity) Sign(sip.Message) error {
	panic("transport selection signer")
}
func (transportSelectionPanickingSecurity) Verify(sip.Message) error { return nil }

func TestSignedSIPRequestLengthRecoversExtensionPanics(t *testing.T) {
	newRequest := func() *sip.Request {
		return sip.NewRequest("", sip.MethodMessage, &sip.URI{FHost: "192.0.2.30"}, sip.DefaultSipVersion, nil, nil)
	}

	t.Run("header clone", func(t *testing.T) {
		request := newRequest()
		request.AppendHeader(&transportSelectionPanickingCloneHeader{})
		if _, err := signedSIPRequestLength(request, transportSelectionNoopSecurity{}); err == nil || !strings.Contains(err.Error(), "clone SIP request") {
			t.Fatalf("panicking Header.Clone error = %v", err)
		}
	})

	t.Run("header serialization", func(t *testing.T) {
		request := newRequest()
		request.AppendHeader(&transportSelectionPanickingStringHeader{})
		if _, err := signedSIPRequestLength(request, nil); err == nil || !strings.Contains(err.Error(), "serialize SIP request") {
			t.Fatalf("panicking Header.String error = %v", err)
		}
	})

	t.Run("security signer", func(t *testing.T) {
		if _, err := signedSIPRequestLength(newRequest(), transportSelectionPanickingSecurity{}); err == nil || !strings.Contains(err.Error(), "sign SIP request") {
			t.Fatalf("panicking MessageSecurity.Sign error = %v", err)
		}
	})
}

func TestResolveSignalDigestSecurityUsesDevicePasswordAcrossVersions(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.Password = "device-signal-seed"
			memory.runtime.setGBVersion(version)
			api := &GB28181API{
				cfg: &conf.SIP{SignalDigest: conf.SIPSignalDigest{
					Enabled: true, Required: true, Algorithm: "MD5", Encoding: "base64",
					AcceptLegacyHex: true, Window: conf.Duration(10 * time.Minute),
				}},
				svr: &Server{memoryStorer: memory},
			}
			request := newFlowRequest(t, newFlowConnection(), sip.MethodMessage, "signal-digest-"+string(version), []byte("payload"))
			request.RemoveHeader("X-GB-Ver")
			request.AppendHeader(&sip.GenericHeader{HeaderName: "X-GB-Ver", Contents: string(version)})
			signer, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
				Seed: "device-signal-seed", Algorithm: "MD5", Encoding: "base64", Required: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := signer.Sign(request); err != nil {
				t.Fatal(err)
			}
			security, err := api.resolveSignalDigestSecurity(request)
			if err != nil {
				t.Fatal(err)
			}
			if security == nil {
				t.Fatal("signal Digest security was not resolved")
			}
			if err := security.Verify(request); err != nil {
				t.Fatalf("version %s signed request rejected: %v", version, err)
			}
		})
	}
}

func TestSignalDigestSeedResolutionAndAlgorithms(t *testing.T) {
	device := &Device{Password: "device-seed"}
	channel := &Channel{device: device}
	if got := targetSignalDigestSeed(device); got != "device-seed" {
		t.Fatalf("device seed = %q", got)
	}
	if got := targetSignalDigestSeed(channel); got != "device-seed" {
		t.Fatalf("channel seed = %q", got)
	}
	device.Password = ignorePassword
	if got := targetSignalDigestSeed(device); got != "" {
		t.Fatalf("ignored device password seed = %q", got)
	}
	if got := targetSignalDigestSeed(channel); got != "" {
		t.Fatalf("ignored channel password seed = %q", got)
	}
	platform := cascadePlatform{password: "upstream-password", signalDigestSeed: "upstream-note-seed"}
	if got := cascadeSignalDigestSeed(platform, "fallback"); got != "upstream-note-seed" {
		t.Fatalf("explicit upstream seed = %q", got)
	}
	platform.signalDigestSeed = ""
	if got := cascadeSignalDigestSeed(platform, "fallback"); got != "upstream-password" {
		t.Fatalf("upstream password seed = %q", got)
	}
	if got := annexGSignalDigestSeed("annex-explicit", "annex-password", "fallback"); got != "annex-explicit" {
		t.Fatalf("explicit Annex G seed = %q", got)
	}
	if got := annexGSignalDigestSeed("", "annex-password", "fallback"); got != "annex-password" {
		t.Fatalf("Annex G password seed = %q", got)
	}
	if got := annexGSignalDigestSeed("", ignorePassword, "fallback"); got != "fallback" {
		t.Fatalf("Annex G fallback seed = %q", got)
	}
	platform.signalDigestSeed = " upstream-note-seed "
	if got := cascadeSignalDigestSeed(platform, "fallback"); got != " upstream-note-seed " {
		t.Fatalf("upstream seed whitespace changed = %q", got)
	}
	platform.signalDigestSeed = ""
	platform.password = " upstream-password "
	if got := cascadeSignalDigestSeed(platform, "fallback"); got != " upstream-password " {
		t.Fatalf("upstream password seed whitespace changed = %q", got)
	}
	if got := annexGSignalDigestSeed(" annex-explicit ", "annex-password", "fallback"); got != " annex-explicit " {
		t.Fatalf("Annex G explicit seed whitespace changed = %q", got)
	}
	if got := annexGSignalDigestSeed("", " annex-password ", "fallback"); got != " annex-password " {
		t.Fatalf("Annex G password seed whitespace changed = %q", got)
	}

	api := &GB28181API{cfg: &conf.SIP{SignalDigest: conf.SIPSignalDigest{
		Enabled: true, Seed: "seed", Algorithm: "SM3", Encoding: "base64", Window: conf.Duration(time.Minute),
	}}}
	if _, err := api.newSignalDigestSecurity(""); err != nil {
		t.Fatalf("SM3 signal Digest was rejected: %v", err)
	}
	api.cfg.SignalDigest.Algorithm = "SM4"
	if _, err := api.newSignalDigestSecurity(""); err == nil {
		t.Fatal("unsupported SM4 signal Digest was accepted")
	}
}

func TestExplicitSignalDigestSeedHashIsNotPasswordPlaceholder(t *testing.T) {
	cfg := &conf.SIP{SignalDigest: conf.SIPSignalDigest{
		Enabled: true, Seed: "fallback", Algorithm: "MD5", Encoding: "base64", Window: conf.Duration(time.Minute),
	}}
	security, err := newSignalDigestSecurity(cfg, "#")
	if err != nil {
		t.Fatal(err)
	}
	request := newFlowRequest(t, newFlowConnection(), sip.MethodMessage, "explicit-hash-seed", []byte("payload"))
	if err := security.Sign(request); err != nil {
		t.Fatal(err)
	}
	explicit, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
		Seed: "#", Algorithm: "MD5", Encoding: "base64", Window: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := explicit.Verify(request); err != nil {
		t.Fatalf("explicit hash seed was replaced by password fallback: %v", err)
	}
	fallback, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
		Seed: "fallback", Algorithm: "MD5", Encoding: "base64", Window: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fallback.Verify(request); err == nil {
		t.Fatal("explicit hash seed unexpectedly used the global fallback")
	}
}

func TestIgnoredDevicePasswordUsesSignalDigestFallback(t *testing.T) {
	cfg := conf.DefaultConfig().Sip
	cfg.SignalDigest.Enabled = true
	cfg.SignalDigest.Seed = "fallback"
	cfg.SignalDigest.Algorithm = "MD5"
	cfg.SignalDigest.Encoding = "base64"
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Password = ignorePassword
	api := &GB28181API{cfg: &cfg, svr: &Server{memoryStorer: memory}}
	request := newFlowRequest(t, newFlowConnection(), sip.MethodMessage, "ignored-password-seed", []byte("payload"))
	security, err := api.resolveSignalDigestSecurity(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := security.Sign(request); err != nil {
		t.Fatal(err)
	}
	fallback, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
		Seed: "fallback", Algorithm: "MD5", Encoding: "base64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fallback.Verify(request); err != nil {
		t.Fatalf("ignored device password did not use signal Digest fallback: %v", err)
	}
}

func TestResolveSignalDigestSecurityUsesRegisteredUpstreamSeed(t *testing.T) {
	platform := testCascadePlatform(t, "1.1")
	platform.signalDigestSeed = "upstream-note-seed"
	worker := newCascadeWorker(nil, platform)
	worker.updateStatus(func(status *CascadePlatformStatus) { status.Registered = true })
	manager := NewCascadeManager(nil)
	manager.items[platform.name] = worker
	api := &GB28181API{cfg: &conf.SIP{SignalDigest: conf.SIPSignalDigest{
		Enabled: true, Required: true, Seed: "global-note-seed", Algorithm: "MD5",
		Encoding: "base64", Window: conf.Duration(10 * time.Minute),
	}}}
	api.svr = &Server{cascade: manager}

	request := newFlowRequest(t, newFlowConnection(), sip.MethodMessage, "upstream-signal-digest", []byte("payload"))
	upstream := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	request.RemoveHeader("From")
	request.AppendHeader(&sip.FromHeader{Address: upstream.URI, Params: sip.NewParams()})
	request.SetSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5090})
	signer, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
		Seed: "upstream-note-seed", Algorithm: "MD5", Encoding: "base64", Required: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.Sign(request); err != nil {
		t.Fatal(err)
	}
	security, err := api.resolveSignalDigestSecurity(request)
	if err != nil {
		t.Fatal(err)
	}
	if security == nil {
		t.Fatal("registered upstream signal Digest security was not resolved")
	}
	if err := security.Verify(request); err != nil {
		t.Fatalf("registered upstream signature rejected: %v", err)
	}
}

func TestResolveSignalDigestSecurityUsesAnnexGSystemSeed(t *testing.T) {
	cfg := annexGTestSIPConfig(annexg.RoleEmergencyCommandSystem, true)
	cfg.SignalDigest = conf.SIPSignalDigest{
		Enabled: true, Required: true, Seed: "global-note-seed", Algorithm: "MD5",
		Encoding: "base64", Window: conf.Duration(10 * time.Minute),
	}
	cfg.AnnexG.Systems[0].SignalDigestSeed = "annex-g-note-seed"
	service, _ := newAnnexGRuntimeWithConfig(t, cfg)
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Password = "ordinary-device-seed"
	api := &GB28181API{cfg: &cfg, annexG: service, svr: &Server{memoryStorer: memory}}

	request := newFlowRequest(t, newFlowConnection(), sip.MethodMessage, "annex-g-signal-digest", testECSAlarmBody(t))
	signer, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
		Seed: "annex-g-note-seed", Algorithm: "MD5", Encoding: "base64", Required: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.Sign(request); err != nil {
		t.Fatal(err)
	}
	security, err := api.resolveSignalDigestSecurity(request)
	if err != nil {
		t.Fatal(err)
	}
	if security == nil {
		t.Fatal("Annex G signal Digest security was not resolved")
	}
	if err := security.Verify(request); err != nil {
		t.Fatalf("Annex G system signature was rejected: %v", err)
	}

	missing, _ := request.Clone().(*sip.Request)
	missing.RemoveHeader("Date")
	missing.RemoveHeader("Note")
	if err := security.Verify(missing); err == nil {
		t.Fatal("required Annex G signal Digest accepted missing Date/Note")
	}
	wrong, _ := missing.Clone().(*sip.Request)
	wrongSigner, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
		Seed: "global-note-seed", Algorithm: "MD5", Encoding: "base64", Required: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wrongSigner.Sign(wrong); err != nil {
		t.Fatal(err)
	}
	if err := security.Verify(wrong); err == nil {
		t.Fatal("Annex G signal Digest accepted the lower-priority global seed")
	}

	cfg.SignalDigest.Algorithm = "SHA-256"
	api.setConfig(cfg)
	updated := newFlowRequest(t, newFlowConnection(), sip.MethodMessage, "annex-g-signal-digest-updated", testECSAlarmBody(t))
	updatedSigner, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
		Seed: "annex-g-note-seed", Algorithm: "SHA-256", Encoding: "base64", Required: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := updatedSigner.Sign(updated); err != nil {
		t.Fatal(err)
	}
	updatedSecurity, err := api.resolveSignalDigestSecurity(updated)
	if err != nil {
		t.Fatal(err)
	}
	if updatedSecurity == nil {
		t.Fatal("updated Annex G signal Digest security was not resolved")
	}
	if err := updatedSecurity.Verify(updated); err != nil {
		t.Fatalf("hot-reloaded Annex G signal Digest was rejected: %v", err)
	}
}

func TestWrapRequestSignsOutboundDeviceMessage(t *testing.T) {
	flow := newFlowConnection()
	connection := &signalDigestFlowTCPConnection{flowConnection: flow}
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	deviceAddress := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	sipServer := sip.NewServer(platform)
	defer sipServer.Close()
	api := &GB28181API{cfg: &conf.SIP{SignalDigest: conf.SIPSignalDigest{
		Enabled: true, Algorithm: "MD5", Encoding: "base64", Window: conf.Duration(10 * time.Minute),
	}}}
	server := &Server{Server: sipServer, gb: api, fromAddress: *platform}
	api.svr = server
	device := &Device{
		Password: "device-signal-seed", conn: connection, source: flow.remote, to: deviceAddress,
		gbVersion: string(GBVersion10),
	}
	if _, err := server.wrapRequest(device, sip.MethodMessage, &sip.ContentTypeXML, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-flow.writes:
		text := string(payload)
		if !strings.Contains(text, "Date: ") || !strings.Contains(text, "Note: Digest nonce=") || !strings.Contains(text, "algorithm=MD5") {
			t.Fatalf("outbound secured MESSAGE = %s", text)
		}
	case <-time.After(time.Second):
		t.Fatal("outbound secured MESSAGE timeout")
	}
}

func TestDialogRequestSignsOutboundDeviceMessage(t *testing.T) {
	fixture := newSignalDigestDialogFixture(t, "dialog-signal-digest")
	request, err := sip.NewRequestFromResponseChecked(sip.MethodBYE, fixture.response)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := fixture.server.requestDialogContext(context.Background(), fixture.channel, request)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	select {
	case payload := <-fixture.flow.writes:
		text := string(payload)
		if !strings.HasPrefix(text, "BYE ") || !strings.Contains(text, "Date: ") ||
			!strings.Contains(text, "Note: Digest nonce=") {
			t.Fatalf("secured dialog request = %s", text)
		}
	case <-time.After(time.Second):
		t.Fatal("secured dialog request timeout")
	}
}

func TestStopPlaySignsProductionBYE(t *testing.T) {
	fixture := newSignalDigestDialogFixture(t, "stop-play-signal-digest")
	input := &StopPlayInput{Channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}}
	fixture.api.streams.Store(resolvePlaySessionKey(gb10DeviceID, gb10ChannelID, ""), &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "secured-live", Resp: fixture.response,
	})

	if err := fixture.api.stopPlay(fixture.channel, input); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-fixture.flow.writes:
		text := string(payload)
		if !strings.HasPrefix(text, "BYE ") || !strings.Contains(text, "Date: ") ||
			!strings.Contains(text, "Note: Digest nonce=") {
			t.Fatalf("secured stop-play BYE = %s", text)
		}
	case <-time.After(time.Second):
		t.Fatal("secured stop-play BYE timeout")
	}
}

func TestControlHistorySignsProductionINFO(t *testing.T) {
	fixture := newSignalDigestDialogFixture(t, "history-info-signal-digest")
	fixture.api.streams.Store(historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID), &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "secured-history", Resp: fixture.response,
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- fixture.api.ControlHistory(ctx, &ControlHistoryInput{
			Channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID},
			Mode:    historyModePlayback,
			Cmd:     "PLAY MANSRTSP/1.0\r\nCSeq: 1\r\n\r\n",
		})
	}()

	select {
	case payload := <-fixture.flow.writes:
		text := string(payload)
		if !strings.HasPrefix(text, "INFO ") || !strings.Contains(text, "Content-Type: Application/MANSRTSP") ||
			!strings.Contains(text, "Date: ") || !strings.Contains(text, "Note: Digest nonce=") {
			t.Fatalf("secured history INFO = %s", text)
		}
		cancel()
	case <-time.After(time.Second):
		t.Fatal("secured history INFO timeout")
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled history INFO result = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("history INFO cancellation timeout")
	}
}

func TestRegisterFollowupDeviceInfoSignsProductionMESSAGE(t *testing.T) {
	fixture := newSignalDigestDialogFixture(t, "register-device-info-signal-digest")
	done := make(chan struct{})
	go func() {
		defer close(done)
		fixture.api.QueryDeviceInfo(&sip.Context{DeviceID: gb10DeviceID, Log: slog.Default()})
	}()

	select {
	case payload := <-fixture.flow.writes:
		text := string(payload)
		if !strings.HasPrefix(text, "MESSAGE ") || !strings.Contains(text, "<CmdType>DeviceInfo</CmdType>") ||
			!strings.Contains(text, "Date: ") || !strings.Contains(text, "Note: Digest nonce=") {
			t.Fatalf("secured register follow-up DeviceInfo = %s", text)
		}
		fixture.server.Server.Close()
	case <-time.After(time.Second):
		t.Fatal("secured register follow-up DeviceInfo timeout")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("register follow-up DeviceInfo shutdown timeout")
	}
}

func TestInboundCascadeBYESignsAndCarriesMonitorUserIdentity(t *testing.T) {
	platform := testCascadeTCPPlatform(t, "3.0")
	platform.signalDigestSeed = "cascade-dialog-seed"
	policy, err := newMonitorUserIdentityPolicy(testMonitorUserIdentityConfig())
	if err != nil {
		t.Fatal(err)
	}
	platform.monitorUserIdentity = policy
	local := mustFlowAddress(t, "sip:"+gb10DeviceID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	sipServer := sip.NewServer(local)
	api := &GB28181API{cfg: &conf.SIP{SignalDigest: conf.SIPSignalDigest{
		Enabled: true, Required: true, Seed: "global-dialog-seed", Algorithm: "MD5",
		Encoding: "base64", Window: conf.Duration(10 * time.Minute),
	}}}
	server := &Server{Server: sipServer, gb: api, fromAddress: *local}
	api.svr = server
	worker := newCascadeWorker(server, platform)
	client, upstream := net.Pipe()
	worker.dialTCP = func(context.Context, string) (net.Conn, error) { return client, nil }
	t.Cleanup(func() {
		worker.cancel()
		worker.closeTCPConnection()
		_ = upstream.Close()
		sipServer.Close()
	})

	incomingValue := strings.Join([]string{
		testRemoteGatewayID, testTrustedGatewayID, testRemoteUserID, "remoteorg", "dispatcher", "level2",
	}, "-")
	identity, err := parseMonitorUserIdentity(incomingValue)
	if err != nil {
		t.Fatal(err)
	}
	callID := sip.CallID("inbound-cascade-dialog-security")
	invite := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetContact(remote).SetMethod(sip.MethodInvite).
			SetSeqNo(23).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	invite.SetSource(&net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060})
	invite.SetDestination(&net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060})
	response := sip.NewResponseFromRequest("", invite, 200, "OK", nil)
	dialog := &inboundInviteDialog{
		CallID: callIDFromRequest(invite), DeviceID: gb10PlatformID, Established: true, LocalCSeq: 23,
		Request: invite, Response: response,
		Cascade: &cascadeMediaSession{worker: worker, identityCtx: withMonitorUserIdentity(context.Background(), identity)},
	}
	messageCh := make(chan string, 1)
	readErrCh := make(chan error, 1)
	go func() {
		message, readErr := readCascadeTestTCPMessage(bufio.NewReader(upstream))
		if readErr != nil {
			readErrCh <- readErr
			return
		}
		messageCh <- message
	}()

	if err := api.sendInboundDialogBYE(dialog); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-messageCh:
		if !strings.HasPrefix(message, "BYE ") {
			t.Fatalf("inbound cascade dialog request = %s", message)
		}
		wantIdentity := testLocalGatewayID + "-" + incomingValue
		if got := cascadeTestHeader(message, monitorUserIdentityHeaderName); got != wantIdentity {
			t.Fatalf("cascade BYE Monitor-User-Identity = %q, want %q", got, wantIdentity)
		}
		if err := verifyCascadeTestSignalDigest(message, platform.signalDigestSeed); err != nil {
			t.Fatal(err)
		}
	case err := <-readErrCh:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("secured inbound cascade BYE timeout")
	}
}

type signalDigestDialogFixture struct {
	api      *GB28181API
	server   *Server
	flow     *flowConnection
	channel  *Channel
	response *sip.Response
}

func newSignalDigestDialogFixture(t *testing.T, callIDValue string) signalDigestDialogFixture {
	t.Helper()
	flow := newFlowConnection()
	connection := &signalDigestFlowTCPConnection{flowConnection: flow}
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	deviceAddress := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	sipServer := sip.NewServer(platform)
	t.Cleanup(sipServer.Close)
	api := &GB28181API{cfg: &conf.SIP{SignalDigest: conf.SIPSignalDigest{
		Enabled: true, Algorithm: "MD5", Encoding: "base64", Window: conf.Duration(10 * time.Minute),
	}}, streams: &conc.Map[string, *Streams]{}}
	device := &Device{
		Password: "device-signal-seed", conn: connection, source: flow.remote, to: deviceAddress,
		gbVersion: string(GBVersion10),
	}
	channel := &Channel{ChannelID: gb10ChannelID, to: deviceAddress, device: device}
	device.Channels.Store(gb10ChannelID, channel)
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime = device
	server := &Server{Server: sipServer, gb: api, fromAddress: *platform, memoryStorer: memory}
	api.svr = server
	callID := sip.CallID(callIDValue)
	invite := sip.NewRequest("", sip.MethodInvite, deviceAddress.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(platform).SetTo(deviceAddress).SetMethod(sip.MethodInvite).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.1", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	invite.SetConnection(connection)
	invite.SetSource(flow.local)
	invite.SetDestination(flow.remote)
	response := sip.NewResponseFromRequest("", invite, 200, "OK", nil)
	response.AppendHeader(&sip.ContactHeader{Address: deviceAddress.URI})
	return signalDigestDialogFixture{api: api, server: server, flow: flow, channel: channel, response: response}
}

type signalDigestFlowTCPConnection struct {
	*flowConnection
}

func (*signalDigestFlowTCPConnection) Network() string { return "tcp" }
