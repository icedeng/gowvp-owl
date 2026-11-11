package gbs

import (
	"fmt"
	"strings"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

// RecordQueryDeviceError 表示设备已明确拒绝录像查询，区别于未收到响应的超时。
type RecordQueryDeviceError struct {
	DeviceID string
	SN       int
	Reason   string
}

func (e *RecordQueryDeviceError) Error() string {
	if e.Reason == "" {
		return "设备录像查询失败（Result=ERROR）"
	}
	return fmt.Sprintf("设备录像查询失败：%s", e.Reason)
}

// 海康等固件查询失败时只返回 Result/Reason，不携带成功响应的 Name、SumNum。
// 此兼容分支只接纳明确的失败信封；成功响应仍使用原有完整校验。
func (g *GB28181API) handleRecordInfoFailure(ctx *sip.Context, msg *MessageRecordInfoResponse) {
	rules := []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		{name: "Result", minOccurs: 1, maxOccurs: 1},
		{name: "Reason", maxOccurs: 1, maxRunes: 1024},
	}
	if err := validateDeviceMANSCDPStructure(ctx.Request.Body(), g.getDeviceGBProtocolVersion(ctx.DeviceID), "Response", "RecordInfo", rules); err != nil {
		ctx.String(400, err.Error())
		return
	}
	msg.CmdType = strings.TrimSpace(msg.CmdType)
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	if !strings.EqualFold(msg.CmdType, "RecordInfo") || msg.SN <= 0 || !isGBDeviceIdentifier(msg.DeviceID) ||
		msg.Result == nil || !strings.EqualFold(strings.TrimSpace(*msg.Result), "ERROR") {
		ctx.String(400, "invalid RecordInfo failure response")
		return
	}
	if err := g.validateAuthenticatedResponseTarget(ctx, msg.DeviceID); err != nil {
		ctx.String(403, err.Error())
		return
	}
	key, expectedTarget := g.recordResponseTarget(ctx, msg)
	if expectedTarget != "" && msg.DeviceID != expectedTarget && msg.DeviceID != strings.TrimSpace(ctx.DeviceID) {
		ctx.String(400, "RecordInfo failure target mismatch")
		return
	}
	entry := g.recordResponses.Entry(key)
	if err := ctx.RespondString(200, "OK"); err != nil {
		ctx.Log.Error("respond RecordInfo failure", "err", err, "sn", msg.SN, "target_id", msg.DeviceID)
		return
	}
	if key == "" || entry == nil {
		ctx.Log.Warn("ignore unassociated RecordInfo failure", "sn", msg.SN, "target_id", msg.DeviceID)
		return
	}
	unlock, err := g.lockAdmittedInboundDeviceStateCommit(ctx)
	if err != nil {
		return
	}
	defer unlock()
	reason := ""
	if msg.Reason != nil {
		reason = strings.TrimSpace(*msg.Reason)
	}
	if g.recordResponses.AbortEntry(key, entry, &RecordQueryDeviceError{DeviceID: msg.DeviceID, SN: msg.SN, Reason: reason}) {
		ctx.Log.Warn("device rejected RecordInfo query", "sn", msg.SN, "target_id", msg.DeviceID, "reason", reason)
	}
}
