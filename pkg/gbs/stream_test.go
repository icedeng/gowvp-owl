package gbs

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/gowvp/owl/internal/conf"
)

func TestGetSSRCUsesValidatedDomainCode(t *testing.T) {
	api := &GB28181API{cfg: &conf.SIP{Domain: "3402000000"}}

	live, releaseLive, err := api.reserveSSRC(0)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLive()
	history, releaseHistory, err := api.reserveSSRC(1)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseHistory()
	if len(live) != 10 || !strings.HasPrefix(live, "020000") {
		t.Fatalf("live SSRC = %q", live)
	}
	if len(history) != 10 || !strings.HasPrefix(history, "120000") {
		t.Fatalf("history SSRC = %q", history)
	}
	if live == history {
		t.Fatalf("live and history SSRC must differ: %q", live)
	}
}

func TestGetSSRCDerivesDomainFromPlatformID(t *testing.T) {
	api := &GB28181API{cfg: &conf.SIP{ID: "34020000002000000001"}}
	ssrc, release, err := api.reserveSSRC(0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !strings.HasPrefix(ssrc, "020000") {
		t.Fatalf("derived-domain SSRC = %q", ssrc)
	}
}

func TestGetSSRCRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name       string
		api        *GB28181API
		streamType int
	}{
		{name: "nil API", api: nil, streamType: 0},
		{name: "nil config", api: &GB28181API{}, streamType: 0},
		{name: "short domain", api: &GB28181API{cfg: &conf.SIP{Domain: "3402"}}, streamType: 0},
		{name: "non-numeric domain", api: &GB28181API{cfg: &conf.SIP{Domain: "local.test"}}, streamType: 0},
		{name: "stream type", api: &GB28181API{cfg: &conf.SIP{Domain: "3402000000"}}, streamType: 2},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := test.api.reserveSSRC(test.streamType); err == nil {
				t.Fatal("getSSRC succeeded, want error")
			}
		})
	}
}

func TestSSRCAllocatorUsesFullDomainSpaceWithoutDuplicateSuffixes(t *testing.T) {
	var allocator ssrcAllocator
	releases := make([]func(), 0, gbSSRCSuffixCount)
	seen := make(map[string]struct{}, gbSSRCSuffixCount)
	for index := 0; index < gbSSRCSuffixCount; index++ {
		ssrc, release, err := allocator.reserve("3402000000", index%2)
		if err != nil {
			t.Fatalf("reserve %d: %v", index, err)
		}
		suffix := ssrc[6:]
		if _, duplicate := seen[suffix]; duplicate {
			t.Fatalf("duplicate SSRC suffix %s at allocation %d", suffix, index)
		}
		seen[suffix] = struct{}{}
		releases = append(releases, release)
	}
	if len(seen) != gbSSRCSuffixCount {
		t.Fatalf("unique suffixes = %d, want %d", len(seen), gbSSRCSuffixCount)
	}
	if _, _, err := allocator.reserve("3402000000", 0); err == nil || !strings.Contains(err.Error(), "space exhausted") {
		t.Fatalf("exhausted reserve error = %v", err)
	}
	for _, release := range releases {
		release()
	}
}

func TestSSRCAllocatorReleaseIsIdempotentAndGenerationBound(t *testing.T) {
	allocator := ssrcAllocator{domains: map[string]*ssrcDomainState{
		"3402000000": {next: gbSSRCSuffixCount - 1, inUse: make(map[uint16]uint64)},
	}}
	oldSSRC, oldRelease, err := allocator.reserve("3402000000", 0)
	if err != nil {
		t.Fatal(err)
	}
	oldRelease()
	allocator.mu.Lock()
	allocator.domains["3402000000"].next = gbSSRCSuffixCount - 1
	allocator.mu.Unlock()
	newSSRC, newRelease, err := allocator.reserve("3402000000", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer newRelease()
	if oldSSRC[6:] != newSSRC[6:] {
		t.Fatalf("reused suffix = %s, want %s", newSSRC[6:], oldSSRC[6:])
	}
	oldRelease()
	allocator.mu.Lock()
	_, stillReserved := allocator.domains["3402000000"].inUse[gbSSRCSuffixCount-1]
	allocator.mu.Unlock()
	if !stillReserved {
		t.Fatal("stale release cleared the replacement SSRC generation")
	}
}

func TestSSRCAllocatorConcurrentReservationsAreUnique(t *testing.T) {
	var allocator ssrcAllocator
	const workers = 256
	type result struct {
		ssrc    string
		release func()
		err     error
	}
	results := make(chan result, workers)
	var start sync.WaitGroup
	start.Add(1)
	var workersDone sync.WaitGroup
	workersDone.Add(workers)
	for index := 0; index < workers; index++ {
		go func(streamType int) {
			defer workersDone.Done()
			start.Wait()
			ssrc, release, err := allocator.reserve("3402000000", streamType)
			results <- result{ssrc: ssrc, release: release, err: err}
		}(index % 2)
	}
	start.Done()
	workersDone.Wait()
	close(results)
	seen := make(map[string]struct{}, workers)
	for item := range results {
		if item.err != nil {
			t.Fatal(item.err)
		}
		suffix := item.ssrc[6:]
		if _, duplicate := seen[suffix]; duplicate {
			t.Fatal(fmt.Sprintf("concurrent duplicate SSRC suffix %s", suffix))
		}
		seen[suffix] = struct{}{}
		item.release()
	}
}
