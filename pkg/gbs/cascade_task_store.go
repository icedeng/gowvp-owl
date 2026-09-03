package gbs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const cascadeTaskRouteSchemaVersion = 1

type gbCascadeTaskRouteStorer interface {
	SaveGBCascadeTaskRoute(context.Context, string, string, string, string, string, string, []byte, time.Time) error
	LoadGBCascadeTaskRouteByDownstream(context.Context, string, string, string) ([]byte, bool, error)
	LoadGBCascadeTaskRouteByUpstream(context.Context, string, string, string, string) ([]byte, bool, error)
	DeleteGBCascadeTaskRoute(context.Context, string, string, string) error
	CleanupGBCascadeTaskRoutes(context.Context, time.Time, int) error
}

type gbCascadeTaskRouteUpstreamDeleter interface {
	DeleteGBCascadeTaskRouteByUpstream(context.Context, string, string, string, string) error
}

type gbCascadeTaskRouteAvailability interface {
	GBCascadeTaskRouteAvailable() bool
}

var errUnsupportedCascadeTaskRouteSchema = errors.New("unsupported persisted cascade task route schema")

type cascadeTaskRoutePersistentState struct {
	SchemaVersion       int                  `json:"schema_version"`
	Kind                string               `json:"kind"`
	PlatformName        string               `json:"platform_name"`
	DownstreamDeviceID  string               `json:"downstream_device_id"`
	DownstreamTargetID  string               `json:"downstream_target_id"`
	ExposedID           string               `json:"exposed_id"`
	UpstreamSessionID   string               `json:"upstream_session_id"`
	DownstreamSessionID string               `json:"downstream_session_id"`
	RequestFingerprint  string               `json:"request_fingerprint"`
	Identity            *monitorUserIdentity `json:"identity,omitempty"`
	LocalGatewayID      string               `json:"local_gateway_id,omitempty"`
	CreatedAt           time.Time            `json:"created_at"`
	UpdatedAt           time.Time            `json:"updated_at"`
	StartFinished       bool                 `json:"start_finished"`
	StartResult         string               `json:"start_result,omitempty"`
	StartError          string               `json:"start_error,omitempty"`
	Completed           bool                 `json:"completed"`
}

func (g *GB28181API) cascadeTaskRouteStorer() gbCascadeTaskRouteStorer {
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return nil
	}
	if availability, ok := g.svr.memoryStorer.(gbCascadeTaskRouteAvailability); ok && !availability.GBCascadeTaskRouteAvailable() {
		return nil
	}
	store, _ := g.svr.memoryStorer.(gbCascadeTaskRouteStorer)
	return store
}

func (route *cascadeTaskRoute) persistentState() cascadeTaskRoutePersistentState {
	if route == nil {
		return cascadeTaskRoutePersistentState{}
	}
	route.notifyMu.Lock()
	defer route.notifyMu.Unlock()
	return route.persistentStateLocked()
}

func (route *cascadeTaskRoute) persistentStateLocked() cascadeTaskRoutePersistentState {
	updatedAt := route.lastUpdatedAt()
	state := cascadeTaskRoutePersistentState{
		SchemaVersion: cascadeTaskRouteSchemaVersion,
		Kind:          route.kind, DownstreamDeviceID: route.downstreamDeviceID,
		DownstreamTargetID: route.downstreamTargetID, ExposedID: route.exposedID,
		UpstreamSessionID: route.upstreamSessionID, DownstreamSessionID: route.downstreamSessionID,
		RequestFingerprint: route.requestFingerprint, Identity: route.identity.clone(),
		LocalGatewayID: route.localGatewayID, CreatedAt: route.createdAt, UpdatedAt: updatedAt,
		StartResult: route.startResult, Completed: route.completed,
	}
	state.PlatformName = route.upstreamPlatform
	if route.worker != nil {
		if state.PlatformName == "" {
			state.PlatformName = route.worker.platform.name
		}
	}
	select {
	case <-route.startDone:
		state.StartFinished = true
	default:
	}
	if route.startErr != nil {
		state.StartError = route.startErr.Error()
	}
	return state
}

