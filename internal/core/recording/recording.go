package recording

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/reason"
	"github.com/jinzhu/copier"
	"gorm.io/gorm"
)

// RecordingStorer Instantiation interface
type RecordingStorer interface {
	List(context.Context, *[]*Recording, orm.Pager, ...orm.QueryOption) (int64, error)
	Get(context.Context, *Recording, ...orm.QueryOption) error
	Create(context.Context, *Recording) error
	Update(context.Context, *Recording, func(*Recording), ...orm.QueryOption) error
	Delete(context.Context, *Recording, ...orm.QueryOption) error
	Count(context.Context, ...orm.QueryOption) (int64, error)

	Session(context.Context, ...func(*gorm.DB) error) error
	UpdateWithSession(*gorm.DB, *Recording, func(b *Recording) error, ...orm.QueryOption) error
}

// ListRecordings 分页查询录像列表，支持通道ID和时间范围筛选
func (c Core) ListRecordings(ctx context.Context, in *FindRecordingInput) ([]*Recording, int64, error) {
	query := orm.NewQuery(4).OrderBy("started_at DESC")

	if in.CID != "" {
		query.Where("cid = ?", in.CID)
	}
	if in.App != "" {
		query.Where("app = ?", in.App)
	}
	if in.Stream != "" {
		query.Where("stream = ?", in.Stream)
	}
	if in.StartMs > 0 && in.EndMs > 0 {
		query.Where("started_at >= ? AND ended_at <= ?", in.StartAt(), in.EndAt())
	}

	items := make([]*Recording, 0, in.Limit())
	total, err := c.store.Recording().List(ctx, &items, in, query.Encode()...)
	if err != nil {
		return nil, 0, reason.ErrDB.Withf(`Find in[%+v] err[%s]`, in, err.Error())
	}
	return items, total, nil
}

// GetRecording Query a single object
func (c Core) GetRecording(ctx context.Context, id int64) (*Recording, error) {
	out := Recording{ID: id}
	if err := c.store.Recording().Get(ctx, &out, orm.Where("id=?", id)); err != nil {
		if orm.IsErrRecordNotFound(err) {
			return nil, reason.ErrNotFound.Withf(`Get id[%v] err[%s]`, id, err.Error())
		}
		return nil, reason.ErrDB.Withf(`Get id[%v] err[%s]`, id, err.Error())
	}
	return &out, nil
}

// CreateRecording Insert into database
func (c Core) CreateRecording(ctx context.Context, in *AddRecordingInput) (*Recording, error) {
	var out Recording
	if err := copier.Copy(&out, in); err != nil {
		slog.ErrorContext(ctx, "Copy", "err", err)
	}

	if out.MediaServerID != "" && out.FilePath != "" {
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(out.MediaServerID+"\x00"+normalizeVhost(out.Vhost)+"\x00"+out.FilePath+"\x00"+strconv.FormatInt(out.StartedAt.Unix(), 10))))
		out.FileKey = &key
	}
	if err := c.store.Recording().Create(ctx, &out); err != nil {
		return nil, reason.ErrDB.Withf(`Create err[%s]`, err.Error())
	}
	return &out, nil
}

// UpdateRecording Update object information
func (c Core) UpdateRecording(ctx context.Context, in *EditRecordingInput, id int64) (*Recording, error) {
	var out Recording
	if err := c.store.Recording().Update(ctx, &out, func(b *Recording) {
		if err := copier.Copy(b, in); err != nil {
			slog.ErrorContext(ctx, "Copy", "err", err)
		}
	}, orm.Where("id=?", id)); err != nil {
		return nil, reason.ErrDB.Withf(`Edit id[%v] err[%s]`, id, err.Error())
	}
	return &out, nil
}

