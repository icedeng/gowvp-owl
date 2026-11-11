package owl

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"gorm.io/gorm"
)

// AuthorizeInbound 也保护 direct 入口，防止网关设备从其他入口的 MESSAGE/NOTIFY 恢复旧绑定。
func (h *Host) AuthorizeInbound(request *sip.Request) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	from, ok := request.From()
	if !ok || from.Address == nil || from.Address.User() == nil {
		return &ipc.IngressDenied{Message: "缺少设备身份"}
	}
	code := from.Address.User().String()
	if h.sip != nil {
		if recognized, err := h.sip.AuthorizeExternalPlatform(code, request.GetConnection()); recognized {
			if err != nil {
				return &ipc.IngressDenied{Message: err.Error()}
			}
			if spec, ok := h.sip.ExternalPlatformSpec(code); ok {
				spec.Cleanup = request.IsAck() || request.Method() == sip.MethodBYE || request.Method() == sip.MethodCancel || request.Method() == sip.MethodInfo
				if err := h.AuthorizePlatform(ctx, spec); err != nil {
					var denied *core.Error
					if errors.As(err, &denied) && denied.Reason == "MIGRATION_DRAINING" {
						return &ipc.IngressDenied{Message: denied.Message, Status: 503}
					}
					return err
				}
				if external, ok := request.GetConnection().(*connection); ok {
					binding, err := h.platformBinding(ctx, spec.SubjectID)
					if err != nil {
						return err
					}
					manager := h.manager()
					if manager == nil || !store.EffectiveBinding(store.Binding{BindingFields: binding.BindingFields}, manager.Snapshot(external.ingress.GatewayPK)) || external.route.BindingRevision == nil || *external.route.BindingRevision != protocol.Number(binding.BindingRevision) {
						return &ipc.IngressDenied{Message: "平台必须在当前入口重新注册", Status: 503}
					}
				}
			}
			return nil
		}
	}
	var device ipc.Device
	e := h.db.WithContext(ctx).Select("id").Where("device_id = ?", code).First(&device).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		if strings.EqualFold(request.Method(), sip.MethodRegister) {
			return nil
		}
		if sip.IsExternal(request.GetConnection()) {
			return &ipc.IngressDenied{Message: "外部设备必须先注册"}
		}
		return nil
	}
	if e != nil {
		return e
	}
	binding, e := h.repo.Binding(ctx, device.ID)
	if store.IsNotFound(e) {
		binding = store.Binding{BindingFields: store.BindingFields{SubjectID: device.ID, IngressType: "direct", BindingRevision: 1}}
	} else if e != nil {
		return e
	}
	external, isGateway := request.GetConnection().(*connection)
	same := !isGateway && binding.IngressType == "direct"
	if isGateway {
		same = binding.GatewayID != nil && *binding.GatewayID == external.ingress.GatewayPK
	}
	if strings.EqualFold(request.Method(), sip.MethodRegister) {
		if same {
			return nil
		}
		var count int64
		q := h.db.WithContext(ctx).Table("access_gateway_migration_items AS i").Joins("JOIN access_gateway_migrations AS m ON m.id = i.migration_id").Where("i.subject_type = ? AND i.subject_id = ? AND i.expected_binding_revision = ? AND i.state IN ? AND i.active_subject_key IS NOT NULL AND m.expires_at > ? AND m.state IN ?", subjectKind(code), device.ID, binding.BindingRevision, []string{"waiting_register", "draining"}, time.Now().UTC(), []string{"waiting_register", "draining"})
		if isGateway {
			q = q.Where("m.target_gateway_id = ?", external.ingress.GatewayPK)
		} else {
			q = q.Where("m.target_gateway_id IS NULL")
		}
		if e = q.Count(&count).Error; e != nil {
			return e
		}
		if count == 1 {
			var draining int64
			if e = q.Where("i.state = ?", "draining").Count(&draining).Error; e != nil {
				return e
			}
			if draining > 0 {
				return &ipc.IngressDenied{Message: "原入口正在排空，请稍后重新注册", Status: 503}
			}
			if h.sip != nil {
				h.sip.RetireServerReplay(request)
			}
			return nil
		}
		return &ipc.IngressDenied{Message: "跨入口注册需要显式迁移"}
	}
	if !same {
		return &ipc.IngressDenied{Message: "请求不属于设备当前入口"}
	}
	if !isGateway {
		return nil
	}
	manager := h.manager()
	if manager == nil {
		return &ipc.IngressDenied{Message: "网关不可达"}
	}
	external.mu.RLock()
	route := external.route
	external.mu.RUnlock()
	if !store.EffectiveBinding(binding, manager.Snapshot(external.ingress.GatewayPK)) || binding.LastFlowID == nil || *binding.LastFlowID != route.FlowID || route.BindingRevision == nil || *route.BindingRevision != protocol.Number(binding.BindingRevision) {
		return &ipc.IngressDenied{Message: "设备尚未在当前网关代次重新认证"}
	}
	return nil
}
