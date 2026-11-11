package owl

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/migrations"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipccache"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/gowvp/owl/pkg/gbs"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRegistrationBindingSharesHostTransaction(t *testing.T) {
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
	if e = db.AutoMigrate(&ipc.Device{}, &ipc.Channel{}); e != nil {
		t.Fatal(e)
	}
	device := ipc.Device{ID: "device-internal", DeviceID: "34020000001320000001", Type: ipc.TypeGB28181, Ext: ipc.DeviceExt{GBRegisterCallID: "old-direct-call", GBRegisterCSeq: 99}}
	if e = db.Create(&device).Error; e != nil {
		t.Fatal(e)
	}
	source := "old-gateway-pk"
	binding := store.Binding{Base: store.Base{ID: "binding"}, BindingFields: store.BindingFields{SubjectID: device.ID, IngressType: "gateway", GatewayID: &source, BindingRevision: 5, AuthenticatedAt: time.Now().UTC()}}
	if e = db.Create(&binding).Error; e != nil {
		t.Fatal(e)
	}
	host := New(db, nil, nil)
	cache := ipccache.NewCache(ipcdb.NewDB(db))
	cache.SetIngressGuard(host)
	ctx := ipc.WithRegistrationIngress(context.Background(), nil, 3600)
	change := func(d *ipc.Device) error { d.IsOnline = true; return nil }
	if e = cache.ChangeContext(ctx, device.DeviceID, change, nil); e == nil {
		t.Fatal("cross-ingress direct REGISTER accepted without migration")
	}
	var persisted ipc.Device
	if e = db.First(&persisted, "id = ?", device.ID).Error; e != nil || persisted.IsOnline {
		t.Fatalf("denied registration changed host state: %v %+v", e, persisted)
	}
	plan := store.Migration{Base: store.Base{ID: "migration"}, SourceGatewayID: &source, State: "waiting_register", ExpiresAt: time.Now().UTC().Add(time.Hour), Revision: 1, OperationID: "migration-op"}
	key := "active-subject"
	item := store.MigrationItem{Base: store.Base{ID: "migration-item"}, MigrationID: plan.ID, SubjectType: "device", SubjectID: device.ID, ExpectedBindingRevision: 5, State: "waiting_register", ActiveSubjectKey: &key, Revision: 1}
	if e = db.Create(&plan).Error; e != nil {
		t.Fatal(e)
	}
	if e = db.Create(&item).Error; e != nil {
		t.Fatal(e)
	}
	wantFailure := errors.New("host registration write failed")
	if e = cache.ChangeContext(ctx, device.DeviceID, func(*ipc.Device) error { return wantFailure }, nil); !errors.Is(e, wantFailure) {
		t.Fatalf("failure = %v", e)
	}
	var after store.Binding
	db.First(&after, "id = ?", binding.ID)
	if after.IngressType != "gateway" || after.BindingRevision != 5 {
		t.Fatalf("binding escaped host transaction rollback: %+v", after)
	}
	db.First(&item, "id = ?", item.ID)
	if item.State != "waiting_register" || item.ActiveSubjectKey == nil {
		t.Fatal("migration consumed on rollback")
	}
	if e = cache.ChangeContext(ctx, device.DeviceID, func(d *ipc.Device) error {
		if d.Ext.GBRegisterCallID != "" || d.Ext.GBRegisterCSeq != 0 {
			t.Fatal("old ingress CSeq baseline retained")
		}
		return change(d)
	}, nil); e != nil {
		t.Fatal(e)
	}
	db.First(&persisted, "id = ?", device.ID)
	db.First(&after, "id = ?", binding.ID)
	if !persisted.IsOnline || after.IngressType != "direct" || after.GatewayID != nil || after.BindingRevision != 6 {
		t.Fatalf("host/binding did not commit together: %+v %+v", persisted, after)
	}
	db.First(&item, "id = ?", item.ID)
	if item.State != "migrated" || item.ActiveSubjectKey != nil {
		t.Fatal("migration authorization not consumed atomically")
	}
}

