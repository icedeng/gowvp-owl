package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/ipc"
	op "github.com/gowvp/owl/internal/core/openplatform"
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/gowvp/owl/internal/web/openapi"
	"github.com/ixugo/goddd/pkg/system"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type openHost struct{ uc *Usecase }

func (h openHost) SnapshotTimeout() time.Duration {
	// 后台任务覆盖宿主设备上传等待与取流/队列余量，同步 HTTP 等待单独限时。
	settings := h.uc.GB28181API.captureSettings()
	return time.Duration(max(settings.UploadTimeoutSeconds, settings.StreamTimeoutSeconds)+120) * time.Second
}

func (h openHost) Start(ctx context.Context, id string) (op.Source, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ch, e := h.uc.GB28181API.ipc.GetChannel(ctx, id)
	if e != nil {
		return op.Source{}, e
	}
	ms, e := h.uc.SMSAPI.smsCore.GetMediaServer(ctx, channelMediaServerID(ch))
	if e != nil {
		return op.Source{}, e
	}
	if ms.Type != "" && !strings.EqualFold(ms.Type, "zlm") {
		return op.Source{}, op.Fail(400, "UNSUPPORTED_MEDIA_SERVER", "开放媒体入口首期支持ZLM节点")
	}
	source := op.Source{MediaServerID: ms.ID, App: ch.GetApp(), Stream: ch.GetStream(), BaseURL: "http://" + net.JoinHostPort(ms.IP, strconv.Itoa(ms.Ports.HTTP))}
	if source.App == "" {
		source.App = "live"
	}
	if !ch.Config.IsAuthDisabled && ch.Config.Session != "" {
		source.Query = "session=" + url.QueryEscape(ch.Config.Session)
	}
	// 已在播放的共享源直接复用，只有缺失时才触发宿主拉流编排。
	if info, err := h.uc.SMSAPI.smsCore.GetMediaInfoContext(ctx, ms, source.App, source.Stream); err == nil && len(info) > 0 {
		return source, nil
	}
	protocol, ok := h.uc.WebHookAPI.protocols[ch.GetType()]
	if !ok {
		return source, op.ErrNotFound
	}
	if aware, ok := protocol.(ipc.MediaServerAwareHooker); ok {
		e = aware.OnStreamNotFoundOnMediaServer(ctx, ms.ID, source.App, source.Stream)
	} else {
		e = protocol.OnStreamNotFound(ctx, source.App, source.Stream)
	}
	if e != nil {
		return source, e
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return source, ctx.Err()
		case <-tick.C:
			info, err := h.uc.SMSAPI.smsCore.GetMediaInfoContext(ctx, ms, source.App, source.Stream)
			if err == nil && len(info) > 0 {
				return source, nil
			}
		}
	}
}
func (h openHost) Snapshot(ctx context.Context, id, dest string) error {
	a := h.uc.GB28181API
	row, err := a.captures.Enqueue(ctx, id, "openapi")
	if err != nil {
		return err
	}
	row, err = a.waitCapture(ctx, row.ID)
	if err != nil {
		return err
	}
	path, err := a.captures.Path(row)
	if err != nil {
		return err
	}
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer target.Close()
	if _, err = io.Copy(target, source); err != nil {
		return err
	}
	return target.Sync()
}

