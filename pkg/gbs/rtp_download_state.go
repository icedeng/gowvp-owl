package gbs

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/core/sms"
)

const (
	rtpDownloadWaiting   = "waiting_media"
	rtpDownloadReceiving = "receiving"
	rtpDownloadCompleted = "completed"
	rtpDownloadStopped   = "stopped"

	rtpDownloadTerminalTTL              = 7 * 24 * time.Hour
	rtpDownloadMaxChannelTerminalStates = 4096
	rtpDownloadMaxSessionTerminalStates = 2048
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

type rtpDownloadTerminalEntry struct {
	key       string
	session   *rtpDownloadSession
	completed time.Time
}

// cleanupRTPDownloads 清理普通 RTP 下载终态。
// 通道最近状态与级联独立会话分别限额，避免级联会话挤占通道查询状态；活动状态不参与清理。
func (g *GB28181API) cleanupRTPDownloads(now time.Time) {
	if g == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-rtpDownloadTerminalTTL)
	channelTerminals := make([]rtpDownloadTerminalEntry, 0)
	sessionTerminals := make([]rtpDownloadTerminalEntry, 0)
	g.rtpDownloads.Range(func(key, value any) bool {
		keyText, keyOK := key.(string)
		session, sessionOK := value.(*rtpDownloadSession)
		if !keyOK || !sessionOK || session == nil {
			g.rtpDownloads.CompareAndDelete(key, value)
			return true
		}
		state := session.snapshot()
		if state.CompletedAt.IsZero() {
			return true
		}
		if !state.CompletedAt.After(cutoff) {
			g.rtpDownloads.CompareAndDelete(key, session)
			return true
		}
		entry := rtpDownloadTerminalEntry{key: keyText, session: session, completed: state.CompletedAt}
		if keyText == historyKey(historyModeDownload, state.DeviceID, state.ChannelID) {
			channelTerminals = append(channelTerminals, entry)
		} else {
			sessionTerminals = append(sessionTerminals, entry)
		}
		return true
	})
	trim := func(terminals []rtpDownloadTerminalEntry, limit int) {
		if len(terminals) <= limit {
			return
		}
		sort.Slice(terminals, func(i, j int) bool {
			if terminals[i].completed.Equal(terminals[j].completed) {
				return terminals[i].key < terminals[j].key
			}
			return terminals[i].completed.Before(terminals[j].completed)
		})
		for _, entry := range terminals[:len(terminals)-limit] {
			g.rtpDownloads.CompareAndDelete(entry.key, entry.session)
		}
	}
	trim(channelTerminals, rtpDownloadMaxChannelTerminalStates)
	trim(sessionTerminals, rtpDownloadMaxSessionTerminalStates)
}

func (g *GB28181API) cleanupRuntimeStates(now time.Time) {
	if g == nil {
		return
	}
	g.cleanupRTPDownloads(now)
	if g.directDownloads != nil {
		g.directDownloads.Cleanup(now)
	}
	g.cleanupQueryStates(now)
	g.cleanupUpgradeStates(now)
	g.cleanupSnapshotStates(now)
	g.cleanupTaskStates(now)
	g.cleanupCascadeTaskRoutes(now)
}

func runtimeStateExpired(updatedAt, now time.Time, ttl time.Duration) bool {
	if updatedAt.IsZero() || ttl <= 0 {
		return true
	}
	if now.IsZero() {
		now = time.Now()
	}
	return !updatedAt.After(now.Add(-ttl))
}

// startRuntimeStateCleaner 的生命周期与 GB28181 服务一致，保证无新请求时也会回收终态、查询/升级/抓拍快照和过期文件。
func (g *GB28181API) startRuntimeStateCleaner() {
	g.cleanupRuntimeStates(time.Now())
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-g.lifecycleDone:
			return
		case now := <-ticker.C:
			g.cleanupRuntimeStates(now)
		}
	}
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
