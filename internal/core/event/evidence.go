package event

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// EvidenceTask 与事件在同一事务落库。策略冻结在任务中，历史事件不追溯补拍。
type EvidenceTask struct {
	EventID        int64      `gorm:"primaryKey" json:"event_id"`
	CID            string     `gorm:"column:cid;size:128;index:idx_event_evidence_channel_time,priority:1" json:"cid"`
	PolicyRevision int        `json:"policy_revision"`
	ClipSeconds    int        `json:"clip_seconds"`
	State          string     `gorm:"size:16;not null;index" json:"state"`
	SnapshotState  string     `gorm:"size:16" json:"snapshot_state"`
	VideoState     string     `gorm:"size:16" json:"video_state"`
	SnapshotError  string     `json:"snapshot_error,omitempty"`
	VideoError     string     `json:"video_error,omitempty"`
	CreatedAt      time.Time  `gorm:"index:idx_event_evidence_channel_time,priority:2" json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	Event          Event      `gorm:"constraint:OnDelete:CASCADE" json:"-"`
}

func (EvidenceTask) TableName() string { return "event_evidence_tasks" }

// EnqueueEvidence 必须在创建事件的事务内调用；取证排队失败不能留下未安排动作的半条事件。
func EnqueueEvidence(tx *gorm.DB, e *Event) error {
	if !strings.EqualFold(e.Model, "GB28181") {
		return nil
	}
	// 与删除使用相同的事件行锁；事件一旦删除，不能再为其建立取证任务。
	var locked Event
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, e.ID).Error; err != nil {
		return err
	}
	var p Policy
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&p, "did = ?", e.DID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	if !p.Snapshot && !p.Record {
		return nil
	}
	t := EvidenceTask{EventID: e.ID, CID: e.CID, PolicyRevision: p.Revision, ClipSeconds: p.ClipSeconds, State: "queued", SnapshotState: "not_requested", VideoState: "not_requested"}
	if p.Snapshot {
		t.SnapshotState = "queued"
	}
	if p.Record {
		t.VideoState = "queued"
	}
	now := time.Now()
	skip := ""
	if e.CID == "" {
		skip = "设备级报警未指定通道，无法取证"
	} else if now.Sub(e.StartedAt.Time) > 120*time.Second || e.StartedAt.Time.After(now.Add(30*time.Second)) {
		skip = "报警时间超出实时取证窗口，保留原始事件"
	} else {
		var recent int64
		if err := tx.Model(&EvidenceTask{}).Where("cid = ? AND ((created_at > ? AND state <> ?) OR state IN ?)", e.CID, now.Add(-time.Duration(p.CooldownSeconds)*time.Second), "skipped", []string{"queued", "working"}).Count(&recent).Error; err != nil {
			return err
		}
		if recent > 0 {
			skip = "同通道取证冷却中，保留原始事件"
		}
		var queued int64
		if err := tx.Model(&EvidenceTask{}).Where("state IN ?", []string{"queued", "working"}).Count(&queued).Error; err != nil {
			return err
		}
		if queued >= 128 {
			skip = "取证队列繁忙，保留原始事件"
		}
	}
	if skip != "" {
		t.State = "skipped"
		if p.Snapshot {
			t.SnapshotState = "skipped"
			t.SnapshotError = skip
		}
		if p.Record {
			t.VideoState = "skipped"
			t.VideoError = skip
		}
		t.FinishedAt = &now
	}
	return tx.Omit("Event").Create(&t).Error
}

type EvidenceExecutor func(context.Context, *Event, string, int) (string, error)

func (w *Workflow) SetEvidenceExecutor(execute EvidenceExecutor) {
	w.mu.Lock()
	w.execute = execute
	w.mu.Unlock()
}

// RunEvidenceWorkers 有限并发处理持久任务。失败不晚到补拍，防止用之后画面冒充报警现场。
func (w *Workflow) RunEvidenceWorkers(ctx context.Context) {
	// 单实例生命周期恢复：执行中的任务在重启后不重放。
	now := time.Now()
	if err := w.db.WithContext(ctx).Model(&EvidenceTask{}).Where("state = ?", "working").Updates(map[string]any{"state": "failed", "snapshot_state": gorm.Expr("CASE WHEN snapshot_state = 'working' THEN 'failed' ELSE snapshot_state END"), "video_state": gorm.Expr("CASE WHEN video_state = 'working' THEN 'failed' ELSE video_state END"), "snapshot_error": gorm.Expr("CASE WHEN snapshot_state = 'working' THEN '服务重启，未完成抓拍' ELSE snapshot_error END"), "video_error": gorm.Expr("CASE WHEN video_state = 'working' THEN '服务重启，未完成录像' ELSE video_error END"), "finished_at": now}).Error; err != nil {
		slog.Error("recover event evidence tasks", "err", err)
		return
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					w.processNextEvidence(ctx)
				}
			}
		}()
	}
	wg.Wait()
}
func (w *Workflow) processNextEvidence(ctx context.Context) {
	w.mu.RLock()
	execute := w.execute
	w.mu.RUnlock()
	if execute == nil {
		return
	}
	var task EvidenceTask
	err := w.db.WithContext(ctx).Where("state = ?", "queued").Order("event_id").First(&task).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return
	}
	if err != nil {
		slog.Error("load event evidence task", "err", err)
		return
	}
	began := time.Now()
	var e Event
	claimed := false
	err = w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&e, task.EventID).Error; err != nil {
			return err
		}
		claim := tx.Model(&EvidenceTask{}).Where("event_id = ? AND state = ?", task.EventID, "queued").Updates(map[string]any{"state": "working", "started_at": began, "snapshot_state": gorm.Expr("CASE WHEN snapshot_state = 'queued' THEN 'working' ELSE snapshot_state END"), "video_state": gorm.Expr("CASE WHEN video_state = 'queued' THEN 'working' ELSE video_state END")})
		claimed = claim.RowsAffected == 1
		return claim.Error
	})
	if err != nil || !claimed {
		return
	}
	task.State = "working"
	task.StartedAt = &began
	stale := time.Since(e.StartedAt.Time) > 120*time.Second
	taskCtx, cancel := context.WithTimeout(ctx, time.Duration(task.ClipSeconds+150)*time.Second)
	defer cancel()
	type result struct {
		kind, path string
		err        error
	}
	results := make(chan result, 2)
	count := 0
	for _, kind := range []string{"snapshot", "video"} {
		state := task.SnapshotState
		if kind == "video" {
			state = task.VideoState
		}
		if state != "queued" {
			continue
		}
		count++
		go func(kind string) {
			out := result{kind: kind}
			if stale {
				out.err = errors.New("排队超出实时取证窗口，未补拍之后的画面")
			} else {
				out.path, out.err = safeExecuteEvidence(execute, taskCtx, &e, kind, task.ClipSeconds)
			}
			results <- out
		}(kind)
	}
	updates := map[string]any{}
	for range count {
		out := <-results
		state := "succeeded"
		message := ""
		if out.err != nil {
			state = "failed"
			message = out.err.Error()
		}
		if out.kind == "snapshot" {
			task.SnapshotState, task.SnapshotError = state, message
			if out.err == nil {
				updates["image_path"] = out.path
			}
		} else {
			task.VideoState, task.VideoError = state, message
			if out.err == nil {
				updates["video_path"] = out.path
			}
		}
	}
	task.State = "succeeded"
	if task.SnapshotState == "failed" || task.VideoState == "failed" {
		task.State = "failed"
	}
	finished := time.Now()
	task.FinishedAt = &finished
	// 关停时仍把结果写入；未写成功的 working 任务由下次启动标记中断。
	saveCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	err = w.db.WithContext(saveCtx).Transaction(func(tx *gorm.DB) error {
		if len(updates) > 0 {
			if err := tx.Model(&Event{}).Where("id = ?", e.ID).Updates(updates).Error; err != nil {
				return err
			}
		}
		return tx.Model(&EvidenceTask{}).Where("event_id = ?", e.ID).Select("state", "snapshot_state", "video_state", "snapshot_error", "video_error", "finished_at").Updates(&task).Error
	})
	if err != nil {
		slog.Error("save event evidence result", "event_id", e.ID, "err", err)
	}
}

func safeExecuteEvidence(execute EvidenceExecutor, ctx context.Context, e *Event, kind string, seconds int) (path string, err error) {
	defer func() {
		if recover() != nil {
			path = ""
			err = fmt.Errorf("取证执行异常，本次未获取现场证据")
		}
	}()
	path, err = execute(ctx, e, kind, seconds)
	if err == nil && strings.TrimSpace(path) == "" {
		err = fmt.Errorf("取证未返回有效文件")
	}
	return
}
