package lease

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/state"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

func TestTicketsFencingAndExactCleanup(t *testing.T) {
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	store, e := state.Open(dir, "b1", "a")
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	r := New(context.Background(), store, nil)
	defer r.Close()
	owner := Owner{PlatformID: "a", Generation: uuid.NewString(), Epoch: "1", OwnerEpoch: "2", Fingerprint: "cert-a"}
	var closed atomic.Int32
	ref, e := r.Add(Resource{Ref: protocol.ResourceRef{ResourceType: "media"}, Owner: owner, OperationID: uuid.NewString(), SubjectID: "device1", SubjectType: "device", BindingRevision: "1", ConfigRevision: "1", TTL: time.Minute, Close: func() error { closed.Add(1); return nil }, Value: "media"})
	if e != nil {
		t.Fatal(e)
	}
	ticket, e := r.Issue(owner, "media", ref.ID)
	if e != nil {
		t.Fatal(e)
	}
	other := owner
	other.Epoch = "2"
	if _, e := r.Consume(other, "media", ref.ID, ticket); e == nil {
		t.Fatal("old ticket crossed control epoch")
	}
	other = owner
	other.Fingerprint = "other"
	if _, e := r.Consume(other, "media", ref.ID, ticket); e == nil {
		t.Fatal("ticket crossed certificate")
	}
	replacement, e := r.Issue(owner, "media", ref.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Consume(owner, "media", ref.ID, ticket); e == nil {
		t.Fatal("reissued ticket did not invalidate old ticket")
	}
	if _, e = r.Consume(owner, "media", ref.ID, replacement); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Consume(owner, "media", ref.ID, replacement); e == nil {
		t.Fatal("ticket replay accepted")
	}
	if e = r.Attached(ref.ID); e != nil {
		t.Fatal(e)
	}
	wrong := ref
	wrong.ConnectionEpoch = "2"
	if _, e = r.CloseRef(owner.PlatformID, wrong); e == nil {
		t.Fatal("wrong epoch cleanup accepted")
	}
	if closed.Load() != 0 {
		t.Fatal("wrong cleanup closed live resource")
	}
	renewed, rejected := r.Renew(other, []string{ref.ID})
	if len(renewed) != 0 || len(rejected) != 1 {
		t.Fatal("wrong certificate renewed lease")
	}
	if ok, e := r.CloseRef(owner.PlatformID, ref); e != nil || !ok {
		t.Fatal(e)
	}
	if ok, e := r.CloseRef(owner.PlatformID, ref); e != nil || !ok {
		t.Fatal(e)
	}
	if closed.Load() != 1 {
		t.Fatal("cleanup not idempotent")
	}
	if !store.Snapshot().HadResources {
		t.Fatal("missing crash recovery marker")
	}
}
