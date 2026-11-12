package gbs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
)

// ResumeExternalDevice 仅供可信外部适配器续接尚未到期的已认证 flow。
// 入口事务再次校验网关、代次、绑定版本及原 flow；不接受普通设备请求调用。
func (s *Server) ResumeExternalDevice(ctx context.Context, code string, conn sip.Connection, expiresAt, lastSeen time.Time) error {
	if !sip.IsExternal(conn) || !expiresAt.After(time.Now()) || lastSeen.After(time.Now().Add(30*time.Second)) {
		return fmt.Errorf("invalid external recovery")
	}
	unlock := s.gb.lockRegisterOperation(code)
	defer unlock()
	var persisted ipc.Device
	if err := s.gb.core.Store().Device().Get(ctx, &persisted, orm.Where("device_id = ?", code)); err != nil {
		return err
	}
	device := NewDevice(conn, &persisted)
	if device == nil {
		return fmt.Errorf("invalid persisted device")
	}
	if err := s.prepareDeviceMemory(ctx, code, device); err != nil {
		return err
	}
	remaining := int(time.Until(expiresAt) / time.Second)
	if remaining < 1 {
		return fmt.Errorf("registration expired")
	}
	if err := s.changeMemory(ipc.WithRegistrationIngress(ctx, conn, remaining), code, func(d *ipc.Device) error {
		closed := false
		d.Ext.GBRegistrationClosed = &closed
		d.IsOnline = true
		d.RegisteredAt = orm.Now()
		d.Expires = remaining
		if lastSeen.After(d.KeepaliveAt.Time) {
			d.KeepaliveAt = orm.Time{Time: lastSeen}
		}
		return nil
	}, func(d *Device) {
		d.conn = conn
		d.source = conn.RemoteAddr()
		d.registrationClosed = false
		d.offlinePersistencePending = false
	}); err != nil {
		return err
	}
	if _, loaded := s.memoryStorer.Load(code); !loaded {
		if err := s.gb.core.Store().Device().Get(ctx, &persisted, orm.Where("device_id = ?", code)); err != nil {
			return err
		}
		device = NewDevice(conn, &persisted)
		if err := s.loadOrStoreDeviceMemory(ctx, code, device); err != nil {
			return err
		}
	}
	if notifier, ok := conn.(ipc.RegistrationCommitNotifier); ok {
		if err := notifier.RegistrationCommitted(ctx, code, remaining); err != nil {
			return err
		}
	}
	s.gb.deviceOfflineTombstones.Delete(code)
	s.gb.deviceDeletionTombstones.Delete(code)
	unlock()
	s.gb.signalManualSubscriptionRecovery(code)
	s.gb.startLifecycleWorker(func() {
		queryCtx, cancel := context.WithTimeout(s.gb.serviceContext(), 15*time.Second)
		defer cancel()
		// 按设备实际目录恢复通道，不能凭已认证的 flow 把全部通道标成在线。
		if err := s.gb.QueryCatalogContext(queryCtx, code); err != nil {
			slog.Warn("网关续接后目录查询失败", "device", code, "error", err)
			s.gb.scheduleCatalogRefreshAfterFailure(code, err)
		}
	})
	return nil
}
