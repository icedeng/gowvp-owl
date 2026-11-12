// accessgateway-migrate 停服后迁移网关恢复及事件字段，不启动 SIP/媒体/回调服务。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/extensions/accessgateway/platform/migrations"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/data"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	check := flag.Bool("check", false, "只校验结构，不执行迁移")
	flag.Parse()
	dsn := os.Getenv("OWL_DATABASE_DSN")
	if dsn == "" {
		fatal("请通过 OWL_DATABASE_DSN 指向同一Owl数据库；SQLite请使用绝对路径")
	}
	var dial gorm.Dialector
	switch {
	case strings.HasPrefix(dsn, "postgres"):
		dial = postgres.Open(dsn)
	case strings.HasPrefix(dsn, "mysql"):
		normalized, e := data.NormalizeMySQLDSN(dsn)
		if e != nil {
			fatal("MySQL DSN格式错误")
		}
		dial = mysql.Open(normalized)
	default:
		if !filepath.IsAbs(dsn) {
			fatal("SQLite数据库须使用绝对路径")
		}
		if _, e := os.Stat(dsn); e != nil {
			fatal("SQLite数据库不存在；请先初始化Owl")
		}
		dial = sqlite.Open(dsn)
	}
	db, e := gorm.Open(dial, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableAutomaticPing: true})
	if e != nil {
		fatal("连接数据库失败，请检查DSN")
	}
	sqlDB, e := db.DB()
	if e != nil {
		fatal("打开数据库失败")
	}
	defer sqlDB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db = db.WithContext(ctx)
	if !db.Migrator().HasTable("devices") {
		fatal("目标不是已初始化的Owl数据库")
	}
	if *check {
		e = verify(db)
	} else {
		e = apply(db)
	}
	if e != nil {
		fatal("网关恢复表或事件字段迁移/校验失败；保留备份并检查结构，不启动新程序")
	}
	fmt.Println("网关恢复表及历史事件字段迁移/校验通过；原有报警与媒体路径保留")

}
func fatal(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(1) }

func apply(db *gorm.DB) error {
	migrate := func(tx *gorm.DB) error {
		if !tx.Migrator().HasTable(&event.Event{}) {
			return fmt.Errorf("events table missing")
		}
		if err := tx.Table("events").AutoMigrate(&recoveryEventColumns{}); err != nil {
			return err
		}
		if err := migrations.Apply(tx); err != nil {
			return err
		}
		return verify(tx)
	}
	if db.Dialector.Name() == "mysql" {
		return migrate(db)
	} // MySQL DDL 不保证事务回滚。
	return db.Transaction(migrate)
}
func verify(db *gorm.DB) error {
	if err := migrations.Verify(db); err != nil {
		return err
	}
	for _, column := range []string{"historical", "received_at", "gateway_id", "gateway_name"} {
		if !db.Migrator().HasColumn(&event.Event{}, column) {
			return fmt.Errorf("events 缺少字段 %s", column)
		}
	}
	return nil
}

// 只补本版本字段，避免 SQLite 为旧表添加 CURRENT_TIMESTAMP 列时失败。
type recoveryEventColumns struct {
	GatewayID   string `gorm:"size:64;not null;default:''"`
	GatewayName string `gorm:"size:80;not null;default:''"`
	Historical  bool   `gorm:"not null;default:false"`
	ReceivedAt  *time.Time
}
