package gbs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestServerChangeMemoryUsesContextCapability(t *testing.T) {
	memory := &contextChangeMemory{flowMemory: newFlowMemory(gb10DeviceID)}
	server := &Server{memoryStorer: memory}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := server.changeMemory(ctx, gb10DeviceID, func(*ipc.Device) error {
		t.Fatal("persistent callback ran after cancellation")
		return nil
	}, func(*Device) {
		t.Fatal("runtime callback ran after cancellation")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("changeMemory error = %v, want context.Canceled", err)
	}
	if !memory.contextCalled || memory.legacyCalled {
		t.Fatalf("change dispatch = context:%t legacy:%t", memory.contextCalled, memory.legacyCalled)
	}
}

func TestServerChangeMemoryFallsBackToLegacyCapability(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	server := &Server{memoryStorer: memory}

	if err := server.changeMemory(t.Context(), gb10DeviceID, func(device *ipc.Device) error {
		device.IsOnline = true
		return nil
	}, func(device *Device) {
		device.IsOnline = false
	}); err != nil {
		t.Fatal(err)
	}
	if !memory.persistent.IsOnline || memory.runtime.IsOnline {
		t.Fatalf("legacy fallback state = persistent:%t runtime:%t", memory.persistent.IsOnline, memory.runtime.IsOnline)
	}
}

func TestServerLoadDeviceMemoryUsesContextCapability(t *testing.T) {
	memory := &contextLoadMemory{flowMemory: newFlowMemory(gb10DeviceID)}
	server := &Server{memoryStorer: memory}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := server.loadDeviceMemory(ctx, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("loadDeviceMemory error = %v, want context.Canceled", err)
	}
	if !memory.contextCalled || memory.legacyCalled {
		t.Fatalf("load dispatch = context:%t legacy:%t", memory.contextCalled, memory.legacyCalled)
	}
}

func TestServerLoadDeviceMemoryFallsBackToLegacyCapability(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	server := &Server{memoryStorer: memory}
	if err := server.loadDeviceMemory(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestServerApplyConfigDoesNotPartiallyCommitWhenCascadeApplyFails(t *testing.T) {
	current := conf.DefaultConfig().Sip
	next := current
	next.Password = "replacement-password"
	api := &GB28181API{cfg: &current}
	server := &Server{gb: api}
	server.cascade = NewCascadeManager(server)
	server.cascade.Close()

	err := server.ApplyConfig(next)
	if !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("SetConfig error = %v, want %v", err, ErrServiceStopped)
	}
	if snapshot := api.configSnapshot(); snapshot == nil || snapshot.Password != current.Password {
		t.Fatalf("config committed after cascade failure: %+v", snapshot)
	}
}

func TestServerApplyConfigCommitsAfterCascadeApplySucceeds(t *testing.T) {
	current := conf.DefaultConfig().Sip
	next := current
	next.Password = "replacement-password"
	api := &GB28181API{cfg: &current}
	server := &Server{gb: api}

	if err := server.ApplyConfig(next); err != nil {
		t.Fatal(err)
	}
	if snapshot := api.configSnapshot(); snapshot == nil || snapshot.Password != next.Password {
		t.Fatalf("committed config = %+v", snapshot)
	}
}

func TestKeepaliveStateCommitStopsWithServiceLifecycle(t *testing.T) {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	memory := &blockingContextChangeMemory{
		flowMemory: newFlowMemory(gb10DeviceID),
		entered:    make(chan struct{}),
	}
	api := &GB28181API{
		lifecycleDone:   make(chan struct{}),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
	}
	server := &Server{gb: api, memoryStorer: memory}
	api.svr = server

	done := make(chan error, 1)
	go func() {
		done <- api.persistKeepaliveObservation(gb10DeviceID, keepaliveObservation{
			online:     true,
			observedAt: time.Now(),
		}, nil)
	}()
	select {
	case <-memory.entered:
	case <-time.After(time.Second):
		t.Fatal("keepalive state commit did not enter context-aware store")
	}

	api.beginClose()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("keepalive state commit error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("keepalive state commit did not stop with service lifecycle")
	}
}

type contextChangeMemory struct {
	*flowMemory
	contextCalled bool
	legacyCalled  bool
}

type blockingContextChangeMemory struct {
	*flowMemory
	entered chan struct{}
}

type contextLoadMemory struct {
	*flowMemory
	contextCalled bool
	legacyCalled  bool
}

func (m *contextLoadMemory) LoadDeviceToMemory(_ sip.Connection) error {
	m.legacyCalled = true
	return nil
}

func (m *contextLoadMemory) LoadDeviceToMemoryContext(ctx context.Context, _ sip.Connection) error {
	m.contextCalled = true
	return ctx.Err()
}

func (m *blockingContextChangeMemory) ChangeContext(ctx context.Context, _ string, _ func(*ipc.Device) error, _ func(*Device)) error {
	close(m.entered)
	<-ctx.Done()
	return ctx.Err()
}

func (m *contextChangeMemory) Change(deviceID string, persistent func(*ipc.Device) error, runtime func(*Device)) error {
	m.legacyCalled = true
	return m.flowMemory.Change(deviceID, persistent, runtime)
}

func (m *contextChangeMemory) ChangeContext(ctx context.Context, deviceID string, persistent func(*ipc.Device) error, runtime func(*Device)) error {
	m.contextCalled = true
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.flowMemory.Change(deviceID, persistent, runtime)
}
