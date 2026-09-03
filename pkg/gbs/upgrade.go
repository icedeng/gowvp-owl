package gbs

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	maxUpgradeStates = 1024
	upgradeStateTTL  = 7 * 24 * time.Hour
)

var errUpgradeFinalConflict = errors.New("upgrade final state conflicts with existing result")

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
	Firmware     *string  `xml:"Firmware"`
	FailedReason string   `xml:"UpgradeFailedReason"`
}

func newDeviceUpgradeConfig(in *UpgradeInput, sessionID string) *deviceUpgradeConfig {
	return &deviceUpgradeConfig{
		Firmware: in.Firmware, FileURL: in.FileURL,
		Manufacturer: in.Manufacturer, SessionID: sessionID,
	}
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

	operation, releaseOperation := g.trackPendingDeviceRequest(ctx, in.DeviceID, in.ChannelID)
	defer releaseOperation()
	sn, waitKey, pending := g.reservePendingDeviceControl(in.DeviceID, in.ChannelID, operation)
	defer g.pendingDeviceControl.CompareAndDelete(waitKey, pending)
	req := deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: sn, DeviceID: in.ChannelID,
		DeviceUpgrade: newDeviceUpgradeConfig(in, sessionID),
	}

	body, err := sip.XMLEncode(req)
	if err != nil {
		return nil, err
	}
	pendingState := UpgradeState{
		SN: sn, DeviceID: in.DeviceID, ChannelID: in.ChannelID, SessionID: sessionID,
		Status: "pending", Firmware: in.Firmware, UpdatedAt: time.Now(),
	}
	var stateErr error
	if !operation.Deliver(func() {
		stateErr = g.storeUpgradeStateContext(ctx, pendingState)
	}) {
		return nil, operation.Cause()
	}
	if stateErr != nil {
		if deleteErr := g.deleteUpgradeStateContext(g.serviceContext(), in.DeviceID, sessionID); deleteErr != nil {
			return nil, errors.Join(stateErr, deleteErr)
		}
		return nil, stateErr
	}

	requestCtx := pending.operation.Context(ctx)
	tx, err := g.svr.wrapRequestContext(requestCtx, ch, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		err = pending.operation.ErrorOr(err)
		if deleteErr := g.deleteUpgradeStateContext(g.serviceContext(), in.DeviceID, sessionID); deleteErr != nil {
			return nil, errors.Join(err, deleteErr)
		}
		return nil, err
	}
	if _, err = sipResponseContext(requestCtx, tx); err != nil {
		err = pending.operation.ErrorOr(err)
		if errors.Is(err, ErrDeviceNotExist) {
			return nil, g.persistUpgradeCancellationError(pendingState, err)
		}
		return nil, g.persistUpgradeOutcomeError(pendingState, "response_timeout", err)
	}

	timer := time.NewTimer(in.Timeout)
	defer timer.Stop()
	select {
	case resp := <-pending.wait:
		result := strings.ToUpper(strings.TrimSpace(resp.Result))
		pendingState.Result = result
		pendingState.UpdatedAt = time.Now()
		if result != "OK" {
			return nil, g.persistUpgradeOperationOutcome(pending.operation, pendingState, "rejected", fmt.Errorf("device upgrade failed: %s", resp.Result))
		}
		pendingState.Status = "accepted"
		var stateErr error
		if !pending.operation.Deliver(func() {
			stateErr = g.storeUpgradeStateContext(g.taskPersistenceContext(), pendingState)
		}) {
			return nil, g.persistUpgradeCancellationError(pendingState, pending.operation.Cause())
		}
		if stateErr != nil {
			return nil, stateErr
		}
		return &UpgradeOutput{
			SN: sn, DeviceID: in.DeviceID, Channel: in.ChannelID, SessionID: sessionID, Result: result,
		}, nil
	case <-timer.C:
		return nil, g.persistUpgradeOperationOutcome(pending.operation, pendingState, "response_timeout", errors.New("wait device upgrade response timeout"))
	case <-pending.operation.Done():
		return nil, g.persistUpgradeCancellationError(pendingState, pending.operation.Cause())
	case <-g.serviceDone():
		return nil, g.persistUpgradeOutcomeError(pendingState, "cancelled", ErrServiceStopped)
	}
}

