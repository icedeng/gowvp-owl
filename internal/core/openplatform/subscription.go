package openplatform

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

func (s *Subscription) AfterFind(*gorm.DB) error { s.KeyConfigured = len(s.Keys) > 0; return nil }

func (s *Service) SaveTarget(app, id, raw string, cidrs []string, status string, revision int64, actor string) (CallbackTarget, error) {
	s.execution.Lock()
	defer s.execution.Unlock()
	var row CallbackTarget
	u, e := url.Parse(raw)
	public, _ := url.Parse(s.Config.PublicBaseURL)
	if e != nil || !s.Config.AllowsScheme(u.Scheme) || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.EqualFold(u.Hostname(), public.Hostname()) {
		return row, Fail(400, "INVALID_TARGET", "请提供独立的HTTPS回调目标；HTTP须由平台显式启用")
	}
	for _, v := range cidrs {
		if _, _, e := net.ParseCIDR(v); e != nil {
			return row, Fail(400, "INVALID_CIDR", "回调CIDR格式错误")
		}
	}
	if status == "approved" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.callbackAddresses(ctx, u.Hostname(), cidrs); err != nil {
			return row, Fail(400, "INVALID_TARGET", "回调目标无法解析或不在批准的地址范围")
		}
	}
	e = s.DB.Transaction(func(tx *gorm.DB) error {
		var parent App
		if err := tx.First(&parent, "id = ? AND status <> ?", app, "archived").Error; err != nil {
			return ErrNotFound
		}
		create := id == ""
		if create {
			row = CallbackTarget{ID: uuid.NewString(), AppID: app, URL: raw, CIDRs: cidrs, Status: status, Revision: 1}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		} else {
			if err := tx.First(&row, "id = ? AND app_id = ?", id, app).Error; err != nil {
				return ErrNotFound
			}
			changed := row.URL != raw || marshal(row.CIDRs) != marshal(cidrs)
			row.URL = raw
			row.CIDRs = cidrs
			row.Status = status
			if changed {
				row.Status = "pending"
			}
			row.Revision = revision + 1
			r := tx.Model(&CallbackTarget{}).Where("id = ? AND app_id = ? AND revision = ?", id, app, revision).Select("url", "cidrs", "status", "revision").Updates(&row)
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return ErrConflict
			}
			if row.Status != "approved" {
				if err := tx.Model(&Subscription{}).Where("callback_target_id = ? AND status = ?", id, "active").Updates(map[string]any{"status": "paused", "revision": gorm.Expr("revision + 1")}).Error; err != nil {
					return err
				}
			}
		}
		return s.audit(tx, Audit{Actor: actor, AppID: app, Action: "callback_target.save", ResourceID: row.ID, Result: row.Status, Status: 200})
	})
	return row, e
}
func targetIPAllowed(ip net.IP, cidrs []string) bool {
	if ip != nil && (ip.Equal(net.ParseIP("100.100.100.200")) || ip.Equal(net.ParseIP("168.63.129.16")) || ip.Equal(net.ParseIP("fd00:ec2::254"))) {
		return false
	}
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if len(cidrs) > 0 {
		for _, v := range cidrs {
			_, n, e := net.ParseCIDR(v)
			if e == nil && n.Contains(ip) {
				return true
			}
		}
		return false
	}
	if ip.IsPrivate() {
		return false
	}
	return ip.IsGlobalUnicast()
}

