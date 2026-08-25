package push

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gowvp/owl/internal/core/event"
)

func TestNotifierCloseDrainsAndIsIdempotent(t *testing.T) {
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
		received <- struct{}{}
	}))
	defer server.Close()

	notifier := NewNotifier([]string{server.URL}, 1, 1)
	notifier.Dispatch(context.Background(), &event.Event{DID: "device-1"})
	notifier.Close()
	notifier.Close()
	select {
	case <-received:
	default:
		t.Fatal("Close returned before the queued event was delivered")
	}

	// 关闭后的投递应安全忽略，不得向已关闭 channel 发送。
	notifier.Dispatch(context.Background(), &event.Event{DID: "device-2"})
}

func TestNotifierConcurrentDispatchAndClose(t *testing.T) {
	notifier := NewNotifier(nil, 0, 0)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				notifier.Dispatch(context.Background(), &event.Event{DID: "device"})
			}
		}()
	}
	notifier.Close()
	wg.Wait()
}

func TestNilNotifierIsSafe(t *testing.T) {
	var notifier *Notifier
	notifier.Dispatch(context.Background(), &event.Event{})
	notifier.Close()
}
