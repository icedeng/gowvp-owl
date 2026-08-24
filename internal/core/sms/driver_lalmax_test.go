package sms

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gowvp/owl/pkg/lalmax"
	"github.com/gowvp/owl/pkg/zlm"
)

type lalmaxRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn lalmaxRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestLalmaxCloseRTPServer(t *testing.T) {
	tests := []struct {
		name    string
		code    int
		wantHit int
		wantErr bool
	}{
		{name: "closed", code: 0, wantHit: 1},
		{name: "already closed", code: 1003, wantHit: 0},
		{name: "server error", code: 2002, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: lalmaxRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodPost || req.URL.Path != "/api/ctrl/stop_rtp_pub" {
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
				}
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["stream_name"] != "stream-1" {
					t.Fatalf("stop RTP body = %+v", body)
				}
				payload, err := json.Marshal(map[string]any{"error_code": test.code, "desp": test.name})
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(payload)))}, nil
			})}
			driver := &LalmaxDriver{engine: lalmax.NewEngine().SetHTTPClient(client)}
			mediaServer := &MediaServer{IP: "lalmax.test", Ports: MediaServerPorts{HTTP: 80}}
			resp, err := driver.CloseRTPServer(context.Background(), mediaServer, &zlm.CloseRTPServerRequest{StreamID: "stream-1"})
			if test.wantErr {
				if err == nil {
					t.Fatal("CloseRTPServer error = nil")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp == nil || resp.Hit != test.wantHit {
				t.Fatalf("CloseRTPServer response = %+v", resp)
			}
		})
	}
}

func TestLalmaxGetMediaInfo(t *testing.T) {
	client := &http.Client{Transport: lalmaxRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Path != "/api/stat/group" || req.URL.Query().Get("app_name") != "rtp" || req.URL.Query().Get("stream_name") != "stream-1" {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
		}
		payload, err := json.Marshal(map[string]any{
			"error_code": 0,
			"desp":       "succ",
			"data": map[string]any{
				"stream_name": "stream-1", "app_name": "rtp", "audio_codec": "PCMA", "video_codec": "H264",
				"video_width": 1280, "video_height": 720,
				"pub":              map[string]any{"protocol": "PS", "read_bytes_sum": 4096, "read_bitrate_kbits": 64},
				"subs":             []map[string]any{{"session_id": "sub-1"}, {"session_id": "sub-2"}},
				"in_frame_per_sec": []map[string]any{{"unix_sec": 1, "v": 25}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(payload)))}, nil
	})}
	driver := &LalmaxDriver{engine: lalmax.NewEngine().SetHTTPClient(client)}
	items, err := driver.GetMediaInfo(context.Background(), &MediaServer{IP: "lalmax.test", Ports: MediaServerPorts{HTTP: 80}}, "rtp", "stream-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].TotalBytes != 4096 || items[0].BytesSpeed != 8192 || items[0].ReaderCount != 2 || len(items[0].Tracks) != 2 {
		t.Fatalf("media info = %+v", items)
	}
	video, audio := items[0].Tracks[0], items[0].Tracks[1]
	if video.CodecID != 0 || video.FPS != 25 || video.Width != 1280 || video.Height != 720 || audio.CodecID != 3 || audio.SampleRate != 8000 {
		t.Fatalf("media tracks = %+v", items[0].Tracks)
	}
}

func TestLalmaxUnsupportedSendRTPOperationsReturnErrors(t *testing.T) {
	driver := NewLalmaxDriver()
	server := &MediaServer{}
	if _, err := driver.StartSendRTP(context.Background(), server, &zlm.StartSendRTPRequest{}); err == nil {
		t.Fatal("StartSendRTP error = nil")
	}
	if _, err := driver.StartSendRTPTalk(context.Background(), server, &zlm.StartSendRTPTalkRequest{}); err == nil {
		t.Fatal("StartSendRTPTalk error = nil")
	}
	if _, err := driver.StopSendRTP(context.Background(), server, &zlm.StopSendRTPRequest{}); err == nil {
		t.Fatal("StopSendRTP error = nil")
	}
}
