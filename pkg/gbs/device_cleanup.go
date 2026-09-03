package gbs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
)

// pendingDeviceOperation 将业务等待绑定到所属设备，便于设备删除时立即收敛。
type pendingDeviceOperation struct {
	deviceID string
	targetID string
	ctx      context.Context
	cancel   context.CancelCauseFunc
	mu       sync.Mutex
}

func newPendingDeviceOperation(ctx context.Context, deviceID string, targetIDs ...string) *pendingDeviceOperation {
	if ctx == nil {
		ctx = context.Background()
	}
	operationCtx, cancel := context.WithCancelCause(ctx)
	operation := &pendingDeviceOperation{
		deviceID: strings.TrimSpace(deviceID),
		ctx:      operationCtx,
		cancel:   cancel,
	}
	if len(targetIDs) > 0 {
		operation.targetID = strings.TrimSpace(targetIDs[0])
	}
	return operation
}

func (g *GB28181API) trackPendingDeviceRequest(ctx context.Context, deviceID string, targetIDs ...string) (*pendingDeviceOperation, func()) {
	operation := newPendingDeviceOperation(ctx, deviceID, targetIDs...)
	if g != nil {
		if operation.deviceID == "" {
			g.pendingDeviceRequests.Store(operation, operation)
		} else {
			unlock := g.lockRegisterOperation(operation.deviceID)
			if err := g.pendingDeviceRequestAdmissionLocked(operation.deviceID); err != nil {
				operation.Cancel(err)
			} else {
				g.pendingDeviceRequests.Store(operation, operation)
			}
			unlock()
		}
	}
	return operation, func() {
		if g != nil {
			g.pendingDeviceRequests.Delete(operation)
		}
		operation.Cancel(nil)
	}
}

func (g *GB28181API) pendingDeviceRequestAdmissionLocked(deviceID string) error {
	if g.deviceDeletionActiveLocked(deviceID) {
		return ErrDeviceNotExist
	}
	if _, offline := g.deviceOfflineTombstones.Load(strings.TrimSpace(deviceID)); offline {
		return ErrDeviceOffline
	}
	return nil
}

// deviceDeletionActiveLocked 在设备注册操作锁内判断删除是否已经提交。
// 协议清理后若持久删除回滚，设备记录仍存在，此时撤销墓碑并恢复后续业务。
func (g *GB28181API) deviceDeletionActiveLocked(deviceID string) bool {
	deviceID = strings.TrimSpace(deviceID)
	if g == nil || deviceID == "" {
		return false
	}
	if _, deleted := g.deviceDeletionTombstones.Load(deviceID); !deleted {
		return false
	}
	store := g.core.Store()
	if store == nil || store.Device() == nil {
		return true
	}
	var device ipc.Device
	if err := store.Device().Get(g.serviceContext(), &device, orm.Where("device_id = ?", deviceID)); err != nil {
		return true
	}
	g.deviceDeletionTombstones.Delete(deviceID)
	return false
}

// clearDeviceDeletionTombstoneIfSettled 在设备删除锁仍持有时检查最终持久化状态。
// 设备已删除或删除事务回滚（设备仍存在）时，墓碑均不再需要；数据库异常时保留
// 墓碑，避免把不确定状态误判为可写入。
func (g *GB28181API) clearDeviceDeletionTombstoneIfSettled(deviceID string) {
	deviceID = strings.TrimSpace(deviceID)
	if g == nil || deviceID == "" {
		return
	}
	if _, ok := g.deviceDeletionTombstones.Load(deviceID); !ok {
		return
	}
	store := g.core.Store()
	if store == nil || store.Device() == nil {
		return
	}
	var device ipc.Device
	err := store.Device().Get(g.serviceContext(), &device, orm.Where("device_id = ?", deviceID))
	if err == nil {
		g.deviceDeletionTombstones.Delete(deviceID)
		return
	}
	if orm.IsErrRecordNotFound(err) {
		g.deviceDeletionTombstones.Delete(deviceID)
		g.deviceOfflineTombstones.Delete(deviceID)
	}
}