func (g *GB28181API) persistUpgradeOperationOutcome(operation *pendingDeviceOperation, state UpgradeState, status string, cause error) error {
	var result error
	if operation.Deliver(func() {
		result = g.persistUpgradeOutcomeError(state, status, cause)
	}) {
		return result
	}
	return g.persistUpgradeCancellationError(state, operation.Cause())
}

func (g *GB28181API) persistUpgradeCancellationError(state UpgradeState, cause error) error {
	if errors.Is(cause, ErrDeviceNotExist) {
		if err := g.deleteUpgradeStateContext(g.serviceContext(), state.DeviceID, state.SessionID); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}
	return g.persistUpgradeOutcomeError(state, "cancelled", cause)
}

func (g *GB28181API) persistUpgradeOutcomeError(state UpgradeState, status string, cause error) error {
	state.Status = status
	state.UpdatedAt = time.Now()
	if err := g.storeUpgradeStateContext(g.taskPersistenceContext(), state); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func normalizeUpgradeSessionID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = sip.RandString(32)
	}
	return value, validateGBSessionID(value)
}

func validateGBSessionID(value string) error {
	if !utf8.ValidString(value) {
		return errors.New("session_id must be valid UTF-8")
	}
	length := utf8.RuneCountInString(value)
	if length < 32 || length > 128 {
		return errors.New("session_id must contain 32 to 128 characters")
	}
	for _, char := range value {
		if char == '\t' || char == '\n' || char == '\r' || char >= 0x20 && char <= 0xD7FF ||
			char >= 0xE000 && char <= 0xFFFD || char >= 0x10000 && char <= 0x10FFFF {
			continue
		}
		return errors.New("session_id contains a character forbidden by XML 1.0")
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
		if isUpgradeFinal(current.Status) && isUpgradeFinal(state.Status) {
			if sameUpgradeFinalOutcome(current, state) {
				return nil
			}
			return errUpgradeFinalConflict
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

func sameUpgradeFinalOutcome(current, candidate UpgradeState) bool {
	return current.Status == candidate.Status &&
		strings.EqualFold(strings.TrimSpace(current.Result), strings.TrimSpace(candidate.Result)) &&
		current.Firmware == candidate.Firmware &&
		strings.TrimSpace(current.FailedReason) == strings.TrimSpace(candidate.FailedReason)
}

// UpgradeState 返回指定设备和会话的最新升级状态。
func (g *GB28181API) UpgradeState(deviceID, sessionID string) (UpgradeState, bool) {
	state, ok, err := g.UpgradeStateContext(context.Background(), deviceID, sessionID)
	if err != nil {
		slog.Error("load upgrade state", "device_id", deviceID, "session_id", sessionID, "err", err)
		return UpgradeState{}, false
	}
	return state, ok
}

// UpgradeStateContext 返回升级状态，并保留持久化读取错误供 API 层区分故障与会话不存在。
func (g *GB28181API) UpgradeStateContext(ctx context.Context, deviceID, sessionID string) (UpgradeState, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return UpgradeState{}, false, err
	}
	return g.loadUpgradeState(ctx, deviceID, sessionID)
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
		if err := g.deleteUpgradeStateContext(ctx, deviceID, sessionID); err != nil {
			return UpgradeState{}, false, err
		}
		return UpgradeState{}, false, nil
	}
	if ok {
		return state, true, nil
	}
	deviceID = strings.TrimSpace(deviceID)
	sessionID = strings.TrimSpace(sessionID)
	payload, found, err := g.loadTaskState(ctx, gbTaskKindUpgrade, deviceID, sessionID)
	if err != nil || !found {
		return UpgradeState{}, false, err
	}
	if err := json.Unmarshal(payload, &state); err != nil {
		return g.quarantineUpgradeState(ctx, deviceID, sessionID, payload, fmt.Errorf("decode upgrade state: %w", err))
	}
	if err := validatePersistedUpgradeState(state, deviceID, sessionID, time.Now()); err != nil {
		return g.quarantineUpgradeState(ctx, deviceID, sessionID, payload, err)
	}
	if runtimeStateExpired(state.UpdatedAt, time.Now(), upgradeStateTTL) {
		if err := g.deleteUpgradeStateContext(ctx, deviceID, sessionID); err != nil {
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

func validatePersistedUpgradeState(state UpgradeState, deviceID, sessionID string, now time.Time) error {
	if upgradeStateKey(state.DeviceID, state.SessionID) != upgradeStateKey(deviceID, sessionID) {
		return errors.New("persisted upgrade state identity mismatch")
	}
	if !isGBDeviceIdentifier(strings.TrimSpace(state.DeviceID)) ||
		(strings.TrimSpace(state.ChannelID) != "" && !isGBDeviceIdentifier(strings.TrimSpace(state.ChannelID))) ||
		validateGBSessionID(strings.TrimSpace(state.SessionID)) != nil || state.SN < 0 {
		return errors.New("persisted upgrade state contains invalid identifiers")
	}
	switch state.Status {
	case "pending", "accepted", "rejected", "response_timeout", "cancelled", "completed", "failed":
	default:
		return fmt.Errorf("persisted upgrade state has invalid status %q", state.Status)
	}
	if state.UpdatedAt.IsZero() || state.UpdatedAt.After(now.Add(5*time.Minute)) {
		return errors.New("persisted upgrade state has invalid update time")
	}
	if state.Status == "completed" && !strings.EqualFold(strings.TrimSpace(state.Result), "OK") {
		return errors.New("persisted completed upgrade state must have an OK result")
	}
	if state.Status == "failed" && (!strings.EqualFold(strings.TrimSpace(state.Result), "ERROR") || !validUpgradeFailedReason(state.FailedReason)) {
		return errors.New("persisted failed upgrade state has an invalid result")
	}
	return nil
}

func (g *GB28181API) quarantineUpgradeState(ctx context.Context, deviceID, sessionID string, invalidPayload []byte, cause error) (UpgradeState, bool, error) {
	slog.Warn("remove invalid persisted upgrade state", "device_id", deviceID, "session_id", sessionID, "err", cause)
	key := upgradeStateKey(deviceID, sessionID)
	g.upgradeStateMu.Lock()
	defer g.upgradeStateMu.Unlock()
	if current, ok := g.upgradeStates[key]; ok {
		return current, true, nil
	}
	currentPayload, found, err := g.loadTaskState(ctx, gbTaskKindUpgrade, deviceID, sessionID)
	if err != nil {
		return UpgradeState{}, false, errors.Join(cause, err)
	}
	if !found {
		return UpgradeState{}, false, nil
	}
	if !bytes.Equal(currentPayload, invalidPayload) {
		return UpgradeState{}, false, fmt.Errorf("persisted upgrade state changed while quarantining: %w", cause)
	}
	if err := g.deleteTaskState(ctx, gbTaskKindUpgrade, deviceID, sessionID); err != nil {
		return UpgradeState{}, false, errors.Join(cause, err)
	}
	delete(g.upgradeStates, key)
	return UpgradeState{}, false, nil
}

func (g *GB28181API) deleteUpgradeState(ctx context.Context, deviceID, sessionID string) {
	if err := g.deleteUpgradeStateContext(ctx, deviceID, sessionID); err != nil {
		slog.Error("delete upgrade state", "device_id", deviceID, "session_id", sessionID, "err", err)
	}
}

func (g *GB28181API) deleteUpgradeStateContext(ctx context.Context, deviceID, sessionID string) error {
	if g == nil {
		return nil
	}
	g.upgradeStateMu.Lock()
	defer g.upgradeStateMu.Unlock()
	if err := g.deleteTaskState(ctx, gbTaskKindUpgrade, deviceID, sessionID); err != nil {
		return err
	}
	delete(g.upgradeStates, upgradeStateKey(deviceID, sessionID))
	return nil
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
	if !requireMessageNotification(ctx, "DeviceUpgradeResult") {
		return
	}
	if err := g.requireGBVersionAtLeast(ctx.DeviceID, gbVersion2022, "设备软件升级结果通知(A.2.5.9)"); err != nil {
		ctx.String(400, err.Error())
		return
	}
	var msg deviceUpgradeResultNotify
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	if err := validateDeviceUpgradeResultStructure(ctx.Request.Body()); err != nil {
		ctx.String(400, err.Error())
		return
	}
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	msg.SessionID = strings.TrimSpace(msg.SessionID)
	msg.Result = strings.ToUpper(strings.TrimSpace(msg.Result))
	msg.FailedReason = strings.TrimSpace(msg.FailedReason)
	if msg.XMLName.Local != "Notify" || msg.SN <= 0 || !strings.EqualFold(strings.TrimSpace(msg.CmdType), "DeviceUpgradeResult") || !isGBDeviceIdentifier(msg.DeviceID) || validateGBSessionID(msg.SessionID) != nil || msg.Firmware == nil ||
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
	binding, hasBinding := admittedInboundRegistrationBinding(ctx)
	var unlockCommit func()
	var err error
	if hasBinding {
		unlockCommit, err = g.lockInboundDeviceStateCommit(ctx.DeviceID, binding)
	} else {
		unlockCommit, err = g.lockInboundDeviceStateCommit(ctx.DeviceID)
	}
	if err != nil {
		if errors.Is(err, errInboundDeviceGenerationChanged) {
			ctx.String(200, "OK")
			return
		}
		ctx.String(403, err.Error())
		return
	}
	defer unlockCommit()
	requestCtx := g.serviceContext()
	state, ok, err := g.loadUpgradeState(requestCtx, ctx.DeviceID, msg.SessionID)
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
	state.Firmware = *msg.Firmware
	state.FailedReason = msg.FailedReason
	state.UpdatedAt = time.Now()
	if strings.EqualFold(msg.Result, "OK") {
		state.Status = "completed"
	} else {
		state.Status = "failed"
	}
	if err := g.storeUpgradeStateContext(requestCtx, state); err != nil {
		if errors.Is(err, errUpgradeFinalConflict) {
			ctx.String(409, "DeviceUpgradeResult conflicts with completed session")
		} else {
			ctx.String(500, "store DeviceUpgradeResult failed")
		}
		return
	}
	if forwarded, err := g.forwardCascadeTaskNotification(requestCtx, cascadeTaskUpgrade, ctx.DeviceID, msg.DeviceID, msg.SessionID, ctx.Request.Body()); forwarded && err != nil {
		ctx.String(502, "forward DeviceUpgradeResult failed")
		return
	}
	respondErr := ctx.RespondString(200, "OK")
	ctx.Abort()
	if respondErr != nil {
		// 最终状态和级联转发必须先可靠完成，才能决定 200/5xx；
		// 写失败保留幂等终态，等待同一 SIP 事务重新进入 handler。
		slog.Error("acknowledge DeviceUpgradeResult failed", "device_id", ctx.DeviceID, "session_id", msg.SessionID, "err", respondErr)
	}
}

func validUpgradeFailedReason(value string) bool {
	switch strings.TrimSpace(value) {
	case "01", "02", "03", "99":
		return true
	default:
		return false
	}
}
