package gbs

import (
	"context"
	"testing"
	"time"

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
