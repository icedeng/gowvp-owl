package owl

import (
	"context"
	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/internal/core/ipc"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

type DeviceIngressSummary struct {
	Type        string           `json:"type"`
	SubjectType string           `json:"subject_type"`
	GatewayID   *string          `json:"gateway_id"`
	GatewayName *string          `json:"gateway_name"`
	Revision    protocol.Decimal `json:"binding_revision"`
	Effective   bool             `json:"effective"`
}
type DeviceView struct {
	*ipc.Device
	Ingress DeviceIngressSummary `json:"ingress"`
}

func (h *Host) DeviceListContext(ctx context.Context, id string) (context.Context, error) {
	if id == "" {
		return ctx, nil
	}
	if id == "direct" {
		return ipc.WithDeviceListOptions(ctx, func(db *gorm.DB) *gorm.DB {
			return db.Where("NOT EXISTS (SELECT 1 FROM access_gateway_bindings AS b WHERE b.subject_id=devices.id AND b.ingress_type=?) AND NOT EXISTS (SELECT 1 FROM access_gateway_platform_bindings AS b WHERE b.subject_id=devices.id AND b.ingress_type=?)", "gateway", "gateway")
		}), nil
	}
	gateway, err := h.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return ipc.WithDeviceListOptions(ctx, func(db *gorm.DB) *gorm.DB {
		return db.Where("EXISTS (SELECT 1 FROM access_gateway_bindings AS b WHERE b.subject_id=devices.id AND b.gateway_id=?) OR EXISTS (SELECT 1 FROM access_gateway_platform_bindings AS b WHERE b.subject_id=devices.id AND b.gateway_id=?)", gateway.ID, gateway.ID)
	}), nil
}
func (h *Host) DeviceViews(ctx context.Context, devices []*ipc.Device) ([]DeviceView, error) {
	ids := make([]string, 0, len(devices))
	for _, d := range devices {
		ids = append(ids, d.ID)
	}
	bindings := []store.Binding{}
	if len(ids) > 0 {
		if err := h.db.WithContext(ctx).Where("subject_id IN ?", ids).Find(&bindings).Error; err != nil {
			return nil, err
		}
	}
	if len(ids) > 0 {
		var platforms []store.PlatformBinding
		if err := h.db.WithContext(ctx).Where("subject_id IN ?", ids).Find(&platforms).Error; err != nil {
			return nil, err
		}
		for _, platform := range platforms {
			bindings = append(bindings, store.Binding{Base: platform.Base, BindingFields: platform.BindingFields})
		}
	}
	byID := map[string]store.Binding{}
	pk := []string{}
	for _, b := range bindings {
		byID[b.SubjectID] = b
		if b.GatewayID != nil {
			pk = append(pk, *b.GatewayID)
		}
	}
	rows := []store.Gateway{}
	if len(pk) > 0 {
		if err := h.db.WithContext(ctx).Where("id IN ?", pk).Find(&rows).Error; err != nil {
			return nil, err
		}
	}
	gateways := map[string]store.Gateway{}
	for _, g := range rows {
		gateways[g.ID] = g
	}
	views := make([]DeviceView, 0, len(devices))
	for _, d := range devices {
		summary := DeviceIngressSummary{Type: "direct", SubjectType: subjectKind(d.DeviceID), Revision: "1", Effective: d.IsOnline}
		if b, ok := byID[d.ID]; ok {
			summary.Type = b.IngressType
			summary.Revision = protocol.Number(b.BindingRevision)
			if b.GatewayID != nil {
				g := gateways[*b.GatewayID]
				summary.GatewayID = &g.StableID
				summary.GatewayName = &g.Name
				summary.Effective = false
				if m := h.manager(); m != nil {
					summary.Effective = store.EffectiveBinding(b, m.Snapshot(g.ID)) && d.IsOnline
				}
			}
		}
		views = append(views, DeviceView{Device: d, Ingress: summary})
	}
	return views, nil
}

// DeleteIngress 与设备删除同事务执行，迁移/活动资源未收敛时保留档案与绑定。
func (h *Host) DeleteIngress(ctx context.Context, tx *gorm.DB, device *ipc.Device) error {
	var binding store.Binding
	kind, id := subjectKind(device.DeviceID), device.ID
	table := "access_gateway_bindings"
	if kind == "platform" {
		table = "access_gateway_platform_bindings"
	}
	err := tx.Table(table).Clauses(clause.Locking{Strength: "UPDATE"}).Where("subject_id = ?", device.ID).First(&binding).Error
	if store.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var n int64
	if err = tx.Model(&store.MigrationItem{}).Where("subject_type = ? AND subject_id = ? AND active_subject_key IS NOT NULL", kind, device.ID).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return core.Fail(409, "MIGRATION_ACTIVE", "请先取消或完成设备迁移")
	}
	if err = tx.Model(&store.Session{}).Where("subject_type = ? AND subject_id = ? AND state <> ?", kind, device.ID, "closed").Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return core.Fail(409, "RESOURCES_ACTIVE", "设备仍有网关资源待清理")
	}
	if err = tx.Create(&store.Event{Base: store.Base{ID: uuid.NewString()}, GatewayID: binding.GatewayID, SubjectType: &kind, SubjectID: &id, EventType: "binding_deleted", Actor: "device_delete", Reason: "device_deleted", TraceID: uuid.NewString(), Details: "{}", OccurredAt: time.Now().UTC()}).Error; err != nil {
		return err
	}
	return tx.Table(table).Where("id = ?", binding.ID).Delete(&store.Binding{}).Error
}
