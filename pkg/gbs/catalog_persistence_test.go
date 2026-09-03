package gbs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

func TestCatalogOperationLockSerializesPerDeviceWithoutCrossDeviceBlocking(t *testing.T) {
	api := &GB28181API{}
	unlockFirst, err := api.lockCatalogOperation(t.Context(), gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}

	waitCtx, cancelWait := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancelWait()
	if _, err = api.lockCatalogOperation(waitCtx, gb10DeviceID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same-device Catalog lock error = %v; want %v", err, context.DeadlineExceeded)
	}

	otherCtx, cancelOther := context.WithTimeout(t.Context(), time.Second)
	defer cancelOther()
	unlockOther, err := api.lockCatalogOperation(otherCtx, gb10ChannelID)
	if err != nil {
		t.Fatalf("different device Catalog operation was blocked: %v", err)
	}
	unlockOther()
	unlockFirst()

	unlockAgain, err := api.lockCatalogOperation(t.Context(), gb10DeviceID)
	if err != nil {
		t.Fatalf("released Catalog lock cannot be reused: %v", err)
	}
	unlockAgain()
	api.catalogOperationMu.Lock()
	retained := len(api.catalogOperations)
	api.catalogOperationMu.Unlock()
	if retained != 0 {
		t.Fatalf("released Catalog locks retained %d entries", retained)
	}
}

func TestCatalogQueryResultErrorReportsIncompleteResponse(t *testing.T) {
	result := multiResponseResult[Channels]{
		Items:    []Channels{{ChannelID: gb10ChannelID}},
		Expected: 2,
	}
	err := catalogQueryResultError(result)
	var incomplete *CatalogIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("partial Catalog result error = %v; want CatalogIncompleteError", err)
	}
	if incomplete.Received != 1 || incomplete.Expected != 2 {
		t.Fatalf("partial Catalog result counts = %+v", incomplete)
	}
	if err = catalogQueryResultError(multiResponseResult[Channels]{Expected: 2}); err == nil ||
		!strings.Contains(err.Error(), "Catalog response timeout") {
		t.Fatalf("empty Catalog result error = %v", err)
	}
	if err = catalogQueryResultError(multiResponseResult[Channels]{Complete: true}); err != nil {
		t.Fatalf("complete Catalog result error = %v", err)
	}
}

