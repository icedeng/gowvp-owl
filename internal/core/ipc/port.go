package ipc

import (
	"context"
	"time"
)

// Protocoler 协议抽象接口（端口）
//
// 设计原则:
// 1. 接口在 ipc 包内定义，避免循环依赖
// 2. 接口方法直接使用领域模型 (*Device, *Channel)
// 3. 适配器实现此接口，可以直接依赖和修改领域模型
// 4. 符合依赖倒置原则 (DIP):
//   - ipc (高层) 依赖 Protocoler 接口
//   - adapter (低层) 实现 Protocoler 接口
//   - adapter (低层) 依赖 ipc.Device (高层) ✅ 合理
//
// 这就是依赖反转！
type Protocoler interface {
	// ValidateDevice 验证设备连接（添加设备前调用）
	// 可以修改设备信息（如从 ONVIF 获取的固件版本等）
	ValidateDevice(ctx context.Context, device *Device) error

	// InitDevice 初始化设备连接（添加设备后调用）
	// 例如: GB28181 不需要主动初始化，ONVIF 需要查询 Profiles 作为通道
	InitDevice(ctx context.Context, device *Device) error

	// QueryCatalog 查询设备目录/通道
	QueryCatalog(ctx context.Context, device *Device) error

	// StartPlay 开始播放
	StartPlay(ctx context.Context, device *Device, channel *Channel) (*PlayResponse, error)

	// StopPlay 停止播放
	StopPlay(ctx context.Context, device *Device, channel *Channel) error

	DeleteDevice(ctx context.Context, device *Device) error

	Hooker
}

// DeviceDeleteLocker 可选地把协议清理和持久化删除纳入同一设备操作临界区。
// 需要与设备注册等异步协议操作串行化的适配器实现此接口。
type DeviceDeleteLocker interface {
	LockDeviceDelete(device *Device) func()
}

// DeviceEditCoordinator 可选地把设备编辑与异步协议状态变更串行化，并在持久化后收敛运行态。
type DeviceEditCoordinator interface {
	LockDeviceEdit(device *Device) func()
	DeviceEdited(ctx context.Context, before, after *Device) error
}

// ChannelMediaBindingCoordinator 可选地把媒体节点绑定与同通道媒体会话串行化。
// 管理端在锁内重新读取通道状态，避免绑定结果与实际运行节点发生漂移。
type ChannelMediaBindingCoordinator interface {
	LockChannelMedia(ctx context.Context, channel *Channel) (func(), error)
}

type Hooker interface {
	OnStreamNotFound(ctx context.Context, app, stream string) error
	// OnStreamChanged 流注销时调用，用于更新通道状态
	// app/stream 用于支持自定义 app/stream 的 RTMP/RTSP 通道
	OnStreamChanged(ctx context.Context, app, stream string) error
}

// MediaServerAwareHooker 是可选的媒体节点感知 Hook 扩展。
// 同一 app/stream 可能同时存在于多个媒体节点；携带 mediaServerId 时
// 按节点隔离生命周期，同时保留 Hooker 旧方法兼容既有适配器。
type MediaServerAwareHooker interface {
	OnStreamNotFoundOnMediaServer(ctx context.Context, mediaServerID, app, stream string) error
	OnStreamChangedOnMediaServer(ctx context.Context, mediaServerID, app, stream string) error
}

// OnPublisher 推流鉴权接口（可选实现）
// 只有 RTMP 需要实现此接口
type OnPublisher interface {
	// OnPublish 处理推流鉴权
	// 返回 true 表示鉴权通过，false 表示鉴权失败
	// app/stream 用于支持自定义 app/stream 的 RTMP/RTSP 通道
	OnPublish(ctx context.Context, app, stream string, params map[string]string) (bool, error)
}

// PlayResponse 播放响应
type PlayResponse struct {
	SSRC   string // GB28181 SSRC
	Stream string // 流 ID
	RTSP   string // RTSP 地址 (ONVIF)
}

type PTZControlInput struct {
	Action  string
	Speed   uint8
	Timeout int // seconds
	Preset  int
	Group   uint8
	Aux     uint8
	Value   uint16
}

type PTZCapable interface {
	PTZControl(ctx context.Context, device *Device, channel *Channel, in *PTZControlInput) error
}

