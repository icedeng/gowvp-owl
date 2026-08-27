package gbs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var errTaskStateSave = errors.New("task state save failed")
var errCascadeTaskRouteSave = errors.New("cascade task route save failed")

type persistentTaskMemory struct {
	*versionGateMemory
	mu               sync.Mutex
	records          map[string][]byte
	downstreamRoutes map[string][]byte
	upstreamRoutes   map[string][]byte
}

func newPersistentTaskMemory(version GBProtocolVersion) *persistentTaskMemory {
	return &persistentTaskMemory{
		versionGateMemory: &versionGateMemory{device: &Device{IsOnline: true, gbVersion: string(version)}},
		records:           make(map[string][]byte),
		downstreamRoutes:  make(map[string][]byte),
		upstreamRoutes:    make(map[string][]byte),
	}
}

func persistentTaskKey(kind, deviceID, sessionID string) string {
	return fmt.Sprintf("%s\x00%s\x00%s", kind, deviceID, sessionID)
}

func (m *persistentTaskMemory) SaveGBTaskState(ctx context.Context, kind, deviceID, sessionID string, payload []byte, _ time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	m.records[persistentTaskKey(kind, deviceID, sessionID)] = append([]byte(nil), payload...)
	m.mu.Unlock()
	return nil
}

func (m *persistentTaskMemory) LoadGBTaskState(ctx context.Context, kind, deviceID, sessionID string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	m.mu.Lock()
	payload, ok := m.records[persistentTaskKey(kind, deviceID, sessionID)]
	payload = append([]byte(nil), payload...)
	m.mu.Unlock()
	return payload, ok, nil
}

func (m *persistentTaskMemory) DeleteGBTaskState(ctx context.Context, kind, deviceID, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.records, persistentTaskKey(kind, deviceID, sessionID))
	m.mu.Unlock()
	return nil
}

func (m *persistentTaskMemory) CleanupGBTaskStates(context.Context, string, time.Time, int) error {
	return nil
}

func persistentCascadeDownstreamKey(kind, deviceID, sessionID string) string {
	return fmt.Sprintf("%s\x00%s\x00%s", kind, deviceID, sessionID)
}

func persistentCascadeUpstreamKey(kind, platformName, exposedID, sessionID string) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s", kind, platformName, exposedID, sessionID)
}

func (m *persistentTaskMemory) SaveGBCascadeTaskRoute(
	ctx context.Context,
	kind, platformName, downstreamDeviceID, downstreamSessionID, exposedID, upstreamSessionID string,
	payload []byte,
	_ time.Time,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	m.downstreamRoutes[persistentCascadeDownstreamKey(kind, downstreamDeviceID, downstreamSessionID)] = append([]byte(nil), payload...)
	m.upstreamRoutes[persistentCascadeUpstreamKey(kind, platformName, exposedID, upstreamSessionID)] = append([]byte(nil), payload...)
	m.mu.Unlock()
	return nil
}

func (m *persistentTaskMemory) LoadGBCascadeTaskRouteByDownstream(ctx context.Context, kind, deviceID, sessionID string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	m.mu.Lock()
	payload, ok := m.downstreamRoutes[persistentCascadeDownstreamKey(kind, deviceID, sessionID)]
	payload = append([]byte(nil), payload...)
	m.mu.Unlock()
	return payload, ok, nil
}

func (m *persistentTaskMemory) LoadGBCascadeTaskRouteByUpstream(ctx context.Context, kind, platformName, exposedID, sessionID string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	m.mu.Lock()
	payload, ok := m.upstreamRoutes[persistentCascadeUpstreamKey(kind, platformName, exposedID, sessionID)]
	payload = append([]byte(nil), payload...)
	m.mu.Unlock()
	return payload, ok, nil
}

func (m *persistentTaskMemory) DeleteGBCascadeTaskRoute(ctx context.Context, kind, deviceID, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	downstreamKey := persistentCascadeDownstreamKey(kind, deviceID, sessionID)
	m.mu.Lock()
	payload := m.downstreamRoutes[downstreamKey]
	delete(m.downstreamRoutes, downstreamKey)
	var state cascadeTaskRoutePersistentState
	if json.Unmarshal(payload, &state) == nil {
		delete(m.upstreamRoutes, persistentCascadeUpstreamKey(state.Kind, state.PlatformName, state.ExposedID, state.UpstreamSessionID))
	}
	m.mu.Unlock()
	return nil
}

func (m *persistentTaskMemory) CleanupGBCascadeTaskRoutes(context.Context, time.Time, int) error {
	return nil
}

type failingTaskMemory struct {
	*persistentTaskMemory
}

func (m *failingTaskMemory) SaveGBTaskState(context.Context, string, string, string, []byte, time.Time) error {
	return errTaskStateSave
}

type unavailableTaskMemory struct {
	*failingTaskMemory
}

func (*unavailableTaskMemory) GBTaskStateAvailable() bool { return false }

type failingCascadeTaskRouteMemory struct {
	*persistentTaskMemory
}

func (*failingCascadeTaskRouteMemory) SaveGBCascadeTaskRoute(
	context.Context,
	string, string, string, string, string, string,
	[]byte,
	time.Time,
) error {
	return errCascadeTaskRouteSave
}

func TestTaskStatePersistenceFailureDoesNotCommitMemory(t *testing.T) {
	store := &failingTaskMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	api := &GB28181API{svr: &Server{memoryStorer: store}}

	upgrade := UpgradeState{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		SessionID: "upgrade-save-failure-000000000001", Status: "pending",
	}
	if err := api.storeUpgradeStateContext(t.Context(), upgrade); !errors.Is(err, errTaskStateSave) {
		t.Fatalf("upgrade persistence error = %v", err)
	}
	if _, ok := api.upgradeStates[upgradeStateKey(upgrade.DeviceID, upgrade.SessionID)]; ok {
		t.Fatal("failed upgrade persistence committed memory state")
	}

	snapshot := SnapshotState{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		SessionID: "snapshot-save-failure-00000000001", Status: "pending",
	}
	if err := api.storeSnapshotStateContext(t.Context(), snapshot); !errors.Is(err, errTaskStateSave) {
		t.Fatalf("snapshot persistence error = %v", err)
	}
	if _, ok := api.snapshotStates[snapshotStateKey(snapshot.DeviceID, snapshot.SessionID)]; ok {
		t.Fatal("failed snapshot persistence committed memory state")
	}
}

func TestUnavailableOptionalTaskStoreFallsBackToMemory(t *testing.T) {
	store := &unavailableTaskMemory{failingTaskMemory: &failingTaskMemory{
		persistentTaskMemory: newPersistentTaskMemory(GBVersion30),
	}}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	state := UpgradeState{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		SessionID: "upgrade-memory-fallback-0000000001", Status: "pending",
	}
	if err := api.storeUpgradeStateContext(t.Context(), state); err != nil {
		t.Fatalf("optional task store fallback failed: %v", err)
	}
	if restored, ok := api.UpgradeState(state.DeviceID, state.SessionID); !ok || restored.Status != state.Status {
		t.Fatalf("memory fallback state = %+v, %v", restored, ok)
	}
}
