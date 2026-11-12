package api

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ixugo/goddd/pkg/web"
)

const sessionVersionClaim = "admin_session_version"

func (uc *Usecase) sessionSnapshot() (username, version string) {
	if uc.UserAPI.credentialsMu != nil {
		uc.UserAPI.credentialsMu.RLock()
		defer uc.UserAPI.credentialsMu.RUnlock()
	}
	return uc.Conf.Server.Username, uc.Conf.AdminSessionVersion
}

func matchesSessionVersion(claims *web.Claims, version string) bool {
	value, exists := claims.Data[sessionVersionClaim]
	if !exists {
		return version == ""
	}
	actual, ok := value.(string)
	return ok && actual == version
}

func (uc *Usecase) validAdminSession(claims *web.Claims) bool {
	username, version := uc.sessionSnapshot()
	return claims != nil && claims.ExpiresAt != nil && claims.Valid() == nil &&
		username != "" && claims.Data[web.KeyUsername] == username && matchesSessionVersion(claims, version)
}

func (uc *Usecase) validPlaySession(claims *web.Claims) bool {
	_, version := uc.sessionSnapshot()
	return claims != nil && claims.ExpiresAt != nil && claims.Valid() == nil && matchesSessionVersion(claims, version)
}

func (uc *Usecase) newPlayToken(c *gin.Context, data map[string]any) (string, error) {
	// 本地管理员请求沿用鉴权时的代次，避免改密前已进入的请求签出新代次凭据。
	version, exists := c.Get(sessionVersionClaim)
	if !exists {
		_, version = uc.sessionSnapshot()
	}
	data[sessionVersionClaim] = version
	return web.NewToken(data, uc.Conf.Server.HTTP.JwtSecret+"_play", web.WithExpiresAt(time.Now().Add(42*time.Hour)))
}
