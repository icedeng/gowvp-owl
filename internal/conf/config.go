package conf

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

type Bootstrap struct {
	Debug        bool   `toml:"-" json:"-"`
	BuildVersion string `toml:"-" json:"-"`
	ConfigDir    string `toml:"-" json:"-"`
	ConfigPath   string `toml:"-" json:"-"`
	// AISecret 进程内随机 UUID，用于 Python AI 回调鉴权，重启后失效，不写配置文件
	AISecret string `toml:"-" json:"-"`

	Server Server // 服务器
	Data   Data   // 数据
	Log    Log    // 日志
	Sip    SIP
	Media  Media // 媒体
}

type Server struct {
	Debug      bool
	RTMPSecret string `comment:"rtmp 推流秘钥"`

	Username string `comment:"登录用户名"`
	Password string `comment:"登录密码"`

	AI        ServerAI        `comment:"ai 分析服务"`
	HTTP      ServerHTTP      `comment:"对外提供的服务，建议由 nginx 代理"` // HTTP服务器
	Recording ServerRecording `comment:"录像配置"`
	Webhook   ServerWebhook   `comment:"告警 webhook 推送与接收配置"`
}

// ServerWebhook webhook 推送与接收配置
type ServerWebhook struct {
	// Targets 推送目标 URL 数组，secret 直接内嵌于 URL query 参数
	// 例如: ["http://192.168.1.20:15123/webhook/event?secret=abc123"]
	Targets    []string `comment:"推送目标 URL 数组，secret 直接内嵌于 URL query 参数，如 http://host/webhook/event?secret=xxx"`
	MaxRetry   int      `comment:"推送最大重试次数，0=内置默认3次"`
	BufferSize int      `comment:"每个目标的 channel 缓冲队列大小，0=内置默认64，满队列时新事件被丢弃并记录警告"`
	// RecvSecret 本节点接收 webhook 时校验的密钥，首次启动若为空则自动生成随机值并持久化
	RecvSecret string `comment:"本节点接收 webhook 事件时校验的密钥，发送方在 URL query 参数 secret 中携带此值"`
}

// ServerRecording 录像配置，控制流媒体录制行为和存储策略
// 默认所有录制均开启，通过 Disabled 字段关闭特定类型的录制
type ServerRecording struct {
	Disabled           bool    `comment:"是否禁用录制（全局开关，true=禁用）"`
	DefaultMode        string  `comment:"通道未设置时的默认录像模式：always/ai/none"`
	StorageDir         string  `comment:"录像存储根目录（相对于工作目录）"`
	RetainDays         int     `comment:"录像保留天数（超过则清理）"`
	DiskUsageThreshold float64 `comment:"磁盘使用率阈值（百分比），超过则触发循环覆盖"`
	SegmentSeconds     int     `comment:"MP4 切片时长（秒）"`
}

type ServerAI struct {
	Disabled   bool `comment:"是否禁用 ai 分析服务"`
	RetainDays int  `comment:"保留天数"`
	// 全局默认分析间隔（秒）。0=内置默认5秒；0.5=每秒2张；1=每秒1张；5=每5秒1张；30=每30秒1张
	AnalysisInterval float32 `comment:"全局默认分析间隔（秒）"`
	// 告警冷却时间（秒）。同一目标在冷却期内不重复告警。0=内置默认30秒；10=10秒；60=1分钟
	AlertCooldownSeconds float32 `comment:"告警冷却时间（秒）"`
}

type ServerHTTP struct {
	Port      int         `comment:"http 端口"`                // 服务器端口号
	Timeout   Duration    `comment:"请求超时时间"`                 // 请求超时时间
	JwtSecret string      `comment:"jwt 秘钥，空串时，每次启动程序将随机赋值"` // JWT密钥
	PProf     ServerPPROF // Pprof配置
	AuthURL   string      `comment:"第三方认证服务地址，空串则不启用，post 请求返回 200 表示认证通过，填写本服务 /health 表示免鉴权但不安全!"`
}

// ServerPPROF 结构体，包含 Enabled 和 AccessIps 两个字段
type ServerPPROF struct {
	Enabled   bool     `comment:"是否启用 pprof, 建议设置为 true"`  // 是否启用
	AccessIps []string `comment:"访问白名单" json:"access_ips"` // 允许访问的IP地址列表
}

// Data 结构体，包含 Database 和 Redis 两个字段
type Data struct {
	// Database 数据库
	Database Database `comment:"数据库支持 sqlite/postgres/mysql, 使用 sqlite 时 dsn 应当填写文件存储路径"`
	// Redis Redis数据库
	// Redis DataRedis
}

// Database 结构体，包含 Dsn、MaxIdleConns、MaxOpenConns、ConnMaxLifetime 和 SlowThreshold 五个字段
type Database struct {
	Dsn             string   // 数据源名称
	MaxIdleConns    int32    // 最大空闲连接数
	MaxOpenConns    int32    // 最大打开连接数
	ConnMaxLifetime Duration // 连接最大生命周期
	SlowThreshold   Duration // 慢查询阈值
}

// Log 结构体，包含 Dir、Level、MaxAge、RotationTime 和 RotationSize 五个字段
type Log struct {
	Dir     string   `comment:"日志存储目录，不能使用特殊符号"`
	Level   string   `comment:"记录级别 debug/info/warn/error"`
	MaxAge  Duration `comment:"弃用，请使用 MaxDays 替代"`
	MaxDays int      `comment:"保留日志天数，超过时间自动删除"`
	MaxSize int      `comment:"日志文件最大大小(MB)"`

	RotationTime Duration `comment:"多久时间，分割一个新的日志文件"`
}

