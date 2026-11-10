package api

import (
	"bytes"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

const (
	loginBodyLimit      = 4 << 10
	loginSourceCapacity = 4096
	loginSourceTTL      = 15 * time.Minute
)

type loginSource struct {
	login, key   *rate.Limiter
	lastSeen     time.Time
	failures     int
	blockedUntil time.Time
}

type loginGuard struct {
	mu         sync.Mutex
	sources    map[string]*loginSource
	login, key *rate.Limiter
	lastSweep  time.Time
	now        func() time.Time
}

func newLoginGuard() *loginGuard {
	return &loginGuard{
		sources: make(map[string]*loginSource),
		login:   rate.NewLimiter(20, 100), key: rate.NewLimiter(20, 100),
		now: time.Now,
	}
}

// 来源仅取 TCP 对端，不信任调用者可伪造的转发头，也不改变全局代理配置。
func loginSourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return "unknown"
}

func (g *loginGuard) source(ip string, now time.Time) *loginSource {
	if now.Sub(g.lastSweep) >= time.Minute {
		for key, source := range g.sources {
			if now.Sub(source.lastSeen) >= loginSourceTTL {
				delete(g.sources, key)
			}
		}
		g.lastSweep = now
	}
	source := g.sources[ip]
	if source == nil {
		// 有界状态：容量用尽时替换最久未访问的来源，不建立全账号锁。
		if len(g.sources) >= loginSourceCapacity {
			var oldestIP string
			var oldest *loginSource
			for key, entry := range g.sources {
				if oldest == nil || entry.lastSeen.Before(oldest.lastSeen) {
					oldestIP, oldest = key, entry
				}
			}
			delete(g.sources, oldestIP)
		}
		source = &loginSource{login: rate.NewLimiter(1, 20), key: rate.NewLimiter(2, 30)}
		g.sources[ip] = source
	}
	source.lastSeen = now
	return source
}

func (g *loginGuard) allow(ip string, key bool) (*loginSource, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	global := g.login
	if key {
		global = g.key
	}
	if global.TokensAt(now) < 1 {
		return nil, time.Second
	}
	source := g.source(ip, now)
	if !key && now.Before(source.blockedUntil) {
		return nil, source.blockedUntil.Sub(now)
	}
	limiter := source.login
	if key {
		limiter = source.key
	}
	if !limiter.AllowN(now, 1) {
		return nil, time.Second
	}
	global.AllowN(now, 1)
	return source, 0
}

func (g *loginGuard) record(source *loginSource, valid bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if valid {
		source.failures = 0
		source.blockedUntil = time.Time{}
		return
	}
	source.failures++
	if source.failures >= 5 {
		// 只对明确的凭据错误退避，最长 30 秒；限流请求本身不延长禁用时间。
		delay := min(time.Second<<min(source.failures-5, 5), 30*time.Second)
		source.blockedUntil = g.now().Add(delay)
	}
}

func (g *loginGuard) middleware(key bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		source, retry := g.allow(loginSourceIP(c.Request), key)
		if retry > 0 {
			seconds := int(math.Ceil(retry.Seconds()))
			c.Header("Retry-After", strconv.Itoa(seconds))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"code": 429, "msg": "请求过于频繁，请在 " + strconv.Itoa(seconds) + " 秒后重试"})
			return
		}
		if !key {
			body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, loginBodyLimit))
			if err != nil {
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"code": 413, "msg": "登录请求内容过大或读取失败"})
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
		}
		c.Next()
		if !key {
			if result, exists := c.Get("login_credentials_result"); exists {
				g.record(source, result.(bool))
			}
		}
	}
}
