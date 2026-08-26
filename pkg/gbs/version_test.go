package gbs

import (
	"slices"
	"sync"
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
)

func TestRefreshDeviceVersionUpdatesCapabilitySnapshot(t *testing.T) {
	device := &ipc.Device{Ext: ipc.DeviceExt{GBManualVersion: "1.1", GBDisabledCapabilities: []string{"direct_tcp_download"}}}
	(&Server{}).RefreshDeviceVersion(device)
	if slices.Contains(device.Ext.GBVersionCapabilities, "direct_tcp_download") ||
		slices.Contains(device.Ext.GBVersionCapabilities, "rtp_over_tcp") {
		t.Fatalf("1.1 capability snapshot = %v", device.Ext.GBVersionCapabilities)
	}
	if !slices.Contains(device.Ext.GBVersionCapabilities, "voice_broadcast") {
		t.Fatalf("unrelated 1.1 capability was removed: %v", device.Ext.GBVersionCapabilities)
	}
}

func TestGBDisabledCapabilitiesNormalizationAndGate(t *testing.T) {
	normalized, err := NormalizeGBDisabledCapabilities([]string{" Voice_Intercom ", "voice_intercom", "ptz_position", "h265", "aac", "target_track"})
	if err != nil || len(normalized) != 5 || normalized[0] != "voice_intercom" || normalized[1] != "ptz_position" || normalized[2] != "h265" || normalized[3] != "aac" || normalized[4] != "target_track" {
		t.Fatalf("normalized capabilities = %v, err = %v", normalized, err)
	}
	if _, err := NormalizeGBDisabledCapabilities([]string{"voice_typo"}); err == nil {
		t.Fatal("unknown capability must be rejected")
	}

	api, memory := newVersionGateAPI(GBVersion20)
	memory.device.setGBProfile(GBVersion20, []string{"voice_intercom", "directory_notify"})
	if err := api.requireGBFeature("device", "voice_intercom", "语音对讲", func(c GBCapabilities) bool {
		return c.VoiceIntercom
	}); err == nil {
		t.Fatal("device-level disabled voice_intercom was accepted")
	}
	if err := api.requireMediaTransport("device", 1, "实时点播"); err != nil {
		t.Fatalf("unrelated RTP over TCP capability rejected: %v", err)
	}
	if err := api.Subscribe(t.Context(), &SubscribeInput{DeviceID: "device", Event: "Catalog"}); err == nil {
		t.Fatal("device-level disabled directory_notify subscription was accepted")
	}
	if err := api.Subscribe(t.Context(), &SubscribeInput{DeviceID: "device", Event: "PTZPosition"}); err == nil {
		t.Fatal("2.0 device accepted the 3.0 PTZ position subscription")
	}
	api, memory = newVersionGateAPI(GBVersion30)
	memory.device.setGBProfile(GBVersion30, []string{"ptz_position", "sdcard", "target_track"})
	if err := api.Subscribe(t.Context(), &SubscribeInput{DeviceID: "device", Event: "PTZPosition"}); err == nil {
		t.Fatal("device-level disabled PTZ position subscription was accepted")
	}
	pan := 1.0
	if err := api.fillDeviceControlRequest("device", deviceControlActionPTZPrecise, &DeviceControlInput{PTZPrecise: &PTZPreciseParam{Pan: &pan}}, &deviceControlA23Request{}); err == nil {
		t.Fatal("device-level disabled precise PTZ control was accepted")
	}
	if _, err := api.resolveDeviceQueryCmdType("device", deviceQueryActionSDCardStatus, ""); err == nil {
		t.Fatal("device-level disabled SD card query was accepted")
	}
	if err := api.requireGBFeature("device", "target_track", "目标跟踪", func(c GBCapabilities) bool {
		return c.TargetTrack
	}); err == nil {
		t.Fatal("device-level disabled target tracking was accepted")
	}
}

func TestAllDeclaredGBCapabilitiesCanBeDisabled(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		for _, capability := range version.CapabilityNames() {
			normalized, err := NormalizeGBDisabledCapabilities([]string{capability})
			if err != nil || len(normalized) != 1 || normalized[0] != capability {
				t.Errorf("%s capability %q cannot be disabled: normalized=%v err=%v", version, capability, normalized, err)
			}
		}
	}
}

