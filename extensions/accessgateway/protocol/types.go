// Package protocol 定义接入网关 v1 的共享线协议，不依赖任一端的业务实现。
package protocol

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
)

const (
	Version         = 1
	Subprotocol     = "owl-gateway.v1"
	ControlPath     = "/gateway/v1/control"
	MediaPath       = "/gateway/v1/media"
	TransferPath    = "/gateway/v1/transfer"
	ProbePath       = "/gateway/v1/probe"
	MaxControlBytes = 2 << 20
	MaxBinaryBytes  = 256 << 10
)

var idPattern = regexp.MustCompile(`^[a-z0-9]{8}-[a-z0-9]{4}-4[a-z0-9]{3}-[89ab][a-z0-9]{3}-[a-z0-9]{12}$`)
var stableIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func ValidID(s string) bool { return idPattern.MatchString(s) && allHex(s) }
func allHex(s string) bool {
	for _, c := range s {
		if c != '-' && !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func ValidGatewayID(s string) bool { return stableIDPattern.MatchString(s) }

// Decimal 使用字符串避免经过 JavaScript 时丢失 int64 精度。
type Decimal string

func Number(n int64) Decimal { return Decimal(strconv.FormatInt(n, 10)) }
func (d Decimal) Int64() (int64, error) {
	s := string(d)
	if s == "" || len(s) > 1 && s[0] == '0' {
		return 0, fmt.Errorf("invalid decimal")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid decimal")
		}
	}
	return strconv.ParseInt(s, 10, 64)
}
func (d *Decimal) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("decimal must be a string")
	}
	v := Decimal(s)
	if _, err := v.Int64(); err != nil {
		return err
	}
	*d = v
	return nil
}

type Endpoint struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

func (e Endpoint) Validate() error {
	ip, err := netip.ParseAddr(e.IP)
	if err != nil || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() || ip.String() != e.IP || e.Port < 1 || e.Port > 65535 {
		return fmt.Errorf("invalid endpoint")
	}
	return nil
}
func (e Endpoint) String() string {
	return netip.AddrPortFrom(netip.MustParseAddr(e.IP), uint16(e.Port)).String()
}

