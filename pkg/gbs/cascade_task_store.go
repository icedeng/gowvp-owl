package gbs

import (
	"context"
	"encoding/json"
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

type gbCascadeTaskRouteAvailability interface {
	GBCascadeTaskRouteAvailable() bool
}

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
	state := cascadeTaskRoutePersistentState{
		SchemaVersion: cascadeTaskRouteSchemaVersion,
		Kind:          route.kind, DownstreamDeviceID: route.downstreamDeviceID,
		DownstreamTargetID: route.downstreamTargetID, ExposedID: route.exposedID,
		UpstreamSessionID: route.upstreamSessionID, DownstreamSessionID: route.downstreamSessionID,
		RequestFingerprint: route.requestFingerprint, Identity: route.identity.clone(),
		LocalGatewayID: route.localGatewayID, CreatedAt: route.createdAt, UpdatedAt: time.Now(),
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
	return g.persistCascadeTaskRouteState(ctx, route.persistentState())
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
		state.ExposedID, state.UpstreamSessionID, payload, state.CreatedAt,
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
	payload, ok, err := store.LoadGBCascadeTaskRouteByDownstream(storeCtx, kind, strings.TrimSpace(deviceID), strings.TrimSpace(sessionID))
	return decodeCascadeTaskRouteState(payload, ok, err)
}

func (g *GB28181API) loadCascadeTaskRouteStateByUpstream(ctx context.Context, kind, platformName, exposedID, sessionID string) (cascadeTaskRoutePersistentState, bool, error) {
	store := g.cascadeTaskRouteStorer()
	if store == nil {
		return cascadeTaskRoutePersistentState{}, false, nil
	}
	storeCtx, cancel := taskStateContext(ctx)
	defer cancel()
	payload, ok, err := store.LoadGBCascadeTaskRouteByUpstream(
		storeCtx, kind, strings.TrimSpace(platformName), strings.TrimSpace(exposedID), strings.TrimSpace(sessionID),
	)
	return decodeCascadeTaskRouteState(payload, ok, err)
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
	if state.SchemaVersion != cascadeTaskRouteSchemaVersion ||
		(state.Kind != cascadeTaskUpgrade && state.Kind != cascadeTaskSnapshot) ||
		strings.TrimSpace(state.PlatformName) == "" || !isGBDeviceIdentifier(state.DownstreamDeviceID) ||
		!isGBDeviceIdentifier(state.DownstreamTargetID) || !isGBDeviceIdentifier(state.ExposedID) ||
		validateGBSessionID(state.UpstreamSessionID) != nil || validateGBSessionID(state.DownstreamSessionID) != nil ||
		strings.TrimSpace(state.RequestFingerprint) == "" || state.CreatedAt.IsZero() {
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
