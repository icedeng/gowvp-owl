package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	agowl "github.com/gowvp/owl/extensions/accessgateway/integration/owl"
	agplatform "github.com/gowvp/owl/extensions/accessgateway/platform"
	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/web"
	"github.com/ixugo/goddd/pkg/ws"
)

func sessionTestUsecase(t *testing.T) *Usecase {
	t.Helper()
	cfg := conf.DefaultConfig()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
	cfg.Server.Password = "initial-password"
	if err := conf.SecureRuntimeSecrets(&cfg); err != nil {
		t.Fatal(err)
	}
	return &Usecase{Conf: &cfg, UserAPI: NewUserAPI(&cfg)}
}

func sessionTestLogin(t *testing.T, uc *Usecase, password string) string {
	t.Helper()
	data := encryptUserTestCredentials(t, uc.UserAPI, map[string]string{"username": uc.Conf.Server.Username, "password": password})
	out, err := uc.UserAPI.login(nil, &loginInput{Data: data})
	if err != nil {
		t.Fatal(err)
	}
	return out.Token
}

func sessionTestChange(t *testing.T, uc *Usecase) {
	t.Helper()
	data := encryptUserTestCredentials(t, uc.UserAPI, map[string]string{"username": uc.Conf.Server.Username, "old_password": "initial-password", "password": "updated-password"})
	if _, err := uc.UserAPI.updateCredentials(nil, &updateCredentialsInput{Data: data}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRevocationAcrossProtectedEntrypointsAndRestart(t *testing.T) {
	uc := sessionTestUsecase(t)
	old := sessionTestLogin(t, uc, "initial-password")
	var externalCalls atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { externalCalls.Add(1); w.WriteHeader(200) }))
	defer external.Close()
	r := gin.New()
	r.GET("/normal", authMiddleware(uc.Conf.Server.HTTP.JwtSecret, external.URL, uc.validAdminSession), func(c *gin.Context) { c.Status(204) })
	r.GET("/open-admin", strictOpenAdmin(uc), func(c *gin.Context) { c.Status(204) })
	uc.Conf.Server.HTTP.PProf.Enabled = true
	registerPProf(r, uc)
	t.Setenv("OWL_UPGRADER_SOCKET", "")
	uc.registerAppUpgrade(r)
	uc.AccessGateway, uc.AccessGatewayHost = &agplatform.Module{}, &agowl.Host{}
	mountAccessGateway(r, uc)
	check := func(token string, revoked bool) {
		t.Helper()
		for _, tc := range []struct {
			path   string
			status int
		}{{"/normal", 204}, {"/open-admin", 204}, {"/debug/pprof/", 200}, {"/app/upgrade/status", 503}, {"/access_gateways", 503}} {
			req := httptest.NewRequest("GET", tc.path, nil)
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			want := tc.status
			if revoked {
				want = 401
			}
			if w.Code != want {
				t.Fatalf("%s revoked=%v status=%d want=%d body=%s", tc.path, revoked, w.Code, want, w.Body.String())
			}
		}
	}
	check(old, false)
	legacy, err := web.NewToken(web.NewClaimsData().SetUsername(uc.Conf.Server.Username), uc.Conf.Server.HTTP.JwtSecret)
	if err != nil {
		t.Fatal(err)
	}
	check(legacy, true)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set(sessionVersionClaim, uc.Conf.AdminSessionVersion)
	play, err := uc.newPlayToken(ctx, map[string]any{"app": "rtp", "stream": "camera", "media_server_id": "default"})
	if err != nil {
		t.Fatal(err)
	}
	playClaims, err := web.ParseToken(play, uc.Conf.Server.HTTP.JwtSecret+"_play")
	if err != nil || !uc.validPlaySession(playClaims) {
		t.Fatal("new playback grant invalid")
	}
	checkPlay := func(wantValid bool) {
		t.Helper()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/proxy/sms/_media/default/rtp/camera.live.flv?token="+play, nil)
		if err := uc.verifyPlayToken(c, "/_media/default/rtp/camera.live.flv"); (err == nil) != wantValid {
			t.Fatalf("playback proxy valid=%v: %v", wantValid, err)
		}
	}
	checkPlay(true)
	onvif := uc.Conf.Server.ONVIF
	sessionTestChange(t, uc)
	check(old, true)
	checkPlay(false)
	queryRequest := httptest.NewRequest("GET", "/normal?token="+old, nil)
	queryResponse := httptest.NewRecorder()
	r.ServeHTTP(queryResponse, queryRequest)
	if queryResponse.Code != 401 {
		t.Fatal("revoked query token accepted")
	}
	if externalCalls.Load() != 0 {
		t.Fatal("revoked local token fell back to AuthURL")
	}
	if uc.Conf.Server.ONVIF != onvif {
		t.Fatal("admin change modified ONVIF credentials")
	}
	if uc.validPlaySession(playClaims) {
		t.Fatal("old playback grant survived password change")
	}
	// 已通过旧鉴权、尚未签发播放地址的请求不能得到新会话代次。
	late, _ := uc.newPlayToken(ctx, map[string]any{})
	lateClaims, _ := web.ParseToken(late, uc.Conf.Server.HTTP.JwtSecret+"_play")
	if uc.validPlaySession(lateClaims) {
		t.Fatal("in-flight old request minted new session grant")
	}
	current := sessionTestLogin(t, uc, "updated-password")
	check(current, false)
	// 外部凭据仍交由第三方认证，保留原有集成行为。
	req := httptest.NewRequest("GET", "/normal", nil)
	req.Header.Set("Authorization", "Bearer external-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 204 || externalCalls.Load() != 1 {
		t.Fatal("external auth compatibility changed")
	}
	restarted := conf.DefaultConfig()
	restarted.ConfigPath = uc.Conf.ConfigPath
	if err := conf.SetupConfig(&restarted, restarted.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if err := conf.SecureRuntimeSecrets(&restarted); err != nil {
		t.Fatal(err)
	}
	restartedUC := &Usecase{Conf: &restarted, UserAPI: NewUserAPI(&restarted)}
	for _, tc := range []struct {
		token string
		valid bool
	}{{old, false}, {current, true}} {
		claims, _ := web.ParseToken(tc.token, restarted.Server.HTTP.JwtSecret)
		if restartedUC.validAdminSession(claims) != tc.valid {
			t.Fatal("restart changed revocation state")
		}
	}
}

func TestSessionConcurrentValidationDuringChange(t *testing.T) {
	uc := sessionTestUsecase(t)
	token := sessionTestLogin(t, uc, "initial-password")
	claims, _ := web.ParseToken(token, uc.Conf.Server.HTTP.JwtSecret)
	var workers sync.WaitGroup
	done := make(chan struct{})
	for range 8 {
		workers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					uc.validAdminSession(claims)
				}
			}
		})
	}
	configAPI := ConfigAPI{conf: uc.Conf, uc: uc, sipMu: new(sync.RWMutex)}
	workers.Go(func() {
		input := map[string]json.RawMessage{"ffmpeg_concurrency": json.RawMessage("3")}
		for range 8 {
			if _, err := configAPI.updateSnapshotConfig(nil, &input); err != nil {
				t.Error(err)
				return
			}
		}
	})
	sessionTestChange(t, uc)
	close(done)
	workers.Wait()
	if uc.validAdminSession(claims) {
		t.Fatal("old token valid after concurrent change")
	}
}

