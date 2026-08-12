package ipc

import (
	"context"
	"sync"
	"time"

	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

const (
	DeviceHistoryHeartbeat = "heartbeat"
	DeviceHistoryRegister  = "register"
)

// DeviceHistoryConfig 控制设备心跳和注册历史的保留范围。
// 两个限制同时生效：每台设备超过条数上限，或记录超过天数上限，都会被清理。
type DeviceHistoryConfig struct {
	MaxRecords int `comment:"每台设备最多保留记录条数，0 表示不限制" json:"max_records"`
	MaxDays    int `comment:"最多保留天数，0 表示不限制" json:"max_days"`
}

// DeviceHistoryRecord 记录一次设备注册或心跳事件。
type DeviceHistoryRecord struct {
	ID              uint64   `gorm:"primaryKey" json:"id"`
	DeviceID        string   `gorm:"column:device_id;notNull;index:idx_device_history,priority:1" json:"device_id"`
	Kind            string   `gorm:"column:kind;notNull;index:idx_device_history,priority:2" json:"kind"`
	RecordedAt      orm.Time `gorm:"column:recorded_at;notNull;index:idx_device_history,priority:3" json:"recorded_at"`
	IntervalSeconds int      `gorm:"column:interval_seconds;notNull;default:0" json:"interval_seconds"`
	Address         string   `gorm:"column:address;notNull;default:''" json:"address"`
	Status          string   `gorm:"column:status;notNull;default:''" json:"status"`
}

func (*DeviceHistoryRecord) TableName() string { return "device_history_records" }

// DeviceHistoryStore 负责历史记录写入、查询和按配置清理。
type DeviceHistoryStore struct {
	db  *gorm.DB
	mu  sync.RWMutex
	cfg DeviceHistoryConfig
}

// NewDeviceHistoryStore 创建历史记录存储并执行表迁移。
func NewDeviceHistoryStore(db *gorm.DB, cfg DeviceHistoryConfig, autoMigrate bool) *DeviceHistoryStore {
	store := &DeviceHistoryStore{db: db, cfg: normalizeDeviceHistoryConfig(cfg)}
	if autoMigrate {
		if err := db.AutoMigrate(new(DeviceHistoryRecord)); err != nil {
			panic(err)
		}
	}
	return store
}

func normalizeDeviceHistoryConfig(cfg DeviceHistoryConfig) DeviceHistoryConfig {
	if cfg.MaxRecords < 0 {
		cfg.MaxRecords = 0
	}
	if cfg.MaxDays < 0 {
		cfg.MaxDays = 0
	}
	return cfg
}

func (s *DeviceHistoryStore) SetConfig(cfg DeviceHistoryConfig) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cfg = normalizeDeviceHistoryConfig(cfg)
	s.mu.Unlock()
}

func (s *DeviceHistoryStore) DeleteDevice(ctx context.Context, deviceID string) error {
	if s == nil || s.db == nil || deviceID == "" {
		return nil
	}
	return s.db.WithContext(ctx).Where("device_id = ?", deviceID).Delete(new(DeviceHistoryRecord)).Error
}

func (s *DeviceHistoryStore) Config() DeviceHistoryConfig {
	if s == nil {
		return DeviceHistoryConfig{}
	}
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	return cfg
}

// Record 写入一条历史，并根据当前配置清理旧记录。
func (s *DeviceHistoryStore) Record(ctx context.Context, deviceID, kind, address, status string, at time.Time) error {
	if s == nil || s.db == nil || deviceID == "" || (kind != DeviceHistoryHeartbeat && kind != DeviceHistoryRegister) {
		return nil
	}
	if at.IsZero() {
		at = time.Now()
	}
	var previous DeviceHistoryRecord
	interval := 0
	if err := s.db.WithContext(ctx).Where("device_id = ? AND kind = ?", deviceID, kind).Order("recorded_at DESC").First(&previous).Error; err == nil {
		interval = maxInt(0, int(at.Sub(previous.RecordedAt.Time).Seconds()))
	}
	record := &DeviceHistoryRecord{
		DeviceID: deviceID, Kind: kind, RecordedAt: orm.Time{Time: at},
		IntervalSeconds: interval, Address: address, Status: status,
	}
	if err := s.db.WithContext(ctx).Create(record).Error; err != nil {
		return err
	}
	return s.prune(ctx, deviceID, kind)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *DeviceHistoryStore) prune(ctx context.Context, deviceID, kind string) error {
	cfg := s.Config()
	query := s.db.WithContext(ctx).Where("device_id = ? AND kind = ?", deviceID, kind)
	if cfg.MaxDays > 0 {
		if err := query.Where("recorded_at < ?", time.Now().AddDate(0, 0, -cfg.MaxDays)).Delete(new(DeviceHistoryRecord)).Error; err != nil {
			return err
		}
	}
	if cfg.MaxRecords <= 0 {
		return nil
	}
	var overflow []DeviceHistoryRecord
	if err := s.db.WithContext(ctx).Where("device_id = ? AND kind = ?", deviceID, kind).Order("recorded_at DESC").Offset(cfg.MaxRecords).Find(&overflow).Error; err != nil {
		return err
	}
	if len(overflow) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(overflow))
	for _, item := range overflow {
		ids = append(ids, item.ID)
	}
	return s.db.WithContext(ctx).Where("id IN ?", ids).Delete(new(DeviceHistoryRecord)).Error
}

func (s *DeviceHistoryStore) List(ctx context.Context, deviceID, kind string, page orm.Pager) ([]*DeviceHistoryRecord, int64, error) {
	if s == nil || s.db == nil {
		return []*DeviceHistoryRecord{}, 0, nil
	}
	var items []*DeviceHistoryRecord
	total, err := orm.FindWithContext(ctx, s.db, &items, page, func(db *gorm.DB) *gorm.DB {
		return db.Where("device_id = ? AND kind = ?", deviceID, kind).Order("recorded_at DESC")
	})
	return items, total, err
}
