package gbs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

type fullManualSubscriptionMemory struct {
	*alarmInboxTestMemory
}

type failingManualSubscriptionTerminationMemory struct {
	*alarmInboxTestMemory
	failMu     sync.Mutex
	failManual bool
}

type blockingManualSubscriptionListMemory struct {
	*alarmInboxTestMemory
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *blockingManualSubscriptionListMemory) ListGBTaskStates(ctx context.Context, kind string, limit int) ([]ipc.GBTaskStateRecord, error) {
	records, err := m.alarmInboxTestMemory.ListGBTaskStates(ctx, kind, limit)
	if err != nil || kind != gbTaskKindManualSubscription {
		return records, err
	}
	blocked := false
	m.once.Do(func() {
		blocked = true
		close(m.entered)
	})
	if blocked {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.release:
		}
	}
	return records, nil
}

func (m *failingManualSubscriptionTerminationMemory) setFailManualSubscriptionSave(fail bool) {
	m.failMu.Lock()
	m.failManual = fail
	m.failMu.Unlock()
}

func (m *failingManualSubscriptionTerminationMemory) SaveGBTaskState(
	ctx context.Context,
	kind, deviceID, sessionID string,
	payload []byte,
	updatedAt time.Time,
) error {
	m.failMu.Lock()
	fail := m.failManual && kind == gbTaskKindManualSubscription
	m.failMu.Unlock()
	if fail {
		return errTaskStateSave
	}
	return m.alarmInboxTestMemory.SaveGBTaskState(ctx, kind, deviceID, sessionID, payload, updatedAt)
}

func (m *fullManualSubscriptionMemory) ListGBTaskStates(ctx context.Context, kind string, limit int) ([]ipc.GBTaskStateRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if kind != gbTaskKindManualSubscription {
		return m.alarmInboxTestMemory.ListGBTaskStates(ctx, kind, limit)
	}
	return make([]ipc.GBTaskStateRecord, maxManualSubscriptionIntentStates), nil
}

func newManualSubscriptionTestMemory(version GBProtocolVersion) *alarmInboxTestMemory {
	return &alarmInboxTestMemory{
		persistentTaskMemory: newPersistentTaskMemory(version),
		updatedAt:            make(map[string]time.Time),
	}
}

func manualSubscriptionTestInput(version GBProtocolVersion) SubscribeInput {
	switch version {
	case GBVersion10:
		return SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "alarm", Expires: 60, AlarmMethod: "2/5"}
	case GBVersion11:
		return SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "catalog", Expires: 600, StartTime: "2026-09-01T00:00:00"}
	case GBVersion20:
		return SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "mobile_position", Expires: 60, Interval: 5}
	default:
		return SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "ptz_position", Expires: 60}
	}
}

func saveManualSubscriptionTestIntent(t *testing.T, api *GB28181API, input SubscribeInput, identity *monitorUserIdentity) string {
	t.Helper()
	ctx := withMonitorUserIdentityRoute(t.Context(), identity, testLocalGatewayID)
	cmdType, ok := normalizeSubscribeCmdType(input.Event)
	if !ok {
		t.Fatalf("normalize subscription event %q", input.Event)
	}
	key := buildOutgoingSubscriptionKey(input.DeviceID, input.TargetID, cmdType, &input) + monitorUserIdentitySubscriptionKey(ctx)
	managed, err := api.saveManualSubscriptionIntent(ctx, key, input, identity, testLocalGatewayID)
	if err != nil || !managed {
		t.Fatalf("save manual subscription intent = managed:%v err:%v", managed, err)
	}
	return key
}

