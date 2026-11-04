// db-migrate 将停服后的 SQLite 一致性快照迁移到空 PostgreSQL 数据库。
// 所有表在一个事务中创建、导入并核对，失败回滚；连接串只从环境读取。
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/config"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/metadata"
	op "github.com/gowvp/owl/internal/core/openplatform"
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/domain/version"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var models = append([]any{
	new(version.Version), new(sms.MediaServer), new(ipc.Device), new(ipc.Channel),
	new(ipc.GBTaskStateRecord), new(ipc.GBCascadeTaskRouteRecord), new(ipc.DeviceHistoryRecord),
	new(uniqueid.UniqueID), new(recording.Recording), new(event.Event), new(config.Config), new(metadata.Metadata),
}, op.Models()...)

func main() {
	source := flag.String("source", "", "停服后的 SQLite 一致性快照绝对路径")
	check := flag.Bool("check", false, "仅核对，不导入或修改目标数据库")
	flag.Parse()
	dsn := os.Getenv("OWL_DATABASE_DSN")
	if !filepath.IsAbs(*source) || !strings.HasPrefix(dsn, "postgres") {
		fmt.Fprintln(os.Stderr, "需要 -source 绝对路径和环境变量 OWL_DATABASE_DSN（PostgreSQL URL）")
		os.Exit(1)
	}
	if err := migrate(context.Background(), *source, dsn, *check, os.Stdout); err != nil {
		// 驱动错误可能包含连接凭据，至少遮蔽完整连接串和 URL 密码。
		message := strings.ReplaceAll(err.Error(), dsn, "[database]")
		if i := strings.Index(dsn, "://"); i >= 0 {
			if end := strings.LastIndex(dsn, "@"); end > i+3 {
				message = strings.ReplaceAll(message, dsn[i+3:end], "[credentials]")
			}
		}
		fmt.Fprintln(os.Stderr, message)
		os.Exit(1)
	}
}

