package gbs

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/pkg/conc"
)

func startVoiceRetryCleanerForTest(t *testing.T, api *GB28181API) {
	t.Helper()
	api.lifecycleDone = make(chan struct{})
	done := make(chan struct{})
	go func() {
		api.runRuntimeStateCleaner(time.Hour, time.Millisecond)
		close(done)
	}()
	t.Cleanup(func() {
		select {
		case <-api.lifecycleDone:
		default:
			close(api.lifecycleDone)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("voice cleanup retry worker did not stop")
		}
	})
}

func waitForVoiceCleanup(t *testing.T, complete func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if complete() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("voice cleanup retry did not converge")
}

func TestStopTalkSessionRetriesFailedMediaCleanup(t *testing.T) {
	closeErr := errors.New("close Talk receiver once")
	api, _, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	media.closeErrs = []error{closeErr, nil}
	stream := &Streams{DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID, StreamID: inputChannel.ID}
	session := &talkSession{
		DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID, ReceiveStream: inputChannel.ID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: mediaServer, SSRC: "0100000001", Stream: stream, receiverOpened: true, rtpStarted: true,
		ready: make(chan error, 1),
	}
	api.talkSessions.Store(session.ReceiveStream, session)

	err := api.stopTalkSession(session, errors.New("stop requested"))
	if !errors.Is(err, closeErr) {
		t.Fatalf("first stop error = %v, want %v", err, closeErr)
	}
	if _, ok := api.talkSessions.Load(session.ReceiveStream); !ok {
		t.Fatal("failed Talk cleanup removed the retryable session")
	}
	session.mu.Lock()
	rtpStarted, receiverOpened := session.rtpStarted, session.receiverOpened
	session.mu.Unlock()
	if rtpStarted || !receiverOpened {
		t.Fatalf("partial Talk cleanup resource flags: rtp=%v receiver=%v, want false/true", rtpStarted, receiverOpened)
	}

	if err := api.stopTalkSession(session, nil); err != nil {
		t.Fatalf("retry Talk cleanup: %v", err)
	}
	if _, ok := api.talkSessions.Load(session.ReceiveStream); ok {
		t.Fatal("successful Talk cleanup retained the session")
	}
	if err := api.stopTalkSession(session, nil); err != nil {
		t.Fatalf("idempotent Talk cleanup: %v", err)
	}
	media.mu.Lock()
	stopCalls, closeCalls := media.stopCalls, media.closeCalls
	media.mu.Unlock()
	if stopCalls != 1 || closeCalls != 2 {
		t.Fatalf("Talk cleanup calls = stop:%d close:%d, want 1/2", stopCalls, closeCalls)
	}
}

func TestRuntimeCleanerRetriesTalkDialogCleanup(t *testing.T) {
	fixture := newSignalDigestDialogFixture(t, "talk-dialog-cleanup-retry")
	key := voiceKey(voiceModeTalk, gb10DeviceID, gb10ChannelID)
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		StreamID: "talk-dialog-cleanup-retry", Resp: fixture.response,
	}
	session := &talkSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		ReceiveStream: stream.StreamID, Stream: stream, ready: make(chan error, 1),
	}
	fixture.api.streams.Store(key, stream)
	fixture.api.talkSessions.Store(session.ReceiveStream, session)

	server := fixture.api.svr
	fixture.api.svr = nil
	err := fixture.api.stopTalkSession(session, errors.New("Talk startup rollback"))
	if err == nil || !strings.Contains(err.Error(), "SIP server is unavailable") {
		t.Fatalf("first Talk dialog cleanup error = %v", err)
	}
	if current, ok := fixture.api.streams.Load(key); !ok || current != stream || !fixture.api.mediaStreamStopping(stream) {
		t.Fatal("failed Talk dialog cleanup lost terminal stream ownership")
	}
	if current, ok := fixture.api.talkSessions.Load(session.ReceiveStream); !ok || current != session {
		t.Fatal("failed Talk dialog cleanup lost retryable session ownership")
	}

	fixture.api.svr = server
	startVoiceRetryCleanerForTest(t, fixture.api)
	waitForVoiceCleanup(t, func() bool {
		_, streamExists := fixture.api.streams.Load(key)
		_, sessionExists := fixture.api.talkSessions.Load(session.ReceiveStream)
		return !streamExists && !sessionExists
	})
	select {
	case payload := <-fixture.flow.writes:
		bye := string(payload)
		if !strings.HasPrefix(bye, "BYE ") || !strings.Contains(bye, "Call-ID: talk-dialog-cleanup-retry") {
			t.Fatalf("retried Talk dialog BYE:\n%s", bye)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime cleaner did not retry Talk dialog BYE")
	}
}