func TestPlatformMigrationCommitsOnlyAfterAuthenticatedRegistration(t *testing.T) {
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
	host := New(db, nil, nil)
	ctx := context.Background()
	spec := gbs.ExternalPlatformSpec{SubjectID: "platform-internal", Register: true}
	if err = host.CommitPlatformRegistration(ctx, spec, nil, 3600); err != nil {
		t.Fatal(err)
	}
	binding, err := host.platformBinding(ctx, spec.SubjectID)
	if err != nil || binding.IngressType != "direct" || binding.Role != "upstream" || binding.BindingRevision != 1 {
		t.Fatalf("direct binding: %+v %v", binding, err)
	}
	// 模拟原网关已完成注册；目标是 direct，只有有效计划才能切换。
	source := "source-gateway"
	binding.IngressType, binding.GatewayID, binding.BindingRevision = "gateway", &source, 7
	if err = db.Save(&binding).Error; err != nil {
		t.Fatal(err)
	}
	if err = host.AuthorizePlatform(ctx, spec); err == nil {
		t.Fatal("cross-ingress registration accepted without plan")
	}
	key := "platform-migration-active"
	plan := store.Migration{Base: store.Base{ID: "platform-migration"}, SourceGatewayID: &source, State: "draining", ExpiresAt: time.Now().Add(time.Hour), Revision: 1, OperationID: "platform-migration-op"}
	item := store.MigrationItem{Base: store.Base{ID: "platform-migration-item"}, MigrationID: plan.ID, SubjectType: "platform", SubjectID: spec.SubjectID, ExpectedBindingRevision: 7, State: "draining", ActiveSubjectKey: &key, Revision: 1}
	if err = db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	var temporary *core.Error
	if err = host.AuthorizePlatform(ctx, spec); !errors.As(err, &temporary) || temporary.Status != 503 {
		t.Fatalf("drain authorization: %v", err)
	}
	if err = host.CommitPlatformRegistration(ctx, spec, nil, 3600); !errors.As(err, &temporary) || temporary.Status != 503 {
		t.Fatalf("early registration committed: %v", err)
	}
	if err = db.Model(&item).Update("state", "waiting_register").Error; err != nil {
		t.Fatal(err)
	}
	if err = host.AuthorizePlatform(ctx, spec); err != nil {
		t.Fatal(err)
	}
	after, _ := host.platformBinding(ctx, spec.SubjectID)
	if after.GatewayID == nil || after.BindingRevision != 7 {
		t.Fatal("authorization itself changed binding")
	}
	// 注册成功但持久化资源未排空，仍不得提交或消费授权。
	resource := store.Session{Base: store.Base{ID: "platform-active-session"}, GatewayID: source, RemoteID: "platform-remote", Generation: "generation", ConnectionEpoch: 1, SubjectType: "platform", SubjectID: spec.SubjectID, State: "closing"}
	if err = db.Create(&resource).Error; err != nil {
		t.Fatal(err)
	}
	if err = host.CommitPlatformRegistration(ctx, spec, nil, 3600); !errors.As(err, &temporary) || temporary.Status != 503 {
		t.Fatalf("active resource ignored: %v", err)
	}
	if err = db.Model(&resource).Update("state", "closed").Error; err != nil {
		t.Fatal(err)
	}
	if err = host.CommitPlatformRegistration(ctx, spec, nil, 3600); err != nil {
		t.Fatal(err)
	}
	after, _ = host.platformBinding(ctx, spec.SubjectID)
	if after.GatewayID != nil || after.IngressType != "direct" || after.BindingRevision != 8 {
		t.Fatalf("migration not committed: %+v", after)
	}
	if err = db.First(&item, "id = ?", item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if item.State != "migrated" || item.ActiveSubjectKey != nil {
		t.Fatal("platform authorization not consumed")
	}
	var devices int64
	if err = db.Model(&store.Binding{}).Count(&devices).Error; err != nil || devices != 0 {
		t.Fatalf("platform counted as device: %d %v", devices, err)
	}
}

func TestTransferPipeFailureIsNotSuccessfulEOF(t *testing.T) {
	reader, writer := net.Pipe()
	defer reader.Close()
	cause := errors.New("WSS transfer interrupted")
	bridge := &transferBridge{}
	bridge.setPipeError(cause)
	writer.Close()
	conn := &transferConn{Conn: reader, bridge: bridge}
	_, err := io.ReadAll(conn)
	if !errors.Is(err, cause) {
		t.Fatalf("transfer error = %v", err)
	}
}

func TestSessionTrafficCannotUpdateNewGenerationOrBeErasedByCleanup(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	defer sqlDB.Close()
	if err = migrations.Apply(db); err != nil {
		t.Fatal(err)
	}
	gateway := store.Gateway{Base: store.Base{ID: "gateway"}, StableID: "b1", AdminState: "enabled", DesiredConfig: core.DesiredConfig{EffectiveConfig: protocol.DefaultEffectiveConfig()}, Revision: 1}
	if err = db.Create(&gateway).Error; err != nil {
		t.Fatal(err)
	}
	repo := store.New(db)
	epoch, err := repo.AcquireOwner(t.Context(), gateway.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	row := store.Session{Base: store.Base{ID: "session"}, GatewayID: gateway.ID, ResourceType: "media", RemoteID: "remote", Generation: "generation", ConnectionEpoch: 1, State: "attached"}
	if err = repo.SaveSession(t.Context(), &row, "owner", epoch); err != nil {
		t.Fatal(err)
	}
	ref := protocol.ResourceRef{ResourceType: "media", ID: "remote", Generation: "generation", ConnectionEpoch: "1"}
	now := time.Now().UTC()
	sample := protocol.ResourceTraffic{Ref: ref, RXBytes: "100", TXBytes: "20", QueueBytes: "0"}
	if err = repo.RuntimeSessionTraffic(t.Context(), gateway.ID, "owner", epoch, []protocol.ResourceTraffic{sample}, now); err != nil {
		t.Fatal(err)
	}
	sample.RXBytes = "1100"
	sample.TXBytes = "520"
	if err = repo.RuntimeSessionTraffic(t.Context(), gateway.ID, "owner", epoch, []protocol.ResourceTraffic{sample}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	row.State = "closing"
	if err = repo.SaveSession(t.Context(), &row, "owner", epoch); err != nil {
		t.Fatal(err)
	}
	after, err := repo.Session(t.Context(), gateway.ID, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Traffic == nil || after.Traffic.RXBytes != "1100" || after.Traffic.RXBPS == nil || *after.Traffic.RXBPS != "8000" || after.Traffic.TXBPS == nil || *after.Traffic.TXBPS != "4000" {
		t.Fatalf("traffic erased by lifecycle: %+v", after.Traffic)
	}
	if err = repo.SaveRecovery(t.Context(), gateway.ID, "owner", epoch, core.RecoverySession{ID: row.ID, Ref: ref}, core.RecoveryOutcome{}); err != nil {
		t.Fatal(err)
	}
	after, _ = repo.Session(t.Context(), gateway.ID, row.ID)
	if after.Traffic == nil || after.Traffic.RXBytes != "1100" {
		t.Fatal("recovery erased traffic")
	}
	sample.Ref.Generation = "old-generation"
	sample.RXBytes = "999999"
	if err = repo.RuntimeSessionTraffic(t.Context(), gateway.ID, "owner", epoch, []protocol.ResourceTraffic{sample}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	after, _ = repo.Session(t.Context(), gateway.ID, row.ID)
	if after.Traffic.RXBytes != "1100" {
		t.Fatal("old generation overwrote new traffic")
	}
}

func TestDownstreamPlatformHasIndependentBindingAndCounts(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	defer sqlDB.Close()
	if err = migrations.Apply(db); err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&ipc.Device{}, &ipc.Channel{}); err != nil {
		t.Fatal(err)
	}
	host := New(db, nil, nil)
	device := ipc.Device{ID: "downstream", DeviceID: "34020000002000000001", Type: ipc.TypeGB28181, IsOnline: true}
	if err = db.Create(&device).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Transaction(func(tx *gorm.DB) error {
		return host.CommitRegistration(t.Context(), tx, &device, ipc.RegistrationIngress{Expires: 3600})
	}); err != nil {
		t.Fatal(err)
	}
	var platform store.PlatformBinding
	if err = db.Where("subject_id = ?", device.ID).First(&platform).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	db.Model(&store.Binding{}).Count(&count)
	if count != 0 || platform.Role != "downstream" || platform.RegisteredUntil == nil {
		t.Fatalf("platform became a camera: %+v cameras=%d", platform, count)
	}
	gateway := store.Gateway{Base: store.Base{ID: "gateway"}, StableID: "b1", AdminState: "enabled", DesiredConfig: core.DesiredConfig{EffectiveConfig: protocol.DefaultEffectiveConfig()}, Revision: 1}
	if err = db.Create(&gateway).Error; err != nil {
		t.Fatal(err)
	}
	generation, flow, epoch := "generation", "flow", int64(1)
	platform.GatewayID = &gateway.ID
	platform.IngressType = "gateway"
	platform.LastGeneration = &generation
	platform.LastConnectionEpoch = &epoch
	platform.LastFlowID = &flow
	if err = db.Save(&platform).Error; err != nil {
		t.Fatal(err)
	}
	channel := ipc.Channel{ID: "channel", DID: device.ID, DeviceID: device.DeviceID, ChannelID: "34020000001320000001", IsOnline: true}
	if err = db.Create(&channel).Error; err != nil {
		t.Fatal(err)
	}
	snapshot := core.RuntimeSnapshot{ConnectionState: "connected", HealthState: "ready", Generation: generation, Epoch: "1"}
	counts, err := host.GatewayCounts(t.Context(), gateway.ID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if counts.DeviceTotal != 0 || counts.PlatformBindings != 1 || counts.ChannelTotal != 1 || counts.ChannelOnline != 1 {
		t.Fatalf("invalid platform counts: %+v", counts)
	}
	expired := time.Now().UTC().Add(-time.Second)
	if err = db.Model(&platform).Update("registered_until", expired).Error; err != nil {
		t.Fatal(err)
	}
	counts, err = host.GatewayCounts(t.Context(), gateway.ID, snapshot)
	if err != nil || counts.ChannelOnline != 0 || counts.ChannelTotal != 1 || counts.PlatformBindings != 1 {
		t.Fatalf("expired platform still online: %+v %v", counts, err)
	}
	resolved, err := host.repo.Binding(t.Context(), device.ID)
	if err != nil || resolved.GatewayID == nil || *resolved.GatewayID != gateway.ID {
		t.Fatalf("host binding lookup lost platform: %+v %v", resolved, err)
	}
	camera := ipc.Device{ID: "camera", DeviceID: "34020000001320000002", Type: ipc.TypeGB28181, IsOnline: true}
	if err = db.Create(&camera).Error; err != nil {
		t.Fatal(err)
	}
	binding := store.Binding{Base: store.Base{ID: "camera-binding"}, BindingFields: store.BindingFields{SubjectID: camera.ID, IngressType: "gateway", GatewayID: &gateway.ID, BindingRevision: 1, LastGeneration: &generation, LastConnectionEpoch: &epoch, LastFlowID: &flow, RegisteredUntil: &expired}}
	if err = db.Create(&binding).Error; err != nil {
		t.Fatal(err)
	}
	counts, err = host.GatewayCounts(t.Context(), gateway.ID, snapshot)
	if err != nil || counts.DeviceTotal != 1 || counts.DeviceOnline != 0 || counts.DeviceOffline != 1 {
		t.Fatalf("expired camera still online: %+v %v", counts, err)
	}
	for _, online := range []bool{false, true} {
		page, err := host.GatewayDevices(t.Context(), gateway.ID, snapshot, core.PageQuery{Page: 1, Size: 20, Online: &online})
		if err != nil {
			t.Fatal(err)
		}
		if online && page.Total != 0 || !online && (page.Total != 1 || page.Items[0].Online) {
			t.Fatalf("expired device filter online=%v: %+v", online, page)
		}
	}
}
