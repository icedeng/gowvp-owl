package gbs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
)

const (
	gbTaskKindUpgrade            = "upgrade"
	gbTaskKindSnapshot           = "snapshot"
	gbTaskKindRTPDownload        = "rtp_download"
	gbTaskKindRTPDownloadSession = "rtp_download_session"
	gbTaskKindDirectTCPDownload  = "direct_tcp_download"
	gbTaskStoreTimeout           = 3 * time.Second
)

// gbTaskStateStorer 是长周期国标任务的可选持久化能力。
// MemoryStorer 的既有实现无需实现该接口；生产缓存层实现后可跨进程重启恢复会话关联。
type gbTaskStateStorer interface {
	SaveGBTaskState(context.Context, string, string, string, []byte, time.Time) error
	LoadGBTaskState(context.Context, string, string, string) ([]byte, bool, error)
	DeleteGBTaskState(context.Context, string, string, string) error
	CleanupGBTaskStates(context.Context, string, time.Time, int) error
}

type gbTaskStateAvailability interface {
	GBTaskStateAvailable() bool
}

type gbTaskStateLister interface {
	ListGBTaskStates(context.Context, string, int) ([]ipc.GBTaskStateRecord, error)
}

func taskStateOperationKey(kind, deviceID, sessionID string) string {
	return kind + "\x00" + deviceID + "\x00" + sessionID
}

func (g *GB28181API) lockTaskStateOperation(ctx context.Context, kind, deviceID, sessionID string) (func(), error) {
	if g == nil {
		return nil, fmt.Errorf("GB28181 service is unavailable")
	}
	key := taskStateOperationKey(kind, deviceID, sessionID)
	g.taskStateOperationMu.Lock()
	if g.taskStateOperations == nil {
		g.taskStateOperations = make(map[string]*keyedOperationLock)
	}
	entry := g.taskStateOperations[key]
	if entry == nil {
		entry = &keyedOperationLock{}
		g.taskStateOperations[key] = entry
	}
	entry.refs++
	g.taskStateOperationMu.Unlock()
	if err := entry.mutex.LockContext(ctx); err != nil {
		g.releaseTaskStateOperation(key, entry)
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mutex.Unlock()
			g.releaseTaskStateOperation(key, entry)
		})
	}, nil
}

func (g *GB28181API) releaseTaskStateOperation(key string, entry *keyedOperationLock) {
	g.taskStateOperationMu.Lock()
	if current := g.taskStateOperations[key]; current == entry {
		entry.refs--
		if entry.refs == 0 {
			delete(g.taskStateOperations, key)
		}
	}
	g.taskStateOperationMu.Unlock()
}

func (g *GB28181API) taskStateStorer() gbTaskStateStorer {
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return nil
	}
	if availability, ok := g.svr.memoryStorer.(gbTaskStateAvailability); ok && !availability.GBTaskStateAvailable() {
		return nil
	}
	store, _ := g.svr.memoryStorer.(gbTaskStateStorer)
	return store
}

func taskStateContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, gbTaskStoreTimeout)
}

func (g *GB28181API) saveTaskState(ctx context.Context, kind, deviceID, sessionID string, payload []byte, updatedAt time.Time) error {
	store := g.taskStateStorer()
	if store == nil {
		return nil
	}
	storeCtx, cancel := taskStateContext(ctx)
	defer cancel()
	if err := store.SaveGBTaskState(storeCtx, kind, deviceID, sessionID, payload, updatedAt); err != nil {
		return fmt.Errorf("save %s task state: %w", kind, err)
	}
	return nil
}

func (g *GB28181API) loadTaskState(ctx context.Context, kind, deviceID, sessionID string) ([]byte, bool, error) {
	store := g.taskStateStorer()
	if store == nil {
		return nil, false, nil
	}
	storeCtx, cancel := taskStateContext(ctx)
	defer cancel()
	payload, ok, err := store.LoadGBTaskState(storeCtx, kind, deviceID, sessionID)
	if err != nil {
		return nil, false, fmt.Errorf("load %s task state: %w", kind, err)
	}
	return payload, ok, nil
}

func (g *GB28181API) listTaskStates(ctx context.Context, kind string, limit int) ([]ipc.GBTaskStateRecord, error) {
	store := g.taskStateStorer()
	if store == nil {
		return nil, nil
	}
	lister, ok := store.(gbTaskStateLister)
	if !ok {
		return nil, nil
	}
	storeCtx, cancel := taskStateContext(ctx)
	defer cancel()
	records, err := lister.ListGBTaskStates(storeCtx, kind, limit)
	if err != nil {
		return nil, fmt.Errorf("list %s task states: %w", kind, err)
	}
	return records, nil
}

