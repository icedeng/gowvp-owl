package openplatform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/event/store/eventdb"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

func TestEventNotificationMigrationPreservesHistoryAndAllowsChanges(t *testing.T) {
	s, _, _ := fixture(t)
	old := Outbox{EventID: 71, Payload: `{"id":71}`, Done: true}
	if err := s.DB.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	d := Delivery{ID: "historic", SubscriptionID: "sub", EventID: 71, OutboxID: old.ID, State: "delivered"}
	if err := s.DB.Create(&d).Error; err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"CREATE UNIQUE INDEX idx_open_event_outbox_event_id ON open_event_outbox(event_id)", "CREATE UNIQUE INDEX idx_open_delivery_event ON open_deliveries(subscription_id,event_id)"} {
		if err := s.DB.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	// 还原升级前的列和约束，验证真实旧结构可升级。
	for _, q := range []string{
		"DROP INDEX idx_open_event_outbox_notification_key",
		"DROP INDEX idx_open_delivery_notification",
		"ALTER TABLE open_event_outbox DROP COLUMN notification_key",
		"ALTER TABLE open_event_outbox DROP COLUMN event_type",
		"ALTER TABLE open_deliveries DROP COLUMN event_type",
	} {
		if err := s.DB.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := New(s.DB, s.Config, s.Host); err == nil {
		t.Fatal("未迁移的旧结构不应启用开放平台")
	}
	for range 2 {
		if err := Migrate(s.DB); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := New(s.DB, s.Config, s.Host); err != nil {
		t.Fatal("迁移后未恢复开放平台", err)
	}
	next := Outbox{EventID: 71, EventType: "event.evidence.updated", Payload: `{}`}
	if err := s.DB.Create(&next).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&Delivery{ID: "new", SubscriptionID: "sub", EventID: 71, OutboxID: next.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&Delivery{ID: "duplicate", SubscriptionID: "sub", EventID: 71, OutboxID: next.ID}).Error; err == nil {
		t.Fatal("重复通知投递未拦截")
	}
	var saved Delivery
	if err := s.DB.First(&saved, "id = ?", "historic").Error; err != nil || saved.State != "delivered" || saved.EventType != "event.created" {
		t.Fatal(saved, err)
	}
}

func TestEventEvidenceChangesAreDurableIndependentAndTransactional(t *testing.T) {
	s, _, _ := fixture(t)
	if err := s.InstallOutbox(); err != nil {
		t.Fatal(err)
	}
	w := event.NewWorkflow(s.DB)
	s.BindEventWorkflow(w)
	if _, err := w.SavePolicy(context.Background(), event.Policy{DID: "d1", Snapshot: true, Record: true, ClipSeconds: 5, CooldownSeconds: 10}, "admin"); err != nil {
		t.Fatal(err)
	}
	ev := event.Event{DID: "d1", CID: "c1", Model: "GB28181", StartedAt: orm.Now(), EndedAt: orm.Now()}
	if err := eventdb.NewEvent(s.DB.Session(&gorm.Session{SkipDefaultTransaction: true})).Create(context.Background(), &ev); err != nil {
		t.Fatal(err)
	}
	video := make(chan struct{})
	snapshot := make(chan struct{})
	w.SetEvidenceExecutor(func(ctx context.Context, _ *event.Event, kind string, _ int) (string, error) {
		if kind == "snapshot" {
			close(snapshot)
			return "INTERNAL-photo.jpg", nil
		}
		select {
		case <-video:
			return "", errors.New("PRIVATE internal failure /disk/path")
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.RunEvidenceWorkers(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-snapshot:
	case <-time.After(4 * time.Second):
		t.Fatal("抓拍未执行")
	}
	waitEvidence := func(state string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			var task event.EvidenceTask
			if err := s.DB.First(&task, "event_id = ?", ev.ID).Error; err != nil {
				t.Fatal(err)
			}
			if state == "snapshot" && task.SnapshotState == "succeeded" && task.VideoState == "working" || state == "failed" && task.State == "failed" {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("未独立提交取证状态", state)
	}
	waitEvidence("snapshot")
	var boxes []Outbox
	if err := s.DB.Where("event_id = ?", ev.ID).Order("id").Find(&boxes).Error; err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 4 || boxes[0].EventType != "event.created" {
		t.Fatal("首次/排队/执行/抓拍通知不完整", len(boxes))
	}
	close(video)
	waitEvidence("failed")
	if err := s.DB.Where("event_id = ?", ev.ID).Order("id").Find(&boxes).Error; err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 5 || strings.Contains(boxes[4].Payload, "PRIVATE") || strings.Contains(boxes[4].Payload, "INTERNAL") {
		t.Fatal("终态通知缺失或泄露路径", boxes)
	}
	// 重复同状态不会再写通知；通知失败应回滚状态变更。
	if err := s.DB.Transaction(func(tx *gorm.DB) error { return s.EnqueueEventChange(tx, ev.ID, "event.evidence.updated") }); err != nil {
		t.Fatal(err)
	}
	var count int64
	s.DB.Model(&Outbox{}).Where("event_id = ?", ev.ID).Count(&count)
	if count != 5 {
		t.Fatal("重复状态通知", count)
	}
	marker := errors.New("outbox unavailable")
	w.SetChangeHandler(func(*gorm.DB, int64, string) error { return marker })
	if err := w.Process(context.Background(), event.ProcessInput{Items: []event.ProcessTarget{{ID: ev.ID, Revision: 1}}, Status: "processing"}, "admin"); !errors.Is(err, marker) {
		t.Fatal(err)
	}
	var saved event.Event
	s.DB.First(&saved, ev.ID)
	if saved.Status != "pending" || saved.Revision != 1 {
		t.Fatal("通知失败未回滚处理", saved.Status, saved.Revision)
	}
}

func TestProcessEventAuthorizationIdempotencyAndRevision(t *testing.T) {
	s, p, _ := fixture(t)
	if err := s.InstallOutbox(); err != nil {
		t.Fatal(err)
	}
	ev := event.Event{DID: "d1", CID: "c1", StartedAt: orm.Now(), EndedAt: orm.Now()}
	if err := s.DB.Create(&ev).Error; err != nil {
		t.Fatal(err)
	}
	call := func(id int64, rev int, status, note, key, fp string) (int64, error) {
		return s.ProcessEvent(context.Background(), p, id, rev, status, note, key, fp)
	}
	first, err := call(ev.ID, 1, "processing", "checking", "k1", "a")
	if err != nil || first == 0 {
		t.Fatal(first, err)
	}
	repeat, err := call(ev.ID, 1, "processing", "checking", "k1", "a")
	if err != nil || repeat != first {
		t.Fatal("幂等重试", repeat, err)
	}
	if _, err = call(ev.ID, 1, "ignored", "ignore", "k1", "b"); AsError(err).Code != "IDEMPOTENCY_MISMATCH" {
		t.Fatal(err)
	}
	if _, err = call(ev.ID, 1, "resolved", "done", "k2", "b"); AsError(err).Status != 409 {
		t.Fatal(err)
	}
	if _, err = call(ev.ID, 2, "resolved", "", "k2", "b"); AsError(err).Status != 400 {
		t.Fatal(err)
	}
	if _, err = call(ev.ID, 2, "resolved", "done", "k2", "b"); err != nil {
		t.Fatal("失败事务占用幂等键", err)
	}
	if _, err = call(ev.ID, 3, "processing", "", "k3", "c"); AsError(err).Status != 400 {
		t.Fatal("结束后未经重开", err)
	}
	if _, err = call(ev.ID, 3, "pending", "reopen", "k3", "c"); err != nil {
		t.Fatal(err)
	}
	denied := event.Event{DID: "d2", CID: "c2"}
	s.DB.Create(&denied)
	if _, err = call(denied.ID, 1, "processing", "", "other", "d"); AsError(err).Status != 404 {
		t.Fatal("越权事件处理", err)
	}
	var actions []event.Action
	s.DB.Where("event_id = ?", ev.ID).Find(&actions)
	if len(actions) != 3 || actions[0].Actor != "open-app:"+p.App.ID {
		t.Fatal(actions)
	}
	var boxes []Outbox
	s.DB.Where("event_id = ? AND event_type = ?", ev.ID, "event.status.changed").Find(&boxes)
	if len(boxes) != 3 {
		t.Fatal("状态通知", len(boxes))
	}
	for _, box := range boxes {
		var payload map[string]any
		if err := json.Unmarshal([]byte(box.Payload), &payload); err != nil || payload["event_type"] != "event.status.changed" {
			t.Fatal(fmt.Sprint(payload), err)
		}
	}
}

func TestEventChangesFanoutAndSignedCallbacks(t *testing.T) {
	s, p, _ := fixture(t)
	// 本测试连续投递四条通知，独立验证身份与签名，不消耗默认每秒两次的额度。
	if err := s.DB.Model(&App{}).Where("id = ?", p.App.ID).Update("delivery_limit", 20).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.InstallOutbox(); err != nil {
		t.Fatal(err)
	}
	target, err := s.SaveTarget(p.App.ID, "", "https://receiver.example/events", nil, "approved", 0, "admin")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.SaveSubscription(p, Subscription{CallbackTargetID: target.ID, Status: "paused"}, true)
	if err != nil {
		t.Fatal(err)
	}
	keyID, secret, err := s.RotateCallbackKey(p, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ValidateSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB.First(&sub, "id = ?", sub.ID).Error; err != nil {
		t.Fatal(err)
	}
	sub.Status = "active"
	if _, err = s.SaveSubscription(p, sub, false); err != nil {
		t.Fatal(err)
	}
	ev := event.Event{DID: "d1", CID: "c1", StartedAt: orm.Now(), EndedAt: orm.Now()}
	if err = s.DB.Create(&ev).Error; err != nil {
		t.Fatal(err)
	}
	task := event.EvidenceTask{EventID: ev.ID, State: "queued", SnapshotState: "queued", VideoState: "queued"}
	if err = s.DB.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&ev).Updates(map[string]any{"image_path": "private.jpg", "video_path": "private.mp4"}).Error; err != nil {
			return err
		}
		if err := tx.Model(&task).Updates(map[string]any{"state": "succeeded", "snapshot_state": "succeeded", "video_state": "succeeded"}).Error; err != nil {
			return err
		}
		return s.EnqueueEventChange(tx, ev.ID, "event.evidence.updated")
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProcessEvent(context.Background(), p, ev.ID, 1, "processing", "核实", "callback-process", "request"); err != nil {
		t.Fatal(err)
	}
	s.fanout()
	s.fanout()
	var deliveries []Delivery
	if err = s.DB.Order("outbox_id").Find(&deliveries).Error; err != nil || len(deliveries) != 4 {
		t.Fatal("每个通知应独立且仅分发一次", len(deliveries), err)
	}
	types := []string{"event.created", "event.evidence.updated", "event.evidence.updated", "event.status.changed"}
	seen := map[string]bool{}
	for i, d := range deliveries {
		if d.EventType != types[i] || d.EventID != ev.ID || seen[d.ID] {
			t.Fatal("通知类型或投递身份不正确", d)
		}
		seen[d.ID] = true
		called := false
		s.callbackTransport = callbackTransportFunc(func(req *http.Request) (*http.Response, error) {
			called = true
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			h := req.Header
			digest := Hash(body)
			want := "OWL-CALLBACK-HMAC-SHA256 Signature=" + CallbackSign(key, CallbackCanonical(h.Get("X-Owl-Timestamp"), h.Get("X-Owl-Nonce"), strconv.FormatInt(ev.ID, 10), d.ID, digest))
			if h.Get("X-Owl-Callback-Key-ID") != keyID || h.Get("Authorization") != want || h.Get("X-Owl-Delivery-ID") != d.ID {
				t.Error("追加通知签名或投递身份不正确")
			}
			var payload struct {
				EventType      string `json:"event_type"`
				NotificationID uint64 `json:"notification_id"`
				DeliveryID     string `json:"delivery_id"`
				ImageURL       string `json:"image_url"`
				ThumbnailURL   string `json:"thumbnail_url"`
				VideoURL       string `json:"video_url"`
				DownloadURL    string `json:"video_download_url"`
			}
			if err = json.Unmarshal(body, &payload); err != nil || payload.EventType != d.EventType || payload.NotificationID != d.OutboxID || payload.DeliveryID != d.ID {
				t.Error("追加通知正文不正确", string(body), err)
			}
			for kind, raw := range map[string]string{"event": payload.ImageURL, "event-thumbnail": payload.ThumbnailURL, "event-video": payload.VideoURL, "event-download": payload.DownloadURL} {
				if !strings.Contains(raw, "/"+kind+"/"+strconv.FormatInt(ev.ID, 10)+"/content?ticket=") {
					t.Error("回调缺少媒体链接", kind, raw)
				}
			}
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
		})
		s.deliver(context.Background(), d)
		var saved Delivery
		if err = s.DB.First(&saved, "id = ?", d.ID).Error; err != nil || !called || saved.State != "delivered" {
			t.Fatal("追加通知未送达", saved, called, err)
		}
	}
}
