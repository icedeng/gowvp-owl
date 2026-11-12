package openplatform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/event/store/eventdb"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/recording"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type testHost struct {
	mu       sync.Mutex
	calls    []string
	failStop bool
}

func (h *testHost) Start(context.Context, string) (Source, error) {
	return Source{App: "live", Stream: "c1", BaseURL: "http://media.invalid"}, nil
}
func (h *testHost) Snapshot(context.Context, string, string) error { return errors.New("not captured") }
func (h *testHost) PTZ(_ context.Context, _ string, in PTZInput) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, in.Action)
	if h.failStop && in.Action == "stop" {
		return errors.New("stop failed")
	}
	return nil
}
func (h *testHost) Capabilities(context.Context, string) ([]string, error) { return BasicActions, nil }
func (h *testHost) RecordingPath(context.Context, int64) (string, error)   { return "", ErrNotFound }
func (h *testHost) EventImagePath(context.Context, int64) (string, error)  { return "", ErrNotFound }
func fixture(t *testing.T) (*Service, Principal, *testHost) {
	t.Helper()
	db, e := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "open.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal(e)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	if e = db.AutoMigrate(&ipc.Device{}, &ipc.Channel{}, &event.Event{}, &event.Policy{}, &event.Action{}, &event.EvidenceTask{}, &recording.Recording{}); e != nil {
		t.Fatal(e)
	}
	if e = Migrate(db); e != nil {
		t.Fatal(e)
	}
	for _, v := range []ipc.Device{{ID: "d1", DeviceID: "gb1"}, {ID: "d2", DeviceID: "gb2"}} {
		if e = db.Create(&v).Error; e != nil {
			t.Fatal(e)
		}
	}
	for _, v := range []ipc.Channel{{ID: "c1", DID: "d1"}, {ID: "c2", DID: "d2"}} {
		if e = db.Create(&v).Error; e != nil {
			t.Fatal(e)
		}
	}
	h := &testHost{}
	s, e := New(db, Config{Enabled: true, Audience: "owl-test", PublicBaseURL: "https://open.example", DataDir: t.TempDir(), KeyID: "v1", Keys: map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)}}, h)
	if e != nil {
		t.Fatal(e)
	}
	a := DefaultApp()
	a.Name = "test"
	a.DeviceIDs = []string{"d1"}
	a.Scopes = append([]string{}, Scopes...)
	a.AllowAnyIP = true
	a, e = s.SaveApp(a, true, "admin")
	if e != nil {
		t.Fatal(e)
	}
	c, _, e := s.CreateCredential(a.ID, nil, "admin")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SetStatus(a.ID, "enabled", "admin"); e != nil {
		t.Fatal(e)
	}
	p, e := s.Current(a.ID, c.ID)
	if e != nil {
		t.Fatal(e)
	}
	s.resolveCallback = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.4.4")}}, nil
	}
	return s, p, h
}
func TestDeviceAuthorizationAndRevocation(t *testing.T) {
	s, p, _ := fixture(t)
	if _, e := s.Channel(p.App, "c2"); AsError(e).Status != 404 {
		t.Fatal("cross-device access", e)
	}
	ch := ipc.Channel{ID: "new-channel", DID: "d1"}
	s.DB.Create(&ch)
	if _, e := s.Channel(p.App, ch.ID); e != nil {
		t.Fatal("new child should inherit", e)
	}
	a := p.App
	a.DeviceIDs = nil
	if _, e := s.SaveApp(a, false, "admin"); e == nil {
		t.Fatal("empty selected accepted")
	}
	a.DeviceAccessMode = "all"
	if _, e := s.SaveApp(a, false, "admin"); e != nil {
		t.Fatal(e)
	}
	a, _ = s.App(a.ID)
	d := ipc.Device{ID: "d3", DeviceID: "gb3"}
	s.DB.Create(&d)
	if !s.AllowedDevice(a, d.ID) {
		t.Fatal("all failed future device")
	}
	row, e := s.CreateSession(context.Background(), p, "c1", "hls")
	if e != nil {
		t.Fatal(e)
	}
	raw, _, e := s.IssueTicket(p, "live", row.ID)
	if e != nil {
		t.Fatal(e)
	}
	ticket, _, e := s.CheckTicket(raw, "live", row.ID)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SetStatus(a.ID, "disabled", "admin"); e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.CheckTicket(raw, "live", row.ID); e == nil {
		t.Fatal("revoked ticket accepted")
	}
	if s.CheckActive(ticket) == nil {
		t.Fatal("active stream not revoked")
	}
}
func TestNonceAtomicPersistenceAndFutureWindow(t *testing.T) {
	s, p, _ := fixture(t)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	stamp := time.Now().Add(290 * time.Second).Unix()
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.UseNonce(p.Credential.AccessKey, "same_nonce", stamp) == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatal(accepted.Load())
	}
	var row Nonce
	s.DB.First(&row)
	if row.ExpiresAt.Unix() != stamp+360 {
		t.Fatal("future window truncated")
	}
	other := &Service{DB: s.DB}
	if other.UseNonce(row.AccessKey, row.Nonce, stamp) == nil {
		t.Fatal("restarted instance accepted nonce")
	}
}
func TestCredentialEncryptionAndRewrapRollback(t *testing.T) {
	s, p, _ := fixture(t)
	plain, e := s.Decrypt(p.App.ID, p.Credential.AccessKey, p.Credential.Secret)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Decrypt("other-app", p.Credential.AccessKey, p.Credential.Secret); e == nil {
		t.Fatal("AAD did not bind app")
	}
	s.Config.Keys["v2"] = bytes.Repeat([]byte{2}, 32)
	s.Config.KeyID = "v2"
	if n, e := s.Rewrap(); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	var c Credential
	s.DB.First(&c)
	if c.Secret.KeyID != "v2" {
		t.Fatal(c.Secret.KeyID)
	}
	got, e := s.Decrypt(c.AppID, c.AccessKey, c.Secret)
	if e != nil || !bytes.Equal(got, plain) {
		t.Fatal("rewrap changed secret")
	}
	s.Config.Keys = map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)}
	if _, e = s.Rewrap(); e == nil {
		t.Fatal("missing old key accepted")
	}
}
func TestPTZStopFailureBlocksTakeoverAndStaleTimer(t *testing.T) {
	s, p, h := fixture(t)
	row, e := s.CreateCommand(context.Background(), p, "c1", PTZInput{Action: "left", Duration: 1})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateCommand(context.Background(), p, "c1", PTZInput{Action: "right"}); AsError(e).Status != 409 {
		t.Fatal("lease not enforced", e)
	}
	h.failStop = true
	if s.StopCommand(context.Background(), p.App.ID, row.ID) == nil {
		t.Fatal("reported failed stop as success")
	}
	var stored Command
	s.DB.First(&stored, "id = ?", row.ID)
	if stored.State != "stop_failed" {
		t.Fatal(stored.State)
	}
	called := false
	if s.GuardPTZ(context.Background(), "c1", func(context.Context) error { called = true; return nil }) == nil || called {
		t.Fatal("takeover ignored stop failure")
	}
	h.failStop = false
	if e = s.GuardPTZ(context.Background(), "c1", func(context.Context) error { called = true; return nil }); e != nil || !called {
		t.Fatal(e)
	}
	before := len(h.calls)
	if e = s.stopLocked(context.Background(), row); e != nil || len(h.calls) != before {
		t.Fatal("old timer stopped later owner")
	}
}
func TestOutboxSharesEventTransactionAndFiltersResources(t *testing.T) {
	s, p, _ := fixture(t)
	if e := s.InstallOutbox(); e != nil {
		t.Fatal(e)
	}
	target, e := s.SaveTarget(p.App.ID, "", "https://receiver.example/alarm", nil, "approved", 0, "admin")
	if e != nil {
		t.Fatal(e)
	}
	sub, e := s.SaveSubscription(p, Subscription{CallbackTargetID: target.ID, Status: "paused"}, true)
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = s.RotateCallbackKey(p, sub.ID)
	if e != nil {
		t.Fatal(e)
	}
	s.DB.First(&sub, "id = ?", sub.ID)
	sub.Status = "active"
	sub, e = s.SaveSubscription(p, sub, false)
	if e != nil {
		t.Fatal(e)
	}
	e = s.DB.Transaction(func(tx *gorm.DB) error {
		ev := event.Event{ID: 10, DID: "d1", CID: "c1"}
		if e := tx.Create(&ev).Error; e != nil {
			return e
		}
		return errors.New("rollback")
	})
	if e == nil {
		t.Fatal("transaction unexpectedly committed")
	}
	var n int64
	s.DB.Model(&Outbox{}).Count(&n)
	if n != 0 {
		t.Fatal("orphaned outbox", n)
	}
	for _, ev := range []event.Event{{ID: 11, DID: "d1", CID: "c1"}, {ID: 12, DID: "d2", CID: "c2"}, {ID: 13, DID: "d2", CID: "c1"}} {
		if e = s.DB.Create(&ev).Error; e != nil {
			t.Fatal(e)
		}
	}
	s.fanout()
	s.fanout()
	var ds []Delivery
	s.DB.Find(&ds)
	if len(ds) != 1 || ds[0].EventID != 11 {
		t.Fatal("incorrect event visibility", ds)
	}
	s.DB.Model(&ds[0]).Update("state", "dead")
	if e = s.Retry(p.App.ID, sub.ID, ds[0].ID, "admin"); e != nil {
		t.Fatal(e)
	}
	if e = s.SetStatus(p.App.ID, "disabled", "admin"); e != nil {
		t.Fatal(e)
	}
	s.deliver(context.Background(), ds[0])
	s.DB.First(&ds[0], "id = ?", ds[0].ID)
	if ds[0].State != "cancelled" {
		t.Fatal("disabled app delivery survived", ds[0].State)
	}
}

