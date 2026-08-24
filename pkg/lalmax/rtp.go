package lalmax

import (
	"context"
	"fmt"
	"strings"
)

const (
	ctrlStartRtpPub = "/api/ctrl/start_rtp_pub"
	ctrlStopRtpPub  = "/api/ctrl/stop_rtp_pub"
)

type ApiCtrlStartRtpPubReq struct {
	StreamName      string `json:"stream_name"`
	Port            int    `json:"port"`
	PeerPort        int    `json:"peer_port"` // 对端收流端口
	TimeoutMs       int    `json:"timeout_ms"`
	IsTcpFlag       int    `json:"is_tcp_flag"`
	IsWaitKeyFrame  int    `json:"is_wait_key_frame"`
	DebugDumpPacket string `json:"debug_dump_packet"`
	IsTcpActive     bool   `json:"is_tcp_active"` // Tcp主动模式
}

type ApiCtrlStartRtpPubResp struct {
	CommonResp
	Data struct {
		StreamName string `json:"stream_name"`
		SessionId  string `json:"session_id"`
		Port       int    `json:"port"`
	} `json:"data"`
}

type ApiCtrlStopRtpPubReq struct {
	StreamName string `json:"stream_name,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
}

type ApiCtrlStopRtpPubResp struct {
	CommonResp
	Data struct {
		SessionID string `json:"session_id"`
	} `json:"data"`
}

func (e *Engine) ApiCtrlStartRtpPub(ctx context.Context, in ApiCtrlStartRtpPubReq) (*ApiCtrlStartRtpPubResp, error) {
	body, err := struct2map(in)
	if err != nil {
		return nil, err
	}
	var resp ApiCtrlStartRtpPubResp
	if err := e.post(ctx, ctrlStartRtpPub, body, &resp); err != nil {
		return nil, err
	}
	if err := resp.Err(); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ApiCtrlStopRtpPub 关闭 lalmax 的 GB28181/RTP 接收会话。
// lalmax 支持按 stream_name 或 session_id 关闭；至少需要提供其中一个。
func (e *Engine) ApiCtrlStopRtpPub(ctx context.Context, in ApiCtrlStopRtpPubReq) (*ApiCtrlStopRtpPubResp, error) {
	if strings.TrimSpace(in.StreamName) == "" && strings.TrimSpace(in.SessionID) == "" {
		return nil, fmt.Errorf("lalmax: stream_name or session_id is required")
	}
	body, err := struct2map(in)
	if err != nil {
		return nil, err
	}
	var resp ApiCtrlStopRtpPubResp
	if err := e.post(ctx, ctrlStopRtpPub, body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
