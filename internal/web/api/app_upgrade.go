package api

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/upgrader"
	"github.com/ixugo/goddd/pkg/web"
)

func upgradeClient(socket string) *http.Client {
	return &http.Client{Timeout: 20 * time.Minute, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", socket)
		},
		ResponseHeaderTimeout: 20 * time.Minute,
	}}
}

func (uc *Usecase) registerAppUpgrade(r gin.IRouter) {
	// 升级仅允许本平台管理员的Header JWT，不接受第三方鉴权或URL令牌。
	admin := func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			c.AbortWithStatusJSON(401, gin.H{"msg": "请使用管理员账号登录"})
			return
		}
		claims, err := web.ParseToken(strings.TrimPrefix(auth, "Bearer "), uc.Conf.Server.HTTP.JwtSecret)
		if err != nil || claims.ExpiresAt == nil || claims.Valid() != nil {
			c.AbortWithStatusJSON(401, gin.H{"msg": "登录已失效，请重新登录"})
			return
		}
		username, _ := uc.sessionSnapshot()
		if claims.Data[web.KeyUsername] != username {
			c.AbortWithStatusJSON(403, gin.H{"msg": "仅本平台管理员可管理版本升级"})
			return
		}
		if !uc.validAdminSession(claims) {
			c.AbortWithStatusJSON(401, gin.H{"msg": "登录已失效，请重新登录"})
			return
		}
		c.Next()
	}
	socket := os.Getenv("OWL_UPGRADER_SOCKET")
	client := upgradeClient(socket)
	proxy := func(path string) gin.HandlerFunc {
		return func(c *gin.Context) {
			if socket == "" {
				c.JSON(503, gin.H{"msg": "宿主机升级服务尚未配置，请联系运维完成安装", "available": false})
				return
			}
			if c.Request.Method == http.MethodPost && path == "/packages" {
				if c.Request.ContentLength <= 0 || c.Request.ContentLength > upgrader.MaxUpload {
					c.JSON(413, gin.H{"msg": "发布包为空或超过2GB"})
					return
				}
				rc := http.NewResponseController(c.Writer)
				_ = rc.SetReadDeadline(time.Now().Add(20 * time.Minute))
				_ = rc.SetWriteDeadline(time.Now().Add(20 * time.Minute))
				c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, upgrader.MaxUpload)
			}
			req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, "http://upgrader"+path, c.Request.Body)
			if err != nil {
				c.JSON(500, gin.H{"msg": "无法创建升级请求"})
				return
			}
			req.ContentLength = c.Request.ContentLength
			req.Header.Set("Content-Type", c.GetHeader("Content-Type"))
			response, err := client.Do(req)
			if err != nil {
				c.JSON(503, gin.H{"msg": "宿主机升级服务暂不可用，安装期间请稍后刷新恢复进度", "available": false})
				return
			}
			defer response.Body.Close()
			c.Header("Content-Type", "application/json")
			c.Header("Cache-Control", "no-store")
			c.Status(response.StatusCode)
			_, _ = io.Copy(c.Writer, io.LimitReader(response.Body, 2<<20))
		}
	}
	r.GET("/app/upgrade/status", admin, proxy("/status"))
	r.POST("/app/upgrade/packages", admin, proxy("/packages"))
	r.POST("/app/upgrade/install", admin, proxy("/install"))
	// 兼容旧书签，但明确失败；不再访问上游或返回虚假的下载成功。
	r.GET("/app/version/check", admin, proxy("/status"))
	r.POST("/app/upgrade", admin, func(c *gin.Context) {
		c.JSON(410, gin.H{"msg": "旧下载接口已停用，请上传签名发布包并确认安装"})
	})
}
