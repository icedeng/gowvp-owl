package api

import (
	"context"
	"expvar"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-contrib/gzip"
	"github.com/gin-gonic/gin"
	docs "github.com/gowvp/owl/docs"
	"github.com/gowvp/owl/internal/core/metadata/metadataapi"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/internal/web/onvifserver"
	"github.com/gowvp/owl/plugin/stat"
	"github.com/gowvp/owl/plugin/stat/statapi"
	"github.com/ixugo/goddd/domain/version/versionapi"
	"github.com/ixugo/goddd/pkg/system"
	"github.com/ixugo/goddd/pkg/web"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

var startRuntime = time.Now()

func setupRouter(r *gin.Engine, uc *Usecase) {
	uc.GB28181API.uc = uc
	uc.SMSAPI.uc = uc
	uc.WebHookAPI.uc = uc
	// uc.MediaAPI.uc = uc // 已移除 push 模块
	const staticPrefix = "/web"

	go stat.LoadTop(system.Getwd(), func(m map[string]any) {
		_ = m
	})
	r.Use(
		// 格式化输出到控制台，然后记录到日志
		// 此处不做 recover，底层 http.server 也会 recover，但不会输出方便查看的格式
		gin.CustomRecovery(func(c *gin.Context, err any) {
			slog.ErrorContext(c.Request.Context(), "panic", "err", err, "stack", string(debug.Stack()))
			c.AbortWithStatus(http.StatusInternalServerError)
		}),
		web.Metrics(),
		web.Logger(web.IgnorePrefix(staticPrefix),
			web.IgnoreMethod(http.MethodOptions),
			web.IgnorePrefix("/openapi/"), web.IgnorePrefix("/openmedia/"), web.IgnorePrefix("/admin/open-platform"),
			web.IgnorePrefix("/events/image"),
			web.IgnorePrefix("/recordings/channels"), // m3u8 播放列表
			web.IgnorePrefix("/static/recordings"),   // 录像文件
		),
		web.LoggerWithBody(web.DefaultBodyLimit,
			web.IgnorePrefix("/app/upgrade"),
			web.IgnoreBool(uc.Conf.Debug),
			web.IgnoreMethod(http.MethodOptions),
			web.IgnorePrefix("/openapi/"), web.IgnorePrefix("/openmedia/"), web.IgnorePrefix("/admin/open-platform"),
			web.IgnorePrefix("/events/image"),
			web.IgnorePrefix("/recordings/channels"),
			web.IgnorePrefix("/static/recordings"),
		),
	)
	go web.CountGoroutines(10*time.Minute, 20)

	corsHandler := cors.New(cors.Config{
		AllowMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"},
		AllowHeaders: []string{
			"Accept", "Content-Length", "Content-Type", "Range", "Accept-Language",
			"Origin", "Authorization", "Referer", "User-Agent",
			"Accept-Encoding",
			"Cache-Control", "Pragma", "X-Requested-With",
			"Sec-Fetch-Mode", "Sec-Fetch-Site", "Sec-Fetch-Dest",
			"Sec-Ch-Ua", "Sec-Ch-Ua-Mobile", "Sec-Ch-Ua-Platform",
			"Dnt", "X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host",
			"X-Real-IP", "X-Request-ID", "X-Request-Start", "X-Request-Time",
		},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
		AllowOriginFunc: func(_ string) bool {
			return true
		},
	})
	r.Use(func(c *gin.Context) {
		path := c.Request.URL.Path
		if strings.HasPrefix(path, "/openapi/") || strings.HasPrefix(path, "/admin/open-platform") {
			if c.GetHeader("Origin") != "" {
				origin, err := url.Parse(c.GetHeader("Origin"))
				if err != nil || origin.Host != c.Request.Host {
					c.AbortWithStatusJSON(403, gin.H{"code": "ORIGIN_DENIED", "message": "开放接口仅支持同源管理或服务端调用"})
					return
				}
			}
			c.Next()
			return
		}
		if strings.HasPrefix(path, "/openmedia/") {
			c.Header("Access-Control-Allow-Origin", "*")
			c.Header("Access-Control-Allow-Methods", "GET, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Range, Authorization")
			c.Header("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")
			if c.Request.Method == "OPTIONS" {
				c.AbortWithStatus(204)
				return
			}
			c.Next()
			return
		}
		corsHandler(c)
	})

	const staticDir = "www"
	admin := r.Group(staticPrefix, gzip.Gzip(gzip.DefaultCompression))
	admin.Static("/", filepath.Join(system.Getwd(), staticDir))
	r.NoRoute(func(c *gin.Context) {
		// react-router 路由指向前端资源
		if strings.HasPrefix(c.Request.URL.Path, staticPrefix) {
			c.File(filepath.Join(system.Getwd(), staticDir, "index.html"))
			return
		}
		c.JSON(404, gin.H{"msg": "来到了无人的荒漠"})
	})
	// 访问根路径时重定向到前端资源
	r.GET("/", func(ctx *gin.Context) {
		ctx.Redirect(http.StatusPermanentRedirect, staticPrefix+"/"+"index.html")
	})

	// Swagger 文档入口。
	docs.SwaggerInfo.BasePath = "/"
	docs.SwaggerInfo.Version = uc.Conf.BuildVersion
	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	auth := AuthMiddleware(uc.Conf.Server.HTTP.JwtSecret, uc.Conf.Server.HTTP.AuthURL)
	onvifserver.Register(r, uc.GB28181API.ipc, uc.SMSAPI.smsCore, uc.Conf)
	r.Any("/health", web.WrapH(uc.getHealth))
	r.GET("/app/metrics/api", web.WrapH(uc.getMetricsAPI))
	uc.registerAppUpgrade(r)

	versionapi.Register(r, uc.Version, auth)
	statapi.Register(r)
	registerZLMWebhookAPI(r, uc.WebHookAPI)
	registerGB28181(r, uc.GB28181API, auth)
 registerCaptures(r, uc.GB28181API, auth)
	uc.ConfigAPI.uc = uc
	registerConfig(r, uc.ConfigAPI, auth)
	registerSms(r, uc.SMSAPI, auth)
	RegisterUser(r, uc.UserAPI, auth)

	registerWS(r, uc.Conf.Server.HTTP.JwtSecret)

	// 反向代理流媒体数据
	r.Any("/proxy/sms/*path", uc.proxySMS)

	// 注册 AI 分析服务回调接口，/ai/events 是 /webhook/events 的别名
	registerAIWebhookAPI(r, uc.AIWebhookAPI, uc.WebHookAPI)
	// 启动 AI 任务同步协程，每 5 分钟检测一次数据库与内存状态差异
	uc.AIWebhookAPI.StartAISyncLoop(context.Background(), uc.SMSAPI.smsCore)
	RegisterEvent(r, uc.EventAPI, auth)
	RegisterRecording(r, uc.RecordingAPI, auth)
	metadataapi.RegisterMetadata(r, uc.MetadataAPI, auth)
}

type playOutput struct {
	App    string               `json:"app"`
	Stream string               `json:"stream"`
	Items  []sms.StreamLiveAddr `json:"items"`
}

type getHealthOutput struct {
	Version   string    `json:"version"`
	StartAt   time.Time `json:"start_at"`
	GitBranch string    `json:"git_branch"`
	GitHash   string    `json:"git_hash"`
}

// getHealth godoc
// @Summary 健康检查
// @Description 返回服务版本、启动时间与构建信息
// @Tags System
// @Produce json
// @Success 200 {object} getHealthOutput
// @Router /health [get]
func (uc *Usecase) getHealth(_ *gin.Context, _ *struct{}) (getHealthOutput, error) {
	return getHealthOutput{
		Version:   uc.Conf.BuildVersion,
		GitBranch: strings.Trim(expvar.Get("git_branch").String(), `"`),
		GitHash:   strings.Trim(expvar.Get("git_hash").String(), `"`),
		StartAt:   startRuntime,
	}, nil
}

type getMetricsAPIOutput struct {
	RealTimeRequests int64  `json:"real_time_requests"` // 实时请求数
	TotalRequests    int64  `json:"total_requests"`     // 总请求数
	TotalResponses   int64  `json:"total_responses"`    // 总响应数
	RequestTop10     []KV   `json:"request_top10"`      // 请求TOP10
	StatusCodeTop10  []KV   `json:"status_code_top10"`  // 状态码TOP10
	Goroutines       any    `json:"goroutines"`         // 协程历史采样
	GoroutineCount   int    `json:"goroutine_count"`    // 当前运行协程数量
	NumGC            uint32 `json:"num_gc"`             // gc 次数
	SysAlloc         uint64 `json:"sys_alloc"`          // 内存占用
	StartAt          string `json:"start_at"`           // 运行时间
}

// getMetricsAPI godoc
// @Summary 获取服务指标
// @Tags System
// @Produce json
// @Success 200 {object} getMetricsAPIOutput
// @Failure 400 {object} SwaggerErrorResponse
// @Router /app/metrics/api [get]
func (uc *Usecase) getMetricsAPI(_ *gin.Context, _ *struct{}) (*getMetricsAPIOutput, error) {
	req := expvar.Get("request").(*expvar.Int).Value()
	reqs := expvar.Get("requests").(*expvar.Int).Value()
	resps := expvar.Get("responses").(*expvar.Int).Value()
	urls := expvar.Get(`requestURLs`).(*expvar.Map)
	status := expvar.Get(`statusCodes`).(*expvar.Map)
	u := sortExpvarMap(urls, 10)
	s := sortExpvarMap(status, 10)
	g := expvar.Get("goroutine_num").(expvar.Func)

	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)

	return &getMetricsAPIOutput{
		RealTimeRequests: req,
		TotalRequests:    reqs,
		TotalResponses:   resps,
		RequestTop10:     u,
		StatusCodeTop10:  s,
		Goroutines:       g(),
		GoroutineCount:   runtime.NumGoroutine(),
		NumGC:            stats.NumGC,
		SysAlloc:         stats.Sys,
		StartAt:          startRuntime.Format(time.DateTime),
	}, nil
}

