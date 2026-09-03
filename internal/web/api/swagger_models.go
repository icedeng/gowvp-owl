package api

import (
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs"
)

// SwaggerMessageResponse 是通用成功响应。
type SwaggerMessageResponse struct {
	Msg string `json:"msg" example:"ok"` // 处理结果说明
}

// SwaggerErrorResponse 是通用错误响应。
type SwaggerErrorResponse struct {
	Msg string `json:"msg" example:"参数错误"` // 错误信息
}

// SwaggerCascadeStatusesResponse 是上级平台级联注册状态列表。
type SwaggerCascadeStatusesResponse struct {
	Items []gbs.CascadePlatformStatus `json:"items"`
}

// SwaggerDevicesResponse 是设备列表响应。
type SwaggerDevicesResponse struct {
	Items []*ipc.Device `json:"items"`
	Total int64         `json:"total"`
}

// SwaggerChannelsResponse 是通道列表响应。
type SwaggerChannelsResponse struct {
	Items []*ipc.Channel `json:"items"`
	Total int64          `json:"total"`
}

// SwaggerMediaServersResponse 是流媒体服务器列表响应。
type SwaggerMediaServersResponse struct {
	Items []*sms.MediaServer `json:"items"` // 流媒体服务器列表
	Total int64              `json:"total"` // 总记录数
}

// SwaggerEventsResponse 是事件列表响应。
type SwaggerEventsResponse struct {
	Items []*event.Event `json:"items"` // 事件列表
	Total int64          `json:"total"` // 总记录数
}

// SwaggerRecordingsResponse 是录像列表响应。
type SwaggerRecordingsResponse struct {
	Items []*recording.Recording `json:"items"` // 录像列表
	Total int64                  `json:"total"` // 总记录数
}

// SwaggerTimelineResponse 是录像时间轴响应。
type SwaggerTimelineResponse struct {
	Items []recording.TimeRange `json:"items"` // 时间轴片段列表
}

// SwaggerPlayResponse 是播放响应。
type SwaggerPlayResponse struct {
	App    string               `json:"app" example:"rtp"`          // 流所在应用名
	Stream string               `json:"stream" example:"340200..."` // 流 ID
	Items  []sms.StreamLiveAddr `json:"items"`                      // 可直接播放的多协议地址
}

// SwaggerConfigInfoOutput 是系统配置摘要响应。
type SwaggerConfigInfoOutput struct {
	SIP        SwaggerSIPConfig `json:"sip"`         // 当前系统使用的 SIP 配置；对端密码和摘要 seed 已脱敏
	SIPSecrets SIPSecretStatus  `json:"sip_secrets"` // 已脱敏密钥的“是否配置”状态
	AccessInfo SIPAccessInfo    `json:"access_info"` // 设备接入所需的精简地址信息
}

// SwaggerSIPConfig 是 Swagger 友好的 SIP 配置模型。
type SwaggerSIPConfig struct {
	Host                    string                            `json:"host" example:"192.0.2.20"`                                            // 对设备和上级宣告的 SIP 地址
	Port                    int                               `json:"port" example:"5060"`                                                  // SIP TCP/UDP 监听端口
	ID                      string                            `json:"id" example:"34020000002000000001"`                                    // 平台 20 位国标编码
	Domain                  string                            `json:"domain" example:"3402000000"`                                          // SIP 域
	Password                string                            `json:"password" example:"123456"`                                            // 注册鉴权密码
	EnableTLS               bool                              `json:"enable_tls" example:"false"`                                           // 是否启用 SIP-TLS
	TLSPort                 int                               `json:"tls_port" example:"5061"`                                              // SIP-TLS 监听端口
	TLSCert                 string                            `json:"tls_cert" example:"configs/certs/sip.crt"`                             // TLS 证书路径
	TLSKey                  string                            `json:"tls_key" example:"configs/certs/sip.key"`                              // TLS 私钥路径
	TLSClientCA             string                            `json:"tls_client_ca" example:"configs/certs/client-ca.crt"`                  // TLS 客户端 CA 路径
	TLSRequireClientCert    bool                              `json:"tls_require_client_cert" example:"false"`                              // 是否强制校验客户端证书
	RegisterRedirect        string                            `json:"register_redirect" example:"sip:34020000002000000001@192.0.2.30:5060"` // 2022 REGISTER 重定向目标；为空表示不重定向
	RegisterCertificateAuth SwaggerSIPRegisterCertificateAuth `json:"register_certificate_auth"`                                            // Capability/Asymmetric 数字证书 REGISTER 认证
	StrictSourceCheck       bool                              `json:"strict_source_check" example:"true"`                                   // 是否严格校验源 IP
	RequireMessageAuth      bool                              `json:"require_message_auth" example:"false"`                                 // 是否要求 MESSAGE/NOTIFY 做 Digest 鉴权
	PTZWeakConfirm          bool                              `json:"ptz_weak_confirm" example:"false"`                                     // 是否启用级联有应答控制弱确认
	SignalDigest            SwaggerSIPSignalDigest            `json:"signal_digest"`                                                        // Date+Note 信令数字摘要配置
	DeviceHistory           SwaggerDeviceHistoryConfig        `json:"device_history"`                                                       // 设备注册与心跳历史保留策略
	DirectTCPDownload       SwaggerDirectTCPDownloadConfig    `json:"direct_tcp_download"`                                                  // 2014 附录 O 裸 TCP 下载配置
	AnnexG                  SwaggerSIPAnnexG                  `json:"annex_g"`                                                              // 2011/2014/2016 附录 G 外部系统接入配置
	AlarmReceivers          []SwaggerSIPAlarmReceiver         `json:"alarm_receivers"`                                                      // 9.4 本域已注册接警 SIP 客户端；默认关闭
	Upstreams               []SwaggerSIPUpstream              `json:"upstreams"`                                                            // 上级 GB/T 28181 平台配置
	Log                     SwaggerSIPLog                     `json:"log"`                                                                  // SIP 报文日志配置；更新时可省略以保留当前值
	SecretClears            SIPSecretClearInput               `json:"secret_clears"`                                                        // 显式清除已脱敏密钥；空密钥字段默认保留原值
}

