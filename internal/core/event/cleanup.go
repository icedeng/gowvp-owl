package event

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

// StartCleanupWorker 保留清理每日执行，已提交删除的附件任务每分钟重试。
// 禁用保留期清理也不会遗失手工删除后尚未完成的附件任务。
func (c Core) StartCleanupWorker(ctx context.Context, days int) {
	if ctx.Err() != nil {
		return
	}
	c.cleanupExpiredEvents(ctx, days)
	retention := time.NewTicker(24 * time.Hour)
	retry := time.NewTicker(time.Minute)
	defer retention.Stop()
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-retention.C:
			c.cleanupExpiredEvents(ctx, days)
		case <-retry.C:
			c.cleanupPendingMedia(ctx, 0)
		}
	}
}

// 先提交事件删除与附件任务，再回收文件；每次删除在事务内重新检查保留期和取证状态。
func (c Core) cleanupExpiredEvents(ctx context.Context, days int) {
	if days <= 0 {
		c.cleanupPendingMedia(ctx, 0)
		return
	}
	cutoff := orm.Time{Time: time.Now().AddDate(0, 0, -days)}
	var cursor int64
	deleted := 0
	defer func() {
		if ctx.Err() == nil {
			c.cleanupPendingMedia(ctx, 0)
		}
		slog.InfoContext(ctx, "event cleanup completed", "events_deleted", deleted)
	}()
	for ctx.Err() == nil {
		var events []*Event
		pager := web.PagerFilter{Page: 1, Size: 100}
		_, err := c.store.Event().List(ctx, &events, &pager,
			orm.Where("started_at < ?", cutoff), orm.Where("id > ?", cursor), orm.OrderBy("id"),
			orm.Where("NOT EXISTS (SELECT 1 FROM event_evidence_tasks t WHERE t.event_id = events.id AND t.state IN ('queued','working'))"))
		if err != nil {
			slog.WarnContext(ctx, "query expired events", "err", err)
			return
		}
		if len(events) == 0 {
			return
		}
		for _, candidate := range events {
			if ctx.Err() != nil {
				return
			}
			cursor = candidate.ID
			var current Event
			err := c.store.Event().Delete(ctx, &current, orm.Where("id = ?", candidate.ID), orm.Where("started_at < ?", cutoff))
			if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, ErrEvidenceActive) {
				continue
			}
			if err != nil {
				slog.WarnContext(ctx, "delete expired event", "event_id", candidate.ID, "err", err)
				return
			}
			deleted++
		}
	}
}
