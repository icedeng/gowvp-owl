package event

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/thumbnail"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/system"
	"gorm.io/gorm"
)

func cleanupEvidence(t *testing.T, store *progressCleanupEventStore, path string) (Event, string) {
	t.Helper()
	root := filepath.Join(system.Getwd(), "configs", "events")
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	e := Event{StartedAt: orm.Time{Time: time.Now().AddDate(0, 0, -10)}, ImagePath: path}
	if err := store.db.Create(&e).Error; err != nil {
		t.Fatal(err)
	}
	return e, full
}

func pendingCleanupCount(t *testing.T, store *progressCleanupEventStore, want int64) {
	t.Helper()
	var n int64
	if err := store.db.Model(&MediaCleanupTask{}).Count(&n).Error; err != nil || n != want {
		t.Fatalf("pending tasks=%d want=%d err=%v", n, want, err)
	}
}

func TestCleanupRecoversCommittedDeletionAfterRestart(t *testing.T) {
	store := newProgressCleanupEventStore(t)
	e, file := cleanupEvidence(t, store, "restart.jpg")
	if err := store.Delete(t.Context(), &Event{}, orm.Where("id = ?", e.ID)); err != nil {
		t.Fatal(err)
	}
	pendingCleanupCount(t, store, 1)
	if _, err := os.Stat(file); err != nil {
		t.Fatal("database deletion touched files", err)
	}
	// 用新 Core 处理已提交任务，模拟进程在事务提交后、文件删除前退出。
	NewCore(cleanupStore{events: store}, nil).cleanupExpiredEvents(t.Context(), 0)
	pendingCleanupCount(t, store, 0)
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("restart did not reclaim file", err)
	}
}