type SwaggerSIPRegisterCertificateAuth struct {
	Enabled            bool              `json:"enabled" example:"false"`
	Required           bool              `json:"required" example:"false"`
	PlatformCert       string            `json:"platform_cert" example:"configs/certs/gb-platform.crt"`
	PlatformKey        string            `json:"platform_key" example:"configs/certs/gb-platform.key"`
	DeviceCA           string            `json:"device_ca" example:"configs/certs/gb-device-ca.crt"`
	CRL                string            `json:"crl" example:"configs/certs/gb-device.crl"`
	DeviceCertificates map[string]string `json:"device_certificates"`
}

type SwaggerSIPSignalDigest struct {
	Enabled         bool   `json:"enabled" example:"false"`
	Required        bool   `json:"required" example:"false"` // 强制模式；同时启用出站签名并拒绝缺失或验签失败的入站报文
	Seed            string `json:"seed" example:""`          // GET 时脱敏为空；PUT 非空替换，空值保留，清除使用 secret_clears
	Algorithm       string `json:"algorithm" example:"MD5"`  // MD5、SHA-1、SHA-256 或 SM3
	Encoding        string `json:"encoding" example:"base64"`
	AcceptLegacyHex bool   `json:"accept_legacy_hex" example:"true"`
	Window          int64  `json:"window" example:"600000000000"` // 纳秒
}

type SwaggerDeviceHistoryConfig struct {
	MaxRecords int `json:"max_records" example:"1000"`
	MaxDays    int `json:"max_days" example:"30"`
}

type SwaggerSIPAnnexG struct {
	Enabled        bool                     `json:"enabled" example:"false"`
	MaxSendRecords int                      `json:"max_send_records" example:"100"`
	InboundRate    int                      `json:"inbound_rate" example:"50"`
	InboundBurst   int                      `json:"inbound_burst" example:"100"`
	PendingTTL     int64                    `json:"pending_ttl" example:"86400000000000"` // 纳秒
	MaxPending     int                      `json:"max_pending" example:"4096"`
	Systems        []SwaggerSIPAnnexGSystem `json:"systems"`
}

type SwaggerSIPAnnexGSystem struct {
	ID                     string   `json:"id" example:"34030000002000000001"`
	Role                   string   `json:"role" example:"emergency_command_system"`
	Version                string   `json:"version" example:"1.0"`
	Password               string   `json:"password" example:""`           // GET 时脱敏；PUT 非空替换
	SignalDigestSeed       string   `json:"signal_digest_seed" example:""` // GET 时脱敏；PUT 非空替换
	Realm                  string   `json:"realm" example:"3403000000"`
	Address                string   `json:"address" example:"ecs.example:5061"`
	Transport              string   `json:"transport" example:"tls"`
	SourceCIDRs            []string `json:"source_cidrs" example:"192.0.2.0/24"`
	AllowInsecureTransport bool     `json:"allow_insecure_transport" example:"false"`
	TLSCA                  string   `json:"tls_ca" example:"configs/certs/ecs-ca.crt"`
	TLSCRL                 string   `json:"tls_crl" example:"configs/certs/ecs.crl"`
	TLSServerName          string   `json:"tls_server_name" example:"ecs.example"`
	TLSCert                string   `json:"tls_cert" example:"configs/certs/annex-g-client.crt"`
	TLSKey                 string   `json:"tls_key" example:"configs/certs/annex-g-client.key"`
}

type SwaggerSIPAlarmReceiver struct {
	Name      string   `json:"name" example:"dispatch-center"`
	Enabled   bool     `json:"enabled" example:"false"`
	DeviceID  string   `json:"device_id" example:"34020000002000000011"`
	SourceIDs []string `json:"source_ids" example:"34020000001320000001"`
}

