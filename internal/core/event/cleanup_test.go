package event

import (
	"context"
	"testing"
	"time"
)

func TestCleanupWorkerStopsWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Core{}.StartCleanupWorker(ctx, 1)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup worker did not stop")
	}
}
