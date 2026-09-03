package gbadapter

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs"
)

func TestValidateDevicePreparesProfileWithoutPublishingThroughServer(t *testing.T) {
	adapter := &Adapter{gbs: &gbs.Server{}}
	device := &ipc.Device{Ext: ipc.DeviceExt{
		GBManualVersion:        "3.0",
		GBDisabledCapabilities: []string{" Snapshot ", "snapshot"},
	}}

	if err := adapter.ValidateDevice(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	if len(device.Ext.GBDisabledCapabilities) != 1 || device.Ext.GBDisabledCapabilities[0] != "snapshot" {
		t.Fatalf("normalized disabled capabilities = %v", device.Ext.GBDisabledCapabilities)
	}
	if slices.Contains(device.Ext.GBVersionCapabilities, "snapshot") ||
		!slices.Contains(device.Ext.GBVersionCapabilities, "upgrade") {
		t.Fatalf("prepared version capabilities = %v", device.Ext.GBVersionCapabilities)
	}
}

func TestValidateDeviceRejectsInvalidStreamMode(t *testing.T) {
	adapter := &Adapter{gbs: &gbs.Server{}}
	for _, streamMode := range []int8{-1, 3} {
		device := &ipc.Device{StreamMode: streamMode}
		if err := adapter.ValidateDevice(t.Context(), device); err == nil ||
			!strings.Contains(err.Error(), "invalid RTP stream mode") {
			t.Fatalf("stream mode %d error = %v", streamMode, err)
		}
	}
}

func TestDeviceRegistrationCredentialChangedIncludesPasswordClear(t *testing.T) {
	tests := []struct {
		name   string
		before *ipc.Device
		after  *ipc.Device
		want   bool
	}{
		{name: "unchanged", before: &ipc.Device{Password: "secret"}, after: &ipc.Device{Password: "secret"}},
		{name: "replace", before: &ipc.Device{Password: "secret"}, after: &ipc.Device{Password: "new-secret"}, want: true},
		{name: "clear to global password", before: &ipc.Device{Password: "secret"}, after: &ipc.Device{}, want: true},
		{name: "set device password", before: &ipc.Device{}, after: &ipc.Device{Password: "secret"}, want: true},
		{name: "missing before", after: &ipc.Device{Password: "secret"}},
		{name: "missing after", before: &ipc.Device{Password: "secret"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := deviceRegistrationCredentialChanged(test.before, test.after); got != test.want {
				t.Fatalf("deviceRegistrationCredentialChanged() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestFinishRecordQueryPreservesPartialResultAndError(t *testing.T) {
	incomplete := &gbs.RecordInfoIncompleteError{Received: 1, Expected: 2}
	out, err := finishRecordQuery(&gbs.Records{
		DayTotal: 1,
		TimeNum:  1,
		Data: []gbs.RecordDate{{
			Date:  "2026-08-29",
			Items: []gbs.RecordInfo{{Start: 10, End: 20}},
		}},
	}, incomplete)
	if !errors.Is(err, incomplete) {
		t.Fatalf("finishRecordQuery error = %v, want %v", err, incomplete)
	}
	if out == nil || out.DayTotal != 1 || out.TimeNum != 1 || len(out.Data) != 1 || len(out.Data[0].Items) != 1 {
		t.Fatalf("finishRecordQuery output = %+v", out)
	}
	if item := out.Data[0].Items[0]; item.Start != 10 || item.End != 20 {
		t.Fatalf("finishRecordQuery item = %+v", item)
	}
}

func TestToGBDeviceConfigInputMapsAllVersionedSections(t *testing.T) {
	input := toGBDeviceConfigInput("34020000001320000001", &ipc.GBDeviceConfigInput{
		TargetID:  "34020000001320000002",
		Timeout:   9,
		ExtraInfo: []string{" first ", "second"},
		BasicParam: &ipc.GBBasicParamInput{
			Name: "IPC", Expiration: 3600, HeartBeatInterval: 60, HeartBeatCount: 3,
		},
		VideoParamConfig: &ipc.GBVideoParamConfigInput{Items: []ipc.GBVideoParamItemInput{{
			StreamName: "Stream1", VideoFormat: "H.264", Resolution: "1920x1080",
			FrameRate: "25", BitRateType: "1", VideoBitRate: "4096",
		}}},
		AudioParamConfig: &ipc.GBAudioParamConfigInput{Items: []ipc.GBAudioParamItemInput{{
			StreamName: "Stream1", AudioFormat: "G.711", AudioBitRate: "64", SamplingRate: "8",
		}}},
		SVACEncodeConfig:    &ipc.GBXMLConfigInput{InnerXML: "<SVCParam/>"},
		SVACDecodeConfig:    &ipc.GBXMLConfigInput{InnerXML: "<SVCParam/>"},
		VideoParamAttribute: &ipc.GBXMLConfigInput{InnerXML: "<Item/>"},
		VideoRecordPlan:     &ipc.GBXMLConfigInput{InnerXML: "<RecordPlan/>"},
		VideoAlarmRecord:    &ipc.GBXMLConfigInput{InnerXML: "<RecordEnable>1</RecordEnable>"},
		PictureMask:         &ipc.GBXMLConfigInput{InnerXML: "<Enable>1</Enable>"},
		FrameMirror:         &ipc.GBXMLConfigInput{InnerXML: "1"},
		AlarmReport:         &ipc.GBXMLConfigInput{InnerXML: "<MotionDetection>1</MotionDetection>"},
		OSDConfig:           &ipc.GBXMLConfigInput{InnerXML: "<Length>1920</Length>"},
		SnapShotConfig: &ipc.GBSnapshotConfigInput{
			SnapNum: 1, Interval: 1, UploadURL: "https://example.invalid/upload",
			SessionID: "snapshot-session-0000000000000001",
		},
	})
	if input == nil || input.DeviceID != "34020000001320000001" || input.TargetID != "34020000001320000002" || input.Timeout.Seconds() != 9 {
		t.Fatalf("mapped DeviceConfig identity = %+v", input)
	}
	if len(input.ExtraInfo) != 2 || input.ExtraInfo[0] != " first " ||
		input.BasicParam == nil || input.BasicParam.HeartBeatCount != 3 ||
		input.VideoParamConfig == nil || len(input.VideoParamConfig.Items) != 1 || input.VideoParamConfig.Items[0].VideoBitRate != "4096" ||
		input.AudioParamConfig == nil || len(input.AudioParamConfig.Items) != 1 || input.AudioParamConfig.Items[0].SamplingRate != "8" ||
		input.SVACEncodeConfig == nil || input.SVACDecodeConfig == nil || input.VideoParamAttribute == nil ||
		input.VideoRecordPlan == nil || input.VideoAlarmRecord == nil || input.PictureMask == nil ||
		input.FrameMirror == nil || input.AlarmReport == nil || input.OSDConfig == nil || input.SnapShotConfig == nil ||
		input.SnapShotConfig.SessionID != "snapshot-session-0000000000000001" {
		t.Fatalf("mapped DeviceConfig sections = %+v", input)
	}
}

func TestToGBTargetTrackMapsManualArea(t *testing.T) {
	input := toGBTargetTrack(&ipc.GBTargetTrackInput{
		Mode:      "Manual",
		DeviceID2: "34020000001320000002",
		TargetArea: &ipc.GBDragZoomInput{
			Length: 1920, Width: 1080, MidPointX: 960, MidPointY: 540, LengthX: 300, LengthY: 200,
		},
	})
	if input == nil || input.Mode != "Manual" || input.DeviceID2 != "34020000001320000002" || input.TargetArea == nil {
		t.Fatalf("mapped TargetTrack = %+v", input)
	}
	if input.TargetArea.Length != 1920 || input.TargetArea.Width != 1080 || input.TargetArea.LengthX != 300 || input.TargetArea.LengthY != 200 {
		t.Fatalf("mapped TargetTrack area = %+v", input.TargetArea)
	}
}

func TestToGBDeviceQueryInputMapsRecordFilters(t *testing.T) {
	streamNumber := 2
	indistinctQuery := 1
	secrecy := 1
	in := toGBDeviceQueryInput("34020000001320000001", &ipc.GBDeviceQueryInput{
		TargetID: "34020000001320000002", Action: "record_info", Timeout: 9,
		Start: 1, End: 2, Type: "all", IndistinctQuery: &indistinctQuery,
		FilePath: "/record/front-gate.ps", Address: "front-gate", Secrecy: &secrecy, RecorderID: "34020000002000000001",
		StreamNumber: &streamNumber, AlarmMethod: "2/5", AlarmType: "2",
	})
	if in.DeviceID != "34020000001320000001" || in.TargetID != "34020000001320000002" || in.Action != "record_info" ||
		in.Timeout.Seconds() != 9 || in.Start != 1 || in.End != 2 || in.FilePath != "/record/front-gate.ps" || in.Address != "front-gate" ||
		in.Secrecy == nil || *in.Secrecy != 1 || in.Type != "all" || in.RecorderID != "34020000002000000001" ||
		in.IndistinctQuery == nil || *in.IndistinctQuery != 1 ||
		in.StreamNumber == nil || *in.StreamNumber != 2 ||
		in.AlarmMethod != "2/5" || in.AlarmType != "2" {
		t.Fatalf("mapped DeviceQuery = %+v", in)
	}
}
