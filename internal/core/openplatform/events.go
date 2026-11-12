package openplatform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/gowvp/owl/internal/core/event"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/reason"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// EventResult 不暴露文件路径、协议原文和取证内部错误。
type EventResult struct {
	Historical           bool           `json:"historical"`
	ReceivedAt           *time.Time     `json:"received_at,omitempty"`
	EvidenceAvailability string         `json:"evidence_availability"`
	ID                   int64          `json:"id"`
	DID                  string         `json:"did"`
	CID                  string         `json:"cid"`
	DeviceName           string         `json:"device_name"`
	DeviceCode           string         `json:"device_code"`
	ChannelName          string         `json:"channel_name"`
	ChannelCode          string         `json:"channel_code"`
	StartedAt            orm.Time       `json:"started_at"`
	EndedAt              orm.Time       `json:"ended_at"`
	UpdatedAt            orm.Time       `json:"updated_at"`
	Label                string         `json:"label"`
	Score                float32        `json:"score"`
	Model                string         `json:"model"`
	Status               string         `json:"status"`
	Revision             int            `json:"revision"`
	OccurrenceCount      int            `json:"occurrence_count"`
	HasImage             bool           `json:"has_image"`
	HasVideo             bool           `json:"has_video"`
	Evidence             *EventEvidence `json:"evidence,omitempty"`
}
type EventEvidence struct {
	State         string     `json:"state"`
	SnapshotState string     `json:"snapshot_state"`
	VideoState    string     `json:"video_state"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

func eventResult(e *event.Event) EventResult {
	out := EventResult{Historical: e.Historical, ReceivedAt: e.ReceivedAt, EvidenceAvailability: "live_policy", ID: e.ID, DID: e.DID, CID: e.CID, DeviceName: e.DeviceName, DeviceCode: e.DeviceCode, ChannelName: e.ChannelName, ChannelCode: e.ChannelCode,
		StartedAt: e.StartedAt, EndedAt: e.EndedAt, UpdatedAt: e.UpdatedAt, Label: e.Label, Score: e.Score, Model: e.Model, Status: e.Status, Revision: e.Revision,
		OccurrenceCount: e.OccurrenceCount, HasImage: e.ImagePath != "", HasVideo: e.VideoPath != ""}
	if e.Historical {
		out.EvidenceAvailability = "historical_unavailable"
	}
	if t := e.Evidence; t != nil {
		out.Evidence = &EventEvidence{State: t.State, SnapshotState: t.SnapshotState, VideoState: t.VideoState, StartedAt: t.StartedAt, FinishedAt: t.FinishedAt}
	}
	return out
}
func (s *Service) EventQuery(app App) *gorm.DB {
	channels := s.DeviceQuery(s.DB.Table("channels").Select("id"), app, "did")
	devices := s.DeviceQuery(s.DB.Table("devices").Select("id"), app, "id")
	return s.DB.Model(&event.Event{}).Where("(cid IN (?) AND (did = '' OR EXISTS (SELECT 1 FROM channels owner_channel WHERE owner_channel.id = events.cid AND owner_channel.did = events.did))) OR (cid = '' AND did IN (?))", channels, devices)
}
func (s *Service) Event(ctx context.Context, app App, id int64) (EventResult, error) {
	var e event.Event
	if err := s.EventQuery(app).WithContext(ctx).First(&e, "events.id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			err = ErrNotFound
		}
		return EventResult{}, err
	}
	if err := event.NewWorkflow(s.DB).FillNames(ctx, []*event.Event{&e}); err != nil {
		return EventResult{}, err
	}
	return eventResult(&e), nil
}

// FillEventMaps 批量补齐开放DTO，沿用现有列表游标与编码筛选。
func (s *Service) FillEventMaps(ctx context.Context, app App, rows []map[string]any) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		id, err := strconv.ParseInt(fmt.Sprint(r["id"]), 10, 64)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	var events []*event.Event
	if err := s.EventQuery(app).WithContext(ctx).Where("events.id IN ?", ids).Find(&events).Error; err != nil {
		return err
	}
	if err := event.NewWorkflow(s.DB).FillNames(ctx, events); err != nil {
		return err
	}
	byID := map[int64]*event.Event{}
	for _, e := range events {
		byID[e.ID] = e
	}
	for i, id := range ids {
		e := byID[id]
		if e == nil {
			return ErrNotFound
		}
		b, _ := json.Marshal(eventResult(e))
		var row map[string]any
		decoder := json.NewDecoder(bytes.NewReader(b))
		decoder.UseNumber()
		if err := decoder.Decode(&row); err != nil {
			return err
		}
		rows[i] = row
	}
	return nil
}
func (s *Service) BindEventWorkflow(w *event.Workflow) {
	if w != nil {
		w.SetChangeHandler(s.EnqueueEventChange)
	}
}

// EnqueueEventChange 与取证/处理状态在同一事务持久化；相同状态不重复生成通知。
func (s *Service) EnqueueEventChange(tx *gorm.DB, id int64, kind string) error {
	// GORM回调携带原写入Statement；保留事务连接，但清除其表/选择列/返回列。
	tx = tx.Session(&gorm.Session{NewDB: true, Initialized: true}).Session(&gorm.Session{})
	var e event.Event
	if err := tx.First(&e, id).Error; err != nil {
		return err
	}
	if err := event.NewWorkflow(tx).FillNames(tx.Statement.Context, []*event.Event{&e}); err != nil {
		return err
	}
	snapshot := eventResult(&e)
	key := fmt.Sprintf("%s:%d", kind, id)
	switch kind {
	case "event.evidence.updated":
		if snapshot.Evidence == nil {
			return nil
		}
		key += ":" + Hash([]byte(marshal(snapshot.Evidence)))
	case "event.status.changed":
		key += ":" + strconv.Itoa(e.Revision)
	}
	payload := map[string]any{}
	b, _ := json.Marshal(snapshot)
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return err
	}
	payload["event_type"] = kind
	box := Outbox{EventID: id, NotificationKey: &key, EventType: kind, DeviceID: e.DID, ChannelID: e.CID, Label: e.Label, Payload: marshal(payload)}
	return tx.Session(&gorm.Session{NewDB: true}).Clauses(clause.OnConflict{DoNothing: true}).Create(&box).Error
}

// ProcessEvent 将幂等结果、工作流、处理记录、通知及应用审计原子提交。
func (s *Service) ProcessEvent(ctx context.Context, p Principal, id int64, revision int, status, note, key, fingerprint string) (int64, error) {
	s.execution.Lock()
	defer s.execution.Unlock()
	p, err := s.Validate(p, "event:process", "")
	if err != nil {
		return 0, err
	}
	if key == "" || len(key) > 128 {
		return 0, Fail(400, "IDEMPOTENCY_REQUIRED", "请提供不超过128字符的Idempotency-Key")
	}
	var actionID int64
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		read := &Service{DB: tx}
		var e event.Event
		if err := read.EventQuery(p.App).Clauses(clause.Locking{Strength: "UPDATE"}).First(&e, "events.id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		record := Idempotency{AppID: p.App.ID, Operation: "event.process", Key: key, Fingerprint: fingerprint, Status: "done", ExpiresAt: time.Now().Add(24 * time.Hour)}
		var existing Idempotency
		err := tx.First(&existing, "app_id = ? AND operation = ? AND key = ?", record.AppID, record.Operation, key).Error
		if err == nil {
			if existing.Fingerprint != fingerprint {
				return Fail(409, "IDEMPOTENCY_MISMATCH", "幂等键已用于其他请求")
			}
			if existing.Status != "done" {
				return ErrConflict
			}
			actionID, err = strconv.ParseInt(existing.ResourceID, 10, 64)
			return err
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if e.Revision != revision {
			return Fail(409, "EVENT_REVISION_CONFLICT", "事件已被修改，请刷新后重试")
		}
		if e.Status == status {
			return Fail(400, "INVALID_TRANSITION", "事件已处于目标状态")
		}
		w := event.NewWorkflow(tx)
		s.BindEventWorkflow(w)
		if err = w.Process(ctx, event.ProcessInput{Items: []event.ProcessTarget{{ID: id, Revision: revision}}, Status: status, Note: note}, "open-app:"+p.App.ID); err != nil {
			if errors.Is(err, reason.ErrBadRequest) {
				return Fail(400, "INVALID_EVENT_PROCESS", err.Error())
			}
			return err
		}
		var action event.Action
		if err = tx.Where("event_id = ?", id).Order("id DESC").First(&action).Error; err != nil {
			return err
		}
		actionID = action.ID
		record.ResourceID = strconv.FormatInt(actionID, 10)
		if err = tx.Create(&record).Error; err != nil {
			return err
		}
		return s.audit(tx, Audit{AppID: p.App.ID, CredentialID: p.Credential.ID, Actor: "open-app:" + p.App.ID, Action: "event.process", ResourceID: strconv.FormatInt(id, 10), Result: status, Status: 200})
	})
	return actionID, err
}

func isEventMedia(kind string) bool {
	return Has([]string{"event", "event-thumbnail", "event-video", "event-download"}, kind)
}
func (s *Service) checkEventTicket(t Ticket) error {
	var e event.Event
	if err := s.DB.First(&e, "id = ?", t.ResourceID).Error; err != nil {
		return ErrNotFound
	}
	if e.CID != t.ChannelID || e.DID != t.DeviceID {
		return ErrAuth
	}
	if (t.Kind == "event" || t.Kind == "event-thumbnail") && e.ImagePath == "" || (t.Kind == "event-video" || t.Kind == "event-download") && e.VideoPath == "" {
		return ErrNotFound
	}
	return nil
}

// AddEventMedia 只在发送时按当前权限签发短期链接，不把票据写入通知快照。
func (s *Service) AddEventMedia(p Principal, id int64, payload map[string]any) error {
	for _, item := range []struct{ kind, url, expiry string }{
		{"event", "image_url", "image_expires_at"}, {"event-thumbnail", "thumbnail_url", "thumbnail_expires_at"},
		{"event-video", "video_url", "video_expires_at"}, {"event-download", "video_download_url", "video_download_expires_at"},
	} {
		if !Has(p.App.Scopes, mediaScope(item.kind)) {
			continue
		}
		raw, t, err := s.IssueTicket(p, item.kind, strconv.FormatInt(id, 10))
		if err != nil {
			if AsError(err).Status == 404 {
				continue
			}
			return err
		}
		payload[item.url] = fmt.Sprintf("%s/openmedia/v1/%s/%d/content?ticket=%s", s.Config.PublicBaseURL, item.kind, id, raw)
		payload[item.expiry] = t.ExpiresAt
	}
	return nil
}
