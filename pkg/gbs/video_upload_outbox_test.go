package gbs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

type videoUploadOutboxTestMemory struct {
	*alarmInboxTestMemory
}

type failingVideoUploadOutboxMemory struct {
	*videoUploadOutboxTestMemory
}

type failingVideoUploadReceiptMemory struct {
	*videoUploadOutboxTestMemory
	failures atomic.Int32
}

type blockingInvalidVideoUploadReceiptDeleteMemory struct {
	*videoUploadOutboxTestMemory
	deleteStarted chan struct{}
	allowDelete   chan struct{}
	deleteCalls   atomic.Int32
}

func (m *blockingInvalidVideoUploadReceiptDeleteMemory) DeleteGBTaskState(
	ctx context.Context,
	kind, deviceID, sessionID string,
) error {
	if kind == gbTaskKindVideoUploadReceipt && m.deleteCalls.Add(1) == 1 {
		close(m.deleteStarted)
		select {
		case <-m.allowDelete:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return m.videoUploadOutboxTestMemory.DeleteGBTaskState(ctx, kind, deviceID, sessionID)
}

type videoUploadStoreWithoutLister struct {
	*persistentTaskMemory
}

func (m *videoUploadStoreWithoutLister) GetChannel(deviceID, channelID string) (*Channel, bool) {
	if deviceID != gb10DeviceID || channelID != testCascadeChannelID {
		return nil, false
	}
	return &Channel{ChannelID: channelID, device: m.device}, true
}

func (m *failingVideoUploadOutboxMemory) SaveGBTaskState(context.Context, string, string, string, []byte, time.Time) error {
	return errTaskStateSave
}

func (m *failingVideoUploadReceiptMemory) SaveGBTaskState(ctx context.Context, kind, deviceID, sessionID string, payload []byte, updatedAt time.Time) error {
	if kind == gbTaskKindVideoUploadReceipt && m.failures.Add(-1) >= 0 {
		return errTaskStateSave
	}
	return m.videoUploadOutboxTestMemory.SaveGBTaskState(ctx, kind, deviceID, sessionID, payload, updatedAt)
}

func newVideoUploadOutboxTestMemory() *videoUploadOutboxTestMemory {
	return &videoUploadOutboxTestMemory{alarmInboxTestMemory: &alarmInboxTestMemory{
		persistentTaskMemory: newPersistentTaskMemory(GBVersion30),
		updatedAt:            make(map[string]time.Time),
	}}
}

func (m *videoUploadOutboxTestMemory) GetChannel(deviceID, channelID string) (*Channel, bool) {
	if deviceID != gb10DeviceID || channelID != testCascadeChannelID {
		return nil, false
	}
	return &Channel{ChannelID: channelID, device: m.device}, true
}

func newVideoUploadOutboxTestAPI(t *testing.T, store MemoryStorer) (*GB28181API, *CascadeManager, *atomic.Int32) {
	t.Helper()
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	worker.effective = GBVersion30
	worker.updateStatus(func(status *CascadePlatformStatus) { status.Registered = true })
	calls := new(atomic.Int32)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		calls.Add(1)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	manager := NewCascadeManager(nil)
	manager.items[platform.name] = worker
	server := &Server{memoryStorer: store, cascade: manager}
	api := &GB28181API{svr: server, videoUploadOutboxWake: make(chan struct{}, 1)}
	server.gb = api
	return api, manager, calls
}

func videoUploadOutboxTestBody() []byte {
	return []byte(`<Notify><CmdType>VideoUploadNotify</CmdType><SN>903</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><Time>2026-09-01T08:00:00</Time><Longitude>120.12</Longitude><Latitude>30.28</Latitude></Notify>`)
}

func TestVideoUploadNotifyDurableOutboxRecoversAfterSuccessfulSIPOK(t *testing.T) {
	store := newVideoUploadOutboxTestMemory()
	api, manager, calls := newVideoUploadOutboxTestAPI(t, store)
	body := videoUploadOutboxTestBody()
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "video-upload-outbox-success", body, api.sipMessageVideoUploadNotify)
	assertFlowOK(t, response)

	outboxID := videoUploadOutboxID(gb10DeviceID, body)
	if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindVideoUploadOutbox, gb10DeviceID, outboxID); err != nil || !exists {
		t.Fatalf("VideoUploadNotify outbox after SIP 200 = %v, %v", exists, err)
	}
	if calls.Load() != 0 {
		t.Fatal("VideoUploadNotify forwarded without the outbox worker")
	}

	restartedServer := &Server{memoryStorer: store, cascade: manager}
	restarted := &GB28181API{svr: restartedServer}
	restartedServer.gb = restarted
	restarted.processVideoUploadOutboxBatch(time.Now())
	if got := calls.Load(); got != 1 {
		t.Fatalf("recovered VideoUploadNotify forwards = %d, want 1", got)
	}
	if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindVideoUploadOutbox, gb10DeviceID, outboxID); err != nil || exists {
		t.Fatalf("completed VideoUploadNotify outbox = %v, %v", exists, err)
	}
	restarted.processVideoUploadOutboxBatch(time.Now())
	if got := calls.Load(); got != 1 {
		t.Fatalf("completed VideoUploadNotify was forwarded again: %d", got)
	}
}

