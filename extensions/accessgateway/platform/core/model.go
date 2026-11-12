package core

import (
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"time"
)

type Counts struct {
	DeviceTotal      int64 `json:"device_total"`
	DeviceOnline     int64 `json:"device_online"`
	DeviceOffline    int64 `json:"device_offline"`
	ChannelTotal     int64 `json:"channel_total"`
	ChannelOnline    int64 `json:"channel_online"`
	PlatformBindings int64 `json:"platform_bindings"`
	ActiveSources    int64 `json:"active_sources"`
	LiveSessions     int64 `json:"live_sessions"`
	PlaybackSessions int64 `json:"playback_sessions"`
	DownloadSessions int64 `json:"download_sessions"`
	VoiceSessions    int64 `json:"voice_sessions"`
	TransferSessions int64 `json:"transfer_sessions"`
	CleanupPending   int64 `json:"cleanup_pending"`
}
type Certificate struct {
	ID            string    `json:"id"`
	Fingerprint   string    `json:"fingerprint"`
	CAFingerprint string    `json:"ca_fingerprint"`
	NotBefore     time.Time `json:"not_before"`
	NotAfter      time.Time `json:"not_after"`
	Status        string    `json:"status"`
}
type SessionTraffic struct {
	SampledAt  time.Time         `json:"sampled_at"`
	RXBytes    protocol.Decimal  `json:"rx_bytes"`
	TXBytes    protocol.Decimal  `json:"tx_bytes"`
	RXBPS      *protocol.Decimal `json:"rx_bps"`
	TXBPS      *protocol.Decimal `json:"tx_bps"`
	QueueBytes protocol.Decimal  `json:"queue_bytes"`
}

