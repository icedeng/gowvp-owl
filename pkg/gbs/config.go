package gbs

import (
	"context"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

// 配置参数类型常量定义
const (
	// basicParam 基本参数配置
	basicParam = "BasicParam"
	// videoParamOpt 视频参数范围配置
	// videoParamOpt = "VideoParamOpt"
	// // SVACEncodeConfig SVAC编码配置
	// SVACEncodeConfig = "SVACEncodeConfig"
	// // SVACDecodeConfig SVAC解码配置
	// SVACDecodeConfig = "SVACDecodeConfig"
	// // videoParamAttribute 视频参数属性配置
	// videoParamAttribute = "VideoParamAttribute"
	// // videoRecordPlan 录像计划
	// videoRecordPlan = "VideoRecordPlan"
	// // videoAlarmRecord 报警录像
	// videoAlarmRecord = "VideoAlarmRecord"
	// // pictureMask 视频画面遮挡
	// pictureMask = "PictureMask"
	// // frameMirror 画面翻转
	// frameMirror = "FrameMirror"
	// // AlarmReport 报警上报开关
	// AlarmReport = "AlarmReport"
	// // OSDConfig 前端OSD设置
	// OSDConfig = "OSDConfig"
)

type ConfigDownloadRequest struct {
	XMLName        xml.Name  `xml:"Query"`
	CmdType        string    `xml:"CmdType"`    // 命令类型：设备配置查询(必选)
	SN             int32     `xml:"SN"`         // 命令序列号(必选)
	DeviceID       string    `xml:"DeviceID"`   // 目标设备编码(必选)
	ConfigType     string    `xml:"ConfigType"` // 查询配置参数类型(必选)
	SnapShotConfig *SnapShot `xml:"SnapShotConfig"`
}

type ConfigDownloadResponse struct {
	XMLName             xml.Name             `xml:"Response"`
	CmdType             string               `xml:"CmdType"`
	SN                  int                  `xml:"SN"`
	DeviceID            string               `xml:"DeviceID"`
	Result              string               `xml:"Result"`
	BasicParam          *BasicParam          `xml:"BasicParam"`
	VideoParamOpt       *VideoParamOpt       `xml:"VideoParamOpt"`
	VideoParamConfig    *VideoParamConfig    `xml:"VideoParamConfig"`
	AudioParamOpt       *AudioParamOpt       `xml:"AudioParamOpt"`
	AudioParamConfig    *AudioParamConfig    `xml:"AudioParamConfig"`
	SVACEncodeConfig    *SVACEncodeConfig    `xml:"SVACEncodeConfig"`
	SVACDecodeConfig    *SVACDecodeConfig    `xml:"SVACDecodeConfig"`
	VideoParamAttribute *VideoParamAttribute `xml:"VideoParamAttribute"`
	VideoRecordPlan     *VideoRecordPlan     `xml:"VideoRecordPlan"`
	VideoAlarmRecord    *VideoAlarmRecord    `xml:"VideoAlarmRecord"`
	PictureMask         *PictureMask         `xml:"PictureMask"`
	FrameMirror         *FrameMirror         `xml:"FrameMirror"`
	AlarmReport         *AlarmReport         `xml:"AlarmReport"`
	OSDConfig           *OSDConfig           `xml:"OSDConfig"`
	// SnapShotConfig 兼容部分厂商在查询应答中沿用配置命令节点名。
	SnapShotConfig *SnapShot `xml:"SnapShotConfig"`
	// SnapShot 是 2022 A.2.6.9 规定的配置查询应答节点。
	SnapShot *SnapShot `xml:"SnapShot"`
}

type SnapShot struct {
	SnapNum   int    `xml:"SnapNum"`   // 连拍张数(必选)，最多10张，当手动抓拍时，取值为1
	Interval  int    `xml:"Interval"`  // 单张抓拍间隔时间，单位：秒(必选)，取值范围:最短1秒
	UploadURL string `xml:"UploadURL"` // 抓拍图像上传路径(必选)
	SessionID string `xml:"SessionID"` // 会话ID，由平台生成，用于关联抓拍的图像与平台请求(必选)
}

// BasicParam 设备基本参数配置
type BasicParam struct {
	Name              string `xml:"Name,omitempty"`              // 设备名称
	DeviceID          string `xml:"DeviceID,omitempty"`          // 设备 ID
	SIPServerID       string `xml:"SIPServerID,omitempty"`       // SIP 服务器 ID
	SIPServerIP       string `xml:"SIPServerIP,omitempty"`       // SIP 服务器 IP
	SIPServerPort     int    `xml:"SIPServerPort,omitempty"`     // SIP 服务器端口
	DomainName        string `xml:"DomainName,omitempty"`        // SIP 服务器域
	Expiration        int    `xml:"Expiration,omitempty"`        // 注册过期时间
	Password          string `xml:"Password,omitempty" json:"-"` // 注册口令
	HeartBeatInterval int    `xml:"HeartBeatInterval,omitempty"` // 心跳间隔时间
	HeartBeatCount    int    `xml:"HeartBeatCount,omitempty"`    // 心跳超时次数
	present           basicParamPresence
}

type basicParamPresence struct {
	Name              bool
	DeviceID          bool
	SIPServerID       bool
	SIPServerIP       bool
	SIPServerPort     bool
	DomainName        bool
	Expiration        bool
	Password          bool
	HeartBeatInterval bool
	HeartBeatCount    bool
}

func (param *BasicParam) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	var decoded struct {
		Name              *string `xml:"Name"`
		DeviceID          *string `xml:"DeviceID"`
		SIPServerID       *string `xml:"SIPServerID"`
		SIPServerIP       *string `xml:"SIPServerIP"`
		SIPServerPort     *int    `xml:"SIPServerPort"`
		DomainName        *string `xml:"DomainName"`
		Expiration        *int    `xml:"Expiration"`
		Password          *string `xml:"Password"`
		HeartBeatInterval *int    `xml:"HeartBeatInterval"`
		HeartBeatCount    *int    `xml:"HeartBeatCount"`
	}
	if err := decoder.DecodeElement(&decoded, &start); err != nil {
		return err
	}
	if decoded.Name != nil {
		param.Name, param.present.Name = *decoded.Name, true
	}
	if decoded.DeviceID != nil {
		param.DeviceID, param.present.DeviceID = *decoded.DeviceID, true
	}
	if decoded.SIPServerID != nil {
		param.SIPServerID, param.present.SIPServerID = *decoded.SIPServerID, true
	}
	if decoded.SIPServerIP != nil {
		param.SIPServerIP, param.present.SIPServerIP = *decoded.SIPServerIP, true
	}
	if decoded.SIPServerPort != nil {
		param.SIPServerPort, param.present.SIPServerPort = *decoded.SIPServerPort, true
	}
	if decoded.DomainName != nil {
		param.DomainName, param.present.DomainName = *decoded.DomainName, true
	}
	if decoded.Expiration != nil {
		param.Expiration, param.present.Expiration = *decoded.Expiration, true
	}
	if decoded.Password != nil {
		param.Password, param.present.Password = *decoded.Password, true
	}
	if decoded.HeartBeatInterval != nil {
		param.HeartBeatInterval, param.present.HeartBeatInterval = *decoded.HeartBeatInterval, true
	}
	if decoded.HeartBeatCount != nil {
		param.HeartBeatCount, param.present.HeartBeatCount = *decoded.HeartBeatCount, true
	}
	return nil
}