func TestManualSubscriptionIntentRecoversAllProtocolProfiles(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardName(), func(t *testing.T) {
			store := newManualSubscriptionTestMemory(version)
			original := &GB28181API{svr: &Server{memoryStorer: store}}
			input := manualSubscriptionTestInput(version)
			identity := &monitorUserIdentity{
				Gateways: []string{testRemoteGatewayID}, UserID: testRemoteUserID,
				Organization: "remoteorg", Category: "dispatcher", Rank: "level2",
			}
			key := saveManualSubscriptionTestIntent(t, original, input, identity)

			restarted := &GB28181API{svr: &Server{memoryStorer: store}}
			var mu sync.Mutex
			var recovered []SubscribeInput
			restarted.manualSubscribeRefresh = func(ctx context.Context, actual *SubscribeInput) error {
				if got := monitorUserIdentityFromContext(ctx); got == nil || got.String() != identity.String() {
					t.Fatalf("recovered identity = %+v", got)
				}
				gateway, _ := ctx.Value(monitorUserIdentityGatewayContextKey{}).(string)
				if gateway != testLocalGatewayID {
					t.Fatalf("recovered gateway = %q", gateway)
				}
				mu.Lock()
				recovered = append(recovered, *actual)
				mu.Unlock()
				return nil
			}
			restarted.processManualSubscriptionRecovery(t.Context(), "", true)
			mu.Lock()
			defer mu.Unlock()
			if len(recovered) != 1 || recovered[0] != input {
				t.Fatalf("recovered subscriptions = %+v; want %+v", recovered, input)
			}
			if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindManualSubscription, gb10DeviceID, key); err != nil || !exists {
				t.Fatalf("recovered intent retained = %v, %v", exists, err)
			}
		})
	}
}

func TestSuccessfulManualSubscriptionPersistsAndCancelDeletesAcrossProfiles(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardName(), func(t *testing.T) {
			store := newManualSubscriptionTestMemory(version)
			platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
			device := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
			sipServer := sip.NewServer(platform)
			server := &Server{Server: sipServer, fromAddress: *platform, memoryStorer: store}
			api := &GB28181API{svr: server}
			server.gb = api
			localRaw, remoteRaw := net.Pipe()
			connection := sip.NewTCPConnection(localRaw)
			go sipServer.ProcessTCPConnection(connection)
			t.Cleanup(func() {
				_ = remoteRaw.Close()
				sipServer.Close()
			})
			store.device.UpdateRuntime(func(current *Device) {
				current.conn = connection
				current.source = connection.RemoteAddr()
				current.to = device
			})

			input := manualSubscriptionTestInput(version)
			remoteErr := make(chan error, 1)
			go func() {
				_ = remoteRaw.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(remoteRaw)
				for index := 0; index < 2; index++ {
					request, err := readAnnexGTestSIPFrame(reader)
					if err != nil {
						remoteErr <- err
						return
					}
					extra := ""
					if index == 0 {
						extra = fmt.Sprintf("Expires: %d", input.Expires)
					}
					response := annexGTestSIPResponse(request, http.StatusOK, "OK", extra)
					if index == 0 {
						to := annexGTestSIPHeader(request, "To")
						response = strings.Replace(response, "To: "+to+"\r\n", "To: "+to+";tag=durable-subscription\r\n", 1)
					}
					if _, err = remoteRaw.Write([]byte(response)); err != nil {
						remoteErr <- err
						return
					}
				}
				remoteErr <- nil
			}()

			if err := api.Subscribe(t.Context(), &input); err != nil {
				t.Fatalf("create durable subscription: %v", err)
			}
			cmdType, _ := normalizeSubscribeCmdType(input.Event)
			key := buildOutgoingSubscriptionKey(input.DeviceID, input.TargetID, cmdType, &input)
			if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindManualSubscription, gb10DeviceID, key); err != nil || !exists {
				t.Fatalf("successful subscription persistence = %v, %v", exists, err)
			}
			cancel := input
			cancel.Cancel = true
			if err := api.Subscribe(t.Context(), &cancel); err != nil {
				t.Fatalf("cancel durable subscription: %v", err)
			}
			if err := <-remoteErr; err != nil {
				t.Fatal(err)
			}
			if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindManualSubscription, gb10DeviceID, key); err != nil || exists {
				t.Fatalf("cancelled subscription persistence = %v, %v", exists, err)
			}
		})
	}
}

func TestManualSubscriptionRecoveryBacksOffOfflineAndRegisterWakeForcesRetry(t *testing.T) {
	store := newManualSubscriptionTestMemory(GBVersion30)
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	input := manualSubscriptionTestInput(GBVersion30)
	key := saveManualSubscriptionTestIntent(t, api, input, nil)
	store.device.IsOnline = false

	api.processManualSubscriptionRecovery(t.Context(), gb10DeviceID, true)
	payload, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindManualSubscription, gb10DeviceID, key)
	if err != nil || !exists {
		t.Fatalf("load deferred subscription intent = %v, %v", exists, err)
	}
	var deferred manualSubscriptionIntentState
	if err := json.Unmarshal(payload, &deferred); err != nil {
		t.Fatal(err)
	}
	if deferred.Attempts != 1 || deferred.NextAttemptAt.IsZero() || deferred.LastError == "" {
		t.Fatalf("offline recovery state = %+v", deferred)
	}

	store.device.IsOnline = true
	calls := 0
	api.manualSubscribeRefresh = func(context.Context, *SubscribeInput) error {
		calls++
		return nil
	}
	api.processManualSubscriptionRecovery(t.Context(), gb10DeviceID, true)
	if calls != 1 {
		t.Fatalf("forced recovery calls = %d; want 1", calls)
	}
}

