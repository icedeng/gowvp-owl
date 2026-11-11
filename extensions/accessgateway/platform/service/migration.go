package service

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type MigrationRequest struct {
	SourceGatewayID *string        `json:"source_gateway_id"`
	TargetGatewayID *string        `json:"target_gateway_id"`
	Subjects        []core.Subject `json:"subjects"`
	ExpiresAt       time.Time      `json:"expires_at"`
}
type migrationDrainHost interface {
	DrainMigrationSubject(context.Context, string, string) (bool, error)
}

func (s *Service) CreateMigration(ctx context.Context, m core.Mutation, in MigrationRequest) (core.Result, error) {
	if len(in.Subjects) < 1 || len(in.Subjects) > 500 || pointerValue(in.SourceGatewayID) == pointerValue(in.TargetGatewayID) {
		return core.Result{}, core.Fail(400, "INVALID_ARGUMENT", "迁移须有不同源/目标及 1–500 个对象")
	}
	if r, e := s.replay(ctx, m, "migration", in); r != nil || e != nil {
		if r != nil {
			return *r, e
		}
		return core.Result{}, e
	}
	key, _ := digest([]string{m.Actor, m.Route, "migration", m.Key})
	hash, _ := requestDigest(m, in)
	var op store.Operation
	err := s.repo.Transaction(ctx, func(tx *gorm.DB) error {
		now, e := store.DBTime(tx)
		if e != nil {
			return e
		}
		if !in.ExpiresAt.After(now) || in.ExpiresAt.After(now.Add(24*time.Hour)) {
			return core.Fail(400, "INVALID_ARGUMENT", "迁移期限须在未来 24 小时内")
		}
		plan := store.Migration{Base: store.Base{ID: uuid.NewString()}, State: "draining", ExpiresAt: in.ExpiresAt.UTC(), Revision: 1, OperationID: uuid.NewString()}
		for _, p := range []struct {
			stable *string
			dest   **string
			target bool
		}{{in.SourceGatewayID, &plan.SourceGatewayID, false}, {in.TargetGatewayID, &plan.TargetGatewayID, true}} {
			if p.stable == nil {
				continue
			}
			var row store.Gateway
			if e = tx.Where("stable_id = ? AND deleted_at IS NULL", *p.stable).First(&row).Error; e != nil {
				return normalizeError(e)
			}
			if p.target {
				r := s.runtime.Snapshot(row.ID)
				if row.AdminState != "enabled" || r.HealthState != "ready" || row.ConfigRevision != row.AppliedRevision {
					return core.Fail(409, "TARGET_UNAVAILABLE", "迁移目标必须已启用并就绪")
				}
			}
			id := row.ID
			*p.dest = &id
		}
		// 按内部主键固定锁序，避免反向迁移与会话创建互相等待。
		gatewayIDs := []string{}
		if plan.SourceGatewayID != nil {
			gatewayIDs = append(gatewayIDs, *plan.SourceGatewayID)
		}
		if plan.TargetGatewayID != nil {
			gatewayIDs = append(gatewayIDs, *plan.TargetGatewayID)
		}
		sort.Strings(gatewayIDs)
		for _, pk := range gatewayIDs {
			if _, e = store.LockGateway(tx, pk); e != nil {
				return e
			}
		}
		if e = tx.Create(&plan).Error; e != nil {
			return e
		}
		seen := map[string]bool{}
		for _, subject := range in.Subjects {
			if subject.Type != "device" && subject.Type != "platform" || subject.ID == "" || seen[subject.Type+"/"+subject.ID] {
				return core.Fail(400, "INVALID_ARGUMENT", "迁移对象无效或重复")
			}
			seen[subject.Type+"/"+subject.ID] = true
			revision, e := subject.BindingRevision.Int64()
			if e != nil || revision < 1 {
				return core.Fail(400, "INVALID_ARGUMENT", "绑定版本无效")
			}
			var binding store.BindingFields
			table := "access_gateway_bindings"
			if subject.Type == "platform" {
				table = "access_gateway_platform_bindings"
			}
			e = tx.Table(table).Clauses(clause.Locking{Strength: "UPDATE"}).Where("subject_id = ?", subject.ID).Take(&binding).Error
			if store.IsNotFound(e) && subject.Type == "device" && plan.SourceGatewayID == nil { // 历史直连设备按现有内部 ID 确认，不创建虚假档案。
				var count int64
				if e = tx.Table("devices").Where("id = ?", subject.ID).Count(&count).Error; e != nil {
					return e
				}
				if count != 1 {
					return core.Fail(404, "NOT_FOUND", "设备不存在")
				}
				binding = store.BindingFields{SubjectID: subject.ID, IngressType: "direct", BindingRevision: 1}
			} else if e != nil {
				return normalizeError(e)
			}
			if !samePointer(binding.GatewayID, plan.SourceGatewayID) || binding.BindingRevision != revision {
				return core.Conflict("对象入口或绑定版本已改变")
			}
			active, _ := digest([]string{subject.Type, subject.ID})
			item := store.MigrationItem{Base: store.Base{ID: uuid.NewString()}, MigrationID: plan.ID, SubjectType: subject.Type, SubjectID: subject.ID, ExpectedBindingRevision: revision, State: "draining", ActiveSubjectKey: &active, Revision: 1}
			if e = tx.Create(&item).Error; e != nil {
				return e
			}
		}
		op = store.Operation{Base: store.Base{ID: plan.OperationID, CreatedAt: now, UpdatedAt: now}, UpdatedAt: now, GatewayID: plan.SourceGatewayID, Type: "migration", Status: "accepted", Step: "draining", OperationKeyHash: key, RequestHash: hash, RequestSnapshot: encode(map[string]string{"migration_id": plan.ID}), Result: encode(map[string]string{"migration_id": plan.ID}), Total: len(in.Subjects), Revision: 1, DeadlineAt: &plan.ExpiresAt}
		if op.GatewayID == nil {
			op.GatewayID = plan.TargetGatewayID
		}
		if e = tx.Create(&op).Error; e != nil {
			return e
		}
		return tx.Create(&store.Event{Base: store.Base{ID: uuid.NewString()}, GatewayID: op.GatewayID, Actor: m.Actor, EventType: "migration_created", Reason: "explicit_migration", TraceID: op.ID, Details: op.Result, OccurredAt: now}).Error
	})
	if err != nil {
		if r, e := s.replay(ctx, m, "migration", in); e == nil && r != nil {
			return *r, nil
		}
		return core.Result{}, normalizeError(err)
	}
	s.notify()
	return core.Result{Status: 202, Body: operation(op, "")}, nil
}
func samePointer(a, b *string) bool { return pointerValue(a) == pointerValue(b) }