type KV struct {
	Key   string
	Value int64
}

func sortExpvarMap(data *expvar.Map, top int) []KV {
	kvs := make([]KV, 0, 8)
	data.Do(func(kv expvar.KeyValue) {
		kvs = append(kvs, KV{
			Key:   kv.Key,
			Value: kv.Value.(*expvar.Int).Value(),
		})
	})

	sort.Slice(kvs, func(i, j int) bool {
		return kvs[i].Value > kvs[j].Value
	})

	idx := top
	if l := len(kvs); l < top {
		idx = len(kvs)
	}
	return kvs[:idx]
}

// proxySMS godoc
// @Summary 反向代理流媒体 HTTP 请求
// @Description 将请求透明转发到配置中的流媒体服务器 HTTP 端口
// @Tags MediaServer
// @Produce json
// @Param path path string true "代理路径"
// @Success 200 {string} string "代理响应"
// @Failure 400 {object} SwaggerErrorResponse
// @Router /proxy/sms/{path} [get]
func (uc *Usecase) proxySMS(c *gin.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.ErrorContext(c.Request.Context(), "proxy media server panic", "panic", recovered, "path", c.Request.URL.Path)
			if !c.Writer.Written() {
				c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"code": http.StatusBadGateway, "msg": "流媒体代理异常"})
			}
		}
	}()

	path := c.Param("path")

	// 播放流鉴权：校验 token 中的 app+stream 与请求路径的包含关系
	if err := uc.verifyPlayToken(c, path); err != nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": 403, "msg": err.Error()})
		return
	}

	rc := http.NewResponseController(c.Writer)
	exp := time.Now().AddDate(99, 0, 0)
	_ = rc.SetReadDeadline(exp)
	_ = rc.SetWriteDeadline(exp)

	mediaServerID := c.GetString("play_media_server_id")
	upstreamPath := path
	if mediaServerID == "" {
		mediaServerID, upstreamPath = uc.inferSMSProxyRoute(c.Request.Context(), path)
	} else {
		var ok bool
		upstreamPath, ok = stripSMSProxyRoute(path, mediaServerID)
		if !ok {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": 403, "msg": "播放 token 与媒体节点路由不匹配"})
			return
		}
	}
	server, err := uc.SMSAPI.smsCore.GetMediaServer(c.Request.Context(), mediaServerID)
	if err != nil {
		if mediaServerID != sms.DefaultMediaServerID {
			web.Fail(c, err)
			return
		}
		// 兼容旧部署：默认节点数据库尚未初始化时仍沿用原全局配置代理。
		server = &sms.MediaServer{ID: sms.DefaultMediaServerID, IP: uc.Conf.Media.IP}
		server.Ports.HTTP = uc.Conf.Media.HTTPPort
	}
	host := strings.TrimSpace(server.IP)
	port := server.Ports.HTTP
	if mediaServerID == sms.DefaultMediaServerID {
		if host == "" {
			host = strings.TrimSpace(uc.Conf.Media.IP)
		}
		if port == 0 {
			port = uc.Conf.Media.HTTPPort
		}
	}
	if host == "" || port <= 0 {
		web.Fail(c, fmt.Errorf("media server %q HTTP address is unavailable", mediaServerID))
		return
	}
	fullAddr := &url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(port))}
	c.Request.URL.Path = ""
	proxy := httputil.NewSingleHostReverseProxy(fullAddr)

	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = "http"
		req.URL.Host = fullAddr.Host
		req.URL.Path = upstreamPath
	}
	proxy.ModifyResponse = func(r *http.Response) error {
		r.Header.Del("Access-Control-Allow-Credentials")
		r.Header.Del("Access-Control-Allow-Origin")
		if r.StatusCode >= 300 && r.StatusCode < 400 {
			if l := r.Header.Get("Location"); l != "" {
				if !strings.HasPrefix(l, "http") {
					r.Header.Set("Location", "/proxy/sms/"+sms.MediaServerProxyRoute(mediaServerID)+"/"+strings.TrimPrefix(l, "/"))
				}
			}
		}
		return nil
	}
	proxy.ServeHTTP(c.Writer, c.Request)
}