type Metrics struct {
	DeviceRXBytes *protocol.Decimal `json:"device_rx_bytes"`
	DeviceTXBytes *protocol.Decimal `json:"device_tx_bytes"`
	QueueBytes    *protocol.Decimal `json:"queue_bytes"`
	ToABPS        *protocol.Decimal `json:"to_a_bps"`
	ToDeviceBPS   *protocol.Decimal `json:"to_device_bps"`
	WSSRXBytes    *protocol.Decimal `json:"wss_rx_bytes"`
	WSSTXBytes    *protocol.Decimal `json:"wss_tx_bytes"`
	RTTMS         *float64          `json:"rtt_ms"`
	PortTotal     *int              `json:"port_total"`
	PortUsed      *int              `json:"port_used"`
	CPUPercent    *float64          `json:"cpu_percent"`
	RSSBytes      *protocol.Decimal `json:"rss_bytes"`
}
type Snapshot struct {
	SnapshotID string     `json:"snapshot_id"`
	SampledAt  time.Time  `json:"sampled_at"`
	ReceivedAt *time.Time `json:"received_at"`
	Stale      bool       `json:"stale"`
}
type GatewayView struct {
	Software           *protocol.GatewayInfo     `json:"software"`
	AlarmError         *protocol.Error           `json:"alarm_error"`
	RecoveryDurationMS *int64                    `json:"recovery_duration_ms"`
	ID                 string                    `json:"id"`
	Name               string                    `json:"name"`
	Group              string                    `json:"group"`
	Description        string                    `json:"description"`
	Revision           protocol.Decimal          `json:"revision"`
	ConfigRevision     protocol.Decimal          `json:"config_revision"`
	AppliedRevision    protocol.Decimal          `json:"applied_revision"`
	DesiredConfig      *DesiredConfig            `json:"desired_config,omitempty"`
	AppliedConfig      *protocol.EffectiveConfig `json:"applied_config,omitempty"`
	AdminState         string                    `json:"admin_state"`
	ConnectionState    string                    `json:"connection_state"`
	HealthState        string                    `json:"health_state"`
	PendingOperationID *string                   `json:"pending_operation_id"`
	LastSeenAt         *time.Time                `json:"last_seen_at"`
	ConnectedAt        *time.Time                `json:"connected_at"`
	LastError          *protocol.Error           `json:"last_error"`
	Capabilities       []string                  `json:"capabilities"`
	Certificate        *Certificate              `json:"certificate"`
	Counts             *Counts                   `json:"counts"`
	CountsError        *protocol.Error           `json:"counts_error"`
	Metrics            Metrics                   `json:"metrics"`
	MetricsStale       bool                      `json:"metrics_stale"`
	Snapshot
}
type Progress struct {
	Completed int `json:"completed"`
	Total     int `json:"total"`
}
type Operation struct {
	OperationID string           `json:"operation_id"`
	GatewayID   *string          `json:"gateway_id"`
	Type        string           `json:"type"`
	Status      string           `json:"status"`
	Step        string           `json:"step"`
	Progress    Progress         `json:"progress"`
	Revision    protocol.Decimal `json:"revision"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
	DeadlineAt  *time.Time       `json:"deadline_at"`
	Result      any              `json:"result"`
	Error       *protocol.Error  `json:"error"`
}
type DiagnosticCheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	Msg        string `json:"msg"`
	DurationMS int    `json:"duration_ms"`
}
type DiagnosticResult struct {
	CheckedAt         time.Time         `json:"checked_at"`
	Checks            []DiagnosticCheck `json:"checks"`
	GatewayID         string            `json:"gateway_id"`
	ConfigRevision    protocol.Decimal  `json:"config_revision"`
	Capabilities      []string          `json:"capabilities"`
	CanEnable         bool              `json:"can_enable"`
	MediaPathVerified bool              `json:"media_path_verified"`
}
type IngressRoute struct {
	IngressType     string
	GatewayPK       string
	GatewayID       string
	Generation      string
	ConnectionEpoch protocol.Decimal
	OwnerEpoch      protocol.Decimal
	FlowID          string
	BindingRevision protocol.Decimal
	SIPAdvertise    protocol.Endpoint
	ConfigRevision  protocol.Decimal
}
type RuntimeSnapshot struct {
	Software           *protocol.GatewayInfo
	AlarmError         *protocol.Error
	RecoveryStartedAt  *time.Time
	RecoveryDurationMS *int64
	ConnectionState    string
	HealthState        string
	ConnectedAt        *time.Time
	LastSeenAt         *time.Time
	Error              *protocol.Error
	Generation         string
	Epoch              protocol.Decimal
	OwnerEpoch         protocol.Decimal
	Capabilities       []string
	Status             *protocol.StatusSnapshot
	Metrics            *protocol.MetricsSnapshot
	ReceivedAt         *time.Time
	MetricsReceivedAt  *time.Time
	StatusReceivedAt   *time.Time
	RTTMS              *float64
	ToABPS             *protocol.Decimal
	ToDeviceBPS        *protocol.Decimal
}
type Subject struct {
	Type            string           `json:"type"`
	ID              string           `json:"id"`
	BindingRevision protocol.Decimal `json:"binding_revision"`
}
type DeviceIngressView struct {
	RecoveryState   string           `json:"recovery_state"`
	DeviceID        string           `json:"device_id"`
	DeviceCode      string           `json:"device_code"`
	Name            string           `json:"name"`
	IngressType     string           `json:"ingress_type"`
	GatewayID       *string          `json:"gateway_id"`
	BindingRevision protocol.Decimal `json:"binding_revision"`
	Online          bool             `json:"online"`
	ChannelTotal    int64            `json:"channel_total"`
	ChannelOnline   int64            `json:"channel_online"`
	RegisteredAt    *time.Time       `json:"registered_at"`
	KeepaliveAt     *time.Time       `json:"keepalive_at"`
}

type PathTimings struct {
	WSSAttachedAt *time.Time `json:"wss_attached_at"`
	AForwardedAt  *time.Time `json:"a_forwarded_at"`
}
