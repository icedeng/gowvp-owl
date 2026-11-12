// Package service 实现管理事务和可恢复操作；数据库锁内不做网络调用。
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/credentials"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
	"gorm.io/gorm"
)

type Service struct {
	diagnosticMu   sync.Mutex
	config         core.Config
	repo           *store.Repository
	runtime        core.RuntimePort
	files          *credentials.Files
	host           core.ManagementHost
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	wake           chan struct{}
	lastPrune      time.Time
	lastFaultCheck time.Time
}

func New(parent context.Context, config core.Config, repo *store.Repository, runtime core.RuntimePort, files *credentials.Files, host core.ManagementHost) *Service {
	ctx, cancel := context.WithCancel(parent)
	return &Service{config: config, repo: repo, runtime: runtime, files: files, host: host, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1)}
}
func (s *Service) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
			case <-s.wake:
			}
			s.runPending()
			s.checkFaults()
		}
	}()
}
func (s *Service) Close() error { s.cancel(); s.wg.Wait(); return nil }
func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func normalizeError(e error) error {
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return core.Fail(404, "NOT_FOUND", "对象不存在")
	}
	if errors.Is(e, gorm.ErrDuplicatedKey) || e != nil && (strings.Contains(e.Error(), "UNIQUE constraint failed") || strings.Contains(e.Error(), "Duplicate entry") || strings.Contains(e.Error(), "duplicate key value violates unique constraint")) {
		return core.Fail(409, "ALREADY_EXISTS", "网关 ID 或端点已存在")
	}
	return e
}
func page[T any](items []T, total int64, q core.PageQuery) core.Page[T] {
	return core.Page[T]{Items: items, Total: total, Page: q.Page, Size: q.Size, SnapshotID: uuid.NewString(), SampledAt: time.Now().UTC()}
}
func (s *Service) Get(ctx context.Context, id string) (core.GatewayView, error) {
	row, e := s.repo.Get(ctx, id)
	if e != nil {
		return core.GatewayView{}, normalizeError(e)
	}
	return s.view(ctx, row, true), nil
}

type viewBatch struct {
	snapshots   map[string]core.RuntimeSnapshot
	credentials map[string]store.Credential
	counts      map[string]core.Counts
	countsError error
}

