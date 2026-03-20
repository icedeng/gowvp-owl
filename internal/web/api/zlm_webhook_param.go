package api

// 注销
//	{
//		"mediaServerId" : "your_server_id",
//		"app" : "live",
//		"regist" : false,
//		"schema" : "rtsp",
//		"stream" : "obs",
//		"vhost" : "__defaultVhost__"
//	}

// 注册
//
//	{
//	    "regist" : true,
//	    "aliveSecond": 0, #存活时间，单位秒
//	    "app": "live", # 应用名
//	    "bytesSpeed": 0, #数据产生速度，单位byte/s
//	    "createStamp": 1617956908,  #GMT unix系统时间戳，单位秒
//	    "mediaServerId": "your_server_id", # 服务器id
//	    "originSock": {
//	        "identifier": "000001C257D35E40",
//	        "local_ip": "172.26.20.112", # 本机ip
//	        "local_port": 50166, # 本机端口
//	        "peer_ip": "172.26.20.112", # 对端ip
//	        "peer_port": 50155 # 对端port
//	    },
//	    "originType": 8,  # 产生源类型，包括 unknown = 0,rtmp_push=1,rtsp_push=2,rtp_push=3,pull=4,ffmpeg_pull=5,mp4_vod=6,device_chn=7,rtc_push=8
//	    "originTypeStr": "rtc_push",
//	    "originUrl": "", #产生源的url
//	    "readerCount": 0, # 本协议观看人数
//	    "schema": "rtsp", # 协议
//	    "stream": "test",  # 流id
//	    "totalReaderCount": 0, # 观看总人数，包括hls/rtsp/rtmp/http-flv/ws-flv/rtc
//	    "tracks": [{
//	       "channels" : 1, # 音频通道数
//	       "codec_id" : 2, # H264 = 0, H265 = 1, AAC = 2, G711A = 3, G711U = 4
//	       "codec_id_name" : "CodecAAC", # 编码类型名称
//	       "codec_type" : 1, # Video = 0, Audio = 1
//	       "ready" : true, # 轨道是否准备就绪
//	       "sample_bit" : 16, # 音频采样位数
//	       "sample_rate" : 8000 # 音频采样率
//	    },
//	    {
//	       "codec_id" : 0, # H264 = 0, H265 = 1, AAC = 2, G711A = 3, G711U = 4
//	       "codec_id_name" : "CodecH264", # 编码类型名称
//	       "codec_type" : 0, # Video = 0, Audio = 1
//	       "fps" : 59,  # 视频fps
//	       "height" : 720, # 视频高
//	       "ready" : true,  # 轨道是否准备就绪
//	       "width" : 1280 # 视频宽
//	    }],
//	    "vhost": "__defaultVhost__"
//	}
type onStreamChangedInput struct {
	Regist           bool       `json:"regist" example:"true"`            // true 表示注册，false 表示注销
	AliveSecond      int        `json:"aliveSecond" example:"0"`          // 存活时长，秒
	App              string     `json:"app" example:"live"`               // 应用名
	BytesSpeed       int        `json:"bytesSpeed" example:"0"`           // 当前字节速度
	CreateStamp      int        `json:"createStamp" example:"1710931200"` // 创建时间戳
	MediaServerID    string     `json:"mediaServerId" example:"local"`    // 流媒体服务器 ID
	OriginSock       OriginSock `json:"originSock"`
	OriginType       int        `json:"originType" example:"2"`            // 源类型
	OriginTypeStr    string     `json:"originTypeStr" example:"rtsp_push"` // 源类型文本
	OriginURL        string     `json:"originUrl" example:""`              // 源地址
	ReaderCount      int        `json:"readerCount" example:"0"`           // 当前协议观看人数
	Schema           string     `json:"schema" example:"rtsp"`             // 协议
	Stream           string     `json:"stream" example:"test"`             // 流 ID
	TotalReaderCount int        `json:"totalReaderCount" example:"0"`      // 总观看人数
	Tracks           []Tracks   `json:"tracks"`
	Vhost            string     `json:"vhost" example:"__defaultVhost__"` // 虚拟主机

	// 以下字段为 lalmax 新增
	AppName    string `json:"app_name" example:"live"`    // 流应用名
	StreamName string `json:"stream_name" example:"test"` // 流名称
}
type OriginSock struct {
	Identifier string `json:"identifier" example:"000001C257D35E40"` // 连接标识
	LocalIP    string `json:"local_ip" example:"172.26.20.112"`      // 本地 IP
	LocalPort  int    `json:"local_port" example:"50166"`            // 本地端口
	PeerIP     string `json:"peer_ip" example:"172.26.20.112"`       // 对端 IP
	PeerPort   int    `json:"peer_port" example:"50155"`             // 对端端口
}
type Tracks struct {
	Channels    int     `json:"channels,omitempty" example:"1"`       // 音频通道数
	CodecID     int     `json:"codec_id" example:"0"`                 // 编码 ID
	CodecIDName string  `json:"codec_id_name" example:"CodecH264"`    // 编码名称
	CodecType   int     `json:"codec_type" example:"0"`               // 0 视频，1 音频
	Ready       bool    `json:"ready" example:"true"`                 // 轨道是否就绪
	SampleBit   int     `json:"sample_bit,omitempty" example:"16"`    // 采样位数
	SampleRate  int     `json:"sample_rate,omitempty" example:"8000"` // 采样率
	Fps         float32 `json:"fps,omitempty" example:"25"`           // 帧率
	Height      int     `json:"height,omitempty" example:"1080"`      // 视频高度
	Width       int     `json:"width,omitempty" example:"1920"`       // 视频宽度
}