func TestTalkMediaDisconnectReturnsCleanupErrorAndRetainsOwnership(t *testing.T) {
	closeErr := errors.New("close Talk receiver after disconnect once")
	api, _, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	media.closeErrs = []error{closeErr, nil}
	key := voiceKey(voiceModeTalk, inputChannel.DeviceID, inputChannel.ChannelID)
	stream := &Streams{
		DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID,
		StreamID: inputChannel.ID,
	}
	session := &talkSession{
		DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID,
		ReceiveStream: inputChannel.ID, SMS: mediaServer, Stream: stream,
		receiverOpened: true, ready: make(chan error, 1),
	}
	api.streams.Store(key, stream)
	api.talkSessions.Store(session.ReceiveStream, session)

	err := api.OnMediaStreamChanged(t.Context(), MediaStreamEvent{StreamID: stream.StreamID, Reason: "rtp_timeout"})
	if !errors.Is(err, closeErr) {
		t.Fatalf("Talk media disconnect error = %v, want %v", err, closeErr)
	}
	if current, ok := api.streams.Load(key); !ok || current != stream || !api.mediaStreamStopping(stream) {
		t.Fatal("failed Talk media disconnect cleanup lost terminal stream ownership")
	}
	if current, ok := api.talkSessions.Load(session.ReceiveStream); !ok || current != session {
		t.Fatal("failed Talk media disconnect cleanup lost session ownership")
	}

	startVoiceRetryCleanerForTest(t, api)
	waitForVoiceCleanup(t, func() bool {
		_, streamExists := api.streams.Load(key)
		_, sessionExists := api.talkSessions.Load(session.ReceiveStream)
		return !streamExists && !sessionExists
	})
}

func TestTalkStopDuringRTPStartRetainsFailedSenderCleanup(t *testing.T) {
	stopErr := errors.New("stop Talk sender after concurrent start once")
	api, _, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	releaseStart := make(chan struct{})
	media.talkPort = 10000
	media.talkEntered = make(chan struct{})
	media.talkRelease = releaseStart
	media.stopErrs = []error{stopErr, nil}
	key := voiceKey(voiceModeTalk, inputChannel.DeviceID, inputChannel.ChannelID)
	stream := &Streams{
		DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID,
		StreamID: inputChannel.ID,
	}
	session := &talkSession{
		DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID,
		ReceiveStream: inputChannel.ID, SMS: mediaServer, Stream: stream,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		ready: make(chan error, 1),
	}
	api.streams.Store(key, stream)
	api.talkSessions.Store(session.ReceiveStream, session)

	startResult := make(chan error, 1)
	go func() { startResult <- api.startTalkRTP(session.ReceiveStream) }()
	select {
	case <-media.talkEntered:
	case <-time.After(time.Second):
		t.Fatal("Talk RTP start did not reach media service")
	}
	if err := api.stopTalkSession(session, errors.New("Talk stopped while RTP start was pending")); err != nil {
		t.Fatalf("stop while Talk RTP start is pending: %v", err)
	}
	if current, ok := api.talkSessions.Load(session.ReceiveStream); !ok || current != session {
		t.Fatal("pending Talk RTP start lost session ownership")
	}
	if current, ok := api.streams.Load(key); !ok || current != stream {
		t.Fatal("pending Talk RTP start lost stream ownership")
	}

	close(releaseStart)
	if err := <-startResult; !errors.Is(err, stopErr) {
		t.Fatalf("concurrent Talk RTP start cleanup error = %v, want %v", err, stopErr)
	}
	if current, ok := api.talkSessions.Load(session.ReceiveStream); !ok || current != session {
		t.Fatal("failed concurrent Talk sender cleanup lost session ownership")
	}
	if current, ok := api.streams.Load(key); !ok || current != stream {
		t.Fatal("failed concurrent Talk sender cleanup lost stream ownership")
	}

	startVoiceRetryCleanerForTest(t, api)
	waitForVoiceCleanup(t, func() bool {
		_, sessionExists := api.talkSessions.Load(session.ReceiveStream)
		_, streamExists := api.streams.Load(key)
		return !sessionExists && !streamExists
	})
	media.mu.Lock()
	stopCalls := media.stopCalls
	media.mu.Unlock()
	if stopCalls != 2 {
		t.Fatalf("concurrent Talk sender cleanup calls = %d, want 2", stopCalls)
	}
}

