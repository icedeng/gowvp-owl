package gbs

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
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

type deviceControlUpgradeRequest struct {
	XMLName    xml.Name `xml:"Control"`
	CmdType    string   `xml:"CmdType"`
	SN         int      `xml:"SN"`
	DeviceID   string   `xml:"DeviceID"`
	DeviceInfo struct {
		Firmware     string `xml:"Firmware"`
		FileURL      string `xml:"FileURL"`
		Manufacturer string `xml:"Manufacturer"`
		SessionID    string `xml:"SessionID,omitempty"`
	} `xml:"DeviceUpgrade"`
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
	req := deviceControlUpgradeRequest{
		CmdType:  ptzCmdTypeDeviceControl,
		SN:       sn,
		DeviceID: in.ChannelID,
	}
	req.DeviceInfo.Firmware = strings.TrimSpace(in.Firmware)
	req.DeviceInfo.FileURL = strings.TrimSpace(in.FileURL)
	req.DeviceInfo.Manufacturer = strings.TrimSpace(in.Manufacturer)
	req.DeviceInfo.SessionID = sessionID

	body, err := sip.XMLEncode(req)
	if err != nil {
		return nil, err
	}

	waitKey := fmt.Sprintf("%s:%d", in.DeviceID, sn)
	pending := &pendingDeviceControl{wait: make(chan *deviceControlResponse, 1)}
	g.pendingDeviceControl.Store(waitKey, pending)
	defer g.pendingDeviceControl.Delete(waitKey)

	tx, err := g.svr.wrapRequestContext(ctx, ch, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		return nil, err
	}
	if _, err = sipResponseContext(ctx, tx); err != nil {
		return nil, err
	}

	timer := time.NewTimer(in.Timeout)
	defer timer.Stop()
	select {
	case resp := <-pending.wait:
		result := strings.ToUpper(strings.TrimSpace(resp.Result))
		if result == "" {
			result = "OK"
		}
		if result != "OK" {
			g.storeUpgradeState(UpgradeState{
				SN: sn, DeviceID: in.DeviceID, ChannelID: in.ChannelID, SessionID: sessionID,
				Status: "rejected", Result: result, Firmware: strings.TrimSpace(in.Firmware), UpdatedAt: time.Now(),
			})
			return nil, fmt.Errorf("device upgrade failed: %s", resp.Result)
		}
		g.storeUpgradeState(UpgradeState{
			SN: sn, DeviceID: in.DeviceID, ChannelID: in.ChannelID, SessionID: sessionID,
			Status: "accepted", Result: result, Firmware: strings.TrimSpace(in.Firmware), UpdatedAt: time.Now(),
		})
		return &UpgradeOutput{
			SN: sn, DeviceID: in.DeviceID, Channel: in.ChannelID, SessionID: sessionID, Result: result,
		}, nil
	case <-timer.C:
		return nil, errors.New("wait device upgrade response timeout")
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.serviceDone():
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
	if g == nil || strings.TrimSpace(state.DeviceID) == "" || strings.TrimSpace(state.SessionID) == "" {
		return
	}
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now()
	}
	g.upgradeStateMu.Lock()
	if g.upgradeStates == nil {
		g.upgradeStates = make(map[string]UpgradeState)
	}
	key := upgradeStateKey(state.DeviceID, state.SessionID)
	if current, ok := g.upgradeStates[key]; ok {
		// DeviceUpgradeResult 是标准定义的最终通知，优先级高于控制应答。
		// 防止通知先到、迟到的 accepted/rejected 再覆盖 completed/failed。
		if isUpgradeFinal(current.Status) && !isUpgradeFinal(state.Status) {
			g.upgradeStateMu.Unlock()
			return
		}
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
	g.upgradeStateMu.Unlock()
}

func isUpgradeFinal(status string) bool {
	return status == "completed" || status == "failed"
}

// UpgradeState 返回指定设备和会话的最新升级状态。
func (g *GB28181API) UpgradeState(deviceID, sessionID string) (UpgradeState, bool) {
	if g == nil {
		return UpgradeState{}, false
	}
	g.upgradeStateMu.Lock()
	defer g.upgradeStateMu.Unlock()
	key := upgradeStateKey(deviceID, sessionID)
	state, ok := g.upgradeStates[key]
	if ok && runtimeStateExpired(state.UpdatedAt, time.Now(), upgradeStateTTL) {
		delete(g.upgradeStates, key)
		return UpgradeState{}, false
	}
	return state, ok
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
	msg.Result = strings.TrimSpace(msg.Result)
	if msg.DeviceID == "" || msg.Result == "" || validateGBSessionID(msg.SessionID) != nil {
		ctx.String(400, "invalid DeviceUpgradeResult notification")
		return
	}
	if msg.DeviceID != ctx.DeviceID {
		if _, ok := g.svr.memoryStorer.GetChannel(ctx.DeviceID, msg.DeviceID); !ok {
			ctx.String(400, "DeviceUpgradeResult target mismatch")
			return
		}
	}
	state, ok := g.UpgradeState(ctx.DeviceID, msg.SessionID)
	if !ok {
		state = UpgradeState{DeviceID: ctx.DeviceID, ChannelID: msg.DeviceID, SessionID: msg.SessionID}
	}
	state.SN = msg.SN
	state.Result = msg.Result
	state.Firmware = strings.TrimSpace(msg.Firmware)
	state.FailedReason = strings.TrimSpace(msg.FailedReason)
	state.UpdatedAt = time.Now()
	if strings.EqualFold(msg.Result, "OK") {
		state.Status = "completed"
	} else {
		state.Status = "failed"
	}
	g.storeUpgradeState(state)
	ctx.String(200, "OK")
}
