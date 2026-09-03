package gbs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	gbTaskKindManualSubscription      = "manual_subscription"
	manualSubscriptionRecoveryBatch   = 100
	maxManualSubscriptionIntentStates = 4096
)

type manualSubscriptionIntentState struct {
	Input             SubscribeInput       `json:"input"`
	Identity          *monitorUserIdentity `json:"identity,omitempty"`
	LocalGatewayID    string               `json:"local_gateway_id,omitempty"`
	CreatedAt         time.Time            `json:"created_at"`
	UpdatedAt         time.Time            `json:"updated_at"`
	LastConfirmedAt   time.Time            `json:"last_confirmed_at,omitempty"`
	NextAttemptAt     time.Time            `json:"next_attempt_at,omitempty"`
	Attempts          int                  `json:"attempts,omitempty"`
	LastError         string               `json:"last_error,omitempty"`
	RetryBlocked      bool                 `json:"retry_blocked,omitempty"`
	TerminationReason string               `json:"termination_reason,omitempty"`
}

type manualSubscriptionIntentRecord struct {
	Key       string
	DeviceID  string
	IndexedAt time.Time
	State     manualSubscriptionIntentState
}

type pendingManualSubscriptionTermination struct {
	Key        string
	DeviceID   string
	State      subscriptionStateValue
	Retry      bool
	Delay      time.Duration
	Terminated time.Time
}

type manualSubscriptionRecoveryContextKey struct{}

func manualSubscriptionOperationKey(key string) string {
	return "manual-outgoing:" + strings.TrimSpace(key)
}

func manualSubscriptionRecoveryContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, manualSubscriptionRecoveryContextKey{}, true)
}

func manualSubscriptionOperationAlreadyHeld(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	held, _ := ctx.Value(manualSubscriptionRecoveryContextKey{}).(bool)
	return held
}

func (g *GB28181API) manualSubscriptionPersistenceAvailable() bool {
	store := g.taskStateStorer()
	if store == nil {
		return false
	}
	_, ok := store.(gbTaskStateLister)
	return ok
}

func normalizedManualSubscriptionInput(in SubscribeInput, deviceID, targetID, cmdType string, expires int) SubscribeInput {
	in.DeviceID = strings.TrimSpace(deviceID)
	in.TargetID = strings.TrimSpace(targetID)
	in.Event = outgoingSubscriptionEventName(cmdType)
	in.Expires = expires
	in.Cancel = false
	return in
}

