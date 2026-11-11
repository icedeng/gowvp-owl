package owl

import (
	"context"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/internal/core/ipc"
)

func (h *Host) DrainMigrationSubject(ctx context.Context, kind, id string) (bool, error) {
	if h.sip == nil {
		return false, core.Fail(503, "DEPENDENCY_UNAVAILABLE", "国标服务未就绪")
	}
	h.transfers.Range(func(_, v any) bool {
		b := v.(*transferBridge)
		if b.row.SubjectType == kind && b.row.SubjectID == id {
			_ = b.Close(ctx)
		}
		return true
	})
	if kind == "platform" && h.isUpstreamPlatform(ctx, id) {
		if err := h.sip.DrainExternalPlatform(ctx, id); err != nil {
			return false, err
		}
	} else if kind == "device" || kind == "platform" {
		var device ipc.Device
		if e := h.db.WithContext(ctx).Select("device_id").Where("id = ?", id).First(&device).Error; e != nil {
			return false, e
		}
		drained, e := h.sip.DrainExternalSubject(ctx, device.DeviceID)
		if e != nil || !drained {
			return drained, e
		}
	} else {
		return false, core.Fail(400, "INVALID_ARGUMENT", "未知迁移对象类型")
	}
	// 对话丢失时依照持久化会话补偿，不能永久停留在排空状态。
	var sessions []store.Session
	if err := h.db.WithContext(ctx).Where("subject_type = ? AND subject_id = ? AND state <> ?", kind, id, "closed").Find(&sessions).Error; err != nil {
		return false, err
	}
	for _, session := range sessions {
		if err := h.StopGatewaySession(ctx, session.GatewayID, session.ID, true); err != nil {
			return false, err
		}
	}
	var count int64
	e := h.db.WithContext(ctx).Model(&store.Session{}).Where("subject_type = ? AND subject_id = ? AND state <> ?", kind, id, "closed").Count(&count).Error
	return count == 0, e
}

func (h *Host) isUpstreamPlatform(ctx context.Context, id string) bool {
	binding, err := h.platformBinding(ctx, id)
	return err == nil && binding.Role == "upstream"
}
