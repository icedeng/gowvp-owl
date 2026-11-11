package service

import (
	"context"
	"slices"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"gorm.io/gorm"
)

func (s *Service) Test(ctx context.Context, m core.Mutation, id string) (core.Result, error) {
	request := core.DraftTestRequest{DraftID: id}
	if result, e := s.replay(ctx, m, id, request); e != nil {
		return core.Result{}, e
	} else if result != nil {
		return *result, nil
	}
	result, e := s.Diagnose(ctx, id, m.ExpectedRevision)
	if e != nil {
		return core.Result{}, e
	}
	return s.mutate(ctx, m, id, "diagnose", request, func(tx *gorm.DB, row *store.Gateway, op *store.Operation) (core.Result, error) {
		return core.Result{Status: 200, Body: result}, nil
	})
}

// Diagnose 始终使用已保存版本并只访问 probe，不建立控制连接或分配媒体端口。
func (s *Service) Diagnose(ctx context.Context, id string, expected *int64) (core.DiagnosticResult, error) {
	row, e := s.repo.Get(ctx, id)
	if e != nil {
		return core.DiagnosticResult{}, normalizeError(e)
	}
	if expected != nil && row.Revision != *expected {
		return core.DiagnosticResult{}, core.Conflict("草稿已更新，请重新加载")
	}
	result := core.DiagnosticResult{CheckedAt: time.Now().UTC(), GatewayID: id, ConfigRevision: protocol.Number(row.ConfigRevision), Checks: []core.DiagnosticCheck{}, Capabilities: []string{}}
	check := func(name string, e error, start time.Time) {
		status, reason, msg := "passed", "OK", "检查通过"
		if e != nil {
			status, reason, msg = "failed", "DEPENDENCY_UNAVAILABLE", e.Error()
		}
		result.Checks = append(result.Checks, core.DiagnosticCheck{Name: name, Status: status, Reason: reason, Msg: msg, DurationMS: int(time.Since(start) / time.Millisecond)})
	}
	start := time.Now()
	e = s.validate(ctx, row.DesiredConfig, row.ID, true)
	check("config", e, start)
	if e != nil {
		return result, nil
	}
	start = time.Now()
	probe, e := s.probeConfig(ctx, row)
	check("identity", e, start)
	if e != nil {
		for _, name := range []string{"dns", "tcp", "tls", "mtls", "protocol", "capabilities"} {
			result.Checks = append(result.Checks, core.DiagnosticCheck{Name: name, Status: "skipped", Reason: "PREREQUISITE_FAILED", Msg: "probe 失败，无法确认此步骤"})
		}
		return result, nil
	}
	for _, name := range []string{"dns", "tcp", "tls", "mtls", "protocol"} {
		check(name, nil, start)
	}
	result.Capabilities = probe.Capabilities
	for _, capability := range row.DesiredConfig.RequiredCapabilities {
		if !slices.Contains(probe.Capabilities, capability) {
			e = core.Fail(409, "CAPABILITY_UNSUPPORTED", "B 缺少要求的能力："+capability)
			break
		}
	}
	if e == nil && probe.CooldownRemainingMS > 0 {
		e = core.Fail(409, "GATEWAY_COOLDOWN", "B 端端口仍处于重启冷却期")
	}
	check("capabilities", e, start)
	result.CanEnable = e == nil
	return result, nil
}