// GBDeviceControlInput 是 GB 附录 A.2.3 统一控制输入。
type GBDeviceControlInput struct {
	TargetID  string
	Action    string
	Timeout   int // seconds
	ExtraInfo []string

	PTZCmd          string
	PTZCmdParam     *GBPTZCmdParamInput
	PTZSpeed        uint8
	PTZPreset       int
	PTZGroup        uint8
	PTZAux          uint8
	PTZValue        uint16
	ControlPriority *int

	StreamNumber int // 2022 录像控制码流编号；0 表示缺省主码流
	AlarmMethod  string
	AlarmType    string

	DragZoom     *GBDragZoomInput
	HomePosition *GBHomePositionInput
	PTZPrecise   *GBPTZPreciseInput
	TargetTrack  *GBTargetTrackInput
	SDCardID     int
}

type GBTargetTrackInput struct {
	Mode       string
	DeviceID2  string
	TargetArea *GBDragZoomInput
}

type GBDragZoomInput struct {
	Length    int
	Width     int
	MidPointX int
	MidPointY int
	LengthX   int
	LengthY   int
}

type GBHomePositionInput struct {
	Enabled     *int
	ResetTime   *int
	PresetIndex *int
}

type GBPTZPreciseInput struct {
	Pan  *float64
	Tilt *float64
	Zoom *float64
}

type GBPTZCmdParamInput struct {
	PresetName      string
	CruiseTrackName string
}

type GBDeviceControlOutput struct {
	SN       int    `json:"sn"`
	DeviceID string `json:"device_id"`
	TargetID string `json:"target_id"`
	Result   string `json:"result"`
}

type GBDeviceControlCapable interface {
	DeviceControl(ctx context.Context, device *Device, in *GBDeviceControlInput) (*GBDeviceControlOutput, error)
}

// GBDeviceQueryInput 是 GB 附录 A.2.4 统一查询输入。
type GBDeviceQueryInput struct {
	TargetID string
	Action   string
	Timeout  int // seconds

	ConfigType         string
	Interval           int
	Number             int   // cruise_track 轨迹编号（0 或 1）
	Start              int64 // catalog/record_info start unix seconds
	End                int64 // catalog/record_info end unix seconds
	FilePath           string
	Address            string
	Secrecy            *int
	Type               string
	RecorderID         string
	IndistinctQuery    *int
	StreamNumber       *int
	AlarmMethod        string
	AlarmType          string
	StartAlarmPriority string
	EndAlarmPriority   string
	StartAlarmTime     string
	EndAlarmTime       string
}

type GBDeviceQueryOutput struct {
	SN       int    `json:"sn"`
	CmdType  string `json:"cmd_type"`
	DeviceID string `json:"device_id"`
	Result   string `json:"result,omitempty"`
	XML      string `json:"xml"`
	Data     any    `json:"data,omitempty"`
	// AppendixA4 为附录 A.4 扩展对象结构化结果。
	AppendixA4 []GBAppendixA4Object `json:"appendix_a4,omitempty"`
}

type GBDeviceQueryCapable interface {
	DeviceQuery(ctx context.Context, device *Device, in *GBDeviceQueryInput) (*GBDeviceQueryOutput, error)
}

type GBBasicParamInput struct {
	Name              string `json:"name"`
	Expiration        int    `json:"expiration"`
	HeartBeatInterval int    `json:"heartbeat_interval"`
	HeartBeatCount    int    `json:"heartbeat_count"`
}

type GBDeviceConfigInput struct {
	TargetID            string                   `json:"target_id"`
	Timeout             int                      `json:"timeout"`
	ExtraInfo           []string                 `json:"extra_info"`
	BasicParam          *GBBasicParamInput       `json:"basic_param"`
	VideoParamConfig    *GBVideoParamConfigInput `json:"video_param_config"`
	AudioParamConfig    *GBAudioParamConfigInput `json:"audio_param_config"`
	SVACEncodeConfig    *GBXMLConfigInput        `json:"svac_encode_config"`
	SVACDecodeConfig    *GBXMLConfigInput        `json:"svac_decode_config"`
	VideoParamAttribute *GBXMLConfigInput        `json:"video_param_attribute"`
	VideoRecordPlan     *GBXMLConfigInput        `json:"video_record_plan"`
	VideoAlarmRecord    *GBXMLConfigInput        `json:"video_alarm_record"`
	PictureMask         *GBXMLConfigInput        `json:"picture_mask"`
	FrameMirror         *GBXMLConfigInput        `json:"frame_mirror"`
	AlarmReport         *GBXMLConfigInput        `json:"alarm_report"`
	OSDConfig           *GBXMLConfigInput        `json:"osd_config"`
	SnapShotConfig      *GBSnapshotConfigInput   `json:"snapshot_config"`
}

