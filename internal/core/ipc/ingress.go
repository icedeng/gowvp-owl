package ipc

import (
	"context"
	"github.com/ixugo/goddd/pkg/orm"

	"github.com/gowvp/owl/pkg/gbs/sip"
	"gorm.io/gorm"
)

type registrationIngressKey struct{}
type RegistrationIngress struct {
	Connection sip.Connection
	Expires    int
	NewDevice  bool
}

// WithRegistrationIngress 仅在通过设备鉴权的 REGISTER 状态提交中使用。
func WithRegistrationIngress(ctx context.Context, conn sip.Connection, expires int, newDevice ...bool) context.Context {
	isNew := len(newDevice) > 0 && newDevice[0]
	return context.WithValue(ctx, registrationIngressKey{}, RegistrationIngress{Connection: conn, Expires: expires, NewDevice: isNew})
}
func RegistrationIngressFrom(ctx context.Context) (RegistrationIngress, bool) {
	v, ok := ctx.Value(registrationIngressKey{}).(RegistrationIngress)
	return v, ok
}

// IngressGuard 的实现由独立适配层注入；设备包不依赖任何网关数据表。
type IngressGuard interface {
	CommitRegistration(context.Context, *gorm.DB, *Device, RegistrationIngress) error
	CanRestoreDirect(context.Context, string) (bool, error)
}

// RegistrationCommitNotifier 在数据库提交后、REGISTER 200 之前更新外部传输的授权绑定。
type RegistrationCommitNotifier interface {
	RegistrationCommitted(context.Context, string, int) error
}

type IngressDenied struct {
	Message string
	Status  int
}

func (e *IngressDenied) Error() string { return e.Message }
func (e *IngressDenied) SIPStatusCode() int {
	if e.Status == 503 {
		return 503
	}
	return 403
}

// WithDeviceListOptions 为宿主装配层提供可选查询条件，不把扩展表结构带入原设备核心。
func WithDeviceListOptions(ctx context.Context, opts ...orm.QueryOption) context.Context {
	return context.WithValue(ctx, deviceListOptionsKey{}, opts)
}

type deviceListOptionsKey struct{}

func deviceListOptions(ctx context.Context, base []orm.QueryOption) []orm.QueryOption {
	if opts, ok := ctx.Value(deviceListOptionsKey{}).([]orm.QueryOption); ok {
		return append(base, opts...)
	}
	return base
}