// lockInboundDeviceStateCommit 将已通过鉴权的迟到业务提交与设备删除事务串行化。
// 调用方必须持有返回的解锁函数，直到所有内存和持久化副作用完成。
func (g *GB28181API) lockInboundDeviceStateCommit(deviceID string, expected ...inboundRegistrationBinding) (func(), error) {
	deviceID = strings.TrimSpace(deviceID)
	if g == nil || deviceID == "" {
		return nil, ErrDeviceNotExist
	}
	unlock := g.lockRegisterOperation(deviceID)
	if g.deviceDeletionActiveLocked(deviceID) {
		unlock()
		return nil, ErrDeviceNotExist
	}
	if len(expected) > 0 && expected[0].device != nil {
		if !g.inboundRegistrationBindingMatchesLocked(deviceID, expected[0]) {
			unlock()
			return nil, errInboundDeviceGenerationChanged
		}
	}
	return unlock, nil
}

// lockAdmittedInboundDeviceStateCommit 将当前请求在访问控制阶段绑定的设备代次带入提交锁。
// 未经过设备访问控制的内部兼容入口仍只执行删除墓碑检查。
func (g *GB28181API) lockAdmittedInboundDeviceStateCommit(ctx *sip.Context) (func(), error) {
	if ctx == nil {
		return nil, ErrDeviceNotExist
	}
	if binding, ok := admittedInboundRegistrationBinding(ctx); ok {
		return g.lockInboundDeviceStateCommit(ctx.DeviceID, binding)
	}
	return g.lockInboundDeviceStateCommit(ctx.DeviceID)
}

func (o *pendingDeviceOperation) Context(fallback context.Context) context.Context {
	if o == nil || o.ctx == nil {
		return fallback
	}
	return o.ctx
}

func (o *pendingDeviceOperation) Done() <-chan struct{} {
	if o == nil || o.ctx == nil {
		return nil
	}
	return o.ctx.Done()
}

func (o *pendingDeviceOperation) Cause() error {
	if o == nil || o.ctx == nil {
		return context.Canceled
	}
	if cause := context.Cause(o.ctx); cause != nil {
		return cause
	}
	return o.ctx.Err()
}

func (o *pendingDeviceOperation) ErrorOr(err error) error {
	if o != nil && o.ctx != nil && o.ctx.Err() != nil {
		return o.Cause()
	}
	return err
}

func (o *pendingDeviceOperation) Cancel(cause error) {
	if o == nil || o.cancel == nil {
		return
	}
	o.mu.Lock()
	o.cancel(cause)
	o.mu.Unlock()
}

