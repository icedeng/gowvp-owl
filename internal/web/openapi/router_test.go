package openapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/ipc"
	op "github.com/gowvp/owl/internal/core/openplatform"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func apiFixture(t *testing.T) (*gin.Engine, *op.Service, op.Principal, []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, e := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal(e)
	}
	rawDB, _ := db.DB()
	rawDB.SetMaxOpenConns(1)
	t.Cleanup(func() { rawDB.Close() })
	if e = db.AutoMigrate(&ipc.Device{}, &ipc.Channel{}); e != nil {
		t.Fatal(e)
	}
	if e = op.Migrate(db); e != nil {
		t.Fatal(e)
	}
	s, e := op.New(db, op.Config{Enabled: true, Audience: "owl-test", KeyID: "v1", Keys: map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)}, DataDir: t.TempDir(), PublicBaseURL: "https://open.example"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, d := range []ipc.Device{{ID: "d1", DeviceID: "gb1", Password: "MUST_NOT_LEAK"}, {ID: "d2", DeviceID: "gb2"}} {
		db.Create(&d)
	}
	for _, ch := range []ipc.Channel{{ID: "c1", DID: "d1"}, {ID: "c2", DID: "d2"}} {
		db.Create(&ch)
	}
	a := op.DefaultApp()
	a.Name = "api-test"
	a.DeviceIDs = []string{"d1"}
	a.AllowAnyIP = true
	a.Scopes = op.Scopes
	a, e = s.SaveApp(a, true, "admin")
	if e != nil {
		t.Fatal(e)
	}
	c, secret, e := s.CreateCredential(a.ID, nil, "admin")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SetStatus(a.ID, "enabled", "admin"); e != nil {
		t.Fatal(e)
	}
	p, e := s.Current(a.ID, c.ID)
	if e != nil {
		t.Fatal(e)
	}
	key, _ := op.ValidateSecret(secret)
	r := gin.New()
	New(s).Register(r, func(c *gin.Context) { c.AbortWithStatus(401) })
	return r, s, p, key
}
func signed(p op.Principal, key []byte, method, path, rawQuery string, body []byte, nonce string) *http.Request {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	ct := ""
	if method != "GET" && method != "DELETE" {
		ct = "application/json"
	}
	digest := op.Hash(body)
	query, _, _ := op.CanonicalQuery(rawQuery)
	req := httptest.NewRequest(method, path+func() string {
		if rawQuery != "" {
			return "?" + rawQuery
		}
		return ""
	}(), bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Owl-Audience", "owl-test")
	req.Header.Set("X-Owl-Timestamp", ts)
	req.Header.Set("X-Owl-Nonce", nonce)
	req.Header.Set("X-Owl-Content-SHA256", digest)
	req.Header.Set("Authorization", "OWL-HMAC-SHA256 Credential="+p.Credential.AccessKey+",Signature="+op.Sign(key, op.Canonical("owl-test", method, path, query, ct, ts, nonce, "", digest)))
	return req
}
func TestSignedAPIIsolationReplayAndResourceFiltering(t *testing.T) {
	r, s, p, key := apiFixture(t)
	req := signed(p, key, "GET", "/openapi/v1/devices", "", nil, op.Random(24))
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != 200 || strings.Contains(out.Body.String(), "MUST_NOT_LEAK") || strings.Contains(out.Body.String(), "gb2") || !strings.Contains(out.Body.String(), "gb1") {
		t.Fatal(out.Code, out.Body.String())
	}
	replay := httptest.NewRecorder()
	r.ServeHTTP(replay, req.Clone(context.Background()))
	if replay.Code != 401 {
		t.Fatal("nonce replay", replay.Code)
	}
	out = httptest.NewRecorder()
	r.ServeHTTP(out, signed(p, key, "GET", "/openapi/v1/devices/d2", "", nil, op.Random(24)))
	if out.Code != 404 {
		t.Fatal(out.Code, out.Body.String())
	}
	out = httptest.NewRecorder()
	bad := signed(p, key, "GET", "/openapi/v1/devices", "", nil, op.Random(24))
	bad.Header.Set("Authorization", "Bearer admin-token")
	r.ServeHTTP(out, bad)
	if out.Code != 401 {
		t.Fatal("JWT accepted", out.Code)
	}
	out = httptest.NewRecorder()
	r.ServeHTTP(out, signed(p, key, "GET", "/openapi/v1/channels", "", nil, op.Random(24)))
	if out.Code != 200 || strings.Contains(out.Body.String(), `"id":"c2"`) {
		t.Fatal(out.Code, out.Body.String())
	}
	if e := s.Revoke(p.App.ID, p.Credential.ID, "admin"); e != nil {
		t.Fatal(e)
	}
	out = httptest.NewRecorder()
	r.ServeHTTP(out, signed(p, key, "GET", "/openapi/v1/devices", "", nil, op.Random(24)))
	if out.Code != 401 {
		t.Fatal("revoked credential", out.Code)
	}
}
func TestTamperedBodyDuplicateHeadersAndJSON(t *testing.T) {
	r, _, p, key := apiFixture(t)
	req := signed(p, key, "POST", "/openapi/v1/snapshot-tasks", "", []byte(`{"channel_id":"c1"}`), op.Random(24))
	req.Body = io.NopCloser(strings.NewReader(`{"channel_id":"c2"}`))
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != 401 {
		t.Fatal("tampered body", out.Code)
	}
	req = signed(p, key, "GET", "/openapi/v1/devices", "", nil, op.Random(24))
	req.Header.Add("X-Owl-Audience", "owl-test")
	out = httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != 401 {
		t.Fatal("duplicate security header", out.Code)
	}
	req = signed(p, key, "POST", "/openapi/v1/snapshot-tasks", "", []byte(`{"channel_id":"c1","channel_id":"c2"}`), op.Random(24))
	out = httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != 400 {
		t.Fatal("duplicate JSON", out.Code, out.Body.String())
	}
	req = signed(p, key, "GET", "/openapi/v1/devices", "q=a+b&q=other", nil, op.Random(24))
	out = httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != 401 {
		t.Fatal("duplicate query", out.Code)
	}
}
func TestHLSAllResourcesUseTicketAndNoRedirect(t *testing.T) {
	r, s, p, _ := apiFixture(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/live/source/hls.fmp4.m3u8":
			fmt.Fprintln(w, "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXT-X-KEY:METHOD=AES-128,URI=\"video.key\"\nchunk.m4s")
		case "/live/source/bad.m3u8":
			fmt.Fprintln(w, "#EXTM3U\nhttps://untrusted.invalid/secret.ts")
		default:
			fmt.Fprint(w, "chunk")
		}
	}))
	defer up.Close()
	row := op.Session{ID: "session", AppID: p.App.ID, CredentialID: p.Credential.ID, ChannelID: "c1", State: "ready", Protocol: "hls", ExpiresAt: time.Now().Add(time.Minute), Revision: p.App.Revision, Source: op.Source{BaseURL: up.URL, App: "live", Stream: "source"}}
	s.DB.Create(&row)
	raw, ticket, e := s.IssueTicket(p, "live", row.ID)
	if e != nil {
		t.Fatal(e)
	}
	out := httptest.NewRecorder()
	r.ServeHTTP(out, httptest.NewRequest("GET", "/openmedia/v1/live/session/hls.fmp4.m3u8?ticket="+raw, nil))
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	for _, part := range []string{"init.mp4", "video.key", "chunk.m4s"} {
		if !strings.Contains(out.Body.String(), "https://open.example/openmedia/v1/live/session/"+part+"?ticket="+raw) {
			t.Fatal("unguarded resource", part, out.Body.String())
		}
	}
	out = httptest.NewRecorder()
	r.ServeHTTP(out, httptest.NewRequest("GET", "/openmedia/v1/live/session/chunk.m4s", nil))
	if out.Code != 401 {
		t.Fatal("segment without ticket", out.Code)
	}
	out = httptest.NewRecorder()
	r.ServeHTTP(out, httptest.NewRequest("GET", "/openmedia/v1/live/session/bad.m3u8?ticket="+raw, nil))
	if out.Code != 503 {
		t.Fatal("external URL accepted", out.Code)
	}
	s.DB.Model(&op.Ticket{}).Where("hash = ?", ticket.Hash).Update("expires_at", time.Now().Add(-time.Second))
	out = httptest.NewRecorder()
	r.ServeHTTP(out, httptest.NewRequest("GET", "/openmedia/v1/live/session/chunk.m4s?ticket="+raw, nil))
	if out.Code != 401 {
		t.Fatal("expired ticket", out.Code)
	}
}
func TestDisabledModuleNoFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	New(nil).Register(r, func(c *gin.Context) { c.Next() })
	for _, path := range []string{"/openapi/v1/devices", "/admin/open-platform/apps", "/openmedia/v1/live/id/chunk.ts"} {
		out := httptest.NewRecorder()
		r.ServeHTTP(out, httptest.NewRequest("GET", path, nil))
		if out.Code != 503 {
			t.Fatal(path, out.Code)
		}
		var v map[string]any
		if json.Unmarshal(out.Body.Bytes(), &v) != nil || v["request_id"] == nil {
			t.Fatal("missing envelope")
		}
	}
}

