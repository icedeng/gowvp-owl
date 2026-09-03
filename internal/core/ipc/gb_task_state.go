package ipc

import (
	"context"
	"errors"
	"time"

	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GBTaskStateRecord 持久化 GB/T 28181 长周期任务的会话关联与状态快照。
type GBTaskStateRecord struct {
	ID        uint64   `gorm:"primaryKey" json:"id"`
	Kind      string   `gorm:"column:kind;notNull;uniqueIndex:idx_gb_task_state_identity,priority:1;index:idx_gb_task_state_updated,priority:1" json:"kind"`
	DeviceID  string   `gorm:"column:device_id;notNull;uniqueIndex:idx_gb_task_state_identity,priority:2" json:"device_id"`
	SessionID string   `gorm:"column:session_id;notNull;uniqueIndex:idx_gb_task_state_identity,priority:3" json:"session_id"`
	Payload   string   `gorm:"column:payload;notNull;type:text" json:"payload"`
	UpdatedAt orm.Time `gorm:"column:updated_at;notNull;index:idx_gb_task_state_updated,priority:2" json:"updated_at"`
}

func (*GBTaskStateRecord) TableName() string { return "gb_task_states" }

// GBTaskStateStoreProvider 由支持国标长周期任务持久化的存储实现提供。
// 保持为可选接口，避免扩大既有 ipc.Storer 对测试桩和外部实现的要求。
type GBTaskStateStoreProvider interface {
	GBTaskState() *GBTaskStateStore
}

type GBTaskStateStore struct {
	db *gorm.DB
}

func NewGBTaskStateStore(db *gorm.DB) *GBTaskStateStore {
	return &GBTaskStateStore{db: db}
}

func (s *GBTaskStateStore) Save(ctx context.Context, record *GBTaskStateRecord) error {
	if s == nil || s.db == nil || record == nil || record.Kind == "" || record.DeviceID == "" || record.SessionID == "" {
		return errors.New("invalid GB task state")
	}
	if record.UpdatedAt.Time.IsZero() {
		record.UpdatedAt = orm.Time{Time: time.Now()}
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "kind"}, {Name: "device_id"}, {Name: "session_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"payload", "updated_at"}),
	}).Create(record).Error
}

func (s *GBTaskStateStore) Load(ctx context.Context, kind, deviceID, sessionID string) (*GBTaskStateRecord, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, errors.New("GB task state store is unavailable")
	}
	var record GBTaskStateRecord
	err := s.db.WithContext(ctx).
		Where("kind = ? AND device_id = ? AND session_id = ?", kind, deviceID, sessionID).
		First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &record, true, nil
}

// List 返回指定任务类型最早更新的记录，供跨进程恢复工作器分批重放。
func (s *GBTaskStateStore) List(ctx context.Context, kind string, limit int) ([]GBTaskStateRecord, error) {
	if s == nil || s.db == nil || kind == "" {
		return nil, errors.New("GB task state store is unavailable")
	}
	if limit <= 0 {
		limit = 100
	}
	var records []GBTaskStateRecord
	err := s.db.WithContext(ctx).
		Where("kind = ?", kind).
		Order("updated_at ASC, id ASC").
		Limit(limit).
		Find(&records).Error
	return records, err
}

func (s *GBTaskStateStore) Delete(ctx context.Context, kind, deviceID, sessionID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.WithContext(ctx).
		Where("kind = ? AND device_id = ? AND session_id = ?", kind, deviceID, sessionID).
		Delete(new(GBTaskStateRecord)).Error
}

func (s *GBTaskStateStore) DeleteDevice(ctx context.Context, deviceID string) error {
	if s == nil || s.db == nil || deviceID == "" {
		return nil
	}
	return s.db.WithContext(ctx).Where("device_id = ?", deviceID).Delete(new(GBTaskStateRecord)).Error
}

// Cleanup 删除过期记录，并按任务类型只保留最近 limit 条。
func (s *GBTaskStateStore) Cleanup(ctx context.Context, kind string, cutoff time.Time, limit int) error {
	if s == nil || s.db == nil || kind == "" {
		return nil
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if !cutoff.IsZero() {
			if err := tx.Where("kind = ? AND updated_at <= ?", kind, cutoff).Delete(new(GBTaskStateRecord)).Error; err != nil {
				return err
			}
		}
		if limit <= 0 {
			return nil
		}
		candidates := tx.Model(new(GBTaskStateRecord)).
			Select("id").
			Where("kind = ?", kind).
			Order("updated_at DESC, id DESC").
			Offset(limit)
		overflow := tx.Table("(?) AS gb_task_state_overflow", candidates).Select("id")
		// 在同一条 DELETE 中重新计算溢出集合，避免先查询到旧 ID 后，
		// 并发刷新已经把该任务变成最新状态，清理器仍按陈旧快照误删。
		// 外层派生表同时兼容 MySQL 对修改目标表直接子查询的限制。
		return tx.Where("id IN (?)", overflow).Delete(new(GBTaskStateRecord)).Error
	})
}
