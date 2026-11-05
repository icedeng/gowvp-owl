package openplatform

import (
	"context"
	"errors"
	"hash/fnv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/internal/core/ipc"
	"gorm.io/gorm"
)

type Playback struct {
	ID           string    `gorm:"primaryKey;size:64" json:"id"`
	AppID        string    `gorm:"index;size:64" json:"app_id"`
	CredentialID string    `json:"-"`
	ChannelID    string    `gorm:"index;size:128" json:"channel_id"`
	DeviceCode   string    `json:"device_code"`
	ChannelCode  string    `json:"channel_code"`
	Protocol     string    `json:"protocol"`
	StartAt      int64     `json:"start_at"`
	EndAt        int64     `json:"end_at"`
	RecordType   int       `json:"record_type"`
	State        string    `gorm:"index;size:24" json:"state"`
	Error        string    `json:"error,omitempty"`
	Revision     int64     `json:"-"`
	Source       Source    `gorm:"serializer:json;type:text" json:"-"`
	StopPending  bool      `gorm:"index" json:"stop_pending"`
	StopAttempts int       `json:"-"`
	NextStopAt   time.Time `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `gorm:"index" json:"expires_at"`
}

func (Playback) TableName() string { return "open_playback_sessions" }

type PlaybackInput struct {
	StartAt    int64  `json:"start_at"`
	EndAt      int64  `json:"end_at"`
	Protocol   string `json:"protocol"`
	RecordType *int   `json:"record_type"`
}

type PlaybackControl struct {
	Action string  `json:"action"`
	Scale  float64 `json:"scale"`
	SeekAt int64   `json:"seek_at"`
}

// PlaybackHost 不依赖管理员凭据，停止/控制始终使用回放创建时绑定的设备编码和节点。
type PlaybackHost interface {
	PreparePlayback(context.Context, Playback) (Source, error)
	StartPlayback(context.Context, Playback) error
	StopPlayback(context.Context, Playback) error
	ControlPlayback(context.Context, Playback, PlaybackControl) error
	PlaybackActive(Playback) bool
}

// 使用有界锁组，避免每次创建UUID会话都永久保留一把内存锁。
func (s *Service) playbackLock(id string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return &s.playbackLocks[h.Sum32()%uint32(len(s.playbackLocks))]
}

func ValidatePlaybackInput(in *PlaybackInput) error {
	if in.StartAt <= 0 || in.EndAt <= in.StartAt || in.EndAt-in.StartAt > 7*24*60*60 {
		return Fail(400, "INVALID_TIME_RANGE", "录像回放须使用Unix秒，结束大于开始，跨度不超过7天")
	}
	if in.Protocol == "" {
		in.Protocol = "hls"
	}
	if in.Protocol != "hls" && in.Protocol != "flv" {
		return Fail(400, "UNSUPPORTED_PROTOCOL", "回放支持hls或flv")
	}
	if in.RecordType != nil && (*in.RecordType < 0 || *in.RecordType > 3) {
		return Fail(400, "INVALID_RECORD_TYPE", "record_type须为0至3")
	}
	return nil
}

func (s *Service) CreatePlayback(ctx context.Context, p Principal, channel string, in PlaybackInput) (Playback, error) {
	var row Playback
	if err := ValidatePlaybackInput(&in); err != nil {
		return row, err
	}
	p, err := s.Validate(p, "recording:play", channel)
	if err != nil {
		return row, err
	}
	ch, err := s.Channel(p.App, channel)
	if err != nil {
		return row, err
	}
	var dev ipc.Device
	if err = s.DB.First(&dev, "id = ?", ch.DID).Error; err != nil {
		return row, ErrNotFound
	}
	if dev.GetType() != ipc.TypeGB28181 {
		return row, Fail(400, "UNSUPPORTED_PLAYBACK", "设备录像回放当前仅支持GB28181")
	}
	if !dev.IsOnline {
		return row, Fail(409, "DEVICE_OFFLINE", "设备离线，无法回放录像")
	}
	host, ok := s.Host.(PlaybackHost)
	if !ok {
		return row, ErrUnavailable
	}
	row = Playback{ID: uuid.NewString(), AppID: p.App.ID, CredentialID: p.Credential.ID, ChannelID: channel, DeviceCode: dev.DeviceID, ChannelCode: ch.ChannelID, Protocol: in.Protocol, StartAt: in.StartAt, EndAt: in.EndAt, State: "preparing", Revision: p.App.Revision, StopPending: true, ExpiresAt: time.Now().Add(30 * time.Minute)}
	if in.RecordType != nil {
		row.RecordType = *in.RecordType
	}
	// 在发送INVITE前持久化绑定的媒体节点，异常重启后也能精确回收接收端。
	prepareCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	row.Source, err = host.PreparePlayback(prepareCtx, row)
	if err != nil {
		return Playback{}, err
	}
	s.execution.Lock()
	defer s.execution.Unlock()
	latest, err := s.Validate(p, "recording:play", channel)
	if err != nil || latest.App.Revision != p.App.Revision {
		return Playback{}, ErrConflict
	}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		if err := checkSessionLimits(tx, latest, channel); err != nil {
			return err
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return s.audit(tx, Audit{AppID: row.AppID, CredentialID: row.CredentialID, Action: "playback.create", ResourceID: row.ID, Status: 200, Result: "accepted"})
	})
	if err != nil {
		return Playback{}, err
	}
	return row, nil
}

// 直播与设备录像回放共同遵守已有应用/通道/全局播放配额。
func checkSessionLimits(tx *gorm.DB, p Principal, channel string) error {
	var total, perChannel, preparing, active int64
	for _, model := range []any{&Session{}, &Playback{}} {
		var n int64
		base := func() *gorm.DB {
			q := tx.Model(model)
			if _, ok := model.(*Playback); ok {
				return q.Where("stop_pending = ?", true)
			}
			return q.Where("state IN ? AND expires_at > ?", []string{"preparing", "ready"}, time.Now())
		}
		for _, item := range []struct {
			q   *gorm.DB
			out *int64
		}{
			{base().Where("app_id = ?", p.App.ID), &total},
			{base().Where("app_id = ? AND channel_id = ?", p.App.ID, channel), &perChannel},
			{base().Where("state = ?", "preparing"), &preparing},
			{base(), &active},
		} {
			if err := item.q.Count(&n).Error; err != nil {
				return err
			}
			*item.out += n
		}
	}
	if total >= int64(p.App.LiveLimit) || perChannel >= int64(p.App.ChannelLimit) || active >= 500 {
		return Fail(429, "CONCURRENT_LIMIT", "播放会话超过配额")
	}
	if preparing >= 10 {
		return Fail(429, "SOURCE_LIMIT", "媒体准备任务已满")
	}
	return nil
}

func (s *Service) Playback(p Principal, id string) (Playback, error) {
	var row Playback
	if err := s.DB.First(&row, "id = ? AND app_id = ?", id, p.App.ID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return row, ErrNotFound
		}
		return row, err
	}
	if _, err := s.Validate(p, "recording:play", row.ChannelID); err != nil {
		return row, err
	}
	if original, err := s.Current(row.AppID, row.CredentialID); err != nil {
		return row, err
	} else if original.App.Revision != row.Revision && Has([]string{"preparing", "ready", "paused"}, row.State) {
		return row, ErrConflict
	}
	return row, nil
}

func (s *Service) StartPlayback(id string) {
	lock := s.playbackLock(id)
	lock.Lock()
	defer lock.Unlock()
	var row Playback
	if s.DB.First(&row, "id = ? AND state = ?", id, "preparing").Error != nil {
		return
	}
	host, ok := s.Host.(PlaybackHost)
	if !ok {
		return
	}
	p, err := s.Current(row.AppID, row.CredentialID)
	if err == nil {
		_, err = s.Validate(p, "recording:play", row.ChannelID)
	}
	if err == nil && (p.App.Revision != row.Revision || !row.ExpiresAt.After(time.Now())) {
		err = ErrConflict
	}
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		err = host.StartPlayback(ctx, row)
		cancel()
	}
	latest, valid := s.Current(row.AppID, row.CredentialID)
	if valid == nil {
		_, valid = s.Validate(latest, "recording:play", row.ChannelID)
	}
	if err == nil && valid == nil && latest.App.Revision == row.Revision && row.ExpiresAt.After(time.Now()) {
		r := s.DB.Model(&Playback{}).Where("id = ? AND state = ?", id, "preparing").Update("state", "ready")
		if r.Error == nil && r.RowsAffected == 1 {
			return
		}
	}
	s.DB.Model(&Playback{}).Where("id = ? AND state = ?", id, "preparing").Updates(map[string]any{"state": "failed", "error": "PLAYBACK_START_FAILED"})
	_ = s.stopPlaybackLocked(row)
}

func (s *Service) StopPlayback(p Principal, id string) error {
	lock := s.playbackLock(id)
	lock.Lock()
	defer lock.Unlock()
	row, err := s.Playback(p, id)
	if err != nil {
		return err
	}
	if !row.StopPending {
		return nil
	}
	if err = s.DB.Model(&Playback{}).Where("id = ?", id).Update("state", "stopped").Error; err != nil {
		return err
	}
	return s.stopPlaybackLocked(row)
}

func (s *Service) stopPlaybackLocked(row Playback) error {
	host, ok := s.Host.(PlaybackHost)
	if !ok {
		return ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := host.StopPlayback(ctx, row)
	updates := map[string]any{"stop_pending": false}
	if err != nil {
		delay := min(60, 1<<min(row.StopAttempts, 6))
		updates = map[string]any{"stop_pending": true, "stop_attempts": row.StopAttempts + 1, "next_stop_at": time.Now().Add(time.Duration(delay) * time.Second)}
	}
	q := s.DB.Model(&Playback{}).Where("id = ?", row.ID)
	if err == nil {
		q = q.Where("state NOT IN ?", []string{"preparing", "ready", "paused"})
	}
	r := q.Updates(updates)
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return ErrConflict
	}
	if err != nil {
		return Fail(503, "PLAYBACK_STOP_PENDING", "回放停止尚未确认，后台将继续重试")
	}
	return nil
}

func (s *Service) ControlPlayback(ctx context.Context, p Principal, id string, in PlaybackControl) (Playback, error) {
	lock := s.playbackLock(id)
	lock.Lock()
	defer lock.Unlock()
	row, err := s.Playback(p, id)
	if err != nil {
		return row, err
	}
	if !row.ExpiresAt.After(time.Now()) || !Has([]string{"ready", "paused"}, row.State) {
		return row, Fail(409, "PLAYBACK_NOT_READY", "回放会话未就绪或已结束")
	}
	if !Has([]string{"pause", "play", "seek", "speed"}, in.Action) || in.Action != "seek" && in.SeekAt != 0 || in.Action != "speed" && in.Scale != 0 {
		return row, Fail(400, "INVALID_PLAYBACK_CONTROL", "请使用pause、play、seek或speed及对应参数")
	}
	if in.Action == "seek" && (in.SeekAt < row.StartAt || in.SeekAt >= row.EndAt) {
		return row, Fail(400, "INVALID_PLAYBACK_CONTROL", "seek_at须位于本次回放时间范围内")
	}
	if in.Action == "speed" && !validPlaybackSpeed(in.Scale) {
		return row, Fail(400, "INVALID_PLAYBACK_CONTROL", "scale支持0.25、0.5、1、2、4")
	}
	host, ok := s.Host.(PlaybackHost)
	if !ok {
		return row, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err = host.ControlPlayback(ctx, row, in); err != nil {
		return row, Fail(503, "PLAYBACK_CONTROL_FAILED", "设备未确认回放控制，可能不支持该操作")
	}
	// 控制发出后重新读取状态；撤权或失效不能被成功的设备应答恢复。
	current, err := s.Playback(p, id)
	if err != nil {
		return row, err
	}
	if !Has([]string{"ready", "paused"}, current.State) || !current.ExpiresAt.After(time.Now()) {
		return row, ErrConflict
	}
	state := "ready" // seek/speed同样下发PLAY并恢复播放。
	if in.Action == "pause" {
		state = "paused"
	}
	r := s.DB.Model(&Playback{}).Where("id = ? AND state IN ?", id, []string{"ready", "paused"}).Update("state", state)
	if r.Error != nil {
		return row, r.Error
	}
	if r.RowsAffected != 1 {
		return row, ErrConflict
	}
	row.State = state
	return row, nil
}

func validPlaybackSpeed(v float64) bool { return v == 0.25 || v == 0.5 || v == 1 || v == 2 || v == 4 }

// checkPlaybackTicket 同时约束会话身份和创建凭据，票据刷新不能复活已结束的回放。
func (s *Service) checkPlaybackTicket(t Ticket) error {
	var row Playback
	if s.DB.First(&row, "id = ? AND app_id = ? AND channel_id = ? AND state IN ? AND expires_at > ?", t.SessionID, t.AppID, t.ChannelID, []string{"ready", "paused"}, time.Now()).Error != nil {
		return ErrAuth
	}
	p, err := s.Current(row.AppID, row.CredentialID)
	if err != nil {
		return err
	}
	if p.App.Revision != row.Revision {
		return ErrAuth
	}
	return nil
}

func (s *Service) maintainPlaybacks(ctx context.Context) {
	var rows []Playback
	// 活跃会话全局最多500；按主键扫描，不能让正常会话遮挡待停止记录。
	last := ""
	for {
		if s.DB.Where("stop_pending = ? AND id > ?", true, last).Order("id").Limit(100).Find(&rows).Error != nil {
			return
		}
		if len(rows) == 0 {
			return
		}
		for _, candidate := range rows {
			if ctx.Err() != nil {
				return
			}
			last = candidate.ID
			lock := s.playbackLock(candidate.ID)
			if !lock.TryLock() {
				continue
			}
			s.maintainPlaybackLocked(candidate.ID)
			lock.Unlock()
		}
	}
}

func (s *Service) maintainPlaybackLocked(id string) {
	var row Playback
	if s.DB.First(&row, "id = ? AND stop_pending = ?", id, true).Error != nil {
		return
	}
	if Has([]string{"preparing", "ready", "paused"}, row.State) {
		p, err := s.Current(row.AppID, row.CredentialID)
		if err == nil {
			_, err = s.Validate(p, "recording:play", row.ChannelID)
		}
		// 数据库暂不可用时保留现有状态，下轮重试。
		if err != nil && AsError(err).Status == 503 {
			return
		}
		valid := err == nil && row.ExpiresAt.After(time.Now()) && p.App.Revision == row.Revision
		if valid {
			if row.State == "preparing" && time.Since(row.CreatedAt) < time.Minute {
				return
			}
			if host, ok := s.Host.(PlaybackHost); ok && row.State != "preparing" && host.PlaybackActive(row) {
				return
			}
		}
		state := "expired"
		if err != nil || p.App.Revision != row.Revision {
			state = "revoked"
		}
		r := s.DB.Model(&Playback{}).Where("id = ? AND state = ?", row.ID, row.State).Update("state", state)
		if r.Error != nil || r.RowsAffected != 1 {
			return
		}
	}
	if row.NextStopAt.After(time.Now()) {
		return
	}
	_ = s.stopPlaybackLocked(row)
}

func (s *Service) shutdownPlaybacks() {
	if !s.Config.Enabled {
		return
	}
	// 所有异步启动退出后再失效并回收，避免停机时新流晚于清理建立。
	if s.DB.Model(&Playback{}).Where("state IN ?", []string{"preparing", "ready", "paused"}).Update("state", "expired").Error != nil {
		return
	}
	s.maintainPlaybacks(context.Background())
}