// Deliver 与 Cancel 串行化，保证设备清理完成后迟到响应不能再投递给原请求。
func (o *pendingDeviceOperation) Deliver(deliver func()) bool {
	if deliver == nil {
		return false
	}
	if o == nil || o.ctx == nil {
		deliver()
		return true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ctx.Err() != nil {
		return false
	}
	deliver()
	return true
}

// CleanupDevice 在删除持久化设备前，释放该设备关联的媒体和协议运行态。
func (s *Server) CleanupDevice(ctx context.Context, deviceID string) error {
	if s == nil || s.gb == nil {
		return nil
	}
	return s.gb.cleanupDevice(ctx, deviceID)
}

// LockDeviceDelete 将协议清理和持久化删除与同设备 REGISTER 串行化。
func (s *Server) LockDeviceDelete(deviceID string) func() {
	if s == nil || s.gb == nil {
		return func() {}
	}
	deviceID = strings.TrimSpace(deviceID)
	unlock := s.gb.lockRegisterOperation(deviceID)
	var once sync.Once
	return func() {
		once.Do(func() {
			// 删除协议清理和持久化删除均在该锁内完成。锁释放前检查最终持久化
			// 状态，成功删除或事务回滚都可以安全移除墓碑；数据库异常则保留，
			// 继续阻止迟到请求写入已进入删除流程的设备。
			s.gb.clearDeviceDeletionTombstoneIfSettled(deviceID)
			unlock()
		})
	}
}

// LockDeviceEdit 将设备档案编辑与同设备 REGISTER 串行化。
func (s *Server) LockDeviceEdit(deviceID string) func() {
	return s.LockDeviceDelete(deviceID)
}

// InvalidateDeviceRegistration 在注册凭据改变后关闭旧绑定并释放旧运行态。
// 调用方必须已持有 LockDeviceEdit 返回的设备操作锁。
func (s *Server) InvalidateDeviceRegistration(ctx context.Context, deviceID string) error {
	if s == nil || s.gb == nil || s.memoryStorer == nil {
		return nil
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return fmt.Errorf("GB28181 device ID is required")
	}
	if err := s.changeMemory(ctx, deviceID, func(device *ipc.Device) error {
		device.IsOnline = false
		setPersistedRegistrationClosed(device, true)
		return nil
	}, func(device *Device) {
		device.IsOnline = false
		device.offlinePersistencePending = false
		device.registrationClosed = true
		clearPendingDeviceStatusLocked(device)
		clearPendingKeepaliveLocked(device)
	}); err != nil {
		return fmt.Errorf("invalidate GB28181 registration: %w", err)
	}
	s.gb.deleteRegisterResultsForDevice(deviceID)
	s.gb.cleanupOfflineDeviceRuntime(deviceID)
	return nil
}

// FinalizeDeviceCredentialEdit 在设备编辑事务已经原子关闭 REGISTER 绑定后收敛进程内状态。
// 生产缓存存储会把口令、设备离线、通道离线和绑定关闭放在同一事务；这里不得再次写库，
// 否则第二次写入失败会把已经提交成功的编辑错误地返回为失败。
func (s *Server) FinalizeDeviceCredentialEdit(ctx context.Context, device *ipc.Device) error {
	if s == nil || s.gb == nil || s.memoryStorer == nil || device == nil {
		return nil
	}
	deviceID := strings.TrimSpace(device.GetGB28181DeviceID())
	if deviceID == "" {
		return fmt.Errorf("GB28181 device ID is required")
	}
	if device.IsOnline || !persistedRegistrationClosed(device) {
		// 保留第三方 MemoryStorer 的兼容行为；生产 ipccache 已在编辑事务内关闭绑定。
		return s.InvalidateDeviceRegistration(ctx, deviceID)
	}
	if runtime, ok := s.memoryStorer.Load(deviceID); ok && runtime != nil {
		runtime.UpdateRuntime(func(current *Device) {
			SyncRegistrationBindingRuntime(current, device)
			current.offlinePersistencePending = false
			clearPendingDeviceStatusLocked(current)
			clearPendingKeepaliveLocked(current)
		})
	}
	s.gb.deleteRegisterResultsForDevice(deviceID)
	s.gb.cleanupOfflineDeviceRuntime(deviceID)
	return nil
}

func (g *GB28181API) deleteRegisterResultsForDevice(deviceID string) {
	if g == nil {
		return
	}
	deviceID = strings.TrimSpace(deviceID)
	g.registerResultMu.Lock()
	for key, state := range g.registerResults {
		if state.DeviceID == deviceID {
			delete(g.registerResults, key)
		}
	}
	g.registerResultMu.Unlock()
}

func (g *GB28181API) cleanupDevice(ctx context.Context, deviceID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return fmt.Errorf("GB28181 device ID is required")
	}

	g.cancelPendingDeviceOperations(deviceID, ErrDeviceNotExist)
	// 设备删除已进入协议清理后，所有者订阅必须完整摘除。HTTP 调用方此时取消
	// 只能阻止后续持久化删除，不能留下可能在删除回滚后重新生效的旧订阅。
	g.releaseInboundEventSubscriptionsOwnedByDeviceContext(g.mediaPersistenceContext(), deviceID)
	g.removeCascadeMobilePositionQueriesForDevice(deviceID)
	g.terminateDeviceMediaSessions(ctx, deviceID, "device_deleted")

	g.deviceDeletionTombstones.Store(deviceID, struct{}{})
	g.queryStateMu.Lock()
	g.queryStates.Range(func(key, value any) bool {
		state, _ := value.(*QueryState)
		if key == deviceID || state != nil && state.ownerDeviceID == deviceID {
			g.queryStates.CompareAndDelete(key, value)
		}
		return true
	})
	g.queryStateMu.Unlock()
	g.rtpDownloads.Range(func(key, value any) bool {
		session, _ := value.(*rtpDownloadSession)
		if session != nil && session.snapshot().DeviceID == deviceID {
			g.rtpDownloads.CompareAndDelete(key, value)
		}
		return true
	})
	g.upgradeStateMu.Lock()
	for key, state := range g.upgradeStates {
		if state.DeviceID == deviceID {
			delete(g.upgradeStates, key)
		}
	}
	g.upgradeStateMu.Unlock()
	g.snapshotStateMu.Lock()
	for key, state := range g.snapshotStates {
		if state.DeviceID == deviceID {
			delete(g.snapshotStates, key)
		}
	}
	g.snapshotStateMu.Unlock()
	g.removeCascadeTaskRoutesForDevice(deviceID)
	g.registerNonceMu.Lock()
	for nonce, state := range g.registerNonces {
		if state.DeviceID == deviceID {
			delete(g.registerNonces, nonce)
		}
	}
	g.registerNonceMu.Unlock()
	g.messageNonceMu.Lock()
	for nonce, state := range g.messageNonces {
		if state.DeviceID == deviceID {
			delete(g.messageNonces, nonce)
		}
	}
	g.messageNonceMu.Unlock()
	g.deleteRegisterResultsForDevice(deviceID)
	g.releaseOutgoingSubscriptionsForDeviceContext(ctx, deviceID)
	g.cascadeSubscriptionMu.Lock()
	for key, state := range g.cascadeSubscriptions {
		if state != nil && state.Input.DeviceID == deviceID {
			delete(g.cascadeSubscriptions, key)
		}
	}
	g.cascadeSubscriptionMu.Unlock()
	return nil
}

