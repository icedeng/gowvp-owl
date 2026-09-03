package gbs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"
)

const (
	gbTaskKindAlarmInbox      = "alarm_inbox"
	gbTaskKindAlarmReceipt    = "alarm_receipt"
	gbTaskKindAlarmDeadLetter = "alarm_dead_letter"
	alarmInboxBatchSize       = 100
	alarmInboxPollInterval    = 5 * time.Second
	alarmInboxMaxRetryDelay   = 5 * time.Minute
	alarmInboxRetention       = 30 * 24 * time.Hour
	alarmInboxMaxStates       = 10000
)

type alarmInboxState struct {
	Event               AlarmEvent `json:"event"`
	ReceivedAt          time.Time  `json:"received_at"`
	BusinessCommittedAt time.Time  `json:"business_committed_at,omitempty"`
	Attempts            int        `json:"attempts,omitempty"`
	NextAttemptAt       time.Time  `json:"next_attempt_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
}

type alarmReceiptState struct {
	CompletedAt time.Time `json:"completed_at"`
}

type alarmDeadLetterState struct {
	Payload  string    `json:"payload"`
	Error    string    `json:"error"`
	FailedAt time.Time `json:"failed_at"`
}

type alarmInboxProcessResult uint8

const (
	alarmInboxProcessIdle alarmInboxProcessResult = iota
	alarmInboxProcessMaintenance
	alarmInboxProcessDelivery
)

func alarmDeliveryID(deviceID, sourceMethod string, body []byte) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(strings.TrimSpace(deviceID)))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(strings.ToUpper(strings.TrimSpace(sourceMethod))))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(body)
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func (g *GB28181API) alarmHandlerSnapshot() func(context.Context, *AlarmEvent) error {
	if g == nil {
		return nil
	}
	g.alarmHandlerMu.RLock()
	handler := g.alarmHandler
	g.alarmHandlerMu.RUnlock()
	return handler
}

func (g *GB28181API) persistAlarmInbox(ctx context.Context, event *AlarmEvent) (bool, error) {
	if g == nil || event == nil || g.taskStateStorer() == nil {
		return false, nil
	}
	unlock, err := g.lockAlarmInboxOperation(ctx, event.DeliveryID)
	if err != nil {
		return false, err
	}
	defer unlock()
	if completed, err := g.alarmReceiptExists(ctx, event.DeviceID, event.DeliveryID); err != nil {
		return false, err
	} else if completed {
		return true, nil
	}
	if payload, exists, err := g.loadTaskState(ctx, gbTaskKindAlarmInbox, event.DeviceID, event.DeliveryID); err != nil {
		return false, err
	} else if exists {
		var state alarmInboxState
		decodeErr := json.Unmarshal(payload, &state)
		if decodeErr == nil {
			decodeErr = validateAlarmInboxState(state, event.DeviceID, event.DeliveryID, time.Now())
		}
		if decodeErr == nil && reflect.DeepEqual(state.Event, *event) {
			// 同一 DeliveryID 的重传不能覆盖原始重试次数、下一次投递时间或业务提交标记。
			return true, nil
		}
		if decodeErr == nil {
			decodeErr = fmt.Errorf("Alarm inbox event does not match retransmission")
		}
		if !g.quarantineAlarmInboxLocked(event.DeviceID, event.DeliveryID, payload, decodeErr, time.Now()) {
			return false, fmt.Errorf("quarantine invalid Alarm inbox: %w", decodeErr)
		}
	}
	state := alarmInboxState{Event: *event, ReceivedAt: time.Now()}
	payload, err := json.Marshal(state)
	if err != nil {
		return false, fmt.Errorf("encode Alarm inbox: %w", err)
	}
	if err := g.saveTaskState(ctx, gbTaskKindAlarmInbox, event.DeviceID, event.DeliveryID, payload, state.ReceivedAt); err != nil {
		return false, err
	}
	return true, nil
}

// commitAlarmBusinessOnce 让报警确认后的非回调副作用按 DeliveryID 至多并发提交一次。
// ran 表示本次是否执行了 fn；businessComplete 表示副作用已经完成，允许继续业务回调。
// 标记在副作用之后持久化：标记写入失败时副作用仍视为完成，但保留错误供调用方记录。
func (g *GB28181API) commitAlarmBusinessOnce(ctx context.Context, deviceID, deliveryID string, fn func() error) (ran, businessComplete bool, err error) {
	if g == nil || g.taskStateStorer() == nil || strings.TrimSpace(deliveryID) == "" {
		if fn != nil {
			err := fn()
			return true, err == nil, err
		}
		return true, true, nil
	}
	unlock, err := g.lockAlarmInboxOperation(ctx, deliveryID)
	if err != nil {
		return false, false, err
	}
	defer unlock()
	if completed, err := g.alarmReceiptExists(ctx, deviceID, deliveryID); err != nil {
		return false, false, err
	} else if completed {
		return false, true, nil
	}
	payload, exists, err := g.loadTaskState(ctx, gbTaskKindAlarmInbox, deviceID, deliveryID)
	if err != nil {
		return false, false, err
	}
	if !exists {
		return false, false, fmt.Errorf("Alarm inbox state is unavailable")
	}
	var state alarmInboxState
	if err := json.Unmarshal(payload, &state); err != nil {
		return false, false, fmt.Errorf("decode Alarm inbox: %w", err)
	}
	if err := validateAlarmInboxState(state, deviceID, deliveryID, time.Now()); err != nil {
		if !g.quarantineAlarmInboxLocked(deviceID, deliveryID, payload, err, time.Now()) {
			return false, false, fmt.Errorf("quarantine invalid Alarm inbox: %w", err)
		}
		return false, false, err
	}
	if !state.BusinessCommittedAt.IsZero() {
		return false, true, nil
	}
	if fn != nil {
		if err := fn(); err != nil {
			return true, false, err
		}
	}
	state.BusinessCommittedAt = time.Now()
	state.NextAttemptAt = time.Time{}
	state.LastError = ""
	updated, err := json.Marshal(state)
	if err != nil {
		return true, true, fmt.Errorf("encode Alarm business commit: %w", err)
	}
	if err := g.saveTaskState(g.taskPersistenceContext(), gbTaskKindAlarmInbox, deviceID, deliveryID, updated, state.BusinessCommittedAt); err != nil {
		return true, true, err
	}
	return true, true, nil
}

func (g *GB28181API) startAlarmInboxWorker() {
	if g == nil || g.serviceDone() == nil || g.taskStateStorer() == nil {
		return
	}
	g.alarmInboxWorkerOnce.Do(func() {
		g.startLifecycleWorker(g.runAlarmInboxWorker)
	})
}

func (g *GB28181API) signalAlarmInboxWorker() {
	if g == nil {
		return
	}
	g.alarmHandlerMu.RLock()
	wake := g.alarmInboxWake
	g.alarmHandlerMu.RUnlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (g *GB28181API) runAlarmInboxWorker() {
	g.processAlarmInboxBatch(time.Now())
	ticker := time.NewTicker(alarmInboxPollInterval)
	defer ticker.Stop()
	for {
		g.alarmHandlerMu.RLock()
		wake := g.alarmInboxWake
		g.alarmHandlerMu.RUnlock()
		select {
		case <-g.serviceDone():
			return
		case <-wake:
			g.processAlarmInboxBatch(time.Now())
		case now := <-ticker.C:
			g.processAlarmInboxBatch(now)
		}
	}
}

func (g *GB28181API) processAlarmInboxBatch(now time.Time) {
	if g == nil || g.alarmHandlerSnapshot() == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	seen := make(map[string]struct{}, alarmInboxBatchSize)
	deliveries := 0
	for deliveries < alarmInboxBatchSize && len(seen) < alarmInboxMaxStates {
		records, err := g.listTaskStates(g.serviceContext(), gbTaskKindAlarmInbox, alarmInboxBatchSize)
		if err != nil {
			if !g.serviceStopped() {
				slog.Warn("list Alarm inbox failed", "err", err)
			}
			return
		}
		if len(records) == 0 {
			return
		}
		discovered := false
		progressed := false
		for _, record := range records {
			key := record.DeviceID + "\x00" + record.SessionID
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			discovered = true
			result := g.processAlarmInboxDeliveryAt(record.DeviceID, record.SessionID, record.UpdatedAt.Time, time.Time{}, now)
			if result != alarmInboxProcessIdle {
				progressed = true
			}
			if result == alarmInboxProcessDelivery {
				deliveries++
				if deliveries >= alarmInboxBatchSize {
					return
				}
			}
		}
		if !discovered || !progressed || len(records) < alarmInboxBatchSize {
			return
		}
	}
}

func (g *GB28181API) processAlarmInboxDelivery(deviceID, deliveryID string) {
	g.processAlarmInboxDeliveryAt(deviceID, deliveryID, time.Time{}, time.Time{}, time.Now())
}

// processCommittedAlarmInboxDelivery 仅供当前请求已在注册代次门禁内完成业务副作用后调用。
// 即使业务提交标志瞬时写入失败，也允许本次请求继续回调并写最终 receipt；后台扫描
// 仍必须依赖持久化的 BusinessCommittedAt，不能用该入口绕过旧注册代次门禁。
func (g *GB28181API) processCommittedAlarmInboxDelivery(deviceID, deliveryID string, committedAt time.Time) {
	if committedAt.IsZero() {
		committedAt = time.Now()
	}
	g.processAlarmInboxDeliveryAt(deviceID, deliveryID, time.Time{}, committedAt, time.Now())
}

func (g *GB28181API) processAlarmInboxDeliveryAt(deviceID, deliveryID string, indexedAt, committedAt, now time.Time) alarmInboxProcessResult {
	if now.IsZero() {
		now = time.Now()
	}
	ctx := g.serviceContext()
	unlock, err := g.lockAlarmInboxOperation(ctx, deliveryID)
	if err != nil {
		return alarmInboxProcessIdle
	}
	defer unlock()
	if completed, err := g.alarmReceiptExists(ctx, deviceID, deliveryID); err != nil {
		slog.Warn("load Alarm receipt failed", "device_id", deviceID, "delivery_id", deliveryID, "err", err)
		return alarmInboxProcessIdle
	} else if completed {
		if err := g.deleteTaskState(g.taskPersistenceContext(), gbTaskKindAlarmInbox, deviceID, deliveryID); err != nil {
			slog.Warn("delete completed Alarm inbox failed", "device_id", deviceID, "delivery_id", deliveryID, "err", err)
			return alarmInboxProcessIdle
		}
		return alarmInboxProcessMaintenance
	}

	payload, ok, err := g.loadTaskState(ctx, gbTaskKindAlarmInbox, deviceID, deliveryID)
	if err != nil {
		slog.Warn("load Alarm inbox failed", "device_id", deviceID, "delivery_id", deliveryID, "err", err)
		return alarmInboxProcessIdle
	}
	if !ok {
		return alarmInboxProcessIdle
	}
	var state alarmInboxState
	if err := json.Unmarshal(payload, &state); err != nil {
		slog.Error("decode Alarm inbox failed", "device_id", deviceID, "delivery_id", deliveryID, "err", err)
		if g.quarantineAlarmInboxLocked(deviceID, deliveryID, payload, err, now) {
			return alarmInboxProcessMaintenance
		}
		return alarmInboxProcessIdle
	}
	if err := validateAlarmInboxState(state, deviceID, deliveryID, now); err != nil {
		slog.Error("invalid Alarm inbox state", "device_id", deviceID, "delivery_id", deliveryID, "err", err)
		if g.quarantineAlarmInboxLocked(deviceID, deliveryID, payload, err, now) {
			return alarmInboxProcessMaintenance
		}
		return alarmInboxProcessIdle
	}
	if !state.NextAttemptAt.IsZero() && now.Before(state.NextAttemptAt) {
		// 旧版本把失败时间写入 UpdatedAt，未来重试记录会长期占据批首。
		// 只重排一次到 NextAttemptAt；已正确索引的记录保持只读。
		if indexedAt.IsZero() || !indexedAt.Before(state.NextAttemptAt) {
			return alarmInboxProcessIdle
		}
		updated, encodeErr := json.Marshal(state)
		if encodeErr != nil {
			slog.Error("encode deferred Alarm inbox failed", "device_id", deviceID, "delivery_id", deliveryID, "err", encodeErr)
			return alarmInboxProcessIdle
		}
		if err := g.saveTaskState(g.taskPersistenceContext(), gbTaskKindAlarmInbox, deviceID, deliveryID, updated, state.NextAttemptAt); err != nil {
			slog.Warn("reindex deferred Alarm inbox failed", "device_id", deviceID, "delivery_id", deliveryID, "err", err)
			return alarmInboxProcessIdle
		}
		return alarmInboxProcessMaintenance
	}
	if state.BusinessCommittedAt.IsZero() && !committedAt.IsZero() {
		// 当前请求已经完成业务副作用；把该事实带入回调重试状态，避免提交标志的
		// 一次瞬时写失败迫使设备重传并重复非回调副作用。
		state.BusinessCommittedAt = committedAt
		state.NextAttemptAt = time.Time{}
		state.LastError = ""
	}
	// 旧注册代次或确认后崩溃留下的未提交记录不能绕过业务门禁直接进入回调。
	if state.BusinessCommittedAt.IsZero() {
		expiresAt := state.ReceivedAt.Add(alarmInboxRetention)
		if !now.Before(expiresAt) {
			if err := g.deleteTaskState(g.taskPersistenceContext(), gbTaskKindAlarmInbox, deviceID, deliveryID); err != nil {
				slog.Warn("delete expired uncommitted Alarm inbox failed", "device_id", deviceID, "delivery_id", deliveryID, "err", err)
				return alarmInboxProcessIdle
			}
			return alarmInboxProcessMaintenance
		}
		if state.NextAttemptAt.Equal(expiresAt) && !indexedAt.Before(expiresAt) {
			return alarmInboxProcessIdle
		}
		state.NextAttemptAt = expiresAt
		state.LastError = "awaiting source retransmission before business commit"
		updated, encodeErr := json.Marshal(state)
		if encodeErr != nil {
			slog.Error("encode uncommitted Alarm inbox failed", "device_id", deviceID, "delivery_id", deliveryID, "err", encodeErr)
			return alarmInboxProcessIdle
		}
		if err := g.saveTaskState(g.taskPersistenceContext(), gbTaskKindAlarmInbox, deviceID, deliveryID, updated, expiresAt); err != nil {
			slog.Warn("defer uncommitted Alarm inbox failed", "device_id", deviceID, "delivery_id", deliveryID, "err", err)
			return alarmInboxProcessIdle
		}
		return alarmInboxProcessMaintenance
	}
	handler := g.alarmHandlerSnapshot()
	if handler != nil {
		if err := handler(ctx, &state.Event); err != nil {
			attemptedAt := time.Now()
			state.Attempts++
			state.LastError = err.Error()
			state.NextAttemptAt = attemptedAt.Add(alarmInboxRetryDelay(state.Attempts))
			updated, encodeErr := json.Marshal(state)
			if encodeErr == nil {
				encodeErr = g.saveTaskState(g.taskPersistenceContext(), gbTaskKindAlarmInbox, deviceID, deliveryID, updated, state.NextAttemptAt)
			}
			if encodeErr != nil {
				slog.Error("update Alarm inbox retry failed", "device_id", deviceID, "delivery_id", deliveryID, "err", encodeErr)
			}
			slog.Warn("Alarm event callback failed; retained for retry", "device_id", deviceID, "delivery_id", deliveryID, "attempts", state.Attempts, "err", err)
			g.signalAlarmInboxWorker()
			return alarmInboxProcessDelivery
		}
	}
	completedAt := time.Now()
	receipt, err := json.Marshal(alarmReceiptState{CompletedAt: completedAt})
	if err == nil {
		err = g.saveTaskState(g.taskPersistenceContext(), gbTaskKindAlarmReceipt, deviceID, deliveryID, receipt, completedAt)
	}
	if err != nil {
		slog.Warn("save processed Alarm receipt failed", "device_id", deviceID, "delivery_id", deliveryID, "err", err)
		// 回调已执行但最终回执尚未落库，保持 at-least-once 重试语义；同时把当前
		// 请求已经确认的业务提交事实补写回 inbox，避免后续设备重传重复非回调副作用。
		attemptedAt := time.Now()
		state.Attempts++
		state.LastError = fmt.Sprintf("save processed Alarm receipt: %v", err)
		state.NextAttemptAt = attemptedAt.Add(alarmInboxRetryDelay(state.Attempts))
		updated, encodeErr := json.Marshal(state)
		if encodeErr == nil {
			encodeErr = g.saveTaskState(g.taskPersistenceContext(), gbTaskKindAlarmInbox, deviceID, deliveryID, updated, state.NextAttemptAt)
		}
		if encodeErr != nil {
			slog.Error("retain Alarm inbox after receipt failure failed", "device_id", deviceID, "delivery_id", deliveryID, "err", encodeErr)
		}
		g.signalAlarmInboxWorker()
		return alarmInboxProcessDelivery
	}
	if err := g.deleteTaskState(g.taskPersistenceContext(), gbTaskKindAlarmInbox, deviceID, deliveryID); err != nil {
		slog.Warn("delete processed Alarm inbox failed", "device_id", deviceID, "delivery_id", deliveryID, "err", err)
		g.signalAlarmInboxWorker()
	}
	return alarmInboxProcessDelivery
}

func validateAlarmInboxState(state alarmInboxState, deviceID, deliveryID string, now time.Time) error {
	deviceID = strings.TrimSpace(deviceID)
	deliveryID = strings.TrimSpace(deliveryID)
	if deviceID == "" || deliveryID == "" {
		return fmt.Errorf("Alarm inbox key identity is missing")
	}
	if strings.TrimSpace(state.Event.DeviceID) != deviceID {
		return fmt.Errorf("Alarm inbox device identity mismatch")
	}
	if strings.TrimSpace(state.Event.DeliveryID) != deliveryID {
		return fmt.Errorf("Alarm inbox delivery identity mismatch")
	}
	if cmdType := strings.TrimSpace(state.Event.CmdType); cmdType != "" && !strings.EqualFold(cmdType, "Alarm") {
		return fmt.Errorf("Alarm inbox command type is invalid")
	}
	if method := strings.ToUpper(strings.TrimSpace(state.Event.SourceMethod)); method != "" && method != "MESSAGE" && method != "NOTIFY" {
		return fmt.Errorf("Alarm inbox source method is invalid")
	}
	if state.ReceivedAt.IsZero() {
		return fmt.Errorf("Alarm inbox received_at is missing")
	}
	if now.IsZero() {
		now = time.Now()
	}
	latestAllowed := now.Add(5 * time.Minute)
	if state.ReceivedAt.After(latestAllowed) {
		return fmt.Errorf("Alarm inbox received_at is in the future")
	}
	if state.Attempts < 0 {
		return fmt.Errorf("Alarm inbox attempts is invalid")
	}
	if !state.BusinessCommittedAt.IsZero() && (state.BusinessCommittedAt.Before(state.ReceivedAt) || state.BusinessCommittedAt.After(latestAllowed)) {
		return fmt.Errorf("Alarm inbox business commit time is invalid")
	}
	if !state.NextAttemptAt.IsZero() && state.NextAttemptAt.Before(state.ReceivedAt) {
		return fmt.Errorf("Alarm inbox next attempt time is invalid")
	}
	return nil
}

// alarmReceiptExists 仅在持有对应 DeliveryID 操作锁时调用。
func (g *GB28181API) alarmReceiptExists(ctx context.Context, deviceID, deliveryID string) (bool, error) {
	payload, found, err := g.loadTaskState(ctx, gbTaskKindAlarmReceipt, deviceID, deliveryID)
	if err != nil || !found {
		return found, err
	}
	var receipt alarmReceiptState
	if err := json.Unmarshal(payload, &receipt); err == nil && !receipt.CompletedAt.IsZero() && !receipt.CompletedAt.After(time.Now().Add(5*time.Minute)) {
		return true, nil
	}
	if err := g.deleteTaskState(ctx, gbTaskKindAlarmReceipt, deviceID, deliveryID); err != nil {
		return false, fmt.Errorf("delete invalid Alarm receipt: %w", err)
	}
	return false, nil
}

func (g *GB28181API) quarantineAlarmInboxLocked(deviceID, deliveryID string, payload []byte, cause error, failedAt time.Time) bool {
	if failedAt.IsZero() {
		failedAt = time.Now()
	}
	state := alarmDeadLetterState{Payload: string(payload), FailedAt: failedAt}
	if cause != nil {
		state.Error = cause.Error()
	}
	encoded, err := json.Marshal(state)
	if err == nil {
		err = g.saveTaskState(g.taskPersistenceContext(), gbTaskKindAlarmDeadLetter, deviceID, deliveryID, encoded, failedAt)
	}
	if err != nil {
		slog.Error("save Alarm dead letter failed", "device_id", deviceID, "delivery_id", deliveryID, "err", err)
		return false
	}
	if err := g.deleteTaskState(g.taskPersistenceContext(), gbTaskKindAlarmInbox, deviceID, deliveryID); err != nil {
		slog.Error("delete quarantined Alarm inbox failed", "device_id", deviceID, "delivery_id", deliveryID, "err", err)
		return false
	}
	return true
}

func alarmInboxRetryDelay(attempt int) time.Duration {
	if attempt <= 1 {
		return alarmInboxPollInterval
	}
	delay := alarmInboxPollInterval
	for i := 1; i < attempt && delay < alarmInboxMaxRetryDelay; i++ {
		delay *= 2
		if delay >= alarmInboxMaxRetryDelay {
			return alarmInboxMaxRetryDelay
		}
	}
	return delay
}

func (g *GB28181API) lockAlarmInboxOperation(ctx context.Context, deliveryID string) (func(), error) {
	if g == nil {
		return nil, fmt.Errorf("GB28181 service is unavailable")
	}
	g.alarmInboxOperationMu.Lock()
	if g.alarmInboxOperations == nil {
		g.alarmInboxOperations = make(map[string]*keyedOperationLock)
	}
	entry := g.alarmInboxOperations[deliveryID]
	if entry == nil {
		entry = &keyedOperationLock{}
		g.alarmInboxOperations[deliveryID] = entry
	}
	entry.refs++
	g.alarmInboxOperationMu.Unlock()
	if err := entry.mutex.LockContext(ctx); err != nil {
		g.releaseAlarmInboxOperation(deliveryID, entry)
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mutex.Unlock()
			g.releaseAlarmInboxOperation(deliveryID, entry)
		})
	}, nil
}

func (g *GB28181API) releaseAlarmInboxOperation(deliveryID string, entry *keyedOperationLock) {
	g.alarmInboxOperationMu.Lock()
	if current := g.alarmInboxOperations[deliveryID]; current == entry {
		entry.refs--
		if entry.refs == 0 {
			delete(g.alarmInboxOperations, deliveryID)
		}
	}
	g.alarmInboxOperationMu.Unlock()
}
