package zlm

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestEngine_GetServerConfig(t *testing.T) {
	const secret = "test-secret"
	engine := NewEngine().SetConfig(Config{URL: "http://zlm.test", Secret: secret})
	engine.cli = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != getServerConfig {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
		}
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["secret"] != secret {
			t.Fatalf("unexpected secret: %v", payload["secret"])
		}
		body := `{"code":0,"data":[{"general.mediaServerId":"zlm-test","http.port":"80","rtsp.port":"554"}]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}

	out, err := engine.GetServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 || out.Data[0].GeneralMediaServerID != "zlm-test" {
		t.Fatalf("unexpected config response: %+v", out)
	}
	if out.Data[0].HTTPPort != 80 || out.Data[0].RtspPort != 554 {
		t.Fatalf("unexpected port config: %+v", out.Data[0])
	}
}

func TestGetSnap(t *testing.T) {
	want := bytes.Repeat([]byte{0xff}, 128)
	engine := NewEngine().SetConfig(Config{URL: "http://zlm.test", Secret: "test-secret"})
	engine.cli = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != getSnapshot {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
		}
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["secret"] != "test-secret" || payload["url"] != "rtmp://example.test/live/stream" {
			t.Fatalf("unexpected snapshot payload: %+v", payload)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(want)),
		}, nil
	})}

	got, err := engine.GetSnap(GetSnapRequest{
		URL:        "rtmp://example.test/live/stream",
		TimeoutSec: 5,
		ExpireSec:  10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("unexpected snapshot bytes: got %d, want %d", len(got), len(want))
	}
}
