package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/reason"
)

// snapshotConfig 只开放平台支持的策略与参数，不接受任意 FFmpeg 命令。
type snapshotConfig struct {
	ImmediateDispatch    bool   `json:"immediate_dispatch"`
	FFmpegMode           string `json:"ffmpeg_mode"`
	UploadTimeoutSeconds int    `json:"upload_timeout_seconds"`
	FFmpegConcurrency    int    `json:"ffmpeg_concurrency"`
	ScanIntervalMS       int    `json:"scan_interval_ms"`
	PollIntervalMS       int    `json:"poll_interval_ms"`
	FrameCount           int    `json:"frame_count"`
	SkipFrames           int    `json:"skip_frames"`
	FrameIntervalMS      int    `json:"frame_interval_ms"`
	StreamTimeoutSeconds int    `json:"stream_timeout_seconds"`
	FFmpegTimeoutSeconds int    `json:"ffmpeg_timeout_seconds"`
	RTSPTimeoutSeconds   int    `json:"rtsp_timeout_seconds"`
	ProbeTimeoutSeconds  int    `json:"probe_timeout_seconds"`
	AnalyzeDurationMS    int    `json:"analyze_duration_ms"`
	ProbeSizeBytes       int    `json:"probe_size_bytes"`
	JPEGQuality          int    `json:"jpeg_quality"`
}

func snapshotSettings(media conf.Media) snapshotConfig {
	value := snapshotConfig{
		ImmediateDispatch:    media.GBSnapshotImmediateDispatch,
		FFmpegMode:           media.GBSnapshotFFmpegMode,
		UploadTimeoutSeconds: media.GBSnapshotUploadTimeoutSeconds,
		FFmpegConcurrency:    media.GBSnapshotFFmpegConcurrency,
		ScanIntervalMS:       media.GBSnapshotScanIntervalMS,
		PollIntervalMS:       media.GBSnapshotPollIntervalMS,
		FrameCount:           media.GBSnapshotFrameCount,
		SkipFrames:           media.GBSnapshotSkipFrames,
		FrameIntervalMS:      media.GBSnapshotFrameIntervalMS,
		StreamTimeoutSeconds: media.GBSnapshotStreamTimeoutSeconds,
		FFmpegTimeoutSeconds: media.GBSnapshotFFmpegTimeoutSeconds,
		RTSPTimeoutSeconds:   media.GBSnapshotRTSPTimeoutSeconds,
		ProbeTimeoutSeconds:  media.GBSnapshotProbeTimeoutSeconds,
		AnalyzeDurationMS:    media.GBSnapshotAnalyzeDurationMS,
		ProbeSizeBytes:       media.GBSnapshotProbeSizeBytes,
		JPEGQuality:          media.GBSnapshotJPEGQuality,
	}
	if value.FFmpegMode != "single_frame" {
		value.FFmpegMode = "multi_frame"
	}
	if value.UploadTimeoutSeconds <= 0 {
		value.UploadTimeoutSeconds = 120
	}
	if value.FFmpegConcurrency <= 0 {
		value.FFmpegConcurrency = 2
	}
	if value.ScanIntervalMS <= 0 {
		value.ScanIntervalMS = 1000
	}
	if value.PollIntervalMS <= 0 {
		value.PollIntervalMS = 2000
	}
	if value.FrameCount <= 0 {
		value.FrameCount = 5
	}
	if value.SkipFrames <= 0 {
		value.SkipFrames = 75
	}
	if value.FrameIntervalMS <= 0 {
		value.FrameIntervalMS = 1000
	}
	if value.StreamTimeoutSeconds <= 0 {
		value.StreamTimeoutSeconds = 60
	}
	if value.FFmpegTimeoutSeconds <= 0 {
		value.FFmpegTimeoutSeconds = 35
	}
	if value.RTSPTimeoutSeconds <= 0 {
		value.RTSPTimeoutSeconds = 10
	}
	if value.ProbeTimeoutSeconds <= 0 {
		value.ProbeTimeoutSeconds = 10
	}
	if value.AnalyzeDurationMS <= 0 {
		value.AnalyzeDurationMS = 5000
	}
	if value.ProbeSizeBytes <= 0 {
		value.ProbeSizeBytes = 5000000
	}
	if value.JPEGQuality <= 0 {
		value.JPEGQuality = 2
	}
	return value
}

