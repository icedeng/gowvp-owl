package gbs

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestRuntimeCleanerRetriesStoppedPlayAfterCloseFailure(t *testing.T) {
	closeErr := errors.New("close Play receiver once")
	api, channel, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	media.closeErrs = []error{closeErr, nil}
	key := resolvePlaySessionKey(inputChannel.DeviceID, inputChannel.ChannelID, "")
	stream := &Streams{
		DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID,
		StreamID: "play-runtime-retry", mediaServer: mediaServer,
	}
	api.streams.Store(key, stream)

	err := api.stopPlayContext(t.Context(), channel, &StopPlayInput{Channel: inputChannel})
	if !errors.Is(err, closeErr) {
		t.Fatalf("first Play cleanup error = %v, want %v", err, closeErr)
	}
	if current, ok := api.streams.Load(key); !ok || current != stream || !api.mediaStreamStopping(stream) {
		t.Fatal("failed Play cleanup did not retain terminal ownership")
	}
	if api.hasActiveChannelStream(stream.DeviceID, stream.ChannelID) {
		t.Fatal("failed Play cleanup remained visible as active")
	}

	startVoiceRetryCleanerForTest(t, api)
	waitForVoiceCleanup(t, func() bool {
		_, exists := api.streams.Load(key)
		return !exists
	})
	media.mu.Lock()
	closeCalls := media.closeCalls
	media.mu.Unlock()
	if closeCalls != 2 {
		t.Fatalf("automatic Play cleanup calls = %d, want 2", closeCalls)
	}
}

func TestRuntimeCleanerRetriesStoppedHistoryAfterCloseFailure(t *testing.T) {
	closeErr := errors.New("close history receiver once")
	api, channel, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	media.closeErrs = []error{closeErr, nil}
	key := historyKey(historyModePlayback, inputChannel.DeviceID, inputChannel.ChannelID)
	stream := &Streams{
		T: 1, DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID,
		StreamID: "history-runtime-retry", mediaServer: mediaServer,
	}
	api.streams.Store(key, stream)

	err := api.stopHistoryNoLockContext(t.Context(), channel, &StopHistoryInput{Channel: inputChannel, Mode: historyModePlayback})
	if !errors.Is(err, closeErr) {
		t.Fatalf("first history cleanup error = %v, want %v", err, closeErr)
	}
	if current, ok := api.streams.Load(key); !ok || current != stream || !api.mediaStreamStopping(stream) {
		t.Fatal("failed history cleanup did not retain terminal ownership")
	}

	startVoiceRetryCleanerForTest(t, api)
	waitForVoiceCleanup(t, func() bool {
		_, exists := api.streams.Load(key)
		return !exists
	})
	media.mu.Lock()
	closeCalls := media.closeCalls
	media.mu.Unlock()
	if closeCalls != 2 {
		t.Fatalf("automatic history cleanup calls = %d, want 2", closeCalls)
	}
}

func TestMediaCleanupRetainsSSRCUntilExternalResourcesClose(t *testing.T) {
	closeErr := errors.New("close receiver once")
	media := &fakeRTPMediaService{closeErrs: []error{closeErr, nil}}
	streams := &conc.Map[string, *Streams]{}
	api := &GB28181API{sms: media, streams: streams}
	var allocator ssrcAllocator
	ssrc, release, err := allocator.reserve("3402000000", 0)
	if err != nil {
		t.Fatal(err)
	}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		StreamID: "ssrc-cleanup-order", mediaServer: &sms.MediaServer{},
	}
	if err := stream.bindSSRCReservation(ssrc, release); err != nil {
		t.Fatal(err)
	}
	key := resolvePlaySessionKey(stream.DeviceID, stream.ChannelID, "")
	streams.Store(key, stream)
	api.markMediaStreamStopped(stream, "test", true)

	complete, err := api.cleanupMediaStreamContext(t.Context(), key, stream)
	if !errors.Is(err, closeErr) || complete {
		t.Fatalf("first cleanup = complete:%v err:%v", complete, err)
	}
	allocator.mu.Lock()
	reservedAfterFailure := len(allocator.domains["3402000000"].inUse)
	allocator.mu.Unlock()
	if reservedAfterFailure != 1 {
		t.Fatalf("reserved SSRC count after failed cleanup = %d, want 1", reservedAfterFailure)
	}

	complete, err = api.cleanupMediaStreamContext(t.Context(), key, stream)
	if err != nil || !complete {
		t.Fatalf("second cleanup = complete:%v err:%v", complete, err)
	}
	allocator.mu.Lock()
	reservedAfterSuccess := len(allocator.domains["3402000000"].inUse)
	allocator.mu.Unlock()
	if reservedAfterSuccess != 0 {
		t.Fatalf("reserved SSRC count after successful cleanup = %d, want 0", reservedAfterSuccess)
	}
}