type SwaggerSIPUpstream struct {
	Name                    string                                    `json:"name" example:"provincial"`
	Enabled                 bool                                      `json:"enabled" example:"true"`
	ServerID                string                                    `json:"server_id" example:"34020000002000000001"`
	Domain                  string                                    `json:"domain" example:"3402000000"`
	Host                    string                                    `json:"host" example:"192.0.2.30"`
	Port                    int                                       `json:"port" example:"5060"`
	Transport               string                                    `json:"transport" example:"tls"`
	TLSCA                   string                                    `json:"tls_ca" example:"configs/certs/upstream-ca.crt"`
	TLSCert                 string                                    `json:"tls_cert" example:"configs/certs/cascade-client.crt"`
	TLSKey                  string                                    `json:"tls_key" example:"configs/certs/cascade-client.key"`
	TLSServerName           string                                    `json:"tls_server_name" example:"sip.example.com"`
	LocalID                 string                                    `json:"local_id" example:"34020000002000000002"`
	LocalDomain             string                                    `json:"local_domain" example:"3402000000"`
	LocalHost               string                                    `json:"local_host" example:"192.0.2.20"`
	LocalPort               int                                       `json:"local_port" example:"5061"`
	Password                string                                    `json:"password" example:""` // GET 时脱敏；PUT 非空替换
	RegisterCertificateAuth SwaggerSIPUpstreamRegisterCertificateAuth `json:"register_certificate_auth"`
	SignalDigestSeed        string                                    `json:"signal_digest_seed" example:""` // GET 时脱敏；PUT 非空替换
	MonitorUserIdentity     SwaggerSIPMonitorUserIdentity             `json:"monitor_user_identity"`
	Version                 string                                    `json:"version" example:"1.1"`
	Expires                 int                                       `json:"expires" example:"3600"`
	KeepaliveInterval       int64                                     `json:"keepalive_interval" example:"60000000000"` // 纳秒，与配置 API 的 Duration 数值一致
	AlarmDispatchEnabled    bool                                      `json:"alarm_dispatch_enabled" example:"false"`
	SharedChannels          []string                                  `json:"shared_channels"`
	ChannelIDMap            map[string]string                         `json:"channel_id_map"`
	MediaAllowedCIDRs       []string                                  `json:"media_allowed_cidrs" example:"198.51.100.0/24"`
}

type SwaggerSIPMonitorUserIdentity struct {
	Enabled              bool     `json:"enabled" example:"false"`
	Required             bool     `json:"required" example:"false"`
	LocalGatewayID       string   `json:"local_gateway_id" example:"34020000002110000001"`
	RemoteGatewayID      string   `json:"remote_gateway_id" example:"34030000002110000001"`
	LocalUserID          string   `json:"local_user_id" example:"34020000003000000001"`
	LocalOrganization    string   `json:"local_organization" example:"340200"`
	LocalCategory        string   `json:"local_category" example:"operator"`
	LocalRank            string   `json:"local_rank" example:"level1"`
	TrustedGatewayIDs    []string `json:"trusted_gateway_ids"`
	AllowedUserIDs       []string `json:"allowed_user_ids"`
	AllowedOrganizations []string `json:"allowed_organizations"`
	AllowedCategories    []string `json:"allowed_categories"`
	AllowedRanks         []string `json:"allowed_ranks"`
	MaxHops              int      `json:"max_hops" example:"8"`
}

type SwaggerSIPUpstreamRegisterCertificateAuth struct {
	Enabled    bool   `json:"enabled" example:"false"`
	Required   bool   `json:"required" example:"false"`
	LocalCert  string `json:"local_cert" example:"configs/certs/cascade-register.crt"`
	LocalKey   string `json:"local_key" example:"configs/certs/cascade-register.key"`
	ServerCert string `json:"server_cert" example:"configs/certs/upstream-register.crt"`
	ServerCA   string `json:"server_ca" example:"configs/certs/upstream-register-ca.crt"`
	CRL        string `json:"crl" example:"configs/certs/upstream-register.crl"`
}

type SwaggerSIPLog struct {
	Enabled      bool   `json:"enabled" example:"false"`
	Dir          string `json:"dir" example:"./logs/sip"`
	MaxAge       int64  `json:"max_age" example:"604800000000000"`      // 纳秒
	RotationTime int64  `json:"rotation_time" example:"43200000000000"` // 纳秒
	RotationSize int64  `json:"rotation_size" example:"50"`
}

