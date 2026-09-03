package gbs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

const (
	videoUploadOutboxPollInterval  = time.Second
	videoUploadOutboxMaxRetryDelay = time.Minute
	videoUploadOutboxBatchSize     = 100
)

type videoUploadOutboxProcessResult uint8

const (
	videoUploadOutboxProcessIdle videoUploadOutboxProcessResult = iota
	videoUploadOutboxProcessMaintenance
	videoUploadOutboxProcessDelivery
)

// persistVideoUploadOutbox 在 SIP 成功应答前保存待投递事实，关闭应答写出后进程崩溃的丢失窗口。
// bool 返回 true 表示此次通知已经由持久化 outbox/receipt 接管；可选任务存储不可用时返回 false，
// 调用方继续使用原有进程内异步投递兼容路径。
func (g *GB28181API) persistVideoUploadOutbox(
	ctx context.Context,
	sourceDeviceID string,
	body []byte,
	binding inboundRegistrationBinding,
	hasBinding bool,
) (string, bool, error) {
	if g == nil || g.svr == nil || g.svr.cascade == nil {
		return "", false, nil
	}
	store := g.taskStateStorer()
	if store == nil {
		return "", false, nil
	}
	if _, ok := store.(gbTaskStateLister); !ok {
		return "", false, nil
	}
	sourceDeviceID = strings.TrimSpace(sourceDeviceID)
	if sourceDeviceID == "" || len(body) == 0 {
		return "", false, fmt.Errorf("invalid VideoUploadNotify outbox payload")
	}
	outboxID := videoUploadOutboxID(sourceDeviceID, body)
	unlock, err := g.lockAlarmInboxOperation(ctx, "video-upload-outbox:"+outboxID)
	if err != nil {
		return outboxID, false, fmt.Errorf("lock VideoUploadNotify outbox: %w", err)
	}
	defer unlock()

	if _, exists, err := g.loadTaskState(ctx, gbTaskKindVideoUploadOutbox, sourceDeviceID, outboxID); err != nil {
		return outboxID, false, err
	} else if exists {
		return outboxID, true, nil
	}

	// 事件到达时上级可能尚未 REGISTER；仍需把已配置目标写入 outbox，
	// 由 worker 在重新注册后补发，不能因瞬时离线退回易丢失的进程内路径。
	workers := g.svr.cascade.configuredWorkers(GBVersion30)
	platforms := make([]string, 0, len(workers))
	seen := make(map[string]struct{}, len(workers))
	for _, worker := range workers {
		if worker == nil {
			continue
		}
		name := strings.TrimSpace(worker.platform.name)
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		receiptID := videoUploadReceiptID(sourceDeviceID, name, body)
		unlockReceipt, err := g.lockAlarmInboxOperation(ctx, "video-upload:"+receiptID)
		if err != nil {
			return outboxID, false, fmt.Errorf("%s: lock VideoUploadNotify receipt: %w", name, err)
		}
		completed, receiptErr := g.videoUploadReceiptExists(ctx, sourceDeviceID, receiptID)
		unlockReceipt()
		err = receiptErr
		if err != nil {
			return outboxID, false, fmt.Errorf("%s: inspect VideoUploadNotify receipt: %w", name, err)
		}
		if completed {
			continue
		}
		seen[name] = struct{}{}
		platforms = append(platforms, name)
	}
	if len(platforms) == 0 {
		// 没有当前已注册的 3.0 上级，或所有目标均已有持久化回执。
		// 后一种情况必须视为已经接管，避免重复通知回退到非持久化路径。
		return outboxID, len(workers) > 0, nil
	}
	sort.Strings(platforms)
	now := time.Now()
	state := videoUploadOutboxState{
		SourceDeviceID: sourceDeviceID,
		Body:           append([]byte(nil), body...),
		Platforms:      platforms,
		ReceivedAt:     now,
		HasBinding:     hasBinding,
	}
	if hasBinding {
		state.BindingRegisteredAt = binding.lastRegisterAt
		state.BindingExpires = binding.expires
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return outboxID, false, fmt.Errorf("encode VideoUploadNotify outbox: %w", err)
	}
	if err := g.saveTaskState(ctx, gbTaskKindVideoUploadOutbox, sourceDeviceID, outboxID, payload, now); err != nil {
		return outboxID, false, err
	}
	return outboxID, true, nil
}