func TestManualSubscriptionRecoveryFailureBackoffStartsAfterAttempt(t *testing.T) {
	store := newManualSubscriptionTestMemory(GBVersion30)
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	input := manualSubscriptionTestInput(GBVersion30)
	key := saveManualSubscriptionTestIntent(t, api, input, nil)
	var failedAt time.Time
	api.manualSubscribeRefresh = func(context.Context, *SubscribeInput) error {
		time.Sleep(5 * time.Millisecond)
		failedAt = time.Now()
		return ErrDeviceOffline
	}

	api.processManualSubscriptionRecovery(t.Context(), "", true)
	payload, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindManualSubscription, input.DeviceID, key)
	if err != nil || !exists {
		t.Fatalf("load failed recovery state = %v, %v", exists, err)
	}
	var state manualSubscriptionIntentState
	if err := json.Unmarshal(payload, &state); err != nil {
		t.Fatal(err)
	}
	if state.UpdatedAt.Before(failedAt) {
		t.Fatalf("failure updated_at = %v, before attempt failure %v", state.UpdatedAt, failedAt)
	}
	wantEarliestRetry := failedAt.Add(manualSubscriptionRetryDelay(1))
	if state.NextAttemptAt.Before(wantEarliestRetry) {
		t.Fatalf("failure next_attempt_at = %v, want >= %v", state.NextAttemptAt, wantEarliestRetry)
	}
}

func TestManualSubscriptionRecoveryAdvancesPastBatch(t *testing.T) {
	store := newManualSubscriptionTestMemory(GBVersion30)
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	total := manualSubscriptionRecoveryBatch + 1
	for index := 0; index < total; index++ {
		input := manualSubscriptionTestInput(GBVersion30)
		input.TargetID = fmt.Sprintf("3402000000132%07d", index)
		saveManualSubscriptionTestIntent(t, api, input, nil)
	}

	seen := make(map[string]int, total)
	api.manualSubscribeRefresh = func(_ context.Context, input *SubscribeInput) error {
		seen[input.TargetID]++
		return ErrDeviceOffline
	}
	api.processManualSubscriptionRecovery(t.Context(), "", true)
	if len(seen) != manualSubscriptionRecoveryBatch {
		t.Fatalf("first recovery batch size = %d, want %d", len(seen), manualSubscriptionRecoveryBatch)
	}
	api.processManualSubscriptionRecovery(t.Context(), "", false)
	if len(seen) != total {
		t.Fatalf("recovered unique intents = %d, want %d", len(seen), total)
	}
	for targetID, calls := range seen {
		if calls != 1 {
			t.Fatalf("subscription %s recovery calls = %d, want 1", targetID, calls)
		}
	}
}

func TestManualSubscriptionRecoverySkipsBlockedBatchHead(t *testing.T) {
	store := newManualSubscriptionTestMemory(GBVersion30)
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	indexedAt := time.Now().Add(-time.Hour)
	for index := 0; index < manualSubscriptionRecoveryBatch; index++ {
		input := manualSubscriptionTestInput(GBVersion30)
		input.TargetID = fmt.Sprintf("3402000000132%07d", index)
		key := saveManualSubscriptionTestIntent(t, api, input, nil)
		payload, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindManualSubscription, input.DeviceID, key)
		if err != nil || !exists {
			t.Fatalf("load blocked subscription intent %d = %v, %v", index, exists, err)
		}
		var state manualSubscriptionIntentState
		if err := json.Unmarshal(payload, &state); err != nil {
			t.Fatal(err)
		}
		state.RetryBlocked = true
		state.TerminationReason = "rejected"
		payload, err = json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveGBTaskState(
			t.Context(), gbTaskKindManualSubscription, input.DeviceID, key, payload, indexedAt.Add(time.Duration(index)*time.Millisecond),
		); err != nil {
			t.Fatal(err)
		}
	}

	ready := manualSubscriptionTestInput(GBVersion30)
	ready.TargetID = "34020000001329999999"
	readyKey := saveManualSubscriptionTestIntent(t, api, ready, nil)
	readyPayload, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindManualSubscription, ready.DeviceID, readyKey)
	if err != nil || !exists {
		t.Fatalf("load ready subscription intent = %v, %v", exists, err)
	}
	if err := store.SaveGBTaskState(
		t.Context(), gbTaskKindManualSubscription, ready.DeviceID, readyKey, readyPayload,
		indexedAt.Add(time.Duration(manualSubscriptionRecoveryBatch)*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}

	calls := 0
	api.manualSubscribeRefresh = func(_ context.Context, input *SubscribeInput) error {
		calls++
		if input.TargetID != ready.TargetID {
			t.Fatalf("recovered blocked subscription %q", input.TargetID)
		}
		return nil
	}
	api.processManualSubscriptionRecovery(t.Context(), "", true)
	if calls != 1 {
		t.Fatalf("ready subscription recovery calls = %d, want 1", calls)
	}
}

