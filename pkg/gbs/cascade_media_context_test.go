package gbs

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
)

func TestInboundDialogBYERejectsUnavailableSIPServer(t *testing.T) {
	dialog := &inboundInviteDialog{Established: true}
	err := (&GB28181API{}).sendInboundDialogBYEContext(t.Context(), dialog)
	if err == nil || !strings.Contains(err.Error(), "SIP server is unavailable") {
		t.Fatalf("inbound dialog BYE error = %v", err)
	}
}

func TestInboundDialogBYEValidatesDialogBeforeAllocatingCSeq(t *testing.T) {
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	callID := sip.CallID("inbound-bye-invalid-dialog")
	request := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetTo(local).SetContact(remote).SetMethod(sip.MethodInvite).
			SetSeqNo(1).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.10", Port: sip.NewPort(5060), Transport: "UDP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	request.SetSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060})
	request.SetDestination(&net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060})
	response := sip.NewResponseFromRequest("", request, 200, "OK", nil)
	dialog := &inboundInviteDialog{
		Established: true, LocalCSeq: 7, Request: request, Response: response,
	}
	api := &GB28181API{svr: &Server{fromAddress: *local}}

	err := api.sendInboundDialogBYEContext(t.Context(), dialog)
	if err == nil || !strings.Contains(err.Error(), "remote From") {
		t.Fatalf("invalid inbound dialog BYE error = %v", err)
	}
	if dialog.LocalCSeq != 7 {
		t.Fatalf("invalid inbound dialog BYE consumed CSeq: got %d, want 7", dialog.LocalCSeq)
	}
}

func TestInboundCascadeBYEIdentityFailureDoesNotConsumeCSeq(t *testing.T) {
	local := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.30:5060")
	remote.Params.Add("tag", sip.String{Str: "inbound-bye-identity-remote"})
	callID := sip.CallID("inbound-bye-invalid-identity")
	request := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetContact(remote).SetMethod(sip.MethodInvite).
			SetSeqNo(7).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Transport: "UDP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	request.SetSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060})
	request.SetDestination(&net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060})
	response := sip.NewResponseFromRequest("", request, 200, "OK", nil)
	server := &Server{fromAddress: *local}
	api := &GB28181API{svr: server}
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	t.Cleanup(worker.cancel)
	policy, err := newMonitorUserIdentityPolicy(testMonitorUserIdentityConfig())
	if err != nil {
		t.Fatal(err)
	}
	worker.platform.monitorUserIdentity = policy
	identityCtx := withMonitorUserIdentity(context.Background(), &monitorUserIdentity{
		Gateways:     []string{testTrustedGatewayID},
		UserID:       testRemoteUserID,
		Organization: "remoteorg",
		Category:     "dispatcher",
		Rank:         "level2",
	})
	dialog := &inboundInviteDialog{
		CallID: callIDFromRequest(request), DeviceID: gb10PlatformID, Established: true, LocalCSeq: 7,
		Request: request, Response: response,
		Cascade: &cascadeMediaSession{worker: worker, identityCtx: identityCtx},
	}

	err = api.sendInboundDialogBYEContext(t.Context(), dialog)
	if err == nil || !strings.Contains(err.Error(), "immediate gateway mismatch") {
		t.Fatalf("invalid cascade BYE identity error = %v", err)
	}
	if dialog.LocalCSeq != 7 {
		t.Fatalf("invalid cascade BYE identity consumed CSeq: got %d, want 7", dialog.LocalCSeq)
	}
}

func TestResolveCascadeChannelHonorsRequestCancellation(t *testing.T) {
	entered := make(chan struct{})
	channelStore := &blockingCascadeChannelStore{entered: entered}
	store := &cascadeContextStore{channel: channelStore}
	api := &GB28181API{core: ipc.NewAdapter(store, uniqueid.Core{})}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := api.resolveCascadeChannelContext(ctx, gb10ChannelID, "", cascadePlatform{})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cascade channel lookup did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cascade channel lookup error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade channel lookup did not stop after request cancellation")
	}
}

