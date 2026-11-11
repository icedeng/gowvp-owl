package gbs

import (
	"context"
	"strings"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

// InvalidateExternalIngress 只注销指定可信作用域的运行态，迟到关闭不会影响新入口。
func (s *Server) InvalidateExternalIngress(ctx context.Context, prefix string) error {
	var first error
	if s.cascade != nil {
		s.cascade.mu.RLock()
		workers := make([]*cascadeWorker, 0, len(s.cascade.items))
		for _, worker := range s.cascade.items {
			workers = append(workers, worker)
		}
		s.cascade.mu.RUnlock()
		for _, worker := range workers {
			worker.mu.Lock()
			affected := worker.externalRegisteredScope != "" && strings.HasPrefix(worker.externalRegisteredScope, prefix)
			if affected {
				worker.externalRegisteredScope = ""
				worker.status.Registered = false
				worker.status.State = "disconnected"
				worker.status.LastError = "网关信令入口已失效，等待重新注册"
			}
			worker.mu.Unlock()
			if affected {
				if err := s.DrainExternalPlatform(ctx, worker.platform.subjectID); err != nil && first == nil {
					first = err
				}
			}
		}
	}
	s.memoryStorer.RangeDevices(func(code string, device *Device) bool {
		if ctx.Err() != nil {
			first = ctx.Err()
			return false
		}
		if !strings.HasPrefix(sip.RoutingScope(device.runtimeSnapshot().Conn), prefix) {
			return true
		}
		unlock := s.gb.lockRegisterOperation(code)
		current := device.runtimeSnapshot()
		if strings.HasPrefix(sip.RoutingScope(current.Conn), prefix) {
			if e := s.gb.setDeviceOfflineContext(ctx, code, true, func(persisted *ipc.Device) error { persisted.IsOnline = false; return nil }); e != nil && first == nil {
				first = e
			}
		}
		unlock()
		return true
	})
	return first
}
