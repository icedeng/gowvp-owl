package gbs

import (
	"context"
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestPlayNoLockReusesEstablishedLiveStream(t *testing.T) {
	device := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	channel := &Channel{ChannelID: gb10DeviceID, device: device}
	input := &PlayInput{Channel: &ipc.Channel{
		ID:        "GB_channel_reuse",
		DeviceID:  gb10DeviceID,
		ChannelID: gb10DeviceID,
	}}
	key := resolvePlaySessionKey(input.Channel.DeviceID, input.Channel.ChannelID, "")
	existing := &Streams{
		DeviceID:  input.Channel.DeviceID,
		ChannelID: input.Channel.ChannelID,
		StreamID:  input.Channel.ID,
		Resp:      sip.NewResponse("reuse-response", sip.DefaultSipVersion, 200, "OK", nil, nil),
	}
	api := &GB28181API{streams: &conc.Map[string, *Streams]{}}
	api.streams.Store(key, existing)

	if err := api.playNoLock(context.Background(), channel, input); err != nil {
		t.Fatalf("reuse established live stream: %v", err)
	}
	got, ok := api.streams.Load(key)
	if !ok || got != existing {
		t.Fatalf("established live stream was replaced: got=%p want=%p", got, existing)
	}
}
