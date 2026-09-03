package gbs

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func float64Pointer(value float64) *float64 { return &value }
func intPointer(value int) *int             { return &value }

func TestPTZControlPriorityVersionMatrix(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion10)
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11} {
		t.Run(version.StandardName(), func(t *testing.T) {
			memory.device.setGBVersion(version)
			request := &deviceControlA23Request{CmdType: ptzCmdTypeDeviceControl, SN: 1, DeviceID: "device"}
			if err := api.fillDeviceControlRequest("device", deviceControlActionCameraControl, &DeviceControlInput{
				PTZCmd: "A50F0100000000B5", ControlPriority: intPointer(5),
			}, request); err != nil {
				t.Fatalf("ControlPriority rejected: %v", err)
			}
			body, err := sip.XMLEncode(request)
			if err != nil {
				t.Fatal(err)
			}
			if text := string(body); !strings.Contains(text, "<Info><ControlPriority>5</ControlPriority></Info>") {
				t.Fatalf("DeviceControl XML = %s", text)
			}
		})
	}
	for _, version := range []GBProtocolVersion{GBVersion20, GBVersion30} {
		t.Run(version.StandardName()+" rejects legacy priority", func(t *testing.T) {
			memory.device.setGBVersion(version)
			if err := api.fillDeviceControlRequest("device", deviceControlActionCameraControl, &DeviceControlInput{
				PTZCmd: "A50F0100000000B5", ControlPriority: intPointer(5),
			}, &deviceControlA23Request{}); err == nil {
				t.Fatal("legacy ControlPriority was accepted")
			}
		})
	}
}

func TestDeviceControlRecordStreamNumberVersionMatrix(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion10)
	actions := []string{deviceControlActionRecordStart, deviceControlActionRecordStop}
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardName()+" default stream", func(t *testing.T) {
			memory.device.setGBVersion(version)
			for _, action := range actions {
				request := &deviceControlA23Request{CmdType: ptzCmdTypeDeviceControl, SN: 1, DeviceID: "device"}
				if err := api.fillDeviceControlRequest("device", action, &DeviceControlInput{}, request); err != nil {
					t.Fatalf("default stream rejected for %s: %v", action, err)
				}
				body, err := sip.XMLEncode(request)
				if err != nil {
					t.Fatal(err)
				}
				if request.StreamNumber != nil || strings.Contains(string(body), "<StreamNumber>") {
					t.Fatalf("protocol %s emitted optional default StreamNumber: %s", version, body)
				}
			}
		})
	}
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20} {
		t.Run(version.StandardName()+" rejects explicit stream", func(t *testing.T) {
			memory.device.setGBVersion(version)
			for _, action := range actions {
				if err := api.fillDeviceControlRequest("device", action, &DeviceControlInput{StreamNumber: 1}, &deviceControlA23Request{}); err == nil {
					t.Fatalf("protocol %s accepted StreamNumber for %s", version, action)
				}
			}
		})
	}
	memory.device.setGBVersion(GBVersion30)
	for _, streamNumber := range []int{1, 2, 3, 255} {
		for _, action := range actions {
			request := &deviceControlA23Request{CmdType: ptzCmdTypeDeviceControl, SN: 1, DeviceID: "device"}
			if err := api.fillDeviceControlRequest("device", action, &DeviceControlInput{StreamNumber: streamNumber}, request); err != nil {
				t.Fatalf("2022 StreamNumber %d rejected for %s: %v", streamNumber, action, err)
			}
			body, err := sip.XMLEncode(request)
			if err != nil {
				t.Fatal(err)
			}
			if request.StreamNumber == nil || *request.StreamNumber != streamNumber || !strings.Contains(string(body), fmt.Sprintf("<StreamNumber>%d</StreamNumber>", streamNumber)) {
				t.Fatalf("2022 StreamNumber %d XML = %s", streamNumber, body)
			}
		}
	}
	if err := api.fillDeviceControlRequest("device", deviceControlActionRecordStart, &DeviceControlInput{StreamNumber: -1}, &deviceControlA23Request{}); err == nil {
		t.Fatal("negative 2022 StreamNumber was accepted")
	}
}

