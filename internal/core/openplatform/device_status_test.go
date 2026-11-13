package openplatform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipccache"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

func TestDeviceStatusTransactionsAndRecovery(t *testing.T) {
	s, _, _ := fixture(t)
	cache := ipccache.NewCache(ipcdb.NewDB(s.DB.Session(&gorm.Session{SkipDefaultTransaction: true})))
	cache.SetDeviceStatusHandler(EnqueueDeviceStatusChange)
	change := func(online bool) error {
		return cache.ChangeContext(ipc.WithDeviceStatusReason(t.Context(), "heartbeat"), "gb1", func(dev *ipc.Device) error {
			dev.IsOnline = online
			return nil
		}, nil)
	}
	for range 2 {
		if err := change(true); err != nil {
			t.Fatal(err)
		}
	}
	var boxes []Outbox
	if err := s.DB.Find(&boxes).Error; err != nil || len(boxes) != 1 || boxes[0].EventType != "device.online" {
		t.Fatal("重复心跳不应再次通知", boxes, err)
	}
	marker := errors.New("outbox unavailable")
	if err := s.DB.Callback().Create().Before("gorm:create").Register("test:fail_device_outbox", func(tx *gorm.DB) {
		if tx.Statement.Table == "open_event_outbox" {
			tx.AddError(marker)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := change(false); !errors.Is(err, marker) {
		t.Fatal("通知错误必须返回协议层重试", err)
	}
	var dev ipc.Device
	if err := s.DB.First(&dev, "id = ?", "d1").Error; err != nil || !dev.IsOnline {
		t.Fatal("通知写入失败未回滚设备状态", dev.IsOnline, err)
	}
	if err := s.DB.Callback().Create().Remove("test:fail_device_outbox"); err != nil {
		t.Fatal(err)
	}
	if err := change(false); err != nil {
		t.Fatal(err)
	}
	if err := change(true); err != nil {
		t.Fatal(err)
	}
	// 通知已写入后，通道状态保存失败也必须把通知和设备状态一起回滚。
	if err := s.DB.Callback().Update().Before("gorm:update").Register("test:fail_device_channels", func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" {
			tx.AddError(marker)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := change(false); !errors.Is(err, marker) {
		t.Fatal(err)
	}
	if err := s.DB.Order("id").Find(&boxes).Error; err != nil || len(boxes) != 3 {
		t.Fatal("通道保存失败留下了未提交状态的通知", boxes, err)
	}
	if err := s.DB.First(&dev, "id = ?", "d1").Error; err != nil || !dev.IsOnline {
		t.Fatal("通道保存失败未回滚设备", err)
	}
	if err := s.DB.Callback().Update().Remove("test:fail_device_channels"); err != nil {
		t.Fatal(err)
	}
	// TCP 启动恢复会清理旧进程绑定，但不把本次清理推送为设备离线。
	if err := s.DB.Model(&ipc.Device{}).Where("id IN ?", []string{"d1", "d2"}).Update("transport", "tcp").Error; err != nil {
		t.Fatal(err)
	}
	if err := cache.LoadDeviceToMemoryContext(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Order("id").Find(&boxes).Error; err != nil || len(boxes) != 3 || boxes[1].EventType != "device.offline" || boxes[2].ID <= boxes[1].ID {
		t.Fatal("上下线通知不完整或启动清理误报", boxes, err)
	}
	// 新建且已探测在线的 ONVIF 设备，以及改凭据导致的离线，也使用同一事务入口。
	created := ipc.Device{ID: "GB_created", DeviceID: "gb-created", IsOnline: true, Password: "old"}
	if err := cache.Device().Create(t.Context(), &created); err != nil {
		t.Fatal(err)
	}
	if err := cache.Device().Update(t.Context(), &created, func(d *ipc.Device) error { d.Password = "new"; return nil }, orm.Where("id = ?", created.ID)); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Order("id").Find(&boxes).Error; err != nil || len(boxes) != 5 || !strings.Contains(boxes[4].Payload, "credentials_changed") {
		t.Fatal(boxes, err)
	}
}

func TestDeviceStatusSubscriptionMigrationAndFilters(t *testing.T) {
	s, p, _ := fixture(t)
	legacy := Subscription{ID: "legacy", AppID: p.App.ID, Status: "paused", Revision: 4, Labels: []string{"person"}}
	if err := s.DB.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"event_types", "device_ids"} {
		if err := s.DB.Exec("ALTER TABLE open_subscriptions DROP COLUMN " + column).Error; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := New(s.DB, s.Config, s.Host); err == nil {
		t.Fatal("未迁移不能启用新订阅")
	}
	for range 2 {
		if err := Migrate(s.DB); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DB.First(&legacy, "id = ?", legacy.ID).Error; err != nil || legacy.Revision != 4 || !Has(legacy.Labels, "person") || !subscriptionAccepts(legacy, "event.created") || subscriptionAccepts(legacy, "device.offline") {
		t.Fatal("旧订阅升级改变了语义", legacy, err)
	}
	target, err := s.SaveTarget(p.App.ID, "", "https://receiver.example/status", nil, "approved", 0, "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []Subscription{
		{EventTypes: []string{"device.unknown"}},
		{EventTypes: []string{"device.online"}, ChannelIDs: []string{"c1"}},
		{EventTypes: []string{"device.offline"}, Labels: []string{"person"}},
		{EventTypes: []string{"device.offline"}, DeviceIDs: []string{"d2"}},
	} {
		row.CallbackTargetID, row.Status = target.ID, "paused"
		if _, err := s.SaveSubscription(p, row, true); err == nil {
			t.Fatal("不合法或未授权的筛选被接受", row)
		}
	}
	for _, tc := range []struct {
		sub  Subscription
		box  Outbox
		want bool
	}{
		{Subscription{}, Outbox{DeviceID: "d1", EventType: "device.online"}, false},
		{Subscription{}, Outbox{DeviceID: "d1", EventType: "event.created"}, true},
		{Subscription{DeviceIDs: []string{"d1"}}, Outbox{ChannelID: "c1", EventType: "event.created"}, true},
		{Subscription{DeviceIDs: []string{"d2"}}, Outbox{ChannelID: "c1", EventType: "event.created"}, false},
		{Subscription{DeviceIDs: []string{"d1"}}, Outbox{DeviceID: "d2", ChannelID: "c1", EventType: "event.created"}, false},
		{Subscription{EventTypes: []string{"device.online"}}, Outbox{DeviceID: "d1", EventType: "device.offline"}, false},
		{Subscription{EventTypes: []string{"device.online"}}, Outbox{DeviceID: "d1", EventType: "device.online"}, true},
		{Subscription{EventTypes: deviceNotificationTypes}, Outbox{DeviceID: "d2", EventType: "device.online"}, false},
		{Subscription{EventTypes: deviceNotificationTypes, DeviceIDs: []string{"d2"}}, Outbox{DeviceID: "d1", EventType: "device.online"}, false},
	} {
		if got, err := s.eventVisibility(p.App, tc.sub, tc.box); err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	noRead := p.App
	noRead.Scopes = []string{"event:subscribe"}
	if s.visibleEvent(noRead, Subscription{EventTypes: deviceNotificationTypes}, Outbox{DeviceID: "d1", EventType: "device.online"}) {
		t.Fatal("撤销 device:read 后仍可收到设备通知")
	}
}

func TestDeviceStatusSignedDeliveryRetryAndRevocation(t *testing.T) {
	s, p, _ := fixture(t)
	if err := s.DB.Model(&App{}).Where("id = ?", p.App.ID).Update("delivery_limit", 20).Error; err != nil {
		t.Fatal(err)
	}
	target, err := s.SaveTarget(p.App.ID, "", "https://receiver.example/status", nil, "approved", 0, "admin")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.SaveSubscription(p, Subscription{CallbackTargetID: target.ID, EventTypes: deviceNotificationTypes, DeviceIDs: []string{"d1"}, Status: "paused"}, true)
	if err != nil {
		t.Fatal(err)
	}
	keyID, secret, err := s.RotateCallbackKey(p, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ValidateSecret(secret)
	if err := s.DB.First(&sub, "id = ?", sub.ID).Error; err != nil {
		t.Fatal(err)
	}
	sub.Status = "active"
	if sub, err = s.SaveSubscription(p, sub, false); err != nil {
		t.Fatal(err)
	}
	cache := ipccache.NewCache(ipcdb.NewDB(s.DB))
	cache.SetDeviceStatusHandler(EnqueueDeviceStatusChange)
	for _, code := range []string{"gb1", "gb2"} {
		if err := cache.ChangeContext(ipc.WithDeviceStatusReason(t.Context(), "registration"), code, func(dev *ipc.Device) error { dev.IsOnline = true; return nil }, nil); err != nil {
			t.Fatal(err)
		}
	}
	s.fanout()
	s.fanout()
	var deliveries []Delivery
	if err := s.DB.Find(&deliveries).Error; err != nil || len(deliveries) != 1 {
		t.Fatal("授权隔离或分发去重失败", deliveries, err)
	}
	d := deliveries[0]
	attempts := 0
	s.callbackTransport = callbackTransportFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		body, _ := io.ReadAll(req.Body)
		h := req.Header
		want := "OWL-CALLBACK-HMAC-SHA256 Signature=" + CallbackSign(key, CallbackCanonical(h.Get("X-Owl-Timestamp"), h.Get("X-Owl-Nonce"), "0", d.ID, Hash(body)))
		if h.Get("X-Owl-Event-ID") != "0" || h.Get("X-Owl-Callback-Key-ID") != keyID || h.Get("X-Owl-Delivery-ID") != d.ID || h.Get("Authorization") != want {
			t.Error("设备回调签名不正确")
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["device_id"] != "d1" || payload["device_code"] != "gb1" || payload["is_online"] != true || payload["previous_is_online"] != false || payload["event_type"] != "device.online" || payload["sequence"] != float64(d.OutboxID) || payload["notification_id"] != payload["sequence"] || payload["event_id"] != nil || payload["image_url"] != nil {
			t.Error("设备快照错误或混入告警媒体", string(body))
		}
		status := 204
		if attempts == 1 {
			status = 500
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	})
	s.deliver(context.Background(), d)
	if err := s.DB.First(&d, "id = ?", d.ID).Error; err != nil || d.State != "queued" || d.Attempt != 1 {
		t.Fatal("设备通知未安排重试", d, err)
	}
	if err := s.DB.Model(&d).Update("next_retry_at", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	s.deliver(context.Background(), d)
	if err := s.DB.First(&d, "id = ?", d.ID).Error; err != nil || d.State != "delivered" || d.Attempt != 2 || attempts != 2 {
		t.Fatal("设备通知未以原 delivery_id 重试成功", d, attempts, err)
	}
	if err := s.DB.Model(&d).Update("state", "failed").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Retry(p.App.ID, sub.ID, d.ID, "application"); err != nil {
		t.Fatal("设备通知受控重投失败", err)
	}
	if err := s.DB.Model(&App{}).Where("id = ?", p.App.ID).Update("scopes", `["event:subscribe"]`).Error; err != nil {
		t.Fatal(err)
	}
	s.deliver(context.Background(), d)
	if err := s.DB.First(&d, "id = ?", d.ID).Error; err != nil || d.State != "cancelled" || attempts != 2 {
		t.Fatal("撤权后设备通知仍被发送", d, attempts, err)
	}
}