// callbackClient 在实际连接时解析并固定批准的IP，避免校验后DNS重绑定。
func (s *Service) callbackAddresses(ctx context.Context, host string, cidrs []string) ([]net.IPAddr, error) {
	resolve := s.resolveCallback
	if resolve == nil {
		resolve = net.DefaultResolver.LookupIPAddr
	}
	addrs, e := resolve(ctx, host)
	if e != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("回调目标解析失败")
	}
	for _, addr := range addrs {
		if !targetIPAllowed(addr.IP, cidrs) {
			return nil, fmt.Errorf("回调目标地址不在批准范围")
		}
		for _, denied := range s.Config.CallbackDenyCIDRs {
			if denied.Contains(addr.IP) {
				return nil, fmt.Errorf("禁止回调平台管理目标")
			}
		}
	}
	return addrs, nil
}
func (s *Service) callbackClient(target CallbackTarget) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ResponseHeaderTimeout: 8 * time.Second, DisableKeepAlives: true}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		addrs, e := s.callbackAddresses(ctx, host, target.CIDRs)
		if e != nil || len(addrs) == 0 {
			return nil, fmt.Errorf("回调目标解析失败")
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addrs[0].IP.String(), port))
	}
	var roundTripper http.RoundTripper = transport
	if s.callbackTransport != nil {
		roundTripper = s.callbackTransport
	}
	return &http.Client{Transport: roundTripper, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func (s *Service) SaveSubscription(p Principal, row Subscription, create bool) (Subscription, error) {
	s.execution.Lock()
	defer s.execution.Unlock()
	latest, e := s.Validate(p, "event:subscribe", "")
	if e != nil {
		return row, e
	}
	if !Has([]string{"paused", "active", "cancelled"}, row.Status) {
		return row, Fail(400, "INVALID_STATUS", "订阅状态不合法")
	}
	if !create && row.Status == "cancelled" {
		err := s.DB.Transaction(func(tx *gorm.DB) error {
			r := tx.Model(&Subscription{}).Where("id = ? AND app_id = ? AND revision = ?", row.ID, p.App.ID, row.Revision).Updates(map[string]any{"status": "cancelled", "revision": row.Revision + 1})
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return ErrConflict
			}
			return s.audit(tx, Audit{AppID: p.App.ID, Action: "subscription.cancel", ResourceID: row.ID, Status: 200, Result: "cancelled"})
		})
		row.Revision++
		return row, err
	}
	var target CallbackTarget
	if s.DB.First(&target, "id = ? AND app_id = ?", row.CallbackTargetID, p.App.ID).Error != nil {
		return row, ErrNotFound
	}
	for _, ch := range row.ChannelIDs {
		if _, e = s.Channel(latest.App, ch); e != nil {
			return row, e
		}
	}
	if len(row.ChannelIDs) > 10000 || len(row.DeviceIDs) > 10000 || len(row.Labels) > 100 || len(row.EventTypes) > 5 {
		return row, Fail(400, "INVALID_FILTER", "订阅筛选条件过多")
	}
	for _, kind := range row.EventTypes {
		if !Has(alarmNotificationTypes, kind) && !isDeviceNotification(kind) {
			return row, Fail(400, "INVALID_EVENT_TYPE", "不支持的通知类型")
		}
		if isDeviceNotification(kind) {
			if e := s.Scope(latest.App, "device:read"); e != nil {
				return row, e
			}
			if len(row.ChannelIDs) > 0 || len(row.Labels) > 0 {
				return row, Fail(400, "INVALID_FILTER", "设备上下线通知不能使用通道或告警标签筛选，请拆分订阅")
			}
		}
	}
	for _, device := range row.DeviceIDs {
		if e := s.CheckDevice(latest.App, device); e != nil {
			return row, e
		}
	}
	if row.ExpiresAt != nil && !row.ExpiresAt.After(time.Now()) {
		return row, Fail(400, "INVALID_EXPIRY", "订阅到期时间必须在未来")
	}
	e = s.DB.Transaction(func(tx *gorm.DB) error {
		if create {
			var n int64
			if e := tx.Model(&Subscription{}).Where("app_id = ? AND status <> ?", p.App.ID, "cancelled").Count(&n).Error; e != nil {
				return e
			}
			if n >= int64(latest.App.SubscriptionLimit) {
				return Fail(429, "SUBSCRIPTION_LIMIT", "订阅数量超过配额")
			}
			row.ID = uuid.NewString()
			row.AppID = p.App.ID
			row.CredentialID = p.Credential.ID
			row.Status = "paused"
			row.Revision = 1
			if e := tx.Create(&row).Error; e != nil {
				return e
			}
		} else {
			var old Subscription
			if e := tx.First(&old, "id = ? AND app_id = ?", row.ID, p.App.ID).Error; e != nil {
				return ErrNotFound
			}
			if old.Status == "cancelled" {
				return ErrConflict
			}
			row.Keys = old.Keys
			row.CredentialID = p.Credential.ID
			row.CreatedAt = old.CreatedAt
			if row.Status == "active" && (target.Status != "approved" || len(row.Keys) == 0) {
				return Fail(400, "SUBSCRIPTION_INCOMPLETE", "请先审核回调目标并配置签名密钥")
			}
			revision := row.Revision
			row.Revision++
			r := tx.Model(&Subscription{}).Where("id = ? AND app_id = ? AND revision = ?", row.ID, p.App.ID, revision).Select("callback_target_id", "event_types", "device_ids", "channel_ids", "labels", "status", "credential_id", "expires_at", "revision").Updates(&row)
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return ErrConflict
			}
		}
		return s.audit(tx, Audit{AppID: p.App.ID, CredentialID: p.Credential.ID, Action: "subscription.save", ResourceID: row.ID, Result: row.Status, Status: 200})
	})
	row.KeyConfigured = len(row.Keys) > 0
	return row, e
}
func (s *Service) RotateCallbackKey(p Principal, id string) (string, string, error) {
	s.execution.Lock()
	defer s.execution.Unlock()
	if _, e := s.Validate(p, "event:subscribe", ""); e != nil {
		return "", "", e
	}
	keyID := uuid.NewString()
	raw := Random(32)
	b, _ := ValidateSecret(raw)
	encrypted, e := s.Encrypt(p.App.ID, "callback:"+id+":"+keyID, b)
	if e != nil {
		return "", "", e
	}
	e = s.DB.Transaction(func(tx *gorm.DB) error {
		var row Subscription
		if e := tx.First(&row, "id = ? AND app_id = ? AND status <> ?", id, p.App.ID, "cancelled").Error; e != nil {
			return ErrNotFound
		}
		keys := []SigningKey{}
		if len(row.Keys) > 0 {
			last := row.Keys[len(row.Keys)-1]
			expiry := time.Now().Add(24 * time.Hour)
			last.ExpiresAt = &expiry
			keys = append(keys, last)
		}
		keys = append(keys, SigningKey{ID: keyID, Secret: encrypted})
		row.Keys = keys
		if e := tx.Model(&row).Select("keys").Updates(&row).Error; e != nil {
			return e
		}
		return s.audit(tx, Audit{AppID: p.App.ID, CredentialID: p.Credential.ID, Action: "callback_key.rotate", ResourceID: id, Status: 200, Result: "success"})
	})
	return keyID, raw, e
}
func CallbackCanonical(timestamp, nonce, event, delivery, digest string) string {
	return strings.Join([]string{"OWL-CALLBACK-HMAC-SHA256", timestamp, nonce, event, delivery, digest}, "\n")
}
func CallbackSign(key []byte, canonical string) string {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(canonical))
	return hex.EncodeToString(m.Sum(nil))
}
func (s *Service) Retry(app, subscription, id, actor string) error {
	s.execution.Lock()
	defer s.execution.Unlock()
	return s.DB.Transaction(func(tx *gorm.DB) error {
		read := &Service{DB: tx}
		var d Delivery
		q := tx.Where("id = ? AND app_id = ?", id, app)
		if subscription != "" {
			q = q.Where("subscription_id = ?", subscription)
		}
		if e := q.First(&d).Error; e != nil {
			return ErrNotFound
		}
		if d.State != "dead" && d.State != "failed" {
			return ErrConflict
		}
		var sub Subscription
		if e := tx.First(&sub, "id = ? AND status = ?", d.SubscriptionID, "active").Error; e != nil {
			return ErrConflict
		}
		p, e := read.Current(sub.AppID, sub.CredentialID)
		if e != nil {
			return e
		}
		if e = s.Scope(p.App, "event:subscribe"); e != nil {
			return e
		}
		var box Outbox
		if e = tx.First(&box, "id = ?", d.OutboxID).Error; e != nil {
			return e
		}
		if !read.visibleEvent(p.App, sub, box) {
			return ErrNotFound
		}
		if e = tx.Model(&d).Updates(map[string]any{"state": "queued", "attempt": 0, "next_retry_at": time.Now()}).Error; e != nil {
			return e
		}
		return s.audit(tx, Audit{Actor: actor, AppID: app, Action: "delivery.retry", ResourceID: id, Status: 200, Result: "queued"})
	})
}
