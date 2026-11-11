package service

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

type draftRuntime struct{ calls int }

func (r *draftRuntime) Snapshot(string) core.RuntimeSnapshot {
	return core.RuntimeSnapshot{ConnectionState: "disconnected", HealthState: "unknown", Capabilities: []string{}}
}
func (r *draftRuntime) Call(context.Context, string, string, any) (any, error) {
	r.calls++
	return nil, core.Fail(503, "UNAVAILABLE", "test runtime")
}
func (r *draftRuntime) Reconcile(context.Context) error         { return nil }
func (r *draftRuntime) OwnerLease(string) (string, int64, bool) { return "", 0, false }
func (r *draftRuntime) Reconnect(context.Context, string) error { r.calls++; return nil }
func (r *draftRuntime) Close() error                            { return nil }

func TestDraftRevisionIdempotencyAndDeletionGuards(t *testing.T) {
	db, e := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal(e)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	if e = migrations.Apply(db); e != nil {
		t.Fatal(e)
	}
	runtime := &draftRuntime{}
	s := New(context.Background(), core.Config{Enabled: true, PlatformID: "platform-a", MaxMediaSessions: 128, MaxTransferSessions: 16}, store.New(db), runtime, nil, nil)
	t.Cleanup(func() { s.Close() })
	config := core.DesiredConfig{EffectiveConfig: protocol.DefaultEffectiveConfig(), Name: "公网网关一", URL: "wss://gateway.example.com/gateway/v1/control", PlatformID: "platform-a", RequiredCapabilities: []string{}}
	config.PublicIP = "8.8.8.8"
	config.SIPPort = 15060
	config.MediaPortStart = 30000
	config.MediaPortEnd = 30199
	ctx := context.Background()
	create := core.CreateRequest{ID: "gateway-b1", Config: config}
	m := core.Mutation{Actor: "admin", Route: "POST /access_gateways", Key: uuid.NewString()}
	first, e := s.Create(ctx, m, create)
	if e != nil || first.Status != 200 {
		t.Fatalf("create: %v %+v", e, first)
	}
	if _, e = s.Create(ctx, m, create); e != nil {
		t.Fatalf("create retry: %v", e)
	}
	var n int64
	db.Model(&store.Gateway{}).Count(&n)
	if n != 1 {
		t.Fatalf("duplicate gateway: %d", n)
	}
	db.Model(&store.Operation{}).Count(&n)
	if n != 1 {
		t.Fatalf("duplicate operation: %d", n)
	}
	view, e := s.Get(ctx, create.ID)
	if e != nil || view.Revision != "1" || view.ConfigRevision != "0" || view.AppliedRevision != "0" {
		t.Fatalf("initial revisions: %v %+v", e, view)
	}
	revision := int64(1)
	config.Description = "改备注不触发远端配置"
	update := core.Mutation{Actor: "admin", Route: "PUT /access_gateways/:id", Key: uuid.NewString(), ExpectedRevision: &revision}
	if _, e = s.Update(ctx, update, create.ID, core.UpdateRequest{Config: config}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Update(ctx, update, create.ID, core.UpdateRequest{Config: config}); e != nil {
		t.Fatalf("same update retry failed: %v", e)
	}
	conflicting := config
	conflicting.Name = "不同请求"
	if _, e = s.Update(ctx, update, create.ID, core.UpdateRequest{Config: conflicting}); e == nil {
		t.Fatal("same key accepted different content")
	}
	update.Key = uuid.NewString()
	if _, e = s.Update(ctx, update, create.ID, core.UpdateRequest{Config: config}); e == nil {
		t.Fatal("stale revision accepted")
	}
	view, _ = s.Get(ctx, create.ID)
	if view.Revision != "2" || view.ConfigRevision != "0" {
		t.Fatalf("draft mutation revisions: %+v", view)
	}
	row, _ := s.repo.Get(ctx, create.ID)
	subject := "device-internal"
	if e = db.Create(&store.Binding{Base: store.Base{ID: uuid.NewString()}, BindingFields: store.BindingFields{SubjectID: subject, IngressType: "gateway", GatewayID: &row.ID, BindingRevision: 1}}).Error; e != nil {
		t.Fatal(e)
	}
	revision = 2
	remove := core.Mutation{Actor: "admin", Route: "DELETE /access_gateways/:id", Key: uuid.NewString(), ExpectedRevision: &revision}
	if _, e = s.Delete(ctx, remove, create.ID); e == nil {
		t.Fatal("deleted gateway with device reference")
	}
	db.Where("subject_id = ?", subject).Delete(&store.Binding{})
	if _, e = s.Delete(ctx, remove, create.ID); e != nil {
		t.Fatal(e)
	}
	m.Key = uuid.NewString()
	if _, e = s.Create(ctx, m, create); e == nil {
		t.Fatal("deleted stable ID was reused")
	}
	if runtime.calls != 0 {
		t.Fatalf("draft contacted gateway %d times", runtime.calls)
	}
}

type rotationRuntime struct {
	draftRuntime
	owner    string
	epoch    int64
	snapshot core.RuntimeSnapshot
}

func (r *rotationRuntime) OwnerLease(string) (string, int64, bool) { return r.owner, r.epoch, true }
func (r *rotationRuntime) Snapshot(string) core.RuntimeSnapshot    { return r.snapshot }
func (r *rotationRuntime) Reconnect(context.Context, string) error {
	r.snapshot = core.RuntimeSnapshot{ConnectionState: "connected", HealthState: "ready"}
	return nil
}
func (r *rotationRuntime) Call(context.Context, string, string, any) (any, error) {
	r.calls++
	return protocol.ResumeResult{DrainState: "enabled"}, nil
}

func TestCertificateRotationFailureRestoresOldCredential(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	if err = migrations.Apply(db); err != nil {
		t.Fatal(err)
	}
	oldID, newID := uuid.NewString(), uuid.NewString()
	previous := core.DesiredConfig{EffectiveConfig: protocol.DefaultEffectiveConfig(), Name: "b1", URL: "wss://gateway.example.com/gateway/v1/control", CredentialID: &oldID}
	candidate := previous
	candidate.CredentialID = &newID
	operationID := uuid.NewString()
	row := store.Gateway{Base: store.Base{ID: uuid.NewString()}, StableID: "b1", Name: "b1", AdminState: "draining", Revision: 2, ConfigRevision: 1, AppliedRevision: 1, DesiredConfig: candidate, CredentialID: &newID, PendingOperationID: &operationID}
	if err = db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	repo := store.New(db)
	epoch, err := repo.AcquireOwner(t.Context(), row.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(-time.Second)
	op := store.Operation{Base: store.Base{ID: operationID}, GatewayID: &row.ID, Type: "certificate_rotation", Status: "running", Step: "drained", Revision: 1, DeadlineAt: &deadline, RequestSnapshot: encode(pending{Previous: &previous, Candidate: &candidate, State: "enabled", TargetRevision: 1})}
	if err = db.Create(&op).Error; err != nil {
		t.Fatal(err)
	}
	runtime := &rotationRuntime{owner: "owner", epoch: epoch, snapshot: core.RuntimeSnapshot{ConnectionState: "reconnecting", HealthState: "unavailable", Error: protocol.Failure(protocol.Forbidden, "new certificate rejected")}}
	service := New(t.Context(), core.Config{}, repo, runtime, nil, nil)
	defer service.Close()
	service.advance(op)
	if err = db.First(&op, "id = ?", op.ID).Error; err != nil {
		t.Fatal(err)
	}
	restored, err := repo.GetPK(t.Context(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op.Step != "rollback_connect" || pointerValue(restored.CredentialID) != oldID || restored.AppliedRevision != 1 {
		t.Fatalf("rollback not persisted: %+v %+v", op, restored)
	}
	service.advance(op)
	if err = db.First(&op, "id = ?", op.ID).Error; err != nil {
		t.Fatal(err)
	}
	restored, _ = repo.GetPK(t.Context(), row.ID)
	if op.Status != "failed" || op.Step != "rolled_back" || restored.AdminState != "enabled" || restored.PendingOperationID != nil || runtime.calls != 1 {
		t.Fatalf("rollback did not converge: %+v %+v calls=%d", op, restored, runtime.calls)
	}
}
