package smsdb

import (
	"context"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

func TestMediaServerGet(t *testing.T) {
	db, mock, err := generateMockDB()
	if err != nil {
		t.Fatal(err)
	}
	userDB := NewMediaServer(db)

	mock.ExpectQuery(`SELECT \* FROM "media_servers" WHERE id=\$1 (.+) LIMIT \$2`).WithArgs("jack", 1).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("jack"))
	var out sms.MediaServer
	if err := userDB.Get(context.Background(), &out, orm.Where("id=?", "jack")); err != nil {
		t.Fatal(err)
	}
	if out.ID != "jack" {
		t.Fatalf("unexpected id: %q", out.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("ExpectationsWereMet err:", err)
	}
}

// 模拟旧库没有 disabled 列，升级不得清空配置或将现有节点停用。
func TestMediaServerDisabledMigrationPreservesExistingNode(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	NewDB(db).AutoMigrate(true)
	if err := db.Migrator().DropColumn(&sms.MediaServer{}, "disabled"); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO media_servers (id, ip, secret) VALUES (?, ?, ?)", "local", "192.0.2.20", "existing-secret").Error; err != nil {
		t.Fatal(err)
	}
	NewDB(db).AutoMigrate(true)
	var node sms.MediaServer
	if err := db.First(&node, "id = ?", "local").Error; err != nil {
		t.Fatal(err)
	}
	if node.Disabled || node.IP != "192.0.2.20" || node.Secret != "existing-secret" {
		t.Fatal("migration changed existing node configuration")
	}
}