type SIP struct {
	Host     string `comment:"对设备宣告的本机地址(可选), 为空时按连接来源自动探测, 探测不可达时回退到 Media.SDPIP" json:"host"`
	Port     int    `comment:"服务监听的 tcp/udp 端口号" json:"port"`
	ID       string `comment:"gb/t28181 20 位国标 ID" json:"id"`
	Domain   string `comment:"域" json:"domain"`
	Password string `comment:"注册密码" json:"password"`

	EnableTLS            bool   `comment:"是否启用 SIP-TLS 监听" json:"enable_tls"`
	TLSPort              int    `comment:"SIP-TLS 监听端口，0 表示与 port 相同" json:"tls_port"`
	TLSCert              string `comment:"SIP-TLS 证书文件路径" json:"tls_cert"`
	TLSKey               string `comment:"SIP-TLS 私钥文件路径" json:"tls_key"`
	TLSClientCA          string `comment:"用于校验 SIP-TLS 客户端证书的 CA 文件路径" json:"tls_client_ca,omitempty"`
	TLSRequireClientCert bool   `comment:"是否要求 SIP-TLS 客户端必须提交有效证书" json:"tls_require_client_cert"`

	StrictSourceCheck       bool                       `comment:"是否校验设备上报源IP与注册源IP一致" json:"strict_source_check"`
	RequireMessageAuth      bool                       `comment:"是否要求 MESSAGE/NOTIFY 携带 Digest 鉴权" json:"require_message_auth"`
	PTZWeakConfirm          bool                       `comment:"是否启用级联有应答控制弱确认；下级已返回 SIP 成功但未返回 DeviceControl 业务应答时按成功处理" json:"ptz_weak_confirm"`
	RegisterRedirect        string                     `comment:"GB/T 28181-2022 注册重定向目标 SIP URI；为空表示不重定向" json:"register_redirect,omitempty"`
	RegisterCertificateAuth SIPRegisterCertificateAuth `comment:"GB/T 28181-2011/2014/2016 Capability/Asymmetric 数字证书 REGISTER 认证" json:"register_certificate_auth"`
	SignalDigest            SIPSignalDigest            `comment:"GB/T 28181 Date+Note 信令数字摘要" json:"signal_digest"`
	DeviceHistory           DeviceHistoryConfig        `comment:"设备心跳与注册历史保留策略" json:"device_history"`
	DirectTCPDownload       SIPDirectTCPDownload       `comment:"GB/T 28181-2014 附录 O 裸 TCP 文件下载" json:"direct_tcp_download"`
	AnnexG                  SIPAnnexG                  `comment:"GB/T 28181-2011/2016 附录 G 外部系统接入；默认关闭" json:"annex_g"`
	AlarmReceivers          []SIPAlarmReceiver         `comment:"GB/T 28181 9.4 本域已注册接警 SIP 客户端；默认关闭" json:"alarm_receivers,omitempty"`
	Upstreams               []SIPUpstream              `comment:"上下级平台级联：本平台作为下级注册到上级平台" json:"upstreams,omitempty"`
	Log                     SIPLog                     `json:"log"`
}

// SIPAlarmReceiver 描述一个由本平台受理 REGISTER、可接收 9.4 报警分发的 SIP 客户端。
type SIPAlarmReceiver struct {
	Name      string   `json:"name" comment:"接警终端配置名称，必须唯一"`
	Enabled   bool     `json:"enabled" comment:"是否启用报警分发；默认关闭"`
	DeviceID  string   `json:"device_id" comment:"接警终端已注册的 20 位国标编码"`
	SourceIDs []string `json:"source_ids,omitempty" comment:"允许分发给该终端的报警设备、通道或 10 位中心编码；空列表不分发"`
}

// SIPAnnexG 控制规范性附录 G 的外部综合接处警、卡口和城市信息系统接入。
// Owl 当前只作为管理平台；外部系统必须使用静态身份、来源网段和 Digest 密码。
type SIPAnnexG struct {
	Enabled        bool              `comment:"是否启用附录 G 外部系统 SIP MESSAGE 接入" json:"enabled"`
	MaxSendRecords int               `comment:"单次查询响应最多发送记录数；0 使用默认值 100" json:"max_send_records"`
	InboundRate    int               `comment:"每个外部系统每秒允许的入向 MESSAGE 数；0 使用默认值 50" json:"inbound_rate"`
	InboundBurst   int               `comment:"每个外部系统允许的入向 MESSAGE 突发数；0 使用默认值 100" json:"inbound_burst"`
	PendingTTL     Duration          `comment:"主动交换在途关联保留时间；0 使用默认值 24h" json:"pending_ttl"`
	MaxPending     int               `comment:"主动交换最大在途数量；0 使用默认值 4096" json:"max_pending"`
	Systems        []SIPAnnexGSystem `comment:"允许接入的外部系统档案" json:"systems"`
}

// SIPAnnexGSystem 是一个已授权附录 G 外部系统档案。
type SIPAnnexGSystem struct {
	ID                     string   `comment:"外部系统 20 位国标编码" json:"id"`
	Role                   string   `comment:"emergency_command_system、tollgate_system 或 city_information_system" json:"role"`
	Version                string   `comment:"1.0、1.1 或 2.0；2022 已删除附录 G" json:"version"`
	Password               string   `comment:"MESSAGE Digest 共享密码" json:"password"`
	SignalDigestSeed       string   `comment:"与该外部系统约定的 Date+Note 摘要 seed；为空时使用 Password 或 Sip.SignalDigest.Seed" json:"signal_digest_seed,omitempty"`
	Realm                  string   `comment:"外部系统 Digest realm；为空时取外部系统 ID 前 10 位" json:"realm"`
	Address                string   `comment:"平台主动请求使用的外部系统 host:port" json:"address"`
	Transport              string   `comment:"平台主动请求传输：udp、tcp 或 tls；为空默认 tls" json:"transport"`
	SourceCIDRs            []string `comment:"允许的来源 IP 或 CIDR，至少一项" json:"source_cidrs"`
	AllowInsecureTransport bool     `comment:"是否显式允许 UDP/TCP 明文；默认只允许 SIP-TLS" json:"allow_insecure_transport"`
	TLSCA                  string   `comment:"校验外部系统 SIP-TLS 证书的 CA 文件；为空使用系统 CA" json:"tls_ca,omitempty"`
	TLSCRL                 string   `comment:"外部系统 SIP-TLS 证书撤销列表；配置时必须同时设置 TLSCA" json:"tls_crl,omitempty"`
	TLSServerName          string   `comment:"外部系统 SIP-TLS 证书服务端名称；为空使用 address 主机" json:"tls_server_name,omitempty"`
	TLSCert                string   `comment:"可选 SIP-TLS 客户端证书" json:"tls_cert,omitempty"`
	TLSKey                 string   `comment:"可选 SIP-TLS 客户端私钥" json:"tls_key,omitempty"`
}

