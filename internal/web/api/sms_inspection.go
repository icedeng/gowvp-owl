package api

import (
	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/zlm"
	"github.com/ixugo/goddd/pkg/reason"
)

type mediaInspectionInput struct {
	ID       string `uri:"id" binding:"required"`
	Resource string `uri:"resource" binding:"required"`
	Schema   string `form:"schema"`
	Vhost    string `form:"vhost"`
	App      string `form:"app"`
	Stream   string `form:"stream"`
	StreamID string `form:"stream_id"`
}

// 保留旧列表字段，仅屏蔽嵌入模型中的连接密钥。
type mediaServerListItem struct {
	*sms.MediaServer
	Secret string `json:"secret,omitempty"`
}

// 使用独立输出模型，节点连接密钥不会进入详情或原始诊断响应。
type mediaServerDetail struct {
	Disabled          bool                 `json:"disabled"`
	ID                string               `json:"id"`
	Type              string               `json:"type"`
	IP                string               `json:"ip"`
	HookIP            string               `json:"hook_ip"`
	SDPIP             string               `json:"sdp_ip"`
	StreamIP          string               `json:"stream_ip"`
	Ports             sms.MediaServerPorts `json:"ports"`
	Status            bool                 `json:"status"`
	LastKeepaliveAt   any                  `json:"last_keepalive_at"`
	RTPPortRange      string               `json:"rtpport_range"`
	RecordMountPath   string               `json:"record_mount_path"`
	RecordPath        string               `json:"record_path"`
	HookAliveInterval int                  `json:"hook_alive_interval"`
}

func (a SmsAPI) mediaServerDetail(c *gin.Context, in *mediaServerIDInput) (any, error) {
	server, err := a.smsCore.MediaServerSnapshot(c.Request.Context(), in.ID)
	if err != nil {
		return nil, err
	}
	return mediaServerDetail{
		Disabled: server.Disabled, ID: server.ID, Type: server.Type, IP: server.IP, HookIP: server.HookIP,
		SDPIP: server.SDPIP, StreamIP: server.StreamIP, Ports: server.Ports,
		Status: server.Status, LastKeepaliveAt: server.LastKeepaliveAt,
		RTPPortRange: server.RTPPortRange, RecordPath: server.RecordPath, RecordMountPath: server.RecordMountPath, HookAliveInterval: server.HookAliveInterval,
	}, nil
}

func (a SmsAPI) inspectMediaServer(c *gin.Context, in *mediaInspectionInput) (any, error) {
	if !zlm.ValidInspectionResource(in.Resource) {
		return nil, reason.ErrBadRequest.WithMsg("不支持的节点查询资源")
	}
	if in.Resource == "stream_info" || in.Resource == "stream_players" {
		if in.Schema == "" || in.Vhost == "" || in.App == "" || in.Stream == "" {
			return nil, reason.ErrBadRequest.WithMsg("请提供 schema、vhost、app、stream")
		}
	}
	if in.Resource == "rtp_info" && in.StreamID == "" {
		return nil, reason.ErrBadRequest.WithMsg("请提供 stream_id")
	}
	return a.smsCore.InspectMediaServer(c.Request.Context(), in.ID, in.Resource, map[string]string{
		"schema": in.Schema, "vhost": in.Vhost, "app": in.App, "stream": in.Stream, "stream_id": in.StreamID,
	})
}
