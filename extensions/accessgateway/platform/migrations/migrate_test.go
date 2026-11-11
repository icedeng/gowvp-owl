package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/migrations"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSchemaReapplyUniquenessAndOwnerFencing(t *testing.T) {
	db, e := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal(e)
	}
	sql, e := db.DB()
	if e != nil {
		t.Fatal(e)
	}
	sql.SetMaxOpenConns(1)
	defer sql.Close()
	if e = migrations.Apply(db); e != nil {
		t.Fatal(e)
	}
	if e = migrations.Apply(db); e != nil {
		t.Fatal("migration not idempotent", e)
	}
	row := store.Gateway{Base: store.Base{ID: uuid.NewString()}, StableID: "B1", Name: "网关", GroupName: "", AdminState: "enabled", DesiredConfig: core.DesiredConfig{EffectiveConfig: protocol.DefaultEffectiveConfig()}, Revision: 1}
	if e = db.Create(&row).Error; e != nil {
		t.Fatal(e)
	}
	lower := row
	lower.ID = uuid.NewString()
	lower.StableID = "b1"
	if e = db.Create(&lower).Error; e != nil {
		t.Fatal("stable IDs should be case sensitive", e)
	}
	tombstone := time.Now().UTC()
	if e = db.Model(&row).Update("deleted_at", &tombstone).Error; e != nil {
		t.Fatal(e)
	}
	duplicate := row
	duplicate.ID = uuid.NewString()
	duplicate.DeletedAt = nil
	if e = db.Create(&duplicate).Error; e == nil {
		t.Fatal("deleted stable ID reused")
	}
	repo := store.New(db)
	ctx := context.Background()
	one := uuid.NewString()
	epoch, e := repo.AcquireOwner(ctx, lower.ID, one)
	if e != nil || epoch != 1 {
		t.Fatal("cannot acquire initial owner", epoch, e)
	}
	if _, e = repo.AcquireOwner(ctx, lower.ID, uuid.NewString()); e == nil {
		t.Fatal("concurrent owner accepted")
	}
	if e = repo.RenewOwner(ctx, lower.ID, one, epoch); e != nil {
		t.Fatal("cannot renew owner", e)
	}
	seen := time.Now().UTC().Truncate(time.Second)
	failure := protocol.Failure(protocol.IOError, "control connection closed")
	if e = repo.RuntimeCheckpoint(ctx, lower.ID, one, epoch, &seen, failure); e != nil {
		t.Fatal("cannot persist serialized runtime error", e)
	}
	var checkpoint store.Gateway
	if e = db.First(&checkpoint, "id = ?", lower.ID).Error; e != nil || checkpoint.LastError == nil || checkpoint.LastError.Code != failure.Code || checkpoint.LastSeenAt == nil || !checkpoint.LastSeenAt.Equal(seen) {
		t.Fatal("runtime error did not round-trip", e)
	}
	if e = repo.RuntimeCheckpoint(ctx, lower.ID, one, epoch, nil, nil); e != nil {
		t.Fatal("cannot clear runtime error", e)
	}
	checkpoint = store.Gateway{}
	if e = db.First(&checkpoint, "id = ?", lower.ID).Error; e != nil || checkpoint.LastError != nil || checkpoint.LastSeenAt != nil {
		t.Fatal("runtime error was not cleared", e)
	}
	if e = db.Model(&store.Gateway{}).Where("id = ?", lower.ID).Update("owner_expires_at", time.Now().UTC().Add(-time.Minute)).Error; e != nil {
		t.Fatal(e)
	}
	two := uuid.NewString()
	newEpoch, e := repo.AcquireOwner(ctx, lower.ID, two)
	if e != nil || newEpoch != 2 {
		t.Fatal("owner fencing did not advance", newEpoch, e)
	}
	if e = repo.RenewOwner(ctx, lower.ID, one, epoch); e == nil {
		t.Fatal("old owner renewed new lease")
	}
	if e = repo.GatewayRuntimeUpdate(ctx, lower.ID, one, epoch, map[string]any{"last_error": nil}); e == nil {
		t.Fatal("old owner wrote current state")
	}
	if e = db.Migrator().DropIndex(&store.Gateway{}, "idx_access_gateways_stable_id"); e != nil {
		t.Fatal(e)
	}
	if e = migrations.Verify(db); e == nil {
		t.Fatal("missing unique index not detected")
	}
	if e = migrations.Apply(db); e != nil {
		t.Fatal("cannot repair interrupted migration", e)
	}
}