func (g *GB28181API) persistCascadeTaskRoute(ctx context.Context, route *cascadeTaskRoute) error {
	if route == nil {
		return nil
	}
	// 与最终通知持有同一把锁直到数据库写入结束，防止较早的 pending
	// 快照在 completed 墓碑之后迟到落库并把终态回退。
	route.notifyMu.Lock()
	defer route.notifyMu.Unlock()
	state := route.persistentStateLocked()
	state.UpdatedAt = time.Now()
	if err := g.persistCascadeTaskRouteState(ctx, state); err != nil {
		return err
	}
	route.setUpdatedAt(state.UpdatedAt)
	return nil
}

func (g *GB28181API) persistCascadeTaskRouteState(ctx context.Context, state cascadeTaskRoutePersistentState) error {
	store := g.cascadeTaskRouteStorer()
	if store == nil {
		return nil
	}
	if err := validateCascadeTaskRoutePersistentState(state); err != nil {
		return err
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode cascade task route: %w", err)
	}
	storeCtx, cancel := taskStateContext(ctx)
	defer cancel()
	if err := store.SaveGBCascadeTaskRoute(
		storeCtx, state.Kind, state.PlatformName, state.DownstreamDeviceID, state.DownstreamSessionID,
		state.ExposedID, state.UpstreamSessionID, payload, state.UpdatedAt,
	); err != nil {
		return fmt.Errorf("save cascade task route: %w", err)
	}
	return nil
}

func (g *GB28181API) loadCascadeTaskRouteStateByDownstream(ctx context.Context, kind, deviceID, sessionID string) (cascadeTaskRoutePersistentState, bool, error) {
	store := g.cascadeTaskRouteStorer()
	if store == nil {
		return cascadeTaskRoutePersistentState{}, false, nil
	}
	storeCtx, cancel := taskStateContext(ctx)
	defer cancel()
	deviceID = strings.TrimSpace(deviceID)
	sessionID = strings.TrimSpace(sessionID)
	payload, ok, err := store.LoadGBCascadeTaskRouteByDownstream(storeCtx, kind, deviceID, sessionID)
	state, found, decodeErr := decodeCascadeTaskRouteState(payload, ok, err)
	if decodeErr == nil || !found || errors.Is(decodeErr, errUnsupportedCascadeTaskRouteSchema) {
		return state, found, decodeErr
	}
	if deleteErr := store.DeleteGBCascadeTaskRoute(storeCtx, kind, deviceID, sessionID); deleteErr != nil {
		return cascadeTaskRoutePersistentState{}, true, errors.Join(decodeErr, fmt.Errorf("delete invalid cascade task route: %w", deleteErr))
	}
	slog.Warn("removed invalid persisted cascade task route", "direction", "downstream", "kind", kind, "device_id", deviceID, "session_id", sessionID, "err", decodeErr)
	return cascadeTaskRoutePersistentState{}, false, nil
}

func (g *GB28181API) loadCascadeTaskRouteStateByUpstream(ctx context.Context, kind, platformName, exposedID, sessionID string) (cascadeTaskRoutePersistentState, bool, error) {
	store := g.cascadeTaskRouteStorer()
	if store == nil {
		return cascadeTaskRoutePersistentState{}, false, nil
	}
	storeCtx, cancel := taskStateContext(ctx)
	defer cancel()
	platformName = strings.TrimSpace(platformName)
	exposedID = strings.TrimSpace(exposedID)
	sessionID = strings.TrimSpace(sessionID)
	payload, ok, err := store.LoadGBCascadeTaskRouteByUpstream(
		storeCtx, kind, platformName, exposedID, sessionID,
	)
	state, found, decodeErr := decodeCascadeTaskRouteState(payload, ok, err)
	if decodeErr == nil || !found || errors.Is(decodeErr, errUnsupportedCascadeTaskRouteSchema) {
		return state, found, decodeErr
	}
	deleter, ok := store.(gbCascadeTaskRouteUpstreamDeleter)
	if !ok {
		return cascadeTaskRoutePersistentState{}, true, fmt.Errorf("delete cascade task route by upstream identity is unavailable")
	}
	if deleteErr := deleter.DeleteGBCascadeTaskRouteByUpstream(storeCtx, kind, platformName, exposedID, sessionID); deleteErr != nil {
		return cascadeTaskRoutePersistentState{}, true, errors.Join(decodeErr, fmt.Errorf("delete invalid cascade task route: %w", deleteErr))
	}
	slog.Warn("removed invalid persisted cascade task route", "direction", "upstream", "kind", kind, "platform", platformName, "session_id", sessionID, "err", decodeErr)
	return cascadeTaskRoutePersistentState{}, false, nil
}

