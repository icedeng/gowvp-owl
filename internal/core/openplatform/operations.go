package openplatform

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/internal/core/capture"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/recording"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Service) Validate(p Principal, scope, channel string) (Principal, error) {
	latest, e := s.Current(p.App.ID, p.Credential.ID)
	if e != nil {
		return latest, e
	}
	if e = s.Scope(latest.App, scope); e != nil {
		return latest, e
	}
	if channel != "" {
		_, e = s.Channel(latest.App, channel)
	}
	return latest, e
}
func (s *Service) CreateSession(ctx context.Context, p Principal, ch, protocol string) (Session, error) {
	s.execution.Lock()
	locked := true
	defer func() {
		if locked {
			s.execution.Unlock()
		}
	}()
	var row Session
	p, e := s.Validate(p, "live:play", ch)
	if e != nil {
		return row, e
	}
	if protocol != "hls" && protocol != "flv" {
		return row, Fail(400, "UNSUPPORTED_PROTOCOL", "首期支持hls和flv")
	}
	e = s.DB.Transaction(func(tx *gorm.DB) error {
		var total, perChannel int64
		q := tx.Model(&Session{}).Where("app_id = ? AND state IN ? AND expires_at > ?", p.App.ID, []string{"preparing", "ready"}, time.Now())
		if err := q.Count(&total).Error; err != nil {
			return err
		}
		if err := q.Where("channel_id = ?", ch).Count(&perChannel).Error; err != nil {
			return err
		}
		if total >= int64(p.App.LiveLimit) || perChannel >= int64(p.App.ChannelLimit) {
			return Fail(429, "CONCURRENT_LIMIT", "播放会话超过配额")
		}
		var global int64
		if err := tx.Model(&Session{}).Where("state = ?", "preparing").Count(&global).Error; err != nil {
			return err
		}
		if global >= 10 {
			return Fail(429, "SOURCE_LIMIT", "拉流准备任务已满")
		}
		var active int64
		if err := tx.Model(&Session{}).Where("state IN ? AND expires_at > ?", []string{"preparing", "ready"}, time.Now()).Count(&active).Error; err != nil {
			return err
		}
		if active >= 500 {
			return Fail(429, "CONCURRENT_LIMIT", "全局播放会话已满")
		}
		row = Session{ID: uuid.NewString(), AppID: p.App.ID, CredentialID: p.Credential.ID, ChannelID: ch, Protocol: protocol, State: "preparing", Revision: p.App.Revision, ExpiresAt: time.Now().Add(30 * time.Minute)}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return s.audit(tx, Audit{AppID: p.App.ID, CredentialID: p.Credential.ID, Action: "live.create", ResourceID: row.ID, Result: "accepted", Status: 202})
	})
	if e != nil {
		return Session{}, e
	}
	// 不停止共享源；设备拉流由宿主现有的复用和无人观看策略管理。
	s.execution.Unlock()
	locked = false
	source, e := s.Host.Start(ctx, ch)
	s.execution.Lock()
	locked = true
	if e != nil {
		row.State = "failed"
		_ = s.DB.Model(&row).Update("state", row.State).Error
		return row, Fail(503, "SOURCE_FAILED", "媒体源准备失败")
	}
	latest, check := s.Validate(p, "live:play", ch)
	if check != nil || latest.App.Revision != row.Revision {
		return row, ErrConflict
	}
	row.Source = source
	row.State = "ready"
	e = s.DB.Model(&row).Select("source", "state").Updates(&row).Error
	return row, e
}
func (s *Service) ReleaseSession(app, id string) error {
	s.execution.Lock()
	defer s.execution.Unlock()
	r := s.DB.Model(&Session{}).Where("id = ? AND app_id = ?", id, app).Update("state", "released")
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
func (s *Service) IssueTicket(p Principal, kind, id string) (string, Ticket, error) {
	s.execution.Lock()
	defer s.execution.Unlock()
	scope := mediaScope(kind)
	var t Ticket
	if scope == "" {
		return "", t, ErrNotFound
	}
	p, e := s.Validate(p, scope, "")
	if e != nil {
		return "", t, e
	}
	t = Ticket{AppID: p.App.ID, CredentialID: p.Credential.ID, Revision: p.App.Revision, Kind: kind, ResourceID: id, ExpiresAt: time.Now().Add(5 * time.Minute)}
	switch kind {
	case "live":
		var row Session
		if e = s.DB.First(&row, "id = ? AND app_id = ? AND state = ? AND expires_at > ?", id, p.App.ID, "ready", time.Now()).Error; e != nil {
			return "", t, ErrNotFound
		}
		if _, e = s.Current(row.AppID, row.CredentialID); e != nil {
			return "", t, e
		}
		t.ChannelID = row.ChannelID
		t.SessionID = row.ID
		if row.ExpiresAt.Before(t.ExpiresAt) {
			t.ExpiresAt = row.ExpiresAt
		}
	case "snapshot":
		var row Snapshot
		if e = s.DB.First(&row, "id = ? AND app_id = ? AND state IN ? AND expires_at > ?", id, p.App.ID, []string{"queued", "running", "succeeded"}, time.Now()).Error; e != nil {
			return "", t, ErrNotFound
		}
		if _, e = s.Current(row.AppID, row.CredentialID); e != nil {
			return "", t, e
		}
		t.ChannelID = row.ChannelID
		if row.ExpiresAt.Before(t.ExpiresAt) {
			t.ExpiresAt = row.ExpiresAt
		}
	case "capture":
		var row capture.Record
		if e = s.DB.First(&row, "id = ? AND state = ?", id, "succeeded").Error; e != nil {
			return "", t, ErrNotFound
		}
		t.ChannelID = row.ChannelID
	case "recording", "download":
		var row recording.Recording
		if e = s.DB.First(&row, "id = ? AND delete_flag = ?", id, false).Error; e != nil {
			return "", t, ErrNotFound
		}
		t.ChannelID = row.CID
	case "event":
		var row event.Event
		if e = s.DB.First(&row, "id = ?", id).Error; e != nil || row.ImagePath == "" {
			return "", t, ErrNotFound
		}
		t.ChannelID = row.CID
		t.DeviceID = row.DID
	}
	if t.ChannelID != "" {
		if ch, err := s.Channel(p.App, t.ChannelID); err != nil {
			return "", t, err
		} else if kind == "event" && t.DeviceID != "" && ch.DID != t.DeviceID {
			return "", t, ErrNotFound
		}
		if _, e = s.Channel(p.App, t.ChannelID); e != nil {
			return "", t, e
		}
	} else if !s.AllowedDevice(p.App, t.DeviceID) {
		return "", t, ErrNotFound
	}
	raw := Random(32)
	t.Hash = Hash([]byte(raw))
	e = s.DB.Create(&t).Error
	return raw, t, e
}
func (s *Service) CheckTicket(raw, kind, id string) (Ticket, Principal, error) {
	var t Ticket
	var p Principal
	if len(raw) > 128 || len(raw) < 32 {
		return t, p, ErrAuth
	}
	if e := s.DB.First(&t, "hash = ? AND kind = ? AND resource_id = ? AND revoked = ? AND expires_at > ?", Hash([]byte(raw)), kind, id, false, time.Now()).Error; e != nil {
		return t, p, ErrAuth
	}
	scope := mediaScope(kind)
	p, e := s.Current(t.AppID, t.CredentialID)
	if e != nil {
		return t, p, e
	}
	p, e = s.Validate(p, scope, t.ChannelID)
	if e != nil {
		return t, p, e
	}
	if p.App.Revision != t.Revision || t.ChannelID == "" && !s.AllowedDevice(p.App, t.DeviceID) {
		return t, p, ErrAuth
	}
	if kind == "snapshot" {
		var row Snapshot
		if e := s.DB.First(&row, "id = ? AND app_id = ? AND state IN ? AND expires_at > ?", id, p.App.ID, []string{"queued", "running", "succeeded"}, time.Now()).Error; e != nil {
			return t, p, ErrAuth
		}
		if _, e := s.Current(row.AppID, row.CredentialID); e != nil {
			return t, p, e
		}
	}
	if kind == "capture" {
		var row capture.Record
		if s.DB.First(&row, "id = ? AND channel_id = ? AND state = ?", id, t.ChannelID, "succeeded").Error != nil {
			return t, p, ErrNotFound
		}
	}
	if kind == "live" {
		var row Session
		if e = s.DB.First(&row, "id = ? AND app_id = ? AND state = ? AND expires_at > ?", t.SessionID, t.AppID, "ready", time.Now()).Error; e != nil {
			return t, p, ErrAuth
		}
		if _, e = s.Current(row.AppID, row.CredentialID); e != nil {
			return t, p, e
		}
	}
	return t, p, nil
}
func (s *Service) CreateSnapshot(p Principal, ch string) (Snapshot, error) {
	s.execution.Lock()
	defer s.execution.Unlock()
	var row Snapshot
	p, e := s.Validate(p, "snapshot:capture", ch)
	if e != nil {
		return row, e
	}
	if !s.Limit("snapshot:"+ch, 1, 1) {
		return row, Fail(429, "SNAPSHOT_COOLDOWN", "通道抓拍冷却中")
	}
	var last Snapshot
	if e = s.DB.Where("channel_id = ? AND created_at > ?", ch, time.Now().Add(-2*time.Second)).First(&last).Error; e == nil {
		return row, Fail(429, "SNAPSHOT_COOLDOWN", "通道抓拍冷却中")
	}
	var n int64
	if e = s.DB.Model(&Snapshot{}).Where("app_id = ? AND state IN ?", p.App.ID, []string{"queued", "running"}).Count(&n).Error; e != nil {
		return row, e
	}
	if n >= int64(p.App.SnapshotLimit) {
		return row, Fail(429, "CONCURRENT_LIMIT", "抓拍并发已满")
	}
	var global int64
	if e = s.DB.Model(&Snapshot{}).Where("state IN ?", []string{"queued", "running"}).Count(&global).Error; e != nil {
		return row, e
	}
	if global >= 10 {
		return row, Fail(429, "CONCURRENT_LIMIT", "全局抓拍并发已满")
	}
	row = Snapshot{ID: uuid.NewString(), AppID: p.App.ID, CredentialID: p.Credential.ID, ChannelID: ch, State: "queued", Revision: p.App.Revision, ExpiresAt: time.Now().Add(24 * time.Hour)}
	e = s.DB.Transaction(func(tx *gorm.DB) error {
		if e := tx.Create(&row).Error; e != nil {
			return e
		}
		return s.audit(tx, Audit{AppID: row.AppID, CredentialID: row.CredentialID, Action: "snapshot.create", ResourceID: row.ID, Status: 202, Result: "accepted"})
	})
	return row, e
}
func (s *Service) Capture(id string) {
	var row Snapshot
	if s.DB.First(&row, "id = ? AND state = ?", id, "queued").Error != nil {
		return
	}
	p, e := s.Current(row.AppID, row.CredentialID)
	if e == nil {
		_, e = s.Validate(p, "snapshot:capture", row.ChannelID)
	}
	if e != nil {
		_ = s.DB.Model(&row).Update("state", "cancelled").Error
		return
	}
	r := s.DB.Model(&Snapshot{}).Where("id = ? AND state = ?", id, "queued").Update("state", "running")
	if r.Error != nil || r.RowsAffected != 1 {
		return
	}
	path := filepath.Join(s.Config.DataDir, "open-snapshots", row.ID+".jpg")
	timeout := 60 * time.Second
	if host, ok := s.Host.(interface{ SnapshotTimeout() time.Duration }); ok {
		if value := host.SnapshotTimeout(); value > 0 && value <= 2*time.Hour {
			timeout = value
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	e = s.Host.Snapshot(ctx, row.ChannelID, path)
	state := "succeeded"
	if e != nil {
		state = "failed"
		_ = os.Remove(path)
	}
	p, check := s.Current(row.AppID, row.CredentialID)
	if check == nil {
		_, check = s.Validate(p, "snapshot:capture", row.ChannelID)
	}
	if check != nil || p.App.Revision != row.Revision {
		state = "cancelled"
		_ = os.Remove(path)
	}
	var captured *time.Time
	if state == "succeeded" {
		now := time.Now()
		captured = &now
	}
	errText := ""
	if e != nil {
		errText = "抓拍失败或设备未返回新图片"
	}
	_ = s.DB.Model(&Snapshot{}).Where("id = ?", id).Updates(map[string]any{"state": state, "path": path, "error": errText, "captured_at": captured}).Error
	s.Record(Audit{AppID: row.AppID, CredentialID: row.CredentialID, Action: "snapshot.result", ResourceID: id, Status: 200, Result: state})
}
func continuous(action string) bool {
	return !Has([]string{"preset_set", "preset_delete", "cruise_add", "cruise_del", "cruise_speed", "cruise_stay", "scan_left", "scan_right", "scan_speed", "aux_on", "aux_off"}, action)
}
func (s *Service) CreateCommand(ctx context.Context, p Principal, ch string, in PTZInput) (Command, error) {
	if _, e := s.Validate(p, "ptz:control", ch); e != nil {
		return Command{}, e
	}
	lock := s.channelLock(ch)
	lock.Lock()
	defer lock.Unlock()
	s.execution.Lock()
	locked := true
	defer func() {
		if locked {
			s.execution.Unlock()
		}
	}()
	var row Command
	p, e := s.Validate(p, "ptz:control", ch)
	if e != nil {
		return row, e
	}
	if !Has(p.App.AllowedActions, in.Action) {
		return row, Fail(403, "ACTION_DENIED", "未授权此云台动作")
	}
	caps, e := s.Host.Capabilities(ctx, ch)
	if e != nil {
		return row, e
	}
	if !Has(caps, in.Action) {
		return row, Fail(400, "UNSUPPORTED_ACTION", "设备不支持此动作")
	}
	if in.Duration == 0 {
		in.Duration = 1
	}
	if in.Duration < 1 || in.Duration > 3 || in.Speed > 100 || (strings.HasPrefix(in.Action, "preset_") || Has([]string{"cruise_add", "cruise_del"}, in.Action)) && (in.Preset < 1 || in.Preset > 255) || in.Value > 4095 {
		return row, Fail(400, "INVALID_PTZ", "持续时间须为1至3秒、速度1至100、预置位1至255")
	}
	if in.Speed == 0 {
		in.Speed = 30
	}
	if !s.Limit("ptz:"+p.App.ID, 2, 2) {
		return row, Fail(429, "RATE_LIMITED", "云台指令过于频繁")
	}
	row = Command{ID: uuid.NewString(), AppID: p.App.ID, CredentialID: p.Credential.ID, ChannelID: ch, Input: in, State: "pending", Revision: p.App.Revision, Deadline: time.Now().Add(time.Duration(in.Duration) * time.Second)}
	e = s.DB.Transaction(func(tx *gorm.DB) error {
		var active int64
		if e := tx.Model(&Lease{}).Count(&active).Error; e != nil {
			return e
		}
		if active >= 100 {
			return Fail(429, "PTZ_CAPACITY", "全局云台控制名额已满")
		}
		r := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&Lease{ChannelID: ch, CommandID: row.ID})
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return Fail(409, "PTZ_BUSY", "通道正在控制或等待停止核对")
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return s.audit(tx, Audit{AppID: p.App.ID, CredentialID: p.Credential.ID, Action: "ptz.execute", ResourceID: row.ID, Result: "accepted", Status: 202})
	})
	if e != nil {
		return Command{}, e
	}
	// 原始执行和停止在同一协调器中，所有其他入口也通过 GuardPTZ。
	s.execution.Unlock()
	locked = false
	commandCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	e = s.Host.PTZ(commandCtx, ch, in)
	row.State = "confirmed"
	if e != nil {
		row.State = "unknown"
		row.Error = "设备命令结果不明确，等待安全停止"
	}
	if !continuous(in.Action) && e == nil {
		row.State = "completed"
	}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if e := tx.Model(&row).Select("state", "error").Updates(&row).Error; e != nil {
			return e
		}
		if row.State == "completed" {
			if e := tx.Where("channel_id = ? AND command_id = ?", ch, row.ID).Delete(&Lease{}).Error; e != nil {
				return e
			}
		}
		return s.audit(tx, Audit{AppID: row.AppID, CredentialID: row.CredentialID, Action: "ptz.result", ResourceID: row.ID, Status: 200, Result: row.State})
	}); err != nil {
		return row, err
	}
	return row, nil
}
func (s *Service) stopLocked(ctx context.Context, row Command) error {
	var lease Lease
	if err := s.DB.First(&lease, "channel_id = ? AND command_id = ?", row.ChannelID, row.ID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil
		}
		return ErrUnavailable
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	stop := PTZInput{Action: "stop", Duration: 1}
	if row.Input.Action == "cruise_start" {
		stop = PTZInput{Action: "cruise_stop", Duration: 1}
	}
	if row.Input.Action == "scan_start" {
		stop = PTZInput{Action: "scan_stop", Duration: 1}
	}
	if strings.HasPrefix(row.Input.Action, "focus_") || strings.HasPrefix(row.Input.Action, "iris_") {
		stop.Action = "fi_stop"
	}
	e := s.Host.PTZ(stopCtx, row.ChannelID, stop)
	state := "stopped"
	message := ""
	if e != nil {
		state = "stop_failed"
		message = "设备停止失败，禁止后续动作"
	}
	attempts := row.StopAttempts + 1
	next := time.Now().Add(time.Second * time.Duration(1<<min(attempts, 5)))
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Command{}).Where("id = ?", row.ID).Updates(map[string]any{"state": state, "error": message, "stop_attempts": attempts, "next_stop_at": next}).Error; err != nil {
			return err
		}
		if e == nil {
			if err := tx.Where("channel_id = ? AND command_id = ?", row.ChannelID, row.ID).Delete(&Lease{}).Error; err != nil {
				return err
			}
		}
		return s.audit(tx, Audit{AppID: row.AppID, CredentialID: row.CredentialID, Action: "ptz.stop", ResourceID: row.ID, Status: 200, Result: state})
	}); err != nil {
		return err
	}
	if e != nil {
		return Fail(503, "STOP_FAILED", message)
	}
	return nil
}
func (s *Service) StopCommand(ctx context.Context, app, id string) error {
	var row Command
	if e := s.DB.First(&row, "id = ? AND app_id = ?", id, app).Error; e != nil {
		return ErrNotFound
	}
	lock := s.channelLock(row.ChannelID)
	lock.Lock()
	defer lock.Unlock()
	return s.stopLocked(ctx, row)
}

