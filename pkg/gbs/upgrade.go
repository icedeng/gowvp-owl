package gbs

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	maxUpgradeStates = 1024
	upgradeStateTTL  = 7 * 24 * time.Hour
)

type UpgradeInput struct {
	DeviceID     string
	ChannelID    string
	Firmware     string
	FileURL      string
	Manufacturer string
	SessionID    string
	Timeout      time.Duration
}

type UpgradeOutput struct {
	SN        int    `json:"sn"`
	DeviceID  string `json:"device_id"`
	Channel   string `json:"channel"`
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
}

// UpgradeState 保存升级控制应答和最终结果通知两个阶段的状态。
// accepted 只表示目标设备已接受升级命令；completed/failed 才是最终状态。
type UpgradeState struct {
	SN           int       `json:"sn"`
	DeviceID     string    `json:"device_id"`
	ChannelID    string    `json:"channel_id,omitempty"`
	SessionID    string    `json:"session_id"`
	Status       string    `json:"status"`
	Result       string    `json:"result,omitempty"`
	Firmware     string    `json:"firmware,omitempty"`
	FailedReason string    `json:"failed_reason,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type deviceUpgradeResultNotify struct {
	XMLName      xml.Name `xml:"Notify"`
	CmdType      string   `xml:"CmdType"`
	SN           int      `xml:"SN"`
	DeviceID     string   `xml:"DeviceID"`
	SessionID    string   `xml:"SessionID"`
	Result       string   `xml:"UpgradeResult"`
	Firmware     string   `xml:"Firmware"`
	FailedReason string   `xml:"UpgradeFailedReason"`
}

// Upgrade 执行设备软件升级（GB/T 28181-2022 9.13，A.2.3.1.12）。
func (g *GB28181API) Upgrade(ctx context.Context, in *UpgradeInput) (*UpgradeOutput, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in == nil || in.DeviceID == "" || in.ChannelID == "" {
		return nil, errors.New("invalid upgrade input")
	}
	if err := g.requireGBVersionAtLeast(in.DeviceID, gbVersion2022, "设备软件升级(9.13)"); err != nil {
		return nil, err
	}
	if err := g.requireGBFeature(in.DeviceID, "upgrade", "设备软件升级(9.13)", func(c GBCapabilities) bool {
		return c.Upgrade
	}); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Firmware) == "" || strings.TrimSpace(in.FileURL) == "" || strings.TrimSpace(in.Manufacturer) == "" {
		return nil, errors.New("firmware/file_url/manufacturer are required")
	}
	sessionID, err := normalizeUpgradeSessionID(in.SessionID)
	if err != nil {
		return nil, err
	}
	if _, exists, err := g.loadUpgradeState(ctx, in.DeviceID, sessionID); err != nil {
		return nil, err
	} else if exists {
		return nil, errors.New("upgrade session_id already exists")
	}

	ipc, ok := g.svr.memoryStorer.Load(in.DeviceID)
	if !ok || !ipc.IsOnlineNow() {
		return nil, ErrDeviceOffline
	}
	ch, ok := g.svr.memoryStorer.GetChannel(in.DeviceID, in.ChannelID)
	if !ok {
		return nil, ErrChannelNotExist
	}
	if in.Timeout <= 0 {
		in.Timeout = 8 * time.Second
	}

	sn := g.nextControlSN()
	req := deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: sn, DeviceID: in.ChannelID,
		DeviceUpgrade: &deviceUpgradeConfig{
			Firmware: strings.TrimSpace(in.Firmware), FileURL: strings.TrimSpace(in.FileURL),
			Manufacturer: strings.TrimSpace(in.Manufacturer), SessionID: sessionID,
		},
	}

	body, err := sip.XMLEncode(req)
	if err != nil {
		return nil, err
	}
	pendingState := UpgradeState{
		SN: sn, DeviceID: in.DeviceID, ChannelID: in.ChannelID, SessionID: sessionID,
		Status: "pending", Firmware: strings.TrimSpace(in.Firmware), UpdatedAt: time.Now(),
	}
	if err := g.storeUpgradeStateContext(ctx, pendingState); err != nil {
		g.deleteUpgradeState(context.Background(), in.DeviceID, sessionID)
		return nil, err
	}

	waitKey := fmt.Sprintf("%s:%d", in.DeviceID, sn)
	pending := &pendingDeviceControl{wait: make(chan *deviceControlResponse, 1), targetID: in.ChannelID}
	g.pendingDeviceControl.Store(waitKey, pending)
	defer g.pendingDeviceControl.Delete(waitKey)

	tx, err := g.svr.wrapRequestContext(ctx, ch, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		g.deleteUpgradeState(context.Background(), in.DeviceID, sessionID)
		return nil, err
	}
	if _, err = sipResponseContext(ctx, tx); err != nil {
		pendingState.Status = "response_timeout"
		pendingState.UpdatedAt = time.Now()
		_ = g.storeUpgradeStateContext(context.Background(), pendingState)
		return nil, err
	}

	timer := time.NewTimer(in.Timeout)
	defer timer.Stop()
	select {
	case resp := <-pending.wait:
		result := strings.ToUpper(strings.TrimSpace(resp.Result))
		pendingState.Result = result
		pendingState.UpdatedAt = time.Now()
		if result != "OK" {
			pendingState.Status = "rejected"
			if err := g.storeUpgradeStateContext(context.Background(), pendingState); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("device upgrade failed: %s", resp.Result)
		}
		pendingState.Status = "accepted"
		if err := g.storeUpgradeStateContext(context.Background(), pendingState); err != nil {
			return nil, err
		}
		return &UpgradeOutput{
			SN: sn, DeviceID: in.DeviceID, Channel: in.ChannelID, SessionID: sessionID, Result: result,
		}, nil
	case <-timer.C:
		pendingState.Status = "response_timeout"
		pendingState.UpdatedAt = time.Now()
		_ = g.storeUpgradeStateContext(context.Background(), pendingState)
		return nil, errors.New("wait device upgrade response timeout")
	case <-ctx.Done():
		pendingState.Status = "cancelled"
		pendingState.UpdatedAt = time.Now()
		_ = g.storeUpgradeStateContext(context.Background(), pendingState)
		return nil, ctx.Err()
	case <-g.serviceDone():
		pendingState.Status = "cancelled"
		pendingState.UpdatedAt = time.Now()
		_ = g.storeUpgradeStateContext(context.Background(), pendingState)
		return nil, ErrServiceStopped
	}
}

func normalizeUpgradeSessionID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = sip.RandString(32)
	}
	return value, validateGBSessionID(value)
}

func validateGBSessionID(value string) error {
	if len(value) < 32 || len(value) > 128 {
		return errors.New("session_id must contain 32 to 128 characters")
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' {
			continue
		}
		return errors.New("session_id may contain only letters, digits, and hyphen")
	}
	return nil
}

func upgradeStateKey(deviceID, sessionID string) string {
	return strings.TrimSpace(deviceID) + ":" + strings.TrimSpace(sessionID)
}

func (g *GB28181API) storeUpgradeState(state UpgradeState) {
	if err := g.storeUpgradeStateContext(context.Background(), state); err != nil {
		slog.Error("store upgrade state", "device_id", state.DeviceID, "session_id", state.SessionID, "err", err)
	}
}

func (g *GB28181API) storeUpgradeStateContext(ctx context.Context, state UpgradeState) error {
	state.DeviceID = strings.TrimSpace(state.DeviceID)
	state.ChannelID = strings.TrimSpace(state.ChannelID)
	state.SessionID = strings.TrimSpace(state.SessionID)
	if g == nil || state.DeviceID == "" || state.SessionID == "" {
		return errors.New("invalid upgrade state")
	}
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now()
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode upgrade state: %w", err)
	}
	persisted, persistedOK, err := g.loadUpgradeState(ctx, state.DeviceID, state.SessionID)
	if err != nil {
		return err
	}
	g.upgradeStateMu.Lock()
	defer g.upgradeStateMu.Unlock()
	if g.upgradeStates == nil {
		g.upgradeStates = make(map[string]UpgradeState)
	}
	key := upgradeStateKey(state.DeviceID, state.SessionID)
	current, ok := g.upgradeStates[key]
	if !ok && persistedOK {
		current, ok = persisted, true
	}
	if ok {
		// DeviceUpgradeResult 是标准定义的最终通知，优先级高于控制应答。
		// 防止通知先到、迟到的 accepted/rejected 再覆盖 completed/failed。
		if isUpgradeFinal(current.Status) && !isUpgradeFinal(state.Status) {
			return nil
		}
	}
	if err := g.saveTaskState(ctx, gbTaskKindUpgrade, state.DeviceID, state.SessionID, payload, state.UpdatedAt); err != nil {
		return err
	}
	g.upgradeStates[key] = state
	if len(g.upgradeStates) > maxUpgradeStates {
		oldestKey := ""
		oldestAt := state.UpdatedAt
		for candidateKey, candidate := range g.upgradeStates {
			if candidateKey != key && (oldestKey == "" || candidate.UpdatedAt.Before(oldestAt)) {
				oldestKey = candidateKey
				oldestAt = candidate.UpdatedAt
			}
		}
		delete(g.upgradeStates, oldestKey)
	}
	return nil
}

func isUpgradeFinal(status string) bool {
	return status == "completed" || status == "failed"
}

// UpgradeState 返回指定设备和会话的最新升级状态。
func (g *GB28181API) UpgradeState(deviceID, sessionID string) (UpgradeState, bool) {
	state, ok, err := g.loadUpgradeState(context.Background(), deviceID, sessionID)
	if err != nil {
		slog.Error("load upgrade state", "device_id", deviceID, "session_id", sessionID, "err", err)
		return UpgradeState{}, false
	}
	return state, ok
}

func (g *GB28181API) loadUpgradeState(ctx context.Context, deviceID, sessionID string) (UpgradeState, bool, error) {
	if g == nil {
		return UpgradeState{}, false, nil
	}
	key := upgradeStateKey(deviceID, sessionID)
	g.upgradeStateMu.RLock()
	state, ok := g.upgradeStates[key]
	g.upgradeStateMu.RUnlock()
	if ok && runtimeStateExpired(state.UpdatedAt, time.Now(), upgradeStateTTL) {
		g.upgradeStateMu.Lock()
		delete(g.upgradeStates, key)
		g.upgradeStateMu.Unlock()
		if err := g.deleteTaskState(ctx, gbTaskKindUpgrade, deviceID, sessionID); err != nil {
			return UpgradeState{}, false, err
		}
		return UpgradeState{}, false, nil
	}
	if ok {
		return state, true, nil
	}
	payload, found, err := g.loadTaskState(ctx, gbTaskKindUpgrade, strings.TrimSpace(deviceID), strings.TrimSpace(sessionID))
	if err != nil || !found {
		return UpgradeState{}, false, err
	}
	if err := json.Unmarshal(payload, &state); err != nil {
		return UpgradeState{}, false, fmt.Errorf("decode upgrade state: %w", err)
	}
	if upgradeStateKey(state.DeviceID, state.SessionID) != key {
		return UpgradeState{}, false, errors.New("persisted upgrade state identity mismatch")
	}
	if runtimeStateExpired(state.UpdatedAt, time.Now(), upgradeStateTTL) {
		if err := g.deleteTaskState(ctx, gbTaskKindUpgrade, deviceID, sessionID); err != nil {
			return UpgradeState{}, false, err
		}
		return UpgradeState{}, false, nil
	}
	g.upgradeStateMu.Lock()
	if g.upgradeStates == nil {
		g.upgradeStates = make(map[string]UpgradeState)
	}
	if current, exists := g.upgradeStates[key]; exists {
		state = current
	} else {
		g.upgradeStates[key] = state
	}
	g.upgradeStateMu.Unlock()
	return state, true, nil
}

func (g *GB28181API) deleteUpgradeState(ctx context.Context, deviceID, sessionID string) {
	if g == nil {
		return
	}
	g.upgradeStateMu.Lock()
	delete(g.upgradeStates, upgradeStateKey(deviceID, sessionID))
	g.upgradeStateMu.Unlock()
	if err := g.deleteTaskState(ctx, gbTaskKindUpgrade, deviceID, sessionID); err != nil {
		slog.Error("delete upgrade state", "device_id", deviceID, "session_id", sessionID, "err", err)
	}
}

func (g *GB28181API) cleanupUpgradeStates(now time.Time) {
	if g == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	g.upgradeStateMu.Lock()
	for key, state := range g.upgradeStates {
		if runtimeStateExpired(state.UpdatedAt, now, upgradeStateTTL) {
			delete(g.upgradeStates, key)
		}
	}
	g.upgradeStateMu.Unlock()
}

// sipMessageDeviceUpgradeResult 处理 2022 A.2.5.9 设备软件升级最终结果通知。
func (g *GB28181API) sipMessageDeviceUpgradeResult(ctx *sip.Context) {
	if err := g.requireGBVersionAtLeast(ctx.DeviceID, gbVersion2022, "设备软件升级结果通知(A.2.5.9)"); err != nil {
		ctx.String(400, err.Error())
		return
	}
	var msg deviceUpgradeResultNotify
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	msg.SessionID = strings.TrimSpace(msg.SessionID)
	msg.Result = strings.ToUpper(strings.TrimSpace(msg.Result))
	msg.Firmware = strings.TrimSpace(msg.Firmware)
	msg.FailedReason = strings.TrimSpace(msg.FailedReason)
	if msg.XMLName.Local != "Notify" || msg.SN <= 0 || !strings.EqualFold(strings.TrimSpace(msg.CmdType), "DeviceUpgradeResult") || !isGBDeviceIdentifier(msg.DeviceID) || validateGBSessionID(msg.SessionID) != nil || msg.Firmware == "" ||
		(msg.Result != "OK" && msg.Result != "ERROR") || msg.Result == "ERROR" && !validUpgradeFailedReason(msg.FailedReason) {
		ctx.String(400, "invalid DeviceUpgradeResult notification")
		return
	}
	if msg.DeviceID != ctx.DeviceID {
		if _, ok := g.svr.memoryStorer.GetChannel(ctx.DeviceID, msg.DeviceID); !ok {
			ctx.String(400, "DeviceUpgradeResult target mismatch")
			return
		}
	}
	state, ok, err := g.loadUpgradeState(context.Background(), ctx.DeviceID, msg.SessionID)
	if err != nil {
		ctx.String(500, "load DeviceUpgradeResult session failed")
		return
	}
	if !ok {
		ctx.String(400, "DeviceUpgradeResult session not found")
		return
	}
	if ok && strings.TrimSpace(state.ChannelID) != "" && strings.TrimSpace(state.ChannelID) != msg.DeviceID {
		ctx.String(400, "DeviceUpgradeResult session target mismatch")
		return
	}
	state.SN = msg.SN
	state.Result = msg.Result
	state.Firmware = msg.Firmware
	state.FailedReason = msg.FailedReason
	state.UpdatedAt = time.Now()
	if strings.EqualFold(msg.Result, "OK") {
		state.Status = "completed"
	} else {
		state.Status = "failed"
	}
	if err := g.storeUpgradeStateContext(context.Background(), state); err != nil {
		ctx.String(500, "store DeviceUpgradeResult failed")
		return
	}
	if forwarded, err := g.forwardCascadeTaskNotification(context.Background(), cascadeTaskUpgrade, ctx.DeviceID, msg.DeviceID, msg.SessionID, ctx.Request.Body()); forwarded && err != nil {
		ctx.String(502, "forward DeviceUpgradeResult failed")
		return
	}
	ctx.String(200, "OK")
}

func validUpgradeFailedReason(value string) bool {
	switch strings.TrimSpace(value) {
	case "01", "02", "03", "99":
		return true
	default:
		return false
	}
}