func (value snapshotConfig) validate() error {
	if value.FFmpegMode != "multi_frame" && value.FFmpegMode != "single_frame" {
		return fmt.Errorf("不支持的FFmpeg取帧模式")
	}
	for _, field := range []struct {
		name            string
		value, min, max int
	}{
		{"设备上传等待秒数", value.UploadTimeoutSeconds, 1, 3600},
		{"FFmpeg并发", value.FFmpegConcurrency, 1, 32},
		{"扫描间隔毫秒", value.ScanIntervalMS, 100, 60000},
		{"轮询间隔毫秒", value.PollIntervalMS, 200, 60000},
		{"多帧候选数量", value.FrameCount, 2, 30},
		{"跳过帧数", value.SkipFrames, 1, 1000},
		{"多帧采样间隔毫秒", value.FrameIntervalMS, 40, 10000},
		{"取流任务总超时秒数", value.StreamTimeoutSeconds, 1, 3600},
		{"FFmpeg取帧超时秒数", value.FFmpegTimeoutSeconds, 1, 3600},
		{"RTSP超时秒数", value.RTSPTimeoutSeconds, 1, 3600},
		{"ffprobe超时秒数", value.ProbeTimeoutSeconds, 1, 3600},
		{"分析预算毫秒", value.AnalyzeDurationMS, 100, 60000},
		{"探测字节预算", value.ProbeSizeBytes, 32768, 100000000},
		{"JPEG质量参数", value.JPEGQuality, 2, 31},
	} {
		if field.value < field.min || field.value > field.max {
			return fmt.Errorf("%s须为%d–%d", field.name, field.min, field.max)
		}
	}
	if value.FFmpegTimeoutSeconds > value.StreamTimeoutSeconds || (value.FFmpegMode == "multi_frame" && value.ProbeTimeoutSeconds > value.StreamTimeoutSeconds) {
		return fmt.Errorf("取流任务总超时不能小于取帧或探测超时")
	}
	if value.RTSPTimeoutSeconds > value.FFmpegTimeoutSeconds {
		return fmt.Errorf("RTSP超时不能大于FFmpeg取帧超时")
	}
	return nil
}

func (value snapshotConfig) applyTo(media *conf.Media) {
	media.GBSnapshotImmediateDispatch = value.ImmediateDispatch
	media.GBSnapshotFFmpegMode = value.FFmpegMode
	media.GBSnapshotUploadTimeoutSeconds = value.UploadTimeoutSeconds
	media.GBSnapshotFFmpegConcurrency = value.FFmpegConcurrency
	media.GBSnapshotScanIntervalMS = value.ScanIntervalMS
	media.GBSnapshotPollIntervalMS = value.PollIntervalMS
	media.GBSnapshotFrameCount = value.FrameCount
	media.GBSnapshotSkipFrames = value.SkipFrames
	media.GBSnapshotFrameIntervalMS = value.FrameIntervalMS
	media.GBSnapshotStreamTimeoutSeconds = value.StreamTimeoutSeconds
	media.GBSnapshotFFmpegTimeoutSeconds = value.FFmpegTimeoutSeconds
	media.GBSnapshotRTSPTimeoutSeconds = value.RTSPTimeoutSeconds
	media.GBSnapshotProbeTimeoutSeconds = value.ProbeTimeoutSeconds
	media.GBSnapshotAnalyzeDurationMS = value.AnalyzeDurationMS
	media.GBSnapshotProbeSizeBytes = value.ProbeSizeBytes
	media.GBSnapshotJPEGQuality = value.JPEGQuality
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

// getSnapshotConfig godoc
// @Summary 获取抓拍设置（不含其他系统配置）
// @Tags Config
// @Produce json
// @Security BearerAuth
// @Success 200 {object} snapshotConfig
// @Router /configs/info/snapshot [get]
func (a ConfigAPI) getSnapshotConfig(_ *gin.Context, _ *struct{}) (*snapshotConfig, error) {
	if a.conf == nil || a.sipMu == nil {
		return nil, reason.ErrServer.WithMsg("抓拍配置不可用")
	}
	a.sipMu.RLock()
	defer a.sipMu.RUnlock()
	value := snapshotSettings(a.conf.Media)
	return &value, nil
}

// updateSnapshotConfig godoc
// @Summary 设置抓拍调度、取帧与管理端轮询参数
// @Tags Config
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body snapshotConfig true "省略的字段沿用当前配置"
// @Success 200 {object} snapshotConfig
// @Router /configs/info/snapshot [put]
func (a ConfigAPI) updateSnapshotConfig(_ *gin.Context, in *map[string]json.RawMessage) (*snapshotConfig, error) {
	if a.conf == nil || a.sipMu == nil {
		return nil, reason.ErrServer.WithMsg("抓拍配置不可用")
	}
	if in == nil || *in == nil {
		return nil, reason.ErrBadRequest.WithMsg("请提供抓拍配置")
	}
	a.sipMu.Lock()
	defer a.sipMu.Unlock()
	value := snapshotSettings(a.conf.Media)
	// 在当前有效配置上合并，兼容只提交旧版两个字段的客户端。
	for _, raw := range *in {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, reason.ErrBadRequest.WithMsg("抓拍配置不能为null")
		}
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, reason.ErrBadRequest.WithMsg(err.Error())
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, reason.ErrBadRequest.WithMsg(err.Error())
	}
	if err := value.validate(); err != nil {
		return nil, reason.ErrBadRequest.WithMsg(err.Error())
	}
	unlock := a.lockCredentials()
	defer unlock()
	candidate := *a.conf
	value.applyTo(&candidate.Media)
	if err := conf.WriteConfig(&candidate, a.conf.ConfigPath); err != nil {
		return nil, reason.ErrServer.WithMsg(err.Error())
	}
	a.conf.Media = candidate.Media
	if a.uc != nil {
		if a.uc.GB28181API.snapshot != nil {
			a.uc.GB28181API.snapshot.setLimit(value.FFmpegConcurrency)
		}
		if a.uc.GB28181API.captures != nil {
			a.uc.GB28181API.captures.SetScheduling(time.Duration(value.ScanIntervalMS)*time.Millisecond, value.ImmediateDispatch)
		}
	}
	return &value, nil
}
