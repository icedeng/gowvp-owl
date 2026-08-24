package gbs

import (
	"testing"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestTalkMediaLifecycleStartsAndStopsBidirectionalRTP(t *testing.T) {
	media := &fakeRTPMediaService{talkPort: 30000}
	api := &GB28181API{
		cfg: &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"}, sms: media,
		streams: &conc.Map[string, *Streams]{},
	}
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "device-audio", Status: 0}
	session := &talkSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, ReceiveStream: stream.StreamID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: &sms.MediaServer{Type: sms.ProtocolZLMediaKit}, Stream: stream,
		receiverOpened: true, ready: make(chan error, 1),
	}
	api.talkSessions.Store(stream.StreamID, session)
	api.streams.Store(voiceKey(voiceModeTalk, gb10DeviceID, gb10ChannelID), stream)

	if err := api.OnMediaStreamChanged(t.Context(), MediaStreamEvent{StreamID: stream.StreamID, Active: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-session.ready:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("Talk RTP start did not complete waiting session")
	}
	media.mu.Lock()
	talk := media.talkStarted
	media.mu.Unlock()
	if talk.RecvStreamID != stream.StreamID || talk.Stream != "microphone" || talk.PT != broadcastPCMAPayload || talk.Type != broadcastRTPTypeES || !talk.OnlyAudio {
		t.Fatalf("unexpected Talk RTP request: %+v", talk)
	}

	if err := api.OnMediaStreamChanged(t.Context(), MediaStreamEvent{StreamID: stream.StreamID, Active: false, Reason: "rtp_timeout"}); err != nil {
		t.Fatal(err)
	}
	media.mu.Lock()
	stopped := media.stopped
	closed := media.closed
	media.mu.Unlock()
	if stopped.SSRC == "" || stopped.Stream != "microphone" {
		t.Fatalf("unexpected Talk RTP stop: %+v", stopped)
	}
	if closed.StreamID != stream.StreamID {
		t.Fatalf("unexpected Talk receiver close: %+v", closed)
	}
	if _, ok := api.talkSessions.Load(stream.StreamID); ok {
		t.Fatal("Talk session was not removed")
	}
}