func TestQueryCatalogContextCancelsWhileWaitingForDeviceOperation(t *testing.T) {
	api := &GB28181API{}
	unlock, err := api.lockCatalogOperation(t.Context(), gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err = api.QueryCatalogContext(ctx, gb10DeviceID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Catalog operation wait error = %v; want %v", err, context.DeadlineExceeded)
	}
	if count := syncMapLen(&api.pendingDeviceRequests); count != 0 {
		t.Fatalf("cancelled Catalog lock wait retained %d pending operations", count)
	}
}

func TestQueryCatalogContextStopsWhileWaitingForDeviceOperation(t *testing.T) {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		lifecycleDone: make(chan struct{}),
	}
	unlock, err := api.lockCatalogOperation(t.Context(), gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	done := make(chan error, 1)
	go func() {
		done <- api.QueryCatalogContext(context.Background(), gb10DeviceID)
	}()
	deadline := time.Now().Add(time.Second)
	for syncMapLen(&api.pendingDeviceRequests) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if count := syncMapLen(&api.pendingDeviceRequests); count != 1 {
		t.Fatalf("waiting Catalog operations = %d; want 1", count)
	}

	api.beginClose()
	select {
	case err = <-done:
		if !errors.Is(err, ErrServiceStopped) {
			t.Fatalf("stopped Catalog operation wait error = %v; want %v", err, ErrServiceStopped)
		}
	case <-time.After(time.Second):
		t.Fatal("Catalog operation wait did not stop during service close")
	}
	if count := syncMapLen(&api.pendingDeviceRequests); count != 0 {
		t.Fatalf("stopped Catalog lock wait retained %d pending operations", count)
	}
}

func TestCatalogNotifyRefreshesCoalescePerDevice(t *testing.T) {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		lifecycleDone: make(chan struct{}),
	}
	unlock, err := api.lockCatalogOperation(t.Context(), gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	unlockOther, err := api.lockCatalogOperation(t.Context(), gb10ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockOther()

	for index := 0; index < 100; index++ {
		if !api.scheduleCatalogRefresh(gb10DeviceID) {
			t.Fatalf("Catalog refresh %d was not scheduled", index)
		}
	}
	if !api.scheduleCatalogRefresh(gb10ChannelID) {
		t.Fatal("different-device Catalog refresh was not scheduled")
	}
	deadline := time.Now().Add(time.Second)
	for syncMapLen(&api.pendingDeviceRequests) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if count := syncMapLen(&api.pendingDeviceRequests); count != 2 {
		t.Fatalf("coalesced Catalog pending operations = %d; want 2 devices", count)
	}
	api.catalogRefreshMu.Lock()
	state := api.catalogRefreshes[gb10DeviceID]
	refreshCount := len(api.catalogRefreshes)
	dirty := state != nil && state.dirty
	api.catalogRefreshMu.Unlock()
	if refreshCount != 2 || !dirty {
		t.Fatalf("coalesced Catalog refresh state = count:%d dirty:%v; want count:2 dirty:true", refreshCount, dirty)
	}

	api.beginClose()
	waitDone := make(chan struct{})
	go func() {
		api.lifecycleWG.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("coalesced Catalog refresh did not stop during service close")
	}
	api.catalogRefreshMu.Lock()
	refreshCount = len(api.catalogRefreshes)
	api.catalogRefreshMu.Unlock()
	if refreshCount != 0 {
		t.Fatalf("stopped Catalog refresh retained %d states", refreshCount)
	}
}

func TestCatalogNotifyRefreshRunsAtMostOneDirtyFollowUp(t *testing.T) {
	api := &GB28181API{catalogRefreshes: make(map[string]*catalogRefreshState)}
	state := &catalogRefreshState{dirty: true}
	api.catalogRefreshes[gb10DeviceID] = state
	calls := 0
	api.runCatalogRefresh(t.Context(), gb10DeviceID, state, func(context.Context, string) error {
		calls++
		return nil
	})
	if calls != 2 {
		t.Fatalf("coalesced Catalog query calls = %d; want 2", calls)
	}
	if len(api.catalogRefreshes) != 0 {
		t.Fatalf("completed Catalog refresh retained %d states", len(api.catalogRefreshes))
	}

	injected := errors.New("injected Catalog query failure")
	state = &catalogRefreshState{dirty: true}
	api.catalogRefreshes[gb10DeviceID] = state
	calls = 0
	api.runCatalogRefresh(t.Context(), gb10DeviceID, state, func(context.Context, string) error {
		calls++
		return injected
	})
	if calls != 2 {
		t.Fatalf("failed coalesced Catalog query calls = %d; want 2", calls)
	}
	if len(api.catalogRefreshes) != 0 {
		t.Fatalf("failed Catalog refresh retained %d states", len(api.catalogRefreshes))
	}
}

func TestCatalogRefreshAfterFailureRetriesOnlyTransientErrors(t *testing.T) {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		lifecycleDone: make(chan struct{}), catalogRefreshes: make(map[string]*catalogRefreshState),
	}
	for _, err := range []error{nil, context.Canceled, ErrServiceStopped, ErrDeviceOffline, ErrDeviceNotExist} {
		if api.scheduleCatalogRefreshAfterFailure(gb10DeviceID, err) {
			t.Fatalf("terminal Catalog error %v scheduled a retry", err)
		}
	}
	if len(api.catalogRefreshes) != 0 {
		t.Fatalf("terminal Catalog errors retained %d refresh states", len(api.catalogRefreshes))
	}

	unlock, err := api.lockCatalogOperation(t.Context(), gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	scheduledAt := time.Now()
	if !api.scheduleCatalogRefreshAfterFailure(gb10DeviceID, errors.New("temporary Catalog persistence failure")) {
		t.Fatal("transient Catalog failure did not schedule a retry")
	}
	api.catalogRefreshMu.Lock()
	refreshCount := len(api.catalogRefreshes)
	retryAt := api.catalogRefreshes[gb10DeviceID].nextAt
	api.catalogRefreshMu.Unlock()
	if refreshCount != 1 {
		t.Fatalf("transient Catalog retry states = %d, want 1", refreshCount)
	}
	if retryAt.Before(scheduledAt.Add(catalogRefreshFailureBackoff)) {
		t.Fatalf("transient Catalog retry deadline = %v, want at least %v", retryAt, scheduledAt.Add(catalogRefreshFailureBackoff))
	}

	api.beginClose()
	unlock()
	api.lifecycleWG.Wait()
}

func TestCatalogRefreshAfterFailureBackoffStopsWithServiceLifecycle(t *testing.T) {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		lifecycleDone: make(chan struct{}), catalogRefreshes: make(map[string]*catalogRefreshState),
	}
	if !api.scheduleCatalogRefreshAfterFailureWithDelay(gb10DeviceID, errors.New("temporary Catalog failure"), time.Hour) {
		t.Fatal("transient Catalog failure did not schedule a delayed retry")
	}

	if requests := api.metrics.Snapshot().CatalogRequests; requests != 0 {
		t.Fatalf("Catalog retry requests before backoff = %d, want 0", requests)
	}

	api.beginClose()
	api.lifecycleWG.Wait()
	if requests := api.metrics.Snapshot().CatalogRequests; requests != 0 {
		t.Fatalf("Catalog retry requests after service close = %d, want 0", requests)
	}
	api.catalogRefreshMu.Lock()
	refreshCount := len(api.catalogRefreshes)
	api.catalogRefreshMu.Unlock()
	if refreshCount != 0 {
		t.Fatalf("stopped delayed Catalog retry retained %d states", refreshCount)
	}
}

func TestCatalogImmediateRefreshAdvancesDelayedRetryWithoutFollowUp(t *testing.T) {
	api := &GB28181API{catalogRefreshes: make(map[string]*catalogRefreshState)}
	state := &catalogRefreshState{
		nextAt: time.Now().Add(time.Hour),
		wake:   make(chan struct{}, 1),
	}
	api.catalogRefreshes[gb10DeviceID] = state
	calls := make(chan time.Time, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.runCatalogRefresh(t.Context(), gb10DeviceID, state, func(context.Context, string) error {
			calls <- time.Now()
			return nil
		})
	}()

	select {
	case <-calls:
		t.Fatal("delayed Catalog retry ran before its deadline")
	case <-time.After(20 * time.Millisecond):
	}
	if !api.scheduleCatalogRefresh(gb10DeviceID) {
		t.Fatal("immediate Catalog refresh did not advance delayed retry")
	}
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("advanced Catalog refresh did not run")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("advanced Catalog refresh did not finish")
	}
	select {
	case <-calls:
		t.Fatal("advanced delayed Catalog retry ran an unnecessary follow-up")
	default:
	}
}

