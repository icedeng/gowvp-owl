package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// checkFaults 周期采样，持久防抖状态在 A 重启后仍然有效。
func (s *Service) checkFaults() {
	if time.Since(s.lastFaultCheck) < 10*time.Second {
		return
	}
	s.lastFaultCheck = time.Now()
	host, ok := s.host.(interface {
		GatewayFault(context.Context, string, string, string, string, bool) error
	})
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
	defer cancel()
	rows, err := s.repo.All(ctx)
	if err != nil {
		return
	}
	for _, row := range rows {
		if row.AdminState == "enabled" {
			if _, _, owned := s.runtime.OwnerLease(row.ID); !owned {
				continue
			}
		}
		snapshot := s.runtime.Snapshot(row.ID)
		now := time.Now().UTC()
		signals := map[string]bool{}
		// 监测源暂不可用时保留上次故障；停用则显式收敛全部告警。
		if row.AdminState != "enabled" {
			for kind := range faultLabels {
				signals[kind] = false
			}
		}
		enabled := row.AdminState == "enabled"
		signals["disconnected"] = enabled && snapshot.ConnectionState != "connected"
		signals["alarm_backlog"] = enabled && (snapshot.AlarmError != nil || snapshot.Software != nil && (snapshot.Software.AlarmPending >= 3072 || snapshot.Software.AlarmStoreError))
		fresh := snapshot.StatusReceivedAt != nil && now.Sub(*snapshot.StatusReceivedAt) < time.Duration(row.DesiredConfig.HeartbeatTimeoutMS)*time.Millisecond
		if enabled && fresh && snapshot.Status != nil {
			signals["cleanup_backlog"] = snapshot.Status.CleanupPending > 0
			signals["quota"] = snapshot.Status.PortTotal > 0 && snapshot.Status.PortUsed*10 >= snapshot.Status.PortTotal*9
		}
		if row.DesiredConfig.CredentialID != nil {
			c, e := s.repo.Credential(ctx, row.ID, *row.DesiredConfig.CredentialID)
			if e == nil {
				signals["certificate_expiring"] = enabled && c.NotAfter.Before(now.Add(30*24*time.Hour))
			}
		}
		// 只统计最近实际失败，不因历史失败永久告警。
		var failed int64
		e := s.repo.Read(ctx, func(db *gorm.DB) error {
			return db.Model(&store.Session{}).Where("gateway_id = ? AND error IS NOT NULL AND updated_at > ?", row.ID, now.Add(-5*time.Minute)).Count(&failed).Error
		})
		if e == nil {
			signals["media_failure"] = enabled && failed > 0
		}
		if snapshot.Metrics != nil && snapshot.MetricsReceivedAt != nil && now.Sub(*snapshot.MetricsReceivedAt) < time.Duration(row.DesiredConfig.HeartbeatTimeoutMS)*time.Millisecond {
			if q := snapshot.Metrics.QueueBytes; q != nil {
				n, _ := q.Int64()
				signals["queue_pressure"] = enabled && n >= 6<<20
			}
			toA, _ := row.DesiredConfig.MaxToABPS.Int64()
			toD, _ := row.DesiredConfig.MaxToDeviceBPS.Int64()
			if snapshot.ToABPS != nil {
				n, _ := snapshot.ToABPS.Int64()
				signals["bandwidth_quota"] = enabled && toA > 0 && n >= toA*9/10
			}
			if snapshot.ToDeviceBPS != nil {
				n, _ := snapshot.ToDeviceBPS.Int64()
				signals["bandwidth_quota"] = signals["bandwidth_quota"] || (enabled && toD > 0 && n >= toD*9/10)
			}
		}
		var disconnection store.Fault
		if e := s.repo.Read(ctx, func(db *gorm.DB) error {
			return db.Where("gateway_id = ? AND kind = ?", row.ID, "disconnected").First(&disconnection).Error
		}); e == nil {
			var times []time.Time
			_ = json.Unmarshal([]byte(disconnection.Transitions), &times)
			count := 0
			for _, at := range times {
				if now.Sub(at) < 10*time.Minute {
					count++
				}
			}
			signals["flapping"] = enabled && count >= 3
		}
		for kind, observed := range signals {
			if err := s.observeFault(ctx, row, kind, observed, now, host.GatewayFault); err != nil {
				slog.Warn("网关故障状态未提交，将重试", "gateway", row.StableID, "kind", kind, "error", err)
			}
		}
	}
}