// ValidateSIPAnnexGConfig 校验附录 G 静态外部系统配置。
func ValidateSIPAnnexGConfig(config SIPAnnexG, localID string, tlsEnabled bool) error {
	if !config.Enabled {
		return nil
	}
	if config.MaxSendRecords < 0 || config.MaxSendRecords > 10000 {
		return fmt.Errorf("附录 G 单次查询记录数应在 0–10000 之间")
	}
	if config.InboundRate < 0 || config.InboundRate > 10000 {
		return fmt.Errorf("附录 G 每系统入向速率应在 0–10000 次/秒之间")
	}
	if config.InboundBurst < 0 || config.InboundBurst > 10000 {
		return fmt.Errorf("附录 G 每系统入向突发量应在 0–10000 之间")
	}
	if pendingTTL := config.PendingTTL.Duration(); pendingTTL != 0 && (pendingTTL < time.Minute || pendingTTL > 7*24*time.Hour) {
		return fmt.Errorf("附录 G 在途关联保留时间应为 1m–168h，0 使用默认值")
	}
	if config.MaxPending < 0 || config.MaxPending > 10000 {
		return fmt.Errorf("附录 G 最大在途数量应在 0–10000 之间")
	}
	if len(config.Systems) == 0 {
		return fmt.Errorf("启用附录 G 时至少配置一个外部系统")
	}
	seen := make(map[string]struct{}, len(config.Systems))
	for index, system := range config.Systems {
		prefix := fmt.Sprintf("附录 G 外部系统[%d]", index)
		id := strings.TrimSpace(system.ID)
		if !isDigitCode(id, 20) {
			return fmt.Errorf("%s ID 必须是 20 位数字", prefix)
		}
		if id == strings.TrimSpace(localID) {
			return fmt.Errorf("%s ID 不能与本平台相同", prefix)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("%s ID 重复", prefix)
		}
		seen[id] = struct{}{}
		switch strings.TrimSpace(system.Role) {
		case "emergency_command_system", "tollgate_system", "city_information_system":
		default:
			return fmt.Errorf("%s role 不受支持", prefix)
		}
		switch strings.TrimSpace(system.Version) {
		case "1.0", "1.1", "2.0", "2011", "2014", "2016", "2011-supplement-2014":
		case "3.0", "2022":
			return fmt.Errorf("%s 使用的 GB/T 28181-2022 已删除附录 G", prefix)
		default:
			return fmt.Errorf("%s version 不受支持", prefix)
		}
		password := system.Password
		if password == "" || password == "#" || len(password) > 128 || strings.ContainsAny(password, "\r\n") {
			return fmt.Errorf("%s password 必须为 1–128 字节、不能使用免鉴权占位符且不能包含换行", prefix)
		}
		signalDigestSeed := system.SignalDigestSeed
		if len(signalDigestSeed) > 128 || strings.ContainsAny(signalDigestSeed, "\r\n") {
			return fmt.Errorf("%s signal_digest_seed 最多 128 字节且不能包含换行", prefix)
		}
		if len(system.SourceCIDRs) == 0 {
			return fmt.Errorf("%s 至少配置一个来源 IP 或 CIDR", prefix)
		}
		for _, source := range system.SourceCIDRs {
			source = strings.TrimSpace(source)
			if net.ParseIP(source) != nil {
				continue
			}
			if _, _, err := net.ParseCIDR(source); err != nil {
				return fmt.Errorf("%s 来源 %q 不是有效 IP 或 CIDR", prefix, source)
			}
		}
		realm := strings.TrimSpace(system.Realm)
		if realm == "" {
			realm = id[:10]
		}
		if !isDigitCode(realm, 10) {
			return fmt.Errorf("%s realm 必须是 10 位数字", prefix)
		}
		host, portText, err := net.SplitHostPort(strings.TrimSpace(system.Address))
		if err != nil || strings.TrimSpace(host) == "" {
			return fmt.Errorf("%s address 必须是有效的 host:port", prefix)
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("%s address 端口应在 1–65535 之间", prefix)
		}
		transport := strings.ToLower(strings.TrimSpace(system.Transport))
		if transport == "" {
			transport = "tls"
		}
		if transport != "udp" && transport != "tcp" && transport != "tls" {
			return fmt.Errorf("%s transport 只支持 udp、tcp 或 tls", prefix)
		}
		if transport != "tls" && !system.AllowInsecureTransport {
			return fmt.Errorf("%s 使用明文 transport 时必须显式允许明文传输", prefix)
		}
		if (strings.TrimSpace(system.TLSCert) == "") != (strings.TrimSpace(system.TLSKey) == "") {
			return fmt.Errorf("%s SIP-TLS 客户端证书和私钥必须同时配置", prefix)
		}
		if strings.TrimSpace(system.TLSCRL) != "" && strings.TrimSpace(system.TLSCA) == "" {
			return fmt.Errorf("%s 配置 SIP-TLS 证书撤销列表时必须同时配置 TLS CA", prefix)
		}
		if !system.AllowInsecureTransport && !tlsEnabled {
			return fmt.Errorf("%s 默认要求 SIP-TLS；请启用 SIP TLS 或显式允许明文传输", prefix)
		}
	}
	return nil
}

// SIPRegisterCertificateAuth 控制 GB/T 28181-2011 9.1.2.2/J.2 定义、
// 2014 修改补充文件继续沿用且 2016 保留的 Capability/Asymmetric 数字证书双向 REGISTER 认证。
// 该证书用途独立于 SIP-TLS。
type SIPRegisterCertificateAuth struct {
	Enabled            bool              `comment:"是否接受 Capability/Asymmetric REGISTER 认证" json:"enabled"`
	Required           bool              `comment:"是否强制所有设备使用数字证书 REGISTER 认证；设置后隐式启用" json:"required"`
	PlatformCert       string            `comment:"平台 X.509 证书文件路径" json:"platform_cert"`
	PlatformKey        string            `comment:"平台 RSA 私钥文件路径" json:"platform_key"`
	DeviceCA           string            `comment:"用于校验设备证书链的 CA 文件路径；为空时按配置证书固定信任" json:"device_ca,omitempty"`
	CRL                string            `comment:"X.509 V2 证书撤销列表文件路径，可包含多个 PEM CRL" json:"crl,omitempty"`
	DeviceCertificates map[string]string `comment:"设备国标 ID 到 X.509 设备证书文件路径的映射" json:"device_certificates"`
}

// Active 返回数字证书 REGISTER 认证是否启用。Required 模式隐式启用，避免出现
// 配置要求强制认证但 Enabled 被遗漏的降级状态。
func (config SIPRegisterCertificateAuth) Active() bool {
	return config.Enabled || config.Required
}