func (g *GB28181API) saveManualSubscriptionIntent(
	ctx context.Context,
	key string,
	input SubscribeInput,
	identity *monitorUserIdentity,
	localGatewayID string,
) (bool, error) {
	if !g.manualSubscriptionPersistenceAvailable() {
		return false, nil
	}
	key = strings.TrimSpace(key)
	input.DeviceID = strings.TrimSpace(input.DeviceID)
	if key == "" || input.DeviceID == "" || input.Cancel {
		return true, fmt.Errorf("invalid manual subscription intent")
	}
	g.manualSubscriptionIntentMu.Lock()
	defer g.manualSubscriptionIntentMu.Unlock()
	now := time.Now()
	state := manualSubscriptionIntentState{CreatedAt: now}
	if payload, exists, err := g.loadTaskState(ctx, gbTaskKindManualSubscription, input.DeviceID, key); err != nil {
		return true, err
	} else if exists {
		decodeErr := json.Unmarshal(payload, &state)
		if decodeErr == nil {
			decodeErr = validateManualSubscriptionIntentState(ctx, state, input.DeviceID, key, now)
		}
		if decodeErr != nil {
			// 用户显式创建或更新时允许替换旧版本留下的损坏状态，避免坏记录
			// 永久阻塞同一订阅键；当前操作持有 intent 锁，不会删除并发新值。
			if err := g.deleteTaskState(ctx, gbTaskKindManualSubscription, input.DeviceID, key); err != nil {
				return true, errors.Join(fmt.Errorf("invalid manual subscription intent: %w", decodeErr), err)
			}
			state = manualSubscriptionIntentState{CreatedAt: now}
		}
	} else {
		records, err := g.listTaskStates(ctx, gbTaskKindManualSubscription, maxManualSubscriptionIntentStates)
		if err != nil {
			return true, err
		}
		if len(records) >= maxManualSubscriptionIntentStates {
			return true, fmt.Errorf("manual subscription intent limit reached: %d", maxManualSubscriptionIntentStates)
		}
	}
	state.Input = input
	state.Identity = identity.clone()
	state.LocalGatewayID = strings.TrimSpace(localGatewayID)
	state.UpdatedAt = now
	// 用户显式创建/更新订阅应解除设备此前报告的永久终止策略；恢复线程
	// 使用专用 context，需保留 retry-after/blocked 状态直到实际成功。
	if !manualSubscriptionOperationAlreadyHeld(ctx) {
		state.NextAttemptAt = time.Time{}
		state.Attempts = 0
		state.LastError = ""
		state.RetryBlocked = false
		state.TerminationReason = ""
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return true, fmt.Errorf("encode manual subscription intent: %w", err)
	}
	if err := g.saveTaskState(ctx, gbTaskKindManualSubscription, input.DeviceID, key, payload, now); err != nil {
		return true, err
	}
	if !manualSubscriptionOperationAlreadyHeld(ctx) {
		// 显式创建/更新在持久化成功后取代旧终止策略。删除必须与
		// 上面的保存共用 intent 锁，防止后台重试在其后写回过期策略。
		g.pendingManualSubscriptionTerminations.Delete(key)
	}
	return true, nil
}

func (g *GB28181API) confirmManualSubscriptionIntent(ctx context.Context, key, deviceID string, refreshAt time.Time) error {
	if !g.manualSubscriptionPersistenceAvailable() {
		return nil
	}
	g.manualSubscriptionIntentMu.Lock()
	defer g.manualSubscriptionIntentMu.Unlock()
	payload, exists, err := g.loadTaskState(ctx, gbTaskKindManualSubscription, deviceID, key)
	if err != nil || !exists {
		return err
	}
	var state manualSubscriptionIntentState
	if err := json.Unmarshal(payload, &state); err != nil {
		return fmt.Errorf("decode manual subscription intent: %w", err)
	}
	now := time.Now()
	if err := validateManualSubscriptionIntentState(ctx, state, deviceID, key, now); err != nil {
		return fmt.Errorf("invalid manual subscription intent: %w", err)
	}
	state.UpdatedAt = now
	state.LastConfirmedAt = now
	state.NextAttemptAt = time.Time{}
	state.Attempts = 0
	state.LastError = ""
	state.RetryBlocked = false
	state.TerminationReason = ""
	updated, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode manual subscription intent: %w", err)
	}
	indexedAt := refreshAt
	if indexedAt.IsZero() || !indexedAt.After(now) {
		indexedAt = now.Add(5 * time.Second)
	}
	if err := g.saveTaskState(ctx, gbTaskKindManualSubscription, deviceID, key, updated, indexedAt); err != nil {
		return err
	}
	// 已成功确认的新对话代表用户意图已恢复，废弃等待落库的旧 terminated 策略。
	g.pendingManualSubscriptionTerminations.Delete(strings.TrimSpace(key))
	return nil
}

func (g *GB28181API) deleteManualSubscriptionIntent(ctx context.Context, key, deviceID string) (managed, existed bool, err error) {
	if !g.manualSubscriptionPersistenceAvailable() {
		return false, false, nil
	}
	g.manualSubscriptionIntentMu.Lock()
	defer g.manualSubscriptionIntentMu.Unlock()
	_, existed, err = g.loadTaskState(ctx, gbTaskKindManualSubscription, deviceID, key)
	if err != nil {
		return true, existed, err
	}
	if existed {
		if err := g.deleteTaskState(ctx, gbTaskKindManualSubscription, deviceID, key); err != nil {
			return true, true, err
		}
	}
	// 用户显式取消已经成功删除（或确认不存在）持久意图，
	// 同时废弃任何尚未落库的旧 terminated 策略。
	g.pendingManualSubscriptionTerminations.Delete(key)
	return true, existed, nil
}

