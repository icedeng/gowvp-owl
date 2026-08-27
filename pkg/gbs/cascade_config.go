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
		var route *cascadeTaskRoute
		var created bool
		if request.SnapShotConfig != nil {
			fingerprint, fingerprintErr := cascadeDeviceConfigFingerprint(&request)
			if fingerprintErr != nil {
				err = fingerprintErr
			} else {
				route, created, err = g.registerCascadeTaskRoute(ctx, cascadeTaskSnapshot, worker, channel, request.DeviceID, request.SnapShotConfig.SessionID, fingerprint, SnapshotState{
					ExpectedCount: request.SnapShotConfig.SnapNum,
				})
			}
			if err == nil {
				request.SnapShotConfig.SessionID = route.downstreamSessionID
			}
		}
		if err == nil {
			switch {
			case route == nil:
				result, err = configure(ctx, channel, &request)
			case created:
				result, err = configure(ctx, channel, &request)
			default:
				result, err = route.waitStart(ctx)
			}
		}
		if route != nil {
			if created {
				result, err = route.finishStart(result, err)
				startErr := err
				routeErr := g.persistCascadeTaskRoute(ctx, route)
				stateErr := g.finishCascadeTaskState(ctx, route, result, startErr)
				if routeErr != nil && err == nil {
					err = routeErr
				} else if stateErr != nil && err == nil {
					err = stateErr
				}
			} else if route.isCompleted() {
				result, err = "OK", nil
			}
		}
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

func cascadeDeviceConfigFingerprint(request *DeviceConfigRequest) (string, error) {
	if request == nil || request.SnapShotConfig == nil {
		return "", fmt.Errorf("cascade snapshot request is unavailable")
	}
	clone := *request
	clone.SN = 0
	body, err := sip.XMLEncode(clone)
	if err != nil {
		return "", fmt.Errorf("encode cascade snapshot fingerprint: %w", err)
	}
	return cascadeTaskFingerprint(body), nil
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
	sections := []struct {
		name    string
		present bool
	}{
		{"BasicParam", request.BasicParam != nil},
		{"VideoParamConfig", request.VideoParamConfig != nil},
		{"AudioParamConfig", request.AudioParamConfig != nil},
		{"SVACEncodeConfig", request.SVACEncodeConfig != nil},
		{"SVACDecodeConfig", request.SVACDecodeConfig != nil},
		{"VideoParamAttribute", request.VideoParamAttribute != nil},
		{"VideoRecordPlan", request.VideoRecordPlan != nil},
		{"VideoAlarmRecord", request.VideoAlarmRecord != nil},
		{"PictureMask", request.PictureMask != nil},
		{"FrameMirror", request.FrameMirror != nil},
		{"AlarmReport", request.AlarmReport != nil},
		{"OSDConfig", request.OSDConfig != nil},
		{"SnapShotConfig", request.SnapShotConfig != nil},
	}
	for _, section := range sections {
		if section.present && !deviceConfigSectionSupported(version, section.name) {
			return fmt.Errorf("%s is not supported by %s", section.name, version.StandardName())
		}
	}
	if request.BasicParam != nil {
		if version == GBVersion11 && (strings.TrimSpace(request.BasicParam.Name) == "" || request.BasicParam.Expiration <= 0 ||
			request.BasicParam.HeartBeatInterval <= 0 || request.BasicParam.HeartBeatCount <= 0) {
			return fmt.Errorf("BasicParam requires name, expiration and heartbeat values for %s", version.StandardName())
		}
		if version != GBVersion11 && (request.BasicParam.Expiration < 0 || request.BasicParam.HeartBeatInterval < 0 || request.BasicParam.HeartBeatCount < 0) {
			return fmt.Errorf("BasicParam values must not be negative")
		}
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
	if request.BasicParam != nil && (request.BasicParam.Expiration < 0 || request.BasicParam.HeartBeatInterval < 0 || request.BasicParam.HeartBeatCount < 0) {
		return fmt.Errorf("BasicParam values must not be negative")
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
			if fragment.name == "VideoParamAttribute" {
				if err := validateVideoParamAttribute30(request.VideoParamAttribute, true); err != nil {
					return err
				}
				continue
			}
			if err := validateDeviceConfig30Fragment(fragment.name, fragment.value); err != nil {
				return err
			}
		}
	}
	if request.SnapShotConfig != nil {
		if err := validateSnapshotConfig(request.SnapShotConfig); err != nil {
			return err
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
