package gbs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func (g *GB28181API) forwardCascadeDeviceConfig(worker *cascadeWorker, body []byte, parents ...context.Context) {
	parent := context.Background()
	if len(parents) > 0 && parents[0] != nil {
		parent = parents[0]
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	var request DeviceConfigRequest
	if err := sip.XMLDecode(body, &request); err != nil {
		return
	}
	result := "ERROR"
	request.CmdType = strings.TrimSpace(request.CmdType)
	request.DeviceID = strings.TrimSpace(request.DeviceID)
	err := validateCascadeDeviceConfigRequest(&request, worker.protocolVersion())
	var channel *ipc.Channel
	if err == nil {
		channel, err = g.loadCascadeExposedChannel(ctx, worker.platform, request.DeviceID)
	}
	if err == nil {
		configure := g.sendCascadeDeviceConfigDownstream
		if g.cascadeDeviceConfig != nil {
			configure = g.cascadeDeviceConfig
		}
		result, err = configure(ctx, channel, &request)
	}
	if err != nil {
		slog.Warn("forward cascade DeviceConfig failed", "upstream", worker.platform.name, "device_id", request.DeviceID, "sn", request.SN, "err", err)
		result = "ERROR"
	}
	if !strings.EqualFold(strings.TrimSpace(result), "OK") {
		result = "ERROR"
	} else {
		result = "OK"
	}
	if err := sendCascadeXML(ctx, worker, DeviceConfigResponse{
		CmdType: "DeviceConfig", SN: int(request.SN), DeviceID: strings.TrimSpace(request.DeviceID), Result: result,
	}); err != nil {
		slog.Warn("send cascade DeviceConfig response failed", "upstream", worker.platform.name, "device_id", request.DeviceID, "sn", request.SN, "err", err)
	}
}

func (g *GB28181API) sendCascadeDeviceConfigDownstream(ctx context.Context, channel *ipc.Channel, request *DeviceConfigRequest) (string, error) {
	if g == nil || channel == nil || request == nil {
		return "ERROR", fmt.Errorf("cascade DeviceConfig target is unavailable")
	}
	state, err := g.SetDeviceConfig(ctx, cascadeDeviceConfigInput(channel, request))
	if state == nil {
		return "ERROR", err
	}
	return strings.TrimSpace(state.Result), err
}

func cascadeDeviceConfigInput(channel *ipc.Channel, request *DeviceConfigRequest) *DeviceConfigInput {
	if channel == nil || request == nil {
		return nil
	}
	return &DeviceConfigInput{
		DeviceID:            channel.DeviceID,
		TargetID:            channel.ChannelID,
		Timeout:             8 * time.Second,
		BasicParam:          request.BasicParam,
		VideoParamConfig:    request.VideoParamConfig,
		AudioParamConfig:    request.AudioParamConfig,
		SVACEncodeConfig:    request.SVACEncodeConfig,
		SVACDecodeConfig:    request.SVACDecodeConfig,
		VideoParamAttribute: request.VideoParamAttribute,
		VideoRecordPlan:     request.VideoRecordPlan,
		VideoAlarmRecord:    request.VideoAlarmRecord,
		PictureMask:         request.PictureMask,
		FrameMirror:         request.FrameMirror,
		AlarmReport:         request.AlarmReport,
		OSDConfig:           request.OSDConfig,
		SnapShotConfig:      request.SnapShotConfig,
	}
}

func validateCascadeDeviceConfigRequest(request *DeviceConfigRequest, version GBProtocolVersion) error {
	if err := validateCascadeDeviceConfigPayload(request); err != nil {
		return err
	}
	if !version.Capabilities().ConfigWrite {
		return fmt.Errorf("DeviceConfig is not supported by negotiated protocol")
	}
	extended := request.VideoParamAttribute != nil || request.VideoRecordPlan != nil || request.VideoAlarmRecord != nil ||
		request.PictureMask != nil || request.FrameMirror != nil || request.AlarmReport != nil || request.OSDConfig != nil || request.SnapShotConfig != nil
	if extended && !version.AtLeast(GBVersion30) {
		return fmt.Errorf("extended DeviceConfig requires protocol 3.0")
	}
	return nil
}

func validateCascadeDeviceConfigPayload(request *DeviceConfigRequest) error {
	if request == nil || request.XMLName.Local != "Control" || !strings.EqualFold(strings.TrimSpace(request.CmdType), "DeviceConfig") ||
		request.SN <= 0 || !isGBDeviceIdentifier(strings.TrimSpace(request.DeviceID)) {
		return fmt.Errorf("invalid cascade DeviceConfig")
	}
	legacy := request.BasicParam != nil || request.VideoParamConfig != nil || request.AudioParamConfig != nil ||
		request.SVACEncodeConfig != nil || request.SVACDecodeConfig != nil
	extended := request.VideoParamAttribute != nil || request.VideoRecordPlan != nil || request.VideoAlarmRecord != nil ||
		request.PictureMask != nil || request.FrameMirror != nil || request.AlarmReport != nil || request.OSDConfig != nil || request.SnapShotConfig != nil
	if !legacy && !extended {
		return fmt.Errorf("DeviceConfig requires at least one configuration section")
	}
	if request.BasicParam != nil && (request.BasicParam.Expiration <= 0 || request.BasicParam.HeartBeatInterval <= 0 || request.BasicParam.HeartBeatCount <= 0) {
		return fmt.Errorf("BasicParam expiration, heartbeat interval and count must be positive")
	}
	if request.VideoParamConfig != nil {
		if err := validateVideoParamConfig(request.VideoParamConfig); err != nil {
			return err
		}
	}
	if request.AudioParamConfig != nil {
		if err := validateAudioParamConfig(request.AudioParamConfig); err != nil {
			return err
		}
	}
	fragments := []struct {
		name    string
		value   string
		present bool
	}{
		{"SVACEncodeConfig", cascadeSVACEncodeXML(request.SVACEncodeConfig), request.SVACEncodeConfig != nil},
		{"SVACDecodeConfig", cascadeSVACDecodeXML(request.SVACDecodeConfig), request.SVACDecodeConfig != nil},
		{"VideoParamAttribute", innerXML(request.VideoParamAttribute), request.VideoParamAttribute != nil},
		{"VideoRecordPlan", innerXML(request.VideoRecordPlan), request.VideoRecordPlan != nil},
		{"VideoAlarmRecord", innerXML(request.VideoAlarmRecord), request.VideoAlarmRecord != nil},
		{"PictureMask", innerXML(request.PictureMask), request.PictureMask != nil},
		{"FrameMirror", innerXML(request.FrameMirror), request.FrameMirror != nil},
		{"AlarmReport", innerXML(request.AlarmReport), request.AlarmReport != nil},
		{"OSDConfig", innerXML(request.OSDConfig), request.OSDConfig != nil},
	}
	for _, fragment := range fragments {
		if fragment.present {
			if err := validateDeviceConfigXMLFragment(fragment.name, fragment.value); err != nil {
				return err
			}
		}
	}
	if request.SnapShotConfig != nil {
		config := request.SnapShotConfig
		if config.SnapNum < 1 || config.SnapNum > 10 || config.Interval < 1 || strings.TrimSpace(config.UploadURL) == "" {
			return fmt.Errorf("SnapShotConfig requires snap_num 1~10, interval >= 1 and upload_url")
		}
		if err := validateGBSessionID(strings.TrimSpace(config.SessionID)); err != nil {
			return fmt.Errorf("SnapShotConfig: %w", err)
		}
	}
	return nil
}

func cascadeSVACEncodeXML(config *SVACEncodeConfig) string {
	if config == nil {
		return ""
	}
	return strings.TrimSpace(config.InnerXML)
}

func cascadeSVACDecodeXML(config *SVACDecodeConfig) string {
	if config == nil {
		return ""
	}
	return strings.TrimSpace(config.InnerXML)
}
