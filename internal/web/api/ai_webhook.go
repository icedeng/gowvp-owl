package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/internal/rpc"
	"github.com/gowvp/owl/protos"
	"github.com/ixugo/goddd/pkg/reason"
	"github.com/ixugo/goddd/pkg/web"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AIWebhookAPI 处理 AI 分析服务的回调请求
type AIWebhookAPI struct {
	log     *slog.Logger
	conf    *conf.Bootstrap
	limiter func(identifier string) bool
	ai      *rpc.AIClient
	// API 会被按值复制，互斥锁必须共享。保护手工开关的 RPC/数据库提交与单步对账。
	taskMu         *sync.Mutex
	statusFn       func(context.Context, *protos.StatusRequest) (*protos.StatusResponse, error)
	listChannelsFn func(context.Context, string, int) ([]*ipc.Channel, error)
	// startCameraFn/stopCameraFn 仅覆盖 RPC 调用边界，便于验证数据库与外部任务的提交顺序。
	startCameraFn func(context.Context, *protos.StartCameraRequest) (*protos.StartCameraResponse, error)
	stopCameraFn  func(context.Context, *protos.StopCameraRequest) (*protos.StopCameraResponse, error)
	eventCore     event.Core
	ipcCore       ipc.Core
	// smsCore 在 StartAISyncLoop 时注入，供 ReloadAITask 使用
	smsCore      sms.Core
	smsCoreReady bool
}

// NewAIWebhookAPI 创建 AI Webhook API 实例
func NewAIWebhookAPI(conf *conf.Bootstrap, eventCore event.Core, ipcCore ipc.Core) AIWebhookAPI {
	return AIWebhookAPI{
		taskMu:    &sync.Mutex{},
		log:       slog.With("hook", "ai"),
		conf:      conf,
		ai:        rpc.NewAIClient("127.0.0.1:50051"),
		eventCore: eventCore,
		ipcCore:   ipcCore,
		limiter:   web.IDRateLimiter(0.2, 1, 3*time.Minute),
	}
}

// registerAIWebhookAPI 注册 AI 生命周期回调路由到 /ai group
// Python callback_url = http://host:port/ai，路径拼接后为：
//   - POST /ai/keepalive  AI 心跳
//   - POST /ai/started    AI 启动通知
//   - POST /ai/stopped    AI 任务停止通知
//   - POST /ai/events     检测事件（别名指向 /webhook/events 的 handler，统一入库逻辑）
func registerAIWebhookAPI(r gin.IRouter, api AIWebhookAPI, webhookAPI WebHookAPI, handler ...gin.HandlerFunc) {
	group := r.Group("/ai", handler...)
	lifecycle := group.Group("", sharedSecretAuth(func() string { return api.conf.AISecret }))
	lifecycle.POST("/keepalive", web.WrapH(api.onKeepalive))
	lifecycle.POST("/started", web.WrapH(api.onStarted))
	lifecycle.POST("/stopped", web.WrapH(api.onStopped))
	group.POST("/events", webhookAPI.onWebhookEvents)
}

// onKeepalive 接收 AI 服务心跳，用于监控 AI 服务存活状态
func (a AIWebhookAPI) onKeepalive(c *gin.Context, in *AIKeepaliveInput) (AIWebhookOutput, error) {
	var activeStreams int
	var uptimeSeconds int64
	if in.Stats != nil {
		activeStreams = in.Stats.ActiveStreams
		uptimeSeconds = in.Stats.UptimeSeconds
	}
	a.log.InfoContext(c.Request.Context(), "ai keepalive",
		"timestamp", in.Timestamp,
		"message", in.Message,
		"active_streams", activeStreams,
		"uptime_seconds", uptimeSeconds,
	)
	return newAIWebhookOutputOK(), nil
}