// ValidateRegisterCertificateAuthConfig 校验数字证书 REGISTER 认证的结构化配置。
// 证书、私钥、证书链和 CRL 的密码学校验在 GB28181 服务启动前完成。
func ValidateRegisterCertificateAuthConfig(config SIPRegisterCertificateAuth) error {
	if !config.Active() {
		return nil
	}
	if strings.TrimSpace(config.PlatformCert) == "" {
		return fmt.Errorf("启用数字证书 REGISTER 认证时必须配置平台证书文件")
	}
	if strings.TrimSpace(config.PlatformKey) == "" {
		return fmt.Errorf("启用数字证书 REGISTER 认证时必须配置平台私钥文件")
	}
	if len(config.DeviceCertificates) == 0 {
		return fmt.Errorf("启用数字证书 REGISTER 认证时必须配置至少一个设备证书")
	}
	for deviceID, certificate := range config.DeviceCertificates {
		if !isDigitCode(strings.TrimSpace(deviceID), 20) {
			return fmt.Errorf("数字证书设备 ID %q 必须是 20 位数字", deviceID)
		}
		if strings.TrimSpace(certificate) == "" {
			return fmt.Errorf("数字证书设备 %s 的证书文件不能为空", deviceID)
		}
	}
	if strings.TrimSpace(config.CRL) != "" && strings.TrimSpace(config.DeviceCA) == "" {
		return fmt.Errorf("配置证书撤销列表 CRL 时必须同时配置设备 CA 文件")
	}
	return nil
}

// SIPUpstream 描述一个上级 GB/T 28181 平台的注册参数。
type SIPUpstream struct {
	Name                    string                             `json:"name" comment:"上级平台配置名称，必须唯一"`
	Enabled                 bool                               `json:"enabled" comment:"是否启用级联注册"`
	ServerID                string                             `json:"server_id" comment:"上级平台 20 位国标编码"`
	Domain                  string                             `json:"domain" comment:"上级平台 SIP 域；为空时取 server_id 前 10 位"`
	Host                    string                             `json:"host" comment:"上级平台 SIP 地址"`
	Port                    int                                `json:"port" comment:"上级平台 SIP 端口"`
	Transport               string                             `json:"transport,omitempty" comment:"上级平台 SIP 信令传输：udp/tcp/tls；空值默认 udp"`
	TLSCA                   string                             `json:"tls_ca,omitempty" comment:"TLS 上级服务端证书的 CA 文件；为空时使用系统 CA"`
	TLSCert                 string                             `json:"tls_cert,omitempty" comment:"TLS 客户端证书文件；与 tls_key 同时配置"`
	TLSKey                  string                             `json:"tls_key,omitempty" comment:"TLS 客户端私钥文件；与 tls_cert 同时配置"`
	TLSServerName           string                             `json:"tls_server_name,omitempty" comment:"TLS 服务端证书名称；为空时使用 host"`
	LocalID                 string                             `json:"local_id" comment:"向上级注册使用的本平台国标编码；为空时使用 Sip.ID"`
	LocalDomain             string                             `json:"local_domain" comment:"向上级注册使用的本平台 SIP 域；为空时使用 Sip.Domain 或 local_id 前 10 位"`
	LocalHost               string                             `json:"local_host" comment:"Contact 宣告地址；为空时使用 Sip.Host"`
	LocalPort               int                                `json:"local_port,omitempty" comment:"Contact 宣告端口；空值按传输使用本机 SIP 或 TLS 监听端口"`
	Password                string                             `json:"password" comment:"上级平台注册密码"`
	RegisterCertificateAuth SIPUpstreamRegisterCertificateAuth `json:"register_certificate_auth" comment:"向上级注册时使用 Capability/Asymmetric 数字证书认证"`
	SignalDigestSeed        string                             `json:"signal_digest_seed,omitempty" comment:"与该上级约定的 Note 摘要 seed；为空时使用 Password 或 Sip.SignalDigest.Seed"`
	MonitorUserIdentity     SIPMonitorUserIdentity             `json:"monitor_user_identity" comment:"跨域 Monitor-User-Identity 身份和访问控制策略"`
	Version                 string                             `json:"version" comment:"级联档案版本：1.0/1.1/2.0/3.0"`
	Expires                 int                                `json:"expires" comment:"注册有效期秒数"`
	KeepaliveInterval       Duration                           `json:"keepalive_interval" comment:"心跳间隔"`
	AlarmDispatchEnabled    bool                               `json:"alarm_dispatch_enabled" comment:"是否按 GB/T 28181 9.4 向该上级分发共享通道报警；默认关闭"`
	SharedChannels          []string                           `json:"shared_channels,omitempty" comment:"共享给该上级的本地国标通道编码；空列表表示不共享"`
	ChannelIDMap            map[string]string                  `json:"channel_id_map,omitempty" comment:"本地通道编码到上级可见国标编码的映射"`
	MediaAllowedCIDRs       []string                           `json:"media_allowed_cidrs,omitempty" comment:"除上级信令 IP 外允许接收级联媒体的 IP/CIDR 白名单"`
}

// SIPMonitorUserIdentity 控制 GB/T 28181 8.3/8.5 规定的跨域用户身份头。
// 各上级可以使用独立的安全路由网关和访问控制策略，避免把某个平台的信任关系复用于其他平台。
type SIPMonitorUserIdentity struct {
	Enabled              bool     `json:"enabled" comment:"是否生成并校验 Monitor-User-Identity；Required 会隐式启用"`
	Required             bool     `json:"required" comment:"是否拒绝上级缺失 Monitor-User-Identity 的跨域请求"`
	LocalGatewayID       string   `json:"local_gateway_id" comment:"本地信令安全路由网关 20 位国标编码，类型码必须为 211"`
	RemoteGatewayID      string   `json:"remote_gateway_id" comment:"直接相连上级信令安全路由网关 20 位国标编码，类型码必须为 211"`
	LocalUserID          string   `json:"local_user_id" comment:"本域发起信令使用的 20 位用户编码，类型码应在 300-499"`
	LocalOrganization    string   `json:"local_organization" comment:"本域用户隶属机构属性，不得包含连字符"`
	LocalCategory        string   `json:"local_category" comment:"本域用户类别属性，不得包含连字符"`
	LocalRank            string   `json:"local_rank" comment:"本域用户职级属性，不得包含连字符"`
	TrustedGatewayIDs    []string `json:"trusted_gateway_ids,omitempty" comment:"除直接上级外允许出现在身份路径中的 211 类型网关编码"`
	AllowedUserIDs       []string `json:"allowed_user_ids,omitempty" comment:"允许跨域访问的用户编码白名单；空表示不按该属性限制"`
	AllowedOrganizations []string `json:"allowed_organizations,omitempty" comment:"允许跨域访问的机构属性白名单；空表示不按该属性限制"`
	AllowedCategories    []string `json:"allowed_categories,omitempty" comment:"允许跨域访问的用户类别白名单；空表示不按该属性限制"`
	AllowedRanks         []string `json:"allowed_ranks,omitempty" comment:"允许跨域访问的用户职级白名单；空表示不按该属性限制"`
	MaxHops              int      `json:"max_hops" comment:"身份路径允许的最大安全路由网关跳数；0 使用缺省值 8"`
}

