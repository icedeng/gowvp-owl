package gbs

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestRTPDownloadProgressUsesFileSizeAndMediaBytes(t *testing.T) {
	media := &fakeRTPMediaService{mediaItems: []zlm.MediaItem{{TotalBytes: 500, BytesSpeed: 128}}}
	api := &GB28181API{sms: media}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "download-stream", CallID: "download-dialog",
		FileSize: 1000, FileSizeKnown: true, mediaServer: &sms.MediaServer{},
	}
	api.registerRTPDownload(stream)
	state, ok := api.RTPDownloadByChannel(gb10DeviceID, gb10ChannelID)
	if !ok {
		t.Fatal("RTP download state not found")
	}
	if state.Received != 500 || state.BytesSpeed != 128 || !state.ProgressKnown || !state.Approximate || state.ProgressPercent != 50 {
		t.Fatalf("unexpected RTP download state: %+v", state)
	}
}

func TestRemoteBYEFinishesOutboundRTPDownload(t *testing.T) {
	conn := newFlowConnection()
	media := &fakeRTPMediaService{mediaItems: []zlm.MediaItem{{TotalBytes: 1000}}}
	api := &GB28181API{
		cfg: &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"}, sms: media,
		streams: &conc.Map[string, *Streams]{},
	}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "download-stream", CallID: "remote-download-bye",
		FileSize: 1000, FileSizeKnown: true, mediaServer: &sms.MediaServer{},
	}
	key := historyKey(historyModeDownload, gb10DeviceID, gb10ChannelID)
	api.streams.Store(key, stream)
	api.registerRTPDownload(stream)

	response := runFlowHandler(t, conn, api, sip.MethodBYE, stream.CallID, nil, api.sipByeGeneric)
	assertFlowOK(t, response)
	if _, ok := api.streams.Load(key); ok {
		t.Fatal("remote BYE did not remove outbound stream")
	}
	state, ok := api.RTPDownloadByChannel(gb10DeviceID, gb10ChannelID)
	if !ok || state.Status != rtpDownloadCompleted || state.EndReason != "remote_bye" || state.CompletedAt.IsZero() {
		t.Fatalf("unexpected terminal state: %+v", state)
	}
	if time.Since(state.CompletedAt) > time.Second {
		t.Fatalf("unexpected completion time: %s", state.CompletedAt)
	}
	media.mu.Lock()
	closed := media.closed
	media.mu.Unlock()
	if closed.StreamID != stream.StreamID {
		t.Fatalf("RTP receiver was not closed: %+v", closed)
	}
}

func TestRemoteBYECannotStopAnotherDevicesOutboundSession(t *testing.T) {
	api := &GB28181API{streams: &conc.Map[string, *Streams]{}}
	conn := newFlowConnection()
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "cross-device-bye", StreamID: "cross-device-stream",
	}
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	api.streams.Store(key, stream)
	request := newFlowRequest(t, conn, sip.MethodBYE, stream.CallID, nil)
	api.sipByeGeneric(&sip.Context{
		Request: request, Tx: sip.NewTransaction("cross-device-bye-tx", conn),
		DeviceID: "34020000001320009999", Source: conn.remote,
	})
	select {
	case response := <-conn.writes:
		if !strings.Contains(string(response), "SIP/2.0 481") {
			t.Fatalf("cross-device BYE response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("cross-device BYE response timeout")
	}
	if current, ok := api.streams.Load(key); !ok || current != stream || stream.Stop {
		t.Fatal("cross-device BYE stopped another device's outbound session")
	}
}

func TestRemoteBYEAcknowledgesBeforeMediaCleanup(t *testing.T) {
	conn := newFlowConnection()
	media := &blockingCloseRTPMediaService{
		fakeRTPMediaService: &fakeRTPMediaService{},
		started:             make(chan struct{}),
		release:             make(chan struct{}),
	}
	api := &GB28181API{sms: media, streams: &conc.Map[string, *Streams]{}}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "slow-bye-stream", CallID: "slow-remote-bye",
		mediaServer: &sms.MediaServer{},
	}
	key := "play:" + gb10DeviceID + ":" + gb10ChannelID
	api.streams.Store(key, stream)
	request := newFlowRequest(t, conn, sip.MethodBYE, stream.CallID, nil)
	to := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.sipByeGeneric(&sip.Context{
			Request: request, Tx: sip.NewTransaction("slow-remote-bye-tx", conn),
			DeviceID: gb10DeviceID, Source: conn.remote, To: to,
		})
	}()
	release := func() {
		select {
		case <-media.release:
		default:
			close(media.release)
		}
	}
	defer release()

	select {
	case <-media.started:
	case <-time.After(time.Second):
		t.Fatal("remote BYE media cleanup was not reached")
	}
	select {
	case payload := <-conn.writes:
		if response := string(payload); !strings.Contains(response, "SIP/2.0 200 OK") {
			t.Fatalf("unexpected BYE response:\n%s", response)
		}
	default:
		release()
		<-done
		t.Fatal("media cleanup delayed BYE 200 OK")
	}
	if _, ok := api.streams.Load(key); ok {
		t.Fatal("remote BYE acknowledged before removing stream state")
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("remote BYE handler did not finish")
	}
}

