package event

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ixugo/goddd/pkg/reason"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MediaCleanupTask 与事件删除原子提交，不关联外键，避免随事件级联删除。
// 记录存在即表示仍需清理；只有全部附件删除成功才移除任务。
type MediaCleanupTask struct {
	ID        int64 `gorm:"primaryKey"`
	EventID   int64 `gorm:"not null;index"`
	ImagePath string
	VideoPath string
	Attempts  int `gorm:"not null;default:0"`
	LastError string
	CreatedAt time.Time
}

func (MediaCleanupTask) TableName() string { return "event_media_cleanup_tasks" }

var ErrEvidenceActive = reason.ErrBadRequest.WithMsg("事件正在取证，请完成后再删除")

// DeleteEventRecord 只操作数据库，调用方必须在同一事务中先锁定事件行。
func DeleteEventRecord(tx *gorm.DB, e *Event) error {
	var tasks []EvidenceTask
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("event_id = ?", e.ID).Find(&tasks).Error; err != nil {
		return err
	}
	for _, task := range tasks {
		if task.State == "queued" || task.State == "working" {
			return ErrEvidenceActive
		}
	}
	if e.ImagePath != "" || e.VideoPath != "" {
		job := MediaCleanupTask{EventID: e.ID, ImagePath: e.ImagePath, VideoPath: e.VideoPath}
		if err := tx.Create(&job).Error; err != nil {
			return err
		}
	}
	for _, model := range []any{&AlarmReport{}, &Action{}, &EvidenceTask{}} {
		if err := tx.Where("event_id = ?", e.ID).Delete(model).Error; err != nil {
			return err
		}
	}
	result := tx.Delete(e)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("事件删除未生效，保留记录及附件等待重试")
	}
	return nil
}

// cleanupPendingMedia 对本轮每个任务最多尝试一次，失败任务不阻塞后续任务。
// eventID=0 扫描全部，否则仅处理本次手工删除的事件。
func (c Core) cleanupPendingMedia(ctx context.Context, eventID int64) {
	var cursor int64
	for ctx.Err() == nil {
		var jobs []MediaCleanupTask
		err := c.store.Event().Session(ctx, func(tx *gorm.DB) error {
			query := tx.Where("id > ?", cursor).Order("id").Limit(100)
			if eventID != 0 {
				query = query.Where("event_id = ?", eventID)
			}
			return query.Find(&jobs).Error
		})
		if err != nil {
			slog.WarnContext(ctx, "load event media cleanup tasks", "err", err)
			return
		}
		if len(jobs) == 0 {
			return
		}
		for _, job := range jobs {
			if ctx.Err() != nil {
				return
			}
			cursor = job.ID
			err = c.store.Event().Session(ctx, func(tx *gorm.DB) error {
				var current MediaCleanupTask
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, job.ID).Error; err != nil {
					if errors.Is(err, gorm.ErrRecordNotFound) {
						return nil
					}
					return err
				}
				for _, path := range []string{current.ImagePath, current.VideoPath} {
					if path == "" {
						continue
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					// 兼容历史上多个事件引用同一文件的情况，不删除仍在使用的证据。
					var references int64
					if err := tx.Model(&Event{}).Where("image_path = ? OR video_path = ?", path, path).Count(&references).Error; err != nil {
						return err
					}
					if references > 0 {
						return fmt.Errorf("附件仍被其他事件引用")
					}
					if err := removeEventMedia(path); err != nil {
						return err
					}
				}
				return tx.Delete(&current).Error
			})
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.WarnContext(ctx, "event media cleanup deferred", "task_id", job.ID, "event_id", job.EventID, "err", err)
				// 文件删除或提交失败都保留任务，重试把已不存在的文件视为成功。
				if updateErr := c.store.Event().Session(ctx, func(tx *gorm.DB) error {
					return tx.Model(&MediaCleanupTask{}).Where("id = ?", job.ID).Updates(map[string]any{"attempts": gorm.Expr("attempts + 1"), "last_error": err.Error()}).Error
				}); updateErr != nil {
					slog.WarnContext(ctx, "record event media cleanup failure", "task_id", job.ID, "err", updateErr)
				}
			}
		}
	}
}