func TestRuntimeCleanerRetriesPlayStartupRollbackAfterCloseFailure(t *testing.T) {
	closeErr := errors.New("close failed Play startup receiver once")
	api, _, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	media.openPort = 30000
	media.closeErrs = []error{closeErr, nil}
	key := resolvePlaySessionKey(inputChannel.DeviceID, inputChannel.ChannelID, "")

	err := api.PlayContext(t.Context(), &PlayInput{Channel: inputChannel, SMS: mediaServer})
	if !errors.Is(err, closeErr) || !strings.Contains(err.Error(), "SIP server is unavailable") {
		t.Fatalf("Play startup rollback error = %v", err)
	}
	stream, ok := api.streams.Load(key)
	if !ok || stream == nil || !api.mediaStreamStopping(stream) {
		t.Fatal("failed Play startup rollback lost retryable stream ownership")
	}

	startVoiceRetryCleanerForTest(t, api)
	waitForVoiceCleanup(t, func() bool {
		_, exists := api.streams.Load(key)
		return !exists
	})
	media.mu.Lock()
	openCalls, closeCalls := media.openCalls, media.closeCalls
	media.mu.Unlock()
	if openCalls != 1 || closeCalls != 2 {
		t.Fatalf("Play startup RTP calls = open:%d close:%d, want 1/2", openCalls, closeCalls)
	}
}

func TestRuntimeCleanerRetriesHistoryStartupRollbackAfterCloseFailure(t *testing.T) {
	closeErr := errors.New("close failed history startup receiver once")
	api, _, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	media.openPort = 30000
	media.closeErrs = []error{closeErr, nil}
	key := historyKey(historyModePlayback, inputChannel.DeviceID, inputChannel.ChannelID)

	err := api.StartHistory(t.Context(), &HistoryInput{
		Channel: inputChannel, SMS: mediaServer, Mode: historyModePlayback,
		StartAt: time.Now().Add(-time.Hour), EndAt: time.Now(),
	})
	if !errors.Is(err, closeErr) || !strings.Contains(err.Error(), "SIP server is unavailable") {
		t.Fatalf("history startup rollback error = %v", err)
	}
	stream, ok := api.streams.Load(key)
	if !ok || stream == nil || !api.mediaStreamStopping(stream) {
		t.Fatal("failed history startup rollback lost retryable stream ownership")
	}

	startVoiceRetryCleanerForTest(t, api)
	waitForVoiceCleanup(t, func() bool {
		_, exists := api.streams.Load(key)
		return !exists
	})
	media.mu.Lock()
	openCalls, closeCalls := media.openCalls, media.closeCalls
	media.mu.Unlock()
	if openCalls != 1 || closeCalls != 2 {
		t.Fatalf("history startup RTP calls = open:%d close:%d, want 1/2", openCalls, closeCalls)
	}
}

