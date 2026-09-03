package gbs

import (
	"encoding/xml"
	"errors"
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

func TestApplyGBVersionProfileDoesNotPublishRuntimeBeforePersistence(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBProfile(GBVersion10, nil)
	server := &Server{memoryStorer: memory}
	device := &ipc.Device{
		DeviceID: gb10DeviceID,
		Ext: ipc.DeviceExt{
			GBManualVersion:        string(GBVersion30),
			GBDisabledCapabilities: []string{"snapshot"},
		},
	}

	if version := ApplyGBVersionProfile(device); version != GBVersion30 {
		t.Fatalf("prepared version = %s, want %s", version, GBVersion30)
	}
	if slices.Contains(device.Ext.GBVersionCapabilities, "snapshot") ||
		!slices.Contains(device.Ext.GBVersionCapabilities, "upgrade") {
		t.Fatalf("prepared capabilities = %v", device.Ext.GBVersionCapabilities)
	}
	if got := memory.runtime.GBVersion(); got != string(GBVersion10) {
		t.Fatalf("validation published runtime version = %s, want %s", got, GBVersion10)
	}

	server.RefreshDeviceVersion(device)
	if got := memory.runtime.GBVersion(); got != string(GBVersion30) {
		t.Fatalf("post-commit refresh version = %s, want %s", got, GBVersion30)
	}
	if !memory.runtime.isCapabilityDisabled("snapshot") {
		t.Fatal("post-commit refresh did not publish disabled capability")
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

func TestCascadeDeviceControlRespectsDisabledCapabilities(t *testing.T) {
	tests := []struct {
		name       string
		capability string
		request    *deviceControlA23Request
	}{
		{name: "upgrade", capability: "upgrade", request: &deviceControlA23Request{DeviceUpgrade: &deviceUpgradeConfig{}}},
		{name: "sdcard", capability: "sdcard", request: &deviceControlA23Request{FormatSDCard: new(int)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, memory := newVersionGateAPI(GBVersion30)
			memory.device.setGBProfile(GBVersion30, []string{test.capability})
			if err := api.validateCascadeDeviceControlOverrides("device", test.request); err == nil {
				t.Fatalf("cascade DeviceControl bypassed disabled %s capability", test.capability)
			}
		})
	}
}

func TestCascadeControlAndConfigRejectOfflineDownstreamBeforeCustomHooks(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion30)
	memory.device.IsOnline = false
	if err := api.validateCascadeDeviceControlOverrides("device", &deviceControlA23Request{RecordCmd: "Record"}); !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("offline cascade DeviceControl error = %v", err)
	}
	request := &DeviceConfigRequest{
		XMLName: xml.Name{Local: "Control"}, CmdType: "DeviceConfig", SN: 1, DeviceID: gb10DeviceID,
		BasicParam: &BasicParam{Name: "camera"},
	}
	if err := api.validateCascadeDeviceConfigOverrides("device", request); !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("offline cascade DeviceConfig error = %v", err)
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

func TestCapabilityNamesAreUniqueCompleteAndStable(t *testing.T) {
	expected := map[GBProtocolVersion][]string{
		GBVersion10: {
			"directory_notify", "media_status", "voice_intercom",
		},
		GBVersion11: {
			"config_query", "config_write", "catalog_extension", "directory_notify", "multi_response", "media_status",
			"voice_broadcast", "voice_intercom", "direct_tcp_download", "download_speed", "drag_zoom_control", "preset_query",
		},
		GBVersion20: {
			"config_query", "config_write", "catalog_extension", "directory_notify", "multi_response", "media_status",
			"voice_broadcast", "voice_intercom", "rtp_over_tcp", "download_speed", "iframe_control", "drag_zoom_control",
			"preset_query", "mobile_position", "home_position",
		},
		GBVersion30: {
			"config_query", "config_write", "catalog_extension", "directory_notify", "multi_response", "media_status",
			"voice_broadcast", "voice_intercom", "rtp_over_tcp", "download_speed", "iframe_control", "drag_zoom_control",
			"preset_query", "mobile_position", "ptz_position", "home_position", "home_position_query", "cruise_track_query",
			"sdcard", "h265", "aac", "snapshot", "upgrade", "target_track",
		},
	}
	declared := make(map[string]struct{}, len(knownGBCapabilityNames))
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		names := version.CapabilityNames()
		if !slices.Equal(names, expected[version]) {
			t.Fatalf("%s capabilities = %v, want stable order %v", version, names, expected[version])
		}
		seen := make(map[string]struct{}, len(names))
		for _, capability := range names {
			if _, exists := seen[capability]; exists {
				t.Fatalf("%s capability %q is listed more than once", version, capability)
			}
			if _, known := knownGBCapabilityNames[capability]; !known {
				t.Fatalf("%s capability %q is not declared as a known capability", version, capability)
			}
			seen[capability] = struct{}{}
			declared[capability] = struct{}{}
		}
	}
	if len(declared) != len(knownGBCapabilityNames) {
		t.Fatalf("capability profiles declare %d known names, want %d: declared=%v known=%v", len(declared), len(knownGBCapabilityNames), declared, knownGBCapabilityNames)
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

func TestHomePositionControlAndQueryUseIndependentVersionGates(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion20)
	if !GBVersion20.Capabilities().HomePosition {
		t.Fatal("2.0 must keep the HomePosition control capability")
	}
	if GBVersion20.Capabilities().HomePositionQuery {
		t.Fatal("2.0 must not enable the HomePositionQuery capability added by 3.0")
	}
	if _, err := api.resolveDeviceQueryCmdType("device", deviceQueryActionHomePositionQuery, ""); err == nil {
		t.Fatal("2.0 device accepted the 3.0 HomePositionQuery command")
	}

	api, memory = newVersionGateAPI(GBVersion30)
	if _, err := api.resolveDeviceQueryCmdType("device", deviceQueryActionHomePositionQuery, ""); err != nil {
		t.Fatalf("3.0 HomePositionQuery rejected: %v", err)
	}
	memory.device.setGBProfile(GBVersion30, []string{"home_position"})
	if _, err := api.resolveDeviceQueryCmdType("device", deviceQueryActionHomePositionQuery, ""); err == nil {
		t.Fatal("legacy home_position disable did not disable HomePositionQuery")
	}
	if slices.Contains(effectiveCapabilityNames(GBVersion30, []string{"home_position"}), "home_position_query") {
		t.Fatal("legacy home_position disable left HomePositionQuery in the effective capability snapshot")
	}
	memory.device.setGBProfile(GBVersion30, []string{"home_position_query"})
	if _, err := api.resolveDeviceQueryCmdType("device", deviceQueryActionHomePositionQuery, ""); err == nil {
		t.Fatal("home_position_query disable was ignored")
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

func TestGBProtocolVersionAliasMethodsMatchCanonicalVersion(t *testing.T) {
	tests := []struct {
		alias     GBProtocolVersion
		canonical GBProtocolVersion
	}{
		{alias: "2011", canonical: GBVersion10},
		{alias: "2014", canonical: GBVersion11},
		{alias: "2011-supplement-2014", canonical: GBVersion11},
		{alias: "2016", canonical: GBVersion20},
		{alias: "2022", canonical: GBVersion30},
	}
	for _, test := range tests {
		t.Run(string(test.alias), func(t *testing.T) {
			if !test.alias.Valid() {
				t.Fatalf("protocol alias %q is not valid", test.alias)
			}
			if test.alias.StandardYear() != test.canonical.StandardYear() ||
				test.alias.StandardName() != test.canonical.StandardName() ||
				test.alias.Capabilities() != test.canonical.Capabilities() ||
				!slices.Equal(test.alias.CapabilityNames(), test.canonical.CapabilityNames()) {
				t.Fatalf("protocol alias %q methods do not match canonical %q", test.alias, test.canonical)
			}
			for _, minimum := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
				if test.alias.AtLeast(minimum) != test.canonical.AtLeast(minimum) {
					t.Fatalf("protocol alias %q ordering differs from canonical %q for minimum %q", test.alias, test.canonical, minimum)
				}
			}
		})
	}
}

func TestDeviceGBVersionAliasesAreStoredCanonically(t *testing.T) {
	device := &Device{}
	device.setGBVersion(GBProtocolVersion("2014"))
	if got := device.GBVersion(); got != string(GBVersion11) {
		t.Fatalf("stored 2014 alias = %q, want %q", got, GBVersion11)
	}
	device.setGBProfile(GBProtocolVersion("2022"), []string{"snapshot"})
	if got := device.GBVersion(); got != string(GBVersion30) || !device.isCapabilityDisabled("snapshot") {
		t.Fatalf("stored 2022 profile = version:%q snapshot-disabled:%v", got, device.isCapabilityDisabled("snapshot"))
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
	if !gb10.MediaStatus || !GBVersion11.Capabilities().MediaStatus || !GBVersion20.Capabilities().MediaStatus || !GBVersion30.Capabilities().MediaStatus {
		t.Fatal("MediaStatus/121 must be enabled for all four protocol profiles")
	}
	if !gb10.DirectoryNotify {
		t.Fatal("1.0 must enable basic Catalog subscription notifications")
	}
	if gb10.MultiResponse {
		t.Fatal("1.0 must not advertise the multi-response capability introduced by the 2014 supplement")
	}
	if !GBVersion11.Capabilities().MultiResponse || !GBVersion20.Capabilities().MultiResponse || !GBVersion30.Capabilities().MultiResponse {
		t.Fatal("multi-response must be advertised for 1.1/2.0/3.0")
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
	if !GBVersion10.Capabilities().VoiceIntercom || !GBVersion11.Capabilities().VoiceIntercom {
		t.Fatal("1.0/1.1 must enable the realtime audio/intercom capability defined by 2011 section 7.2")
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
		!GBVersion30.Capabilities().HomePositionQuery ||
		!GBVersion30.Capabilities().CruiseTrackQuery || !GBVersion30.Capabilities().SDCard ||
		!GBVersion30.Capabilities().AAC || !GBVersion30.Capabilities().TargetTrack {
		t.Fatal("3.0 must enable 2022 snapshot, upgrade, PTZ, cruise, SD card, AAC and target track capabilities")
	}
	if GBVersion20.Capabilities().PTZPosition {
		t.Fatal("2.0 must not enable the 3.0 PTZ position event")
	}
}

func TestVoiceCompatibilityAndStandardModeVersionGates(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		api, _ := newVersionGateAPI(version)
		if err := api.requireGBFeature("device", "voice_intercom", "语音对讲", func(c GBCapabilities) bool {
			return c.VoiceIntercom
		}); err != nil {
			t.Fatalf("%s compatibility Talk rejected: %v", version, err)
		}
		err := api.requireGBVersionAtLeast("device", string(GBVersion20), "标准双流程语音对讲")
		if version.AtLeast(GBVersion20) && err != nil {
			t.Fatalf("%s standard Talk rejected: %v", version, err)
		}
		if !version.AtLeast(GBVersion20) && err == nil {
			t.Fatalf("%s standard Talk was accepted", version)
		}
	}
}

func TestShouldEnableTCPRTCPVersionMatrix(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		isTCP   bool
		want    bool
	}{
		{name: "2011 UDP", version: GBVersion10},
		{name: "2011 TCP compatibility input", version: GBVersion10, isTCP: true},
		{name: "2014 UDP", version: GBVersion11},
		{name: "2014 direct TCP is not RTP over TCP", version: GBVersion11, isTCP: true},
		{name: "2016 UDP", version: GBVersion20},
		{name: "2016 TCP", version: GBVersion20, isTCP: true, want: true},
		{name: "2022 UDP", version: GBVersion30},
		{name: "2022 TCP", version: GBVersion30, isTCP: true, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldEnableTCPRTCP(test.version, test.isTCP); got != test.want {
				t.Fatalf("shouldEnableTCPRTCP(%s, %v) = %v, want %v", test.version, test.isTCP, got, test.want)
			}
		})
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