func TestCatalogNotifyRefreshConsumesDirtyMarkerAfterQueryFailure(t *testing.T) {
	api := &GB28181API{catalogRefreshes: make(map[string]*catalogRefreshState)}
	state := &catalogRefreshState{}
	api.catalogRefreshes[gb10DeviceID] = state
	injected := errors.New("injected Catalog query failure")
	calls := 0
	api.runCatalogRefresh(t.Context(), gb10DeviceID, state, func(context.Context, string) error {
		calls++
		if calls == 1 {
			api.catalogRefreshMu.Lock()
			state.dirty = true
			api.catalogRefreshMu.Unlock()
		}
		return injected
	})
	if calls != 2 {
		t.Fatalf("Catalog query calls after dirty failure = %d; want 2", calls)
	}
	if len(api.catalogRefreshes) != 0 {
		t.Fatalf("failed Catalog refresh retained %d states", len(api.catalogRefreshes))
	}
}

func TestCatalogPersistenceFailurePreservesPublishedSnapshotByVersion(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		for _, empty := range []bool{false, true} {
			name := string(version) + "/non_empty"
			if empty {
				name = string(version) + "/empty"
			}
			t.Run(name, func(t *testing.T) {
				base, device, persisted := newCascadeMediaCore(t)
				if err := base.Store().Device().Update(t.Context(), device, func(current *ipc.Device) error {
					current.Channels = 1
					return nil
				}, orm.Where("id = ?", device.ID)); err != nil {
					t.Fatal(err)
				}
				injected := errors.New("injected Catalog persistence failure")
				channelStore := &catalogFailCommitChannelStore{ChannelStorer: base.Store().Channel(), err: injected}
				store := &catalogFailCommitStore{Storer: base.Store(), channel: channelStore}

				memory := newFlowMemory(device.DeviceID)
				memory.runtime.setGBVersion(version)
				existingRuntime := &Channel{ChannelID: persisted.ChannelID, device: memory.runtime}
				if err := existingRuntime.init("3402000000"); err != nil {
					t.Fatal(err)
				}
				memory.runtime.Channels.Store(existingRuntime.ChannelID, existingRuntime)
				streams := &conc.Map[string, *Streams]{}
				streamKey := resolvePlaySessionKey(device.DeviceID, persisted.ChannelID, "")
				stream := &Streams{DeviceID: device.DeviceID, ChannelID: persisted.ChannelID, StreamID: "catalog-rollback"}
				streams.Store(streamKey, stream)
				api := &GB28181API{
					cfg:  &conf.SIP{Domain: "3402000000"},
					core: ipc.NewAdapter(store, uniqueid.Core{}), streams: streams, lifecycleClosed: true,
				}
				api.svr = &Server{memoryStorer: memory}

				var items []Channels
				if !empty {
					items = []Channels{{ChannelID: persisted.ChannelID, Name: "changed", Status: "OFF"}}
				}
				err := api.saveCatalogChannels(device.DeviceID, items)
				if !errors.Is(err, injected) {
					t.Fatalf("saveCatalogChannels error = %v, want %v", err, injected)
				}
				if current, ok := memory.runtime.Channels.Load(persisted.ChannelID); !ok || current != existingRuntime {
					t.Fatalf("runtime channel changed after persistence failure: ok=%v current=%p want=%p", ok, current, existingRuntime)
				}
				if current, ok := streams.Load(streamKey); !ok || current != stream || stream.Stop || stream.EndReason != "" {
					t.Fatalf("media session changed after persistence failure: ok=%v current=%p stream=%+v", ok, current, stream)
				}
				var persistedDevice ipc.Device
				if err := base.Store().Device().Get(t.Context(), &persistedDevice, orm.Where("id = ?", device.ID)); err != nil {
					t.Fatal(err)
				}
				if persistedDevice.Channels != 1 {
					t.Fatalf("device channel count = %d, want 1", persistedDevice.Channels)
				}
				var persistedChannel ipc.Channel
				if err := base.Store().Channel().Get(t.Context(), &persistedChannel, orm.Where("id = ?", persisted.ID)); err != nil {
					t.Fatal(err)
				}
				if persistedChannel.Name != persisted.Name || !persistedChannel.IsOnline {
					t.Fatalf("persisted channel changed after rollback: %+v", persistedChannel)
				}
			})
		}
	}
}