func TestCredentialSecretRequiresHTTPSAndOnlyAppearsOnce(t *testing.T) {
	_, s, p, _ := apiFixture(t)
	r := gin.New()
	New(s).Register(r, func(c *gin.Context) { c.Set("open_admin", "test-admin"); c.Next() })
	path := "/admin/open-platform/apps/" + p.App.ID + "/credentials"
	out := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader("{}"))
	req.Header.Set("X-Forwarded-Proto", "https")
	r.ServeHTTP(out, req)
	if out.Code != 403 {
		t.Fatal("untrusted HTTPS header accepted", out.Code)
	}
	out = httptest.NewRecorder()
	r.ServeHTTP(out, httptest.NewRequest("POST", "https://management.example"+path, strings.NewReader("{}")))
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	var once struct {
		Data struct {
			SecretKey string `json:"secret_key"`
		}
	}
	if e := json.Unmarshal(out.Body.Bytes(), &once); e != nil || once.Data.SecretKey == "" {
		t.Fatal("secret not issued", e)
	}
	out = httptest.NewRecorder()
	r.ServeHTTP(out, httptest.NewRequest("GET", "https://management.example"+path, nil))
	if out.Code != 200 || strings.Contains(out.Body.String(), once.Data.SecretKey) || strings.Contains(out.Body.String(), "ciphertext") || strings.Contains(out.Body.String(), "secret_key") {
		t.Fatal("credential list leaks secret", out.Code, out.Body.String())
	}
}

