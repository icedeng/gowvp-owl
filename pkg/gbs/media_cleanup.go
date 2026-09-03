package gbs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
)

// rememberMediaDialogCleanupResult 记录已确认对话的最佳努力 BYE 结果。
// BYE 失败时保留响应，后续统一清理器可重建请求并重试。
func (g *GB28181API) rememberMediaDialogCleanupResult(stream *Streams, response *sip.Response, cleanupErr error) {
	if stream == nil {
		return
	}
	stream.cleanupMu.Lock()
	if cleanupErr == nil {
		stream.dialogStopped = true
	} else if stream.Resp == nil {
		stream.Resp = response
	}
	stream.cleanupMu.Unlock()
}

func (g *GB28181API) deferMediaStreamDialogCleanup(stream *Streams) {
	if stream == nil {
		return
	}
	stream.cleanupMu.Lock()
	if stream.Resp != nil && !stream.dialogStopped {
		stream.dialogCleanupDeferred = true
	}
	stream.cleanupMu.Unlock()
}

func (g *GB28181API) resumeMediaStreamDialogCleanup(stream *Streams) {
	if stream == nil {
		return
	}
	stream.cleanupMu.Lock()
	stream.dialogCleanupDeferred = false
	stream.cleanupMu.Unlock()
}

// cleanupFailedMediaStart 把启动失败对象提交为可重试终态；只有 SIP 与 RTP 均清理成功才移除索引。
func (g *GB28181API) cleanupFailedMediaStart(key string, stream *Streams, reason string) error {
	if stream == nil {
		return nil
	}
	g.markMediaStreamStopped(stream, reason, false)
	_, err := g.cleanupMediaStreamContext(g.mediaPersistenceContext(), key, stream)
	return err
}

// markMediaStreamStopped 先把会话从活动语义中摘除，再执行可能失败的外部清理。
// 返回 true 表示本次首次提交终止态，调用方可据此只执行一次下载/级联等业务副作用。
func (g *GB28181API) markMediaStreamStopped(stream *Streams, reason string, dialogEnded bool) bool {
	if stream == nil {
		return false
	}
	stream.cleanupMu.Lock()
	first := !stream.Stop
	stream.Stop = true
	stream.cleanupRequested.Store(true)
	stream.Stream = false
	stream.Status = 1
	if first || strings.TrimSpace(stream.EndReason) == "" {
		stream.EndReason = strings.TrimSpace(reason)
	}
	if dialogEnded || stream.Resp == nil {
		stream.dialogStopped = true
	}
	stream.cleanupMu.Unlock()
	return first
}

func (g *GB28181API) mediaStreamStopping(stream *Streams) bool {
	if stream == nil {
		return false
	}
	return stream.cleanupRequested.Load()
}

// cleanupMediaStreamContext 分别提交 SIP 对话与 RTP 接收端口的清理结果。
// 失败步骤保留在原流对象上供后续重试；只有两步都完成后才精确删除当前代次。
func (g *GB28181API) cleanupMediaStreamContext(ctx context.Context, key string, stream *Streams) (bool, error) {
	if g == nil || stream == nil {
		return true, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	stream.cleanupMu.Lock()
	if !stream.cleanupRequested.Load() {
		stream.cleanupMu.Unlock()
		return false, nil
	}
	var cleanupErr error
	if stream.Resp == nil {
		stream.dialogStopped = true
	} else if !stream.dialogStopped && !stream.dialogCleanupDeferred {
		err := g.sendStreamBYEContext(ctx, stream)
		if err == nil {
			stream.dialogStopped = true
		}
		cleanupErr = errors.Join(cleanupErr, err)
	}

	needsRTPServerClose := stream.mediaServer != nil && strings.TrimSpace(stream.StreamID) != ""
	if !needsRTPServerClose {
		stream.rtpClosed = true
	} else if !stream.rtpClosed && g.sms == nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("RTP media service is unavailable"))
	} else if !stream.rtpClosed {
		_, err := closeRTPServerContext(g.mediaPersistenceContext(), g.sms, stream.mediaServer, zlm.CloseRTPServerRequest{StreamID: stream.StreamID})
		if err == nil {
			stream.rtpClosed = true
		}
		cleanupErr = errors.Join(cleanupErr, err)
	}
	complete := stream.dialogStopped && stream.rtpClosed
	stream.cleanupMu.Unlock()

	if complete && g.streams != nil {
		g.compareAndDeleteChannelStream(key, stream)
	}
	if complete {
		stream.releaseSSRCReservation()
	}
	return complete, cleanupErr
}

// cleanupStoppedMediaSessions 只扫描已提交终止态的普通媒体对象，活动流和语音状态机不受影响。
func (g *GB28181API) cleanupStoppedMediaSessions() (pending bool) {
	if g == nil || g.streams == nil {
		return false
	}
	g.streams.Range(func(key string, stream *Streams) bool {
		if stream == nil {
			g.streams.CompareAndDelete(key, nil)
			return true
		}
		if g.pendingVoiceCleanupOwnsStream(key, stream) || !g.mediaStreamStopping(stream) {
			return true
		}
		if _, err := g.cleanupMediaStreamContext(g.mediaPersistenceContext(), key, stream); err != nil {
			slog.WarnContext(g.mediaPersistenceContext(), "retry GB28181 media cleanup failed",
				"key", key, "device_id", stream.DeviceID, "channel_id", stream.ChannelID, "stream_id", stream.StreamID, "err", err)
		}
		if current, exists := g.streams.Load(key); exists && current == stream {
			pending = true
		}
		return true
	})
	return pending
}

func (g *GB28181API) retryStoppedMediaSessions(ctx context.Context, interval time.Duration) error {
	if g == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = voiceShutdownRetryInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if !g.cleanupStoppedMediaSessions() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