// onStarted 接收 AI 服务启动通知，确认 AI 服务已就绪
func (a AIWebhookAPI) onStarted(c *gin.Context, in *AIStartedInput) (AIWebhookOutput, error) {
	a.log.InfoContext(c.Request.Context(), "ai started",
		"timestamp", in.Timestamp,
		"message", in.Message,
	)
	return newAIWebhookOutputOK(), nil
}

// onStopped 接收 AI 任务停止通知，记录停止原因
func (a AIWebhookAPI) onStopped(c *gin.Context, in *AIStoppedInput) (AIWebhookOutput, error) {
	a.log.InfoContext(c.Request.Context(), "ai task stopped",
		"camera_id", in.CameraID,
		"timestamp", in.Timestamp,
		"reason", in.Reason,
		"message", in.Message,
	)
	// 回调可能迟到；实际任务状态只以 GetStatus 为准。
	return newAIWebhookOutputOK(), nil
}

// StartAISyncLoop 启动 30 秒后首次同步，之后每 5 分钟对账并恢复失败任务。
func (a *AIWebhookAPI) StartAISyncLoop(ctx context.Context, smsCore sms.Core) {
	a.smsCore = smsCore
	a.smsCoreReady = true
	go func() {
		// 延迟 30 秒再首次同步，等待设备注册和 catalog 更新完成，避免读到过期状态
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
			a.syncAITasks(ctx, smsCore)
		}

		ticker := time.NewTimer(5 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				a.log.Info("AI sync loop stopped")
				return
			case <-ticker.C:
				a.syncAITasks(ctx, smsCore)
				ticker.Reset(5 * time.Minute)
			}
		}
	}()
}

// syncAITasks 同步 AI 任务状态，确保数据库中 enabled_ai=true 的通道正在运行检测，enabled_ai=false 的已停止
func (a *AIWebhookAPI) syncAITasks(ctx context.Context, smsCore sms.Core) {
	if a.conf.Server.AI.Disabled || a.ai == nil {
		return
	}

	ctx, cancelSync := context.WithTimeout(ctx, 5*time.Minute)
	defer cancelSync()
	// 必须先拿到完整期望集合和成功的实际状态，查询失败时不做任何启停。
	channels, err := a.listAIChannels(ctx)
	if err != nil {
		a.log.ErrorContext(ctx, "sync AI channels failed", "err", err)
		return
	}
	statusCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	var actual *protos.StatusResponse
	if a.statusFn != nil {
		actual, err = a.statusFn(statusCtx, &protos.StatusRequest{})
	} else {
		actual, err = a.ai.GetStatus(statusCtx, &protos.StatusRequest{})
	}
	cancel()
	if err != nil || actual == nil || !actual.GetIsReady() {
		a.log.WarnContext(ctx, "AI status unavailable; skip sync", "err", err)
		return
	}

	// 先停止禁用/离线任务释放容量；终态任务由 Python 在 StartCamera 内安全重建。
	present := make(map[string]string, len(actual.GetCameras()))
	for _, task := range actual.GetCameras() {
		id := task.GetCameraId()
		present[id] = task.GetStatus()
		a.taskMu.Lock()
		ch, getErr := a.ipcCore.GetChannel(ctx, id)
		if errors.Is(getErr, reason.ErrNotFound) || (getErr == nil && (!ch.Ext.EnabledAI || !ch.IsOnline)) {
			if stopErr := a.stopAIDetection(ctx, id); stopErr != nil {
				a.log.WarnContext(ctx, "sync: stop AI task failed", "channel_id", id, "err", stopErr)
			} else {
				delete(present, id)
			}
		} else if getErr != nil {
			a.log.WarnContext(ctx, "sync: read AI channel failed", "channel_id", id, "err", getErr)
		}
		a.taskMu.Unlock()
	}
	ids := make([]string, 0, len(channels))
	for id := range channels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	waiting := 0
	defer func() {
		if waiting > 0 {
			a.log.WarnContext(ctx, "AI task capacity reached; channels wait for next sync", "limit", a.conf.Server.AI.TaskLimit(), "waiting", waiting)
		}
	}()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		state, exists := present[id]
		if exists && state != "error" && state != "stopped" && state != "stopping" {
			continue
		}
		if limit := a.conf.Server.AI.TaskLimit(); !exists && limit > 0 && len(present) >= limit {
			waiting++
			continue
		}
		a.taskMu.Lock()
		// 分页期间可能发生开关/上下线变化，执行前重读，不使用过期快照。
		ch, getErr := a.ipcCore.GetChannel(ctx, id)
		if getErr == nil && ch.Ext.EnabledAI && ch.IsOnline {
			if startErr := a.startAITask(ctx, smsCore, ch); startErr != nil {
				a.log.WarnContext(ctx, "sync: start AI task failed", "channel_id", id, "err", startErr)
				// 全局容量/连接故障停止本轮，避免大批通道逐个等待 RPC 超时。
				code := status.Code(startErr)
				if code == codes.ResourceExhausted || code == codes.Unavailable || code == codes.DeadlineExceeded || code == codes.Canceled {
					a.taskMu.Unlock()
					return
				}
			} else {
				present[id] = "running"
			}
		}
		a.taskMu.Unlock()
	}
}

