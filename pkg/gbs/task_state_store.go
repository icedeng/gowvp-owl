package gbs

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const (
	gbTaskKindUpgrade  = "upgrade"
	gbTaskKindSnapshot = "snapshot"
	gbTaskStoreTimeout = 3 * time.Second
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
	store := g.taskStateStorer()
	if store == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
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
}
