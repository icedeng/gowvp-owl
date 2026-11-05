package sms

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/zlm"
)

type InspectionResult struct {
	Resource  string    `json:"resource"`
	Status    string    `json:"status"` // ok / unsupported / unavailable
	Message   string    `json:"message,omitempty"`
	Data      any       `json:"data"`
	FetchedAt time.Time `json:"fetched_at"`
}

// MediaServerSnapshot 使用数据库配置与内存心跳的快照，读取不会重连或下发配置。
func (c Core) MediaServerSnapshot(ctx context.Context, id string) (*MediaServer, error) {
	server, err := c.GetMediaServer(ctx, id)
	if err != nil {
		return nil, err
	}
	server.Status = false
	if state, ok := c.cacheServers.Load(id); ok && !server.Disabled {
		online, updatedAt, _ := state.status()
		server.Status = online
		if !updatedAt.IsZero() {
			server.LastKeepaliveAt.Time = updatedAt
		}
	}
	return server, nil
}

func (c Core) InspectMediaServer(ctx context.Context, id, resource string, params map[string]string) (*InspectionResult, error) {
	if !zlm.ValidInspectionResource(resource) {
		return nil, fmt.Errorf("不支持的节点查询资源")
	}
	server, err := c.MediaServerSnapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	result := &InspectionResult{Resource: resource, Status: "unsupported", FetchedAt: time.Now()}
	if server.Type != "" && !strings.EqualFold(server.Type, ProtocolZLMediaKit) {
		result.Message = "当前媒体驱动暂未提供此节点查询能力"
		return result, nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	driver, ok := c.drivers[ProtocolZLMediaKit].(*ZLMDriver)
	if !ok {
		return nil, fmt.Errorf("ZLM 查询驱动不可用")
	}
	engine := driver.withConfig(server)
	data, err := engine.Inspect(queryCtx, resource, params)
	result.FetchedAt = time.Now()
	if errors.Is(err, zlm.ErrInspectionUnsupported) {
		result.Message = "当前节点版本或编译选项未提供该查询接口"
	} else if err != nil {
		result.Status = "unavailable"
		result.Message = err.Error()
	} else {
		result.Status = "ok"
		result.Data = data
	}
	return result, nil
}