func (param *BasicParam) hasBothHeartbeatFields() bool {
	return param != nil && param.present.HeartBeatInterval && param.present.HeartBeatCount
}

// 下述配置结构体采用 innerxml 承接，保证协议字段兼容且不阻塞解析。
// 后续可按业务需要继续细化字段。
type VideoParamOpt struct {
	InnerXML string `xml:",innerxml" json:"inner_xml"`
}
type VideoParamConfig struct {
	InnerXML string `xml:",innerxml" json:"inner_xml"`
}
type AudioParamOpt struct {
	InnerXML string `xml:",innerxml" json:"inner_xml"`
}
type AudioParamConfig struct {
	InnerXML string `xml:",innerxml" json:"inner_xml"`
}
type SVACEncodeConfig struct {
	InnerXML string `xml:",innerxml" json:"inner_xml"`
}
type SVACDecodeConfig struct {
	InnerXML string `xml:",innerxml" json:"inner_xml"`
}
type VideoParamAttribute struct {
	Num        int    `xml:"Num,attr" json:"num"`
	InnerXML   string `xml:",innerxml" json:"inner_xml"`
	numPresent bool
}
type VideoRecordPlan struct {
	InnerXML string `xml:",innerxml" json:"inner_xml"`
}
type VideoAlarmRecord struct {
	InnerXML string `xml:",innerxml" json:"inner_xml"`
}
type PictureMask struct {
	InnerXML string `xml:",innerxml" json:"inner_xml"`
}
type FrameMirror struct {
	InnerXML string `xml:",innerxml" json:"inner_xml"`
}
type AlarmReport struct {
	InnerXML string `xml:",innerxml" json:"inner_xml"`
}
type OSDConfig struct {
	InnerXML string `xml:",innerxml" json:"inner_xml"`
}

