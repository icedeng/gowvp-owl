package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type unreadableLogBody struct{ t *testing.T }

func (b unreadableLogBody) Read([]byte) (int, error) {
	b.t.Fatal("logging read the request body before authentication")
	return 0, nil
}
func (unreadableLogBody) Close() error { return nil }

func TestRequestLoggerPreservesRequestWithoutLoggingSecrets(t *testing.T) {
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	r := gin.New()
	r.Use(requestLogger())
	r.POST("/test", func(c *gin.Context) {
		if c.Query("token") != "private-token" || c.GetHeader("Authorization") != "Bearer private-header" {
			t.Fatal("logger changed authentication inputs")
		}
		c.String(http.StatusUnauthorized, "private-response")
	})
	req := httptest.NewRequest(http.MethodPost, "/test?token=private-token&secret=private-key", nil)
	req.Body, req.ContentLength = unreadableLogBody{t}, -1
	req.Header.Set("Authorization", "Bearer private-header")
	r.ServeHTTP(httptest.NewRecorder(), req)
	if strings.Contains(log.String(), "private-") {
		t.Fatalf("sensitive content logged: %s", log.String())
	}
	if !strings.Contains(log.String(), `"statuscode":401`) || !strings.Contains(log.String(), `"path":"/test"`) {
		t.Fatalf("missing request metadata: %s", log.String())
	}
}