// releaseInboundEventSubscriptionsOwnedByDeviceContext 清理设备作为订阅方建立的事件订阅。
// 订阅目标可能是其他设备，因此不能仅按事件正文中的 DeviceID 判断所有者。
func (g *GB28181API) releaseInboundEventSubscriptionsOwnedByDeviceContext(ctx context.Context, deviceID string) {
	if g == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return
	}
	g.eventSubscribers.Range(func(rawKey, value any) bool {
		if ctx.Err() != nil {
			return false
		}
		sub, ok := value.(*eventSubscription)
		if !ok || sub == nil {
			return true
		}
		sub.mu.Lock()
		ownerDeviceID := strings.TrimSpace(sub.OwnerDeviceID)
		operationKey := strings.TrimSpace(sub.Key)
		sub.mu.Unlock()
		if ownerDeviceID != deviceID {
			return true
		}
		if operationKey == "" {
			operationKey, _ = rawKey.(string)
		}
		unlock, err := g.lockEventSubscriptionOperation(ctx, operationKey)
		if err != nil {
			return ctx.Err() == nil
		}
		current, loaded := g.eventSubscribers.Load(rawKey)
		removed := loaded && current == sub && g.eventSubscribers.CompareAndDelete(rawKey, sub)
		var downstreamKeys []string
		if removed {
			sub.mu.Lock()
			sub.ExpiresAt = time.Now()
			sub.DialogRequest = nil
			sub.Response = nil
			downstreamKeys = append(downstreamKeys, sub.DownstreamKeys...)
			sub.DownstreamKeys = nil
			sub.mu.Unlock()
		}
		unlock()
		if removed {
			g.releaseCascadeDownstreamSubscriptions(ctx, downstreamKeys)
		}
		return ctx.Err() == nil
	})
}

// releaseOutgoingSubscriptionsForDevice 在删除仍在线的设备前，尽力发送同对话退订。
// 发送或应答失败不能阻止设备删除，本地索引始终立即释放。
func (g *GB28181API) releaseOutgoingSubscriptionsForDevice(deviceID string) {
	g.releaseOutgoingSubscriptionsForDeviceContext(g.serviceContext(), deviceID)
}