func TestDeviceSupportsGBFeatureUsesDisabledCapabilityGate(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion11)
	memory.device.setGBProfile(GBVersion11, []string{"config_query"})
	if api.deviceSupportsGBFeature("device", "config_query", GBVersion11, func(c GBCapabilities) bool { return c.ConfigQuery }) {
		t.Fatal("registration ConfigDownload gate ignored disabled config_query capability")
	}
	memory.device.setGBProfile(GBVersion11, nil)
	if !api.deviceSupportsGBFeature("device", "config_query", GBVersion11, func(c GBCapabilities) bool { return c.ConfigQuery }) {
		t.Fatal("registration ConfigDownload gate rejected enabled config_query capability")
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

func TestGBIdentifiersRequireASCIIDigits(t *testing.T) {
	if err := filterUnknowDevices("١١١١١١١١١١"); err == nil {
		t.Fatal("REGISTER accepted non-ASCII numeric device ID")
	}
	if allDecimalDigits("１２３４５６") {
		t.Fatal("Catalog accepted non-ASCII numeric ID")
	}
	if validGBSSRC("١١١١١") {
		t.Fatal("SDP accepted non-ASCII numeric SSRC")
	}
}

func TestGBProtocolVersionCapabilities(t *testing.T) {
	gb10 := GBVersion10.Capabilities()
	if !gb10.MediaStatus {
		t.Fatal("1.0 must enable MediaStatus/121")
	}
	if !gb10.DirectoryNotify {
		t.Fatal("1.0 must enable basic Catalog subscription notifications")
	}
	if !gb10.MultiResponse {
		t.Fatal("1.0 must enable Catalog and RecordInfo multi-response collection")
	}
	if gb10.ConfigQuery {
		t.Fatal("1.0 must not enable ConfigDownload")
	}
	if !GBVersion11.Capabilities().DirectTCPDownload {
		t.Fatal("1.1 must enable direct TCP download capability")
	}
	if GBVersion10.Capabilities().DownloadSpeed || !GBVersion11.Capabilities().DownloadSpeed || !GBVersion20.Capabilities().DownloadSpeed || !GBVersion30.Capabilities().DownloadSpeed {
		t.Fatal("download speed must be enabled for 1.1/2.0/3.0 only")
	}
	if GBVersion11.Capabilities().RTPOverTCP {
		t.Fatal("1.1 direct TCP download must not be treated as RTP over TCP")
	}
	if GBVersion11.Capabilities().VoiceIntercom {
		t.Fatal("1.1 broadcast capability must not enable 2.0 voice intercom")
	}
	if GBVersion11.Capabilities().IFrameControl {
		t.Fatal("1.1 must not enable the 2.0 IFrame command")
	}
	if !GBVersion11.Capabilities().PresetQuery {
		t.Fatal("1.1 must enable PresetQuery")
	}
	if !GBVersion20.Capabilities().RTPOverTCP {
		t.Fatal("2.0 must enable RTP over TCP")
	}
	if !GBVersion20.Capabilities().IFrameControl {
		t.Fatal("2.0 must enable IFrame control")
	}
	if !GBVersion30.Capabilities().Snapshot || !GBVersion30.Capabilities().Upgrade || !GBVersion30.Capabilities().PTZPosition ||
		!GBVersion30.Capabilities().CruiseTrackQuery || !GBVersion30.Capabilities().SDCard ||
		!GBVersion30.Capabilities().AAC || !GBVersion30.Capabilities().TargetTrack {
		t.Fatal("3.0 must enable 2022 snapshot, upgrade, PTZ, cruise, SD card, AAC and target track capabilities")
	}
	if GBVersion20.Capabilities().PTZPosition {
		t.Fatal("2.0 must not enable the 3.0 PTZ position event")
	}
}

func TestDeviceGBVersionConcurrentReadWrite(t *testing.T) {
	device := &Device{gbVersion: string(GBVersion10)}
	channel := &Channel{device: device}
	versions := []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(offset int) {
			defer wg.Done()
			for n := 0; n < 1000; n++ {
				device.setGBVersion(versions[(n+offset)%len(versions)])
			}
		}(i)
		go func() {
			defer wg.Done()
			for n := 0; n < 1000; n++ {
				if version, ok := ParseGBProtocolVersion(device.GBVersion()); !ok || version == "" {
					t.Errorf("invalid device version %q", device.GBVersion())
					return
				}
				if version, ok := ParseGBProtocolVersion(channel.GBVersion()); !ok || version == "" {
					t.Errorf("invalid channel version %q", channel.GBVersion())
					return
				}
			}
		}()
	}
	wg.Wait()
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
