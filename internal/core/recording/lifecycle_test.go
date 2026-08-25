package recording

import (
	"context"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
)

func TestCleanupWorkerStopsWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Core{conf: &conf.ServerRecording{}}.StartCleanupWorker(ctx)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recording cleanup worker did not stop")
	}
}

func TestRecordingSyncLoopReportsStoppedWhenDisabled(t *testing.T) {
	done := Core{}.StartRecordingSyncLoop(context.Background())
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled recording sync loop did not stop")
	}
}