// DeviceConfigResponse 是 9.7 设备配置应答。
type DeviceConfigResponse struct {
	XMLName        xml.Name  `xml:"Response"`
	CmdType        string    `xml:"CmdType"`
	SN             int       `xml:"SN"`
	DeviceID       string    `xml:"DeviceID"`
	Result         string    `xml:"Result"`
	SnapShotConfig *SnapShot `xml:"SnapShotConfig"`
	RawXML         string    `xml:"-" json:"-"`
}

// BasicParamConfigInput 是 2014 修改补充文件定义的基础参数写入请求。
type BasicParamConfigInput struct {
	DeviceID string
	TargetID string
	Timeout  time.Duration
	Param    BasicParam
}

// DeviceConfigInput 是 2014 修改补充文件定义的统一设备配置写入请求。
type DeviceConfigInput struct {
	DeviceID            string
	TargetID            string
	Timeout             time.Duration
	BasicParam          *BasicParam
	VideoParamConfig    *VideoParamConfigWrite
	AudioParamConfig    *AudioParamConfigWrite
	SVACEncodeConfig    *SVACEncodeConfig
	SVACDecodeConfig    *SVACDecodeConfig
	VideoParamAttribute *VideoParamAttribute
	VideoRecordPlan     *VideoRecordPlan
	VideoAlarmRecord    *VideoAlarmRecord
	PictureMask         *PictureMask
	FrameMirror         *FrameMirror
	AlarmReport         *AlarmReport
	OSDConfig           *OSDConfig
	SnapShotConfig      *SnapShot
}

type pendingDeviceConfig struct {
	wait     chan *DeviceConfigResponse
	targetID string
}

const CMDTypeConfigDownload = "ConfigDownload"

func NewBasicParamRequest(sn int32, deviceID string) []byte {
	c := ConfigDownloadRequest{
		CmdType:    CMDTypeConfigDownload,
		SN:         sn,
		DeviceID:   deviceID,
		ConfigType: basicParam,
	}
	xmlData, _ := sip.XMLEncode(c)
	return xmlData
}

func (g *GB28181API) newBasicParamRequest(deviceID string) []byte {
	return NewBasicParamRequest(int32(g.nextQuerySN()), deviceID)
}

func (g *GB28181API) QueryConfigDownloadBasic(deviceID string) error {
	slog.Debug("QueryConfigDownloadBasic", "deviceID", deviceID)
	ipc, ok := g.svr.memoryStorer.Load(deviceID)
	if !ok || !ipc.IsOnlineNow() {
		return ErrDeviceOffline
	}

	tx, err := g.svr.wrapRequest(ipc, sip.MethodMessage, &sip.ContentTypeXML, g.newBasicParamRequest(deviceID))
	if err != nil {
		return err
	}
	_, err = sipResponse(tx)
	return err
}

func (g *GB28181API) handleDeviceConfig(ctx *sip.Context) {
	slog.Debug("handleDeviceConfig", "deviceID", ctx.DeviceID)
	var msg DeviceConfigResponse
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.Log.Error("handleDeviceConfig", "err", err, "body", hex.EncodeToString(ctx.Request.Body()))
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	msg.CmdType = strings.TrimSpace(msg.CmdType)
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	msg.Result = strings.TrimSpace(msg.Result)
	if msg.XMLName.Local != "Response" || !strings.EqualFold(msg.CmdType, "DeviceConfig") || !isGBResultValue(msg.Result) {
		ctx.String(400, "invalid DeviceConfig response")
		return
	}
	if err := g.validateGenericDeviceQueryResponse(ctx, genericDeviceQueryResponse{
		XMLName: msg.XMLName, CmdType: msg.CmdType, SN: msg.SN, DeviceID: msg.DeviceID, Result: msg.Result,
	}); err != nil {
		ctx.String(400, err.Error())
		return
	}
	waitKey := buildPendingDeviceConfigKey(ctx.DeviceID, msg.SN)
	var pending *pendingDeviceConfig
	if value, ok := g.pendingDeviceConfig.Load(waitKey); ok {
		var pendingOK bool
		pending, pendingOK = value.(*pendingDeviceConfig)
		if !pendingOK || pending == nil {
			ctx.String(400, "invalid DeviceConfig pending state")
			return
		}
		if expected := strings.TrimSpace(pending.targetID); expected != "" && expected != msg.DeviceID {
			ctx.String(400, "DeviceConfig response target mismatch")
			return
		}
	}
	msg.RawXML = string(ctx.Request.Body())

	state := &DeviceConfigState{
		CmdType:  msg.CmdType,
		SN:       msg.SN,
		DeviceID: msg.DeviceID,
		Result:   strings.TrimSpace(msg.Result),
		SnapShot: msg.SnapShotConfig,
		RawXML:   msg.RawXML,
	}
	g.storeDeviceConfigState(ctx.DeviceID, state)
	ext := g.decodeAppendixA4Objects(msg.CmdType, ctx.Request.Body())
	if len(ext) > 0 {
		g.storeAppendixA4State(ctx.DeviceID, ext)
	}

	if pending != nil {
		select {
		case pending.wait <- &msg:
		default:
		}
	}

	ctx.String(200, "OK")
	if len(ext) > 0 {
		g.persistAppendixA4Objects(ctx.DeviceID, ext)
	}
}