// 心跳
// {
// 	"data" : {
// 		"Buffer" : 12,
// 		"BufferLikeString" : 0,
// 		"BufferList" : 0,
// 		"BufferRaw" : 12,
// 		"Frame" : 0,
// 		"FrameImp" : 0,
// 		"MediaSource" : 0,
// 		"MultiMediaSourceMuxer" : 0,
// 		"RtmpPacket" : 0,
// 		"RtpPacket" : 0,
// 		"Socket" : 108,
// 		"TcpClient" : 0,
// 		"TcpServer" : 96,
// 		"TcpSession" : 0,
// 		"UdpServer" : 12,
// 		"UdpSession" : 0
// 	 },
// 	 "mediaServerId" : "192.168.255.10"
//   }

type onServerKeepaliveInput struct {
	Data          Data   `json:"data"`
	HookIndex     int    `json:"hook_index" example:"0"`        // Webhook 索引
	MediaServerID string `json:"mediaServerId" example:"local"` // 流媒体服务器 ID
}
type Data struct {
	Buffer                int `json:"Buffer"`
	BufferLikeString      int `json:"BufferLikeString"`
	BufferList            int `json:"BufferList"`
	BufferRaw             int `json:"BufferRaw"`
	Frame                 int `json:"Frame"`
	FrameImp              int `json:"FrameImp"`
	MediaSource           int `json:"MediaSource"`
	MultiMediaSourceMuxer int `json:"MultiMediaSourceMuxer"`
	RtmpPacket            int `json:"RtmpPacket"`
	RtpPacket             int `json:"RtpPacket"`
	Socket                int `json:"Socket"`
	TCPClient             int `json:"TcpClient"`
	TCPServer             int `json:"TcpServer"`
	TCPSession            int `json:"TcpSession"`
	UDPServer             int `json:"UdpServer"`
	UDPSession            int `json:"UdpSession"`
}

type onPublishInput struct {
	MediaServerID string `json:"mediaServerId" example:"local"`    // 流媒体服务器 ID
	App           string `json:"app" example:"live"`               // 应用名
	ID            string `json:"id" example:"1"`                   // TCP 链接唯一 ID
	IP            string `json:"ip" example:"192.168.1.10"`        // 推流或播放端 IP
	Params        string `json:"params" example:"token=abc"`       // URL 参数
	Port          int    `json:"port" example:"50000"`             // 端口号
	Schema        string `json:"schema" example:"rtsp"`            // 协议，rtsp/rtmp/http 等
	Stream        string `json:"stream" example:"camera001"`       // 流 ID
	Vhost         string `json:"vhost" example:"__defaultVhost__"` // 虚拟主机
}

