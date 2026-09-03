package gbs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockingDirectTCPTaskMemory struct {
	*rtpDownloadTaskMemory
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (m *blockingDirectTCPTaskMemory) SaveGBTaskState(ctx context.Context, kind, deviceID, sessionID string, payload []byte, updatedAt time.Time) error {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()
	if call == 1 {
		close(m.entered)
		select {
		case <-m.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return m.rtpDownloadTaskMemory.SaveGBTaskState(ctx, kind, deviceID, sessionID, payload, updatedAt)
}

func directTCPTerminalTestState(status, sessionID string, now time.Time) DirectTCPDownloadState {
	state := DirectTCPDownloadState{
		SessionID: sessionID, DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		Status: status, Received: 10, FileSize: 10, FileSizeKnown: true,
		StartedAt: now.Add(-time.Minute), UpdatedAt: now, CompletedAt: now,
	}
	switch status {
	case directTCPStatusCompleted:
		state.SizeVerified = true
		state.Output = sessionID + ".ps"
		state.SHA256 = strings.Repeat("ab", 32)
		state.EndReason = "size_reached"
	case directTCPStatusFailed:
		state.Received = 4
		state.EndReason = "early_eof"
		state.Error = "unexpected EOF"
	case directTCPStatusCancelled:
		state.Received = 4
		state.EndReason = "cancelled"
	}
	return state
}

func directTCPPersistenceTestAPI(store MemoryStorer, manager *DirectTCPDownloadManager) *GB28181API {
	return &GB28181API{svr: &Server{memoryStorer: store}, directDownloads: manager}
}

func TestDirectTCPTerminalStatesRestoreAcrossRestart(t *testing.T) {
	for _, status := range []string{directTCPStatusCompleted, directTCPStatusFailed, directTCPStatusCancelled} {
		t.Run(status, func(t *testing.T) {
			store := newRTPDownloadTaskMemory(GBVersion11)
			state := directTCPTerminalTestState(status, "direct-"+status, time.Now())
			firstManager := newTestDirectTCPManager(t)
			if status == directTCPStatusCompleted {
				if err := os.WriteFile(filepath.Join(firstManager.opts.StorageDir, state.Output), []byte(strings.Repeat("x", int(state.Received))), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			first := directTCPPersistenceTestAPI(store, firstManager)
			if err := first.persistDirectTCPDownloadState(state, true); err != nil {
				t.Fatal(err)
			}

			restartedManager := NewDirectTCPDownloadManager(firstManager.opts)
			restarted := directTCPPersistenceTestAPI(store, restartedManager)
			if err := restarted.restoreDirectTCPDownloadStates(t.Context()); err != nil {
				t.Fatal(err)
			}
			got, ok := restartedManager.State(state.SessionID)
			if !ok || got.Status != status || got.EndReason != state.EndReason || got.CompletedAt.IsZero() {
				t.Fatalf("restored direct TCP state = %+v, exists=%v", got, ok)
			}
			latest, ok := restartedManager.FindByChannel(state.DeviceID, state.ChannelID)
			if !ok || latest.SessionID != state.SessionID {
				t.Fatalf("restored channel state = %+v, exists=%v", latest, ok)
			}
		})
	}
}

func TestDirectTCPActiveMarkerSupersedesOldTerminalAndIsRemovedOnRestart(t *testing.T) {
	store := newRTPDownloadTaskMemory(GBVersion11)
	first := directTCPPersistenceTestAPI(store, newTestDirectTCPManager(t))
	now := time.Now()
	oldState := directTCPTerminalTestState(directTCPStatusCompleted, "old-direct", now.Add(-time.Minute))
	if err := first.persistDirectTCPDownloadState(oldState, true); err != nil {
		t.Fatal(err)
	}
	marker := DirectTCPDownloadState{
		SessionID: "new-direct", DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		Status: directTCPStatusConnecting, StartedAt: now, UpdatedAt: now,
	}
	if err := first.persistDirectTCPDownloadState(marker, false); err != nil {
		t.Fatal(err)
	}
	payload, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindDirectTCPDownload, marker.DeviceID, marker.ChannelID)
	if err != nil || !ok {
		t.Fatalf("load direct TCP marker: exists=%v err=%v", ok, err)
	}
	var persisted directTCPDownloadPersistentState
	if err := json.Unmarshal(payload, &persisted); err != nil || persisted.Terminal || persisted.State.SessionID != marker.SessionID {
		t.Fatalf("persisted direct TCP marker = %+v, err=%v", persisted, err)
	}

	restartedManager := newTestDirectTCPManager(t)
	restarted := directTCPPersistenceTestAPI(store, restartedManager)
	if err := restarted.restoreDirectTCPDownloadStates(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := restartedManager.FindByChannel(marker.DeviceID, marker.ChannelID); ok {
		t.Fatal("restart restored an interrupted direct TCP download or the superseded terminal")
	}
	if _, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindDirectTCPDownload, marker.DeviceID, marker.ChannelID); err != nil || ok {
		t.Fatalf("interrupted direct TCP marker remained: exists=%v err=%v", ok, err)
	}
}

func TestDirectTCPPersistenceKeepsNewMarkerAfterSlowTerminalSave(t *testing.T) {
	store := &blockingDirectTCPTaskMemory{
		rtpDownloadTaskMemory: newRTPDownloadTaskMemory(GBVersion11),
		entered:               make(chan struct{}),
		release:               make(chan struct{}),
	}
	api := &GB28181API{svr: &Server{memoryStorer: store}, directDownloads: newTestDirectTCPManager(t)}
	now := time.Now()
	terminal := directTCPTerminalTestState(directTCPStatusCompleted, "slow-terminal", now.Add(-time.Minute))
	terminalDone := make(chan error, 1)
	go func() { terminalDone <- api.persistDirectTCPDownloadState(terminal, true) }()
	<-store.entered

	marker := DirectTCPDownloadState{
		SessionID: "new-marker", DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		Status: directTCPStatusConnecting, StartedAt: now, UpdatedAt: now,
	}
	markerDone := make(chan error, 1)
	go func() { markerDone <- api.persistDirectTCPDownloadState(marker, false) }()
	deadline := time.Now().Add(time.Second)
	for {
		api.directDownloadPersistenceMu.Lock()
		slot := api.directDownloadPersistence[directTCPDownloadPersistenceKey(marker.DeviceID, marker.ChannelID)]
		api.directDownloadPersistenceMu.Unlock()
		if slot != nil {
			slot.mu.RLock()
			revision := slot.revision
			slot.mu.RUnlock()
			if revision >= 2 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("new direct TCP marker did not advance persistence revision")
		}
		time.Sleep(time.Millisecond)
	}
	close(store.release)
	if err := <-terminalDone; err != nil {
		t.Fatal(err)
	}
	if err := <-markerDone; err != nil {
		t.Fatal(err)
	}
	payload, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindDirectTCPDownload, marker.DeviceID, marker.ChannelID)
	if err != nil || !ok {
		t.Fatalf("load serialized direct TCP state: exists=%v err=%v", ok, err)
	}
	var persisted directTCPDownloadPersistentState
	if err := json.Unmarshal(payload, &persisted); err != nil || persisted.Terminal || persisted.State.SessionID != marker.SessionID {
		t.Fatalf("slow terminal save overwrote new marker: %+v, err=%v", persisted, err)
	}
}

func TestDirectTCPTerminalPersistenceRetriesAfterFailure(t *testing.T) {
	store := newRTPDownloadTaskMemory(GBVersion11)
	store.failSaves = 1
	api := directTCPPersistenceTestAPI(store, newTestDirectTCPManager(t))
	state := directTCPTerminalTestState(directTCPStatusCompleted, "retry-direct", time.Now())
	if err := api.persistDirectTCPDownloadState(state, true); !errors.Is(err, errTaskStateSave) {
		t.Fatalf("first terminal save error = %v", err)
	}
	api.cleanupRuntimeStates(time.Now())
	payload, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindDirectTCPDownload, state.DeviceID, state.ChannelID)
	if err != nil || !ok {
		t.Fatalf("retried direct TCP state: exists=%v err=%v", ok, err)
	}
	var persisted directTCPDownloadPersistentState
	if err := json.Unmarshal(payload, &persisted); err != nil || !persisted.Terminal || persisted.State.SessionID != state.SessionID {
		t.Fatalf("retried direct TCP state = %+v, err=%v", persisted, err)
	}
	api.directDownloadPersistenceMu.Lock()
	pending := len(api.directDownloadPersistence)
	api.directDownloadPersistenceMu.Unlock()
	if pending != 0 {
		t.Fatalf("successful retry retained %d persistence slots", pending)
	}
}

func TestDirectTCPTerminalPersistenceRetriesDuringShutdown(t *testing.T) {
	store := newRTPDownloadTaskMemory(GBVersion11)
	store.failSaves = 1
	api := directTCPPersistenceTestAPI(store, newTestDirectTCPManager(t))
	api.lifecycleDone = make(chan struct{})
	state := directTCPTerminalTestState(directTCPStatusCancelled, "shutdown-retry-direct", time.Now())
	if err := api.persistDirectTCPDownloadState(state, true); !errors.Is(err, errTaskStateSave) {
		t.Fatalf("first terminal save error = %v", err)
	}
	api.close()
	payload, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindDirectTCPDownload, state.DeviceID, state.ChannelID)
	if err != nil || !ok {
		t.Fatalf("shutdown-retried direct TCP state: exists=%v err=%v", ok, err)
	}
	var persisted directTCPDownloadPersistentState
	if err := json.Unmarshal(payload, &persisted); err != nil || !persisted.Terminal || persisted.State.SessionID != state.SessionID {
		t.Fatalf("shutdown-retried direct TCP state = %+v, err=%v", persisted, err)
	}
}

func TestDirectTCPActiveMarkerFailureDoesNotBecomeBackgroundRetry(t *testing.T) {
	store := newRTPDownloadTaskMemory(GBVersion11)
	store.failSaves = 1
	api := directTCPPersistenceTestAPI(store, newTestDirectTCPManager(t))
	now := time.Now()
	marker := DirectTCPDownloadState{
		SessionID: "failed-marker", DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		Status: directTCPStatusConnecting, StartedAt: now, UpdatedAt: now,
	}
	if err := api.persistDirectTCPDownloadState(marker, false); !errors.Is(err, errTaskStateSave) {
		t.Fatalf("active marker save error = %v", err)
	}
	api.retryPendingDirectTCPDownloadStates()
	if _, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindDirectTCPDownload, marker.DeviceID, marker.ChannelID); err != nil || ok {
		t.Fatalf("failed active marker was retried: exists=%v err=%v", ok, err)
	}
}