func TestStopVoiceRetainsTalkStreamUntilCleanupSucceeds(t *testing.T) {
	stopErr := errors.New("stop Talk RTP once")
	api, channel, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	media.stopErrs = []error{stopErr, nil}
	key := voiceKey(voiceModeTalk, inputChannel.DeviceID, inputChannel.ChannelID)
	stream := &Streams{DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID, StreamID: inputChannel.ID}
	session := &talkSession{
		DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID, ReceiveStream: inputChannel.ID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: mediaServer, SSRC: "0100000002", Stream: stream, rtpStarted: true, ready: make(chan error, 1),
	}
	api.streams.Store(key, stream)
	api.talkSessions.Store(session.ReceiveStream, session)

	err := api.stopVoiceNoLock(t.Context(), channel, &StopVoiceInput{Channel: inputChannel, Mode: voiceModeTalk})
	if !errors.Is(err, stopErr) {
		t.Fatalf("first StopVoice error = %v, want %v", err, stopErr)
	}
	if current, ok := api.streams.Load(key); !ok || current != stream {
		t.Fatal("failed StopVoice removed the Talk stream index")
	}
	if err := api.stopVoiceNoLock(t.Context(), channel, &StopVoiceInput{Channel: inputChannel, Mode: voiceModeTalk}); err != nil {
		t.Fatalf("retry StopVoice: %v", err)
	}
	if _, ok := api.streams.Load(key); ok {
		t.Fatal("successful StopVoice retained the Talk stream index")
	}
}

func TestStopPlayRetriesFailedRTPServerClose(t *testing.T) {
	closeErr := errors.New("close Play receiver once")
	api, channel, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	media.closeErrs = []error{closeErr, nil}
	key := standardTalkPlayKey(inputChannel.DeviceID, inputChannel.ChannelID)
	stream := &Streams{
		DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID,
		StreamID: standardTalkStreamID(inputChannel), mediaServer: mediaServer,
	}
	api.streams.Store(key, stream)

	err := api.stopPlayContext(t.Context(), channel, &StopPlayInput{Channel: inputChannel, sessionKey: key})
	if !errors.Is(err, closeErr) {
		t.Fatalf("first StopPlay error = %v, want %v", err, closeErr)
	}
	if current, ok := api.streams.Load(key); !ok || current != stream {
		t.Fatal("failed StopPlay removed the retryable stream")
	}
	if err := api.stopPlayContext(t.Context(), channel, &StopPlayInput{Channel: inputChannel, sessionKey: key}); err != nil {
		t.Fatalf("retry StopPlay: %v", err)
	}
	if _, ok := api.streams.Load(key); ok {
		t.Fatal("successful StopPlay retained the stream")
	}
	if err := api.stopPlayContext(t.Context(), channel, &StopPlayInput{Channel: inputChannel, sessionKey: key}); err != nil {
		t.Fatalf("idempotent StopPlay: %v", err)
	}
	media.mu.Lock()
	closeCalls := media.closeCalls
	media.mu.Unlock()
	if closeCalls != 2 {
		t.Fatalf("StopPlay close calls = %d, want 2", closeCalls)
	}
}

