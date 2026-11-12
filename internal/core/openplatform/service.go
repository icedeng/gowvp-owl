package openplatform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/internal/core/ipc"
	"golang.org/x/time/rate"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type bucket struct {
	Tokens float64
	At     time.Time
}
type Service struct {
	callbackTransport http.RoundTripper
	resolveCallback   func(context.Context, string) ([]net.IPAddr, error)
	channels          sync.Map
	playbackLocks     [256]sync.Mutex
	deliveryMu        sync.Mutex
	delivering        map[string]int
	DB                *gorm.DB
	Config            Config
	Host              Host
	mu                sync.Mutex
	buckets           map[string]bucket
	execution         sync.Mutex // 首期单实例：副作用、撤权和名额预占按同一顺序执行。
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	auditFailures     atomic.Uint64
	connections       map[string]int
	bandwidth         map[string]*rate.Limiter
}

func Models() []any {
	return []any{&App{}, &Resource{}, &Credential{}, &Nonce{}, &Idempotency{}, &Audit{}, &Session{}, &Playback{}, &Ticket{}, &Snapshot{}, &Command{}, &Lease{}, &CallbackTarget{}, &Subscription{}, &Outbox{}, &Delivery{}}
}

// Migrate 先建立新去重索引，再移除旧唯一约束；历史投递和ID保持不变，可重复执行。
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(Models()...); err != nil {
		return err
	}
	for _, index := range []struct {
		model any
		name  string
	}{
		{&Outbox{}, "idx_open_event_outbox_event_id"}, {&Delivery{}, "idx_open_delivery_event"},
	} {
		if db.Migrator().HasIndex(index.model, index.name) {
			if err := db.Migrator().DropIndex(index.model, index.name); err != nil {
				return err
			}
		}
	}
	for _, model := range []any{&Outbox{}, &Delivery{}} {
		if err := db.Model(model).Where("event_type = ? OR event_type IS NULL", "").Update("event_type", "event.created").Error; err != nil {
			return err
		}
	}
	return nil
}
func New(db *gorm.DB, cfg Config, host Host) (*Service, error) {
	s := &Service{DB: db, Config: cfg, Host: host, buckets: map[string]bucket{}}
	if !cfg.Enabled {
		return s, nil
	}
	if db == nil {
		return nil, ErrUnavailable
	}
	for _, m := range Models() {
		if !db.Migrator().HasTable(m) {
			return nil, fmt.Errorf("开放平台数据库尚未迁移")
		}
	}
	if !db.Migrator().HasIndex(&Delivery{}, "idx_open_delivery_notification") || !db.Migrator().HasIndex(&Outbox{}, "idx_open_event_outbox_notification_key") || !db.Migrator().HasColumn(&Outbox{}, "notification_key") || !db.Migrator().HasColumn(&Outbox{}, "event_type") || !db.Migrator().HasColumn(&Delivery{}, "event_type") || db.Migrator().HasIndex(&Delivery{}, "idx_open_delivery_event") || db.Migrator().HasIndex(&Outbox{}, "idx_open_event_outbox_event_id") {
		return nil, fmt.Errorf("开放平台事件通知结构尚未迁移，请运行开放平台迁移工具")
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "open-snapshots"), 0700); err != nil {
		return nil, err
	}
	// 首期仅单实例；遗留播放会话失效，未知设备命令在 worker 中优先停止。
	if err := db.Model(&Session{}).Where("state IN ?", []string{"preparing", "ready"}).Update("state", "expired").Error; err != nil {
		return nil, err
	}
	if err := db.Model(&Playback{}).Where("state IN ?", []string{"preparing", "ready", "paused"}).Update("state", "expired").Error; err != nil {
		return nil, err
	}
	if err := db.Model(&Snapshot{}).Where("state IN ?", []string{"queued", "running"}).Update("state", "unknown").Error; err != nil {
		return nil, err
	}
	if err := db.Model(&Command{}).Where("state IN ?", []string{"pending", "confirmed", "unknown", "stop_failed"}).Updates(map[string]any{"state": "unknown", "next_stop_at": time.Now()}).Error; err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Service) Start(ctx context.Context) {
	if !s.Config.Enabled {
		return
	}
	ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.run(ctx) }()
}
func (s *Service) Close() {
	if s.cancel != nil {
		s.cancel()
		s.wg.Wait()
		s.shutdownPlaybacks()
	}
}
func (s *Service) Spawn(fn func()) { s.wg.Add(1); go func() { defer s.wg.Done(); fn() }() }
func (s *Service) App(id string) (App, error) {
	var a App
	err := s.DB.First(&a, "id = ?", id).Error
	return a, err
}
func (s *Service) Current(appID, credentialID string) (Principal, error) {
	var p Principal
	if err := s.DB.First(&p.App, "id = ? AND status = ?", appID, "enabled").Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return p, ErrAuth
		}
		return p, ErrUnavailable
	}
	if p.App.ExpiresAt != nil && !p.App.ExpiresAt.After(time.Now()) {
		return p, ErrAuth
	}
	if err := s.DB.First(&p.Credential, "id = ? AND app_id = ? AND status = ?", credentialID, appID, "active").Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return p, ErrAuth
		}
		return p, ErrUnavailable
	}
	if p.Credential.ExpiresAt != nil && !p.Credential.ExpiresAt.After(time.Now()) {
		return p, ErrAuth
	}
	return p, nil
}
func (s *Service) Lookup(ak string) (Principal, []byte, error) {
	var c Credential
	if err := s.DB.First(&c, "access_key = ?", ak).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Principal{}, nil, ErrAuth
		}
		return Principal{}, nil, ErrUnavailable
	}
	p, err := s.Current(c.AppID, c.ID)
	if err != nil {
		return p, nil, err
	}
	key, err := s.Decrypt(c.AppID, c.AccessKey, c.Secret)
	if err != nil {
		slog.Error("开放平台凭据解密失败，检查独立密钥环", "credential_id", c.ID, "key_id", c.Secret.KeyID)
		return p, nil, ErrUnavailable
	}
	return p, key, nil
}
func (s *Service) UseNonce(ak, nonce string, timestamp int64) error {
	r := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&Nonce{ak, nonce, time.Unix(timestamp+360, 0)})
	if r.Error != nil {
		return ErrUnavailable
	}
	if r.RowsAffected != 1 {
		return ErrAuth
	}
	return nil
}
func (s *Service) Limit(key string, qps, burst int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	b, ok := s.buckets[key]
	if !ok {
		if len(s.buckets) >= 10000 {
			for k, v := range s.buckets {
				if now.Sub(v.At) > time.Minute {
					delete(s.buckets, k)
				}
			}
			if len(s.buckets) >= 10000 {
				return false
			}
		}
		b = bucket{float64(burst), now}
	}
	b.Tokens += now.Sub(b.At).Seconds() * float64(qps)
	if b.Tokens > float64(burst) {
		b.Tokens = float64(burst)
	}
	b.At = now
	allowed := b.Tokens >= 1
	if allowed {
		b.Tokens--
	}
	s.buckets[key] = b
	return allowed
}
func (s *Service) CheckDevice(a App, id string) error {
	if id == "" || a.DeviceAccessMode != "all" && (a.DeviceAccessMode != "selected_devices" || !Has(a.DeviceIDs, id)) {
		return ErrNotFound
	}
	var n int64
	if e := s.DB.Model(&ipc.Device{}).Where("id = ?", id).Count(&n).Error; e != nil {
		return ErrUnavailable
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}
func (s *Service) AllowedDevice(a App, id string) bool { return s.CheckDevice(a, id) == nil }
func (s *Service) Channel(a App, id string) (ipc.Channel, error) {
	var ch ipc.Channel
	if e := s.DB.First(&ch, "id = ?", id).Error; e != nil {
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return ch, ErrNotFound
		}
		return ch, ErrUnavailable
	}
	return ch, s.CheckDevice(a, ch.DID)
}
func (s *Service) Scope(a App, scope string) error {
	if !Has(a.Scopes, scope) {
		return Fail(403, "SCOPE_DENIED", "未授予此接口权限")
	}
	return nil
}
func (s *Service) DeviceQuery(db *gorm.DB, a App, column string) *gorm.DB {
	if a.DeviceAccessMode == "all" {
		return db.Where(column+" IN (?)", s.DB.Model(&ipc.Device{}).Select("id"))
	}
	if a.DeviceAccessMode != "selected_devices" || len(a.DeviceIDs) == 0 {
		return db.Where("1=0")
	}
	return db.Where(column+" IN ?", a.DeviceIDs).Where(column+" IN (?)", s.DB.Model(&ipc.Device{}).Select("id"))
}
func (s *Service) ChannelQuery(db *gorm.DB, a App, column string) *gorm.DB {
	q := s.DeviceQuery(s.DB.Model(&ipc.Channel{}).Select("id"), a, "did")
	return db.Where(column+" IN (?)", q)
}
func ValidateApp(a *App) error {
	if a.Name == "" || len(a.Name) > 120 || len(a.Purpose) > 2000 || len(a.DeviceIDs) > 10000 {
		return Fail(400, "INVALID_APP", "应用名称或设备范围不合法")
	}
	if a.DeviceAccessMode != "all" && a.DeviceAccessMode != "selected_devices" {
		return Fail(400, "INVALID_DEVICE_SCOPE", "请选择全部设备或指定设备")
	}
	if a.DeviceAccessMode == "all" && len(a.DeviceIDs) > 0 || a.DeviceAccessMode == "selected_devices" && len(a.DeviceIDs) == 0 {
		return Fail(400, "INVALID_DEVICE_SCOPE", "设备范围与勾选设备不一致")
	}
	seen := map[string]bool{}
	for _, id := range a.DeviceIDs {
		if id == "" || seen[id] {
			return Fail(400, "INVALID_DEVICE_SCOPE", "设备ID为空或重复")
		}
		seen[id] = true
	}
	for _, v := range a.Scopes {
		if !Has(Scopes, v) {
			return Fail(400, "INVALID_SCOPE", "接口权限不合法")
		}
	}
	actions := append(append([]string{}, BasicActions...), "preset_set", "preset_delete", "cruise_add", "cruise_del", "cruise_speed", "cruise_stay", "cruise_start", "scan_start", "scan_left", "scan_right", "scan_speed", "aux_on", "aux_off")
	for _, v := range a.AllowedActions {
		if !Has(actions, v) {
			return Fail(400, "INVALID_ACTION", "云台动作不合法")
		}
	}
	for _, v := range a.CIDRs {
		if _, _, err := net.ParseCIDR(v); err != nil {
			return Fail(400, "INVALID_CIDR", "来源CIDR格式错误")
		}
	}
	if a.AllowAnyIP && len(a.CIDRs) > 0 {
		return Fail(400, "INVALID_CIDR", "不限来源不能同时配置CIDR")
	}
	if a.QPS < 1 || a.QPS > 1000 || a.Burst < a.QPS || a.Burst > 2000 || a.LiveLimit < 1 || a.LiveLimit > 100 || a.ChannelLimit < 1 || a.ChannelLimit > 100 || a.DownloadLimit < 1 || a.DownloadLimit > 20 || a.SnapshotLimit < 1 || a.SnapshotLimit > 20 || a.SubscriptionLimit < 1 || a.SubscriptionLimit > 100 || a.DeliveryLimit < 1 || a.DeliveryLimit > 20 {
		return Fail(400, "INVALID_QUOTA", "配额超出允许范围")
	}
	return nil
}
func DefaultApp() App {
	return App{Status: "disabled", Scopes: []string{}, DeviceAccessMode: "selected_devices", DeviceIDs: []string{}, AllowedActions: append([]string{}, BasicActions...), CIDRs: []string{}, QPS: 20, Burst: 40, LiveLimit: 5, ChannelLimit: 2, DownloadLimit: 2, SnapshotLimit: 2, SubscriptionLimit: 10, DeliveryLimit: 2, Revision: 1}
}
func (s *Service) audit(tx *gorm.DB, a Audit) error { return tx.Create(&a).Error }
func (s *Service) Record(a Audit) {
	if err := s.DB.Create(&a).Error; err != nil { /* 查询日志失败不改变已经完成的响应，由调用方指标监测。 */
		s.auditFailures.Add(1)
		slog.Error("开放平台审计写入失败", "failures", s.auditFailures.Load())
	}
}
func (s *Service) SaveApp(a App, create bool, actor string) (App, error) {
	s.execution.Lock()
	defer s.execution.Unlock()
	if err := ValidateApp(&a); err != nil {
		return a, err
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if a.DeviceAccessMode == "selected_devices" {
			var n int64
			if err := tx.Model(&ipc.Device{}).Where("id IN ?", a.DeviceIDs).Count(&n).Error; err != nil {
				return err
			}
			if n != int64(len(a.DeviceIDs)) {
				return Fail(400, "INVALID_DEVICE_SCOPE", "部分设备不存在")
			}
		}
		action := "app.update"
		if create {
			a.ID = uuid.NewString()
			a.Status = "disabled"
			a.Revision = 1
			a.CreatedAt = time.Now()
			action = "app.create"
			if err := tx.Create(&a).Error; err != nil {
				return err
			}
		} else {
			var old App
			if err := tx.First(&old, "id = ?", a.ID).Error; err != nil {
				return ErrNotFound
			}
			if old.Status == "archived" {
				return ErrConflict
			}
			a.Status = old.Status
			a.CreatedAt = old.CreatedAt
			revision := a.Revision
			a.Revision++
			r := tx.Model(&App{}).Where("id = ? AND revision = ?", a.ID, revision).Select("*").Updates(&a)
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return ErrConflict
			}
			if err := s.invalidate(tx, a.ID, ""); err != nil {
				return err
			}
		}
		if err := tx.Where("app_id = ?", a.ID).Delete(&Resource{}).Error; err != nil {
			return err
		}
		for _, id := range a.DeviceIDs {
			if err := tx.Create(&Resource{a.ID, "device", id}).Error; err != nil {
				return err
			}
		}
		return s.audit(tx, Audit{Actor: actor, AppID: a.ID, Action: action, ResourceID: a.ID, Result: "success", Status: 200})
	})
	return a, err
}
func (s *Service) invalidate(tx *gorm.DB, app, credential string) error {
	q := tx.Model(&Ticket{}).Where("app_id = ?", app)
	sessions := tx.Model(&Session{}).Where("app_id = ? AND state IN ?", app, []string{"preparing", "ready"})
	playbacks := tx.Model(&Playback{}).Where("app_id = ? AND state IN ?", app, []string{"preparing", "ready", "paused"})
	snap := tx.Model(&Snapshot{}).Where("app_id = ? AND state = ?", app, "queued")
	subs := tx.Model(&Subscription{}).Where("app_id = ? AND status = ?", app, "active")
	if credential != "" {
		q = q.Where("credential_id = ?", credential)
		sessions = sessions.Where("credential_id = ?", credential)
		playbacks = playbacks.Where("credential_id = ?", credential)
		snap = snap.Where("credential_id = ?", credential)
		subs = subs.Where("credential_id = ?", credential)
	}
	for _, r := range []*gorm.DB{q.Update("revoked", true), sessions.Update("state", "revoked"), playbacks.Update("state", "revoked"), snap.Update("state", "cancelled"), subs.Update("status", "paused")} {
		if r.Error != nil {
			return r.Error
		}
	}
	return nil
}
func (s *Service) SetStatus(id, status, actor string) error {
	s.execution.Lock()
	defer s.execution.Unlock()
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var a App
		if err := tx.First(&a, "id = ?", id).Error; err != nil {
			return ErrNotFound
		}
		if a.Status == "archived" {
			return ErrConflict
		}
		if status == "enabled" {
			if err := ValidateApp(&a); err != nil {
				return err
			}
			if len(a.Scopes) == 0 || !a.AllowAnyIP && len(a.CIDRs) == 0 || a.ExpiresAt != nil && !a.ExpiresAt.After(time.Now()) {
				return Fail(400, "APP_INCOMPLETE", "请配置接口权限、来源策略及有效期")
			}
			var n int64
			if err := tx.Model(&Credential{}).Where("app_id = ? AND status = ? AND (expires_at IS NULL OR expires_at > ?)", id, "active", time.Now()).Count(&n).Error; err != nil {
				return err
			}
			if n == 0 {
				return Fail(400, "APP_INCOMPLETE", "请先创建有效AK/SK")
			}
		} else {
			if status == "archived" && a.Status != "disabled" {
				return ErrConflict
			}
			if err := s.invalidate(tx, id, ""); err != nil {
				return err
			}
			if status == "archived" {
				if err := tx.Model(&Credential{}).Where("app_id = ?", id).Updates(map[string]any{"status": "revoked", "revoked_at": time.Now()}).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.Model(&a).Updates(map[string]any{"status": status, "revision": a.Revision + 1}).Error; err != nil {
			return err
		}
		return s.audit(tx, Audit{Actor: actor, AppID: id, Action: "app." + status, Status: 200, Result: "success"})
	})
}
func (s *Service) CreateCredential(app string, expires *time.Time, actor string) (Credential, string, error) {
	s.execution.Lock()
	defer s.execution.Unlock()
	var c Credential
	secret := Random(32)
	raw, _ := ValidateSecret(secret)
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var a App
		if err := tx.First(&a, "id = ?", app).Error; err != nil {
			return ErrNotFound
		}
		if a.Status == "archived" {
			return ErrConflict
		}
		var n int64
		if err := tx.Model(&Credential{}).Where("app_id = ? AND status = ? AND (expires_at IS NULL OR expires_at > ?)", app, "active", time.Now()).Count(&n).Error; err != nil {
			return err
		}
		if n >= 2 {
			return Fail(409, "CREDENTIAL_LIMIT", "最多同时存在两组有效凭据")
		}
		if expires != nil && !expires.After(time.Now()) {
			return Fail(400, "INVALID_EXPIRY", "到期时间必须在未来")
		}
		c = Credential{ID: uuid.NewString(), AppID: app, AccessKey: "owl_ak_" + Random(24), Status: "active", ExpiresAt: expires}
		encrypted, e := s.Encrypt(app, c.AccessKey, raw)
		if e != nil {
			return e
		}
		c.Secret = encrypted
		if err := tx.Create(&c).Error; err != nil {
			return err
		}
		return s.audit(tx, Audit{Actor: actor, AppID: app, CredentialID: c.ID, Action: "credential.create", Status: 200, Result: "success"})
	})
	if err != nil {
		secret = ""
	}
	return c, secret, err
}
func (s *Service) Revoke(app, id, actor string) error {
	s.execution.Lock()
	defer s.execution.Unlock()
	return s.DB.Transaction(func(tx *gorm.DB) error {
		r := tx.Model(&Credential{}).Where("app_id = ? AND id = ?", app, id).Updates(map[string]any{"status": "revoked", "revoked_at": time.Now()})
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected == 0 {
			return ErrNotFound
		}
		if err := s.invalidate(tx, app, id); err != nil {
			return err
		}
		return s.audit(tx, Audit{Actor: actor, AppID: app, CredentialID: id, Action: "credential.revoke", Status: 200, Result: "success"})
	})
}
func (s *Service) Begin(p Principal, operation, key, fingerprint string) (string, error) {
	if key == "" || len(key) > 128 {
		return "", Fail(400, "IDEMPOTENCY_REQUIRED", "请提供不超过128字符的Idempotency-Key")
	}
	row := Idempotency{AppID: p.App.ID, Operation: operation, Key: key, Fingerprint: fingerprint, Status: "pending", ExpiresAt: time.Now().Add(24 * time.Hour)}
	r := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if r.Error != nil {
		return "", r.Error
	}
	if r.RowsAffected == 1 {
		return "", nil
	}
	if err := s.DB.First(&row, "app_id = ? AND operation = ? AND key = ?", p.App.ID, operation, key).Error; err != nil {
		return "", err
	}
	if row.Fingerprint != fingerprint {
		return "", Fail(409, "IDEMPOTENCY_MISMATCH", "幂等键已用于其他请求")
	}
	if row.Status != "done" {
		return "", Fail(409, "OPERATION_PENDING", "操作执行中或结果待核对，请查询任务状态")
	}
	return row.ResourceID, nil
}
func (s *Service) Finish(p Principal, op, key, id string) error {
	return s.DB.Model(&Idempotency{}).Where("app_id = ? AND operation = ? AND key = ?", p.App.ID, op, key).Updates(map[string]any{"resource_id": id, "status": "done"}).Error
}
func marshal(v any) string { b, _ := json.Marshal(v); return string(b) }

func (s *Service) channelLock(id string) *sync.Mutex {
	v, _ := s.channels.LoadOrStore(id, new(sync.Mutex))
	return v.(*sync.Mutex)
}