func (s *Service) view(ctx context.Context, row store.Gateway, detail bool, batch ...viewBatch) core.GatewayView {
	r := s.runtime.Snapshot(row.ID)
	if len(batch) > 0 && batch[0].snapshots != nil {
		r = batch[0].snapshots[row.ID]
	}
	now := time.Now().UTC()
	v := core.GatewayView{Software: r.Software, AlarmError: r.AlarmError, RecoveryDurationMS: r.RecoveryDurationMS, ID: row.StableID, Name: row.Name, Group: row.GroupName, Description: row.Description, Revision: protocol.Number(row.Revision), ConfigRevision: protocol.Number(row.ConfigRevision), AppliedRevision: protocol.Number(row.AppliedRevision), AdminState: row.AdminState, ConnectionState: r.ConnectionState, HealthState: r.HealthState, PendingOperationID: row.PendingOperationID, LastSeenAt: row.LastSeenAt, ConnectedAt: r.ConnectedAt, LastError: row.LastError, Capabilities: r.Capabilities, MetricsStale: true, Snapshot: core.Snapshot{SnapshotID: uuid.NewString(), SampledAt: now, ReceivedAt: r.ReceivedAt, Stale: true}}
	if v.Capabilities == nil {
		v.Capabilities = []string{}
	}
	if detail {
		v.DesiredConfig = &row.DesiredConfig
		v.AppliedConfig = row.AppliedConfig
	}
	if r.LastSeenAt != nil {
		v.LastSeenAt = r.LastSeenAt
	}
	if r.Error != nil {
		v.LastError = r.Error
	}
	if row.DesiredConfig.CredentialID != nil {
		var c store.Credential
		var e error
		if len(batch) > 0 {
			c = batch[0].credentials[*row.DesiredConfig.CredentialID]
		} else {
			c, e = s.repo.Credential(ctx, row.ID, *row.DesiredConfig.CredentialID)
		}
		if e == nil && c.ID != "" && c.GatewayID == row.ID {
			v.Certificate = &core.Certificate{ID: c.ID, Fingerprint: c.Fingerprint, CAFingerprint: c.CAFingerprint, NotBefore: c.NotBefore, NotAfter: c.NotAfter, Status: c.State}
			if now.After(c.NotAfter) {
				v.Certificate.Status = "expired"
			}
		}
	}
	if s.host != nil {
		var counts core.Counts
		var e error
		if len(batch) > 0 && batch[0].counts != nil {
			counts, e = batch[0].counts[row.ID], batch[0].countsError
		} else {
			counts, e = s.host.GatewayCounts(ctx, row.ID, r)
		}
		if e == nil {
			v.Counts = &counts
		} else {
			v.CountsError = protocol.Failure(protocol.IOError, "绑定统计暂不可用")
		}
	} else {
		v.CountsError = protocol.Failure(protocol.CapabilityUnsupported, "宿主统计适配未装配")
	}
	if r.ReceivedAt != nil {
		v.Stale = now.Sub(*r.ReceivedAt) > time.Duration(row.DesiredConfig.HeartbeatTimeoutMS)*time.Millisecond
	}
	v.MetricsStale = r.MetricsReceivedAt == nil || now.Sub(*r.MetricsReceivedAt) > time.Duration(row.DesiredConfig.HeartbeatTimeoutMS)*time.Millisecond
	v.Metrics.RTTMS = r.RTTMS
	v.Metrics.ToABPS = r.ToABPS
	v.Metrics.ToDeviceBPS = r.ToDeviceBPS
	if r.Status != nil {
		v.Metrics.PortTotal = &r.Status.PortTotal
		v.Metrics.PortUsed = &r.Status.PortUsed
	}
	if r.Metrics != nil {
		v.Metrics.DeviceRXBytes = r.Metrics.DeviceRXBytes
		v.Metrics.DeviceTXBytes = r.Metrics.DeviceTXBytes
		v.Metrics.QueueBytes = r.Metrics.QueueBytes
		v.Metrics.CPUPercent = r.Metrics.CPUPercent
		v.Metrics.RSSBytes = r.Metrics.RSSBytes
		v.Metrics.WSSRXBytes = r.Metrics.WSSRXBytes
		v.Metrics.WSSTXBytes = r.Metrics.WSSTXBytes
	}
	return v
}
func (s *Service) List(ctx context.Context, q core.PageQuery) (core.Page[core.GatewayView], error) {
	rows, e := s.repo.All(ctx)
	if e != nil {
		return core.Page[core.GatewayView]{}, e
	}
	items := []core.GatewayView{}
	data := viewBatch{credentials: map[string]store.Credential{}}
	credentialIDs := []string{}
	snapshots := map[string]core.RuntimeSnapshot{}
	for _, row := range rows {
		snapshots[row.ID] = s.runtime.Snapshot(row.ID)
		if row.DesiredConfig.CredentialID != nil {
			credentialIDs = append(credentialIDs, *row.DesiredConfig.CredentialID)
		}
	}
	if len(credentialIDs) > 0 {
		var credentials []store.Credential
		if err := s.repo.Read(ctx, func(db *gorm.DB) error { return db.Where("id IN ?", credentialIDs).Find(&credentials).Error }); err != nil {
			return core.Page[core.GatewayView]{}, err
		}
		for _, credential := range credentials {
			data.credentials[credential.ID] = credential
		}
	}
	if host, ok := s.host.(interface {
		GatewayCountsBatch(context.Context, map[string]core.RuntimeSnapshot) (map[string]core.Counts, error)
	}); ok {
		data.counts, data.countsError = host.GatewayCountsBatch(ctx, snapshots)
	}
	data.snapshots = snapshots
	created := map[string]time.Time{}
	for _, row := range rows {
		created[row.StableID] = row.CreatedAt
		if q.Q != "" && !strings.Contains(strings.ToLower(row.Name+" "+row.StableID+" "+row.DesiredConfig.URL), strings.ToLower(q.Q)) {
			continue
		}
		if q.Group != "" && row.GroupName != q.Group || q.AdminState != "" && row.AdminState != q.AdminState {
			continue
		}
		v := s.view(ctx, row, false, data)
		if q.ConnectionState != "" && v.ConnectionState != q.ConnectionState || q.HealthState != "" && v.HealthState != q.HealthState {
			continue
		}
		items = append(items, v)
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		cmp := 0
		switch q.Sort {
		case "created_at":
			if created[a.ID].Before(created[b.ID]) {
				cmp = -1
			} else if created[a.ID].After(created[b.ID]) {
				cmp = 1
			}
		case "name":
			cmp = strings.Compare(a.Name, b.Name)
		case "device_total", "device_online":
			var x, y int64
			if a.Counts != nil {
				if q.Sort == "device_total" {
					x = a.Counts.DeviceTotal
				} else {
					x = a.Counts.DeviceOnline
				}
			}
			if b.Counts != nil {
				if q.Sort == "device_total" {
					y = b.Counts.DeviceTotal
				} else {
					y = b.Counts.DeviceOnline
				}
			}
			if x < y {
				cmp = -1
			} else if x > y {
				cmp = 1
			}
		case "last_seen_at":
			if a.LastSeenAt != nil && b.LastSeenAt != nil {
				if a.LastSeenAt.Before(*b.LastSeenAt) {
					cmp = -1
				} else if a.LastSeenAt.After(*b.LastSeenAt) {
					cmp = 1
				}
			}
		}
		if cmp == 0 {
			return a.ID < b.ID
		}
		if q.Order == "desc" {
			return cmp > 0
		}
		return cmp < 0
	})
	total := len(items)
	start := min((q.Page-1)*q.Size, total)
	end := min(start+q.Size, total)
	return page(items[start:end], int64(total), q), nil
}
func (s *Service) validate(ctx context.Context, c core.DesiredConfig, gatewayPK string, requireCredential bool) error {
	if e := c.Validate(s.config.PlatformID); e != nil {
		return core.Fail(400, "INVALID_ARGUMENT", e.Error())
	}
	if c.MaxMediaSessions > s.config.MaxMediaSessions || c.MaxTransferSessions > s.config.MaxTransferSessions {
		return core.Fail(429, "RESOURCE_EXHAUSTED", "配置超过 A 的全局额度")
	}
	if c.ProxyRef != nil {
		if _, err := transport.LoadProxy(transport.ProxyDirectory(s.config.CredentialDir), *c.ProxyRef, c.URL); err != nil {
			return core.Fail(400, "PROXY_INVALID", err.Error())
		}
	}
	if s.host != nil {
		if e := s.host.ValidateMediaServer(ctx, c.DefaultMediaServerID, c.RequiredCapabilities); e != nil {
			return e
		}
	}
	if c.CredentialID == nil {
		if requireCredential {
			return core.Fail(409, "CREDENTIAL_REQUIRED", "请先导入并选择此网关的客户端凭据")
		}
		return nil
	}
	credential, e := s.repo.Credential(ctx, gatewayPK, *c.CredentialID)
	if e != nil {
		return core.Fail(409, "CREDENTIAL_INVALID", "凭据不存在或不属于此网关")
	}
	if credential.State == "retired" || time.Now().Before(credential.NotBefore) || !time.Now().Before(credential.NotAfter) {
		return core.Fail(409, "CREDENTIAL_INVALID", "凭据尚未生效、已过期或退役")
	}
	_, e = s.files.Load(credential.CAFileRef, credential.CertFileRef, credential.KeyFileRef)
	if e != nil {
		return core.Fail(409, "CREDENTIAL_INVALID", "凭据文件无效")
	}
	return nil
}