func (a *AIWebhookAPI) listAIChannels(ctx context.Context) (map[string]*ipc.Channel, error) {
	const pageSize = 200
	result := make(map[string]*ipc.Channel)
	after := ""
	for {
		var page []*ipc.Channel
		var err error
		if a.listChannelsFn != nil {
			page, err = a.listChannelsFn(ctx, after, pageSize)
		} else {
			page, err = a.ipcCore.ListAIChannelsAfter(ctx, after, pageSize)
		}
		if err != nil {
			return nil, err
		}
		for _, ch := range page {
			if ch.ID == "" || ch.ID == after {
				return nil, fmt.Errorf("AI channel cursor did not advance")
			}
			after = ch.ID
			if ch.Ext.EnabledAI && ch.IsOnline {
				result[ch.ID] = ch
			}
		}
		if len(page) < pageSize {
			return result, nil
		}
	}
}

// startAITask 启动单个通道的 AI 检测任务（内部使用，自动构建 RTSP URL）
func (a *AIWebhookAPI) startAITask(ctx context.Context, smsCore sms.Core, ch *ipc.Channel) error {
	rtspURL, err := buildChannelRTSPURL(ctx, smsCore, ch)
	if err != nil {
		return fmt.Errorf("build AI RTSP URL: %w", err)
	}
	_, err = a.startAIDetection(ctx, ch, rtspURL)
	return err
}

// stopAITask 停止单个通道的 AI 检测任务（内部使用）
func (a *AIWebhookAPI) stopAITask(ctx context.Context, channelID string) error {
	return a.stopAIDetection(ctx, channelID)
}

// StartAIDetection 启动 AI 检测任务，供外部调用（如 ipc.go 中的 enableAI）
func (a *AIWebhookAPI) StartAIDetection(ctx context.Context, ch *ipc.Channel, rtspURL string) (*protos.StartCameraResponse, error) {
	a.taskMu.Lock()
	defer a.taskMu.Unlock()
	return a.startAIDetection(ctx, ch, rtspURL)
}

