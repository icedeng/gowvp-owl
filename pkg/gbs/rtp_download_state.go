package gbs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
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
// 有文件大小时 ProgressPercent 来自媒体源字节统计，仅作为近似进度；
// 无文件大小时优先使用媒体轨道时长计算，媒体服务器不提供时长则保持进度未知。
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
	mu                  sync.RWMutex
	persistMu           sync.Mutex
	state               RTPDownloadState
	key                 string
	stateRevision       uint64
	persistedRevision   uint64
	persistencePending  bool
	server              *sms.MediaServer
	streamID            string
	requestedDurationMS int64
}

type rtpDownloadPersistentState struct {
	Key      string           `json:"key"`
	Terminal bool             `json:"terminal"`
	State    RTPDownloadState `json:"state"`
}

func (s *rtpDownloadSession) snapshot() RTPDownloadState {
	s.mu.RLock()
	state := s.state
	s.mu.RUnlock()
	return state
}

func (g *GB28181API) registerRTPDownload(stream *Streams) error {
	if stream == nil || stream.mediaServer == nil {
		return nil
	}
	now := time.Now()
	requestedDurationMS := int64(0)
	if stream.E.After(stream.S) {
		requestedDurationMS = stream.E.Sub(stream.S).Milliseconds()
	}
	key := resolveHistorySessionKey(historyModeDownload, stream.DeviceID, stream.ChannelID, stream.sessionKey)
	session := &rtpDownloadSession{
		state: RTPDownloadState{
			SessionID: stream.CallID, DeviceID: stream.DeviceID, ChannelID: stream.ChannelID,
			Status: rtpDownloadWaiting, FileSize: stream.FileSize, FileSizeKnown: stream.FileSizeKnown,
			StartedAt: now, UpdatedAt: now,
		},
		key: key, stateRevision: 1, persistencePending: true,
		server: stream.mediaServer, streamID: stream.StreamID, requestedDurationMS: requestedDurationMS,
	}
	// 先用活动标记覆盖同通道旧终态。进程若在新下载期间退出，启动恢复会删除该标记，
	// 不会把更早一次下载误报成“最近完成”；未装配持久存储时保持原内存行为。
	if err := g.persistRTPDownloadState(session); err != nil {
		return err
	}
	g.rtpDownloads.Store(key, session)
	return nil
}