func TestVideoUploadNotifyDuplicateBeforeWorkerCreatesOneOutbox(t *testing.T) {
	store := newVideoUploadOutboxTestMemory()
	api, _, calls := newVideoUploadOutboxTestAPI(t, store)
	body := videoUploadOutboxTestBody()
	for _, callID := range []string{"video-upload-outbox-duplicate-first", "video-upload-outbox-duplicate-second"} {
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, callID, body, api.sipMessageVideoUploadNotify)
		assertFlowOK(t, response)
	}
	outboxID := videoUploadOutboxID(gb10DeviceID, body)
	if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindVideoUploadOutbox, gb10DeviceID, outboxID); err != nil || !exists {
		t.Fatalf("deduplicated VideoUploadNotify outbox = %v, %v", exists, err)
	}
	api.processVideoUploadOutboxBatch(time.Now())
	if got := calls.Load(); got != 1 {
		t.Fatalf("duplicate VideoUploadNotify forwards = %d, want 1", got)
	}
}

func TestVideoUploadNotifySIPWriteFailureKeepsDurableOutbox(t *testing.T) {
	store := newVideoUploadOutboxTestMemory()
	api, manager, calls := newVideoUploadOutboxTestAPI(t, store)
	body := videoUploadOutboxTestBody()
	writeErr := errors.New("response write failed")
	conn, done := startBlockingFlowHandler(t, api, sip.MethodMessage, "video-upload-outbox-write-failure", body, api.sipMessageVideoUploadNotify, writeErr)
	outboxID := videoUploadOutboxID(gb10DeviceID, body)
	if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindVideoUploadOutbox, gb10DeviceID, outboxID); err != nil || !exists {
		t.Fatalf("VideoUploadNotify outbox before SIP response completion = %v, %v", exists, err)
	}
	finishBlockingFlowHandler(t, conn, done)
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.VideoUpload != nil {
		t.Fatalf("failed SIP response committed VideoUploadNotify query state: %+v", state.VideoUpload)
	}

	restartedServer := &Server{memoryStorer: store, cascade: manager}
	restarted := &GB28181API{svr: restartedServer}
	restartedServer.gb = restarted
	restarted.processVideoUploadOutboxBatch(time.Now())
	if got := calls.Load(); got != 1 {
		t.Fatalf("uncertain SIP response recovered forwards = %d, want 1", got)
	}
}

