package event

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/glebarez/sqlite"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/system"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestCleanupWorkerStopsWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Core{}.StartCleanupWorker(ctx, 1)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup worker did not stop")
	}
}

type cleanupTimeArgument struct{}

func (cleanupTimeArgument) Match(value driver.Value) bool {
	_, ok := value.(time.Time)
	return ok
}

type cleanupEventStore struct {
	EventStorer
	db *gorm.DB
}

func (s cleanupEventStore) List(ctx context.Context, out *[]*Event, _ orm.Pager, options ...orm.QueryOption) (int64, error) {
	query := s.db.WithContext(ctx)
	for _, option := range options {
		query = option(query)
	}
	return 0, query.Find(out).Error
}

type cleanupStore struct{ events EventStorer }

func (s cleanupStore) Event() EventStorer { return s.events }

func TestCleanupBindsTimestampForPostgres(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT .* FROM "events" WHERE started_at < \$1`).
		WithArgs(cleanupTimeArgument{}).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	NewCore(cleanupStore{events: cleanupEventStore{db: db}}, nil).cleanupExpiredEvents(30)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

type progressCleanupEventStore struct {
	EventStorer
	db      *gorm.DB
	queries int
	failDB  bool
}

func (s *progressCleanupEventStore) List(ctx context.Context, out *[]*Event, pager orm.Pager, options ...orm.QueryOption) (int64, error) {
	s.queries++
	if s.queries > 6 {
		return 0, errors.New("cleanup test query limit exceeded")
	}
	return orm.FindWithContext(ctx, s.db, out, pager, options...)
}

func (s *progressCleanupEventStore) Session(ctx context.Context, fns ...func(*gorm.DB) error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, fn := range fns {
			if err := fn(tx); err != nil {
				return err
			}
		}
		// 模拟事务在执行删除后提交失败，必须保留记录并结束本轮。
		if s.failDB {
			return errors.New("test database failure")
		}
		return nil
	})
}

func newProgressCleanupEventStore(t *testing.T) *progressCleanupEventStore {
	t.Helper()
	t.Chdir(t.TempDir())
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
	if err := db.AutoMigrate(&Event{}); err != nil {
		t.Fatal(err)
	}
	return &progressCleanupEventStore{db: db}
}

func TestExpiredEventCleanupStopsOnFailedOrZeroRowDeletionAndCanRetry(t *testing.T) {
	for _, failure := range []string{"database_failure", "zero_rows"} {
		t.Run(failure, func(t *testing.T) {
			store := newProgressCleanupEventStore(t)
			core := NewCore(cleanupStore{events: store}, nil)
			expired := Event{StartedAt: orm.Time{Time: time.Now().AddDate(0, 0, -10)}}
			recent := Event{StartedAt: orm.Time{Time: time.Now()}}
			if err := store.db.Create(&expired).Error; err != nil {
				t.Fatal(err)
			}
			if err := store.db.Create(&recent).Error; err != nil {
				t.Fatal(err)
			}
			originalDelete := store.db.Callback().Delete().Get("gorm:delete")
			if failure == "database_failure" {
				store.failDB = true
			} else {
				if err := store.db.Callback().Delete().Replace("gorm:delete", func(tx *gorm.DB) { tx.RowsAffected = 0 }); err != nil {
					t.Fatal(err)
				}
			}
			core.cleanupExpiredEvents(7)
			var count int64
			if err := store.db.Model(&Event{}).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 2 || store.queries != 1 {
				t.Fatalf("failed cleanup records=%d queries=%d, want 2/1", count, store.queries)
			}
			store.failDB, store.queries = false, 0
			if err := store.db.Callback().Delete().Replace("gorm:delete", originalDelete); err != nil {
				t.Fatal(err)
			}
			core.cleanupExpiredEvents(7)
			var remaining []Event
			if err := store.db.Find(&remaining).Error; err != nil {
				t.Fatal(err)
			}
			if len(remaining) != 1 || remaining[0].ID != recent.ID || store.queries != 2 {
				t.Fatalf("retry did not recover or retention changed: remaining=%v queries=%d", remaining, store.queries)
			}
		})
	}
}

func TestExpiredEventCleanupContinuesWithProgressAndPreservesRecentImages(t *testing.T) {
	store := newProgressCleanupEventStore(t)
	core := NewCore(cleanupStore{events: store}, nil)
	// 只在测试拥有的子目录放图片；不改变应用的既有图片根目录规则。
	imagesRoot := filepath.Join(system.Getwd(), "configs", "events")
	if err := os.MkdirAll(imagesRoot, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(imagesRoot, "cleanup-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	items := make([]Event, 205)
	for i := range items {
		image := fmt.Sprintf("expired-%d.jpg", i)
		items[i] = Event{StartedAt: orm.Time{Time: time.Now().AddDate(0, 0, -10)}, ImagePath: filepath.Join(filepath.Base(dir), image)}
		if i%2 == 0 {
			if err := os.WriteFile(filepath.Join(dir, image), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.db.CreateInBatches(&items, 100).Error; err != nil {
		t.Fatal(err)
	}
	recent := Event{StartedAt: orm.Time{Time: time.Now()}, ImagePath: filepath.Join(filepath.Base(dir), "recent.jpg")}
	if err := store.db.Create(&recent).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "recent.jpg"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	core.cleanupExpiredEvents(7)
	var remaining []Event
	if err := store.db.Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != recent.ID || store.queries != 4 {
		t.Fatalf("retention or batching changed: remaining=%v queries=%d", remaining, store.queries)
	}
	for _, item := range items {
		if _, err := os.Stat(filepath.Join(imagesRoot, item.ImagePath)); !os.IsNotExist(err) {
			t.Fatalf("expired image remains: %s %v", item.ImagePath, err)
		}
	}
	if body, err := os.ReadFile(filepath.Join(dir, "recent.jpg")); err != nil || string(body) != "keep" {
		t.Fatal("recent event image changed")
	}
}
