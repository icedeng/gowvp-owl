package event

import (
	"context"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ixugo/goddd/pkg/orm"
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
