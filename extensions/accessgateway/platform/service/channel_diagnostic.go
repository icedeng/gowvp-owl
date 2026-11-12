package service

import (
	"context"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"gorm.io/gorm"
)

type ChannelDiagnosticRequest struct {
	ChannelID string `json:"channel_id"`
}

func (s *Service) ChannelDiagnostic(ctx context.Context, m core.Mutation, id string, in ChannelDiagnosticRequest) (core.Result, error) {
	if in.ChannelID == "" || len(in.ChannelID) > 64 {
		return core.Result{}, core.Fail(400, "INVALID_ARGUMENT", "请选择此网关下的通道")
	}
	if !s.diagnosticMu.TryLock() {
		return core.Result{}, core.Fail(429, "DIAGNOSTIC_BUSY", "已有链路诊断运行中，请稍后重试")
	}
	defer s.diagnosticMu.Unlock()
	if r, err := s.replay(ctx, m, id, in); err != nil {
		return core.Result{}, err
	} else if r != nil {
		return *r, nil
	}
	row, err := s.repo.Get(ctx, id)
	if err != nil {
		return core.Result{}, err
	}
	if m.ExpectedRevision == nil || *m.ExpectedRevision != row.Revision {
		return core.Result{}, core.Conflict("网关配置已更新")
	}
	if row.AdminState != "enabled" {
		return core.Result{}, core.Fail(409, "GATEWAY_DISABLED", "请先启用网关")
	}
	host, ok := s.host.(interface {
		DiagnoseChannel(context.Context, string, string) (core.DiagnosticResult, error)
	})
	if !ok {
		return core.Result{}, core.Fail(409, "CAPABILITY_UNSUPPORTED", "宿主未提供链路诊断")
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	result, err := host.DiagnoseChannel(ctx, row.ID, in.ChannelID)
	if err != nil {
		return core.Result{}, err
	}
	result.GatewayID = id
	result.ConfigRevision = protocol.Number(row.ConfigRevision)
	result.Capabilities = s.runtime.Snapshot(row.ID).Capabilities
	if result.Capabilities == nil {
		result.Capabilities = []string{}
	}
	commitCtx, commitCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer commitCancel()
	return s.mutate(commitCtx, m, id, "channel_diagnostic", in, func(tx *gorm.DB, g *store.Gateway, op *store.Operation) (core.Result, error) {
		return core.Result{Status: 200, Body: result}, nil
	})
}