func (g *GB28181API) persistManualSubscriptionTermination(
	key, deviceID string,
	state subscriptionStateValue,
	retry bool,
	delay time.Duration,
	now time.Time,
) (bool, error) {
	if !g.manualSubscriptionPersistenceAvailable() {
		return false, nil
	}
	key = strings.TrimSpace(key)
	deviceID = strings.TrimSpace(deviceID)
	if key == "" || deviceID == "" {
		return false, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	pending := &pendingManualSubscriptionTermination{
		Key: key, DeviceID: deviceID, State: state, Retry: retry, Delay: delay, Terminated: now,
	}
	// 先建立内存保护，即使本次保存失败，恢复线程也不会按旧意图重订。
	g.pendingManualSubscriptionTerminations.Store(key, pending)
	managed, err := g.persistPendingManualSubscriptionTermination(pending)
	if !managed && err == nil {
		g.pendingManualSubscriptionTerminations.CompareAndDelete(key, pending)
	}
	return managed, err
}

func (g *GB28181API) persistPendingManualSubscriptionTermination(pending *pendingManualSubscriptionTermination) (bool, error) {
	if g == nil || pending == nil || !g.manualSubscriptionPersistenceAvailable() {
		return false, nil
	}
	ctx := g.taskPersistenceContext()
	g.manualSubscriptionIntentMu.Lock()
	defer g.manualSubscriptionIntentMu.Unlock()
	current, exists := g.pendingManualSubscriptionTerminations.Load(pending.Key)
	if !exists || current != pending {
		return false, nil
	}
	payload, exists, err := g.loadTaskState(ctx, gbTaskKindManualSubscription, pending.DeviceID, pending.Key)
	if err != nil || !exists {
		return false, err
	}
	var intent manualSubscriptionIntentState
	if err := json.Unmarshal(payload, &intent); err != nil {
		return true, fmt.Errorf("decode manual subscription intent: %w", err)
	}
	if err := validateManualSubscriptionIntentState(ctx, intent, pending.DeviceID, pending.Key, time.Now()); err != nil {
		return true, fmt.Errorf("invalid manual subscription intent: %w", err)
	}
	now := pending.Terminated
	intent.UpdatedAt = now
	intent.RetryBlocked = !pending.Retry
	intent.TerminationReason = strings.TrimSpace(pending.State.reason)
	intent.Attempts = 0
	intent.NextAttemptAt = time.Time{}
	if pending.Retry {
		intent.NextAttemptAt = now.Add(pending.Delay)
	}
	intent.LastError = "subscription terminated"
	if intent.TerminationReason != "" {
		intent.LastError += ": " + intent.TerminationReason
	}
	updated, err := json.Marshal(intent)
	if err != nil {
		return true, fmt.Errorf("encode manual subscription intent: %w", err)
	}
	indexedAt := intent.NextAttemptAt
	if indexedAt.IsZero() || !indexedAt.After(now) {
		indexedAt = now
	}
	if err := g.saveTaskState(ctx, gbTaskKindManualSubscription, pending.DeviceID, pending.Key, updated, indexedAt); err != nil {
		return true, err
	}
	g.pendingManualSubscriptionTerminations.CompareAndDelete(pending.Key, pending)
	return true, nil
}

func (g *GB28181API) applyPendingManualSubscriptionTermination(key string, state *manualSubscriptionIntentState) {
	if g == nil || state == nil {
		return
	}
	value, exists := g.pendingManualSubscriptionTerminations.Load(strings.TrimSpace(key))
	pending, ok := value.(*pendingManualSubscriptionTermination)
	if !exists || !ok || pending == nil {
		return
	}
	now := pending.Terminated
	state.UpdatedAt = now
	state.RetryBlocked = !pending.Retry
	state.TerminationReason = strings.TrimSpace(pending.State.reason)
	state.Attempts = 0
	state.NextAttemptAt = time.Time{}
	if pending.Retry {
		state.NextAttemptAt = now.Add(pending.Delay)
	}
	state.LastError = "subscription terminated"
	if state.TerminationReason != "" {
		state.LastError += ": " + state.TerminationReason
	}
}

func (g *GB28181API) flushPendingManualSubscriptionTerminations(deviceID string) {
	if g == nil {
		return
	}
	deviceID = strings.TrimSpace(deviceID)
	g.pendingManualSubscriptionTerminations.Range(func(_, value any) bool {
		pending, ok := value.(*pendingManualSubscriptionTermination)
		if !ok || pending == nil || deviceID != "" && pending.DeviceID != deviceID {
			return true
		}
		if _, err := g.persistPendingManualSubscriptionTermination(pending); err != nil && !g.serviceStopped() {
			slog.Warn("retry manual subscription termination persistence failed", "device_id", pending.DeviceID, "key", pending.Key, "err", err)
		}
		return true
	})
}

func (g *GB28181API) forgetManualSubscriptionDialog(key string) {
	if g == nil || strings.TrimSpace(key) == "" {
		return
	}
	value, exists := g.outgoingSubscriptions.Load(key)
	if !exists {
		return
	}
	dialog, ok := value.(*outgoingSubscriptionDialog)
	if !ok || dialog == nil {
		g.outgoingSubscriptions.CompareAndDelete(key, value)
		return
	}
	dialog.notifyOperationMu.Lock()
	deleted := g.outgoingSubscriptions.CompareAndDelete(key, dialog)
	dialog.notifyOperationMu.Unlock()
	if !deleted {
		return
	}
	dialog.mu.Lock()
	dialog.autoRefresh = false
	dialog.refreshing = false
	dialog.cancelPending.Store(true)
	dialog.mu.Unlock()
}

func manualSubscriptionRetryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	shift := attempts - 1
	if shift > 5 {
		shift = 5
	}
	delay := 5 * time.Second * time.Duration(1<<shift)
	if delay > 2*time.Minute {
		return 2 * time.Minute
	}
	return delay
}

