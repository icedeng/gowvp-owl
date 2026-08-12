package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/ipc"
)

func TestSharedSecretAuth(t *testing.T) {
	r := gin.New()
	r.GET("/hook", sharedSecretAuth(func() string { return "correct-secret" }), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	for _, tc := range []struct {
		name   string
		url    string
		header string
		want   int
	}{
		{name: "missing", url: "/hook", want: http.StatusUnauthorized},
		{name: "wrong", url: "/hook?secret=wrong", want: http.StatusUnauthorized},
		{name: "query", url: "/hook?secret=correct-secret", want: http.StatusNoContent},
		{name: "header", url: "/hook", header: "correct-secret", want: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			if tc.header != "" {
				req.Header.Set("Secret", tc.header)
			}
			r.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestZLMWebhookRoutesRequireMediaSecret(t *testing.T) {
	r := gin.New()
	bc := &conf.Bootstrap{}
	bc.Media.Secret = "media-secret"
	registerZLMWebhookAPI(r, WebHookAPI{conf: bc, log: slog.Default()})

	for _, tc := range []struct {
		name   string
		target string
		want   int
	}{
		{name: "missing", target: "/webhook/on_rtp_server_timeout", want: http.StatusUnauthorized},
		{name: "wrong", target: "/webhook/on_rtp_server_timeout?secret=wrong", want: http.StatusUnauthorized},
		{name: "valid", target: "/webhook/on_rtp_server_timeout?secret=media-secret", want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.target, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestAIWebhookLifecycleRoutesRequireSecret(t *testing.T) {
	r := gin.New()
	bc := &conf.Bootstrap{AISecret: "ai-secret"}
	registerAIWebhookAPI(r, NewAIWebhookAPI(bc, event.Core{}, ipc.Core{}), WebHookAPI{conf: bc})

	for _, target := range []string{"/ai/keepalive", "/ai/started", "/ai/stopped"} {
		for _, tc := range []struct {
			name   string
			secret string
			want   int
		}{
			{name: "missing", want: http.StatusUnauthorized},
			{name: "wrong", secret: "wrong", want: http.StatusUnauthorized},
			{name: "valid", secret: "ai-secret", want: http.StatusOK},
		} {
			t.Run(target+"/"+tc.name, func(t *testing.T) {
				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{}`))
				req.Header.Set("Content-Type", "application/json")
				if tc.secret != "" {
					req.Header.Set("Secret", tc.secret)
				}
				r.ServeHTTP(w, req)
				if w.Code != tc.want {
					t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
				}
			})
		}
	}
}
