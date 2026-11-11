package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/internal/core/sms/store/smsdb"
	"github.com/gowvp/owl/pkg/zlm"
	"gorm.io/gorm"
)

func TestMediaServerManagementLifecycle(t *testing.T) {
	var hasStreams atomic.Bool
	var setups atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index/api/getServerConfig":
			fmt.Fprint(w, `{"code":0,"data":[{"rtc.port":"30000","rtsp.port":"554"}]}`)
		case "/index/api/setServerConfig":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			hook, err := url.Parse(fmt.Sprint(body["hook.on_server_keepalive"]))
			if err != nil || hook.Query().Get("media_server_id") != "edge-1" || hook.Query().Get("secret") != "node-secret" {
				t.Errorf("invalid node callback: %v", hook)
			}
			setups.Add(1)
			fmt.Fprint(w, `{"code":0}`)
		case "/index/api/getMediaList":
			if hasStreams.Load() {
				fmt.Fprint(w, `{"code":0,"data":[{"app":"rtp","stream":"busy"}]}`)
			} else {
				fmt.Fprint(w, `{"code":0,"data":[]}`)
			}
		case "/index/api/listRtpServer":
			fmt.Fprint(w, `{"code":0,"data":[]}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer upstream.Close()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	store := smsdb.NewDB(db).AutoMigrate(true)
	if err := db.AutoMigrate(&ipc.Channel{}, &recording.Recording{}); err != nil {
		t.Fatal(err)
	}
	if err := store.MediaServer().Create(t.Context(), &sms.MediaServer{ID: "local", IP: "192.0.2.1", Secret: "global-secret"}); err != nil {
		t.Fatal(err)
	}
	core := sms.NewCore(store)
	defer core.Close()
	api := NewSmsAPI(core)
	cfg := conf.DefaultConfig()
	cfg.ConfigPath = t.TempDir() + "/config.toml"
	api.uc = &Usecase{Conf: &cfg, DB: db}
	router := gin.New()
	registerSms(router, api, func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer test" {
			c.AbortWithStatus(401)
		}
	})
	request := func(method, path string, body any, auth bool) *httptest.ResponseRecorder {
		t.Helper()
		data, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(data)))
		req.Header.Set("Content-Type", "application/json")
		if auth {
			req.Header.Set("Authorization", "Bearer test")
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	mustOK := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		rec := request(method, path, body, true)
		if rec.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "node-secret") || strings.Contains(rec.Body.String(), "global-secret") {
			t.Fatalf("secret leaked: %s", rec.Body.String())
		}
		return rec
	}
	mustFail := func(method, path string, body any) {
		t.Helper()
		rec := request(method, path, body, true)
		if rec.Code < 400 {
			t.Fatalf("unexpected success %s %s: %s", method, path, rec.Body.String())
		}
	}
	host, port, _ := net.SplitHostPort(upstream.Listener.Addr().String())
	portNum, _ := strconv.Atoi(port)
	body := map[string]any{"id": "edge-1", "ip": host, "http_port": portNum, "hook_ip": "192.0.2.10", "sdp_ip": "192.0.2.20", "secret": "node-secret", "type": "zlm", "rtpport_range": "30000-50000"}
	for _, action := range []struct{ method, path string }{{"POST", "/media_servers"}, {"PUT", "/media_servers/edge-1"}, {"PUT", "/media_servers/edge-1/state"}, {"DELETE", "/media_servers/edge-1"}} {
		if rec := request(action.method, action.path, body, false); rec.Code != 401 {
			t.Fatalf("unprotected route: %s", action.path)
		}
	}
	mustOK("POST", "/media_servers", body)
	if !core.IsOnline("edge-1") {
		t.Fatal("created node was not connected")
	}
	mustFail("POST", "/media_servers", body)
	body["id"] = "another"
	mustFail("POST", "/media_servers", body) // 同一个 ZLM 不得被两个标识覆盖配置。
	body["id"] = "local"
	mustFail("POST", "/media_servers", body)
	body["id"] = "bad/id"
	mustFail("POST", "/media_servers", body)
	body["id"] = "another"
	body["http_port"] = 70000
	mustFail("POST", "/media_servers", body)
	body["id"] = "edge-1"
	body["http_port"] = portNum
	body["secret"] = ""
	mustOK("PUT", "/media_servers/edge-1", body)
	saved, _ := core.GetMediaServer(t.Context(), "edge-1")
	if saved.Secret != "node-secret" {
		t.Fatal("empty edit erased secret")
	}
	mustFail("DELETE", "/media_servers/local", nil)
	mustFail("DELETE", "/media_servers/edge-1", nil)
	hasStreams.Store(true)
	mustFail("PUT", "/media_servers/edge-1/state", map[string]any{"disabled": true})
	mustFail("PUT", "/media_servers/edge-1", body)
	hasStreams.Store(false)
	ch := &ipc.Channel{ID: "ch-bound", IsPlaying: true, Config: ipc.StreamConfig{MediaServerID: "edge-1"}}
	if err := db.Create(ch).Error; err != nil {
		t.Fatal(err)
	}
	mustFail("PUT", "/media_servers/edge-1/state", map[string]any{"disabled": true})
	if err := db.Model(ch).Update("is_playing", false).Error; err != nil {
		t.Fatal(err)
	}
	mustFail("PUT", "/media_servers/edge-1/state", map[string]any{})
	mustOK("PUT", "/media_servers/edge-1/state", map[string]any{"disabled": true})
	if core.IsOnline("edge-1") {
		t.Fatal("disabled node remains online")
	}
	core.Keepalive("edge-1")
	if core.IsOnline("edge-1") {
		t.Fatal("keepalive revived disabled node")
	}
	if _, err := core.OpenRTPServerContext(context.Background(), saved, zlm.OpenRTPServerRequest{}); err == nil {
		t.Fatal("disabled node accepted new RTP operation from stale snapshot")
	}
	if !strings.Contains(mustOK("GET", "/media_servers/edge-1", nil).Body.String(), `"disabled":true`) {
		t.Fatal("disabled state missing from detail")
	}
	mustFail("DELETE", "/media_servers/edge-1", nil) // 停用节点仍保留引用保护。
	before := setups.Load()
	core2 := sms.NewCore(store)
	restartCfg := conf.DefaultConfig()
	restartCfg.Media.Secret = "global-secret"
	if err := db.Model(&sms.MediaServer{}).Where("id = ?", "local").Update("disabled", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := core2.Run(&restartCfg, 15123); err != nil {
		t.Fatal(err)
	}
	core2.Close()
	if setups.Load() != before {
		t.Fatal("restart reconnected disabled node")
	}
	node, _ := core.GetMediaServer(t.Context(), "edge-1")
	if !node.Disabled {
		t.Fatal("disabled state was lost on restart")
	}
	mustOK("PUT", "/media_servers/edge-1/state", map[string]any{"disabled": false})
	if !core.IsOnline("edge-1") {
		t.Fatal("enable did not reconnect")
	}
	mustOK("PUT", "/media_servers/edge-1/state", map[string]any{"disabled": true})
	if err := db.Delete(ch).Error; err != nil {
		t.Fatal(err)
	}
	mustOK("DELETE", "/media_servers/edge-1", nil)
	mustFail("GET", "/media_servers/edge-1", nil)
}

func TestMediaServerWebhookCredentialsAndScope(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	store := smsdb.NewDB(db).AutoMigrate(true)
	for _, node := range []*sms.MediaServer{{ID: "local", Secret: "local-secret"}, {ID: "edge", Secret: "edge-secret"}, {ID: "disabled", Secret: "disabled-secret", Disabled: true}} {
		if err := store.MediaServer().Create(t.Context(), node); err != nil {
			t.Fatal(err)
		}
	}
	core := sms.NewCore(store)
	defer core.Close()
	cfg := conf.DefaultConfig()
	cfg.Media.Secret = "local-secret"
	hook := WebHookAPI{conf: &cfg, smsCore: core}
	router := gin.New()
	router.POST("/hook", hook.mediaServerWebhookAuth(), func(c *gin.Context) { c.Status(204) })
	for _, tc := range []struct {
		query, body string
		want        int
	}{
		{"secret=local-secret", `{}`, 204},
		{"media_server_id=edge&secret=edge-secret", `{"mediaServerId":"edge"}`, 204},
		{"media_server_id=edge&secret=local-secret", `{"mediaServerId":"edge"}`, 401},
		{"media_server_id=edge&secret=edge-secret", `{"mediaServerId":"local"}`, 401},
		{"secret=local-secret", `{"mediaServerId":"edge"}`, 401},
		{"media_server_id=disabled&secret=disabled-secret", `{}`, 401},
		{"media_server_id=missing&secret=local-secret", `{}`, 401},
	} {
		req := httptest.NewRequest("POST", "/hook?"+tc.query, strings.NewReader(tc.body))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("query %s: got %d want %d", tc.query, rec.Code, tc.want)
		}
	}
}
