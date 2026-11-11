package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository struct {
	db    *gorm.DB
	mu    sync.Mutex
	gates map[string]*sync.Mutex
}

func New(db *gorm.DB) *Repository {
	return &Repository{db: db.Session(&gorm.Session{NowFunc: func() time.Time { return time.Now().UTC() }}), gates: map[string]*sync.Mutex{}}
}

// LockGateways 跨网关按内部 UUID 排序取锁，持锁期间禁止网络调用。
func (r *Repository) LockGateways(ids ...string) func() {
	sort.Strings(ids)
	locks := []*sync.Mutex{}
	r.mu.Lock()
	previous := ""
	for _, id := range ids {
		if id == "" || id == previous {
			continue
		}
		previous = id
		m := r.gates[id]
		if m == nil {
			m = &sync.Mutex{}
			r.gates[id] = m
		}
		locks = append(locks, m)
	}
	r.mu.Unlock()
	for _, m := range locks {
		m.Lock()
	}
	return func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}
}
func (r *Repository) Transaction(ctx context.Context, fn func(*gorm.DB) error) error {
	return r.db.WithContext(ctx).Transaction(fn)
}
func (r *Repository) Get(ctx context.Context, stableID string) (Gateway, error) {
	var v Gateway
	e := r.db.WithContext(ctx).Where("stable_id = ? AND deleted_at IS NULL", stableID).First(&v).Error
	return v, e
}
func (r *Repository) GetPK(ctx context.Context, id string) (Gateway, error) {
	var v Gateway
	e := r.db.WithContext(ctx).Where("id = ? AND deleted_at IS NULL", id).First(&v).Error
	return v, e
}
func (r *Repository) All(ctx context.Context) ([]Gateway, error) {
	v := []Gateway{}
	e := r.db.WithContext(ctx).Where("deleted_at IS NULL").Order("stable_id ASC").Find(&v).Error
	return v, e
}
func (r *Repository) Enabled(ctx context.Context) ([]Gateway, error) {
	v := []Gateway{}
	e := r.db.WithContext(ctx).Where("deleted_at IS NULL AND admin_state IN ?", []string{"enabled", "draining"}).Find(&v).Error
	return v, e
}
func (r *Repository) Credential(ctx context.Context, gatewayPK, id string) (Credential, error) {
	var v Credential
	e := r.db.WithContext(ctx).Where("id = ? AND gateway_id = ?", id, gatewayPK).First(&v).Error
	return v, e
}
func (r *Repository) Credentials(ctx context.Context, gatewayPK string) ([]Credential, error) {
	v := []Credential{}
	e := r.db.WithContext(ctx).Where("gateway_id = ?", gatewayPK).Find(&v).Error
	return v, e
}
func (r *Repository) Operation(ctx context.Context, gatewayPK, id string) (Operation, error) {
	var v Operation
	q := r.db.WithContext(ctx).Where("id = ?", id)
	if gatewayPK != "" {
		q = q.Where("gateway_id = ?", gatewayPK)
	}
	e := q.First(&v).Error
	return v, e
}
func (r *Repository) OperationByKey(ctx context.Context, key string) (Operation, error) {
	var v Operation
	e := r.db.WithContext(ctx).Where("operation_key_hash = ?", key).First(&v).Error
	return v, e
}
func (r *Repository) PendingOperations(ctx context.Context) ([]Operation, error) {
	v := []Operation{}
	e := r.db.WithContext(ctx).Where("status IN ?", []string{"accepted", "running", "waiting_external"}).Find(&v).Error
	return v, e
}
func (r *Repository) ActiveSessions(ctx context.Context, gatewayPK string) ([]Session, error) {
	v := []Session{}
	q := r.db.WithContext(ctx).Where("state <> ?", "closed")
	if gatewayPK != "" {
		q = q.Where("gateway_id = ?", gatewayPK)
	}
	e := q.Find(&v).Error
	return v, e
}
func (r *Repository) Session(ctx context.Context, gatewayPK, id string) (Session, error) {
	var v Session
	e := r.db.WithContext(ctx).Where("id = ? AND gateway_id = ?", id, gatewayPK).First(&v).Error
	return v, e
}
func (r *Repository) SaveSession(ctx context.Context, value *Session, ownerID string, epoch int64) error {
	unlock := r.LockGateways(value.GatewayID)
	defer unlock()
	return r.Transaction(ctx, func(tx *gorm.DB) error {
		if e := CheckOwner(tx, value.GatewayID, ownerID, epoch); e != nil {
			return e
		}
		// 流量采样由独立写入方更新，生命周期快照不得覆盖新采样。
		return tx.Omit("Traffic").Save(value).Error
	})
}
func (r *Repository) MarkOperation(ctx context.Context, op *Operation) error {
	return r.db.WithContext(ctx).Save(op).Error
}
func (r *Repository) AddEvent(ctx context.Context, event *Event) error {
	return r.db.WithContext(ctx).Create(event).Error
}
func LockGateway(tx *gorm.DB, id string) (Gateway, error) {
	var row Gateway
	e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND deleted_at IS NULL", id).First(&row).Error
	return row, e
}

