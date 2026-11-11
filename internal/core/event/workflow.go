package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/reason"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Policy 只控制平台收到国标报警后的动作，不修改设备侦测、布防和录像计划。
type Policy struct {
	DID             string    `gorm:"column:did;primaryKey;size:128" json:"did"`
	Snapshot        bool      `gorm:"not null;default:false" json:"snapshot"`
	Record          bool      `gorm:"not null;default:false" json:"record"`
	ClipSeconds     int       `gorm:"not null;default:15" json:"clip_seconds"`
	CooldownSeconds int       `gorm:"not null;default:60" json:"cooldown_seconds"`
	Revision        int       `gorm:"not null;default:1" json:"revision"`
	UpdatedBy       string    `gorm:"size:128" json:"updated_by"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (Policy) TableName() string { return "event_policies" }

type Action struct {
	ID         int64     `gorm:"primaryKey" json:"id"`
	EventID    int64     `gorm:"not null;index" json:"event_id"`
	FromStatus string    `gorm:"size:16" json:"from_status"`
	ToStatus   string    `gorm:"size:16" json:"to_status"`
	Note       string    `gorm:"size:1000" json:"note"`
	Actor      string    `gorm:"size:128" json:"actor"`
	CreatedAt  time.Time `json:"created_at"`
	Event      Event     `gorm:"constraint:OnDelete:CASCADE" json:"-"`
}

func (Action) TableName() string { return "event_actions" }

type ProcessTarget struct {
	ID       int64 `json:"id" binding:"required,gt=0"`
	Revision int   `json:"revision" binding:"required,gt=0"`
}
type ProcessInput struct {
	Items  []ProcessTarget `json:"items" binding:"required,min=1,max=100,dive"`
	Status string          `json:"status" binding:"required,oneof=pending processing resolved ignored"`
	Note   string          `json:"note" binding:"max=1000"`
}

type Workflow struct {
	db      *gorm.DB
	mu      sync.RWMutex
	execute EvidenceExecutor
}

func NewWorkflow(db *gorm.DB) *Workflow      { return &Workflow{db: db} }
func (c Core) WithWorkflow(w *Workflow) Core { c.workflow = w; return c }
func (c Core) Workflow() *Workflow           { return c.workflow }

func (w *Workflow) Policy(ctx context.Context, did string) (*Policy, error) {
	p := Policy{DID: did, ClipSeconds: 15, CooldownSeconds: 60}
	err := w.db.WithContext(ctx).First(&p, "did = ?", did).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &p, nil
	}
	return &p, err
}
func (w *Workflow) SavePolicy(ctx context.Context, p Policy, actor string) (*Policy, error) {
	if p.ClipSeconds < 5 || p.ClipSeconds > 120 || p.CooldownSeconds < 10 || p.CooldownSeconds > 3600 {
		return nil, reason.ErrBadRequest.WithMsg("视频时长须为 5–120 秒，取证间隔须为 10–3600 秒")
	}
	var saved Policy
	err := w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 不存在时先插入默认策略，再在同一行上串行检查版本和取证冷却。
		seed := Policy{DID: p.DID, ClipSeconds: 15, CooldownSeconds: 60}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&saved, "did = ?", p.DID).Error; err != nil {
			return err
		}
		// revision=0 仅允许首次保存默认策略。
		if p.Revision != saved.Revision && !(p.Revision == 0 && saved.Revision == 1 && saved.UpdatedBy == "") {
			return reason.ErrBadRequest.WithMsg("策略已被修改，请刷新后重新保存")
		}
		saved.Snapshot, saved.Record = p.Snapshot, p.Record
		saved.ClipSeconds, saved.CooldownSeconds = p.ClipSeconds, p.CooldownSeconds
		saved.Revision++
		saved.UpdatedBy = actor
		saved.UpdatedAt = time.Now()
		return tx.Save(&saved).Error
	})
	return &saved, err
}

func (w *Workflow) Process(ctx context.Context, in ProcessInput, actor string) error {
	if len(in.Items) == 0 || len(in.Items) > 100 || !validStatus(in.Status) {
		return reason.ErrBadRequest.WithMsg("事件处理参数无效")
	}
	in.Note = strings.TrimSpace(in.Note)
	if utf8.RuneCountInString(in.Note) > 1000 || ((in.Status == "resolved" || in.Status == "ignored" || in.Status == "pending") && in.Note == "") {
		return reason.ErrBadRequest.WithMsg("完成、忽略或重新打开事件时，请填写处理说明（最多 1000 字）")
	}
	return w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		seen := map[int64]bool{}
		// 固定加锁顺序，避免两个批量操作以相反顺序锁定相同事件。
		items := append([]ProcessTarget(nil), in.Items...)
		sortProcessTargets(items)
		for _, target := range items {
			if target.ID <= 0 || target.Revision <= 0 || seen[target.ID] {
				return reason.ErrBadRequest.WithMsg("事件列表无效或重复")
			}
			seen[target.ID] = true
			var e Event
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&e, target.ID).Error; err != nil {
				return err
			}
			if e.Revision != target.Revision {
				return reason.ErrBadRequest.WithMsg("事件已被他人处理，请刷新后重试")
			}
			if e.Status == in.Status && in.Note == "" {
				continue
			}
			if !allowedTransition(e.Status, in.Status) {
				return reason.ErrBadRequest.WithMsg("已结束事件需先重新打开，才能继续处理")
			}
			previous := e.Status
			result := tx.Model(&Event{}).Where("id = ? AND revision = ?", e.ID, e.Revision).Updates(map[string]any{"status": in.Status, "revision": e.Revision + 1, "updated_at": orm.Now()})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("事件状态发生变化，请刷新后重试")
			}
			if err := tx.Omit("Event").Create(&Action{EventID: e.ID, FromStatus: previous, ToStatus: in.Status, Note: in.Note, Actor: actor}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
func validStatus(s string) bool {
	return s == "pending" || s == "processing" || s == "resolved" || s == "ignored"
}
func allowedTransition(from, to string) bool {
	if from == "resolved" || from == "ignored" {
		return to == "pending"
	}
	return (from == "pending" || from == "processing") && validStatus(to)
}
func sortProcessTargets(items []ProcessTarget) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].ID < items[j-1].ID; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
func (w *Workflow) Actions(ctx context.Context, id int64) ([]Action, error) {
	out := make([]Action, 0)
	err := w.db.WithContext(ctx).Omit("Event").Where("event_id = ?", id).Order("id DESC").Limit(100).Find(&out).Error
	return out, err
}

// FillNames 在一次批量查询中补齐设备与通道，不逐条发起资源接口请求。
func (w *Workflow) FillNames(ctx context.Context, items []*Event) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(items))
	for _, e := range items {
		ids = append(ids, e.ID)
	}
	var names []struct {
		ID                                               int64
		DeviceName, DeviceCode, ChannelName, ChannelCode string
		DeviceExt                                        string
	}
	err := w.db.WithContext(ctx).Table("events AS e").Joins("LEFT JOIN devices AS d ON d.id = e.did").Joins("LEFT JOIN channels AS ch ON ch.id = e.cid AND (e.did = '' OR ch.did = e.did)").Where("e.id IN ?", ids).
		Select("e.id, COALESCE(NULLIF(d.name, ''), '') AS device_name, COALESCE(d.device_id, '') AS device_code, COALESCE(CAST(d.ext AS TEXT), '{}') AS device_ext, COALESCE(ch.name, '') AS channel_name, COALESCE(ch.channel_id, '') AS channel_code").Scan(&names).Error
	if err != nil {
		return err
	}
	byID := map[int64]*Event{}
	for _, e := range items {
		byID[e.ID] = e
	}
	for _, n := range names {
		e := byID[n.ID]
		if n.DeviceName == "" {
			var ext struct {
				Name string `json:"name"`
			}
			if json.Unmarshal([]byte(n.DeviceExt), &ext) == nil {
				n.DeviceName = ext.Name
			}
		}
		e.DeviceName, e.DeviceCode, e.ChannelName, e.ChannelCode = n.DeviceName, n.DeviceCode, n.ChannelName, n.ChannelCode
	}
	var tasks []EvidenceTask
	if err := w.db.WithContext(ctx).Where("event_id IN ?", ids).Find(&tasks).Error; err != nil {
		return err
	}
	for i := range tasks {
		byID[tasks[i].EventID].Evidence = &tasks[i]
	}
	return nil
}

func (w *Workflow) CheckDevice(ctx context.Context, did string) error {
	var total int64
	if err := w.db.WithContext(ctx).Table("devices").Where("id = ? AND (type = ? OR type = '')", did, "GB28181").Count(&total).Error; err != nil {
		return err
	}
	if total != 1 {
		return reason.ErrNotFound.WithMsg("国标设备不存在")
	}
	return nil
}
