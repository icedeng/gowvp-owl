package capture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testService(t *testing.T) *Service {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	sql.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sql.Close() })
	if err := db.AutoMigrate(&Record{}, &Schedule{}); err != nil {
		t.Fatal(err)
	}
	return New(db, filepath.Join(t.TempDir(), "captures"))
}
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 3), uint8(y * 4), 100, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func running(t *testing.T, s *Service, channel string) *Record {
	t.Helper()
	row, err := s.Enqueue(t.Context(), channel, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(row).Update("state", "running").Error; err != nil {
		t.Fatal(err)
	}
	return row
}
func saved(t *testing.T, s *Service, channel string) *Record {
	t.Helper()
	row, err := s.Complete(t.Context(), running(t, s, channel).ID, testPNG(t), "ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestPermanentFilesNamingRenameDeleteAndLateUpload(t *testing.T) {
	s := testService(t)
	row := saved(t, s, "ch")
	if err := s.SetSession(t.Context(), row.ID, "session-123"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := s.Get(t.Context(), row.ID)
	if err != nil || reloaded.SessionID != "session-123" {
		t.Fatal("设备会话未持久化", reloaded, err)
	}
	other := saved(t, s, "ch")
	if !regexp.MustCompile(`^\d{14}\.jpg$`).MatchString(row.Name) {
		t.Fatal(row.Name)
	}
	path, _ := s.Path(row)
	otherPath, _ := s.Path(other)
	if path == otherPath {
		t.Fatal("两次抓拍覆盖同一路径")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, format, err := image.Decode(bytes.NewReader(body))
	if err != nil || format != "jpeg" {
		t.Fatalf("没有转换为JPEG: %s %v", format, err)
	}
	for _, bad := range []string{"../test.jpg", "a/b.jpg", "a\\b.jpg", "C:test.jpg", "a\n.jpg", ".hidden.jpg", "a.png", " ", ".jpg"} {
		if _, err := s.Rename(t.Context(), row.ID, bad); err == nil {
			t.Fatalf("接受危险名称%q", bad)
		}
	}
	row, err = s.Rename(t.Context(), row.ID, "九龙坡门口.jpg")
	if err != nil {
		t.Fatal(err)
	}
	newPath, _ := s.Path(row)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("旧文件名仍存在")
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context(), row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(newPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("图片文件没有删除")
	}
	if _, err := s.Get(t.Context(), row.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal("记录没有删除")
	}
	if _, err := s.Complete(t.Context(), row.ID, testPNG(t), "upload"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal("迟到上传恢复了已删除任务", err)
	}
	if _, err := os.Stat(otherPath); err != nil {
		t.Fatal("删除影响其他图片", err)
	}
}

func TestFailedDiskDeleteCanBeRetried(t *testing.T) {
	s := testService(t)
	row := saved(t, s, "ch")
	backup := s.root + "-backup"
	if err := os.Rename(s.root, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.root, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context(), row.ID); err == nil {
		t.Fatal("文件删除失败没有上报")
	}
	state, err := s.Get(t.Context(), row.ID)
	if err != nil || state.State != "deleting" {
		t.Fatal("未保留删除状态", state, err)
	}
	if err := os.Remove(s.root); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, s.root); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context(), row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.root, row.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("重试未删除文件")
	}
}

func TestRenameDatabaseFailureRollsBackFileAndCompleteFailureCleansFile(t *testing.T) {
	s := testService(t)
	row := saved(t, s, "ch")
	path, _ := s.Path(row)
	if err := s.db.Callback().Update().Before("gorm:update").Register("reject", func(tx *gorm.DB) { tx.AddError(fmt.Errorf("database unavailable")) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rename(t.Context(), row.ID, "new.jpg"); err == nil {
		t.Fatal("未返回数据库错误")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("重命名失败未恢复文件", err)
	}
	_ = s.db.Callback().Update().Remove("reject")
	pending := running(t, s, "second")
	_ = s.db.Callback().Update().Before("gorm:update").Register("reject", func(tx *gorm.DB) { tx.AddError(fmt.Errorf("database unavailable")) })
	if _, err := s.Complete(t.Context(), pending.ID, testPNG(t), "ffmpeg"); err == nil {
		t.Fatal("记录写入失败却返回成功")
	}
	files, _ := filepath.Glob(filepath.Join(s.root, pending.ID, "*.jpg"))
	if len(files) != 0 {
		t.Fatal("保存失败遗留无记录的图片")
	}
}

func TestQueueDeduplicationAndTerminalStateRejectsUpload(t *testing.T) {
	s := testService(t)
	row := running(t, s, "ch")
	same, err := s.Enqueue(t.Context(), "ch", "schedule")
	if err != nil || same.ID != row.ID {
		t.Fatal("同通道重复抓拍", same, err)
	}
	if err := s.Delete(t.Context(), row.ID); err == nil {
		t.Fatal("允许删除运行中的任务")
	}
	if err := s.finish(t.Context(), row.ID, fmt.Errorf("upload timeout")); err == nil {
		t.Fatal("失败未上报")
	}
	if _, err := s.Complete(t.Context(), row.ID, testPNG(t), "upload"); err == nil {
		t.Fatal("超时后迟到上传改变结果")
	}
	if _, err := s.Complete(t.Context(), running(t, s, "bad").ID, []byte("bad"), "ffmpeg"); err == nil {
		t.Fatal("接受了无效图片")
	}
}

func TestSchedulesPersistClaimOnceAndDisableOnRemoval(t *testing.T) {
	s := testService(t)
	due := time.Now().Add(-24 * time.Hour)
	if err := s.SaveSchedule(t.Context(), &Schedule{ChannelID: "ch", Enabled: true, IntervalSeconds: 10, NextRunAt: due}); err != nil {
		t.Fatal(err)
	}
	restarted := New(s.db, s.root)
	restarted.tickSchedules(t.Context())
	restarted.tickSchedules(t.Context())
	rows, total, err := s.List(t.Context(), "ch", 1, 100)
	if err != nil || total != 1 || rows[0].Source != "schedule" {
		t.Fatal("定时未去重或回补了积压", total, err)
	}
	plan, err := restarted.GetSchedule(t.Context(), "ch")
	if err != nil || time.Until(plan.NextRunAt) < 8*time.Second {
		t.Fatal("下次时间未按当前时间计算", plan, err)
	}
	if err := s.DisableSchedules(t.Context(), []string{"ch"}); err != nil {
		t.Fatal(err)
	}
	plan, _ = s.GetSchedule(t.Context(), "ch")
	row, _ := s.Get(t.Context(), rows[0].ID)
	if plan.Enabled || row.State != "failed" {
		t.Fatal("删除通道未停计划与排队任务")
	}
}

func TestWorkerRestartRecoversQueueAndShutdownCancelsWork(t *testing.T) {
	s := testService(t)
	interrupted := running(t, s, "old")
	queued, err := s.Enqueue(t.Context(), "new", "manual")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, 1)
	done := make(chan struct{})
	var calls atomic.Int32
	go func() {
		defer close(done)
		s.Run(ctx, func(ctx context.Context, row *Record) error {
			calls.Add(1)
			started <- row.ID
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	select {
	case id := <-started:
		if id != queued.ID {
			t.Fatal(id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("排队任务没有恢复")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("退出时worker未取消")
	}
	old, _ := s.Get(t.Context(), interrupted.ID)
	stopped, _ := s.Get(t.Context(), queued.ID)
	if old.State != "failed" || stopped.State != "failed" || calls.Load() != 1 {
		t.Fatal("重启恢复/退出状态错误", old, stopped, calls.Load())
	}
}