func (g *GB28181API) releaseOutgoingSubscriptionsForDeviceContext(ctx context.Context, deviceID string) {
	if g == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return
	}

	g.outgoingSubscriptions.Range(func(key, value any) bool {
		keyText, ok := key.(string)
		if !ok || !strings.HasPrefix(keyText, deviceID+"|") {
			return true
		}
		dialog, ok := value.(*outgoingSubscriptionDialog)
		if !ok || dialog == nil {
			g.outgoingSubscriptions.CompareAndDelete(key, value)
			return true
		}
		dialog.notifyOperationMu.Lock()
		deleted := g.outgoingSubscriptions.CompareAndDelete(key, dialog)
		dialog.notifyOperationMu.Unlock()
		if !deleted {
			return true
		}
		tx, targetID, err := g.sendOutgoingSubscriptionCancellationContext(ctx, dialog, false)
		if err != nil {
			slog.Warn("send outgoing subscription cancellation failed", "device_id", deviceID, "target_id", targetID, "err", err)
			return true
		}
		if tx == nil {
			return true
		}
		if !g.startLifecycleTask(context.Background(), func(taskCtx context.Context) {
			responseCtx, cancel := context.WithTimeout(taskCtx, 3*time.Second)
			defer cancel()
			if _, responseErr := sipResponseContext(responseCtx, tx); responseErr != nil && responseCtx.Err() == nil {
				slog.Warn("wait outgoing subscription cancellation acknowledgement failed", "device_id", deviceID, "target_id", targetID, "err", responseErr)
			}
		}) {
			tx.Close()
		}
		return true
	})
}