func TestCredentialSecretHTTPOptIn(t *testing.T) {
	_, s, p, _ := apiFixture(t)
	s.Config.AllowHTTP = true
	s.Config.PublicBaseURL = "http://10.144.0.20:15123"
	r := gin.New()
	New(s).Register(r, func(c *gin.Context) { c.Set("open_admin", "test-admin"); c.Next() })
	out := httptest.NewRecorder()
	r.ServeHTTP(out, httptest.NewRequest("POST", "/admin/open-platform/apps/"+p.App.ID+"/credentials", strings.NewReader("{}")))
	if out.Code != 200 || !strings.Contains(out.Body.String(), "secret_key") {
		t.Fatal("explicit HTTP deployment cannot create credentials", out.Code, out.Body.String())
	}
}

func TestPublicDocumentationConfigDoesNotExposeSecrets(t *testing.T) {
	_, s, _, _ := apiFixture(t)
	r := gin.New()
	New(s).Register(r, func(c *gin.Context) { t.Fatal("public documentation must not require administrator credentials") })
	out := httptest.NewRecorder()
	r.ServeHTTP(out, httptest.NewRequest("GET", "/open-platform/docs/config", nil))
	var result struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &result); err != nil || out.Code != 200 {
		t.Fatal("public documentation config unavailable", out.Code, err)
	}
	if len(result.Data) != 4 || result.Data["audience"] != "owl-test" || result.Data["public_url"] != "https://open.example" || result.Data["enabled"] != true || result.Data["allow_http"] != false {
		t.Fatal("public documentation must only expose deployment metadata")
	}
	for _, forbidden := range []string{"secret", "credential", "device", "ciphertext", "keys"} {
		if strings.Contains(out.Body.String(), forbidden) {
			t.Fatal("public documentation exposes private data", forbidden)
		}
	}
}