func TestDirectTCPRestoreRejectsInvalidPayloadAndIdentity(t *testing.T) {
	store := newRTPDownloadTaskMemory(GBVersion11)
	now := time.Now()
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindDirectTCPDownload, gb10DeviceID, gb10ChannelID, []byte("not-json"), now); err != nil {
		t.Fatal(err)
	}
	state := directTCPTerminalTestState(directTCPStatusCompleted, "mismatched-direct", now)
	payload, err := json.Marshal(directTCPDownloadPersistentState{Terminal: true, State: state})
	if err != nil {
		t.Fatal(err)
	}
	otherChannel := "34020000001320009999"
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindDirectTCPDownload, gb10DeviceID, otherChannel, payload, now); err != nil {
		t.Fatal(err)
	}
	future := directTCPTerminalTestState(directTCPStatusFailed, "future-direct", now)
	future.ChannelID = "34020000001320009998"
	future.UpdatedAt = now.Add(24 * time.Hour)
	futurePayload, _ := json.Marshal(directTCPDownloadPersistentState{Terminal: true, State: future})
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindDirectTCPDownload, future.DeviceID, future.ChannelID, futurePayload, future.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	manager := newTestDirectTCPManager(t)
	api := directTCPPersistenceTestAPI(store, manager)
	if err := api.restoreDirectTCPDownloadStates(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.State(state.SessionID); ok {
		t.Fatal("invalid persisted direct TCP identity was restored")
	}
	if _, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindDirectTCPDownload, gb10DeviceID, gb10ChannelID); err != nil || found {
		t.Fatalf("malformed direct TCP download record was not removed: found=%v err=%v", found, err)
	}
	if _, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindDirectTCPDownload, gb10DeviceID, otherChannel); err != nil || found {
		t.Fatalf("mismatched direct TCP download record was not removed: found=%v err=%v", found, err)
	}
	if _, ok := manager.State(future.SessionID); ok {
		t.Fatal("direct TCP download record with a future update time was restored")
	}
	if _, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindDirectTCPDownload, future.DeviceID, future.ChannelID); err != nil || found {
		t.Fatalf("direct TCP download record with a future update time was not removed: found=%v err=%v", found, err)
	}
}

