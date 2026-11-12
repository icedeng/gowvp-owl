package owl

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/ipc"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (h *Host) SetAlarmSink(sink func(context.Context, protocol.AlarmRecord, string, string) error) {
	h.mu.Lock()
	h.alarmSink = sink
	h.mu.Unlock()
}

// ReceiveAlarm 根据设备编码和同一网关持久绑定恢复归属；绝不创建或迁移设备。
func (h *Host) AlarmSpoolAllowed() bool { return h.sip == nil || h.sip.GatewayTextAlarmsAllowed() }

func (h *Host) ReceiveAlarm(ctx context.Context, pk string, a protocol.AlarmRecord) error {
	if !h.AlarmSpoolAllowed() {
		return fmt.Errorf("强制信令验签启用，报警须沿原有 SIP 验证路径接纳")
	}
	if err := protocol.ValidateAlarm(a); err != nil {
		return err
	}
	received, err := time.Parse(time.RFC3339Nano, a.ReceivedAt)
	if err != nil || received.After(time.Now().Add(30*time.Second)) {
		return fmt.Errorf("invalid alarm receipt time")
	}
	unlock := h.repo.LockGateways(pk)
	defer unlock()
	var device ipc.Device
	if err = h.db.WithContext(ctx).Where("device_id = ? AND id = ?", a.DeviceCode, a.SubjectID).First(&device).Error; err != nil {
		return err
	}
	binding, err := h.repo.Binding(ctx, device.ID)
	if err != nil {
		return err
	}
	revision, _ := a.BindingRevision.Int64()
	if binding.IngressType != "gateway" || binding.GatewayID == nil || *binding.GatewayID != pk || revision < 1 || revision > binding.BindingRevision {
		return fmt.Errorf("alarm does not belong to current gateway binding")
	}
	row, err := h.repo.GetPK(ctx, pk)
	if err != nil {
		return err
	}
	if row.AdminState != "enabled" && row.AdminState != "draining" {
		return fmt.Errorf("gateway disabled")
	}
	cid := ""
	if a.ChannelCode != "" && a.ChannelCode != a.DeviceCode && !(len(a.ChannelCode) == 10 && strings.HasPrefix(a.DeviceCode, a.ChannelCode)) {
		var ch ipc.Channel
		if err = h.db.WithContext(ctx).Where("did = ? AND channel_id = ?", device.ID, a.ChannelCode).First(&ch).Error; err != nil {
			return err
		}
		cid = ch.ID
	}
	h.mu.RLock()
	sink := h.alarmSink
	h.mu.RUnlock()
	if sink == nil {
		return fmt.Errorf("alarm event sink not ready")
	}
	// 迟到的报警只能补文字；收到时尚未配置取证也不能在重试后补拍当前现场。
	a.Historical = a.Historical || time.Since(received) > 15*time.Second
	ctx = event.WithAdmission(ctx, func(tx *gorm.DB) error {
		var current ipc.Device
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND device_id = ?", device.ID, a.DeviceCode).First(&current).Error; err != nil {
			return err
		}
		gateway, err := store.LockGateway(tx, pk)
		if err != nil {
			return err
		}
		if gateway.AdminState != "enabled" && gateway.AdminState != "draining" {
			return fmt.Errorf("gateway disabled before alarm commit")
		}
		var bound store.Binding
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("subject_id = ?", device.ID).First(&bound).Error; err != nil {
			return err
		}
		if bound.IngressType != "gateway" || bound.GatewayID == nil || *bound.GatewayID != pk || revision > bound.BindingRevision {
			return fmt.Errorf("alarm ingress changed before commit")
		}
		if cid != "" {
			var channel ipc.Channel
			if err := tx.Where("id = ? AND did = ? AND channel_id = ?", cid, device.ID, a.ChannelCode).First(&channel).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return sink(ctx, a, device.ID, cid)
}
