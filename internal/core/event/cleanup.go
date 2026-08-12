package event

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/system"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

// StartCleanupWorker 启动定时清理协程，每天凌晨 3 点执行一次清理
// days 参数指定保留的天数，超过该天数的事件将被删除
func (c Core) StartCleanupWorker(days int) {
	if days <= 0 {
		slog.Info("event cleanup disabled", "days", days)
		return
	}

	slog.Info("event cleanup worker started", "retain_days", days)

	// 启动时先执行一次清理
	c.cleanupExpiredEvents(days)

	// 计算到下一个凌晨 3 点的时间
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		c.cleanupExpiredEvents(days)
	}
}

// cleanupExpiredEvents 清理过期的事件，先删除本地图片文件，再删除数据库记录
func (c Core) cleanupExpiredEvents(days int) {
	ctx := context.Background()
	cutoffTime := time.Now().AddDate(0, 0, -days)
	cutoffMs := cutoffTime.UnixMilli()

	slog.Info("starting event cleanup", "cutoff_time", cutoffTime.Format(time.DateTime), "retain_days", days)

	// 分批查询并删除，避免一次性加载过多数据
	batchSize := 100
	totalDeleted := 0
	totalFilesDeleted := 0

	for {
		// 查询一批过期事件
		var events []*Event
		pager := web.PagerFilter{Page: 1, Size: batchSize}
		_, err := c.store.Event().List(ctx, &events, &pager,
			orm.Where("started_at < ?", cutoffMs),
		)
		if err != nil {
			slog.Error("failed to query expired events", "err", err)
			break
		}

		if len(events) == 0 {
			break
		}

		// 收集需要删除的图片路径（去重）
		imagePaths := make(map[string]struct{})
		eventIDs := make([]int64, 0, len(events))
		for _, e := range events {
			eventIDs = append(eventIDs, e.ID)
			if e.ImagePath != "" {
				imagePaths[e.ImagePath] = struct{}{}
			}
		}

		// 先删除本地图片文件
		eventsDir := filepath.Join(system.Getwd(), "configs", "events")
		for imagePath := range imagePaths {
			fullPath := filepath.Join(eventsDir, imagePath)
			if err := os.Remove(fullPath); err != nil {
				if !os.IsNotExist(err) {
					slog.Warn("failed to delete event image", "path", fullPath, "err", err)
				}
			} else {
				totalFilesDeleted++
			}
		}

		// 批量删除数据库记录，使用 WHERE IN 一次性删除
		err = c.store.Event().Session(ctx, func(tx *gorm.DB) error {
			return tx.Where("id IN ?", eventIDs).Delete(&Event{}).Error
		})
		if err != nil {
			slog.Warn("failed to batch delete events", "count", len(eventIDs), "err", err)
		} else {
			totalDeleted += len(eventIDs)
		}
	}

	// 清理空目录
	cleanupEmptyDirs(filepath.Join(system.Getwd(), "configs", "events"))

	slog.Info("event cleanup completed",
		"events_deleted", totalDeleted,
		"files_deleted", totalFilesDeleted,
	)
}

// cleanupEmptyDirs 递归删除空目录
func cleanupEmptyDirs(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			subDir := filepath.Join(dir, entry.Name())
			cleanupEmptyDirs(subDir)

			// 检查子目录是否为空
			subEntries, err := os.ReadDir(subDir)
			if err == nil && len(subEntries) == 0 {
				if err := os.Remove(subDir); err == nil {
					slog.Debug("removed empty directory", "path", subDir)
				}
			}
		}
	}
}
