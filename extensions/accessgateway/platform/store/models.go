package store

import (
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"time"
)

type Base struct {
	ID        string    `gorm:"type:varchar(64);primaryKey"`
	CreatedAt time.Time `gorm:"not null"`
	UpdatedAt time.Time `gorm:"not null"`
}
type Gateway struct {
	Base
	StableID           string                    `gorm:"type:varchar(64);not null;uniqueIndex"`
	Name               string                    `gorm:"type:varchar(80);not null"`
	GroupName          string                    `gorm:"type:varchar(64);not null;index:idx_ag_group_deleted,priority:1"`
	Description        string                    `gorm:"type:text;not null"`
	AdminState         string                    `gorm:"type:varchar(24);not null;index:idx_ag_state_deleted,priority:1"`
	Revision           int64                     `gorm:"not null;default:1"`
	ConfigRevision     int64                     `gorm:"not null;default:0"`
	AppliedRevision    int64                     `gorm:"not null;default:0"`
	DesiredConfig      core.DesiredConfig        `gorm:"serializer:json;type:text;not null"`
	AppliedConfig      *protocol.EffectiveConfig `gorm:"serializer:json;type:text"`
	CredentialID       *string                   `gorm:"type:varchar(64)"`
	EndpointActiveKey  *string                   `gorm:"type:char(64);uniqueIndex"`
	OwnerID            *string                   `gorm:"type:varchar(64)"`
	OwnerEpoch         int64                     `gorm:"not null;default:0"`
	OwnerExpiresAt     *time.Time
	PendingOperationID *string    `gorm:"type:varchar(64)"`
	DeletedAt          *time.Time `gorm:"index:idx_ag_state_deleted,priority:2;index:idx_ag_group_deleted,priority:2"`
	LastSeenAt         *time.Time
	LastError          *protocol.Error `gorm:"serializer:json;type:text"`
}

func (Gateway) TableName() string { return "access_gateways" }

type Credential struct {
	Base
	GatewayID     string    `gorm:"type:varchar(64);not null;index:idx_agc_gateway_state,priority:1"`
	CAFileRef     string    `gorm:"type:varchar(128);not null"`
	CertFileRef   string    `gorm:"type:varchar(128);not null"`
	KeyFileRef    string    `gorm:"type:varchar(128);not null"`
	Fingerprint   string    `gorm:"type:char(64);not null"`
	CAFingerprint string    `gorm:"type:char(64);not null"`
	NotBefore     time.Time `gorm:"not null"`
	NotAfter      time.Time `gorm:"not null"`
	State         string    `gorm:"type:varchar(24);not null;index:idx_agc_gateway_state,priority:2"`
	Revision      int64     `gorm:"not null;default:1"`
}

func (Credential) TableName() string { return "access_gateway_credentials" }

type BindingFields struct {
	RegisteredUntil     *time.Time
	SubjectID           string    `gorm:"type:varchar(64);not null;uniqueIndex;index:,composite:gateway_subject,priority:2"`
	IngressType         string    `gorm:"type:varchar(24);not null"`
	GatewayID           *string   `gorm:"type:varchar(64);index:,composite:gateway_subject,priority:1;uniqueIndex:,composite:ingress_flow,priority:1"`
	BindingRevision     int64     `gorm:"not null;default:1"`
	AuthenticatedAt     time.Time `gorm:"not null"`
	LastGeneration      *string   `gorm:"type:varchar(64);uniqueIndex:,composite:ingress_flow,priority:2"`
	LastConnectionEpoch *int64    `gorm:"uniqueIndex:,composite:ingress_flow,priority:3"`
	LastFlowID          *string   `gorm:"type:varchar(64);uniqueIndex:,composite:ingress_flow,priority:4"`
}
type Binding struct {
	Base
	BindingFields
}

func (Binding) TableName() string { return "access_gateway_bindings" }

type PlatformBinding struct {
	Base
	BindingFields
	Role string `gorm:"type:varchar(24);not null"`
}

func (PlatformBinding) TableName() string { return "access_gateway_platform_bindings" }