func TestDeviceControlStandardParameterRanges(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion20)
	for _, input := range []*DeviceControlInput{
		{HomePosition: &HomePositionParam{Enabled: intPointer(-1)}},
		{HomePosition: &HomePositionParam{Enabled: intPointer(2)}},
		{HomePosition: &HomePositionParam{Enabled: intPointer(1), PresetIndex: intPointer(-1)}},
		{HomePosition: &HomePositionParam{Enabled: intPointer(1), PresetIndex: intPointer(256)}},
		{HomePosition: &HomePositionParam{Enabled: intPointer(0), ResetTime: intPointer(30)}},
		{HomePosition: &HomePositionParam{Enabled: intPointer(0), PresetIndex: intPointer(1)}},
	} {
		if err := api.fillDeviceControlRequest("device", deviceControlActionHomePosition, input, &deviceControlA23Request{}); err == nil {
			t.Fatalf("invalid HomePosition accepted: %+v", input.HomePosition)
		}
	}
	for _, preset := range []int{0, 255} {
		input := &DeviceControlInput{HomePosition: &HomePositionParam{Enabled: intPointer(1), PresetIndex: intPointer(preset)}}
		if err := api.fillDeviceControlRequest("device", deviceControlActionHomePosition, input, &deviceControlA23Request{}); err != nil {
			t.Fatalf("valid HomePosition preset %d rejected: %v", preset, err)
		}
	}
	if err := api.fillDeviceControlRequest("device", deviceControlActionHomePosition,
		&DeviceControlInput{HomePosition: &HomePositionParam{Enabled: intPointer(0)}}, &deviceControlA23Request{}); err != nil {
		t.Fatalf("valid HomePosition disable rejected: %v", err)
	}
	request := &deviceControlA23Request{}
	if err := api.fillDeviceControlRequest("device", deviceControlActionHomePosition,
		&DeviceControlInput{HomePosition: &HomePositionParam{Enabled: intPointer(1), ResetTime: intPointer(-1)}}, request); err != nil {
		t.Fatalf("standard integer HomePosition reset time rejected: %v", err)
	}
	if request.HomePosition == nil || request.HomePosition.ResetTime == nil || *request.HomePosition.ResetTime != -1 {
		t.Fatalf("HomePosition reset time was not preserved: %+v", request.HomePosition)
	}
	legacyAlarm := &deviceControlA23Request{}
	if err := api.fillDeviceControlRequest("device", deviceControlActionAlarmReset,
		&DeviceControlInput{AlarmMethod: "vendor", AlarmType: "vendor"}, legacyAlarm); err != nil {
		t.Fatalf("2.0 compatible alarm reset extension rejected: %v", err)
	}
	if err := api.fillDeviceControlRequest("device", deviceControlActionCameraControl,
		&DeviceControlInput{PTZCmd: "A50F0100000000B5", PTZCmdParam: &PTZCmdParam{PresetName: "gate"}},
		&deviceControlA23Request{}); err == nil {
		t.Fatal("2.0 must reject 3.0 PTZ command parameters")
	}

	memory.device.setGBVersion(GBVersion30)
	for _, input := range []*DeviceControlInput{
		{PTZPrecise: &PTZPreciseParam{Pan: float64Pointer(-0.01)}},
		{PTZPrecise: &PTZPreciseParam{Pan: float64Pointer(360.01)}},
		{PTZPrecise: &PTZPreciseParam{Pan: float64Pointer(math.NaN())}},
		{PTZPrecise: &PTZPreciseParam{Tilt: float64Pointer(-30.01)}},
		{PTZPrecise: &PTZPreciseParam{Tilt: float64Pointer(90.01)}},
		{PTZPrecise: &PTZPreciseParam{Tilt: float64Pointer(math.Inf(-1))}},
		{PTZPrecise: &PTZPreciseParam{Zoom: float64Pointer(math.Inf(1))}},
	} {
		if err := api.fillDeviceControlRequest("device", deviceControlActionPTZPrecise, input, &deviceControlA23Request{}); err == nil {
			t.Fatalf("invalid PTZPrecise accepted: %+v", input.PTZPrecise)
		}
	}
	valid := &DeviceControlInput{PTZPrecise: &PTZPreciseParam{
		Pan: float64Pointer(360), Tilt: float64Pointer(-30), Zoom: float64Pointer(1),
	}}
	if err := api.fillDeviceControlRequest("device", deviceControlActionPTZPrecise, valid, &deviceControlA23Request{}); err != nil {
		t.Fatalf("valid PTZPrecise boundary rejected: %v", err)
	}
	valid.PTZPrecise.Tilt = float64Pointer(90)
	if err := api.fillDeviceControlRequest("device", deviceControlActionPTZPrecise, valid, &deviceControlA23Request{}); err != nil {
		t.Fatalf("valid PTZPrecise upper tilt boundary rejected: %v", err)
	}
	for _, input := range []*DeviceControlInput{
		{AlarmMethod: "8"},
		{AlarmMethod: "2//5"},
		{AlarmMethod: "0/2"},
		{AlarmMethod: "2/2"},
		{AlarmMethod: "2/5", AlarmType: "1"},
		{AlarmMethod: "1", AlarmType: "1"},
		{AlarmMethod: "2", AlarmType: "6"},
		{AlarmMethod: "5", AlarmType: "14"},
		{AlarmMethod: "6", AlarmType: "3"},
		{AlarmType: "1"},
	} {
		if err := api.fillDeviceControlRequest("device", deviceControlActionAlarmReset, input, &deviceControlA23Request{}); err == nil {
			t.Fatalf("invalid 3.0 alarm reset accepted: %+v", input)
		}
	}
	for _, input := range []*DeviceControlInput{
		{AlarmMethod: "0"},
		{AlarmMethod: "1/2/5/7"},
		{AlarmMethod: "2", AlarmType: "5"},
		{AlarmMethod: "5", AlarmType: "13"},
		{AlarmMethod: "6", AlarmType: "2"},
	} {
		request := &deviceControlA23Request{}
		if err := api.fillDeviceControlRequest("device", deviceControlActionAlarmReset, input, request); err != nil || request.Info == nil {
			t.Fatalf("valid 3.0 alarm reset rejected: %+v, err = %v", input, err)
		}
	}
	for _, length := range []int{32, 33} {
		request := &deviceControlA23Request{}
		err := api.fillDeviceControlRequest("device", deviceControlActionCameraControl,
			&DeviceControlInput{PTZCmd: string(PTZActionCruiseStart), PTZCmdParam: &PTZCmdParam{CruiseTrackName: strings.Repeat("a", length)}}, request)
		if length == 32 && (err != nil || request.PTZCmdParams == nil) {
			t.Fatalf("32-byte cruise track name rejected: %v", err)
		}
		if length == 33 && err == nil {
			t.Fatal("33-byte cruise track name accepted")
		}
	}
}

func TestDeviceControlPTZCommandEncodingAndParams(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	type testCase struct {
		name       string
		input      *DeviceControlInput
		wantByte4  byte
		wantRaw    string
		wantParams bool
		wantErr    bool
	}
	tests := []testCase{
		{name: "set preset action", input: &DeviceControlInput{PTZCmd: "preset_set", PTZPreset: 1}, wantByte4: 0x81},
		{name: "documented preset call", input: &DeviceControlInput{PTZCmd: "preset_call", PTZPreset: 1}, wantByte4: 0x82},
		{name: "raw command compatibility", input: &DeviceControlInput{PTZCmd: "a50f0100000000b5"}, wantRaw: "A50F0100000000B5"},
		{name: "bad checksum", input: &DeviceControlInput{PTZCmd: "A50F0100000000B4"}, wantErr: true},
		{name: "bad version byte", input: &DeviceControlInput{PTZCmd: "A50E0100000000B4"}, wantErr: true},
		{name: "set preset name", input: &DeviceControlInput{PTZCmd: "preset_set", PTZPreset: 1, PTZCmdParam: &PTZCmdParam{PresetName: "entrance"}}, wantByte4: 0x81, wantParams: true},
		{name: "call preset name", input: &DeviceControlInput{PTZCmd: "preset_call", PTZPreset: 1, PTZCmdParam: &PTZCmdParam{PresetName: "entrance"}}, wantErr: true},
		{name: "ordinary PTZ preset name", input: &DeviceControlInput{PTZCmd: "left", PTZCmdParam: &PTZCmdParam{PresetName: "entrance"}}, wantErr: true},
		{name: "ordinary PTZ cruise name", input: &DeviceControlInput{PTZCmd: "left", PTZCmdParam: &PTZCmdParam{CruiseTrackName: "day"}}, wantErr: true},
		{name: "preset PTZ cruise name", input: &DeviceControlInput{PTZCmd: "preset_set", PTZPreset: 1, PTZCmdParam: &PTZCmdParam{CruiseTrackName: "day"}}, wantErr: true},
		{name: "two command names", input: &DeviceControlInput{PTZCmd: "cruise_start", PTZCmdParam: &PTZCmdParam{PresetName: "entrance", CruiseTrackName: "day"}}, wantErr: true},
	}
	for index, action := range []PTZAction{PTZActionCruiseAdd, PTZActionCruiseDel, PTZActionCruiseSpeed, PTZActionCruiseStay, PTZActionCruiseStart} {
		tests = append(tests, testCase{
			name: "cruise name " + string(action),
			input: &DeviceControlInput{
				PTZCmd: string(action), PTZPreset: 1, PTZCmdParam: &PTZCmdParam{CruiseTrackName: "day"},
			},
			wantByte4:  0x84 + byte(index),
			wantParams: true,
		})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := &deviceControlA23Request{}
			err := api.fillDeviceControlRequest("device", deviceControlActionCameraControl, test.input, request)
			if test.wantErr {
				if err == nil {
					t.Fatalf("invalid PTZ input accepted: %+v", test.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("valid PTZ input rejected: %v", err)
			}
			if test.wantRaw != "" && request.PTZCmd != test.wantRaw {
				t.Fatalf("PTZCmd = %s, want %s", request.PTZCmd, test.wantRaw)
			}
			command, err := parsePTZCommand(request.PTZCmd)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantByte4 != 0 && command[3] != test.wantByte4 {
				t.Fatalf("PTZ command byte 4 = 0x%02X, want 0x%02X", command[3], test.wantByte4)
			}
			if test.wantParams != (request.PTZCmdParams != nil) {
				t.Fatalf("PTZCmdParams = %+v, want present %t", request.PTZCmdParams, test.wantParams)
			}
		})
	}
}

