package api

import (
	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/reason"
)

type snapshotConfig struct {
	UploadTimeoutSeconds int `json:"upload_timeout_seconds"`
	FFmpegConcurrency    int `json:"ffmpeg_concurrency"`
}

func snapshotSettings(media conf.Media) snapshotConfig {
	value := snapshotConfig{UploadTimeoutSeconds: media.GBSnapshotUploadTimeoutSeconds, FFmpegConcurrency: media.GBSnapshotFFmpegConcurrency}
	if value.UploadTimeoutSeconds <= 0 {
		value.UploadTimeoutSeconds = 120
	}
	if value.FFmpegConcurrency <= 0 {
		value.FFmpegConcurrency = 2
	}
	return value
}

func (a IPCAPI) captureSettings() snapshotConfig {
	if a.uc == nil || a.uc.Conf == nil {
		return snapshotSettings(conf.Media{})
	}
	mu := a.uc.ConfigAPI.sipMu
	if mu != nil {
		mu.RLock()
		defer mu.RUnlock()
	}
	return snapshotSettings(a.uc.Conf.Media)
}

// updateSnapshotConfig godoc
// @Summary 设置抓拍上传等待时间与 FFmpeg 并发
// @Tags Config
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body snapshotConfig true "上传等待秒数 1–3600，并发 1–32"
// @Success 200 {object} snapshotConfig
// @Router /configs/info/snapshot [put]
func (a ConfigAPI) updateSnapshotConfig(_ *gin.Context, in *snapshotConfig) (*snapshotConfig, error) {
	if in.UploadTimeoutSeconds < 1 || in.UploadTimeoutSeconds > 3600 || in.FFmpegConcurrency < 1 || in.FFmpegConcurrency > 32 {
		return nil, reason.ErrBadRequest.WithMsg("设备上传等待须为 1–3600 秒，FFmpeg 并发须为 1–32")
	}
	if a.conf == nil || a.sipMu == nil {
		return nil, reason.ErrServer.WithMsg("抓拍配置不可用")
	}
	a.sipMu.Lock()
	defer a.sipMu.Unlock()
	candidate := *a.conf
	candidate.Media.GBSnapshotUploadTimeoutSeconds = in.UploadTimeoutSeconds
	candidate.Media.GBSnapshotFFmpegConcurrency = in.FFmpegConcurrency
	if err := conf.WriteConfig(&candidate, a.conf.ConfigPath); err != nil {
		return nil, reason.ErrServer.WithMsg(err.Error())
	}
	a.conf.Media.GBSnapshotUploadTimeoutSeconds = in.UploadTimeoutSeconds
	a.conf.Media.GBSnapshotFFmpegConcurrency = in.FFmpegConcurrency
	if a.uc != nil && a.uc.GB28181API.snapshot != nil {
		a.uc.GB28181API.snapshot.setLimit(in.FFmpegConcurrency)
	}
	return in, nil
}