func decodeCascadeTaskRouteState(payload []byte, found bool, loadErr error) (cascadeTaskRoutePersistentState, bool, error) {
	if loadErr != nil {
		return cascadeTaskRoutePersistentState{}, false, fmt.Errorf("load cascade task route: %w", loadErr)
	}
	if !found {
		return cascadeTaskRoutePersistentState{}, false, nil
	}
	var state cascadeTaskRoutePersistentState
	if err := json.Unmarshal(payload, &state); err != nil {
		return cascadeTaskRoutePersistentState{}, true, fmt.Errorf("decode cascade task route: %w", err)
	}
	if err := validateCascadeTaskRoutePersistentState(state); err != nil {
		return cascadeTaskRoutePersistentState{}, true, err
	}
	return state, true, nil
}

func validateCascadeTaskRoutePersistentState(state cascadeTaskRoutePersistentState) error {
	if state.SchemaVersion > cascadeTaskRouteSchemaVersion {
		return fmt.Errorf("%w: %d", errUnsupportedCascadeTaskRouteSchema, state.SchemaVersion)
	}
	if state.SchemaVersion != cascadeTaskRouteSchemaVersion {
		return fmt.Errorf("invalid persisted cascade task route schema: %d", state.SchemaVersion)
	}
	latestAllowed := time.Now().Add(5 * time.Minute)
	if (state.Kind != cascadeTaskUpgrade && state.Kind != cascadeTaskSnapshot) ||
		strings.TrimSpace(state.PlatformName) == "" || !isGBDeviceIdentifier(state.DownstreamDeviceID) ||
		!isGBDeviceIdentifier(state.DownstreamTargetID) || !isGBDeviceIdentifier(state.ExposedID) ||
		validateGBSessionID(state.UpstreamSessionID) != nil || validateGBSessionID(state.DownstreamSessionID) != nil ||
		strings.TrimSpace(state.RequestFingerprint) == "" || state.CreatedAt.IsZero() || state.CreatedAt.After(latestAllowed) ||
		(!state.UpdatedAt.IsZero() && (state.UpdatedAt.Before(state.CreatedAt) || state.UpdatedAt.After(latestAllowed))) {
		return fmt.Errorf("invalid persisted cascade task route")
	}
	if state.Identity != nil {
		identity, err := parseMonitorUserIdentity(state.Identity.String())
		if err != nil || identity.String() != state.Identity.String() {
			return fmt.Errorf("invalid persisted cascade task identity")
		}
	}
	return nil
}

func (g *GB28181API) deletePersistedCascadeTaskRoute(ctx context.Context, route *cascadeTaskRoute) error {
	store := g.cascadeTaskRouteStorer()
	if store == nil || route == nil {
		return nil
	}
	storeCtx, cancel := taskStateContext(ctx)
	defer cancel()
	if err := store.DeleteGBCascadeTaskRoute(storeCtx, route.kind, route.downstreamDeviceID, route.downstreamSessionID); err != nil {
		return fmt.Errorf("delete cascade task route: %w", err)
	}
	return nil
}

func (g *GB28181API) deletePersistedCascadeTaskRouteByUpstream(ctx context.Context, kind, platformName, exposedID, sessionID string) error {
	store := g.cascadeTaskRouteStorer()
	if store == nil {
		return nil
	}
	storeCtx, cancel := taskStateContext(ctx)
	defer cancel()
	deleter, ok := store.(gbCascadeTaskRouteUpstreamDeleter)
	if !ok {
		return fmt.Errorf("delete cascade task route by upstream identity is unavailable")
	}
	if err := deleter.DeleteGBCascadeTaskRouteByUpstream(
		storeCtx, kind, strings.TrimSpace(platformName), strings.TrimSpace(exposedID), strings.TrimSpace(sessionID),
	); err != nil {
		return fmt.Errorf("delete cascade task route by upstream identity: %w", err)
	}
	return nil
}

func (g *GB28181API) cleanupPersistedCascadeTaskRoutes(now time.Time) {
	store := g.cascadeTaskRouteStorer()
	if store == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	ctx, cancel := taskStateContext(context.Background())
	defer cancel()
	if err := store.CleanupGBCascadeTaskRoutes(ctx, now.Add(-cascadeTaskRouteTTL), maxCascadeTaskRoutes); err != nil {
		slog.Error("cleanup cascade task routes", "err", err)
	}
}