type Endpoints struct {
	RTP  *protocol.Endpoint `json:"rtp"`
	RTCP *protocol.Endpoint `json:"rtcp"`
}
type CleanupSteps struct {
	TransferCommitted   bool `json:"transfer_committed"`
	BusinessInvalidated bool `json:"business_invalidated"`
	DialogClosed        bool `json:"dialog_closed"`
	DialogUnavailable   bool `json:"dialog_unavailable"`
	RemoteClosed        bool `json:"remote_closed"`
	BridgeClosed        bool `json:"bridge_closed"`
	MediaClosed         bool `json:"media_closed"`
	Committed           bool `json:"committed"`
}
type Session struct {
	Timings *core.PathTimings    `gorm:"serializer:json;type:text"`
	Traffic *core.SessionTraffic `gorm:"serializer:json;type:text"`
	Base
	GatewayID         string       `gorm:"type:varchar(64);not null;uniqueIndex:idx_ags_remote,priority:1;index:idx_ags_gateway_state,priority:1"`
	ResourceType      string       `gorm:"type:varchar(24);not null;uniqueIndex:idx_ags_remote,priority:4"`
	RemoteID          string       `gorm:"type:varchar(64);not null;uniqueIndex:idx_ags_remote,priority:5"`
	Generation        string       `gorm:"type:varchar(64);not null;uniqueIndex:idx_ags_remote,priority:2"`
	ConnectionEpoch   int64        `gorm:"not null;uniqueIndex:idx_ags_remote,priority:3"`
	OwnerEpoch        int64        `gorm:"not null"`
	SubjectType       string       `gorm:"type:varchar(24);not null;index:idx_ags_subject_state,priority:1"`
	SubjectID         string       `gorm:"type:varchar(64);not null;index:idx_ags_subject_state,priority:2"`
	ChannelID         *string      `gorm:"type:varchar(64)"`
	BindingRevision   int64        `gorm:"not null"`
	ConfigRevision    int64        `gorm:"not null"`
	Purpose           string       `gorm:"type:varchar(24);not null"`
	WireMode          string       `gorm:"type:varchar(24);not null"`
	LocalMode         string       `gorm:"type:varchar(24);not null"`
	Direction         string       `gorm:"type:varchar(24);not null"`
	SSRC              *string      `gorm:"type:char(10)"`
	SendSSRC          *string      `gorm:"type:char(10)"`
	MediaServerID     *string      `gorm:"type:varchar(64)"`
	StreamID          *string      `gorm:"type:varchar(128)"`
	MediaNodeSnapshot string       `gorm:"type:text;not null"`
	PublicEndpoint    Endpoints    `gorm:"serializer:json;type:text;not null"`
	LocalEndpoint     Endpoints    `gorm:"serializer:json;type:text;not null"`
	State             string       `gorm:"type:varchar(24);not null;index:idx_ags_gateway_state,priority:2;index:idx_ags_subject_state,priority:3;index:idx_ags_state_expiry,priority:1"`
	CleanupSteps      CleanupSteps `gorm:"serializer:json;type:text;not null"`
	LastRenewedAt     *time.Time
	ExpiresAt         *time.Time `gorm:"index:idx_ags_state_expiry,priority:2"`
	ClosedAt          *time.Time
	Error             *protocol.Error `gorm:"serializer:json;type:text"`
}

func (Session) TableName() string { return "access_gateway_sessions" }

type SSRCReservation struct {
	SSRC            string     `gorm:"type:char(10);primaryKey"`
	SessionID       string     `gorm:"type:varchar(64);not null"`
	State           string     `gorm:"type:varchar(24);not null;index:idx_agssrc_quarantine,priority:1"`
	ReservedAt      time.Time  `gorm:"not null"`
	QuarantineUntil *time.Time `gorm:"index:idx_agssrc_quarantine,priority:2"`
}

func (SSRCReservation) TableName() string { return "access_gateway_ssrc_reservations" }

type Operation struct {
	Base
	UpdatedAt        time.Time       `gorm:"not null;index:idx_ago_status_updated,priority:2"`
	GatewayID        *string         `gorm:"type:varchar(64);index:idx_ago_gateway_status,priority:1"`
	Type             string          `gorm:"type:varchar(40);not null"`
	Status           string          `gorm:"type:varchar(24);not null;index:idx_ago_gateway_status,priority:2;index:idx_ago_status_updated,priority:1"`
	Step             string          `gorm:"type:varchar(64);not null"`
	ExpectedRevision int64           `gorm:"not null"`
	OperationKeyHash string          `gorm:"type:char(64);not null;uniqueIndex"`
	RequestHash      string          `gorm:"type:char(64);not null"`
	RequestSnapshot  string          `gorm:"type:text;not null"`
	Result           string          `gorm:"type:text;not null"`
	Error            *protocol.Error `gorm:"serializer:json;type:text"`
	Completed        int             `gorm:"not null;default:0"`
	Total            int             `gorm:"not null;default:0"`
	DeadlineAt       *time.Time
	Revision         int64 `gorm:"not null;default:1"`
}

