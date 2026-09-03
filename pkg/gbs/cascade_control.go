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

const cascadeDeviceControlTimeout = 6 * time.Second

func (g *GB28181API) forwardCascadeDeviceControl(worker *cascadeWorker, body []byte, parents ...context.Context) {
	parent := context.Background()
	if len(parents) > 0 && parents[0] != nil {
		parent = parents[0]
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	var request deviceControlA23Request
	if err := decodeDeviceControlRequest(body, &request); err != nil {
		return
	}
	result := "ERROR"
	channel, err := g.loadCascadeExposedChannel(ctx, worker.platform, request.DeviceID)
	if err == nil {
		downstreamVersion := GBVersion10
		if g.svr != nil && g.svr.memoryStorer != nil {
			downstreamVersion = g.getDeviceGBProtocolVersion(channel.DeviceID)
		}
		err = validateCascadeDeviceControl(&request, worker.protocolVersion(), downstreamVersion)
		if err == nil {
			err = translateCascadeIFrameCommand(&request, worker.protocolVersion(), downstreamVersion)
		}
		if err == nil {
			err = translateCascadeStreamNumber(&request, downstreamVersion)
		}
		if err == nil {
			err = g.validateCascadeDeviceControlOverrides(channel.DeviceID, &request)
		}
		if err == nil {
			var panoramaID string
			panoramaID, err = g.resolveCascadeTargetTrackDeviceID2(ctx, worker.platform, channel, &request)
			if err == nil && strings.TrimSpace(request.DeviceID2) != "" {
				request.DeviceID2 = panoramaID
			}
		}
		if err == nil {
			control := g.sendCascadeDeviceControlDownstream
			if g.cascadeDeviceControl != nil {
				control = g.cascadeDeviceControl
			}
			var route *cascadeTaskRoute
			var created bool
			if request.DeviceUpgrade != nil {
				fingerprint := cascadeUpgradeFingerprint(request.DeviceUpgrade)
				route, created, err = g.registerCascadeTaskRoute(ctx, cascadeTaskUpgrade, worker, channel, request.DeviceID, request.DeviceUpgrade.SessionID, fingerprint, UpgradeState{
					Firmware: request.DeviceUpgrade.Firmware,
				})
				if err == nil {
					request.DeviceUpgrade.SessionID = route.downstreamSessionID
				}
			}
			if err == nil {
				switch {
				case route == nil:
					result, err = control(ctx, channel, &request)
				case created:
					result, err = control(ctx, channel, &request)
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
					result, err = ptzResultOK, nil
				}
			}
		}
	}
	if err != nil {
		slog.Warn("forward cascade DeviceControl failed", "upstream", worker.platform.name, "device_id", request.DeviceID, "sn", request.SN, "err", err)
		result = "ERROR"
	}
	if !strings.EqualFold(strings.TrimSpace(result), ptzResultOK) {
		result = "ERROR"
	} else {
		result = ptzResultOK
	}
	if !deviceControlRequiresBusinessResponse(&request) {
		return
	}
	if err := sendCascadeXML(ctx, worker, deviceControlResponse{
		CmdType: ptzCmdTypeDeviceControl, SN: request.SN, DeviceID: request.DeviceID, Result: result,
	}); err != nil {
		slog.Warn("send cascade DeviceControl response failed", "upstream", worker.platform.name, "device_id", request.DeviceID, "sn", request.SN, "err", err)
	}
}

func cascadeUpgradeFingerprint(config *deviceUpgradeConfig) string {
	if config == nil {
		return ""
	}
	return cascadeTaskFingerprint([]byte(fmt.Sprintf("upgrade:%q:%q:%q:%q", config.Firmware, config.FileURL,
		config.Manufacturer, strings.TrimSpace(config.SessionID))))
}

func (g *GB28181API) validateCascadeDeviceControlOverrides(deviceID string, request *deviceControlA23Request) error {
	if err := g.validateCascadeRuntimeDeviceTarget(deviceID); err != nil {
		return err
	}
	checks := []struct {
		name string
		on   bool
	}{
		{name: "iframe_control", on: strings.TrimSpace(request.IFameCmd) != "" || strings.TrimSpace(request.IFrameCmd) != ""},
		{name: "drag_zoom_control", on: request.DragZoomIn != nil || request.DragZoomOut != nil},
		{name: "home_position", on: request.HomePosition != nil},
		{name: "ptz_position", on: request.PTZPreciseCtrl != nil},
		{name: "sdcard", on: request.FormatSDCard != nil},
		{name: "target_track", on: strings.TrimSpace(request.TargetTrack) != ""},
		{name: "upgrade", on: request.DeviceUpgrade != nil},
	}
	for _, check := range checks {
		if check.on && g.isDeviceCapabilityDisabled(deviceID, check.name) {
			return fmt.Errorf("cascade DeviceControl capability %s is disabled for device", check.name)
		}
	}
	return nil
}

func (g *GB28181API) sendCascadeDeviceControlDownstream(ctx context.Context, channel *ipc.Channel, request *deviceControlA23Request) (string, error) {
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil || channel == nil || request == nil {
		return "ERROR", fmt.Errorf("cascade DeviceControl target is unavailable")
	}
	target, ok := g.svr.memoryStorer.GetChannel(channel.DeviceID, channel.ChannelID)
	if !ok || target == nil || target.device == nil || !target.device.IsOnlineNow() {
		return "ERROR", ErrDeviceOffline
	}
	downstream := *request
	downstream.DeviceID = channel.ChannelID
	downstreamVersion := g.getDeviceGBProtocolVersion(channel.DeviceID)
	if command, present, commandErr := deviceControlIFrameCommand(&downstream); commandErr != nil {
		return "ERROR", commandErr
	} else if present {
		if commandErr = setDeviceControlIFrameCommand(&downstream, downstreamVersion, command); commandErr != nil {
			return "ERROR", commandErr
		}
	}
	if err := translateCascadeStreamNumber(&downstream, downstreamVersion); err != nil {
		return "ERROR", err
	}
	downstreamSN := g.nextControlSN()
	downstream.SN = downstreamSN
	body, err := encodeDeviceControlRequest(&downstream, downstreamVersion)
	if err != nil {
		return "ERROR", err
	}
	requiresBusinessResponse := deviceControlRequiresBusinessResponse(&downstream)
	operation, releaseOperation := g.trackPendingDeviceRequest(ctx, channel.DeviceID, channel.ChannelID)
	defer releaseOperation()
	var pending *pendingDeviceControl
	if requiresBusinessResponse {
		waitKey := fmt.Sprintf("%s:%d", channel.DeviceID, downstreamSN)
		pending = &pendingDeviceControl{
			wait:      make(chan *deviceControlResponse, 1),
			targetID:  channel.ChannelID,
			operation: operation,
		}
		if _, loaded := g.pendingDeviceControl.LoadOrStore(waitKey, pending); loaded {
			return "ERROR", fmt.Errorf("cascade DeviceControl sequence collision")
		}
		defer g.pendingDeviceControl.CompareAndDelete(waitKey, pending)
		defer pending.operation.Cancel(nil)
	}
	requestCtx := operation.Context(ctx)
	tx, err := g.svr.wrapRequestContext(requestCtx, target, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		return "ERROR", operation.ErrorOr(err)
	}
	if _, err = sipResponseContext(requestCtx, tx); err != nil {
		return "ERROR", operation.ErrorOr(err)
	}
	if !requiresBusinessResponse {
		if !operation.Deliver(func() {}) {
			return "ERROR", operation.Cause()
		}
		return ptzResultOK, nil
	}
	timer := time.NewTimer(cascadeDeviceControlTimeout)
	defer timer.Stop()
	select {
	case <-operation.Done():
		return "ERROR", operation.Cause()
	case <-g.serviceDone():
		return "ERROR", ErrServiceStopped
	case response := <-pending.wait:
		result := strings.ToUpper(strings.TrimSpace(response.Result))
		return result, nil
	case <-timer.C:
		cfg := g.configSnapshot()
		if cfg != nil && cfg.PTZWeakConfirm {
			return ptzResultOK, nil
		}
		return "ERROR", fmt.Errorf("%s", ptzTimeoutErrorMessage)
	}
}

// resolveCascadeTargetTrackDeviceID2 将上级看到的全景通道编码映射为同一设备的真实下级通道编码。
func (g *GB28181API) resolveCascadeTargetTrackDeviceID2(ctx context.Context, platform cascadePlatform, target *ipc.Channel, request *deviceControlA23Request) (string, error) {
	if request == nil || strings.TrimSpace(request.TargetTrack) == "" {
		return "", nil
	}
	exposedID := strings.TrimSpace(request.DeviceID2)
	if exposedID == "" {
		return "", nil
	}
	panorama, err := g.loadCascadeExposedChannel(ctx, platform, exposedID)
	if err != nil {
		return "", fmt.Errorf("cascade TargetTrack DeviceID2 must reference a shared panorama channel: %w", err)
	}
	if target == nil || panorama.DeviceID != target.DeviceID {
		return "", fmt.Errorf("cascade TargetTrack DeviceID2 must belong to the target device")
	}
	if panorama.ChannelID == target.ChannelID {
		return "", fmt.Errorf("cascade TargetTrack DeviceID2 must differ from the ball camera channel")
	}
	return panorama.ChannelID, nil
}

func validateCascadeDeviceControl(request *deviceControlA23Request, upstream, downstream GBProtocolVersion) error {
	if request == nil || request.XMLName.Local != "Control" || request.CmdType != ptzCmdTypeDeviceControl || request.SN <= 0 || strings.TrimSpace(request.DeviceID) == "" {
		return fmt.Errorf("invalid cascade DeviceControl")
	}
	if request.StreamNumber != nil && strings.TrimSpace(request.RecordCmd) == "" {
		return fmt.Errorf("StreamNumber requires RecordCmd")
	}
	if err := validateDeviceControlExtraInfo(deviceControlTextInfo(request)); err != nil {
		return err
	}
	if err := validateCascadeDeviceControlInfo(request, upstream, downstream); err != nil {
		return err
	}
	if (strings.TrimSpace(request.DeviceID2) != "" || request.TargetArea != nil) && strings.TrimSpace(request.TargetTrack) == "" {
		return fmt.Errorf("DeviceID2 and TargetArea require TargetTrack")
	}
	actions := 0
	if request.PTZCmdParams != nil {
		if !upstream.AtLeast(GBVersion30) || !downstream.AtLeast(GBVersion30) {
			return fmt.Errorf("PTZCmdParams requires protocol 3.0")
		}
		if strings.TrimSpace(request.PTZCmd) == "" {
			return fmt.Errorf("PTZCmdParams requires PTZCmd")
		}
		if len(request.PTZCmdParams.CruiseTrackName) > 32 {
			return fmt.Errorf("PTZCmdParams CruiseTrackName must not exceed 32 bytes")
		}
	}
	if strings.TrimSpace(request.PTZCmd) != "" {
		actions++
		command, err := parsePTZCommand(request.PTZCmd)
		if err != nil {
			return err
		}
		if err := validatePTZCmdParams(command, request.PTZCmdParams); err != nil {
			return err
		}
	}
	if strings.TrimSpace(request.RecordCmd) != "" {
		actions++
		command := strings.ToLower(strings.TrimSpace(request.RecordCmd))
		if command != "record" && command != "stoprecord" {
			return fmt.Errorf("unsupported cascade RecordCmd")
		}
		if request.StreamNumber != nil {
			if upstream != GBVersion30 {
				return fmt.Errorf("StreamNumber requires upstream protocol 3.0")
			}
			if *request.StreamNumber < 0 {
				return fmt.Errorf("StreamNumber must be >= 0")
			}
			if *request.StreamNumber != 0 && downstream != GBVersion30 {
				return fmt.Errorf("non-zero StreamNumber requires downstream protocol 3.0")
			}
		}
	}
	if _, present, err := validateDeviceControlIFrameCommand(request, upstream); err != nil {
		return err
	} else if present {
		actions++
		if !downstream.Capabilities().IFrameControl {
			return fmt.Errorf("force-I-frame control is not supported by downstream protocol %s", downstream)
		}
	}
	if request.DragZoomIn != nil || request.DragZoomOut != nil {
		actions++
		if request.DragZoomIn != nil && request.DragZoomOut != nil {
			return fmt.Errorf("DeviceControl must contain one DragZoom action")
		}
		if !upstream.Capabilities().DragZoomControl || !downstream.Capabilities().DragZoomControl {
			return fmt.Errorf("DragZoom is not supported by negotiated protocol")
		}
		zoom := request.DragZoomIn
		if zoom == nil {
			zoom = request.DragZoomOut
		}
		if err := validateCascadeDragZoom(zoom); err != nil {
			return fmt.Errorf("invalid cascade DragZoom: %w", err)
		}
	}
	if request.HomePosition != nil {
		actions++
		if !upstream.Capabilities().HomePosition || !downstream.Capabilities().HomePosition {
			return fmt.Errorf("HomePosition is not supported by negotiated protocol")
		}
		if request.HomePosition.Enabled == nil || (*request.HomePosition.Enabled != 0 && *request.HomePosition.Enabled != 1) {
			return fmt.Errorf("HomePosition Enabled must be 0 or 1")
		}
		// A.2.3.1.10 将 ResetTime 声明为 integer，未定义非负范围。
		if request.HomePosition.PresetIndex != nil && (*request.HomePosition.PresetIndex < 0 || *request.HomePosition.PresetIndex > 255) {
			return fmt.Errorf("HomePosition PresetIndex must be in [0,255]")
		}
		if *request.HomePosition.Enabled == 0 && (request.HomePosition.ResetTime != nil || request.HomePosition.PresetIndex != nil) {
			return fmt.Errorf("HomePosition ResetTime and PresetIndex require Enabled=1")
		}
	}
	if request.PTZPreciseCtrl != nil {
		actions++
		if !upstream.AtLeast(GBVersion30) || !downstream.AtLeast(GBVersion30) {
			return fmt.Errorf("PTZPreciseCtrl requires protocol 3.0")
		}
		precise := request.PTZPreciseCtrl
		if precise.Pan == nil && precise.Tilt == nil && precise.Zoom == nil {
			return fmt.Errorf("PTZPreciseCtrl requires at least one of Pan, Tilt or Zoom")
		}
		if precise.Pan != nil && !validFiniteRange(*precise.Pan, 0, 360) {
			return fmt.Errorf("PTZPreciseCtrl Pan must be in [0,360]")
		}
		if precise.Tilt != nil && !validFiniteRange(*precise.Tilt, -30, 90) {
			return fmt.Errorf("PTZPreciseCtrl Tilt must be in [-30,90]")
		}
		if precise.Zoom != nil && (!validFinite(*precise.Zoom) || *precise.Zoom < 1) {
			return fmt.Errorf("PTZPreciseCtrl Zoom must be finite and >= 1.0")
		}
	}
	if strings.TrimSpace(request.TargetTrack) != "" {
		actions++
		if !upstream.Capabilities().TargetTrack || !downstream.Capabilities().TargetTrack {
			return fmt.Errorf("TargetTrack requires protocol 3.0")
		}
		mode := strings.ToLower(strings.TrimSpace(request.TargetTrack))
		if mode != "auto" && mode != "manual" && mode != "stop" {
			return fmt.Errorf("unsupported TargetTrack mode")
		}
		if request.DeviceID2 != "" && !isGBDeviceIdentifier(strings.TrimSpace(request.DeviceID2)) {
			return fmt.Errorf("cascade TargetTrack DeviceID2 must be 20 digits")
		}
		if mode == "manual" && request.TargetArea == nil {
			return fmt.Errorf("manual TargetTrack requires TargetArea")
		}
		if mode != "manual" && request.TargetArea != nil {
			return fmt.Errorf("TargetArea is only valid for manual TargetTrack")
		}
		if request.TargetArea != nil {
			if err := validateCascadeDragZoom(request.TargetArea); err != nil {
				return fmt.Errorf("invalid TargetTrack TargetArea: %w", err)
			}
		}
	}
	if request.DeviceUpgrade != nil {
		actions++
		if !upstream.Capabilities().Upgrade || !downstream.Capabilities().Upgrade {
			return fmt.Errorf("DeviceUpgrade requires protocol 3.0")
		}
		if err := validateDeviceUpgradeConfig(request.DeviceUpgrade); err != nil {
			return err
		}
	}
	if strings.TrimSpace(request.TeleBoot) != "" || strings.TrimSpace(request.GuardCmd) != "" || strings.TrimSpace(request.AlarmCmd) != "" || request.FormatSDCard != nil {
		return fmt.Errorf("device-scoped cascade control is not allowed for a shared channel")
	}
	if actions != 1 {
		return fmt.Errorf("cascade DeviceControl must contain exactly one channel action")
	}
	return nil
}

func validateCascadeDeviceControlInfo(request *deviceControlA23Request, upstream, downstream GBProtocolVersion) error {
	if request == nil || request.Info == nil {
		return nil
	}
	info := request.Info
	hasPriority := info.ControlPriority != nil
	hasAlarmInfo := strings.TrimSpace(info.AlarmMethod) != "" || strings.TrimSpace(info.AlarmType) != ""
	if hasPriority {
		if strings.TrimSpace(request.PTZCmd) == "" {
			return fmt.Errorf("ControlPriority requires PTZCmd")
		}
		if strings.TrimSpace(request.AlarmCmd) != "" || hasAlarmInfo {
			return fmt.Errorf("ControlPriority must not be combined with AlarmCmd Info")
		}
		if !supportsPTZControlPriority(upstream) || !supportsPTZControlPriority(downstream) {
			return fmt.Errorf("ControlPriority requires GB/T 28181-2011/2014 on both cascade sides")
		}
		return nil
	}
	if strings.TrimSpace(request.AlarmCmd) == "" {
		return fmt.Errorf("DeviceControl Info requires AlarmCmd or PTZ ControlPriority")
	}
	if hasAlarmInfo && (!upstream.AtLeast(GBVersion20) || !downstream.AtLeast(GBVersion20)) {
		return fmt.Errorf("AlarmCmd Info requires protocol 2.0 or later")
	}
	return nil
}

func translateCascadeIFrameCommand(request *deviceControlA23Request, upstream, downstream GBProtocolVersion) error {
	command, present, err := validateDeviceControlIFrameCommand(request, upstream)
	if err != nil || !present {
		return err
	}
	return setDeviceControlIFrameCommand(request, downstream, command)
}

func translateCascadeStreamNumber(request *deviceControlA23Request, downstream GBProtocolVersion) error {
	if request == nil || request.StreamNumber == nil {
		return nil
	}
	if *request.StreamNumber < 0 {
		return fmt.Errorf("StreamNumber must be >= 0")
	}
	if *request.StreamNumber == 0 {
		request.StreamNumber = nil
		return nil
	}
	if downstream != GBVersion30 {
		return fmt.Errorf("non-zero StreamNumber requires downstream protocol 3.0")
	}
	return nil
}

func validateDeviceUpgradeConfig(value *deviceUpgradeConfig) error {
	if value == nil || strings.TrimSpace(value.Firmware) == "" || strings.TrimSpace(value.FileURL) == "" || strings.TrimSpace(value.Manufacturer) == "" {
		return fmt.Errorf("DeviceUpgrade requires Firmware, FileURL and Manufacturer")
	}
	if err := validateGBSessionID(strings.TrimSpace(value.SessionID)); err != nil {
		return fmt.Errorf("DeviceUpgrade: %w", err)
	}
	return nil
}
