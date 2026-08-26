package api

import (
	"encoding/json"
	"testing"
)

func TestGBDeviceConfigInputDecodesAllVersionedSections(t *testing.T) {
	var input gbDeviceConfigInput
	body := []byte(`{
		"target_id":"34020000001320000002",
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
	if input.VideoParamConfig == nil || len(input.VideoParamConfig.Items) != 1 || input.VideoParamConfig.Items[0].VideoBitRate != "4096" ||
		input.AudioParamConfig == nil || len(input.AudioParamConfig.Items) != 1 || input.AudioParamConfig.Items[0].SamplingRate != "8" ||
		input.SVACEncodeConfig == nil || input.SVACDecodeConfig == nil || input.VideoParamAttribute == nil ||
		input.VideoRecordPlan == nil || input.VideoAlarmRecord == nil || input.PictureMask == nil ||
		input.FrameMirror == nil || input.AlarmReport == nil || input.OSDConfig == nil || input.SnapShotConfig == nil ||
		input.SnapShotConfig.SessionID != "snapshot-session-0000000000000001" {
		t.Fatalf("decoded DeviceConfig = %+v", input)
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

func TestGBDeviceQueryInputDecodesRecordFilters(t *testing.T) {
	var input gbDeviceQueryInput
	body := []byte(`{
		"action":"record_info","target_id":"34020000001320000002","start":1,"end":2,
		"type":"all","stream_number":2,"alarm_method":"2/5","alarm_type":"2"
	}`)
	if err := json.Unmarshal(body, &input); err != nil {
		t.Fatal(err)
	}
	if input.Action != "record_info" || input.TargetID != "34020000001320000002" || input.Start != 1 || input.End != 2 || input.Type != "all" ||
		input.StreamNumber == nil || *input.StreamNumber != 2 || input.AlarmMethod != "2/5" || input.AlarmType != "2" {
		t.Fatalf("decoded DeviceQuery = %+v", input)
	}
}