func TestOutboxFailureRollsBackHostEventWithImplicitTransactionsDisabled(t *testing.T) {
	s, _, _ := fixture(t)
	if e := s.InstallOutbox(); e != nil {
		t.Fatal(e)
	}
	if e := s.DB.Migrator().DropTable(&Outbox{}); e != nil {
		t.Fatal(e)
	}
	store := eventdb.NewEvent(s.DB.Session(&gorm.Session{SkipDefaultTransaction: true}))
	if e := store.Create(context.Background(), &event.Event{ID: 99, DID: "d1", CID: "c1"}); e == nil {
		t.Fatal("outbox error swallowed")
	}
	var n int64
	s.DB.Model(&event.Event{}).Where("id = ?", 99).Count(&n)
	if n != 0 {
		t.Fatal("event escaped failed outbox transaction")
	}
}
func TestCallbackAddressPolicy(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "::1", "169.254.169.254", "100.100.100.200", "168.63.129.16", "fd00:ec2::254", "224.0.0.1", "0.0.0.0"} {
		if targetIPAllowed(net.ParseIP(raw), []string{"0.0.0.0/0", "::/0"}) {
			t.Fatal("unsafe target", raw)
		}
	}
	if targetIPAllowed(net.ParseIP("10.2.3.4"), nil) {
		t.Fatal("implicit private approval")
	}
	if !targetIPAllowed(net.ParseIP("10.2.3.4"), []string{"10.2.0.0/16"}) {
		t.Fatal("explicit private policy ignored")
	}
}

