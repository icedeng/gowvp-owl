package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gowvp/owl/internal/core/capture"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs"
	"github.com/ixugo/goddd/pkg/reason"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

func registerCaptures(r gin.IRouter, a IPCAPI, auth gin.HandlerFunc) {
	g := r.Group("/captures", auth)
	g.GET("", web.WrapH(a.listCaptures))
	g.GET("/:capture_id", web.WrapH(a.getCapture))
	g.GET("/:capture_id/image", a.captureImage)
	g.PUT("/:capture_id", web.WrapH(a.renameCapture))
	g.DELETE("/:capture_id", web.WrapH(a.deleteCapture))
}

func captureError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, capture.ErrBusy) {
		return reason.ErrBadRequest.WithMsg(err.Error())
	}
	if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, os.ErrNotExist) {
		return reason.ErrNotFound.WithMsg("抓拍记录或图片不存在")
	}
	return reason.ErrServer.WithMsg(err.Error())
}

// createCapture godoc
// @Summary 创建后台抓拍任务
// @Tags Capture
// @Security BearerAuth
// @Produce json
// @Param id path string true "通道 ID"
// @Success 200 {object} capture.Record
// @Router /channels/{id}/captures [post]
func (a IPCAPI) createCapture(c *gin.Context, _ *struct{}) (*capture.Record, error) {
	id, err := a.resolveChannelParamID(c)
	if err != nil {
		return nil, err
	}
	if _, err := a.ipc.GetChannel(c.Request.Context(), id); err != nil {
		return nil, err
	}
	row, err := a.captures.Enqueue(c.Request.Context(), id, "manual")
	return row, captureError(err)
}

type captureListInput struct {
	ChannelID string `form:"channel_id"`
	Page      int    `form:"page"`
	Size      int    `form:"size"`
}

type capturePage struct {
	Items []capture.Record `json:"items"`
	Total int64            `json:"total"`
}

// listCaptures godoc
// @Summary 分页查询永久抓拍记录
// @Tags Capture
// @Security BearerAuth
// @Produce json
// @Param channel_id query string false "通道 ID"
// @Param page query int false "页码"
// @Param size query int false "每页数量，最大 100"
// @Success 200 {object} capturePage
// @Router /captures [get]
func (a IPCAPI) listCaptures(c *gin.Context, in *captureListInput) (*capturePage, error) {
	if in.Page < 1 {
		in.Page = 1
	}
	if in.Size < 1 {
		in.Size = 24
	}
	if in.Size > 100 {
		in.Size = 100
	}
	rows, total, err := a.captures.List(c.Request.Context(), in.ChannelID, in.Page, in.Size)
	return &capturePage{Items: rows, Total: total}, captureError(err)
}

// getCapture godoc
// @Summary 查询抓拍任务与图片状态
// @Tags Capture
// @Security BearerAuth
// @Produce json
// @Param capture_id path string true "抓拍 ID"
// @Success 200 {object} capture.Record
// @Router /captures/{capture_id} [get]
func (a IPCAPI) getCapture(c *gin.Context, _ *struct{}) (*capture.Record, error) {
	row, err := a.captures.Get(c.Request.Context(), c.Param("capture_id"))
	return row, captureError(err)
}

// captureImage godoc
// @Summary 查看或下载抓拍图片
// @Tags Capture
// @Security BearerAuth
// @Produce image/jpeg
// @Param capture_id path string true "抓拍 ID"
// @Param download query bool false "下载文件"
// @Success 200 {file} file
// @Router /captures/{capture_id}/image [get]
func (a IPCAPI) captureImage(c *gin.Context) {
	row, err := a.captures.Get(c.Request.Context(), c.Param("capture_id"))
	if err != nil || row.State != "succeeded" {
		c.AbortWithStatusJSON(404, gin.H{"msg": "图片不存在或尚未完成"})
		return
	}
	path, err := a.captures.Path(row)
	if err != nil {
		c.AbortWithStatus(400)
		return
	}
	disposition := "inline"
	if c.Query("download") == "true" {
		disposition = "attachment"
	}
	c.Header("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": row.Name}))
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.File(path)
}

type renameCaptureInput struct {
	Name string `json:"name"`
}

