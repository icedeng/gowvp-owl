package gbs

import (
	"fmt"
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
