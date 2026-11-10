package api

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/pkg/web"
)

type smsPlayGrant struct {
	app, stream, mediaServerID, token string
}

// verifyPlayToken 只接受绑定节点和流的播放凭据，不依赖登录、Origin 或 Referer。
func (uc *Usecase) verifyPlayToken(c *gin.Context, requestPath string) error {
	query, err := smsPlayQuery(c.Request.URL.RawQuery)
	if err != nil {
		return err
	}
	token := query.Get("token")
	if token == "" {
		return fmt.Errorf("缺少播放鉴权 token")
	}
	claims, err := web.ParseToken(token, uc.Conf.Server.HTTP.JwtSecret+"_play")
	if err != nil {
		return fmt.Errorf("无效的播放 token")
	}
	if err := claims.Valid(); err != nil {
		return fmt.Errorf("播放 token 已过期")
	}
	grant := smsPlayGrant{token: token}
	app, hasApp := claims.Data["app"].(string)
	grant.app = app
	grant.stream, _ = claims.Data["stream"].(string)
	grant.mediaServerID, _ = claims.Data["media_server_id"].(string)
	// Lalmax 没有 app 路径段，允许现有签发方显式传入空 app。
	if !hasApp || (grant.app != "" && !smsPlaySegment(grant.app)) || !smsPlaySegment(grant.stream) || !smsPlaySegment(grant.mediaServerID) {
		return fmt.Errorf("播放 token 缺少有效的应用、流或节点信息")
	}
	if !smsPlayPath(requestPath) {
		return fmt.Errorf("无效的播放路径")
	}
	upstreamPath, ok := stripSMSProxyRoute(requestPath, grant.mediaServerID)
	if !ok {
		return fmt.Errorf("播放 token 与媒体节点路由不匹配")
	}
	// 先限定为已支持的播放路径，取到节点后再按其真实驱动校验。
	if err := grant.authorize(c.Request.Method, upstreamPath, c.Request.URL.RawQuery, sms.ProtocolZLMediaKit); err != nil {
		if err := grant.authorize(c.Request.Method, upstreamPath, c.Request.URL.RawQuery, sms.ProtocolLalmax); err != nil {
			return err
		}
	}
	c.Set("play_media_server_id", grant.mediaServerID)
	c.Set("sms_play_grant", grant)
	return nil
}

func smsPlayQuery(raw string) (url.Values, error) {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return nil, fmt.Errorf("无效的播放参数")
	}
	for _, values := range query {
		if len(values) != 1 {
			return nil, fmt.Errorf("播放参数不允许重复")
		}
	}
	return query, nil
}

func smsPlayPath(p string) bool {
	return strings.HasPrefix(p, "/") && path.Clean(p) == p &&
		!strings.ContainsAny(p, "%\\?#") && !strings.ContainsFunc(p, unicode.IsControl)
}

func smsPlaySegment(s string) bool {
	return s != "" && !strings.Contains(s, "/") && smsPlayPath("/"+s) && strings.TrimSpace(s) == s
}

func (g smsPlayGrant) authorize(method, p, rawQuery, driver string) error {
	switch driver {
	case "", sms.ProtocolZLMediaKit:
		if !smsPlaySegment(g.app) {
			return fmt.Errorf("播放 token 缺少有效的应用信息")
		}
	case sms.ProtocolLalmax:
	default:
		return fmt.Errorf("不支持的媒体播放节点类型")
	}
	if !smsPlayPath(p) {
		return fmt.Errorf("无效的播放路径")
	}
	q, err := smsPlayQuery(rawQuery)
	if err != nil {
		return err
	}
	// 避免参数覆盖路径归属，或借用相同 app/stream 访问另一个虚拟主机。
	for key, expected := range map[string]string{"app": g.app, "stream": g.stream, "type": "play", "vhost": "__defaultVhost__"} {
		if values, exists := q[key]; exists && values[0] != expected {
			return fmt.Errorf("播放参数与授权不匹配")
		}
	}
	if p == "/index/api/webrtc" {
		_, hasApp := q["app"]
		if (method == http.MethodPost || method == http.MethodGet) &&
			hasApp && q.Get("app") == g.app && q.Get("stream") == g.stream && q.Get("type") == "play" {
			return nil
		}
		return fmt.Errorf("仅允许授权流的 WebRTC 播放请求")
	}
	if method != http.MethodGet && method != http.MethodHead {
		return fmt.Errorf("仅允许读取播放资源")
	}
	if strings.HasPrefix(p, "/index/api/") {
		return fmt.Errorf("播放代理不允许访问媒体管理接口")
	}
	prefix := "/" + g.app + "/" + g.stream
	flv := prefix + ".live.flv"
	if driver == sms.ProtocolLalmax {
		prefix = "/" + g.stream
		flv = prefix + ".flv"
	}
	if p == flv {
		return nil
	}
	if strings.HasPrefix(p, prefix+"/") {
		switch path.Ext(p) {
		case ".m3u8", ".mp4", ".m4s", ".ts", ".key":
			return nil
		}
	}
	return fmt.Errorf("播放 token 与请求流不匹配")
}
