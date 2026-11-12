package openapi

import (
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// mediaBaseURL 让同步返回的媒体地址跟随请求入口；异步通知仍使用配置地址。
func (a *API) mediaBaseURL(c *gin.Context) string {
	scheme, host := "http", c.Request.Host
	if c.Request.TLS != nil {
		scheme = "https"
	}
	peer, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err == nil {
		ip := net.ParseIP(peer)
		for _, network := range a.S.Config.TrustedProxies {
			if !network.Contains(ip) {
				continue
			}
			// 代理必须覆盖这些头；拒绝多值，避免猜测代理链中的外部入口。
			if values := c.Request.Header.Values("X-Forwarded-Host"); len(values) == 1 && validMediaHost(values[0]) {
				host = values[0]
			}
			if values := c.Request.Header.Values("X-Forwarded-Proto"); len(values) == 1 && (values[0] == "http" || values[0] == "https") {
				scheme = values[0]
			}
			break
		}
	}
	if !validMediaHost(host) {
		return a.S.Config.PublicBaseURL
	}
	return scheme + "://" + host
}

func validMediaHost(host string) bool {
	if host == "" || strings.ContainsAny(host, "\\/?#@%, \t\r\n") {
		return false
	}
	u, err := url.Parse("http://" + host)
	if err != nil || u.Host != host || u.Hostname() == "" || u.User != nil || strings.HasSuffix(host, ":") {
		return false
	}
	if strings.Contains(u.Hostname(), ":") {
		if net.ParseIP(u.Hostname()) == nil || !strings.HasPrefix(host, "[") {
			return false
		}
	} else {
		for _, char := range u.Hostname() {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '.' || char == '-') {
				return false
			}
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return true
}
