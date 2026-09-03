package gbs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/pkg/conc"
)

func newStandardTalkTestAPI(t *testing.T) (*GB28181API, *Channel, *ipc.Channel, *fakeRTPMediaService, *sms.MediaServer) {
	t.Helper()
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion20)
	channel := &Channel{ChannelID: gb10ChannelID, device: memory.runtime}
	memory.runtime.Channels.Store(gb10ChannelID, channel)
	media := &fakeRTPMediaService{}
	mediaServer := &sms.MediaServer{ID: "local", Type: sms.ProtocolZLMediaKit, SDPIP: "192.0.2.20"}
	api := &GB28181API{
		cfg: &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"}, sms: media,
		streams: &conc.Map[string, *Streams]{},
	}
	api.svr = &Server{gb: api, memoryStorer: memory}
	inputChannel := &ipc.Channel{ID: "channel-internal", DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}
	return api, channel, inputChannel, media, mediaServer
}

func TestStandardTalkSecondFlowFailureCleansUpPlay(t *testing.T) {
	api, channel, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	in := &VoiceInput{
		Channel: inputChannel, SMS: mediaServer, StreamMode: 1, Mode: voiceModeTalkStandard,
		SourceStream: "microphone",
	}
	var playInput *PlayInput
	err := api.startStandardTalkWith(t.Context(), channel, in,
		func(_ context.Context, _ *Channel, play *PlayInput) error {
			playInput = play
			api.streams.Store(play.sessionKey, &Streams{
				DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: play.streamID,
				mediaServer: mediaServer, sessionKey: play.sessionKey,
			})
			return nil
		},
		func(context.Context, *Channel, *VoiceInput) error {
			return errors.New("broadcast failed")
		},
	)
	if err == nil || !strings.Contains(err.Error(), "broadcast failed") {
		t.Fatalf("error = %v", err)
	}
	if playInput == nil || playInput.sessionKey != standardTalkPlayKey(gb10DeviceID, gb10ChannelID) || playInput.streamID != "channel-internal-talk-upstream" || !playInput.audioOnly {
		t.Fatalf("play input = %+v", playInput)
	}
	if _, ok := api.streams.Load(playInput.sessionKey); ok {
		t.Fatal("failed standard Talk left the upstream Play session")
	}
	media.mu.Lock()
	closed := media.closed
	media.mu.Unlock()
	if closed.StreamID != playInput.streamID {
		t.Fatalf("closed RTP server = %+v", closed)
	}
}

