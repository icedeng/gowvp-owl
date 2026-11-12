package owl

import (
	"context"
	"errors"
	"gorm.io/gorm/clause"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/internal/core/ipc"
	"gorm.io/gorm"
)

func (h *Host) CanRestoreDirect(ctx context.Context, id string) (bool, error) {
	return h.repo.CanRestoreDirect(ctx, id)
}
func (h *Host) CommitRegistration(ctx context.Context, tx *gorm.DB, device *ipc.Device, ingress ipc.RegistrationIngress) error {
	route := core.IngressRoute{IngressType: "direct"}
	owner := ""
	if external, ok := ingress.Connection.(*connection); ok {
		if external.host != h {
			return &ipc.IngressDenied{Message: "外部连接不属于此宿主"}
		}
		route = external.ingress
		manager := h.manager()
		if manager == nil {
			return &ipc.IngressDenied{Message: "网关模块未运行"}
		}
		var epoch int64
		var owned bool
		owner, epoch, owned = manager.OwnerLease(route.GatewayPK)
		if !owned || protocol.Number(epoch) != route.OwnerEpoch {
			return &ipc.IngressDenied{Message: "网关所有权已失效"}
		}
	}
	if external, ok := ingress.Connection.(*connection); ok && external.resume != nil {
		f := external.resume
		// 与普通注册、报警接纳统一使用设备→网关→绑定的锁顺序。
		if _, err := store.LockGateway(tx, route.GatewayPK); err != nil {
			return err
		}
		var binding store.Binding
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("subject_id = ?", device.ID).First(&binding).Error; err != nil {
			return err
		}
		if err := validateResumeBinding(device.ID, route, binding, *f, time.Now()); err != nil {
			return &ipc.IngressDenied{Message: "续接凭据过期或不属于当前设备入口"}
		}
	}
	claim := store.RegistrationClaim{SubjectID: device.ID, Route: route, OwnerID: owner, NewDevice: ingress.NewDevice, Unregister: ingress.Expires == 0, ExpiresSeconds: ingress.Expires}
	commit := store.CommitRegistration
	if subjectKind(device.DeviceID) == "platform" {
		claim.PlatformRole = "downstream"
		commit = store.CommitPlatformRegistration
	}
	_, changed, e := commit(tx, claim)
	if e != nil {
		var denied *core.Error
		if errors.As(e, &denied) && (denied.Status < 500 || denied.Status == 503) {
			return &ipc.IngressDenied{Message: denied.Message, Status: denied.Status}
		}
		return e
	}
	if changed {
		device.Ext.GBRegisterCallID = ""
		device.Ext.GBRegisterCSeq = 0
		device.Ext.GBRegisterRequestFingerprint = ""
		device.Ext.GBRegisterResponseDate = ""
		device.Ext.GBRegisterResponseConfirmed = false
	}
	return nil
}

// 国标系统类型码 200 与宿主现有级联标识校验保持一致；档案仍复用原设备核心。
func subjectKind(code string) string {
	if len(code) == 20 && code[10:13] == "200" {
		for _, c := range code {
			if c < '0' || c > '9' {
				return "device"
			}
		}
		return "platform"
	}
	return "device"
}
