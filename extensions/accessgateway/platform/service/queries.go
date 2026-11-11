package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"gorm.io/gorm"
)

type SessionView struct {
	MediaServerID   *string              `json:"media_server_id"`
	StreamID        *string              `json:"stream_id"`
	Traffic         *core.SessionTraffic `json:"traffic"`
	ID              string               `json:"id"`
	ResourceType    string               `json:"resource_type"`
	RemoteID        string               `json:"remote_id"`
	Generation      string               `json:"generation"`
	ConnectionEpoch protocol.Decimal     `json:"connection_epoch"`
	SubjectType     string               `json:"subject_type"`
	SubjectID       string               `json:"subject_id"`
	ChannelID       *string              `json:"channel_id"`
	BindingRevision protocol.Decimal     `json:"binding_revision"`
	ConfigRevision  protocol.Decimal     `json:"config_revision"`
	Purpose         string               `json:"purpose"`
	State           string               `json:"state"`
	WireMode        string               `json:"wire_mode"`
	LocalMode       string               `json:"local_mode"`
	Direction       string               `json:"direction"`
	SSRC            *string              `json:"ssrc"`
	SendSSRC        *string              `json:"send_ssrc"`
	PublicEndpoint  store.Endpoints      `json:"public_endpoint"`
	LocalEndpoint   store.Endpoints      `json:"local_endpoint"`
	CreatedAt       time.Time            `json:"created_at"`
	ExpiresAt       *time.Time           `json:"expires_at"`
	ClosedAt        *time.Time           `json:"closed_at"`
	CleanupSteps    store.CleanupSteps   `json:"cleanup_steps"`
	Error           *protocol.Error      `json:"error"`
	RXBytes         *protocol.Decimal    `json:"rx_bytes"`
	TXBytes         *protocol.Decimal    `json:"tx_bytes"`
}

func (s *Service) Sessions(ctx context.Context, id string, q core.PageQuery) (core.Page[SessionView], error) {
	row, e := s.repo.Get(ctx, id)
	if e != nil {
		return core.Page[SessionView]{}, normalizeError(e)
	}
	rows, total, e := s.repo.Sessions(ctx, row.ID, q)
	if e != nil {
		return core.Page[SessionView]{}, e
	}
	items := make([]SessionView, 0, len(rows))
	for _, r := range rows {
		items = append(items, SessionView{MediaServerID: r.MediaServerID, StreamID: r.StreamID, Traffic: r.Traffic, ID: r.ID, RXBytes: trafficRX(r.Traffic), TXBytes: trafficTX(r.Traffic), ResourceType: r.ResourceType, RemoteID: r.RemoteID, Generation: r.Generation, ConnectionEpoch: protocol.Number(r.ConnectionEpoch), SubjectType: r.SubjectType, SubjectID: r.SubjectID, ChannelID: r.ChannelID, BindingRevision: protocol.Number(r.BindingRevision), ConfigRevision: protocol.Number(r.ConfigRevision), Purpose: r.Purpose, State: r.State, WireMode: r.WireMode, LocalMode: r.LocalMode, Direction: r.Direction, SSRC: r.SSRC, SendSSRC: r.SendSSRC, PublicEndpoint: r.PublicEndpoint, LocalEndpoint: r.LocalEndpoint, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, ClosedAt: r.ClosedAt, CleanupSteps: r.CleanupSteps, Error: r.Error})
	}
	return page(items, total, q), nil
}

type PlatformView struct {
	State           string           `json:"state"`
	Online          bool             `json:"online"`
	ID              string           `json:"id"`
	SubjectType     string           `json:"subject_type"`
	SubjectID       string           `json:"subject_id"`
	Role            string           `json:"role"`
	IngressType     string           `json:"ingress_type"`
	BindingRevision protocol.Decimal `json:"binding_revision"`
	AuthenticatedAt time.Time        `json:"authenticated_at"`
}

func (s *Service) Platforms(ctx context.Context, id string, q core.PageQuery) (core.Page[PlatformView], error) {
	row, e := s.repo.Get(ctx, id)
	if e != nil {
		return core.Page[PlatformView]{}, normalizeError(e)
	}
	if q.State != "" && q.State != "online" && q.State != "offline" {
		return core.Page[PlatformView]{}, core.Fail(400, "INVALID_ARGUMENT", "平台状态须为 online/offline")
	}
	snapshot := s.runtime.Snapshot(row.ID)
	reachable := snapshot.ConnectionState == "connected" && snapshot.HealthState == "ready"
	online := func(binding store.PlatformBinding) bool {
		return reachable && (binding.RegisteredUntil == nil || time.Now().Before(*binding.RegisteredUntil)) && binding.LastGeneration != nil && *binding.LastGeneration == snapshot.Generation && binding.LastConnectionEpoch != nil && protocol.Number(*binding.LastConnectionEpoch) == snapshot.Epoch && binding.LastFlowID != nil
	}
	items := []PlatformView{}
	var total int64
	e = s.repo.Read(ctx, func(db *gorm.DB) error {
		query := db.Model(&store.PlatformBinding{}).Where("gateway_id = ?", row.ID)
		if q.Q != "" {
			query = query.Where("subject_id LIKE ?", "%"+q.Q+"%")
		}
		if q.State != "" {
			if !reachable {
				if q.State == "online" {
					query = query.Where("1 = 0")
				}
			} else if q.State == "online" {
				query = query.Where("last_generation = ? AND last_connection_epoch = ? AND last_flow_id IS NOT NULL AND (registered_until IS NULL OR registered_until > ?)", snapshot.Generation, snapshot.Epoch, time.Now().UTC())
			} else {
				query = query.Where("(last_generation IS NULL OR last_generation <> ? OR last_connection_epoch IS NULL OR last_connection_epoch <> ? OR last_flow_id IS NULL OR registered_until <= ?)", snapshot.Generation, snapshot.Epoch, time.Now().UTC())
			}
		}
		if e := query.Count(&total).Error; e != nil {
			return e
		}
		rows := []store.PlatformBinding{}
		if e := query.Order("subject_id ASC").Offset((q.Page - 1) * q.Size).Limit(q.Size).Find(&rows).Error; e != nil {
			return e
		}
		for _, r := range rows {
			state := "offline"
			if online(r) {
				state = "online"
			}
			items = append(items, PlatformView{State: state, Online: online(r), ID: r.ID, SubjectType: "platform", SubjectID: r.SubjectID, Role: r.Role, IngressType: r.IngressType, BindingRevision: protocol.Number(r.BindingRevision), AuthenticatedAt: r.AuthenticatedAt})
		}
		return nil
	})
	return page(items, total, q), e
}

