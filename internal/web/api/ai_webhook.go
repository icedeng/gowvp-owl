package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/internal/rpc"
	"github.com/gowvp/owl/protos"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/web"
)

// AIWebhookAPI 处理 AI 分析服务的回调请求
type AIWebhookAPI struct {
	log     *slog.Logger
	conf    *conf.Bootstrap
	aiTasks *conc.Map[string, struct{}]
	limiter func(identifier string) bool
	ai      *rpc.AIClient
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
		log:       slog.With("hook", "ai"),
		conf:      conf,
		ai:        rpc.NewAIClient("127.0.0.1:50051"),
		aiTasks:   conc.NewMap[string, struct{}](),
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
	a.aiTasks.Delete(in.CameraID)
	return newAIWebhookOutputOK(), nil
}

// StartAISyncLoop 启动 AI 任务同步协程，启动后立即执行一次同步，之后每 5 分钟检测一次
// 立即执行是为了在服务重启后尽快恢复之前开启的 AI 分析任务
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

		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				a.log.Info("AI sync loop stopped")
				return
			case <-ticker.C:
				a.syncAITasks(ctx, smsCore)
			}
		}
	}()
}

// syncAITasks 同步 AI 任务状态，确保数据库中 enabled_ai=true 的通道正在运行检测，enabled_ai=false 的已停止
func (a *AIWebhookAPI) syncAITasks(ctx context.Context, smsCore sms.Core) {
	if a.conf.Server.AI.Disabled || a.ai == nil {
		return
	}

	// 查询所有在线通道
	channels, _, err := a.ipcCore.ListChannels(ctx, &ipc.FindChannelInput{
		PagerFilter: web.PagerFilter{Page: 1, Size: 999},
		IsOnline:    "true",
	})
	if err != nil {
		a.log.ErrorContext(ctx, "sync ai tasks: find channels failed", "err", err)
		return
	}

	// 构建数据库中 enabled_ai=true 的通道集合
	dbEnabledSet := make(map[string]*ipc.Channel)
	for _, ch := range channels {
		if ch.Ext.EnabledAI {
			dbEnabledSet[ch.ID] = ch
		}
	}

	// 收集内存中正在运行的任务
	memoryTasks := make(map[string]struct{})
	a.aiTasks.Range(func(key string, _ struct{}) bool {
		memoryTasks[key] = struct{}{}
		return true
	})

	// 需要启动的任务：数据库中 enabled 但内存中没有
	for channelID, ch := range dbEnabledSet {
		if _, exists := memoryTasks[channelID]; !exists {
			a.log.Info("sync: starting AI task", "channel_id", channelID)
			if err := a.startAITask(ctx, smsCore, ch); err != nil {
				a.log.ErrorContext(ctx, "sync: start AI task failed", "channel_id", channelID, "err", err)
			}
		}
	}

	// 需要停止的任务：内存中有但数据库中 disabled
	for channelID := range memoryTasks {
		if _, exists := dbEnabledSet[channelID]; !exists {
			a.log.Info("sync: stopping AI task", "channel_id", channelID)
			if err := a.stopAITask(ctx, channelID); err != nil {
				a.log.ErrorContext(ctx, "sync: stop AI task failed", "channel_id", channelID, "err", err)
			}
		}
	}
}

// startAITask 启动单个通道的 AI 检测任务（内部使用，自动构建 RTSP URL）
func (a *AIWebhookAPI) startAITask(ctx context.Context, smsCore sms.Core, ch *ipc.Channel) error {
	rtspURL, err := buildChannelRTSPURL(ctx, smsCore, ch)
	if err != nil {
		return fmt.Errorf("build AI RTSP URL: %w", err)
	}
	_, err = a.StartAIDetection(ctx, ch, rtspURL)
	return err
}

// stopAITask 停止单个通道的 AI 检测任务（内部使用）
func (a *AIWebhookAPI) stopAITask(ctx context.Context, channelID string) error {
	return a.StopAIDetection(ctx, channelID)
}

// StartAIDetection 启动 AI 检测任务，供外部调用（如 ipc.go 中的 enableAI）
func (a *AIWebhookAPI) StartAIDetection(ctx context.Context, ch *ipc.Channel, rtspURL string) (*protos.StartCameraResponse, error) {
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

	a.aiTasks.Store(ch.ID, struct{}{})
	return resp, nil
}

// StopAIDetection 停止 AI 检测任务，供外部调用（如 ipc.go 中的 disableAI）
func (a *AIWebhookAPI) StopAIDetection(ctx context.Context, channelID string) error {
	if a.ai == nil && a.stopCameraFn == nil {
		return nil
	}
	request := &protos.StopCameraRequest{
		CameraId: channelID,
	}
	var err error
	if a.stopCameraFn != nil {
		_, err = a.stopCameraFn(ctx, request)
	} else {
		_, err = a.ai.StopCamera(ctx, request)
	}
	if err != nil {
		return err
	}
	// 只有外部任务确认停止后才释放所有权；失败时由同步器继续重试。
	a.aiTasks.Delete(channelID)
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
	if !a.smsCoreReady {
		return fmt.Errorf("smsCore not initialized, StartAISyncLoop not called yet")
	}
	if err := a.stopAITask(ctx, ch.ID); err != nil {
		return fmt.Errorf("stop AI task before reload: %w", err)
	}
	return a.startAITask(ctx, a.smsCore, ch)
}
