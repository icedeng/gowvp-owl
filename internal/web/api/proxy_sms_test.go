package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/internal/core/sms/store/smsdb"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

const testPlaySecret = "unit-test-secret"

// makePlayToken 生成播放 token 用于测试
func makePlayToken(t *testing.T, app, stream string, expiresAt time.Time) string {
	t.Helper()
	secret := testPlaySecret + "_play"
	token, err := web.NewToken(
		map[string]any{"stream": stream, "app": app},
		secret,
		web.WithExpiresAt(expiresAt),
	)
	if err != nil {
		t.Fatalf("生成播放 token 失败: %v", err)
	}
	return token
}

func makePlayTokenForMedia(t *testing.T, app, stream, mediaServerID string) string {
	t.Helper()
	token, err := web.NewToken(
		map[string]any{"stream": stream, "app": app, "media_server_id": mediaServerID},
		testPlaySecret+"_play",
		web.WithExpiresAt(time.Now().Add(time.Hour)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// newTestUsecase 创建带最小配置的 Usecase 用于测试 verifyPlayToken
func newTestUsecase() *Usecase {
	return &Usecase{
		Conf: &conf.Bootstrap{
			Server: conf.Server{
				HTTP: conf.ServerHTTP{
					JwtSecret: testPlaySecret,
				},
			},
		},
	}
}

func TestVerifyPlayToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	uc := newTestUsecase()

	tests := []struct {
		name       string
		path       string
		query      string
		wantStatus int
	}{
		{
			name:       "正常 http_flv 请求",
			path:       "/rtp/34020000001320000001.live.flv",
			query:      "token=" + makePlayToken(t, "rtp", "34020000001320000001", time.Now().Add(42*time.Hour)),
			wantStatus: http.StatusOK,
		},
		{
			name:       "正常 hls 请求",
			path:       "/live/camera01/hls.fmp4.m3u8",
			query:      "token=" + makePlayToken(t, "live", "camera01", time.Now().Add(42*time.Hour)),
			wantStatus: http.StatusOK,
		},
		{
			name:       "webrtc 请求（stream 在 query 中）",
			path:       "/index/api/webrtc",
			query:      "app=rtp&stream=34020000001320000001&type=play&token=" + makePlayToken(t, "rtp", "34020000001320000001", time.Now().Add(42*time.Hour)),
			wantStatus: http.StatusOK,
		},
		{
			name:       "缺少 token",
			path:       "/rtp/34020000001320000001.live.flv",
			query:      "",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "token 已过期",
			path:       "/rtp/34020000001320000001.live.flv",
			query:      "token=" + makePlayToken(t, "rtp", "34020000001320000001", time.Now().Add(-1*time.Hour)),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "token 中 stream 与路径不匹配",
			path:       "/rtp/other_stream.live.flv",
			query:      "token=" + makePlayToken(t, "rtp", "34020000001320000001", time.Now().Add(42*time.Hour)),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "错误密钥签发的 token",
			path:       "/rtp/34020000001320000001.live.flv",
			query:      "token=" + makeTokenWithWrongSecret(t),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "HLS 分片 init.mp4 无 token 豁免",
			path:       "/rtp/34020000001320000001/init.mp4",
			query:      "",
			wantStatus: http.StatusOK,
		},
		{
			name:       "HLS 分片 ts 无 token 豁免",
			path:       "/rtp/34020000001320000001/2026-07-29/22/56-45_21.mp4",
			query:      "",
			wantStatus: http.StatusOK,
		},
		{
			name:       "HLS 分片 m4s 无 token 豁免",
			path:       "/rtp/34020000001320000001/seg-1.m4s",
			query:      "",
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := gin.New()
			r.GET("/proxy/sms/*path", func(c *gin.Context) {
				path := c.Param("path")
				if err := uc.verifyPlayToken(c, path); err != nil {
					c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": 403, "msg": err.Error()})
					return
				}
				c.String(http.StatusOK, "pass")
			})

			w := httptest.NewRecorder()
			reqURL := "/proxy/sms" + tt.path
			if tt.query != "" {
				reqURL += "?" + tt.query
			}
			req := httptest.NewRequest(http.MethodGet, reqURL, nil)
			r.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("want status %d, got %d, body: %s", tt.wantStatus, w.Code, w.Body.String())
			}
		})
	}
}

func TestProxySMSRoutesBoundMediaServerAndStripsNodePrefix(t *testing.T) {
	var upstreamPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		_, _ = w.Write([]byte("edge media"))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, found := strings.Cut(upstreamURL.Host, ":")
	if !found {
		t.Fatalf("unexpected upstream host %q", upstreamURL.Host)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	store := smsdb.NewDB(db).AutoMigrate(true)
	server := &sms.MediaServer{ID: "edge-zlm-1", IP: host, Ports: sms.MediaServerPorts{HTTP: port}}
	if err := store.MediaServer().Create(t.Context(), server); err != nil {
		t.Fatal(err)
	}
	core := sms.NewCore(store)
	t.Cleanup(core.Close)
	uc := newTestUsecase()
	uc.SMSAPI = SmsAPI{smsCore: core}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/proxy/sms/*path", uc.proxySMS)
	proxyServer := httptest.NewServer(router)
	defer proxyServer.Close()
	token := makePlayTokenForMedia(t, "rtp", "stream-1", server.ID)
	response, err := http.Get(proxyServer.URL + "/proxy/sms/_media/edge-zlm-1/rtp/stream-1.live.flv?token=" + url.QueryEscape(token))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "edge media" {
		t.Fatalf("proxy response = %d %q, want edge media", response.StatusCode, body)
	}
	if upstreamPath != "/rtp/stream-1.live.flv" {
		t.Fatalf("upstream path = %q, want stripped media route", upstreamPath)
	}
}

func TestVerifyPlayTokenRejectsDifferentMediaServerRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	uc := newTestUsecase()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	token := makePlayTokenForMedia(t, "rtp", "stream-1", "edge-zlm-1")
	ctx.Request = httptest.NewRequest(
		http.MethodGet,
		"/proxy/sms/_media/edge-zlm-2/rtp/stream-1.live.flv?token="+url.QueryEscape(token),
		nil,
	)
	if err := uc.verifyPlayToken(ctx, "/_media/edge-zlm-2/rtp/stream-1.live.flv"); err == nil {
		t.Fatal("token bound to edge-zlm-1 accepted edge-zlm-2 route")
	}
}

func TestLegacyProxyPathNeverTreatsAppAsMediaServerRoute(t *testing.T) {
	uc := newTestUsecase()
	mediaServerID, upstreamPath := uc.inferSMSProxyRoute(t.Context(), "/rtp/stream-1.live.flv")
	if mediaServerID != sms.DefaultMediaServerID || upstreamPath != "/rtp/stream-1.live.flv" {
		t.Fatalf("legacy route = %q %q, want unchanged default-node route", mediaServerID, upstreamPath)
	}
}

// makeTokenWithWrongSecret 用错误密钥生成 token
func makeTokenWithWrongSecret(t *testing.T) string {
	t.Helper()
	token, err := web.NewToken(
		map[string]any{"stream": "34020000001320000001", "app": "rtp"},
		"wrong-secret_play",
	)
	if err != nil {
		t.Fatalf("生成错误密钥 token 失败: %v", err)
	}
	return token
}
