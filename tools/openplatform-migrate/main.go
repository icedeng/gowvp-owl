// openplatform-migrate 仅新增开放平台表或重新加密密钥，不启动设备及回调服务。
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
	op "github.com/gowvp/owl/internal/core/openplatform"
	"github.com/gowvp/owl/internal/data"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	rewrap := flag.Bool("rewrap", false, "使用当前主密钥重加密全部应用和回调密钥；加载完整新旧密钥环")
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
	if e = op.Migrate(db); e != nil {
		fatal("开放平台表迁移失败，请检查数据库权限与结构")
	}
	if *rewrap {
		cfg, e := op.ConfigFromEnv(os.TempDir())
		if e != nil || !cfg.Enabled {
			fatal("重加密需要启用配置及完整新旧密钥环")
		}
		s := &op.Service{DB: db, Config: cfg}
		n, e := s.Rewrap()
		if e != nil {
			fatal("重加密失败，事务已回滚；检查密钥环并保留旧版本")
		}
		fmt.Printf("已迁移开放平台表，验证并重加密 %d 组密钥\n", n)
	} else {
		fmt.Println("已迁移开放平台表；请配置独立密钥环后启用服务")
	}
}
func fatal(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(1) }
