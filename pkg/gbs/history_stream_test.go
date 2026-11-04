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

func TestHistoryDefaultReceiverDoesNotReuseLiveStream(t *testing.T) {
	for _, mode := range []string{historyModePlayback, historyModeDownload} {
		t.Run(mode, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			media := &fakeRTPMediaService{}
			streams := &conc.Map[string, *Streams]{}
			api := &GB28181API{cfg: &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"}, sms: media, streams: streams}
			api.svr = &Server{gb: api, memoryStorer: memory}
			ch := &ipc.Channel{ID: "ch-test", DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}
			// 假媒体服务在打开端口时返回错误，检查失败前实际申请的收流标识。
			if err := api.StartHistory(t.Context(), &HistoryInput{Channel: ch, SMS: &sms.MediaServer{}, Mode: mode, StartAt: time.Now().Add(-time.Hour), EndAt: time.Now()}); err == nil {
				t.Fatal("expected media receiver failure")
			}
			if media.openCalls != 1 || media.opened.StreamID != HistoryStreamID(mode, ch.ID) || media.opened.StreamID == ch.ID {
				t.Fatalf("receiver request = %+v, calls = %d", media.opened, media.openCalls)
			}
			if streams.Len() != 0 {
				t.Fatal("failed start retained history session")
			}
		})
	}
}

func TestHistoryReceiverLossLeavesLiveAndDownloadUntouched(t *testing.T) {
	streams := &conc.Map[string, *Streams]{}
	live := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "ch-test", Stream: true}
	playback := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: HistoryStreamID(historyModePlayback, "ch-test")}
	download := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: HistoryStreamID(historyModeDownload, "ch-test"), Stream: true}
	liveKey := resolvePlaySessionKey(gb10DeviceID, gb10ChannelID, "")
	downloadKey := historyKey(historyModeDownload, gb10DeviceID, gb10ChannelID)
	streams.Store(liveKey, live)
	streams.Store(historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID), playback)
	streams.Store(downloadKey, download)
	api := &GB28181API{streams: streams}
	if err := api.OnMediaStreamChanged(context.Background(), MediaStreamEvent{StreamID: playback.StreamID, Reason: "rtp_server_timeout"}); err != nil {
		t.Fatal(err)
	}
	if current, ok := streams.Load(liveKey); !ok || current != live || !live.Stream || live.Stop {
		t.Fatal("history cleanup changed live session")
	}
	if current, ok := streams.Load(downloadKey); !ok || current != download || !download.Stream || download.Stop {
		t.Fatal("playback cleanup changed download session")
	}
}
