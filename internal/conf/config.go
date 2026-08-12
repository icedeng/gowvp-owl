package conf

import "time"

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

	EnableTLS bool   `comment:"是否启用 SIP-TLS 监听" json:"enable_tls"`
	TLSPort   int    `comment:"SIP-TLS 监听端口，0 表示与 port 相同" json:"tls_port"`
	TLSCert   string `comment:"SIP-TLS 证书文件路径" json:"tls_cert"`
	TLSKey    string `comment:"SIP-TLS 私钥文件路径" json:"tls_key"`

	StrictSourceCheck  bool                 `comment:"是否校验设备上报源IP与注册源IP一致" json:"strict_source_check"`
	RequireMessageAuth bool                 `comment:"是否要求 MESSAGE/NOTIFY 携带 Digest 鉴权" json:"require_message_auth"`
	PTZWeakConfirm     bool                 `comment:"是否启用 PTZ 弱确认模式；命令发送成功但设备未返回 DeviceControl 应答时按成功处理" json:"ptz_weak_confirm"`
	DirectTCPDownload  SIPDirectTCPDownload `comment:"GB/T 28181-2014 附录 O 裸 TCP 文件下载" json:"direct_tcp_download"`
	Log                SIPLog
}

// SIPDirectTCPDownload 控制 2014 附录 O 无 RTP 封装的 TCP 文件下载。
// 该能力默认关闭，并通过设备白名单显式启用，避免影响现有 RTP 下载链路。
type SIPDirectTCPDownload struct {
	Enabled              bool     `comment:"是否启用 2014 裸 TCP 文件下载" json:"enabled"`
	DeviceAllowlist      []string `comment:"允许使用裸 TCP 下载的设备国标 ID 白名单" json:"device_allowlist"`
	StorageDir           string   `comment:"下载文件存储根目录" json:"storage_dir"`
	RetainDays           int      `comment:"完成文件保留天数" json:"retain_days"`
	OfferPort            int      `comment:"INVITE SDP 中声明的接收端占位端口" json:"offer_port"`
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
	Enabled      bool     `comment:"是否启用 SIP 报文落盘"`
	Dir          string   `comment:"SIP 日志目录，建议独立目录"`
	MaxAge       Duration `comment:"SIP 日志保留时长"`
	RotationTime Duration `comment:"SIP 日志按时间分割间隔"`
	RotationSize int64    `comment:"SIP 日志按文件大小分割(MB)"`
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

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.Duration().String()), nil
}

func (d *Duration) Duration() time.Duration {
	return time.Duration(*d)
}