func (config SIPMonitorUserIdentity) Active() bool {
	return config.Enabled || config.Required
}

// ValidateMonitorUserIdentityConfig 校验跨域身份配置。
// 标准以连字符分段，因此所有属性都禁止连字符和控制字符，避免产生歧义或头注入。
func ValidateMonitorUserIdentityConfig(config SIPMonitorUserIdentity) error {
	if !config.Active() {
		return nil
	}
	if !isGBDeviceType(config.LocalGatewayID, "211") {
		return fmt.Errorf("Monitor-User-Identity 本地安全路由网关 ID 必须是类型码 211 的 20 位国标编码")
	}
	if !isGBDeviceType(config.RemoteGatewayID, "211") {
		return fmt.Errorf("Monitor-User-Identity 上级安全路由网关 ID 必须是类型码 211 的 20 位国标编码")
	}
	if strings.TrimSpace(config.LocalGatewayID) == strings.TrimSpace(config.RemoteGatewayID) {
		return fmt.Errorf("Monitor-User-Identity 本地和上级安全路由网关 ID 不能相同")
	}
	if !isGBUserCode(config.LocalUserID) {
		return fmt.Errorf("Monitor-User-Identity 本域用户 ID 必须是类型码 300-499 的 20 位国标编码")
	}
	for name, value := range map[string]string{
		"用户隶属机构": config.LocalOrganization,
		"用户类别":   config.LocalCategory,
		"用户职级":   config.LocalRank,
	} {
		if err := validateMonitorIdentityAttribute(value); err != nil {
			return fmt.Errorf("Monitor-User-Identity %s属性无效: %w", name, err)
		}
	}
	maxHops := config.MaxHops
	if maxHops == 0 {
		maxHops = 8
	}
	if maxHops < 2 || maxHops > 32 {
		return fmt.Errorf("Monitor-User-Identity max_hops 应在 2-32 之间；0 表示缺省值 8")
	}
	if config.Required && len(config.AllowedUserIDs) == 0 && len(config.AllowedOrganizations) == 0 &&
		len(config.AllowedCategories) == 0 && len(config.AllowedRanks) == 0 {
		return fmt.Errorf("Monitor-User-Identity Required 模式必须配置至少一项用户或身份属性白名单")
	}
	seenGateways := map[string]struct{}{strings.TrimSpace(config.RemoteGatewayID): {}}
	for _, gatewayID := range config.TrustedGatewayIDs {
		gatewayID = strings.TrimSpace(gatewayID)
		if !isGBDeviceType(gatewayID, "211") {
			return fmt.Errorf("Monitor-User-Identity 可信网关 %q 必须是类型码 211 的 20 位国标编码", gatewayID)
		}
		if gatewayID == strings.TrimSpace(config.LocalGatewayID) {
			return fmt.Errorf("Monitor-User-Identity 可信网关不能包含本地网关 ID")
		}
		if _, exists := seenGateways[gatewayID]; exists {
			return fmt.Errorf("Monitor-User-Identity 可信网关重复: %s", gatewayID)
		}
		seenGateways[gatewayID] = struct{}{}
	}
	for _, userID := range config.AllowedUserIDs {
		if !isGBUserCode(userID) {
			return fmt.Errorf("Monitor-User-Identity 允许用户 %q 必须是类型码 300-499 的 20 位国标编码", userID)
		}
	}
	for name, values := range map[string][]string{
		"允许机构": config.AllowedOrganizations,
		"允许类别": config.AllowedCategories,
		"允许职级": config.AllowedRanks,
	} {
		seen := make(map[string]struct{}, len(values))
		for _, value := range values {
			if err := validateMonitorIdentityAttribute(value); err != nil {
				return fmt.Errorf("Monitor-User-Identity %s %q 无效: %w", name, value, err)
			}
			value = strings.TrimSpace(value)
			if _, exists := seen[value]; exists {
				return fmt.Errorf("Monitor-User-Identity %s重复: %s", name, value)
			}
			seen[value] = struct{}{}
		}
	}
	return nil
}

func validateMonitorIdentityAttribute(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("不能为空")
	}
	if len(value) > 64 {
		return fmt.Errorf("长度不能超过 64 字节")
	}
	if strings.Contains(value, "-") {
		return fmt.Errorf("不能包含分段符号 -")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("不能包含控制字符")
		}
	}
	return nil
}

func isGBDeviceType(value, deviceType string) bool {
	value = strings.TrimSpace(value)
	return isDigitCode(value, 20) && len(deviceType) == 3 && value[10:13] == deviceType
}

func isGBUserCode(value string) bool {
	value = strings.TrimSpace(value)
	if !isDigitCode(value, 20) {
		return false
	}
	deviceType, err := strconv.Atoi(value[10:13])
	return err == nil && deviceType >= 300 && deviceType <= 499
}

// SIPUpstreamRegisterCertificateAuth 控制本平台作为 SIP UA 向上级注册时使用的
// GB/T 28181-2011/2014/2016 Capability/Asymmetric 数字证书双向认证。
type SIPUpstreamRegisterCertificateAuth struct {
	Enabled    bool   `json:"enabled" comment:"是否向上级声明数字证书认证能力"`
	Required   bool   `json:"required" comment:"是否拒绝 Digest 或无挑战成功造成的认证降级；设置后隐式启用"`
	LocalCert  string `json:"local_cert" comment:"本平台用于向该上级注册的 X.509 证书"`
	LocalKey   string `json:"local_key" comment:"本平台用于向该上级注册的 RSA 私钥"`
	ServerCert string `json:"server_cert" comment:"上级平台 X.509 证书"`
	ServerCA   string `json:"server_ca,omitempty" comment:"用于校验上级平台证书链的 CA 文件；为空时固定信任 ServerCert"`
	CRL        string `json:"crl,omitempty" comment:"上级平台证书撤销列表；配置时必须同时设置 ServerCA"`
}

func (config SIPUpstreamRegisterCertificateAuth) Active() bool {
	return config.Enabled || config.Required
}

func ValidateUpstreamRegisterCertificateAuthConfig(config SIPUpstreamRegisterCertificateAuth) error {
	if !config.Active() {
		return nil
	}
	for name, value := range map[string]string{
		"本平台证书":  config.LocalCert,
		"本平台私钥":  config.LocalKey,
		"上级平台证书": config.ServerCert,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("启用上级数字证书 REGISTER 认证时必须配置%s", name)
		}
	}
	if strings.TrimSpace(config.CRL) != "" && strings.TrimSpace(config.ServerCA) == "" {
		return fmt.Errorf("配置上级平台 CRL 时必须同时配置上级平台 CA 文件")
	}
	return nil
}

