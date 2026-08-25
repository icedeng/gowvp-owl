package gbs

import (
	"testing"
	"time"
)

func TestTickerCheckStopsWithGB28181Lifecycle(t *testing.T) {
	api := &GB28181API{lifecycleDone: make(chan struct{})}
	server := &Server{gb: api}
	done := make(chan struct{})
	go func() {
		server.startTickerCheck()
		close(done)
	}()
	api.close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("device ticker did not stop with GB28181 lifecycle")
	}
}