// SwaggerDirectTCPDownloadConfig 是 2014 裸 TCP 下载配置。
type SwaggerDirectTCPDownloadConfig struct {
	Enabled              bool     `json:"enabled" example:"false"`
	CascadeRelayEnabled  bool     `json:"cascade_relay_enabled" example:"false"`
	DeviceAllowlist      []string `json:"device_allowlist"`
	StorageDir           string   `json:"storage_dir" example:"./configs/downloads/gb28181"`
	RetainDays           int      `json:"retain_days" example:"7"`
	OfferPort            int      `json:"offer_port" example:"9"`
	RelayListenIP        string   `json:"relay_listen_ip" example:"0.0.0.0"`
	RelayAdvertiseIP     string   `json:"relay_advertise_ip" example:"192.0.2.10"`
	RelayPortStart       int      `json:"relay_port_start" example:"30200"`
	RelayPortEnd         int      `json:"relay_port_end" example:"30300"`
	MaxFileSize          int64    `json:"max_file_size" example:"10737418240"`
	GlobalConcurrency    int      `json:"global_concurrency" example:"4"`
	DeviceConcurrency    int      `json:"device_concurrency" example:"1"`
	DialTimeout          int64    `json:"dial_timeout" example:"5000000000"`        // 纳秒
	FirstByteTimeout     int64    `json:"first_byte_timeout" example:"15000000000"` // 纳秒
	IdleTimeout          int64    `json:"idle_timeout" example:"30000000000"`       // 纳秒
	TotalTimeout         int64    `json:"total_timeout" example:"7200000000000"`    // 纳秒
	AllowAddressMismatch bool     `json:"allow_address_mismatch" example:"false"`
	AllowedAddressCIDRs  []string `json:"allowed_address_cidrs"`
}

