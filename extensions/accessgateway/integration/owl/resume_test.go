package owl

import (
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"testing"
	"time"
)

func TestResumeOnlySameAuthenticatedGatewayFlowBeforeExpiry(t *testing.T) {
	now := time.Now().UTC()
	until := now.Add(time.Hour)
	pk, generation, flow := "pk", "generation", "flow"
	binding := store.Binding{BindingFields: store.BindingFields{SubjectID: "device", IngressType: "gateway", GatewayID: &pk, LastGeneration: &generation, LastFlowID: &flow, BindingRevision: 4, RegisteredUntil: &until}}
	route := core.IngressRoute{GatewayPK: pk, Generation: generation}
	record := protocol.ResumeFlow{SubjectID: "device", BindingRevision: "4", Route: protocol.Route{FlowID: flow}, ExpiresAt: until.Format(time.RFC3339Nano), LastSeenAt: now.Format(time.RFC3339Nano)}
	if err := validateResumeBinding("device", route, binding, record, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*core.IngressRoute, *store.Binding, *protocol.ResumeFlow)
	}{
		{"other gateway", func(r *core.IngressRoute, _ *store.Binding, _ *protocol.ResumeFlow) { r.GatewayPK = "other" }},
		{"B restarted", func(r *core.IngressRoute, _ *store.Binding, _ *protocol.ResumeFlow) { r.Generation = "new" }},
		{"other device", func(_ *core.IngressRoute, _ *store.Binding, f *protocol.ResumeFlow) { f.SubjectID = "other" }},
		{"stale revision", func(_ *core.IngressRoute, _ *store.Binding, f *protocol.ResumeFlow) { f.BindingRevision = "3" }},
		{"other flow", func(_ *core.IngressRoute, _ *store.Binding, f *protocol.ResumeFlow) { f.Route.FlowID = "other" }},
		{"expired registration", func(_ *core.IngressRoute, b *store.Binding, _ *protocol.ResumeFlow) {
			at := now.Add(-time.Second)
			b.RegisteredUntil = &at
		}},
		{"extended TTL", func(_ *core.IngressRoute, _ *store.Binding, f *protocol.ResumeFlow) {
			f.ExpiresAt = until.Add(time.Minute).Format(time.RFC3339Nano)
		}},
		{"stale heartbeat", func(_ *core.IngressRoute, _ *store.Binding, f *protocol.ResumeFlow) {
			f.LastSeenAt = now.Add(-2 * time.Minute).Format(time.RFC3339Nano)
		}},
		{"future heartbeat", func(_ *core.IngressRoute, _ *store.Binding, f *protocol.ResumeFlow) {
			f.LastSeenAt = now.Add(time.Minute).Format(time.RFC3339Nano)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, b, f := route, binding, record
			tc.change(&r, &b, &f)
			if validateResumeBinding("device", r, b, f, now) == nil {
				t.Fatal("invalid recovery accepted")
			}
		})
	}
}
