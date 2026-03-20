package api

import (
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/gowvp/owl/internal/core/sms"
)

// SwaggerMessageResponse 是通用成功响应。
type SwaggerMessageResponse struct {
	Msg string `json:"msg" example:"ok"` // 处理结果说明
}

// SwaggerErrorResponse 是通用错误响应。
type SwaggerErrorResponse struct {
	Msg string `json:"msg" example:"参数错误"` // 错误信息
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
	SIP SwaggerSIPConfig `json:"sip"` // 当前系统使用的 SIP 配置
}

// SwaggerSIPConfig 是 Swagger 友好的 SIP 配置模型。
type SwaggerSIPConfig struct {
	Port               int    `json:"port" example:"5060"`                      // SIP TCP/UDP 监听端口
	ID                 string `json:"id" example:"34020000002000000001"`        // 平台 20 位国标编码
	Domain             string `json:"domain" example:"3402000000"`              // SIP 域
	Password           string `json:"password" example:"123456"`                // 注册鉴权密码
	EnableTLS          bool   `json:"enable_tls" example:"false"`               // 是否启用 SIP-TLS
	TLSPort            int    `json:"tls_port" example:"5061"`                  // SIP-TLS 监听端口
	TLSCert            string `json:"tls_cert" example:"configs/certs/sip.crt"` // TLS 证书路径
	TLSKey             string `json:"tls_key" example:"configs/certs/sip.key"`  // TLS 私钥路径
	StrictSourceCheck  bool   `json:"strict_source_check" example:"true"`       // 是否严格校验源 IP
	RequireMessageAuth bool   `json:"require_message_auth" example:"false"`     // 是否要求 MESSAGE/NOTIFY 做 Digest 鉴权
}

// SwaggerLoginKeyOutput 是登录公钥响应。
type SwaggerLoginKeyOutput struct {
	Key string `json:"key"` // Base64 编码的 PEM 公钥，用于前端加密登录数据
}

// SwaggerSnapshotLinkOutput 是快照刷新响应。
type SwaggerSnapshotLinkOutput struct {
	Link string `json:"link" example:"http://127.0.0.1:9900/channels/xxx/snapshot?token=xxx"` // 可直接访问的快照地址
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
	DayTotal int              `json:"daynum" example:"2"`  // 有录像的日期数量
	TimeNum  int              `json:"timenum" example:"6"` // 录像时间片段总数
	Data     []ipc.RecordDate `json:"list"`                // 按日期归类的录像片段
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

// SwaggerGBDeviceControlInput 是 GB 统一控制请求体。
type SwaggerGBDeviceControlInput struct {
	TargetID     string                      `json:"target_id" example:"34020000001320000001"` // 目标设备或通道国标编码；为空时默认当前设备
	Action       string                      `json:"action" example:"ptz_cmd"`                 // GB 统一控制动作名
	Timeout      int                         `json:"timeout" example:"5"`                      // 等待设备响应超时时间，单位秒
	PTZCmd       string                      `json:"ptz_cmd" example:"preset_call"`            // PTZCmd 子动作名
	PTZCmdParam  *SwaggerGBPTZCmdParamInput  `json:"ptz_cmd_param"`                            // PTZCmd 扩展参数
	StreamNumber int                         `json:"stream_number" example:"1"`                // 录像/强制关键帧等场景使用的码流序号
	AlarmMethod  string                      `json:"alarm_method" example:"2"`                 // 报警复位方式
	AlarmType    string                      `json:"alarm_type" example:"1"`                   // 报警类型
	DragZoom     *SwaggerGBDragZoomInput     `json:"drag_zoom"`                                // 拉框放大/缩小参数
	HomePosition *SwaggerGBHomePositionInput `json:"home_position"`                            // 看守位控制参数
	PTZPrecise   *SwaggerGBPTZPreciseInput   `json:"ptz_precise"`                              // 精确 PTZ 参数
	SDCardID     int                         `json:"sdcard_id" example:"1"`                    // SD 卡编号
}

// SwaggerGBDeviceQueryInput 是 GB 统一查询请求体。
type SwaggerGBDeviceQueryInput struct {
	TargetID   string `json:"target_id" example:"34020000001320000001"` // 目标设备或通道国标编码；为空时默认当前设备
	Action     string `json:"action" example:"device_status"`           // GB 统一查询动作名
	Timeout    int    `json:"timeout" example:"5"`                      // 等待查询应答超时时间，单位秒
	ConfigType string `json:"config_type" example:"basic_param"`        // 配置查询时的配置类型
	Interval   int    `json:"interval" example:"60"`                    // 订阅或统计类查询的时间间隔
	Start      int64  `json:"start" example:"1710864000"`               // 起始时间，Unix 秒
	End        int64  `json:"end" example:"1710950400"`                 // 结束时间，Unix 秒
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
}

// SwaggerGBAppendixA4SnapshotOutput 是附录 A.4 快照响应。
type SwaggerGBAppendixA4SnapshotOutput struct {
	DeviceID string                      `json:"device_id" example:"340200..."`                 // 当前设备国标编码
	Filter   string                      `json:"filter,omitempty" example:"Alarm,DeviceStatus"` // 生效的过滤条件
	Total    int                         `json:"total" example:"2"`                             // 本次返回条数
	Items    []SwaggerGBAppendixA4Output `json:"items"`                                         // 快照列表
}