func (g *GB28181API) deleteTaskState(ctx context.Context, kind, deviceID, sessionID string) error {
	store := g.taskStateStorer()
	if store == nil {
		return nil
	}
	storeCtx, cancel := taskStateContext(ctx)
	defer cancel()
	if err := store.DeleteGBTaskState(storeCtx, kind, deviceID, sessionID); err != nil {
		return fmt.Errorf("delete %s task state: %w", kind, err)
	}
	return nil
}

func (g *GB28181API) cleanupTaskStates(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	g.cleanupPersistedCascadeTaskRoutes(now)
	g.cleanupVideoUploadReceipts(now)
	store := g.taskStateStorer()
	if store == nil {
		return
	}
	ctx, cancel := taskStateContext(context.Background())
	err := store.CleanupGBTaskStates(ctx, gbTaskKindUpgrade, now.Add(-upgradeStateTTL), maxUpgradeStates)
	cancel()
	if err != nil {
		slog.Error("cleanup upgrade task states", "err", err)
	}
	ctx, cancel = taskStateContext(context.Background())
	err = store.CleanupGBTaskStates(ctx, gbTaskKindSnapshot, now.Add(-snapshotStateTTL), maxSnapshotStates)
	cancel()
	if err != nil {
		slog.Error("cleanup snapshot task states", "err", err)
	}
	ctx, cancel = taskStateContext(context.Background())
	err = store.CleanupGBTaskStates(ctx, gbTaskKindRTPDownload, now.Add(-rtpDownloadTerminalTTL), rtpDownloadMaxChannelTerminalStates)
	cancel()
	if err != nil {
		slog.Error("cleanup RTP download task states", "err", err)
	}
	ctx, cancel = taskStateContext(context.Background())
	err = store.CleanupGBTaskStates(ctx, gbTaskKindRTPDownloadSession, now.Add(-rtpDownloadTerminalTTL), rtpDownloadMaxSessionTerminalStates)
	cancel()
	if err != nil {
		slog.Error("cleanup RTP download session task states", "err", err)
	}
	ctx, cancel = taskStateContext(context.Background())
	err = store.CleanupGBTaskStates(ctx, gbTaskKindDirectTCPDownload, now.Add(-g.directTCPDownloadRetention()), directTCPMaxTerminalStates)
	cancel()
	if err != nil {
		slog.Error("cleanup direct TCP download task states", "err", err)
	}
	ctx, cancel = taskStateContext(context.Background())
	err = store.CleanupGBTaskStates(ctx, gbTaskKindAlarmInbox, now.Add(-alarmInboxRetention), alarmInboxMaxStates)
	cancel()
	if err != nil {
		slog.Error("cleanup Alarm inbox states", "err", err)
	}
	ctx, cancel = taskStateContext(context.Background())
	err = store.CleanupGBTaskStates(ctx, gbTaskKindAlarmReceipt, now.Add(-alarmInboxRetention), alarmInboxMaxStates)
	cancel()
	if err != nil {
		slog.Error("cleanup Alarm receipt states", "err", err)
	}
	ctx, cancel = taskStateContext(context.Background())
	err = store.CleanupGBTaskStates(ctx, gbTaskKindAlarmDeadLetter, now.Add(-alarmInboxRetention), alarmInboxMaxStates)
	cancel()
	if err != nil {
		slog.Error("cleanup Alarm dead letter states", "err", err)
	}
	ctx, cancel = taskStateContext(context.Background())
	err = store.CleanupGBTaskStates(ctx, gbTaskKindVideoUploadOutbox, now.Add(-videoUploadOutboxRetention), maxVideoUploadOutboxStates)
	cancel()
	if err != nil {
		slog.Error("cleanup VideoUploadNotify outbox states", "err", err)
	}
	ctx, cancel = taskStateContext(context.Background())
	err = store.CleanupGBTaskStates(ctx, gbTaskKindVideoUploadReceipt, now.Add(-videoUploadReceiptRetention), maxVideoUploadReceipts)
	cancel()
	if err != nil {
		slog.Error("cleanup VideoUploadNotify receipt states", "err", err)
	}
}