func TestStopBroadcastSessionRetriesFailedRTPStop(t *testing.T) {
	stopErr := errors.New("stop Broadcast RTP once")
	api, _, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	media.stopErrs = []error{stopErr, nil}
	stream := &Streams{DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID, StreamID: "voice"}
	session := &broadcastSession{
		DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: mediaServer, SSRC: "0100000003", Stream: stream, rtpStarted: true, ready: make(chan error, 1),
	}
	key := voiceKey(voiceModeBroadcast, session.DeviceID, session.ChannelID)
	api.broadcastSessions.Store(session.ChannelID, session)
	api.streams.Store(key, stream)

	err := api.stopBroadcastSession(session, false)
	if !errors.Is(err, stopErr) {
		t.Fatalf("first Broadcast stop error = %v, want %v", err, stopErr)
	}
	if current, ok := api.broadcastSessions.Load(session.ChannelID); !ok || current != session {
		t.Fatal("failed Broadcast cleanup removed the session")
	}
	if current, ok := api.streams.Load(key); !ok || current != stream {
		t.Fatal("failed Broadcast cleanup removed the stream")
	}
	session.mu.Lock()
	rtpStarted := session.rtpStarted
	session.mu.Unlock()
	if !rtpStarted {
		t.Fatal("failed Broadcast cleanup cleared the RTP resource flag")
	}

	if err := api.stopBroadcastSession(session, false); err != nil {
		t.Fatalf("retry Broadcast cleanup: %v", err)
	}
	if _, ok := api.broadcastSessions.Load(session.ChannelID); ok {
		t.Fatal("successful Broadcast cleanup retained the session")
	}
	if _, ok := api.streams.Load(key); ok {
		t.Fatal("successful Broadcast cleanup retained the stream")
	}
	if err := api.stopBroadcastSession(session, false); err != nil {
		t.Fatalf("idempotent Broadcast cleanup: %v", err)
	}
	media.mu.Lock()
	stopCalls := media.stopCalls
	media.mu.Unlock()
	if stopCalls != 2 {
		t.Fatalf("Broadcast stop calls = %d, want 2", stopCalls)
	}
}

func TestStartTalkBlocksReplacementWhenOldCleanupFails(t *testing.T) {
	stopErr := errors.New("old Talk RTP remains active")
	api, channel, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	media.stopErr = stopErr
	key := voiceKey(voiceModeTalk, inputChannel.DeviceID, inputChannel.ChannelID)
	oldStream := &Streams{DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID, StreamID: inputChannel.ID}
	old := &talkSession{
		DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID, ReceiveStream: inputChannel.ID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "old-microphone",
		SMS: mediaServer, SSRC: "0100000004", Stream: oldStream, rtpStarted: true, ready: make(chan error, 1),
	}
	api.streams.Store(key, oldStream)
	api.talkSessions.Store(old.ReceiveStream, old)

	err := api.startTalk(t.Context(), channel, &VoiceInput{
		Channel: inputChannel, SMS: mediaServer, Mode: voiceModeTalk, SourceStream: "new-microphone",
	})
	if !errors.Is(err, stopErr) || !strings.Contains(err.Error(), "previous Talk") {
		t.Fatalf("start Talk replacement error = %v", err)
	}
	if current, ok := api.talkSessions.Load(old.ReceiveStream); !ok || current != old {
		t.Fatal("failed Talk replacement displaced the old session")
	}
	if current, ok := api.streams.Load(key); !ok || current != oldStream {
		t.Fatal("failed Talk replacement displaced the old stream")
	}
	media.mu.Lock()
	openCalls := media.openCalls
	media.mu.Unlock()
	if openCalls != 0 {
		t.Fatalf("failed Talk replacement opened %d new RTP receivers", openCalls)
	}
}

