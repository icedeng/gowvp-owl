package api

import "github.com/ixugo/goddd/pkg/orm"

// AIKeepaliveInput 心跳回调请求体
type AIKeepaliveInput struct {
	Timestamp int64          `json:"timestamp" example:"1710931200000"` // Unix 时间戳（毫秒）
	Stats     *AIGlobalStats `json:"stats"`                             // 全局统计信息
	Message   string         `json:"message" example:"alive"`           // 附加消息
}

// AIStartedInput 服务启动回调请求体
type AIStartedInput struct {
	Timestamp int64  `json:"timestamp" example:"1710931200000"` // Unix 时间戳（毫秒）
	Message   string `json:"message" example:"service started"` // 启动消息
}

// AIDetectionInput 检测事件回调请求体
type AIDetectionInput struct {
	CameraID       string        `json:"camera_id" example:"GB_34020000001320000001"` // 摄像头 ID
	Timestamp      orm.Time      `json:"timestamp"`                                   // Unix 时间戳 (毫秒)
	Detections     []AIDetection `json:"detections"`                                  // 检测结果列表
	Snapshot       string        `json:"snapshot"`                                    // Base64 编码的快照（JPEG）
	SnapshotWidth  int           `json:"snapshot_width" example:"1920"`               // 快照宽度
	SnapshotHeight int           `json:"snapshot_height" example:"1080"`              // 快照高度
}

// AIStoppedInput 任务停止回调请求体
type AIStoppedInput struct {
	CameraID  string   `json:"camera_id" example:"GB_34020000001320000001"` // 摄像头 ID
	Timestamp orm.Time `json:"timestamp"`                                   // Unix 时间戳 (毫秒)
	Reason    string   `json:"reason" example:"user_requested"`             // 停止原因（user_requested/error）
	Message   string   `json:"message" example:"manual stop"`               // 详细信息
}

// AIDetection 检测对象
type AIDetection struct {
	Label      string        `json:"label" example:"person"`    // 物体类别
	Confidence float64       `json:"confidence" example:"0.92"` // 置信度（0.0-1.0）
	Box        AIBoundingBox `json:"box"`                       // 像素坐标边界框
	Area       int           `json:"area" example:"35800"`      // 边界框像素面积
	NormBox    *AINormBox    `json:"norm_box"`                  // 归一化边界框
}

// AIBoundingBox 像素坐标边界框
type AIBoundingBox struct {
	XMin int `json:"x_min" example:"120"`
	YMin int `json:"y_min" example:"80"`
	XMax int `json:"x_max" example:"360"`
	YMax int `json:"y_max" example:"500"`
}

// AINormBox 归一化边界框
type AINormBox struct {
	X float64 `json:"x" example:"0.42"` // 中心点 X 坐标
	Y float64 `json:"y" example:"0.51"` // 中心点 Y 坐标
	W float64 `json:"w" example:"0.18"` // 宽度
	H float64 `json:"h" example:"0.32"` // 高度
}

// AIGlobalStats 全局统计信息
type AIGlobalStats struct {
	ActiveStreams   int   `json:"active_streams" example:"8"`      // 活跃流数量
	TotalDetections int64 `json:"total_detections" example:"1234"` // 总检测次数
	UptimeSeconds   int64 `json:"uptime_seconds" example:"3600"`   // 运行时间（秒）
}

// AIWebhookOutput 通用响应体
type AIWebhookOutput struct {
	Code int    `json:"code" example:"0"`      // 错误代码，0 表示成功
	Msg  string `json:"msg" example:"success"` // 消息
}

func newAIWebhookOutputOK() AIWebhookOutput {
	return AIWebhookOutput{Code: 0, Msg: "success"}
}