// CapturePath 只向开放平台内部返回文件路径，HTTP 响应使用受控票据地址。
func (h openHost) CapturePath(ctx context.Context, id string) (string, error) {
	row, err := h.uc.GB28181API.captures.Get(ctx, id)
	if err != nil || row.State != "succeeded" {
		return "", op.ErrNotFound
	}
	return h.uc.GB28181API.captures.Path(row)
}
func (h openHost) Capabilities(ctx context.Context, id string) ([]string, error) {
	ch, e := h.uc.GB28181API.ipc.GetChannel(ctx, id)
	if e != nil {
		return nil, e
	}
	dev, e := h.uc.GB28181API.ipc.GetDevice(ctx, ch.DID)
	if e != nil {
		return nil, e
	}
	switch dev.GetType() {
	case ipc.TypeGB28181:
		return append(append([]string{}, op.BasicActions...), "preset_set", "preset_delete", "cruise_add", "cruise_del", "cruise_speed", "cruise_stay", "cruise_start", "scan_start", "scan_left", "scan_right", "scan_speed", "aux_on", "aux_off"), nil
	case ipc.TypeOnvif:
		return []string{"left", "right", "up", "down", "left_up", "left_down", "right_up", "right_down", "zoom_in", "zoom_out", "preset_call", "preset_set", "preset_delete"}, nil
	}
	return []string{}, nil
}
func (h openHost) PTZ(ctx context.Context, id string, in op.PTZInput) error {
	return h.uc.GB28181API.ipc.PTZControl(ipc.PTZBypass(ctx), id, &ipc.PTZControlInput{Action: in.Action, Speed: uint8(int(in.Speed) * 255 / 100), Timeout: 1, Preset: in.Preset, Group: in.Group, Aux: in.Aux, Value: in.Value, StopManaged: true})
}
func (h openHost) RecordingPath(ctx context.Context, id int64) (string, error) {
	var row recording.Recording
	if e := h.uc.DB.WithContext(ctx).First(&row, "id = ? AND delete_flag = ?", id, false).Error; e != nil {
		return "", e
	}
	return h.uc.RecordingAPI.recordingCore.ResolvePath(row.Path)
}
func (h openHost) EventImagePath(ctx context.Context, id int64) (string, error) {
	var row event.Event
	if e := h.uc.DB.WithContext(ctx).First(&row, "id = ?", id).Error; e != nil || row.ImagePath == "" {
		return "", op.ErrNotFound
	}
	root, e := filepath.EvalSymlinks(filepath.Join(system.Getwd(), "configs", "events"))
	if e != nil {
		return "", e
	}
	target, e := filepath.EvalSymlinks(filepath.Join(root, row.ImagePath))
	if e != nil {
		return "", e
	}
	rel, e := filepath.Rel(root, target)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", op.ErrNotFound
	}
	return target, nil
}
func strictOpenAdmin(uc *Usecase) gin.HandlerFunc {
	return func(c *gin.Context) {
		deny := func() {
			c.AbortWithStatusJSON(401, gin.H{"code": "ADMIN_REQUIRED", "message": "需要有效的平台管理员身份"})
		}
		auth := c.GetHeader("Authorization")
		if len(c.Request.Header.Values("Authorization")) != 1 || !strings.HasPrefix(auth, "Bearer ") {
			deny()
			return
		}
		claims, e := web.ParseToken(strings.TrimPrefix(auth, "Bearer "), uc.Conf.Server.HTTP.JwtSecret)
		if e != nil || claims.Valid() != nil || claims.ExpiresAt == nil {
			deny()
			return
		}
		uc.UserAPI.credentialsMu.RLock()
		username := uc.Conf.Server.Username
		uc.UserAPI.credentialsMu.RUnlock()
		if username == "" || claims.Data[web.KeyUsername] != username {
			deny()
			return
		}
		c.Set("open_admin", username)
		c.Next()
	}
}

type openHTTPHandler struct {
	http.Handler
	service *op.Service
}

func (h *openHTTPHandler) Close() error {
	if h.service != nil {
		h.service.Close()
		ipc.SetPTZCoordinator(nil, nil)
	}
	return nil
}
func setupOpenPlatform(r *gin.Engine, uc *Usecase) http.Handler {
	cfg, e := op.ConfigFromEnv(uc.Conf.ConfigDir)
	var service *op.Service
	if e == nil {
		service, e = op.New(uc.DB.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}), cfg, openHost{uc})
	}
	if e != nil {
		slog.Error("开放平台未就绪，独立入口拒绝服务；检查配置、密钥环与迁移", "reason", e.Error())
		service = nil
	}
	if service != nil && cfg.Enabled {
		if e = service.InstallOutbox(); e != nil {
			slog.Error("开放平台事件事务桥接失败")
			service = nil
		} else {
			ipc.SetPTZCoordinator(service.GuardPTZ, func(ctx context.Context, dev, ch string) (string, error) {
				row, e := uc.GB28181API.ipc.GetChannelByDeviceChannelID(ctx, dev, ch)
				if e != nil {
					return "", e
				}
				return row.ID, nil
			})
			service.Start(context.Background())
		}
	}
	openapi.New(service).Register(r, strictOpenAdmin(uc))
	// 包装层在Gin路由匹配和自动重定向之前拒绝不规范签名路径。
	return &openHTTPHandler{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/openapi/") && (!op.ValidPath(req.URL.EscapedPath()) || req.URL.RawPath != "" || strings.HasSuffix(req.URL.Path, "/")) {
			requestID := uuid.NewString()
			w.Header().Set("X-Request-ID", requestID)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_PATH", "message": "请求路径不规范", "request_id": requestID})
			return
		}
		r.ServeHTTP(w, req)
	}), service: service}
}
