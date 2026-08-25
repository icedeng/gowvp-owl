package gbs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/netpulse/ip"
)

const (
	maxSnapshotStates = 1024
	snapshotStateTTL  = 7 * 24 * time.Hour
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
	CmdType   string   `xml:"CmdType"`
	SN        int      `xml:"SN"`
	DeviceID  string   `xml:"DeviceID"`
	SessionID string   `xml:"SessionID"`
	FileIDs   []string `xml:"SnapShotList>SnapShotFileID"`
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

	sn := int32(g.nextControlSN())
	sessionID := sip.RandString(32)
	g.storeSnapshotState(SnapshotState{
		DeviceID: deviceID, ChannelID: targetID, CoverKey: coverKey, SessionID: sessionID,
		Status: "pending", ExpectedCount: 1, UpdatedAt: time.Now(),
	})
	body := NewDeviceConfig(targetID).SetSN(sn).SetSnapShotConfig(&SnapShot{
		SnapNum:   1,
		Interval:  1,
		UploadURL: g.buildSnapshotUploadURL(deviceID, coverKey, sessionID),
		SessionID: sessionID,
	}).Marshal()

	waitKey := buildPendingDeviceConfigKey(deviceID, int(sn))
	pending := &pendingDeviceConfig{wait: make(chan *DeviceConfigResponse, 1)}
	g.pendingDeviceConfig.Store(waitKey, pending)
	defer g.pendingDeviceConfig.Delete(waitKey)

	tx, err := g.svr.wrapRequestContext(ctx, target, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		g.deleteSnapshotState(deviceID, sessionID)
		return nil, err
	}
	if _, err = sipResponseContext(ctx, tx); err != nil {
		g.transitionSnapshotState(deviceID, sessionID, "failed")
		return nil, err
	}

	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	select {
	case resp := <-pending.wait:
		if strings.ToUpper(strings.TrimSpace(resp.Result)) == "OK" || strings.TrimSpace(resp.Result) == "" {
			state, exists := g.transitionSnapshotState(deviceID, sessionID, "accepted")
			if !exists {
				return nil, fmt.Errorf("snapshot session disappeared before acceptance")
			}
			return &state, nil
		}
		g.transitionSnapshotState(deviceID, sessionID, "rejected")
		return nil, fmt.Errorf("snapshot config failed: %s", resp.Result)
	case <-g.serviceDone():
		g.transitionSnapshotState(deviceID, sessionID, "cancelled")
		return nil, ErrServiceStopped
	case <-ctx.Done():
		g.transitionSnapshotState(deviceID, sessionID, "cancelled")
		return nil, ctx.Err()
	case <-timer.C:
		g.transitionSnapshotState(deviceID, sessionID, "response_timeout")
		return nil, fmt.Errorf("wait snapshot response timeout")
	}
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
	if g == nil || strings.TrimSpace(state.DeviceID) == "" || strings.TrimSpace(state.SessionID) == "" {
		return
	}
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now()
	}
	state.FileIDs = append([]string(nil), state.FileIDs...)
	g.snapshotStateMu.Lock()
	g.storeSnapshotStateLocked(state)
	g.snapshotStateMu.Unlock()
}

func (g *GB28181API) storeSnapshotStateLocked(state SnapshotState) {
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
	if g == nil {
		return
	}
	g.snapshotStateMu.Lock()
	delete(g.snapshotStates, snapshotStateKey(deviceID, sessionID))
	g.snapshotStateMu.Unlock()
}

