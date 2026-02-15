package ipc

import (
	"context"
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

type Hooker interface {
	OnStreamNotFound(ctx context.Context, app, stream string) error
	// OnStreamChanged 流注销时调用，用于更新通道状态
	// app/stream 用于支持自定义 app/stream 的 RTMP/RTSP 通道
	OnStreamChanged(ctx context.Context, app, stream string) error
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
	TargetID string
	Action   string
	Timeout  int // seconds

	PTZCmd      string
	PTZCmdParam *GBPTZCmdParamInput

	StreamNumber int
	AlarmMethod  string
	AlarmType    string

	DragZoom     *GBDragZoomInput
	HomePosition *GBHomePositionInput
	PTZPrecise   *GBPTZPreciseInput
	SDCardID     int
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

	ConfigType string
	Interval   int
	Start      int64 // record_info start unix seconds
	End        int64 // record_info end unix seconds
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
	StartAt int64 // unix seconds
	EndAt   int64 // unix seconds
	Timeout int   // seconds
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

type UpgradeCapable interface {
	Upgrade(ctx context.Context, device *Device, channel *Channel, in *UpgradeInput) error
}

type HistoryControlInput struct {
	StartAt int64  // unix seconds
	EndAt   int64  // unix seconds
	Mode    string // playback/download
	Cmd     string // INFO 控制命令（原文透传）
	Action  string // 结构化动作：play/pause/speed/seek
	Scale   float64
	SeekAt  int64 // unix seconds
}

type HistoryCapable interface {
	StartHistory(ctx context.Context, device *Device, channel *Channel, in *HistoryControlInput) error
	StopHistory(ctx context.Context, device *Device, channel *Channel, in *HistoryControlInput) error
	ControlHistory(ctx context.Context, device *Device, channel *Channel, in *HistoryControlInput) error
}

type TimeSyncCapable interface {
	SyncTime(ctx context.Context, device *Device) error
}

type SubscribeInput struct {
	Event   string
	Expires int
}

type SubscribeCapable interface {
	Subscribe(ctx context.Context, device *Device, in *SubscribeInput) error
}

type OptionsProbeInput struct {
	Timeout int // seconds
}

type OptionsProbeCapable interface {
	ProbeOptions(ctx context.Context, device *Device, in *OptionsProbeInput) error
}

type VoiceControlInput struct {
	Mode string // talk/broadcast
}

type VoiceCapable interface {
	StartVoice(ctx context.Context, device *Device, channel *Channel, in *VoiceControlInput) error
	StopVoice(ctx context.Context, device *Device, channel *Channel, in *VoiceControlInput) error
}