func (uc *Usecase) inferSMSProxyRoute(ctx context.Context, path string) (string, string) {
	trimmed := strings.TrimPrefix(path, "/")
	prefix, rest, found := strings.Cut(trimmed, "/")
	if !found || prefix != sms.MediaServerProxyRoutePrefix {
		return sms.DefaultMediaServerID, path
	}
	segment, _, found := strings.Cut(rest, "/")
	if !found {
		return sms.DefaultMediaServerID, path
	}
	mediaServerID, err := url.PathUnescape(segment)
	if err == nil && strings.TrimSpace(mediaServerID) != "" {
		if _, err := uc.SMSAPI.smsCore.GetMediaServer(ctx, mediaServerID); err == nil {
			if stripped, ok := stripSMSProxyRoute(path, mediaServerID); ok {
				return mediaServerID, stripped
			}
		}
	}
	return sms.DefaultMediaServerID, path
}

func stripSMSProxyRoute(path, mediaServerID string) (string, bool) {
	trimmed := strings.TrimPrefix(path, "/")
	prefix, trimmed, found := strings.Cut(trimmed, "/")
	if !found || prefix != sms.MediaServerProxyRoutePrefix {
		return "", false
	}
	segment, rest, found := strings.Cut(trimmed, "/")
	decoded, err := url.PathUnescape(segment)
	if err != nil || decoded != mediaServerID {
		return "", false
	}
	if !found {
		return "/", true
	}
	return "/" + rest, true
}

