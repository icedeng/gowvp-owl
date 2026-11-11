// Package capture 管理永久抓拍文件、抓拍任务与持久化定时计划。
package capture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/gowvp/owl/pkg/thumbnail"
	"gorm.io/gorm"
)

var ErrBusy = errors.New("抓拍尚未完成，请完成后再操作")

type Record struct {
	RequestURL string     `json:"-"`
	SessionID  string     `gorm:"size:64" json:"session_id,omitempty"`
	ID         string     `gorm:"primaryKey;size:36" json:"id"`
	ChannelID  string     `gorm:"size:128;not null;index:idx_captures_channel_created,priority:1" json:"channel_id"`
	Name       string     `gorm:"size:180" json:"name"`
	State      string     `gorm:"size:16;not null;index:idx_captures_state_created,priority:1" json:"state"`
	Source     string     `gorm:"size:16" json:"source"`
	Method     string     `gorm:"size:40" json:"method"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `gorm:"index:idx_captures_channel_created,priority:2,sort:desc;index:idx_captures_state_created,priority:2" json:"created_at"`
	CapturedAt *time.Time `json:"captured_at,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

func (Record) TableName() string { return "channel_captures" }

type Schedule struct {
	ChannelID       string     `gorm:"primaryKey;size:128" json:"channel_id"`
	Enabled         bool       `gorm:"not null;index:idx_capture_schedules_due,priority:1" json:"enabled"`
	IntervalSeconds int        `json:"interval_seconds"`
	NextRunAt       time.Time  `gorm:"index:idx_capture_schedules_due,priority:2" json:"next_run_at"`
	LastRunAt       *time.Time `json:"last_run_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func (Schedule) TableName() string { return "channel_capture_schedules" }

type Service struct {
	db                *gorm.DB
	root              string
	mu                sync.Mutex // 序列化文件重命名、删除与任务去重，避免元数据和磁盘相互覆盖。
	wake              chan struct{}
	schedulingMu      sync.RWMutex
	scanInterval      time.Duration
	immediateDispatch bool
}

func New(db *gorm.DB, root string) *Service {
	return &Service{db: db, root: root, wake: make(chan struct{}, 1), scanInterval: time.Second}
}

// SetScheduling 热更新队列扫描周期；默认每秒扫描，不主动唤醒入队任务。
func (s *Service) SetScheduling(interval time.Duration, immediate bool) {
	if interval <= 0 {
		interval = time.Second
	}
	s.schedulingMu.Lock()
	s.scanInterval, s.immediateDispatch = interval, immediate
	s.schedulingMu.Unlock()
	// 使新的扫描周期立即生效，不等待旧周期结束。
	s.wakeWorker()
}

func (s *Service) scheduling() (time.Duration, bool) {
	s.schedulingMu.RLock()
	defer s.schedulingMu.RUnlock()
	return s.scanInterval, s.immediateDispatch
}

// notify 合并突发入队通知；任务仍以数据库为准，定时扫描负责重启恢复和漏通知兜底。
func (s *Service) notify() {
	if _, immediate := s.scheduling(); !immediate {
		return
	}
	s.wakeWorker()
}

func (s *Service) wakeWorker() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) Enqueue(ctx context.Context, channelID, source string) (*Record, error) {
	return s.EnqueueURL(ctx, channelID, source, "")
}

func (s *Service) EnqueueURL(ctx context.Context, channelID, source, requestURL string) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var existing Record
	err := s.db.WithContext(ctx).Where("channel_id = ? AND state IN ?", channelID, []string{"queued", "running"}).Order("created_at").First(&existing).Error
	if err == nil {
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	var pending int64
	if err := s.db.WithContext(ctx).Model(&Record{}).Where("state IN ?", []string{"queued", "running"}).Count(&pending).Error; err != nil {
		return nil, err
	}
	if pending >= 512 {
		return nil, fmt.Errorf("抓拍队列已满，请稍后重试")
	}
	row := Record{ID: uuid.NewString(), ChannelID: channelID, State: "queued", Source: source, RequestURL: requestURL}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	s.notify()
	return &row, nil
}

func (s *Service) Get(ctx context.Context, id string) (*Record, error) {
	var row Record
	err := s.db.WithContext(ctx).First(&row, "id = ?", id).Error
	return &row, err
}

func (s *Service) List(ctx context.Context, channelID string, page, size int) ([]Record, int64, error) {
	q := s.db.WithContext(ctx).Model(&Record{})
	if channelID != "" {
		q = q.Where("channel_id = ?", channelID)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	rows := make([]Record, 0)
	err := q.Order("created_at DESC, id DESC").Offset((page - 1) * size).Limit(size).Find(&rows).Error
	return rows, total, err
}

func (s *Service) Path(row *Record) (string, error) {
	if _, err := uuid.Parse(row.ID); err != nil {
		return "", fmt.Errorf("invalid capture id")
	}
	if err := ValidateName(row.Name); err != nil {
		return "", err
	}
	return filepath.Join(s.root, row.ID, row.Name), nil
}

// ThumbnailPath 与重命名、删除互斥，历史图片在首次预览时生成缩略图。
func (s *Service) ThumbnailPath(ctx context.Context, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if row.State != "succeeded" {
		return "", ErrBusy
	}
	path, err := s.Path(row)
	if err != nil {
		return "", err
	}
	return thumbnail.Ensure(ctx, path)
}

func ValidateName(name string) error {
	if !utf8.ValidString(name) || len(name) > 180 || strings.TrimSpace(name) != name || strings.TrimSuffix(strings.ToLower(name), ".jpg") == "" || !strings.HasSuffix(strings.ToLower(name), ".jpg") {
		return fmt.Errorf("图片名称须为不超过 180 字节的 JPG 文件名")
	}
	if strings.ContainsAny(name, "/\\:") || strings.HasPrefix(name, ".") || strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("图片名称不能包含路径、冒号或控制字符")
	}
	return nil
}

// Complete 将各来源图片统一保存为 JPEG，已有 JPEG 不重复编码。每次抓拍使用独立目录，同秒抓拍也不会覆盖。
func (s *Service) Complete(ctx context.Context, id string, body []byte, method string) (*Record, error) {
	if len(body) == 0 || len(body) > 20<<20 {
		return nil, fmt.Errorf("图片内容为空或超过 20MB")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > 32000000 {
		return nil, fmt.Errorf("无效图片或分辨率过大")
	}
	img, format, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.State == "succeeded" {
		return row, nil
	} // 设备重复上传只确认已保存结果。
	if row.State != "running" {
		return nil, fmt.Errorf("抓拍任务已结束，不能再接收图片")
	}
	now := time.Now()
	row.CapturedAt = &now
	row.Name = now.In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("20060102150405") + ".jpg"
	path, err := s.Path(row)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".capture-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	var encodeErr error
	if format == "jpeg" {
		_, encodeErr = file.Write(body)
	} else {
		encodeErr = jpeg.Encode(file, img, &jpeg.Options{Quality: 90})
	}
	closeErr := file.Close()
	if err := errors.Join(encodeErr, closeErr); err != nil {
		return nil, err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return nil, err
	}
	if _, err := thumbnail.Ensure(ctx, path); err != nil {
		// 缩略图失败不丢弃已取得的现场原图，预览请求可重新生成。
		slog.WarnContext(ctx, "generate capture thumbnail", "capture_id", id, "err", err)
	}
	result := s.db.WithContext(ctx).Model(&Record{}).Where("id = ? AND state = ?", id, "running").Updates(map[string]any{"name": row.Name, "captured_at": now, "state": "succeeded", "method": method, "error": "", "request_url": ""})
	if result.Error != nil || result.RowsAffected != 1 {
		_ = thumbnail.Remove(path)
		return nil, errors.Join(result.Error, fmt.Errorf("保存抓拍记录失败"))
	}
	return s.Get(ctx, id)
}

func (s *Service) Rename(ctx context.Context, id, name string) (*Record, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.State != "succeeded" {
		return nil, ErrBusy
	}
	oldPath, err := s.Path(row)
	if err != nil {
		return nil, err
	}
	row.Name = name
	newPath, err := s.Path(row)
	if err != nil {
		return nil, err
	}
	if oldPath == newPath {
		return row, nil
	}
	if err := thumbnail.Rename(oldPath, newPath); err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Model(&Record{}).Where("id = ?", id).Update("name", name).Error; err != nil {
		return nil, errors.Join(err, thumbnail.Rename(newPath, oldPath))
	}
	return s.Get(ctx, id)
}

// Delete 先持久化删除状态，再删文件、最后删记录；中断后可以安全重试，不会丢失待清理路径。
func (s *Service) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if row.State == "queued" || row.State == "running" {
		return ErrBusy
	}
	if _, err := uuid.Parse(row.ID); err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Model(row).Update("state", "deleting").Error; err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.root, row.ID)); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Delete(&Record{}, "id = ?", id).Error
}

func (s *Service) GetSchedule(ctx context.Context, channelID string) (*Schedule, error) {
	var row Schedule
	err := s.db.WithContext(ctx).First(&row, "channel_id = ?", channelID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &Schedule{ChannelID: channelID, IntervalSeconds: 300}, nil
	}
	return &row, err
}

func (s *Service) SaveSchedule(ctx context.Context, row *Schedule) error {
	if row.IntervalSeconds < 10 || row.IntervalSeconds > 31536000 {
		return fmt.Errorf("抓拍间隔须为 10–31536000 秒")
	}
	if row.NextRunAt.IsZero() {
		row.NextRunAt = time.Now().Add(time.Duration(row.IntervalSeconds) * time.Second)
	}
	// 保留最近执行结果，编辑计划不会伪造一次新执行。
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Schedule
		if err := tx.FirstOrCreate(&current, Schedule{ChannelID: row.ChannelID}).Error; err != nil {
			return err
		}
		return tx.Model(&current).Updates(map[string]any{"enabled": row.Enabled, "interval_seconds": row.IntervalSeconds, "next_run_at": row.NextRunAt}).Error
	})
}

func (s *Service) Latest(ctx context.Context, channelID string) (*Record, error) {
	var row Record
	err := s.db.WithContext(ctx).Where("channel_id = ? AND state = ?", channelID, "succeeded").Order("captured_at DESC, created_at DESC").First(&row).Error
	return &row, err
}

func (s *Service) DisableSchedules(ctx context.Context, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Schedule{}).Where("channel_id IN ?", ids).Update("enabled", false).Error; err != nil {
			return err
		}
		return tx.Model(&Record{}).Where("channel_id IN ? AND state IN ?", ids, []string{"queued", "running"}).Updates(map[string]any{"state": "failed", "error": "通道已删除"}).Error
	})
}

func (s *Service) finish(ctx context.Context, id string, runErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if row.State == "succeeded" {
		return nil
	}
	if runErr == nil {
		runErr = fmt.Errorf("抓拍未生成图片")
	}
	if row.State != "running" {
		return runErr
	}
	if err := s.db.WithContext(ctx).Model(&Record{}).Where("id = ? AND state = ?", id, "running").Updates(map[string]any{"state": "failed", "error": runErr.Error(), "request_url": ""}).Error; err != nil {
		return errors.Join(runErr, err)
	}
	return runErr
}

// SetSession 保留旧版同步抓拍返回的设备会话 ID，状态查询接口仍可使用。
func (s *Service) SetSession(ctx context.Context, id, sessionID string) error {
	return s.db.WithContext(ctx).Model(&Record{}).Where("id = ?", id).Update("session_id", sessionID).Error
}
