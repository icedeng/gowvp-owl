package gbs

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
)

func TestCapabilityNamesByVersion(t *testing.T) {
	if names := GBVersion10.CapabilityNames(); !slices.Contains(names, "media_status") ||
		!slices.Contains(names, "directory_notify") || !slices.Contains(names, "voice_intercom") || slices.Contains(names, "config_query") ||
		slices.Contains(names, "multi_response") {
		t.Fatalf("1.0 capabilities = %v", names)
	}
	if names := GBVersion11.CapabilityNames(); !slices.Contains(names, "media_status") ||
		!slices.Contains(names, "direct_tcp_download") || !slices.Contains(names, "multi_response") ||
		!slices.Contains(names, "voice_intercom") || slices.Contains(names, "rtp_over_tcp") {
		t.Fatalf("1.1 capabilities = %v", names)
	}
	if names := GBVersion20.CapabilityNames(); !slices.Contains(names, "rtp_over_tcp") || slices.Contains(names, "snapshot") {
		t.Fatalf("2.0 capabilities = %v", names)
	}
	if names := GBVersion30.CapabilityNames(); !slices.Contains(names, "snapshot") || !slices.Contains(names, "upgrade") ||
		!slices.Contains(names, "aac") || !slices.Contains(names, "target_track") {
		t.Fatalf("3.0 capabilities = %v", names)
	}
}

func TestEffectiveCapabilityNamesExcludeDeviceOverrides(t *testing.T) {
	names := effectiveCapabilityNames(GBVersion30, []string{"snapshot", "upgrade"})
	if slices.Contains(names, "snapshot") || slices.Contains(names, "upgrade") || !slices.Contains(names, "voice_intercom") {
		t.Fatalf("effective capabilities = %v", names)
	}
}

func TestUnsupportedFeatureUpdatesDiagnostics(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	if err := api.requireGBFeature(gb10DeviceID, "voice_broadcast", "语音广播", func(c GBCapabilities) bool { return c.VoiceBroadcast }); err == nil {
		t.Fatal("1.0 Broadcast should be rejected")
	}
	if memory.persistent.Ext.GBLastUnsupportedCommand != "语音广播" ||
		memory.persistent.Ext.GBLastUnsupportedVersion != string(GBVersion10) ||
		memory.persistent.Ext.GBLastUnsupportedUpdatedAt == 0 {
		t.Fatalf("diagnostics = %+v", memory.persistent.Ext)
	}
}

func TestRecordUnsupportedFeatureReturnsPersistenceError(t *testing.T) {
	persistErr := errors.New("diagnostics persistence failed")
	memory := &diagnosticsFailureMemory{
		flowMemory: newFlowMemory(gb10DeviceID),
		changeErr:  persistErr,
	}
	api := &GB28181API{svr: &Server{memoryStorer: memory}}

	if err := api.recordUnsupportedGBFeature(gb10DeviceID, "语音广播", GBVersion10); !errors.Is(err, persistErr) {
		t.Fatalf("recordUnsupportedGBFeature error = %v, want %v", err, persistErr)
	}
	if err := api.requireGBFeature(gb10DeviceID, "voice_broadcast", "语音广播", func(c GBCapabilities) bool {
		return c.VoiceBroadcast
	}); err == nil || !strings.Contains(err.Error(), "不受当前协议档案") {
		t.Fatalf("requireGBFeature error = %v, want unsupported feature error", err)
	}
}

type diagnosticsFailureMemory struct {
	*flowMemory
	changeErr error
}

func (m *diagnosticsFailureMemory) Change(string, func(*ipc.Device) error, func(*Device)) error {
	return m.changeErr
}