func (s *Service) GetMigration(ctx context.Context, id string, q core.PageQuery) (map[string]any, error) {
	var plan store.Migration
	var items []store.MigrationItem
	counts := map[string]int64{}
	ids := []string{}
	stable := map[string]string{}
	err := s.repo.Read(ctx, func(db *gorm.DB) error {
		if e := db.Where("id = ?", id).First(&plan).Error; e != nil {
			return normalizeError(e)
		}
		if plan.SourceGatewayID != nil {
			ids = append(ids, *plan.SourceGatewayID)
		}
		if plan.TargetGatewayID != nil {
			ids = append(ids, *plan.TargetGatewayID)
		}
		var gateways []store.Gateway
		if len(ids) > 0 {
			if e := db.Where("id IN ?", ids).Find(&gateways).Error; e != nil {
				return e
			}
			for _, g := range gateways {
				stable[g.ID] = g.StableID
			}
		}
		var aggregates []struct {
			State string
			N     int64
		}
		if e := db.Model(&store.MigrationItem{}).Select("state, count(*) AS n").Where("migration_id = ?", id).Group("state").Scan(&aggregates).Error; e != nil {
			return e
		}
		for _, a := range aggregates {
			counts[a.State] = a.N
		}
		return db.Where("migration_id = ?", id).Order("id").Limit(q.Size).Offset((q.Page - 1) * q.Size).Find(&items).Error
	})
	if err != nil {
		return nil, err
	}
	var source, target *string
	if plan.SourceGatewayID != nil {
		v := stable[*plan.SourceGatewayID]
		source = &v
	}
	if plan.TargetGatewayID != nil {
		v := stable[*plan.TargetGatewayID]
		target = &v
	}
	views := []map[string]any{}
	for _, i := range items {
		views = append(views, map[string]any{"id": i.ID, "subject_type": i.SubjectType, "subject_id": i.SubjectID, "binding_revision": protocol.Number(i.ExpectedBindingRevision), "state": i.State, "error": i.Error, "revision": protocol.Number(i.Revision)})
	}
	return map[string]any{"id": plan.ID, "source": source, "target": target, "state": plan.State, "revision": protocol.Number(plan.Revision), "operation_id": plan.OperationID, "expires_at": plan.ExpiresAt, "counts": counts, "items": views, "page": q.Page, "size": q.Size}, nil
}
func (s *Service) CancelMigration(ctx context.Context, m core.Mutation, id string, req core.ReasonRequest) (core.Result, error) {
	if m.ExpectedRevision == nil || strings.TrimSpace(req.Reason) == "" || len(req.Reason) > 500 {
		return core.Result{}, core.Fail(400, "INVALID_ARGUMENT", "取消须提供版本和原因")
	}
	if r, e := s.replay(ctx, m, id, req); r != nil || e != nil {
		if r != nil {
			return *r, e
		}
		return core.Result{}, e
	}
	key, _ := digest([]string{m.Actor, m.Route, id, m.Key})
	hash, _ := requestDigest(m, req)
	var op store.Operation
	err := s.repo.Transaction(ctx, func(tx *gorm.DB) error {
		var plan store.Migration
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).First(&plan).Error; e != nil {
			return normalizeError(e)
		}
		if plan.Revision != *m.ExpectedRevision {
			return core.Conflict("迁移计划版本已改变")
		}
		now, e := store.DBTime(tx)
		if e != nil {
			return e
		}
		plan.State = "cancelled"
		plan.Revision++
		if e = tx.Save(&plan).Error; e != nil {
			return e
		}
		if e = tx.Model(&store.MigrationItem{}).Where("migration_id = ? AND active_subject_key IS NOT NULL", id).Updates(map[string]any{"state": "cancelled", "active_subject_key": nil, "revision": gorm.Expr("revision+1")}).Error; e != nil {
			return e
		}
		if e = tx.Model(&store.Operation{}).Where("id = ?", plan.OperationID).Updates(map[string]any{"status": "cancelled", "step": "cancelled", "updated_at": now}).Error; e != nil {
			return e
		}
		op = store.Operation{Base: store.Base{ID: uuid.NewString(), CreatedAt: now, UpdatedAt: now}, UpdatedAt: now, GatewayID: plan.SourceGatewayID, Type: "migration_cancel", Status: "succeeded", Step: "committed", OperationKeyHash: key, RequestHash: hash, RequestSnapshot: encode(req), Result: encode(map[string]string{"migration_id": id}), Revision: 1, Total: 1, Completed: 1}
		if op.GatewayID == nil {
			op.GatewayID = plan.TargetGatewayID
		}
		if e = tx.Create(&op).Error; e != nil {
			return e
		}
		return tx.Create(&store.Event{Base: store.Base{ID: uuid.NewString()}, GatewayID: op.GatewayID, Actor: m.Actor, EventType: "migration_cancelled", Reason: req.Reason, TraceID: op.ID, Details: op.Result, OccurredAt: now}).Error
	})
	if err != nil {
		if r, e := s.replay(ctx, m, id, req); e == nil && r != nil {
			return *r, nil
		}
		return core.Result{}, normalizeError(err)
	}
	return core.Result{Status: 202, Body: operation(op, "")}, nil
}
func (s *Service) advanceMigration(op store.Operation) {
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	defer cancel()
	var p struct {
		ID string `json:"migration_id"`
	}
	if json.Unmarshal([]byte(op.RequestSnapshot), &p) != nil {
		return
	}
	var plan store.Migration
	var items []store.MigrationItem
	if s.repo.Read(ctx, func(db *gorm.DB) error {
		if e := db.Where("id = ?", p.ID).First(&plan).Error; e != nil {
			return e
		}
		return db.Where("migration_id = ? AND active_subject_key IS NOT NULL", p.ID).Find(&items).Error
	}) != nil {
		return
	}
	if plan.SourceGatewayID != nil && time.Now().Before(plan.ExpiresAt) {
		if _, _, ok := s.runtime.OwnerLease(*plan.SourceGatewayID); !ok { // 已停用且无资源时允许直接进入等待注册。
			active, e := s.repo.ActiveSessions(ctx, *plan.SourceGatewayID)
			if e != nil || len(active) > 0 {
				return
			}
		}
	}
	for _, i := range items {
		if !time.Now().Before(plan.ExpiresAt) {
			break
		}
		if i.State == "waiting_register" {
			continue
		}
		drainer, ok := s.host.(migrationDrainHost)
		if !ok {
			continue
		}
		drained, err := drainer.DrainMigrationSubject(ctx, i.SubjectType, i.SubjectID)
		if err != nil || !drained {
			continue
		}
		_ = s.repo.Transaction(ctx, func(tx *gorm.DB) error {
			var current store.Migration
			if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", plan.ID).First(&current).Error; e != nil {
				return e
			}
			now, e := store.DBTime(tx)
			if e != nil {
				return e
			}
			if current.State == "cancelled" || !now.Before(current.ExpiresAt) {
				return nil
			}
			return tx.Model(&store.MigrationItem{}).Where("id = ? AND state = ? AND active_subject_key IS NOT NULL", i.ID, "draining").Updates(map[string]any{"state": "waiting_register", "revision": gorm.Expr("revision+1")}).Error
		})
	}
	_ = s.repo.Transaction(ctx, func(tx *gorm.DB) error {
		var current store.Migration
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", plan.ID).First(&current).Error; e != nil {
			return e
		}
		if current.State == "cancelled" || current.State == "completed" || current.State == "expired" {
			return nil
		}
		now, e := store.DBTime(tx)
		if e != nil {
			return e
		}
		if !now.Before(current.ExpiresAt) {
			if e = tx.Model(&store.MigrationItem{}).Where("migration_id = ? AND active_subject_key IS NOT NULL", plan.ID).Updates(map[string]any{"state": "expired", "active_subject_key": nil, "revision": gorm.Expr("revision+1")}).Error; e != nil {
				return e
			}
			current.State = "expired"
		} else {
			var draining int64
			if e = tx.Model(&store.MigrationItem{}).Where("migration_id = ? AND state = ?", plan.ID, "draining").Count(&draining).Error; e != nil {
				return e
			}
			if draining > 0 {
				current.State = "draining"
			} else {
				current.State = "waiting_register"
			}
		}
		var active, completed int64
		if e = tx.Model(&store.MigrationItem{}).Where("migration_id = ? AND active_subject_key IS NOT NULL", plan.ID).Count(&active).Error; e != nil {
			return e
		}
		if e = tx.Model(&store.MigrationItem{}).Where("migration_id = ? AND state = ?", plan.ID, "migrated").Count(&completed).Error; e != nil {
			return e
		}
		status := "waiting_external"
		if active == 0 {
			status = "succeeded"
			if current.State == "expired" {
				status = "failed"
			} else {
				current.State = "completed"
			}
		}
		if current.State != plan.State {
			current.Revision++
		}
		if e = tx.Save(&current).Error; e != nil {
			return e
		}
		return tx.Model(&store.Operation{}).Where("id = ?", op.ID).Updates(map[string]any{"status": status, "step": current.State, "completed": completed, "updated_at": now, "revision": gorm.Expr("revision+1")}).Error
	})
}