// SIPSignalDigest 控制 GB/T 28181 除 REGISTER 外的 Date + Note 信令摘要。
type SIPSignalDigest struct {
	Enabled         bool     `comment:"是否为出站请求和响应添加 Date+Note" json:"enabled"`
	Required        bool     `comment:"强制模式；同时启用出站签名，并拒绝未携带或校验失败的 Date+Note 入站消息" json:"required"`
	Seed            string   `comment:"设备未配置密码时使用的全局摘要 seed" json:"seed,omitempty"`
	Algorithm       string   `comment:"摘要算法：MD5/SHA-1/SHA-256/SM3" json:"algorithm"`
	Encoding        string   `comment:"nonce 编码：base64/hex" json:"encoding"`
	AcceptLegacyHex bool     `comment:"base64 模式下是否兼容接收厂商十六进制 nonce" json:"accept_legacy_hex"`
	Window          Duration `comment:"Date 允许的时间偏差，缺省 10 分钟" json:"window"`
}

// ValidateSignalDigestConfig 校验 Date+Note 信令摘要配置。
// Required 模式在运行时同时启用出站签名，因此不要求调用方额外设置 Enabled。
func ValidateSignalDigestConfig(config SIPSignalDigest) error {
	algorithm := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(config.Algorithm), "_", "-"))
	switch algorithm {
	case "MD5", "SHA-1", "SHA1", "SHA-256", "SHA256", "SM3":
	default:
		return fmt.Errorf("不支持的信令摘要算法 %q，仅支持 MD5、SHA-1、SHA-256、SM3", config.Algorithm)
	}

	switch strings.ToLower(strings.TrimSpace(config.Encoding)) {
	case "base64", "hex":
	default:
		return fmt.Errorf("不支持的信令摘要编码 %q，仅支持 base64、hex", config.Encoding)
	}

	window := config.Window.Duration()
	if window < time.Second || window > 24*time.Hour {
		return fmt.Errorf("信令摘要时间窗应在 1s–24h 之间")
	}
	return nil
}

// ValidateSIPConfig 校验 SIP 服务启动和运行所依赖的基础配置。
func ValidateSIPConfig(config SIP) error {
	if !isDigitCode(config.ID, 20) {
		return fmt.Errorf("SIP 平台 ID 必须是 20 位数字")
	}
	domain := config.GetDomain()
	if !isDigitCode(domain, 10) {
		return fmt.Errorf("SIP 域必须是 10 位数字；留空时从平台 ID 前 10 位派生")
	}
	if config.Port < 1 || config.Port > 65535 {
		return fmt.Errorf("SIP 监听端口应在 1–65535 之间")
	}
	if config.TLSPort < 0 || config.TLSPort > 65535 {
		return fmt.Errorf("SIP-TLS 监听端口应为 0 或 1–65535；0 表示与 SIP 端口相同")
	}
	if config.EnableTLS {
		tlsPort := config.TLSPort
		if tlsPort == 0 {
			tlsPort = config.Port
		}
		if tlsPort < 1 || tlsPort > 65535 {
			return fmt.Errorf("SIP-TLS 监听端口应为 0 或 1–65535；0 表示与 SIP 端口相同")
		}
		if strings.TrimSpace(config.TLSCert) == "" {
			return fmt.Errorf("启用 SIP-TLS 时必须配置证书文件")
		}
		if strings.TrimSpace(config.TLSKey) == "" {
			return fmt.Errorf("启用 SIP-TLS 时必须配置私钥文件")
		}
		if config.TLSRequireClientCert && strings.TrimSpace(config.TLSClientCA) == "" {
			return fmt.Errorf("要求 SIP-TLS 客户端证书时必须配置客户端 CA 文件")
		}
	}
	if err := ValidateSIPRegisterRedirect(config.RegisterRedirect, config.ID); err != nil {
		return err
	}
	if config.DeviceHistory.MaxRecords < 0 || config.DeviceHistory.MaxRecords > 100000 {
		return fmt.Errorf("设备历史最大记录数应在 0–100000 之间")
	}
	if config.DeviceHistory.MaxDays < 0 || config.DeviceHistory.MaxDays > 3650 {
		return fmt.Errorf("设备历史保留天数应在 0–3650 之间")
	}
	if err := ValidateSIPDirectTCPDownloadConfig(config.DirectTCPDownload); err != nil {
		return err
	}
	if err := ValidateRegisterCertificateAuthConfig(config.RegisterCertificateAuth); err != nil {
		return err
	}
	if err := ValidateSIPAnnexGConfig(config.AnnexG, config.ID, config.EnableTLS); err != nil {
		return err
	}
	if err := ValidateSIPAlarmReceivers(config.AlarmReceivers); err != nil {
		return err
	}
	return ValidateSignalDigestConfig(config.SignalDigest)
}

// ValidateSIPRegisterRedirect 校验 GB/T 28181-2022 REGISTER 重定向 Contact。
// 配置保存和 REGISTER 运行时复用同一 SIP URI 解析器，避免两套语法判断产生分歧。
func ValidateSIPRegisterRedirect(value, platformID string) error {
	raw := value
	if strings.IndexFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return fmt.Errorf("REGISTER 重定向 URI 不能包含控制字符")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.IndexFunc(value, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) >= 0 {
		return fmt.Errorf("REGISTER 重定向 URI 不能包含空白字符")
	}
	uri, err := sip.ParseSipURI(value)
	if err != nil {
		return fmt.Errorf("REGISTER 重定向 URI 无效")
	}
	if uri.FPassword != nil {
		return fmt.Errorf("REGISTER 重定向 URI 不能包含密码")
	}
	if user := uri.User(); user != nil && user.String() != "" {
		if platformID = strings.TrimSpace(platformID); platformID != "" && user.String() != platformID {
			return fmt.Errorf("REGISTER 重定向 URI 用户必须与 SIP 平台 ID 一致")
		}
	}
	if strings.TrimSpace(uri.Host()) == "" {
		return fmt.Errorf("REGISTER 重定向 URI host 不能为空")
	}

	transport := ""
	if uri.FUriParams != nil {
		if value, exists := uri.FUriParams.Get("transport"); exists {
			if value == nil || strings.TrimSpace(value.String()) == "" {
				return fmt.Errorf("REGISTER 重定向 URI transport 不能为空")
			}
			transport = strings.ToLower(strings.TrimSpace(value.String()))
		}
	}
	if sipURIParameterCount(value, "transport") > 1 {
		return fmt.Errorf("REGISTER 重定向 URI 不能重复配置 transport")
	}
	if transport != "" && transport != "udp" && transport != "tcp" && transport != "tls" {
		return fmt.Errorf("REGISTER 重定向 URI transport 只支持 udp、tcp 或 tls")
	}
	if uri.FIsEncrypted && transport != "" && transport != "tls" {
		return fmt.Errorf("REGISTER 重定向 SIPS URI 的 transport 必须为 tls")
	}
	return nil
}