func TestCascadeMediaSessionContextStopsWithService(t *testing.T) {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		lifecycleDone:   make(chan struct{}),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
	}
	sessionCtx, cancel := api.newCascadeMediaSessionContext(nil, nil)
	defer cancel()
	api.beginClose()
	select {
	case <-sessionCtx.Done():
		if !errors.Is(sessionCtx.Err(), context.Canceled) {
			t.Fatalf("cascade media session context error = %v", sessionCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("cascade media session context did not stop with service")
	}
}

func TestCascadeMediaSessionContextStopsWithWorker(t *testing.T) {
	api := &GB28181API{}
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	sessionCtx, cancel := api.newCascadeMediaSessionContext(nil, worker)
	defer cancel()
	worker.cancel()
	select {
	case <-sessionCtx.Done():
		if !errors.Is(sessionCtx.Err(), context.Canceled) {
			t.Fatalf("cascade media session context error = %v", sessionCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("cascade media session context did not stop with worker")
	}
}

type contextAwareStopRTPMediaService struct {
	*fakeRTPMediaService
	stopContexts  []context.Context
	closeContexts []context.Context
	stopErr       error
	closeErr      error
}

func (f *contextAwareStopRTPMediaService) StopSendRTPContext(ctx context.Context, _ *sms.MediaServer, _ zlm.StopSendRTPRequest) (*zlm.StopSendRTPResponse, error) {
	f.stopContexts = append(f.stopContexts, ctx)
	return nil, f.stopErr
}

func (f *contextAwareStopRTPMediaService) CloseRTPServerContext(ctx context.Context, _ *sms.MediaServer, _ zlm.CloseRTPServerRequest) (*zlm.CloseRTPServerResponse, error) {
	f.closeContexts = append(f.closeContexts, ctx)
	return nil, f.closeErr
}

func TestStopCascadeMediaSessionUsesShutdownCleanupContext(t *testing.T) {
	type cleanupContextKey struct{}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithValue(context.Background(), cleanupContextKey{}, "shutdown"), time.Second)
	defer shutdownCancel()
	media := &contextAwareStopRTPMediaService{
		fakeRTPMediaService: &fakeRTPMediaService{},
		stopErr:             errors.New("simulated stop failure"),
	}
	source := &cascadeSourceRef{
		key: "cascade-stop-context", refs: 1, channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID},
		stopDone: make(chan struct{}),
	}
	api := &GB28181API{
		sms:                    media,
		lifecycleClosed:        true,
		shutdownPersistenceCtx: shutdownCtx,
		cascadeSources:         map[string]*cascadeSourceRef{source.key: source},
	}
	session := &cascadeMediaSession{
		server: &sms.MediaServer{}, source: source, vhost: "__defaultVhost__", app: "rtp",
		stream: "cascade-stream", ssrc: "0100000001",
	}

	api.stopCascadeMediaSession(session, false, false)
	if len(media.stopContexts) != 1 {
		t.Fatalf("StopSendRTPContext calls = %d, want 1", len(media.stopContexts))
	}
	if marker := media.stopContexts[0].Value(cleanupContextKey{}); marker != "shutdown" {
		t.Fatalf("cascade RTP stop context marker = %v", marker)
	}
	if _, ok := api.cascadeSources[source.key]; !ok {
		t.Fatal("failed RTP sender stop released cascade source ownership")
	}
	if current, ok := api.pendingCascadeMediaCleanups.Load(session); !ok || current != session {
		t.Fatal("failed RTP sender stop was not retained for retry")
	}
}

func TestCascadeStopDuringRTPStartRetainsSenderCleanup(t *testing.T) {
	startEntered := make(chan struct{})
	startRelease := make(chan struct{})
	stopErr := errors.New("simulated cascade RTP stop failure")
	media := &fakeRTPMediaService{
		startPort:    40000,
		startEntered: startEntered,
		startRelease: startRelease,
		stopErrs:     []error{stopErr, nil},
	}
	source := &cascadeSourceRef{
		key: "cascade-start-stop-race", refs: 1,
		channel:  &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID},
		stream:   &Streams{StreamID: "cascade-start-stop-race"},
		stopDone: make(chan struct{}),
	}
	api := &GB28181API{
		sms: media, cascadeSources: map[string]*cascadeSourceRef{source.key: source},
	}
	startCtx, cancelStart := context.WithCancel(t.Context())
	session := &cascadeMediaSession{
		source: source, server: &sms.MediaServer{}, cancel: cancelStart,
		vhost: cascadeSourceVHost, app: cascadeSourceApp,
		stream: source.stream.StreamID, ssrc: "0100000001",
	}
	startDone := make(chan error, 1)
	go func() {
		_, err := api.startCascadeSessionRTP(startCtx, session, GBVersion10, session.server,
			&cascadeVideoOffer{IsUDP: true}, zlm.StartSendRTPRequest{
				Vhost: session.vhost, App: session.app, Stream: session.stream, SSRC: session.ssrc,
				DstURL: "192.0.2.30", DstPort: 30000, IsUDP: true, Type: 1, PT: 96,
			})
		startDone <- err
	}()

	select {
	case <-startEntered:
	case <-time.After(time.Second):
		close(startRelease)
		t.Fatal("cascade RTP start did not enter media adapter")
	}
	api.stopCascadeMediaSession(session, false, false)
	media.mu.Lock()
	stopCalls := media.stopCalls
	media.mu.Unlock()
	if stopCalls != 0 {
		close(startRelease)
		t.Fatalf("RTP stop ran before start returned: calls=%d", stopCalls)
	}
	if current, ok := api.pendingCascadeMediaCleanups.Load(session); !ok || current != session {
		close(startRelease)
		t.Fatal("in-flight cascade RTP start lost terminal cleanup ownership")
	}
	if current := api.cascadeSources[source.key]; current != source || source.refs != 1 {
		close(startRelease)
		t.Fatal("in-flight cascade RTP start released its source early")
	}

	close(startRelease)
	select {
	case err := <-startDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stopped cascade RTP start error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stopped cascade RTP start did not return")
	}
	media.mu.Lock()
	stopCalls = media.stopCalls
	media.mu.Unlock()
	if stopCalls != 1 {
		t.Fatalf("failed cascade RTP cleanup calls = %d, want 1", stopCalls)
	}
	if current, ok := api.pendingCascadeMediaCleanups.Load(session); !ok || current != session {
		t.Fatal("failed cascade RTP cleanup lost retry ownership")
	}
	if current := api.cascadeSources[source.key]; current != source || source.refs != 1 {
		t.Fatal("failed cascade RTP cleanup released its source")
	}

	if pending := api.cleanupStoppedCascadeMediaSessions(); pending {
		t.Fatal("successful cascade RTP cleanup retry remained pending")
	}
	media.mu.Lock()
	stopCalls = media.stopCalls
	media.mu.Unlock()
	if stopCalls != 2 {
		t.Fatalf("cascade RTP cleanup retry calls = %d, want 2", stopCalls)
	}
	if _, ok := api.pendingCascadeMediaCleanups.Load(session); ok {
		t.Fatal("successful cascade RTP cleanup retry retained terminal session")
	}
	if _, ok := api.cascadeSources[source.key]; ok {
		t.Fatal("successful cascade RTP cleanup retry retained source")
	}
}