func TestManualSubscriptionInvalidSnapshotCannotDeleteConcurrentUpdate(t *testing.T) {
	base := newManualSubscriptionTestMemory(GBVersion30)
	original := &GB28181API{svr: &Server{memoryStorer: base}}
	input := manualSubscriptionTestInput(GBVersion30)
	key := saveManualSubscriptionTestIntent(t, original, input, nil)
	payload, exists, err := base.LoadGBTaskState(t.Context(), gbTaskKindManualSubscription, input.DeviceID, key)
	if err != nil || !exists {
		t.Fatalf("load original subscription intent = %v, %v", exists, err)
	}
	var stale manualSubscriptionIntentState
	if err := json.Unmarshal(payload, &stale); err != nil {
		t.Fatal(err)
	}
	stale.Input.TargetID = "34020000001329999998"
	stalePayload, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := base.SaveGBTaskState(t.Context(), gbTaskKindManualSubscription, input.DeviceID, key, stalePayload, time.Now()); err != nil {
		t.Fatal(err)
	}

	store := &blockingManualSubscriptionListMemory{
		alarmInboxTestMemory: base,
		entered:              make(chan struct{}),
		release:              make(chan struct{}),
	}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	listed := make(chan error, 1)
	go func() {
		_, err := api.listManualSubscriptionIntentRecords(t.Context(), "", maxManualSubscriptionIntentStates)
		listed <- err
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("manual subscription listing did not reach stale snapshot barrier")
	}
	if managed, err := api.saveManualSubscriptionIntent(t.Context(), key, input, nil, testLocalGatewayID); err != nil || !managed {
		t.Fatalf("update manual subscription intent = managed:%v err:%v", managed, err)
	}
	close(store.release)
	if err := <-listed; err != nil {
		t.Fatal(err)
	}

	currentPayload, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindManualSubscription, input.DeviceID, key)
	if err != nil || !exists {
		t.Fatalf("concurrently updated subscription intent = %v, %v", exists, err)
	}
	var current manualSubscriptionIntentState
	if err := json.Unmarshal(currentPayload, &current); err != nil {
		t.Fatal(err)
	}
	if current.Input != input {
		t.Fatalf("concurrently updated subscription input = %+v, want %+v", current.Input, input)
	}
}

func TestManualSubscriptionInvalidPersistentStateIsQuarantined(t *testing.T) {
	store := newManualSubscriptionTestMemory(GBVersion30)
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	input := manualSubscriptionTestInput(GBVersion30)
	key := saveManualSubscriptionTestIntent(t, api, input, nil)
	now := time.Now()

	for _, test := range []struct {
		name   string
		mutate func(*manualSubscriptionIntentState)
	}{
		{name: "missing created time", mutate: func(state *manualSubscriptionIntentState) { state.CreatedAt = time.Time{} }},
		{name: "negative attempts", mutate: func(state *manualSubscriptionIntentState) { state.Attempts = -1 }},
		{name: "future update", mutate: func(state *manualSubscriptionIntentState) { state.UpdatedAt = now.Add(10 * time.Minute) }},
		{name: "confirmation before creation", mutate: func(state *manualSubscriptionIntentState) { state.LastConfirmedAt = state.CreatedAt.Add(-time.Second) }},
		{name: "retry before creation", mutate: func(state *manualSubscriptionIntentState) { state.NextAttemptAt = state.CreatedAt.Add(-time.Second) }},
		{name: "blocked retry contradiction", mutate: func(state *manualSubscriptionIntentState) {
			state.RetryBlocked = true
			state.TerminationReason = "rejected"
			state.NextAttemptAt = now.Add(time.Minute)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindManualSubscription, input.DeviceID, key)
			if err != nil || !found {
				t.Fatalf("load subscription intent = found:%v err:%v", found, err)
			}
			var state manualSubscriptionIntentState
			if err := json.Unmarshal(payload, &state); err != nil {
				t.Fatal(err)
			}
			test.mutate(&state)
			invalidPayload, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SaveGBTaskState(t.Context(), gbTaskKindManualSubscription, input.DeviceID, key, invalidPayload, now); err != nil {
				t.Fatal(err)
			}

			records, err := api.listManualSubscriptionIntentRecords(t.Context(), "", maxManualSubscriptionIntentStates)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 0 {
				t.Fatalf("invalid subscription records = %+v", records)
			}
			if _, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindManualSubscription, input.DeviceID, key); err != nil || found {
				t.Fatalf("invalid subscription intent survived: found=%v err=%v", found, err)
			}
			key = saveManualSubscriptionTestIntent(t, api, input, nil)
		})
	}
}