// GuardPTZ 由宿主接到所有既有控制入口；管理员/级联接管先停止应用动作。
func (s *Service) GuardPTZ(ctx context.Context, ch string, fn func(context.Context) error) error {
	lock := s.channelLock(ch)
	lock.Lock()
	defer lock.Unlock()
	var lease Lease
	e := s.DB.First(&lease, "channel_id = ?", ch).Error
	if e == nil {
		var row Command
		if err := s.DB.First(&row, "id = ?", lease.CommandID).Error; err != nil {
			return ErrUnavailable
		}
		if e = s.stopLocked(ctx, row); e != nil {
			return e
		}
	} else if e != gorm.ErrRecordNotFound {
		return ErrUnavailable
	}
	return fn(ctx)
}
func (s *Service) File(t Ticket) (string, error) {
	id, e := strconv.ParseInt(t.ResourceID, 10, 64)
	switch t.Kind {
	case "recording", "download":
		if e != nil {
			return "", ErrNotFound
		}
		return s.Host.RecordingPath(context.Background(), id)
	case "event":
		if e != nil {
			return "", ErrNotFound
		}
		return s.Host.EventImagePath(context.Background(), id)
	case "snapshot":
		var row Snapshot
		if err := s.DB.First(&row, "id = ? AND app_id = ? AND expires_at > ?", t.ResourceID, t.AppID, time.Now()).Error; err != nil {
			return "", ErrNotFound
		}
		if row.State == "queued" || row.State == "running" {
			return "", Fail(409, "SNAPSHOT_PENDING", "抓拍处理中，请稍后重试图片地址或查询任务")
		}
		if row.State != "succeeded" {
			return "", ErrNotFound
		}
		return row.Path, nil
	case "capture":
		var row capture.Record
		if s.DB.First(&row, "id = ? AND channel_id = ? AND state = ?", t.ResourceID, t.ChannelID, "succeeded").Error != nil {
			return "", ErrNotFound
		}
		host, ok := s.Host.(interface {
			CapturePath(context.Context, string) (string, error)
		})
		if !ok {
			return "", ErrUnavailable
		}
		return host.CapturePath(context.Background(), row.ID)
	}
	return "", fmt.Errorf("unsupported media")
}
