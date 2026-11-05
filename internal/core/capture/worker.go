package capture

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Run 有界执行后台抓拍。重启后保留排队任务、标记中断任务，不回补错过的定时拍照。
func (s *Service) Run(ctx context.Context, run func(context.Context, *Record) error) {
	for {
		if err := s.db.WithContext(ctx).Model(&Record{}).Where("state = ?", "running").Updates(map[string]any{"state": "failed", "error": "服务重启，抓拍中断，请重新抓拍"}).Error; err == nil {
			break
		} else {
			slog.Error("recover capture tasks", "err", err)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	slots := make(chan struct{}, 64)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		s.tickSchedules(ctx)
		var deleting []Record
		if err := s.db.WithContext(ctx).Where("state = ?", "deleting").Limit(20).Find(&deleting).Error; err == nil {
			for _, row := range deleting {
				if err := s.Delete(ctx, row.ID); err != nil {
					slog.Warn("delete capture file", "id", row.ID, "err", err)
				}
			}
		}
		var queue []Record
		if err := s.db.WithContext(ctx).Where("state = ?", "queued").Order("created_at").Limit(64).Find(&queue).Error; err != nil {
			slog.Warn("load capture queue", "err", err)
		}
		for _, row := range queue {
			select {
			case slots <- struct{}{}:
			default:
				continue
			}
			claim := s.db.WithContext(ctx).Model(&Record{}).Where("id = ? AND state = ?", row.ID, "queued").Update("state", "running")
			if claim.Error != nil || claim.RowsAffected != 1 {
				<-slots
				continue
			}
			wg.Add(1)
			go func(row Record) {
				defer wg.Done()
				defer func() { <-slots }()
				err := run(ctx, &row)
				resultCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				err = s.finish(resultCtx, row.ID, err)
				if err != nil {
					slog.Warn("capture task failed", "id", row.ID, "channel_id", row.ChannelID, "err", err)
				}
				if row.Source == "schedule" {
					message := ""
					if err != nil {
						message = err.Error()
					}
					if err := s.db.WithContext(resultCtx).Model(&Schedule{}).Where("channel_id = ?", row.ChannelID).Updates(map[string]any{"last_run_at": time.Now(), "last_error": message}).Error; err != nil {
						slog.Warn("update capture schedule result", "err", err)
					}
				}
			}(row)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) tickSchedules(ctx context.Context) {
	var rows []Schedule
	if err := s.db.WithContext(ctx).Where("enabled = ? AND next_run_at <= ?", true, time.Now()).Order("next_run_at").Limit(50).Find(&rows).Error; err != nil {
		slog.Warn("load capture schedules", "err", err)
		return
	}
	for _, row := range rows {
		if row.IntervalSeconds < 10 {
			continue
		}
		next := time.Now().Add(time.Duration(row.IntervalSeconds) * time.Second)
		claim := s.db.WithContext(ctx).Model(&Schedule{}).Where("channel_id = ? AND enabled = ? AND next_run_at = ?", row.ChannelID, true, row.NextRunAt).Update("next_run_at", next)
		if claim.Error != nil || claim.RowsAffected != 1 {
			continue
		}
		if _, err := s.Enqueue(ctx, row.ChannelID, "schedule"); err != nil {
			s.db.WithContext(ctx).Model(&Schedule{}).Where("channel_id = ?", row.ChannelID).Update("last_error", err.Error())
		}
	}
}