func (g *GB28181API) recordManualSubscriptionRecoveryFailure(record manualSubscriptionIntentRecord, cause error, now time.Time) {
	state := record.State
	state.Attempts++
	state.UpdatedAt = now
	state.NextAttemptAt = now.Add(manualSubscriptionRetryDelay(state.Attempts))
	if cause != nil {
		state.LastError = cause.Error()
	}
	payload, err := json.Marshal(state)
	if err == nil {
		err = g.saveTaskState(g.taskPersistenceContext(), gbTaskKindManualSubscription, record.DeviceID, record.Key, payload, state.NextAttemptAt)
	}
	if err != nil && !g.serviceStopped() {
		slog.Warn("update manual subscription recovery state failed", "device_id", record.DeviceID, "event", state.Input.Event, "err", err)
	}
}

func (g *GB28181API) deleteInvalidManualSubscriptionIntentIfUnchanged(deviceID, key, expectedPayload string) error {
	if g == nil || !g.manualSubscriptionPersistenceAvailable() {
		return nil
	}
	g.manualSubscriptionIntentMu.Lock()
	defer g.manualSubscriptionIntentMu.Unlock()
	ctx := g.taskPersistenceContext()
	payload, exists, err := g.loadTaskState(ctx, gbTaskKindManualSubscription, deviceID, key)
	if err != nil || !exists {
		return err
	}
	// 列表扫描取得的是无事务快照。用户可能已在扫描期间重新保存同一意图；
	// 只能删除仍与非法快照完全相同的记录，不能让旧扫描覆盖新状态。
	if string(payload) != expectedPayload {
		return nil
	}
	return g.deleteTaskState(ctx, gbTaskKindManualSubscription, deviceID, key)
}

