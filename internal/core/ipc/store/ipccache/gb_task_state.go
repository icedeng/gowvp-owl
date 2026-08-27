package ipccache

import (
	"context"
	"errors"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/ixugo/goddd/pkg/orm"
)

func (c *Cache) GBTaskStateAvailable() bool {
	if c == nil || c.Storer == nil {
		return false
	}
	provider, ok := c.Storer.(ipc.GBTaskStateStoreProvider)
	return ok && provider.GBTaskState() != nil
}

func (c *Cache) gbTaskStateStore() (*ipc.GBTaskStateStore, error) {
	if c == nil || c.Storer == nil {
		return nil, errors.New("GB task state store is unavailable")
	}
	provider, ok := c.Storer.(ipc.GBTaskStateStoreProvider)
	if !ok {
		return nil, errors.New("GB task state store is unavailable")
	}
	store := provider.GBTaskState()
	if store == nil {
		return nil, errors.New("GB task state store is unavailable")
	}
	return store, nil
}

func (c *Cache) SaveGBTaskState(ctx context.Context, kind, deviceID, sessionID string, payload []byte, updatedAt time.Time) error {
	store, err := c.gbTaskStateStore()
	if err != nil {
		return err
	}
	return store.Save(ctx, &ipc.GBTaskStateRecord{
		Kind: kind, DeviceID: deviceID, SessionID: sessionID, Payload: string(payload),
		UpdatedAt: orm.Time{Time: updatedAt},
	})
}

func (c *Cache) LoadGBTaskState(ctx context.Context, kind, deviceID, sessionID string) ([]byte, bool, error) {
	store, err := c.gbTaskStateStore()
	if err != nil {
		return nil, false, err
	}
	record, ok, err := store.Load(ctx, kind, deviceID, sessionID)
	if err != nil || !ok {
		return nil, ok, err
	}
	return []byte(record.Payload), true, nil
}

func (c *Cache) DeleteGBTaskState(ctx context.Context, kind, deviceID, sessionID string) error {
	store, err := c.gbTaskStateStore()
	if err != nil {
		return err
	}
	return store.Delete(ctx, kind, deviceID, sessionID)
}

func (c *Cache) CleanupGBTaskStates(ctx context.Context, kind string, cutoff time.Time, limit int) error {
	store, err := c.gbTaskStateStore()
	if err != nil {
		return err
	}
	return store.Cleanup(ctx, kind, cutoff, limit)
}
