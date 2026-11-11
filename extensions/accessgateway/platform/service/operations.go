package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
	"gorm.io/gorm"
)

func (s *Service) runPending() {
	if time.Since(s.lastPrune) > time.Minute {
		ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
		_ = s.repo.PruneHistory(ctx)
		cancel()
		s.lastPrune = time.Now()
	}
	ops, e := s.repo.PendingOperations(s.ctx)
	if e != nil {
		return
	}
	// 每个网关最多一个独立任务，慢网关不阻塞其他入口。
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, op := range ops {
		if time.Since(op.UpdatedAt) < 2*time.Second {
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-s.ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(op store.Operation) { defer wg.Done(); defer func() { <-slots }(); s.advance(op) }(op)
	}
	wg.Wait()
}
func (s *Service) advance(op store.Operation) {
	if op.Type == "migration" {
		s.advanceMigration(op)
		return
	}
	if op.GatewayID == nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	row, e := s.repo.GetPK(ctx, *op.GatewayID)
	if e != nil {
		return
	}
	var p pending
	if e = json.Unmarshal([]byte(op.RequestSnapshot), &p); e != nil {
		_ = s.progress(ctx, row, op, "failed", "invalid_snapshot", nil, protocol.Failure(protocol.InvalidArgument, "操作快照损坏"), false)
		return
	}
	if e = s.runtime.Reconcile(ctx); e != nil {
		return
	}
	_, _, owned := s.runtime.OwnerLease(row.ID)
	if !owned {
		if (row.AdminState == "draft" || row.AdminState == "disabled") && op.Type == "state" && p.State != "enabled" {
			active, e := s.repo.ActiveSessions(ctx, row.ID)
			if e == nil && len(active) == 0 {
				_ = s.finish(ctx, row, op, p, false)
			}
		}
		return
	}
	if op.Type == "certificate_rotation" && p.Previous != nil && p.Candidate != nil {
		if op.Step == "rollback_connect" {
			snapshot := s.runtime.Snapshot(row.ID)
			if snapshot.ConnectionState != "connected" || snapshot.HealthState != "ready" {
				return
			}
			if p.State == "enabled" {
				if _, err := s.runtime.Call(ctx, row.ID, "gateway.resume", protocol.Resume{OperationID: actionID(op.ID, "rollback_resume"), ConfigRevision: protocol.Number(row.ConfigRevision)}); err != nil {
					return
				}
			}
			_ = s.withOperation(ctx, row, op, true, func(tx *gorm.DB, current *store.Gateway, latest *store.Operation) error {
				current.AdminState = p.State
				current.PendingOperationID = nil
				current.Revision++
				latest.Status = "failed"
				latest.Step = "rolled_back"
				latest.Revision++
				latest.Result = encode(map[string]any{"restored_credential_id": p.Previous.CredentialID})
				latest.Error = protocol.Failure(protocol.Forbidden, "新凭据连接失败，已恢复旧凭据并重新建连")
				current.LastError = latest.Error
				if err := tx.Save(current).Error; err != nil {
					return err
				}
				return tx.Save(latest).Error
			})
			return
		}
		switched := pointerValue(row.DesiredConfig.CredentialID) == pointerValue(p.Candidate.CredentialID) && pointerValue(p.Previous.CredentialID) != pointerValue(p.Candidate.CredentialID)
		if switched {
			snapshot := s.runtime.Snapshot(row.ID)
			expired := op.DeadlineAt != nil && time.Now().After(*op.DeadlineAt)
			rejected := snapshot.Error != nil && !snapshot.Error.Retryable
			if (snapshot.ConnectionState != "connected" || snapshot.HealthState != "ready") && (expired || rejected) {
				if err := s.withOperation(ctx, row, op, true, func(tx *gorm.DB, current *store.Gateway, latest *store.Operation) error {
					current.DesiredConfig = *p.Previous
					current.CredentialID = p.Previous.CredentialID
					latest.Status = "running"
					latest.Step = "rollback_connect"
					latest.Revision++
					latest.Error = protocol.Failure(protocol.Forbidden, "新凭据连接失败，正在恢复旧凭据")
					if err := tx.Save(current).Error; err != nil {
						return err
					}
					return tx.Save(latest).Error
				}); err == nil {
					_ = s.runtime.Reconnect(ctx, row.ID)
				}
				return
			}
			// 等待新连接完成身份/配置核对，避免暂时未就绪被当作轮换完成。
			if snapshot.ConnectionState != "connected" || snapshot.HealthState != "ready" {
				return
			}
		}
	}
	if op.Type == "session_stop" {
		e = s.stopSession(ctx, row, op, p)
		s.operationError(ctx, row, op, e)
		return
	}
	if p.State != "enabled" || op.Type == "reconnect" || op.Type == "certificate_rotation" || op.Type == "configure" {
		if e = s.drain(ctx, row, op, p); e != nil {
			s.operationError(ctx, row, op, e)
			return
		}
	}
	if p.Candidate != nil {
		// 连接凭据/目标变更必须先排空并持久化，再重新建立受 mTLS 保护的控制通道。
		old := row.DesiredConfig
		next := *p.Candidate
		connectionChanged := old.URL != next.URL || pointerValue(old.CredentialID) != pointerValue(next.CredentialID)
		if connectionChanged {
			if e = s.saveCandidate(ctx, row, op, p); e != nil {
				s.operationError(ctx, row, op, e)
				return
			}
			if e = s.runtime.Reconnect(ctx, row.ID); e != nil {
				return
			}
			return
		}
		if e = s.apply(ctx, row, op, p); e != nil {
			s.operationError(ctx, row, op, e)
			return
		}
	}
	if p.State == "enabled" {
		if _, e = s.runtime.Call(ctx, row.ID, "gateway.resume", protocol.Resume{OperationID: actionID(op.ID, "resume"), ConfigRevision: protocol.Number(max(p.TargetRevision, row.ConfigRevision))}); e != nil {
			s.operationError(ctx, row, op, e)
			return
		}
	}
	if op.Type == "reconnect" && op.Step != "reconnected" {
		if e = s.progress(ctx, row, op, "running", "reconnected", nil, nil, true); e != nil {
			return
		}
		_ = s.runtime.Reconnect(ctx, row.ID)
		return
	}
	if e = s.finish(ctx, row, op, p, true); e == nil {
		_ = s.runtime.Reconcile(ctx)
	}
}
func pointerValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
func actionID(operationID, name string) string {
	id, e := uuid.Parse(operationID)
	if e != nil {
		panic(e)
	}
	derived := uuid.NewSHA1(id, []byte(name))
	derived[6] = (derived[6] & 0x0f) | 0x40
	return derived.String()
}

var errWaiting = errors.New("waiting for resources")

func (s *Service) drain(ctx context.Context, row store.Gateway, op store.Operation, p pending) error {
	if _, e := s.runtime.Call(ctx, row.ID, "gateway.drain", protocol.Drain{OperationID: actionID(op.ID, "drain"), DeadlineMS: 300000, Reason: p.Reason}); e != nil {
		return e
	}
	active, e := s.repo.ActiveSessions(ctx, row.ID)
	if e != nil {
		return e
	}
	if p.Force {
		for _, session := range active {
			if s.host == nil {
				return core.Fail(503, "DEPENDENCY_UNAVAILABLE", "宿主清理适配未装配")
			}
			if e = s.host.StopGatewaySession(ctx, row.ID, session.ID, true); e != nil {
				return e
			}
		}
		active, e = s.repo.ActiveSessions(ctx, row.ID)
		if e != nil {
			return e
		}
	}
	remote, e := s.runtime.Call(ctx, row.ID, "resources.list", protocol.ResourcesList{ResourceType: "all", Size: 200})
	if e != nil {
		return e
	}
	resources := remote.(*protocol.ResourcesResult)
	count := 0
	for _, item := range resources.Items {
		if item.Ref.ResourceType != "flow" {
			count++
		}
	}
	if len(active) > 0 || count > 0 || resources.NextCursor != nil {
		status := "running"
		if op.DeadlineAt != nil && time.Now().After(*op.DeadlineAt) {
			status = "waiting_external"
		}
		_ = s.progress(ctx, row, op, status, "draining", map[string]any{"active_sessions": len(active), "remote_resources": count}, nil, true)
		return errWaiting
	}
	return nil
}
func (s *Service) probeConfig(ctx context.Context, row store.Gateway) (protocol.Probe, error) {
	if row.DesiredConfig.CredentialID == nil {
		return protocol.Probe{}, core.Fail(409, "CREDENTIAL_REQUIRED", "缺少凭据")
	}
	c, e := s.repo.Credential(ctx, row.ID, *row.DesiredConfig.CredentialID)
	if e != nil {
		return protocol.Probe{}, e
	}
	tlsConfig, e := s.files.Load(c.CAFileRef, c.CertFileRef, c.KeyFileRef)
	if e != nil {
		return protocol.Probe{}, core.Fail(409, "CREDENTIAL_INVALID", "客户端凭据无效")
	}
	probe, e := transport.Probe(ctx, row.DesiredConfig.URL, tlsConfig)
	if e != nil {
		return probe, e
	}
	if probe.Version != protocol.Version || probe.GatewayID != row.StableID || probe.PlatformID != s.config.PlatformID {
		return probe, core.Fail(409, "IDENTITY_MISMATCH", "远端网关或平台身份不匹配")
	}
	return probe, nil
}
func (s *Service) apply(ctx context.Context, row store.Gateway, op store.Operation, p pending) error {
	config := p.Candidate.EffectiveConfig
	hash, e := protocol.ConfigHash(config)
	if e != nil {
		return e
	}
	probe, e := s.probeConfig(ctx, row)
	if e != nil {
		return e
	}
	target := protocol.Number(p.TargetRevision)
	if probe.ConfigRevision == target && probe.ConfigSHA256 == hash {
		if e = s.confirmConfig(ctx, row, op, p); e != nil {
			return e
		}
		return s.runtime.Reconcile(ctx)
	}
	remoteRevision, e := probe.ConfigRevision.Int64()
	if e != nil {
		return e
	}
	if remoteRevision >= p.TargetRevision {
		return core.Fail(409, "CONFIG_CONFLICT", "B 已处于目标以上版本或同版本不同配置，禁止覆盖")
	}
	if remoteRevision != row.AppliedRevision {
		return core.Fail(409, "CONFIG_CONFLICT", "B 实际版本与保存的最后已应用版本不一致")
	}
	reply, e := s.runtime.Call(ctx, row.ID, "config.prepare", protocol.ConfigPrepare{OperationID: actionID(op.ID, fmt.Sprintf("prepare/%d", remoteRevision)), ExpectedRevision: probe.ConfigRevision, DesiredRevision: target, Config: config, ConfigSHA256: hash})
	if e != nil {
		return e
	}
	prepared := reply.(*protocol.ConfigPrepared)
	if prepared.RequiresDrain {
		if e = s.drain(ctx, row, op, p); e != nil {
			return e
		}
	}
	if e = s.saveCandidate(ctx, row, op, p); e != nil {
		return e
	}
	response, e := s.runtime.Call(ctx, row.ID, "config.apply", protocol.ConfigApply{OperationID: actionID(op.ID, "apply/"+prepared.PrepareID), PrepareID: prepared.PrepareID, DesiredRevision: target, ConfigSHA256: hash})
	if e != nil {
		return e
	}
	result := response.(*protocol.ConfigResult)
	if !result.Applied || result.AppliedRevision != target || result.ConfigSHA256 != hash {
		return core.Fail(409, "CONFIG_CONFLICT", "B 应用结果不匹配候选配置")
	}
	if e = s.confirmConfig(ctx, row, op, p); e != nil {
		return e
	}
	return s.runtime.Reconcile(ctx)
}
func (s *Service) withOperation(ctx context.Context, row store.Gateway, op store.Operation, requireOwner bool, fn func(*gorm.DB, *store.Gateway, *store.Operation) error) error {
	unlock := s.repo.LockGateways(row.ID)
	defer unlock()
	return s.repo.Transaction(ctx, func(tx *gorm.DB) error {
		current, e := store.LockGateway(tx, row.ID)
		if e != nil {
			return e
		}
		var latest store.Operation
		if e = tx.Where("id = ? AND gateway_id = ?", op.ID, row.ID).First(&latest).Error; e != nil {
			return e
		}
		if latest.Revision != op.Revision || latest.Status == "succeeded" || latest.Status == "failed" || latest.Status == "cancelled" {
			return core.Conflict("操作已被其他执行者推进")
		}
		if requireOwner {
			owner, epoch, ok := s.runtime.OwnerLease(row.ID)
			if !ok {
				return core.Fail(409, "STALE_EPOCH", "此进程不持有网关运行态")
			}
			if e = store.CheckOwner(tx, row.ID, owner, epoch); e != nil {
				return e
			}
		}
		if e = fn(tx, &current, &latest); e != nil {
			return e
		}
		return nil
	})
}
func (s *Service) saveCandidate(ctx context.Context, row store.Gateway, op store.Operation, p pending) error {
	return s.withOperation(ctx, row, op, true, func(tx *gorm.DB, current *store.Gateway, latest *store.Operation) error {
		next := *p.Candidate
		key, _ := core.EndpointKey(next.URL)
		current.DesiredConfig = next
		current.Name = next.Name
		current.GroupName = next.Group
		current.Description = next.Description
		current.CredentialID = next.CredentialID
		current.EndpointActiveKey = &key
		current.ConfigRevision = p.TargetRevision
		return tx.Save(current).Error
	})
}
func (s *Service) confirmConfig(ctx context.Context, row store.Gateway, op store.Operation, p pending) error {
	return s.withOperation(ctx, row, op, true, func(tx *gorm.DB, current *store.Gateway, latest *store.Operation) error {
		next := *p.Candidate
		key, _ := core.EndpointKey(next.URL)
		current.DesiredConfig = next
		current.Name = next.Name
		current.GroupName = next.Group
		current.Description = next.Description
		current.CredentialID = next.CredentialID
		current.EndpointActiveKey = &key
		current.ConfigRevision = p.TargetRevision
		current.AppliedRevision = p.TargetRevision
		current.AppliedConfig = &next.EffectiveConfig
		if next.CredentialID != nil {
			if e := tx.Model(&store.Credential{}).Where("gateway_id = ? AND state = ? AND id <> ?", row.ID, "active", *next.CredentialID).Update("state", "retired").Error; e != nil {
				return e
			}
			if e := tx.Model(&store.Credential{}).Where("id = ? AND gateway_id = ?", *next.CredentialID, row.ID).Update("state", "active").Error; e != nil {
				return e
			}
		}
		return tx.Save(current).Error
	})
}
func (s *Service) progress(ctx context.Context, row store.Gateway, op store.Operation, status, step string, result any, failure *protocol.Error, requireOwner bool) error {
	return s.withOperation(ctx, row, op, requireOwner, func(tx *gorm.DB, current *store.Gateway, latest *store.Operation) error {
		latest.Status = status
		latest.Step = step
		latest.Revision++
		latest.Error = failure
		if result != nil {
			latest.Result = encode(result)
		}
		if status == "failed" || status == "cancelled" {
			if current.PendingOperationID != nil && *current.PendingOperationID == op.ID {
				current.PendingOperationID = nil
				current.Revision++
				current.LastError = failure
				if e := tx.Save(current).Error; e != nil {
					return e
				}
			}
		}
		return tx.Save(latest).Error
	})
}
func (s *Service) finish(ctx context.Context, row store.Gateway, op store.Operation, p pending, owned bool) error {
	return s.withOperation(ctx, row, op, owned, func(tx *gorm.DB, current *store.Gateway, latest *store.Operation) error {
		if p.State != "" {
			current.AdminState = p.State
		}
		if current.PendingOperationID != nil && *current.PendingOperationID == op.ID {
			current.PendingOperationID = nil
		}
		current.Revision++
		current.LastError = nil
		latest.Status = "succeeded"
		latest.Step = "committed"
		latest.Completed = latest.Total
		latest.Revision++
		latest.Error = nil
		latest.Result = encode(map[string]any{"gateway_id": current.StableID, "revision": protocol.Number(current.Revision)})
		if e := tx.Save(current).Error; e != nil {
			return e
		}
		return tx.Save(latest).Error
	})
}
func (s *Service) operationError(ctx context.Context, row store.Gateway, op store.Operation, e error) {
	if e == nil || errors.Is(e, errWaiting) || s.ctx.Err() != nil {
		return
	}
	status := "waiting_external"
	failure := protocol.Failure(protocol.IOError, "网关操作等待依赖恢复")
	var pe *protocol.Error
	if errors.As(e, &pe) {
		failure = pe
		if !pe.Retryable {
			status = "failed"
		}
	}
	var ce *core.Error
	if errors.As(e, &ce) {
		failure = protocol.Failure(protocol.ConfigConflict, ce.Message)
		if ce.Status < 500 {
			status = "failed"
		}
	}
	_ = s.progress(ctx, row, op, status, "waiting_dependency", nil, failure, true)
}

func (s *Service) Stop(ctx context.Context, m core.Mutation, id, sessionID string, req core.ReasonRequest) (core.Result, error) {
	return s.mutate(ctx, m, id, "session_stop", struct {
		SessionID string
		Reason    string
	}{sessionID, req.Reason}, func(tx *gorm.DB, row *store.Gateway, op *store.Operation) (core.Result, error) {
		var session store.Session
		if e := tx.Where("id = ? AND gateway_id = ?", sessionID, row.ID).First(&session).Error; e != nil {
			return core.Result{}, normalizeError(e)
		}
		if session.State == "closed" {
			return core.Result{Status: 200, Body: map[string]any{"id": sessionID, "closed": true}}, nil
		}
		op.Status = "accepted"
		op.Step = "stop"
		op.Completed = 0
		op.RequestSnapshot = encode(pending{SessionID: sessionID, Reason: req.Reason})
		return core.Result{}, nil
	})
}
func (s *Service) stopSession(ctx context.Context, row store.Gateway, op store.Operation, p pending) error {
	if s.host == nil {
		return core.Fail(503, "DEPENDENCY_UNAVAILABLE", "宿主清理适配未装配")
	}
	if e := s.host.StopGatewaySession(ctx, row.ID, p.SessionID, false); e != nil {
		return e
	}
	session, e := s.repo.Session(ctx, row.ID, p.SessionID)
	if e != nil {
		return e
	}
	if session.State != "closed" {
		return errWaiting
	}
	return s.progress(ctx, row, op, "succeeded", "closed", map[string]any{"session_id": session.ID, "closed": true}, nil, true)
}
