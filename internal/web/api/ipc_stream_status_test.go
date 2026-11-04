package api

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/zlm"
)

func TestChannelStreamStatusesUseLiveMediaPerNode(t *testing.T) {
	channels := []*ipc.Channel{
		{ID: "live", Type: ipc.TypeGB28181, IsPlaying: false},
		{ID: "history-only", Type: ipc.TypeGB28181, IsPlaying: true},
		{ID: "live", Type: ipc.TypeGB28181, Config: ipc.StreamConfig{MediaServerID: "other"}},
		{ID: "failed", Type: ipc.TypeGB28181, Config: ipc.StreamConfig{MediaServerID: "failed"}},
		{ID: "rtsp", Type: ipc.TypeRTSP, Stream: "source"},
		{ID: "unsupported", Type: "unsupported"},
	}
	var mu sync.Mutex
	calls := make(map[string]int)
	got := queryChannelStreamStatuses(t.Context(), channels, func(_ context.Context, node string) (*zlm.GetMediaListResponse, error) {
		mu.Lock()
		calls[node]++
		mu.Unlock()
		if node == "failed" {
			return nil, errors.New("node unavailable")
		}
		if node == "other" {
			return &zlm.GetMediaListResponse{}, nil
		}
		return &zlm.GetMediaListResponse{Data: []zlm.MediaItem{
			{App: "rtp", Stream: "live", Schema: "rtsp", ReaderCount: 0},
			{App: "rtp", Stream: "live", Schema: "rtmp"},
			{App: "rtp", Stream: "history-playback-history-only"},
			{App: "rtp", Stream: "history-download-history-only"},
			{App: "live", Stream: "source"},
		}}, nil
	})
	for i, want := range []string{"streaming", "idle", "idle", "unknown", "streaming", "unknown"} {
		if got[i].StreamStatus != want {
			t.Errorf("channel %d: got %s, want %s", i, got[i].StreamStatus, want)
		}
	}
	if len(calls) != 3 {
		t.Fatalf("expected 3 nodes, got %v", calls)
	}
	for node, count := range calls {
		if count != 1 {
			t.Errorf("node %s queried %d times", node, count)
		}
	}
	data, err := json.Marshal(got[0])
	var response map[string]any
	if err != nil || json.Unmarshal(data, &response) != nil || response["id"] != "live" || response["stream_status"] != "streaming" || response["is_playing"] != false {
		t.Fatalf("channel response is not backward compatible: %s, %v", data, err)
	}
}

func TestChannelStreamStatusCancellationAndMediaErrors(t *testing.T) {
	channel := &ipc.Channel{ID: "ch", Type: ipc.TypeGB28181}
	for _, response := range []*zlm.GetMediaListResponse{nil, {FixedHeader: zlm.FixedHeader{Code: -1}}} {
		got := queryChannelStreamStatuses(t.Context(), []*ipc.Channel{channel}, func(context.Context, string) (*zlm.GetMediaListResponse, error) { return response, nil })
		if got[0].StreamStatus != "unknown" {
			t.Fatal("failed media query reported idle")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	got := queryChannelStreamStatuses(ctx, []*ipc.Channel{channel}, func(ctx context.Context, _ string) (*zlm.GetMediaListResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if got[0].StreamStatus != "unknown" || ctx.Err() == nil {
		t.Fatal("cancelled query did not preserve unknown status")
	}
}
