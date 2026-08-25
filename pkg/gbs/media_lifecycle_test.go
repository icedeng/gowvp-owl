package gbs

import (
	"context"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestMediaStreamLifecycleActiveAndInactive(t *testing.T) {
	streams := &conc.Map[string, *Streams]{}
	stream := &Streams{
		DeviceID:  gb10DeviceID,
		ChannelID: gb10ChannelID,
		StreamID:  "internal-stream-1",
	}
	streams.Store("history:Playback:"+gb10DeviceID+":"+gb10ChannelID, stream)
	api := &GB28181API{streams: streams}
	now := time.Now()

	if err := api.OnMediaStreamChanged(context.Background(), MediaStreamEvent{StreamID: stream.StreamID, Active: true, At: now}); err != nil {
		t.Fatal(err)
	}
	if !stream.Stream || !stream.LastMediaAt.Equal(now) {
		t.Fatalf("active stream state = %+v", stream)
	}
	if err := api.OnMediaStreamChanged(context.Background(), MediaStreamEvent{StreamID: stream.StreamID, Reason: "rtp_server_timeout", At: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if stream.Stream || !stream.Stop || stream.EndReason != "rtp_server_timeout" {
		t.Fatalf("inactive stream state = %+v", stream)
	}
	if streams.Len() != 0 {
		t.Fatal("inactive stream was not removed")
	}
	if got := api.metrics.Snapshot().MediaDisconnects; got != 1 {
		t.Fatalf("media disconnect metric = %d; want 1", got)
	}

	// 重复注销和 MediaStatus 竞争后均应保持幂等。
	if err := api.OnMediaStreamChanged(context.Background(), MediaStreamEvent{StreamID: stream.StreamID, Reason: "duplicate"}); err != nil {
		t.Fatal(err)
	}
}

func TestMediaStartFailuresDoNotRetainPlaceholders(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	channel := &Channel{ChannelID: gb10ChannelID, device: memory.runtime}
	memory.runtime.Channels.Store(gb10ChannelID, channel)
	media := &fakeRTPMediaService{}
	streams := &conc.Map[string, *Streams]{}
	cfg := conf.SIP{ID: gb10PlatformID, Domain: "3402000000"}
	api := &GB28181API{cfg: &cfg, sms: media, streams: streams}
	api.svr = &Server{gb: api, memoryStorer: memory}
	inputChannel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}
	mediaServer := &sms.MediaServer{}

	if err := api.Play(&PlayInput{Channel: inputChannel, SMS: mediaServer}); err == nil {
		t.Fatal("Play unexpectedly succeeded")
	}
	if streams.Len() != 0 {
		t.Fatal("failed Play retained a stream placeholder")
	}

	if err := api.StartHistory(t.Context(), &HistoryInput{
		Channel: inputChannel, SMS: mediaServer, Mode: historyModePlayback,
		StartAt: time.Now().Add(-time.Hour), EndAt: time.Now(),
	}); err == nil {
		t.Fatal("StartHistory unexpectedly succeeded")
	}
	if streams.Len() != 0 {
		t.Fatal("failed history start retained a stream placeholder")
	}

	placeholderKey := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	streams.Store(placeholderKey, &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID})
	if err := api.stopHistoryNoLock(channel, &StopHistoryInput{Channel: inputChannel, Mode: historyModePlayback}); err != nil {
		t.Fatal(err)
	}
	if streams.Len() != 0 {
		t.Fatal("stopping an incomplete history session retained its placeholder")
	}
}