func validateManualSubscriptionIntentState(
	ctx context.Context,
	state manualSubscriptionIntentState,
	deviceID, key string,
	now time.Time,
) error {
	deviceID = strings.TrimSpace(deviceID)
	key = strings.TrimSpace(key)
	state.Input.DeviceID = strings.TrimSpace(state.Input.DeviceID)
	state.Input.TargetID = strings.TrimSpace(state.Input.TargetID)
	if deviceID == "" || key == "" || state.Input.DeviceID == "" || state.Input.DeviceID != deviceID || state.Input.Cancel {
		return fmt.Errorf("manual subscription identity is invalid")
	}
	if state.Input.TargetID == "" {
		return fmt.Errorf("manual subscription target is missing")
	}
	cmdType, ok := normalizeSubscribeCmdType(state.Input.Event)
	if !ok {
		return fmt.Errorf("manual subscription event is unsupported")
	}
	identityCtx := withMonitorUserIdentityRoute(ctx, state.Identity, state.LocalGatewayID)
	expectedKey := buildOutgoingSubscriptionKey(state.Input.DeviceID, state.Input.TargetID, cmdType, &state.Input) + monitorUserIdentitySubscriptionKey(identityCtx)
	if expectedKey != key {
		return fmt.Errorf("manual subscription key mismatch")
	}
	if state.Identity != nil {
		if _, err := parseMonitorUserIdentity(state.Identity.String()); err != nil {
			return fmt.Errorf("manual subscription identity: %w", err)
		}
		if localGatewayID := strings.TrimSpace(state.LocalGatewayID); !isMonitorIdentityCodeType(localGatewayID, 211, 211) {
			return fmt.Errorf("manual subscription local gateway is invalid")
		}
	}
	if err := validateOutgoingSubscribeExpires(state.Input.Expires); err != nil || state.Input.Expires == 0 {
		if err == nil {
			err = fmt.Errorf("expires must be positive")
		}
		return fmt.Errorf("manual subscription expires: %w", err)
	}
	if state.Input.Interval < 0 {
		return fmt.Errorf("manual subscription interval is invalid")
	}
	if state.CreatedAt.IsZero() || state.UpdatedAt.IsZero() || state.UpdatedAt.Before(state.CreatedAt) {
		return fmt.Errorf("manual subscription timestamps are invalid")
	}
	if now.IsZero() {
		now = time.Now()
	}
	latestAllowed := now.Add(5 * time.Minute)
	if state.CreatedAt.After(latestAllowed) || state.UpdatedAt.After(latestAllowed) {
		return fmt.Errorf("manual subscription timestamp is in the future")
	}
	if !state.LastConfirmedAt.IsZero() && (state.LastConfirmedAt.Before(state.CreatedAt) || state.LastConfirmedAt.After(latestAllowed)) {
		return fmt.Errorf("manual subscription confirmation time is invalid")
	}
	if !state.NextAttemptAt.IsZero() && state.NextAttemptAt.Before(state.UpdatedAt) {
		return fmt.Errorf("manual subscription retry time is invalid")
	}
	if state.Attempts < 0 {
		return fmt.Errorf("manual subscription attempts is invalid")
	}
	if state.RetryBlocked && !state.NextAttemptAt.IsZero() {
		return fmt.Errorf("blocked manual subscription cannot have a retry time")
	}
	return nil
}

