package event

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AlarmQuietWindow 内同类报警归入同一事件；新的有效上报才延长末次时间。
const AlarmQuietWindow = 10 * time.Second

// AlarmIdentity 不包含传输方式、SN、XML 格式和协议版本，兼容同一报警的双路上报。
// 类型、进入/离开阶段、等级、描述及位置仍区分，避免合并不同规则或报警。
type AlarmIdentity struct {
	DeviceCode, ChannelCode          string
	Method, Type, Priority           string
	Description, Longitude, Latitude string
	EventType                        *int
}

// AlarmReport 保留每次有效上报的原始字段，重复上报不增加次数。
type AlarmReport struct {
	Key        string    `gorm:"column:report_key;size:64;primaryKey" json:"-"`
	EventID    int64     `gorm:"not null;index:idx_alarm_reports_event_time,priority:1" json:"event_id"`
	ReportedAt time.Time `gorm:"not null;index:idx_alarm_reports_event_time,priority:2" json:"reported_at"`
	Payload    string    `gorm:"not null" json:"payload"`
	CreatedAt  time.Time `json:"received_at"`
	Event      Event     `gorm:"constraint:OnDelete:CASCADE" json:"-"`
}

func (AlarmReport) TableName() string { return "event_alarm_reports" }

type alarmEventStorer interface {
	CreateAlarm(context.Context, *Event, string) (bool, error)
}

func (a AlarmIdentity) keys(in *AddEventInput) (string, string) {
	values := []string{in.DID, in.CID, a.DeviceCode, a.ChannelCode, a.Method, a.Type, a.Priority, a.Description, a.Longitude, a.Latitude}
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	phase := ""
	if a.EventType != nil {
		phase = fmt.Sprint(*a.EventType)
	}
	values = append(values, phase)
	hash := func(values []string) string {
		body, _ := json.Marshal(values)
		digest := sha256.Sum256(body)
		return hex.EncodeToString(digest[:])
	}
	occurrence := hash(append(append([]string{}, values...), in.StartedAt.UTC().Format(time.RFC3339Nano)))
	// 聚合分组隔离历史补报；同一次报警跨重启仍共用 occurrence 幂等键。
	if in.Historical {
		values = append(values, "historical")
	}
	return hash(values), occurrence
}

// CreateAlarmAndNotify 对国标报警做语义去重及连续聚合，仅新建事件时通知和取证。
func (c Core) CreateAlarmAndNotify(ctx context.Context, in *AddEventInput, identity AlarmIdentity) (*Event, error) {
	if in == nil || in.DID == "" || in.StartedAt.IsZero() || in.Model != "GB28181" {
		return nil, fmt.Errorf("无效国标报警")
	}
	store, ok := c.store.Event().(alarmEventStorer)
	if !ok {
		return nil, fmt.Errorf("报警存储不支持原子聚合")
	}
	group, occurrence := identity.keys(in)
	out := Event{DID: in.DID, CID: in.CID, StartedAt: in.StartedAt, EndedAt: in.StartedAt,
		Label: in.Label, Score: in.Score, Zones: in.Zones, ImagePath: in.ImagePath, Model: in.Model,
		SourceKey: in.SourceKey, Historical: in.Historical, ReceivedAt: in.ReceivedAt, AggregationKey: &group, OccurrenceCount: 1}
	created, err := store.CreateAlarm(ctx, &out, occurrence)
	if err != nil {
		return nil, err
	}
	if created && c.notifier != nil {
		c.notifier.Dispatch(ctx, &out)
	}
	return &out, nil
}