type GBVideoParamConfigInput struct {
	Items []GBVideoParamItemInput `json:"items"`
}

type GBVideoParamItemInput struct {
	StreamName   string `json:"stream_name"`
	VideoFormat  string `json:"video_format"`
	Resolution   string `json:"resolution"`
	FrameRate    string `json:"frame_rate"`
	BitRateType  string `json:"bit_rate_type"`
	VideoBitRate string `json:"video_bit_rate"`
}

type GBAudioParamConfigInput struct {
	Items []GBAudioParamItemInput `json:"items"`
}

type GBAudioParamItemInput struct {
	StreamName   string `json:"stream_name"`
	AudioFormat  string `json:"audio_format"`
	AudioBitRate string `json:"audio_bit_rate"`
	SamplingRate string `json:"sampling_rate"`
}

type GBXMLConfigInput struct {
	InnerXML string `json:"inner_xml"`
}

type GBSnapshotConfigInput struct {
	SnapNum   int    `json:"snap_num"`
	Interval  int    `json:"interval"`
	UploadURL string `json:"upload_url"`
	SessionID string `json:"session_id"`
}

type GBDeviceConfigOutput struct {
	SN       int    `json:"sn"`
	CmdType  string `json:"cmd_type"`
	DeviceID string `json:"device_id"`
	Result   string `json:"result,omitempty"`
	RawXML   string `json:"raw_xml,omitempty"`
}

type GBDeviceConfigCapable interface {
	DeviceConfig(ctx context.Context, device *Device, in *GBDeviceConfigInput) (*GBDeviceConfigOutput, error)
}

// GBAppendixA4SnapshotInput 是附录 A.4 快照查询参数。
type GBAppendixA4SnapshotInput struct {
	// CmdType 可选，支持逗号分隔多值（如 "Alarm,DeviceStatus"）。
	CmdType string `json:"cmd_type"`
	// Limit 可选，<=0 表示默认 200，最大 1000。
	Limit int `json:"limit"`
}

// GBAppendixA4SnapshotOutput 是附录 A.4 快照查询结果。
type GBAppendixA4SnapshotOutput struct {
	DeviceID string               `json:"device_id"`
	Filter   string               `json:"filter,omitempty"`
	Total    int                  `json:"total"`
	Items    []GBAppendixA4Object `json:"items"`
}

// RecordQueryInput 录像目录查询参数。
type RecordQueryInput struct {
	StartAt         int64 // unix seconds
	EndAt           int64 // unix seconds
	Timeout         int   // seconds
	FilePath        string
	Address         string
	Secrecy         *int
	Type            string
	RecorderID      string
	IndistinctQuery *int
	StreamNumber    *int
	AlarmMethod     string
	AlarmType       string
}

// RecordSegment 单段录像时间范围。
type RecordSegment struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// RecordDate 某一天的录像片段列表。
type RecordDate struct {
	Date  string          `json:"date"`
	Items []RecordSegment `json:"items"`
}

// RecordQueryOutput 录像目录查询结果。
type RecordQueryOutput struct {
	DayTotal int          `json:"daynum"`
	TimeNum  int          `json:"timenum"`
	Data     []RecordDate `json:"list"`
}

// RecordQueryable 协议层可选能力：查询设备录像目录。
type RecordQueryable interface {
	QueryRecords(ctx context.Context, device *Device, channel *Channel, in *RecordQueryInput) (*RecordQueryOutput, error)
}

type UpgradeInput struct {
	ChannelID    string
	Firmware     string
	FileURL      string
	Manufacturer string
	SessionID    string
	Timeout      int // seconds
}

type UpgradeOutput struct {
	SN        int    `json:"sn"`
	DeviceID  string `json:"device_id"`
	ChannelID string `json:"channel_id"`
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
}