func TestRuntimeCleanerRetriesDeviceAndMediaTermination(t *testing.T) {
	for _, tc := range []struct {
		name      string
		terminate func(*GB28181API, *Streams)
	}{
		{
			name: "device offline",
			terminate: func(api *GB28181API, stream *Streams) {
				api.terminateMediaSessions(t.Context(), stream.DeviceID, map[string]struct{}{stream.ChannelID: {}}, "device_offline")
			},
		},
		{
			name: "media unregister",
			terminate: func(api *GB28181API, stream *Streams) {
				if err := api.OnMediaStreamChanged(t.Context(), MediaStreamEvent{StreamID: stream.StreamID, Reason: "rtp_timeout"}); err != nil {
					t.Errorf("media unregister: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closeErr := errors.New("close ordinary media once")
			media := &fakeRTPMediaService{closeErrs: []error{closeErr, nil}}
			server := &sms.MediaServer{}
			streams := &conc.Map[string, *Streams]{}
			api := &GB28181API{sms: media, streams: streams}
			stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "ordinary-" + strings.ReplaceAll(tc.name, " ", "-"), mediaServer: server}
			key := resolvePlaySessionKey(stream.DeviceID, stream.ChannelID, "")
			streams.Store(key, stream)

			tc.terminate(api, stream)
			if current, ok := streams.Load(key); !ok || current != stream || !api.mediaStreamStopping(stream) {
				t.Fatal("single termination removed retryable ordinary media")
			}

			startVoiceRetryCleanerForTest(t, api)
			waitForVoiceCleanup(t, func() bool {
				_, exists := streams.Load(key)
				return !exists
			})
			media.mu.Lock()
			closeCalls := media.closeCalls
			media.mu.Unlock()
			if closeCalls != 2 {
				t.Fatalf("automatic ordinary media cleanup calls = %d, want 2", closeCalls)
			}
		})
	}
}

func TestRuntimeCleanerRetriesRemoteBYEMediaCleanup(t *testing.T) {
	closeErr := errors.New("close remote BYE receiver once")
	media := &fakeRTPMediaService{closeErrs: []error{closeErr, nil}}
	streams := &conc.Map[string, *Streams]{}
	api := &GB28181API{sms: media, streams: streams}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "remote-bye-runtime-retry",
		CallID: "remote-bye-runtime-retry", mediaServer: &sms.MediaServer{},
	}
	key := resolvePlaySessionKey(stream.DeviceID, stream.ChannelID, "")
	streams.Store(key, stream)

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodBYE, stream.CallID, nil, api.sipByeGeneric)
	assertFlowOK(t, response)
	if current, ok := streams.Load(key); !ok || current != stream || !api.mediaStreamStopping(stream) {
		t.Fatal("failed remote BYE cleanup did not retain terminal ownership")
	}

	startVoiceRetryCleanerForTest(t, api)
	waitForVoiceCleanup(t, func() bool {
		_, exists := streams.Load(key)
		return !exists
	})
	media.mu.Lock()
	closeCalls := media.closeCalls
	media.mu.Unlock()
	if closeCalls != 2 {
		t.Fatalf("automatic remote BYE cleanup calls = %d, want 2", closeCalls)
	}
}

func TestRuntimeCleanerRetriesCascadeMediaStatusDeviceBYE(t *testing.T) {
	fixture := newSignalDigestDialogFixture(t, "cascade-media-status-bye-retry")
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID) + ":cascade:retry"
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		StreamID: "cascade-media-status-bye-retry", CallID: "cascade-media-status-bye-retry",
		Resp: fixture.response,
	}
	fixture.api.streams.Store(key, stream)
	fixture.api.deferMediaStreamDialogCleanup(stream)
	fixture.api.markMediaStreamStopped(stream, "media_status", false)
	source := &cascadeSourceRef{
		key: key, refs: 1, owned: true, ended: true, mediaStatusFinished: true,
		channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID},
		stream:  stream, mode: historyModePlayback, stopDone: make(chan struct{}),
	}
	fixture.api.cascadeSources = map[string]*cascadeSourceRef{key: source}

	server := fixture.api.svr
	fixture.api.svr = nil
	fixture.api.releaseCascadeSource(source, false)
	if current, ok := fixture.api.streams.Load(key); !ok || current != stream || !fixture.api.mediaStreamStopping(stream) {
		t.Fatal("failed cascade device BYE lost retryable stream ownership")
	}
	if _, ok := fixture.api.cascadeSources[key]; ok {
		t.Fatal("released cascade source remained after transferring cleanup ownership")
	}

	fixture.api.svr = server
	startVoiceRetryCleanerForTest(t, fixture.api)
	waitForVoiceCleanup(t, func() bool {
		_, exists := fixture.api.streams.Load(key)
		return !exists
	})
	select {
	case payload := <-fixture.flow.writes:
		bye := string(payload)
		if !strings.HasPrefix(bye, "BYE ") || !strings.Contains(bye, "Call-ID: "+stream.CallID) {
			t.Fatalf("retried cascade MediaStatus device BYE:\n%s", bye)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime cleaner did not retry cascade MediaStatus device BYE")
	}
}