func TestVideoUploadNotifyOutboxSaveFailureRejectsBeforeSIPOK(t *testing.T) {
	store := &failingVideoUploadOutboxMemory{videoUploadOutboxTestMemory: newVideoUploadOutboxTestMemory()}
	api, _, calls := newVideoUploadOutboxTestAPI(t, store)
	body := videoUploadOutboxTestBody()
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "video-upload-outbox-save-failure", body, api.sipMessageVideoUploadNotify)
	if !strings.Contains(response, "SIP/2.0 503") {
		t.Fatalf("VideoUploadNotify outbox save failure response = %s", response)
	}
	if calls.Load() != 0 {
		t.Fatal("VideoUploadNotify was forwarded after outbox persistence failure")
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.VideoUpload != nil {
		t.Fatalf("outbox persistence failure committed query state: %+v", state.VideoUpload)
	}
}

func TestVideoUploadNotifyStoreWithoutListerKeepsCompatibilityPath(t *testing.T) {
	store := &videoUploadStoreWithoutLister{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	api, _, calls := newVideoUploadOutboxTestAPI(t, store)
	body := videoUploadOutboxTestBody()
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "video-upload-outbox-no-lister", body, api.sipMessageVideoUploadNotify)
	assertFlowOK(t, response)
	api.lifecycleWG.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("non-listing task store compatibility forwards = %d, want 1", got)
	}
	if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindVideoUploadOutbox, gb10DeviceID, videoUploadOutboxID(gb10DeviceID, body)); err != nil || exists {
		t.Fatalf("non-listing task store outbox = %v, %v", exists, err)
	}
}

func TestVideoUploadNotifyOutboxRejectsRecreatedDeviceGeneration(t *testing.T) {
	store := newVideoUploadOutboxTestMemory()
	registeredAt := time.Now().Add(-time.Minute).Round(time.Millisecond)
	store.device.LastRegisterAt = registeredAt
	store.device.Expires = 3600
	api, _, calls := newVideoUploadOutboxTestAPI(t, store)
	_, binding, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	body := videoUploadOutboxTestBody()
	outboxID, queued, err := api.persistVideoUploadOutbox(t.Context(), gb10DeviceID, body, binding, true)
	if err != nil || !queued {
		t.Fatalf("persist VideoUploadNotify outbox = %v, %v", queued, err)
	}
	store.device = &Device{
		IsOnline:       true,
		gbVersion:      string(GBVersion30),
		LastRegisterAt: registeredAt.Add(time.Second),
		Expires:        3600,
	}
	api.processVideoUploadOutboxBatch(time.Now())
	if calls.Load() != 0 {
		t.Fatal("old-generation VideoUploadNotify outbox was forwarded")
	}
	if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindVideoUploadOutbox, gb10DeviceID, outboxID); err != nil || exists {
		t.Fatalf("stale VideoUploadNotify outbox = %v, %v", exists, err)
	}
}

func TestVideoUploadNotifyOutboxWaitsForConfiguredUpstreamToRegister(t *testing.T) {
	store := newVideoUploadOutboxTestMemory()
	api, manager, calls := newVideoUploadOutboxTestAPI(t, store)
	worker, ok := manager.workerByName(testSharedCascadePlatform(t).name)
	if !ok {
		t.Fatal("configured cascade worker is missing")
	}
	worker.updateStatus(func(status *CascadePlatformStatus) { status.Registered = false })
	body := videoUploadOutboxTestBody()
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "video-upload-outbox-waits-register", body, api.sipMessageVideoUploadNotify)
	assertFlowOK(t, response)
	outboxID := videoUploadOutboxID(gb10DeviceID, body)
	if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindVideoUploadOutbox, gb10DeviceID, outboxID); err != nil || !exists {
		t.Fatalf("offline configured upstream outbox = %v, %v", exists, err)
	}
	api.processVideoUploadOutboxBatch(time.Now())
	if calls.Load() != 0 {
		t.Fatal("offline upstream received VideoUploadNotify")
	}
	worker.updateStatus(func(status *CascadePlatformStatus) {
		status.Registered = true
		status.ExpiresAt = time.Now().Add(time.Hour)
	})
	api.processVideoUploadOutboxBatch(time.Now().Add(2 * time.Second))
	if calls.Load() != 1 {
		t.Fatalf("registered upstream forwards = %d, want 1", calls.Load())
	}
	if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindVideoUploadOutbox, gb10DeviceID, outboxID); err != nil || exists {
		t.Fatalf("delivered offline-upstream outbox = %v, %v", exists, err)
	}
}

