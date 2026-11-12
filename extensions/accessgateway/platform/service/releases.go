package service

import (
	"context"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"gorm.io/gorm"
)

// ReleaseReport 由统一发布 CLI 回写，管理端只展示审计结果，不提供远程命令执行。
type ReleaseReport struct {
	ReleaseID string `json:"release_id"`
	Component string `json:"component"`
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	Outcome   string `json:"outcome"`
	Previous  string `json:"previous"`
}

func (s *Service) ReportRelease(ctx context.Context, m core.Mutation, id string, in ReleaseReport) (core.Result, error) {
	if !protocol.ValidGatewayID(in.ReleaseID) || len(in.Version) < 1 || len(in.Version) > 128 || !protocol.ValidHash(in.SHA256) || len(in.Previous) > 256 || (in.Component != "a" && in.Component != "b" && in.Component != "zlm") || (in.Outcome != "succeeded" && in.Outcome != "rolled_back" && in.Outcome != "rollback_failed") {
		return core.Result{}, core.Fail(400, "INVALID_ARGUMENT", "发布报告字段无效")
	}
	return s.mutate(ctx, m, id, "release_report", in, func(tx *gorm.DB, g *store.Gateway, op *store.Operation) (core.Result, error) {
		return core.Result{Status: 200, Body: in}, nil
	})
}
