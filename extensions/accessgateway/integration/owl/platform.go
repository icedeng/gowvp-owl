package owl

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/pkg/gbs"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"gorm.io/gorm"
)

func (c *connection) GatewayID() string { return c.ingress.GatewayID }

func (h *Host) platformBinding(ctx context.Context, id string) (store.PlatformBinding, error) {
	var binding store.PlatformBinding
	err := h.db.WithContext(ctx).Where("subject_id = ?", id).First(&binding).Error
	return binding, err
}

// AuthorizePlatform 只校验已保存配置产生的路由；认证提交在 REGISTER 成功之后。
func (h *Host) AuthorizePlatform(ctx context.Context, in gbs.ExternalPlatformSpec) error {
	binding, err := h.platformBinding(ctx, in.SubjectID)
	if store.IsNotFound(err) {
		if in.Register || in.GatewayID == "" {
			return nil
		}
		return core.Fail(503, "PLATFORM_UNREGISTERED", "平台尚未注册")
	}
	if err != nil {
		return err
	}
	var target *string
	if in.GatewayID != "" {
		row, err := h.repo.Get(ctx, in.GatewayID)
		if err != nil {
			return err
		}
		target = &row.ID
	}
	same := (target == nil && binding.GatewayID == nil) || (target != nil && binding.GatewayID != nil && *target == *binding.GatewayID)
	var items []struct {
		State           string
		TargetGatewayID *string
	}
	err = h.db.WithContext(ctx).Table("access_gateway_migration_items AS i").Select("i.state, m.target_gateway_id").Joins("JOIN access_gateway_migrations AS m ON m.id=i.migration_id").Where("i.subject_type = ? AND i.subject_id = ? AND i.expected_binding_revision = ? AND i.active_subject_key IS NOT NULL AND m.expires_at > ? AND m.state IN ?", "platform", in.SubjectID, binding.BindingRevision, time.Now().UTC(), []string{"draining", "waiting_register"}).Scan(&items).Error
	if err != nil {
		return err
	}
	if same {
		if !in.Register && !in.Cleanup && len(items) > 0 {
			return core.Fail(409, "MIGRATION_DRAINING", "平台正在迁移，禁止创建新业务")
		}
		return nil
	}
	if in.Register {
		for _, item := range items {
			match := target == nil && item.TargetGatewayID == nil || target != nil && item.TargetGatewayID != nil && *target == *item.TargetGatewayID
			if !match {
				continue
			}
			if item.State == "waiting_register" {
				return nil
			}
			return core.Fail(503, "MIGRATION_DRAINING", "平台原入口正在排空，请稍后重新注册")
		}
	}
	return core.Fail(403, "INGRESS_CONFLICT", "更换平台入口需要显式迁移授权")
}