func TestDeviceControlPTZCmdParamsPreserveWhitespace(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	tests := []struct {
		name       string
		input      *DeviceControlInput
		wantPreset string
		wantCruise string
	}{
		{
			name:       "preset name",
			input:      &DeviceControlInput{PTZCmd: "preset_set", PTZPreset: 1, PTZCmdParam: &PTZCmdParam{PresetName: " entrance "}},
			wantPreset: " entrance ",
		},
		{
			name:       "cruise track name",
			input:      &DeviceControlInput{PTZCmd: "cruise_start", PTZCmdParam: &PTZCmdParam{CruiseTrackName: " day "}},
			wantCruise: " day ",
		},
		{
			name:       "32-byte cruise track name",
			input:      &DeviceControlInput{PTZCmd: "cruise_start", PTZCmdParam: &PTZCmdParam{CruiseTrackName: " " + strings.Repeat("a", 30) + " "}},
			wantCruise: " " + strings.Repeat("a", 30) + " ",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := &deviceControlA23Request{}
			if err := api.fillDeviceControlRequest("device", deviceControlActionCameraControl, test.input, request); err != nil {
				t.Fatalf("valid PTZCmdParams rejected: %v", err)
			}
			if request.PTZCmdParams == nil || request.PTZCmdParams.PresetName != test.wantPreset || request.PTZCmdParams.CruiseTrackName != test.wantCruise {
				t.Fatalf("PTZCmdParams = %+v", request.PTZCmdParams)
			}
			body, err := sip.XMLEncode(request)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantPreset != "" && !strings.Contains(string(body), "<PresetName>"+test.wantPreset+"</PresetName>") {
				t.Fatalf("PresetName XML = %s", body)
			}
			if test.wantCruise != "" && !strings.Contains(string(body), "<CruiseTrackName>"+test.wantCruise+"</CruiseTrackName>") {
				t.Fatalf("CruiseTrackName XML = %s", body)
			}
		})
	}

	request := &deviceControlA23Request{}
	if err := api.fillDeviceControlRequest("device", deviceControlActionCameraControl,
		&DeviceControlInput{PTZCmd: "preset_set", PTZPreset: 1, PTZCmdParam: &PTZCmdParam{PresetName: " \t "}}, request); err != nil {
		t.Fatalf("blank PTZCmdParams rejected: %v", err)
	}
	if request.PTZCmdParams != nil {
		t.Fatalf("blank PTZCmdParams emitted: %+v", request.PTZCmdParams)
	}

	err := api.fillDeviceControlRequest("device", deviceControlActionCameraControl,
		&DeviceControlInput{PTZCmd: "cruise_start", PTZCmdParam: &PTZCmdParam{CruiseTrackName: " " + strings.Repeat("a", 31) + " "}},
		&deviceControlA23Request{})
	if err == nil {
		t.Fatal("33-byte CruiseTrackName with surrounding whitespace was accepted")
	}
}

func TestNormalizeDeviceControlDocumentedActions(t *testing.T) {
	tests := map[string]string{
		"ptz_cmd":       deviceControlActionCameraControl,
		"teleboot":      deviceControlActionTeleBoot,
		"record_start":  deviceControlActionRecordStart,
		"record_stop":   deviceControlActionRecordStop,
		"guard_set":     deviceControlActionGuardSet,
		"guard_reset":   deviceControlActionGuardReset,
		"alarm_reset":   deviceControlActionAlarmReset,
		"iframe_send":   deviceControlActionIFrameSend,
		"ifame_cmd":     deviceControlActionIFrameSend,
		"drag_zoom_in":  deviceControlActionDragZoomIn,
		"drag_zoom_out": deviceControlActionDragZoomOut,
		"home_position": deviceControlActionHomePosition,
		"ptz_precise":   deviceControlActionPTZPrecise,
		"format_sdcard": deviceControlActionFormatSDCard,
		"target_track":  deviceControlActionTargetTrack,
	}
	for input, expected := range tests {
		if actual := normalizeDeviceControlAction(input); actual != expected {
			t.Fatalf("normalizeDeviceControlAction(%q) = %q, want %q", input, actual, expected)
		}
	}
}

