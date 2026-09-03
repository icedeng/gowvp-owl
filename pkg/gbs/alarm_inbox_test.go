package gbs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
)

type alarmInboxTestMemory struct {
	*persistentTaskMemory
	updatedAt map[string]time.Time
}

func (m *alarmInboxTestMemory) GetChannel(deviceID, channelID string) (*Channel, bool) {
	if deviceID != gb10DeviceID || channelID != gb10ChannelID {
		return nil, false
	}
	return &Channel{ChannelID: channelID, device: m.device}, true
}

type failingAlarmInboxMemory struct {
	*alarmInboxTestMemory
}

func (m *failingAlarmInboxMemory) SaveGBTaskState(context.Context, string, string, string, []byte, time.Time) error {
	return errTaskStateSave
}

type failAlarmBusinessCommitOnceMemory struct {
	*alarmInboxTestMemory
	inboxSaves atomic.Int32
	failures   atomic.Int32
}

func (m *failAlarmBusinessCommitOnceMemory) SaveGBTaskState(
	ctx context.Context,
	kind, deviceID, sessionID string,
	payload []byte,
	updatedAt time.Time,
) error {
	if kind == gbTaskKindAlarmInbox && m.inboxSaves.Add(1) == 2 {
		m.failures.Add(1)
		return errTaskStateSave
	}
	if kind == gbTaskKindAlarmReceipt && m.failures.Load() == 1 {
		m.failures.Add(1)
		return errTaskStateSave
	}
	return m.alarmInboxTestMemory.SaveGBTaskState(ctx, kind, deviceID, sessionID, payload, updatedAt)
}

func (m *alarmInboxTestMemory) SaveGBTaskState(ctx context.Context, kind, deviceID, sessionID string, payload []byte, updatedAt time.Time) error {
	if err := m.persistentTaskMemory.SaveGBTaskState(ctx, kind, deviceID, sessionID, payload, updatedAt); err != nil {
		return err
	}
	m.mu.Lock()
	if m.updatedAt == nil {
		m.updatedAt = make(map[string]time.Time)
	}
	m.updatedAt[persistentTaskKey(kind, deviceID, sessionID)] = updatedAt
	m.mu.Unlock()
	return nil
}

func (m *alarmInboxTestMemory) ListGBTaskStates(ctx context.Context, kind string, limit int) ([]ipc.GBTaskStateRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	records := make([]ipc.GBTaskStateRecord, 0)
	for key, payload := range m.records {
		parts := strings.Split(key, "\x00")
		if len(parts) != 3 || parts[0] != kind {
			continue
		}
		records = append(records, ipc.GBTaskStateRecord{
			Kind: parts[0], DeviceID: parts[1], SessionID: parts[2], Payload: string(payload),
			UpdatedAt: orm.Time{Time: m.updatedAt[key]},
		})
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].UpdatedAt.Time.Equal(records[j].UpdatedAt.Time) {
			return records[i].SessionID < records[j].SessionID
		}
		return records[i].UpdatedAt.Time.Before(records[j].UpdatedAt.Time)
	})
	if limit > 0 && len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}

func waitForAlarmReceipt(t *testing.T, store *alarmInboxTestMemory, deviceID, deliveryID string) []byte {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		inbox, inboxOK, inboxErr := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmInbox, deviceID, deliveryID)
		receipt, receiptOK, receiptErr := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmReceipt, deviceID, deliveryID)
		if inboxErr != nil || receiptErr != nil {
			t.Fatalf("load Alarm completion state: inbox_err=%v receipt_err=%v", inboxErr, receiptErr)
		}
		if !inboxOK && receiptOK {
			return receipt
		}
		if time.Now().After(deadline) {
			t.Fatalf("Alarm completion did not settle: inbox_ok=%v inbox=%q receipt_ok=%v", inboxOK, inbox, receiptOK)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAlarmInboxPersistenceFailureRejectsBeforeSIPSuccess(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	t.Cleanup(func() { config = previousConfig })

	store := &failingAlarmInboxMemory{alarmInboxTestMemory: &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion10)}}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	var calls atomic.Int32
	api.SetReliableAlarmHandler(func(context.Context, *AlarmEvent) error {
		calls.Add(1)
		return nil
	})

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "alarm-inbox-save-failure", readGB10Fixture(t, "alarm-notify.xml"), api.sipNotifyAlarm)
	if !strings.Contains(response, "SIP/2.0 500") {
		t.Fatalf("Alarm persistence failure response = %s", response)
	}
	if calls.Load() != 0 {
		t.Fatalf("Alarm callback ran %d times before durable acceptance", calls.Load())
	}
}

