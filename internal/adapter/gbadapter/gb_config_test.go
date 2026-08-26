package gbadapter

import (
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
)

func TestToGBDeviceConfigInputMapsAllVersionedSections(t *testing.T) {
	input := toGBDeviceConfigInput("34020000001320000001", &ipc.GBDeviceConfigInput{
		TargetID: "34020000001320000002",
		Timeout:  9,
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
	if input.BasicParam == nil || input.BasicParam.HeartBeatCount != 3 ||
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
	in := toGBDeviceQueryInput("34020000001320000001", &ipc.GBDeviceQueryInput{
		TargetID: "34020000001320000002", Action: "record_info", Timeout: 9,
		Start: 1, End: 2, Type: "all", StreamNumber: &streamNumber, AlarmMethod: "2/5", AlarmType: "2",
	})
	if in.DeviceID != "34020000001320000001" || in.TargetID != "34020000001320000002" || in.Action != "record_info" ||
		in.Timeout.Seconds() != 9 || in.Start != 1 || in.End != 2 || in.Type != "all" || in.StreamNumber == nil || *in.StreamNumber != 2 ||
		in.AlarmMethod != "2/5" || in.AlarmType != "2" {
		t.Fatalf("mapped DeviceQuery = %+v", in)
	}
}