type Error struct {
	Code         string         `json:"code"`
	Message      string         `json:"message"`
	Retryable    bool           `json:"retryable"`
	RetryAfterMS int            `json:"retry_after_ms"`
	Details      map[string]any `json:"details"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }
func Failure(code, message string) *Error {
	return &Error{Code: code, Message: message, Details: map[string]any{}}
}

const (
	InvalidArgument       = "INVALID_ARGUMENT"
	ProtocolUnsupported   = "PROTOCOL_VERSION_UNSUPPORTED"
	IdentityMismatch      = "IDENTITY_MISMATCH"
	Forbidden             = "FORBIDDEN"
	OwnerConflict         = "OWNER_CONFLICT"
	StaleEpoch            = "STALE_EPOCH"
	ConfigConflict        = "CONFIG_REVISION_CONFLICT"
	BindingConflict       = "BINDING_CONFLICT"
	IdempotencyConflict   = "IDEMPOTENCY_CONFLICT"
	CapabilityUnsupported = "CAPABILITY_UNSUPPORTED"
	GatewayDraining       = "GATEWAY_DRAINING"
	GatewayDisabled       = "GATEWAY_DISABLED"
	ConfigBoundary        = "CONFIG_BOUNDARY_VIOLATION"
	NotFound              = "RESOURCE_NOT_FOUND"
	Exhausted             = "RESOURCE_EXHAUSTED"
	PortBindFailed        = "PORT_BIND_FAILED"
	DeviceUnreachable     = "DEVICE_UNREACHABLE"
	UpstreamTimeout       = "UPSTREAM_TIMEOUT"
	MediaBackpressure     = "MEDIA_BACKPRESSURE"
	TransferLimit         = "TRANSFER_LIMIT_EXCEEDED"
	TicketInvalid         = "ATTACH_TICKET_INVALID"
	InternalError         = "INTERNAL_ERROR"
	IOError               = "IO_ERROR"
)

var Capabilities = []string{"sip_udp", "sip_tcp", "rtp_udp", "rtp_tcp_passive", "rtp_tcp_active", "rtcp_udp", "rtcp_tcp_mux", "playback", "rtp_download", "raw_tcp_download", "voice_duplex", "broadcast", "cascade", "https_transfer"}

func ValidCapability(s string) bool {
	for _, c := range Capabilities {
		if c == s {
			return true
		}
	}
	return false
}

type Envelope struct {
	Version         int             `json:"version"`
	Type            string          `json:"type"`
	Kind            string          `json:"kind"`
	RequestID       string          `json:"request_id"`
	ReplyTo         string          `json:"reply_to,omitempty"`
	GatewayID       string          `json:"gateway_id"`
	PlatformID      string          `json:"platform_id"`
	Generation      *string         `json:"generation"`
	ConnectionEpoch *Decimal        `json:"connection_epoch"`
	OwnerEpoch      Decimal         `json:"owner_epoch"`
	Payload         json.RawMessage `json:"payload"`
	Error           *Error          `json:"error,omitempty"`
}

type Route struct {
	FlowID          string   `json:"flow_id"`
	Transport       string   `json:"transport"`
	Peer            Endpoint `json:"peer"`
	Listener        Endpoint `json:"listener"`
	BindingRevision *Decimal `json:"binding_revision"`
}
type Binding struct {
	SubjectType     string  `json:"subject_type"`
	SubjectID       string  `json:"subject_id"`
	FlowID          string  `json:"flow_id"`
	BindingRevision Decimal `json:"binding_revision"`
	ExpiresInMS     int     `json:"expires_in_ms"`
}
type SIPPacket struct {
	Route     Route  `json:"route"`
	RawBase64 string `json:"raw_base64"`
}
type FlowOpen struct {
	ConfiguredPlatform bool     `json:"configured_platform,omitempty"`
	OperationID        string   `json:"operation_id"`
	SubjectType        string   `json:"subject_type"`
	SubjectID          string   `json:"subject_id"`
	Target             Endpoint `json:"target"`
	Transport          string   `json:"transport"`
	BindingRevision    Decimal  `json:"binding_revision"`
}
type FlowOpenResult struct {
	Route Route `json:"route"`
}
type FlowClose struct {
	OperationID string `json:"operation_id"`
	FlowID      string `json:"flow_id"`
	Reason      string `json:"reason"`
}
type FlowCloseResult struct {
	FlowID string `json:"flow_id"`
	Closed bool   `json:"closed"`
}
type BindingUpdate struct {
	OperationID string  `json:"operation_id"`
	Action      string  `json:"action"`
	Binding     Binding `json:"binding"`
}
type BindingResult struct {
	SubjectID       string  `json:"subject_id"`
	BindingRevision Decimal `json:"binding_revision"`
	Applied         bool    `json:"applied"`
}
type FlowClosed struct {
	FlowID string `json:"flow_id"`
	Reason string `json:"reason"`
}

type MediaSpec struct {
	SubjectType    string   `json:"subject_type"`
	SubjectID      string   `json:"subject_id"`
	ChannelID      *string  `json:"channel_id"`
	Purpose        string   `json:"purpose"`
	Direction      string   `json:"direction"`
	WireMode       string   `json:"wire_mode"`
	SSRC           *string  `json:"ssrc"`
	SendSSRC       *string  `json:"send_ssrc"`
	FlowID         string   `json:"flow_id"`
	ConfigRevision Decimal  `json:"config_revision"`
	SourceIPs      []string `json:"source_ips"`
	RTCP           string   `json:"rtcp"`
}
type Lease struct {
	SessionID      string    `json:"session_id"`
	State          string    `json:"state"`
	PublicRTP      *Endpoint `json:"public_rtp"`
	PublicRTCP     *Endpoint `json:"public_rtcp"`
	LeaseTTLMS     int       `json:"lease_ttl_ms"`
	AttachTTLMS    int       `json:"attach_ttl_ms"`
	ConfigRevision Decimal   `json:"config_revision"`
	WireMode       string    `json:"wire_mode"`
	Direction      string    `json:"direction"`
}
type MediaOpen struct {
	OperationID string    `json:"operation_id"`
	Spec        MediaSpec `json:"spec"`
}
type MediaOpenResult struct {
	Lease Lease `json:"lease"`
}
type MediaConnect struct {
	OperationID string    `json:"operation_id"`
	SessionID   string    `json:"session_id"`
	Target      Endpoint  `json:"target"`
	RTCPTarget  *Endpoint `json:"rtcp_target,omitempty"`
}
type MediaConnectResult struct {
	SessionID string `json:"session_id"`
	Connected bool   `json:"connected"`
}
type MediaClose struct {
	OperationID string `json:"operation_id"`
	SessionID   string `json:"session_id"`
	Reason      string `json:"reason"`
}
type MediaCloseResult struct {
	SessionID      string `json:"session_id"`
	Closed         bool   `json:"closed"`
	CleanupPending bool   `json:"cleanup_pending"`
}
type MediaReady struct {
	SessionID string `json:"session_id"`
}
type MediaFailed struct {
	SessionID string `json:"session_id"`
	Error     Error  `json:"error"`
}

type ResourceRef struct {
	ResourceType    string  `json:"resource_type"`
	ID              string  `json:"id"`
	Generation      string  `json:"generation"`
	ConnectionEpoch Decimal `json:"connection_epoch"`
}
type ResourceRecord struct {
	Ref             ResourceRef `json:"ref"`
	OperationID     *string     `json:"operation_id"`
	SubjectType     string      `json:"subject_type"`
	SubjectID       string      `json:"subject_id"`
	BindingRevision Decimal     `json:"binding_revision"`
	ConfigRevision  Decimal     `json:"config_revision"`
	State           string      `json:"state"`
	RemainingTTLMS  int         `json:"remaining_ttl_ms"`
	BytesReceived   Decimal     `json:"bytes_received"`
	BytesSent       Decimal     `json:"bytes_sent"`
	LocalEOF        bool        `json:"local_eof"`
	RemoteEOF       bool        `json:"remote_eof"`
}
type ResourcesList struct {
	ResourceType string  `json:"resource_type"`
	Cursor       *string `json:"cursor"`
	Size         int     `json:"size"`
}
type ResourcesResult struct {
	Items      []ResourceRecord `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}
type ResourceClose struct {
	OperationID string      `json:"operation_id"`
	Ref         ResourceRef `json:"ref"`
	Reason      string      `json:"reason"`
}
type ResourceCloseResult struct {
	Ref            ResourceRef `json:"ref"`
	Closed         bool        `json:"closed"`
	CleanupPending bool        `json:"cleanup_pending"`
}

type AttachmentIssue struct {
	OperationID  string `json:"operation_id"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}
type AttachmentResult struct {
	Ticket      string `json:"ticket"`
	ExpiresInMS int    `json:"expires_in_ms"`
}
