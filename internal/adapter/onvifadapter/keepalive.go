package onvifadapter

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

const heartbeatTimeout = 70 * time.Second

func (a *Adapter) startHealthCheck(ctx context.Context) {
	go a.startHeartbeat(ctx, 30*time.Second)
	go a.startStatusChecker(ctx, time.Second, heartbeatTimeout)
}
func (a *Adapter) startHeartbeat(ctx context.Context, interval time.Duration) {
	conc.Timer(ctx, 0, interval, func() { a.heartbeatRound(ctx) })
}

// 每轮重读数据库，初始化失败和删除回滚的设备也能重试。
// 固定 8 个工作协程，等待本轮结束再计时，不叠加心跳请求。
func (a *Adapter) heartbeatRound(ctx context.Context) {
	jobs := make(chan string)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range jobs {
				if ctx.Err() != nil {
					return
				}
				a.sendHeartbeat(ctx, id)
			}
		}()
	}
	defer workers.Wait()
	defer close(jobs)
	const pageSize = 200
	after := ""
	for {
		var devices []*ipc.Device
		query := orm.NewQuery(2).Where("id > ?", after).OrderBy("id ASC")
		_, err := a.adapter.Store().Device().List(ctx, &devices, web.PagerFilter{Page: 1, Size: pageSize}, query.Encode()...)
		if err != nil {
			slog.ErrorContext(ctx, "读取 ONVIF 设备失败，下轮重试", "err", err)
			return
		}
		for _, device := range devices {
			if device.ID == "" || device.ID == after {
				slog.ErrorContext(ctx, "ONVIF 设备扫描游标未推进")
				return
			}
			after = device.ID
			if device.IsOnvif() {
				select {
				case jobs <- device.ID:
				case <-ctx.Done():
					return
				}
			}
		}
		if len(devices) < pageSize {
			return
		}
	}
}

func (a *Adapter) sendHeartbeat(ctx context.Context, id string) {
	unlock := a.lockDevice(id)
	defer unlock()
	current, runtime, err := a.loadDeviceLocked(ctx, id)
	if err != nil {
		return
	}
	if err := a.refreshDeviceLocked(ctx, current, runtime); err != nil {
		slog.WarnContext(ctx, "ONVIF 探测或目录同步失败，下轮重试", "device_id", id, "err", err)
	}
	a.checkStatusLocked(ctx, id, runtime, time.Now(), heartbeatTimeout)
}

func (a *Adapter) startStatusChecker(ctx context.Context, interval, timeout time.Duration) {
	conc.Timer(ctx, interval, interval, func() { a.checkStatuses(ctx, time.Now(), timeout) })
}
func (a *Adapter) checkStatuses(ctx context.Context, now time.Time, timeout time.Duration) {
	a.devices.Range(func(id string, _ *Device) bool {
		if ctx.Err() != nil {
			return false
		}
		mu := a.deviceLock(id)
		if !mu.TryLock() {
			return true
		} // 正在探测、编辑或删除，下轮再检查。
		defer mu.Unlock()
		// Range 对象可能刚被替换，锁内重新读取。
		if runtime, ok := a.devices.Load(id); ok {
			a.checkStatusLocked(ctx, id, runtime, now, timeout)
		}
		return true
	})
}

func (a *Adapter) checkStatusLocked(ctx context.Context, id string, runtime *Device, now time.Time, timeout time.Duration) {
	online := !runtime.KeepaliveAt.IsZero() && now.Sub(runtime.KeepaliveAt.Time) < timeout
	if runtime.statusSynced && runtime.IsOnline == online && runtime.syncedKeepalive == runtime.KeepaliveAt {
		return
	}
	if err := a.syncDeviceStatusToDB(ctx, id, runtime, online); err != nil {
		if orm.IsErrRecordNotFound(err) {
			a.devices.Delete(id)
		} else {
			slog.ErrorContext(ctx, "更新 ONVIF 状态失败，下轮重试", "device_id", id, "err", err)
		}
		return
	}
	if !runtime.statusSynced || runtime.IsOnline != online {
		slog.InfoContext(ctx, "ONVIF 状态已同步", "device_id", id, "online", online)
	}
	// 事务成功后才确认；失败保留差异，不依赖再次上下线触发。
	runtime.IsOnline = online
	runtime.syncedKeepalive = runtime.KeepaliveAt
	runtime.statusSynced = true
}

func (a *Adapter) syncDeviceStatusToDB(ctx context.Context, id string, runtime *Device, online bool) error {
	return a.adapter.Store().Device().Session(ctx, func(tx *gorm.DB) error {
		var device ipc.Device
		if err := tx.WithContext(ctx).Where("id=?", id).First(&device).Error; err != nil {
			return err
		}
		updates := map[string]any{"is_online": online}
		if !runtime.KeepaliveAt.IsZero() {
			updates["keepalive_at"] = runtime.KeepaliveAt
		}
		if err := tx.WithContext(ctx).Model(&ipc.Device{}).Where("id=?", id).Updates(updates).Error; err != nil {
			return err
		}
		channels := tx.WithContext(ctx).Model(&ipc.Channel{}).Where("did=?", id)
		if online {
			if len(runtime.profileIDs) == 0 {
				return nil
			} // 目录未获取，不凭空宣告旧通道在线。
			channels = channels.Where("channel_id IN ?", runtime.profileIDs)
		}
		return channels.Update("is_online", online).Error
	})
}