type clockValue struct{ time.Time }

func (c *clockValue) Scan(v any) error {
	switch value := v.(type) {
	case time.Time:
		c.Time = value.UTC()
		return nil
	case []byte:
		return c.Scan(string(value))
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999-07", "2006-01-02 15:04:05.999999999"} {
			if parsed, e := time.Parse(layout, value); e == nil {
				c.Time = parsed.UTC()
				return nil
			}
		}
	}
	return fmt.Errorf("unsupported database clock value")
}
func (c clockValue) Value() (driver.Value, error) { return c.Time, nil }
func DBTime(tx *gorm.DB) (time.Time, error) {
	rows, e := tx.Raw("SELECT CURRENT_TIMESTAMP").Rows()
	if e != nil {
		return time.Time{}, e
	}
	defer rows.Close()
	if !rows.Next() {
		return time.Time{}, fmt.Errorf("database clock unavailable")
	}
	var clock clockValue
	if e = rows.Scan(&clock); e != nil {
		return time.Time{}, e
	}
	return clock.Time, nil
}
func (r *Repository) AcquireOwner(ctx context.Context, gatewayPK, ownerID string) (int64, error) {
	unlock := r.LockGateways(gatewayPK)
	defer unlock()
	var epoch int64
	e := r.Transaction(ctx, func(tx *gorm.DB) error {
		now, e := DBTime(tx)
		if e != nil {
			return e
		}
		expires := now.Add(45 * time.Second)
		result := tx.Model(&Gateway{}).Where("id = ? AND deleted_at IS NULL AND admin_state IN ? AND owner_epoch < ? AND (owner_id IS NULL OR owner_id = ? OR owner_expires_at <= ?)", gatewayPK, []string{"enabled", "draining"}, int64(1<<63-1), ownerID, now).Updates(map[string]any{"owner_id": ownerID, "owner_epoch": gorm.Expr("owner_epoch + 1"), "owner_expires_at": expires})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return core.Fail(409, "OWNER_CONFLICT", "网关运行态已由其他进程持有或已停用")
		}
		var row Gateway
		if e = tx.Where("id = ?", gatewayPK).First(&row).Error; e != nil {
			return e
		}
		epoch = row.OwnerEpoch
		return nil
	})
	return epoch, e
}
func (r *Repository) RenewOwner(ctx context.Context, gatewayPK, ownerID string, epoch int64) error {
	return r.Transaction(ctx, func(tx *gorm.DB) error {
		now, e := DBTime(tx)
		if e != nil {
			return e
		}
		result := tx.Model(&Gateway{}).Where("id = ? AND owner_id = ? AND owner_epoch = ? AND owner_expires_at > ? AND deleted_at IS NULL", gatewayPK, ownerID, epoch, now).Update("owner_expires_at", now.Add(45*time.Second))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return core.Fail(409, "STALE_EPOCH", "网关所有权租约已失效")
		}
		return nil
	})
}
func CheckOwner(tx *gorm.DB, gatewayPK, ownerID string, epoch int64) error {
	now, e := DBTime(tx)
	if e != nil {
		return e
	}
	var count int64
	if e = tx.Model(&Gateway{}).Where("id = ? AND owner_id = ? AND owner_epoch = ? AND owner_expires_at > ? AND deleted_at IS NULL", gatewayPK, ownerID, epoch, now).Count(&count).Error; e != nil {
		return e
	}
	if count != 1 {
		return core.Fail(409, "STALE_EPOCH", "网关所有权已失效")
	}
	return nil
}
func (r *Repository) ReleaseOwner(ctx context.Context, gatewayPK, ownerID string, epoch int64) error {
	return r.db.WithContext(ctx).Model(&Gateway{}).Where("id = ? AND owner_id = ? AND owner_epoch = ?", gatewayPK, ownerID, epoch).Updates(map[string]any{"owner_id": nil, "owner_expires_at": nil}).Error
}
func (r *Repository) GatewayRuntimeUpdate(ctx context.Context, gatewayPK, ownerID string, epoch int64, updates map[string]any) error {
	return r.Transaction(ctx, func(tx *gorm.DB) error {
		if e := CheckOwner(tx, gatewayPK, ownerID, epoch); e != nil {
			return e
		}
		return tx.Model(&Gateway{}).Where("id = ? AND owner_id = ? AND owner_epoch = ?", gatewayPK, ownerID, epoch).Updates(updates).Error
	})
}
func IsNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }
