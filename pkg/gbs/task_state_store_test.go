package gbs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

var errTaskStateSave = errors.New("task state save failed")
var errTaskStateLoad = errors.New("task state load failed")
var errTaskStateDelete = errors.New("task state delete failed")
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

func (m *persistentTaskMemory) DeleteGBCascadeTaskRouteByUpstream(ctx context.Context, kind, platformName, exposedID, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	upstreamKey := persistentCascadeUpstreamKey(kind, platformName, exposedID, sessionID)
	m.mu.Lock()
	payload := m.upstreamRoutes[upstreamKey]
	delete(m.upstreamRoutes, upstreamKey)
	var state cascadeTaskRoutePersistentState
	if json.Unmarshal(payload, &state) == nil {
		delete(m.downstreamRoutes, persistentCascadeDownstreamKey(state.Kind, state.DownstreamDeviceID, state.DownstreamSessionID))
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

type failingTaskLoadMemory struct {
	*persistentTaskMemory
}

func (m *failingTaskLoadMemory) LoadGBTaskState(context.Context, string, string, string) ([]byte, bool, error) {
	return nil, false, errTaskStateLoad
}

type failingTaskDeleteMemory struct {
	*persistentTaskMemory
}

func (m *failingTaskDeleteMemory) DeleteGBTaskState(context.Context, string, string, string) error {
	return errTaskStateDelete
}

func (m *failingTaskDeleteMemory) GetChannel(deviceID, channelID string) (*Channel, bool) {
	if deviceID != gb10DeviceID || channelID != gb10ChannelID {
		return nil, false
	}
	return &Channel{ChannelID: channelID, device: m.device}, true
}

type observingTaskContextMemory struct {
	*persistentTaskMemory
	saveContexts chan context.Context
}

func (m *observingTaskContextMemory) SaveGBTaskState(
	ctx context.Context,
	kind, deviceID, sessionID string,
	payload []byte,
	updatedAt time.Time,
) error {
	m.saveContexts <- ctx
	return m.persistentTaskMemory.SaveGBTaskState(ctx, kind, deviceID, sessionID, payload, updatedAt)
}

type taskContextMarkerKey struct{}

type unavailableTaskMemory struct {
	*failingTaskMemory
}

func (*unavailableTaskMemory) GBTaskStateAvailable() bool { return false }

type cleanupRecordingMemory struct {
	*persistentTaskMemory
	cleanupMu sync.Mutex
	cleanups  []taskStateCleanupCall
}

type taskStateCleanupCall struct {
	kind      string
	olderThan time.Time
	max       int
}

func (m *cleanupRecordingMemory) CleanupGBTaskStates(_ context.Context, kind string, olderThan time.Time, max int) error {
	m.cleanupMu.Lock()
	m.cleanups = append(m.cleanups, taskStateCleanupCall{kind: kind, olderThan: olderThan, max: max})
	m.cleanupMu.Unlock()
	return nil
}

type failingCascadeTaskRouteMemory struct {
	*persistentTaskMemory
}

func TestTaskStateCleanupIncludesAlarmReceipts(t *testing.T) {
	store := &cleanupRecordingMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	now := time.Date(2026, time.September, 1, 2, 0, 0, 0, time.UTC)

	api.cleanupTaskStates(now)

	store.cleanupMu.Lock()
	calls := append([]taskStateCleanupCall(nil), store.cleanups...)
	store.cleanupMu.Unlock()
	var receipt *taskStateCleanupCall
	var deadLetter *taskStateCleanupCall
	var videoUploadOutbox *taskStateCleanupCall
	var videoUploadReceipt *taskStateCleanupCall
	var rtpDownload *taskStateCleanupCall
	var rtpDownloadSession *taskStateCleanupCall
	var directTCPDownload *taskStateCleanupCall
	for index := range calls {
		if calls[index].kind == gbTaskKindAlarmReceipt {
			receipt = &calls[index]
		}
		if calls[index].kind == gbTaskKindAlarmDeadLetter {
			deadLetter = &calls[index]
		}
		if calls[index].kind == gbTaskKindVideoUploadReceipt {
			videoUploadReceipt = &calls[index]
		}
		if calls[index].kind == gbTaskKindVideoUploadOutbox {
			videoUploadOutbox = &calls[index]
		}
		if calls[index].kind == gbTaskKindRTPDownload {
			rtpDownload = &calls[index]
		}
		if calls[index].kind == gbTaskKindRTPDownloadSession {
			rtpDownloadSession = &calls[index]
		}
		if calls[index].kind == gbTaskKindDirectTCPDownload {
			directTCPDownload = &calls[index]
		}
	}
	if receipt == nil {
		t.Fatalf("Alarm receipt cleanup missing: %+v", calls)
	}
	if !receipt.olderThan.Equal(now.Add(-alarmInboxRetention)) || receipt.max != alarmInboxMaxStates {
		t.Fatalf("Alarm receipt cleanup = %+v", *receipt)
	}
	if deadLetter == nil {
		t.Fatalf("Alarm dead letter cleanup missing: %+v", calls)
	}
	if !deadLetter.olderThan.Equal(now.Add(-alarmInboxRetention)) || deadLetter.max != alarmInboxMaxStates {
		t.Fatalf("Alarm dead letter cleanup = %+v", *deadLetter)
	}
	if videoUploadReceipt == nil {
		t.Fatalf("VideoUploadNotify receipt cleanup missing: %+v", calls)
	}
	if videoUploadOutbox == nil {
		t.Fatalf("VideoUploadNotify outbox cleanup missing: %+v", calls)
	}
	if !videoUploadOutbox.olderThan.Equal(now.Add(-videoUploadOutboxRetention)) || videoUploadOutbox.max != maxVideoUploadOutboxStates {
		t.Fatalf("VideoUploadNotify outbox cleanup = %+v", *videoUploadOutbox)
	}
	if !videoUploadReceipt.olderThan.Equal(now.Add(-videoUploadReceiptRetention)) || videoUploadReceipt.max != maxVideoUploadReceipts {
		t.Fatalf("VideoUploadNotify receipt cleanup = %+v", *videoUploadReceipt)
	}
	if rtpDownload == nil || !rtpDownload.olderThan.Equal(now.Add(-rtpDownloadTerminalTTL)) || rtpDownload.max != rtpDownloadMaxChannelTerminalStates {
		t.Fatalf("RTP download cleanup = %+v", rtpDownload)
	}
	if rtpDownloadSession == nil || !rtpDownloadSession.olderThan.Equal(now.Add(-rtpDownloadTerminalTTL)) || rtpDownloadSession.max != rtpDownloadMaxSessionTerminalStates {
		t.Fatalf("RTP download session cleanup = %+v", rtpDownloadSession)
	}
	if directTCPDownload == nil || !directTCPDownload.olderThan.Equal(now.Add(-7*24*time.Hour)) || directTCPDownload.max != directTCPMaxTerminalStates {
		t.Fatalf("direct TCP download cleanup = %+v", directTCPDownload)
	}
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

func TestSnapshotUploadPersistenceFailureDoesNotCommitMemory(t *testing.T) {
	store := &failingTaskMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	api := &GB28181API{
		svr:            &Server{memoryStorer: store},
		snapshotStates: make(map[string]SnapshotState),
	}
	state := SnapshotState{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		SessionID: "snapshot-upload-save-failure-0001", Status: "accepted", UpdatedAt: time.Now(),
	}
	api.snapshotStates[snapshotStateKey(state.DeviceID, state.SessionID)] = state

	err := api.MarkSnapshotUploadedContext(t.Context(), state.DeviceID, state.SessionID)
	if !errors.Is(err, errTaskStateSave) {
		t.Fatalf("snapshot upload persistence error = %v", err)
	}
	current := api.snapshotStates[snapshotStateKey(state.DeviceID, state.SessionID)]
	if current.Status != state.Status || current.ReceivedCount != 0 || !current.UpdatedAt.Equal(state.UpdatedAt) {
		t.Fatalf("failed snapshot upload persistence committed memory state: %+v", current)
	}
}

func TestSnapshotUploadValidationReturnsTaskStoreFailure(t *testing.T) {
	store := &failingTaskLoadMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	api := &GB28181API{
		svr:            &Server{memoryStorer: store},
		snapshotStates: make(map[string]SnapshotState),
	}
	err := api.ValidateSnapshotUploadContext(t.Context(), gb10DeviceID, "snapshot-cover", "snapshot-load-failure-0000000001")
	if !errors.Is(err, errTaskStateLoad) {
		t.Fatalf("snapshot upload validation error = %v", err)
	}
	if errors.Is(err, ErrSnapshotSessionNotFound) {
		t.Fatalf("task store failure was reported as missing session: %v", err)
	}
}

func TestTaskStatusQueriesReturnTaskStoreFailure(t *testing.T) {
	store := &failingTaskLoadMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	api := &GB28181API{
		svr:            &Server{memoryStorer: store},
		upgradeStates:  make(map[string]UpgradeState),
		snapshotStates: make(map[string]SnapshotState),
	}
	if _, ok, err := api.UpgradeStateContext(t.Context(), gb10DeviceID, "upgrade-load-failure-00000000001"); ok || !errors.Is(err, errTaskStateLoad) {
		t.Fatalf("upgrade state query = ok %v, err %v", ok, err)
	}
	if _, ok, err := api.SnapshotStateContext(t.Context(), gb10DeviceID, "snapshot-load-failure-000000001"); ok || !errors.Is(err, errTaskStateLoad) {
		t.Fatalf("snapshot state query = ok %v, err %v", ok, err)
	}
}

func TestTaskStatusQueriesQuarantineInvalidPersistentState(t *testing.T) {
	now := time.Now()
	validUpgrade := UpgradeState{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		SessionID: "upgrade-corrupt-state-000000000001", Status: "pending", UpdatedAt: now,
	}
	validSnapshot := SnapshotState{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		SessionID: "snapshot-corrupt-state-00000000001", Status: "pending", ExpectedCount: 1, UpdatedAt: now,
	}
	tests := []struct {
		name      string
		kind      string
		deviceID  string
		sessionID string
		payload   func() []byte
		load      func(*GB28181API, context.Context, string, string) (bool, error)
	}{
		{
			name: "upgrade malformed JSON", kind: gbTaskKindUpgrade, deviceID: validUpgrade.DeviceID, sessionID: validUpgrade.SessionID,
			payload: func() []byte { return []byte("not-json") },
			load: func(api *GB28181API, ctx context.Context, deviceID, sessionID string) (bool, error) {
				_, ok, err := api.UpgradeStateContext(ctx, deviceID, sessionID)
				return ok, err
			},
		},
		{
			name: "upgrade unknown status", kind: gbTaskKindUpgrade, deviceID: validUpgrade.DeviceID, sessionID: validUpgrade.SessionID,
			payload: func() []byte {
				state := validUpgrade
				state.Status = "unknown"
				payload, _ := json.Marshal(state)
				return payload
			},
			load: func(api *GB28181API, ctx context.Context, deviceID, sessionID string) (bool, error) {
				_, ok, err := api.UpgradeStateContext(ctx, deviceID, sessionID)
				return ok, err
			},
		},
		{
			name: "upgrade future timestamp", kind: gbTaskKindUpgrade, deviceID: validUpgrade.DeviceID, sessionID: validUpgrade.SessionID,
			payload: func() []byte {
				state := validUpgrade
				state.UpdatedAt = now.Add(6 * time.Minute)
				payload, _ := json.Marshal(state)
				return payload
			},
			load: func(api *GB28181API, ctx context.Context, deviceID, sessionID string) (bool, error) {
				_, ok, err := api.UpgradeStateContext(ctx, deviceID, sessionID)
				return ok, err
			},
		},
		{
			name: "snapshot invalid counters", kind: gbTaskKindSnapshot, deviceID: validSnapshot.DeviceID, sessionID: validSnapshot.SessionID,
			payload: func() []byte {
				state := validSnapshot
				state.ExpectedCount = 11
				payload, _ := json.Marshal(state)
				return payload
			},
			load: func(api *GB28181API, ctx context.Context, deviceID, sessionID string) (bool, error) {
				_, ok, err := api.SnapshotStateContext(ctx, deviceID, sessionID)
				return ok, err
			},
		},
		{
			name: "snapshot unknown status", kind: gbTaskKindSnapshot, deviceID: validSnapshot.DeviceID, sessionID: validSnapshot.SessionID,
			payload: func() []byte {
				state := validSnapshot
				state.Status = "unknown"
				payload, _ := json.Marshal(state)
				return payload
			},
			load: func(api *GB28181API, ctx context.Context, deviceID, sessionID string) (bool, error) {
				_, ok, err := api.SnapshotStateContext(ctx, deviceID, sessionID)
				return ok, err
			},
		},
		{
			name: "snapshot future timestamp", kind: gbTaskKindSnapshot, deviceID: validSnapshot.DeviceID, sessionID: validSnapshot.SessionID,
			payload: func() []byte {
				state := validSnapshot
				state.UpdatedAt = now.Add(6 * time.Minute)
				payload, _ := json.Marshal(state)
				return payload
			},
			load: func(api *GB28181API, ctx context.Context, deviceID, sessionID string) (bool, error) {
				_, ok, err := api.SnapshotStateContext(ctx, deviceID, sessionID)
				return ok, err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newPersistentTaskMemory(GBVersion30)
			if err := store.SaveGBTaskState(t.Context(), test.kind, test.deviceID, test.sessionID, test.payload(), now); err != nil {
				t.Fatal(err)
			}
			api := &GB28181API{svr: &Server{memoryStorer: store}}
			if ok, err := test.load(api, t.Context(), test.deviceID, test.sessionID); err != nil || ok {
				t.Fatalf("invalid persisted state load = ok %v, err %v", ok, err)
			}
			if _, found, err := store.LoadGBTaskState(t.Context(), test.kind, test.deviceID, test.sessionID); err != nil || found {
				t.Fatalf("invalid persisted state was not quarantined: found %v, err %v", found, err)
			}
		})
	}
}

func TestTaskStatusQuarantineDeleteFailureIsReturned(t *testing.T) {
	tests := []struct {
		name      string
		kind      string
		sessionID string
		load      func(*GB28181API, context.Context, string) error
	}{
		{
			name: "upgrade", kind: gbTaskKindUpgrade, sessionID: "upgrade-corrupt-delete-00000000001",
			load: func(api *GB28181API, ctx context.Context, sessionID string) error {
				_, _, err := api.UpgradeStateContext(ctx, gb10DeviceID, sessionID)
				return err
			},
		},
		{
			name: "snapshot", kind: gbTaskKindSnapshot, sessionID: "snapshot-corrupt-delete-000000001",
			load: func(api *GB28181API, ctx context.Context, sessionID string) error {
				_, _, err := api.SnapshotStateContext(ctx, gb10DeviceID, sessionID)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := newPersistentTaskMemory(GBVersion30)
			if err := base.SaveGBTaskState(t.Context(), test.kind, gb10DeviceID, test.sessionID, []byte("not-json"), time.Now()); err != nil {
				t.Fatal(err)
			}
			store := &failingTaskDeleteMemory{persistentTaskMemory: base}
			api := &GB28181API{svr: &Server{memoryStorer: store}}
			if err := test.load(api, t.Context(), test.sessionID); !errors.Is(err, errTaskStateDelete) || !strings.Contains(err.Error(), "decode") {
				t.Fatalf("quarantine error = %v", err)
			}
			if _, found, err := base.LoadGBTaskState(t.Context(), test.kind, gb10DeviceID, test.sessionID); err != nil || !found {
				t.Fatalf("failed quarantine did not retain state: found %v, err %v", found, err)
			}
		})
	}
}

func TestTaskStatusInvalidSnapshotCannotDeleteConcurrentUpdate(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name       string
		kind       string
		sessionID  string
		state      any
		quarantine func(*GB28181API, context.Context, string, []byte) error
		load       func(*GB28181API, context.Context, string) (bool, error)
	}{
		{
			name: "upgrade", kind: gbTaskKindUpgrade, sessionID: "upgrade-concurrent-repair-000000001",
			state: UpgradeState{
				DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SessionID: "upgrade-concurrent-repair-000000001",
				Status: "pending", UpdatedAt: now,
			},
			quarantine: func(api *GB28181API, ctx context.Context, sessionID string, invalid []byte) error {
				_, _, err := api.quarantineUpgradeState(ctx, gb10DeviceID, sessionID, invalid, errors.New("old invalid upgrade snapshot"))
				return err
			},
			load: func(api *GB28181API, ctx context.Context, sessionID string) (bool, error) {
				_, ok, err := api.UpgradeStateContext(ctx, gb10DeviceID, sessionID)
				return ok, err
			},
		},
		{
			name: "snapshot", kind: gbTaskKindSnapshot, sessionID: "snapshot-concurrent-repair-0000001",
			state: SnapshotState{
				DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SessionID: "snapshot-concurrent-repair-0000001",
				Status: "pending", ExpectedCount: 1, UpdatedAt: now,
			},
			quarantine: func(api *GB28181API, ctx context.Context, sessionID string, invalid []byte) error {
				_, _, err := api.quarantineSnapshotState(ctx, gb10DeviceID, sessionID, invalid, errors.New("old invalid snapshot"))
				return err
			},
			load: func(api *GB28181API, ctx context.Context, sessionID string) (bool, error) {
				_, ok, err := api.SnapshotStateContext(ctx, gb10DeviceID, sessionID)
				return ok, err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newPersistentTaskMemory(GBVersion30)
			payload, err := json.Marshal(test.state)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SaveGBTaskState(t.Context(), test.kind, gb10DeviceID, test.sessionID, payload, now); err != nil {
				t.Fatal(err)
			}
			api := &GB28181API{svr: &Server{memoryStorer: store}}
			if err := test.quarantine(api, t.Context(), test.sessionID, []byte("old-invalid-payload")); err == nil || !strings.Contains(err.Error(), "changed while quarantining") {
				t.Fatalf("stale quarantine error = %v", err)
			}
			if ok, err := test.load(api, t.Context(), test.sessionID); err != nil || !ok {
				t.Fatalf("concurrent repaired state load = ok %v, err %v", ok, err)
			}
		})
	}
}

func TestSnapshotUploadPersistenceUsesServiceLifecycleContext(t *testing.T) {
	store := &observingTaskContextMemory{
		persistentTaskMemory: newPersistentTaskMemory(GBVersion30),
		saveContexts:         make(chan context.Context, 1),
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.WithValue(context.Background(), taskContextMarkerKey{}, "snapshot-upload"))
	defer lifecycleCancel()
	server := &Server{memoryStorer: store}
	api := &GB28181API{
		svr: server, lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		snapshotStates: make(map[string]SnapshotState),
	}
	server.gb = api
	state := SnapshotState{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		SessionID: "snapshot-upload-lifecycle-0000001", Status: "completed", UpdatedAt: time.Now(),
	}
	api.snapshotStates[snapshotStateKey(state.DeviceID, state.SessionID)] = state

	if err := server.CommitSnapshotUpload(state.DeviceID, state.SessionID); err != nil {
		t.Fatalf("mark snapshot uploaded: %v", err)
	}
	if ctx := <-store.saveContexts; ctx.Value(taskContextMarkerKey{}) != "snapshot-upload" {
		t.Fatalf("snapshot upload persistence context marker = %v", ctx.Value(taskContextMarkerKey{}))
	}
	current := api.snapshotStates[snapshotStateKey(state.DeviceID, state.SessionID)]
	if current.Status != "completed" || current.ReceivedCount != 1 {
		t.Fatalf("snapshot upload state = %+v", current)
	}
}

func TestTaskOutcomePersistenceFailureIsReturned(t *testing.T) {
	store := &failingTaskMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	api := &GB28181API{
		svr:            &Server{memoryStorer: store},
		upgradeStates:  make(map[string]UpgradeState),
		snapshotStates: make(map[string]SnapshotState),
	}

	t.Run("upgrade", func(t *testing.T) {
		state := UpgradeState{
			DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
			SessionID: "upgrade-outcome-failure-0000000001", Status: "pending", UpdatedAt: time.Now(),
		}
		api.upgradeStates[upgradeStateKey(state.DeviceID, state.SessionID)] = state
		cause := errors.New("upgrade response timeout")
		err := api.persistUpgradeOutcomeError(state, "response_timeout", cause)
		if !errors.Is(err, cause) || !errors.Is(err, errTaskStateSave) {
			t.Fatalf("upgrade outcome error = %v", err)
		}
		if current := api.upgradeStates[upgradeStateKey(state.DeviceID, state.SessionID)]; current.Status != "pending" {
			t.Fatalf("failed upgrade outcome persistence committed status %q", current.Status)
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		state := SnapshotState{
			DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
			SessionID: "snapshot-outcome-failure-00000001", Status: "pending", UpdatedAt: time.Now(),
		}
		api.snapshotStates[snapshotStateKey(state.DeviceID, state.SessionID)] = state
		cause := errors.New("snapshot response timeout")
		err := api.persistSnapshotOutcomeError(state.DeviceID, state.SessionID, "response_timeout", cause)
		if !errors.Is(err, cause) || !errors.Is(err, errTaskStateSave) {
			t.Fatalf("snapshot outcome error = %v", err)
		}
		if current := api.snapshotStates[snapshotStateKey(state.DeviceID, state.SessionID)]; current.Status != "pending" {
			t.Fatalf("failed snapshot outcome persistence committed status %q", current.Status)
		}
	})
}

func TestUnsentTaskCleanupFailureIsReturnedAndStateRetained(t *testing.T) {
	newAPI := func() (*GB28181API, *failingTaskDeleteMemory) {
		store := &failingTaskDeleteMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
		server := &Server{memoryStorer: store}
		api := &GB28181API{
			svr: server, upgradeStates: make(map[string]UpgradeState), snapshotStates: make(map[string]SnapshotState),
		}
		server.gb = api
		return api, store
	}

	t.Run("upgrade", func(t *testing.T) {
		api, store := newAPI()
		sessionID := "upgrade-delete-failure-000000000001"
		_, err := api.Upgrade(t.Context(), &UpgradeInput{
			DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SessionID: sessionID,
			Firmware: "V1.2.3", FileURL: "https://example.invalid/fw.bin", Manufacturer: "vendor",
		})
		if err == nil || !strings.Contains(err.Error(), "SIP server is unavailable") || !errors.Is(err, errTaskStateDelete) {
			t.Fatalf("unsent upgrade cleanup error = %v", err)
		}
		state, ok := api.upgradeStates[upgradeStateKey(gb10DeviceID, sessionID)]
		if !ok || state.Status != "pending" {
			t.Fatalf("upgrade state after delete failure = %+v, %v", state, ok)
		}
		store.mu.Lock()
		_, persisted := store.records[persistentTaskKey(gbTaskKindUpgrade, gb10DeviceID, sessionID)]
		store.mu.Unlock()
		if !persisted {
			t.Fatal("upgrade delete failure removed persisted state but retained memory")
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		api, store := newAPI()
		_, err := api.QuerySnapshotContext(t.Context(), gb10DeviceID, gb10DeviceID, "delete-failure-cover")
		if err == nil || !strings.Contains(err.Error(), "SIP server is unavailable") || !errors.Is(err, errTaskStateDelete) {
			t.Fatalf("unsent snapshot cleanup error = %v", err)
		}
		if len(api.snapshotStates) != 1 {
			t.Fatalf("snapshot states after delete failure = %d; want 1", len(api.snapshotStates))
		}
		for _, state := range api.snapshotStates {
			if state.Status != "pending" {
				t.Fatalf("snapshot state after delete failure = %+v", state)
			}
			store.mu.Lock()
			_, persisted := store.records[persistentTaskKey(gbTaskKindSnapshot, state.DeviceID, state.SessionID)]
			store.mu.Unlock()
			if !persisted {
				t.Fatal("snapshot delete failure removed persisted state but retained memory")
			}
		}
	})
}

func TestTaskOutcomePersistenceUsesServiceLifecycleContext(t *testing.T) {
	store := &observingTaskContextMemory{
		persistentTaskMemory: newPersistentTaskMemory(GBVersion30),
		saveContexts:         make(chan context.Context, 2),
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.WithValue(context.Background(), taskContextMarkerKey{}, "service"))
	defer lifecycleCancel()
	api := &GB28181API{
		svr: &Server{memoryStorer: store}, lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		upgradeStates: make(map[string]UpgradeState), snapshotStates: make(map[string]SnapshotState),
	}

	upgrade := UpgradeState{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		SessionID: "upgrade-lifecycle-context-00000001", Status: "pending", UpdatedAt: time.Now(),
	}
	api.upgradeStates[upgradeStateKey(upgrade.DeviceID, upgrade.SessionID)] = upgrade
	upgradeCause := errors.New("upgrade cancelled")
	if err := api.persistUpgradeOutcomeError(upgrade, "cancelled", upgradeCause); !errors.Is(err, upgradeCause) {
		t.Fatalf("upgrade outcome error = %v", err)
	}
	if ctx := <-store.saveContexts; ctx.Value(taskContextMarkerKey{}) != "service" {
		t.Fatalf("upgrade outcome persistence context marker = %v", ctx.Value(taskContextMarkerKey{}))
	}

	snapshot := SnapshotState{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		SessionID: "snapshot-lifecycle-context-0000001", Status: "pending", UpdatedAt: time.Now(),
	}
	api.snapshotStates[snapshotStateKey(snapshot.DeviceID, snapshot.SessionID)] = snapshot
	snapshotCause := errors.New("snapshot cancelled")
	if err := api.persistSnapshotOutcomeError(snapshot.DeviceID, snapshot.SessionID, "cancelled", snapshotCause); !errors.Is(err, snapshotCause) {
		t.Fatalf("snapshot outcome error = %v", err)
	}
	if ctx := <-store.saveContexts; ctx.Value(taskContextMarkerKey{}) != "service" {
		t.Fatalf("snapshot outcome persistence context marker = %v", ctx.Value(taskContextMarkerKey{}))
	}
}

func TestTaskFinalNotificationsUseServiceLifecycleContext(t *testing.T) {
	newAPI := func() (*GB28181API, *observingTaskContextMemory, context.CancelFunc) {
		store := &observingTaskContextMemory{
			persistentTaskMemory: newPersistentTaskMemory(GBVersion30),
			saveContexts:         make(chan context.Context, 1),
		}
		lifecycleCtx, lifecycleCancel := context.WithCancel(context.WithValue(context.Background(), taskContextMarkerKey{}, "notification"))
		api := &GB28181API{
			svr: &Server{memoryStorer: store}, lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
			upgradeStates: make(map[string]UpgradeState), snapshotStates: make(map[string]SnapshotState),
		}
		return api, store, lifecycleCancel
	}

	t.Run("upgrade", func(t *testing.T) {
		api, store, cancel := newAPI()
		defer cancel()
		sessionID := "upgrade-notification-context-000001"
		api.upgradeStates[upgradeStateKey(gb10DeviceID, sessionID)] = UpgradeState{
			DeviceID: gb10DeviceID, ChannelID: gb10DeviceID, SessionID: sessionID,
			Status: "accepted", Firmware: "V1", UpdatedAt: time.Now(),
		}
		body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>701</SN><DeviceID>` + gb10DeviceID +
			`</DeviceID><SessionID>` + sessionID + `</SessionID><UpgradeResult>OK</UpgradeResult><Firmware>V2</Firmware></Notify>`)
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "upgrade-notification-context", body, api.sipMessageDeviceUpgradeResult)
		assertFlowOK(t, response)
		if ctx := <-store.saveContexts; ctx.Value(taskContextMarkerKey{}) != "notification" {
			t.Fatalf("upgrade notification persistence context marker = %v", ctx.Value(taskContextMarkerKey{}))
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		api, store, cancel := newAPI()
		defer cancel()
		sessionID := "snapshot-notification-context-00001"
		api.snapshotStates[snapshotStateKey(gb10DeviceID, sessionID)] = SnapshotState{
			DeviceID: gb10DeviceID, ChannelID: gb10DeviceID, SessionID: sessionID,
			Status: "accepted", ExpectedCount: 1, UpdatedAt: time.Now(),
		}
		body := []byte(`<Notify><CmdType>UploadSnapShotFinished</CmdType><SN>702</SN><DeviceID>` + gb10DeviceID +
			`</DeviceID><SessionID>` + sessionID + `</SessionID><SnapShotList><SnapShotFileID>` + gb10DeviceID +
			`022026082508150000001</SnapShotFileID></SnapShotList></Notify>`)
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "snapshot-notification-context", body, api.sipMessageSnapshotFinished)
		assertFlowOK(t, response)
		if ctx := <-store.saveContexts; ctx.Value(taskContextMarkerKey{}) != "notification" {
			t.Fatalf("snapshot notification persistence context marker = %v", ctx.Value(taskContextMarkerKey{}))
		}
	})
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
