package zlm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var ErrInspectionUnsupported = errors.New("ZLM 当前版本未提供该查询接口")

type inspectionEndpoint struct {
	path     string
	list     bool
	required []string
}

// 查询端点显式列出，不接受任意路径、配置操作或调用方传入的 secret。
var inspectionEndpoints = map[string]inspectionEndpoint{
	"version":        {path: "version"},
	"streams":        {path: "getMediaList", list: true},
	"sessions":       {path: "getAllSession", list: true},
	"rtp_servers":    {path: "listRtpServer", list: true},
	"pull_proxies":   {path: "listStreamProxy", list: true},
	"push_proxies":   {path: "listStreamPusherProxy", list: true},
	"ffmpeg":         {path: "listFFmpegSource", list: true},
	"threads":        {path: "getThreadsLoad", list: true},
	"work_threads":   {path: "getWorkThreadsLoad", list: true},
	"statistics":     {path: "getStatistic"},
	"apis":           {path: "getApiList", list: true},
	"stream_info":    {path: "getMediaInfo", required: []string{"schema", "vhost", "app", "stream"}},
	"stream_players": {path: "getMediaPlayerList", list: true, required: []string{"schema", "vhost", "app", "stream"}},
	"rtp_info":       {path: "getRtpInfo", required: []string{"stream_id"}},
}

func ValidInspectionResource(resource string) bool {
	_, ok := inspectionEndpoints[resource]
	return ok
}

// Inspect 只读获取完整运行数据；超大响应显式失败，不静默截断列表。
func (e *Engine) Inspect(ctx context.Context, resource string, params map[string]string) (any, error) {
	endpoint, ok := inspectionEndpoints[resource]
	if !ok {
		return nil, fmt.Errorf("不支持的节点查询资源")
	}
	body := map[string]string{"secret": e.cfg.Secret}
	for _, key := range endpoint.required {
		if strings.TrimSpace(params[key]) == "" {
			return nil, fmt.Errorf("缺少查询参数 %s", key)
		}
		body[key] = params[key]
	}
	if resource == "rtp_info" {
		for _, key := range []string{"vhost", "app"} {
			if value := params[key]; value != "" {
				body[key] = value
			}
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.URL+"/index/api/"+endpoint.path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	client := *e.cli
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("无法连接节点查询接口")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusNotImplemented {
		return nil, ErrInspectionUnsupported
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("节点查询返回 HTTP %d", response.StatusCode)
	}
	const maxResponseBytes = 16 << 20
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取节点响应失败")
	}
	if len(payload) > maxResponseBytes {
		return nil, fmt.Errorf("节点响应超过 16 MiB，请缩小节点数据规模后重试")
	}
	var envelope map[string]any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("节点返回的内容不是有效 JSON")
	}
	code, ok := envelope["code"].(json.Number)
	if !ok {
		return nil, fmt.Errorf("节点响应缺少状态码")
	}
	if code.String() != "0" {
		// 保留错误码供排障；不直接输出可能包含源地址或凭据的上游错误原文。
		if code.String() == "-100" {
			return nil, fmt.Errorf("节点 API 鉴权失败，请检查节点连接密钥")
		}
		return nil, fmt.Errorf("节点查询失败（ZLM code=%s）", code)
	}
	data, exists := envelope["data"]
	if endpoint.list && data == nil {
		data = []any{}
	} else if !exists {
		delete(envelope, "code")
		delete(envelope, "msg")
		data = envelope
	}
	return redactInspection(data, e.cfg.Secret), nil
}

var inspectionURLPattern = regexp.MustCompile(`(?i)(?:https?|rtsps?|rtmps?|srt|webrtc)://[^\s"'<>]+`)

// 原始详情也会经过同一脱敏链路，保留地址用于定位，但隐藏 URL 认证与查询凭据。
func redactInspection(value any, secret string) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(key))
			if strings.Contains(normalized, "secret") || strings.Contains(normalized, "password") || strings.Contains(normalized, "passwd") || strings.Contains(normalized, "token") || normalized == "authorization" || normalized == "cookie" || normalized == "params" || normalized == "cmd" {
				typed[key] = "[已隐藏]"
			} else {
				typed[key] = redactInspection(item, secret)
			}
		}
	case []any:
		for index, item := range typed {
			typed[index] = redactInspection(item, secret)
		}
	case string:
		if secret != "" {
			typed = strings.ReplaceAll(typed, secret, "[已隐藏]")
		}
		return inspectionURLPattern.ReplaceAllStringFunc(typed, func(raw string) string {
			parsed, err := url.Parse(raw)
			if err != nil {
				return "[地址已隐藏]"
			}
			if parsed.User != nil {
				parsed.User = url.User("REDACTED")
			}
			if parsed.RawQuery != "" {
				parsed.RawQuery = "REDACTED"
			}
			parsed.Fragment = ""
			return parsed.String()
		})
	}
	return value
}
