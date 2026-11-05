package openapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gowvp/owl/internal/core/capture"
	op "github.com/gowvp/owl/internal/core/openplatform"
)

type snapshotFileHost struct {
	op.Host
	file    string
	started chan struct{}
	release chan struct{}
	count   atomic.Int32
}

func (h *snapshotFileHost) Snapshot(ctx context.Context, _ string, dest string) error {
	h.count.Add(1)
	if h.started != nil {
		close(h.started)
	}
	if h.release != nil {
		select {
		case <-h.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return os.WriteFile(dest, []byte("test-image-bytes"), 0600)
}
func (h *snapshotFileHost) CapturePath(context.Context, string) (string, error) { return h.file, nil }
func resultData(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var response struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	return response.Data
}
func mediaRequest(r *gin.Engine, raw string) *httptest.ResponseRecorder {
	u, _ := url.Parse(raw)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", u.RequestURI(), nil))
	return w
}
func TestSnapshotSyncAndAsyncImageAddresses(t *testing.T) {
	for _, mode := range []string{"sync", "async"} {
		t.Run(mode, func(t *testing.T) {
			r, s, p, key, _ := codeFixture(t)
			h := &snapshotFileHost{started: make(chan struct{}), release: make(chan struct{})}
			s.Host = h
			t.Cleanup(s.Close)
			s.Start(context.Background())
			body := `{"mode":"` + mode + `"}`
			if mode == "sync" {
				close(h.release)
			}
			out := codeRequest(r, p, key, "POST", codePath+"/snapshot-tasks", "", body, "once")
			if out.Code != 200 {
				t.Fatal(out.Code, out.Body.String())
			}
			row := resultData(t, out.Body.Bytes())
			imageURL, _ := row["image_url"].(string)
			if imageURL == "" || row["image_expires_at"] == nil {
				t.Fatal(row)
			}
			if mode == "async" {
				<-h.started
				pending := mediaRequest(r, imageURL)
				if pending.Code != 409 || pending.Header().Get("Retry-After") != "1" || !strings.Contains(pending.Body.String(), "SNAPSHOT_PENDING") {
					t.Fatal(pending.Code, pending.Body.String())
				}
				close(h.release)
			} else if row["state"] != "succeeded" {
				t.Fatal(row)
			}
			// 等待后台写入完成，同一个异步返回地址随后可直接取图。
			deadline := time.Now().Add(3 * time.Second)
			for {
				image := mediaRequest(r, imageURL)
				if image.Code == 200 {
					if image.Body.String() != "test-image-bytes" {
						t.Fatal(image.Body.String())
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal(image.Code, image.Body.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			repeat := codeRequest(r, p, key, "POST", codePath+"/snapshot-tasks", "", body, "once")
			if repeat.Code != 200 || resultData(t, repeat.Body.Bytes())["id"] != row["id"] || h.count.Load() != 1 {
				t.Fatal(repeat.Code, repeat.Body.String(), h.count.Load())
			}
			detail := codeRequest(r, p, key, "GET", "/openapi/v1/snapshot-tasks/"+row["id"].(string), "", "", "")
			if detail.Code != 200 || resultData(t, detail.Body.Bytes())["image_url"] == nil {
				t.Fatal(detail.Body.String())
			}
			s.DB.Model(&op.App{}).Where("id = ?", p.App.ID).Update("device_ids", `["d2"]`)
			denied := mediaRequest(r, imageURL)
			if denied.Code == 200 {
				t.Fatal("revoked device could read image")
			}
		})
	}
}
func TestSnapshotSyncTimeoutKeepsSingleTask(t *testing.T) {
	r, s, p, key, _ := codeFixture(t)
	h := &snapshotFileHost{release: make(chan struct{})}
	s.Host = h
	s.Start(context.Background())
	t.Cleanup(s.Close)
	out := codeRequest(r, p, key, "POST", codePath+"/snapshot-tasks", "", `{"mode":"sync","timeout_seconds":1}`, "timeout")
	if out.Code != 504 || !strings.Contains(out.Body.String(), "SNAPSHOT_TIMEOUT") {
		t.Fatal(out.Code, out.Body.String())
	}
	close(h.release)
	s.Close()
	out = codeRequest(r, p, key, "POST", codePath+"/snapshot-tasks", "", `{"mode":"sync","timeout_seconds":1}`, "timeout")
	if out.Code != 200 || h.count.Load() != 1 {
		t.Fatal(out.Code, out.Body.String(), h.count.Load())
	}
}
func TestPersistentCapturesAuthorizationURLsAndDeletion(t *testing.T) {
	r, s, p, key, _ := codeFixture(t)
	if err := s.DB.AutoMigrate(&capture.Record{}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "capture.jpg")
	os.WriteFile(file, []byte("permanent-picture"), 0600)
	s.Host = &snapshotFileHost{file: file}
	id := uuid.NewString()
	for _, row := range []capture.Record{
		{ID: id, ChannelID: "c1", State: "succeeded", Source: "schedule", Name: "picture.jpg", RequestURL: "MUST_NOT_LEAK"},
		{ID: uuid.NewString(), ChannelID: "c2", State: "succeeded"},
		{ID: uuid.NewString(), ChannelID: "c1", State: "running"},
	} {
		if err := s.DB.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	var imageURL string
	for _, path := range []string{"/openapi/v1/captures", "/openapi/v1/channels/c1/captures", "/openapi/v1/devices/by-code/" + codeDevice + "/captures", codePath + "/captures"} {
		out := codeRequest(r, p, key, "GET", path, "", "", "")
		if out.Code != 200 || strings.Contains(out.Body.String(), "MUST_NOT_LEAK") {
			t.Fatal(path, out.Code, out.Body.String())
		}
		data := resultData(t, out.Body.Bytes())
		items := data["items"].([]any)
		if len(items) != 1 {
			t.Fatal(path, items)
		}
		item := items[0].(map[string]any)
		if item["id"] != id || item["image_url"] == nil {
			t.Fatal(item)
		}
		imageURL = item["image_url"].(string)
	}
	detail := codeRequest(r, p, key, "GET", "/openapi/v1/captures/"+id, "", "", "")
	if detail.Code != 200 || resultData(t, detail.Body.Bytes())["image_url"] == nil {
		t.Fatal(detail.Code, detail.Body.String())
	}
	image := mediaRequest(r, imageURL)
	if image.Code != 200 || image.Body.String() != "permanent-picture" {
		t.Fatal(image.Code, image.Body.String())
	}
	out := codeRequest(r, p, key, "GET", "/openapi/v1/captures", "device_code="+otherDevice, "", "")
	if out.Code != 200 || len(resultData(t, out.Body.Bytes())["items"].([]any)) != 0 {
		t.Fatal(out.Code, out.Body.String())
	}
	s.DB.Model(&capture.Record{}).Where("id = ?", id).Update("state", "deleting")
	if mediaRequest(r, imageURL).Code == 200 {
		t.Fatal("deleted image still readable")
	}
	s.DB.Model(&capture.Record{}).Where("id = ?", id).Update("state", "succeeded")
	s.DB.Model(&op.App{}).Where("id = ?", p.App.ID).Update("device_ids", `["d2"]`)
	if mediaRequest(r, imageURL).Code == 200 {
		t.Fatal("revoked image still readable")
	}
	if codeRequest(r, p, key, "GET", "/openapi/v1/captures/"+id, "", "", "").Code != 404 {
		t.Fatal("capture detail authorization bypass")
	}
}

func TestSnapshotInvalidOptionsAndCaptureScope(t *testing.T) {
	r, s, p, key, _ := codeFixture(t)
	for _, body := range []string{`{"mode":"invalid"}`, `{"mode":"sync","timeout_seconds":-1}`, `{"mode":"sync","timeout_seconds":121}`} {
		out := codeRequest(r, p, key, "POST", codePath+"/snapshot-tasks", "", body, "invalid")
		if out.Code != 400 {
			t.Fatal(out.Code, out.Body.String())
		}
	}
	s.DB.Model(&op.App{}).Where("id = ?", p.App.ID).Update("scopes", `["channel:read"]`)
	for _, path := range []string{"/openapi/v1/captures", codePath + "/captures", "/openapi/v1/captures/unknown"} {
		if out := codeRequest(r, p, key, "GET", path, "", "", ""); out.Code != 403 {
			t.Fatal(path, out.Code)
		}
	}
}