func TestStandardTalkStopCleansBothIndependentFlows(t *testing.T) {
	api, channel, inputChannel, media, mediaServer := newStandardTalkTestAPI(t)
	in := &VoiceInput{
		Channel: inputChannel, SMS: mediaServer, StreamMode: 1, Mode: voiceModeTalkStandard,
		SourceStream: "microphone",
	}
	var session *broadcastSession
	err := api.startStandardTalkWith(t.Context(), channel, in,
		func(_ context.Context, _ *Channel, play *PlayInput) error {
			api.streams.Store(play.sessionKey, &Streams{
				DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: play.streamID,
				mediaServer: mediaServer, sessionKey: play.sessionKey,
			})
			return nil
		},
		func(_ context.Context, _ *Channel, voice *VoiceInput) error {
			var err error
			session, err = api.newBroadcastSession(voice)
			if err != nil {
				return err
			}
			api.broadcastSessions.Store(session.ChannelID, session)
			api.streams.Store(voiceKey(voiceModeBroadcast, session.DeviceID, session.ChannelID), session.Stream)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil || session.StandardTalkPlayKey != standardTalkPlayKey(gb10DeviceID, gb10ChannelID) {
		t.Fatalf("standard Talk broadcast session = %+v", session)
	}
	if err := api.stopVoiceNoLock(t.Context(), channel, &StopVoiceInput{Channel: inputChannel, Mode: voiceModeTalkStandard}); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.broadcastSessions.Load(gb10ChannelID); ok {
		t.Fatal("standard Talk left the Broadcast session")
	}
	if _, ok := api.streams.Load(standardTalkPlayKey(gb10DeviceID, gb10ChannelID)); ok {
		t.Fatal("standard Talk left the upstream Play session")
	}
	media.mu.Lock()
	closed := media.closed
	media.mu.Unlock()
	if closed.StreamID != standardTalkStreamID(inputChannel) {
		t.Fatalf("closed RTP server = %+v", closed)
	}
}

func TestRestartStandardTalkStopsOldPairBeforePublishingNewPlay(t *testing.T) {
	api, channel, inputChannel, _, mediaServer := newStandardTalkTestAPI(t)
	playKey := standardTalkPlayKey(gb10DeviceID, gb10ChannelID)
	oldPlay := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		StreamID: standardTalkStreamID(inputChannel), mediaServer: mediaServer, sessionKey: playKey,
	}
	api.streams.Store(playKey, oldPlay)
	oldBroadcast := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SMS: mediaServer,
		Stream:              &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID},
		StandardTalkPlayKey: playKey, ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(gb10ChannelID, oldBroadcast)

	newPlay := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		StreamID: standardTalkStreamID(inputChannel), mediaServer: mediaServer, sessionKey: playKey,
	}
	var newBroadcast *broadcastSession
	err := api.startStandardTalkWith(t.Context(), channel, &VoiceInput{
		Channel: inputChannel, SMS: mediaServer, StreamMode: 1, Mode: voiceModeTalkStandard,
		SourceStream: "microphone",
	}, func(_ context.Context, _ *Channel, play *PlayInput) error {
		if _, exists := api.broadcastSessions.Load(gb10ChannelID); exists {
			t.Fatal("new Play started before the old standard Talk pair was removed")
		}
		if current, exists := api.streams.Load(playKey); exists || current != nil {
			t.Fatal("new Play started while the old Play was still indexed")
		}
		api.streams.Store(play.sessionKey, newPlay)
		return nil
	}, func(_ context.Context, _ *Channel, voice *VoiceInput) error {
		var createErr error
		newBroadcast, createErr = api.newBroadcastSession(voice)
		if createErr != nil {
			return createErr
		}
		api.broadcastSessions.Store(newBroadcast.ChannelID, newBroadcast)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if current, ok := api.streams.Load(playKey); !ok || current != newPlay {
		t.Fatalf("restarted standard Talk Play = %p, %v; want %p", current, ok, newPlay)
	}
	if current, ok := api.broadcastSessions.Load(gb10ChannelID); !ok || current != newBroadcast {
		t.Fatal("restarted standard Talk did not retain its new Broadcast half")
	}
}

func TestStoppingStandardTalkPlayAlsoStopsBroadcast(t *testing.T) {
	api, channel, inputChannel, _, mediaServer := newStandardTalkTestAPI(t)
	playKey := standardTalkPlayKey(gb10DeviceID, gb10ChannelID)
	api.streams.Store(playKey, &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: standardTalkStreamID(inputChannel),
		mediaServer: mediaServer, sessionKey: playKey,
	})
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SMS: mediaServer,
		Stream:              &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID},
		StandardTalkPlayKey: playKey, ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(gb10ChannelID, session)
	if err := api.stopPlayContext(t.Context(), channel, &StopPlayInput{Channel: inputChannel, sessionKey: playKey}); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.broadcastSessions.Load(gb10ChannelID); ok {
		t.Fatal("stopping standard Talk Play did not stop Broadcast")
	}
}

func TestStandardTalkUpstreamMediaLossStopsBroadcast(t *testing.T) {
	api, _, inputChannel, _, mediaServer := newStandardTalkTestAPI(t)
	playKey := standardTalkPlayKey(gb10DeviceID, gb10ChannelID)
	streamID := standardTalkStreamID(inputChannel)
	api.streams.Store(playKey, &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: streamID,
		mediaServer: mediaServer, sessionKey: playKey,
	})
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SMS: mediaServer,
		Stream:              &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID},
		StandardTalkPlayKey: playKey, ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(gb10ChannelID, session)
	if err := api.OnMediaStreamChanged(t.Context(), MediaStreamEvent{StreamID: streamID, Active: false, Reason: "rtp_timeout"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.broadcastSessions.Load(gb10ChannelID); ok {
		t.Fatal("upstream media loss did not stop standard Talk Broadcast")
	}
	if _, ok := api.streams.Load(playKey); ok {
		t.Fatal("upstream media loss left standard Talk Play state")
	}
}