// SetBasicParam 保留原有 BasicParam API，并复用统一 DeviceConfig 写入链路。
func (g *GB28181API) SetBasicParam(ctx context.Context, in *BasicParamConfigInput) (*DeviceConfigState, error) {
	if in == nil {
		return nil, fmt.Errorf("invalid BasicParam config input")
	}
	param := in.Param
	return g.SetDeviceConfig(ctx, &DeviceConfigInput{
		DeviceID: in.DeviceID, TargetID: in.TargetID, Timeout: in.Timeout, BasicParam: &param,
	})
}

// SetDeviceConfig 下发 2014 DeviceConfig 并等待设备的业务应答。
func (g *GB28181API) SetDeviceConfig(ctx context.Context, in *DeviceConfigInput) (*DeviceConfigState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in == nil || strings.TrimSpace(in.DeviceID) == "" {
		return nil, fmt.Errorf("invalid DeviceConfig input")
	}
	deviceID := strings.TrimSpace(in.DeviceID)
	if err := g.requireGBFeature(deviceID, "config_write", "设备配置(DeviceConfig)", func(c GBCapabilities) bool {
		return c.ConfigWrite
	}); err != nil {
		return nil, err
	}
	device, ok := g.svr.memoryStorer.Load(deviceID)
	if !ok || !device.IsOnlineNow() {
		return nil, ErrDeviceOffline
	}
	targetID := strings.TrimSpace(in.TargetID)
	if targetID == "" {
		targetID = deviceID
	}
	var target Targeter = device
	if targetID != deviceID {
		channel, ok := g.svr.memoryStorer.GetChannel(deviceID, targetID)
		if !ok {
			return nil, ErrChannelNotExist
		}
		target = channel
	}
	request, err := g.buildDeviceConfigRequest(targetID, device, in)
	if err != nil {
		return nil, err
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}

	sn := int32(g.nextControlSN())
	request.SetSN(sn)
	body, err := sip.XMLEncode(request)
	if err != nil {
		return nil, err
	}
	waitKey := buildPendingDeviceConfigKey(deviceID, int(sn))
	pending := &pendingDeviceConfig{wait: make(chan *DeviceConfigResponse, 1), targetID: targetID}
	g.pendingDeviceConfig.Store(waitKey, pending)
	defer g.pendingDeviceConfig.Delete(waitKey)

	tx, err := g.svr.wrapRequestContext(ctx, target, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		return nil, err
	}
	if _, err = sipResponseContext(ctx, tx); err != nil {
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case response := <-pending.wait:
		state := &DeviceConfigState{
			CmdType:  strings.TrimSpace(response.CmdType),
			SN:       response.SN,
			DeviceID: strings.TrimSpace(response.DeviceID),
			Result:   strings.TrimSpace(response.Result),
			SnapShot: response.SnapShotConfig,
			RawXML:   response.RawXML,
		}
		if !strings.EqualFold(state.Result, "OK") {
			return state, fmt.Errorf("DeviceConfig failed: %s", state.Result)
		}
		return state, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.serviceDone():
		return nil, ErrServiceStopped
	case <-timer.C:
		return nil, fmt.Errorf("wait DeviceConfig response timeout")
	}
}