func (g *GB28181API) refreshRTPDownload(session *rtpDownloadSession) RTPDownloadState {
	if session == nil {
		return RTPDownloadState{}
	}
	if g.sms == nil || session.server == nil {
		return session.snapshot()
	}
	items, err := getMediaInfoContext(g.serviceContext(), g.sms, session.server, "rtp", session.streamID)
	if err == nil {
		var total, speed uint64
		var mediaDurationMS int64
		for _, item := range items {
			if item.TotalBytes > total {
				total = item.TotalBytes
			}
			if item.BytesSpeed > speed {
				speed = item.BytesSpeed
			}
			for _, track := range item.Tracks {
				if track.Duration > mediaDurationMS {
					mediaDurationMS = track.Duration
				}
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
		} else if session.requestedDurationMS > 0 && mediaDurationMS > 0 {
			progress := float64(mediaDurationMS) * 100 / float64(session.requestedDurationMS)
			if progress > 100 {
				progress = 100
			}
			if !session.state.ProgressKnown || progress > session.state.ProgressPercent {
				session.state.ProgressPercent = progress
			}
			session.state.ProgressKnown = true
			session.state.Approximate = false
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
	if session.snapshot().SessionID != stream.CallID {
		return
	}
	_ = g.refreshRTPDownload(session)
	now := time.Now()
	session.mu.Lock()
	if !allowRTPDownloadTerminalTransition(session.state, reason) {
		session.mu.Unlock()
		return
	}
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
	session.stateRevision++
	session.persistencePending = true
	session.mu.Unlock()
	if err := g.persistRTPDownloadState(session); err != nil && !g.serviceStopped() {
		slog.Warn("persist RTP download terminal state failed", "device_id", stream.DeviceID, "channel_id", stream.ChannelID, "session_id", stream.CallID, "err", err)
	}
}

// finishRTPDownloadByCallID 收敛流索引已被并发 BYE 删除、但 MediaStatus/121 随后到达的下载。
// 已由用户取消或停服结束的终态不能被迟到通知覆盖。
func (g *GB28181API) finishRTPDownloadByCallID(deviceID, targetID, callID, status, reason string) bool {
	if g == nil {
		return false
	}
	deviceID = strings.TrimSpace(deviceID)
	targetID = strings.TrimSpace(targetID)
	callID = normalizeStoredCallID(callID)
	if deviceID == "" || targetID == "" || callID == "" {
		return false
	}
	var matched *rtpDownloadSession
	g.rtpDownloads.Range(func(_, value any) bool {
		session, ok := value.(*rtpDownloadSession)
		if !ok || session == nil {
			return true
		}
		state := session.snapshot()
		if state.DeviceID != deviceID || normalizeStoredCallID(state.SessionID) != callID ||
			!mediaStatusTargetMatches(targetID, state.DeviceID, state.ChannelID) {
			return true
		}
		matched = session
		return false
	})
	if matched == nil {
		return false
	}
	_ = g.refreshRTPDownload(matched)
	now := time.Now()
	matched.mu.Lock()
	if !allowRTPDownloadTerminalTransition(matched.state, reason) {
		matched.mu.Unlock()
		return true
	}
	matched.state.Status = status
	matched.state.EndReason = reason
	matched.state.UpdatedAt = now
	matched.state.CompletedAt = now
	if status == rtpDownloadCompleted && matched.state.FileSizeKnown && matched.state.Received >= matched.state.FileSize {
		matched.state.ProgressKnown = true
		matched.state.ProgressPercent = 100
	}
	matched.stateRevision++
	matched.persistencePending = true
	matched.mu.Unlock()
	if err := g.persistRTPDownloadState(matched); err != nil && !g.serviceStopped() {
		slog.Warn("persist RTP download terminal state failed", "device_id", deviceID, "channel_id", targetID, "session_id", callID, "err", err)
	}
	return true
}

func rtpDownloadTaskIdentity(key string, state RTPDownloadState) (kind, sessionID string, ok bool) {
	channelKey := historyKey(historyModeDownload, state.DeviceID, state.ChannelID)
	if key == channelKey {
		return gbTaskKindRTPDownload, state.ChannelID, state.ChannelID != ""
	}
	if strings.HasPrefix(key, channelKey+":cascade:") && len(key) > len(channelKey+":cascade:") {
		return gbTaskKindRTPDownloadSession, state.SessionID, state.SessionID != ""
	}
	return "", "", false
}

func validPersistedRTPDownloadState(key, kind, recordDeviceID, recordSessionID string, state RTPDownloadState, now time.Time) bool {
	if state.DeviceID == "" || state.ChannelID == "" || state.SessionID == "" || state.DeviceID != recordDeviceID {
		return false
	}
	if state.Status != rtpDownloadCompleted && state.Status != rtpDownloadStopped {
		return false
	}
	if state.CompletedAt.IsZero() || state.UpdatedAt.IsZero() || state.StartedAt.IsZero() || state.EndReason == "" {
		return false
	}
	if state.UpdatedAt.Before(state.StartedAt) || state.CompletedAt.Before(state.StartedAt) || state.UpdatedAt.After(now.Add(5*time.Minute)) {
		return false
	}
	if state.FileSize < 0 || state.Received < 0 || math.IsNaN(state.ProgressPercent) || math.IsInf(state.ProgressPercent, 0) || state.ProgressPercent < 0 || state.ProgressPercent > 100 {
		return false
	}
	if runtimeStateExpired(state.CompletedAt, now, rtpDownloadTerminalTTL) || state.CompletedAt.After(now.Add(5*time.Minute)) {
		return false
	}
	expectedKind, expectedSessionID, ok := rtpDownloadTaskIdentity(key, state)
	return ok && kind == expectedKind && recordSessionID == expectedSessionID
}

func (g *GB28181API) persistRTPDownloadState(session *rtpDownloadSession) error {
	if g == nil || session == nil {
		return nil
	}
	if g.taskStateStorer() == nil {
		session.mu.Lock()
		session.persistedRevision = session.stateRevision
		session.persistencePending = false
		session.mu.Unlock()
		return nil
	}
	session.persistMu.Lock()
	defer session.persistMu.Unlock()

	session.mu.RLock()
	if !session.persistencePending {
		session.mu.RUnlock()
		return nil
	}
	state := session.state
	key := session.key
	revision := session.stateRevision
	session.mu.RUnlock()

	kind, sessionID, ok := rtpDownloadTaskIdentity(key, state)
	if !ok {
		return fmt.Errorf("invalid RTP download persistence identity")
	}
	terminal := !state.CompletedAt.IsZero() && (state.Status == rtpDownloadCompleted || state.Status == rtpDownloadStopped)
	payload, err := json.Marshal(rtpDownloadPersistentState{Key: key, Terminal: terminal, State: state})
	if err != nil {
		return fmt.Errorf("encode RTP download task state: %w", err)
	}
	persistCtx := g.taskPersistenceContext()
	unlock, err := g.lockTaskStateOperation(persistCtx, kind, state.DeviceID, sessionID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := g.saveTaskState(persistCtx, kind, state.DeviceID, sessionID, payload, state.UpdatedAt); err != nil {
		return err
	}
	session.mu.Lock()
	if session.stateRevision == revision {
		session.persistedRevision = revision
		session.persistencePending = false
	}
	session.mu.Unlock()
	return nil
}

func (g *GB28181API) restoreRTPDownloadStates(ctx context.Context) error {
	if g == nil || g.taskStateStorer() == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	types := []struct {
		kind  string
		limit int
	}{
		{kind: gbTaskKindRTPDownload, limit: rtpDownloadMaxChannelTerminalStates},
		{kind: gbTaskKindRTPDownloadSession, limit: rtpDownloadMaxSessionTerminalStates},
	}
	for _, item := range types {
		storeCtx, cancel := taskStateContext(ctx)
		err := g.taskStateStorer().CleanupGBTaskStates(storeCtx, item.kind, now.Add(-rtpDownloadTerminalTTL), item.limit)
		cancel()
		if err != nil {
			return fmt.Errorf("cleanup %s task states before restore: %w", item.kind, err)
		}
		records, err := g.listTaskStates(ctx, item.kind, item.limit)
		if err != nil {
			return err
		}
		for _, record := range records {
			unlock, err := g.lockTaskStateOperation(ctx, item.kind, record.DeviceID, record.SessionID)
			if err != nil {
				return fmt.Errorf("lock %s task state for restore: %w", item.kind, err)
			}
			payload, found, err := g.loadTaskState(ctx, item.kind, record.DeviceID, record.SessionID)
			if err != nil {
				unlock()
				return err
			}
			// ListGBTaskStates 返回无事务快照。若同键状态已在扫描期间更新，
			// 当前业务路径已经负责其内存态，旧快照不得再删除或恢复它。
			if !found || string(payload) != record.Payload {
				unlock()
				continue
			}
			var persisted rtpDownloadPersistentState
			if err := json.Unmarshal(payload, &persisted); err != nil {
				slog.Warn("remove invalid persisted RTP download state", "kind", record.Kind, "device_id", record.DeviceID, "session_id", record.SessionID)
				if err := g.deleteTaskState(ctx, item.kind, record.DeviceID, record.SessionID); err != nil {
					unlock()
					return fmt.Errorf("delete invalid persisted RTP download state: %w", err)
				}
				unlock()
				continue
			}
			if !persisted.Terminal {
				expectedKind, expectedSessionID, valid := rtpDownloadTaskIdentity(persisted.Key, persisted.State)
				if !valid || expectedKind != item.kind || expectedSessionID != record.SessionID || persisted.State.DeviceID != record.DeviceID {
					slog.Warn("remove invalid persisted RTP download marker", "kind", record.Kind, "device_id", record.DeviceID, "session_id", record.SessionID)
					if err := g.deleteTaskState(ctx, item.kind, record.DeviceID, record.SessionID); err != nil {
						unlock()
						return fmt.Errorf("delete invalid persisted RTP download marker: %w", err)
					}
					unlock()
					continue
				}
				if err := g.deleteTaskState(ctx, item.kind, record.DeviceID, record.SessionID); err != nil {
					unlock()
					return fmt.Errorf("delete interrupted RTP download marker: %w", err)
				}
				unlock()
				continue
			}
			if !validPersistedRTPDownloadState(persisted.Key, item.kind, record.DeviceID, record.SessionID, persisted.State, now) {
				slog.Warn("remove invalid persisted RTP download state", "kind", record.Kind, "device_id", record.DeviceID, "session_id", record.SessionID)
				if err := g.deleteTaskState(ctx, item.kind, record.DeviceID, record.SessionID); err != nil {
					unlock()
					return fmt.Errorf("delete invalid persisted RTP download state: %w", err)
				}
				unlock()
				continue
			}
			session := &rtpDownloadSession{
				state: persisted.State, key: persisted.Key,
				stateRevision: 1, persistedRevision: 1,
			}
			if current, ok := g.rtpDownloads.Load(persisted.Key); ok {
				currentSession, valid := current.(*rtpDownloadSession)
				if valid && currentSession != nil && !currentSession.snapshot().UpdatedAt.Before(persisted.State.UpdatedAt) {
					unlock()
					continue
				}
			}
			g.rtpDownloads.Store(persisted.Key, session)
			unlock()
		}
	}
	g.cleanupRTPDownloads(now)
	return nil
}

// allowRTPDownloadTerminalTransition 保证并发终态单调收敛：MediaStatus 可以确认远端 BYE，
// 但迟到的 BYE、用户停止或停服清理不能覆盖已经确认的媒体发送完成状态。
func allowRTPDownloadTerminalTransition(current RTPDownloadState, nextReason string) bool {
	if current.CompletedAt.IsZero() {
		return true
	}
	return nextReason == "media_status" && (current.EndReason == "remote_bye" || current.EndReason == "media_status")
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
	pendingPersistence := make([]*rtpDownloadSession, 0)
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
		session.mu.RLock()
		pending := session.persistencePending
		session.mu.RUnlock()
		if pending {
			pendingPersistence = append(pendingPersistence, session)
		}
		entry := rtpDownloadTerminalEntry{key: keyText, session: session, completed: state.CompletedAt}
		if keyText == historyKey(historyModeDownload, state.DeviceID, state.ChannelID) {
			channelTerminals = append(channelTerminals, entry)
		} else {
			sessionTerminals = append(sessionTerminals, entry)
		}
		return true
	})
	for _, session := range pendingPersistence {
		if err := g.persistRTPDownloadState(session); err != nil && !g.serviceStopped() {
			state := session.snapshot()
			slog.Warn("retry persisted RTP download terminal state failed", "device_id", state.DeviceID, "channel_id", state.ChannelID, "session_id", state.SessionID, "err", err)
		}
	}
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
	g.retryPendingDirectTCPDownloadStates()
	if g.directDownloads != nil {
		g.directDownloads.Cleanup(now)
	}
	g.cleanupQueryStates(now)
	g.cleanupUpgradeStates(now)
	g.cleanupSnapshotStates(now)
	g.retryPendingCompletedCascadeTaskRoutes()
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
	g.runRuntimeStateCleaner(time.Hour, voiceCleanupRetryInterval)
}

func (g *GB28181API) runRuntimeStateCleaner(stateInterval, voiceInterval time.Duration) {
	if stateInterval <= 0 {
		stateInterval = time.Hour
	}
	if voiceInterval <= 0 {
		voiceInterval = voiceCleanupRetryInterval
	}
	g.cleanupRuntimeStates(time.Now())
	g.cleanupStoppedVoiceSessions()
	g.cleanupStoppedMediaSessions()
	g.cleanupStoppedCascadeMediaSessions()
	g.cleanupPendingInboundDialogCleanups()
	stateTicker := time.NewTicker(stateInterval)
	voiceTicker := time.NewTicker(voiceInterval)
	defer stateTicker.Stop()
	defer voiceTicker.Stop()
	for {
		select {
		case <-g.lifecycleDone:
			return
		case now := <-stateTicker.C:
			g.cleanupRuntimeStates(now)
		case <-voiceTicker.C:
			g.retryPendingCompletedCascadeTaskRoutes()
			g.cleanupStoppedVoiceSessions()
			g.cleanupStoppedMediaSessions()
			g.cleanupStoppedCascadeMediaSessions()
			g.cleanupPendingInboundDialogCleanups()
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