func TestGB20And30FeatureRegression(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion20)
	if err := api.requireGBFeature("device", "voice_intercom", "语音对讲", func(c GBCapabilities) bool { return c.VoiceIntercom }); err != nil {
		t.Fatalf("2.0 voice intercom rejected: %v", err)
	}
	if err := api.requireGBFeature("device", "direct_tcp_download", "直接 TCP", func(c GBCapabilities) bool { return c.DirectTCPDownload }); err == nil {
		t.Fatal("2.0 must not inherit the 1.1 direct TCP profile")
	}
	pan := 12.5
	precise := &DeviceControlInput{PTZPrecise: &PTZPreciseParam{Pan: &pan}}
	if err := api.fillDeviceControlRequest("device", deviceControlActionPTZPrecise, precise, &deviceControlA23Request{}); err == nil {
		t.Fatal("2.0 must reject 3.0 precise PTZ")
	}
	if err := api.fillDeviceControlRequest("device", deviceControlActionFormatSDCard, &DeviceControlInput{SDCardID: 1}, &deviceControlA23Request{}); err == nil {
		t.Fatal("2.0 must reject 3.0 SD card formatting")
	}
	track := &DeviceControlInput{TargetTrack: &TargetTrackParam{Mode: "Manual", TargetArea: &DragZoomParam{Length: 1920, Width: 1080, MidPointX: 960, MidPointY: 540, LengthX: 300, LengthY: 200}}}
	if err := api.fillDeviceControlRequest("device", deviceControlActionTargetTrack, track, &deviceControlA23Request{}); err == nil {
		t.Fatal("2.0 must reject 3.0 target tracking")
	}
	for _, action := range []string{deviceQueryActionPTZPosition, deviceQueryActionSDCardStatus, deviceQueryActionCruiseTrackList, deviceQueryActionCruiseTrack} {
		if _, err := api.resolveDeviceQueryCmdType("device", action, ""); err == nil {
			t.Fatalf("2.0 must reject 3.0 query %s", action)
		}
	}
	if err := api.requireConfigTypeVersion("device", "SnapShotConfig"); err == nil {
		t.Fatal("2.0 must reject 3.0 snapshot configuration")
	}
	if _, err := api.Upgrade(context.Background(), &UpgradeInput{DeviceID: "device", ChannelID: "channel"}); err == nil || !strings.Contains(err.Error(), "2022") {
		t.Fatalf("2.0 upgrade gate error = %v", err)
	}

	memory.device.setGBVersion(GBVersion30)
	preciseRequest := &deviceControlA23Request{}
	if err := api.fillDeviceControlRequest("device", deviceControlActionPTZPrecise, precise, preciseRequest); err != nil || preciseRequest.PTZPreciseCtrl == nil {
		t.Fatalf("3.0 precise PTZ request = %+v, err = %v", preciseRequest, err)
	}
	sdRequest := &deviceControlA23Request{}
	if err := api.fillDeviceControlRequest("device", deviceControlActionFormatSDCard, &DeviceControlInput{SDCardID: 1}, sdRequest); err != nil || sdRequest.FormatSDCard == nil || *sdRequest.FormatSDCard != 1 {
		t.Fatalf("3.0 SD card request = %+v, err = %v", sdRequest, err)
	}
	trackRequest := &deviceControlA23Request{}
	if err := api.fillDeviceControlRequest("device", deviceControlActionTargetTrack, track, trackRequest); err != nil ||
		trackRequest.TargetTrack != "Manual" || trackRequest.TargetArea == nil {
		t.Fatalf("3.0 target track request = %+v, err = %v", trackRequest, err)
	}
	invalidTrack := &DeviceControlInput{TargetTrack: &TargetTrackParam{Mode: "Manual", TargetArea: &DragZoomParam{
		Length: 1920, Width: 1080, MidPointX: 1921, MidPointY: 540, LengthX: 300, LengthY: 200,
	}}}
	if err := api.fillDeviceControlRequest("device", deviceControlActionTargetTrack, invalidTrack, &deviceControlA23Request{}); err == nil {
		t.Fatal("3.0 target track accepted a midpoint outside the playback window")
	}
	for _, mode := range []string{"Auto", "Stop"} {
		invalidTrack = &DeviceControlInput{TargetTrack: &TargetTrackParam{Mode: mode, TargetArea: track.TargetTrack.TargetArea}}
		if err := api.fillDeviceControlRequest("device", deviceControlActionTargetTrack, invalidTrack, &deviceControlA23Request{}); err == nil {
			t.Fatalf("3.0 %s target track accepted manual-only target_area", mode)
		}
	}
	invalidZoom := 0.99
	if err := api.fillDeviceControlRequest("device", deviceControlActionPTZPrecise,
		&DeviceControlInput{PTZPrecise: &PTZPreciseParam{Zoom: &invalidZoom}}, &deviceControlA23Request{}); err == nil {
		t.Fatal("3.0 precise PTZ accepted an optical zoom below 1.0")
	}
	validZoom := 1.0
	if err := api.fillDeviceControlRequest("device", deviceControlActionPTZPrecise,
		&DeviceControlInput{PTZPrecise: &PTZPreciseParam{Zoom: &validZoom}}, &deviceControlA23Request{}); err != nil {
		t.Fatalf("3.0 precise PTZ rejected 1.0x optical zoom: %v", err)
	}
	for action, want := range map[string]string{
		deviceQueryActionPTZPosition:     "PTZPosition",
		deviceQueryActionSDCardStatus:    "SDCardStatus",
		deviceQueryActionCruiseTrackList: "CruiseTrackListQuery",
		deviceQueryActionCruiseTrack:     "CruiseTrackQuery",
	} {
		got, err := api.resolveDeviceQueryCmdType("device", action, "")
		if err != nil || got != want {
			t.Fatalf("3.0 query %s = %q, %v; want %q", action, got, err, want)
		}
	}
	if err := api.requireConfigTypeVersion("device", "SnapShotConfig"); err != nil {
		t.Fatalf("3.0 snapshot config rejected: %v", err)
	}
	if normalized, ok := normalizeConfigType("SnapShot"); !ok || normalized != "SnapShotConfig" {
		t.Fatalf("legacy snapshot config alias = %q, %v", normalized, ok)
	}
	if _, err := api.Upgrade(context.Background(), &UpgradeInput{DeviceID: "device", ChannelID: "channel"}); err == nil || !strings.Contains(err.Error(), "firmware/file_url/manufacturer") {
		t.Fatalf("3.0 upgrade did not pass version gate: %v", err)
	}

	body, err := buildGBSDP(gbSDPInput{
		Version: GBVersion30, SessionName: historyModePlayback,
		ChannelID: gb10ChannelID, URI: gb10ChannelID + ":0",
		IP: "192.0.2.20", Port: 30000, StreamMode: 1,
		StartAt: time.Unix(1711929600, 0), EndAt: time.Unix(1711933200, 0), SSRC: "1100000001",
	})
	if err != nil || !strings.Contains(string(body), "TCP/RTP/AVP") {
		t.Fatalf("3.0 RTP over TCP SDP = %s, err = %v", body, err)
	}
}

func TestGB30AppendixA4Regression(t *testing.T) {
	body := []byte(`<Notify><CmdType>Alarm</CmdType><Info><alarmType level="2"><Code>door_open</Code><VendorField>retained</VendorField></alarmType><behavioralEventType><Code>loitering</Code></behavioralEventType></Info></Notify>`)
	objects := (&GB28181API{}).decodeAppendixA4Objects("Alarm", body)
	if len(objects) != 2 {
		t.Fatalf("A.4 objects = %+v", objects)
	}
	object := objects[0]
	if object.Type != "alarmType" || object.CmdType != "Alarm" || object.Fields["Code"] != "door_open" || object.Fields["@level"] != "2" || !strings.Contains(object.RawXML, "VendorField") {
		t.Fatalf("A.4 object = %+v", object)
	}
	if objects[1].Type != "behavioralEventType" || objects[1].Fields["Code"] != "loitering" {
		t.Fatalf("standard behavioralEventType = %+v", objects[1])
	}
}

func TestGB30AppendixA4ExtraInfoJSONPreservesNumbersAndArrays(t *testing.T) {
	const extraInfo = `  [{"type":"doorType","DeviceID":"34020000001320000001","Sequence":100,"Zero":0,"Enabled":true}]  `
	body := []byte(`<Notify><CmdType>Alarm</CmdType><Info><ExtraInfo>` + extraInfo + `</ExtraInfo></Info></Notify>`)
	objects := (&GB28181API{}).decodeAppendixA4Objects("Alarm", body)
	if len(objects) != 1 {
		t.Fatalf("A.4 ExtraInfo objects = %+v", objects)
	}
	object := objects[0]
	if object.Type != "doorType" || object.Fields["[0].DeviceID"] != "34020000001320000001" ||
		object.Fields["[0].Sequence"] != "100" || object.Fields["[0].Zero"] != "0" || object.Fields["[0].Enabled"] != "true" {
		t.Fatalf("A.4 ExtraInfo fields = %+v", object.Fields)
	}
	if object.Fields["value"] != extraInfo {
		t.Fatalf("A.4 ExtraInfo value = %q, want %q", object.Fields["value"], extraInfo)
	}
	if values := appendixA4ExtraInfoValues(objects); len(values) != 1 || values[0] != extraInfo {
		t.Fatalf("A.4 ExtraInfo rebuilt values = %#v", values)
	}

	var root a4XMLNode
	if err := xml.Unmarshal([]byte(`<Item><ExtraInfo>`+extraInfo+`</ExtraInfo></Item>`), &root); err != nil {
		t.Fatal(err)
	}
	var cascadeValues []string
	collectCascadeExtraInfo(root, &cascadeValues)
	if len(cascadeValues) != 1 || cascadeValues[0] != extraInfo {
		t.Fatalf("cascade ExtraInfo values = %#v", cascadeValues)
	}
}