type pending struct {
	Candidate      *core.DesiredConfig `json:"candidate,omitempty"`
	Previous       *core.DesiredConfig `json:"previous,omitempty"`
	TargetRevision int64               `json:"target_revision"`
	State          string              `json:"state"`
	Force          bool                `json:"force"`
	Reason         string              `json:"reason"`
	SessionID      string              `json:"session_id"`
}

func encode(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return string(b)
}
func digest(v any) (string, error) {
	b, e := protocol.Canonical(v)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// mutate 将幂等回执、审计与业务更新放在同一短事务中。网络操作只写 durable pending。
func (s *Service) mutate(ctx context.Context, m core.Mutation, id, kind string, request any, fn func(*gorm.DB, *store.Gateway, *store.Operation) (core.Result, error)) (core.Result, error) {
	if _, e := uuid.Parse(m.Key); e != nil || m.Actor == "" {
		return core.Result{}, core.Fail(400, "INVALID_IDEMPOTENCY_KEY", "缺少合法幂等键或操作者")
	}
	key, _ := digest([]string{m.Actor, m.Route, id, m.Key})
	hash, e := requestDigest(m, request)
	if e != nil {
		return core.Result{}, e
	}
	unlock := s.repo.LockGateways(id)
	defer unlock()
	var result core.Result
	e = s.repo.Transaction(ctx, func(tx *gorm.DB) error {
		var existing store.Operation
		if e := tx.Where("operation_key_hash = ?", key).First(&existing).Error; e == nil {
			if existing.RequestHash != hash {
				return core.Fail(409, "IDEMPOTENCY_CONFLICT", "同一幂等键已用于不同参数")
			}
			var replayErr error
			result, replayErr = replayResult(existing, id)
			return replayErr
		} else if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		row := store.Gateway{}
		if kind != "create" {
			if e := tx.Where("stable_id = ? AND deleted_at IS NULL", id).First(&row).Error; e != nil {
				return normalizeError(e)
			}
			locked, e := store.LockGateway(tx, row.ID)
			if e != nil {
				return e
			}
			row = locked
			requiresVersion := kind == "configure" || kind == "state" || kind == "delete" || kind == "certificate_rotation"
			if requiresVersion && m.ExpectedRevision == nil {
				return core.Fail(400, "IF_MATCH_REQUIRED", "缺少 If-Match 版本")
			}
			if m.ExpectedRevision != nil && *m.ExpectedRevision != row.Revision {
				return core.Conflict("管理记录已被其他操作更新")
			}
		}
		now := time.Now().UTC()
		op := store.Operation{Base: store.Base{ID: uuid.NewString(), CreatedAt: now, UpdatedAt: now}, UpdatedAt: now, Type: kind, Status: "succeeded", Step: "committed", ExpectedRevision: row.Revision, OperationKeyHash: key, RequestHash: hash, RequestSnapshot: "{}", Result: "null", Total: 1, Completed: 1, Revision: 1}
		if row.ID != "" {
			op.GatewayID = &row.ID
		}
		var e error
		result, e = fn(tx, &row, &op)
		if e != nil {
			return e
		}
		if op.Status == "accepted" {
			result = core.Result{Status: 202, Body: operation(op, row.StableID)}
		} else {
			op.Result = encode(result)
		}
		if e = tx.Create(&op).Error; e != nil {
			return e
		}
		gatewayID := op.GatewayID
		event := store.Event{Base: store.Base{ID: uuid.NewString()}, GatewayID: gatewayID, GatewayNameSnapshot: row.Name, Actor: m.Actor, EventType: kind, TraceID: op.ID, Reason: kind, Details: encode(map[string]any{"operation_id": op.ID}), OccurredAt: now}
		if kind == "release_report" || kind == "channel_diagnostic" {
			event.Details = encode(result.Body)
		}
		return tx.Create(&event).Error
	})
	if e != nil {
		if previous, replayErr := s.replay(ctx, m, id, request); replayErr == nil && previous != nil {
			return *previous, nil
		}
		return core.Result{}, normalizeError(e)
	}
	s.notify()
	return result, nil
}
func operation(op store.Operation, stable string) core.Operation {
	var result any
	if op.Result != "" && op.Result != "null" {
		_ = json.Unmarshal([]byte(op.Result), &result)
	}
	var gateway *string
	if op.GatewayID != nil {
		gateway = &stable
	}
	return core.Operation{OperationID: op.ID, GatewayID: gateway, Type: op.Type, Status: op.Status, Step: op.Step, Progress: core.Progress{Completed: op.Completed, Total: op.Total}, Revision: protocol.Number(op.Revision), CreatedAt: op.CreatedAt, UpdatedAt: op.UpdatedAt, DeadlineAt: op.DeadlineAt, Result: result, Error: op.Error}
}
func reserve(tx *gorm.DB, row *store.Gateway, op *store.Operation, p pending, timeout int) error {
	if row.PendingOperationID != nil {
		e := core.Fail(409, "OPERATION_IN_PROGRESS", "此网关已有进行中的管理操作")
		e.Details["operation_id"] = *row.PendingOperationID
		return e
	}
	if timeout == 0 {
		timeout = 300000
	}
	if timeout < 1000 || timeout > 3600000 {
		return core.Fail(400, "INVALID_ARGUMENT", "排空时限须为 1000—3600000 毫秒")
	}
	deadline := time.Now().UTC().Add(time.Duration(timeout) * time.Millisecond)
	op.GatewayID = &row.ID
	op.Status = "accepted"
	op.Step = "validate"
	op.Completed = 0
	op.Total = 4
	op.DeadlineAt = &deadline
	op.RequestSnapshot = encode(p)
	row.PendingOperationID = &op.ID
	row.Revision++
	return tx.Save(row).Error
}
func (s *Service) Create(ctx context.Context, m core.Mutation, req core.CreateRequest) (core.Result, error) {
	if result, e := s.replay(ctx, m, req.ID, req); e != nil {
		return core.Result{}, e
	} else if result != nil {
		return *result, nil
	}
	if !protocol.ValidGatewayID(req.ID) {
		return core.Result{}, core.Fail(400, "INVALID_ARGUMENT", "稳定网关 ID 无效")
	}
	if req.Config.CredentialID != nil {
		return core.Result{}, core.Fail(400, "CREDENTIAL_INVALID", "新增时请先保存草稿，再导入该网关凭据")
	}
	if e := s.validate(ctx, req.Config, "", req.Enable); e != nil {
		return core.Result{}, e
	}
	return s.mutate(ctx, m, req.ID, "create", req, func(tx *gorm.DB, row *store.Gateway, op *store.Operation) (core.Result, error) {
		var n int64
		if e := tx.Model(&store.Gateway{}).Where("stable_id = ?", req.ID).Count(&n).Error; e != nil {
			return core.Result{}, e
		}
		if n != 0 {
			return core.Result{}, core.Fail(409, "ALREADY_EXISTS", "稳定网关 ID 不可重复使用")
		}
		key, _ := core.EndpointKey(req.Config.URL)
		config := req.Config
		config.URL, _ = core.NormalizeURL(config.URL)
		*row = store.Gateway{Base: store.Base{ID: uuid.NewString()}, StableID: req.ID, Name: config.Name, GroupName: config.Group, Description: config.Description, AdminState: "draft", Revision: 1, DesiredConfig: config, EndpointActiveKey: &key}
		if e := tx.Create(row).Error; e != nil {
			return core.Result{}, e
		}
		op.GatewayID = &row.ID
		// 新建没有可引用的凭据，enable=true 会在上方验证中明确拒绝。
		return core.Result{Status: 200, Body: s.mutationView(*row)}, nil
	})
}
func (s *Service) Update(ctx context.Context, m core.Mutation, id string, req core.UpdateRequest) (core.Result, error) {
	if result, e := s.replay(ctx, m, id, req); e != nil {
		return core.Result{}, e
	} else if result != nil {
		return *result, nil
	}
	row, e := s.repo.Get(ctx, id)
	if e != nil {
		return core.Result{}, normalizeError(e)
	}
	if e = s.validate(ctx, req.Config, row.ID, row.AdminState == "enabled" || row.AdminState == "draining"); e != nil {
		return core.Result{}, e
	}
	return s.mutate(ctx, m, id, "configure", req, func(tx *gorm.DB, row *store.Gateway, op *store.Operation) (core.Result, error) {
		candidate := req.Config
		candidate.URL, _ = core.NormalizeURL(candidate.URL)
		old := row.DesiredConfig
		old.Name = candidate.Name
		old.Group = candidate.Group
		old.Description = candidate.Description
		a, _ := digest(old)
		b, _ := digest(candidate)
		if row.PendingOperationID != nil {
			return core.Result{}, core.Fail(409, "OPERATION_IN_PROGRESS", "已有待完成的管理操作")
		}
		if a == b || row.AdminState == "draft" || row.AdminState == "disabled" {
			key, _ := core.EndpointKey(candidate.URL)
			row.Name = candidate.Name
			row.GroupName = candidate.Group
			row.Description = candidate.Description
			row.DesiredConfig = candidate
			row.CredentialID = candidate.CredentialID
			row.EndpointActiveKey = &key
			row.Revision++
			if e := tx.Save(row).Error; e != nil {
				return core.Result{}, e
			}
			return core.Result{Status: 200, Body: s.mutationView(*row)}, nil
		}
		p := pending{Candidate: &candidate, Previous: &row.DesiredConfig, TargetRevision: row.ConfigRevision + 1, State: row.AdminState, Reason: "configuration_change"}
		if e := reserve(tx, row, op, p, 0); e != nil {
			return core.Result{}, e
		}
		return core.Result{}, nil
	})
}
func (s *Service) State(ctx context.Context, m core.Mutation, id string, req core.StateRequest) (core.Result, error) {
	if result, e := s.replay(ctx, m, id, req); e != nil {
		return core.Result{}, e
	} else if result != nil {
		return *result, nil
	}
	if req.State != "enabled" && req.State != "draining" && req.State != "disabled" {
		return core.Result{}, core.Fail(400, "INVALID_ARGUMENT", "状态值无效")
	}
	row, e := s.repo.Get(ctx, id)
	if e != nil {
		return core.Result{}, normalizeError(e)
	}
	if req.State == "enabled" {
		if e = s.validate(ctx, row.DesiredConfig, row.ID, true); e != nil {
			return core.Result{}, e
		}
	}
	return s.mutate(ctx, m, id, "state", req, func(tx *gorm.DB, row *store.Gateway, op *store.Operation) (core.Result, error) {
		if req.Force && req.State == "disabled" && row.PendingOperationID != nil {
			var previous store.Operation
			if e := tx.Where("id = ? AND gateway_id = ?", *row.PendingOperationID, row.ID).First(&previous).Error; e != nil {
				return core.Result{}, e
			}
			if previous.Type == "state" && previous.Status == "waiting_external" {
				previous.Status = "cancelled"
				previous.Step = "superseded_by_force_disable"
				previous.Revision++
				if e := tx.Save(&previous).Error; e != nil {
					return core.Result{}, e
				}
				row.PendingOperationID = nil
			}
		}
		p := pending{State: req.State, Force: req.Force, Reason: "administrator_state_change", TargetRevision: row.ConfigRevision}
		if req.State == "enabled" {
			row.AdminState = "enabled"
			if row.ConfigRevision == 0 {
				p.TargetRevision = 1
			}
			p.Candidate = &row.DesiredConfig
		} else if row.AdminState != "draft" && row.AdminState != "disabled" {
			row.AdminState = "draining"
		}
		if e := reserve(tx, row, op, p, req.DrainTimeoutMS); e != nil {
			return core.Result{}, e
		}
		return core.Result{}, nil
	})
}
func (s *Service) Reconnect(ctx context.Context, m core.Mutation, id string, req core.ReasonRequest) (core.Result, error) {
	return s.mutate(ctx, m, id, "reconnect", req, func(tx *gorm.DB, row *store.Gateway, op *store.Operation) (core.Result, error) {
		if row.AdminState != "enabled" {
			return core.Result{}, core.Fail(409, "GATEWAY_DISABLED", "仅可重连已启用网关")
		}
		e := reserve(tx, row, op, pending{State: "enabled", Reason: req.Reason}, 0)
		return core.Result{}, e
	})
}
func (s *Service) Delete(ctx context.Context, m core.Mutation, id string) (core.Result, error) {
	return s.mutate(ctx, m, id, "delete", struct{}{}, func(tx *gorm.DB, row *store.Gateway, op *store.Operation) (core.Result, error) {
		if row.AdminState != "draft" && row.AdminState != "disabled" {
			return core.Result{}, core.Fail(409, "GATEWAY_ENABLED", "请先停用网关")
		}
		if e := store.RequireNoReferences(tx, row.ID); e != nil {
			return core.Result{}, e
		}
		now := time.Now().UTC()
		row.DeletedAt = &now
		row.EndpointActiveKey = nil
		row.Revision++
		if e := tx.Save(row).Error; e != nil {
			return core.Result{}, e
		}
		return core.Result{Status: 200, Body: map[string]any{"id": id, "deleted": true}}, nil
	})
}
func (s *Service) GetOperation(ctx context.Context, id, operationID string) (core.Operation, error) {
	row, e := s.repo.Get(ctx, id)
	if e != nil {
		return core.Operation{}, normalizeError(e)
	}
	op, e := s.repo.Operation(ctx, row.ID, operationID)
	if e != nil {
		return core.Operation{}, normalizeError(e)
	}
	return operation(op, id), nil
}
func (s *Service) ImportCredential(ctx context.Context, m core.Mutation, id string, ca, cert, key []byte) (core.Result, error) {
	defer clear(key)
	hasher := sha256.New()
	hasher.Write(ca)
	hasher.Write(cert)
	hasher.Write(key)
	request := hex.EncodeToString(hasher.Sum(nil))
	if result, e := s.replay(ctx, m, id, request); e != nil {
		return core.Result{}, e
	} else if result != nil {
		return *result, nil
	}
	row, e := s.repo.Get(ctx, id)
	if e != nil {
		return core.Result{}, normalizeError(e)
	}
	bundle, e := s.files.Import(ca, cert, key)
	if e != nil {
		return core.Result{}, core.Fail(400, "CREDENTIAL_INVALID", e.Error())
	}
	keep := false
	defer func() {
		if !keep {
			_ = s.files.Remove(bundle)
		}
	}()
	result, e := s.mutate(ctx, m, id, "credential_import", request, func(tx *gorm.DB, g *store.Gateway, op *store.Operation) (core.Result, error) {
		if g.ID != row.ID {
			return core.Result{}, core.Conflict("网关已变化")
		}
		c := store.Credential{Base: store.Base{ID: bundle.ID}, GatewayID: g.ID, CAFileRef: bundle.CARef, CertFileRef: bundle.CertRef, KeyFileRef: bundle.KeyRef, Fingerprint: bundle.Metadata.Fingerprint, CAFingerprint: bundle.Metadata.CAFingerprint, NotBefore: bundle.Metadata.NotBefore, NotAfter: bundle.Metadata.NotAfter, State: "staged", Revision: 1}
		if e := tx.Create(&c).Error; e != nil {
			return core.Result{}, e
		}
		return core.Result{Status: 200, Body: bundle.Metadata}, nil
	})
	if e == nil {
		var count int64
		_ = s.repo.Read(ctx, func(db *gorm.DB) error {
			return db.Model(&store.Credential{}).Where("id = ?", bundle.ID).Count(&count).Error
		})
		keep = count == 1
	}
	return result, e
}
func (s *Service) Rotate(ctx context.Context, m core.Mutation, id string, req core.RotationRequest) (core.Result, error) {
	if result, e := s.replay(ctx, m, id, req); e != nil {
		return core.Result{}, e
	} else if result != nil {
		return *result, nil
	}
	row, e := s.repo.Get(ctx, id)
	if e != nil {
		return core.Result{}, normalizeError(e)
	}
	candidate := row.DesiredConfig
	candidate.CredentialID = &req.NewCredentialID
	if e = s.validate(ctx, candidate, row.ID, true); e != nil {
		return core.Result{}, e
	}
	return s.mutate(ctx, m, id, "certificate_rotation", req, func(tx *gorm.DB, row *store.Gateway, op *store.Operation) (core.Result, error) {
		if row.AdminState == "draft" || row.AdminState == "disabled" {
			if row.PendingOperationID != nil {
				return core.Result{}, core.Fail(409, "OPERATION_IN_PROGRESS", "已有待完成的操作")
			}
			row.DesiredConfig = candidate
			row.CredentialID = candidate.CredentialID
			row.Revision++
			if e := tx.Save(row).Error; e != nil {
				return core.Result{}, e
			}
			return core.Result{Status: 200, Body: s.mutationView(*row)}, nil
		}
		return core.Result{}, reserve(tx, row, op, pending{Candidate: &candidate, Previous: &row.DesiredConfig, TargetRevision: row.ConfigRevision, State: row.AdminState, Reason: "certificate_rotation"}, req.DrainTimeoutMS)
	})
}
func (s *Service) Devices(ctx context.Context, id string, q core.PageQuery) (core.Page[core.DeviceIngressView], error) {
	row, e := s.repo.Get(ctx, id)
	if e != nil {
		return core.Page[core.DeviceIngressView]{}, normalizeError(e)
	}
	if s.host == nil {
		return core.Page[core.DeviceIngressView]{}, core.Fail(503, "DEPENDENCY_UNAVAILABLE", "宿主设备查询未装配")
	}
	return s.host.GatewayDevices(ctx, row.ID, s.runtime.Snapshot(row.ID), q)
}

// 写响应中的统计明确不可用；后续 GET 返回独立采样的权威统计，避免事务中跨库或等待网络。
func (s *Service) mutationView(row store.Gateway) core.GatewayView {
	r := s.runtime.Snapshot(row.ID)
	return core.GatewayView{ID: row.StableID, Name: row.Name, Group: row.GroupName, Description: row.Description, Revision: protocol.Number(row.Revision), ConfigRevision: protocol.Number(row.ConfigRevision), AppliedRevision: protocol.Number(row.AppliedRevision), DesiredConfig: &row.DesiredConfig, AppliedConfig: row.AppliedConfig, AdminState: row.AdminState, ConnectionState: r.ConnectionState, HealthState: r.HealthState, PendingOperationID: row.PendingOperationID, LastSeenAt: row.LastSeenAt, LastError: row.LastError, Capabilities: []string{}, CountsError: protocol.Failure(protocol.IOError, "写入响应未采样统计，请读取最新快照"), MetricsStale: true, Snapshot: core.Snapshot{SnapshotID: uuid.NewString(), SampledAt: time.Now().UTC(), Stale: true}}
}
