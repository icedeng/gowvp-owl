package api

import (
	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/web"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOpenAdminRequiresCurrentAdministratorJWT(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &conf.Bootstrap{}
	cfg.Server.Username = "admin"
	cfg.Server.HTTP.JwtSecret = "test-only-admin-key"
	cfg.Server.HTTP.AuthURL = "https://must-not-be-called.invalid"
	uc := &Usecase{Conf: cfg, UserAPI: NewUserAPI(cfg)}
	valid, _ := web.NewToken(web.NewClaimsData().SetUsername("admin"), cfg.Server.HTTP.JwtSecret, web.WithExpiresAt(time.Now().Add(time.Hour)))
	old, _ := web.NewToken(web.NewClaimsData().SetUsername("old-admin"), cfg.Server.HTTP.JwtSecret, web.WithExpiresAt(time.Now().Add(time.Hour)))
	expired, _ := web.NewToken(web.NewClaimsData().SetUsername("admin"), cfg.Server.HTTP.JwtSecret, web.WithExpiresAt(time.Now().Add(-time.Hour)))
	r := gin.New()
	r.GET("/test", strictOpenAdmin(uc), func(c *gin.Context) { c.Status(204) })
	for _, tc := range []struct {
		auth, path string
		status     int
	}{{"Bearer " + valid, "/test", 204}, {"Bearer " + old, "/test", 401}, {"Bearer " + expired, "/test", 401}, {"", "/test?token=" + valid, 401}, {"OWL-HMAC-SHA256 Credential=owl_ak_fake,Signature=fake", "/test", 401}, {"", "/test", 401}} {
		out := httptest.NewRecorder()
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Header.Set("Authorization", tc.auth)
		r.ServeHTTP(out, req)
		if out.Code != tc.status {
			t.Fatal(tc.status, out.Code)
		}
	}
}
