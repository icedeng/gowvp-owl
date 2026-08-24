package lalmax

import (
	"context"
	"net/http"
	"testing"
)

func TestApiStatGroup(t *testing.T) {
	engine := NewEngine().SetConfig(Config{URL: "http://lalmax.test"})
	engine.cli = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Path != apiStatGroup || req.URL.Query().Get("app_name") != "live" || req.URL.Query().Get("stream_name") != "stream/1" {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
		}
		return jsonHTTPResponse(t, map[string]any{
			"error_code": 0,
			"desp":       "succ",
			"data": map[string]any{
				"stream_name": "stream/1", "app_name": "live", "audio_codec": "PCMA", "video_codec": "H264",
				"video_width": 1920, "video_height": 1080,
				"pub":              map[string]any{"session_id": "PSSUB1", "protocol": "PS", "read_bytes_sum": 4096, "read_bitrate_kbits": 64},
				"subs":             []map[string]any{{"session_id": "FLVSUB1"}},
				"in_frame_per_sec": []map[string]any{{"unix_sec": 1, "v": 25}},
			},
		}), nil
	})}

	resp, err := engine.ApiStatGroup(context.Background(), "live", "stream/1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Data.StreamName != "stream/1" || resp.Data.Pub.ReadBytesSum != 4096 || resp.Data.Pub.ReadBitrateKbits != 64 || len(resp.Data.Subs) != 1 || len(resp.Data.FPS) != 1 || resp.Data.FPS[0].Value != 25 {
		t.Fatalf("unexpected stat response: %+v", resp.Data)
	}
	if _, err := engine.ApiStatGroup(context.Background(), "", ""); err == nil {
		t.Fatal("empty stream status request was accepted")
	}
}