func videoUploadOutboxID(sourceDeviceID string, body []byte) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(strings.TrimSpace(sourceDeviceID)))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(body)
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func (g *GB28181API) startVideoUploadOutboxWorker() {
	if g == nil || g.serviceDone() == nil || g.videoUploadOutboxWake == nil {
		return
	}
	store := g.taskStateStorer()
	if store == nil {
		return
	}
	if _, ok := store.(gbTaskStateLister); !ok {
		return
	}
	g.videoUploadOutboxWorkerOnce.Do(func() {
		g.startLifecycleWorker(g.runVideoUploadOutboxWorker)
	})
}

func (g *GB28181API) signalVideoUploadOutboxWorker() {
	if g == nil || g.videoUploadOutboxWake == nil {
		return
	}
	select {
	case g.videoUploadOutboxWake <- struct{}{}:
	default:
	}
}

func (g *GB28181API) runVideoUploadOutboxWorker() {
	g.processVideoUploadOutboxBatch(time.Now())
	ticker := time.NewTicker(videoUploadOutboxPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-g.serviceDone():
			return
		case <-g.videoUploadOutboxWake:
			g.processVideoUploadOutboxBatch(time.Now())
		case now := <-ticker.C:
			g.processVideoUploadOutboxBatch(now)
		}
	}
}

