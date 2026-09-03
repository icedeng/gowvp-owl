package zlm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestEngineSendRTP(t *testing.T) {
	engine := NewEngine().SetConfig(Config{URL: "http://zlm.test", Secret: "secret"})
	requests := make([]map[string]any, 0, 3)
	engine.cli = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		payload["path"] = req.URL.Path
		requests = append(requests, payload)
		body := `{"code":0,"local_port":30000}`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}

	started, err := engine.StartSendRTP(StartSendRTPRequest{
		Vhost: "__defaultVhost__", App: "live", Stream: "mic", SSRC: "0100000001",
		DstURL: "192.0.2.10", DstPort: 8000, IsUDP: false, Type: 1, PT: 96, OnlyAudio: true, TCPRTCP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.LocalPort != 30000 {
		t.Fatalf("local port = %d", started.LocalPort)
	}
	if _, err := engine.StartSendRTPTalk(StartSendRTPTalkRequest{
		Vhost: "__defaultVhost__", App: "live", Stream: "mic", SSRC: "0100000002",
		RecvStreamID: "device-audio", Type: 0, PT: 8, OnlyAudio: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.StopSendRTP(StopSendRTPRequest{Vhost: "__defaultVhost__", App: "live", Stream: "mic", SSRC: "0100000001"}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 || requests[0]["path"] != startSendRtp || requests[1]["path"] != startSendRtpTalk || requests[2]["path"] != stopSendRtp {
		t.Fatalf("unexpected requests: %+v", requests)
	}
	if requests[0]["secret"] != "secret" || requests[0]["only_audio"] != true || requests[0]["type"] != float64(1) || requests[0]["pt"] != float64(96) || requests[0]["tcp_rtcp"] != true {
		t.Fatalf("unexpected start payload: %+v", requests[0])
	}
	if requests[1]["recv_stream_id"] != "device-audio" || requests[1]["pt"] != float64(8) || requests[1]["type"] != float64(0) {
		t.Fatalf("unexpected talk payload: %+v", requests[1])
	}
	if requests[2]["ssrc"] != "0100000001" {
		t.Fatalf("unexpected stop payload: %+v", requests[2])
	}
}

func TestEngineOpenRTPServerSendsTCPRTCPFlag(t *testing.T) {
	engine := NewEngine().SetConfig(Config{URL: "http://zlm.test", Secret: "secret"})
	engine.cli = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["tcp_mode"] != float64(1) || payload["tcp_rtcp"] != true || payload["stream_id"] != "stream-1" || payload["ssrc"] != float64(1200000001) {
			t.Fatalf("payload = %+v", payload)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"port":30000}`))}, nil
	})}
	if _, err := engine.OpenRTPServer(OpenRTPServerRequest{TCPMode: 1, TCPRTCP: true, StreamID: "stream-1", SSRC: 1200000001}); err != nil {
		t.Fatal(err)
	}
}

func TestEngineConnectRTPServer(t *testing.T) {
	engine := NewEngine().SetConfig(Config{URL: "http://zlm.test", Secret: "secret"})
	engine.cli = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != connectRtpServer {
			t.Fatalf("path = %s", req.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["secret"] != "secret" || payload["stream_id"] != "stream-1" || payload["dst_url"] != "192.0.2.20" || payload["dst_port"] != float64(9000) {
			t.Fatalf("payload = %+v", payload)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"msg":"success"}`))}, nil
	})}
	if _, err := engine.ConnectRTPServer(ConnectRTPServerRequest{StreamID: "stream-1", DstURL: "192.0.2.20", DstPort: 9000}); err != nil {
		t.Fatal(err)
	}
}

func TestEngineConnectRTPServerHonorsContextCancellation(t *testing.T) {
	engine := NewEngine().SetConfig(Config{URL: "http://zlm.test", Secret: "secret"})
	engine.cli = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := engine.ConnectRTPServerContext(ctx, ConnectRTPServerRequest{StreamID: "stream-1", DstURL: "192.0.2.20", DstPort: 9000})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
}

func TestEngineCloseRTPServerHonorsContextCancellation(t *testing.T) {
	engine := NewEngine().SetConfig(Config{URL: "http://zlm.test", Secret: "secret"})
	engine.cli = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := engine.CloseRTPServerContext(ctx, CloseRTPServerRequest{StreamID: "stream-1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
}

func TestEngineStopSendRTPHonorsContextCancellation(t *testing.T) {
	engine := NewEngine().SetConfig(Config{URL: "http://zlm.test", Secret: "secret"})
	engine.cli = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := engine.StopSendRTPContext(ctx, StopSendRTPRequest{Vhost: "__defaultVhost__", App: "live", Stream: "stream-1", SSRC: "0100000001"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
}

func TestEngineOpenRTPServerHonorsContextCancellation(t *testing.T) {
	engine := NewEngine().SetConfig(Config{URL: "http://zlm.test", Secret: "secret"})
	engine.cli = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := engine.OpenRTPServerContext(ctx, OpenRTPServerRequest{StreamID: "stream-1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
}

func TestEngineStartSendRTPTalkHonorsContextCancellation(t *testing.T) {
	engine := NewEngine().SetConfig(Config{URL: "http://zlm.test", Secret: "secret"})
	engine.cli = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := engine.StartSendRTPTalkContext(ctx, StartSendRTPTalkRequest{Stream: "stream-1", RecvStreamID: "receiver-1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
}

func TestEngineGetMediaInfoHonorsContextCancellation(t *testing.T) {
	engine := NewEngine().SetConfig(Config{URL: "http://zlm.test", Secret: "secret"})
	engine.cli = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := engine.GetMediaInfoContext(ctx, GetMediaInfoRequest{Schema: "rtsp", Vhost: "__defaultVhost__", App: "live", Stream: "stream-1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
}