func TestGB30AppendixA4ExtraInfoPreservesEmptyAndWhitespaceValues(t *testing.T) {
	body := []byte(`<Response><CmdType>RecordInfo</CmdType>` +
		`<ExtraInfo>  keep  </ExtraInfo><ExtraInfo>   </ExtraInfo><ExtraInfo></ExtraInfo><ExtraInfo> x </ExtraInfo></Response>`)
	objects := (&GB28181API{}).decodeAppendixA4Objects("RecordInfo", body)
	want := []string{"  keep  ", "   ", "", " x "}
	if values := appendixA4ExtraInfoValues(objects); !reflect.DeepEqual(values, want) {
		t.Fatalf("A.4 ExtraInfo values = %#v, want %#v; objects = %+v", values, want, objects)
	}
}

func TestGB30AppendixA4StructuredStringsPreserveEmptyAndWhitespaceValues(t *testing.T) {
	body := []byte(`<Response><CmdType>Alarm</CmdType><Info>` +
		`<alarmType level="  high  "><Code>  door_open  </Code><Description></Description></alarmType>` +
		`<alarmType level="high"><Code>door_open</Code><Description>   </Description></alarmType>` +
		`</Info></Response>`)
	objects := (&GB28181API{}).decodeAppendixA4Objects("Alarm", body)
	if len(objects) != 2 {
		t.Fatalf("structured A.4 objects = %+v", objects)
	}
	if objects[0].Fields["@level"] != "  high  " || objects[0].Fields["Code"] != "  door_open  " || objects[0].Fields["Description"] != "" {
		t.Fatalf("first structured A.4 fields = %#v", objects[0].Fields)
	}
	if objects[1].Fields["@level"] != "high" || objects[1].Fields["Code"] != "door_open" || objects[1].Fields["Description"] != "   " {
		t.Fatalf("second structured A.4 fields = %#v", objects[1].Fields)
	}
	if !strings.Contains(objects[0].RawXML, `<Code>  door_open  </Code>`) || !strings.Contains(objects[1].RawXML, `<Description>   </Description>`) {
		t.Fatalf("structured A.4 RawXML lost whitespace: %#v, %#v", objects[0].RawXML, objects[1].RawXML)
	}
}

func TestMergeAppendixA4ObjectsPreservesEqualTimestampXMLOrder(t *testing.T) {
	values := []string{"first", "  second  ", "", "   ", "last"}
	objects := make([]AppendixA4Object, 0, len(values))
	for _, value := range values {
		objects = append(objects, AppendixA4Object{
			Type: "ExtraInfo", CmdType: "RecordInfo", Path: "/Response/ExtraInfo",
			Fields: map[string]string{"value": value}, UpdatedAt: 100,
		})
	}
	// map 枚举顺序不稳定，重复执行可覆盖旧实现偶然与 XML 顺序一致的情况。
	for i := 0; i < 100; i++ {
		merged := mergeAppendixA4Objects(nil, objects, 128)
		if got := appendixA4ExtraInfoValues(merged); !reflect.DeepEqual(got, values) {
			t.Fatalf("merged A.4 ExtraInfo values = %#v, want %#v", got, values)
		}
	}
}

func TestAppendixA4Requires2022(t *testing.T) {
	body := []byte(`<Notify><CmdType>Alarm</CmdType><Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID></doorType></Info></Notify>`)
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20} {
		api, _ := newVersionGateAPI(version)
		if objects, err := api.validateAndDecodeAppendixA4(gb10DeviceID, "Alarm", body); err == nil || len(objects) != 0 {
			t.Fatalf("%s Appendix A.4 result = %+v, %v", version, objects, err)
		}
		decoded := api.decodeAndStoreQueryResult(gb10DeviceID, "Alarm", body)
		if len(decoded.appendixA4) != 0 {
			t.Fatalf("%s defensively stored Appendix A.4: %+v", version, decoded.appendixA4)
		}
	}

	api, _ := newVersionGateAPI(GBVersion30)
	objects, err := api.validateAndDecodeAppendixA4(gb10DeviceID, "Alarm", body)
	if err != nil || len(objects) != 1 || objects[0].Type != "doorType" {
		t.Fatalf("2022 Appendix A.4 result = %+v, %v", objects, err)
	}

	empty := []byte(`<Response><CmdType>RecordInfo</CmdType><ExtraInfo></ExtraInfo></Response>`)
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20} {
		legacy, _ := newVersionGateAPI(version)
		if _, err := legacy.validateAndDecodeAppendixA4(gb10DeviceID, "RecordInfo", empty); err == nil {
			t.Fatalf("%s accepted empty 2022 ExtraInfo", version)
		}
	}
	objects, err = api.validateAndDecodeAppendixA4(gb10DeviceID, "RecordInfo", empty)
	if err != nil || len(objects) != 1 || objects[0].Type != "ExtraInfo" || objects[0].Fields["value"] != "" {
		t.Fatalf("2022 empty ExtraInfo result = %+v, %v", objects, err)
	}
}

