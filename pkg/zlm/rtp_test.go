package zlm

import (
	"encoding/json"
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
		DstURL: "192.0.2.10", DstPort: 8000, IsUDP: true, Type: 1, PT: 96, OnlyAudio: true,
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
	if requests[0]["secret"] != "secret" || requests[0]["only_audio"] != true || requests[0]["type"] != float64(1) || requests[0]["pt"] != float64(96) {
		t.Fatalf("unexpected start payload: %+v", requests[0])
	}
	if requests[1]["recv_stream_id"] != "device-audio" || requests[1]["pt"] != float64(8) || requests[1]["type"] != float64(0) {
		t.Fatalf("unexpected talk payload: %+v", requests[1])
	}
	if requests[2]["ssrc"] != "0100000001" {
		t.Fatalf("unexpected stop payload: %+v", requests[2])
	}
}