func TestCascadeSourceAcquiredAfterStopIsReleased(t *testing.T) {
	source := &cascadeSourceRef{
		key: "cascade-source-after-stop", refs: 1,
		stream:   &Streams{StreamID: "cascade-source-after-stop"},
		stopDone: make(chan struct{}),
	}
	api := &GB28181API{cascadeSources: map[string]*cascadeSourceRef{source.key: source}}
	session := &cascadeMediaSession{}

	api.stopCascadeMediaSession(session, false, false)
	if _, ok := api.pendingCascadeMediaCleanups.Load(session); ok {
		t.Fatal("empty stopped cascade session remained pending")
	}
	if api.attachCascadeSource(session, source, &sms.MediaServer{}) {
		t.Fatal("stopped cascade session accepted a late source")
	}
	if session.sourceSnapshot() != nil {
		t.Fatal("stopped cascade session retained a late source")
	}
	if _, ok := api.cascadeSources[source.key]; ok {
		t.Fatal("late cascade source was not released")
	}
	if source.refs != 0 {
		t.Fatalf("late cascade source refs = %d, want 0", source.refs)
	}
}

func TestReleaseCascadeSourceUsesShutdownContextForRTPClose(t *testing.T) {
	type cleanupContextKey struct{}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithValue(context.Background(), cleanupContextKey{}, "shutdown"), time.Second)
	defer shutdownCancel()
	media := &contextAwareStopRTPMediaService{
		fakeRTPMediaService: &fakeRTPMediaService{},
		closeErr:            errors.New("simulated close failure"),
	}
	server := &sms.MediaServer{}
	stream := &Streams{StreamID: "cascade-source-stream", mediaServer: server}
	source := &cascadeSourceRef{
		key: "cascade-close-context", refs: 1, owned: true, mediaStatusFinished: true,
		channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID},
		stream:  stream, server: server, stopDone: make(chan struct{}),
	}
	api := &GB28181API{
		sms:                    media,
		streams:                &conc.Map[string, *Streams]{},
		lifecycleClosed:        true,
		shutdownPersistenceCtx: shutdownCtx,
		cascadeSources:         map[string]*cascadeSourceRef{source.key: source},
	}
	api.streams.Store(source.key, stream)

	api.releaseCascadeSource(source, false)
	if len(media.closeContexts) != 1 {
		t.Fatalf("CloseRTPServerContext calls = %d, want 1", len(media.closeContexts))
	}
	if marker := media.closeContexts[0].Value(cleanupContextKey{}); marker != "shutdown" {
		t.Fatalf("cascade RTP close context marker = %v", marker)
	}
	if _, ok := api.cascadeSources[source.key]; ok {
		t.Fatal("failed RTP server close retained cascade source")
	}
	if current, ok := api.streams.Load(source.key); !ok || current != stream || !api.mediaStreamStopping(stream) {
		t.Fatal("failed RTP server close lost retryable stream ownership")
	}
}

type cascadeContextStore struct {
	ipc.Storer
	channel ipc.ChannelStorer
}

func (s *cascadeContextStore) Channel() ipc.ChannelStorer { return s.channel }

type blockingCascadeChannelStore struct {
	ipc.ChannelStorer
	entered chan struct{}
	once    sync.Once
}

func (s *blockingCascadeChannelStore) Get(ctx context.Context, _ *ipc.Channel, _ ...orm.QueryOption) error {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	return ctx.Err()
}