func TestAppendixA4ExtraInfoSchemaLimits(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	for _, test := range []struct {
		name    string
		content string
		wantErr bool
	}{
		{name: "1024 characters", content: strings.Repeat("门", 1024)},
		{name: "1025 characters", content: strings.Repeat("门", 1025), wantErr: true},
		{name: "nested element", content: "<Value>door</Value>", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`<Response><CmdType>RecordInfo</CmdType><ExtraInfo>` + test.content + `</ExtraInfo></Response>`)
			_, err := api.validateAndDecodeAppendixA4(gb10DeviceID, "RecordInfo", body)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateAndDecodeAppendixA4() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestGB30PTZPositionNotifyStoresStructuredState(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	conn := newFlowConnection()
	body := []byte(`<Notify><CmdType>PTZPosition</CmdType><SN>72</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Pan>12.5</Pan><Tilt>-3.25</Tilt><Zoom>2</Zoom></Notify>`)
	response := runFlowHandler(t, conn, api, sip.MethodNotify, "ptz-position-notify", body, api.sipMessageQueryGeneric)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10ChannelID)
	if !ok || state.PTZPosition == nil || state.PTZPosition.Pan == nil || *state.PTZPosition.Pan != 12.5 ||
		state.PTZPosition.Tilt == nil || *state.PTZPosition.Tilt != -3.25 || state.PTZPosition.Zoom == nil || *state.PTZPosition.Zoom != 2 {
		t.Fatalf("PTZPosition state = %+v", state)
	}
	if _, ok := api.GetQueryState(gb10DeviceID); ok {
		t.Fatal("channel PTZPosition NOTIFY overwrote parent device query state")
	}
}

func TestGenericQueryResponseRejectsInvalidEnvelopeVersionAndTarget(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion20)
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "PTZPosition", 72), pending)
	tests := []struct {
		name string
		body string
	}{
		{name: "invalid root", body: `<Query><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`},
		{name: "notify root over message", body: `<Notify><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status></Notify>`},
		{name: "non-positive SN", body: `<Response><CmdType>DeviceStatus</CmdType><SN>0</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Response>`},
		{name: "missing device", body: `<Response><CmdType>DeviceStatus</CmdType><SN>1</SN></Response>`},
		{name: "unknown command", body: `<Response><CmdType>Unknown</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Response>`},
		{name: "newer-version command", body: `<Notify><CmdType>PTZPosition</CmdType><SN>72</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Pan>1</Pan></Notify>`},
		{name: "unknown target", body: `<Response><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>34020000001320000009</DeviceID></Response>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "generic-invalid-"+test.name, []byte(test.body), api.sipMessageQueryGeneric)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid generic query response = %s", response)
			}
		})
	}
	select {
	case out := <-pending.wait:
		t.Fatalf("invalid response resolved pending query: %+v", out)
	default:
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && (state.DeviceStatus != nil || state.PTZPosition != nil) {
		t.Fatalf("invalid generic query response changed state: %+v", state)
	}
}

func TestGenericQueryNotifyDoesNotResolveMessagePending(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10ChannelID}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "PTZPosition", 72), pending)
	body := []byte(`<Notify><CmdType>PTZPosition</CmdType><SN>72</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Pan>1</Pan></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "ptz-notify-not-query-response", body, api.sipMessageQueryGeneric)
	assertFlowOK(t, response)
	select {
	case output := <-pending.wait:
		t.Fatalf("PTZPosition NOTIFY resolved MESSAGE query: %+v", output)
	default:
	}
	state, ok := api.GetQueryState(gb10ChannelID)
	if !ok || state.PTZPosition == nil || state.PTZPosition.Pan == nil || *state.PTZPosition.Pan != 1 {
		t.Fatalf("valid PTZPosition NOTIFY state = %+v", state)
	}
	if _, ok := api.GetQueryState(gb10DeviceID); ok {
		t.Fatal("channel PTZPosition NOTIFY overwrote parent device query state")
	}
}

func TestGenericQueryNotifyRejectsResponseRoot(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	body := []byte(`<Response><CmdType>PTZPosition</CmdType><SN>72</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Pan>1</Pan></Response>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "ptz-notify-response-root", body, api.sipMessageQueryGeneric)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("PTZPosition NOTIFY Response root = %s", response)
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.PTZPosition != nil {
		t.Fatalf("invalid PTZPosition NOTIFY changed state: %+v", state.PTZPosition)
	}
}

func TestGenericQueryNotifyRejectsNonEventCommands(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	for _, cmdType := range []string{
		"DeviceStatus", "PresetQuery", "HomePositionQuery", "CruiseTrackListQuery",
		"CruiseTrackQuery", "SDCardStatus", "ConfigDownload",
	} {
		t.Run(cmdType, func(t *testing.T) {
			body := []byte(`<Notify><CmdType>` + cmdType + `</CmdType><SN>73</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Notify>`)
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "non-event-notify-"+cmdType, body, api.sipMessageQueryGeneric)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("non-event NOTIFY response = %s", response)
			}
		})
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state != nil {
		t.Fatalf("non-event NOTIFY changed query state: %+v", state)
	}
}

func TestGenericQueryResponseRejectsSiblingPendingTargetBeforeState(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	firstChannelID := gb10ChannelID
	secondChannelID := "34020000001320000003"
	memory.runtime.Channels.Store(firstChannelID, &Channel{ChannelID: firstChannelID, device: memory.runtime})
	memory.runtime.Channels.Store(secondChannelID, &Channel{ChannelID: secondChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: firstChannelID}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "PTZPosition", 76), pending)
	body := []byte(`<Response><CmdType>PTZPosition</CmdType><SN>76</SN><DeviceID>` + secondChannelID + `</DeviceID><Pan>1</Pan></Response>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "query-sibling-target", body, api.sipMessageQueryGeneric)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("sibling query response = %s", response)
	}
	select {
	case out := <-pending.wait:
		t.Fatalf("sibling response resolved pending query: %+v", out)
	default:
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.PTZPosition != nil {
		t.Fatalf("sibling response changed query state: %+v", state.PTZPosition)
	}
}

func TestDeviceStatusResponseValidatesRequiredFieldsBeforeStateAndRuntime(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion10)
	initialOnline := memory.device.runtimeSnapshot().IsOnline
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "DeviceStatus", 81), pending)
	tests := []struct {
		name string
		body string
	}{
		{name: "missing result", body: `<Response><CmdType>DeviceStatus</CmdType><SN>81</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Online>ONLINE</Online><Status>OK</Status></Response>`},
		{name: "invalid result", body: `<Response><CmdType>DeviceStatus</CmdType><SN>81</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>SUCCESS</Result><Online>ONLINE</Online><Status>OK</Status></Response>`},
		{name: "invalid online", body: `<Response><CmdType>DeviceStatus</CmdType><SN>81</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Online>ON</Online><Status>OK</Status></Response>`},
		{name: "invalid status", body: `<Response><CmdType>DeviceStatus</CmdType><SN>81</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>ON</Status></Response>`},
		{name: "2011 Appendix A.4 extension", body: `<Response><CmdType>DeviceStatus</CmdType><SN>81</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID></doorType></Info></Response>`},
		{name: "duplicate online", body: `<Response><CmdType>DeviceStatus</CmdType><SN>81</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Online>OFFLINE</Online><Online>ONLINE</Online><Status>OK</Status></Response>`},
		{name: "unknown field", body: `<Response><CmdType>DeviceStatus</CmdType><SN>81</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><VendorField>value</VendorField></Response>`},
		{name: "out of order", body: `<Response><CmdType>DeviceStatus</CmdType><SN>81</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><DeviceTime>2026-08-29T12:00:00</DeviceTime><Encode>ON</Encode></Response>`},
		{name: "nested status", body: `<Response><CmdType>DeviceStatus</CmdType><SN>81</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status><Value>OK</Value></Status></Response>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "device-status-invalid-"+test.name, []byte(test.body), api.sipMessageQueryGeneric)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid DeviceStatus response = %s", response)
			}
		})
	}
	select {
	case output := <-pending.wait:
		t.Fatalf("invalid DeviceStatus resolved pending query: %+v", output)
	default:
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.DeviceStatus != nil {
		t.Fatalf("invalid DeviceStatus changed state: %+v", state.DeviceStatus)
	}
	if memory.device.runtimeSnapshot().IsOnline != initialOnline {
		t.Fatal("invalid DeviceStatus changed device runtime")
	}
}

func TestDeviceStatusFailureAndChildResponseDoNotChangeParentRuntime(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion10)
	memory.runtime.UpdateRuntime(func(device *Device) { device.IsOnline = false })
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	for _, body := range []string{
		`<Response><CmdType>DeviceStatus</CmdType><SN>82</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>ERROR</Result><Online>ONLINE</Online><Status>OK</Status></Response>`,
		`<Response><CmdType>DeviceStatus</CmdType><SN>83</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status></Response>`,
	} {
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "device-status-no-parent-update", []byte(body), api.sipMessageQueryGeneric)
		assertFlowOK(t, response)
	}
	if memory.runtime.runtimeSnapshot().IsOnline {
		t.Fatal("failed/child DeviceStatus changed parent runtime")
	}
}

func TestGB30VideoUploadNotifyStoresStructuredState(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion30)
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "VideoUploadNotify", 108), pending)
	defer api.pendingDeviceQuery.Delete(buildPendingQueryKey(gb10DeviceID, "VideoUploadNotify", 108))
	body := []byte(`<?xml version="1.0"?><Notify><CmdType>VideoUploadNotify</CmdType><SN>108</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Time>2026-08-25T08:48:00</Time><Longitude>120.12</Longitude><Latitude>30.28</Latitude></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "video-upload-notify", body, api.sipMessageVideoUploadNotify)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.VideoUpload == nil || state.VideoUpload.Time != "2026-08-25T08:48:00" ||
		state.VideoUpload.Longitude == nil || *state.VideoUpload.Longitude != 120.12 {
		t.Fatalf("VideoUploadNotify state = %+v, %v", state, ok)
	}
	select {
	case output := <-pending.wait:
		t.Fatalf("VideoUploadNotify resolved query pending: %+v", output)
	default:
	}
	memory.device.setGBVersion(GBVersion20)
	response = runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "video-upload-old", body, api.sipMessageVideoUploadNotify)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("2.0 VideoUploadNotify response = %s", response)
	}
}