func TestAlarmInboxRetriesAcrossServiceRestart(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	t.Cleanup(func() { config = previousConfig })

	store := &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion10)}
	first := &GB28181API{svr: &Server{memoryStorer: store}}
	callbackErr := errors.New("event database temporarily unavailable")
	var firstCalls atomic.Int32
	first.SetReliableAlarmHandler(func(context.Context, *AlarmEvent) error {
		firstCalls.Add(1)
		return callbackErr
	})

	response := runFlowHandler(t, newFlowConnection(), first, sip.MethodNotify, "alarm-inbox-restart", readGB10Fixture(t, "alarm-notify.xml"), first.sipNotifyAlarm)
	assertFlowOK(t, response)
	if firstCalls.Load() != 1 {
		t.Fatalf("initial Alarm callback calls = %d", firstCalls.Load())
	}

	store.mu.Lock()
	var deliveryID string
	for key, payload := range store.records {
		parts := strings.Split(key, "\x00")
		if len(parts) != 3 || parts[0] != gbTaskKindAlarmInbox {
			continue
		}
		deliveryID = parts[2]
		var state alarmInboxState
		if err := json.Unmarshal(payload, &state); err != nil {
			store.mu.Unlock()
			t.Fatal(err)
		}
		state.NextAttemptAt = time.Time{}
		updated, err := json.Marshal(state)
		if err != nil {
			store.mu.Unlock()
			t.Fatal(err)
		}
		store.records[key] = updated
	}
	store.mu.Unlock()
	if deliveryID == "" {
		t.Fatal("failed Alarm callback was not retained in durable inbox")
	}

	restarted := &GB28181API{
		svr:           &Server{memoryStorer: store},
		lifecycleDone: make(chan struct{}),
	}
	processed := make(chan *AlarmEvent, 1)
	restarted.SetReliableAlarmHandler(func(_ context.Context, event *AlarmEvent) error {
		processed <- event
		return nil
	})
	t.Cleanup(func() {
		restarted.beginClose()
		restarted.lifecycleWG.Wait()
	})

	select {
	case event := <-processed:
		if event.DeliveryID != deliveryID || event.DeviceID != gb10DeviceID {
			t.Fatalf("replayed Alarm = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("persisted Alarm was not replayed after service restart")
	}
	receiptPayload := waitForAlarmReceipt(t, store, gb10DeviceID, deliveryID)
	var receipt alarmReceiptState
	if err := json.Unmarshal(receiptPayload, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.CompletedAt.IsZero() {
		t.Fatal("processed Alarm receipt has no completion time")
	}
}

func TestAlarmBusinessCommitSaveFailureStillCompletesCallbackWithoutDuplicateSideEffects(t *testing.T) {
	store := &failAlarmBusinessCommitOnceMemory{
		alarmInboxTestMemory: &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion10)},
	}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	var callbackCalls atomic.Int32
	api.SetReliableAlarmHandler(func(context.Context, *AlarmEvent) error {
		callbackCalls.Add(1)
		return nil
	})
	deliveryID := "alarm-business-commit-save-failure"
	event := &AlarmEvent{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, DeliveryID: deliveryID, SourceMethod: sip.MethodNotify}
	if persisted, err := api.persistAlarmInbox(t.Context(), event); err != nil || !persisted {
		t.Fatalf("persist Alarm inbox = persisted=%v err=%v", persisted, err)
	}
	var businessCalls atomic.Int32
	ran, completed, err := api.commitAlarmBusinessOnce(t.Context(), gb10DeviceID, deliveryID, func() error {
		businessCalls.Add(1)
		return nil
	})
	if !ran || !completed || !errors.Is(err, errTaskStateSave) {
		t.Fatalf("commit Alarm business = ran=%v completed=%v err=%v", ran, completed, err)
	}
	api.processCommittedAlarmInboxDelivery(gb10DeviceID, deliveryID, time.Now())
	if got := store.failures.Load(); got != 2 {
		t.Fatalf("injected Alarm persistence failures = %d, want 2", got)
	}
	payload, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmInbox, gb10DeviceID, deliveryID)
	if err != nil || !ok {
		t.Fatalf("Alarm inbox after receipt failure = ok=%v err=%v", ok, err)
	}
	var pending alarmInboxState
	if err := json.Unmarshal(payload, &pending); err != nil {
		t.Fatal(err)
	}
	if pending.BusinessCommittedAt.IsZero() || pending.NextAttemptAt.IsZero() {
		t.Fatalf("Alarm retry state lost committed business fact: %+v", pending)
	}
	api.processAlarmInboxDeliveryAt(gb10DeviceID, deliveryID, pending.NextAttemptAt, time.Time{}, pending.NextAttemptAt)
	waitForAlarmReceipt(t, store.alarmInboxTestMemory, gb10DeviceID, deliveryID)

	if persisted, err := api.persistAlarmInbox(t.Context(), event); err != nil || !persisted {
		t.Fatalf("persist duplicate Alarm inbox = persisted=%v err=%v", persisted, err)
	}
	ran, completed, err = api.commitAlarmBusinessOnce(t.Context(), gb10DeviceID, deliveryID, func() error {
		businessCalls.Add(1)
		return nil
	})
	if ran || !completed || err != nil {
		t.Fatalf("commit duplicate Alarm business = ran=%v completed=%v err=%v", ran, completed, err)
	}
	if got := callbackCalls.Load(); got != 2 {
		t.Fatalf("Alarm at-least-once callback calls = %d, want 2", got)
	}
	if got := businessCalls.Load(); got != 1 {
		t.Fatalf("Alarm non-callback side effects = %d, want 1", got)
	}
}