func TestManualSubscriptionCancelWhileOfflineDeletesDurableIntent(t *testing.T) {
	store := newManualSubscriptionTestMemory(GBVersion20)
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	input := manualSubscriptionTestInput(GBVersion20)
	key := saveManualSubscriptionTestIntent(t, api, input, nil)
	store.device.IsOnline = false
	cancel := input
	cancel.Cancel = true
	if err := api.Subscribe(t.Context(), &cancel); err != nil {
		t.Fatalf("cancel offline durable subscription: %v", err)
	}
	if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindManualSubscription, gb10DeviceID, key); err != nil || exists {
		t.Fatalf("cancelled manual subscription intent = %v, %v", exists, err)
	}
}

func TestManualSubscriptionPersistenceFailureRejectsBeforeSIPSend(t *testing.T) {
	base := newManualSubscriptionTestMemory(GBVersion30)
	store := &failingAlarmInboxMemory{alarmInboxTestMemory: base}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	err := api.Subscribe(t.Context(), &SubscribeInput{DeviceID: gb10DeviceID, Event: "Alarm", Expires: 60})
	if !errors.Is(err, errTaskStateSave) {
		t.Fatalf("manual subscription persistence error = %v", err)
	}
	count := 0
	api.outgoingSubscriptions.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Fatalf("failed durable subscription published %d dialogs", count)
	}
}

func TestManualSubscriptionIntentLimitRejectsBeforeSIPSend(t *testing.T) {
	base := newManualSubscriptionTestMemory(GBVersion30)
	store := &fullManualSubscriptionMemory{alarmInboxTestMemory: base}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	err := api.Subscribe(t.Context(), &SubscribeInput{DeviceID: gb10DeviceID, Event: "Alarm", Expires: 60})
	if err == nil || !strings.Contains(err.Error(), "manual subscription intent limit reached") {
		t.Fatalf("manual subscription intent limit error = %v", err)
	}
	count := 0
	api.outgoingSubscriptions.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Fatalf("subscription limit published %d dialogs", count)
	}
}

func TestOutgoingSubscriptionStatesIncludePendingDurableIntent(t *testing.T) {
	store := newManualSubscriptionTestMemory(GBVersion11)
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	input := manualSubscriptionTestInput(GBVersion11)
	saveManualSubscriptionTestIntent(t, api, input, nil)
	states, err := api.OutgoingSubscriptionStates(t.Context(), gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("subscription states = %+v", states)
	}
	state := states[0]
	if state.Status != "recovering" || !state.Persisted || state.Event != "catalog" || state.StartTime != input.StartTime {
		t.Fatalf("pending durable subscription state = %+v", state)
	}
}