func (g *GB28181API) transitionSnapshotState(deviceID, sessionID, status string) (SnapshotState, bool) {
	if g == nil {
		return SnapshotState{}, false
	}
	g.snapshotStateMu.Lock()
	defer g.snapshotStateMu.Unlock()
	key := snapshotStateKey(deviceID, sessionID)
	state, ok := g.snapshotStates[key]
	if !ok || runtimeStateExpired(state.UpdatedAt, time.Now(), snapshotStateTTL) {
		delete(g.snapshotStates, key)
		return SnapshotState{}, false
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
	g.storeSnapshotStateLocked(state)
	state.FileIDs = append([]string(nil), state.FileIDs...)
	return state, true
}

func (g *GB28181API) SnapshotState(deviceID, sessionID string) (SnapshotState, bool) {
	if g == nil {
		return SnapshotState{}, false
	}
	g.snapshotStateMu.Lock()
	defer g.snapshotStateMu.Unlock()
	key := snapshotStateKey(deviceID, sessionID)
	state, ok := g.snapshotStates[key]
	if ok && runtimeStateExpired(state.UpdatedAt, time.Now(), snapshotStateTTL) {
		delete(g.snapshotStates, key)
		return SnapshotState{}, false
	}
	state.FileIDs = append([]string(nil), state.FileIDs...)
	return state, ok
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
	state, ok := g.SnapshotState(deviceID, sessionID)
	if !ok {
		return fmt.Errorf("snapshot session not found")
	}
	if state.CoverKey != "" && strings.TrimSpace(coverKey) != state.CoverKey {
		return fmt.Errorf("snapshot cover key mismatch")
	}
	return nil
}

func (g *GB28181API) MarkSnapshotUploaded(deviceID, sessionID string) {
	if g == nil {
		return
	}
	key := snapshotStateKey(deviceID, sessionID)
	now := time.Now()
	g.snapshotStateMu.Lock()
	state, ok := g.snapshotStates[key]
	if !ok {
		g.snapshotStateMu.Unlock()
		return
	}
	if runtimeStateExpired(state.UpdatedAt, now, snapshotStateTTL) {
		delete(g.snapshotStates, key)
		g.snapshotStateMu.Unlock()
		return
	}
	state.ReceivedCount++
	if !isSnapshotTerminal(state.Status) {
		state.Status = "uploading"
	}
	state.UpdatedAt = now
	g.storeSnapshotStateLocked(state)
	g.snapshotStateMu.Unlock()
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
	if err := g.requireGBVersionAtLeast(ctx.DeviceID, gbVersion2022, "图像抓拍完成通知(A.2.5.7)"); err != nil {
		ctx.String(400, err.Error())
		return
	}
	var msg snapshotFinishedNotify
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	msg.SessionID = strings.TrimSpace(msg.SessionID)
	if msg.DeviceID == "" || validateGBSessionID(msg.SessionID) != nil {
		ctx.String(400, "invalid UploadSnapShotFinished notification")
		return
	}
	if msg.DeviceID != ctx.DeviceID {
		if _, ok := g.svr.memoryStorer.GetChannel(ctx.DeviceID, msg.DeviceID); !ok {
			ctx.String(400, "UploadSnapShotFinished target mismatch")
			return
		}
	}
	fileIDs := make([]string, 0, len(msg.FileIDs))
	seen := make(map[string]struct{}, len(msg.FileIDs))
	for _, fileID := range msg.FileIDs {
		fileID = strings.TrimSpace(fileID)
		if !validSnapshotFileID(fileID, msg.DeviceID) {
			continue
		}
		if _, ok := seen[fileID]; !ok && len(fileIDs) < 10 {
			seen[fileID] = struct{}{}
			fileIDs = append(fileIDs, fileID)
		}
	}
	g.snapshotStateMu.Lock()
	state, ok := g.snapshotStates[snapshotStateKey(ctx.DeviceID, msg.SessionID)]
	now := time.Now()
	if ok && !runtimeStateExpired(state.UpdatedAt, now, snapshotStateTTL) && strings.TrimSpace(state.ChannelID) != "" && strings.TrimSpace(state.ChannelID) != msg.DeviceID {
		g.snapshotStateMu.Unlock()
		ctx.String(400, "UploadSnapShotFinished session target mismatch")
		return
	}
	if !ok || runtimeStateExpired(state.UpdatedAt, now, snapshotStateTTL) {
		state = SnapshotState{DeviceID: ctx.DeviceID, ChannelID: msg.DeviceID, SessionID: msg.SessionID}
	}
	state.FileIDs = fileIDs
	state.UpdatedAt = now
	switch {
	case len(state.FileIDs) == 0:
		state.Status = "failed"
	case state.ExpectedCount > 0 && len(state.FileIDs) < state.ExpectedCount:
		state.Status = "partial_failed"
	default:
		state.Status = "completed"
	}
	g.storeSnapshotStateLocked(state)
	g.snapshotStateMu.Unlock()
	ctx.String(200, "OK")
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
	return true
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
		return fmt.Sprintf("http://%s:%d%s", host, port, path)
	}
	return "http://127.0.0.1:15123" + path
}
