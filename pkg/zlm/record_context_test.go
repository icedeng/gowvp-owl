package zlm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGetMediaListContextCancelsRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	engine := NewEngine().SetConfig(Config{URL: server.URL})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := engine.GetMediaListContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("media query ignored caller deadline: %v", err)
	}
}
