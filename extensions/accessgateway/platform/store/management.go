package store

import (
	"context"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"gorm.io/gorm"
)

func (r *Repository) Read(ctx context.Context, fn func(*gorm.DB) error) error {
	return fn(r.db.WithContext(ctx))
}

// References 必须在持有网关行锁的事务内调用。未确认回收的资源始终属于引用。
func References(tx *gorm.DB, id string) (map[string]any, error) {
	counts := map[string]any{}
	queries := []struct {
		name  string
		model any
		where string
		args  []any
	}{
		{"devices", &Binding{}, "gateway_id = ?", []any{id}},
		{"platforms", &PlatformBinding{}, "gateway_id = ?", []any{id}},
		{"sessions", &Session{}, "gateway_id = ? AND state <> ?", []any{id, "closed"}},
		{"operations", &Operation{}, "gateway_id = ? AND status IN ?", []any{id, []string{"accepted", "running", "waiting_external"}}},
		{"migrations", &Migration{}, "(source_gateway_id = ? OR target_gateway_id = ?) AND state IN ?", []any{id, id, []string{"pending", "draining", "waiting_register"}}},
	}
	for _, q := range queries {
		var count int64
		if err := tx.Model(q.model).Where(q.where, q.args...).Count(&count).Error; err != nil {
			return nil, err
		}
		counts[q.name] = count
	}
	return counts, nil
}
func RequireNoReferences(tx *gorm.DB, id string) error {
	counts, err := References(tx, id)
	if err != nil {
		return err
	}
	for _, v := range counts {
		if v.(int64) > 0 {
			e := core.Fail(409, "GATEWAY_REFERENCED", "网关仍被设备、平台、会话或操作引用")
			e.Details = counts
			return e
		}
	}
	return nil
}

func (r *Repository) Sessions(ctx context.Context, id string, q core.PageQuery) ([]Session, int64, error) {
	rows := []Session{}
	db := r.db.WithContext(ctx).Model(&Session{}).Where("gateway_id = ?", id)
	if q.Purpose != "" {
		db = db.Where("purpose = ?", q.Purpose)
	}
	if q.State != "" {
		db = db.Where("state = ?", q.State)
	}
	if q.SubjectID != "" {
		db = db.Where("subject_id = ?", q.SubjectID)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := db.Order("created_at DESC, id ASC").Offset((q.Page - 1) * q.Size).Limit(q.Size).Find(&rows).Error
	return rows, total, err
}
func (r *Repository) Events(ctx context.Context, id string, q core.PageQuery) ([]Event, int64, error) {
	rows := []Event{}
	db := r.db.WithContext(ctx).Model(&Event{}).Where("gateway_id = ? AND occurred_at >= ? AND occurred_at <= ?", id, q.Start, q.End)
	if q.Type != "" {
		db = db.Where("event_type = ?", q.Type)
	}
	if q.SubjectID != "" {
		db = db.Where("subject_id = ?", q.SubjectID)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := db.Order("occurred_at DESC, id ASC").Offset((q.Page - 1) * q.Size).Limit(q.Size).Find(&rows).Error
	return rows, total, err
}