func TestManualDeletionRetriesFilesAndCleanupCommitFailure(t *testing.T) {
	for _, failure := range []string{"file_failure", "cleanup_commit_failure"} {
		t.Run(failure, func(t *testing.T) {
			store := newProgressCleanupEventStore(t)
			core := NewCore(cleanupStore{events: store}, nil)
			e, file := cleanupEvidence(t, store, "retry.jpg")
			preview := thumbnail.Path(file)
			if err := os.WriteFile(preview, []byte("preview"), 0600); err != nil {
				t.Fatal(err)
			}
			if failure == "file_failure" {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(file, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := store.db.Callback().Delete().After("gorm:delete").Register("test:fail_cleanup", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "event_media_cleanup_tasks" {
						tx.AddError(errors.New("cleanup commit failed"))
					}
				}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := core.DeleteEvent(t.Context(), e.ID); err != nil {
				t.Fatal(err)
			}
			var n int64
			store.db.Model(&Event{}).Where("id = ?", e.ID).Count(&n)
			if n != 0 {
				t.Fatal("manual event deletion not committed")
			}
			var job MediaCleanupTask
			if err := store.db.First(&job).Error; err != nil || job.Attempts != 1 || job.LastError == "" {
				t.Fatal("failure not retained", job, err)
			}
			if failure == "file_failure" {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte("evidence"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				store.db.Callback().Delete().Remove("test:fail_cleanup")
			}
			NewCore(cleanupStore{events: store}, nil).cleanupPendingMedia(t.Context(), 0)
			pendingCleanupCount(t, store, 0)
			for _, path := range []string{file, preview} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("retry left media", path, err)
				}
			}
		})
	}
}

func TestCleanupRechecksEvidenceAfterCandidateQuery(t *testing.T) {
	store := newProgressCleanupEventStore(t)
	core := NewCore(cleanupStore{events: store}, nil)
	e, file := cleanupEvidence(t, store, "active.jpg")
	store.beforeDelete = func(_ *Event) {
		store.beforeDelete = nil
		if err := store.db.Create(&EvidenceTask{EventID: e.ID, State: "queued"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	core.cleanupExpiredEvents(t.Context(), 7)
	pendingCleanupCount(t, store, 0)
	if _, err := os.Stat(file); err != nil {
		t.Fatal("newly queued evidence deleted", err)
	}
	if err := store.db.First(&Event{}, e.ID).Error; err != nil {
		t.Fatal("active event deleted", err)
	}
}

func TestCleanupCancellationRollsBackWithoutRemovingMedia(t *testing.T) {
	store := newProgressCleanupEventStore(t)
	core := NewCore(cleanupStore{events: store}, nil)
	e, file := cleanupEvidence(t, store, "cancel.jpg")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := store.db.Callback().Delete().After("gorm:delete").Register("test:cancel", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "events" {
			cancel()
			tx.AddError(ctx.Err())
		}
	}); err != nil {
		t.Fatal(err)
	}
	core.cleanupExpiredEvents(ctx, 7)
	if store.queries != 1 {
		t.Fatal("canceled cleanup continued querying")
	}
	if err := store.db.First(&Event{}, e.ID).Error; err != nil {
		t.Fatal("canceled transaction committed", err)
	}
	pendingCleanupCount(t, store, 0)
	if _, err := os.Stat(file); err != nil {
		t.Fatal("canceled cleanup removed media", err)
	}
}

func TestCleanupPreservesSharedFilesAndRetriesPastBlockedJob(t *testing.T) {
	store := newProgressCleanupEventStore(t)
	core := NewCore(cleanupStore{events: store}, nil)
	a, shared := cleanupEvidence(t, store, "shared.jpg")
	b := Event{StartedAt: orm.Now(), ImagePath: a.ImagePath}
	if err := store.db.Create(&b).Error; err != nil {
		t.Fatal(err)
	}
	_, other := cleanupEvidence(t, store, "other.jpg")
	core.cleanupExpiredEvents(t.Context(), 7)
	pendingCleanupCount(t, store, 1)
	if _, err := os.Stat(shared); err != nil {
		t.Fatal("live reference lost", err)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatal("blocked job starved later job", err)
	}
	if _, err := core.DeleteEvent(t.Context(), b.ID); err != nil {
		t.Fatal(err)
	}
	core.cleanupPendingMedia(t.Context(), 0)
	pendingCleanupCount(t, store, 0)
}

func TestCleanupDoesNotFollowEscapingPaths(t *testing.T) {
	store := newProgressCleanupEventStore(t)
	root := filepath.Join(system.Getwd(), "configs", "events")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	file := filepath.Join(outside, "keep.jpg")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, "../keep.jpg", "link/keep.jpg"} {
		if err := store.db.Create(&MediaCleanupTask{EventID: 99, ImagePath: path}).Error; err != nil {
			t.Fatal(err)
		}
	}
	NewCore(cleanupStore{events: store}, nil).cleanupPendingMedia(t.Context(), 0)
	pendingCleanupCount(t, store, 3)
	if body, err := os.ReadFile(file); err != nil || string(body) != "keep" {
		t.Fatal("cleanup escaped root", err)
	}
}

func TestCleanupExcludesWorkingEvidenceUntilCompletion(t *testing.T) {
	store := newProgressCleanupEventStore(t)
	core := NewCore(cleanupStore{events: store}, nil)
	e, file := cleanupEvidence(t, store, "original.jpg")
	if err := store.db.Model(&e).Update("started_at", orm.Now()).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.db.Create(&EvidenceTask{EventID: e.ID, State: "queued", SnapshotState: "queued"}).Error; err != nil {
		t.Fatal(err)
	}
	w := NewWorkflow(store.db)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseWorker := sync.OnceFunc(func() { close(release); <-done })
	defer releaseWorker()
	w.SetEvidenceExecutor(func(ctx context.Context, e *Event, kind string, seconds int) (string, error) {
		close(started)
		<-release
		return "original.jpg", nil
	})
	go func() { defer close(done); w.processNextEvidence(t.Context()) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	if err := store.db.Model(&e).Update("started_at", orm.Time{Time: time.Now().AddDate(0, 0, -10)}).Error; err != nil {
		t.Fatal(err)
	}
	core.cleanupExpiredEvents(t.Context(), 7)
	pendingCleanupCount(t, store, 0)
	if _, err := core.DeleteEvent(t.Context(), e.ID); !errors.Is(err, ErrEvidenceActive) {
		t.Fatal("working event not protected", err)
	}
	releaseWorker()
	core.cleanupExpiredEvents(t.Context(), 7)
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("completed evidence was not cleaned", err)
	}
	pendingCleanupCount(t, store, 0)
}

func TestCleanupMissingMigrationFailsWithoutRemovingEvidence(t *testing.T) {
	store := newProgressCleanupEventStore(t)
	e, file := cleanupEvidence(t, store, "schema.jpg")
	if err := store.db.Migrator().DropTable(&MediaCleanupTask{}); err != nil {
		t.Fatal(err)
	}
	core := NewCore(cleanupStore{events: store}, nil)
	core.cleanupExpiredEvents(t.Context(), 7)
	if err := store.db.First(&Event{}, e.ID).Error; err != nil {
		t.Fatal("missing schema allowed event deletion", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("missing schema lost evidence", err)
	}
	if err := store.db.AutoMigrate(&MediaCleanupTask{}); err != nil {
		t.Fatal(err)
	}
	core.cleanupExpiredEvents(t.Context(), 7)
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("cleanup did not recover after migration", err)
	}
}
