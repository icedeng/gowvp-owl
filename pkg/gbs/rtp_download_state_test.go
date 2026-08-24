package gbs

import (
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