func (g *GB28181API) processVideoUploadOutboxBatch(now time.Time) {
	if g == nil || g.taskStateStorer() == nil || g.svr == nil || g.svr.cascade == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	seen := make(map[string]struct{}, videoUploadOutboxBatchSize)
	deliveries := 0
	for deliveries < videoUploadOutboxBatchSize && len(seen) < maxVideoUploadOutboxStates {
		records, err := g.listTaskStates(g.serviceContext(), gbTaskKindVideoUploadOutbox, videoUploadOutboxBatchSize)
		if err != nil {
			if !g.serviceStopped() {
				slog.Warn("list VideoUploadNotify outbox failed", "err", err)
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
			result := g.processVideoUploadOutbox(record.DeviceID, record.SessionID, record.UpdatedAt.Time, now)
			if result != videoUploadOutboxProcessIdle {
				progressed = true
			}
			if result == videoUploadOutboxProcessDelivery {
				deliveries++
				if deliveries >= videoUploadOutboxBatchSize {
					return
				}
			}
		}
		if !discovered || !progressed || len(records) < videoUploadOutboxBatchSize {
			return
		}
	}
}

func (g *GB28181API) processVideoUploadOutbox(deviceID, outboxID string, indexedAt, now time.Time) videoUploadOutboxProcessResult {
	ctx := g.serviceContext()
	unlock, err := g.lockAlarmInboxOperation(ctx, "video-upload-outbox:"+outboxID)
	if err != nil {
		return videoUploadOutboxProcessIdle
	}
	defer unlock()
	payload, exists, err := g.loadTaskState(ctx, gbTaskKindVideoUploadOutbox, deviceID, outboxID)
	if err != nil || !exists {
		if err != nil && !g.serviceStopped() {
			slog.Warn("load VideoUploadNotify outbox failed", "device_id", deviceID, "outbox_id", outboxID, "err", err)
		}
		return videoUploadOutboxProcessIdle
	}
	var state videoUploadOutboxState
	if err := json.Unmarshal(payload, &state); err != nil {
		slog.Error("decode VideoUploadNotify outbox failed", "device_id", deviceID, "outbox_id", outboxID, "err", err)
		if deleteErr := g.deleteTaskState(g.taskPersistenceContext(), gbTaskKindVideoUploadOutbox, deviceID, outboxID); deleteErr != nil {
			slog.Warn("delete invalid VideoUploadNotify outbox failed", "device_id", deviceID, "outbox_id", outboxID, "err", deleteErr)
			return videoUploadOutboxProcessIdle
		}
		return videoUploadOutboxProcessMaintenance
	}
	if err := validateVideoUploadOutboxState(state, deviceID, outboxID, now); err != nil {
		slog.Error("invalid VideoUploadNotify outbox", "device_id", deviceID, "outbox_id", outboxID, "err", err)
		if deleteErr := g.deleteTaskState(g.taskPersistenceContext(), gbTaskKindVideoUploadOutbox, deviceID, outboxID); deleteErr != nil {
			slog.Warn("delete invalid VideoUploadNotify outbox failed", "device_id", deviceID, "outbox_id", outboxID, "err", deleteErr)
			return videoUploadOutboxProcessIdle
		}
		return videoUploadOutboxProcessMaintenance
	}
	if !state.NextAttemptAt.IsZero() && now.Before(state.NextAttemptAt) {
		// 升级前的实现使用尝试时间作为 updated_at，尚未到期的记录会长期占据
		// 固定大小批次的前部。只重排一次到 NextAttemptAt，让后续已到期通知可见。
		if indexedAt.IsZero() || !indexedAt.Before(state.NextAttemptAt) {
			return videoUploadOutboxProcessIdle
		}
		updated, encodeErr := json.Marshal(state)
		if encodeErr == nil {
			encodeErr = g.saveTaskState(
				g.taskPersistenceContext(), gbTaskKindVideoUploadOutbox, deviceID, outboxID, updated, state.NextAttemptAt,
			)
		}
		if encodeErr != nil {
			slog.Warn("reindex deferred VideoUploadNotify outbox failed", "device_id", deviceID, "outbox_id", outboxID, "err", encodeErr)
			return videoUploadOutboxProcessIdle
		}
		return videoUploadOutboxProcessMaintenance
	}

	unlockDevice, err := g.lockInboundDeviceStateCommit(state.SourceDeviceID)
	if err != nil {
		if errors.Is(err, ErrDeviceNotExist) {
			if deleteErr := g.deleteTaskState(g.taskPersistenceContext(), gbTaskKindVideoUploadOutbox, deviceID, outboxID); deleteErr != nil {
				slog.Warn("delete orphan VideoUploadNotify outbox failed", "device_id", deviceID, "outbox_id", outboxID, "err", deleteErr)
				return videoUploadOutboxProcessIdle
			}
			return videoUploadOutboxProcessMaintenance
		}
		return videoUploadOutboxProcessIdle
	}
	defer unlockDevice()
	if state.HasBinding && !g.videoUploadOutboxBindingMatchesLocked(state) {
		if err := g.deleteTaskState(g.taskPersistenceContext(), gbTaskKindVideoUploadOutbox, deviceID, outboxID); err != nil {
			slog.Warn("delete stale VideoUploadNotify outbox failed", "device_id", deviceID, "outbox_id", outboxID, "err", err)
			return videoUploadOutboxProcessIdle
		}
		return videoUploadOutboxProcessMaintenance
	}

	pending := make([]string, 0, len(state.Platforms))
	var deliveryErrors []error
	for _, platformName := range state.Platforms {
		worker, ok := g.svr.cascade.workerByName(platformName)
		if !ok || !worker.protocolVersion().AtLeast(GBVersion30) {
			// 目标已被配置删除或降级为旧版本，不再保留无法完成的 2022 通知。
			continue
		}
		if !worker.registrationActive(now) {
			pending = append(pending, platformName)
			deliveryErrors = append(deliveryErrors, fmt.Errorf("%s: upstream is not registered", platformName))
			continue
		}
		completed, err := g.forwardCascadeVideoUploadNotifyToWorker(ctx, state.SourceDeviceID, state.Body, worker)
		if err != nil || !completed {
			pending = append(pending, platformName)
			if err != nil {
				deliveryErrors = append(deliveryErrors, err)
			}
		}
	}
	if len(pending) == 0 {
		if err := g.deleteTaskState(g.taskPersistenceContext(), gbTaskKindVideoUploadOutbox, deviceID, outboxID); err != nil {
			slog.Warn("delete completed VideoUploadNotify outbox failed", "device_id", deviceID, "outbox_id", outboxID, "err", err)
		}
		return videoUploadOutboxProcessDelivery
	}
	state.Platforms = pending
	state.Attempt++
	state.NextAttemptAt = now.Add(videoUploadOutboxRetryDelay(state.Attempt))
	if joined := errors.Join(deliveryErrors...); joined != nil {
		state.LastError = joined.Error()
	} else {
		state.LastError = "VideoUploadNotify delivery remains pending"
	}
	updated, err := json.Marshal(state)
	if err == nil {
		err = g.saveTaskState(
			g.taskPersistenceContext(), gbTaskKindVideoUploadOutbox, deviceID, outboxID, updated, state.NextAttemptAt,
		)
	}
	if err != nil {
		slog.Warn("update VideoUploadNotify outbox retry failed", "device_id", deviceID, "outbox_id", outboxID, "err", err)
	}
	return videoUploadOutboxProcessDelivery
}

func validateVideoUploadOutboxState(state videoUploadOutboxState, deviceID, outboxID string, now time.Time) error {
	deviceID = strings.TrimSpace(deviceID)
	outboxID = strings.TrimSpace(outboxID)
	if !isGBDeviceIdentifier(deviceID) || strings.TrimSpace(state.SourceDeviceID) != deviceID || len(state.Body) == 0 {
		return errors.New("VideoUploadNotify outbox identity is invalid")
	}
	if videoUploadOutboxID(deviceID, state.Body) != outboxID {
		return errors.New("VideoUploadNotify outbox payload does not match its key")
	}
	if now.IsZero() {
		now = time.Now()
	}
	latestAllowed := now.Add(5 * time.Minute)
	if state.ReceivedAt.IsZero() || state.ReceivedAt.After(latestAllowed) || state.Attempt < 0 ||
		(!state.NextAttemptAt.IsZero() && (state.NextAttemptAt.Before(state.ReceivedAt) || state.NextAttemptAt.After(latestAllowed))) {
		return errors.New("VideoUploadNotify outbox timing is invalid")
	}
	if state.HasBinding && (state.BindingRegisteredAt.IsZero() || state.BindingRegisteredAt.After(latestAllowed) || state.BindingExpires <= 0) {
		return errors.New("VideoUploadNotify outbox registration binding is invalid")
	}
	if len(state.Platforms) == 0 {
		return errors.New("VideoUploadNotify outbox has no target platform")
	}
	seen := make(map[string]struct{}, len(state.Platforms))
	for _, platformName := range state.Platforms {
		name := strings.TrimSpace(platformName)
		if name == "" || name != platformName {
			return errors.New("VideoUploadNotify outbox contains an invalid target platform")
		}
		if _, duplicate := seen[name]; duplicate {
			return errors.New("VideoUploadNotify outbox contains duplicate target platforms")
		}
		seen[name] = struct{}{}
	}
	return nil
}

// videoUploadOutboxBindingMatchesLocked 仅在持有同设备 REGISTER 操作锁时调用。
func (g *GB28181API) videoUploadOutboxBindingMatchesLocked(state videoUploadOutboxState) bool {
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return false
	}
	device, ok := g.svr.memoryStorer.Load(strings.TrimSpace(state.SourceDeviceID))
	if !ok || device == nil {
		return false
	}
	current := device.runtimeSnapshot()
	return runtimeRegistrationBindingActive(current, time.Now()) &&
		current.Expires == state.BindingExpires && current.LastRegisterAt.Equal(state.BindingRegisteredAt)
}

func videoUploadOutboxRetryDelay(attempt int) time.Duration {
	if attempt <= 1 {
		return videoUploadOutboxPollInterval
	}
	delay := videoUploadOutboxPollInterval
	for i := 1; i < attempt && delay < videoUploadOutboxMaxRetryDelay; i++ {
		delay *= 2
		if delay >= videoUploadOutboxMaxRetryDelay {
			return videoUploadOutboxMaxRetryDelay
		}
	}
	return delay
}
