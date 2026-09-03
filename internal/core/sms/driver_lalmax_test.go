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

func TestLalmaxRejectsActiveTCPRTP(t *testing.T) {
	driver := NewLalmaxDriver()
	mediaServer := &MediaServer{IP: "lalmax.test", Ports: MediaServerPorts{HTTP: 80}}
	if _, err := driver.OpenRTPServer(context.Background(), mediaServer, &zlm.OpenRTPServerRequest{TCPMode: 2, StreamID: "stream-1"}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("OpenRTPServer error = %v", err)
	}
	if _, err := driver.ConnectRTPServer(context.Background(), mediaServer, &zlm.ConnectRTPServerRequest{StreamID: "stream-1", DstURL: "192.0.2.20", DstPort: 9000}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("ConnectRTPServer error = %v", err)
	}
}

func TestLalmaxRejectsTCPRTCP(t *testing.T) {
	driver := NewLalmaxDriver()
	mediaServer := &MediaServer{IP: "lalmax.test", Ports: MediaServerPorts{HTTP: 80}}
	if _, err := driver.OpenRTPServer(context.Background(), mediaServer, &zlm.OpenRTPServerRequest{TCPMode: 1, TCPRTCP: true, StreamID: "stream-1"}); err == nil || !strings.Contains(err.Error(), "RTP/RTCP") {
		t.Fatalf("OpenRTPServer error = %v", err)
	}
	if _, err := driver.StartSendRTP(context.Background(), mediaServer, &zlm.StartSendRTPRequest{TCPRTCP: true}); err == nil || !strings.Contains(err.Error(), "RTP/RTCP") {
		t.Fatalf("StartSendRTP error = %v", err)
	}
}

func TestLalmaxGetMediaInfo(t *testing.T) {
	client := &http.Client{Transport: lalmaxRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Path != "/api/stat/group" || req.URL.Query().Get("app_name") != "rtp" || req.URL.Query().Get("stream_name") != "stream-1" || req.URL.Query().Get("token") != "test-secret" {
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
	items, err := driver.GetMediaInfo(context.Background(), &MediaServer{IP: "lalmax.test", Secret: "test-secret", Ports: MediaServerPorts{HTTP: 80}}, "rtp", "stream-1")
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

func TestBuildLalmaxMediaTracksMapsGBStandardAudioCodecs(t *testing.T) {
	tests := []struct {
		codec      string
		wantID     int
		wantSample int
	}{
		{codec: "AAC", wantID: 2},
		{codec: "PCMA", wantID: 3, wantSample: 8000},
		{codec: "SVACA", wantID: 17, wantSample: 8000},
		{codec: "G.722.1", wantID: 18, wantSample: 16000},
		{codec: "G723.1", wantID: 19, wantSample: 8000},
		{codec: "G729", wantID: 21, wantSample: 8000},
		{codec: "vendor-audio", wantID: -1},
	}
	for _, test := range tests {
		t.Run(test.codec, func(t *testing.T) {
			tracks := buildLalmaxMediaTracks(&lalmax.StatGroup{AudioCodec: test.codec})
			if len(tracks) != 1 || tracks[0].CodecID != test.wantID || tracks[0].SampleRate != test.wantSample || tracks[0].CodecIDName != strings.ToUpper(test.codec) {
				t.Fatalf("tracks = %+v", tracks)
			}
		})
	}
}

func TestLalmaxSupportedCompatOperations(t *testing.T) {
	client := &http.Client{Transport: lalmaxRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Get("token") != "test-secret" {
			t.Fatalf("missing lalmax token: %s", req.URL.String())
		}
		payload := map[string]any{"code": 0}
		switch req.URL.Path {
		case "/api/config/svr_config":
			if req.Method != http.MethodGet {
				t.Fatalf("Ping method = %s", req.Method)
			}
			payload["data"] = map[string]any{"server_id": "lalmax-test"}
		case "/index/api/close_streams":
			if req.Method != http.MethodPost {
				t.Fatalf("CloseStreams method = %s", req.Method)
			}
			var body lalmax.CloseStreamsRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.App != "rtp" || body.Stream != "stream-1" || !body.Force {
				t.Fatalf("CloseStreams body = %+v", body)
			}
			payload["count_hit"] = 1
			payload["count_closed"] = 1
		case "/index/api/startRecord":
			if req.Method != http.MethodPost {
				t.Fatalf("StartRecord method = %s", req.Method)
			}
			var body lalmax.StartRecordRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Type != 1 || body.App != "rtp" || body.Stream != "stream-1" || body.CustomPath != "/record" || body.MaxSecond != 60 {
				t.Fatalf("StartRecord body = %+v", body)
			}
			payload["result"] = true
		case "/index/api/stopRecord":
			if req.Method != http.MethodPost {
				t.Fatalf("StopRecord method = %s", req.Method)
			}
			var body lalmax.StopRecordRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Type != 1 || body.App != "rtp" || body.Stream != "stream-1" {
				t.Fatalf("StopRecord body = %+v", body)
			}
			payload["result"] = true
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
	})}
	driver := &LalmaxDriver{engine: lalmax.NewEngine().SetHTTPClient(client)}
	mediaServer := &MediaServer{IP: "lalmax.test", Secret: "test-secret", Ports: MediaServerPorts{HTTP: 80}}

	if err := driver.Ping(context.Background(), mediaServer); err != nil {
		t.Fatal(err)
	}
	closed, err := driver.CloseStreams(context.Background(), mediaServer, &zlm.CloseStreamsRequest{App: "rtp", Stream: "stream-1", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if closed.CountHit != 1 || closed.CountClosed != 1 {
		t.Fatalf("CloseStreams response = %+v", closed)
	}
	started, err := driver.StartRecord(context.Background(), mediaServer, &zlm.StartRecordRequest{Type: 1, Vhost: "__defaultVhost__", App: "rtp", Stream: "stream-1", CustomPath: "/record", MaxSecond: 60})
	if err != nil {
		t.Fatal(err)
	}
	if !started.Result {
		t.Fatalf("StartRecord response = %+v", started)
	}
	stopped, err := driver.StopRecord(context.Background(), mediaServer, &zlm.StopRecordRequest{Type: 1, Vhost: "__defaultVhost__", App: "rtp", Stream: "stream-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !stopped.Result {
		t.Fatalf("StopRecord response = %+v", stopped)
	}
}

func TestLalmaxUnsupportedRecordingStateReturnsError(t *testing.T) {
	driver := NewLalmaxDriver()
	if _, err := driver.GetMediaList(context.Background(), &MediaServer{}); err == nil {
		t.Fatal("GetMediaList error = nil")
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