func TestAlarmInboxBatchSkipsDeferredUncommittedAndCorruptHeadRecords(t *testing.T) {
	store := &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion10)}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	now := time.Date(2026, time.September, 1, 4, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	const blockedRecords = alarmInboxBatchSize
	var corruptDeliveryID string
	var uncommittedDeliveryID string

	for index := 0; index < blockedRecords; index++ {
		deliveryID := fmt.Sprintf("blocked-%03d", index)
		updatedAt := now.Add(-2*time.Hour + time.Duration(index)*time.Millisecond)
		var payload []byte
		var err error
		switch {
		case index < 49:
			state := alarmInboxState{
				Event:               AlarmEvent{DeviceID: gb10DeviceID, DeliveryID: deliveryID},
				ReceivedAt:          updatedAt,
				BusinessCommittedAt: updatedAt,
				NextAttemptAt:       future,
			}
			payload, err = json.Marshal(state)
		case index < 99:
			state := alarmInboxState{
				Event:      AlarmEvent{DeviceID: gb10DeviceID, DeliveryID: deliveryID},
				ReceivedAt: updatedAt,
			}
			payload, err = json.Marshal(state)
			if uncommittedDeliveryID == "" {
				uncommittedDeliveryID = deliveryID
			}
		default:
			payload = []byte(`{"event":`)
			corruptDeliveryID = deliveryID
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveGBTaskState(t.Context(), gbTaskKindAlarmInbox, gb10DeviceID, deliveryID, payload, updatedAt); err != nil {
			t.Fatal(err)
		}
	}

	dueDeliveryID := "due-behind-blocked-head"
	dueState := alarmInboxState{
		Event: AlarmEvent{
			DeviceID:   gb10DeviceID,
			DeliveryID: dueDeliveryID,
		},
		ReceivedAt:          now.Add(-time.Hour),
		BusinessCommittedAt: now.Add(-time.Hour),
	}
	duePayload, err := json.Marshal(dueState)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindAlarmInbox, gb10DeviceID, dueDeliveryID, duePayload, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	api.alarmHandler = func(_ context.Context, event *AlarmEvent) error {
		if event.DeliveryID != dueDeliveryID {
			t.Fatalf("unexpected Alarm delivery = %+v", event)
		}
		calls.Add(1)
		return nil
	}
	api.processAlarmInboxBatch(now)

	if got := calls.Load(); got != 1 {
		t.Fatalf("due Alarm callback calls = %d, want 1", got)
	}
	if _, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmReceipt, gb10DeviceID, dueDeliveryID); err != nil || !ok {
		t.Fatalf("due Alarm receipt = ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmInbox, gb10DeviceID, corruptDeliveryID); err != nil || ok {
		t.Fatalf("corrupt Alarm inbox survived quarantine: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmDeadLetter, gb10DeviceID, corruptDeliveryID); err != nil || !ok {
		t.Fatalf("corrupt Alarm dead letter = ok=%v err=%v", ok, err)
	}
	uncommittedPayload, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmInbox, gb10DeviceID, uncommittedDeliveryID)
	if err != nil || !ok {
		t.Fatalf("uncommitted Alarm inbox = ok=%v err=%v", ok, err)
	}
	var uncommitted alarmInboxState
	if err := json.Unmarshal(uncommittedPayload, &uncommitted); err != nil {
		t.Fatal(err)
	}
	if want := uncommitted.ReceivedAt.Add(alarmInboxRetention); !uncommitted.NextAttemptAt.Equal(want) {
		t.Fatalf("uncommitted Alarm next attempt = %v, want %v", uncommitted.NextAttemptAt, want)
	}
}

func TestAlarmDuplicateTransactionsCommitBusinessOnce(t *testing.T) {
	var notifyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		notifyCalls.Add(1)
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("OK"))
	}))
	t.Cleanup(server.Close)
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{NotifyMethodAlarm: server.URL}}
	t.Cleanup(func() { config = previousConfig })

	store := &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion10)}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	var callbackCalls atomic.Int32
	api.SetReliableAlarmHandler(func(context.Context, *AlarmEvent) error {
		callbackCalls.Add(1)
		return nil
	})
	body := readGB10Fixture(t, "alarm-notify.xml")
	assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "alarm-duplicate-first", body, api.sipNotifyAlarm))
	assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "alarm-duplicate-second", body, api.sipNotifyAlarm))

	if got := callbackCalls.Load(); got != 1 {
		t.Fatalf("duplicate Alarm callback calls = %d, want 1", got)
	}
	if got := notifyCalls.Load(); got != 1 {
		t.Fatalf("duplicate external Alarm notifications = %d, want 1", got)
	}
	deliveryID := alarmDeliveryID(gb10DeviceID, sip.MethodNotify, body)
	waitForAlarmReceipt(t, store, gb10DeviceID, deliveryID)
}