func (g *GB28181API) buildDeviceConfigRequest(targetID string, device *Device, in *DeviceConfigInput) (*DeviceConfigRequest, error) {
	request := NewDeviceConfig(targetID)
	version := g.deviceConfigVersion(in.DeviceID, device)
	configured := false
	if in.BasicParam != nil {
		param, err := g.prepareBasicParam(targetID, device, *in.BasicParam, version)
		if err != nil {
			return nil, err
		}
		request.BasicParam = &param
		configured = true
	}
	if in.VideoParamConfig != nil {
		if !deviceConfigSectionSupported(version, "VideoParamConfig") {
			return nil, fmt.Errorf("VideoParamConfig is not supported by %s", version.StandardName())
		}
		config := *in.VideoParamConfig
		config.Items = append([]VideoParamWriteItem(nil), in.VideoParamConfig.Items...)
		if err := validateVideoParamConfig(&config); err != nil {
			return nil, err
		}
		config.Num = len(config.Items)
		request.VideoParamConfig = &config
		configured = true
	}
	if in.AudioParamConfig != nil {
		if !deviceConfigSectionSupported(version, "AudioParamConfig") {
			return nil, fmt.Errorf("AudioParamConfig is not supported by %s", version.StandardName())
		}
		config := *in.AudioParamConfig
		config.Items = append([]AudioParamWriteItem(nil), in.AudioParamConfig.Items...)
		if err := validateAudioParamConfig(&config); err != nil {
			return nil, err
		}
		config.Num = len(config.Items)
		request.AudioParamConfig = &config
		configured = true
	}
	if in.SVACEncodeConfig != nil {
		if !deviceConfigSectionSupported(version, "SVACEncodeConfig") {
			return nil, fmt.Errorf("SVACEncodeConfig is not supported by %s", version.StandardName())
		}
		config := *in.SVACEncodeConfig
		if err := validateDeviceConfigXMLFragment("SVACEncodeConfig", config.InnerXML); err != nil {
			return nil, err
		}
		request.SVACEncodeConfig = &config
		configured = true
	}
	if in.SVACDecodeConfig != nil {
		if !deviceConfigSectionSupported(version, "SVACDecodeConfig") {
			return nil, fmt.Errorf("SVACDecodeConfig is not supported by %s", version.StandardName())
		}
		config := *in.SVACDecodeConfig
		if err := validateDeviceConfigXMLFragment("SVACDecodeConfig", config.InnerXML); err != nil {
			return nil, err
		}
		request.SVACDecodeConfig = &config
		configured = true
	}
	sections2022 := []struct {
		name    string
		value   string
		present bool
		set     func(string)
	}{
		{"VideoParamAttribute", innerXML(in.VideoParamAttribute), in.VideoParamAttribute != nil, func(value string) { request.VideoParamAttribute = &VideoParamAttribute{InnerXML: value} }},
		{"VideoRecordPlan", innerXML(in.VideoRecordPlan), in.VideoRecordPlan != nil, func(value string) { request.VideoRecordPlan = &VideoRecordPlan{InnerXML: value} }},
		{"VideoAlarmRecord", innerXML(in.VideoAlarmRecord), in.VideoAlarmRecord != nil, func(value string) { request.VideoAlarmRecord = &VideoAlarmRecord{InnerXML: value} }},
		{"PictureMask", innerXML(in.PictureMask), in.PictureMask != nil, func(value string) { request.PictureMask = &PictureMask{InnerXML: value} }},
		{"FrameMirror", innerXML(in.FrameMirror), in.FrameMirror != nil, func(value string) { request.FrameMirror = &FrameMirror{InnerXML: value} }},
		{"AlarmReport", innerXML(in.AlarmReport), in.AlarmReport != nil, func(value string) { request.AlarmReport = &AlarmReport{InnerXML: value} }},
		{"OSDConfig", innerXML(in.OSDConfig), in.OSDConfig != nil, func(value string) { request.OSDConfig = &OSDConfig{InnerXML: value} }},
	}
	for _, section := range sections2022 {
		if !section.present {
			continue
		}
		if err := g.requireGBVersionAtLeast(in.DeviceID, gbVersion2022, "设备配置("+section.name+")"); err != nil {
			return nil, err
		}
		if err := validateDeviceConfigXMLFragment(section.name, section.value); err != nil {
			return nil, err
		}
		if section.name == "VideoParamAttribute" {
			config := *in.VideoParamAttribute
			if err := validateVideoParamAttribute30(&config, false); err != nil {
				return nil, err
			}
			request.VideoParamAttribute = &config
			configured = true
			continue
		}
		if err := validateDeviceConfig30Fragment(section.name, section.value); err != nil {
			return nil, err
		}
		section.set(section.value)
		configured = true
	}
	if in.SnapShotConfig != nil {
		if err := g.requireGBVersionAtLeast(in.DeviceID, gbVersion2022, "设备配置(SnapShotConfig)"); err != nil {
			return nil, err
		}
		if err := g.requireGBFeature(in.DeviceID, "snapshot", "设备配置(SnapShotConfig)", func(c GBCapabilities) bool { return c.Snapshot }); err != nil {
			return nil, err
		}
		config := *in.SnapShotConfig
		if err := validateSnapshotConfig(&config); err != nil {
			return nil, err
		}
		config.UploadURL = strings.TrimSpace(config.UploadURL)
		config.SessionID = strings.TrimSpace(config.SessionID)
		request.SnapShotConfig = &config
		configured = true
	}
	if !configured {
		return nil, fmt.Errorf("DeviceConfig requires at least one configuration section")
	}
	return request, nil
}

func (g *GB28181API) deviceConfigVersion(deviceID string, device *Device) GBProtocolVersion {
	if device != nil {
		if version, ok := ParseGBProtocolVersion(device.GBVersion()); ok {
			return version
		}
	}
	if g != nil && g.svr != nil && g.svr.memoryStorer != nil {
		return g.getDeviceGBProtocolVersion(deviceID)
	}
	// 直接构造请求的兼容 API 源自 2014 修改补充文件；无设备上下文时沿用该档案。
	return GBVersion11
}