func TestVideoUploadNotifyReceiptPersistenceRetryDoesNotResendUpstream(t *testing.T) {
	store := &failingVideoUploadReceiptMemory{videoUploadOutboxTestMemory: newVideoUploadOutboxTestMemory()}
	store.failures.Store(1)
	api, _, calls := newVideoUploadOutboxTestAPI(t, store)
	body := videoUploadOutboxTestBody()
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "video-upload-receipt-retry", body, api.sipMessageVideoUploadNotify)
	assertFlowOK(t, response)

	outboxID := videoUploadOutboxID(gb10DeviceID, body)
	api.processVideoUploadOutboxBatch(time.Now())
	if calls.Load() != 1 {
		t.Fatalf("initial VideoUploadNotify forwards = %d, want 1", calls.Load())
	}
	if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindVideoUploadOutbox, gb10DeviceID, outboxID); err != nil || !exists {
		t.Fatalf("outbox after receipt persistence failure = %v, %v", exists, err)
	}

	api.processVideoUploadOutboxBatch(time.Now().Add(2 * time.Second))
	if calls.Load() != 1 {
		t.Fatalf("receipt persistence retry resent VideoUploadNotify: %d", calls.Load())
	}
	platform := testSharedCascadePlatform(t)
	receiptID := videoUploadReceiptID(gb10DeviceID, platform.name, body)
	if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindVideoUploadReceipt, gb10DeviceID, receiptID); err != nil || !exists {
		t.Fatalf("retried VideoUploadNotify receipt = %v, %v", exists, err)
	}
	if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindVideoUploadOutbox, gb10DeviceID, outboxID); err != nil || exists {
		t.Fatalf("completed outbox after receipt retry = %v, %v", exists, err)
	}
}

