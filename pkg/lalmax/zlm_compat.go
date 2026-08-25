package lalmax

import "context"

const (
	closeStreamsPath = "/index/api/close_streams"
	startRecordPath  = "/index/api/startRecord"
	stopRecordPath   = "/index/api/stopRecord"
)

// CloseStreamsRequest 描述 LALMAX ZLM 兼容接口的关闭流参数。
type CloseStreamsRequest struct {
	Schema string `json:"schema,omitempty"`
	Vhost  string `json:"vhost,omitempty"`
	App    string `json:"app,omitempty"`
	Stream string `json:"stream,omitempty"`
	Force  bool   `json:"force,omitempty"`
}

type CloseStreamsResponse struct {
	CommonResp
	CountHit    int `json:"count_hit"`
	CountClosed int `json:"count_closed"`
}

func (e *Engine) CloseStreams(ctx context.Context, in CloseStreamsRequest) (*CloseStreamsResponse, error) {
	body, err := struct2map(in)
	if err != nil {
		return nil, err
	}
	var resp CloseStreamsResponse
	if err := e.post(ctx, closeStreamsPath, body, &resp); err != nil {
		return nil, err
	}
	if err := resp.Err(); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StartRecordRequest 描述 LALMAX ZLM 兼容接口的录像启动参数。
type StartRecordRequest struct {
	Type       int    `json:"type"`
	Vhost      string `json:"vhost"`
	App        string `json:"app"`
	Stream     string `json:"stream"`
	CustomPath string `json:"customized_path,omitempty"`
	MaxSecond  int    `json:"max_second,omitempty"`
}

type StartRecordResponse struct {
	CommonResp
	Result bool `json:"result"`
}

func (e *Engine) StartRecord(ctx context.Context, in StartRecordRequest) (*StartRecordResponse, error) {
	body, err := struct2map(in)
	if err != nil {
		return nil, err
	}
	var resp StartRecordResponse
	if err := e.post(ctx, startRecordPath, body, &resp); err != nil {
		return nil, err
	}
	if err := resp.Err(); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StopRecordRequest 描述 LALMAX ZLM 兼容接口的录像停止参数。
type StopRecordRequest struct {
	Type   int    `json:"type"`
	Vhost  string `json:"vhost"`
	App    string `json:"app"`
	Stream string `json:"stream"`
}

type StopRecordResponse struct {
	CommonResp
	Result bool `json:"result"`
}

func (e *Engine) StopRecord(ctx context.Context, in StopRecordRequest) (*StopRecordResponse, error) {
	body, err := struct2map(in)
	if err != nil {
		return nil, err
	}
	var resp StopRecordResponse
	if err := e.post(ctx, stopRecordPath, body, &resp); err != nil {
		return nil, err
	}
	if err := resp.Err(); err != nil {
		return nil, err
	}
	return &resp, nil
}
