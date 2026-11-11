package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	agowl "github.com/gowvp/owl/extensions/accessgateway/integration/owl"
	agplatform "github.com/gowvp/owl/extensions/accessgateway/platform"
	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/web"
)

func TestAccessGatewayRequiresCurrentAdminBearerOnEveryRoute(t *testing.T) {
	const secret = "gateway-admin-test-secret"
	cfg := &conf.Bootstrap{Server: conf.Server{Username: "admin", HTTP: conf.ServerHTTP{JwtSecret: secret}}}
	uc := &Usecase{Conf: cfg, UserAPI: NewUserAPI(cfg), AccessGateway: &agplatform.Module{}, AccessGatewayHost: &agowl.Host{}}
	router := gin.New()
	mountAccessGateway(router, uc)
	token := func(username string, opts ...web.TokenOptions) string {
		t.Helper()
		value, err := web.NewToken(map[string]any{web.KeyUsername: username}, secret, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	admin := token("admin")
	cases := []struct {
		name    string
		headers []string
		query   string
		status  int
	}{
		{name: "missing", status: http.StatusUnauthorized},
		{name: "ordinary user", headers: []string{"Bearer " + token("viewer")}, status: http.StatusUnauthorized},
		{name: "expired admin", headers: []string{"Bearer " + token("admin", web.WithExpiresAt(time.Now().Add(-time.Hour)))}, status: http.StatusUnauthorized},
		{name: "no expiry", headers: []string{"Bearer " + token("admin", func(c *web.Claims) { c.ExpiresAt = nil })}, status: http.StatusUnauthorized},
		{name: "query token", headers: []string{"Bearer " + admin}, query: "?token=ignored", status: http.StatusUnauthorized},
		{name: "query access token", headers: []string{"Bearer " + admin}, query: "?access_token=ignored", status: http.StatusUnauthorized},
		{name: "duplicate header", headers: []string{"Bearer " + admin, "Bearer " + admin}, status: http.StatusUnauthorized},
		// 模块未启用返回503，证明管理员已经通过门禁；不需要启动数据库或网关。
		{name: "current admin", headers: []string{"Bearer " + admin}, status: http.StatusServiceUnavailable},
	}
	for _, route := range router.Routes() {
		path := strings.NewReplacer(":id", "b1", ":session", "session", ":operation", "operation").Replace(route.Path)
		for _, tc := range cases {
			t.Run(route.Method+" "+route.Path+"/"+tc.name, func(t *testing.T) {
				request := httptest.NewRequest(route.Method, path+tc.query, nil)
				for _, value := range tc.headers {
					request.Header.Add("Authorization", value)
				}
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != tc.status {
					t.Fatalf("status=%d, want %d: %s", response.Code, tc.status, response.Body.String())
				}
			})
		}
	}
	// 修改管理员名后，旧账号 token 不能继续使用管理接口。
	uc.UserAPI.credentialsMu.Lock()
	cfg.Server.Username = "new-admin"
	uc.UserAPI.credentialsMu.Unlock()
	request := httptest.NewRequest(http.MethodGet, "/access_gateways", nil)
	request.Header.Set("Authorization", "Bearer "+admin)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("旧管理员仍被接受: %d", response.Code)
	}
}
