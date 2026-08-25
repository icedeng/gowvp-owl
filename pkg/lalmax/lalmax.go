package lalmax

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

type Config struct {
	URL    string
	Secret string
}

type Engine struct {
	cfg Config
	cli *http.Client
}

func NewEngine() Engine {
	return Engine{
		cli: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        30,
				MaxIdleConnsPerHost: 30,
				MaxConnsPerHost:     100,
			},
		},
	}
}

func (e Engine) SetConfig(cfg Config) Engine {
	e.cfg = cfg
	return e
}

// SetHTTPClient 注入自定义 HTTP 客户端，供代理、链路追踪和无监听测试使用。
func (e Engine) SetHTTPClient(client *http.Client) Engine {
	if client != nil {
		e.cli = client
	}
	return e
}

func (e *Engine) endpoint(path string) (string, error) {
	u, err := url.Parse(e.cfg.URL + path)
	if err != nil {
		return "", err
	}
	if e.cfg.Secret != "" {
		query := u.Query()
		query.Set("token", e.cfg.Secret)
		u.RawQuery = query.Encode()
	}
	return u.String(), nil
}

// post 发送 POST 请求到 lalmax API
// 用法示例：e.post(ctx, "/api/path", map[string]any{"key": "value"}, &response)
func (e *Engine) post(ctx context.Context, path string, data map[string]any, out any) error {
	body, err := json.Marshal(data)
	if err != nil {
		return err
	}
	endpoint, err := e.endpoint(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// get 发送 GET 请求到 lalmax API
// 用法示例：e.get(ctx, "/api/path", &response)
func (e *Engine) get(ctx context.Context, path string, out any) error {
	endpoint, err := e.endpoint(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := e.cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}
