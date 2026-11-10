package recording

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

type cleanupTestStore struct {
	RecordingStorer
	db        *gorm.DB
	lists     int
	failDB    bool
	listLimit int
}

func (s *cleanupTestStore) Recording() RecordingStorer { return s }
func (s *cleanupTestStore) List(ctx context.Context, out *[]*Recording, pager orm.Pager, options ...orm.QueryOption) (int64, error) {
	s.lists++
	if s.listLimit > 0 && s.lists > s.listLimit {
		return 0, errors.New("cleanup test query limit exceeded")
	}
	return orm.FindWithContext(ctx, s.db, out, pager, options...)
}
func (s *cleanupTestStore) Session(ctx context.Context, fns ...func(*gorm.DB) error) error {
	if s.failDB {
		return errors.New("test database failure")
	}
	for _, fn := range fns {
		if err := fn(s.db.WithContext(ctx)); err != nil {
			return err
		}
	}
	return nil
}

func TestDiskCleanupStopsWithoutProgressAndPreservesRecords(t *testing.T) {
	for _, scenario := range []string{"empty", "invalid_path", "file_delete_failure", "database_failure", "missing_file"} {
		t.Run(scenario, func(t *testing.T) {
			c, store := newDiskCleanupTestCore(t)
			if scenario != "empty" {
				rec := Recording{Path: "test.mp4"}
				switch scenario {
				case "invalid_path":
					rec.Path = "../outside.mp4"
				case "file_delete_failure":
					// 非空目录稳定复现 os.Remove 失败，不依赖测试账号权限。
					dir := filepath.Join(c.conf.StorageDir, rec.Path)
					if err := os.Mkdir(dir, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("keep"), 0600); err != nil {
						t.Fatal(err)
					}
				case "database_failure":
					store.failDB = true
				}
				if err := store.db.Create(&rec).Error; err != nil {
					t.Fatal(err)
				}
			}
			if retry := c.cleanupByDiskUsageUsing(func(string) (float64, error) { return 99, nil }); retry {
				t.Fatal("cleanup requested accelerated retry without remaining progress")
			}
			var count int64
			if err := store.db.Model(&Recording{}).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			wantCount, wantLists := int64(1), 1
			if scenario == "empty" {
				wantCount = 0
			}
			if scenario == "missing_file" {
				wantCount, wantLists = 0, 2
			}
			if count != wantCount || store.lists != wantLists {
				t.Fatalf("records=%d lists=%d, want %d/%d", count, store.lists, wantCount, wantLists)
			}
			// 停止加速不禁用下一轮常规调用。
			store.lists = 0
			c.cleanupByDiskUsageUsing(func(string) (float64, error) { return 99, nil })
			if store.lists == 0 {
				t.Fatal("next regular cleanup was skipped")
			}
		})
	}
}

func TestDiskCleanupContinuesOnlyWithProgressAndHighUsage(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%t", missing), func(t *testing.T) {
			c, store := newDiskCleanupTestCore(t)
			items := make([]Recording, 1001)
			for i := range items {
				items[i].Path = fmt.Sprintf("%d.mp4", i)
			}
			if !missing {
				for _, rec := range items {
					if err := os.WriteFile(filepath.Join(c.conf.StorageDir, rec.Path), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := store.db.CreateInBatches(&items, 100).Error; err != nil {
				t.Fatal(err)
			}
			if !c.cleanupByDiskUsageUsing(func(string) (float64, error) { return 99, nil }) {
				t.Fatal("progress at batch cap did not permit accelerated retry")
			}
			var count int64
			store.db.Model(&Recording{}).Count(&count)
			if count != 1 || store.lists != 20 {
				t.Fatalf("batch limit changed: count=%d lists=%d", count, store.lists)
			}
			// 下一轮达到阈值即停止，保留剩余录像。
			if c.cleanupByDiskUsageUsing(func(string) (float64, error) { return 10, nil }) {
				t.Fatal("retry below threshold")
			}
			if store.lists != 20 {
				t.Fatal("deleted recordings below disk threshold")
			}
		})
	}
}

func newDiskCleanupTestCore(t *testing.T) (Core, *cleanupTestStore) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&Recording{}); err != nil {
		t.Fatal(err)
	}
	store := &cleanupTestStore{db: db}
	return NewCore(store, WithConfig(&conf.ServerRecording{StorageDir: t.TempDir(), DiskUsageThreshold: 90})), store
}

