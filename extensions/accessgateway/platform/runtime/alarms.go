package runtime

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
)

// 独立于心跳和实时 SIP 队列，失败不确认；A 重启后从 B 的持久队列继续。
func (w *worker) alarmLoop(ctx context.Context, p *transport.Peer, ack *protocol.HelloAck) {
	if !slices.Contains(ack.Capabilities, "alarm_spool_v1") {
		return
	}
	sink, ok := w.manager.host.(interface {
		ReceiveAlarm(context.Context, string, protocol.AlarmRecord) error
	})
	if !ok {
		return
	}
	cursor := ""
	var cycleErr error
	allowed := true
	if policy, ok := w.manager.host.(interface{ AlarmSpoolAllowed() bool }); ok {
		allowed = policy.AlarmSpoolAllowed()
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		err := func() error {
			w.mu.RLock()
			ready := w.snapshot.ConnectionState == "connected" && w.snapshot.HealthState == "ready"
			connected := w.snapshot.ConnectedAt
			w.mu.RUnlock()
			if !ready {
				return nil
			}
			callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if slices.Contains(ack.Capabilities, "gateway_info_v1") {
				info, e := p.Call(callCtx, "gateway.info", protocol.GatewayInfoRequest{})
				if e == nil {
					w.mu.Lock()
					w.snapshot.Software = info.Payload.(*protocol.GatewayInfo)
					w.mu.Unlock()
				}
			}
			if !allowed {
				return nil
			}
			reply, err := p.Call(callCtx, "alarm.pull", protocol.AlarmPull{After: cursor})
			if err != nil {
				return err
			}
			batch := reply.Payload.(*protocol.AlarmBatch)
			ids := []string{}
			var batchErr error
			if batch.StoreError {
				batchErr = errors.New("B 报警队列存在损坏记录，已保留待恢复")
			}
			for _, a := range batch.Items {
				received, e := time.Parse(time.RFC3339Nano, a.ReceivedAt)
				if e != nil {
					batchErr = errors.Join(batchErr, e)
					continue
				}
				a.Historical = a.Historical || (connected != nil && received.Before(*connected))
				if e := sink.ReceiveAlarm(callCtx, w.configuration().GatewayPK, a); e != nil {
					batchErr = errors.Join(batchErr, e)
					continue // 不确认失败记录；游标继续，避免一条坏记录阻塞整个队列。
				}
				ids = append(ids, a.ID)
			}
			if len(ids) > 0 {
				_, err = p.Call(callCtx, "alarm.ack", protocol.AlarmAck{IDs: ids})
			}
			cycleErr = errors.Join(cycleErr, batchErr, err)
			cursor = batch.Next
			if cursor == "" {
				err = cycleErr
				cycleErr = nil
			} else {
				err = cycleErr
			}

			return err
		}()
		w.mu.Lock()
		if err != nil {
			w.snapshot.AlarmError = protocol.Failure(protocol.IOError, "报警补传失败，B 保留数据等待重试")
		} else {
			w.snapshot.AlarmError = nil
		}
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-p.Done():
			return
		case <-ticker.C:
		}
	}
}
