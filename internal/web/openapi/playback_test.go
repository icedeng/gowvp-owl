package openapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	op "github.com/gowvp/owl/internal/core/openplatform"
)

type apiPlaybackHost struct {
	op.Host
	mu     sync.Mutex
	starts int
	source string
}

func (h *apiPlaybackHost) PreparePlayback(_ context.Context, row op.Playback) (op.Source, error) {
	return op.Source{MediaServerID: "private-node", App: "rtp", Stream: row.ID, BaseURL: h.source}, nil
}
func (h *apiPlaybackHost) StartPlayback(context.Context, op.Playback) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.starts++
	return nil
}
func (h *apiPlaybackHost) StopPlayback(context.Context, op.Playback) error { return nil }
func (h *apiPlaybackHost) ControlPlayback(context.Context, op.Playback, op.PlaybackControl) error {
	return nil
}
func (h *apiPlaybackHost) PlaybackActive(op.Playback) bool { return true }
func TestPlaybackCodeAPIIdempotencyAndMediaIsolation(t *testing.T) {
	r, s, p, key, oldHost := codeFixture(t)
	s.DB.Model(&ipc.Device{}).Where("id = ?", "d1").Updates(map[string]any{"type": ipc.TypeGB28181, "is_online": true})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte("#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\nsegment.m4s\n"))
			return
		}
		w.Write([]byte("mock-video"))
	}))
	defer upstream.Close()
	h := &apiPlaybackHost{Host: oldHost, source: upstream.URL}
	s.Host = h
	// 仅录像播放权限即可按编码申请，不需要设备/通道读取权限。
	s.DB.Model(&op.App{}).Where("id = ?", p.App.ID).Update("scopes", `["recording:play"]`)
	create := func(body, idem string) *httptest.ResponseRecorder {
		return codeRequest(r, p, key, "POST", codePath+"/playback-sessions", "", body, idem)
	}
	body := `{"start_at":1700000000,"end_at":1700000060,"protocol":"hls"}`
	out := create(body, "playback-first")
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	var response struct {
		Data op.Playback `json:"data"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	id := response.Data.ID
	if id == "" || response.Data.DeviceCode != codeDevice || response.Data.ChannelCode != codeChannel || strings.Contains(out.Body.String(), "private-node") || strings.Contains(out.Body.String(), upstream.URL) {
		t.Fatal(out.Body.String())
	}
	for deadline := time.Now().Add(2 * time.Second); ; {
		row, err := s.Playback(p, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.State == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(row)
		}
		time.Sleep(5 * time.Millisecond)
	}
	out = create(body, "playback-first")
	if out.Code != 200 || !strings.Contains(out.Body.String(), id) {
		t.Fatal("retry", out.Code, out.Body.String())
	}
	h.mu.Lock()
	starts := h.starts
	h.mu.Unlock()
	if starts != 1 {
		t.Fatal("duplicate INVITE", starts)
	}
	out = create(strings.Replace(body, "1700000060", "1700000090", 1), "playback-first")
	if out.Code != 409 || !strings.Contains(out.Body.String(), "IDEMPOTENCY_MISMATCH") {
		t.Fatal(out.Code, out.Body.String())
	}
	for _, path := range []string{"/openapi/v1/playback-sessions", codePath + "/playback-sessions", "/openapi/v1/devices/by-code/" + codeDevice + "/playback-sessions"} {
		out = codeRequest(r, p, key, "GET", path, "", "", "")
		if out.Code != 200 || !strings.Contains(out.Body.String(), id) || strings.Contains(out.Body.String(), upstream.URL) {
			t.Fatal(path, out.Code, out.Body.String())
		}
	}
	out = codeRequest(r, p, key, "POST", "/openapi/v1/playback-sessions/"+id+"/tickets", "", `{}`, "")
	var ticket struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	json.Unmarshal(out.Body.Bytes(), &ticket)
	if out.Code != 200 || !strings.Contains(ticket.Data.URL, "/playback/"+id+"/hls.fmp4.m3u8") {
		t.Fatal(out.Code, out.Body.String())
	}
	mediaURL := strings.TrimPrefix(ticket.Data.URL, s.Config.PublicBaseURL)
	mediaOut := httptest.NewRecorder()
	r.ServeHTTP(mediaOut, httptest.NewRequest("GET", mediaURL, nil))
	if mediaOut.Code != 200 || strings.Contains(mediaOut.Body.String(), "/openmedia/v1/live/") || strings.Contains(mediaOut.Body.String(), upstream.URL) || !strings.Contains(mediaOut.Body.String(), "/playback/"+id+"/init.mp4?ticket=") || !strings.Contains(mediaOut.Body.String(), "/playback/"+id+"/segment.m4s?ticket=") {
		t.Fatal(mediaOut.Code, mediaOut.Body.String())
	}
	for _, suffix := range []string{"init.mp4", "segment.m4s"} {
		child := strings.Replace(mediaURL, "hls.fmp4.m3u8", suffix, 1)
		res := httptest.NewRecorder()
		r.ServeHTTP(res, httptest.NewRequest("GET", child, nil))
		if res.Code != 200 || res.Body.String() != "mock-video" {
			t.Fatal(res.Code, res.Body.String())
		}
	}
	// URL中的会话与kind不能被换成其他会话或live入口。
	for _, bad := range []string{strings.Replace(mediaURL, "/playback/", "/live/", 1), strings.Replace(mediaURL, id, "other-session", 1)} {
		res := httptest.NewRecorder()
		r.ServeHTTP(res, httptest.NewRequest("GET", bad, nil))
		if res.Code != 401 {
			t.Fatal("ticket rebound", res.Code)
		}
	}
	out = codeRequest(r, p, key, "POST", "/openapi/v1/playback-sessions/"+id+"/control", "", `{"action":"pause"}`, "pause-first")
	if out.Code != 200 || !strings.Contains(out.Body.String(), `"state":"paused"`) {
		t.Fatal(out.Code, out.Body.String())
	}
	out = codeRequest(r, p, key, "DELETE", "/openapi/v1/playback-sessions/"+id, "", "", "")
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	mediaOut = httptest.NewRecorder()
	r.ServeHTTP(mediaOut, httptest.NewRequest("GET", mediaURL, nil))
	if mediaOut.Code != 401 {
		t.Fatal("stopped media survived", mediaOut.Code)
	}
}
