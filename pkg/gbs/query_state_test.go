package gbs

import (
	"fmt"
	"testing"
	"time"
)

func TestCleanupQueryStatesExpiresAndBoundsSnapshots(t *testing.T) {
	now := time.Now()
	api := &GB28181API{}
	api.queryStates.Store("expired", &QueryState{UpdatedAt: now.Add(-queryStateTTL - time.Second)})
	api.queryStates.Store("invalid", "unexpected")
	for i := 0; i < maxQueryStateEntries+2; i++ {
		deviceID := fmt.Sprintf("device-%04d", i)
		api.queryStates.Store(deviceID, &QueryState{UpdatedAt: now.Add(time.Duration(i) * time.Nanosecond)})
	}

	api.cleanupQueryStates(now.Add(time.Second))

	if _, ok := api.queryStates.Load("expired"); ok {
		t.Fatal("expired query state was retained")
	}
	if _, ok := api.queryStates.Load("invalid"); ok {
		t.Fatal("invalid query state was retained")
	}
	count := 0
	api.queryStates.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != maxQueryStateEntries {
		t.Fatalf("query states = %d; want %d", count, maxQueryStateEntries)
	}
	if _, ok := api.queryStates.Load("device-0000"); ok {
		t.Fatal("oldest query state was retained")
	}
}