func TestDirectTCPRestoreRemovesCompletedStateWhenOutputIsMissing(t *testing.T) {
	store := newRTPDownloadTaskMemory(GBVersion11)
	now := time.Now()
	state := directTCPTerminalTestState(directTCPStatusCompleted, "missing-output-direct", now)
	api := directTCPPersistenceTestAPI(store, newTestDirectTCPManager(t))
	if err := api.persistDirectTCPDownloadState(state, true); err != nil {
		t.Fatal(err)
	}

	restartedManager := newTestDirectTCPManager(t)
	restarted := directTCPPersistenceTestAPI(store, restartedManager)
	if err := restarted.restoreDirectTCPDownloadStates(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := restartedManager.State(state.SessionID); ok {
		t.Fatal("completed direct TCP state without its output file was restored")
	}
	if _, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindDirectTCPDownload, state.DeviceID, state.ChannelID); err != nil || found {
		t.Fatalf("completed direct TCP state without output was not removed: found=%v err=%v", found, err)
	}
}

func TestDirectTCPRestoreCannotDeleteConcurrentMarker(t *testing.T) {
	base := newRTPDownloadTaskMemory(GBVersion11)
	now := time.Now()
	old := DirectTCPDownloadState{SessionID: "old-direct-restore-marker", DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, Status: directTCPStatusConnecting, StartedAt: now.Add(-time.Minute), UpdatedAt: now}
	payload, err := json.Marshal(directTCPDownloadPersistentState{State: old})
	if err != nil {
		t.Fatal(err)
	}
	if err := base.SaveGBTaskState(t.Context(), gbTaskKindDirectTCPDownload, old.DeviceID, old.ChannelID, payload, now); err != nil {
		t.Fatal(err)
	}
	store := &blockingTaskStateLoadMemory{
		rtpDownloadTaskMemory: base, kind: gbTaskKindDirectTCPDownload, deviceID: old.DeviceID, sessionID: old.ChannelID,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	api := directTCPPersistenceTestAPI(store, newTestDirectTCPManager(t))
	restoreDone := make(chan error, 1)
	go func() { restoreDone <- api.restoreDirectTCPDownloadStates(t.Context()) }()
	<-store.entered
	newState := DirectTCPDownloadState{SessionID: "new-direct-restore-marker", DeviceID: old.DeviceID, ChannelID: old.ChannelID, Status: directTCPStatusConnecting, StartedAt: now, UpdatedAt: now}
	persistDone := make(chan error, 1)
	go func() { persistDone <- api.persistDirectTCPDownloadState(newState, false) }()
	select {
	case err := <-persistDone:
		t.Fatalf("new direct TCP marker persisted before restore released: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(store.release)
	if err := <-restoreDone; err != nil {
		t.Fatal(err)
	}
	if err := <-persistDone; err != nil {
		t.Fatal(err)
	}
	finalPayload, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindDirectTCPDownload, old.DeviceID, old.ChannelID)
	if err != nil || !found {
		t.Fatalf("new direct TCP marker missing after restore: found=%v err=%v", found, err)
	}
	var final directTCPDownloadPersistentState
	if err := json.Unmarshal(finalPayload, &final); err != nil || final.Terminal || final.State.SessionID != newState.SessionID {
		t.Fatalf("restore removed or replaced new direct TCP marker: %+v err=%v", final, err)
	}
}