func TestTerminatedNotifyPersistsManualSubscriptionRetryPolicy(t *testing.T) {
	for _, test := range []struct {
		name              string
		version           GBProtocolVersion
		state             string
		wantRetryBlocked  bool
		wantDeferredRetry bool
		wantRecoveryCalls int
	}{
		{
			name: "rejected is permanently blocked", version: GBVersion20,
			state: "terminated;reason=rejected", wantRetryBlocked: true,
		},
		{
			name: "2022 invariant is permanently blocked", version: GBVersion30,
			state: "terminated;reason=invariant", wantRetryBlocked: true,
		},
		{
			name: "probation respects retry-after", version: GBVersion20,
			state: "terminated;reason=probation;retry-after=60", wantDeferredRetry: true,
		},
		{
			name: "timeout retries immediately", version: GBVersion10,
			state: "terminated;reason=timeout", wantRecoveryCalls: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newManualSubscriptionTestMemory(test.version)
			api := &GB28181API{svr: &Server{memoryStorer: store}}
			input := manualSubscriptionTestInput(test.version)
			key := saveManualSubscriptionTestIntent(t, api, input, nil)

			connection := newFlowConnection()
			subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "manual-terminated-"+test.name, []byte("query"))
			dialog := &outgoingSubscriptionDialog{
				response: sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil),
				deviceID: input.DeviceID,
			}
			prepareOutgoingNotifyDialog(t, dialog, "presence", outgoingSubscriptionEventName(input.Event), input.TargetID)
			api.outgoingSubscriptions.Store(key, dialog)

			request := newFlowRequest(t, connection, sip.MethodNotify, "manual-terminated-"+test.name, nil)
			applyTerminatedNotifyDialog(t, request, dialog.response)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: test.state})
			if _, err := api.validateOutgoingSubscriptionNotifyModeAt(true, time.Now(), input.DeviceID, request, ""); err != nil {
				t.Fatalf("commit terminated manual subscription: %v", err)
			}

			payload, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindManualSubscription, input.DeviceID, key)
			if err != nil || !exists {
				t.Fatalf("load terminated manual subscription intent = %v, %v", exists, err)
			}
			var state manualSubscriptionIntentState
			if err := json.Unmarshal(payload, &state); err != nil {
				t.Fatal(err)
			}
			if state.RetryBlocked != test.wantRetryBlocked {
				t.Fatalf("retry_blocked = %v, want %v; state=%+v", state.RetryBlocked, test.wantRetryBlocked, state)
			}
			if test.wantDeferredRetry && !state.NextAttemptAt.After(time.Now().Add(50*time.Second)) {
				t.Fatalf("next_attempt_at = %v, want retry-after deferral", state.NextAttemptAt)
			}

			calls := 0
			api.manualSubscribeRefresh = func(context.Context, *SubscribeInput) error {
				calls++
				return nil
			}
			// 设备重新 REGISTER 会触发 force=true；它只能绕过普通离线退避，
			// 不能绕过事件源明确给出的 retry-after 或永久终止原因。
			api.processManualSubscriptionRecovery(t.Context(), input.DeviceID, true)
			if calls != test.wantRecoveryCalls {
				t.Fatalf("recovery calls = %d, want %d", calls, test.wantRecoveryCalls)
			}
		})
	}
}

func TestTerminatedManualSubscriptionPersistenceFailureDoesNotRecoverOldIntent(t *testing.T) {
	base := newManualSubscriptionTestMemory(GBVersion20)
	store := &failingManualSubscriptionTerminationMemory{alarmInboxTestMemory: base}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	input := manualSubscriptionTestInput(GBVersion20)
	key := saveManualSubscriptionTestIntent(t, api, input, nil)

	store.setFailManualSubscriptionSave(true)
	managed, err := api.persistManualSubscriptionTermination(key, input.DeviceID, subscriptionStateValue{reason: "rejected"}, false, 0, time.Now())
	if !managed || err == nil {
		t.Fatalf("persist failed termination = managed:%v err:%v; want managed with error", managed, err)
	}

	var calls int
	api.manualSubscribeRefresh = func(context.Context, *SubscribeInput) error {
		calls++
		return nil
	}
	api.processManualSubscriptionRecovery(t.Context(), input.DeviceID, true)
	if calls != 0 {
		t.Fatalf("recovery calls while termination persistence is pending = %d, want 0", calls)
	}

	store.setFailManualSubscriptionSave(false)
	api.processManualSubscriptionRecovery(t.Context(), input.DeviceID, true)
	if calls != 0 {
		t.Fatalf("recovery calls after termination persistence retry = %d, want 0", calls)
	}
	payload, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindManualSubscription, input.DeviceID, key)
	if err != nil || !exists {
		t.Fatalf("load persisted blocked intent = %v, %v", exists, err)
	}
	var state manualSubscriptionIntentState
	if err := json.Unmarshal(payload, &state); err != nil {
		t.Fatal(err)
	}
	if !state.RetryBlocked || state.TerminationReason != "rejected" {
		t.Fatalf("persisted termination state = %+v; want blocked rejected", state)
	}
}
