package gbs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
)

// MediaStreamEvent 抽象 ZLM/LALMAX 的流注册、注销和 RTP 超时事件。
type MediaStreamEvent struct {
	StreamID string
	Active   bool
	Reason   string
	At       time.Time
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
		talkErr = g.startTalkRTP(streamID)
	} else if value, ok := g.talkSessions.Load(streamID); ok {
		if session, ok := value.(*talkSession); ok {
			_ = g.stopTalkSession(session, fmt.Errorf("Talk media stream stopped: %s", strings.TrimSpace(event.Reason)))
		}
	}
	if !event.Active {
		g.terminateCascadeVoiceSource(streamID)
	}
	g.streams.Range(func(key string, stream *Streams) bool {
		if stream == nil || stream.StreamID != streamID {
			return true
		}
		if event.Active {
			stream.Stream = true
			stream.LastMediaAt = event.At
			stream.Status = 0
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
		g.metrics.mediaDisconnects.Add(1)
		if !g.streams.CompareAndDelete(key, stream) {
			return true
		}
		stream.Stream = false
		stream.Stop = true
		stream.Status = 1
		stream.LastMediaAt = event.At
		stream.EndReason = strings.TrimSpace(event.Reason)
		if stream.EndReason == "" {
			stream.EndReason = "stream_unregistered"
		}
		if strings.HasPrefix(key, "history:"+historyModeDownload+":") && !stream.DirectTCP {
			status := rtpDownloadStopped
			if stream.FileSizeKnown {
				if state, ok := g.RTPDownloadByChannel(stream.DeviceID, stream.ChannelID); ok && state.Received >= stream.FileSize {
					status = rtpDownloadCompleted
				}
			}
			g.finishRTPDownload(stream, status, stream.EndReason)
		}
		if g.core.Store() != nil {
			if !g.hasActiveChannelStream(stream.DeviceID, stream.ChannelID) {
				_ = g.core.EditPlaying(ctx, stream.DeviceID, stream.ChannelID, false)
			}
		}
		g.terminateDeviceMediaStream(ctx, stream)
		g.terminateCascadeSessionsForStream(stream)
		return true
	})
	return talkErr
}

// terminateDeviceMediaStream 在媒体服务器确认流丢失后通知直接下级设备并释放 RTP 接收端口。
// 流已由调用方原子删除，因此重复注销事件不会重复发送 BYE。
func (g *GB28181API) terminateDeviceMediaStream(ctx context.Context, stream *Streams) {
	if g == nil || stream == nil {
		return
	}
	if stream.Resp != nil && g.svr != nil && g.svr.Server != nil {
		request, err := sip.NewRequestFromResponseChecked(sip.MethodBYE, stream.Resp)
		var target Targeter
		if g.svr.memoryStorer != nil {
			if channel, ok := g.svr.memoryStorer.GetChannel(stream.DeviceID, stream.ChannelID); ok {
				target = channel
			}
		}
		if err == nil {
			err = prepareDialogRequestTransport(request, target)
		}
		if err == nil {
			_, err = g.svr.Request(request)
		}
		if err != nil {
			slog.WarnContext(ctx, "notify device after GB28181 media stream loss failed",
				"device_id", stream.DeviceID, "channel_id", stream.ChannelID, "stream_id", stream.StreamID, "err", err)
		}
	}
	if g.sms != nil && stream.mediaServer != nil && strings.TrimSpace(stream.StreamID) != "" {
		if _, err := g.sms.CloseRTPServer(stream.mediaServer, zlm.CloseRTPServerRequest{StreamID: stream.StreamID}); err != nil {
			slog.WarnContext(ctx, "close GB28181 RTP server after media stream loss failed",
				"device_id", stream.DeviceID, "channel_id", stream.ChannelID, "stream_id", stream.StreamID, "err", err)
		}
	}
}

func (s *Server) OnMediaStreamChanged(ctx context.Context, streamID string, active bool, reason string) error {
	if s == nil || s.gb == nil {
		return nil
	}
	return s.gb.OnMediaStreamChanged(ctx, MediaStreamEvent{
		StreamID: streamID,
		Active:   active,
		Reason:   reason,
	})
}
