package state

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

func TestJournalFullStillAllowsReservedReceiptAndAtomicRestart(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir, "b1", "a")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	receipt := TransferResult{PlatformID: "a", TaskID: uuid.NewString(), TransferID: uuid.NewString(), Ref: protocol.ResourceRef{ResourceType: "transfer", ID: uuid.NewString(), Generation: uuid.NewString(), ConnectionEpoch: "1"}, Received: "0", Sent: "0", Phase: "allocated", State: "allocated", CleanupReserve: strings.Repeat(" ", 8192), ExpiresAt: time.Now().Add(time.Hour)}
	if err = store.Update(func(d *Data) error {
		d.Transfers = []TransferResult{receipt}
		encoded, _ := json.Marshal(d)
		d.ConfigSHA256 = strings.Repeat("x", (16<<20)-len(encoded)-64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(d *Data) error { d.Transfers = append(d.Transfers, receipt); return nil }); err == nil {
		t.Fatal("accepted creation beyond journal budget")
	}
	if len(store.Snapshot().Transfers) != 1 {
		t.Fatal("failed transaction changed in-memory receipts")
	}
	digest := strings.Repeat("a", 64)
	if err = store.Update(func(d *Data) error {
		v := &d.Transfers[0]
		v.CleanupReserve = ""
		v.Phase = "completed"
		v.State = "closed"
		v.Received = "123456"
		v.Committed = true
		v.SHA256 = &digest
		return nil
	}); err != nil {
		t.Fatal("reserved completion capacity unavailable", err)
	}
	reopened, err := Open(dir, "b1", "a")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after := reopened.Snapshot()
	if len(after.Transfers) != 1 || !after.Transfers[0].Committed || after.Transfers[0].SHA256 == nil || *after.Transfers[0].SHA256 != digest {
		t.Fatal("durable receipt lost across reopen")
	}
}
