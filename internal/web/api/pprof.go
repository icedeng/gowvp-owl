package api

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/ixugo/goddd/pkg/web"
)

func registerPProf(r gin.IRouter, uc *Usecase) {
	if !uc.Conf.Server.HTTP.PProf.Enabled {
		return
	}
	// 仅复用依赖的诊断路由；以外层中间件替换其基于 ClientIP 的白名单。
	// 空列表只关闭依赖内部的重复检查，外层对白名单为空的配置一律拒绝。
	var dependencyAccessIPs []string
	web.SetupPProf(r.Group("", pprofAccess(uc)), &dependencyAccessIPs)
}

func pprofAccess(uc *Usecase) gin.HandlerFunc {
	allowed := make(map[netip.Addr]struct{})
	for _, raw := range uc.Conf.Server.HTTP.PProf.AccessIps {
		if ip, err := netip.ParseAddr(strings.TrimSpace(raw)); err == nil {
			allowed[ip.Unmap()] = struct{}{}
		}
	}
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
		// 不依赖 Gin 全局可信代理设置，也不读取客户端提供的任何转发头。
		host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
		ip, parseErr := netip.ParseAddr(host)
		_, permitted := allowed[ip.Unmap()]
		if err != nil || parseErr != nil || !permitted {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "诊断接口连接来源不在白名单内"})
			return
		}

		// 即使对端是本机代理，也必须证明管理员身份，不能仅凭回环地址放行。
		deny := func() {
			c.Header("WWW-Authenticate", "Bearer")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "诊断接口需要有效的平台管理员 Bearer 身份"})
		}
		query := c.Request.URL.Query()
		_, hasToken := query["token"]
		_, hasAccessToken := query["access_token"]
		auth := c.GetHeader("Authorization")
		const prefix = "Bearer "
		if hasToken || hasAccessToken || len(c.Request.Header.Values("Authorization")) != 1 ||
			len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
			deny()
			return
		}
		claims, err := web.ParseToken(auth[len(prefix):], uc.Conf.Server.HTTP.JwtSecret)
		if err != nil || claims.ExpiresAt == nil || claims.Valid() != nil {
			deny()
			return
		}
		if !uc.validAdminSession(claims) {
			deny()
			return
		}
		c.Next()
	}
}
