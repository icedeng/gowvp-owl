package api

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/web"
)

func TestAppUpgradeRequiresLocalAdminHeaderToken(t *testing.T) {
	secret := "upgrade-test-secret"
	uc := &Usecase{Conf: &conf.Bootstrap{Server: conf.Server{Username: "operator", HTTP: conf.ServerHTTP{JwtSecret: secret}}}}
	t.Setenv("OWL_UPGRADER_SOCKET", "")
	r := gin.New()
	uc.registerAppUpgrade(r)
	token := func(username string) string {
		value, err := web.NewToken(web.NewClaimsData().SetUsername(username), secret, web.WithExpiresAt(time.Now().Add(time.Hour)))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, tc := range []struct {
		method, path, auth string
		want               int
	}{
		{"GET", "/app/upgrade/status", "", 401},
		{"POST", "/app/upgrade/packages", "", 401},
		{"POST", "/app/upgrade/install", "", 401},
		{"GET", "/app/version/check", "", 401},
		{"GET", "/app/upgrade/status?token=" + token("operator"), "", 401},
		{"GET", "/app/upgrade/status", "Bearer " + token("viewer"), 403},
		{"GET", "/app/upgrade/status", "Bearer " + token("operator"), 503},
		{"POST", "/app/upgrade", "Bearer " + token("operator"), 410},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", tc.auth)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("%s %s: got %d want %d", tc.method, tc.path, w.Code, tc.want)
		}
	}
}

func TestAppUpgradeProxyDoesNotForwardCredentials(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "owl-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "agent.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/install" || r.Header.Get("Authorization") != "" {
			t.Error("unsafe proxy target or leaked JWT")
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"package_id":"abc"}` {
			t.Error("request changed")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"phase":"queued"}`))
	})}
	go s.Serve(l)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	t.Setenv("OWL_UPGRADER_SOCKET", socket)
	uc := &Usecase{Conf: &conf.Bootstrap{Server: conf.Server{Username: "operator", HTTP: conf.ServerHTTP{JwtSecret: "secret"}}}}
	r := gin.New()
	uc.registerAppUpgrade(r)
	token, _ := web.NewToken(web.NewClaimsData().SetUsername("operator"), "secret", web.WithExpiresAt(time.Now().Add(time.Hour)))
	req := httptest.NewRequest("POST", "/app/upgrade/install", strings.NewReader(`{"package_id":"abc"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 202 || !strings.Contains(w.Body.String(), "queued") {
		t.Fatalf("proxy response: %d %s", w.Code, w.Body.String())
	}
}
