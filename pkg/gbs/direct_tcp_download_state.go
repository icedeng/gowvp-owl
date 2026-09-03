package gbs

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type directTCPDownloadPersistentState struct {
	Terminal bool                   `json:"terminal"`
	State    DirectTCPDownloadState `json:"state"`
}

// directTCPDownloadPersistenceSlot 串行化同一通道的活动标记与终态写入。
// 新标记必须在旧终态之后落盘，避免慢写把重启边界回退到上一次下载。
type directTCPDownloadPersistenceSlot struct {
	mu                 sync.RWMutex
	persistMu          sync.Mutex
	state              DirectTCPDownloadState
	terminal           bool
	revision           uint64
	persistencePending bool
}

func directTCPDownloadPersistenceKey(deviceID, channelID string) string {
	return strings.TrimSpace(deviceID) + "\x00" + strings.TrimSpace(channelID)
}

func (g *GB28181API) directTCPDownloadRetention() time.Duration {
	if g != nil && g.directDownloads != nil {
		return g.directDownloads.terminalRetention()
	}
	return 7 * 24 * time.Hour
}

func (g *GB28181API) updateDirectTCPDownloadPersistenceSlot(state DirectTCPDownloadState, terminal bool) (string, *directTCPDownloadPersistenceSlot, uint64) {
	key := directTCPDownloadPersistenceKey(state.DeviceID, state.ChannelID)
	g.directDownloadPersistenceMu.Lock()
	if g.directDownloadPersistence == nil {
		g.directDownloadPersistence = make(map[string]*directTCPDownloadPersistenceSlot)
	}
	slot := g.directDownloadPersistence[key]
	if slot == nil {
		slot = &directTCPDownloadPersistenceSlot{}
		g.directDownloadPersistence[key] = slot
	}
	// 与 map 更新保持同一锁序，release 不能在调用方取得 slot 后、提交新 revision 前删除它。
	slot.mu.Lock()
	slot.state = state
	slot.terminal = terminal
	slot.revision++
	revision := slot.revision
	slot.persistencePending = true
	slot.mu.Unlock()
	g.directDownloadPersistenceMu.Unlock()
	return key, slot, revision
}

func (g *GB28181API) releaseDirectTCPDownloadPersistenceSlot(key string, slot *directTCPDownloadPersistenceSlot) {
	if g == nil || slot == nil {
		return
	}
	g.directDownloadPersistenceMu.Lock()
	slot.mu.RLock()
	if g.directDownloadPersistence[key] == slot && !slot.persistencePending {
		delete(g.directDownloadPersistence, key)
	}
	slot.mu.RUnlock()
	g.directDownloadPersistenceMu.Unlock()
}

func (g *GB28181API) persistDirectTCPDownloadState(state DirectTCPDownloadState, terminal bool) error {
	if g == nil {
		return nil
	}
	state.SessionID = strings.TrimSpace(state.SessionID)
	state.DeviceID = strings.TrimSpace(state.DeviceID)
	state.ChannelID = strings.TrimSpace(state.ChannelID)
	if state.SessionID == "" || state.DeviceID == "" || state.ChannelID == "" {
		return fmt.Errorf("invalid direct TCP download persistence identity")
	}
	if terminal != isDirectTCPTerminalStatus(state.Status) {
		return fmt.Errorf("invalid direct TCP download persistence status: %s", state.Status)
	}
	if !terminal && state.Status != directTCPStatusConnecting && state.Status != directTCPStatusReceiving {
		return fmt.Errorf("invalid direct TCP download active status: %s", state.Status)
	}
	key, slot, revision := g.updateDirectTCPDownloadPersistenceSlot(state, terminal)
	if err := g.persistDirectTCPDownloadPersistenceSlot(key, slot); err != nil {
		// 活动标记失败会拒绝启动；它不应作为后台终态重试项留下。
		if !terminal {
			slot.mu.Lock()
			if slot.revision == revision {
				slot.persistencePending = false
			}
			slot.mu.Unlock()
			g.releaseDirectTCPDownloadPersistenceSlot(key, slot)
		}
		return err
	}
	return nil
}

