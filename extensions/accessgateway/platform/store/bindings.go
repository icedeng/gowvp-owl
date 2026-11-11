package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type RegistrationClaim struct {
	SubjectID      string
	PlatformRole   string
	Route          core.IngressRoute
	OwnerID        string
	NewDevice      bool
	Unregister     bool
	ExpiresSeconds int
}

// CommitRegistration 只能传入宿主注册事务；任何错误都会同时回滚设备状态与入口绑定。
// changed 供宿主清除旧入口的 SIP CSeq 基线，不能把不同入口的序号直接比较。
func CommitRegistration(tx *gorm.DB, claim RegistrationClaim) (Binding, bool, error) {
	return commitRegistration(tx, claim, "device")
}

// CommitPlatformRegistration 由上级 REGISTER 成功的回调提交；出站建连本身不构成认证。
func CommitPlatformRegistration(tx *gorm.DB, claim RegistrationClaim) (Binding, bool, error) {
	return commitRegistration(tx, claim, "platform")
}

func commitRegistration(tx *gorm.DB, claim RegistrationClaim, kind string) (binding Binding, changed bool, err error) {
	table := "access_gateway_bindings"
	if kind == "platform" {
		table = "access_gateway_platform_bindings"
	}
	now, e := DBTime(tx)
	if e != nil {
		return binding, false, e
	}
	route := claim.Route
	if route.IngressType != "direct" && route.IngressType != "gateway" {
		return binding, false, core.Fail(403, "INGRESS_INVALID", "入口类型无效")
	}
	var target *string
	if route.IngressType == "gateway" {
		target = &route.GatewayPK
		gateway, e := LockGateway(tx, route.GatewayPK)
		if e != nil {
			return binding, false, e
		}
		epoch, e := route.OwnerEpoch.Int64()
		if e != nil {
			return binding, false, e
		}
		if e = CheckOwner(tx, route.GatewayPK, claim.OwnerID, epoch); e != nil {
			return binding, false, e
		}
		if gateway.ConfigRevision != gateway.AppliedRevision || protocol.Number(gateway.ConfigRevision) != route.ConfigRevision {
			return binding, false, core.Fail(403, "CONFIG_CONFLICT", "注册入口配置尚未确认")
		}
		if gateway.AdminState != "enabled" && gateway.AdminState != "draining" {
			return binding, false, core.Fail(403, "GATEWAY_DISABLED", "入口网关未启用")
		}
	}
	e = tx.Table(table).Clauses(clause.Locking{Strength: "UPDATE"}).Where("subject_id = ?", claim.SubjectID).First(&binding).Error
	missing := errors.Is(e, gorm.ErrRecordNotFound)
	if e != nil && !missing {
		return binding, false, e
	}
	if missing {
		binding = Binding{Base: Base{ID: uuid.NewString()}, BindingFields: BindingFields{SubjectID: claim.SubjectID, IngressType: "direct", BindingRevision: 1, AuthenticatedAt: now}}
		if claim.NewDevice {
			binding.IngressType = route.IngressType
			binding.GatewayID = target
		}
	}
	same := binding.IngressType == route.IngressType && pointerEqual(binding.GatewayID, target)
	var item MigrationItem
	var plan Migration
	if !same {
		if claim.Unregister {
			return binding, false, core.Fail(403, "INGRESS_CONFLICT", "注销请求不属于当前入口")
		}
		query := tx.Table("access_gateway_migration_items AS i").Select("i.*").Joins("JOIN access_gateway_migrations AS m ON m.id = i.migration_id").Where("i.subject_type = ? AND i.subject_id = ? AND i.active_subject_key IS NOT NULL AND i.state = ? AND m.expires_at > ? AND m.state IN ?", kind, claim.SubjectID, "waiting_register", now, []string{"waiting_register", "draining"})
		if e = query.First(&item).Error; e != nil {
			if !IsNotFound(e) {
				return binding, false, e
			}
			var pending int64
			q := tx.Table("access_gateway_migration_items AS i").Joins("JOIN access_gateway_migrations AS m ON m.id = i.migration_id").Where("i.subject_type = ? AND i.subject_id = ? AND i.expected_binding_revision = ? AND i.active_subject_key IS NOT NULL AND i.state = ? AND m.expires_at > ? AND m.state = ?", kind, claim.SubjectID, binding.BindingRevision, "draining", now, "draining")
			if target == nil {
				q = q.Where("m.target_gateway_id IS NULL")
			} else {
				q = q.Where("m.target_gateway_id = ?", *target)
			}
			if e = q.Count(&pending).Error; e != nil {
				return binding, false, e
			}
			if pending > 0 {
				return binding, false, core.Fail(503, "MIGRATION_DRAINING", "原入口正在排空，请稍后重新注册")
			}
			return binding, false, core.Fail(403, "INGRESS_CONFLICT", "对象已属于其他入口，需显式迁移授权")
		}
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", item.MigrationID).First(&plan).Error; e != nil {
			return binding, false, e
		}
		if !pointerEqual(plan.SourceGatewayID, binding.GatewayID) || !pointerEqual(plan.TargetGatewayID, target) || item.ExpectedBindingRevision != binding.BindingRevision || !now.Before(plan.ExpiresAt) || plan.State == "cancelled" {
			return binding, false, core.Fail(403, "MIGRATION_CONFLICT", "迁移授权已过期或绑定版本改变")
		}
		var active int64
		if e = tx.Model(&Session{}).Where("subject_type = ? AND subject_id = ? AND state <> ?", kind, claim.SubjectID, "closed").Count(&active).Error; e != nil {
			return binding, false, e
		}
		if active != 0 {
			return binding, false, core.Fail(503, "MIGRATION_DRAINING", "原入口资源尚未回收")
		}
		binding.IngressType = route.IngressType
		binding.GatewayID = target
		binding.BindingRevision++
		changed = true
	}
	if target != nil {
		var gateway Gateway
		if e = tx.Where("id = ?", *target).First(&gateway).Error; e != nil {
			return binding, false, e
		}
		newBinding := missing || changed
		if gateway.AdminState == "draining" && newBinding {
			return binding, false, core.Fail(503, "GATEWAY_DRAINING", "网关排空时不接受新绑定")
		}
	}
	save := func() error {
		if kind == "platform" {
			role := claim.PlatformRole
			if role == "" {
				role = "upstream"
			}
			return tx.Save(&PlatformBinding{Base: binding.Base, BindingFields: binding.BindingFields, Role: role}).Error
		}
		return tx.Save(&binding).Error
	}
	if claim.Unregister {
		binding.RegisteredUntil = &now
		return binding, false, save()
	}
	if claim.ExpiresSeconds > 0 {
		until := now.Add(time.Duration(claim.ExpiresSeconds) * time.Second)
		binding.RegisteredUntil = &until
	}
	// 新代次必须重新通过 REGISTER，绑定版本递增使旧心跳/操作失效。
	if !missing && !changed && target != nil {
		epoch, e := route.ConnectionEpoch.Int64()
		if e != nil {
			return binding, false, e
		}
		if binding.LastGeneration == nil || *binding.LastGeneration != route.Generation || binding.LastConnectionEpoch == nil || *binding.LastConnectionEpoch != epoch || binding.LastFlowID == nil || *binding.LastFlowID != route.FlowID {
			binding.BindingRevision++
			changed = true
		}
	}
	binding.AuthenticatedAt = now
	if target != nil {
		epoch, e := route.ConnectionEpoch.Int64()
		if e != nil {
			return binding, false, e
		}
		binding.LastGeneration = &route.Generation
		binding.LastConnectionEpoch = &epoch
		binding.LastFlowID = &route.FlowID
	} else {
		binding.LastGeneration = nil
		binding.LastConnectionEpoch = nil
		binding.LastFlowID = nil
	}
	e = save()
	if e != nil {
		return binding, false, e
	}
	if item.ID != "" {
		item.State = "migrated"
		item.ActiveSubjectKey = nil
		item.Revision++
		if e = tx.Save(&item).Error; e != nil {
			return binding, false, e
		}
		event := Event{Base: Base{ID: uuid.NewString()}, GatewayID: target, Actor: "device_registration", SubjectType: stringPointer(kind), SubjectID: &claim.SubjectID, EventType: "binding_migrated", Reason: "authorized_registration", TraceID: plan.OperationID, Details: "{}", OccurredAt: now}
		if e = tx.Create(&event).Error; e != nil {
			return binding, false, e
		}
	}
	return binding, changed, nil
}
func pointerEqual(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
func stringPointer(s string) *string { return &s }
func (r *Repository) Binding(ctx context.Context, subjectID string) (Binding, error) {
	var row Binding
	e := r.db.WithContext(ctx).Where("subject_id = ?", subjectID).First(&row).Error
	if IsNotFound(e) {
		var platform PlatformBinding
		e = r.db.WithContext(ctx).Where("subject_id = ?", subjectID).First(&platform).Error
		row = Binding{Base: platform.Base, BindingFields: platform.BindingFields}
	}
	return row, e
}
func (r *Repository) CanRestoreDirect(ctx context.Context, subjectID string) (bool, error) {
	b, e := r.Binding(ctx, subjectID)
	if IsNotFound(e) {
		return true, nil
	}
	return e == nil && b.IngressType == "direct", e
}

// EffectiveBinding 对网关入口必须同时核对进程、连接代次和已认证的绑定版本。
func EffectiveBinding(binding Binding, snapshot core.RuntimeSnapshot) bool {
	if binding.RegisteredUntil != nil && !time.Now().Before(*binding.RegisteredUntil) {
		return false
	}
	if binding.IngressType == "direct" {
		return true
	}
	if snapshot.ConnectionState != "connected" || snapshot.HealthState != "ready" || binding.LastGeneration == nil || binding.LastConnectionEpoch == nil {
		return false
	}
	epoch, e := snapshot.Epoch.Int64()
	return e == nil && *binding.LastGeneration == snapshot.Generation && *binding.LastConnectionEpoch == epoch && snapshot.LastSeenAt != nil
}
