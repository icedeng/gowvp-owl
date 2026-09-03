package ipc

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GBCascadeTaskRouteRecord 持久化 2022 升级/抓拍级联任务的上下游路由。
type GBCascadeTaskRouteRecord struct {
	ID                  uint64   `gorm:"primaryKey" json:"id"`
	RouteKey            string   `gorm:"column:route_key;size:64;notNull;uniqueIndex" json:"route_key"`
	UpstreamKey         string   `gorm:"column:upstream_key;size:64;notNull;uniqueIndex" json:"upstream_key"`
	Kind                string   `gorm:"column:kind;size:16;notNull" json:"kind"`
	PlatformName        string   `gorm:"column:platform_name;size:128;notNull" json:"platform_name"`
	DownstreamDeviceID  string   `gorm:"column:downstream_device_id;size:128;notNull;index" json:"downstream_device_id"`
	DownstreamSessionID string   `gorm:"column:downstream_session_id;size:128;notNull" json:"downstream_session_id"`
	ExposedID           string   `gorm:"column:exposed_id;size:128;notNull" json:"exposed_id"`
	UpstreamSessionID   string   `gorm:"column:upstream_session_id;size:128;notNull" json:"upstream_session_id"`
	Payload             string   `gorm:"column:payload;notNull;type:text" json:"payload"`
	UpdatedAt           orm.Time `gorm:"column:updated_at;notNull;index" json:"updated_at"`
}

func (*GBCascadeTaskRouteRecord) TableName() string { return "gb_cascade_task_routes" }

// GBCascadeTaskRouteStoreProvider 由支持级联任务路由持久化的存储实现提供。
type GBCascadeTaskRouteStoreProvider interface {
	GBCascadeTaskRoute() *GBCascadeTaskRouteStore
}

type GBCascadeTaskRouteStore struct {
	db *gorm.DB
}

func NewGBCascadeTaskRouteStore(db *gorm.DB) *GBCascadeTaskRouteStore {
	return &GBCascadeTaskRouteStore{db: db}
}

func GBCascadeTaskRouteKey(kind, deviceID, sessionID string) string {
	return gbCascadeTaskRouteIdentityKey("downstream", kind, deviceID, sessionID)
}

func GBCascadeTaskUpstreamKey(kind, platformName, exposedID, sessionID string) string {
	return gbCascadeTaskRouteIdentityKey("upstream", kind, platformName, exposedID, sessionID)
}

func gbCascadeTaskRouteIdentityKey(parts ...string) string {
	digest := sha256.New()
	for _, part := range parts {
		_, _ = digest.Write([]byte(strings.TrimSpace(part)))
		_, _ = digest.Write([]byte{0})
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func (s *GBCascadeTaskRouteStore) Save(ctx context.Context, record *GBCascadeTaskRouteRecord) error {
	if s == nil || s.db == nil || record == nil || record.Kind == "" || record.PlatformName == "" ||
		record.DownstreamDeviceID == "" || record.DownstreamSessionID == "" || record.ExposedID == "" || record.UpstreamSessionID == "" {
		return errors.New("invalid GB cascade task route")
	}
	record.RouteKey = GBCascadeTaskRouteKey(record.Kind, record.DownstreamDeviceID, record.DownstreamSessionID)
	record.UpstreamKey = GBCascadeTaskUpstreamKey(record.Kind, record.PlatformName, record.ExposedID, record.UpstreamSessionID)
	if record.UpdatedAt.Time.IsZero() {
		record.UpdatedAt = orm.Time{Time: time.Now()}
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "route_key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"upstream_key", "kind", "platform_name", "downstream_device_id", "downstream_session_id",
			"exposed_id", "upstream_session_id", "payload", "updated_at",
		}),
	}).Create(record).Error
}

func (s *GBCascadeTaskRouteStore) LoadDownstream(ctx context.Context, kind, deviceID, sessionID string) (*GBCascadeTaskRouteRecord, bool, error) {
	return s.load(ctx, "route_key", GBCascadeTaskRouteKey(kind, deviceID, sessionID))
}

func (s *GBCascadeTaskRouteStore) LoadUpstream(ctx context.Context, kind, platformName, exposedID, sessionID string) (*GBCascadeTaskRouteRecord, bool, error) {
	return s.load(ctx, "upstream_key", GBCascadeTaskUpstreamKey(kind, platformName, exposedID, sessionID))
}

func (s *GBCascadeTaskRouteStore) load(ctx context.Context, column, key string) (*GBCascadeTaskRouteRecord, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, errors.New("GB cascade task route store is unavailable")
	}
	var record GBCascadeTaskRouteRecord
	err := s.db.WithContext(ctx).Where(column+" = ?", key).First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &record, true, nil
}

func (s *GBCascadeTaskRouteStore) Delete(ctx context.Context, kind, deviceID, sessionID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.WithContext(ctx).
		Where("route_key = ?", GBCascadeTaskRouteKey(kind, deviceID, sessionID)).
		Delete(new(GBCascadeTaskRouteRecord)).Error
}

func (s *GBCascadeTaskRouteStore) DeleteUpstream(ctx context.Context, kind, platformName, exposedID, sessionID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.WithContext(ctx).
		Where("upstream_key = ?", GBCascadeTaskUpstreamKey(kind, platformName, exposedID, sessionID)).
		Delete(new(GBCascadeTaskRouteRecord)).Error
}

// Cleanup 删除过期路由，并只保留最近 limit 条。
func (s *GBCascadeTaskRouteStore) Cleanup(ctx context.Context, cutoff time.Time, limit int) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if !cutoff.IsZero() {
			if err := tx.Where("updated_at <= ?", cutoff).Delete(new(GBCascadeTaskRouteRecord)).Error; err != nil {
				return err
			}
		}
		if limit <= 0 {
			return nil
		}
		candidates := tx.Model(new(GBCascadeTaskRouteRecord)).
			Select("id").
			Order("updated_at DESC, id DESC").
			Offset(limit)
		overflow := tx.Table("(?) AS gb_cascade_task_route_overflow", candidates).Select("id")
		// 容量裁剪使用单条语句按当前排序删除，不能让查询后刚刷新的
		// 活跃路由仍被陈旧 overflow ID 集合删除。派生表也避免 MySQL
		// 拒绝从正在修改的目标表直接读取。
		return tx.Where("id IN (?)", overflow).Delete(new(GBCascadeTaskRouteRecord)).Error
	})
}
