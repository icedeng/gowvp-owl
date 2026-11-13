// Package openplatform 提供与管理员账号隔离的三方应用身份和资源授权。
package openplatform

import (
	"context"
	"errors"
	"time"
)

var Scopes = []string{"device:read", "channel:read", "live:play", "recording:read", "recording:play", "recording:download", "event:read", "event:image", "event:video", "event:download", "event:process", "snapshot:capture", "ptz:control", "event:subscribe"}
var BasicActions = []string{"left", "right", "up", "down", "left_up", "left_down", "right_up", "right_down", "zoom_in", "zoom_out", "focus_near", "focus_far", "iris_open", "iris_close", "preset_call"}

type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string                    { return e.Message }
func Fail(status int, code, message string) error { return &Error{status, code, message} }

var ErrNotFound = Fail(404, "NOT_FOUND", "资源不存在或无权访问")
var ErrAuth = Fail(401, "AUTHENTICATION_FAILED", "认证失败")
var ErrConflict = Fail(409, "CONFLICT", "状态已变化，请刷新后重试")
var ErrUnavailable = Fail(503, "SERVICE_UNAVAILABLE", "开放平台暂不可用")

func Has(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

type App struct {
	ID                string     `gorm:"primaryKey;size:64" json:"id"`
	Name              string     `json:"name"`
	Organization      string     `json:"organization"`
	Owner             string     `json:"owner"`
	Contact           string     `json:"contact"`
	Purpose           string     `json:"purpose"`
	Status            string     `gorm:"index;size:16" json:"status"`
	ExpiresAt         *time.Time `json:"expires_at"`
	Scopes            []string   `gorm:"serializer:json;type:text" json:"scopes"`
	DeviceAccessMode  string     `json:"device_access_mode"`
	DeviceIDs         []string   `gorm:"serializer:json;type:text" json:"device_ids"`
	AllowedActions    []string   `gorm:"serializer:json;type:text" json:"allowed_actions"`
	AllowAnyIP        bool       `json:"allow_any_ip"`
	CIDRs             []string   `gorm:"serializer:json;type:text" json:"cidrs"`
	QPS               int        `json:"qps"`
	Burst             int        `json:"burst"`
	LiveLimit         int        `json:"live_limit"`
	ChannelLimit      int        `json:"channel_limit"`
	DownloadLimit     int        `json:"download_limit"`
	SnapshotLimit     int        `json:"snapshot_limit"`
	SubscriptionLimit int        `json:"subscription_limit"`
	DeliveryLimit     int        `json:"delivery_limit"`
	Revision          int64      `json:"revision"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

func (App) TableName() string { return "open_apps" }

type Resource struct {
	AppID        string `gorm:"primaryKey;size:64"`
	ResourceType string `gorm:"primaryKey;size:16"`
	ResourceID   string `gorm:"primaryKey;size:128"`
}

func (Resource) TableName() string { return "open_app_resources" }

type Encrypted struct {
	KeyID      string `json:"key_id"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}
type Credential struct {
	ID        string     `gorm:"primaryKey;size:64" json:"id"`
	AppID     string     `gorm:"index;size:64" json:"app_id"`
	AccessKey string     `gorm:"uniqueIndex;size:80" json:"access_key"`
	Secret    Encrypted  `gorm:"serializer:json;type:text" json:"-"`
	Status    string     `json:"status"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

func (Credential) TableName() string { return "open_credentials" }

type Nonce struct {
	AccessKey string    `gorm:"primaryKey;size:80"`
	Nonce     string    `gorm:"primaryKey;size:128"`
	ExpiresAt time.Time `gorm:"index"`
}

func (Nonce) TableName() string { return "open_request_nonces" }

type Idempotency struct {
	AppID       string `gorm:"primaryKey;size:64"`
	Operation   string `gorm:"primaryKey;size:160"`
	Key         string `gorm:"primaryKey;size:128"`
	Fingerprint string
	ResourceID  string
	Status      string
	ExpiresAt   time.Time `gorm:"index"`
}

func (Idempotency) TableName() string { return "open_idempotency_records" }

type Audit struct {
	ID           uint64    `gorm:"primaryKey" json:"id"`
	RequestID    string    `gorm:"index;size:64" json:"request_id"`
	AppID        string    `gorm:"index:idx_open_audit_app_time;size:64" json:"app_id"`
	CredentialID string    `json:"credential_id"`
	Actor        string    `json:"actor"`
	IP           string    `json:"ip"`
	Route        string    `json:"route"`
	Action       string    `json:"action"`
	ResourceID   string    `json:"resource_id"`
	Result       string    `json:"result"`
	Status       int       `json:"status"`
	DurationMS   int64     `json:"duration_ms"`
	CreatedAt    time.Time `gorm:"index;index:idx_open_audit_app_time" json:"created_at"`
}

func (Audit) TableName() string { return "open_audit_logs" }

type Session struct {
	ID           string    `gorm:"primaryKey;size:64" json:"id"`
	AppID        string    `gorm:"index;size:64" json:"app_id"`
	CredentialID string    `json:"-"`
	ChannelID    string    `gorm:"index;size:128" json:"channel_id"`
	Protocol     string    `json:"protocol"`
	State        string    `gorm:"index;size:24" json:"state"`
	Revision     int64     `json:"-"`
	Source       Source    `gorm:"serializer:json;type:text" json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `gorm:"index" json:"expires_at"`
}

func (Session) TableName() string { return "open_media_sessions" }

type Ticket struct {
	Hash         string    `gorm:"primaryKey;size:64" json:"-"`
	AppID        string    `gorm:"index;size:64" json:"-"`
	CredentialID string    `json:"-"`
	ChannelID    string    `json:"-"`
	DeviceID     string    `json:"-"`
	SessionID    string    `gorm:"index;size:64" json:"-"`
	ResourceID   string    `json:"-"`
	Kind         string    `json:"-"`
	Revision     int64     `json:"-"`
	ExpiresAt    time.Time `gorm:"index" json:"expires_at"`
	Revoked      bool      `json:"-"`
}

func (Ticket) TableName() string { return "open_media_tickets" }

type Snapshot struct {
	ImageURL       string     `gorm:"-" json:"image_url,omitempty"`
	ImageExpiresAt *time.Time `gorm:"-" json:"image_expires_at,omitempty"`
	ID             string     `gorm:"primaryKey;size:64" json:"id"`
	AppID          string     `gorm:"index;size:64" json:"app_id"`
	CredentialID   string     `json:"-"`
	ChannelID      string     `gorm:"index;size:128" json:"channel_id"`
	State          string     `gorm:"index;size:24" json:"state"`
	Revision       int64      `json:"-"`
	Path           string     `json:"-"`
	Error          string     `json:"error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	CapturedAt     *time.Time `json:"captured_at"`
	ExpiresAt      time.Time  `gorm:"index" json:"expires_at"`
}

func (Snapshot) TableName() string { return "open_snapshot_tasks" }

type PTZInput struct {
	Action   string `json:"action"`
	Speed    uint8  `json:"speed"`
	Duration int    `json:"duration"`
	Preset   int    `json:"preset"`
	Group    uint8  `json:"group"`
	Aux      uint8  `json:"aux"`
	Value    uint16 `json:"value"`
}
type Command struct {
	StopAttempts int       `json:"stop_attempts"`
	NextStopAt   time.Time `gorm:"index" json:"next_stop_at"`
	ID           string    `gorm:"primaryKey;size:64" json:"id"`
	AppID        string    `gorm:"index;size:64" json:"app_id"`
	CredentialID string    `json:"-"`
	ChannelID    string    `gorm:"index;size:128" json:"channel_id"`
	Input        PTZInput  `gorm:"serializer:json;type:text" json:"input"`
	State        string    `gorm:"index;size:24" json:"state"`
	Error        string    `json:"error,omitempty"`
	Revision     int64     `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	Deadline     time.Time `json:"deadline"`
}

func (Command) TableName() string { return "open_ptz_commands" }

type Lease struct {
	ChannelID string `gorm:"primaryKey;size:128"`
	CommandID string `gorm:"size:64"`
}

func (Lease) TableName() string { return "open_ptz_leases" }

type CallbackTarget struct {
	ID        string    `gorm:"primaryKey;size:64" json:"id"`
	AppID     string    `gorm:"index;size:64" json:"app_id"`
	URL       string    `json:"url"`
	CIDRs     []string  `gorm:"serializer:json;type:text" json:"cidrs"`
	Status    string    `json:"status"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
}

func (CallbackTarget) TableName() string { return "open_callback_targets" }

type SigningKey struct {
	ID        string
	Secret    Encrypted
	ExpiresAt *time.Time
}
type Subscription struct {
	ID               string       `gorm:"primaryKey;size:64" json:"id"`
	AppID            string       `gorm:"index;size:64" json:"app_id"`
	CredentialID     string       `json:"-"`
	CallbackTargetID string       `json:"callback_target_id"`
	EventTypes       []string     `gorm:"serializer:json;type:text" json:"event_types"`
	DeviceIDs        []string     `gorm:"serializer:json;type:text" json:"device_ids"`
	ChannelIDs       []string     `gorm:"serializer:json;type:text" json:"channel_ids"`
	Labels           []string     `gorm:"serializer:json;type:text" json:"labels"`
	Status           string       `json:"status"`
	Keys             []SigningKey `gorm:"serializer:json;type:text" json:"-"`
	KeyConfigured    bool         `gorm:"-" json:"key_configured"`
	ExpiresAt        *time.Time   `json:"expires_at"`
	Revision         int64        `json:"revision"`
	CreatedAt        time.Time    `json:"created_at"`
}

func (Subscription) TableName() string { return "open_subscriptions" }

type Outbox struct {
	ID              uint64  `gorm:"primaryKey"`
	EventID         int64   `gorm:"index:idx_open_outbox_event"`
	NotificationKey *string `gorm:"size:128;uniqueIndex"`
	EventType       string  `gorm:"size:64"`
	DeviceID        string
	ChannelID       string
	Label           string
	Payload         string `gorm:"type:text"`
	CreatedAt       time.Time
	Done            bool `gorm:"index"`
}

func (Outbox) TableName() string { return "open_event_outbox" }

type Delivery struct {
	ID             string    `gorm:"primaryKey;size:64" json:"id"`
	AppID          string    `gorm:"index;size:64" json:"app_id"`
	SubscriptionID string    `gorm:"uniqueIndex:idx_open_delivery_notification;size:64" json:"subscription_id"`
	EventID        int64     `gorm:"index" json:"event_id"`
	OutboxID       uint64    `gorm:"uniqueIndex:idx_open_delivery_notification" json:"-"`
	EventType      string    `gorm:"size:64" json:"event_type"`
	State          string    `gorm:"index;size:24" json:"state"`
	Attempt        int       `json:"attempt"`
	LastStatus     int       `json:"last_status"`
	Error          string    `json:"error,omitempty"`
	NextRetryAt    time.Time `gorm:"index" json:"next_retry_at"`
	LeaseUntil     time.Time `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
}

func (Delivery) TableName() string { return "open_deliveries" }

type Principal struct {
	App        App
	Credential Credential
}
type Source struct {
	Query         string
	MediaServerID string
	App           string
	Stream        string
	BaseURL       string
}

// Host 只暴露必要的 Owl 业务能力，不依赖 HTTP handler 或管理员凭据。
type Host interface {
	Start(context.Context, string) (Source, error)
	Snapshot(context.Context, string, string) error
	PTZ(context.Context, string, PTZInput) error
	Capabilities(context.Context, string) ([]string, error)
	RecordingPath(context.Context, int64) (string, error)
	EventImagePath(context.Context, int64) (string, error)
}

func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{503, "SERVICE_UNAVAILABLE", "开放平台暂不可用"}
}
