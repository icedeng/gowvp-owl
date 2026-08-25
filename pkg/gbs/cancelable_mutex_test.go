package gbs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
)

func TestCancelableMutexStopsWaitingWithContext(t *testing.T) {
	var mutex cancelableMutex
	mutex.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- mutex.LockContext(ctx)
	}()
	cancel()
	select {
	case err := <-waitDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled mutex wait error = %v; want %v", err, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled mutex waiter did not exit")
	}
	mutex.Unlock()

	if err := mutex.LockContext(context.Background()); err != nil {
		t.Fatalf("mutex was not reusable after cancellation: %v", err)
	}
	mutex.Unlock()
}

func TestCancelableMutexSerializesConcurrentWork(t *testing.T) {
	var mutex cancelableMutex
	var workers sync.WaitGroup
	value := 0
	for worker := 0; worker < 100; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := mutex.LockContext(context.Background()); err != nil {
				t.Errorf("lock context: %v", err)
				return
			}
			current := value
			time.Sleep(time.Microsecond)
			value = current + 1
			mutex.Unlock()
		}()
	}
	workers.Wait()
	if value != 100 {
		t.Fatalf("serialized value = %d; want 100", value)
	}
}

func TestPlayContextCancelsWhileWaitingForDeviceMediaLock(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	channel := &Channel{ChannelID: gb10ChannelID, device: memory.runtime}
	if err := channel.init("3402000000"); err != nil {
		t.Fatal(err)
	}
	memory.runtime.Channels.Store(gb10ChannelID, channel)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}

	memory.runtime.playMutex.Lock()
	defer memory.runtime.playMutex.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := api.PlayContext(ctx, &PlayInput{Channel: &ipc.Channel{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, ID: "stream-waiting-for-lock",
	}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("media lock wait error = %v; want %v", err, context.DeadlineExceeded)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("media lock cancellation took %s", elapsed)
	}
}
