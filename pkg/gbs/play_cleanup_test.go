package gbs

import (
	"strings"
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestStopPlayClosesRTPServerForHalfInitializedSession(t *testing.T) {
	media := &fakeRTPMediaService{}
	server := &sms.MediaServer{ID: sms.DefaultMediaServerID}
	streams := &conc.Map[string, *Streams]{}
	api := &GB28181API{sms: media, streams: streams}
	input := &StopPlayInput{Channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}}
	key := resolvePlaySessionKey(gb10DeviceID, gb10ChannelID, "")
	streams.Store(key, &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		StreamID: "half-initialized-live", mediaServer: server,
	})

	if err := api.stopPlay(&Channel{}, input); err != nil {
		t.Fatal(err)
	}
	media.mu.Lock()
	closeCalls, closed := media.closeCalls, media.closed
	media.mu.Unlock()
	if closeCalls != 1 || closed.StreamID != "half-initialized-live" {
		t.Fatalf("RTP cleanup = calls:%d request:%+v", closeCalls, closed)
	}
	if _, ok := streams.Load(key); ok {
		t.Fatal("stopped live session remained in stream registry")
	}
}

func TestStopPlayClosesRTPServerWhenBYECannotBeBuilt(t *testing.T) {
	media := &fakeRTPMediaService{}
	server := &sms.MediaServer{ID: sms.DefaultMediaServerID}
	streams := &conc.Map[string, *Streams]{}
	api := &GB28181API{sms: media, streams: streams}
	input := &StopPlayInput{Channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}}
	key := resolvePlaySessionKey(gb10DeviceID, gb10ChannelID, "")
	streams.Store(key, &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "malformed-live",
		mediaServer: server, Resp: sip.NewResponse("", sip.DefaultSipVersion, 200, "OK", nil, nil),
	})

	err := api.stopPlay(&Channel{}, input)
	if err == nil || !strings.Contains(err.Error(), "Contact or To") {
		t.Fatalf("malformed BYE result = %v", err)
	}
	media.mu.Lock()
	closeCalls, closed := media.closeCalls, media.closed
	media.mu.Unlock()
	if closeCalls != 1 || closed.StreamID != "malformed-live" {
		t.Fatalf("RTP cleanup after malformed BYE = calls:%d request:%+v", closeCalls, closed)
	}
}
