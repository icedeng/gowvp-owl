package owl

import (
	"context"
	"fmt"
)

func (h *Host) SetFaultSink(sink func(context.Context, string, string, string, string, bool) error) {
	h.mu.Lock()
	h.faultSink = sink
	h.mu.Unlock()
}
func (h *Host) GatewayFault(ctx context.Context, id, name, incident, label string, recovered bool) error {
	h.mu.RLock()
	sink := h.faultSink
	h.mu.RUnlock()
	if sink == nil {
		return fmt.Errorf("event center not ready")
	}
	return sink(ctx, id, name, incident, label, recovered)
}
