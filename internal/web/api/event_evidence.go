package api

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/ixugo/goddd/pkg/system"
)

func setupEventEvidence(uc *Usecase) {
	w := uc.EventAPI.eventCore.Workflow()
	if w == nil {
		return
	}
	w.SetEvidenceExecutor(func(ctx context.Context, e *event.Event, kind string, seconds int) (string, error) {
		return uc.GB28181API.collectEventEvidence(ctx, e, kind, seconds)
	})
}

func (a IPCAPI) collectEventEvidence(ctx context.Context, e *event.Event, kind string, seconds int) (string, error) {
	ch, err := a.ipc.GetChannel(ctx, e.CID)
	if err != nil || ch.DID != e.DID {
		return "", fmt.Errorf("通道不存在或已变更归属，无法取证")
	}
	if !ch.IsOnline {
		return "", fmt.Errorf("通道离线，无法获取报警现场画面")
	}
	root := filepath.Join(system.Getwd(), "configs", "events")
	if err := os.MkdirAll(root, 0750); err != nil {
		return "", fmt.Errorf("无法创建事件取证目录")
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(root, &stat); err != nil {
		return "", fmt.Errorf("无法检查取证存储空间")
	}
	if stat.Blocks > 0 && (float64(stat.Bavail)/float64(stat.Blocks) < 0.05 || uint64(stat.Bavail)*uint64(stat.Bsize) < 256<<20) {
		return "", fmt.Errorf("事件存储空间不足，已暂停取证")
	}
	if kind == "video" && (a.uc == nil || a.uc.Conf.Server.Recording.Disabled) {
		return "", fmt.Errorf("平台录制已禁用")
	}
	if seconds < 5 || seconds > 120 {
		return "", fmt.Errorf("事件录像时长无效")
	}
	release, _, err := a.snapshotCoordinator().acquire(ctx, a.snapshotFFmpegConcurrency())
	if err != nil {
		return "", fmt.Errorf("事件录像等待资源超时")
	}
	defer release()
	if time.Since(e.StartedAt.Time) > 120*time.Second {
		return "", fmt.Errorf("等待资源超出实时取证窗口，未补拍之后的画面")
	}
	// 复用实时取流，不改变常规录像策略，也不停止其他观看者使用的流。
	if !ch.IsPlaying && ch.GetType() != ipc.TypeRTMP {
		dev, err := a.ipc.GetDevice(ctx, ch.DID)
		if err != nil {
			return "", fmt.Errorf("设备不存在")
		}
		protocol := a.ipc.GetProtocol(ch.GetType())
		if protocol == nil {
			return "", fmt.Errorf("设备协议不支持取流")
		}
		if _, err = protocol.StartPlay(ctx, dev, ch); err != nil {
			return "", fmt.Errorf("报警录像取流失败，请检查媒体链路")
		}
	}
	rtsp, err := a.buildRTSPURL(ctx, ch.ID)
	if err != nil {
		return "", fmt.Errorf("无法获取报警录像媒体地址")
	}
	if ch.GetType() == ipc.TypeRTMP && !ch.Config.IsAuthDisabled && ch.Config.Session != "" {
		rtsp += "?session=" + url.QueryEscape(ch.Config.Session)
	}
	if kind == "snapshot" {
		settings := a.captureSettings()
		count := settings.FrameCount
		if settings.FFmpegMode == "single_frame" {
			count = 1
		}
		image, err := a.extractSnapshotWithSettings(ctx, ch.ID, rtsp, time.Now(), count, settings)
		if err != nil {
			return "", fmt.Errorf("报警抓拍失败，请检查媒体链路及 FFmpeg")
		}
		relative := filepath.Join("snapshots", fmt.Sprintf("event-%d.jpg", e.ID))
		target := filepath.Join(root, relative)
		if err = os.MkdirAll(filepath.Dir(target), 0750); err != nil {
			return "", err
		}
		defer os.Remove(target + ".part")
		if err = os.WriteFile(target+".part", image.Image, 0640); err != nil {
			return "", fmt.Errorf("保存事件抓拍失败")
		}
		if err = os.Rename(target+".part", target); err != nil {
			return "", err
		}
		return filepath.ToSlash(relative), nil
	}
	relative := filepath.Join("clips", fmt.Sprintf("event-%d.mp4", e.ID))
	target := filepath.Join(root, relative)
	if err = os.MkdirAll(filepath.Dir(target), 0750); err != nil {
		return "", err
	}
	part := target + ".part.mp4"
	defer os.Remove(part)
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds+30)*time.Second)
	defer cancel()
	// 独立文件避免 stopRecord 中断常规录像；不收集带凭据的 FFmpeg 输出。
	cmd := exec.CommandContext(runCtx, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-rtsp_transport", "tcp", "-timeout", "10000000", "-i", rtsp, "-t", strconv.Itoa(seconds), "-map", "0:v:0", "-an", "-vf", "scale=w='min(1280,iw)':h=-2", "-c:v", "libx264", "-preset", "veryfast", "-crf", "25", "-threads", "1", "-movflags", "+faststart", "-fs", "67108864", part)
	if err = cmd.Run(); err != nil {
		return "", fmt.Errorf("报警录像失败，请检查媒体链路及 FFmpeg；本次未保存完整片段")
	}
	info, err := os.Stat(part)
	if err != nil || info.Size() < 1024 {
		return "", fmt.Errorf("未获取到有效的视频片段")
	}
	duration, probeErr := exec.CommandContext(runCtx, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", part).Output()
	length, parseErr := strconv.ParseFloat(strings.TrimSpace(string(duration)), 64)
	if probeErr != nil || parseErr != nil || length < float64(seconds)-1 {
		return "", fmt.Errorf("视频片段不完整，本次未保存")
	}
	if err = os.Chmod(part, 0640); err != nil {
		return "", err
	}
	if err = os.Rename(part, target); err != nil {
		return "", err
	}
	return filepath.ToSlash(relative), nil
}
