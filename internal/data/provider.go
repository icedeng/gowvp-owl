package data

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/glebarez/sqlite"
	"github.com/google/wire"
	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/system"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// ProviderSet is data providers.
var ProviderSet = wire.NewSet(SetupDB)

// SetupDB 初始化数据存储
func SetupDB(c *conf.Bootstrap) (*gorm.DB, error) {
	cfg := c.Data.Database
	// 部署凭据仅由环境注入，不写回可通过管理端修改的主配置。
	if dsn := strings.TrimSpace(os.Getenv("OWL_DATABASE_DSN")); dsn != "" {
		cfg.Dsn = dsn
	}
	dial, isSQLite, err := getDialector(cfg.Dsn)
	if err != nil {
		return nil, err
	}
	if isSQLite {
		cfg.MaxIdleConns = 1
		cfg.MaxOpenConns = 1
	}
	db, err := orm.New(dial, orm.Config{
		MaxIdleConns:    int(cfg.MaxIdleConns),
		MaxOpenConns:    int(cfg.MaxOpenConns),
		ConnMaxLifetime: cfg.ConnMaxLifetime.Duration(),
		SlowThreshold:   cfg.SlowThreshold.Duration(),
	})
	if err != nil {
		return nil, err
	}

	return db, nil
}

// getDialector 返回 dial 和 是否 sqlite
func getDialector(dsn string) (gorm.Dialector, bool, error) {
	switch true {
	case strings.HasPrefix(dsn, "postgres"):
		return postgres.New(postgres.Config{
			DriverName: "pgx",
			DSN:        dsn,
		}), false, nil
	case strings.HasPrefix(dsn, "mysql"):
		normalized, e := NormalizeMySQLDSN(dsn)
		if e != nil {
			return nil, false, e
		}
		return mysql.Open(normalized), false, nil
	default:
		if filepath.IsAbs(dsn) {
			return sqlite.Open(dsn), true, nil
		}
		return sqlite.Open(filepath.Join(system.Getwd(), dsn)), true, nil
	}
}