var faultLabels = map[string]string{"disconnected": "网关控制连接中断", "flapping": "网关反复断连", "alarm_backlog": "网关报警补传积压", "cleanup_backlog": "网关资源清理积压", "quota": "网关媒体端口配额告警", "certificate_expiring": "网关证书即将到期", "media_failure": "网关媒体链路失败", "queue_pressure": "网关发送队列积压", "bandwidth_quota": "网关带宽配额告警"}

func (s *Service) observeFault(ctx context.Context, g store.Gateway, kind string, observed bool, now time.Time, notify func(context.Context, string, string, string, string, bool) error) error {
	unlock := s.repo.LockGateways(g.ID)
	defer unlock()
	var f store.Fault
	// 先持久化观察时间与事件身份，再发通知。重启或多实例重试共用同一来源键。
	err := s.repo.Transaction(ctx, func(tx *gorm.DB) error {
		if _, err := store.LockGateway(tx, g.ID); err != nil {
			return err
		}
		if g.AdminState == "enabled" && s.runtime != nil {
			owner, epoch, owned := s.runtime.OwnerLease(g.ID)
			if !owned {
				return core.Fail(409, "OWNER_CONFLICT", "网关故障监测所有权已换代")
			}
			if err := store.CheckOwner(tx, g.ID, owner, epoch); err != nil {
				return err
			}
		}
		initial := store.Fault{GatewayID: g.ID, Kind: kind, Observed: observed, ObservedSince: now, Transitions: "[]"}
		if kind == "disconnected" && observed {
			data, _ := json.Marshal([]time.Time{now})
			initial.Transitions = string(data)
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&initial).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("gateway_id = ? AND kind = ?", g.ID, kind).First(&f).Error; err != nil {
			return err
		}
		if f.Observed != observed {
			f.Observed = observed
			f.ObservedSince = now
			if kind == "disconnected" && observed {
				var times []time.Time
				_ = json.Unmarshal([]byte(f.Transitions), &times)
				recent := []time.Time{}
				for _, at := range times {
					if now.Sub(at) < 10*time.Minute {
						recent = append(recent, at)
					}
				}
				recent = append(recent, now)
				body, _ := json.Marshal(recent)
				f.Transitions = string(body)
			}
		}
		delay := 30 * time.Second
		if !observed {
			delay = 20 * time.Second
		}
		if f.Active != observed && now.Sub(f.ObservedSince) >= delay && observed && f.IncidentID == "" {
			f.IncidentID = uuid.NewString()
		}
		return tx.Save(&f).Error
	})
	if err != nil {
		return err
	}
	delay := 30 * time.Second
	if !observed {
		delay = 20 * time.Second
	}
	if (f.Active != observed || !observed && f.IncidentID != "") && now.Sub(f.ObservedSince) >= delay {
		// 创建回执丢失后可能已恢复：先幂等补齐首次事件，再收敛同一事件，避免遗留待处理故障。
		if !observed && !f.Active && f.IncidentID != "" {
			if err := notify(ctx, g.StableID, g.Name, f.IncidentID, faultLabels[kind], false); err != nil {
				return err
			}
		}
		if err := notify(ctx, g.StableID, g.Name, f.IncidentID, faultLabels[kind], !observed); err != nil {
			return err
		}
		updates := map[string]any{"active": observed}
		if !observed {
			updates["incident_id"] = ""
		}
		err = s.repo.Transaction(ctx, func(tx *gorm.DB) error {
			return tx.Model(&store.Fault{}).Where("gateway_id = ? AND kind = ? AND observed = ? AND observed_since = ? AND incident_id = ?", g.ID, kind, observed, f.ObservedSince, f.IncidentID).Updates(updates).Error
		})
		if err != nil {
			return err
		}
	}
	return nil
}
