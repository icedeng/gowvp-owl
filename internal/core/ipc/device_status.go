package ipc

import (
	"context"

	"gorm.io/gorm"
)

// DeviceStatusChangeHandler 随设备状态事务写入通知，失败必须回滚状态，交由协议层重试。
type DeviceStatusChangeHandler func(tx *gorm.DB, before bool, device *Device, reason string) error

type DeviceStatusChangeRecorder interface {
	RecordDeviceStatusChange(tx *gorm.DB, before bool, device *Device, reason string) error
}

type deviceStatusReasonKey struct{}
type suppressDeviceStatusKey struct{}

func WithDeviceStatusReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, deviceStatusReasonKey{}, reason)
}

func DeviceStatusReason(ctx context.Context) string {
	reason, _ := ctx.Value(deviceStatusReasonKey{}).(string)
	if reason == "" {
		return "status_changed"
	}
	return reason
}

// WithoutDeviceStatusNotification 仅用于启动清理，不能把宿主进程恢复误报成设备故障。
func WithoutDeviceStatusNotification(ctx context.Context) context.Context {
	return context.WithValue(ctx, suppressDeviceStatusKey{}, true)
}

func DeviceStatusNotificationSuppressed(ctx context.Context) bool {
	suppressed, _ := ctx.Value(suppressDeviceStatusKey{}).(bool)
	return suppressed
}