func validateSnapshotConfig(config *SnapShot) error {
	if config == nil || config.SnapNum < 1 || config.SnapNum > 10 || config.Interval < 1 || strings.TrimSpace(config.UploadURL) == "" {
		return fmt.Errorf("SnapShotConfig requires snap_num 1~10, interval >= 1 and upload_url")
	}
	if err := validateGBSessionID(strings.TrimSpace(config.SessionID)); err != nil {
		return fmt.Errorf("SnapShotConfig: %w", err)
	}
	return nil
}

func innerXML(value any) string {
	switch section := value.(type) {
	case *VideoParamAttribute:
		if section != nil {
			return strings.TrimSpace(section.InnerXML)
		}
	case *VideoRecordPlan:
		if section != nil {
			return strings.TrimSpace(section.InnerXML)
		}
	case *VideoAlarmRecord:
		if section != nil {
			return strings.TrimSpace(section.InnerXML)
		}
	case *PictureMask:
		if section != nil {
			return strings.TrimSpace(section.InnerXML)
		}
	case *FrameMirror:
		if section != nil {
			return strings.TrimSpace(section.InnerXML)
		}
	case *AlarmReport:
		if section != nil {
			return strings.TrimSpace(section.InnerXML)
		}
	case *OSDConfig:
		if section != nil {
			return strings.TrimSpace(section.InnerXML)
		}
	}
	return ""
}

func validateVideoParamConfig(config *VideoParamConfigWrite) error {
	if config == nil || len(config.Items) == 0 {
		return fmt.Errorf("VideoParamConfig requires at least one item")
	}
	for index, item := range config.Items {
		if strings.TrimSpace(item.StreamName) == "" || strings.TrimSpace(item.VideoFormat) == "" ||
			strings.TrimSpace(item.Resolution) == "" || strings.TrimSpace(item.FrameRate) == "" ||
			strings.TrimSpace(item.BitRateType) == "" || strings.TrimSpace(item.VideoBitRate) == "" {
			return fmt.Errorf("VideoParamConfig item %d requires all standard fields", index+1)
		}
	}
	return nil
}

func validateAudioParamConfig(config *AudioParamConfigWrite) error {
	if config == nil || len(config.Items) == 0 {
		return fmt.Errorf("AudioParamConfig requires at least one item")
	}
	for index, item := range config.Items {
		if strings.TrimSpace(item.StreamName) == "" || strings.TrimSpace(item.AudioFormat) == "" ||
			strings.TrimSpace(item.AudioBitRate) == "" || strings.TrimSpace(item.SamplingRate) == "" {
			return fmt.Errorf("AudioParamConfig item %d requires all standard fields", index+1)
		}
	}
	return nil
}

func validateDeviceConfigXMLFragment(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s inner_xml is required", name)
	}
	decoder := xml.NewDecoder(strings.NewReader("<DeviceConfigFragment>" + value + "</DeviceConfigFragment>"))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s inner_xml is invalid: %w", name, err)
		}
		switch token.(type) {
		case xml.Directive, xml.ProcInst:
			return fmt.Errorf("%s inner_xml contains unsupported XML directive", name)
		}
	}
}

func (g *GB28181API) completeBasicParam(targetID string, device *Device, in BasicParam) (BasicParam, error) {
	cfg := g.configSnapshot()
	if cfg == nil {
		return BasicParam{}, fmt.Errorf("SIP configuration is unavailable")
	}
	out := in
	out.DeviceID = strings.TrimSpace(targetID)
	out.SIPServerID = strings.TrimSpace(cfg.ID)
	out.SIPServerIP = strings.TrimSpace(cfg.Host)
	out.SIPServerPort = cfg.Port
	out.DomainName = strings.TrimSpace(cfg.GetDomain())
	if device != nil {
		out.Password = device.PasswordValue()
	}
	if out.Password == "" {
		out.Password = cfg.Password
	}
	if out.Password == ignorePassword {
		out.Password = ""
	}
	if g.svr != nil && g.svr.fromAddress.URI != nil {
		if out.SIPServerIP == "" {
			out.SIPServerIP = strings.TrimSpace(g.svr.fromAddress.URI.Host())
		}
		if out.SIPServerPort <= 0 && g.svr.fromAddress.URI.FPort != nil {
			out.SIPServerPort = int(*g.svr.fromAddress.URI.FPort)
		}
	}
	if out.SIPServerPort <= 0 {
		out.SIPServerPort = 5060
	}
	if out.DeviceID == "" || out.SIPServerID == "" || out.SIPServerIP == "" || out.DomainName == "" {
		return BasicParam{}, fmt.Errorf("BasicParam requires device and SIP server identity")
	}
	return out, nil
}