func (a *AIWebhookAPI) startAIDetection(ctx context.Context, ch *ipc.Channel, rtspURL string) (*protos.StartCameraResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if a.ai == nil && a.startCameraFn == nil {
		return nil, fmt.Errorf("AI service not initialized")
	}

	zones := a.extractZoneConfig(ch)

	// 三级回退：通道自定义 → 配置文件全局默认 → 内置兜底 5 秒
	interval := ch.Ext.AnalysisInterval
	if interval <= 0 {
		interval = a.conf.Server.AI.AnalysisInterval
	}
	if interval <= 0 {
		interval = 5.0
	}
	cooldown := a.conf.Server.AI.AlertCooldownSeconds
	request := &protos.StartCameraRequest{
		CameraId:              ch.ID,
		CameraName:            ch.Name,
		RtspUrl:               rtspURL,
		DetectIntervalSeconds: interval,
		AlertCooldownSeconds:  cooldown,
		Zones:                 zones,
		Threshold:             0.75,
		RetryLimit:            10,
		CallbackUrl:           fmt.Sprintf("http://127.0.0.1:%d/ai", a.conf.Server.HTTP.Port),
		CallbackSecret:        a.conf.AISecret,
	}
	var resp *protos.StartCameraResponse
	var err error
	if a.startCameraFn != nil {
		resp, err = a.startCameraFn(ctx, request)
	} else {
		resp, err = a.ai.StartCamera(ctx, request)
	}
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("AI service returned an empty start response")
	}
	if !resp.GetSuccess() {
		message := strings.TrimSpace(resp.GetMessage())
		if message == "" {
			message = "AI service rejected the start request"
		}
		return nil, errors.New(message)
	}

	return resp, nil
}

// StopAIDetection 停止 AI 检测任务，供外部调用（如 ipc.go 中的 disableAI）
func (a *AIWebhookAPI) StopAIDetection(ctx context.Context, channelID string) error {
	a.taskMu.Lock()
	defer a.taskMu.Unlock()
	return a.stopAIDetection(ctx, channelID)
}

func (a *AIWebhookAPI) stopAIDetection(ctx context.Context, channelID string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if a.ai == nil && a.stopCameraFn == nil {
		return fmt.Errorf("AI service not initialized")
	}
	request := &protos.StopCameraRequest{
		CameraId: channelID,
	}
	var err error
	var resp *protos.StopCameraResponse
	if a.stopCameraFn != nil {
		resp, err = a.stopCameraFn(ctx, request)
	} else {
		resp, err = a.ai.StopCamera(ctx, request)
	}
	if err != nil {
		return err
	}
	if resp == nil || !resp.GetSuccess() {
		return fmt.Errorf("AI service did not confirm stop: %s", resp.GetMessage())
	}
	// 失败时保持数据库的期望状态，由下一轮实际状态对账重试。
	return nil
}

// extractZoneConfig 从通道配置中提取多区域信息，转为 proto AnalysisZone 列表
func (a *AIWebhookAPI) extractZoneConfig(ch *ipc.Channel) []*protos.AnalysisZone {
	var zones []*protos.AnalysisZone
	defaultLabels := []string{"person", "car", "cat", "dog"}

	for _, z := range ch.Ext.Zones {
		labels := z.Labels
		if len(labels) == 0 {
			labels = defaultLabels
		}
		zones = append(zones, &protos.AnalysisZone{
			Points: z.Coordinates,
			Labels: labels,
			Name:   z.Name,
		})
	}

	// 无区域配置时，下发空切片；Python 侧无区域则全图检测
	return zones
}

// ReloadAITask 停止再重启指定通道的 AI 任务，使区域、标签等配置立即生效
// 需要 StartAISyncLoop 已调用（smsCore 已注入）
func (a *AIWebhookAPI) ReloadAITask(ctx context.Context, ch *ipc.Channel) error {
	a.taskMu.Lock()
	defer a.taskMu.Unlock()
	// 区域更新后的旧快照不能重新启用已经被用户关闭的任务。
	latest, err := a.ipcCore.GetChannel(ctx, ch.ID)
	if err != nil {
		return err
	}
	if !latest.Ext.EnabledAI {
		return nil
	}
	ch = latest
	if !a.smsCoreReady {
		return fmt.Errorf("smsCore not initialized, StartAISyncLoop not called yet")
	}
	if err := a.stopAITask(ctx, ch.ID); err != nil {
		return fmt.Errorf("stop AI task before reload: %w", err)
	}
	return a.startAITask(ctx, a.smsCore, ch)
}
