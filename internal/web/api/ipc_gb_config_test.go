package api

import (
	"encoding/json"
	"testing"
)

func TestGBDeviceConfigInputDecodesAll2014Sections(t *testing.T) {
	var input gbDeviceConfigInput
	body := []byte(`{
		"target_id":"34020000001320000002",
		"video_param_config":{"items":[{"stream_name":"Stream1","video_format":"H.264","resolution":"1920x1080","frame_rate":"25","bit_rate_type":"1","video_bit_rate":"4096"}]},
		"audio_param_config":{"items":[{"stream_name":"Stream1","audio_format":"G.711","audio_bit_rate":"64","sampling_rate":"8"}]},
		"svac_encode_config":{"inner_xml":"<SVCParam><SVCFlag>1</SVCFlag></SVCParam>"},
		"svac_decode_config":{"inner_xml":"<SVCParam><SVCSTMMode>1</SVCSTMMode></SVCParam>"}
	}`)
	if err := json.Unmarshal(body, &input); err != nil {
		t.Fatal(err)
	}
	if input.VideoParamConfig == nil || len(input.VideoParamConfig.Items) != 1 || input.VideoParamConfig.Items[0].VideoBitRate != "4096" ||
		input.AudioParamConfig == nil || len(input.AudioParamConfig.Items) != 1 || input.AudioParamConfig.Items[0].SamplingRate != "8" ||
		input.SVACEncodeConfig == nil || input.SVACDecodeConfig == nil {
		t.Fatalf("decoded DeviceConfig = %+v", input)
	}
}