func TestExpiredRecordingCleanupStopsWithoutProgressAndCanRetry(t *testing.T) {
	for _, scenario := range []string{"invalid_path", "file_delete_failure", "database_failure", "zero_rows"} {
		t.Run(scenario, func(t *testing.T) {
			c, store := newDiskCleanupTestCore(t)
			c.conf.RetainDays = 7
			store.listLimit = 5 // 错误实现也能有界失败，不让回归检查自身无限循环。
			originalDelete := store.db.Callback().Delete().Get("gorm:delete")
			expired := Recording{Path: "expired.mp4", StartedAt: orm.Time{Time: time.Now().AddDate(0, 0, -10)}}
			switch scenario {
			case "invalid_path":
				expired.Path = "../outside.mp4"
			case "file_delete_failure":
				dir := filepath.Join(c.conf.StorageDir, expired.Path)
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "keep"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "database_failure", "zero_rows":
				if err := os.WriteFile(filepath.Join(c.conf.StorageDir, expired.Path), nil, 0600); err != nil {
					t.Fatal(err)
				}
				if scenario == "database_failure" {
					store.failDB = true
				} else {
					if err := store.db.Callback().Delete().Replace("gorm:delete", func(tx *gorm.DB) { tx.RowsAffected = 0 }); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := store.db.Create(&expired).Error; err != nil {
				t.Fatal(err)
			}
			recent := Recording{Path: "recent.mp4", StartedAt: orm.Time{Time: time.Now()}}
			if err := store.db.Create(&recent).Error; err != nil {
				t.Fatal(err)
			}
			c.cleanupExpiredRecordings()
			var count int64
			if err := store.db.Model(&Recording{}).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if store.lists != 1 || count != 2 {
				t.Fatalf("failed cleanup lists=%d records=%d, want 1/2", store.lists, count)
			}
			if scenario == "zero_rows" {
				if err := store.db.Callback().Delete().Replace("gorm:delete", originalDelete); err != nil {
					t.Fatal(err)
				}
			}
			store.failDB = false
			// 失败原因解除后，下一轮仍能清理原记录；近期记录继续保留。
			if err := store.db.Model(&Recording{}).Where("id = ?", expired.ID).Update("path", "missing-recovered.mp4").Error; err != nil {
				t.Fatal(err)
			}
			store.lists = 0
			c.cleanupExpiredRecordings()
			if err := store.db.Model(&Recording{}).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 1 || store.lists != 2 {
				t.Fatalf("retry did not recover: records=%d lists=%d", count, store.lists)
			}
		})
	}
}

func TestExpiredRecordingCleanupPartialFailureDoesNotBlockSuccessfulBatches(t *testing.T) {
	c, store := newDiskCleanupTestCore(t)
	c.conf.RetainDays = 7
	store.listLimit = 5
	items := make([]Recording, 151)
	for i := range items {
		items[i] = Recording{Path: fmt.Sprintf("missing-%d.mp4", i), StartedAt: orm.Time{Time: time.Now().AddDate(0, 0, -10)}}
	}
	items[0].Path = "../outside.mp4"
	if err := store.db.CreateInBatches(&items, 100).Error; err != nil {
		t.Fatal(err)
	}
	c.cleanupExpiredRecordings()
	var remaining []Recording
	if err := store.db.Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != items[0].ID || store.lists != 3 {
		t.Fatalf("partial failure lost records or blocked progress: remaining=%v queries=%d", remaining, store.lists)
	}
}

func TestExpiredRecordingCleanupPreservesPolicyAndContinuesWithProgress(t *testing.T) {
	c, store := newDiskCleanupTestCore(t)
	c.conf.RetainDays = 7
	store.listLimit = 6
	items := make([]Recording, 205)
	for i := range items {
		items[i] = Recording{Path: fmt.Sprintf("expired-%d.mp4", i), StartedAt: orm.Time{Time: time.Now().AddDate(0, 0, -10)}}
		if i%2 == 0 {
			if err := os.WriteFile(filepath.Join(c.conf.StorageDir, items[i].Path), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.db.CreateInBatches(&items, 100).Error; err != nil {
		t.Fatal(err)
	}
	recent := Recording{Path: "recent.mp4", StartedAt: orm.Time{Time: time.Now()}}
	if err := store.db.Create(&recent).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.conf.StorageDir, recent.Path), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	c.cleanupExpiredRecordings()
	var remaining []Recording
	if err := store.db.Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != recent.ID || store.lists != 4 {
		t.Fatalf("retention or batching changed: remaining=%v queries=%d", remaining, store.lists)
	}
	if body, err := os.ReadFile(filepath.Join(c.conf.StorageDir, recent.Path)); err != nil || string(body) != "keep" {
		t.Fatal("recent recording file changed")
	}
}