func (Operation) TableName() string { return "access_gateway_operations" }

type Metric struct {
	Base
	GatewayID          string    `gorm:"type:varchar(64);not null;uniqueIndex:idx_agm_bucket,priority:1"`
	GranularitySeconds int       `gorm:"not null;uniqueIndex:idx_agm_bucket,priority:2"`
	BucketAt           time.Time `gorm:"not null;uniqueIndex:idx_agm_bucket,priority:3;index"`
	SampleCount        int       `gorm:"not null"`
	ValidCount         int       `gorm:"not null"`
	Counters           string    `gorm:"type:text;not null"`
	Gauges             string    `gorm:"type:text;not null"`
	LastGeneration     string    `gorm:"type:varchar(64);not null"`
	LastSampleSeq      int64     `gorm:"not null"`
}

func (Metric) TableName() string { return "access_gateway_metrics" }

type Event struct {
	Base
	GatewayID           *string   `gorm:"type:varchar(64);index:idx_age_gateway_time,priority:1"`
	GatewayNameSnapshot string    `gorm:"type:varchar(80);not null"`
	SubjectType         *string   `gorm:"type:varchar(24);index:idx_age_subject_time,priority:1"`
	SubjectID           *string   `gorm:"type:varchar(64);index:idx_age_subject_time,priority:2"`
	Actor               string    `gorm:"type:varchar(128);not null"`
	EventType           string    `gorm:"type:varchar(64);not null"`
	Reason              string    `gorm:"type:varchar(128);not null"`
	TraceID             string    `gorm:"type:varchar(64);not null"`
	Details             string    `gorm:"type:text;not null"`
	OccurredAt          time.Time `gorm:"not null;index;index:idx_age_gateway_time,priority:2;index:idx_age_subject_time,priority:3"`
}

func (Event) TableName() string { return "access_gateway_events" }

type Migration struct {
	Base
	SourceGatewayID *string   `gorm:"type:varchar(64);index"`
	TargetGatewayID *string   `gorm:"type:varchar(64);index"`
	State           string    `gorm:"type:varchar(24);not null;index:idx_agmig_state_expiry,priority:1"`
	ExpiresAt       time.Time `gorm:"not null;index:idx_agmig_state_expiry,priority:2"`
	Revision        int64     `gorm:"not null;default:1"`
	OperationID     string    `gorm:"type:varchar(64);not null"`
}

func (Migration) TableName() string { return "access_gateway_migrations" }

type MigrationItem struct {
	Base
	MigrationID             string          `gorm:"type:varchar(64);not null;uniqueIndex:idx_agmi_subject,priority:1"`
	SubjectType             string          `gorm:"type:varchar(24);not null;uniqueIndex:idx_agmi_subject,priority:2"`
	SubjectID               string          `gorm:"type:varchar(64);not null;uniqueIndex:idx_agmi_subject,priority:3"`
	ExpectedBindingRevision int64           `gorm:"not null"`
	State                   string          `gorm:"type:varchar(24);not null"`
	ActiveSubjectKey        *string         `gorm:"type:char(64);uniqueIndex"`
	Error                   *protocol.Error `gorm:"serializer:json;type:text"`
	Revision                int64           `gorm:"not null;default:1"`
}

func (MigrationItem) TableName() string { return "access_gateway_migration_items" }
func Models() []any {
	return []any{&Fault{}, &Gateway{}, &Credential{}, &Binding{}, &PlatformBinding{}, &Session{}, &SSRCReservation{}, &Operation{}, &Metric{}, &Event{}, &Migration{}, &MigrationItem{}}
}

// Fault 持久化采样、防抖与事件中心关联；同一故障只创建一次事件。
type Fault struct {
	GatewayID     string    `gorm:"type:varchar(64);primaryKey"`
	Kind          string    `gorm:"type:varchar(40);primaryKey"`
	Observed      bool      `gorm:"not null"`
	ObservedSince time.Time `gorm:"not null"`
	Active        bool      `gorm:"not null"`
	IncidentID    string    `gorm:"type:varchar(64);not null"`
	Transitions   string    `gorm:"type:text;not null"`
}

func (Fault) TableName() string { return "access_gateway_faults" }
