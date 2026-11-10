package api

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestWebRootRedirectPreservesQueryAndStaticRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/web", redirectWebRoot)
	r.HEAD("/web", redirectWebRoot)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("admin-ui"), 0600); err != nil {
		t.Fatal(err)
	}
	r.Static("/web/", dir)
	r.GET("/webhook", func(c *gin.Context) { c.String(200, "webhook") })
	for _, tc := range []struct {
		method, path string
		status       int
		location     string
	}{
		{"GET", "/web", 308, "/web/"},
		{"HEAD", "/web?redirect=%2Fchannels%3Fid%3D1&name=a%20b", 308, "/web/?redirect=%2Fchannels%3Fid%3D1&name=a%20b"},
		{"GET", "/web/", 200, ""},
		{"GET", "/webhook", 200, ""},
		{"GET", "/web-extra", 404, ""},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status || w.Header().Get("Location") != tc.location {
			t.Fatalf("%s %s => %d %q, want %d %q", tc.method, tc.path, w.Code, w.Header().Get("Location"), tc.status, tc.location)
		}
	}
}
