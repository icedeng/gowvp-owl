package zlm

import "context"

const (
	openRtpServer    = `/index/api/openRtpServer`
	connectRtpServer = `/index/api/connectRtpServer`
	closeRtpServer   = `/index/api/closeRtpServer`
	startSendRtp     = `/index/api/startSendRtp`
	startSendRtpTalk = `/index/api/startSendRtpTalk`
	stopSendRtp      = `/index/api/stopSendRtp`
)

type ConnectRTPServerRequest struct {
	StreamID string `json:"stream_id"`
	DstURL   string `json:"dst_url"`
	DstPort  int    `json:"dst_port"`
}

type ConnectRTPServerResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// ConnectRTPServer 让主动 TCP 模式的 RTP 服务连接设备在 SDP 应答中声明的地址。
func (e *Engine) ConnectRTPServer(in ConnectRTPServerRequest) (*ConnectRTPServerResponse, error) {
	return e.ConnectRTPServerContext(context.Background(), in)
}

func (e *Engine) ConnectRTPServerContext(ctx context.Context, in ConnectRTPServerRequest) (*ConnectRTPServerResponse, error) {
	body, err := struct2map(in)
	if err != nil {
		return nil, err
	}
	var resp ConnectRTPServerResponse
	if err := e.postContext(ctx, connectRtpServer, body, &resp); err != nil {
		return nil, err
	}
	if err := e.ErrHandle(resp.Code, resp.Msg); err != nil {
		return nil, err
	}
	return &resp, nil
}

type OpenRTPServerResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Port int    `json:"port"` // 接收端口，方便获取随机端口号
}

// StartSendRTPTalkRequest 复用接收设备 RTP 的链路反向发送对讲音频。
type StartSendRTPTalkRequest struct {
	Vhost        string `json:"vhost"`
	App          string `json:"app"`
	Stream       string `json:"stream"`
	SSRC         string `json:"ssrc"`
	RecvStreamID string `json:"recv_stream_id"`
	Type         int    `json:"type"`
	PT           int    `json:"pt"`
	OnlyAudio    bool   `json:"only_audio"`
}

// StartSendRTPTalk 使用已建立的 RTP 接收连接发送双向对讲音频。
func (e *Engine) StartSendRTPTalk(in StartSendRTPTalkRequest) (*StartSendRTPResponse, error) {
	return e.StartSendRTPTalkContext(context.Background(), in)
}

func (e *Engine) StartSendRTPTalkContext(ctx context.Context, in StartSendRTPTalkRequest) (*StartSendRTPResponse, error) {
	body, err := struct2map(in)
	if err != nil {
		return nil, err
	}
	var resp StartSendRTPResponse
	if err := e.postContext(ctx, startSendRtpTalk, body, &resp); err != nil {
		return nil, err
	}
	if err := e.ErrHandle(resp.Code, resp.Msg); err != nil {
		return nil, err
	}
	return &resp, nil
}

type OpenRTPServerRequest struct {
	Port     int    `json:"port"`               // 接收端口，0 则为随机端口
	TCPMode  int8   `json:"tcp_mode"`           // 0 udp 模式，1 tcp 被动模式, 2 tcp 主动模式。 (兼容 enable_tcp 为 0/1)
	StreamID string `json:"stream_id"`          // 该端口绑定的流 ID，该端口只能创建这一个流(而不是根据 ssrc 创建多个)
	SSRC     uint64 `json:"ssrc"`               // 防串流：非零时 ZLM 只接收匹配该 SSRC 的 RTP 包，0 则不过滤
	TCPRTCP  bool   `json:"tcp_rtcp,omitempty"` // 在协商的 RFC 4571 TCP 连接中复用 RTP/RTCP；需使用 Owl 补丁版 ZLM
}

// OpenRTPServer 创建 GB28181 RTP 接收端口，如果该端口接收数据超时，则会自动被回收(不用调用 closeRtpServer 接口)
// https://docs.zlmediakit.com/zh/guide/media_server/restful_api.html#_24%E3%80%81-index-api-openrtpserver
func (e *Engine) OpenRTPServer(in OpenRTPServerRequest) (*OpenRTPServerResponse, error) {
	return e.OpenRTPServerContext(context.Background(), in)
}

