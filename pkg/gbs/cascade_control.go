package gbs

import (
	"context"
	"encoding/hex"
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
	if err := sip.XMLDecode(body, &request); err != nil {
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
			err = g.validateCascadeDeviceControlOverrides(channel.DeviceID, &request)
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
					Firmware: strings.TrimSpace(request.DeviceUpgrade.Firmware),
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
	return cascadeTaskFingerprint([]byte(fmt.Sprintf("upgrade:%q:%q:%q:%q", strings.TrimSpace(config.Firmware), strings.TrimSpace(config.FileURL),
		strings.TrimSpace(config.Manufacturer), strings.TrimSpace(config.SessionID))))
}

func (g *GB28181API) validateCascadeDeviceControlOverrides(deviceID string, request *deviceControlA23Request) error {
	checks := []struct {
		name string
		on   bool
	}{
		{name: "iframe_control", on: strings.TrimSpace(request.IFrameCmd) != ""},
		{name: "drag_zoom_control", on: request.DragZoomIn != nil || request.DragZoomOut != nil},
		{name: "home_position", on: request.HomePosition != nil},
		{name: "ptz_position", on: request.PTZPreciseCtrl != nil},
		{name: "target_track", on: strings.TrimSpace(request.TargetTrack) != ""},
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
	downstreamSN := g.nextControlSN()
	downstream := *request
	downstream.SN = downstreamSN
	upstreamTargetID := downstream.DeviceID
	downstream.DeviceID = channel.ChannelID
	if strings.TrimSpace(downstream.DeviceID2) == upstreamTargetID {
		downstream.DeviceID2 = channel.ChannelID
	}
	body, err := sip.XMLEncode(downstream)
	if err != nil {
		return "ERROR", err
	}
	waitKey := fmt.Sprintf("%s:%d", channel.DeviceID, downstreamSN)
	pending := &pendingDeviceControl{wait: make(chan *deviceControlResponse, 1), targetID: channel.ChannelID}
	if _, loaded := g.pendingDeviceControl.LoadOrStore(waitKey, pending); loaded {
		return "ERROR", fmt.Errorf("cascade DeviceControl sequence collision")
	}
	defer g.pendingDeviceControl.Delete(waitKey)
	tx, err := g.svr.wrapRequestContext(ctx, target, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		return "ERROR", err
	}
	if _, err = sipResponseContext(ctx, tx); err != nil {
		return "ERROR", err
	}
	timer := time.NewTimer(cascadeDeviceControlTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "ERROR", ctx.Err()
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

func validateCascadeDeviceControl(request *deviceControlA23Request, upstream, downstream GBProtocolVersion) error {
	if request == nil || request.XMLName.Local != "Control" || request.CmdType != ptzCmdTypeDeviceControl || request.SN <= 0 || strings.TrimSpace(request.DeviceID) == "" {
		return fmt.Errorf("invalid cascade DeviceControl")
	}
	actions := 0
	if strings.TrimSpace(request.PTZCmd) != "" {
		actions++
		if err := validateCascadePTZCmd(request.PTZCmd); err != nil {
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
			if !upstream.AtLeast(GBVersion20) || !downstream.AtLeast(GBVersion20) {
				return fmt.Errorf("StreamNumber requires protocol 2.0")
			}
			if *request.StreamNumber < 0 || *request.StreamNumber > 2 {
				return fmt.Errorf("StreamNumber must be in [0,2]")
			}
		}
	}
	if strings.TrimSpace(request.IFrameCmd) != "" {
		actions++
		if !upstream.Capabilities().IFrameControl || !downstream.Capabilities().IFrameControl {
			return fmt.Errorf("IFrameCmd is not supported by negotiated protocol")
		}
		if !strings.EqualFold(strings.TrimSpace(request.IFrameCmd), "Send") {
			return fmt.Errorf("unsupported cascade IFrameCmd")
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
		if request.HomePosition.ResetTime != nil && *request.HomePosition.ResetTime < 0 {
			return fmt.Errorf("HomePosition ResetTime must not be negative")
		}
		if request.HomePosition.PresetIndex != nil && (*request.HomePosition.PresetIndex < 0 || *request.HomePosition.PresetIndex > 255) {
			return fmt.Errorf("HomePosition PresetIndex must be in [0,255]")
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
		if precise.Zoom != nil && !validFinite(*precise.Zoom) {
			return fmt.Errorf("PTZPreciseCtrl Zoom must be finite")
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
		if request.DeviceID2 != "" && request.DeviceID2 != request.DeviceID {
			return fmt.Errorf("cascade TargetTrack DeviceID2 must reference the shared channel")
		}
		if mode == "manual" && request.TargetArea == nil {
			return fmt.Errorf("manual TargetTrack requires TargetArea")
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

func validateDeviceUpgradeConfig(value *deviceUpgradeConfig) error {
	if value == nil || strings.TrimSpace(value.Firmware) == "" || strings.TrimSpace(value.FileURL) == "" || strings.TrimSpace(value.Manufacturer) == "" {
		return fmt.Errorf("DeviceUpgrade requires Firmware, FileURL and Manufacturer")
	}
	if err := validateGBSessionID(strings.TrimSpace(value.SessionID)); err != nil {
		return fmt.Errorf("DeviceUpgrade: %w", err)
	}
	return nil
}

func validateCascadePTZCmd(value string) error {
	command, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil || len(command) != 8 || command[0] != 0xA5 {
		return fmt.Errorf("invalid cascade PTZCmd")
	}
	var checksum byte
	for _, item := range command[:7] {
		checksum += item
	}
	if checksum != command[7] {
		return fmt.Errorf("invalid cascade PTZCmd checksum")
	}
	return nil
}