// SwaggerDirectTCPDownloadState 是附录 O 下载进度和终态快照。
type SwaggerDirectTCPDownloadState struct {
	SessionID     string `json:"session_id" example:"call-id@example"`
	DeviceID      string `json:"device_id" example:"34020000001320000001"`
	ChannelID     string `json:"channel_id" example:"34020000001320000002"`
	Status        string `json:"status" example:"receiving"`
	Received      int64  `json:"received" example:"1048576"`
	FileSize      int64  `json:"file_size" example:"2097152"`
	FileSizeKnown bool   `json:"file_size_known" example:"true"`
	SizeVerified  bool   `json:"size_verified" example:"false"`
	Output        string `json:"output,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	StartedAt     string `json:"started_at" example:"2026-08-10T19:30:00+08:00"`
	UpdatedAt     string `json:"updated_at" example:"2026-08-10T19:30:05+08:00"`
	CompletedAt   string `json:"completed_at,omitempty"`
	EndReason     string `json:"end_reason,omitempty" example:"size_reached"`
	Error         string `json:"error,omitempty"`
}

// SwaggerHistoryDownloadState 是通道最近一次 RTP 或附录 O 下载状态。
type SwaggerHistoryDownloadState struct {
	Transport       string  `json:"transport" example:"rtp"`
	SessionID       string  `json:"session_id" example:"call-id@example"`
	DeviceID        string  `json:"device_id" example:"34020000001320000001"`
	ChannelID       string  `json:"channel_id" example:"34020000001320000002"`
	Status          string  `json:"status" example:"receiving"`
	Received        int64   `json:"received" example:"1048576"`
	FileSize        int64   `json:"file_size" example:"2097152"`
	FileSizeKnown   bool    `json:"file_size_known" example:"true"`
	BytesSpeed      uint64  `json:"bytes_speed,omitempty" example:"524288"`
	ProgressPercent float64 `json:"progress_percent,omitempty" example:"50"`
	ProgressKnown   bool    `json:"progress_known,omitempty" example:"true"`
	Approximate     bool    `json:"approximate,omitempty" example:"true"`
	SizeVerified    bool    `json:"size_verified,omitempty" example:"false"`
	Output          string  `json:"output,omitempty"`
	SHA256          string  `json:"sha256,omitempty"`
	StartedAt       string  `json:"started_at" example:"2026-08-10T19:30:00+08:00"`
	UpdatedAt       string  `json:"updated_at" example:"2026-08-10T19:30:05+08:00"`
	CompletedAt     string  `json:"completed_at,omitempty"`
	EndReason       string  `json:"end_reason,omitempty" example:"size_reached"`
	Error           string  `json:"error,omitempty"`
}

// SwaggerGBMetricsSnapshot 是 GB28181 灰度发布单调计数器。
type SwaggerGBMetricsSnapshot struct {
	RegisterRequests       uint64 `json:"register_requests"`
	RegisterSuccess        uint64 `json:"register_success"`
	RegisterFailures       uint64 `json:"register_failures"`
	CatalogRequests        uint64 `json:"catalog_requests"`
	CatalogSuccess         uint64 `json:"catalog_success"`
	CatalogTimeouts        uint64 `json:"catalog_timeouts"`
	CatalogPartial         uint64 `json:"catalog_partial"`
	MediaRequests          uint64 `json:"media_requests"`
	MediaSuccess           uint64 `json:"media_success"`
	MediaFailures          uint64 `json:"media_failures"`
	MediaDisconnects       uint64 `json:"media_disconnects"`
	DirectStarted          uint64 `json:"direct_tcp_started"`
	DirectCompleted        uint64 `json:"direct_tcp_completed"`
	DirectFailed           uint64 `json:"direct_tcp_failed"`
	DirectCancelled        uint64 `json:"direct_tcp_cancelled"`
	DirectBytes            uint64 `json:"direct_tcp_bytes"`
	AnnexGRequests         uint64 `json:"annex_g_inbound_requests"`
	AnnexGAccepted         uint64 `json:"annex_g_inbound_accepted"`
	AnnexGRejected         uint64 `json:"annex_g_inbound_rejected"`
	AnnexGRateLimited      uint64 `json:"annex_g_inbound_rate_limited"`
	AnnexGBusinessFailures uint64 `json:"annex_g_business_failures"`
	AnnexGPending          uint64 `json:"annex_g_pending"`
}

// SwaggerLoginKeyOutput 是登录公钥响应。
type SwaggerLoginKeyOutput struct {
	Key string `json:"key"` // Base64 编码的 PEM 公钥，用于前端加密登录数据
}

// SwaggerSnapshotLinkOutput 是快照刷新响应。
type SwaggerSnapshotLinkOutput struct {
	Link     string   `json:"link" example:"http://127.0.0.1:9900/channels/xxx/snapshot?token=xxx"` // 可直接访问的快照地址
	Method   string   `json:"method" example:"ffmpeg"`                                              // 本次快照来源：cache/gb28181_device_upload/ffmpeg/zlm_get_snap/url/none
	Attempts []string `json:"attempts"`                                                             // 抓拍尝试过程，成功走 fallback 时会包含前序失败原因
}

// SwaggerZonesResponse 是区域列表响应。
type SwaggerZonesResponse struct {
	Items []ipc.Zone `json:"items"` // 当前通道配置的检测区域列表
}

// SwaggerAIEnableOutput 是启用 AI 的响应。
type SwaggerAIEnableOutput struct {
	Enabled      bool    `json:"enabled" example:"true"`           // 是否启用成功
	Message      string  `json:"message" example:"camera started"` // AI 服务返回的结果信息
	SourceWidth  uint32  `json:"source_width" example:"1920"`      // 视频源宽度
	SourceHeight uint32  `json:"source_height" example:"1080"`     // 视频源高度
	SourceFps    float32 `json:"source_fps" example:"25"`          // 视频源帧率
}

// SwaggerAIDisableOutput 是停用 AI 的响应。
type SwaggerAIDisableOutput struct {
	Enabled bool   `json:"enabled" example:"false"`    // 是否已关闭
	Message string `json:"message" example:"AI 检测已停止"` // 返回说明
}

// SwaggerRecordModeOutput 是录像模式设置响应。
type SwaggerRecordModeOutput struct {
	ID         string `json:"id"`                           // 通道 ID
	RecordMode string `json:"record_mode" example:"always"` // 当前生效录像模式
}

// SwaggerRecordQueryOutput 是录像目录查询响应。
type SwaggerRecordQueryOutput struct {
	DayTotal   int                        `json:"daynum" example:"2"`   // 有录像的日期数量
	TimeNum    int                        `json:"timenum" example:"6"`  // 录像时间片段总数
	Data       []ipc.RecordDate           `json:"list"`                 // 按日期归类的录像片段
	Incomplete *SwaggerGBIncompleteOutput `json:"incomplete,omitempty"` // 设备只返回部分分包时的完成度
}

// SwaggerPTZControlInput 是 PTZ 控制请求体。
type SwaggerPTZControlInput struct {
	Action  string `json:"action" example:"left"` // PTZ 动作名，如 left/right/up/down/zoom_in/preset_call
	Speed   uint8  `json:"speed" example:"30"`    // 速度值，具体范围由协议适配器决定
	Timeout int    `json:"timeout" example:"5"`   // 动作持续时间，单位秒；部分动作可忽略
	Preset  int    `json:"preset" example:"1"`    // 预置位编号，用于预置位相关动作
	Group   uint8  `json:"group" example:"1"`     // 巡航组编号
	Aux     uint8  `json:"aux" example:"1"`       // 辅助开关编号
	Value   uint16 `json:"value" example:"50"`    // 通用附加值，用于聚焦/光圈/扫描等扩展动作
}

// SwaggerPTZProbeInput 是 PTZ 能力探测请求体。
type SwaggerPTZProbeInput struct {
	Action  string `json:"action" example:"stop"` // 探测动作，默认使用 stop
	Speed   uint8  `json:"speed" example:"30"`    // 速度值
	Timeout int    `json:"timeout" example:"5"`   // 等待设备应答超时时间，单位秒
}

// SwaggerPTZProbeOutput 是 PTZ 能力探测响应。
type SwaggerPTZProbeOutput struct {
	ChannelID   string `json:"channel_id" example:"GB_34020000001320000001"` // 通道 ID
	PTZCapable  bool   `json:"ptz_capable" example:"true"`                   // 静态或探测后判断的 PTZ 能力
	PTZVerified bool   `json:"ptz_verified" example:"true"`                  // 是否已通过实际命令验证
	VerifiedNow bool   `json:"verified_now" example:"true"`                  // 本次是否探测成功
	Message     string `json:"message" example:"ok"`                         // 探测结果说明
}

// SwaggerPTZBatchProbeOutput 是设备级批量 PTZ 探测响应。
type SwaggerPTZBatchProbeOutput struct {
	DeviceID     string                  `json:"device_id" example:"GB_34020000002000000001"` // 设备 ID
	Total        int                     `json:"total" example:"4"`                           // 总通道数
	SuccessCount int                     `json:"success_count" example:"3"`                   // 探测成功数
	FailedCount  int                     `json:"failed_count" example:"1"`                    // 探测失败数
	Items        []SwaggerPTZProbeOutput `json:"items"`                                       // 每个通道的探测结果
}

// SwaggerGBDragZoomInput 是拉框控制参数。
type SwaggerGBDragZoomInput struct {
	Length    int `json:"length" example:"1920"`     // 图像总长度
	Width     int `json:"width" example:"1080"`      // 图像总宽度
	MidPointX int `json:"mid_point_x" example:"960"` // 拉框中心点 X 坐标
	MidPointY int `json:"mid_point_y" example:"540"` // 拉框中心点 Y 坐标
	LengthX   int `json:"length_x" example:"400"`    // 拉框宽度
	LengthY   int `json:"length_y" example:"300"`    // 拉框高度
}

// SwaggerGBHomePositionInput 是看守位控制参数。
type SwaggerGBHomePositionInput struct {
	Enabled     *int `json:"enabled" example:"1"`      // 是否启用看守位，1 启用，0 关闭
	ResetTime   *int `json:"reset_time" example:"60"`  // 空闲多久后回到看守位，单位秒
	PresetIndex *int `json:"preset_index" example:"1"` // 看守位使用的预置位编号
}

// SwaggerGBPTZPreciseInput 是 PTZ 精准控制参数。
type SwaggerGBPTZPreciseInput struct {
	Pan  *float64 `json:"pan" example:"10.5"` // 精确水平角度
	Tilt *float64 `json:"tilt" example:"5.2"` // 精确垂直角度
	Zoom *float64 `json:"zoom" example:"2.0"` // 精确变倍值
}

// SwaggerGBPTZCmdParamInput 是 PTZCmd 附加参数。
type SwaggerGBPTZCmdParamInput struct {
	PresetName      string `json:"preset_name" example:"大门"`         // 预置位名称
	CruiseTrackName string `json:"cruise_track_name" example:"白天巡航"` // 巡航路线名称
}

type SwaggerGBTargetTrackInput struct {
	Mode       string                  `json:"mode" example:"Manual"`
	DeviceID2  string                  `json:"device_id2" example:"34020000001320000002"`
	TargetArea *SwaggerGBDragZoomInput `json:"target_area"`
}

// SwaggerGBDeviceControlInput 是 GB 统一控制请求体。
type SwaggerGBDeviceControlInput struct {
	TargetID        string                      `json:"target_id" example:"34020000001320000001"` // 目标设备或通道国标编码；为空时默认当前设备
	Action          string                      `json:"action" example:"ptz_cmd"`                 // GB 统一控制动作名
	Timeout         int                         `json:"timeout" example:"5"`                      // 等待设备响应超时时间，单位秒
	ExtraInfo       []string                    `json:"extra_info"`                               // 设备控制普通文本扩展；旧版编码为 Info，2022 编码为 ExtraInfo
	PTZCmd          string                      `json:"ptz_cmd" example:"preset_call"`            // PTZ 动作名或 16 位原始 PTZCmd
	PTZCmdParam     *SwaggerGBPTZCmdParamInput  `json:"ptz_cmd_param"`                            // PTZCmd 扩展参数
	PTZSpeed        uint8                       `json:"ptz_speed" example:"40"`                   // PTZ 速度
	PTZPreset       int                         `json:"ptz_preset" example:"1"`                   // 预置位编号
	PTZGroup        uint8                       `json:"ptz_group" example:"1"`                    // 巡航或扫描组编号
	PTZAux          uint8                       `json:"ptz_aux" example:"1"`                      // 辅助开关编号
	PTZValue        uint16                      `json:"ptz_value" example:"40"`                   // 巡航、扫描或辅助控制值
	ControlPriority *int                        `json:"control_priority" example:"5"`             // 2011/2014 PTZ 控制优先级
	StreamNumber    int                         `json:"stream_number" example:"1"`                // 2022 录像控制码流编号：0 主码流（缺省省略），1/2/... 子码流
	AlarmMethod     string                      `json:"alarm_method" example:"2"`                 // 报警复位方式
	AlarmType       string                      `json:"alarm_type" example:"1"`                   // 报警类型
	DragZoom        *SwaggerGBDragZoomInput     `json:"drag_zoom"`                                // 拉框放大/缩小参数
	HomePosition    *SwaggerGBHomePositionInput `json:"home_position"`                            // 看守位控制参数
	PTZPrecise      *SwaggerGBPTZPreciseInput   `json:"ptz_precise"`                              // 精确 PTZ 参数
	TargetTrack     *SwaggerGBTargetTrackInput  `json:"target_track"`                             // 2022 目标跟踪参数
	SDCardID        int                         `json:"sdcard_id" example:"1"`                    // SD 卡编号
}

// SwaggerGBDeviceQueryInput 是 GB 统一查询请求体。
type SwaggerGBDeviceQueryInput struct {
	TargetID           string `json:"target_id" example:"34020000001320000001"`       // 目标设备或通道国标编码；为空时默认当前设备
	Action             string `json:"action" example:"device_status"`                 // GB 统一查询动作名
	Timeout            int    `json:"timeout" example:"5"`                            // 等待查询应答超时时间，单位秒
	ConfigType         string `json:"config_type" example:"basic_param"`              // 配置查询时的配置类型
	Interval           int    `json:"interval" example:"60"`                          // 订阅或统计类查询的时间间隔
	Number             int    `json:"number" example:"0"`                             // 巡航轨迹编号（0 或 1）
	Start              int64  `json:"start" example:"1710864000"`                     // Catalog、RecordInfo 开始时间，Unix 秒
	End                int64  `json:"end" example:"1710950400"`                       // Catalog、RecordInfo 结束时间，Unix 秒
	FilePath           string `json:"file_path" example:"/record/front-gate.ps"`      // RecordInfo 文件路径过滤条件
	Address            string `json:"address" example:"front-gate"`                   // RecordInfo 录像地址过滤条件
	Secrecy            *int   `json:"secrecy" example:"0"`                            // RecordInfo 涉密属性：0 不涉密，1 涉密
	Type               string `json:"type" example:"all"`                             // RecordInfo 录像类型：time、alarm、manual、all；空值默认 time
	RecorderID         string `json:"recorder_id" example:"recorder-main"`            // RecordInfo 录像触发者标识字符串
	IndistinctQuery    *int   `json:"indistinct_query" example:"0"`                   // 2014+ 录像模糊查询：0 按 To URI 选位置，1 同时检索中心和前端
	StreamNumber       *int   `json:"stream_number" example:"0"`                      // 2022 码流编号：0 主码流，1/2/... 子码流
	AlarmMethod        string `json:"alarm_method" example:"5"`                       // 2022 报警方式过滤条件
	AlarmType          string `json:"alarm_type" example:"2"`                         // 2022 报警类型过滤条件
	StartAlarmPriority string `json:"start_alarm_priority" example:"1"`               // Alarm 查询起始级别，0 为全部
	EndAlarmPriority   string `json:"end_alarm_priority" example:"4"`                 // Alarm 查询终止级别，0 为全部
	StartAlarmTime     string `json:"start_alarm_time" example:"2026-08-25T08:00:00"` // Alarm 查询起始时间
	EndAlarmTime       string `json:"end_alarm_time" example:"2026-08-25T09:00:00"`   // Alarm 查询终止时间
}

// SwaggerGBAppendixA4Output 是附录 A.4 结构化对象。
type SwaggerGBAppendixA4Output struct {
	Type      string            `json:"type" example:"alarmType"`                          // 附录 A.4 扩展对象类型名称
	CmdType   string            `json:"cmd_type,omitempty" example:"Alarm"`                // 来源命令类型
	Path      string            `json:"path,omitempty" example:"/Response/Info/AlarmInfo"` // 在 XML 中的路径
	Fields    map[string]string `json:"fields,omitempty"`                                  // 结构化键值对
	RawXML    string            `json:"raw_xml,omitempty"`                                 // 原始 XML 片段
	UpdatedAt int64             `json:"updated_at,omitempty" example:"1710931200"`         // 最近更新时间
}

// SwaggerGBDeviceQueryOutput 是 GB 查询响应。
type SwaggerGBDeviceQueryOutput struct {
	SN         int                         `json:"sn" example:"12"`                 // 本次查询的命令序列号
	CmdType    string                      `json:"cmd_type" example:"DeviceStatus"` // 应答中的命令类型
	DeviceID   string                      `json:"device_id" example:"340200..."`   // 返回结果对应的设备编码
	Result     string                      `json:"result,omitempty" example:"OK"`   // 设备处理结果
	XML        string                      `json:"xml"`                             // 原始 XML 应答全文
	Data       any                         `json:"data,omitempty"`                  // 协议解析后的主体数据
	AppendixA4 []SwaggerGBAppendixA4Output `json:"appendix_a4,omitempty"`           // 附录 A.4 扩展对象结构化结果
	Incomplete *SwaggerGBIncompleteOutput  `json:"incomplete,omitempty"`            // 多响应查询未收齐时的完成度
}

// SwaggerGBIncompleteOutput 是 Catalog、RecordInfo、ConfigDownload 部分结果说明。
type SwaggerGBIncompleteOutput struct {
	Kind           string   `json:"kind" example:"record_info"`
	Message        string   `json:"message" example:"RecordInfo response incomplete: received 1 of 2"`
	ReceivedCount  int      `json:"received_count,omitempty" example:"1"`
	ExpectedCount  int      `json:"expected_count,omitempty" example:"2"`
	ReceivedConfig []string `json:"received_config,omitempty" example:"BasicParam"`
	MissingConfig  []string `json:"missing_config,omitempty" example:"VideoParamOpt"`
}

// SwaggerGBDeviceConfigInput 是 2014 DeviceConfig 写入请求。
type SwaggerGBDeviceConfigInput struct {
	TargetID            string                          `json:"target_id" example:"34020000001320000001"`
	Timeout             int                             `json:"timeout" example:"8"`
	ExtraInfo           []string                        `json:"extra_info"` // 2022 扩展信息，每项最长 1024 字符
	BasicParam          *SwaggerGBBasicParamInput       `json:"basic_param,omitempty"`
	VideoParamConfig    *SwaggerGBVideoParamConfigInput `json:"video_param_config,omitempty"`
	AudioParamConfig    *SwaggerGBAudioParamConfigInput `json:"audio_param_config,omitempty"`
	SVACEncodeConfig    *SwaggerGBXMLConfigInput        `json:"svac_encode_config,omitempty"`
	SVACDecodeConfig    *SwaggerGBXMLConfigInput        `json:"svac_decode_config,omitempty"`
	VideoParamAttribute *SwaggerGBXMLConfigInput        `json:"video_param_attribute,omitempty"`
	VideoRecordPlan     *SwaggerGBXMLConfigInput        `json:"video_record_plan,omitempty"`
	VideoAlarmRecord    *SwaggerGBXMLConfigInput        `json:"video_alarm_record,omitempty"`
	PictureMask         *SwaggerGBXMLConfigInput        `json:"picture_mask,omitempty"`
	FrameMirror         *SwaggerGBXMLConfigInput        `json:"frame_mirror,omitempty"`
	AlarmReport         *SwaggerGBXMLConfigInput        `json:"alarm_report,omitempty"`
	OSDConfig           *SwaggerGBXMLConfigInput        `json:"osd_config,omitempty"`
	SnapShotConfig      *SwaggerGBSnapshotConfigInput   `json:"snapshot_config,omitempty"`
}

type SwaggerGBBasicParamInput struct {
	Name              string `json:"name" example:"IPC"`
	Expiration        int    `json:"expiration" example:"3600"`
	HeartBeatInterval int    `json:"heartbeat_interval" example:"60"`
	HeartBeatCount    int    `json:"heartbeat_count" example:"3"`
}

type SwaggerGBVideoParamConfigInput struct {
	Items []SwaggerGBVideoParamItemInput `json:"items"`
}

type SwaggerGBVideoParamItemInput struct {
	StreamName   string `json:"stream_name" example:"Stream1"`
	VideoFormat  string `json:"video_format" example:"H.264"`
	Resolution   string `json:"resolution" example:"1920x1080"`
	FrameRate    string `json:"frame_rate" example:"25"`
	BitRateType  string `json:"bit_rate_type" example:"1"`
	VideoBitRate string `json:"video_bit_rate" example:"4096"`
}

type SwaggerGBAudioParamConfigInput struct {
	Items []SwaggerGBAudioParamItemInput `json:"items"`
}

type SwaggerGBAudioParamItemInput struct {
	StreamName   string `json:"stream_name" example:"Stream1"`
	AudioFormat  string `json:"audio_format" example:"G.711"`
	AudioBitRate string `json:"audio_bit_rate" example:"64"`
	SamplingRate string `json:"sampling_rate" example:"8"`
}

type SwaggerGBXMLConfigInput struct {
	InnerXML string `json:"inner_xml" example:"<SVCParam><SVCFlag>1</SVCFlag></SVCParam>"`
}

type SwaggerGBSnapshotConfigInput struct {
	SnapNum   int    `json:"snap_num" example:"1"`
	Interval  int    `json:"interval" example:"1"`
	UploadURL string `json:"upload_url" example:"https://example.com/gb28181/snapshot"`
	SessionID string `json:"session_id" example:"snapshot-session-0000000000000001"`
}

type SwaggerGBDeviceConfigOutput struct {
	SN       int    `json:"sn" example:"12"`
	CmdType  string `json:"cmd_type" example:"DeviceConfig"`
	DeviceID string `json:"device_id" example:"34020000001320000001"`
	Result   string `json:"result,omitempty" example:"OK"`
	RawXML   string `json:"raw_xml,omitempty"`
}

// SwaggerGBAppendixA4SnapshotOutput 是附录 A.4 快照响应。
type SwaggerGBAppendixA4SnapshotOutput struct {
	DeviceID string                      `json:"device_id" example:"340200..."`                 // 当前设备国标编码
	Filter   string                      `json:"filter,omitempty" example:"Alarm,DeviceStatus"` // 生效的过滤条件
	Total    int                         `json:"total" example:"2"`                             // 本次返回条数
	Items    []SwaggerGBAppendixA4Output `json:"items"`                                         // 快照列表
}
