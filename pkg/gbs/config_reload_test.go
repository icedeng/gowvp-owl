package gbs

import (
	"fmt"
	"sync"
	"testing"

	"github.com/gowvp/owl/internal/conf"
)

func TestGB28181ConfigSnapshotConcurrentReload(t *testing.T) {
	initial := conf.DefaultConfig().Sip
	api := &GB28181API{cfg: &initial}

	var wg sync.WaitGroup
	for reader := 0; reader < 8; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				cfg := api.configSnapshot()
				if cfg == nil || cfg.ID == "" || cfg.GetDomain() == "" {
					t.Errorf("invalid config snapshot: %+v", cfg)
					return
				}
			}
		}()
	}
	for writer := 0; writer < 2; writer++ {
		writer := writer
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				next := initial
				next.Password = fmt.Sprintf("password-%d-%d", writer, i)
				api.setConfig(next)
			}
		}()
	}
	wg.Wait()
}
