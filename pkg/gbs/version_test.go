package gbs

import (
	"slices"
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
)

func TestRefreshDeviceVersionUpdatesCapabilitySnapshot(t *testing.T) {
	device := &ipc.Device{Ext: ipc.DeviceExt{GBManualVersion: "1.1"}}
	(&Server{}).RefreshDeviceVersion(device)
	if !slices.Contains(device.Ext.GBVersionCapabilities, "direct_tcp_download") ||
		slices.Contains(device.Ext.GBVersionCapabilities, "rtp_over_tcp") {
		t.Fatalf("1.1 capability snapshot = %v", device.Ext.GBVersionCapabilities)
	}
}

func TestParseGBProtocolVersion(t *testing.T) {
	tests := []struct {
		input string
		want  GBProtocolVersion
	}{
		{"1.0", GBVersion10},
		{"2011", GBVersion10},
		{"1.1", GBVersion11},
		{"2014", GBVersion11},
		{"2011-supplement-2014", GBVersion11},
		{"2.0", GBVersion20},
		{"2016", GBVersion20},
		{"3.0", GBVersion30},
		{"2022", GBVersion30},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, ok := ParseGBProtocolVersion(tt.input)
			if !ok || got != tt.want {
				t.Fatalf("ParseGBProtocolVersion(%q) = %q, %v; want %q, true", tt.input, got, ok, tt.want)
			}
		})
	}

	if got, ok := ParseGBProtocolVersion("9.9"); ok || got != "" {
		t.Fatalf("unknown version = %q, %v; want empty, false", got, ok)
	}
}

func TestGBProtocolVersionCapabilities(t *testing.T) {
	if GBVersion10.Capabilities().ConfigQuery {
		t.Fatal("1.0 must not enable ConfigDownload")
	}
	if !GBVersion11.Capabilities().DirectTCPDownload {
		t.Fatal("1.1 must enable direct TCP download capability")
	}
	if GBVersion11.Capabilities().RTPOverTCP {
		t.Fatal("1.1 direct TCP download must not be treated as RTP over TCP")
	}
	if GBVersion11.Capabilities().VoiceIntercom {
		t.Fatal("1.1 broadcast capability must not enable 2.0 voice intercom")
	}
	if !GBVersion20.Capabilities().RTPOverTCP {
		t.Fatal("2.0 must enable RTP over TCP")
	}
	if !GBVersion30.Capabilities().Snapshot || !GBVersion30.Capabilities().Upgrade {
		t.Fatal("3.0 must enable 2022 snapshot and upgrade capabilities")
	}
}

func TestGBProtocolVersionOrdering(t *testing.T) {
	if !GBVersion11.AtLeast(GBVersion10) {
		t.Fatal("1.1 must be at least 1.0")
	}
	if GBVersion11.AtLeast(GBVersion20) {
		t.Fatal("1.1 must not be at least 2.0")
	}
	if GBProtocolVersion("9.9").AtLeast(GBVersion10) {
		t.Fatal("unknown version must not satisfy minimum version")
	}
}

func TestApplyGBProtocolVersionPrecedence(t *testing.T) {
	ext := ipc.DeviceExt{
		GBManualVersion:   "2.0",
		GBDeclaredVersion: "1.1",
	}
	if got := applyGBProtocolVersion(&ext, "1.0"); got != GBVersion20 {
		t.Fatalf("effective version = %q; want 2.0", got)
	}
	if ext.GBDeclaredVersion != "1.0" || ext.GBEffectiveVersion != "2.0" || ext.GBVersionSource != "manual" || ext.GBVersion != "2016" {
		t.Fatalf("unexpected persisted version state: %+v", ext)
	}
}

func TestApplyGBProtocolVersionKeepsDeclarationWhenHeaderMissing(t *testing.T) {
	ext := ipc.DeviceExt{
		GBVersion:          "2014",
		GBDeclaredVersion:  "1.1",
		GBEffectiveVersion: "1.1",
		GBVersionSource:    "header",
	}
	if got := applyGBProtocolVersion(&ext, ""); got != GBVersion11 {
		t.Fatalf("effective version = %q; want 1.1", got)
	}
	if ext.GBDeclaredVersion != "1.1" {
		t.Fatalf("missing header cleared declaration: %+v", ext)
	}
}
