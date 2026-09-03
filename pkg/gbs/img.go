package gbs

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/netpulse/ip"
)

const (
	maxSnapshotStates = 1024
	snapshotStateTTL  = 7 * 24 * time.Hour
)

var (
	ErrSnapshotSessionNotFound  = errors.New("snapshot session not found")
	ErrSnapshotCoverKeyMismatch = errors.New("snapshot cover key mismatch")
)

type SnapshotState struct {
	DeviceID      string    `json:"device_id"`
	ChannelID     string    `json:"channel_id,omitempty"`
	CoverKey      string    `json:"cover_key,omitempty"`
	SessionID     string    `json:"session_id"`
	Status        string    `json:"status"`
	ExpectedCount int       `json:"expected_count"`
	ReceivedCount int       `json:"received_count"`
	FileIDs       []string  `json:"file_ids,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type snapshotFinishedNotify struct {
	XMLName      xml.Name
	CmdType      string `xml:"CmdType"`
	SN           int    `xml:"SN"`
	DeviceID     string `xml:"DeviceID"`
	SessionID    string `xml:"SessionID"`
	SnapShotList struct {
		XMLName xml.Name
		FileIDs []string `xml:"SnapShotFileID"`
	} `xml:"SnapShotList"`
}

func (g *GB28181API) QuerySnapshot(deviceID, targetID, coverKey string) (*SnapshotState, error) {
	return g.QuerySnapshotContext(context.Background(), deviceID, targetID, coverKey)
}

// QuerySnapshotContext 下发抓拍并允许调用方取消 SIP 及业务应答等待。
func (g *GB28181API) QuerySnapshotContext(ctx context.Context, deviceID, targetID, coverKey string) (*SnapshotState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deviceID = strings.TrimSpace(deviceID)
	targetID = strings.TrimSpace(targetID)
	coverKey = strings.TrimSpace(coverKey)
	if targetID == "" {
		targetID = deviceID
	}
	if err := validateSnapshotCoverKey(coverKey); err != nil {
		return nil, err
	}
	slog.Debug("QuerySnapshot", "deviceID", deviceID)
	if err := g.requireGBVersionAtLeast(deviceID, gbVersion2022, "图像抓拍(9.14)"); err != nil {
		return nil, err
	}
	if err := g.requireGBFeature(deviceID, "snapshot", "图像抓拍(9.14)", func(c GBCapabilities) bool {
		return c.Snapshot
	}); err != nil {
		return nil, err
	}
	ipc, ok := g.svr.memoryStorer.Load(deviceID)
	if !ok || !ipc.IsOnlineNow() {
		return nil, ErrDeviceOffline
	}
	var target Targeter = ipc
	if targetID != deviceID {
		channel, exists := g.svr.memoryStorer.GetChannel(deviceID, targetID)
		if !exists {
			return nil, ErrChannelNotExist
		}
		target = channel
	}

	sn, waitKey, pending, releaseOperation := g.reservePendingDeviceConfig(ctx, deviceID, targetID)
	defer releaseOperation()
	defer g.pendingDeviceConfig.CompareAndDelete(waitKey, pending)
	sessionID := sip.RandString(32)
	initialState := SnapshotState{
		DeviceID: deviceID, ChannelID: targetID, CoverKey: coverKey, SessionID: sessionID,
		Status: "pending", ExpectedCount: 1, UpdatedAt: time.Now(),
	}
	var stateErr error
	if !pending.operation.Deliver(func() {
		stateErr = g.storeSnapshotStateContext(ctx, initialState)
	}) {
		return nil, pending.operation.Cause()
	}
	if stateErr != nil {
		if deleteErr := g.deleteSnapshotStateContext(g.serviceContext(), deviceID, sessionID); deleteErr != nil {
			return nil, errors.Join(stateErr, deleteErr)
		}
		return nil, stateErr
	}
	body := NewDeviceConfig(targetID).SetSN(sn).SetSnapShotConfig(&SnapShot{
		SnapNum:   1,
		Interval:  1,
		UploadURL: g.buildSnapshotUploadURL(deviceID, coverKey, sessionID),
		SessionID: sessionID,
	}).Marshal()

	requestCtx := pending.operation.Context(ctx)
	tx, err := g.svr.wrapRequestContext(requestCtx, target, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		err = pending.operation.ErrorOr(err)
		if deleteErr := g.deleteSnapshotStateContext(g.serviceContext(), deviceID, sessionID); deleteErr != nil {
			return nil, errors.Join(err, deleteErr)
		}
		return nil, err
	}
	if _, err = sipResponseContext(requestCtx, tx); err != nil {
		cause := pending.operation.ErrorOr(err)
		if errors.Is(cause, ErrDeviceNotExist) {
			return nil, g.persistSnapshotCancellationError(deviceID, sessionID, cause)
		}
		return nil, g.persistSnapshotOutcomeError(deviceID, sessionID, "failed", cause)
	}

	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	select {
	case resp := <-pending.wait:
		if strings.EqualFold(strings.TrimSpace(resp.Result), "OK") {
			var (
				state    SnapshotState
				exists   bool
				stateErr error
			)
			if !pending.operation.Deliver(func() {
				state, exists, stateErr = g.transitionSnapshotStateContext(g.taskPersistenceContext(), deviceID, sessionID, "accepted")
			}) {
				return nil, g.persistSnapshotCancellationError(deviceID, sessionID, pending.operation.Cause())
			}
			if stateErr != nil {
				return nil, stateErr
			}
			if !exists {
				return nil, fmt.Errorf("snapshot session disappeared before acceptance")
			}
			return &state, nil
		}
		return nil, g.persistSnapshotOperationOutcome(pending.operation, deviceID, sessionID, "rejected", fmt.Errorf("snapshot config failed: %s", resp.Result))
	case <-g.serviceDone():
		return nil, g.persistSnapshotOutcomeError(deviceID, sessionID, "cancelled", ErrServiceStopped)
	case <-pending.operation.Done():
		return nil, g.persistSnapshotCancellationError(deviceID, sessionID, pending.operation.Cause())
	case <-timer.C:
		return nil, g.persistSnapshotOperationOutcome(pending.operation, deviceID, sessionID, "response_timeout", errors.New("wait snapshot response timeout"))
	}
}

func (g *GB28181API) persistSnapshotOperationOutcome(operation *pendingDeviceOperation, deviceID, sessionID, status string, cause error) error {
	var result error
	if operation.Deliver(func() {
		result = g.persistSnapshotOutcomeError(deviceID, sessionID, status, cause)
	}) {
		return result
	}
	return g.persistSnapshotCancellationError(deviceID, sessionID, operation.Cause())
}

func (g *GB28181API) persistSnapshotCancellationError(deviceID, sessionID string, cause error) error {
	if errors.Is(cause, ErrDeviceNotExist) {
		if err := g.deleteSnapshotStateContext(g.serviceContext(), deviceID, sessionID); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}
	return g.persistSnapshotOutcomeError(deviceID, sessionID, "cancelled", cause)
}

func (g *GB28181API) persistSnapshotOutcomeError(deviceID, sessionID, status string, cause error) error {
	_, _, err := g.transitionSnapshotStateContext(g.taskPersistenceContext(), deviceID, sessionID, status)
	if err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func snapshotStateKey(deviceID, sessionID string) string {
	return strings.TrimSpace(deviceID) + ":" + strings.TrimSpace(sessionID)
}

func validateSnapshotCoverKey(value string) error {
	if len(value) == 0 || len(value) > 128 {
		return fmt.Errorf("snapshot cover key must contain 1 to 128 characters")
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return fmt.Errorf("snapshot cover key may contain only letters, digits, hyphen, and underscore")
	}
	return nil
}

func (g *GB28181API) storeSnapshotState(state SnapshotState) {
	if err := g.storeSnapshotStateContext(context.Background(), state); err != nil {
		slog.Error("store snapshot state", "device_id", state.DeviceID, "session_id", state.SessionID, "err", err)
	}
}

func (g *GB28181API) storeSnapshotStateContext(ctx context.Context, state SnapshotState) error {
	state.DeviceID = strings.TrimSpace(state.DeviceID)
	state.ChannelID = strings.TrimSpace(state.ChannelID)
	state.SessionID = strings.TrimSpace(state.SessionID)
	state.CoverKey = strings.TrimSpace(state.CoverKey)
	if g == nil || state.DeviceID == "" || state.SessionID == "" {
		return fmt.Errorf("invalid snapshot state")
	}
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now()
	}
	state.FileIDs = append([]string(nil), state.FileIDs...)
	persisted, persistedOK, err := g.loadSnapshotState(ctx, state.DeviceID, state.SessionID)
	if err != nil {
		return err
	}
	g.snapshotStateMu.Lock()
	defer g.snapshotStateMu.Unlock()
	key := snapshotStateKey(state.DeviceID, state.SessionID)
	current, ok := g.snapshotStates[key]
	if !ok && persistedOK {
		current, ok = persisted, true
	}
	if ok && isSnapshotTerminal(current.Status) && !isSnapshotTerminal(state.Status) {
		return nil
	}
	if err := g.persistSnapshotState(ctx, state); err != nil {
		return err
	}
	g.storeSnapshotStateMemoryLocked(state)
	return nil
}

func (g *GB28181API) storeSnapshotStateMemoryLocked(state SnapshotState) {
	if g.snapshotStates == nil {
		g.snapshotStates = make(map[string]SnapshotState)
	}
	key := snapshotStateKey(state.DeviceID, state.SessionID)
	g.snapshotStates[key] = state
	if len(g.snapshotStates) > maxSnapshotStates {
		oldestKey := ""
		oldestAt := state.UpdatedAt
		for candidateKey, candidate := range g.snapshotStates {
			if candidateKey != key && (oldestKey == "" || candidate.UpdatedAt.Before(oldestAt)) {
				oldestKey = candidateKey
				oldestAt = candidate.UpdatedAt
			}
		}
		delete(g.snapshotStates, oldestKey)
	}
}

func (g *GB28181API) deleteSnapshotState(deviceID, sessionID string) {
	if err := g.deleteSnapshotStateContext(context.Background(), deviceID, sessionID); err != nil {
		slog.Error("delete snapshot state", "device_id", deviceID, "session_id", sessionID, "err", err)
	}
}

func (g *GB28181API) deleteSnapshotStateContext(ctx context.Context, deviceID, sessionID string) error {
	if g == nil {
		return nil
	}
	g.snapshotStateMu.Lock()
	defer g.snapshotStateMu.Unlock()
	if err := g.deleteTaskState(ctx, gbTaskKindSnapshot, deviceID, sessionID); err != nil {
		return err
	}
	delete(g.snapshotStates, snapshotStateKey(deviceID, sessionID))
	return nil
}

func (g *GB28181API) transitionSnapshotState(deviceID, sessionID, status string) (SnapshotState, bool) {
	state, ok, err := g.transitionSnapshotStateContext(context.Background(), deviceID, sessionID, status)
	if err != nil {
		slog.Error("transition snapshot state", "device_id", deviceID, "session_id", sessionID, "status", status, "err", err)
		return SnapshotState{}, false
	}
	return state, ok
}

func (g *GB28181API) transitionSnapshotStateContext(ctx context.Context, deviceID, sessionID, status string) (SnapshotState, bool, error) {
	if g == nil {
		return SnapshotState{}, false, nil
	}
	if _, ok, err := g.loadSnapshotState(ctx, deviceID, sessionID); err != nil || !ok {
		return SnapshotState{}, false, err
	}
	g.snapshotStateMu.Lock()
	defer g.snapshotStateMu.Unlock()
	key := snapshotStateKey(deviceID, sessionID)
	state, ok := g.snapshotStates[key]
	if !ok || runtimeStateExpired(state.UpdatedAt, time.Now(), snapshotStateTTL) {
		delete(g.snapshotStates, key)
		return SnapshotState{}, false, nil
	}
	switch status {
	case "accepted":
		if state.Status == "pending" {
			state.Status = status
		}
	default:
		if state.Status == "pending" || state.Status == "accepted" || state.Status == "response_timeout" {
			state.Status = status
		}
	}
	state.UpdatedAt = time.Now()
	if err := g.persistSnapshotState(ctx, state); err != nil {
		return SnapshotState{}, false, err
	}
	g.storeSnapshotStateMemoryLocked(state)
	state.FileIDs = append([]string(nil), state.FileIDs...)
	return state, true, nil
}

func (g *GB28181API) SnapshotState(deviceID, sessionID string) (SnapshotState, bool) {
	state, ok, err := g.SnapshotStateContext(context.Background(), deviceID, sessionID)
	if err != nil {
		slog.Error("load snapshot state", "device_id", deviceID, "session_id", sessionID, "err", err)
		return SnapshotState{}, false
	}
	return state, ok
}

// SnapshotStateContext 返回抓拍状态，并保留持久化读取错误供 API 层区分故障与会话不存在。
func (g *GB28181API) SnapshotStateContext(ctx context.Context, deviceID, sessionID string) (SnapshotState, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return SnapshotState{}, false, err
	}
	return g.loadSnapshotState(ctx, deviceID, sessionID)
}

func (g *GB28181API) loadSnapshotState(ctx context.Context, deviceID, sessionID string) (SnapshotState, bool, error) {
	if g == nil {
		return SnapshotState{}, false, nil
	}
	key := snapshotStateKey(deviceID, sessionID)
	g.snapshotStateMu.RLock()
	state, ok := g.snapshotStates[key]
	g.snapshotStateMu.RUnlock()
	if ok && runtimeStateExpired(state.UpdatedAt, time.Now(), snapshotStateTTL) {
		if err := g.deleteSnapshotStateContext(ctx, deviceID, sessionID); err != nil {
			return SnapshotState{}, false, err
		}
		return SnapshotState{}, false, nil
	}
	if !ok {
		deviceID = strings.TrimSpace(deviceID)
		sessionID = strings.TrimSpace(sessionID)
		payload, found, err := g.loadTaskState(ctx, gbTaskKindSnapshot, deviceID, sessionID)
		if err != nil || !found {
			return SnapshotState{}, false, err
		}
		if err := json.Unmarshal(payload, &state); err != nil {
			return g.quarantineSnapshotState(ctx, deviceID, sessionID, payload, fmt.Errorf("decode snapshot state: %w", err))
		}
		if err := validatePersistedSnapshotState(state, deviceID, sessionID, time.Now()); err != nil {
			return g.quarantineSnapshotState(ctx, deviceID, sessionID, payload, err)
		}
		if runtimeStateExpired(state.UpdatedAt, time.Now(), snapshotStateTTL) {
			if err := g.deleteSnapshotStateContext(ctx, deviceID, sessionID); err != nil {
				return SnapshotState{}, false, err
			}
			return SnapshotState{}, false, nil
		}
		g.snapshotStateMu.Lock()
		if g.snapshotStates == nil {
			g.snapshotStates = make(map[string]SnapshotState)
		}
		if current, exists := g.snapshotStates[key]; exists {
			state = current
		} else {
			g.storeSnapshotStateMemoryLocked(state)
		}
		g.snapshotStateMu.Unlock()
		ok = true
	}
	state.FileIDs = append([]string(nil), state.FileIDs...)
	return state, ok, nil
}

func validatePersistedSnapshotState(state SnapshotState, deviceID, sessionID string, now time.Time) error {
	if snapshotStateKey(state.DeviceID, state.SessionID) != snapshotStateKey(deviceID, sessionID) {
		return errors.New("persisted snapshot state identity mismatch")
	}
	if !isGBDeviceIdentifier(strings.TrimSpace(state.DeviceID)) ||
		(strings.TrimSpace(state.ChannelID) != "" && !isGBDeviceIdentifier(strings.TrimSpace(state.ChannelID))) ||
		validateGBSessionID(strings.TrimSpace(state.SessionID)) != nil {
		return errors.New("persisted snapshot state contains invalid identifiers")
	}
	switch state.Status {
	case "pending", "accepted", "uploading", "response_timeout", "cancelled", "completed", "failed", "partial_failed", "rejected":
	default:
		return fmt.Errorf("persisted snapshot state has invalid status %q", state.Status)
	}
	if state.ExpectedCount < 0 || state.ExpectedCount > 10 || state.ReceivedCount < 0 ||
		(state.ExpectedCount > 0 && state.ReceivedCount > state.ExpectedCount) || len(state.FileIDs) > 10 {
		return errors.New("persisted snapshot state contains invalid counters")
	}
	if state.UpdatedAt.IsZero() || state.UpdatedAt.After(now.Add(5*time.Minute)) {
		return errors.New("persisted snapshot state has invalid update time")
	}
	seen := make(map[string]struct{}, len(state.FileIDs))
	targetID := strings.TrimSpace(state.ChannelID)
	if targetID == "" {
		targetID = strings.TrimSpace(state.DeviceID)
	}
	for _, fileID := range state.FileIDs {
		fileID = strings.TrimSpace(fileID)
		if !validSnapshotFileID(fileID, targetID) {
			return errors.New("persisted snapshot state contains an invalid file identifier")
		}
		if _, duplicate := seen[fileID]; duplicate {
			return errors.New("persisted snapshot state contains duplicate file identifiers")
		}
		seen[fileID] = struct{}{}
	}
	return nil
}

func (g *GB28181API) quarantineSnapshotState(ctx context.Context, deviceID, sessionID string, invalidPayload []byte, cause error) (SnapshotState, bool, error) {
	slog.Warn("remove invalid persisted snapshot state", "device_id", deviceID, "session_id", sessionID, "err", cause)
	key := snapshotStateKey(deviceID, sessionID)
	g.snapshotStateMu.Lock()
	defer g.snapshotStateMu.Unlock()
	if current, ok := g.snapshotStates[key]; ok {
		current.FileIDs = append([]string(nil), current.FileIDs...)
		return current, true, nil
	}
	currentPayload, found, err := g.loadTaskState(ctx, gbTaskKindSnapshot, deviceID, sessionID)
	if err != nil {
		return SnapshotState{}, false, errors.Join(cause, err)
	}
	if !found {
		return SnapshotState{}, false, nil
	}
	if !bytes.Equal(currentPayload, invalidPayload) {
		return SnapshotState{}, false, fmt.Errorf("persisted snapshot state changed while quarantining: %w", cause)
	}
	if err := g.deleteTaskState(ctx, gbTaskKindSnapshot, deviceID, sessionID); err != nil {
		return SnapshotState{}, false, errors.Join(cause, err)
	}
	delete(g.snapshotStates, key)
	return SnapshotState{}, false, nil
}

func (g *GB28181API) persistSnapshotState(ctx context.Context, state SnapshotState) error {
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode snapshot state: %w", err)
	}
	return g.saveTaskState(ctx, gbTaskKindSnapshot, state.DeviceID, state.SessionID, payload, state.UpdatedAt)
}

func (g *GB28181API) cleanupSnapshotStates(now time.Time) {
	if g == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	g.snapshotStateMu.Lock()
	for key, state := range g.snapshotStates {
		if runtimeStateExpired(state.UpdatedAt, now, snapshotStateTTL) {
			delete(g.snapshotStates, key)
		}
	}
	g.snapshotStateMu.Unlock()
}

// ValidateSnapshotUpload 校验公开上传回调只能写入平台已创建的抓拍会话。
func (g *GB28181API) ValidateSnapshotUpload(deviceID, coverKey, sessionID string) error {
	return g.ValidateSnapshotUploadContext(context.Background(), deviceID, coverKey, sessionID)
}

// ValidateSnapshotUploadContext 区分无效会话与任务存储故障，避免把可重试故障误报为客户端错误。
func (g *GB28181API) ValidateSnapshotUploadContext(ctx context.Context, deviceID, coverKey, sessionID string) error {
	if g == nil {
		return errors.New("GB28181 server is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deviceID = strings.TrimSpace(deviceID)
	coverKey = strings.TrimSpace(coverKey)
	sessionID = strings.TrimSpace(sessionID)
	state, ok, err := g.loadSnapshotState(ctx, deviceID, sessionID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrSnapshotSessionNotFound
	}
	if state.CoverKey != "" && coverKey != state.CoverKey {
		return ErrSnapshotCoverKeyMismatch
	}
	return nil
}

func (g *GB28181API) MarkSnapshotUploaded(deviceID, sessionID string) {
	if err := g.MarkSnapshotUploadedContext(g.taskPersistenceContext(), deviceID, sessionID); err != nil {
		slog.Error("mark snapshot uploaded", "device_id", deviceID, "session_id", sessionID, "err", err)
	}
}

// MarkSnapshotUploadedContext 在持久化成功后提交抓拍上传计数，调用方可据此决定是否确认上传成功。
func (g *GB28181API) MarkSnapshotUploadedContext(ctx context.Context, deviceID, sessionID string) error {
	if g == nil {
		return errors.New("GB28181 server is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deviceID = strings.TrimSpace(deviceID)
	sessionID = strings.TrimSpace(sessionID)
	if deviceID == "" || sessionID == "" {
		return errors.New("snapshot device_id and session_id are required")
	}
	if _, ok, err := g.loadSnapshotState(ctx, deviceID, sessionID); err != nil {
		return err
	} else if !ok {
		return ErrSnapshotSessionNotFound
	}
	key := snapshotStateKey(deviceID, sessionID)
	now := time.Now()
	g.snapshotStateMu.Lock()
	defer g.snapshotStateMu.Unlock()
	state, ok := g.snapshotStates[key]
	if !ok {
		return ErrSnapshotSessionNotFound
	}
	if runtimeStateExpired(state.UpdatedAt, now, snapshotStateTTL) {
		delete(g.snapshotStates, key)
		return errors.New("snapshot session expired")
	}
	// 设备可能在 HTTP 应答丢失后重传同一张图片。抓拍状态没有从上传请求中
	// 获得标准文件标识时，以请求张数为上限保持计数幂等；完成通知仍以设备
	// 提供的 SnapShotFileID 列表判定最终结果。
	if state.ExpectedCount > 0 && state.ReceivedCount >= state.ExpectedCount {
		return nil
	}
	state.ReceivedCount++
	if !isSnapshotTerminal(state.Status) {
		state.Status = "uploading"
	}
	state.UpdatedAt = now
	if err := g.persistSnapshotState(ctx, state); err != nil {
		return err
	}
	g.storeSnapshotStateMemoryLocked(state)
	return nil
}

func isSnapshotTerminal(status string) bool {
	switch status {
	case "completed", "failed", "partial_failed", "rejected":
		return true
	default:
		return false
	}
}

// sipMessageSnapshotFinished 处理 2022 A.2.5.7 图像抓拍传输完成通知。
func (g *GB28181API) sipMessageSnapshotFinished(ctx *sip.Context) {
	if !requireMessageNotification(ctx, "UploadSnapShotFinished") {
		return
	}
	if err := g.requireGBVersionAtLeast(ctx.DeviceID, gbVersion2022, "图像抓拍完成通知(A.2.5.7)"); err != nil {
		ctx.String(400, err.Error())
		return
	}
	var msg snapshotFinishedNotify
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	if err := validateSnapshotFinishedStructure(ctx.Request.Body()); err != nil {
		ctx.String(400, err.Error())
		return
	}
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	msg.SessionID = strings.TrimSpace(msg.SessionID)
	if msg.XMLName.Local != "Notify" || msg.SN <= 0 || !strings.EqualFold(strings.TrimSpace(msg.CmdType), "UploadSnapShotFinished") || !isGBDeviceIdentifier(msg.DeviceID) ||
		validateGBSessionID(msg.SessionID) != nil || msg.SnapShotList.XMLName.Local == "" || len(msg.SnapShotList.FileIDs) > 10 {
		ctx.String(400, "invalid UploadSnapShotFinished notification")
		return
	}
	if msg.DeviceID != ctx.DeviceID {
		if _, ok := g.svr.memoryStorer.GetChannel(ctx.DeviceID, msg.DeviceID); !ok {
			ctx.String(400, "UploadSnapShotFinished target mismatch")
			return
		}
	}
	fileIDs := make([]string, 0, len(msg.SnapShotList.FileIDs))
	seen := make(map[string]struct{}, len(msg.SnapShotList.FileIDs))
	for _, fileID := range msg.SnapShotList.FileIDs {
		fileID = strings.TrimSpace(fileID)
		if !validSnapshotFileID(fileID, msg.DeviceID) {
			ctx.String(400, "invalid SnapShotFileID")
			return
		}
		if _, ok := seen[fileID]; !ok && len(fileIDs) < 10 {
			seen[fileID] = struct{}{}
			fileIDs = append(fileIDs, fileID)
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
	state, ok, err := g.loadSnapshotState(requestCtx, ctx.DeviceID, msg.SessionID)
	if err != nil {
		ctx.String(500, "load UploadSnapShotFinished session failed")
		return
	}
	if !ok {
		ctx.String(400, "UploadSnapShotFinished session not found")
		return
	}
	g.snapshotStateMu.Lock()
	state = g.snapshotStates[snapshotStateKey(ctx.DeviceID, msg.SessionID)]
	now := time.Now()
	if !runtimeStateExpired(state.UpdatedAt, now, snapshotStateTTL) && strings.TrimSpace(state.ChannelID) != "" && strings.TrimSpace(state.ChannelID) != msg.DeviceID {
		g.snapshotStateMu.Unlock()
		ctx.String(400, "UploadSnapShotFinished session target mismatch")
		return
	}
	if runtimeStateExpired(state.UpdatedAt, now, snapshotStateTTL) {
		delete(g.snapshotStates, snapshotStateKey(ctx.DeviceID, msg.SessionID))
		g.snapshotStateMu.Unlock()
		ctx.String(400, "UploadSnapShotFinished session not found")
		return
	}
	candidate := state
	candidate.FileIDs = fileIDs
	candidate.UpdatedAt = now
	switch {
	case len(candidate.FileIDs) == 0:
		candidate.Status = "failed"
	case candidate.ExpectedCount > 0 && len(candidate.FileIDs) < candidate.ExpectedCount:
		candidate.Status = "partial_failed"
	default:
		candidate.Status = "completed"
	}
	if isSnapshotTerminal(state.Status) {
		if !sameSnapshotFinalOutcome(state, candidate) {
			g.snapshotStateMu.Unlock()
			ctx.String(409, "UploadSnapShotFinished conflicts with completed session")
			return
		}
		g.snapshotStateMu.Unlock()
	} else if err := g.persistSnapshotState(requestCtx, candidate); err != nil {
		g.snapshotStateMu.Unlock()
		ctx.String(500, "store UploadSnapShotFinished failed")
		return
	} else {
		g.storeSnapshotStateMemoryLocked(candidate)
		g.snapshotStateMu.Unlock()
	}
	if forwarded, err := g.forwardCascadeTaskNotification(requestCtx, cascadeTaskSnapshot, ctx.DeviceID, msg.DeviceID, msg.SessionID, ctx.Request.Body()); forwarded && err != nil {
		ctx.String(502, "forward UploadSnapShotFinished failed")
		return
	}
	respondErr := ctx.RespondString(200, "OK")
	ctx.Abort()
	if respondErr != nil {
		// 最终状态和级联转发必须先可靠完成，才能决定 200/5xx；
		// 写失败保留幂等终态，等待同一 SIP 事务重新进入 handler。
		slog.Error("acknowledge UploadSnapShotFinished failed", "device_id", ctx.DeviceID, "session_id", msg.SessionID, "err", respondErr)
	}
}

func sameSnapshotFinalOutcome(current, candidate SnapshotState) bool {
	if current.Status != candidate.Status || len(current.FileIDs) != len(candidate.FileIDs) {
		return false
	}
	counts := make(map[string]int, len(current.FileIDs))
	for _, fileID := range current.FileIDs {
		counts[fileID]++
	}
	for _, fileID := range candidate.FileIDs {
		if counts[fileID] == 0 {
			return false
		}
		counts[fileID]--
	}
	return true
}

func validSnapshotFileID(value, deviceID string) bool {
	if len(value) != 41 || !strings.HasPrefix(value, strings.TrimSpace(deviceID)+"02") {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	// 表 4 的第 23~39 位是精确到毫秒的生成时间，不能只校验为数字。
	_, err := sip.ParseGBTime("20060102150405.000", value[22:36]+"."+value[36:39])
	return err == nil
}

// buildSnapshotUploadURL 生成抓拍回传地址，避免硬编码固定地址。
// 通过路径参数携带 device/coverKey/session，兼容部分设备不接受 query 参数的场景。
func (g *GB28181API) buildSnapshotUploadURL(deviceID, coverKey, sessionID string) string {
	path := fmt.Sprintf("/gb28181/snapshot/%s/%s/%s", deviceID, coverKey, sessionID)
	if g.boot != nil {
		baseURL := strings.TrimSpace(g.boot.Media.GBSnapshotBaseURL)
		if baseURL != "" {
			return strings.TrimRight(baseURL, "/") + path
		}
		host := strings.TrimSpace(g.boot.Media.WebHookIP)
		if host == "" {
			host = ip.InternalIP()
		}
		port := g.boot.Server.HTTP.Port
		if port <= 0 {
			port = 15123
		}
		if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
			return strings.TrimRight(host, "/") + path
		}
		return "http://" + net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port)) + path
	}
	return "http://127.0.0.1:15123" + path
}