func TestStartBroadcastBlocksReplacementWhenOldCleanupFails(t *testing.T) {
	stopErr := errors.New("old Broadcast RTP remains active")
	api, channel, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	media.stopErr = stopErr
	oldStream := &Streams{DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID, StreamID: "old-voice"}
	old := &broadcastSession{
		DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "old-microphone",
		SMS: mediaServer, SSRC: "0100000005", Stream: oldStream, rtpStarted: true, ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(old.ChannelID, old)
	api.streams.Store(voiceKey(voiceModeBroadcast, old.DeviceID, old.ChannelID), oldStream)

	err := api.startBroadcast(t.Context(), channel, &VoiceInput{
		Channel: inputChannel, SMS: mediaServer, Mode: voiceModeBroadcast, SourceStream: "new-microphone",
	})
	if !errors.Is(err, stopErr) || !strings.Contains(err.Error(), "previous Broadcast") {
		t.Fatalf("start Broadcast replacement error = %v", err)
	}
	if current, ok := api.broadcastSessions.Load(old.ChannelID); !ok || current != old {
		t.Fatal("failed Broadcast replacement displaced the old session")
	}
	if current, ok := api.streams.Load(voiceKey(voiceModeBroadcast, old.DeviceID, old.ChannelID)); !ok || current != oldStream {
		t.Fatal("failed Broadcast replacement displaced the old stream")
	}
}

func TestStopCascadeVoiceSourceRetriesFailedRTPServerClose(t *testing.T) {
	closeErr := errors.New("close cascade voice receiver once")
	media := &fakeRTPMediaService{closeErrs: []error{closeErr, nil}}
	source := &cascadeVoiceSourceSession{
		server: &sms.MediaServer{}, streamID: "cascade-voice-retry", callID: "cascade-voice-retry",
		opened: true, done: make(chan struct{}),
	}
	api := &GB28181API{sms: media}
	api.cascadeVoiceDialogs.Store(source.callID, source)

	err := api.stopCascadeVoiceSource(source, false)
	if !errors.Is(err, closeErr) {
		t.Fatalf("first cascade voice cleanup error = %v, want %v", err, closeErr)
	}
	if current, ok := api.cascadeVoiceDialogs.Load(source.callID); !ok || current != source {
		t.Fatal("failed cascade voice cleanup removed the session")
	}
	source.mu.Lock()
	opened := source.opened
	source.mu.Unlock()
	if !opened {
		t.Fatal("failed cascade voice cleanup cleared the receiver flag")
	}

	if err := api.stopCascadeVoiceSource(source, false); err != nil {
		t.Fatalf("retry cascade voice cleanup: %v", err)
	}
	if _, ok := api.cascadeVoiceDialogs.Load(source.callID); ok {
		t.Fatal("successful cascade voice cleanup retained the session")
	}
	if err := api.stopCascadeVoiceSource(source, false); err != nil {
		t.Fatalf("idempotent cascade voice cleanup: %v", err)
	}
	media.mu.Lock()
	closeCalls := media.closeCalls
	media.mu.Unlock()
	if closeCalls != 2 {
		t.Fatalf("cascade voice close calls = %d, want 2", closeCalls)
	}
}

func TestRuntimeCleanerRetriesTalkAfterSingleDeviceTermination(t *testing.T) {
	stopErr := errors.New("stop Talk RTP once")
	api, _, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	media.stopErrs = []error{stopErr, nil}
	key := voiceKey(voiceModeTalk, inputChannel.DeviceID, inputChannel.ChannelID)
	stream := &Streams{DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID, StreamID: inputChannel.ID}
	session := &talkSession{
		DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID, ReceiveStream: inputChannel.ID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: mediaServer, SSRC: "0100000006", Stream: stream, rtpStarted: true, ready: make(chan error, 1),
	}
	api.streams.Store(key, stream)
	api.talkSessions.Store(session.ReceiveStream, session)

	api.terminateMediaSessions(t.Context(), inputChannel.DeviceID, map[string]struct{}{inputChannel.ChannelID: {}}, "device_offline")
	if current, ok := api.talkSessions.Load(session.ReceiveStream); !ok || current != session {
		t.Fatal("single device termination removed retryable Talk session")
	}
	if current, ok := api.streams.Load(key); !ok || current != stream {
		t.Fatal("single device termination removed retryable Talk stream")
	}

	startVoiceRetryCleanerForTest(t, api)
	waitForVoiceCleanup(t, func() bool {
		_, sessionExists := api.talkSessions.Load(session.ReceiveStream)
		_, streamExists := api.streams.Load(key)
		return !sessionExists && !streamExists
	})
	media.mu.Lock()
	stopCalls := media.stopCalls
	media.mu.Unlock()
	if stopCalls != 2 {
		t.Fatalf("automatic Talk cleanup calls = %d, want 2", stopCalls)
	}
}

func TestRuntimeCleanerRetriesBroadcastWithoutDroppingDialog(t *testing.T) {
	stopErr := errors.New("stop Broadcast RTP once")
	api, _, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	media.stopErrs = []error{stopErr, nil}
	key := voiceKey(voiceModeBroadcast, inputChannel.DeviceID, inputChannel.ChannelID)
	stream := &Streams{DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID, StreamID: "voice-retry"}
	dialog := &inboundInviteDialog{CallID: "broadcast-cleanup-retry", DeviceID: inputChannel.DeviceID}
	session := &broadcastSession{
		DeviceID: inputChannel.DeviceID, ChannelID: inputChannel.ChannelID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: mediaServer, SSRC: "0100000007", Stream: stream, Dialog: dialog, rtpStarted: true, ready: make(chan error, 1),
	}
	dialog.Broadcast = session
	api.streams.Store(key, stream)
	api.broadcastSessions.Store(session.ChannelID, session)
	api.inviteDialogs.Store(dialog.CallID, dialog)

	api.terminateMediaSessions(t.Context(), inputChannel.DeviceID, map[string]struct{}{inputChannel.ChannelID: {}}, "device_offline")
	if current, ok := api.broadcastSessions.Load(session.ChannelID); !ok || current != session {
		t.Fatal("single device termination removed retryable Broadcast session")
	}
	if current, ok := api.streams.Load(key); !ok || current != stream {
		t.Fatal("single device termination removed retryable Broadcast stream")
	}
	if current, ok := api.inviteDialogs.Load(dialog.CallID); !ok || current != dialog {
		t.Fatal("single device termination removed retryable Broadcast dialog")
	}

	startVoiceRetryCleanerForTest(t, api)
	waitForVoiceCleanup(t, func() bool {
		_, sessionExists := api.broadcastSessions.Load(session.ChannelID)
		_, streamExists := api.streams.Load(key)
		_, dialogExists := api.inviteDialogs.Load(dialog.CallID)
		return !sessionExists && !streamExists && !dialogExists
	})
	media.mu.Lock()
	stopCalls := media.stopCalls
	media.mu.Unlock()
	if stopCalls != 2 {
		t.Fatalf("automatic Broadcast cleanup calls = %d, want 2", stopCalls)
	}
}

func TestRuntimeCleanerRetriesCascadeVoiceAfterSingleTermination(t *testing.T) {
	closeErr := errors.New("close cascade voice receiver once")
	media := &fakeRTPMediaService{closeErrs: []error{closeErr, nil}}
	source := &cascadeVoiceSourceSession{
		server: &sms.MediaServer{}, streamID: "cascade-voice-worker-retry", callID: "cascade-voice-worker-retry",
		opened: true, done: make(chan struct{}),
	}
	api := &GB28181API{sms: media, streams: &conc.Map[string, *Streams]{}}
	api.cascadeVoiceDialogs.Store(source.callID, source)

	api.terminateCascadeVoiceSource(source.streamID)
	if current, ok := api.cascadeVoiceDialogs.Load(source.callID); !ok || current != source {
		t.Fatal("single cascade termination removed retryable source")
	}

	startVoiceRetryCleanerForTest(t, api)
	waitForVoiceCleanup(t, func() bool {
		_, exists := api.cascadeVoiceDialogs.Load(source.callID)
		return !exists
	})
	media.mu.Lock()
	closeCalls := media.closeCalls
	media.mu.Unlock()
	if closeCalls != 2 {
		t.Fatalf("automatic cascade voice cleanup calls = %d, want 2", closeCalls)
	}
}

func TestCloseRetriesVoiceCleanupWithinShutdownWindow(t *testing.T) {
	closeErr := errors.New("close Talk receiver once during shutdown")
	media := &fakeRTPMediaService{closeErrs: []error{closeErr, nil}}
	mediaServer := &sms.MediaServer{}
	streams := &conc.Map[string, *Streams]{}
	api := &GB28181API{sms: media, streams: streams, lifecycleDone: make(chan struct{})}
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "talk-shutdown-retry"}
	session := &talkSession{
		DeviceID: stream.DeviceID, ChannelID: stream.ChannelID, ReceiveStream: stream.StreamID,
		SMS: mediaServer, Stream: stream, receiverOpened: true, ready: make(chan error, 1),
	}
	api.talkSessions.Store(session.ReceiveStream, session)
	streams.Store(voiceKey(voiceModeTalk, session.DeviceID, session.ChannelID), stream)

	api.close()
	media.mu.Lock()
	closeCalls := media.closeCalls
	media.mu.Unlock()
	if closeCalls != 2 {
		t.Fatalf("shutdown Talk cleanup calls = %d, want 2", closeCalls)
	}
	if _, ok := api.talkSessions.Load(session.ReceiveStream); ok {
		t.Fatal("shutdown retained Talk session after successful retry")
	}
	if _, ok := streams.Load(voiceKey(voiceModeTalk, session.DeviceID, session.ChannelID)); ok {
		t.Fatal("shutdown retained Talk stream after successful retry")
	}
}
