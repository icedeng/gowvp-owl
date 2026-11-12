package owl

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"gorm.io/gorm"
)

func (h *Host) GatewayCounts(ctx context.Context, pk string, snapshot core.RuntimeSnapshot) (core.Counts, error) {
	counts, err := h.GatewayCountsBatch(ctx, map[string]core.RuntimeSnapshot{pk: snapshot})
	return counts[pk], err
}

// GatewayCountsBatch 按网关分组查询，列表大小不会增加 SQL 往返次数。
func (h *Host) GatewayCountsBatch(ctx context.Context, snapshots map[string]core.RuntimeSnapshot) (map[string]core.Counts, error) {
	result := map[string]core.Counts{}
	ids := make([]string, 0, len(snapshots))
	for pk := range snapshots {
		ids = append(ids, pk)
		result[pk] = core.Counts{}
	}
	sort.Strings(ids)
	now := time.Now().UTC()
	if len(ids) == 0 {
		return result, nil
	}
	err := h.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		type aggregate struct {
			GatewayID string
			N         int64
		}
		apply := func(query *gorm.DB, assign func(*core.Counts, int64)) error {
			var rows []aggregate
			if err := query.Scan(&rows).Error; err != nil {
				return err
			}
			for _, row := range rows {
				v := result[row.GatewayID]
				assign(&v, row.N)
				result[row.GatewayID] = v
			}
			return nil
		}
		bindings := func() *gorm.DB {
			return tx.Table("access_gateway_bindings AS b").Where("b.gateway_id IN ? AND b.ingress_type = ?", ids, "gateway")
		}
		online := func(q *gorm.DB) *gorm.DB {
			conditions := []string{}
			args := []any{}
			for _, pk := range ids {
				snapshot := snapshots[pk]
				if snapshot.ConnectionState != "connected" || snapshot.HealthState != "ready" {
					continue
				}
				conditions = append(conditions, "(b.gateway_id = ? AND b.last_generation = ? AND b.last_connection_epoch = ?)")
				args = append(args, pk, snapshot.Generation, snapshot.Epoch)
			}
			if len(conditions) == 0 {
				return q.Where("1 = 0")
			}
			return q.Where("("+strings.Join(conditions, " OR ")+")", args...).Where("d.is_online = ? AND (b.registered_until IS NULL OR b.registered_until > ?)", true, now)
		}
		if err := apply(bindings().Select("b.gateway_id, COUNT(*) AS n").Group("b.gateway_id"), func(c *core.Counts, n int64) { c.DeviceTotal = n }); err != nil {
			return err
		}
		if err := apply(online(bindings().Joins("JOIN devices AS d ON d.id=b.subject_id")).Select("b.gateway_id, COUNT(*) AS n").Group("b.gateway_id"), func(c *core.Counts, n int64) { c.DeviceOnline = n }); err != nil {
			return err
		}
		channels := func() *gorm.DB {
			return tx.Table("channels AS c").Joins("JOIN (SELECT subject_id,gateway_id,ingress_type,last_generation,last_connection_epoch,registered_until FROM access_gateway_bindings UNION ALL SELECT subject_id,gateway_id,ingress_type,last_generation,last_connection_epoch,registered_until FROM access_gateway_platform_bindings WHERE role = 'downstream') AS b ON b.subject_id=c.did").Where("b.gateway_id IN ? AND b.ingress_type = ?", ids, "gateway")
		}
		if err := apply(channels().Select("b.gateway_id, COUNT(*) AS n").Group("b.gateway_id"), func(c *core.Counts, n int64) { c.ChannelTotal = n }); err != nil {
			return err
		}
		if err := apply(online(channels().Joins("JOIN devices AS d ON d.id=b.subject_id")).Where("c.is_online = ?", true).Select("b.gateway_id, COUNT(*) AS n").Group("b.gateway_id"), func(c *core.Counts, n int64) { c.ChannelOnline = n }); err != nil {
			return err
		}
		if err := apply(tx.Model(&store.PlatformBinding{}).Where("gateway_id IN ?", ids).Select("gateway_id, COUNT(*) AS n").Group("gateway_id"), func(c *core.Counts, n int64) { c.PlatformBindings = n }); err != nil {
			return err
		}
		var sessions []struct {
			GatewayID, Purpose, ResourceType, State, Direction string
			N                                                  int64
		}
		if err := tx.Model(&store.Session{}).Select("gateway_id,purpose,resource_type,state,direction,COUNT(*) AS n").Where("gateway_id IN ? AND state <> ?", ids, "closed").Group("gateway_id,purpose,resource_type,state,direction").Scan(&sessions).Error; err != nil {
			return err
		}
		for _, row := range sessions {
			c := result[row.GatewayID]
			if row.State == "closing" {
				c.CleanupPending += row.N
			}
			if row.ResourceType == "transfer" {
				c.TransferSessions += row.N
			} else {
				if row.Direction != "a_to_device" {
					c.ActiveSources += row.N
				}
				switch row.Purpose {
				case "live":
					c.LiveSessions += row.N
				case "playback":
					c.PlaybackSessions += row.N
				case "download":
					c.DownloadSessions += row.N
				case "voice", "talk", "broadcast":
					c.VoiceSessions += row.N
				}
			}
			result[row.GatewayID] = c
		}
		return nil
	})
	for pk, c := range result {
		c.DeviceOffline = c.DeviceTotal - c.DeviceOnline
		result[pk] = c
	}
	return result, err
}
func (h *Host) GatewayDevices(ctx context.Context, pk string, snapshot core.RuntimeSnapshot, q core.PageQuery) (core.Page[core.DeviceIngressView], error) {
	result := core.Page[core.DeviceIngressView]{Items: []core.DeviceIngressView{}, Page: q.Page, Size: q.Size, SnapshotID: uuid.NewString(), SampledAt: time.Now().UTC()}
	ready := snapshot.ConnectionState == "connected" && snapshot.HealthState == "ready"
	now := time.Now().UTC()
	online := func(db *gorm.DB) *gorm.DB {
		if !ready {
			return db.Where("1 = 0")
		}
		return db.Where("d.is_online = ? AND b.last_generation = ? AND b.last_connection_epoch = ? AND (b.registered_until IS NULL OR b.registered_until > ?)", true, snapshot.Generation, snapshot.Epoch, now)
	}
	query := h.db.WithContext(ctx).Table("devices AS d").Joins("JOIN access_gateway_bindings AS b ON b.subject_id = d.id").Where("b.gateway_id = ? AND b.ingress_type = ?", pk, "gateway")
	if q.Q != "" {
		query = query.Where("d.device_id LIKE ? OR d.name LIKE ?", "%"+q.Q+"%", "%"+q.Q+"%")
	}
	if q.Online != nil {
		if *q.Online {
			query = online(query)
		} else if ready {
			query = query.Where("NOT (d.is_online = ? AND b.last_generation = ? AND b.last_connection_epoch = ?) OR b.last_generation IS NULL OR b.last_connection_epoch IS NULL OR b.registered_until <= ?", true, snapshot.Generation, snapshot.Epoch, now)
		}
	}
	if e := query.Count(&result.Total).Error; e != nil {
		return result, e
	}
	rows := []struct {
		ID                  string
		DeviceID            string
		Name                string
		IsOnline            bool
		RegisteredAt        time.Time
		KeepaliveAt         time.Time
		BindingRevision     int64
		LastGeneration      *string
		LastConnectionEpoch *int64
		RegisteredUntil     *time.Time
	}{}
	if e := query.Select("d.id,d.device_id,d.name,d.is_online,d.registered_at,d.keepalive_at,b.binding_revision,b.last_generation,b.last_connection_epoch,b.registered_until").Order("d.device_id ASC, d.id ASC").Offset((q.Page - 1) * q.Size).Limit(q.Size).Scan(&rows).Error; e != nil {
		return result, e
	}
	ids := []string{}
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	var channels []struct {
		DID    string
		Total  int64
		Online int64
	}
	if len(ids) > 0 {
		if e := h.db.WithContext(ctx).Table("channels").Select("did,COUNT(*) AS total,SUM(CASE WHEN is_online = ? THEN 1 ELSE 0 END) AS online", true).Where("did IN ?", ids).Group("did").Scan(&channels).Error; e != nil {
			return result, e
		}
	}
	counts := map[string][2]int64{}
	for _, r := range channels {
		counts[r.DID] = [2]int64{r.Total, r.Online}
	}
	gateway, e := h.repo.GetPK(ctx, pk)
	if e != nil {
		return result, e
	}
	stable := gateway.StableID
	for _, r := range rows {
		isOnline := ready && r.IsOnline && (r.RegisteredUntil == nil || now.Before(*r.RegisteredUntil)) && r.LastGeneration != nil && *r.LastGeneration == snapshot.Generation && r.LastConnectionEpoch != nil && protocol.Number(*r.LastConnectionEpoch) == snapshot.Epoch
		count := counts[r.ID]
		if !isOnline {
			count[1] = 0
		}
		state := "device_offline"
		if snapshot.ConnectionState != "connected" {
			state = "gateway_unreachable"
		} else if snapshot.HealthState != "ready" || snapshot.RecoveryStartedAt != nil && snapshot.RecoveryDurationMS == nil {
			state = "gateway_recovering"
		} else if r.LastGeneration == nil || *r.LastGeneration != snapshot.Generation || r.LastConnectionEpoch == nil || protocol.Number(*r.LastConnectionEpoch) != snapshot.Epoch {
			state = "awaiting_registration"
		} else if isOnline {
			state = "online"
		}
		result.Items = append(result.Items, core.DeviceIngressView{RecoveryState: state, DeviceID: r.ID, DeviceCode: r.DeviceID, Name: r.Name, IngressType: "gateway", GatewayID: &stable, BindingRevision: protocol.Number(r.BindingRevision), Online: isOnline, ChannelTotal: count[0], ChannelOnline: count[1], RegisteredAt: &r.RegisteredAt, KeepaliveAt: &r.KeepaliveAt})
	}
	return result, nil
}