type onPublishOutput struct {
	DefaultOutput
	AddMuteAudio   *bool   `json:"add_mute_audio,omitempty"`
	ContinuePushMs *int    `json:"continue_push_ms,omitempty"`
	EnableAudio    *bool   `json:"enable_audio,omitempty"`
	EnableFmp4     *bool   `json:"enable_fmp4,omitempty"`
	EnableHls      *bool   `json:"enable_hls,omitempty"`
	EnableHlsFmp4  *bool   `json:"enable_hls_fmp4,omitempty"`
	EnableMp4      *bool   `json:"enable_mp4,omitempty"`
	EnableRtmp     *bool   `json:"enable_rtmp,omitempty"`
	EnableRtsp     *bool   `json:"enable_rtsp,omitempty"`
	EnableTs       *bool   `json:"enable_ts,omitempty"`
	HlsSavePath    *string `json:"hls_save_path,omitempty"`
	ModifyStamp    *bool   `json:"modify_stamp,omitempty"`
	Mp4AsPlayer    *bool   `json:"mp4_as_player,omitempty"`
	Mp4MaxSecond   *int    `json:"mp4_max_second,omitempty"`
	Mp4SavePath    *string `json:"mp4_save_path,omitempty"`
	AutoClose      *bool   `json:"auto_close,omitempty"`
	StreamReplace  *string `json:"stream_replace,omitempty"`
}

type DefaultOutput struct {
	Code int    `json:"code" example:"0"`      // 错误代码，0 代表允许
	Msg  string `json:"msg" example:"success"` // 结果说明
}

func newDefaultOutputOK() DefaultOutput {
	return DefaultOutput{Code: 0, Msg: "success"}
}

type onStreamNoneReaderOutput struct {
	Code  int  `json:"code" example:"0"`      // 返回码
	Close bool `json:"close" example:"false"` // 是否关闭流
}

type onStreamNoneReaderInput struct {
	App           string `json:"app" example:"live"`               // 流应用名
	Schema        string `json:"schema" example:"rtsp"`            // 协议
	Stream        string `json:"stream" example:"camera001"`       // 流 ID
	Vhost         string `json:"vhost" example:"__defaultVhost__"` // 流虚拟主机
	MediaServerID string `json:"mediaServerId" example:"local"`    // 流媒体服务器 ID
}

type onRTPServerTimeoutInput struct {
	LocalPort     int    `json:"local_port" example:"30000"`    // RTP 本地端口
	ReUsePort     bool   `json:"re_use_port" example:"false"`   // 是否复用端口
	SSRC          uint32 `json:"ssrc" example:"12345678"`       // SSRC
	StreamID      string `json:"stream_id" example:"camera001"` // 流 ID
	TCPMode       int    `json:"tcp_mode" example:"0"`          // TCP 模式
	MediaServerID string `json:"mediaServerId" example:"local"` // 流媒体服务器 ID
}

type onStreamNotFoundInput struct {
	MediaServerID string `json:"mediaServerId" example:"local"`    // 流媒体服务器 ID
	App           string `json:"app" example:"live"`               // 应用名
	ID            string `json:"id" example:"1"`                   // TCP 连接 ID
	IP            string `json:"ip" example:"192.168.1.20"`        // 播放端 IP
	Params        string `json:"params" example:"token=abc"`       // URL 参数
	Port          int    `json:"port" example:"52000"`             // 播放端端口
	Schema        string `json:"schema" example:"rtsp"`            // 协议
	Stream        string `json:"stream" example:"camera001"`       // 流 ID
	Vhost         string `json:"vhost" example:"__defaultVhost__"` // 虚拟主机

	// 以下字段为 lalmax 新增
	AppName    string `json:"app_name" example:"live"`         // 流应用名
	StreamName string `json:"stream_name" example:"camera001"` // 流名称
}

// onRecordMP4Input 录制 mp4 完成后通知事件参数
// https://docs.zlmediakit.com/zh/guide/media_server/web_hook_api.html#_8%E3%80%81on-record-mp4
type onRecordMP4Input struct {
	MediaServerID string  `json:"mediaServerId" example:"local"`                  // 流媒体服务器 ID
	App           string  `json:"app" example:"live"`                             // 录制流应用名
	FileName      string  `json:"file_name" example:"camera001_20260320.mp4"`     // 文件名
	FilePath      string  `json:"file_path" example:"/data/record/camera001.mp4"` // 文件绝对路径
	FileSize      int64   `json:"file_size" example:"1024000"`                    // 文件大小，字节
	Folder        string  `json:"folder" example:"/data/record"`                  // 文件目录
	StartTime     int64   `json:"start_time" example:"1710931200"`                // 开始录制时间戳（秒）
	Stream        string  `json:"stream" example:"camera001"`                     // 录制流 ID
	TimeLen       float64 `json:"time_len" example:"60"`                          // 录制时长，秒
	URL           string  `json:"url" example:"/record/live/camera001.mp4"`       // 点播相对 URL
	Vhost         string  `json:"vhost" example:"__defaultVhost__"`               // 流虚拟主机
}