func (g *GB28181API) listManualSubscriptionIntentRecords(ctx context.Context, deviceID string, limit int) ([]manualSubscriptionIntentRecord, error) {
	if !g.manualSubscriptionPersistenceAvailable() {
		return nil, nil
	}
	if limit <= 0 || limit > maxManualSubscriptionIntentStates {
		limit = maxManualSubscriptionIntentStates
	}
	records, err := g.listTaskStates(ctx, gbTaskKindManualSubscription, limit)
	if err != nil {
		return nil, err
	}
	deviceID = strings.TrimSpace(deviceID)
	result := make([]manualSubscriptionIntentRecord, 0, len(records))
	for _, record := range records {
		if deviceID != "" && strings.TrimSpace(record.DeviceID) != deviceID {
			continue
		}
		var state manualSubscriptionIntentState
		if err := json.Unmarshal([]byte(record.Payload), &state); err != nil {
			slog.Error("decode manual subscription intent failed", "device_id", record.DeviceID, "key", record.SessionID, "err", err)
			if deleteErr := g.deleteInvalidManualSubscriptionIntentIfUnchanged(record.DeviceID, record.SessionID, record.Payload); deleteErr != nil {
				slog.Warn("delete invalid manual subscription intent failed", "device_id", record.DeviceID, "key", record.SessionID, "err", deleteErr)
			}
			continue
		}
		if err := validateManualSubscriptionIntentState(ctx, state, record.DeviceID, record.SessionID, time.Now()); err != nil {
			slog.Error("invalid manual subscription intent", "device_id", record.DeviceID, "key", record.SessionID, "err", err)
			if deleteErr := g.deleteInvalidManualSubscriptionIntentIfUnchanged(record.DeviceID, record.SessionID, record.Payload); deleteErr != nil {
				slog.Warn("delete invalid manual subscription intent failed", "device_id", record.DeviceID, "key", record.SessionID, "err", deleteErr)
			}
			continue
		}
		g.applyPendingManualSubscriptionTermination(record.SessionID, &state)
		result = append(result, manualSubscriptionIntentRecord{
			Key: record.SessionID, DeviceID: record.DeviceID, IndexedAt: record.UpdatedAt.Time, State: state,
		})
	}
	return result, nil
}

func (g *GB28181API) processManualSubscriptionRecovery(ctx context.Context, deviceID string, force bool) {
	if g == nil || !g.manualSubscriptionPersistenceAvailable() {
		return
	}
	if ctx == nil {
		ctx = g.serviceContext()
	}
	// 先重试 terminated 策略落库；若仍失败，后续列表会叠加内存策略，
	// 不会因持久层还是旧值而错误恢复。
	g.flushPendingManualSubscriptionTerminations(deviceID)
	records, err := g.listManualSubscriptionIntentRecords(ctx, deviceID, maxManualSubscriptionIntentStates)
	if err != nil {
		if !g.serviceStopped() {
			slog.Warn("list manual subscription intents failed", "err", err)
		}
		return
	}
	now := time.Now()
	recoveryAttempts := 0
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return
		}
		if record.State.RetryBlocked {
			continue
		}
		if !record.State.NextAttemptAt.IsZero() && now.Before(record.State.NextAttemptAt) &&
			(!force || record.State.TerminationReason != "") {
			continue
		}
		if value, exists := g.outgoingSubscriptions.Load(record.Key); exists {
			dialog, ok := value.(*outgoingSubscriptionDialog)
			if !ok || dialog == nil {
				g.outgoingSubscriptions.CompareAndDelete(record.Key, value)
			} else {
				dialog.mu.Lock()
				pending := dialog.response == nil
				active := dialog.response != nil && dialog.autoRefresh && !dialog.cancelPending.Load()
				dialog.mu.Unlock()
				if pending || active {
					continue
				}
			}
		}
		unlock, lockErr := g.lockEventSubscriptionOperation(ctx, manualSubscriptionOperationKey(record.Key))
		if lockErr != nil {
			return
		}
		payload, exists, loadErr := g.loadTaskState(ctx, gbTaskKindManualSubscription, record.DeviceID, record.Key)
		if loadErr != nil || !exists {
			unlock()
			continue
		}
		if err := json.Unmarshal(payload, &record.State); err != nil {
			unlock()
			continue
		}
		if err := validateManualSubscriptionIntentState(ctx, record.State, record.DeviceID, record.Key, time.Now()); err != nil {
			if deleteErr := g.deleteInvalidManualSubscriptionIntentIfUnchanged(record.DeviceID, record.Key, string(payload)); deleteErr != nil && !g.serviceStopped() {
				slog.Warn("delete invalid manual subscription intent during recovery failed", "device_id", record.DeviceID, "key", record.Key, "err", deleteErr)
			}
			unlock()
			continue
		}
		g.applyPendingManualSubscriptionTermination(record.Key, &record.State)
		if record.State.RetryBlocked || !record.State.NextAttemptAt.IsZero() && time.Now().Before(record.State.NextAttemptAt) &&
			(!force || record.State.TerminationReason != "") {
			unlock()
			continue
		}
		if recoveryAttempts >= manualSubscriptionRecoveryBatch {
			unlock()
			return
		}
		recoveryAttempts++
		recoveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		recoveryCtx = withMonitorUserIdentityRoute(recoveryCtx, record.State.Identity, record.State.LocalGatewayID)
		recoveryCtx = manualSubscriptionRecoveryContext(recoveryCtx)
		err := g.invokeManualSubscribeRefresh(recoveryCtx, &record.State.Input)
		cancel()
		if err == nil {
			unlock()
			continue
		}
		// 退避必须从本次网络尝试实际结束时开始计算。整批共用开始时间时，
		// 前面的慢超时会让退避在批次结束前过期，并反复占据最早一批记录。
		g.recordManualSubscriptionRecoveryFailure(record, err, time.Now())
		unlock()
		if !errors.Is(err, ErrDeviceOffline) && !errors.Is(err, ErrDeviceNotExist) && !errors.Is(err, context.Canceled) {
			slog.Warn("recover manual device subscription failed", "device_id", record.DeviceID, "target_id", record.State.Input.TargetID, "event", record.State.Input.Event, "err", err)
		}
	}
}

