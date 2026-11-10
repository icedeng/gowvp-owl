package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/web"
	"golang.org/x/time/rate"
)

func TestLoginGuardCredentialBackoffAndRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := conf.DefaultConfig()
	cfg.Server.Username, cfg.Server.Password = "admin", "test-password"
	cfg.Server.HTTP.JwtSecret = "login-guard-test-secret"
	api := NewUserAPI(&cfg)
	guard := newLoginGuard()
	now := time.Now()
	guard.now = func() time.Time { return now }
	router := gin.New()
	router.POST("/login", guard.middleware(false), web.WrapH(api.login))
	router.GET("/login/key", guard.middleware(true), web.WrapH(api.getPublicKey))
	request := func(method, path, ip, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = ip + ":1234"
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	payload := func(password string) string {
		return `{"data":"` + encryptUserTestCredentials(t, api, map[string]string{"username": "admin", "password": password}) + `"}`
	}
	wrong, correct := payload("wrong"), payload("test-password")
	for range 5 {
		w := request("POST", "/login", "192.0.2.1", wrong)
		if w.Code == http.StatusOK || w.Code == http.StatusTooManyRequests {
			t.Fatalf("unexpected credential failure response: %d %s", w.Code, w.Body)
		}
	}
	if w := request("POST", "/login", "192.0.2.1", correct); w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("missing recoverable cooldown: %d %s", w.Code, w.Body)
	}
	if w := request("POST", "/login", "192.0.2.2", correct); w.Code != 200 {
		t.Fatalf("one source locked out another: %d %s", w.Code, w.Body)
	}
	if w := request("GET", "/login/key", "192.0.2.1", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"oaep_seed"`) || !strings.Contains(w.Body.String(), `"key"`) {
		t.Fatalf("cooldown changed login key contract: %d %s", w.Code, w.Body)
	}
	now = now.Add(time.Second)
	if w := request("POST", "/login", "192.0.2.1", correct); w.Code != 200 {
		t.Fatalf("valid credentials did not recover: %d %s", w.Code, w.Body)
	}
	if source := guard.sources["192.0.2.1"]; source.failures != 0 || !source.blockedUntil.IsZero() {
		t.Fatal("successful login retained failure backoff")
	}
	for range 5 {
		request("POST", "/login", "192.0.2.3", `{"data":"bad-ciphertext"}`)
	}
	if source := guard.sources["192.0.2.3"]; source.failures != 0 {
		t.Fatal("decryption error counted as incorrect credentials")
	}
	if w := request("POST", "/login", "192.0.2.3", correct); w.Code != 200 {
		t.Fatalf("decryption failures locked out valid credentials: %d", w.Code)
	}
	if w := request("POST", "/login", "192.0.2.4", strings.Repeat("x", loginBodyLimit+1)); w.Code != 413 {
		t.Fatalf("oversize login body returned %d", w.Code)
	}
}

func TestLoginGuardConcurrentLimitAndUntrustedForwarding(t *testing.T) {
	guard := newLoginGuard()
	now := time.Now()
	guard.now = func() time.Time { return now }
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			req := httptest.NewRequest("POST", "/login", nil)
			req.RemoteAddr = "192.0.2.1:1234"
			req.Header.Set("X-Forwarded-For", "198.51.100.9")
			if ip := loginSourceIP(req); ip != "192.0.2.1" {
				t.Errorf("trusted unconfigured forwarding header: %q", ip)
			}
			if _, delay := guard.allow(loginSourceIP(req), false); delay == 0 {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 20 {
		t.Fatalf("concurrent source accepted %d, want burst 20", accepted.Load())
	}
	for range 30 {
		if _, delay := guard.allow("192.0.2.1", true); delay != 0 {
			t.Fatal("key limiter reused login quota")
		}
	}
	if _, delay := guard.allow("192.0.2.1", true); delay == 0 {
		t.Fatal("key endpoint has no source limit")
	}
	if _, delay := guard.allow("192.0.2.2", false); delay != 0 {
		t.Fatal("rejected requests from one source exhausted global quota")
	}
	globalGuard := newLoginGuard()
	globalGuard.now = func() time.Time { return now }
	for i := range 100 {
		if _, delay := globalGuard.allow(strconv.Itoa(i), false); delay != 0 {
			t.Fatal("global burst rejected early")
		}
	}
	if _, delay := globalGuard.allow("another-source", false); delay == 0 {
		t.Fatal("missing global request cap")
	}
}

func TestLoginGuardStateIsBoundedExpiresAndCapsBackoff(t *testing.T) {
	guard := newLoginGuard()
	now := time.Now()
	guard.now = func() time.Time { return now }
	guard.login = rate.NewLimiter(rate.Inf, 1)
	for i := range loginSourceCapacity + 1 {
		guard.allow(string(rune(i)), false)
		now = now.Add(time.Nanosecond)
	}
	if len(guard.sources) != loginSourceCapacity {
		t.Fatalf("source state size = %d", len(guard.sources))
	}
	source, _ := guard.allow("test-source", false)
	for range 20 {
		guard.record(source, false)
	}
	if delay := source.blockedUntil.Sub(now); delay != 30*time.Second {
		t.Fatalf("backoff is not capped: %s", delay)
	}
	now = now.Add(loginSourceTTL)
	guard.allow("fresh-source", false)
	if len(guard.sources) != 1 {
		t.Fatalf("idle source state did not expire: %d", len(guard.sources))
	}
}