func sipURIParameterCount(value, name string) int {
	start := strings.IndexByte(value, ';')
	if start < 0 {
		return 0
	}
	params := value[start+1:]
	if end := strings.IndexByte(params, '?'); end >= 0 {
		params = params[:end]
	}
	count := 0
	for _, param := range strings.Split(params, ";") {
		key, _, _ := strings.Cut(param, "=")
		if strings.EqualFold(strings.TrimSpace(key), name) {
			count++
		}
	}
	return count
}

// ValidateSIPAlarmReceivers 校验本域接警终端及其最小授权范围。
func ValidateSIPAlarmReceivers(receivers []SIPAlarmReceiver) error {
	names := make(map[string]struct{}, len(receivers))
	targets := make(map[string]struct{}, len(receivers))
	for index, receiver := range receivers {
		if !receiver.Enabled {
			continue
		}
		name := strings.TrimSpace(receiver.Name)
		if name == "" {
			return fmt.Errorf("接警终端 %d 的名称不能为空", index+1)
		}
		if _, exists := names[name]; exists {
			return fmt.Errorf("接警终端名称重复: %s", name)
		}
		names[name] = struct{}{}
		deviceID := strings.TrimSpace(receiver.DeviceID)
		if !isDigitCode(deviceID, 20) {
			return fmt.Errorf("接警终端 %s 的 device_id 必须是 20 位数字", name)
		}
		if _, exists := targets[deviceID]; exists {
			return fmt.Errorf("接警终端编码重复: %s", deviceID)
		}
		targets[deviceID] = struct{}{}
		if len(receiver.SourceIDs) == 0 {
			return fmt.Errorf("接警终端 %s 必须配置至少一个 source_ids", name)
		}
		sources := make(map[string]struct{}, len(receiver.SourceIDs))
		for _, sourceID := range receiver.SourceIDs {
			sourceID = strings.TrimSpace(sourceID)
			if !isDigitCode(sourceID, 20) && !isDigitCode(sourceID, 10) {
				return fmt.Errorf("接警终端 %s 的 source_id %q 必须是 10 位或 20 位数字", name, sourceID)
			}
			if _, exists := sources[sourceID]; exists {
				return fmt.Errorf("接警终端 %s 的 source_id 重复: %s", name, sourceID)
			}
			sources[sourceID] = struct{}{}
		}
	}
	return nil
}