func (h *Host) OpenPlatform(ctx context.Context, in gbs.ExternalPlatformSpec) (sip.Connection, error) {
	if in.GatewayID == "" || in.SubjectID == "" || in.Target == nil {
		return nil, fmt.Errorf("缺少平台网关配置")
	}
	ip, rawPort, err := net.SplitHostPort(in.Target.String())
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || !protocol.ValidPublicIP(ip) {
		return nil, core.Fail(400, "INVALID_PLATFORM_TARGET", "网关级联目标必须是保存配置的公网地址")
	}
	h.platformMu.Lock()
	defer h.platformMu.Unlock()
	if err = h.AuthorizePlatform(ctx, in); err != nil {
		return nil, err
	}
	row, err := h.repo.Get(ctx, in.GatewayID)
	if err != nil {
		return nil, err
	}
	manager := h.manager()
	if manager == nil {
		return nil, core.Fail(503, "GATEWAY_UNAVAILABLE", "网关未启用")
	}
	snap := manager.Snapshot(row.ID)
	if row.AdminState != "enabled" || row.AppliedRevision != row.ConfigRevision || snap.HealthState != "ready" {
		return nil, core.Fail(503, "GATEWAY_UNAVAILABLE", "级联网关尚未就绪")
	}
	owner, epoch, owned := manager.OwnerLease(row.ID)
	if !owned {
		return nil, protocol.Failure(protocol.OwnerConflict, "当前实例不是 owner")
	}
	binding, err := h.platformBinding(ctx, in.SubjectID)
	missing := store.IsNotFound(err)
	if err != nil && !missing {
		return nil, err
	}
	revision := int64(1)
	if !missing {
		revision = binding.BindingRevision
		if binding.GatewayID == nil || *binding.GatewayID != row.ID || binding.LastGeneration == nil || *binding.LastGeneration != snap.Generation || binding.LastConnectionEpoch == nil || protocol.Number(*binding.LastConnectionEpoch) != snap.Epoch {
			revision++
		}
	}
	// 在发起外部动作前再次核对 owner 和期望配置，避免配置/所有权切换间隙发包。
	err = h.repo.Transaction(ctx, func(tx *gorm.DB) error {
		current, err := store.LockGateway(tx, row.ID)
		if err != nil {
			return err
		}
		if err = store.CheckOwner(tx, row.ID, owner, epoch); err != nil {
			return err
		}
		if current.ConfigRevision != row.ConfigRevision || current.AppliedRevision != row.ConfigRevision || current.AdminState != "enabled" {
			return core.Conflict("网关配置已改变")
		}
		if !in.Register && !in.Cleanup {
			return store.CheckBusinessBinding(tx, "platform", in.SubjectID, row.ID, binding.BindingRevision)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	ref := protocol.ResourceRef{Generation: snap.Generation, ConnectionEpoch: snap.Epoch}
	result, err := manager.CallExact(ctx, row.ID, ref, "sip.flow.open", protocol.FlowOpen{OperationID: uuid.NewString(), ConfiguredPlatform: true, SubjectType: "platform", SubjectID: in.SubjectID, Target: protocol.Endpoint{IP: ip, Port: port}, Transport: in.Transport, BindingRevision: protocol.Number(revision)})
	if err != nil {
		return nil, err
	}
	route := result.(*protocol.FlowOpenResult).Route
	if in.Register && !missing && revision == binding.BindingRevision && (binding.LastFlowID == nil || *binding.LastFlowID != route.FlowID) {
		revision++
	}
	_, err = manager.CallExact(ctx, row.ID, ref, "sip.binding.update", protocol.BindingUpdate{OperationID: uuid.NewString(), Action: "bind", Binding: protocol.Binding{SubjectType: "platform", SubjectID: in.SubjectID, FlowID: route.FlowID, BindingRevision: protocol.Number(revision), ExpiresInMS: max(90000, min(86400000, in.Expires*1000))}})
	if err != nil {
		return nil, err
	}
	routeRevision := protocol.Number(revision)
	route.BindingRevision = &routeRevision
	ingress := core.IngressRoute{IngressType: "gateway", GatewayPK: row.ID, GatewayID: row.StableID, Generation: snap.Generation, ConnectionEpoch: snap.Epoch, OwnerEpoch: protocol.Number(epoch), FlowID: route.FlowID, BindingRevision: routeRevision, ConfigRevision: protocol.Number(row.ConfigRevision), SIPAdvertise: protocol.Endpoint{IP: row.DesiredConfig.PublicIP, Port: row.DesiredConfig.SIPPort}}
	c := newConnection(h, core.SignalIngress{Ingress: ingress, Route: route})
	c.platformSubject = in.SubjectID
	return c, nil
}

func (h *Host) CommitPlatformRegistration(ctx context.Context, in gbs.ExternalPlatformSpec, conn sip.Connection, expires int) error {
	route := core.IngressRoute{IngressType: "direct"}
	owner := ""
	if external, ok := conn.(*connection); ok {
		if external.host != h || external.platformSubject != in.SubjectID || external.ingress.GatewayID != in.GatewayID {
			return core.Fail(403, "INGRESS_CONFLICT", "平台注册连接归属无效")
		}
		route = external.ingress
		manager := h.manager()
		if manager == nil {
			return core.Fail(503, "GATEWAY_UNAVAILABLE", "网关未启用")
		}
		var epoch int64
		var owned bool
		owner, epoch, owned = manager.OwnerLease(route.GatewayPK)
		if !owned || protocol.Number(epoch) != route.OwnerEpoch {
			return protocol.Failure(protocol.OwnerConflict, "网关所有权已改变")
		}
	} else if in.GatewayID != "" {
		return core.Fail(403, "INGRESS_CONFLICT", "网关平台注册缺少可信连接")
	}
	var binding store.Binding
	err := h.repo.Transaction(ctx, func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&store.PlatformBinding{}).Where("subject_id = ?", in.SubjectID).Count(&count).Error; err != nil {
			return err
		}
		var err error
		binding, _, err = store.CommitPlatformRegistration(tx, store.RegistrationClaim{SubjectID: in.SubjectID, Route: route, OwnerID: owner, NewDevice: count == 0, Unregister: expires == 0, ExpiresSeconds: expires})
		if err != nil {
			return err
		}
		if expires > 0 && route.IngressType == "gateway" && protocol.Number(binding.BindingRevision) != route.BindingRevision {
			return core.Conflict("平台注册绑定版本已改变，请重试")
		}
		return nil
	})
	if err != nil || route.IngressType == "direct" {
		return err
	}
	action := "bind"
	if expires == 0 {
		action = "unbind"
	}
	_, err = h.manager().CallExact(ctx, route.GatewayPK, protocol.ResourceRef{Generation: route.Generation, ConnectionEpoch: route.ConnectionEpoch}, "sip.binding.update", protocol.BindingUpdate{OperationID: uuid.NewString(), Action: action, Binding: protocol.Binding{SubjectType: "platform", SubjectID: in.SubjectID, FlowID: route.FlowID, BindingRevision: protocol.Number(binding.BindingRevision), ExpiresInMS: max(90000, min(86400000, expires*1000))}})
	return err
}

var _ gbs.ExternalPlatformBindingPort = (*Host)(nil)

func (h *Host) PlatformIngress(ctx context.Context, subject string) (string, string, error) {
	binding, err := h.platformBinding(ctx, subject)
	if err != nil {
		return "", "", err
	}
	gateway := ""
	if binding.GatewayID != nil {
		row, err := h.repo.GetPK(ctx, *binding.GatewayID)
		if err != nil {
			return "", "", err
		}
		gateway = row.StableID
	}
	return gateway, string(protocol.Number(binding.BindingRevision)), nil
}