func (g *GB28181API) prepareBasicParam(targetID string, device *Device, in BasicParam, version GBProtocolVersion) (BasicParam, error) {
	if version == GBVersion11 {
		if strings.TrimSpace(in.Name) == "" || in.Expiration <= 0 || in.HeartBeatInterval <= 0 || in.HeartBeatCount <= 0 {
			return BasicParam{}, fmt.Errorf("BasicParam requires name, expiration and heartbeat values for %s", version.StandardName())
		}
		return g.completeBasicParam(targetID, device, in)
	}
	if !deviceConfigSectionSupported(version, "BasicParam") {
		return BasicParam{}, fmt.Errorf("BasicParam is not supported by %s", version.StandardName())
	}
	if in.Expiration < 0 || in.HeartBeatInterval < 0 || in.HeartBeatCount < 0 {
		return BasicParam{}, fmt.Errorf("BasicParam values must not be negative")
	}
	return BasicParam{
		Name: strings.TrimSpace(in.Name), Expiration: in.Expiration,
		HeartBeatInterval: in.HeartBeatInterval, HeartBeatCount: in.HeartBeatCount,
	}, nil
}

func validateBasicParamResponse(param *BasicParam, version GBProtocolVersion) error {
	if param == nil {
		return nil
	}
	if version == GBVersion11 {
		p := param.present
		if !p.Name || !p.DeviceID || !p.SIPServerID || !p.SIPServerIP || !p.SIPServerPort || !p.DomainName ||
			!p.Expiration || !p.Password || !p.HeartBeatInterval || !p.HeartBeatCount {
			return fmt.Errorf("ConfigDownload BasicParam is incomplete for %s", version.StandardName())
		}
	} else if version == GBVersion20 {
		p := param.present
		if !p.Name || !p.Expiration || !p.HeartBeatInterval || !p.HeartBeatCount {
			return fmt.Errorf("ConfigDownload BasicParam is incomplete for %s", version.StandardName())
		}
	} else if version != GBVersion30 {
		return fmt.Errorf("ConfigDownload BasicParam is not supported by %s", version.StandardName())
	}
	if param.present.Expiration && param.Expiration < 0 {
		return fmt.Errorf("ConfigDownload BasicParam expiration is invalid")
	}
	if param.present.HeartBeatInterval && (param.HeartBeatInterval <= 0 || param.HeartBeatInterval > 65535) {
		return fmt.Errorf("ConfigDownload BasicParam heartbeat interval is invalid")
	}
	if param.present.HeartBeatCount && (param.HeartBeatCount <= 0 || param.HeartBeatCount > 65535) {
		return fmt.Errorf("ConfigDownload BasicParam heartbeat count is invalid")
	}
	return nil
}

func buildPendingDeviceConfigKey(deviceID string, sn int) string {
	return strings.TrimSpace(deviceID) + ":" + strconv.Itoa(sn)
}

