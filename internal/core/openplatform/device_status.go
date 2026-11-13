package openplatform

import (
	"encoding/json"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"gorm.io/gorm"
)

var alarmNotificationTypes = []string{"event.created", "event.evidence.updated", "event.status.changed"}
var deviceNotificationTypes = []string{"device.online", "device.offline"}

func isDeviceNotification(kind string) bool { return Has(deviceNotificationTypes, kind) }

func subscriptionAccepts(sub Subscription, kind string) bool {
	if len(sub.EventTypes) == 0 {
		// 历史订阅没有类型字段，继续只接收原有三类告警通知。
		return Has(alarmNotificationTypes, kind)
	}
	return Has(sub.EventTypes, kind)
}

// EnqueueDeviceStatusChange 由 IPC 存储在已加锁的设备事务内调用，不依赖 HTTP 服务启动时序。
// Outbox 自增 ID 作为通知序号；同设备写入由设备行锁串行，接收方可据此忽略迟到通知。
func EnqueueDeviceStatusChange(tx *gorm.DB, before bool, device *ipc.Device, reason string) error {
	if before == device.IsOnline {
		return nil
	}
	kind := "device.offline"
	if device.IsOnline {
		kind = "device.online"
	}
	// CreatedAt 沿用 GORM 的数据库时区，避免 SQLite 的时间文本比较漏掉订阅；正文统一 UTC。
	now := tx.NowFunc()
	payload, err := json.Marshal(struct {
		DeviceID       string    `json:"device_id"`
		DeviceCode     string    `json:"device_code"`
		DeviceName     string    `json:"device_name"`
		DeviceType     string    `json:"device_type"`
		PreviousOnline bool      `json:"previous_is_online"`
		IsOnline       bool      `json:"is_online"`
		OccurredAt     time.Time `json:"occurred_at"`
		Reason         string    `json:"reason"`
	}{device.ID, device.DeviceID, device.GetName(), device.GetType(), before, device.IsOnline, now.UTC(), reason})
	if err != nil {
		return err
	}
	return tx.Create(&Outbox{EventType: kind, DeviceID: device.ID, Payload: string(payload), CreatedAt: now}).Error
}