func (g *GB28181API) persistDirectTCPDownloadPersistenceSlot(key string, slot *directTCPDownloadPersistenceSlot) error {
	if g == nil || slot == nil {
		return nil
	}
	slot.persistMu.Lock()
	defer slot.persistMu.Unlock()

	slot.mu.RLock()
	if !slot.persistencePending {
		slot.mu.RUnlock()
		g.releaseDirectTCPDownloadPersistenceSlot(key, slot)
		return nil
	}
	state := slot.state
	terminal := slot.terminal
	revision := slot.revision
	slot.mu.RUnlock()

	payload, err := json.Marshal(directTCPDownloadPersistentState{Terminal: terminal, State: state})
	if err != nil {
		return fmt.Errorf("encode direct TCP download task state: %w", err)
	}
	persistCtx := g.taskPersistenceContext()
	unlock, err := g.lockTaskStateOperation(persistCtx, gbTaskKindDirectTCPDownload, state.DeviceID, state.ChannelID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := g.saveTaskState(persistCtx, gbTaskKindDirectTCPDownload, state.DeviceID, state.ChannelID, payload, state.UpdatedAt); err != nil {
		return err
	}
	slot.mu.Lock()
	if slot.revision == revision {
		slot.persistencePending = false
	}
	slot.mu.Unlock()
	g.releaseDirectTCPDownloadPersistenceSlot(key, slot)
	return nil
}

func (g *GB28181API) retryPendingDirectTCPDownloadStates() {
	if g == nil {
		return
	}
	g.directDownloadPersistenceMu.Lock()
	slots := make(map[string]*directTCPDownloadPersistenceSlot, len(g.directDownloadPersistence))
	for key, slot := range g.directDownloadPersistence {
		slots[key] = slot
	}
	g.directDownloadPersistenceMu.Unlock()
	for key, slot := range slots {
		if err := g.persistDirectTCPDownloadPersistenceSlot(key, slot); err != nil && !g.serviceStopped() {
			slot.mu.RLock()
			state := slot.state
			slot.mu.RUnlock()
			slog.Warn("retry persisted direct TCP download terminal state failed", "device_id", state.DeviceID, "channel_id", state.ChannelID, "session_id", state.SessionID, "err", err)
		}
	}
}

func (g *GB28181API) deleteDirectTCPDownloadMarker(state DirectTCPDownloadState) error {
	if g == nil || strings.TrimSpace(state.DeviceID) == "" || strings.TrimSpace(state.ChannelID) == "" {
		return nil
	}
	persistCtx := g.taskPersistenceContext()
	unlock, err := g.lockTaskStateOperation(persistCtx, gbTaskKindDirectTCPDownload, state.DeviceID, state.ChannelID)
	if err != nil {
		return err
	}
	defer unlock()
	return g.deleteTaskState(persistCtx, gbTaskKindDirectTCPDownload, state.DeviceID, state.ChannelID)
}

func validPersistedDirectTCPDownloadIdentity(recordDeviceID, recordSessionID string, state DirectTCPDownloadState) bool {
	return state.DeviceID != "" && state.ChannelID != "" && state.SessionID != "" &&
		state.DeviceID == recordDeviceID && state.ChannelID == recordSessionID
}

func validPersistedDirectTCPDownloadState(recordDeviceID, recordSessionID string, state DirectTCPDownloadState, now time.Time, retention time.Duration) bool {
	if !validPersistedDirectTCPDownloadIdentity(recordDeviceID, recordSessionID, state) || !isDirectTCPTerminalStatus(state.Status) {
		return false
	}
	if state.StartedAt.IsZero() || state.UpdatedAt.IsZero() || state.CompletedAt.IsZero() || state.EndReason == "" ||
		state.UpdatedAt.Before(state.StartedAt) || state.CompletedAt.Before(state.StartedAt) || state.UpdatedAt.After(now.Add(5*time.Minute)) {
		return false
	}
	if state.Received < 0 || state.FileSize < 0 || state.FileSizeKnown && state.Received > state.FileSize {
		return false
	}
	if runtimeStateExpired(state.CompletedAt, now, retention) || state.CompletedAt.After(now.Add(5*time.Minute)) {
		return false
	}
	if state.Status == directTCPStatusCompleted {
		if state.Output == "" || filepath.IsAbs(state.Output) || filepath.Base(state.Output) != state.Output {
			return false
		}
		digest, err := hex.DecodeString(state.SHA256)
		if err != nil || len(digest) != 32 {
			return false
		}
	} else if state.Output != "" || state.SHA256 != "" {
		return false
	}
	return true
}

func (g *GB28181API) validPersistedDirectTCPDownloadOutput(state DirectTCPDownloadState) (bool, error) {
	if state.Status != directTCPStatusCompleted {
		return true, nil
	}
	if g == nil || g.directDownloads == nil {
		return false, nil
	}
	g.directDownloads.mu.RLock()
	storageDir := g.directDownloads.opts.StorageDir
	g.directDownloads.mu.RUnlock()
	root, err := filepath.Abs(storageDir)
	if err != nil {
		return false, fmt.Errorf("resolve direct TCP download storage: %w", err)
	}
	path := filepath.Join(root, state.Output)
	if !pathWithinRoot(root, path) {
		return false, nil
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect direct TCP download output: %w", err)
	}
	return info.Mode().IsRegular() && info.Size() == state.Received, nil
}

func (g *GB28181API) restoreDirectTCPDownloadStates(ctx context.Context) error {
	if g == nil || g.taskStateStorer() == nil || g.directDownloads == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	retention := g.directTCPDownloadRetention()
	storeCtx, cancel := taskStateContext(ctx)
	err := g.taskStateStorer().CleanupGBTaskStates(storeCtx, gbTaskKindDirectTCPDownload, now.Add(-retention), directTCPMaxTerminalStates)
	cancel()
	if err != nil {
		return fmt.Errorf("cleanup direct TCP download task states before restore: %w", err)
	}
	records, err := g.listTaskStates(ctx, gbTaskKindDirectTCPDownload, directTCPMaxTerminalStates)
	if err != nil {
		return err
	}
	for _, record := range records {
		unlock, err := g.lockTaskStateOperation(ctx, gbTaskKindDirectTCPDownload, record.DeviceID, record.SessionID)
		if err != nil {
			return fmt.Errorf("lock direct TCP download task state for restore: %w", err)
		}
		payload, found, err := g.loadTaskState(ctx, gbTaskKindDirectTCPDownload, record.DeviceID, record.SessionID)
		if err != nil {
			unlock()
			return err
		}
		// 列表是无事务快照；扫描期间已经更新的同键记录属于当前业务会话，
		// 不能再按旧快照清理或恢复。
		if !found || string(payload) != record.Payload {
			unlock()
			continue
		}
		var persisted directTCPDownloadPersistentState
		if err := json.Unmarshal(payload, &persisted); err != nil {
			slog.Warn("remove invalid persisted direct TCP download state", "device_id", record.DeviceID, "channel_id", record.SessionID)
			if err := g.deleteTaskState(ctx, gbTaskKindDirectTCPDownload, record.DeviceID, record.SessionID); err != nil {
				unlock()
				return fmt.Errorf("delete invalid persisted direct TCP download state: %w", err)
			}
			unlock()
			continue
		}
		if !persisted.Terminal {
			if !validPersistedDirectTCPDownloadIdentity(record.DeviceID, record.SessionID, persisted.State) {
				slog.Warn("remove invalid persisted direct TCP download marker", "device_id", record.DeviceID, "channel_id", record.SessionID)
				if err := g.deleteTaskState(ctx, gbTaskKindDirectTCPDownload, record.DeviceID, record.SessionID); err != nil {
					unlock()
					return fmt.Errorf("delete invalid persisted direct TCP download marker: %w", err)
				}
				unlock()
				continue
			}
			if err := g.deleteTaskState(ctx, gbTaskKindDirectTCPDownload, record.DeviceID, record.SessionID); err != nil {
				unlock()
				return fmt.Errorf("delete interrupted direct TCP download marker: %w", err)
			}
			unlock()
			continue
		}
		if !validPersistedDirectTCPDownloadState(record.DeviceID, record.SessionID, persisted.State, now, retention) {
			slog.Warn("remove invalid persisted direct TCP download state", "device_id", record.DeviceID, "channel_id", record.SessionID)
			if err := g.deleteTaskState(ctx, gbTaskKindDirectTCPDownload, record.DeviceID, record.SessionID); err != nil {
				unlock()
				return fmt.Errorf("delete invalid persisted direct TCP download state: %w", err)
			}
			unlock()
			continue
		}
		outputValid, err := g.validPersistedDirectTCPDownloadOutput(persisted.State)
		if err != nil {
			unlock()
			return err
		}
		if !outputValid {
			slog.Warn("remove persisted direct TCP download state without a valid output file", "device_id", record.DeviceID, "channel_id", record.SessionID)
			if err := g.deleteTaskState(ctx, gbTaskKindDirectTCPDownload, record.DeviceID, record.SessionID); err != nil {
				unlock()
				return fmt.Errorf("delete persisted direct TCP download state without output: %w", err)
			}
			unlock()
			continue
		}
		g.directDownloads.restoreTerminalState(persisted.State)
		unlock()
	}
	g.directDownloads.Cleanup(now)
	return nil
}
