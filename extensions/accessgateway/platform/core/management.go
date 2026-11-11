package core

import (
	"context"
	"time"
)

type PageQuery struct {
	Page            int
	Size            int
	Q               string
	Group           string
	AdminState      string
	ConnectionState string
	HealthState     string
	Sort            string
	Order           string
	Purpose         string
	State           string
	SubjectID       string
	Online          *bool
	Start           *time.Time
	End             *time.Time
	Type            string
}
type Page[T any] struct {
	Items      []T       `json:"items"`
	Total      int64     `json:"total"`
	Page       int       `json:"page"`
	Size       int       `json:"size"`
	SnapshotID string    `json:"snapshot_id"`
	SampledAt  time.Time `json:"sampled_at"`
	Stale      bool      `json:"stale"`
}

// ManagementHost 聚合查询和清理只接受明确对象，禁止把整个宿主 Usecase 传入扩展。
type ManagementHost interface {
	GatewayCounts(context.Context, string, RuntimeSnapshot) (Counts, error)
	GatewayDevices(context.Context, string, RuntimeSnapshot, PageQuery) (Page[DeviceIngressView], error)
	ValidateMediaServer(context.Context, *string, []string) error
	StopGatewaySession(context.Context, string, string, bool) error
}
type Mutation struct {
	Actor            string
	Route            string
	Key              string
	ExpectedRevision *int64
}
type CreateRequest struct {
	ID     string        `json:"id"`
	Config DesiredConfig `json:"config"`
	Enable bool          `json:"enable,omitempty"`
}
type UpdateRequest struct {
	Config DesiredConfig `json:"config"`
}
type StateRequest struct {
	State          string `json:"state"`
	Force          bool   `json:"force,omitempty"`
	DrainTimeoutMS int    `json:"drain_timeout_ms,omitempty"`
}
type ReasonRequest struct {
	Reason string `json:"reason"`
}
type RotationRequest struct {
	NewCredentialID string `json:"new_credential_id"`
	DrainTimeoutMS  int    `json:"drain_timeout_ms,omitempty"`
}
type DraftTestRequest struct {
	DraftID string `json:"draft_id"`
}
type Result struct {
	Status int `json:"status"`
	Body   any `json:"body"`
}