// DeleteRecording Delete object
func (c Core) DeleteRecording(ctx context.Context, id int64) (*Recording, error) {
	rec, err := c.GetRecording(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := c.removeRecordingFile(ctx, rec); err != nil {
		return nil, reason.ErrServer.WithMsg("录像文件删除失败，已保留记录: " + err.Error())
	}
	out := *rec
	if err := c.store.Recording().Delete(ctx, &out, orm.Where("id=?", id)); err != nil {
		return nil, reason.ErrDB.Withf(`Del id[%v] err[%s]`, id, err.Error())
	}
	return &out, nil
}

// GetTimeline 获取时间轴数据，返回指定时间范围内的录像时段列表
func (c Core) GetTimeline(ctx context.Context, in *TimelineInput) ([]TimeRange, error) {
	if in.CID == "" {
		return nil, reason.ErrBadRequest.Withf("cid is required")
	}
	if in.StartMs <= 0 || in.EndMs <= in.StartMs {
		return nil, reason.ErrBadRequest.Withf("a valid start_ms and end_ms range is required")
	}

	result := make([]TimeRange, 0)
	err := c.store.Recording().Session(ctx, func(db *gorm.DB) error {
		// 时间轴返回整个查询范围；仅逐行读取展示字段，避免分页上限及完整模型的额外内存。
		rows, err := db.Model(&Recording{}).
			Select("id, started_at, ended_at, duration, object_count, delete_flag").
			Where("cid = ? AND started_at < ? AND ended_at > ?", in.CID, in.EndAt(), in.StartAt()).
			Order("started_at ASC, id ASC").Rows()
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Recording
			if err := rows.Scan(&r.ID, &r.StartedAt, &r.EndedAt, &r.Duration, &r.ObjectCount, &r.DeleteFlag); err != nil {
				return err
			}
			result = append(result, TimeRange{
				ID: r.ID, StartMs: r.StartedAt.UnixMilli(), EndMs: r.EndedAt.UnixMilli(),
				Duration: r.Duration, ObjectCount: r.ObjectCount, DeleteFlag: r.DeleteFlag,
			})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, reason.ErrDB.Withf(`GetTimeline err[%s]`, err.Error())
	}
	return result, nil
}

// cidCount 用于接收 GROUP BY 查询结果
type cidCount struct {
	CID   string `gorm:"column:cid"`
	Count int64  `gorm:"column:cnt"`
}

// HasRecordings 批量检查通道是否有录像
// 使用 WHERE IN + GROUP BY 一次性查询所有通道是否有录像
func (c Core) HasRecordings(ctx context.Context, cids []string) (map[string]bool, error) {
	result := make(map[string]bool, len(cids))
	if len(cids) == 0 {
		return result, nil
	}

	// 使用 Session 执行自定义 SQL：SELECT cid, COUNT(*) as cnt FROM recordings WHERE cid IN (?) GROUP BY cid
	var counts []cidCount
	err := c.store.Recording().Session(ctx, func(db *gorm.DB) error {
		return db.Model(&Recording{}).
			Select("cid, COUNT(*) as cnt").
			Where("cid IN ?", cids).
			Group("cid").
			Find(&counts).Error
	})
	if err != nil {
		return result, err
	}

	// 转换结果
	for _, c := range counts {
		result[c.CID] = c.Count > 0
	}
	return result, nil
}

// GetMonthlyStats 获取月度录像统计
// 返回指定月份每天是否有录像的位图字符串
func (c Core) GetMonthlyStats(ctx context.Context, in *MonthlyStatsInput) (*MonthlyStatsOutput, error) {
	if in.Year <= 0 || in.Month < 1 || in.Month > 12 {
		return nil, reason.ErrBadRequest.Withf("invalid year or month")
	}

	// 按服务所在时区生成半开日期范围，不依赖数据库会话时区或时间字符串格式。
	firstDay := time.Date(in.Year, time.Month(in.Month), 1, 0, 0, 0, 0, time.Local)
	daysInMonth := firstDay.AddDate(0, 1, -1).Day()
	queries := make([]string, 0, daysInMonth)
	args := make([]any, 0, daysInMonth*3)
	for day := 1; day <= daysInMonth; day++ {
		start := firstDay.AddDate(0, 0, day-1)
		query := "SELECT " + strconv.Itoa(day) + " AS day WHERE EXISTS (SELECT 1 FROM recordings WHERE started_at >= ? AND started_at < ?"
		args = append(args, orm.Time{Time: start}, orm.Time{Time: start.AddDate(0, 0, 1)})
		if in.CID != "" {
			query += " AND cid = ?"
			args = append(args, in.CID)
		}
		queries = append(queries, query+")")
	}
	// 一次查询最多返回 31 个日期。每个 EXISTS 命中首条即停止，兼容 PG、MySQL 和 SQLite。
	var days []struct{ Day int }
	err := c.store.Recording().Session(ctx, func(db *gorm.DB) error {
		return db.Raw(strings.Join(queries, " UNION ALL "), args...).Scan(&days).Error
	})
	if err != nil {
		return nil, reason.ErrDB.Withf(`GetMonthlyStats err[%s]`, err.Error())
	}
	bitmap := make([]byte, daysInMonth)
	for i := range bitmap {
		bitmap[i] = '0'
	}
	for _, item := range days {
		bitmap[item.Day-1] = '1'
	}

	return &MonthlyStatsOutput{
		Year:     in.Year,
		Month:    in.Month,
		Days:     daysInMonth,
		HasVideo: string(bitmap),
	}, nil
}

func (c Core) RecordingsInRange(ctx context.Context, cid string, startMs, endMs int64) ([]*Recording, error) {
	if cid == "" || startMs <= 0 || endMs <= startMs {
		return nil, reason.ErrBadRequest.WithMsg("invalid recording time range")
	}
	var result []*Recording
	err := c.store.Recording().Session(ctx, func(tx *gorm.DB) error {
		return tx.Where("cid = ? AND started_at < ? AND ended_at > ?", cid, orm.Time{Time: time.UnixMilli(endMs)}, orm.Time{Time: time.UnixMilli(startMs)}).Order("started_at ASC, id ASC").Find(&result).Error
	})
	return result, err
}