func (g *GB28181API) sipMessageConfigDownload(ctx *sip.Context) {
	slog.Debug("sipMessageConfigDownload", "deviceID", ctx.DeviceID)

	var msg ConfigDownloadResponse
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.Log.Error("sipMessageConfigDownload", "err", err, "body", hex.EncodeToString(ctx.Request.Body()))
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	msg.CmdType = strings.TrimSpace(msg.CmdType)
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	msg.Result = strings.TrimSpace(msg.Result)
	if !strings.EqualFold(msg.CmdType, CMDTypeConfigDownload) {
		ctx.String(400, "invalid ConfigDownload response")
		return
	}
	if err := g.validateGenericDeviceQueryResponse(ctx, genericDeviceQueryResponse{
		XMLName: msg.XMLName, CmdType: msg.CmdType, SN: msg.SN, DeviceID: msg.DeviceID, Result: msg.Result,
	}); err != nil {
		ctx.String(400, err.Error())
		return
	}
	if !isGBResultValue(msg.Result) {
		ctx.String(400, "invalid ConfigDownload result")
		return
	}

	resultOK := strings.EqualFold(msg.Result, "OK")
	version := g.getDeviceGBProtocolVersion(ctx.DeviceID)
	if resultOK {
		responseSections := []struct {
			name    string
			present bool
		}{
			{"BasicParam", msg.BasicParam != nil},
			{"VideoParamOpt", msg.VideoParamOpt != nil},
			{"VideoParamConfig", msg.VideoParamConfig != nil},
			{"AudioParamOpt", msg.AudioParamOpt != nil},
			{"AudioParamConfig", msg.AudioParamConfig != nil},
			{"SVACEncodeConfig", msg.SVACEncodeConfig != nil},
			{"SVACDecodeConfig", msg.SVACDecodeConfig != nil},
			{"VideoParamAttribute", msg.VideoParamAttribute != nil},
			{"VideoRecordPlan", msg.VideoRecordPlan != nil},
			{"VideoAlarmRecord", msg.VideoAlarmRecord != nil},
			{"PictureMask", msg.PictureMask != nil},
			{"FrameMirror", msg.FrameMirror != nil},
			{"AlarmReport", msg.AlarmReport != nil},
			{"OSDConfig", msg.OSDConfig != nil},
			{"SnapShotConfig", msg.SnapShotConfig != nil || msg.SnapShot != nil},
		}
		for _, section := range responseSections {
			if section.present && !configTypeSupported(version, section.name) {
				ctx.String(400, "ConfigDownload "+section.name+" is not supported by "+version.StandardName())
				return
			}
		}
		sections := []struct {
			name    string
			value   string
			present bool
		}{
			{"VideoParamAttribute", innerXML(msg.VideoParamAttribute), msg.VideoParamAttribute != nil},
			{"VideoRecordPlan", innerXML(msg.VideoRecordPlan), msg.VideoRecordPlan != nil},
			{"VideoAlarmRecord", innerXML(msg.VideoAlarmRecord), msg.VideoAlarmRecord != nil},
			{"PictureMask", innerXML(msg.PictureMask), msg.PictureMask != nil},
			{"FrameMirror", innerXML(msg.FrameMirror), msg.FrameMirror != nil},
			{"AlarmReport", innerXML(msg.AlarmReport), msg.AlarmReport != nil},
			{"OSDConfig", innerXML(msg.OSDConfig), msg.OSDConfig != nil},
		}
		for _, section := range sections {
			if !section.present {
				continue
			}
			if err := g.requireGBVersionAtLeast(ctx.DeviceID, gbVersion2022, "配置查询应答("+section.name+")"); err != nil {
				ctx.String(400, err.Error())
				return
			}
			if err := validateDeviceConfigXMLFragment(section.name, section.value); err != nil {
				ctx.String(400, err.Error())
				return
			}
			if section.name == "VideoParamAttribute" {
				if err := validateVideoParamAttribute30(msg.VideoParamAttribute, true); err != nil {
					ctx.String(400, err.Error())
					return
				}
				continue
			}
			if err := validateDeviceConfig30Fragment(section.name, section.value); err != nil {
				ctx.String(400, err.Error())
				return
			}
		}
		for _, snapshot := range []*SnapShot{msg.SnapShotConfig, msg.SnapShot} {
			if snapshot == nil {
				continue
			}
			if err := g.requireGBVersionAtLeast(ctx.DeviceID, gbVersion2022, "配置查询应答(SnapShotConfig)"); err != nil {
				ctx.String(400, err.Error())
				return
			}
			if err := validateSnapshotConfig(snapshot); err != nil {
				ctx.String(400, err.Error())
				return
			}
		}
	}
	if resultOK && msg.BasicParam != nil {
		if err := validateBasicParamResponse(msg.BasicParam, version); err != nil {
			ctx.String(400, err.Error())
			return
		}
	}
	if msg.BasicParam != nil && msg.DeviceID == strings.TrimSpace(ctx.DeviceID) && resultOK && msg.BasicParam.hasBothHeartbeatFields() {
		ipc, ok := g.svr.memoryStorer.Load(ctx.DeviceID)
		if !ok {
			ctx.Log.Debug("sipMessageConfigDownload", "deviceID", ctx.DeviceID, "err", "device offline")
			return
		}

		// 计算设备离线超时时间
		interval := uint16(msg.BasicParam.HeartBeatInterval) // nolint:gosec -- 上方已限制为 uint16 范围
		timeout := uint16(msg.BasicParam.HeartBeatCount)     // nolint:gosec -- 上方已限制为 uint16 范围
		ipc.UpdateRuntime(func(device *Device) {
			device.keepaliveInterval = interval
			device.keepaliveTimeout = timeout
		})
		ctx.Log.Debug("sipMessageConfigDownload update", "deviceID", ctx.DeviceID, "keepaliveInterval", interval, "keepaliveTimeout", timeout)
	}

	// 命中通用查询等待队列（A.2.4 ConfigDownload 查询等待）。
	decoded := g.decodeAndStoreQueryResult(ctx.DeviceID, msg.CmdType, ctx.Request.Body())
	g.resolvePendingDeviceQueryResult(ctx.DeviceID, msg.CmdType, msg.SN, msg.Result, ctx.Request.Body(), msg.DeviceID, decoded)
	ctx.String(200, "OK")
	g.persistDecodedQuery(ctx.DeviceID, msg.CmdType, decoded)
	g.publishEventNotify(msg.CmdType, ctx.DeviceID, ctx.Request.Body())
}
