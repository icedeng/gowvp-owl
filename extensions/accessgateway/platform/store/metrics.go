package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type GaugeAggregate struct {
	Count int     `json:"count"`
	Sum   float64 `json:"sum"`
	Max   float64 `json:"max"`
}
type MetricGauges map[string]GaugeAggregate

func (g MetricGauges) Values() map[string]any {
	values := map[string]any{}
	for _, name := range []string{"cpu_percent", "rss_bytes", "port_total", "port_used", "sip_flows", "media_sessions", "transfer_sessions", "to_a_bps", "to_device_bps", "device_total", "device_online", "device_offline", "channel_total", "channel_online", "active_sources", "queue_bytes", "rtt_ms"} {
		v := g[name]
		if v.Count == 0 {
			values[name] = nil
			values[name+"_max"] = nil
		} else {
			values[name] = v.Sum / float64(v.Count)
			values[name+"_max"] = v.Max
		}
	}
	return values
}

func (r *Repository) RuntimeMetrics(ctx context.Context, pk, ownerID string, epoch int64, generation string, sample protocol.MetricsSnapshot, status *protocol.StatusSnapshot, received time.Time, rates core.Metrics, counts *core.Counts) error {
	seq, e := sample.SampleSeq.Int64()
	if e != nil {
		return e
	}
	return r.Transaction(ctx, func(tx *gorm.DB) error {
		if e := CheckOwner(tx, pk, ownerID, epoch); e != nil {
			return e
		}
		for _, step := range []int{60, 300} {
			at := received.Truncate(time.Duration(step) * time.Second)
			row := Metric{}
			e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("gateway_id = ? AND granularity_seconds = ? AND bucket_at = ?", pk, step, at).First(&row).Error
			if IsNotFound(e) {
				row = Metric{Base: Base{ID: uuid.NewString()}, GatewayID: pk, GranularitySeconds: step, BucketAt: at, Counters: "{}", Gauges: "{}"}
			} else if e != nil {
				return e
			}
			if row.LastGeneration == generation && row.LastSampleSeq >= seq {
				continue
			}
			gauges := MetricGauges{}
			if e = json.Unmarshal([]byte(row.Gauges), &gauges); e != nil {
				return e
			}
			add := func(name string, value float64) {
				g := gauges[name]
				if g.Count == 0 || value > g.Max {
					g.Max = value
				}
				g.Sum += value
				g.Count++
				gauges[name] = g
			}
			add("sip_flows", float64(sample.SIPFlows))
			add("media_sessions", float64(sample.MediaSessions))
			add("transfer_sessions", float64(sample.TransferSessions))
			for name, value := range map[string]*protocol.Decimal{"to_a_bps": rates.ToABPS, "to_device_bps": rates.ToDeviceBPS, "queue_bytes": sample.QueueBytes} {
				if value != nil {
					if n, err := value.Int64(); err == nil {
						add(name, float64(n))
					}
				}
			}
			if counts != nil {
				for name, value := range map[string]int64{"device_total": counts.DeviceTotal, "device_online": counts.DeviceOnline, "device_offline": counts.DeviceOffline, "channel_total": counts.ChannelTotal, "channel_online": counts.ChannelOnline, "active_sources": counts.ActiveSources} {
					add(name, float64(value))
				}
			}
			if rates.RTTMS != nil {
				add("rtt_ms", *rates.RTTMS)
			}
			if sample.CPUPercent != nil {
				add("cpu_percent", *sample.CPUPercent)
			}
			if sample.RSSBytes != nil {
				if n, e := sample.RSSBytes.Int64(); e == nil {
					add("rss_bytes", float64(n))
				}
			}
			if status != nil {
				add("port_total", float64(status.PortTotal))
				add("port_used", float64(status.PortUsed))
			}
			data, e := json.Marshal(gauges)
			if e != nil {
				return e
			}
			row.Gauges = string(data)
			counters, e := json.Marshal(map[string]any{"wss_rx_bytes": sample.WSSRXBytes, "wss_tx_bytes": sample.WSSTXBytes, "device_rx_bytes": sample.DeviceRXBytes, "device_tx_bytes": sample.DeviceTXBytes})
			if e != nil {
				return e
			}
			row.Counters = string(counters)
			row.SampleCount++
			row.ValidCount++
			row.LastGeneration = generation
			row.LastSampleSeq = seq
			if e = tx.Save(&row).Error; e != nil {
				return e
			}
		}
		return nil
	})
}

// PruneHistory 每次只删除有界批次，保留未结束操作及所有绑定/网关 ID。
func (r *Repository) PruneHistory(ctx context.Context) error {
	now := time.Now().UTC()
	for _, q := range []struct {
		model any
		where string
		args  []any
	}{
		{&Metric{}, "(granularity_seconds = ? AND bucket_at < ?) OR (granularity_seconds = ? AND bucket_at < ?)", []any{60, now.Add(-24 * time.Hour), 300, now.Add(-30 * 24 * time.Hour)}},
		{&Event{}, "occurred_at < ?", []any{now.Add(-90 * 24 * time.Hour)}},
		{&Operation{}, "status IN ? AND updated_at < ?", []any{[]string{"succeeded", "failed", "cancelled"}, now.Add(-24 * time.Hour)}},
	} {
		ids := []string{}
		if e := r.db.WithContext(ctx).Model(q.model).Where(q.where, q.args...).Limit(1000).Pluck("id", &ids).Error; e != nil {
			return e
		}
		if len(ids) > 0 {
			if e := r.db.WithContext(ctx).Where("id IN ?", ids).Delete(q.model).Error; e != nil {
				return e
			}
		}
	}
	return nil
}

// RuntimeSessionTraffic 不改变会话生命周期，只更新同代次且尚未关闭的采样。
func (r *Repository) RuntimeSessionTraffic(ctx context.Context, pk, owner string, epoch int64, samples []protocol.ResourceTraffic, now time.Time) error {
	return r.Transaction(ctx, func(tx *gorm.DB) error {
		if err := CheckOwner(tx, pk, owner, epoch); err != nil {
			return err
		}
		for _, sample := range samples {
			var row Session
			connectionEpoch, err := sample.Ref.ConnectionEpoch.Int64()
			if err != nil {
				return err
			}
			q := tx.Where("gateway_id = ? AND resource_type = ? AND remote_id = ? AND generation = ? AND connection_epoch = ? AND state <> ?", pk, sample.Ref.ResourceType, sample.Ref.ID, sample.Ref.Generation, connectionEpoch, "closed")
			if err := q.First(&row).Error; IsNotFound(err) {
				continue
			} else if err != nil {
				return err
			}
			traffic := core.SessionTraffic{SampledAt: now, RXBytes: sample.RXBytes, TXBytes: sample.TXBytes, QueueBytes: sample.QueueBytes}
			if row.Traffic != nil && now.After(row.Traffic.SampledAt) && now.Sub(row.Traffic.SampledAt) <= 45*time.Second {
				traffic.RXBPS = protocol.BitRate(&row.Traffic.RXBytes, &traffic.RXBytes, now.Sub(row.Traffic.SampledAt))
				traffic.TXBPS = protocol.BitRate(&row.Traffic.TXBytes, &traffic.TXBytes, now.Sub(row.Traffic.SampledAt))
			}
			if err := tx.Model(&Session{}).Where("id = ? AND gateway_id = ? AND generation = ? AND connection_epoch = ? AND state <> ?", row.ID, pk, sample.Ref.Generation, connectionEpoch, "closed").Update("traffic", gorm.Expr("?", string(protocol.Payload(traffic)))).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