func (g *GB28181API) signalManualSubscriptionRecovery(deviceID string) {
	if g == nil || g.manualSubscriptionRecoveryWake == nil {
		return
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return
	}
	select {
	case g.manualSubscriptionRecoveryWake <- deviceID:
	default:
	}
}

func (g *GB28181API) runManualSubscriptionRecoveryWorker() {
	if g == nil {
		return
	}
	ctx := g.serviceContext()
	// 服务启动后旧 SIP 对话已不可复用；按持久化意图建立全新的订阅对话。
	g.processManualSubscriptionRecovery(ctx, "", true)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-g.lifecycleDone:
			return
		case <-ctx.Done():
			return
		case deviceID := <-g.manualSubscriptionRecoveryWake:
			g.processManualSubscriptionRecovery(ctx, deviceID, true)
		case <-ticker.C:
			g.processManualSubscriptionRecovery(ctx, "", false)
		}
	}
}

func manualSubscriptionIntentStateToOutgoing(record manualSubscriptionIntentRecord) OutgoingSubscriptionState {
	input := record.State.Input
	status := "recovering"
	if record.State.RetryBlocked {
		status = "blocked"
	}
	return OutgoingSubscriptionState{
		DeviceID: input.DeviceID, TargetID: input.TargetID, Event: outgoingSubscriptionEventName(input.Event),
		Status: status, Expires: input.Expires, Persisted: true,
		UpdatedAt: record.State.UpdatedAt, NextAttemptAt: record.State.NextAttemptAt, LastError: record.State.LastError,
		RetryBlocked: record.State.RetryBlocked, TerminationReason: record.State.TerminationReason,
		StartAlarmPriority: input.StartAlarmPriority, EndAlarmPriority: input.EndAlarmPriority,
		AlarmMethod: input.AlarmMethod, AlarmType: input.AlarmType,
		StartAlarmTime: input.StartAlarmTime, EndAlarmTime: input.EndAlarmTime,
		StartTime: input.StartTime, EndTime: input.EndTime, Interval: input.Interval,
	}
}
