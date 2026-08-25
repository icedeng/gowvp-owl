package gbs

import (
	"encoding/xml"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const snapShotConfig = "SnapShotConfig" // 图像抓拍配置

// 设备配置 A.2.3.2.1
type DeviceConfigRequest struct {
	XMLName             xml.Name               `xml:"Control"`
	CmdType             string                 `xml:"CmdType"`  // 命令类型：设备配置查询(必选)
	SN                  int32                  `xml:"SN"`       // 命令序列号(必选)
	DeviceID            string                 `xml:"DeviceID"` // 目标设备编码(必选)
	BasicParam          *BasicParam            `xml:"BasicParam,omitempty"`
	VideoParamConfig    *VideoParamConfigWrite `xml:"VideoParamConfig,omitempty"`
	AudioParamConfig    *AudioParamConfigWrite `xml:"AudioParamConfig,omitempty"`
	SVACEncodeConfig    *SVACEncodeConfig      `xml:"SVACEncodeConfig,omitempty"`
	SVACDecodeConfig    *SVACDecodeConfig      `xml:"SVACDecodeConfig,omitempty"`
	VideoParamAttribute *VideoParamAttribute   `xml:"VideoParamAttribute,omitempty"`
	VideoRecordPlan     *VideoRecordPlan       `xml:"VideoRecordPlan,omitempty"`
	VideoAlarmRecord    *VideoAlarmRecord      `xml:"VideoAlarmRecord,omitempty"`
	PictureMask         *PictureMask           `xml:"PictureMask,omitempty"`
	FrameMirror         *FrameMirror           `xml:"FrameMirror,omitempty"`
	AlarmReport         *AlarmReport           `xml:"AlarmReport,omitempty"`
	OSDConfig           *OSDConfig             `xml:"OSDConfig,omitempty"`
	SnapShotConfig      *SnapShot              `xml:"SnapShotConfig,omitempty"`
}

type VideoParamConfigWrite struct {
	Num   int                   `xml:"Num,attr" json:"num"`
	Items []VideoParamWriteItem `xml:"Item" json:"items"`
}

type VideoParamWriteItem struct {
	StreamName   string `xml:"StreamName" json:"stream_name"`
	VideoFormat  string `xml:"VideoFormat" json:"video_format"`
	Resolution   string `xml:"Resolution" json:"resolution"`
	FrameRate    string `xml:"FrameRate" json:"frame_rate"`
	BitRateType  string `xml:"BitRateType" json:"bit_rate_type"`
	VideoBitRate string `xml:"VideoBitRate" json:"video_bit_rate"`
}

type AudioParamConfigWrite struct {
	Num   int                   `xml:"Num,attr" json:"num"`
	Items []AudioParamWriteItem `xml:"Item" json:"items"`
}

type AudioParamWriteItem struct {
	StreamName   string `xml:"StreamName" json:"stream_name"`
	AudioFormat  string `xml:"AudioFormat" json:"audio_format"`
	AudioBitRate string `xml:"AudioBitRate" json:"audio_bit_rate"`
	SamplingRate string `xml:"SamplingRate" json:"sampling_rate"`
}

func (d *DeviceConfigRequest) SetBasicParam(param *BasicParam) *DeviceConfigRequest {
	d.BasicParam = param
	return d
}

func NewDeviceConfig(deviceID string) *DeviceConfigRequest {
	return &DeviceConfigRequest{
		CmdType:  "DeviceConfig",
		SN:       1,
		DeviceID: deviceID,
	}
}

func (d *DeviceConfigRequest) SetSN(sn int32) *DeviceConfigRequest {
	d.SN = sn
	return d
}

func (d *DeviceConfigRequest) SetSnapShotConfig(snapShot *SnapShot) *DeviceConfigRequest {
	d.SnapShotConfig = snapShot
	return d
}

func (d *DeviceConfigRequest) Marshal() []byte {
	b, _ := sip.XMLEncode(d)
	return b
}