func migrate(ctx context.Context, source, dsn string, check bool, out io.Writer) error {
	if _, err := os.Stat(source); err != nil {
		return err
	}
	options := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), SkipDefaultTransaction: true}
	src, err := gorm.Open(sqlite.Open("file:"+source+"?mode=ro"), options)
	if err != nil {
		return fmt.Errorf("打开源快照: %w", err)
	}
	sourceDB, err := src.DB()
	if err != nil {
		return err
	}
	defer sourceDB.Close()
	var integrity string
	if err := src.Raw("PRAGMA integrity_check").Scan(&integrity).Error; err != nil || integrity != "ok" {
		return fmt.Errorf("源快照完整性检查失败: %s %v", integrity, err)
	}
	dst, err := gorm.Open(postgres.Open(dsn), options)
	if err != nil {
		return fmt.Errorf("连接目标 PostgreSQL: %w", err)
	}
	targetDB, err := dst.DB()
	if err != nil {
		return err
	}
	defer targetDB.Close()
	var tables []string
	if err := src.Raw("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name").Scan(&tables).Error; err != nil {
		return err
	}
	known := map[string]bool{}
	for _, model := range models {
		known[model.(interface{ TableName() string }).TableName()] = true
	}
	for _, table := range tables {
		if !known[table] {
			return fmt.Errorf("源库存在未适配表 %s，拒绝遗漏数据", table)
		}
	}
	return dst.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if !check {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(604280134)").Error; err != nil {
				return err
			}
			var existing []string
			if err := tx.Raw("SELECT tablename FROM pg_tables WHERE schemaname=current_schema()").Scan(&existing).Error; err != nil {
				return err
			}
			// 不接受预先存在的表，避免误覆盖、与正在运行的应用竞争或重复导入。
			if len(existing) != 0 {
				return fmt.Errorf("目标数据库已有表，拒绝导入，请使用独立空数据库")
			}
			if err := tx.AutoMigrate(models...); err != nil {
				return fmt.Errorf("创建 PostgreSQL 表结构: %w", err)
			}
		}
		for _, table := range tables {
			if err := copyTable(ctx, sourceDB, tx, table, check, out); err != nil {
				return fmt.Errorf("表 %s: %w", table, err)
			}
		}
		return nil
	})
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func copyTable(ctx context.Context, source *sql.DB, tx *gorm.DB, table string, check bool, out io.Writer) error {
	var columns []struct{ ColumnName, DataType string }
	if err := tx.Raw("SELECT column_name, data_type FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=?", table).Scan(&columns).Error; err != nil {
		return err
	}
	types := make(map[string]string)
	for _, column := range columns {
		types[column.ColumnName] = column.DataType
	}
	rows, err := source.QueryContext(ctx, "SELECT * FROM "+quote(table))
	if err != nil {
		return err
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return err
	}
	for _, name := range names {
		if types[name] == "" {
			return fmt.Errorf("目标缺少列 %s，拒绝丢弃字段", name)
		}
	}
	var hashes []string
	for rows.Next() {
		values := make([]any, len(names))
		pointers := make([]any, len(names))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		record := make(map[string]any, len(names))
		for i, name := range names {
			value, err := normalize(values[i], types[name])
			if err != nil {
				return fmt.Errorf("列 %s: %w", name, err)
			}
			record[name] = value
		}
		hash, err := hashRecord(record, types)
		if err != nil {
			return err
		}
		hashes = append(hashes, hash)
		if !check {
			if err := tx.Table(table).Create(record).Error; err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var imported []map[string]any
	if err := tx.Table(table).Select(names).Find(&imported).Error; err != nil {
		return err
	}
	var actual []string
	for _, record := range imported {
		hash, err := hashRecord(record, types)
		if err != nil {
			return err
		}
		actual = append(actual, hash)
	}
	sort.Strings(hashes)
	sort.Strings(actual)
	if strings.Join(hashes, "\n") != strings.Join(actual, "\n") {
		return fmt.Errorf("逐行内容校验失败（源 %d 行，目标 %d 行）", len(hashes), len(actual))
	}
	if !check {
		// 导入保留原 ID 后推进序列，避免新历史或录像与已有 ID 冲突。
		for _, name := range names {
			var sequence sql.NullString
			if err := tx.Raw("SELECT pg_get_serial_sequence(?, ?)", quote(table), name).Row().Scan(&sequence); err != nil {
				return err
			}
			if sequence.Valid {
				statement := "SELECT setval(?::regclass, GREATEST(COALESCE(MAX(" + quote(name) + "), 0), 1), COUNT(*) > 0) FROM " + quote(table)
				if err := tx.Exec(statement, sequence.String).Error; err != nil {
					return err
				}
			}
		}
	}
	fmt.Fprintf(out, "VERIFIED %s rows=%d\n", table, len(hashes))
	return nil
}

func normalize(value any, kind string) (any, error) {
	if value == nil {
		return nil, nil
	}
	if bytes, ok := value.([]byte); ok {
		value = string(bytes)
	}
	switch kind {
	case "boolean":
		switch v := value.(type) {
		case bool:
			return v, nil
		case int64:
			if v == 0 || v == 1 {
				return v == 1, nil
			}
		}
		return nil, fmt.Errorf("非法布尔值")
	case "timestamp with time zone", "timestamp without time zone":
		if v, ok := value.(time.Time); ok {
			return v.UTC().Truncate(time.Microsecond), nil
		}
		if v, ok := value.(string); ok {
			for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999 -0700 MST", "2006-01-02 15:04:05.999999999"} {
				if parsed, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
					return parsed.UTC().Truncate(time.Microsecond), nil
				}
			}
		}
		return nil, fmt.Errorf("无法转换时间类型")
	}
	return value, nil
}

func hashRecord(record map[string]any, types map[string]string) (string, error) {
	canonical := make(map[string]any, len(record))
	for name, value := range record {
		v, err := normalize(value, types[name])
		if err != nil {
			return "", err
		}
		if types[name] == "jsonb" || types[name] == "json" {
			if text, ok := v.(string); ok {
				decoder := json.NewDecoder(strings.NewReader(text))
				decoder.UseNumber()
				if err := decoder.Decode(&v); err != nil {
					return "", fmt.Errorf("非法 JSON 列 %s", name)
				}
			}
		}
		canonical[name] = v
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded)), nil
}
