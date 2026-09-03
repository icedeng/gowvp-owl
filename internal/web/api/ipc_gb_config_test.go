package api

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs"
)

func TestGBDeviceConfigInputDecodesAllVersionedSections(t *testing.T) {
	var input gbDeviceConfigInput
	body := []byte(`{
		"target_id":"34020000001320000002",
		"extra_info":[" first ","second"],
		"video_param_config":{"items":[{"stream_name":"Stream1","video_format":"H.264","resolution":"1920x1080","frame_rate":"25","bit_rate_type":"1","video_bit_rate":"4096"}]},
		"audio_param_config":{"items":[{"stream_name":"Stream1","audio_format":"G.711","audio_bit_rate":"64","sampling_rate":"8"}]},
		"svac_encode_config":{"inner_xml":"<SVCParam><SVCFlag>1</SVCFlag></SVCParam>"},
		"svac_decode_config":{"inner_xml":"<SVCParam><SVCSTMMode>1</SVCSTMMode></SVCParam>"},
		"video_param_attribute":{"inner_xml":"<Item/>"},
		"video_record_plan":{"inner_xml":"<RecordPlan/>"},
		"video_alarm_record":{"inner_xml":"<RecordEnable>1</RecordEnable>"},
		"picture_mask":{"inner_xml":"<Enable>1</Enable>"},
		"frame_mirror":{"inner_xml":"1"},
		"alarm_report":{"inner_xml":"<MotionDetection>1</MotionDetection>"},
		"osd_config":{"inner_xml":"<Length>1920</Length>"},
		"snapshot_config":{"snap_num":1,"interval":1,"upload_url":"https://example.invalid/upload","session_id":"snapshot-session-0000000000000001"}
	}`)
	if err := json.Unmarshal(body, &input); err != nil {
		t.Fatal(err)
	}
	if len(input.ExtraInfo) != 2 || input.ExtraInfo[0] != " first " ||
		input.VideoParamConfig == nil || len(input.VideoParamConfig.Items) != 1 || input.VideoParamConfig.Items[0].VideoBitRate != "4096" ||
		input.AudioParamConfig == nil || len(input.AudioParamConfig.Items) != 1 || input.AudioParamConfig.Items[0].SamplingRate != "8" ||
		input.SVACEncodeConfig == nil || input.SVACDecodeConfig == nil || input.VideoParamAttribute == nil ||
		input.VideoRecordPlan == nil || input.VideoAlarmRecord == nil || input.PictureMask == nil ||
		input.FrameMirror == nil || input.AlarmReport == nil || input.OSDConfig == nil || input.SnapShotConfig == nil ||
		input.SnapShotConfig.SessionID != "snapshot-session-0000000000000001" {
		t.Fatalf("decoded DeviceConfig = %+v", input)
	}
}

func TestRecordQueryAPIResultPreservesIncompleteData(t *testing.T) {
	incomplete := &gbs.RecordInfoIncompleteError{Received: 1, Expected: 2}
	value, err := recordQueryAPIResult(&ipc.RecordQueryOutput{DayTotal: 1, TimeNum: 1}, incomplete)
	if err != nil {
		t.Fatalf("recordQueryAPIResult error = %v", err)
	}
	out, ok := value.(recordQueryOutput)
	if !ok || out.RecordQueryOutput == nil || out.TimeNum != 1 || out.Incomplete == nil {
		t.Fatalf("recordQueryAPIResult output = %#v", value)
	}
	if out.Incomplete.Kind != "record_info" || out.Incomplete.ReceivedCount != 1 || out.Incomplete.ExpectedCount != 2 {
		t.Fatalf("recordQueryAPIResult incomplete = %+v", out.Incomplete)
	}
}

func TestGBDeviceQueryAPIResultPreservesIncompleteData(t *testing.T) {
	incomplete := &gbs.ConfigDownloadIncompleteError{
		Received: []string{"BasicParam"},
		Missing:  []string{"VideoParamOpt"},
	}
	value, err := gbDeviceQueryAPIResult(&ipc.GBDeviceQueryOutput{
		SN: 8, CmdType: "ConfigDownload", DeviceID: "34020000001320000001",
	}, incomplete)
	if err != nil {
		t.Fatalf("gbDeviceQueryAPIResult error = %v", err)
	}
	out, ok := value.(gbDeviceQueryOutput)
	if !ok || out.SN != 8 || out.Incomplete == nil {
		t.Fatalf("gbDeviceQueryAPIResult output = %#v", value)
	}
	if out.Incomplete.Kind != "config_download" ||
		len(out.Incomplete.ReceivedConfig) != 1 || out.Incomplete.ReceivedConfig[0] != "BasicParam" ||
		len(out.Incomplete.MissingConfig) != 1 || out.Incomplete.MissingConfig[0] != "VideoParamOpt" {
		t.Fatalf("gbDeviceQueryAPIResult incomplete = %+v", out.Incomplete)
	}

	incomplete.Received[0] = "mutated"
	if out.Incomplete.ReceivedConfig[0] != "BasicParam" {
		t.Fatalf("gbDeviceQueryAPIResult shares error slices = %+v", out.Incomplete)
	}

	ordinary := errors.New("query failed")
	value, err = gbDeviceQueryAPIResult(&ipc.GBDeviceQueryOutput{SN: 9}, ordinary)
	if value != nil || err == nil {
		t.Fatalf("ordinary query failure = %#v, %v", value, err)
	}
}