func TestGB30VideoUploadNotifyStoresOnlyAfterSuccessfulSIPOK(t *testing.T) {
	for _, test := range []struct {
		name     string
		writeErr error
		stored   bool
	}{
		{name: "success", stored: true},
		{name: "write failure", writeErr: errors.New("write failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(GBVersion30)
			body := []byte(`<Notify><CmdType>VideoUploadNotify</CmdType><SN>112</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Time>2026-08-25T08:52:00</Time><Longitude>120.12</Longitude><Latitude>30.28</Latitude></Notify>`)
			conn, done := startBlockingFlowHandler(t, api, sip.MethodMessage, "video-upload-commit-"+test.name, body, api.sipMessageVideoUploadNotify, test.writeErr)
			_, storedBeforeSIP := api.GetQueryState(gb10DeviceID)
			finishBlockingFlowHandler(t, conn, done)
			if storedBeforeSIP {
				t.Fatal("VideoUploadNotify state stored before SIP 200 was written")
			}
			state, stored := api.GetQueryState(gb10DeviceID)
			stored = stored && state.VideoUpload != nil
			if stored != test.stored {
				t.Fatalf("VideoUploadNotify state stored = %v, want %v", stored, test.stored)
			}
		})
	}
}

func TestGB30VideoUploadNotifyAcceptsProjectGB2312XML(t *testing.T) {
	longitude := 120.12
	latitude := 30.28
	body, err := sip.XMLEncode(videoUploadNotifyXML{
		XMLName: xml.Name{Local: "Notify"}, CmdType: "VideoUploadNotify", SN: 111, DeviceID: gb10DeviceID,
		Time: "2026-08-25T08:51:00", Longitude: &longitude, Latitude: &latitude,
	})
	if err != nil {
		t.Fatal(err)
	}
	api, _ := newVersionGateAPI(GBVersion30)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "video-upload-gb2312", body, api.sipMessageVideoUploadNotify)
	assertFlowOK(t, response)
}

func TestGB30VideoUploadNotifyAcceptsOptionalCoordinates(t *testing.T) {
	tests := []struct {
		name      string
		location  string
		longitude *float64
		latitude  *float64
	}{
		{name: "time only"},
		{name: "longitude only", location: `<Longitude>120.12</Longitude>`, longitude: float64Pointer(120.12)},
		{name: "latitude only", location: `<Latitude>30.28</Latitude>`, latitude: float64Pointer(30.28)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(GBVersion30)
			body := []byte(`<Notify><CmdType>VideoUploadNotify</CmdType><SN>120</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Time>2026-08-25T08:53:00</Time>` + test.location + `</Notify>`)
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "video-upload-optional-location-"+test.name, body, api.sipMessageVideoUploadNotify)
			assertFlowOK(t, response)
			state, ok := api.GetQueryState(gb10DeviceID)
			if !ok || state.VideoUpload == nil || !sameOptionalFloat(state.VideoUpload.Longitude, test.longitude) || !sameOptionalFloat(state.VideoUpload.Latitude, test.latitude) {
				t.Fatalf("VideoUploadNotify optional location state = %+v, %v", state.VideoUpload, ok)
			}
		})
	}
}

func sameOptionalFloat(got, want *float64) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