// renameCapture godoc
// @Summary 重命名抓拍图片（同时更新磁盘文件）
// @Tags Capture
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param capture_id path string true "抓拍 ID"
// @Param body body renameCaptureInput true "JPG 文件名"
// @Success 200 {object} capture.Record
// @Router /captures/{capture_id} [put]
func (a IPCAPI) renameCapture(c *gin.Context, in *renameCaptureInput) (*capture.Record, error) {
	if err := capture.ValidateName(in.Name); err != nil {
		return nil, reason.ErrBadRequest.WithMsg(err.Error())
	}
	row, err := a.captures.Rename(c.Request.Context(), c.Param("capture_id"), in.Name)
	return row, captureError(err)
}

// deleteCapture godoc
// @Summary 永久删除抓拍记录和图片文件
// @Tags Capture
// @Security BearerAuth
// @Produce json
// @Param capture_id path string true "抓拍 ID"
// @Success 200 {object} SwaggerMessageResponse
// @Router /captures/{capture_id} [delete]
func (a IPCAPI) deleteCapture(c *gin.Context, _ *struct{}) (gin.H, error) {
	err := a.captures.Delete(c.Request.Context(), c.Param("capture_id"))
	return gin.H{"msg": "ok"}, captureError(err)
}

// getCaptureSchedule godoc
// @Summary 获取通道定时抓拍计划
// @Tags Capture
// @Security BearerAuth
// @Produce json
// @Param id path string true "通道 ID"
// @Success 200 {object} capture.Schedule
// @Router /channels/{id}/capture_schedule [get]
func (a IPCAPI) getCaptureSchedule(c *gin.Context, _ *struct{}) (*capture.Schedule, error) {
	id, err := a.resolveChannelParamID(c)
	if err != nil {
		return nil, err
	}
	if _, err := a.ipc.GetChannel(c.Request.Context(), id); err != nil {
		return nil, err
	}
	row, err := a.captures.GetSchedule(c.Request.Context(), id)
	return row, captureError(err)
}

// saveCaptureSchedule godoc
// @Summary 保存通道定时抓拍计划
// @Tags Capture
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param id path string true "通道 ID"
// @Param body body capture.Schedule true "间隔秒数，最少 10 秒；可选首次执行时间"
// @Success 200 {object} capture.Schedule
// @Router /channels/{id}/capture_schedule [put]
func (a IPCAPI) saveCaptureSchedule(c *gin.Context, in *capture.Schedule) (*capture.Schedule, error) {
	id, err := a.resolveChannelParamID(c)
	if err != nil {
		return nil, err
	}
	if _, err := a.ipc.GetChannel(c.Request.Context(), id); err != nil {
		return nil, err
	}
	in.ChannelID = id
	if in.IntervalSeconds < 10 || in.IntervalSeconds > 31536000 {
		return nil, reason.ErrBadRequest.WithMsg("抓拍间隔须为 10–31536000 秒")
	}
	if err := a.captures.SaveSchedule(c.Request.Context(), in); err != nil {
		return nil, captureError(err)
	}
	return a.captures.GetSchedule(c.Request.Context(), id)
}

func deviceSnapshotMode(ch *ipc.Channel, dev *ipc.Device) string {
	if ch.GetType() == ipc.TypeGB28181 && dev != nil {
		profile := *dev
		gbs.ApplyGBVersionProfile(&profile)
		if slices.Contains(profile.Ext.GBVersionCapabilities, "snapshot") {
			return "gb28181_device_upload"
		}
	}
	return "ffmpeg"
}