func TestCatalogEmptySnapshotCommitsBeforeRuntimeCleanup(t *testing.T) {
	base, device, persisted := newCascadeMediaCore(t)
	if err := base.Store().Device().Update(t.Context(), device, func(current *ipc.Device) error {
		current.Channels = 1
		return nil
	}, orm.Where("id = ?", device.ID)); err != nil {
		t.Fatal(err)
	}
	memory := newFlowMemory(device.DeviceID)
	existingRuntime := &Channel{ChannelID: persisted.ChannelID, device: memory.runtime}
	if err := existingRuntime.init("3402000000"); err != nil {
		t.Fatal(err)
	}
	memory.runtime.Channels.Store(existingRuntime.ChannelID, existingRuntime)
	streams := &conc.Map[string, *Streams]{}
	streamKey := resolvePlaySessionKey(device.DeviceID, persisted.ChannelID, "")
	stream := &Streams{DeviceID: device.DeviceID, ChannelID: persisted.ChannelID, StreamID: "catalog-empty"}
	streams.Store(streamKey, stream)
	api := &GB28181API{core: base, streams: streams, lifecycleClosed: true}
	api.svr = &Server{memoryStorer: memory}

	if err := api.saveCatalogChannels(device.DeviceID, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := memory.runtime.Channels.Load(persisted.ChannelID); ok {
		t.Fatal("empty Catalog snapshot retained runtime channel")
	}
	if _, ok := streams.Load(streamKey); ok || !stream.Stop || stream.EndReason != "channel_offline" {
		t.Fatalf("empty Catalog media cleanup = exists:%v stream:%+v", ok, stream)
	}
	var persistedDevice ipc.Device
	if err := base.Store().Device().Get(t.Context(), &persistedDevice, orm.Where("id = ?", device.ID)); err != nil {
		t.Fatal(err)
	}
	if persistedDevice.Channels != 0 {
		t.Fatalf("empty Catalog device channel count = %d, want 0", persistedDevice.Channels)
	}
	var persistedChannel ipc.Channel
	if err := base.Store().Channel().Get(t.Context(), &persistedChannel, orm.Where("id = ?", persisted.ID)); err != nil {
		t.Fatal(err)
	}
	if persistedChannel.IsOnline {
		t.Fatalf("empty Catalog retained persisted online channel: %+v", persistedChannel)
	}
}

func TestCatalogPersistenceCancellationPreservesPublishedSnapshot(t *testing.T) {
	base, device, persisted := newCascadeMediaCore(t)
	memory := newFlowMemory(device.DeviceID)
	existingRuntime := &Channel{ChannelID: persisted.ChannelID, device: memory.runtime}
	if err := existingRuntime.init("3402000000"); err != nil {
		t.Fatal(err)
	}
	memory.runtime.Channels.Store(existingRuntime.ChannelID, existingRuntime)
	api := &GB28181API{
		cfg: &conf.SIP{Domain: "3402000000"}, core: base,
		lifecycleClosed: true,
	}
	api.svr = &Server{memoryStorer: memory}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := api.saveCatalogChannelsContext(ctx, device.DeviceID, []Channels{{
		ChannelID: persisted.ChannelID, Name: "cancelled update", Status: "OFF",
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("saveCatalogChannelsContext error = %v, want context cancellation", err)
	}
	if current, ok := memory.runtime.Channels.Load(persisted.ChannelID); !ok || current != existingRuntime {
		t.Fatalf("cancelled Catalog changed runtime channel: ok=%v current=%p want=%p", ok, current, existingRuntime)
	}
	var persistedChannel ipc.Channel
	if err := base.Store().Channel().Get(t.Context(), &persistedChannel, orm.Where("id = ?", persisted.ID)); err != nil {
		t.Fatal(err)
	}
	if persistedChannel.Name != persisted.Name || !persistedChannel.IsOnline {
		t.Fatalf("cancelled Catalog changed persisted channel: %+v", persistedChannel)
	}
}

type catalogFailCommitStore struct {
	ipc.Storer
	channel ipc.ChannelStorer
}

func (s *catalogFailCommitStore) Channel() ipc.ChannelStorer { return s.channel }

type catalogFailCommitChannelStore struct {
	ipc.ChannelStorer
	err error
}

func (s *catalogFailCommitChannelStore) Session(ctx context.Context, changeFns ...func(*gorm.DB) error) error {
	return s.ChannelStorer.Session(ctx, append(changeFns, func(*gorm.DB) error { return s.err })...)
}