func TestGB30VideoUploadNotifyRejectsSchemaAndTargetViolations(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong command", body: `<Notify><CmdType>Catalog</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-25T08:48:00</Time><Longitude>120</Longitude><Latitude>30</Latitude></Notify>`},
		{name: "non-positive SN", body: `<Notify><CmdType>VideoUploadNotify</CmdType><SN>0</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-25T08:48:00</Time><Longitude>120</Longitude><Latitude>30</Latitude></Notify>`},
		{name: "missing device", body: `<Notify><CmdType>VideoUploadNotify</CmdType><SN>1</SN><Time>2026-08-25T08:48:00</Time><Longitude>120</Longitude><Latitude>30</Latitude></Notify>`},
		{name: "invalid time", body: `<Notify><CmdType>VideoUploadNotify</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>not-a-time</Time><Longitude>120</Longitude><Latitude>30</Latitude></Notify>`},
		{name: "non-finite longitude", body: `<Notify><CmdType>VideoUploadNotify</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-25T08:48:00</Time><Longitude>NaN</Longitude><Latitude>30</Latitude></Notify>`},
		{name: "longitude out of range", body: `<Notify><CmdType>VideoUploadNotify</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-25T08:48:00</Time><Longitude>181</Longitude><Latitude>30</Latitude></Notify>`},
		{name: "latitude out of range", body: `<Notify><CmdType>VideoUploadNotify</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-25T08:48:00</Time><Longitude>120</Longitude><Latitude>-91</Latitude></Notify>`},
		{name: "unknown target", body: `<Notify><CmdType>VideoUploadNotify</CmdType><SN>1</SN><DeviceID>34020000001320000009</DeviceID><Time>2026-08-25T08:48:00</Time><Longitude>120</Longitude><Latitude>30</Latitude></Notify>`},
		{name: "duplicate time", body: `<Notify><CmdType>VideoUploadNotify</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-25T08:48:00</Time><Time>2026-08-25T08:49:00</Time><Longitude>120</Longitude><Latitude>30</Latitude></Notify>`},
		{name: "unknown element", body: `<Notify><CmdType>VideoUploadNotify</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-25T08:48:00</Time><Longitude>120</Longitude><Latitude>30</Latitude><Info><doorType/></Info></Notify>`},
		{name: "root attribute", body: `<Notify vendor="1"><CmdType>VideoUploadNotify</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-25T08:48:00</Time><Longitude>120</Longitude><Latitude>30</Latitude></Notify>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "video-upload-invalid-"+test.name, []byte(test.body), api.sipMessageVideoUploadNotify)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid VideoUploadNotify response = %s", response)
			}
		})
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && (state.VideoUpload != nil || len(state.AppendixA4) != 0) {
		t.Fatalf("invalid VideoUploadNotify changed state: VideoUpload=%+v AppendixA4=%+v", state.VideoUpload, state.AppendixA4)
	}
}

func TestGB30VideoUploadNotifyAcceptsOwnedChannel(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	body := []byte(`<Notify><CmdType>VideoUploadNotify</CmdType><SN>109</SN><DeviceID>` + gb10ChannelID +
		`</DeviceID><Time>2026-08-25T08:49:00.123</Time><Longitude>120.12</Longitude><Latitude>30.28</Latitude></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "video-upload-channel", body, api.sipMessageVideoUploadNotify)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10ChannelID)
	if !ok || state.VideoUpload == nil || state.VideoUpload.Time != "2026-08-25T08:49:00.123" {
		t.Fatalf("owned-channel VideoUploadNotify state = %+v, %v", state, ok)
	}
	if parentState, ok := api.GetQueryState(gb10DeviceID); ok && parentState.VideoUpload != nil {
		t.Fatalf("owned-channel VideoUploadNotify overwrote parent state: %+v", parentState.VideoUpload)
	}
}

func TestGB30VideoUploadNotifyDoesNotPublishSubscriptionNotify(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	connection := newFlowConnection()
	subscription := &eventSubscription{
		Key: "video-upload-message", CmdType: "VideoUploadNotify", DeviceID: gb10DeviceID,
		ExpiresAt: time.Now().Add(time.Minute),
		To:        mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
		Source:    connection.remote, Conn: connection, GBVersion: string(GBVersion30), Event: "presence",
	}
	attachFlowEventSubscriptionDialog(t, subscription, connection, "video-upload-message-dialog")
	api.eventSubscribers.Store(subscription.Key, subscription)
	defer api.eventSubscribers.Delete(subscription.Key)

	body := []byte(`<Notify><CmdType>VideoUploadNotify</CmdType><SN>110</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-25T08:50:00</Time><Longitude>120.12</Longitude><Latitude>30.28</Latitude></Notify>`)
	response := runFlowHandler(t, connection, api, sip.MethodMessage, "video-upload-message-no-subscription-notify", body, api.sipMessageVideoUploadNotify)
	assertFlowOK(t, response)

	subscription.mu.Lock()
	cseq := subscription.CSeq
	subscription.mu.Unlock()
	if cseq != 0 {
		t.Fatalf("VideoUploadNotify MESSAGE advanced subscription NOTIFY CSeq to %d", cseq)
	}
}

func TestGB30CruiseTrackQueriesDecodeStructuredState(t *testing.T) {
	api := &GB28181API{}
	listBody := []byte(`<Response><CmdType>CruiseTrackListQuery</CmdType><SN>74</SN><DeviceID>` + gb10ChannelID + `</DeviceID><SumNum>2</SumNum><CruiseTrackList Num="2"><CruiseTrack><Number>0</Number><Name>白天</Name></CruiseTrack><CruiseTrack><Number>1</Number><Name>夜间</Name></CruiseTrack></CruiseTrackList></Response>`)
	list := api.decodeAndStoreQueryResult(gb10DeviceID, "CruiseTrackListQuery", listBody).data
	tracks, ok := list.([]CruiseTrackData)
	if !ok || len(tracks) != 2 || tracks[0].Number != 0 || tracks[0].Name != "白天" || tracks[1].Number != 1 {
		t.Fatalf("CruiseTrackListQuery data = %+v", list)
	}

	detailBody := []byte(`<Response><CmdType>CruiseTrackQuery</CmdType><SN>75</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Number>1</Number><Name>夜间</Name><SumNum>2</SumNum><CruisePointList Num="2"><CruisePoint><PresetIndex>3</PresetIndex><StayTime>10</StayTime><Speed>5</Speed></CruisePoint><CruisePoint><PresetIndex>7</PresetIndex><StayTime>20</StayTime><Speed>8</Speed></CruisePoint></CruisePointList></Response>`)
	detail := api.decodeAndStoreQueryResult(gb10DeviceID, "CruiseTrackQuery", detailBody).data
	track, ok := detail.(*CruiseTrackData)
	if !ok || track.Number != 1 || track.Name != "夜间" || len(track.Points) != 2 || track.Points[0].PresetIndex != 3 || track.Points[1].Speed != 8 {
		t.Fatalf("CruiseTrackQuery data = %+v", detail)
	}
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || len(state.CruiseTracks) != 2 || state.CruiseTrack == nil || state.CruiseTrack.Number != 1 {
		t.Fatalf("cruise query state = %+v", state)
	}
}

func TestGB20MobilePositionNotifyStoresStructuredState(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion20)
	conn := newFlowConnection()
	body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>73</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-25T12:00:00</Time><Longitude>120.5</Longitude><Latitude>30.25</Latitude><Speed>18.5</Speed><Direction>90</Direction></Notify>`)
	response := runFlowHandler(t, conn, api, sip.MethodNotify, "mobile-position-notify", body, api.sipNotifyMobilePosition)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.MobilePosition == nil || state.MobilePosition.Longitude == nil || *state.MobilePosition.Longitude != 120.5 ||
		state.MobilePosition.Latitude == nil || *state.MobilePosition.Latitude != 30.25 || state.MobilePosition.Speed == nil || *state.MobilePosition.Speed != 18.5 {
		t.Fatalf("MobilePosition state = %+v", state)
	}
}

func TestGB30SIPTLSListenPlanDoesNotReuseTCPPort(t *testing.T) {
	plain := buildSIPListenPlan(conf.SIP{Port: 5060})
	if !plain.PlainTCP || plain.TLS {
		t.Fatalf("plain listen plan = %+v", plain)
	}
	shared := buildSIPListenPlan(conf.SIP{Port: 5060, EnableTLS: true})
	if shared.PlainTCP || !shared.TLS || shared.TLSPort != 5060 {
		t.Fatalf("shared TLS listen plan = %+v", shared)
	}
	separate := buildSIPListenPlan(conf.SIP{Port: 5060, EnableTLS: true, TLSPort: 5061})
	if !separate.PlainTCP || !separate.TLS || separate.TLSPort != 5061 {
		t.Fatalf("separate TLS listen plan = %+v", separate)
	}
}
