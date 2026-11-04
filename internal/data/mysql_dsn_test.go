package data

import (
	"github.com/glebarez/sqlite"
	driver "github.com/go-sql-driver/mysql"
	"path/filepath"
	"testing"
)

func TestDatabaseDSNCompatibility(t *testing.T) {
	for _, raw := range []string{"mysql://user:p%40ss@db.example:3307/owl?sslmode=disable", "mysql:user:p@tcp(db.example:3307)/owl?parseTime=true"} {
		normalized, e := NormalizeMySQLDSN(raw)
		if e != nil {
			t.Fatal("normalization failed")
		}
		cfg, e := driver.ParseDSN(normalized)
		if e != nil || cfg.User != "user" || cfg.Addr != "db.example:3307" || cfg.DBName != "owl" || !cfg.ParseTime {
			t.Fatal("incompatible MySQL DSN")
		}
	}
	path := filepath.Join(t.TempDir(), "database.db")
	dial, isSQLite, e := getDialector(path)
	if e != nil || !isSQLite || dial.(*sqlite.Dialector).DSN != path {
		t.Fatal("absolute SQLite path changed")
	}
}
