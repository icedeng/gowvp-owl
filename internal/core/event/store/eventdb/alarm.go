package eventdb

import (
	"context"
	"errors"

	"github.com/gowvp/owl/internal/core/event"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CreateAlarm 的去重、聚合、明细和首次取证在同一事务提交。
func (d Event) CreateAlarm(ctx context.Context, model *event.Event, occurrence string) (bool, error) {
	created := false
	err := d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := event.CheckAdmission(ctx, tx); err != nil {
			return err
		}
		// 与策略保存和取证使用同一设备行锁，多实例也不会并发建立重复事件。
		seed := event.Policy{DID: model.DID, ClipSeconds: 15, CooldownSeconds: 60}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&seed, "did = ?", model.DID).Error; err != nil {
			return err
		}
		var report event.AlarmReport
		if err := tx.First(&report, "report_key = ?", occurrence).Error; err == nil {
			return tx.First(model, report.EventID).Error
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		report = event.AlarmReport{Key: occurrence, ReportedAt: model.StartedAt.Time, Payload: model.Zones}
		// 升级时仍接受旧版本已入库的同一投递重试，不受新语义键影响。
		if model.SourceKey != nil && *model.SourceKey != "" {
			var existing event.Event
			if err := tx.First(&existing, "source_key = ?", *model.SourceKey).Error; err == nil {
				report.EventID = existing.ID
				*model = existing
				return tx.Omit("Event").Create(&report).Error
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		var existing event.Event
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("aggregation_key = ? AND started_at <= ? AND ended_at >= ?", *model.AggregationKey,
			orm.Time{Time: model.StartedAt.Add(event.AlarmQuietWindow)}, orm.Time{Time: model.StartedAt.Add(-event.AlarmQuietWindow)}).
			Order("ended_at DESC, id DESC").First(&existing).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && (existing.Status == "pending" || existing.Status == "processing") {
			start, end := existing.StartedAt, existing.EndedAt
			if model.StartedAt.Before(start.Time) {
				start = model.StartedAt
			}
			if model.StartedAt.After(end.Time) {
				end = model.StartedAt
			}
			// 聚合不改变人工处理状态及版本，不使正在填写的处理结果频繁冲突。
			if err := tx.Model(&existing).Updates(map[string]any{"started_at": start, "ended_at": end,
				"occurrence_count": gorm.Expr("occurrence_count + 1"), "updated_at": orm.Now()}).Error; err != nil {
				return err
			}
			if err := tx.First(model, existing.ID).Error; err != nil {
				return err
			}
		} else {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
			if err := event.EnqueueEvidence(tx, model); err != nil {
				return err
			}
			created = true
		}
		report.EventID = model.ID
		return tx.Omit("Event").Create(&report).Error
	})
	return created, err
}