func TestLoginKeyRotationGraceAndKeyID(t *testing.T) {
	var secret Secret
	old, err := secret.GetOrCreatePublicKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, old, []byte("credentials"), nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(cipher)
	secret.expiredAt = time.Now().Add(-time.Second)
	current, err := secret.GetOrCreatePublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if loginKeyID(old) == loginKeyID(current) {
		t.Fatal("key did not rotate")
	}
	for _, id := range []string{"", loginKeyID(old)} {
		if value, err := secret.Decrypt(encoded, id); err != nil || string(value) != "credentials" {
			t.Fatal("previous key not accepted in grace period", err)
		}
	}
	if _, err := secret.Decrypt(encoded, loginKeyID(current)); err == nil {
		t.Fatal("wrong key ID accepted")
	}
	secret.previousUntil = time.Now().Add(-time.Second)
	if _, err := secret.Decrypt(encoded); err == nil {
		t.Fatal("expired previous key accepted")
	}
	secret.expiredAt = time.Now().Add(-loginKeyGrace - time.Second)
	newCipher, _ := rsa.EncryptOAEP(sha256.New(), rand.Reader, current, []byte("credentials"), nil)
	if _, err := secret.Decrypt(base64.StdEncoding.EncodeToString(newCipher)); err == nil {
		t.Fatal("unrotated key accepted beyond expiry and grace")
	}
}

func TestSessionChangeClosesWebSocketsAndRejectsOldToken(t *testing.T) {
	uc := sessionTestUsecase(t)
	old := sessionTestLogin(t, uc, "initial-password")
	sockets := uc.UserAPI.sockets
	sockets.create = func() ws.Huber { return newAdminHub(uc) }
	sockets.reset()
	defer sockets.Close()
	server := httptest.NewServer(sockets)
	defer server.Close()
	connect := func(token, want string) *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var message struct {
			Type string `json:"type"`
		}
		if err := conn.ReadJSON(&message); err != nil || message.Type != "auth" {
			t.Fatal("no auth challenge", err)
		}
		if err := conn.WriteJSON(map[string]any{"type": "auth", "data": map[string]string{"token": token}}); err != nil {
			t.Fatal(err)
		}
		if err := conn.ReadJSON(&message); err != nil || message.Type != want {
			t.Fatalf("auth result=%s want=%s err=%v", message.Type, want, err)
		}
		return conn
	}
	conn := connect(old, "auth_ok")
	defer conn.Close()
	sessionTestChange(t, uc)
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("old websocket remained open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("old websocket was not closed before the deadline")
	}
	rejected := connect(old, "error")
	rejected.Close()
	accepted := connect(sessionTestLogin(t, uc, "updated-password"), "auth_ok")
	accepted.Close()
}
