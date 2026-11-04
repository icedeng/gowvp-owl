package main

import (
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 使用独立临时 schema，不连接生产库；真实验证 JSONB、时间、布尔值、序列和原子回滚。
func TestPostgresMigration(t *testing.T) {
	dsn := os.Getenv("OWL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("需要独立测试 PostgreSQL 的 OWL_TEST_POSTGRES_DSN")
	}
	options := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	db, err := gorm.Open(postgres.Open(dsn), options)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	schema := "migration_test_" + time.Now().Format("20060102150405_000000000")
	if err := db.Exec("CREATE SCHEMA " + quote(schema)).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP SCHEMA " + quote(schema) + " CASCADE")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	// GORM 的 TimeZone 检测使用原始 URL，不对斜杠做额外百分号编码。
	originalQuery := u.RawQuery
	u.RawQuery = originalQuery + "&search_path=" + url.QueryEscape(schema)
	target := u.String()
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	src, err := gorm.Open(sqlite.Open(snapshot), options)
	if err != nil {
		t.Fatal(err)
	}
	sourceDB, _ := src.DB()
	defer sourceDB.Close()
	if err := src.AutoMigrate(new(ipc.Device), new(ipc.DeviceHistoryRecord)); err != nil {
		t.Fatal(err)
	}
	confirmed := false
	device := ipc.Device{ID: "test-device", DeviceID: "50010700001110000001", IsOnline: true,
		Ext: ipc.DeviceExt{GBRegisterCallID: "private-register-state", GBRegistrationClosed: &confirmed}}
	if err := src.Create(&device).Error; err != nil {
		t.Fatal(err)
	}
	record := ipc.DeviceHistoryRecord{ID: 41, DeviceID: device.DeviceID, Kind: ipc.DeviceHistoryHeartbeat,
		RecordedAt: orm.Time{Time: time.Now()}, Address: "udp://127.0.0.1:5060", Status: "OK"}
	if err := src.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrate(context.Background(), snapshot, target, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := migrate(context.Background(), snapshot, target, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := migrate(context.Background(), snapshot, target, false, io.Discard); err == nil || !strings.Contains(err.Error(), "已有表") {
		t.Fatalf("重复迁移未拒绝: %v", err)
	}
	dst, err := gorm.Open(postgres.Open(target), options)
	if err != nil {
		t.Fatal(err)
	}
	targetDB, _ := dst.DB()
	defer targetDB.Close()
	var actual ipc.Device
	if err := dst.First(&actual, "id=?", device.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !actual.IsOnline || actual.Ext.GBRegisterCallID != device.Ext.GBRegisterCallID {
		t.Fatal("注册私有状态或布尔字段丢失")
	}
	record.ID = 0
	if err := dst.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	if record.ID <= 41 {
		t.Fatal("序列未推进")
	}
	// 在另一个空 schema 中注入坏时间，确认表结构和已插入数据一起回滚。
	broken := schema + "_broken"
	if err := db.Exec("CREATE SCHEMA " + quote(broken)).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP SCHEMA " + quote(broken) + " CASCADE")
	u.RawQuery = originalQuery + "&search_path=" + url.QueryEscape(broken)
	if err := src.Exec("UPDATE device_history_records SET recorded_at='invalid-time'").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrate(context.Background(), snapshot, u.String(), false, io.Discard); err == nil {
		t.Fatal("坏时间未拒绝")
	}
	var count int64
	db.Raw("SELECT count(*) FROM pg_tables WHERE schemaname=?", broken).Scan(&count)
	if count != 0 {
		t.Fatal("失败迁移留下了部分表")
	}
}