// verifyPlayToken 校验播放 token：解析 JWT，确认 token 中的 app+stream 被请求路径包含
// HLS 子片段（init.mp4、ts 切片等）由播放器自动请求且不带 token，予以豁免
func (uc *Usecase) verifyPlayToken(c *gin.Context, path string) error {
	tokenStr := c.Query("token")
	if tokenStr == "" {
		if isHLSSegment(path) {
			return nil
		}
		return fmt.Errorf("缺少播放鉴权 token")
	}

	secret := uc.Conf.Server.HTTP.JwtSecret + "_play"
	claims, err := web.ParseToken(tokenStr, secret)
	if err != nil {
		return fmt.Errorf("无效的播放 token")
	}
	if err := claims.Valid(); err != nil {
		return fmt.Errorf("播放 token 已过期")
	}

	app, _ := claims.Data["app"].(string)
	stream, _ := claims.Data["stream"].(string)
	mediaServerID, _ := claims.Data["media_server_id"].(string)
	mediaServerID = strings.TrimSpace(mediaServerID)
	if mediaServerID != "" {
		if _, ok := stripSMSProxyRoute(path, mediaServerID); !ok {
			return fmt.Errorf("播放 token 与媒体节点路由不匹配")
		}
		c.Set("play_media_server_id", mediaServerID)
	}
	if stream == "" {
		return fmt.Errorf("播放 token 缺少 stream 信息")
	}

	// 校验：path 中必须包含 stream（对于 webrtc 类请求，stream 在 query 参数中）
	if strings.Contains(path, stream) {
		return nil
	}
	// webrtc 的 path 是 /index/api/webrtc，stream 在 query 中
	if qStream := c.Query("stream"); qStream == stream && c.Query("app") == app {
		return nil
	}

	return fmt.Errorf("播放 token 与请求流不匹配")
}

// isHLSSegment 判断路径是否为 HLS 子片段资源（m3u8 播放列表引用的 init.mp4、ts 切片等）
// 这些资源由播放器内部解析 m3u8 后自动请求，无法携带 query token，故豁免鉴权
func isHLSSegment(path string) bool {
	for _, suffix := range []string{".mp4", ".m4s", ".ts"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}