type UpgradeCapable interface {
	Upgrade(ctx context.Context, device *Device, channel *Channel, in *UpgradeInput) (*UpgradeOutput, error)
}

type HistoryControlInput struct {
	StartAt int64   // unix seconds
	EndAt   int64   // unix seconds
	Mode    string  // playback/download
	Cmd     string  // INFO 控制命令（原文透传）
	Action  string  // 结构化动作：play/pause/speed/seek
	Scale   float64 // Action=speed 时使用；负值表示倒放
	SeekAt  int64   // unix seconds；Action=seek 的目标时间，或 2011 倍速/2022 负倍速的播放起点
	// Transport 为空或 rtp 时保持现有媒体服务器链路；direct_tcp 仅用于 1.1 文件下载。
	Transport string
	// DownloadSpeed 是 Download INVITE 的整数倍速；0 表示使用协议默认 1 倍速。
	DownloadSpeed int
	// RecordType 是 SDP u 字段录像/下载类型；nil 默认按时间类型 3。
	RecordType *int
}

const (
	HistoryTransportRTP       = "rtp"
	HistoryTransportDirectTCP = "direct_tcp"
)

type HistoryCapable interface {
	StartHistory(ctx context.Context, device *Device, channel *Channel, in *HistoryControlInput) error
	StopHistory(ctx context.Context, device *Device, channel *Channel, in *HistoryControlInput) error
	ControlHistory(ctx context.Context, device *Device, channel *Channel, in *HistoryControlInput) error
}

// TimeSyncCapable 表示协议适配器支持厂商扩展主动校时。
type TimeSyncCapable interface {
	SyncTime(ctx context.Context, device *Device) error
}

type SubscribeInput struct {
	TargetID string
	Event    string
	Expires  int
	Cancel   bool

	StartAlarmPriority string
	EndAlarmPriority   string
	AlarmMethod        string
	AlarmType          string
	StartAlarmTime     string
	EndAlarmTime       string
	StartTime          string
	EndTime            string
	Interval           int
}

type SubscribeCapable interface {
	Subscribe(ctx context.Context, device *Device, in *SubscribeInput) error
}

// SubscriptionState 是平台向协议设备建立的事件订阅运行态。
type SubscriptionState struct {
	DeviceID  string    `json:"device_id"`
	TargetID  string    `json:"target_id"`
	Event     string    `json:"event"`
	Status    string    `json:"status"`
	Expires   int       `json:"expires"`
	ExpiresAt time.Time `json:"expires_at"`
	RefreshAt time.Time `json:"refresh_at,omitempty"`

	Refreshing    bool `json:"refreshing"`
	CancelPending bool `json:"cancel_pending"`

	NotifyCSeq      uint32    `json:"notify_cseq,omitempty"`
	NotifyExpiresAt time.Time `json:"notify_expires_at,omitempty"`

	StartAlarmPriority string `json:"start_alarm_priority,omitempty"`
	EndAlarmPriority   string `json:"end_alarm_priority,omitempty"`
	AlarmMethod        string `json:"alarm_method,omitempty"`
	AlarmType          string `json:"alarm_type,omitempty"`
	StartAlarmTime     string `json:"start_alarm_time,omitempty"`
	EndAlarmTime       string `json:"end_alarm_time,omitempty"`
	StartTime          string `json:"start_time,omitempty"`
	EndTime            string `json:"end_time,omitempty"`
	Interval           int    `json:"interval,omitempty"`
}

type SubscriptionStateCapable interface {
	SubscriptionStates(ctx context.Context, device *Device) ([]SubscriptionState, error)
}

type OptionsProbeInput struct {
	Timeout int // seconds
}

type OptionsProbeCapable interface {
	ProbeOptions(ctx context.Context, device *Device, in *OptionsProbeInput) error
}

type VoiceControlInput struct {
	Mode          string // talk/talk_standard/broadcast
	MediaServerID string
	SourceID      string
	SourceVHost   string
	SourceApp     string
	SourceStream  string
}

type VoiceCapable interface {
	StartVoice(ctx context.Context, device *Device, channel *Channel, in *VoiceControlInput) error
	StopVoice(ctx context.Context, device *Device, channel *Channel, in *VoiceControlInput) error
}