func (e *Engine) OpenRTPServerContext(ctx context.Context, in OpenRTPServerRequest) (*OpenRTPServerResponse, error) {
	body, err := struct2map(in)
	if err != nil {
		return nil, err
	}
	var resp OpenRTPServerResponse
	if err := e.postContext(ctx, openRtpServer, body, &resp); err != nil {
		return nil, err
	}
	if err := e.ErrHandle(resp.Code, resp.Msg); err != nil {
		return nil, err
	}
	return &resp, nil
}

type CloseRTPServerRequest struct {
	StreamID string `json:"stream_id"` // 调用 openRtpServer 接口时提供的流 ID
}

type CloseRTPServerResponse struct {
	Code int `json:"code"`
	Hit  int `json:"hit"` // 是否找到记录并关闭
}

// CloseRTPServer 关闭 GB28181 RTP 接收端口
// https://docs.zlmediakit.com/zh/guide/media_server/restful_api.html#_25%E3%80%81-index-api-closertpserver
func (e *Engine) CloseRTPServer(in CloseRTPServerRequest) (*CloseRTPServerResponse, error) {
	return e.CloseRTPServerContext(context.Background(), in)
}

func (e *Engine) CloseRTPServerContext(ctx context.Context, in CloseRTPServerRequest) (*CloseRTPServerResponse, error) {
	body, err := struct2map(in)
	if err != nil {
		return nil, err
	}
	var resp CloseRTPServerResponse
	if err := e.postContext(ctx, closeRtpServer, body, &resp); err != nil {
		return nil, err
	}
	if err := e.ErrHandle(resp.Code, "rtp close err"); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StartSendRTPRequest 描述 ZLMediaKit 主动发送 RTP 的参数。
// Type=1 表示 PS 负载；OnlyAudio=true 时仅将音频轨道封装进 PS。
type StartSendRTPRequest struct {
	Vhost     string `json:"vhost"`
	App       string `json:"app"`
	Stream    string `json:"stream"`
	SSRC      string `json:"ssrc"`
	DstURL    string `json:"dst_url"`
	DstPort   int    `json:"dst_port"`
	IsUDP     bool   `json:"is_udp"`
	Type      int    `json:"type"`
	PT        int    `json:"pt"`
	OnlyAudio bool   `json:"only_audio"`
	TCPRTCP   bool   `json:"tcp_rtcp,omitempty"` // TCP 发送时在同一 RFC 4571 连接中复用 RTP/RTCP
}

type StartSendRTPResponse struct {
	Code      int    `json:"code"`
	Msg       string `json:"msg"`
	LocalPort int    `json:"local_port"`
}

// StartSendRTP 从已有媒体流向指定地址发送 RTP。
func (e *Engine) StartSendRTP(in StartSendRTPRequest) (*StartSendRTPResponse, error) {
	return e.StartSendRTPContext(context.Background(), in)
}

func (e *Engine) StartSendRTPContext(ctx context.Context, in StartSendRTPRequest) (*StartSendRTPResponse, error) {
	body, err := struct2map(in)
	if err != nil {
		return nil, err
	}
	var resp StartSendRTPResponse
	if err := e.postContext(ctx, startSendRtp, body, &resp); err != nil {
		return nil, err
	}
	if err := e.ErrHandle(resp.Code, resp.Msg); err != nil {
		return nil, err
	}
	return &resp, nil
}

type StopSendRTPRequest struct {
	Vhost  string `json:"vhost"`
	App    string `json:"app"`
	Stream string `json:"stream"`
	SSRC   string `json:"ssrc,omitempty"`
}

type StopSendRTPResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// StopSendRTP 停止指定 SSRC 的 RTP 发送任务。
func (e *Engine) StopSendRTP(in StopSendRTPRequest) (*StopSendRTPResponse, error) {
	return e.StopSendRTPContext(context.Background(), in)
}

func (e *Engine) StopSendRTPContext(ctx context.Context, in StopSendRTPRequest) (*StopSendRTPResponse, error) {
	body, err := struct2map(in)
	if err != nil {
		return nil, err
	}
	var resp StopSendRTPResponse
	if err := e.postContext(ctx, stopSendRtp, body, &resp); err != nil {
		return nil, err
	}
	if err := e.ErrHandle(resp.Code, resp.Msg); err != nil {
		return nil, err
	}
	return &resp, nil
}
