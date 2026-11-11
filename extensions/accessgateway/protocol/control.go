package protocol

// EffectiveConfig 只含 B 可应用的策略，不含 A 的媒体节点、代理和凭据。
type EffectiveConfig struct {
	PublicIP                  string   `json:"public_ip"`
	SIPPort                   int      `json:"sip_port"`
	MediaPortStart            int      `json:"media_port_start"`
	MediaPortEnd              int      `json:"media_port_end"`
	TaskPublicBaseURL         *string  `json:"task_public_base_url"`
	MaxMediaSessions          int      `json:"max_media_sessions"`
	MaxTransferSessions       int      `json:"max_transfer_sessions"`
	MaxToABPS                 Decimal  `json:"max_to_a_bps"`
	MaxToDeviceBPS            Decimal  `json:"max_to_device_bps"`
	HeartbeatIntervalMS       int      `json:"heartbeat_interval_ms"`
	HeartbeatTimeoutMS        int      `json:"heartbeat_timeout_ms"`
	LeaseTTLMS                int      `json:"lease_ttl_ms"`
	MediaConnectTimeoutMS     int      `json:"media_connect_timeout_ms"`
	MediaFirstPacketTimeoutMS int      `json:"media_first_packet_timeout_ms"`
	SourceCIDRs               []string `json:"source_cidrs"`
}

func DefaultEffectiveConfig() EffectiveConfig {
	return EffectiveConfig{MaxMediaSessions: 64, MaxTransferSessions: 8, MaxToABPS: "100000000", MaxToDeviceBPS: "100000000", HeartbeatIntervalMS: 15000, HeartbeatTimeoutMS: 45000, LeaseTTLMS: 60000, MediaConnectTimeoutMS: 10000, MediaFirstPacketTimeoutMS: 30000, SourceCIDRs: []string{}}
}

type Hello struct {
	ClientInstanceID     string   `json:"client_instance_id"`
	SupportedVersions    []int    `json:"supported_versions"`
	RequiredCapabilities []string `json:"required_capabilities"`
	LastAppliedRevision  Decimal  `json:"last_applied_revision"`
}
type HelloAck struct {
	SelectedVersion     int             `json:"selected_version"`
	Capabilities        []string        `json:"capabilities"`
	Config              EffectiveConfig `json:"config"`
	ConfigRevision      Decimal         `json:"config_revision"`
	HeartbeatIntervalMS int             `json:"heartbeat_interval_ms"`
	HeartbeatTimeoutMS  int             `json:"heartbeat_timeout_ms"`
	LeaseTTLMS          int             `json:"lease_ttl_ms"`
	CooldownRemainingMS int             `json:"cooldown_remaining_ms"`
}
type Heartbeat struct {
	HeartbeatSeq     Decimal  `json:"heartbeat_seq"`
	RenewSessionIDs  []string `json:"renew_session_ids"`
	RenewTransferIDs []string `json:"renew_transfer_ids"`
	PartIndex        int      `json:"part_index,omitempty"`
	PartCount        int      `json:"part_count,omitempty"`
}
type ResourceTraffic struct {
	Ref        ResourceRef `json:"ref"`
	RXBytes    Decimal     `json:"rx_bytes"`
	TXBytes    Decimal     `json:"tx_bytes"`
	QueueBytes Decimal     `json:"queue_bytes"`
}

type HeartbeatAck struct {
	Traffic            []ResourceTraffic `json:"traffic,omitempty"`
	HeartbeatSeq       Decimal           `json:"heartbeat_seq"`
	RenewedSessionIDs  []string          `json:"renewed_session_ids"`
	RenewedTransferIDs []string          `json:"renewed_transfer_ids"`
	RejectedIDs        []string          `json:"rejected_ids"`
	DrainState         string            `json:"drain_state"`
	PartIndex          int               `json:"part_index,omitempty"`
	PartCount          int               `json:"part_count,omitempty"`
}
type ConfigPrepare struct {
	OperationID      string          `json:"operation_id"`
	ExpectedRevision Decimal         `json:"expected_revision"`
	DesiredRevision  Decimal         `json:"desired_revision"`
	Config           EffectiveConfig `json:"config"`
	ConfigSHA256     string          `json:"config_sha256"`
}
type ConfigPrepared struct {
	PrepareID     string `json:"prepare_id"`
	PrepareTTLMS  int    `json:"prepare_ttl_ms"`
	RequiresDrain bool   `json:"requires_drain"`
}
type ConfigApply struct {
	OperationID     string  `json:"operation_id"`
	PrepareID       string  `json:"prepare_id"`
	DesiredRevision Decimal `json:"desired_revision"`
	ConfigSHA256    string  `json:"config_sha256"`
}
type ConfigResult struct {
	AppliedRevision Decimal `json:"applied_revision"`
	ConfigSHA256    string  `json:"config_sha256"`
	Applied         bool    `json:"applied"`
}
type Drain struct {
	OperationID string `json:"operation_id"`
	DeadlineMS  int    `json:"deadline_ms"`
	Reason      string `json:"reason"`
}
type DrainResult struct {
	DrainState      string `json:"drain_state"`
	ActiveResources int    `json:"active_resources"`
}
type Resume struct {
	OperationID    string  `json:"operation_id"`
	ConfigRevision Decimal `json:"config_revision"`
}
type ResumeResult struct {
	DrainState string `json:"drain_state"`
}
type Probe struct {
	Version             int      `json:"version"`
	GatewayID           string   `json:"gateway_id"`
	PlatformID          string   `json:"platform_id"`
	Generation          string   `json:"generation"`
	ConfigRevision      Decimal  `json:"config_revision"`
	Capabilities        []string `json:"capabilities"`
	ConfigSHA256        string   `json:"config_sha256"`
	OwnerConnected      bool     `json:"owner_connected"`
	CooldownRemainingMS int      `json:"cooldown_remaining_ms"`
}
type StatusSnapshot struct {
	SampleSeq      Decimal `json:"sample_seq"`
	SampledAt      string  `json:"sampled_at"`
	SIPUDPBound    bool    `json:"sip_udp_bound"`
	SIPTCPBound    bool    `json:"sip_tcp_bound"`
	MediaReady     bool    `json:"media_ready"`
	ConfigRevision Decimal `json:"config_revision"`
	DrainState     string  `json:"drain_state"`
	PortTotal      int     `json:"port_total"`
	PortUsed       int     `json:"port_used"`
	CleanupPending int     `json:"cleanup_pending"`
}
type MetricsSnapshot struct {
	SampleSeq        Decimal  `json:"sample_seq"`
	SampledAt        string   `json:"sampled_at"`
	UptimeMS         int      `json:"uptime_ms"`
	CPUPercent       *float64 `json:"cpu_percent"`
	RSSBytes         *Decimal `json:"rss_bytes"`
	SIPFlows         int      `json:"sip_flows"`
	MediaSessions    int      `json:"media_sessions"`
	TransferSessions int      `json:"transfer_sessions"`
	DeviceRXBytes    *Decimal `json:"device_rx_bytes"`
	DeviceTXBytes    *Decimal `json:"device_tx_bytes"`
	WSSRXBytes       *Decimal `json:"wss_rx_bytes"`
	WSSTXBytes       *Decimal `json:"wss_tx_bytes"`
	QueueBytes       *Decimal `json:"queue_bytes"`
}
