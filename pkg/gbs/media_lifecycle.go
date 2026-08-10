package gbs

import (
	"context"
	"strings"
	"time"
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
	streamID := strings.TrimSpace(event.StreamID)
	if streamID == "" || g.streams == nil {
		return nil
	}
	if event.At.IsZero() {
		event.At = time.Now()
	}
	g.streams.Range(func(key string, stream *Streams) bool {
		if stream == nil || stream.StreamID != streamID {
			return true
		}
		if event.Active {
			stream.Stream = true
			stream.LastMediaAt = event.At
			stream.Status = 0
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
		if g.core.Store() != nil {
			_ = g.core.EditPlaying(ctx, stream.DeviceID, stream.ChannelID, false)
		}
		return true
	})
	return nil
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