func TestRuntimeCleanerRetriesDetachedInboundDialogBYE(t *testing.T) {
	fixture := newSignalDigestDialogFixture(t, "detached-inbound-bye-retry")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	contact := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	remote.Params.Add("tag", sip.String{Str: "detached-inbound-remote"})
	callID := sip.CallID("detached-inbound-bye-retry")
	request := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetContact(contact).SetMethod(sip.MethodInvite).
			SetSeqNo(1).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.10", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	request.SetConnection(fixture.channel.device.conn)
	request.SetSource(fixture.flow.remote)
	request.SetDestination(fixture.flow.local)
	response := sip.NewResponseFromRequest("", request, 200, "OK", nil)
	dialog := &inboundInviteDialog{
		CallID: string(callID), DeviceID: gb10DeviceID, LocalCSeq: 1,
		Request: request, Response: response, Established: true,
	}

	server := fixture.api.svr
	fixture.api.svr = nil
	err := fixture.api.requestInboundDialogCleanup(t.Context(), dialog)
	if err == nil || !strings.Contains(err.Error(), "SIP server is unavailable") {
		t.Fatalf("first detached inbound BYE error = %v", err)
	}
	if current, ok := fixture.api.pendingInboundDialogCleanups.Load(dialog); !ok || current == nil {
		t.Fatal("failed detached inbound BYE lost retry ownership")
	}

	fixture.api.svr = server
	startVoiceRetryCleanerForTest(t, fixture.api)
	waitForVoiceCleanup(t, func() bool {
		_, exists := fixture.api.pendingInboundDialogCleanups.Load(dialog)
		return !exists
	})
	select {
	case payload := <-fixture.flow.writes:
		bye := string(payload)
		if !strings.HasPrefix(bye, "BYE ") || !strings.Contains(bye, "Call-ID: "+string(callID)) {
			t.Fatalf("retried detached inbound dialog BYE:\n%s", bye)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime cleaner did not retry detached inbound dialog BYE")
	}
}

func TestRuntimeCleanerRetriesCascadeRTPSenderStop(t *testing.T) {
	stopErr := errors.New("stop cascade RTP sender once")
	media := &fakeRTPMediaService{stopErrs: []error{stopErr, nil}}
	source := &cascadeSourceRef{
		key: "cascade-rtp-stop-retry", refs: 1,
		channel:  &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID},
		stopDone: make(chan struct{}),
	}
	api := &GB28181API{
		sms: media, cascadeSources: map[string]*cascadeSourceRef{source.key: source},
	}
	session := &cascadeMediaSession{
		server: &sms.MediaServer{}, source: source,
		vhost: cascadeSourceVHost, app: cascadeSourceApp,
		stream: "cascade-rtp-stop-retry", ssrc: "0100000001",
	}

	api.stopCascadeMediaSession(session, false, false)
	if current, ok := api.pendingCascadeMediaCleanups.Load(session); !ok || current != session {
		t.Fatal("failed cascade RTP sender stop lost retry ownership")
	}
	if current := api.cascadeSources[source.key]; current != source || source.refs != 1 {
		t.Fatalf("failed cascade RTP sender stop released source: current=%p refs=%d", current, source.refs)
	}

	startVoiceRetryCleanerForTest(t, api)
	waitForVoiceCleanup(t, func() bool {
		_, exists := api.pendingCascadeMediaCleanups.Load(session)
		return !exists
	})
	if _, ok := api.cascadeSources[source.key]; ok {
		t.Fatal("successful cascade RTP sender retry retained source")
	}
	media.mu.Lock()
	stopCalls := media.stopCalls
	media.mu.Unlock()
	if stopCalls != 2 {
		t.Fatalf("automatic cascade RTP sender stop calls = %d, want 2", stopCalls)
	}
	api.stopCascadeMediaSession(session, false, false)
	media.mu.Lock()
	stopCalls = media.stopCalls
	media.mu.Unlock()
	if stopCalls != 2 {
		t.Fatalf("completed cascade RTP sender stop retried again: calls=%d", stopCalls)
	}
}

func TestStartPlayBlocksReplacementWhileOldCleanupFails(t *testing.T) {
	closeErr := errors.New("old Play receiver remains active")
	api, _, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	media.closeErr = closeErr
	key := resolvePlaySessionKey(inputChannel.DeviceID, inputChannel.ChannelID, "")
	old := &Streams{
		DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID,
		StreamID: "old-play", mediaServer: mediaServer,
	}
	api.streams.Store(key, old)

	err := api.PlayContext(t.Context(), &PlayInput{Channel: inputChannel, SMS: mediaServer})
	if !errors.Is(err, closeErr) || !strings.Contains(err.Error(), "previous Play") {
		t.Fatalf("Play replacement error = %v", err)
	}
	if current, ok := api.streams.Load(key); !ok || current != old {
		t.Fatal("failed Play replacement displaced the old cleanup owner")
	}
	media.mu.Lock()
	openCalls := media.openCalls
	media.mu.Unlock()
	if openCalls != 0 {
		t.Fatalf("failed Play replacement opened %d RTP receivers", openCalls)
	}
}