func TestVideoUploadNotifyDeferredBatchDoesNotStarveReadyOutbox(t *testing.T) {
	store := newVideoUploadOutboxTestMemory()
	api, _, calls := newVideoUploadOutboxTestAPI(t, store)
	base := time.Now().Round(time.Millisecond)
	nextAttemptAt := base.Add(time.Minute)

	for index := 0; index < videoUploadOutboxBatchSize; index++ {
		body := []byte(fmt.Sprintf(
			`<Notify><CmdType>VideoUploadNotify</CmdType><SN>%d</SN><DeviceID>%s</DeviceID><Time>2026-09-01T08:00:00</Time></Notify>`,
			index+1,
			testCascadeChannelID,
		))
		state := videoUploadOutboxState{
			SourceDeviceID: gb10DeviceID,
			Body:           body,
			Platforms:      []string{testSharedCascadePlatform(t).name},
			ReceivedAt:     base,
			Attempt:        6,
			NextAttemptAt:  nextAttemptAt,
		}
		payload, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveGBTaskState(
			t.Context(),
			gbTaskKindVideoUploadOutbox,
			gb10DeviceID,
			videoUploadOutboxID(gb10DeviceID, body),
			payload,
			base.Add(time.Duration(index)*time.Nanosecond),
		); err != nil {
			t.Fatal(err)
		}
	}

	readyBody := []byte(`<Notify><CmdType>VideoUploadNotify</CmdType><SN>1001</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><Time>2026-09-01T08:00:01</Time></Notify>`)
	readyState := videoUploadOutboxState{
		SourceDeviceID: gb10DeviceID,
		Body:           readyBody,
		Platforms:      []string{testSharedCascadePlatform(t).name},
		ReceivedAt:     base.Add(time.Second),
	}
	readyPayload, err := json.Marshal(readyState)
	if err != nil {
		t.Fatal(err)
	}
	readyOutboxID := videoUploadOutboxID(gb10DeviceID, readyBody)
	if err := store.SaveGBTaskState(
		t.Context(), gbTaskKindVideoUploadOutbox, gb10DeviceID, readyOutboxID, readyPayload, base.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}

	// 同一批把升级前按尝试时间索引的延迟记录重排到 NextAttemptAt，
	// 并越过它们立即处理已经到期的新通知。
	api.processVideoUploadOutboxBatch(base.Add(2 * time.Second))
	if got := calls.Load(); got != 1 {
		t.Fatalf("ready VideoUploadNotify forwards behind deferred batch = %d, want 1", got)
	}
	if _, exists, err := store.LoadGBTaskState(t.Context(), gbTaskKindVideoUploadOutbox, gb10DeviceID, readyOutboxID); err != nil || exists {
		t.Fatalf("ready VideoUploadNotify outbox after batch = %v, %v", exists, err)
	}
	records, err := store.ListGBTaskStates(t.Context(), gbTaskKindVideoUploadOutbox, videoUploadOutboxBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != videoUploadOutboxBatchSize {
		t.Fatalf("deferred VideoUploadNotify outbox count = %d, want %d", len(records), videoUploadOutboxBatchSize)
	}
	for _, record := range records {
		if !record.UpdatedAt.Time.Equal(nextAttemptAt) {
			t.Fatalf("deferred VideoUploadNotify index = %v, want %v", record.UpdatedAt.Time, nextAttemptAt)
		}
	}
}

func TestVideoUploadNotifyOutboxQuarantinesInvalidPersistentIdentityBeforeDelivery(t *testing.T) {
	now := time.Now().Round(time.Millisecond)
	body := videoUploadOutboxTestBody()
	platformName := testSharedCascadePlatform(t).name
	base := videoUploadOutboxState{
		SourceDeviceID: gb10DeviceID,
		Body:           body,
		Platforms:      []string{platformName},
		ReceivedAt:     now,
	}
	tests := []struct {
		name     string
		deviceID string
		outboxID string
		mutate   func(*videoUploadOutboxState)
	}{
		{
			name: "source device mismatch", deviceID: gb10DeviceID, outboxID: videoUploadOutboxID(gb10DeviceID, body),
			mutate: func(state *videoUploadOutboxState) { state.SourceDeviceID = "34020000001320009999" },
		},
		{
			name: "payload key mismatch", deviceID: gb10DeviceID, outboxID: strings.Repeat("0", 64),
		},
		{
			name: "future retry", deviceID: gb10DeviceID, outboxID: videoUploadOutboxID(gb10DeviceID, body),
			mutate: func(state *videoUploadOutboxState) {
				state.Attempt = 1
				state.NextAttemptAt = now.Add(6 * time.Minute)
			},
		},
		{
			name: "duplicate platform", deviceID: gb10DeviceID, outboxID: videoUploadOutboxID(gb10DeviceID, body),
			mutate: func(state *videoUploadOutboxState) { state.Platforms = []string{platformName, platformName} },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newVideoUploadOutboxTestMemory()
			api, _, calls := newVideoUploadOutboxTestAPI(t, store)
			state := base
			state.Body = append([]byte(nil), base.Body...)
			state.Platforms = append([]string(nil), base.Platforms...)
			if test.mutate != nil {
				test.mutate(&state)
			}
			payload, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SaveGBTaskState(t.Context(), gbTaskKindVideoUploadOutbox, test.deviceID, test.outboxID, payload, now); err != nil {
				t.Fatal(err)
			}
			if result := api.processVideoUploadOutbox(test.deviceID, test.outboxID, now, now); result != videoUploadOutboxProcessMaintenance {
				t.Fatalf("invalid outbox result = %v", result)
			}
			if calls.Load() != 0 {
				t.Fatalf("invalid outbox was delivered %d times", calls.Load())
			}
			if _, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindVideoUploadOutbox, test.deviceID, test.outboxID); err != nil || found {
				t.Fatalf("invalid outbox was not quarantined: found %v, err %v", found, err)
			}
		})
	}
}

