package gbs

import (
	"slices"
	"testing"
)

func TestCapabilityNamesByVersion(t *testing.T) {
	if names := GBVersion10.CapabilityNames(); !slices.Contains(names, "media_status") ||
		!slices.Contains(names, "directory_notify") || slices.Contains(names, "config_query") {
		t.Fatalf("1.0 capabilities = %v", names)
	}
	if names := GBVersion11.CapabilityNames(); !slices.Contains(names, "direct_tcp_download") || slices.Contains(names, "rtp_over_tcp") {
		t.Fatalf("1.1 capabilities = %v", names)
	}
	if names := GBVersion20.CapabilityNames(); !slices.Contains(names, "rtp_over_tcp") || slices.Contains(names, "snapshot") {
		t.Fatalf("2.0 capabilities = %v", names)
	}
	if names := GBVersion30.CapabilityNames(); !slices.Contains(names, "snapshot") || !slices.Contains(names, "upgrade") {
		t.Fatalf("3.0 capabilities = %v", names)
	}
}

func TestUnsupportedFeatureUpdatesDiagnostics(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	if err := api.requireGBFeature(gb10DeviceID, "语音广播", func(c GBCapabilities) bool { return c.VoiceBroadcast }); err == nil {
		t.Fatal("1.0 Broadcast should be rejected")
	}
	if memory.persistent.Ext.GBLastUnsupportedCommand != "语音广播" ||
		memory.persistent.Ext.GBLastUnsupportedVersion != string(GBVersion10) ||
		memory.persistent.Ext.GBLastUnsupportedUpdatedAt == 0 {
		t.Fatalf("diagnostics = %+v", memory.persistent.Ext)
	}
}
