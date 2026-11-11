package gbs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"
)

const (
	gbTaskKindAlarmEventInbox   = "alarm_event_inbox"
	gbTaskKindAlarmEventReceipt = "alarm_event_receipt"
	gbTaskKindAlarmEventFailed  = "alarm_event_failed"
)

type alarmEventDeliveryState struct {
	Version       int        `json:"version"`
	Event         AlarmEvent `json:"event"`
	RawXML        string     `json:"raw_xml"`
	AcceptedAt    time.Time  `json:"accepted_at"`
	Attempts      int        `json:"attempts,omitempty"`
	NextAttemptAt time.Time  `json:"next_attempt_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

// SetDurableAlarmHandler 注册事件中心的独立持久投递。
func (s *Server) SetDurableAlarmHandler(prepare, deliver func(context.Context, *AlarmEvent) error) {
	s.gb.SetDurableAlarmHandler(prepare, deliver)
}

// SetDurableAlarmHandler 为事件中心注册独立持久投递；prepare 在接纳时冻结内部身份。
// 既有 SetReliableAlarmHandler 保留给依赖原协议副作用门禁的调用方。
func (g *GB28181API) SetDurableAlarmHandler(prepare, deliver func(context.Context, *AlarmEvent) error) {
	g.alarmHandlerMu.Lock()
	g.alarmEventPrepare, g.alarmEventDeliver = prepare, deliver
	if g.alarmEventWake == nil {
		g.alarmEventWake = make(chan struct{}, 1)
	}
	g.alarmHandlerMu.Unlock()
	if deliver != nil && g.serviceDone() != nil && g.taskStateStorer() != nil {
		g.alarmEventWorkerOnce.Do(func() { g.startLifecycleWorker(g.runAlarmEventDeliveryWorker) })
	}
}

func (g *GB28181API) alarmEventHandlers() (func(context.Context, *AlarmEvent) error, func(context.Context, *AlarmEvent) error) {
	g.alarmHandlerMu.RLock()
	defer g.alarmHandlerMu.RUnlock()
	return g.alarmEventPrepare, g.alarmEventDeliver
}

func (g *GB28181API) persistAlarmEventDelivery(ctx context.Context, event *AlarmEvent, raw []byte) error {
	prepare, deliver := g.alarmEventHandlers()
	if deliver == nil {
		return nil
	}
	if g.taskStateStorer() == nil {
		return fmt.Errorf("durable Alarm event storage unavailable")
	}
	unlock, err := g.lockTaskStateOperation(ctx, gbTaskKindAlarmEventInbox, event.DeviceID, event.DeliveryID)
	if err != nil {
		return err
	}
	defer unlock()
	if found, err := g.alarmEventReceiptExists(ctx, event.DeviceID, event.DeliveryID); err != nil || found {
		return err
	}
	// 重传不重新解析已冻结的内部身份，也不重置失败退避。
	if payload, found, err := g.loadTaskState(ctx, gbTaskKindAlarmEventInbox, event.DeviceID, event.DeliveryID); err != nil {
		return err
	} else if found {
		var stored alarmEventDeliveryState
		if err := json.Unmarshal(payload, &stored); err != nil {
			return err
		}
		if err := validateAlarmEventDelivery(stored, event.DeviceID, event.DeliveryID, time.Now()); err != nil {
			return err
		}
		copyEvent := stored.Event
		copyEvent.InternalDeviceID, copyEvent.InternalChannelID, copyEvent.ProtocolVersion = "", "", ""
		if stored.RawXML != string(raw) || !reflect.DeepEqual(copyEvent, *event) {
			return fmt.Errorf("Alarm event retransmission identity mismatch")
		}
		return nil
	}
	if prepare == nil {
		return fmt.Errorf("Alarm event identity resolver unavailable")
	}
	snapshot := *event
	snapshot.ProtocolVersion = string(g.getDeviceGBProtocolVersion(event.DeviceID))
	if err := prepare(ctx, &snapshot); err != nil {
		return err
	}
	now := time.Now()
	state := alarmEventDeliveryState{Version: 1, Event: snapshot, RawXML: string(raw), AcceptedAt: now}
	if err := validateAlarmEventDelivery(state, event.DeviceID, event.DeliveryID, now); err != nil {
		return err
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := g.saveTaskState(ctx, gbTaskKindAlarmEventInbox, event.DeviceID, event.DeliveryID, payload, now); err != nil {
		return err
	}
	select {
	case g.alarmEventWake <- struct{}{}:
	default:
	}
	return nil
}

func validateAlarmEventDelivery(state alarmEventDeliveryState, deviceID, deliveryID string, now time.Time) error {
	switch GBProtocolVersion(state.Event.ProtocolVersion) {
	case GBVersion10, GBVersion11, GBVersion20, GBVersion30:
	default:
		return fmt.Errorf("invalid accepted Alarm protocol version")
	}
	if state.Version != 1 || strings.TrimSpace(state.Event.InternalDeviceID) == "" || state.RawXML == "" ||
		state.Event.DeviceID != deviceID || state.Event.DeliveryID != deliveryID ||
		!strings.EqualFold(state.Event.CmdType, "Alarm") ||
		(state.Event.SourceMethod != "MESSAGE" && state.Event.SourceMethod != "NOTIFY") ||
		alarmDeliveryID(deviceID, state.Event.SourceMethod, []byte(state.RawXML)) != deliveryID ||
		state.AcceptedAt.IsZero() || state.AcceptedAt.After(now.Add(5*time.Minute)) || state.Attempts < 0 ||
		(!state.NextAttemptAt.IsZero() && state.NextAttemptAt.Before(state.AcceptedAt)) {
		return fmt.Errorf("invalid accepted Alarm event identity or state")
	}
	return nil
}

func (g *GB28181API) runAlarmEventDeliveryWorker() {
	ticker := time.NewTicker(alarmInboxPollInterval)
	defer ticker.Stop()
	for {
		g.processAlarmEventDeliveryBatch(time.Now())
		select {
		case <-g.serviceDone():
			return
		case <-g.alarmEventWake:
		case <-ticker.C:
		}
	}
}

func (g *GB28181API) processAlarmEventDeliveryBatch(now time.Time) {
	_, deliver := g.alarmEventHandlers()
	if deliver == nil {
		return
	}
	records, err := g.listTaskStates(g.serviceContext(), gbTaskKindAlarmEventInbox, alarmInboxBatchSize)
	if err != nil {
		slog.Error("list Alarm event deliveries", "err", err)
		return
	}
	for _, record := range records {
		g.processAlarmEventDelivery(record.DeviceID, record.SessionID, now, deliver)
	}
}

func (g *GB28181API) processAlarmEventDelivery(deviceID, deliveryID string, now time.Time, deliver func(context.Context, *AlarmEvent) error) {
	unlock, err := g.lockTaskStateOperation(g.serviceContext(), gbTaskKindAlarmEventInbox, deviceID, deliveryID)
	if err != nil {
		return
	}
	defer unlock()
	payload, found, err := g.loadTaskState(g.serviceContext(), gbTaskKindAlarmEventInbox, deviceID, deliveryID)
	if err != nil || !found {
		return
	}
	var state alarmEventDeliveryState
	err = json.Unmarshal(payload, &state)
	if err == nil {
		err = validateAlarmEventDelivery(state, deviceID, deliveryID, now)
	}
	if err != nil {
		failed, _ := json.Marshal(alarmDeadLetterState{Payload: string(payload), Error: err.Error(), FailedAt: now})
		if saveErr := g.saveTaskState(g.taskPersistenceContext(), gbTaskKindAlarmEventFailed, deviceID, deliveryID, failed, now); saveErr == nil {
			_ = g.deleteTaskState(g.taskPersistenceContext(), gbTaskKindAlarmEventInbox, deviceID, deliveryID)
		}
		slog.Error("invalid Alarm event delivery retained", "device_id", deviceID, "delivery_id", deliveryID, "err", err)
		return
	}
	if completed, receiptErr := g.alarmEventReceiptExists(g.serviceContext(), deviceID, deliveryID); receiptErr != nil {
		return
	} else if completed {
		_ = g.deleteTaskState(g.taskPersistenceContext(), gbTaskKindAlarmEventInbox, deviceID, deliveryID)
		return
	}

	if now.Before(state.NextAttemptAt) {
		return
	}
	ctx, cancel := context.WithTimeout(g.serviceContext(), 20*time.Second)
	snapshot := state.Event
	err = callAlarmEventHandler(ctx, deliver, &snapshot)
	cancel()
	if err == nil {
		receipt, _ := json.Marshal(alarmReceiptState{CompletedAt: time.Now()})
		err = g.saveTaskState(g.taskPersistenceContext(), gbTaskKindAlarmEventReceipt, deviceID, deliveryID, receipt, time.Now())
	}
	if err == nil {
		if err := g.deleteTaskState(g.taskPersistenceContext(), gbTaskKindAlarmEventInbox, deviceID, deliveryID); err != nil {
			slog.Warn("delete completed Alarm event delivery", "err", err)
		}
		return
	}
	state.Attempts++
	state.LastError = err.Error()
	state.NextAttemptAt = time.Now().Add(alarmInboxRetryDelay(state.Attempts))
	updated, _ := json.Marshal(state)
	if saveErr := g.saveTaskState(g.taskPersistenceContext(), gbTaskKindAlarmEventInbox, deviceID, deliveryID, updated, state.NextAttemptAt); saveErr != nil {
		slog.Error("save Alarm event retry", "err", saveErr)
	}
	slog.Warn("Alarm event retained for retry", "device_id", deviceID, "delivery_id", deliveryID, "attempts", state.Attempts, "err", err)
}

func callAlarmEventHandler(ctx context.Context, handler func(context.Context, *AlarmEvent) error, event *AlarmEvent) (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("Alarm event handler panic: %v", value)
		}
	}()
	return handler(ctx, event)
}

// 损坏回执不能压制合法重传或恢复任务；事件中心唯一键仍提供最终幂等。
func (g *GB28181API) alarmEventReceiptExists(ctx context.Context, deviceID, deliveryID string) (bool, error) {
	payload, found, err := g.loadTaskState(ctx, gbTaskKindAlarmEventReceipt, deviceID, deliveryID)
	if err != nil || !found {
		return found, err
	}
	var receipt alarmReceiptState
	if err := json.Unmarshal(payload, &receipt); err == nil && !receipt.CompletedAt.IsZero() && !receipt.CompletedAt.After(time.Now().Add(5*time.Minute)) {
		return true, nil
	}
	return false, g.deleteTaskState(ctx, gbTaskKindAlarmEventReceipt, deviceID, deliveryID)
}