func TestAlarmDuplicateTransactionsWithoutCallbackCommitBusinessOnce(t *testing.T) {
	var notifyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		notifyCalls.Add(1)
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("OK"))
	}))
	t.Cleanup(server.Close)
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{NotifyMethodAlarm: server.URL}}
	t.Cleanup(func() { config = previousConfig })

	store := &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion10)}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	body := readGB10Fixture(t, "alarm-notify.xml")
	assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "alarm-no-callback-first", body, api.sipNotifyAlarm))
	assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "alarm-no-callback-second", body, api.sipNotifyAlarm))

	if got := notifyCalls.Load(); got != 1 {
		t.Fatalf("duplicate Alarm notifications without callback = %d, want 1", got)
	}
	deliveryID := alarmDeliveryID(gb10DeviceID, sip.MethodNotify, body)
	waitForAlarmReceipt(t, store, gb10DeviceID, deliveryID)
}

func TestConcurrentAlarmDuplicateTransactionsCommitBusinessOnce(t *testing.T) {
	var notifyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		notifyCalls.Add(1)
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("OK"))
	}))
	t.Cleanup(server.Close)
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{NotifyMethodAlarm: server.URL}}
	t.Cleanup(func() { config = previousConfig })

	store := &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion10)}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	var callbackCalls atomic.Int32
	api.SetReliableAlarmHandler(func(context.Context, *AlarmEvent) error {
		callbackCalls.Add(1)
		return nil
	})
	body := readGB10Fixture(t, "alarm-notify.xml")
	const transactions = 8
	type invocation struct {
		conn *flowConnection
		ctx  *sip.Context
	}
	items := make([]invocation, 0, transactions)
	to := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	for i := 0; i < transactions; i++ {
		callID := fmt.Sprintf("alarm-duplicate-concurrent-%d", i)
		conn := newFlowConnection()
		request := newFlowRequest(t, conn, sip.MethodNotify, callID, body)
		items = append(items, invocation{conn: conn, ctx: &sip.Context{
			Request: request, Tx: sip.NewTransaction(callID+"-tx", conn), DeviceID: gb10DeviceID,
			Source: conn.remote, To: to, Log: slog.Default(),
		}})
	}
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(len(items))
	for _, item := range items {
		item := item
		go func() {
			defer wait.Done()
			<-start
			api.sipNotifyAlarm(item.ctx)
		}()
	}
	close(start)
	wait.Wait()
	for index, item := range items {
		select {
		case response := <-item.conn.writes:
			assertFlowOK(t, string(response))
		case <-time.After(time.Second):
			t.Fatalf("concurrent Alarm response %d timed out", index)
		}
	}
	if got := callbackCalls.Load(); got != 1 {
		t.Fatalf("concurrent duplicate Alarm callback calls = %d, want 1", got)
	}
	if got := notifyCalls.Load(); got != 1 {
		t.Fatalf("concurrent duplicate external Alarm notifications = %d, want 1", got)
	}
}

