package gbadapter

import (
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
)

func TestToGBDeviceConfigInputMapsAll2014Sections(t *testing.T) {
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
		SVACEncodeConfig: &ipc.GBXMLConfigInput{InnerXML: "<SVCParam/>"},
		SVACDecodeConfig: &ipc.GBXMLConfigInput{InnerXML: "<SVCParam/>"},
	})
	if input == nil || input.DeviceID != "34020000001320000001" || input.TargetID != "34020000001320000002" || input.Timeout.Seconds() != 9 {
		t.Fatalf("mapped DeviceConfig identity = %+v", input)
	}
	if input.BasicParam == nil || input.BasicParam.HeartBeatCount != 3 ||
		input.VideoParamConfig == nil || len(input.VideoParamConfig.Items) != 1 || input.VideoParamConfig.Items[0].VideoBitRate != "4096" ||
		input.AudioParamConfig == nil || len(input.AudioParamConfig.Items) != 1 || input.AudioParamConfig.Items[0].SamplingRate != "8" ||
		input.SVACEncodeConfig == nil || input.SVACDecodeConfig == nil {
		t.Fatalf("mapped DeviceConfig sections = %+v", input)
	}
}
