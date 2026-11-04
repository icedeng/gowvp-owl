package zlm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func inspectionTestEngine(handler roundTripFunc) Engine {
	engine := NewEngine().SetConfig(Config{URL: "http://zlm.test", Secret: "node-secret"})
	engine.cli = &http.Client{Transport: handler}
	return engine
}

func inspectionResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestInspectionEndpointsAreReadOnlyAndPreserveFields(t *testing.T) {
	paths := map[string]string{
		"version": "version", "streams": "getMediaList", "sessions": "getAllSession", "rtp_servers": "listRtpServer",
		"pull_proxies": "listStreamProxy", "push_proxies": "listStreamPusherProxy", "ffmpeg": "listFFmpegSource",
		"threads": "getThreadsLoad", "work_threads": "getWorkThreadsLoad", "statistics": "getStatistic", "apis": "getApiList",
		"stream_info": "getMediaInfo", "stream_players": "getMediaPlayerList", "rtp_info": "getRtpInfo",
	}
	for resource, path := range paths {
		t.Run(resource, func(t *testing.T) {
			engine := inspectionTestEngine(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/index/api/"+path || request.Method != http.MethodPost {
					t.Fatalf("unexpected endpoint: %s %s", request.Method, request.URL.Path)
				}
				var body map[string]string
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["secret"] != "node-secret" || body["force"] != "" || body["url"] != "" {
					t.Fatalf("caller escaped query whitelist: %v", body)
				}
				return inspectionResponse(200, `{"code":0,"data":{"futureField":9007199254740993,"tracks":[{"codec_id_name":"H265","loss":2.1}]}}`), nil
			})
			result, err := engine.Inspect(t.Context(), resource, map[string]string{"secret": "wrong", "schema": "rtsp", "vhost": "__defaultVhost__", "app": "rtp", "stream": "ch1", "stream_id": "ch1", "force": "1", "url": "http://other"})
			if err != nil {
				t.Fatal(err)
			}
			data := result.(map[string]any)
			if data["futureField"].(json.Number).String() != "9007199254740993" || len(data["tracks"].([]any)) != 1 {
				t.Fatalf("lost original fields or precision: %v", data)
			}
		})
	}
	engine := inspectionTestEngine(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid endpoint or params reached network")
		return nil, nil
	})
	for _, resource := range []string{"setServerConfig", "restartServer", "close_streams", "../getServerConfig", "getServerConfig"} {
		if _, err := engine.Inspect(t.Context(), resource, nil); err == nil {
			t.Errorf("accepted non-inspection endpoint: %s", resource)
		}
	}
	if _, err := engine.Inspect(t.Context(), "stream_info", map[string]string{"stream": "ch"}); err == nil {
		t.Fatal("accepted incomplete stream identity")
	}
}

func TestInspectionFailureAndEmptyAreDistinct(t *testing.T) {
	for _, test := range []struct {
		name, body  string
		status      int
		wantError   bool
		unsupported bool
	}{
		{name: "empty null", status: 200, body: `{"code":0,"data":null}`},
		{name: "empty missing", status: 200, body: `{"code":0}`},
		{name: "authentication", status: 200, body: `{"code":-100,"msg":"node-secret"}`, wantError: true},
		{name: "business", status: 200, body: `{"code":-500,"msg":"node-secret"}`, wantError: true},
		{name: "unsupported", status: 404, body: `not found`, wantError: true, unsupported: true},
		{name: "redirect", status: 302, body: `redirect`, wantError: true},
		{name: "HTTP failure", status: 503, body: `unavailable`, wantError: true},
		{name: "HTML", status: 200, body: `<html>not zlm</html>`, wantError: true},
		{name: "missing code", status: 200, body: `{"data":[]}`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := inspectionTestEngine(func(*http.Request) (*http.Response, error) { return inspectionResponse(test.status, test.body), nil })
			data, err := engine.Inspect(t.Context(), "streams", nil)
			if (err != nil) != test.wantError || errors.Is(err, ErrInspectionUnsupported) != test.unsupported {
				t.Fatalf("unexpected status: data=%v err=%v", data, err)
			}
			if err != nil && strings.Contains(err.Error(), "node-secret") {
				t.Fatal("error leaked secret")
			}
			if err == nil && len(data.([]any)) != 0 {
				t.Fatalf("expected empty collection, got %v", data)
			}
		})
	}
}

func TestInspectionRedactsNestedCredentialsAndCommandLines(t *testing.T) {
	engine := inspectionTestEngine(func(*http.Request) (*http.Response, error) {
		return inspectionResponse(200, `{"code":0,"data":[{"url":"rtsp://user:camera-pass@192.0.2.10/live?stream_key=private","originUrl":"http://node.test/x?secret=node-secret","cmd":"ffmpeg -i rtsp://user:camera-pass@node/live","key":"task-id","future":{"auth_token":"private-token","password":"private-password","params":"authorization=private-param"}}]}`), nil
	})
	data, err := engine.Inspect(t.Context(), "ffmpeg", nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range []string{"camera-pass", "node-secret", "stream_key", "private-token", "private-password", "private-param", "ffmpeg -i"} {
		if strings.Contains(string(encoded), sensitive) {
			t.Errorf("response exposed %q: %s", sensitive, encoded)
		}
	}
	if !strings.Contains(string(encoded), "192.0.2.10/live") || !strings.Contains(string(encoded), "task-id") {
		t.Fatal("redaction destroyed operational identifiers")
	}
}

func TestInspectionPreservesLongListsAndTopLevelResponses(t *testing.T) {
	for _, resource := range []string{"stream_info", "rtp_info"} {
		engine := inspectionTestEngine(func(*http.Request) (*http.Response, error) {
			return inspectionResponse(200, `{"code":0,"exist":false,"readerCount":0}`), nil
		})
		data, err := engine.Inspect(t.Context(), resource, map[string]string{"schema": "rtsp", "vhost": "v", "app": "rtp", "stream": "ch", "stream_id": "ch"})
		if err != nil || data.(map[string]any)["exist"] != false {
			t.Fatalf("lost top-level result: %v %v", data, err)
		}
	}
	engine := inspectionTestEngine(func(*http.Request) (*http.Response, error) {
		return inspectionResponse(200, `{"code":0,"data":[`+strings.Repeat(`{"stream":"x"},`, 10000)+`{"stream":"last"}]}`), nil
	})
	data, err := engine.Inspect(t.Context(), "streams", nil)
	if err != nil || len(data.([]any)) != 10001 {
		t.Fatalf("long list was truncated: %v", err)
	}
}

func TestInspectionCancellationAndRedirectDoNotContinueQuery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	engine := inspectionTestEngine(func(request *http.Request) (*http.Response, error) { return nil, request.Context().Err() })
	if _, err := engine.Inspect(ctx, "streams", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled query: %v", err)
	}
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	engine = NewEngine().SetConfig(Config{URL: source.URL, Secret: "node-secret"})
	if _, err := engine.Inspect(t.Context(), "streams", nil); err == nil || redirected {
		t.Fatalf("followed redirect or accepted it: err=%v redirected=%v", err, redirected)
	}
}