func TestCorruptAlarmReceiptDoesNotSuppressInboxDelivery(t *testing.T) {
	store := &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	called := 0
	api.SetReliableAlarmHandler(func(context.Context, *AlarmEvent) error {
		called++
		return nil
	})
	now := time.Now()
	event := AlarmEvent{DeviceID: gb10DeviceID, DeliveryID: "alarm-corrupt-receipt-delivery"}
	inbox := alarmInboxState{Event: event, ReceivedAt: now, BusinessCommittedAt: now}
	payload, err := json.Marshal(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindAlarmInbox, event.DeviceID, event.DeliveryID, payload, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindAlarmReceipt, event.DeviceID, event.DeliveryID, []byte(`{"completed_at":"not-a-time"}`), now); err != nil {
		t.Fatal(err)
	}

	if result := api.processAlarmInboxDeliveryAt(event.DeviceID, event.DeliveryID, now, time.Time{}, now); result != alarmInboxProcessDelivery {
		t.Fatalf("Alarm inbox result = %v", result)
	}
	if called != 1 {
		t.Fatalf("corrupt Alarm receipt suppressed callback; calls = %d", called)
	}
	receipt, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmReceipt, event.DeviceID, event.DeliveryID)
	if err != nil || !found {
		t.Fatalf("repaired Alarm receipt = %s, found %v, err %v", receipt, found, err)
	}
	var state alarmReceiptState
	if err := json.Unmarshal(receipt, &state); err != nil || state.CompletedAt.IsZero() {
		t.Fatalf("repaired Alarm receipt = %s, err %v", receipt, err)
	}
}

func TestAlarmInboxQuarantinesMismatchedPersistentIdentityBeforeDelivery(t *testing.T) {
	store := &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	var calls atomic.Int32
	api.SetReliableAlarmHandler(func(context.Context, *AlarmEvent) error {
		calls.Add(1)
		return nil
	})
	now := time.Now()
	deliveryID := "alarm-mismatched-persistent-identity"
	state := alarmInboxState{
		Event: AlarmEvent{
			DeviceID:   "34020000001320000999",
			DeliveryID: deliveryID,
		},
		ReceivedAt:          now,
		BusinessCommittedAt: now,
	}
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindAlarmInbox, gb10DeviceID, deliveryID, payload, now); err != nil {
		t.Fatal(err)
	}

	if result := api.processAlarmInboxDeliveryAt(gb10DeviceID, deliveryID, now, time.Time{}, now); result != alarmInboxProcessMaintenance {
		t.Fatalf("mismatched Alarm inbox result = %v, want maintenance", result)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("mismatched Alarm inbox reached callback %d times", got)
	}
	if _, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmInbox, gb10DeviceID, deliveryID); err != nil || found {
		t.Fatalf("mismatched Alarm inbox survived quarantine: found=%v err=%v", found, err)
	}
	if _, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmDeadLetter, gb10DeviceID, deliveryID); err != nil || !found {
		t.Fatalf("mismatched Alarm dead letter = found=%v err=%v", found, err)
	}
}

func TestAlarmRetransmissionReplacesMismatchedPersistentIdentity(t *testing.T) {
	store := &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	now := time.Now()
	deliveryID := "alarm-retransmission-replaces-mismatch"
	invalid := alarmInboxState{
		Event:      AlarmEvent{DeviceID: "34020000001320000999", DeliveryID: deliveryID},
		ReceivedAt: now,
	}
	payload, err := json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindAlarmInbox, gb10DeviceID, deliveryID, payload, now); err != nil {
		t.Fatal(err)
	}
	event := &AlarmEvent{
		DeviceID:     gb10DeviceID,
		ChannelID:    gb10ChannelID,
		DeliveryID:   deliveryID,
		CmdType:      "Alarm",
		SourceMethod: sip.MethodNotify,
	}

	if persisted, err := api.persistAlarmInbox(t.Context(), event); err != nil || !persisted {
		t.Fatalf("persist replacement Alarm inbox = persisted=%v err=%v", persisted, err)
	}
	repairedPayload, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmInbox, gb10DeviceID, deliveryID)
	if err != nil || !found {
		t.Fatalf("replacement Alarm inbox = found=%v err=%v", found, err)
	}
	var repaired alarmInboxState
	if err := json.Unmarshal(repairedPayload, &repaired); err != nil {
		t.Fatal(err)
	}
	if repaired.Event != *event {
		t.Fatalf("replacement Alarm event = %+v, want %+v", repaired.Event, *event)
	}
}
