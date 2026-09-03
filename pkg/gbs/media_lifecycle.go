package gbs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gowvp/owl/internal/core/sms"
)

// MediaStreamEvent 抽象 ZLM/LALMAX 的流注册、注销和 RTP 超时事件。
type MediaStreamEvent struct {
	MediaServerID string
	StreamID      string
	Active        bool
	Reason        string
	At            time.Time
}

func mediaServerEventMatches(server *sms.MediaServer, reportedID string) bool {
	reportedID = strings.TrimSpace(reportedID)
	if reportedID == "" {
		return true
	}
	return server != nil && strings.TrimSpace(server.ID) == reportedID
}

// OnMediaStreamChanged 将媒体服务器状态收敛到 GB 会话。
// 媒体服务器负责无流/超时检测，本层只做状态和资源清理，不重复实现探测定时器。
func (g *GB28181API) OnMediaStreamChanged(ctx context.Context, event MediaStreamEvent) error {
	if ctx == nil {
		ctx = context.Background()
	}
	streamID := strings.TrimSpace(event.StreamID)
	if streamID == "" || g.streams == nil {
		return nil
	}
	if event.At.IsZero() {
		event.At = time.Now()
	}
	var talkErr error
	if event.Active {
		talkErr = g.startTalkRTPForMediaServer(streamID, event.MediaServerID)
	} else if value, ok := g.talkSessions.Load(streamID); ok {
		if session, ok := value.(*talkSession); ok {
			if mediaServerEventMatches(session.SMS, event.MediaServerID) {
				talkErr = g.stopTalkSession(session, fmt.Errorf("Talk media stream stopped: %s", strings.TrimSpace(event.Reason)))
			}
		}
	}
	if !event.Active {
		g.terminateCascadeVoiceSourceForMediaServer(streamID, event.MediaServerID)
	}
	g.streams.Range(func(key string, stream *Streams) bool {
		if stream == nil || stream.StreamID != streamID || !mediaServerEventMatches(stream.mediaServer, event.MediaServerID) {
			return true
		}
		if event.Active {
			stream.cleanupMu.Lock()
			if stream.cleanupRequested.Load() {
				stream.cleanupMu.Unlock()
				return true
			}
			stream.Stream = true
			stream.LastMediaAt = event.At
			stream.Status = 0
			stream.cleanupMu.Unlock()
			if strings.HasPrefix(key, "history:"+historyModeDownload+":") {
				if value, ok := g.rtpDownloads.Load(key); ok {
					if session, ok := value.(*rtpDownloadSession); ok {
						session.mu.Lock()
						session.state.Status = rtpDownloadReceiving
						session.state.UpdatedAt = event.At
						session.mu.Unlock()
					}
				}
			}
			return true
		}
		reason := strings.TrimSpace(event.Reason)
		if reason == "" {
			reason = "stream_unregistered"
		}
		firstStop := g.markMediaStreamStopped(stream, reason, false)
		stream.cleanupMu.Lock()
		stream.LastMediaAt = event.At
		endReason := stream.EndReason
		stream.cleanupMu.Unlock()
		if firstStop {
			g.metrics.mediaDisconnects.Add(1)
		}
		if firstStop && strings.HasPrefix(key, "history:"+historyModeDownload+":") && !stream.DirectTCP {
			status := rtpDownloadStopped
			if stream.FileSizeKnown {
				if state, ok := g.RTPDownloadByChannel(stream.DeviceID, stream.ChannelID); ok && state.Received >= stream.FileSize {
					status = rtpDownloadCompleted
				}
			}
			g.finishRTPDownload(stream, status, endReason)
		}
		if firstStop {
			if err := g.persistChannelIdleIfNoActive(ctx, stream.DeviceID, stream.ChannelID); err != nil {
				slog.WarnContext(ctx, "persist media disconnect channel state", "device_id", stream.DeviceID, "channel_id", stream.ChannelID, "err", err)
			}
			g.terminateCascadeSessionsForStream(stream)
			g.stopStandardTalkForPlayKey(key)
		}
		// Talk 会话分别提交 SIP、RTP 发送端与接收端的清理结果。
		// 任一步失败时必须由 Talk 状态机继续持有流，不能让通用清理提前删除重试索引。
		if g.pendingVoiceCleanupOwnsStream(key, stream) {
			return true
		}
		if _, err := g.cleanupMediaStreamContext(ctx, key, stream); err != nil {
			slog.WarnContext(ctx, "cleanup GB28181 media stream after disconnect failed",
				"device_id", stream.DeviceID, "channel_id", stream.ChannelID, "stream_id", stream.StreamID, "err", err)
		}
		return true
	})
	return talkErr
}

func (s *Server) OnMediaStreamChanged(ctx context.Context, streamID string, active bool, reason string) error {
	return s.OnMediaServerStreamChanged(ctx, "", streamID, active, reason)
}

// OnMediaServerStreamChanged 按媒体节点和流 ID 精确收敛 Webhook 生命周期。
// mediaServerID 为空时保留旧调用方仅按流 ID 匹配的兼容语义。
func (s *Server) OnMediaServerStreamChanged(ctx context.Context, mediaServerID, streamID string, active bool, reason string) error {
	if s == nil || s.gb == nil {
		return nil
	}
	return s.gb.OnMediaStreamChanged(ctx, MediaStreamEvent{
		MediaServerID: mediaServerID,
		StreamID:      streamID,
		Active:        active,
		Reason:        reason,
	})
}
