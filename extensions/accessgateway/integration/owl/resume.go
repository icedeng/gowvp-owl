package owl

import (
	"context"
	"fmt"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/internal/core/ipc"
)

func (h *Host) ResumeFlow(ctx context.Context, route core.IngressRoute, f protocol.ResumeFlow) error {
	if h.sip == nil {
		return fmt.Errorf("SIP server not ready")
	}
	var device ipc.Device
	if err := h.db.WithContext(ctx).Where("id = ?", f.SubjectID).First(&device).Error; err != nil {
		return err
	}
	if subjectKind(device.DeviceID) != "device" {
		return fmt.Errorf("only device flow can resume")
	}
	expires, err := time.Parse(time.RFC3339Nano, f.ExpiresAt)
	if err != nil {
		return err
	}
	seen, err := time.Parse(time.RFC3339Nano, f.LastSeenAt)
	if err != nil {
		return err
	}
	binding, err := h.repo.Binding(ctx, device.ID)
	if err != nil {
		return err
	}
	if err := validateResumeBinding(device.ID, route, binding, f, time.Now()); err != nil {
		return err
	}

	conn := newConnection(h, core.SignalIngress{Ingress: route, Route: f.Route})
	conn.resume = &f
	return h.sip.ResumeExternalDevice(ctx, device.DeviceID, conn, expires, seen)
}

func validateResumeBinding(subject string, route core.IngressRoute, binding store.Binding, f protocol.ResumeFlow, now time.Time) error {
	expires, e1 := time.Parse(time.RFC3339Nano, f.ExpiresAt)
	seen, e2 := time.Parse(time.RFC3339Nano, f.LastSeenAt)
	if e1 != nil || e2 != nil || !expires.After(now) || seen.After(now.Add(30*time.Second)) || now.Sub(seen) > 90*time.Second {
		return fmt.Errorf("resume flow expired or heartbeat stale")
	}
	if subject != f.SubjectID || binding.SubjectID != subject || binding.IngressType != "gateway" || binding.GatewayID == nil || *binding.GatewayID != route.GatewayPK || binding.LastGeneration == nil || *binding.LastGeneration != route.Generation || binding.LastFlowID == nil || *binding.LastFlowID != f.Route.FlowID || binding.BindingRevision < 1 || protocol.Number(binding.BindingRevision) != f.BindingRevision || binding.RegisteredUntil == nil || !now.Before(*binding.RegisteredUntil) || expires.After(binding.RegisteredUntil.Add(time.Second)) {
		return fmt.Errorf("resume binding mismatch or expired")
	}
	return nil
}
