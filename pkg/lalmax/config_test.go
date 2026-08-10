package lalmax

import (
	"context"
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

func jsonHTTPResponse(t *testing.T, payload any) *http.Response {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}
}

func TestGetServerConfig(t *testing.T) {
	engine := NewEngine().SetConfig(Config{URL: "http://lalmax.test"})
	engine.cli = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Path != "/api/config/svr_config" {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
		}
		return jsonHTTPResponse(t, map[string]any{
			"code": 0,
			"data": map[string]any{
				"conf_version":   "test-v1",
				"server_id":      "lalmax-test",
				"key_frame_path": "/tmp/keyframes",
				"max_open_files": 1024,
				"gop_cache_config": map[string]any{
					"gop_cache_num":            2,
					"single_gop_max_frame_num": 300,
				},
				"rtmp": map[string]any{"enable": true, "addr": ":1935"},
				"rtsp": map[string]any{"enable": true, "addr": ":5544"},
				"gb28181": map[string]any{
					"enable": true,
					"sip_ip": "192.0.2.10",
				},
			},
		}), nil
	})}

	resp, err := engine.GetServerConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resp.ConfVersion != "test-v1" || resp.ServerId != "lalmax-test" || resp.KeyFramePath != "/tmp/keyframes" {
		t.Fatalf("unexpected basic config: %+v", resp)
	}
	if resp.GopCacheConfig.GopNum != 2 || !resp.RtmpConfig.Enable || !resp.RtspConfig.Enable {
		t.Fatalf("unexpected nested config: %+v", resp)
	}
	if !resp.Gb28181Config.Enable || resp.Gb28181Config.SipIP != "192.0.2.10" {
		t.Fatalf("unexpected GB28181 config: %+v", resp.Gb28181Config)
	}
}

func TestSetHttpNotifyConfig(t *testing.T) {
	notifyConfig := HttpNotifyConfig{
		Enable:            true,
		UpdateIntervalSec: 5,
		OnPubStart:        "http://127.0.0.1:18080/webhook/on_pub_start",
		OnPubStop:         "http://127.0.0.1:18080/webhook/on_pub_stop",
		OnSubStart:        "http://127.0.0.1:18080/webhook/on_sub_start",
		OnSubStop:         "http://127.0.0.1:18080/webhook/on_sub_stop",
		OnStreamChanged:   "http://127.0.0.1:18080/webhook/on_stream_changed",
	}
	mediaConfig := MediaConfig{ListenPort: 8080, MultiPortMaxIncrement: 10}
	var stored HttpNotifyConfig

	engine := NewEngine().SetConfig(Config{URL: "http://lalmax.test"})
	engine.cli = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/api/config/set_server_config":
			if req.URL.Query().Get("merge") != "true" {
				t.Fatal("set_server_config must use merge mode")
			}
			var payload struct {
				HTTPNotify HttpNotifyConfig `json:"http_notify"`
				RTSP       struct {
					PubNotSubAutoCloseSec int `json:"pub_not_sub_auto_close_sec"`
				} `json:"rtsp"`
				GB28181 struct {
					Enable                bool        `json:"enable"`
					PubNotSubAutoCloseSec int         `json:"pub_not_sub_auto_close_sec"`
					MediaConfig           MediaConfig `json:"media_config"`
				} `json:"gb28181"`
			}
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.RTSP.PubNotSubAutoCloseSec != 30 || payload.GB28181.PubNotSubAutoCloseSec != 30 {
				t.Fatalf("unexpected auto-close config: %+v", payload)
			}
			if payload.GB28181.Enable || payload.GB28181.MediaConfig != mediaConfig {
				t.Fatalf("unexpected GB28181 media config: %+v", payload.GB28181)
			}
			stored = payload.HTTPNotify
			return jsonHTTPResponse(t, map[string]any{"code": 0}), nil
		case req.Method == http.MethodGet && req.URL.Path == "/api/config/svr_config":
			return jsonHTTPResponse(t, map[string]any{
				"code": 0,
				"data": map[string]any{"http_notify": stored},
			}), nil
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			return nil, nil
		}
	})}

	ctx := context.Background()
	if err := engine.SetHttpNotifyConfig(ctx, notifyConfig, mediaConfig); err != nil {
		t.Fatal(err)
	}
	resp, err := engine.GetServerConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resp.HttpNotifyConfig != notifyConfig {
		t.Fatalf("unexpected HTTP notify config: %+v", resp.HttpNotifyConfig)
	}
}