func TestCleanupRTPDownloadsExpiresOnlyTerminalStates(t *testing.T) {
	now := time.Now()
	api := &GB28181API{}
	expiredKey := "cascade-download-expired"
	activeKey := "cascade-download-active"
	recentKey := historyKey(historyModeDownload, gb10DeviceID, gb10ChannelID)
	api.rtpDownloads.Store(expiredKey, testRTPDownloadSession(gb10DeviceID, gb10ChannelID, now.Add(-rtpDownloadTerminalTTL-time.Second)))
	api.rtpDownloads.Store(activeKey, testRTPDownloadSession(gb10DeviceID, gb10ChannelID, time.Time{}))
	api.rtpDownloads.Store(recentKey, testRTPDownloadSession(gb10DeviceID, gb10ChannelID, now.Add(-time.Minute)))
	api.rtpDownloads.Store("invalid", "unexpected")

	api.cleanupRTPDownloads(now)

	if _, ok := api.rtpDownloads.Load(expiredKey); ok {
		t.Fatal("expired RTP terminal state was retained")
	}
	if _, ok := api.rtpDownloads.Load(activeKey); !ok {
		t.Fatal("active RTP download was removed")
	}
	if _, ok := api.rtpDownloads.Load(recentKey); !ok {
		t.Fatal("recent channel RTP terminal state was removed")
	}
	if _, ok := api.rtpDownloads.Load("invalid"); ok {
		t.Fatal("invalid RTP download entry was retained")
	}
}

func TestCleanupRTPDownloadsBoundsIndependentSessionStates(t *testing.T) {
	now := time.Now()
	api := &GB28181API{}
	for i := 0; i < rtpDownloadMaxSessionTerminalStates+2; i++ {
		key := fmt.Sprintf("cascade-download-%04d", i)
		api.rtpDownloads.Store(key, testRTPDownloadSession(gb10DeviceID, gb10ChannelID, now.Add(time.Duration(i)*time.Nanosecond)))
	}
	activeKey := "cascade-download-active"
	api.rtpDownloads.Store(activeKey, testRTPDownloadSession(gb10DeviceID, gb10ChannelID, time.Time{}))

	api.cleanupRTPDownloads(now.Add(time.Second))

	terminalCount := 0
	api.rtpDownloads.Range(func(key, value any) bool {
		session, ok := value.(*rtpDownloadSession)
		if ok && !session.snapshot().CompletedAt.IsZero() {
			terminalCount++
		}
		return true
	})
	if terminalCount != rtpDownloadMaxSessionTerminalStates {
		t.Fatalf("RTP session terminal states = %d; want %d", terminalCount, rtpDownloadMaxSessionTerminalStates)
	}
	if _, ok := api.rtpDownloads.Load("cascade-download-0000"); ok {
		t.Fatal("oldest RTP session terminal state was retained")
	}
	if _, ok := api.rtpDownloads.Load(activeKey); !ok {
		t.Fatal("active RTP session was removed by capacity cleanup")
	}
}

func testRTPDownloadSession(deviceID, channelID string, completedAt time.Time) *rtpDownloadSession {
	state := RTPDownloadState{
		SessionID: "test-session", DeviceID: deviceID, ChannelID: channelID,
		Status: rtpDownloadReceiving, StartedAt: completedAt.Add(-time.Second), UpdatedAt: completedAt,
		CompletedAt: completedAt,
	}
	if completedAt.IsZero() {
		state.StartedAt = time.Now()
		state.UpdatedAt = state.StartedAt
	}
	return &rtpDownloadSession{state: state}
}
