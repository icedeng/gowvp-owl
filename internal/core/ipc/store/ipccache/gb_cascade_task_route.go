package ipccache

import (
	"context"
	"errors"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/ixugo/goddd/pkg/orm"
)

func (c *Cache) GBCascadeTaskRouteAvailable() bool {
	if c == nil || c.Storer == nil {
		return false
	}
	provider, ok := c.Storer.(ipc.GBCascadeTaskRouteStoreProvider)
	return ok && provider.GBCascadeTaskRoute() != nil
}

func (c *Cache) gbCascadeTaskRouteStore() (*ipc.GBCascadeTaskRouteStore, error) {
	if c == nil || c.Storer == nil {
		return nil, errors.New("GB cascade task route store is unavailable")
	}
	provider, ok := c.Storer.(ipc.GBCascadeTaskRouteStoreProvider)
	if !ok {
		return nil, errors.New("GB cascade task route store is unavailable")
	}
	store := provider.GBCascadeTaskRoute()
	if store == nil {
		return nil, errors.New("GB cascade task route store is unavailable")
	}
	return store, nil
}

func (c *Cache) SaveGBCascadeTaskRoute(
	ctx context.Context,
	kind, platformName, downstreamDeviceID, downstreamSessionID, exposedID, upstreamSessionID string,
	payload []byte,
	updatedAt time.Time,
) error {
	store, err := c.gbCascadeTaskRouteStore()
	if err != nil {
		return err
	}
	return store.Save(ctx, &ipc.GBCascadeTaskRouteRecord{
		Kind: kind, PlatformName: platformName,
		DownstreamDeviceID: downstreamDeviceID, DownstreamSessionID: downstreamSessionID,
		ExposedID: exposedID, UpstreamSessionID: upstreamSessionID, Payload: string(payload),
		UpdatedAt: orm.Time{Time: updatedAt},
	})
}

func (c *Cache) LoadGBCascadeTaskRouteByDownstream(ctx context.Context, kind, deviceID, sessionID string) ([]byte, bool, error) {
	store, err := c.gbCascadeTaskRouteStore()
	if err != nil {
		return nil, false, err
	}
	record, ok, err := store.LoadDownstream(ctx, kind, deviceID, sessionID)
	if err != nil || !ok {
		return nil, ok, err
	}
	return []byte(record.Payload), true, nil
}

func (c *Cache) LoadGBCascadeTaskRouteByUpstream(ctx context.Context, kind, platformName, exposedID, sessionID string) ([]byte, bool, error) {
	store, err := c.gbCascadeTaskRouteStore()
	if err != nil {
		return nil, false, err
	}
	record, ok, err := store.LoadUpstream(ctx, kind, platformName, exposedID, sessionID)
	if err != nil || !ok {
		return nil, ok, err
	}
	return []byte(record.Payload), true, nil
}

func (c *Cache) DeleteGBCascadeTaskRoute(ctx context.Context, kind, deviceID, sessionID string) error {
	store, err := c.gbCascadeTaskRouteStore()
	if err != nil {
		return err
	}
	return store.Delete(ctx, kind, deviceID, sessionID)
}

func (c *Cache) DeleteGBCascadeTaskRouteByUpstream(ctx context.Context, kind, platformName, exposedID, sessionID string) error {
	store, err := c.gbCascadeTaskRouteStore()
	if err != nil {
		return err
	}
	return store.DeleteUpstream(ctx, kind, platformName, exposedID, sessionID)
}

func (c *Cache) CleanupGBCascadeTaskRoutes(ctx context.Context, cutoff time.Time, limit int) error {
	store, err := c.gbCascadeTaskRouteStore()
	if err != nil {
		return err
	}
	return store.Cleanup(ctx, cutoff, limit)
}
