package controller

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/config"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/lease"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/state"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
)

func testController(t *testing.T) *Controller {
	t.Helper()
	c := config.Config{GatewayID: "b1", PublicIP: "203.0.113.1", MediaPortRange: "30000-30199", TLSCertFile: "cert", TLSKeyFile: "key", ClientCAFile: "ca", Platform: config.Platform{ID: "a", ClientCertificateCN: "a"}}
	c.Defaults()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	s, e := state.Open(dir, c.GatewayID, c.Platform.ID)
	if e != nil {
		t.Fatal(e)
	}
	v, e := New(context.Background(), c, s)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { v.Close(); s.Close() })
	return v
}
func TestConfigPrepareApplyAndIdempotency(t *testing.T) {
	c := testController(t)
	next := c.effective
	next.MaxMediaSessions = 20
	hash, e := protocol.ConfigHash(next)
	if e != nil {
		t.Fatal(e)
	}
	owner := lease.Owner{PlatformID: "a", Generation: uuid.NewString(), Epoch: "1", OwnerEpoch: "1", Fingerprint: "cert"}
	request := protocol.ConfigPrepare{OperationID: uuid.NewString(), ExpectedRevision: "0", DesiredRevision: "1", Config: next, ConfigSHA256: hash}
	in := transport.Incoming{Envelope: protocol.Envelope{Type: "config.prepare"}, Payload: &request}
	result, failure := c.executeIdempotent(context.Background(), owner, in)
	if failure != nil {
		t.Fatal(failure)
	}
	prepared := result.(protocol.ConfigPrepared)
	again, failure := c.executeIdempotent(context.Background(), owner, in)
	if failure != nil || again.(protocol.ConfigPrepared).PrepareID != prepared.PrepareID {
		t.Fatal("prepare allocated duplicate ticket")
	}
	changed := request
	changed.Config.MaxMediaSessions = 21
	changed.ConfigSHA256, _ = protocol.ConfigHash(changed.Config)
	in.Payload = &changed
	if _, failure = c.executeIdempotent(context.Background(), owner, in); failure == nil || failure.Code != protocol.IdempotencyConflict {
		t.Fatal("same operation accepted different config")
	}
	applied, e := c.apply(protocol.ConfigApply{OperationID: uuid.NewString(), PrepareID: prepared.PrepareID, DesiredRevision: "1", ConfigSHA256: hash})
	if e != nil || !applied.Applied {
		t.Fatal(e)
	}
	if c.store.Snapshot().AppliedRevision != "1" {
		t.Fatal("applied config not persisted")
	}
	if _, e = c.apply(protocol.ConfigApply{OperationID: uuid.NewString(), PrepareID: prepared.PrepareID, DesiredRevision: "1", ConfigSHA256: hash}); e != nil {
		t.Fatal("same version/hash retry rejected", e)
	}
	if _, e = c.prepare(request); e == nil {
		t.Fatal("stale expected version accepted")
	}
	rollback := request
	rollback.ExpectedRevision = "1"
	rollback.DesiredRevision = "0"
	if protocol.Validate(&rollback) == nil {
		t.Fatal("revision rollback accepted")
	}
	if len(c.Capabilities()) == 0 {
		t.Fatal("missing actual capabilities")
	}
}
func TestExpiredPreparationDoesNotChangeConfig(t *testing.T) {
	c := testController(t)
	next := c.effective
	next.MaxMediaSessions = 10
	hash, _ := protocol.ConfigHash(next)
	p, e := c.prepare(protocol.ConfigPrepare{OperationID: uuid.NewString(), ExpectedRevision: "0", DesiredRevision: "1", Config: next, ConfigSHA256: hash})
	if e != nil {
		t.Fatal(e)
	}
	c.mu.Lock()
	v := c.prepared[p.PrepareID]
	v.expires = time.Now().Add(-time.Second)
	c.prepared[p.PrepareID] = v
	c.mu.Unlock()
	if _, e = c.apply(protocol.ConfigApply{OperationID: uuid.NewString(), PrepareID: p.PrepareID, DesiredRevision: "1", ConfigSHA256: hash}); e == nil {
		t.Fatal("expired prepare applied")
	}
	if c.revision != "0" || c.store.Snapshot().AppliedConfig != nil {
		t.Fatal("failed apply modified config")
	}
}
func TestHelloSampleRemainsStrict(t *testing.T) {
	data := protocol.Payload(protocol.Hello{ClientInstanceID: uuid.NewString(), SupportedVersions: []int{1}, RequiredCapabilities: []string{}, LastAppliedRevision: "0"})
	var decoded protocol.Hello
	if e := protocol.DecodeStrict(json.RawMessage(data), &decoded); e != nil {
		t.Fatal(e)
	}
}

func TestRestartClosesUncommittedReceiptsWithoutInventingSuccess(t *testing.T) {
	original := testController(t)
	pending := state.TransferResult{Ref: protocol.ResourceRef{ResourceType: "transfer", ID: uuid.NewString(), Generation: uuid.NewString(), ConnectionEpoch: "1"}, PlatformID: "a", TaskID: uuid.NewString(), TransferID: uuid.NewString(), Received: "8", Sent: "0", Phase: "waiting_commit", State: "receiving", CleanupReserve: "reserved", ExpiresAt: time.Now().Add(time.Hour)}
	committed := pending
	committed.TaskID = uuid.NewString()
	committed.TransferID = uuid.NewString()
	committed.Committed = true
	committed.Phase = "completed"
	committed.State = "closed"
	if err := original.store.Update(func(d *state.Data) error { d.Transfers = []state.TransferResult{pending, committed}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(t.Context(), original.config, original.store)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.generation == original.generation {
		t.Fatal("restart reused generation")
	}
	receipts := original.store.Snapshot().Transfers
	if len(receipts) != 2 || receipts[0].Committed || receipts[0].State != "closed" || receipts[0].Phase != "failed" || !receipts[1].Committed || receipts[1].Phase != "completed" || receipts[0].CleanupReserve != "" {
		t.Fatalf("invalid recovered receipts: %+v", receipts)
	}
}