type callbackTransportFunc func(*http.Request) (*http.Response, error)

func (f callbackTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDeliverySignsAndRetriesSameIdentity(t *testing.T) {
	s, p, _ := fixture(t)
	s.InstallOutbox()
	target, e := s.SaveTarget(p.App.ID, "", "https://receiver.example/alarm", nil, "approved", 0, "admin")
	if e != nil {
		t.Fatal(e)
	}
	sub, e := s.SaveSubscription(p, Subscription{CallbackTargetID: target.ID, Status: "paused"}, true)
	if e != nil {
		t.Fatal(e)
	}
	keyID, secret, e := s.RotateCallbackKey(p, sub.ID)
	if e != nil {
		t.Fatal(e)
	}
	key, _ := ValidateSecret(secret)
	s.DB.First(&sub, "id = ?", sub.ID)
	sub.Status = "active"
	sub, e = s.SaveSubscription(p, sub, false)
	if e != nil {
		t.Fatal(e)
	}
	ev := event.Event{ID: 123, DID: "d1", CID: "c1"}
	if e = s.DB.Create(&ev).Error; e != nil {
		t.Fatal(e)
	}
	s.fanout()
	var d Delivery
	s.DB.First(&d)
	count := 0
	nonces := map[string]bool{}
	s.callbackTransport = callbackTransportFunc(func(req *http.Request) (*http.Response, error) {
		count++
		b, _ := io.ReadAll(req.Body)
		h := req.Header
		nonce := h.Get("X-Owl-Nonce")
		if nonces[nonce] {
			t.Error("nonce reused")
		}
		nonces[nonce] = true
		digest := Hash(b)
		if h.Get("X-Owl-Callback-Key-ID") != keyID || h.Get("X-Owl-Delivery-ID") != d.ID || h.Get("X-Owl-Event-ID") != strconv.FormatInt(ev.ID, 10) || h.Get("X-Owl-Content-SHA256") != digest || h.Get("Authorization") != "OWL-CALLBACK-HMAC-SHA256 Signature="+CallbackSign(key, CallbackCanonical(h.Get("X-Owl-Timestamp"), nonce, strconv.FormatInt(ev.ID, 10), d.ID, digest)) {
			t.Error("callback signature or identity mismatch")
		}
		var payload map[string]any
		json.Unmarshal(b, &payload)
		if payload["delivery_id"] != d.ID || payload["image_url"] != nil {
			t.Error("wrong payload or nonexistent image exposed")
		}
		status := 500
		if count == 2 {
			status = 204
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	})
	s.deliver(context.Background(), d)
	s.DB.First(&d, "id = ?", d.ID)
	if d.State != "queued" || d.Attempt != 1 || !d.NextRetryAt.After(time.Now()) {
		t.Fatal("retry not scheduled", d)
	}
	s.DB.Model(&d).Update("next_retry_at", time.Now().Add(-time.Second))
	s.deliver(context.Background(), d)
	s.DB.First(&d, "id = ?", d.ID)
	if d.State != "delivered" || d.Attempt != 2 || count != 2 {
		t.Fatal("delivery retry failed", d, count)
	}
}
