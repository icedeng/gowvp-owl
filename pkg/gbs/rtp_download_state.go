package gbs

import (
	"math"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/core/sms"
)

const (
	rtpDownloadWaiting   = "waiting_media"
	rtpDownloadReceiving = "receiving"
	rtpDownloadCompleted = "completed"
	rtpDownloadStopped   = "stopped"
)

// RTPDownloadState 描述普通 RTP 下载的媒体服务器接收进度。
// Received/ProgressPercent 来自 ZLMediaKit 媒体源字节统计，包含封装差异时仅作为近似进度。
type RTPDownloadState struct {
	SessionID       string    `json:"session_id"`
	DeviceID        string    `json:"device_id"`
	ChannelID       string    `json:"channel_id"`
	Status          string    `json:"status"`
	FileSize        int64     `json:"file_size"`
	FileSizeKnown   bool      `json:"file_size_known"`
	Received        int64     `json:"received"`
	BytesSpeed      uint64    `json:"bytes_speed"`
	ProgressPercent float64   `json:"progress_percent"`
	ProgressKnown   bool      `json:"progress_known"`
	Approximate     bool      `json:"approximate"`
	EndReason       string    `json:"end_reason,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	CompletedAt     time.Time `json:"completed_at,omitempty"`
}

type rtpDownloadSession struct {
	mu       sync.RWMutex
	state    RTPDownloadState
	server   *sms.MediaServer
	streamID string
}

func (s *rtpDownloadSession) snapshot() RTPDownloadState {
	s.mu.RLock()
	state := s.state
	s.mu.RUnlock()
	return state
}

func (g *GB28181API) registerRTPDownload(stream *Streams) {
	if stream == nil || stream.mediaServer == nil {
		return
	}
	now := time.Now()
	key := resolveHistorySessionKey(historyModeDownload, stream.DeviceID, stream.ChannelID, stream.sessionKey)
	g.rtpDownloads.Store(key, &rtpDownloadSession{
		state: RTPDownloadState{
			SessionID: stream.CallID, DeviceID: stream.DeviceID, ChannelID: stream.ChannelID,
			Status: rtpDownloadWaiting, FileSize: stream.FileSize, FileSizeKnown: stream.FileSizeKnown,
			StartedAt: now, UpdatedAt: now,
		},
		server: stream.mediaServer, streamID: stream.StreamID,
	})
}

func (g *GB28181API) refreshRTPDownload(session *rtpDownloadSession) RTPDownloadState {
	if session == nil {
		return RTPDownloadState{}
	}
	if g.sms == nil || session.server == nil {
		return session.snapshot()
	}
	items, err := g.sms.GetMediaInfo(session.server, "rtp", session.streamID)
	if err == nil {
		var total, speed uint64
		for _, item := range items {
			if item.TotalBytes > total {
				total = item.TotalBytes
			}
			if item.BytesSpeed > speed {
				speed = item.BytesSpeed
			}
		}
		received := int64(total)
		if total > math.MaxInt64 {
			received = math.MaxInt64
		}
		session.mu.Lock()
		if received > session.state.Received {
			session.state.Received = received
		}
		session.state.BytesSpeed = speed
		if session.state.Status == rtpDownloadWaiting && (received > 0 || len(items) > 0) {
			session.state.Status = rtpDownloadReceiving
		}
		if session.state.FileSizeKnown {
			session.state.ProgressKnown = true
			session.state.Approximate = true
			if session.state.FileSize == 0 {
				session.state.ProgressPercent = 100
			} else {
				session.state.ProgressPercent = float64(session.state.Received) * 100 / float64(session.state.FileSize)
				if session.state.ProgressPercent > 100 {
					session.state.ProgressPercent = 100
				}
			}
		}
		session.state.UpdatedAt = time.Now()
		session.mu.Unlock()
	}
	return session.snapshot()
}

func (g *GB28181API) finishRTPDownload(stream *Streams, status, reason string) {
	if stream == nil {
		return
	}
	key := resolveHistorySessionKey(historyModeDownload, stream.DeviceID, stream.ChannelID, stream.sessionKey)
	value, ok := g.rtpDownloads.Load(key)
	if !ok {
		return
	}
	session, ok := value.(*rtpDownloadSession)
	if !ok || session == nil {
		return
	}
	_ = g.refreshRTPDownload(session)
	now := time.Now()
	session.mu.Lock()
	if status == rtpDownloadStopped && session.state.FileSizeKnown && session.state.Received >= session.state.FileSize {
		status = rtpDownloadCompleted
	}
	session.state.Status = status
	session.state.EndReason = reason
	session.state.UpdatedAt = now
	session.state.CompletedAt = now
	if status == rtpDownloadCompleted && session.state.FileSizeKnown && session.state.Received >= session.state.FileSize {
		session.state.ProgressKnown = true
		session.state.ProgressPercent = 100
	}
	session.mu.Unlock()
}

// RTPDownloadByChannel 返回通道最近一次普通 RTP 下载状态。
func (g *GB28181API) RTPDownloadByChannel(deviceID, channelID string) (RTPDownloadState, bool) {
	value, ok := g.rtpDownloads.Load(historyKey(historyModeDownload, deviceID, channelID))
	if !ok {
		return RTPDownloadState{}, false
	}
	session, ok := value.(*rtpDownloadSession)
	if !ok || session == nil {
		return RTPDownloadState{}, false
	}
	return g.refreshRTPDownload(session), true
}
