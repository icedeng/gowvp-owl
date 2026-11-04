package openapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/ipc"
	op "github.com/gowvp/owl/internal/core/openplatform"
)

const codeDevice = "50010700001110000001"
const codeChannel = "50010700001320000011"
const otherDevice = "50010700001110000002"
const codePath = "/openapi/v1/devices/by-code/" + codeDevice + "/channels/" + codeChannel

type codeHost struct {
	op.Host
	mu           sync.Mutex
	ptzChannels  []string
	started      []string
	captured     []string
	capabilities []string
}

func (h *codeHost) Capabilities(_ context.Context, channel string) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.capabilities = append(h.capabilities, channel)
	return []string{"left", "stop"}, nil
}
func (h *codeHost) PTZ(_ context.Context, channel string, _ op.PTZInput) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ptzChannels = append(h.ptzChannels, channel)
	return nil
}
func (h *codeHost) Start(_ context.Context, channel string) (op.Source, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started = append(h.started, channel)
	return op.Source{App: "rtp", Stream: channel, BaseURL: "http://media.invalid"}, nil
}
func (h *codeHost) Snapshot(_ context.Context, channel, _ string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.captured = append(h.captured, channel)
	return op.ErrUnavailable
}
func codeFixture(t *testing.T) (*gin.Engine, *op.Service, op.Principal, []byte, *codeHost) {
	t.Helper()
	r, s, p, key := apiFixture(t)
	for id, code := range map[string]string{"d1": codeDevice, "d2": otherDevice} {
		if err := s.DB.Model(&ipc.Device{}).Where("id = ?", id).Update("device_id", code).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"c1", "c2"} {
		if err := s.DB.Model(&ipc.Channel{}).Where("id = ?", id).Updates(map[string]any{"channel_id": codeChannel, "device_id": "untrusted-redundant-code"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	h := &codeHost{}
	s.Host = h
	return r, s, p, key, h
}
func codeRequest(r *gin.Engine, p op.Principal, key []byte, method, path, query, body, idem string) *httptest.ResponseRecorder {
	req := signed(p, key, method, path, query, []byte(body), op.Random(24))
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
		req.Header.Set("Authorization", "OWL-HMAC-SHA256 Credential="+p.Credential.AccessKey+",Signature="+op.Sign(key, op.Canonical("owl-test", method, path, query, req.Header.Get("Content-Type"), req.Header.Get("X-Owl-Timestamp"), req.Header.Get("X-Owl-Nonce"), idem, op.Hash([]byte(body)))))
	}
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	return out
}
func TestResourceCodesLookupFiltersAndIsolation(t *testing.T) {
	r, s, p, key, _ := codeFixture(t)
	for _, tc := range []struct {
		path, query, want string
		status            int
	}{
		{"/openapi/v1/devices/by-code/" + codeDevice, "", `"id":"d1"`, 200},
		{codePath, "", `"id":"c1"`, 200},
		{"/openapi/v1/devices", "device_code=" + codeDevice, `"id":"d1"`, 200},
		{"/openapi/v1/channels", "device_code=" + codeDevice + "&channel_code=" + codeChannel, `"id":"c1"`, 200},
		{"/openapi/v1/devices/by-code/" + otherDevice, "", "NOT_FOUND", 404},
		{"/openapi/v1/devices/by-code/" + otherDevice + "/channels/" + codeChannel, "", "NOT_FOUND", 404},
		{codePath + "0", "", "NOT_FOUND", 404},
		{"/openapi/v1/devices", "device_code=" + otherDevice, `"items":[]`, 200},
		{"/openapi/v1/channels", "device_code=" + otherDevice + "&channel_code=" + codeChannel, `"items":[]`, 200},
		{"/openapi/v1/channels", "device_id=d1", `"id":"c1"`, 200},
		{"/openapi/v1/channels/c1", "", `"id":"c1"`, 200},
	} {
		out := codeRequest(r, p, key, http.MethodGet, tc.path, tc.query, "", "")
		if out.Code != tc.status || !strings.Contains(out.Body.String(), tc.want) || strings.Contains(out.Body.String(), "MUST_NOT_LEAK") {
			t.Fatalf("%s?%s: %d %s", tc.path, tc.query, out.Code, out.Body.String())
		}
	}
	// 设备编码不可冒充内部设备 ID；跨设备相同通道编码不会被错误匹配。
	if err := s.DB.Model(&op.App{}).Where("id = ?", p.App.ID).Update("scopes", `["ptz:control"]`).Error; err != nil {
		t.Fatal(err)
	}
	out := codeRequest(r, p, key, "GET", codePath, "", "", "")
	if out.Code != 403 {
		t.Fatal(out.Code, out.Body.String())
	}
	out = codeRequest(r, p, key, "GET", codePath+"/ptz/capabilities", "", "", "")
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	out = codeRequest(r, p, key, "GET", strings.Replace(codePath, codeDevice, otherDevice, 1)+"/ptz/capabilities", "", "", "")
	if out.Code != 404 {
		t.Fatal(out.Code, out.Body.String())
	}
}
func TestResourceCodesRejectAmbiguityBeforeControl(t *testing.T) {
	r, s, p, key, h := codeFixture(t)
	if err := s.DB.Create(&ipc.Channel{ID: "duplicate", DID: "d1", ChannelID: codeChannel}).Error; err != nil {
		t.Fatal(err)
	}
	out := codeRequest(r, p, key, "POST", codePath+"/ptz", "", `{"action":"left","duration":1}`, "ambiguous")
	if out.Code != 409 || !strings.Contains(out.Body.String(), "AMBIGUOUS_RESOURCE_CODE") {
		t.Fatal(out.Code, out.Body.String())
	}
	if len(h.ptzChannels) != 0 || len(h.capabilities) != 0 {
		t.Fatal("ambiguous selector reached host")
	}
}
func TestResourceCodesControlPlaybackSnapshotAndIdempotency(t *testing.T) {
	r, s, p, key, h := codeFixture(t)
	call := func(method, path, body, idem string) map[string]any {
		t.Helper()
		out := codeRequest(r, p, key, method, path, "", body, idem)
		if out.Code != 200 {
			t.Fatal(path, out.Code, out.Body.String())
		}
		var response struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.Data
	}
	command := call("POST", codePath+"/ptz", `{"action":"left","speed":30,"duration":1}`, "move")
	repeat := call("POST", codePath+"/ptz", `{"action":"left","speed":30,"duration":1}`, "move")
	if command["id"] != repeat["id"] || command["channel_id"] != "c1" || len(h.ptzChannels) != 1 || h.ptzChannels[0] != "c1" {
		t.Fatal(command, repeat, h.ptzChannels)
	}
	out := codeRequest(r, p, key, "POST", codePath+"/ptz", "", `{"action":"right","duration":1}`, "move")
	if out.Code != 409 || !strings.Contains(out.Body.String(), "IDEMPOTENCY_MISMATCH") {
		t.Fatal(out.Code, out.Body.String())
	}
	call("POST", "/openapi/v1/ptz-commands/"+command["id"].(string)+"/stop", `{}`, "")
	if len(h.ptzChannels) != 2 {
		t.Fatal("original stop flow was not used", h.ptzChannels)
	}
	session := call("POST", codePath+"/live-sessions", `{"protocol":"hls"}`, "live")
	if session["channel_id"] != "c1" || len(h.started) != 1 || h.started[0] != "c1" {
		t.Fatal(session, h.started)
	}
	out = codeRequest(r, p, key, "POST", codePath+"/live-sessions", "", `{"channel_id":"c2","protocol":"hls"}`, "wrong")
	if out.Code != 400 || !strings.Contains(out.Body.String(), "INVALID_RESOURCE_SELECTOR") {
		t.Fatal(out.Code, out.Body.String())
	}
	s.Start(context.Background())
	t.Cleanup(s.Close)
	snapshot := call("POST", codePath+"/snapshot-tasks", `{}`, "snapshot")
	if snapshot["channel_id"] != "c1" {
		t.Fatal(snapshot)
	}
	s.Close() // 等待模拟抓拍结束，不能让异步任务逃出测试生命周期。
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.captured) != 1 || h.captured[0] != "c1" {
		t.Fatal(h.captured)
	}
}

func TestResourceCodesDoNotTransferAuthorizationToRecreatedDevice(t *testing.T) {
	r, s, p, key, h := codeFixture(t)
	for _, suffix := range []string{"/ptz", "/live-sessions", "/snapshot-tasks"} {
		out := codeRequest(r, p, key, "POST", strings.Replace(codePath, codeDevice, otherDevice, 1)+suffix, "", `{}`, "denied")
		if out.Code != 404 {
			t.Fatal(suffix, out.Code, out.Body.String())
		}
	}
	if err := s.DB.Delete(&ipc.Device{}, "id = ?", "d1").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&ipc.Device{ID: "replacement", DeviceID: codeDevice}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&ipc.Channel{ID: "replacement-channel", DID: "replacement", ChannelID: codeChannel}).Error; err != nil {
		t.Fatal(err)
	}
	out := codeRequest(r, p, key, "GET", codePath, "", "", "")
	if out.Code != 404 {
		t.Fatal("authorization transferred by code", out.Code, out.Body.String())
	}
	if len(h.ptzChannels)+len(h.started)+len(h.captured) != 0 {
		t.Fatal("unauthorized operation reached host")
	}
}
