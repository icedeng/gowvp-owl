package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/event/store/eventdb"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

func TestEventWorkflowAPIAuthenticationIdentityAndPolicyBinding(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	store := eventdb.NewDB(db).AutoMigrate(true)
	w := event.NewWorkflow(db)
	core := event.NewCore(store, nil).WithWorkflow(w)
	db.Exec("CREATE TABLE devices (id text PRIMARY KEY,name text,device_id text,ext text,type text)")
	db.Exec("CREATE TABLE channels (id text PRIMARY KEY,did text,name text,channel_id text)")
	db.Exec("INSERT INTO devices VALUES ('device','大门摄像机','34020000001320000101','{}','GB28181')")
	db.Exec("INSERT INTO channels VALUES ('channel','device','入口','34020000001320000001')")
	cfg := conf.DefaultConfig()
	api := NewEventAPI(core, &cfg)
	r := gin.New()
	RegisterEvent(r, api, AuthMiddleware("event-test-secret", ""))
	token, err := web.NewToken(web.NewClaimsData().SetUsername("alice"), "event-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string, auth bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if auth {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		return out
	}
	for _, path := range []string{"/events", "/events/1/actions", "/events/1/video", "/event_policies/device"} {
		if out := request(http.MethodGet, path, "", false); out.Code != 401 {
			t.Fatal(path, out.Code, out.Body.String())
		}
	}
	if out := request(http.MethodPost, "/events/process", `{"items":[{"id":1,"revision":1}],"status":"processing"}`, false); out.Code != 401 {
		t.Fatal(out.Code)
	}
	out := request(http.MethodPut, "/event_policies/device", `{"did":"attacker-device","snapshot":true,"record":false,"clip_seconds":15,"cooldown_seconds":60,"revision":0,"updated_by":"forged"}`, true)
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	p, err := w.Policy(t.Context(), "device")
	if err != nil || !p.Snapshot || p.UpdatedBy != "alice" {
		t.Fatal(p, err)
	}
	e, err := core.CreateEvent(t.Context(), &event.AddEventInput{DID: "device", CID: "channel", Model: "GB28181", StartedAt: orm.Now()})
	if err != nil {
		t.Fatal(err)
	}
	out = request(http.MethodGet, "/events?key=大门摄像机&status=pending&page=1&size=20", "", true)
	var page struct {
		Items []event.Event
		Total int
	}
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &page) != nil || page.Total != 1 || page.Items[0].DeviceName != "大门摄像机" || page.Items[0].ChannelName != "入口" {
		t.Fatal(out.Code, out.Body.String())
	}
	body := fmt.Sprintf(`{"items":[{"id":%d,"revision":1}],"status":"resolved","note":"已核实","actor":"forged"}`, e.ID)
	out = request(http.MethodPost, "/events/process", body, true)
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	history, err := w.Actions(t.Context(), e.ID)
	if err != nil || len(history) != 1 || history[0].Actor != "alice" {
		t.Fatal(history, err)
	}
	out = request(http.MethodPost, "/events/process", body, true)
	if out.Code == 200 {
		t.Fatal("stale event accepted")
	}
}

func TestEventVideoServesRangesAndRejectsOutsideFiles(t *testing.T) {
	t.Chdir(t.TempDir())
	root := filepath.Join("configs", "events")
	if err := os.MkdirAll(filepath.Join(root, "clips"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "clips", "one.mp4"), []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(sqlite.Open("file:event-video-range?mode=memory&cache=shared"))
	if err != nil {
		t.Fatal(err)
	}
	store := eventdb.NewDB(db).AutoMigrate(true)
	core := event.NewCore(store, nil)
	e, err := core.CreateEvent(t.Context(), &event.AddEventInput{StartedAt: orm.Now()})
	if err != nil {
		t.Fatal(err)
	}
	cfg := conf.DefaultConfig()
	a := NewEventAPI(core, &cfg)
	r := gin.New()
	RegisterEvent(r, a)
	db.Model(e).Update("video_path", "clips/one.mp4")
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/events/%d/video", e.ID), nil)
	req.Header.Set("Range", "bytes=2-5")
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != http.StatusPartialContent || out.Body.String() != "2345" || out.Header().Get("Content-Type") != "video/mp4" {
		t.Fatal(out.Code, out.Body.String())
	}
	outside := filepath.Join(t.TempDir(), "outside.mp4")
	os.WriteFile(outside, []byte("private"), 0600)
	if err = os.Symlink(outside, filepath.Join(root, "escape.mp4")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../outside.mp4", outside, "escape.mp4", "clips"} {
		db.Model(e).Update("video_path", path)
		out = httptest.NewRecorder()
		r.ServeHTTP(out, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/events/%d/video", e.ID), nil))
		if out.Code == 200 || out.Code == 206 {
			t.Fatal("unsafe video path accepted", path)
		}
	}
}
