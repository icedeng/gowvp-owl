package openapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gowvp/owl/internal/core/event"
	op "github.com/gowvp/owl/internal/core/openplatform"
	"github.com/ixugo/goddd/pkg/orm"
)

type eventMediaHost struct {
	op.Host
	image, thumbnail, video string
}

func (h eventMediaHost) EventImagePath(context.Context, int64) (string, error) { return h.image, nil }
func (h eventMediaHost) EventMediaPath(_ context.Context, _ int64, kind string) (string, error) {
	if kind == "event-thumbnail" {
		return h.thumbnail, nil
	}
	return h.video, nil
}

func TestOpenEventEvidenceTicketsProcessingAndScopeIsolation(t *testing.T) {
	r, s, p, key := apiFixture(t)
	if err := s.InstallOutbox(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	host := eventMediaHost{image: filepath.Join(dir, "original.jpg"), thumbnail: filepath.Join(dir, "thumbnail.jpg"), video: filepath.Join(dir, "video.mp4")}
	for path, body := range map[string]string{host.image: "original-image", host.thumbnail: "small-image", host.video: "0123456789-video"} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s.Host = host
	ev := event.Event{DID: "d1", CID: "c1", StartedAt: orm.Now(), EndedAt: orm.Now(), ImagePath: "PRIVATE/photo.jpg", VideoPath: "PRIVATE/clip.mp4"}
	if err := s.DB.Create(&ev).Error; err != nil {
		t.Fatal(err)
	}
	denied := event.Event{DID: "d2", CID: "c2", ImagePath: "PRIVATE/photo.jpg", VideoPath: "PRIVATE/clip.mp4"}
	s.DB.Create(&denied)
	request := func(method, path, body, idem string) *httptest.ResponseRecorder {
		t.Helper()
		req := signed(p, key, method, path, "", []byte(body), op.Random(24))
		// Idempotency-Key属于签名，使用现有测试签名助手后重新签名。
		if idem != "" {
			req.Header.Set("Idempotency-Key", idem)
			req.Header.Set("Authorization", "OWL-HMAC-SHA256 Credential="+p.Credential.AccessKey+",Signature="+op.Sign(key, op.Canonical("owl-test", method, path, "", req.Header.Get("Content-Type"), req.Header.Get("X-Owl-Timestamp"), req.Header.Get("X-Owl-Nonce"), idem, op.Hash([]byte(body)))))
		}
		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		return out
	}
	out := request("GET", "/openapi/v1/events/1", "", "")
	if out.Code != 200 || strings.Contains(out.Body.String(), "PRIVATE") || !strings.Contains(out.Body.String(), `"revision":1`) || !strings.Contains(out.Body.String(), `"has_video":true`) {
		t.Fatal(out.Code, out.Body.String())
	}
	ticket := func(suffix, body string) string {
		t.Helper()
		o := request("POST", "/openapi/v1/events/1/"+suffix, body, "")
		var v struct {
			Data struct {
				URL string `json:"url"`
			} `json:"data"`
		}
		if o.Code != 200 || json.Unmarshal(o.Body.Bytes(), &v) != nil || v.Data.URL == "" {
			t.Fatal(o.Code, o.Body.String())
		}
		return v.Data.URL
	}
	media := func(url, rangeHeader string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", url, nil)
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		o := httptest.NewRecorder()
		r.ServeHTTP(o, req)
		return o
	}
	original := ticket("image-tickets", `{}`)
	thumb := ticket("thumbnail-tickets", `{}`)
	video := ticket("video-tickets", `{"purpose":"play"}`)
	download := ticket("video-tickets", `{"purpose":"download"}`)
	for url, want := range map[string]string{original: "original-image", thumb: "small-image"} {
		o := media(url, "")
		if o.Code != 200 || o.Body.String() != want {
			t.Fatal(o.Code, o.Body.String())
		}
	}
	o := media(video, "bytes=2-5")
	if o.Code != 206 || o.Body.String() != "2345" || o.Header().Get("Content-Type") != "video/mp4" || o.Header().Get("Content-Disposition") != "" {
		t.Fatal(o.Code, o.Header(), o.Body.String())
	}
	o = media(download, "")
	if o.Code != 200 || !strings.Contains(o.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal(o.Code, o.Header())
	}
	o = media(strings.Replace(video, "/event-video/", "/event-download/", 1), "")
	if o.Code != 401 {
		t.Fatal("播放票据升级下载", o.Code)
	}
	o = media(video, "bytes=0-1,3-4")
	if o.Code != 400 {
		t.Fatal("多段Range", o.Code)
	}
	for _, suffix := range []string{"image-tickets", "thumbnail-tickets", "video-tickets"} {
		body := `{}`
		if suffix == "video-tickets" {
			body = `{"purpose":"play"}`
		}
		o = request("POST", "/openapi/v1/events/2/"+suffix, body, "")
		if o.Code != 404 {
			t.Fatal("越权取证", suffix, o.Code, o.Body.String())
		}
	}
	body := `{"revision":1,"status":"processing","note":"核实报警"}`
	o = request("POST", "/openapi/v1/events/1/process", body, "process-1")
	if o.Code != 200 {
		t.Fatal(o.Code, o.Body.String())
	}
	first := o.Body.String()
	o = request("POST", "/openapi/v1/events/1/process", body, "process-1")
	if o.Code != 200 || !strings.Contains(o.Body.String(), `"action_id":1`) || !strings.Contains(first, `"action_id":1`) {
		t.Fatal(o.Code, o.Body.String())
	}
	o = request("POST", "/openapi/v1/events/1/process", `{"revision":1,"status":"resolved","note":"已完成"}`, "process-2")
	if o.Code != 409 {
		t.Fatal(o.Code, o.Body.String())
	}
	o = request("GET", "/openapi/v1/events/1/actions", "", "")
	if o.Code != 200 || !strings.Contains(o.Body.String(), "open-app:") {
		t.Fatal(o.Code, o.Body.String())
	}
	o = request("POST", "/openapi/v1/events/2/process", body, "process-other")
	if o.Code != 404 {
		t.Fatal("越权处理", o.Code, o.Body.String())
	}
	// 仅图片权限无法播放视频或处理事件；已签发视频票据随撤权失效。
	if err := s.DB.Model(&op.App{}).Where("id = ?", p.App.ID).Updates(map[string]any{"scopes": `["event:read","event:image"]`, "revision": p.App.Revision + 1}).Error; err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"video-tickets", "process"} {
		b := `{"purpose":"play"}`
		if path == "process" {
			b = body
		}
		o = request("POST", "/openapi/v1/events/1/"+path, b, "other")
		if o.Code != 403 {
			t.Fatal("新权限未隔离", path, o.Code, o.Body.String())
		}
	}
	o = media(video, "")
	if o.Code < 400 {
		t.Fatal("撤权后仍能读视频", o.Code)
	}
}