type EventView struct {
	ID          string    `json:"event_id"`
	Type        string    `json:"type"`
	OccurredAt  time.Time `json:"occurred_at"`
	Actor       string    `json:"actor"`
	SubjectType *string   `json:"subject_type"`
	SubjectID   *string   `json:"subject_id"`
	Reason      string    `json:"reason"`
	TraceID     string    `json:"trace_id"`
	Details     any       `json:"details"`
}

func (s *Service) Events(ctx context.Context, id string, q core.PageQuery) (core.Page[EventView], error) {
	row, e := s.repo.Get(ctx, id)
	if e != nil {
		return core.Page[EventView]{}, normalizeError(e)
	}
	now := time.Now().UTC()
	if q.End == nil {
		q.End = &now
	}
	start := q.End.Add(-24 * time.Hour)
	if q.Start == nil {
		q.Start = &start
	}
	if q.End.Before(*q.Start) || q.End.Sub(*q.Start) > 90*24*time.Hour {
		return core.Page[EventView]{}, core.Fail(400, "INVALID_ARGUMENT", "事件查询时间范围须在 90 天内")
	}
	rows, total, e := s.repo.Events(ctx, row.ID, q)
	if e != nil {
		return core.Page[EventView]{}, e
	}
	items := []EventView{}
	for _, r := range rows {
		var details any
		_ = json.Unmarshal([]byte(r.Details), &details)
		items = append(items, EventView{ID: r.ID, Type: r.EventType, OccurredAt: r.OccurredAt, Actor: r.Actor, SubjectType: r.SubjectType, SubjectID: r.SubjectID, Reason: r.Reason, TraceID: r.TraceID, Details: details})
	}
	return page(items, total, q), nil
}

type MetricPoint struct {
	SampledAt time.Time `json:"sampled_at"`
	Valid     bool      `json:"valid"`
	Values    any       `json:"values"`
}
type MetricSeries struct {
	Points      []MetricPoint `json:"points"`
	StepSeconds int           `json:"step_seconds"`
	Stale       bool          `json:"stale"`
}

func (s *Service) Metrics(ctx context.Context, id string, start, end time.Time, step int) (MetricSeries, error) {
	result := MetricSeries{Points: []MetricPoint{}, StepSeconds: step, Stale: true}
	row, e := s.repo.Get(ctx, id)
	if e != nil {
		return result, normalizeError(e)
	}
	if step != 60 && step != 300 || end.Before(start) || end.Sub(start) > 30*24*time.Hour || int64(end.Sub(start)/time.Second)/int64(step) > 9999 {
		return result, core.Fail(400, "INVALID_ARGUMENT", "指标区间、步长或点数超限")
	}
	rows := []store.Metric{}
	e = s.repo.Read(ctx, func(db *gorm.DB) error {
		return db.Where("gateway_id = ? AND granularity_seconds = ? AND bucket_at >= ? AND bucket_at <= ?", row.ID, step, start.Truncate(time.Duration(step)*time.Second), end).Order("bucket_at ASC").Limit(10000).Find(&rows).Error
	})
	if e != nil {
		return result, e
	}
	byTime := map[int64]store.Metric{}
	for _, r := range rows {
		byTime[r.BucketAt.Unix()] = r
	}
	for at := start.Truncate(time.Duration(step) * time.Second); !at.After(end); at = at.Add(time.Duration(step) * time.Second) {
		point := MetricPoint{SampledAt: at}
		if r, ok := byTime[at.Unix()]; ok && r.ValidCount > 0 {
			point.Valid = true
			gauges := store.MetricGauges{}
			if json.Unmarshal([]byte(r.Gauges), &gauges) == nil {
				point.Values = gauges.Values()
			}
		}
		result.Points = append(result.Points, point)
	}
	snapshot := s.runtime.Snapshot(row.ID)
	result.Stale = snapshot.ReceivedAt == nil || time.Since(*snapshot.ReceivedAt) > time.Duration(row.DesiredConfig.HeartbeatTimeoutMS)*time.Millisecond
	return result, nil
}

func trafficRX(t *core.SessionTraffic) *protocol.Decimal {
	if t == nil {
		return nil
	}
	return &t.RXBytes
}
func trafficTX(t *core.SessionTraffic) *protocol.Decimal {
	if t == nil {
		return nil
	}
	return &t.TXBytes
}
