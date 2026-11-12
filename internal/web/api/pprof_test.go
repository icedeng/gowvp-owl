package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/web"
)

func pprofTestUsecase() *Usecase {
	cfg := conf.DefaultConfig()
	cfg.Server.HTTP.PProf.Enabled = true
	cfg.Server.HTTP.JwtSecret = "pprof-test-signing-key"
	return &Usecase{Conf: &cfg, UserAPI: NewUserAPI(&cfg)}
}

func pprofTestToken(t *testing.T, username, secret string, expiry time.Time) string {
	t.Helper()
	opts := []web.TokenOptions{}
	if !expiry.IsZero() {
		opts = append(opts, web.WithExpiresAt(expiry))
	}
	token, err := web.NewToken(web.NewClaimsData().SetUsername(username), secret, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestPProfAccessBoundary(t *testing.T) {
	uc := pprofTestUsecase()
	valid := pprofTestToken(t, uc.Conf.Server.Username, uc.Conf.Server.HTTP.JwtSecret, time.Now().Add(time.Hour))
	wrongUser := pprofTestToken(t, "other-user", uc.Conf.Server.HTTP.JwtSecret, time.Now().Add(time.Hour))
	expired := pprofTestToken(t, uc.Conf.Server.Username, uc.Conf.Server.HTTP.JwtSecret, time.Now().Add(-time.Hour))
	wrongKey := pprofTestToken(t, uc.Conf.Server.Username, "other-secret", time.Now().Add(time.Hour))
	// 不带过期时间的令牌即使签名有效也不能用于诊断接口。
	claims := web.NewClaimsData().SetUsername(uc.Conf.Server.Username)
	noExpiry, err := web.NewToken(claims, uc.Conf.Server.HTTP.JwtSecret, func(c *web.Claims) { c.ExpiresAt = nil })
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, peer, token, query string
		forwarded, duplicate     bool
		want                     int
	}{
		{"local administrator", "127.0.0.1:1234", valid, "", false, false, 200},
		{"IPv6 local administrator", "[::1]:1234", valid, "", false, false, 200},
		{"IPv4 mapped local administrator", "[::ffff:127.0.0.1]:1234", valid, "", false, false, 200},
		{"forwarding cannot grant source", "203.0.113.9:1234", valid, "", true, false, 403},
		{"forwarding cannot grant anonymous source", "203.0.113.9:1234", "", "", true, false, 403},
		{"local proxy still needs administrator", "127.0.0.1:1234", "", "", true, false, 401},
		{"local anonymous", "127.0.0.1:1234", "", "", false, false, 401},
		{"wrong user", "127.0.0.1:1234", wrongUser, "", false, false, 401},
		{"expired", "127.0.0.1:1234", expired, "", false, false, 401},
		{"wrong key", "127.0.0.1:1234", wrongKey, "", false, false, 401},
		{"missing expiry", "127.0.0.1:1234", noExpiry, "", false, false, 401},
		{"duplicate authorization", "127.0.0.1:1234", valid, "", false, true, 401},
		{"query token", "127.0.0.1:1234", "", "?token=" + valid, false, false, 401},
		{"query access token with header", "127.0.0.1:1234", valid, "?access_token=" + valid, false, false, 401},
		{"malformed peer", "not-an-address", valid, "", true, false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			registerPProf(r, uc)
			req := httptest.NewRequest(http.MethodGet, "/debug/pprof/"+tc.query, nil)
			req.RemoteAddr = tc.peer
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.duplicate {
				req.Header.Add("Authorization", "Bearer "+tc.token)
			}
			if tc.forwarded {
				req.Header.Set("X-Forwarded-For", "127.0.0.1")
				req.Header.Set("X-Real-IP", "::1")
				req.Header.Set("Forwarded", "for=127.0.0.1")
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status=%d, want=%d, body=%s", w.Code, tc.want, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("diagnostics must not be cached")
			}
		})
	}
}

func TestPProfAllRoutesRequireAuthorization(t *testing.T) {
	uc := pprofTestUsecase()
	r := gin.New()
	registerPProf(r, uc)
	for _, route := range r.Routes() {
		t.Run(route.Method+route.Path, func(t *testing.T) {
			req := httptest.NewRequest(route.Method, route.Path, nil)
			req.RemoteAddr = "127.0.0.1:1234"
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("unprotected diagnostic route: %s %s: %d", route.Method, route.Path, w.Code)
			}
		})
	}
	if len(r.Routes()) < 14 {
		t.Fatal("diagnostic routes were unexpectedly removed")
	}
}

func TestPProfAllowlistFailsClosedAndNormalRoutesAreUnchanged(t *testing.T) {
	for _, allowed := range [][]string{nil, {"invalid", "0.0.0.0/0"}, {"192.0.2.5"}} {
		uc := pprofTestUsecase()
		uc.Conf.Server.HTTP.PProf.AccessIps = allowed
		r := gin.New()
		registerPProf(r, uc)
		r.GET("/normal", func(c *gin.Context) { c.String(200, c.ClientIP()) })
		for _, path := range []string{"/debug/vars", "/normal"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set("X-Forwarded-For", "198.51.100.10")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if path == "/debug/vars" && w.Code != 403 {
				t.Fatalf("empty/invalid/nonmatching allowlist accepted: %v", allowed)
			}
			if path == "/normal" && (w.Code != 200 || w.Body.String() != "198.51.100.10") {
				t.Fatal("normal route or global proxy behavior changed")
			}
		}
	}
}

func TestPProfDisabledAndExternalAuthenticationCannotGrantAccess(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
	defer srv.Close()
	uc := pprofTestUsecase()
	uc.Conf.Server.HTTP.AuthURL = srv.URL
	for _, enabled := range []bool{false, true} {
		uc.Conf.Server.HTTP.PProf.Enabled = enabled
		r := gin.New()
		registerPProf(r, uc)
		req := httptest.NewRequest(http.MethodGet, "/debug/vars", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Authorization", "Bearer external-token")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		want := 404
		if enabled {
			want = 401
		}
		if w.Code != want {
			t.Fatalf("enabled=%t status=%d want=%d", enabled, w.Code, want)
		}
	}
	if calls != 0 {
		t.Fatal("diagnostic authentication called external AuthURL")
	}
}

func TestPProfAdministratorCanUseVarsAndSymbol(t *testing.T) {
	uc := pprofTestUsecase()
	uc.Conf.Server.HTTP.PProf.AccessIps = []string{"192.0.2.5"}
	token := pprofTestToken(t, uc.Conf.Server.Username, uc.Conf.Server.HTTP.JwtSecret, time.Now().Add(time.Hour))
	r := gin.New()
	registerPProf(r, uc)
	for _, tc := range []struct{ method, path string }{{"GET", "/debug/vars"}, {"POST", "/debug/pprof/symbol"}} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(""))
		req.RemoteAddr = "192.0.2.5:1234"
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s %s: status=%d", tc.method, tc.path, w.Code)
		}
	}
}
