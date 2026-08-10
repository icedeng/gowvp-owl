package proxydb

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gowvp/owl/internal/core/proxy"
	"github.com/ixugo/goddd/pkg/orm"
)

func TestStreamProxyGet(t *testing.T) {
	db, mock, err := generateMockDB()
	if err != nil {
		t.Fatal(err)
	}
	userDB := NewStreamProxy(db)

	mock.ExpectQuery(`SELECT \* FROM "stream_proxys" WHERE id=\$1 (.+) LIMIT \$2`).WithArgs("jack", 1).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("jack"))
	var out proxy.StreamProxy
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