func TestGBDeviceControlInputDecodesTargetTrack(t *testing.T) {
	var input gbDeviceControlInput
	body := []byte(`{
		"action":"target_track",
		"target_track":{
			"mode":"Manual",
			"device_id2":"34020000001320000002",
			"target_area":{"length":1920,"width":1080,"mid_point_x":960,"mid_point_y":540,"length_x":300,"length_y":200}
		}
	}`)
	if err := json.Unmarshal(body, &input); err != nil {
		t.Fatal(err)
	}
	mapped := toIPCTargetTrack(input.TargetTrack)
	if input.Action != "target_track" || mapped == nil || mapped.Mode != "Manual" || mapped.DeviceID2 != "34020000001320000002" || mapped.TargetArea == nil {
		t.Fatalf("decoded TargetTrack = input:%+v mapped:%+v", input, mapped)
	}
	if mapped.TargetArea.MidPointX != 960 || mapped.TargetArea.MidPointY != 540 || mapped.TargetArea.LengthX != 300 || mapped.TargetArea.LengthY != 200 {
		t.Fatalf("decoded TargetTrack area = %+v", mapped.TargetArea)
	}
}

func TestGBDeviceControlInputDecodesPTZEncodingParameters(t *testing.T) {
	var input gbDeviceControlInput
	body := []byte(`{
		"action":"ptz_cmd",
		"ptz_cmd":"cruise_speed",
		"ptz_speed":30,
		"ptz_preset":7,
		"ptz_group":2,
		"ptz_aux":3,
		"ptz_value":120,
		"control_priority":5,
		"extra_info":[" first ","second"],
		"ptz_cmd_param":{"cruise_track_name":"day"}
	}`)
	if err := json.Unmarshal(body, &input); err != nil {
		t.Fatal(err)
	}
	if input.Action != "ptz_cmd" || input.PTZCmd != "cruise_speed" || input.PTZSpeed != 30 || input.PTZPreset != 7 ||
		input.PTZGroup != 2 || input.PTZAux != 3 || input.PTZValue != 120 || input.ControlPriority == nil || *input.ControlPriority != 5 ||
		input.PTZCmdParam == nil || input.PTZCmdParam.CruiseTrackName != "day" || len(input.ExtraInfo) != 2 || input.ExtraInfo[0] != " first " {
		t.Fatalf("decoded PTZ control = %+v", input)
	}
}

func TestGBDeviceQueryInputDecodesRecordFilters(t *testing.T) {
	var input gbDeviceQueryInput
	body := []byte(`{
			"action":"record_info","target_id":"34020000001320000002","start":1,"end":2,
			"file_path":"/record/front-gate.ps","address":"front-gate","secrecy":1,"recorder_id":"recorder-main",
			"type":"all","indistinct_query":1,"stream_number":2,"alarm_method":"2/5","alarm_type":"2"
	}`)
	if err := json.Unmarshal(body, &input); err != nil {
		t.Fatal(err)
	}
	if input.Action != "record_info" || input.TargetID != "34020000001320000002" || input.Start != 1 || input.End != 2 || input.Type != "all" ||
		input.FilePath != "/record/front-gate.ps" || input.Address != "front-gate" || input.Secrecy == nil || *input.Secrecy != 1 ||
		input.RecorderID != "recorder-main" ||
		input.IndistinctQuery == nil || *input.IndistinctQuery != 1 || input.StreamNumber == nil || *input.StreamNumber != 2 ||
		input.AlarmMethod != "2/5" || input.AlarmType != "2" {
		t.Fatalf("decoded DeviceQuery = %+v", input)
	}
}

func TestQueryRecordsInputDecodesRecordFilters(t *testing.T) {
	var input queryRecordsInput
	if err := json.Unmarshal([]byte(`{
		"start_at":1,"end_at":2,"file_path":"/record/front-gate.ps","address":"front-gate",
		"secrecy":1,"recorder_id":"recorder-main","indistinct_query":1
	}`), &input); err != nil {
		t.Fatal(err)
	}
	if input.StartAt != 1 || input.EndAt != 2 || input.FilePath != "/record/front-gate.ps" || input.Address != "front-gate" ||
		input.Secrecy == nil || *input.Secrecy != 1 || input.RecorderID != "recorder-main" ||
		input.IndistinctQuery == nil || *input.IndistinctQuery != 1 {
		t.Fatalf("decoded RecordInfo input = %+v", input)
	}
}