func (g *GB28181API) sendOutgoingSubscriptionCancellationContext(ctx context.Context, dialog *outgoingSubscriptionDialog, allowStopped bool) (*sip.Transaction, string, error) {
	if g == nil || dialog == nil {
		return nil, "", nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dialog.mu.Lock()
	response := dialog.response
	body := append([]byte(nil), dialog.requestBody...)
	eventValue := dialog.eventValue
	deviceID := strings.TrimSpace(dialog.deviceID)
	targetID := strings.TrimSpace(dialog.targetID)
	identity := dialog.identity.clone()
	localGatewayID := strings.TrimSpace(dialog.localGatewayID)
	dialog.response = nil
	dialog.requestBody = nil
	dialog.eventValue = ""
	dialog.identity = nil
	dialog.localGatewayID = ""
	dialog.expiresAt = time.Time{}
	dialog.expires = 0
	dialog.refreshAt = time.Time{}
	dialog.refreshing = false
	dialog.mu.Unlock()
	dialog.clearPendingNotifyDialog()
	if response == nil || len(body) == 0 || strings.TrimSpace(eventValue) == "" || g.svr == nil || g.svr.memoryStorer == nil {
		return nil, targetID, nil
	}

	device, exists := g.svr.memoryStorer.Load(deviceID)
	if !exists || device == nil || !device.IsOnlineNow() {
		return nil, targetID, nil
	}
	var target Targeter = device
	if targetID != "" && targetID != deviceID {
		channel, channelExists := g.svr.memoryStorer.GetChannel(deviceID, targetID)
		if !channelExists || channel == nil {
			return nil, targetID, nil
		}
		target = channel
	}

	identityCtx := withMonitorUserIdentityRoute(ctx, identity, localGatewayID)
	prepare := func(request *sip.Request) error {
		applyOutgoingSubscriptionPayload(g.svr, target, request, body, eventValue, 0)
		return nil
	}
	var tx *sip.Transaction
	var err error
	if allowStopped {
		tx, err = g.svr.requestFromResponseContextInternal(identityCtx, target, sip.MethodSubscribe, response, true, prepare, nil)
	} else {
		tx, err = g.svr.requestFromResponsePreparedContext(identityCtx, target, sip.MethodSubscribe, response, prepare)
	}
	if err != nil {
		return nil, targetID, err
	}
	return tx, targetID, nil
}

func (g *GB28181API) cancelPendingDeviceOperations(deviceID string, cause error) {
	if g == nil {
		return
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return
	}
	if cause == nil {
		cause = context.Canceled
	}
	for _, pendingMap := range []*sync.Map{
		&g.pendingDeviceControl,
		&g.pendingDeviceQuery,
		&g.pendingMultiResponse,
		&g.pendingDeviceRequests,
		&g.pendingDeviceConfig,
		&g.pendingBroadcast,
	} {
		pendingMap.Range(func(key, value any) bool {
			operation := pendingOperation(value)
			if operation == nil || operation.deviceID != deviceID {
				return true
			}
			cancelPendingValue(value, cause)
			pendingMap.CompareAndDelete(key, value)
			return true
		})
	}
	if g.catalogResponses != nil {
		g.catalogResponses.AbortPrefix(deviceID+":Catalog:", cause)
	}
	g.recordResponseAliases.Range(func(key, value any) bool {
		keyText, ok := key.(string)
		if !ok || !strings.HasPrefix(keyText, deviceID+":RecordInfo:") {
			return true
		}
		if recordKey, generation, ok := recordResponseAliasDetails(value); ok {
			if g.recordResponses != nil {
				g.recordResponses.Abort(recordKey, cause)
			}
			if generation != nil {
				g.clearRecordResponseExtraGeneration(recordKey, generation)
			} else {
				g.clearRecordResponseExtra(recordKey)
			}
		}
		g.recordResponseAliases.CompareAndDelete(key, value)
		return true
	})
}

func (g *GB28181API) cancelAllPendingDeviceRequests(cause error) {
	if g == nil {
		return
	}
	if cause == nil {
		cause = context.Canceled
	}
	for _, pendingMap := range []*sync.Map{
		&g.pendingDeviceControl,
		&g.pendingDeviceQuery,
		&g.pendingMultiResponse,
		&g.pendingDeviceRequests,
		&g.pendingDeviceConfig,
		&g.pendingBroadcast,
	} {
		pendingMap.Range(func(key, value any) bool {
			cancelPendingValue(value, cause)
			pendingMap.CompareAndDelete(key, value)
			return true
		})
	}
}

func (g *GB28181API) cancelPendingChannelOperations(deviceID string, channelIDs map[string]struct{}, cause error) {
	if g == nil || len(channelIDs) == 0 {
		return
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return
	}
	if cause == nil {
		cause = context.Canceled
	}
	for _, pendingMap := range []*sync.Map{
		&g.pendingDeviceControl,
		&g.pendingDeviceQuery,
		&g.pendingMultiResponse,
		&g.pendingDeviceRequests,
		&g.pendingDeviceConfig,
		&g.pendingBroadcast,
	} {
		pendingMap.Range(func(key, value any) bool {
			operation := pendingOperation(value)
			if operation == nil || operation.deviceID != deviceID {
				return true
			}
			if _, matches := channelIDs[operation.targetID]; !matches {
				return true
			}
			cancelPendingValue(value, cause)
			pendingMap.CompareAndDelete(key, value)
			return true
		})
	}
	g.recordResponseAliases.Range(func(key, value any) bool {
		keyText, keyOK := key.(string)
		recordKey, generation, valueOK := recordResponseAliasDetails(value)
		if !keyOK || !valueOK || !strings.HasPrefix(keyText, deviceID+":RecordInfo:") {
			return true
		}
		separator := strings.IndexByte(recordKey, ':')
		if separator <= 0 {
			return true
		}
		if _, matches := channelIDs[recordKey[:separator]]; !matches {
			return true
		}
		if g.recordResponses != nil {
			g.recordResponses.Abort(recordKey, cause)
		}
		if generation != nil {
			g.clearRecordResponseExtraGeneration(recordKey, generation)
		} else {
			g.clearRecordResponseExtra(recordKey)
		}
		g.recordResponseAliases.CompareAndDelete(key, value)
		return true
	})
}

func pendingOperation(value any) *pendingDeviceOperation {
	switch pending := value.(type) {
	case *pendingDeviceControl:
		if pending != nil {
			return pending.operation
		}
	case *pendingQueryWait:
		if pending != nil {
			return pending.operation
		}
	case *pendingDeviceConfig:
		if pending != nil {
			return pending.operation
		}
	case *pendingBroadcastResponse:
		if pending != nil {
			return pending.operation
		}
	case *pendingDeviceOperation:
		return pending
	}
	return nil
}

func cancelPendingValue(value any, cause error) {
	if pending, ok := value.(*pendingQueryWait); ok {
		pending.cancel(cause)
		return
	}
	if operation := pendingOperation(value); operation != nil {
		operation.Cancel(cause)
	}
}

// terminateDeviceMediaSessions 在设备注销、离线或删除时释放其现有媒体链路。
// 只删除迭代时观察到的会话指针，避免并发重注册后误删新建立的同键会话。
func (g *GB28181API) terminateDeviceMediaSessions(ctx context.Context, deviceID, reason string) {
	g.terminateMediaSessions(ctx, deviceID, nil, reason)
}

func (g *GB28181API) terminateChannelMediaSessions(ctx context.Context, deviceID string, channelIDs map[string]struct{}, reason string) {
	if len(channelIDs) == 0 {
		return
	}
	g.cancelPendingChannelOperations(deviceID, channelIDs, ErrChannelOffline)
	g.removeCascadeMobilePositionQueriesForChannels(deviceID, channelIDs)
	g.terminateMediaSessions(ctx, deviceID, channelIDs, reason)
}

func (g *GB28181API) terminateMediaSessions(ctx context.Context, deviceID string, channelIDs map[string]struct{}, reason string) {
	if g == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	deviceID = strings.TrimSpace(deviceID)
	reason = strings.TrimSpace(reason)
	if deviceID == "" {
		return
	}
	if reason == "" {
		reason = "device_offline"
	}
	channelAllowed := func(channelID string) bool {
		if channelIDs == nil {
			return true
		}
		_, allowed := channelIDs[strings.TrimSpace(channelID)]
		return allowed
	}
	cause := fmt.Errorf("GB28181 device %s media stopped: %s", deviceID, reason)
	g.talkSessions.Range(func(_, value any) bool {
		session, _ := value.(*talkSession)
		if session == nil || session.DeviceID != deviceID || !channelAllowed(session.ChannelID) {
			return true
		}
		if err := g.stopTalkSession(session, cause); err != nil {
			slog.WarnContext(ctx, "停止离线设备的语音对讲失败", "device_id", deviceID, "reason", reason, "err", err)
		} else if g.streams != nil && session.Stream != nil {
			g.compareAndDeleteChannelStream(voiceKey(voiceModeTalk, session.DeviceID, session.ChannelID), session.Stream)
		}
		return true
	})
	g.broadcastSessions.Range(func(_, value any) bool {
		session, _ := value.(*broadcastSession)
		if session == nil || session.DeviceID != deviceID || !channelAllowed(session.ChannelID) {
			return true
		}
		if err := g.stopBroadcastSession(session, true); err != nil {
			slog.WarnContext(ctx, "停止离线设备的语音广播失败", "device_id", deviceID, "reason", reason, "err", err)
		}
		return true
	})

	if g.directDownloads != nil && channelIDs == nil {
		g.directDownloads.CancelDevice(deviceID)
	} else if g.directDownloads != nil {
		for channelID := range channelIDs {
			g.directDownloads.CancelChannel(deviceID, channelID)
		}
	}
	if g.streams != nil {
		g.streams.Range(func(key string, stream *Streams) bool {
			if stream == nil || stream.DeviceID != deviceID || !channelAllowed(stream.ChannelID) || g.pendingVoiceCleanupOwnsStream(key, stream) {
				return true
			}
			if stream.DirectTCP && g.directDownloads != nil {
				g.markMediaStreamStopped(stream, reason, true)
				if g.directDownloads.Cancel(stream.DirectSessionID) {
					return true
				}
			}
			firstStop := g.markMediaStreamStopped(stream, reason, false)
			if firstStop && strings.HasPrefix(key, "history:"+historyModeDownload+":") {
				g.finishRTPDownload(stream, rtpDownloadStopped, reason)
			}
			if firstStop {
				g.terminateCascadeSessionsForStream(stream)
			}
			if _, err := g.cleanupMediaStreamContext(ctx, key, stream); err != nil {
				slog.WarnContext(ctx, "清理离线设备的媒体会话失败", "device_id", deviceID, "channel_id", stream.ChannelID, "stream_id", stream.StreamID, "reason", reason, "err", err)
			}
			return true
		})
	}
}