func TestVideoUploadNotifyCorruptReceiptDoesNotSuppressDelivery(t *testing.T) {
	store := newVideoUploadOutboxTestMemory()
	api, _, calls := newVideoUploadOutboxTestAPI(t, store)
	body := videoUploadOutboxTestBody()
	platformName := testSharedCascadePlatform(t).name
	outboxID := videoUploadOutboxID(gb10DeviceID, body)
	receiptID := videoUploadReceiptID(gb10DeviceID, platformName, body)
	now := time.Now()
	state := videoUploadOutboxState{
		SourceDeviceID: gb10DeviceID,
		Body:           body,
		Platforms:      []string{platformName},
		ReceivedAt:     now,
	}
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindVideoUploadOutbox, gb10DeviceID, outboxID, payload, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindVideoUploadReceipt, gb10DeviceID, receiptID, []byte(`{"completed":false}`), now); err != nil {
		t.Fatal(err)
	}

	if result := api.processVideoUploadOutbox(gb10DeviceID, outboxID, now, now); result != videoUploadOutboxProcessDelivery {
		t.Fatalf("outbox result = %v", result)
	}
	if calls.Load() != 1 {
		t.Fatalf("corrupt receipt suppressed delivery; calls = %d", calls.Load())
	}
	receipt, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindVideoUploadReceipt, gb10DeviceID, receiptID)
	if err != nil || !found || !strings.Contains(string(receipt), `"completed":true`) {
		t.Fatalf("repaired receipt = %s, found %v, err %v", receipt, found, err)
	}
}

func TestVideoUploadNotifyInvalidReceiptCannotDeleteConcurrentCompletion(t *testing.T) {
	store := &blockingInvalidVideoUploadReceiptDeleteMemory{
		videoUploadOutboxTestMemory: newVideoUploadOutboxTestMemory(),
		deleteStarted:               make(chan struct{}),
		allowDelete:                 make(chan struct{}),
	}
	api, manager, calls := newVideoUploadOutboxTestAPI(t, store)
	body := videoUploadOutboxTestBody()
	worker, ok := manager.workerByName(testSharedCascadePlatform(t).name)
	if !ok {
		t.Fatal("test cascade worker is unavailable")
	}
	receiptID := videoUploadReceiptID(gb10DeviceID, worker.platform.name, body)
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindVideoUploadReceipt, gb10DeviceID, receiptID, []byte(`{"completed":false}`), time.Now()); err != nil {
		t.Fatal(err)
	}

	persistDone := make(chan error, 1)
	go func() {
		_, _, err := api.persistVideoUploadOutbox(t.Context(), gb10DeviceID, body, inboundRegistrationBinding{}, false)
		persistDone <- err
	}()
	select {
	case <-store.deleteStarted:
	case <-time.After(time.Second):
		t.Fatal("invalid receipt deletion did not start")
	}

	forwardDone := make(chan error, 1)
	go func() {
		_, err := api.forwardCascadeVideoUploadNotifyToWorker(t.Context(), gb10DeviceID, body, worker)
		forwardDone <- err
	}()
	forwardFinished := false
	select {
	case err := <-forwardDone:
		if err != nil {
			t.Fatalf("concurrent VideoUploadNotify delivery: %v", err)
		}
		forwardFinished = true
	case <-time.After(100 * time.Millisecond):
		// 同键锁正确时，实际投递必须等待旧坏回执完成隔离。
	}
	close(store.allowDelete)
	select {
	case err := <-persistDone:
		if err != nil {
			t.Fatalf("persist VideoUploadNotify outbox: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("VideoUploadNotify outbox persistence timed out")
	}
	if !forwardFinished {
		select {
		case err := <-forwardDone:
			if err != nil {
				t.Fatalf("VideoUploadNotify delivery after receipt quarantine: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("VideoUploadNotify delivery timed out after receipt quarantine")
		}
	}

	api.processVideoUploadOutboxBatch(time.Now())
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent valid receipt was deleted; deliveries = %d, want 1", got)
	}
	payload, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindVideoUploadReceipt, gb10DeviceID, receiptID)
	if err != nil || !found || !strings.Contains(string(payload), `"completed":true`) {
		t.Fatalf("completed receipt = %s, found=%v err=%v", payload, found, err)
	}
}
