package lalmax

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestApiCtrlStartRtpPub(t *testing.T) {
	want := ApiCtrlStartRtpPubReq{
		StreamName:     "test110",
		TimeoutMs:      10000,
		IsWaitKeyFrame: 1,
	}
	engine := NewEngine().SetConfig(Config{URL: "http://lalmax.test"})
	engine.cli = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != ctrlStartRtpPub {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
		}
		var got ApiCtrlStartRtpPubReq
		if err := json.NewDecoder(req.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("unexpected RTP request: %+v", got)
		}
		return jsonHTTPResponse(t, map[string]any{
			"code": 0,
			"data": map[string]any{
				"stream_name": want.StreamName,
				"session_id":  "session-test110",
				"port":        30000,
			},
		}), nil
	})}

	resp, err := engine.ApiCtrlStartRtpPub(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Code != 0 || resp.Data.StreamName != want.StreamName || resp.Data.SessionId != "session-test110" || resp.Data.Port != 30000 {
		t.Fatalf("unexpected RTP response: %+v", resp)
	}
}
