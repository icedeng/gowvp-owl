package ipccache

import (
	"sync"
	"testing"

	"github.com/gowvp/owl/pkg/gbs"
)

func TestRuntimeCacheRejectsNilAndRepairsLegacyNil(t *testing.T) {
	cache := NewCache(nil)
	cache.Store("empty", nil)
	cache.LoadOrStore("empty", nil)
	if _, exists := cache.devices.Load("empty"); exists {
		t.Fatal("nil runtime was cached")
	}

	// 模拟旧版本初始化失败后留下的空运行态。
	cache.devices.Store("legacy", nil)
	if _, exists := cache.Load("legacy"); exists {
		t.Fatal("nil runtime was reported as available")
	}
	var group sync.WaitGroup
	candidates := make([]*gbs.Device, 16)
	for i := range candidates {
		candidates[i] = &gbs.Device{}
		group.Go(func() { cache.LoadOrStore("legacy", candidates[i]) })
	}
	group.Wait()
	runtime, exists := cache.Load("legacy")
	if !exists || runtime == nil {
		t.Fatal("registration did not replace the legacy nil runtime")
	}
	cache.LoadOrStore("legacy", &gbs.Device{})
	cache.Store("legacy", nil)
	if current, _ := cache.Load("legacy"); current != runtime {
		t.Fatal("existing runtime was overwritten")
	}
}