func (a IPCAPI) runCapture(ctx context.Context, row *capture.Record) error {
	ch, err := a.ipc.GetChannel(ctx, row.ChannelID)
	if err != nil {
		if errors.Is(err, reason.ErrNotFound) {
			_ = a.captures.DisableSchedules(ctx, []string{row.ChannelID})
		}
		return err
	}
	var dev *ipc.Device
	if ch.GetType() == ipc.TypeGB28181 || ch.GetType() == ipc.TypeOnvif {
		dev, err = a.ipc.GetDevice(ctx, ch.DID)
		if err != nil {
			return err
		}
	}
	if row.RequestURL == "" && deviceSnapshotMode(ch, dev) == "gb28181_device_upload" {
		// 每个任务独立回传键，禁止使用旧封面冒充本次结果。
		state, err := a.uc.SipServer.QuerySnapshotContext(ctx, ch.DeviceID, ch.ChannelID, row.ID)
		if err != nil {
			if saved, getErr := a.captures.Get(ctx, row.ID); getErr == nil && saved.State == "succeeded" {
				return nil
			}
			return err
		}
		if err := a.captures.SetSession(ctx, row.ID, state.SessionID); err != nil {
			return err
		}
		timeout := a.captureSettings().UploadTimeoutSeconds
		waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
		_, err = a.waitCapture(waitCtx, row.ID)
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("设备未在 %d 秒内上传图片", timeout)
		}
		return err
	}
	// 先排队取得 FFmpeg 名额，再启动取流计时，排队不会消耗设备取帧超时。
	release, _, err := a.snapshotCoordinator().acquire(ctx, a.snapshotFFmpegConcurrency())
	if err != nil {
		return err
	}
	defer release()
	// 旧版国标、ONVIF 和流协议直接取流，不先尝试设备上传或多级回退。
	streamCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if row.RequestURL == "" && !ch.IsPlaying && ch.GetType() != ipc.TypeRTMP {
		protocol := a.ipc.GetProtocol(ch.GetType())
		if protocol == nil {
			return fmt.Errorf("协议 %s 不支持实时流抓拍", ch.GetType())
		}
		if dev == nil {
			dev = &ipc.Device{}
		}
		if _, err := protocol.StartPlay(streamCtx, dev, ch); err != nil {
			return err
		}
	}
	rtspURL := row.RequestURL
	if rtspURL == "" {
		rtspURL, err = a.buildRTSPURL(streamCtx, ch.ID)
		if err != nil {
			return err
		}
	}
	if row.RequestURL == "" && ch.GetType() == ipc.TypeRTMP && !ch.Config.IsAuthDisabled && ch.Config.Session != "" {
		rtspURL += "?session=" + url.QueryEscape(ch.Config.Session)
	}
	at := time.Now()
	result, err := a.extractSnapshotByFFmpeg(streamCtx, ch.ID, rtspURL, at, 1)
	if err != nil {
		return err
	}
	_, err = a.captures.Complete(streamCtx, row.ID, result.Image, "ffmpeg")
	return err
}

func (a IPCAPI) waitCapture(ctx context.Context, id string) (*capture.Record, error) {
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		row, err := a.captures.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		switch row.State {
		case "succeeded":
			return row, nil
		case "failed", "deleting":
			return nil, fmt.Errorf("抓拍失败：%s", row.Error)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

func (a IPCAPI) captureSynchronously(c *gin.Context, requestURL string) (any, error) {
	id, err := a.resolveChannelParamID(c)
	if err != nil {
		return nil, err
	}
	if _, err := a.ipc.GetChannel(c.Request.Context(), id); err != nil {
		return nil, err
	}
	row, err := a.captures.EnqueueURL(c.Request.Context(), id, "manual", requestURL)
	if err != nil {
		return nil, err
	}
	row, err = a.waitCapture(c.Request.Context(), row.ID)
	if err != nil {
		return nil, captureError(err)
	}
	prefix := web.GetBaseURL(c.Request)
	if value := c.GetHeader("X-Forwarded-Prefix"); value != "" {
		prefix = value
	}
	return gin.H{"link": fmt.Sprintf("%s/captures/%s/image?token=%s", prefix, row.ID, url.QueryEscape(c.GetString("token"))), "method": row.Method, "capture_id": row.ID, "session_id": row.SessionID}, nil
}

// captureHandler 将后台任务生命周期绑定到应用退出，不绑定某一次 HTTP 请求。
type captureHandler struct {
	http.Handler
	cancel context.CancelFunc
	done   <-chan struct{}
}

func (h *captureHandler) Close() error {
	h.cancel()
	<-h.done
	if closer, ok := h.Handler.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func isPersistentCaptureKey(key string) bool { _, err := uuid.Parse(key); return err == nil }
func validateLegacySnapshotUpload(body []byte, key string) error {
	if isPersistentCaptureKey(key) {
		return nil
	} // 永久抓拍在 Complete 中做大小、分辨率与解码检查。
	return validateSnapshotImage(body)
}
func validateLegacySnapshotFrame(body []byte, count int) error {
	if count == 1 {
		return nil
	}
	return validateSnapshotImage(body)
}