func TestStartHistoryBlocksReplacementWhileOldCleanupFails(t *testing.T) {
	closeErr := errors.New("old history receiver remains active")
	api, _, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	media.closeErr = closeErr
	key := historyKey(historyModePlayback, inputChannel.DeviceID, inputChannel.ChannelID)
	old := &Streams{
		T: 1, DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID,
		StreamID: "old-history", mediaServer: mediaServer,
	}
	api.streams.Store(key, old)

	err := api.StartHistory(t.Context(), &HistoryInput{
		Channel: inputChannel, SMS: mediaServer, Mode: historyModePlayback,
		StartAt: time.Now().Add(-time.Hour), EndAt: time.Now(),
	})
	if !errors.Is(err, closeErr) || !strings.Contains(err.Error(), "previous history") {
		t.Fatalf("history replacement error = %v", err)
	}
	if current, ok := api.streams.Load(key); !ok || current != old {
		t.Fatal("failed history replacement displaced the old cleanup owner")
	}
	media.mu.Lock()
	openCalls := media.openCalls
	media.mu.Unlock()
	if openCalls != 0 {
		t.Fatalf("failed history replacement opened %d RTP receivers", openCalls)
	}
}

func TestMediaCleanupDoesNotDeleteNewGeneration(t *testing.T) {
	media := &blockingCloseRTPMediaService{
		fakeRTPMediaService: &fakeRTPMediaService{},
		started:             make(chan struct{}),
		release:             make(chan struct{}),
	}
	streams := &conc.Map[string, *Streams]{}
	api := &GB28181API{sms: media, streams: streams}
	key := resolvePlaySessionKey(gb10DeviceID, gb10ChannelID, "")
	old := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "old-generation", mediaServer: &sms.MediaServer{}}
	newGeneration := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "new-generation"}
	streams.Store(key, old)
	api.markMediaStreamStopped(old, "test", true)
	done := make(chan error, 1)
	go func() {
		_, err := api.cleanupMediaStreamContext(t.Context(), key, old)
		done <- err
	}()
	select {
	case <-media.started:
	case <-time.After(time.Second):
		t.Fatal("old generation cleanup did not reach media service")
	}
	streams.Store(key, newGeneration)
	close(media.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("old generation cleanup did not finish")
	}
	if current, ok := streams.Load(key); !ok || current != newGeneration {
		t.Fatal("old generation cleanup deleted the replacement stream")
	}
}

func TestMediaCleanupDoesNotRepeatCompletedRTPStep(t *testing.T) {
	media := &fakeRTPMediaService{}
	streams := &conc.Map[string, *Streams]{}
	api := &GB28181API{sms: media, streams: streams}
	key := resolvePlaySessionKey(gb10DeviceID, gb10ChannelID, "")
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "partial-cleanup",
		mediaServer: &sms.MediaServer{}, Resp: sip.NewResponse("", sip.DefaultSipVersion, 200, "OK", nil, nil),
	}
	streams.Store(key, stream)
	input := &StopPlayInput{Channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}}

	for attempt := 0; attempt < 2; attempt++ {
		err := api.stopPlayContext(t.Context(), nil, input)
		if err == nil || !strings.Contains(err.Error(), "Contact or To") {
			t.Fatalf("cleanup attempt %d error = %v", attempt+1, err)
		}
	}
	media.mu.Lock()
	closeCalls := media.closeCalls
	media.mu.Unlock()
	if closeCalls != 1 {
		t.Fatalf("completed RTP cleanup repeated %d times, want 1", closeCalls)
	}
}

func TestCloseRetriesOrdinaryMediaCleanupWithinShutdownWindow(t *testing.T) {
	closeErr := errors.New("close ordinary receiver once during shutdown")
	media := &fakeRTPMediaService{closeErrs: []error{closeErr, nil}}
	streams := &conc.Map[string, *Streams]{}
	api := &GB28181API{sms: media, streams: streams, lifecycleDone: make(chan struct{})}
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "ordinary-shutdown-retry", mediaServer: &sms.MediaServer{}}
	key := resolvePlaySessionKey(stream.DeviceID, stream.ChannelID, "")
	streams.Store(key, stream)

	api.close()
	media.mu.Lock()
	closeCalls := media.closeCalls
	media.mu.Unlock()
	if closeCalls != 2 {
		t.Fatalf("shutdown ordinary cleanup calls = %d, want 2", closeCalls)
	}
	if _, ok := streams.Load(key); ok {
		t.Fatal("shutdown retained ordinary media after successful retry")
	}
}
