package recording_test

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/gowvp/owl/internal/core/recording/store/recordingdb"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRecordingQueries(t *testing.T) {
	// 月统计沿用服务时区，测试数据库会话保持 UTC，验证跨时区的日期边界。
	previousLocation := time.Local
	time.Local = time.FixedZone("CST", 8*60*60)
	t.Cleanup(func() { time.Local = previousLocation })
	for _, engine := range []string{"sqlite", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			db := openQueryDatabase(t, engine)
			core := recording.NewCore(recordingdb.NewDB(db))
			t.Run("timeline_exceeds_1000", func(t *testing.T) {
				start := time.Date(2026, 11, 4, 0, 0, 0, 0, time.Local)
				const count = 1105
				items := make([]recording.Recording, count)
				for i := range items {
					items[i] = queryRecording("timeline", start.Add(time.Duration(i/2)*time.Second))
					items[i].ObjectCount = i
					items[i].DeleteFlag = i%2 == 0
				}
				if err := db.CreateInBatches(&items, 100).Error; err != nil {
					t.Fatal(err)
				}
				end := start.Add(time.Hour)
				extra := []recording.Recording{
					queryRecording("other", start),
					queryRecording("timeline", start.Add(-time.Minute)), // 恰好在范围开始前结束。
					queryRecording("timeline", end),
					queryRecording("timeline", start.Add(-time.Second)), // 跨越范围开始，仍应返回。
				}
				if err := db.Create(&extra).Error; err != nil {
					t.Fatal(err)
				}
				in := &recording.TimelineInput{CID: "timeline", DateFilter: web.DateFilter{StartMs: start.UnixMilli(), EndMs: end.UnixMilli()}}
				got, err := core.GetTimeline(t.Context(), in)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != count+1 || got[0].ID != extra[3].ID {
					t.Fatalf("时间轴截断或边界筛选错误: count=%d", len(got))
				}
				for i, want := range items {
					row := got[i+1]
					if row.ID != want.ID || row.StartMs != want.StartedAt.UnixMilli() || row.EndMs != want.EndedAt.UnixMilli() ||
						row.Duration != want.Duration || row.ObjectCount != want.ObjectCount || row.DeleteFlag != want.DeleteFlag {
						t.Fatalf("时间轴第 %d 条内容或顺序不一致: %+v", i, row)
					}
				}
				in.CID = "empty"
				got, err = core.GetTimeline(t.Context(), in)
				if err != nil || got == nil || len(got) != 0 {
					t.Fatalf("空时间轴 = %v, %v", got, err)
				}
				in.EndMs = in.StartMs
				if _, err := core.GetTimeline(t.Context(), in); err == nil {
					t.Fatal("未拒绝无效时间范围")
				}
			})
			t.Run("monthly_exceeds_10000", func(t *testing.T) {
				start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
				items := make([]recording.Recording, 10001)
				for i := range items {
					items[i] = queryRecording("monthly", start.Add(time.Duration(i)*time.Second))
				}
				if err := db.CreateInBatches(&items, 100).Error; err != nil {
					t.Fatal(err)
				}
				fixtures := []recording.Recording{
					queryRecording("monthly", start.AddDate(0, 1, 0).Add(-time.Microsecond)),
					queryRecording("monthly", start.AddDate(0, 1, 0)),
					queryRecording("monthly", start.Add(-time.Microsecond)),
					queryRecording("other", start.AddDate(0, 0, 14)),
					queryRecording("monthly", start.AddDate(0, 0, 1)),
				}
				if engine == "postgres" {
					// PG timestamptz 以绝对时间比较，写入 UTC 的本地 2 日零点仍须算在第 2 日。
					fixtures[4].StartedAt.Time = fixtures[4].StartedAt.UTC()
				}
				if err := db.Create(&fixtures).Error; err != nil {
					t.Fatal(err)
				}
				for _, test := range []struct{ cid, bitmap string }{
					{"monthly", "11" + strings.Repeat("0", 28) + "1"},
					{"", "11" + strings.Repeat("0", 12) + "1" + strings.Repeat("0", 15) + "1"},
					{"empty", strings.Repeat("0", 31)},
				} {
					got, err := core.GetMonthlyStats(t.Context(), &recording.MonthlyStatsInput{CID: test.cid, Year: 2026, Month: 10})
					if err != nil {
						t.Fatal(err)
					}
					if got.Days != 31 || got.HasVideo != test.bitmap {
						t.Fatalf("通道 %q 月统计 = %+v, want %s", test.cid, got, test.bitmap)
					}
				}
				got, err := core.GetMonthlyStats(t.Context(), &recording.MonthlyStatsInput{Year: 2024, Month: 2})
				if err != nil || got == nil || got.Days != 29 || got.HasVideo != strings.Repeat("0", 29) {
					t.Fatalf("闰月统计 = %+v, %v", got, err)
				}
			})
			t.Run("indexes", func(t *testing.T) {
				for _, index := range []struct {
					model any
					name  string
				}{
					{&recording.Recording{}, "idx_recordings_cid_started_at"},
					{&ipc.Channel{}, "idx_channels_did_created_at"},
				} {
					if !db.Migrator().HasIndex(index.model, index.name) {
						t.Errorf("缺少复合索引 %s", index.name)
					}
				}
			})
		})
	}
}

func queryRecording(cid string, start time.Time) recording.Recording {
	return recording.Recording{CID: cid, StartedAt: orm.Time{Time: start}, EndedAt: orm.Time{Time: start.Add(time.Minute)}, Duration: 60}
}

func openQueryDatabase(t *testing.T, engine string) *gorm.DB {
	t.Helper()
	var dialector gorm.Dialector
	if engine == "postgres" {
		dsn := os.Getenv("OWL_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("需要独立测试 PostgreSQL 的 OWL_TEST_POSTGRES_DSN")
		}
		admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		adminDB, err := admin.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { adminDB.Close() })
		schema := fmt.Sprintf("recording_query_test_%d", time.Now().UnixNano())
		if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
				t.Error(err)
			}
		})
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("search_path", schema)
		q.Set("TimeZone", "UTC")
		u.RawQuery = q.Encode()
		dialector = postgres.Open(u.String())
	} else {
		dialector = sqlite.Open(filepath.Join(t.TempDir(), "query.db"))
	}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.AutoMigrate(&recording.Recording{}, &ipc.Channel{}); err != nil {
		t.Fatal(err)
	}
	return db
}