func isDigitCode(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

type DeviceHistoryConfig struct {
	MaxRecords int `comment:"每台设备最多保留记录条数，0 表示不限制" json:"max_records"`
	MaxDays    int `comment:"最多保留天数，0 表示不限制" json:"max_days"`
}

// SIPDirectTCPDownload 控制 2014 附录 O 无 RTP 封装的 TCP 文件下载。
// 该能力默认关闭，并通过设备白名单显式启用，避免影响现有 RTP 下载链路。
type SIPDirectTCPDownload struct {
	Enabled              bool     `comment:"是否启用 2014 裸 TCP 文件下载" json:"enabled"`
	CascadeRelayEnabled  bool     `comment:"是否启用 2014 上级平台裸 TCP 下载中继；独立于设备直连下载开关" json:"cascade_relay_enabled"`
	DeviceAllowlist      []string `comment:"允许使用裸 TCP 下载的设备国标 ID 白名单" json:"device_allowlist"`
	StorageDir           string   `comment:"下载文件存储根目录" json:"storage_dir"`
	RetainDays           int      `comment:"完成文件保留天数" json:"retain_days"`
	OfferPort            int      `comment:"INVITE SDP 中声明的接收端占位端口" json:"offer_port"`
	RelayListenIP        string   `comment:"裸 TCP 级联中继监听 IP；可使用 0.0.0.0 或 ::" json:"relay_listen_ip"`
	RelayAdvertiseIP     string   `comment:"裸 TCP 级联向上级 SDP 宣告的可达 IP；为空时使用 Media.SDPIP" json:"relay_advertise_ip"`
	RelayPortStart       int      `comment:"裸 TCP 级联中继监听端口范围起点" json:"relay_port_start"`
	RelayPortEnd         int      `comment:"裸 TCP 级联中继监听端口范围终点" json:"relay_port_end"`
	MaxFileSize          int64    `comment:"单文件最大字节数" json:"max_file_size"`
	GlobalConcurrency    int      `comment:"全局最大并发下载数" json:"global_concurrency"`
	DeviceConcurrency    int      `comment:"单设备最大并发下载数" json:"device_concurrency"`
	DialTimeout          Duration `comment:"TCP 连接超时" json:"dial_timeout"`
	FirstByteTimeout     Duration `comment:"首字节超时" json:"first_byte_timeout"`
	IdleTimeout          Duration `comment:"下载空闲超时" json:"idle_timeout"`
	TotalTimeout         Duration `comment:"单次下载总超时" json:"total_timeout"`
	AllowAddressMismatch bool     `comment:"是否允许 SDP 地址与设备注册地址不一致" json:"allow_address_mismatch"`
	AllowedAddressCIDRs  []string `comment:"地址不一致时允许连接的 CIDR 白名单" json:"allowed_address_cidrs"`
}

// ValidateSIPDirectTCPDownloadConfig 校验 2014 附录 O 裸 TCP 下载的资源和地址安全边界。
// 关闭时允许保留旧版不完整配置；启用前必须提供完整且可执行的限制。
func ValidateSIPDirectTCPDownloadConfig(config SIPDirectTCPDownload) error {
	if !config.Enabled && !config.CascadeRelayEnabled {
		return nil
	}
	if len(config.DeviceAllowlist) == 0 {
		return fmt.Errorf("启用 2014 裸 TCP 下载时必须配置设备白名单")
	}
	devices := make(map[string]struct{}, len(config.DeviceAllowlist))
	for _, value := range config.DeviceAllowlist {
		deviceID := strings.TrimSpace(value)
		if !isDigitCode(deviceID, 20) {
			return fmt.Errorf("2014 裸 TCP 下载设备白名单必须使用 20 位国标编码")
		}
		if _, exists := devices[deviceID]; exists {
			return fmt.Errorf("2014 裸 TCP 下载设备白名单编码重复: %s", deviceID)
		}
		devices[deviceID] = struct{}{}
	}
	if config.Enabled && strings.TrimSpace(config.StorageDir) == "" {
		return fmt.Errorf("启用 2014 裸 TCP 下载时必须配置存储目录")
	}
	if config.Enabled && config.RetainDays <= 0 {
		return fmt.Errorf("2014 裸 TCP 下载文件保留天数必须大于 0")
	}
	if config.OfferPort < 1 || config.OfferPort > 65535 {
		return fmt.Errorf("2014 裸 TCP 下载 SDP 端口应在 1–65535 之间")
	}
	if config.CascadeRelayEnabled {
		listenIP := net.ParseIP(strings.TrimSpace(config.RelayListenIP))
		if listenIP == nil || listenIP.IsMulticast() {
			return fmt.Errorf("2014 裸 TCP 级联中继监听地址必须是有效的非组播 IP")
		}
		if value := strings.TrimSpace(config.RelayAdvertiseIP); value != "" {
			advertiseIP := net.ParseIP(value)
			if advertiseIP == nil || advertiseIP.IsUnspecified() || advertiseIP.IsMulticast() {
				return fmt.Errorf("2014 裸 TCP 级联中继宣告地址必须是有效的单播 IP")
			}
		}
		if config.RelayPortStart < 1 || config.RelayPortStart > 65535 ||
			config.RelayPortEnd < config.RelayPortStart || config.RelayPortEnd > 65535 {
			return fmt.Errorf("2014 裸 TCP 级联中继端口范围应在 1–65535 之间且起点不大于终点")
		}
	}
	if config.MaxFileSize <= 0 {
		return fmt.Errorf("2014 裸 TCP 下载单文件上限必须大于 0")
	}
	if config.GlobalConcurrency <= 0 {
		return fmt.Errorf("2014 裸 TCP 下载全局并发数必须大于 0")
	}
	if config.DeviceConcurrency <= 0 || config.DeviceConcurrency > config.GlobalConcurrency {
		return fmt.Errorf("2014 裸 TCP 下载单设备并发数应在 1–全局并发数之间")
	}
	phaseTimeouts := []struct {
		name  string
		value Duration
	}{
		{name: "连接", value: config.DialTimeout},
		{name: "首字节", value: config.FirstByteTimeout},
		{name: "空闲", value: config.IdleTimeout},
	}
	for _, timeout := range phaseTimeouts {
		if timeout.value.Duration() <= 0 {
			return fmt.Errorf("2014 裸 TCP 下载%s超时必须大于 0", timeout.name)
		}
	}
	if config.TotalTimeout.Duration() <= 0 {
		return fmt.Errorf("2014 裸 TCP 下载总超时必须大于 0")
	}
	for _, timeout := range phaseTimeouts {
		if config.TotalTimeout.Duration() < timeout.value.Duration() {
			return fmt.Errorf("2014 裸 TCP 下载总超时不能小于%s超时", timeout.name)
		}
	}
	if config.AllowAddressMismatch && len(config.AllowedAddressCIDRs) == 0 {
		return fmt.Errorf("允许 2014 裸 TCP 下载地址不一致时必须配置 CIDR 白名单")
	}
	cidrs := make(map[string]struct{}, len(config.AllowedAddressCIDRs))
	for _, value := range config.AllowedAddressCIDRs {
		value = strings.TrimSpace(value)
		if _, _, err := net.ParseCIDR(value); err != nil {
			return fmt.Errorf("2014 裸 TCP 下载允许地址 %q 不是有效 CIDR", value)
		}
		if _, exists := cidrs[value]; exists {
			return fmt.Errorf("2014 裸 TCP 下载允许地址 CIDR 重复: %s", value)
		}
		cidrs[value] = struct{}{}
	}
	return nil
}

// GetDomain 返回 SIP 域。
// 优先使用 uxl-v2 既有的显式 Domain 配置；为空时再按 GB/T28181 ID 前 10 位派生，
// 用于兼容 main 中新增的域派生调用，同时不改变已有配置语义。
func (s *SIP) GetDomain() string {
	if s.Domain != "" {
		return s.Domain
	}
	if len(s.ID) >= 10 {
		return s.ID[:10]
	}
	return s.ID
}

type SIPLog struct {
	Enabled      bool     `json:"enabled" comment:"是否启用 SIP 报文落盘"`
	Dir          string   `json:"dir" comment:"SIP 日志目录，建议独立目录"`
	MaxAge       Duration `json:"max_age" comment:"SIP 日志保留时长"`
	RotationTime Duration `json:"rotation_time" comment:"SIP 日志按时间分割间隔"`
	RotationSize int64    `json:"rotation_size" comment:"SIP 日志按文件大小分割(MB)"`
}

type Media struct {
	IP                          string `comment:"媒体服务器 IP"`
	HTTPPort                    int    `comment:"媒体服务器 HTTP 端口"`
	Secret                      string `comment:"媒体服务器密钥"`
	Type                        string `comment:"媒体服务器类型 zlm/lalmax"`
	GBSnapshotBaseURL           string `comment:"GB28181 抓拍图片回传基础地址，优先于 WebHookIP"`
	GBSnapshotFFmpegConcurrency int    `comment:"GB28181 FFmpeg 抓拍最大并发数，0 表示使用默认值 2"`
	WebHookIP                   string `comment:"用于流媒体 webhook 回调"`
	RTPPortRange                string `comment:"媒体服务器 RTP 端口范围"`
	SDPIP                       string `comment:"媒体服务器 SDP IP"`
}

type Duration time.Duration

func (d *Duration) UnmarshalText(b []byte) error {
	x, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(x)
	return nil
}

// UnmarshalJSON 同时兼容配置 API 历史使用的纳秒整数和便于人工调用的时长字符串。
func (d *Duration) UnmarshalJSON(body []byte) error {
	if d == nil {
		return fmt.Errorf("nil duration target")
	}
	if len(body) > 0 && body[0] == '"' {
		var value string
		if err := json.Unmarshal(body, &value); err != nil {
			return err
		}
		return d.UnmarshalText([]byte(value))
	}
	var nanoseconds int64
	if err := json.Unmarshal(body, &nanoseconds); err != nil {
		return fmt.Errorf("duration must be a nanosecond integer or duration string: %w", err)
	}
	*d = Duration(nanoseconds)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.Duration().String()), nil
}

func (d *Duration) Duration() time.Duration {
	return time.Duration(*d)
}
