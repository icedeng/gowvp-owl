package gbs

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func subscriptionFilterFromRequest(req subscribeEventRequest) eventSubscriptionFilter {
	filter := eventSubscriptionFilter{}
	switch {
	case strings.EqualFold(strings.TrimSpace(req.CmdType), "Alarm"):
		filter.StartAlarmPriority = strings.TrimSpace(req.StartAlarmPriority)
		filter.EndAlarmPriority = strings.TrimSpace(req.EndAlarmPriority)
		filter.AlarmMethod, _ = normalizeAlarmMethodFilter(req.AlarmMethod)
		filter.AlarmType = strings.TrimSpace(req.AlarmType)
		filter.StartAlarmTime = strings.TrimSpace(req.StartAlarmTime)
		filter.EndAlarmTime = strings.TrimSpace(req.EndAlarmTime)
	case strings.EqualFold(strings.TrimSpace(req.CmdType), "Catalog"):
		filter.CatalogStartTime = strings.TrimSpace(req.StartTime)
		filter.CatalogEndTime = strings.TrimSpace(req.EndTime)
	}
	return filter
}

func validateSubscribeEventEnvelope(req subscribeEventRequest, cmdType string) error {
	if req.SN <= 0 {
		return fmt.Errorf("invalid subscription SN")
	}
	deviceID := strings.TrimSpace(req.DeviceID)
	if strings.EqualFold(strings.TrimSpace(cmdType), "Catalog") {
		if deviceID != "*" && classifyGBCatalogItem(deviceID) == GBCatalogItemUnknown {
			return fmt.Errorf("invalid subscription DeviceID")
		}
	} else if !isGBDeviceIdentifier(deviceID) {
		return fmt.Errorf("invalid subscription DeviceID")
	}
	if _, ok := subscribeEventMinimumVersion(cmdType); !ok {
		return fmt.Errorf("unsupported subscribe cmd_type")
	}
	return nil
}

func validateSubscribeEventRequest(req subscribeEventRequest, cmdType string, version GBProtocolVersion) error {
	if err := validateSubscribeEventEnvelope(req, cmdType); err != nil {
		return err
	}
	if req.Interval != nil && *req.Interval <= 0 {
		return fmt.Errorf("subscription interval must be positive")
	}
	minimum, _ := subscribeEventMinimumVersion(cmdType)
	if !version.AtLeast(minimum) {
		return fmt.Errorf("%s subscription requires %s or later", cmdType, minimum.StandardName())
	}
	if strings.EqualFold(strings.TrimSpace(cmdType), "Catalog") {
		deviceID := strings.TrimSpace(req.DeviceID)
		if deviceID != "*" && !validCatalogTargetID(version, deviceID) {
			return fmt.Errorf("Catalog subscription target is not supported by %s", version.StandardName())
		}
	}
	alarmFieldsPresent := strings.TrimSpace(req.StartAlarmPriority) != "" || strings.TrimSpace(req.EndAlarmPriority) != "" ||
		strings.TrimSpace(req.AlarmMethod) != "" || strings.TrimSpace(req.AlarmType) != "" ||
		strings.TrimSpace(req.StartAlarmTime) != "" || strings.TrimSpace(req.EndAlarmTime) != ""
	switch strings.TrimSpace(cmdType) {
	case "Catalog":
		if alarmFieldsPresent || req.Interval != nil {
			return fmt.Errorf("Catalog subscription contains fields from another event")
		}
		startTime, hasStart, err := parseSubscriptionTime(req.StartTime)
		if err != nil {
			return fmt.Errorf("invalid StartTime: %w", err)
		}
		endTime, hasEnd, err := parseSubscriptionTime(req.EndTime)
		if err != nil {
			return fmt.Errorf("invalid EndTime: %w", err)
		}
		if hasStart && hasEnd && endTime.Before(startTime) {
			return fmt.Errorf("StartTime must not be after EndTime")
		}
		return nil
	case "MobilePosition":
		if alarmFieldsPresent || strings.TrimSpace(req.StartTime) != "" || strings.TrimSpace(req.EndTime) != "" {
			return fmt.Errorf("MobilePosition subscription contains fields from another event")
		}
		return nil
	case "PTZPosition":
		if alarmFieldsPresent || strings.TrimSpace(req.StartTime) != "" || strings.TrimSpace(req.EndTime) != "" || req.Interval != nil {
			return fmt.Errorf("PTZPosition subscription contains fields from another event")
		}
		return nil
	case "Alarm":
		if req.Interval != nil {
			return fmt.Errorf("Alarm subscription contains fields from another event")
		}
	default:
		return nil
	}
	startAlarmTime := strings.TrimSpace(req.StartAlarmTime)
	if value := strings.TrimSpace(req.StartTime); value != "" {
		if startAlarmTime != "" && startAlarmTime != value {
			return fmt.Errorf("Alarm subscription contains conflicting StartTime fields")
		}
		startAlarmTime = value
	}
	endAlarmTime := strings.TrimSpace(req.EndAlarmTime)
	if value := strings.TrimSpace(req.EndTime); value != "" {
		if endAlarmTime != "" && endAlarmTime != value {
			return fmt.Errorf("Alarm subscription contains conflicting EndTime fields")
		}
		endAlarmTime = value
	}
	start, err := parseAlarmPriorityFilter(req.StartAlarmPriority)
	if err != nil {
		return fmt.Errorf("invalid StartAlarmPriority: %w", err)
	}
	end, err := parseAlarmPriorityFilter(req.EndAlarmPriority)
	if err != nil {
		return fmt.Errorf("invalid EndAlarmPriority: %w", err)
	}
	if start > 0 && end > 0 && start > end {
		return fmt.Errorf("StartAlarmPriority must not exceed EndAlarmPriority")
	}
	method, err := normalizeAlarmMethodFilter(req.AlarmMethod)
	if err != nil {
		return fmt.Errorf("invalid AlarmMethod: %w", err)
	}
	alarmType := strings.TrimSpace(req.AlarmType)
	if alarmType != "" && !version.AtLeast(GBVersion20) {
		return fmt.Errorf("AlarmType subscription requires GB/T 28181-2016 or later")
	}
	if alarmType != "" && !alarmTypeMatchesMethods(version, method, alarmType) {
		return fmt.Errorf("AlarmType is invalid for AlarmMethod")
	}
	startTime, hasStart, err := parseSubscriptionTime(startAlarmTime)
	if err != nil {
		return fmt.Errorf("invalid StartAlarmTime: %w", err)
	}
	endTime, hasEnd, err := parseSubscriptionTime(endAlarmTime)
	if err != nil {
		return fmt.Errorf("invalid EndAlarmTime: %w", err)
	}
	if hasStart && hasEnd && endTime.Before(startTime) {
		return fmt.Errorf("StartAlarmTime must not be after EndAlarmTime")
	}
	return nil
}

func normalizeAlarmMethodFilter(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	compact := strings.ReplaceAll(value, "/", "")
	if strings.Contains(compact, " ") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") {
		return "", fmt.Errorf("must be 0 or a combination of 1..7")
	}
	if strings.Contains(value, "/") {
		for _, part := range strings.Split(value, "/") {
			if len(part) != 1 {
				return "", fmt.Errorf("separated values must contain one method")
			}
		}
	}
	seen := make(map[byte]struct{}, len(compact))
	for index := 0; index < len(compact); index++ {
		method := compact[index]
		if method < '0' || method > '7' {
			return "", fmt.Errorf("must be 0 or a combination of 1..7")
		}
		if _, exists := seen[method]; exists {
			return "", fmt.Errorf("must not contain duplicate values")
		}
		seen[method] = struct{}{}
	}
	if len(compact) > 1 && strings.Contains(compact, "0") {
		return "", fmt.Errorf("0 cannot be combined with other values")
	}
	canonical := make([]byte, 0, len(seen))
	for method := byte('0'); method <= '7'; method++ {
		if _, exists := seen[method]; exists {
			canonical = append(canonical, method)
		}
	}
	return string(canonical), nil
}

func formatAlarmMethodFilter(version GBProtocolVersion, value string) (string, error) {
	compact, err := normalizeAlarmMethodFilter(value)
	if err != nil || compact == "" || !version.AtLeast(GBVersion30) {
		return compact, err
	}
	parts := make([]string, len(compact))
	for index := range compact {
		parts[index] = compact[index : index+1]
	}
	return strings.Join(parts, "/"), nil
}

func alarmTypeMatchesMethods(version GBProtocolVersion, methods, alarmType string) bool {
	if strings.TrimSpace(alarmType) == "" {
		return true
	}
	for _, method := range methods {
		if validateAlarmTypeForMethod(version, string(method), alarmType) == nil {
			return true
		}
	}
	return false
}

func subscribeEventMinimumVersion(cmdType string) (GBProtocolVersion, bool) {
	switch strings.TrimSpace(cmdType) {
	case "Alarm", "Catalog":
		return GBVersion10, true
	case "MobilePosition":
		return GBVersion20, true
	case "PTZPosition":
		return GBVersion30, true
	default:
		return "", false
	}
}

func parseAlarmPriorityFilter(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	priority, err := strconv.Atoi(value)
	if err != nil || priority < 0 || priority > 4 {
		return 0, fmt.Errorf("must be between 0 and 4")
	}
	return priority, nil
}

func parseSubscriptionTime(value string) (time.Time, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", time.RFC3339, time.DateTime} {
		if parsed, err := sip.ParseGBTime(layout, value); err == nil {
			return parsed, true, nil
		}
	}
	return time.Time{}, false, fmt.Errorf("unsupported time format")
}

func alarmMatchesSubscription(filter eventSubscriptionFilter, body []byte) bool {
	if filter == (eventSubscriptionFilter{}) {
		return true
	}
	var alarm messageAlarm
	if err := sip.XMLDecode(body, &alarm); err != nil {
		return false
	}
	start, _ := parseAlarmPriorityFilter(filter.StartAlarmPriority)
	end, _ := parseAlarmPriorityFilter(filter.EndAlarmPriority)
	priority, err := strconv.Atoi(strings.TrimSpace(alarm.AlarmPriority))
	if err != nil && (start > 0 || end > 0) {
		return false
	}
	if start > 0 && priority < start {
		return false
	}
	if end > 0 && priority > end {
		return false
	}
	method := strings.TrimSpace(alarm.AlarmMethod)
	infoAlarmType, infoMethod, _, err := alarmTypedInfo(alarm.Info)
	if err != nil {
		return false
	}
	if method == "" {
		method = infoMethod
	}
	if wanted := strings.TrimSpace(filter.AlarmMethod); wanted != "" && wanted != "0" && !alarmMethodIntersects(wanted, method) {
		return false
	}
	alarmType := strings.TrimSpace(alarm.AlarmType)
	if alarmType == "" {
		alarmType = infoAlarmType
	}
	if wanted := strings.TrimSpace(filter.AlarmType); wanted != "" && wanted != alarmType {
		return false
	}
	if filter.StartAlarmTime != "" || filter.EndAlarmTime != "" {
		alarmTime, ok := (&AlarmEvent{AlarmTime: alarm.AlarmTime}).ParseTime()
		if !ok {
			return false
		}
		startTime, hasStart, _ := parseSubscriptionTime(filter.StartAlarmTime)
		endTime, hasEnd, _ := parseSubscriptionTime(filter.EndAlarmTime)
		if hasStart && alarmTime.Before(startTime) {
			return false
		}
		if hasEnd && alarmTime.After(endTime) {
			return false
		}
	}
	return true
}

func alarmMethodIntersects(wanted, actual string) bool {
	wanted, _ = normalizeAlarmMethodFilter(wanted)
	for _, value := range actual {
		if value >= '1' && value <= '7' && strings.ContainsRune(wanted, value) {
			return true
		}
	}
	return false
}

func copyCascadeSubscribeInput(req subscribeEventRequest, deviceID, targetID, cmdType string, expires int) SubscribeInput {
	input := SubscribeInput{
		DeviceID: deviceID, TargetID: targetID, Event: cmdType, Expires: expires,
	}
	switch strings.TrimSpace(cmdType) {
	case "Alarm":
		input.StartAlarmPriority = strings.TrimSpace(req.StartAlarmPriority)
		input.EndAlarmPriority = strings.TrimSpace(req.EndAlarmPriority)
		input.AlarmMethod, _ = normalizeAlarmMethodFilter(req.AlarmMethod)
		input.AlarmType = strings.TrimSpace(req.AlarmType)
		input.StartAlarmTime = strings.TrimSpace(req.StartAlarmTime)
		input.EndAlarmTime = strings.TrimSpace(req.EndAlarmTime)
	case "Catalog":
		input.StartTime = strings.TrimSpace(req.StartTime)
		input.EndTime = strings.TrimSpace(req.EndTime)
	case "MobilePosition":
		input.Interval = subscribeRequestInterval(req)
	}
	return input
}

func subscribeRequestInterval(req subscribeEventRequest) int {
	if req.Interval == nil {
		return 0
	}
	return *req.Interval
}

// desiredCascadeDownstreamSubscriptions 把上级可见编码转换为本地设备/通道订阅。
// 平台级订阅按父设备聚合，避免共享通道较多时产生订阅风暴；指定共享通道的位置类事件按通道订阅。
func (g *GB28181API) desiredCascadeDownstreamSubscriptions(ctx context.Context, cascade *cascadeWorker, req subscribeEventRequest, cmdType, deviceID string, expires int) (map[string]SubscribeInput, error) {
	if cascade == nil {
		return nil, nil
	}
	if !strings.EqualFold(cmdType, "Catalog") && !strings.EqualFold(cmdType, "Alarm") && !strings.EqualFold(cmdType, "MobilePosition") && !strings.EqualFold(cmdType, "PTZPosition") {
		return nil, nil
	}
	// 单元测试或嵌入模式可能只使用事件源能力，没有设备存储；生产 Server 始终同时具备二者。
	if g == nil || g.core.Store() == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return nil, nil
	}
	channels, err := g.loadCascadeChannels(ctx, cascade.platform)
	if err != nil {
		return nil, err
	}
	localTarget := cascade.platform.exposedChannelMap[strings.TrimSpace(deviceID)]
	desired := make(map[string]SubscribeInput)
	for _, channel := range channels {
		if channel == nil || strings.TrimSpace(channel.DeviceID) == "" || strings.TrimSpace(channel.ChannelID) == "" {
			continue
		}
		if localTarget != "" && channel.ChannelID != localTarget {
			continue
		}
		targetID := channel.DeviceID
		if localTarget != "" && !strings.EqualFold(cmdType, "Alarm") {
			// MobilePosition/PTZPosition 的指定通道订阅保持精确目标；平台级订阅则由父设备聚合。
			targetID = channel.ChannelID
		}
		input := copyCascadeSubscribeInput(req, channel.DeviceID, targetID, cmdType, expires)
		key := buildOutgoingSubscriptionKey(input.DeviceID, input.TargetID, cmdType, &input)
		key += monitorUserIdentitySubscriptionKey(ctx)
		desired[key] = input
	}
	if localTarget != "" && len(desired) == 0 {
		return nil, fmt.Errorf("shared cascade channel is unavailable")
	}
	return desired, nil
}

func monitorUserIdentitySubscriptionKey(ctx context.Context) string {
	identity := monitorUserIdentityFromContext(ctx)
	if identity == nil {
		return ""
	}
	localGatewayID, _ := ctx.Value(monitorUserIdentityGatewayContextKey{}).(string)
	sum := sha256.Sum256([]byte(strings.TrimSpace(localGatewayID) + "\x00" + identity.String()))
	return fmt.Sprintf("|identity=%x", sum[:8])
}

type cascadeSubscribeContextKey struct{}

func isCascadeSubscribeContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value, _ := ctx.Value(cascadeSubscribeContextKey{}).(bool)
	return value
}

func (g *GB28181API) invokeCascadeSubscribe(ctx context.Context, input *SubscribeInput) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, cascadeSubscribeContextKey{}, true)
	if g.cascadeSubscribe != nil {
		if err := g.validateCascadeSubscribeTarget(input); err != nil {
			return err
		}
		return g.cascadeSubscribe(ctx, input)
	}
	return g.Subscribe(ctx, input)
}

// validateCascadeSubscribeTarget 防止自定义下游订阅钩子绕过直连 Subscribe
// 已执行的设备在线、通道归属和设备级能力门禁。取消操作保留原有尽力清理语义。
func (g *GB28181API) validateCascadeSubscribeTarget(input *SubscribeInput) error {
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return nil
	}
	if input == nil || strings.TrimSpace(input.DeviceID) == "" {
		return ErrDeviceNotExist
	}
	if input.Cancel {
		return nil
	}
	deviceID := strings.TrimSpace(input.DeviceID)
	device, ok := g.svr.memoryStorer.Load(deviceID)
	if !ok || device == nil || !device.IsOnlineNow() {
		return ErrDeviceOffline
	}
	targetID := strings.TrimSpace(input.TargetID)
	if targetID != "" && targetID != deviceID {
		if _, exists := g.svr.memoryStorer.GetChannel(deviceID, targetID); !exists {
			return ErrChannelNotExist
		}
	}
	cmdType, ok := normalizeSubscribeCmdType(input.Event)
	if strings.TrimSpace(input.Event) == "" {
		cmdType, ok = "Alarm", true
	}
	if !ok {
		return fmt.Errorf("unsupported subscribe event: %s", input.Event)
	}
	switch cmdType {
	case "Catalog":
		return g.requireGBFeature(deviceID, "directory_notify", "目录订阅", func(c GBCapabilities) bool {
			return c.DirectoryNotify
		})
	case "MobilePosition":
		return g.requireGBFeature(deviceID, "mobile_position", "移动位置订阅", func(c GBCapabilities) bool {
			return c.MobilePosition
		})
	case "PTZPosition":
		return g.requireGBFeature(deviceID, "ptz_position", "PTZ精准位置变化订阅", func(c GBCapabilities) bool {
			return c.PTZPosition
		})
	default:
		return nil
	}
}

func (g *GB28181API) lockCascadeSubscriptionOperation(ctx context.Context, key string) (func(), error) {
	if g == nil {
		return nil, fmt.Errorf("GB28181 service is unavailable")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("cascade subscription key is unavailable")
	}
	g.cascadeSubscriptionOpMu.Lock()
	if g.cascadeSubscriptionOps == nil {
		g.cascadeSubscriptionOps = make(map[string]*keyedOperationLock)
	}
	entry := g.cascadeSubscriptionOps[key]
	if entry == nil {
		entry = &keyedOperationLock{}
		g.cascadeSubscriptionOps[key] = entry
	}
	entry.refs++
	g.cascadeSubscriptionOpMu.Unlock()
	if err := entry.mutex.LockContext(ctx); err != nil {
		g.releaseCascadeSubscriptionOperationRef(key, entry)
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mutex.Unlock()
			g.releaseCascadeSubscriptionOperationRef(key, entry)
		})
	}, nil
}

func (g *GB28181API) releaseCascadeSubscriptionOperationRef(key string, entry *keyedOperationLock) {
	g.cascadeSubscriptionOpMu.Lock()
	if current := g.cascadeSubscriptionOps[key]; current == entry {
		entry.refs--
		if entry.refs == 0 {
			delete(g.cascadeSubscriptionOps, key)
		}
	}
	g.cascadeSubscriptionOpMu.Unlock()
}

func (g *GB28181API) lockEventSubscriptionOperation(ctx context.Context, key string) (func(), error) {
	if g == nil {
		return nil, fmt.Errorf("GB28181 service is unavailable")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("event subscription key is unavailable")
	}
	g.eventSubscriptionMu.Lock()
	if g.eventSubscriptionOps == nil {
		g.eventSubscriptionOps = make(map[string]*keyedOperationLock)
	}
	entry := g.eventSubscriptionOps[key]
	if entry == nil {
		entry = &keyedOperationLock{}
		g.eventSubscriptionOps[key] = entry
	}
	entry.refs++
	g.eventSubscriptionMu.Unlock()
	if err := entry.mutex.LockContext(ctx); err != nil {
		g.releaseEventSubscriptionOperationRef(key, entry)
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mutex.Unlock()
			g.releaseEventSubscriptionOperationRef(key, entry)
		})
	}, nil
}

func (g *GB28181API) releaseEventSubscriptionOperationRef(key string, entry *keyedOperationLock) {
	g.eventSubscriptionMu.Lock()
	if current := g.eventSubscriptionOps[key]; current == entry {
		entry.refs--
		if entry.refs == 0 {
			delete(g.eventSubscriptionOps, key)
		}
	}
	g.eventSubscriptionMu.Unlock()
}

func (g *GB28181API) syncCascadeDownstreamSubscriptions(ctx context.Context, previous []string, desired map[string]SubscribeInput) ([]string, error) {
	return g.syncCascadeDownstreamSubscriptionsMode(ctx, previous, desired, true)
}

func (g *GB28181API) syncCascadeDownstreamSubscriptionsMode(ctx context.Context, previous []string, desired map[string]SubscribeInput, refreshOwned bool) ([]string, error) {
	if len(previous) == 0 && len(desired) == 0 {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	previousSet := make(map[string]struct{}, len(previous))
	for _, key := range previous {
		previousSet[key] = struct{}{}
	}
	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	acquired := make([]string, 0, len(keys))
	for _, key := range keys {
		unlock, err := g.lockCascadeSubscriptionOperation(ctx, key)
		if err != nil {
			g.rollbackCascadeDownstreamSubscriptions(ctx, acquired)
			return nil, err
		}
		input := desired[key]
		g.cascadeSubscriptionMu.Lock()
		if g.cascadeSubscriptions == nil {
			g.cascadeSubscriptions = make(map[string]*cascadeDownstreamSubscription)
		}
		state := g.cascadeSubscriptions[key]
		_, alreadyOwned := previousSet[key]
		if state != nil {
			nextInput := state.Input
			retryGeneration := state.RetryGeneration
			if input.Expires > nextInput.Expires {
				nextInput.Expires = input.Expires
			}
			if alreadyOwned {
				retryDeferred := state.RetryBlocked || !state.RetryAt.IsZero() && time.Now().Before(state.RetryAt)
				if !refreshOwned || retryDeferred {
					state.Input = nextInput
					g.cascadeSubscriptionMu.Unlock()
					unlock()
					continue
				}
				refresh := nextInput
				g.cascadeSubscriptionMu.Unlock()
				if err := g.invokeCascadeSubscribe(ctx, &refresh); err != nil {
					unlock()
					g.rollbackCascadeDownstreamSubscriptions(ctx, acquired)
					return nil, fmt.Errorf("renew downstream %s subscription: %w", input.Event, err)
				}
				g.cascadeSubscriptionMu.Lock()
				if g.cascadeSubscriptions[key] == state {
					state.Input = nextInput
					if state.RetryGeneration == retryGeneration {
						state.RetryAt = time.Time{}
						state.RetryBlocked = false
					}
				}
				g.cascadeSubscriptionMu.Unlock()
				unlock()
				continue
			}
			state.Input = nextInput
			state.Refs++
			g.cascadeSubscriptionMu.Unlock()
			unlock()
			acquired = append(acquired, key)
			continue
		}
		// 先登记占位，使关闭路径能发现并等待在途创建。
		localGatewayID, _ := ctx.Value(monitorUserIdentityGatewayContextKey{}).(string)
		state = &cascadeDownstreamSubscription{
			Input: input, Identity: monitorUserIdentityFromContext(ctx), LocalGatewayID: strings.TrimSpace(localGatewayID),
		}
		g.cascadeSubscriptions[key] = state
		g.cascadeSubscriptionMu.Unlock()
		if err := g.invokeCascadeSubscribe(ctx, &input); err != nil {
			g.cascadeSubscriptionMu.Lock()
			if g.cascadeSubscriptions[key] == state {
				delete(g.cascadeSubscriptions, key)
			}
			g.cascadeSubscriptionMu.Unlock()
			unlock()
			g.rollbackCascadeDownstreamSubscriptions(ctx, acquired)
			return nil, fmt.Errorf("create downstream %s subscription: %w", input.Event, err)
		}
		g.cascadeSubscriptionMu.Lock()
		state.Refs = 1
		g.cascadeSubscriptionMu.Unlock()
		unlock()
		acquired = append(acquired, key)
	}
	for _, key := range previous {
		if _, keep := desired[key]; !keep {
			g.releaseCascadeDownstreamSubscription(ctx, key)
		}
	}
	return keys, nil
}

func (g *GB28181API) rollbackCascadeDownstreamSubscriptions(ctx context.Context, keys []string) {
	rollbackCtx := context.Background()
	if ctx != nil {
		// 回滚必须继续发送已经建立成功的下级退订，同时保留跨域身份等业务值。
		rollbackCtx = context.WithoutCancel(ctx)
	}
	for index := len(keys) - 1; index >= 0; index-- {
		g.releaseCascadeDownstreamSubscription(rollbackCtx, keys[index])
	}
}

func (g *GB28181API) snapshotCascadeDownstreamSubscriptions(keys []string) map[string]SubscribeInput {
	if g == nil || len(keys) == 0 {
		return nil
	}
	snapshot := make(map[string]SubscribeInput, len(keys))
	g.cascadeSubscriptionMu.Lock()
	for _, key := range keys {
		if state := g.cascadeSubscriptions[key]; state != nil {
			snapshot[key] = state.Input
		}
	}
	g.cascadeSubscriptionMu.Unlock()
	return snapshot
}

// rollbackCascadeDownstreamSubscriptionSync 恢复上级 SUBSCRIBE 响应写出前暂存的下级引用关系。
func (g *GB28181API) rollbackCascadeDownstreamSubscriptionSync(ctx context.Context, current []string, previous map[string]SubscribeInput) {
	if len(current) == 0 && len(previous) == 0 {
		return
	}
	rollbackCtx := context.Background()
	if ctx != nil {
		rollbackCtx = context.WithoutCancel(ctx)
	}
	if _, err := g.syncCascadeDownstreamSubscriptionsMode(rollbackCtx, current, previous, false); err != nil {
		slog.Warn("rollback cascade downstream subscriptions after SIP response failure", "err", err)
	}
}

func (g *GB28181API) reconcileCascadeDownstreamSubscriptions(ctx context.Context) {
	if g == nil {
		return
	}
	now := time.Now()
	g.eventSubscribers.Range(func(rawKey, value any) bool {
		key, ok := rawKey.(string)
		if !ok || strings.TrimSpace(key) == "" {
			return true
		}
		unlock, err := g.lockEventSubscriptionOperation(ctx, key)
		if err != nil {
			return true
		}
		defer unlock()
		value, exists := g.eventSubscribers.Load(key)
		if !exists {
			return true
		}
		sub, ok := value.(*eventSubscription)
		if !ok || sub == nil {
			return true
		}
		sub.mu.Lock()
		worker := sub.Cascade
		identity := sub.Identity.clone()
		localGatewayID := sub.LocalGatewayID
		cmdType := sub.CmdType
		deviceID := sub.DeviceID
		expiresAt := sub.ExpiresAt
		filter := sub.Filter
		interval := sub.Interval
		previous := append([]string(nil), sub.DownstreamKeys...)
		sub.mu.Unlock()
		if worker == nil || !now.Before(expiresAt) {
			return true
		}
		expires := max(1, int(time.Until(expiresAt).Seconds()))
		request := subscribeEventRequest{
			CmdType: cmdType, SN: 1, DeviceID: deviceID,
			StartAlarmPriority: filter.StartAlarmPriority,
			EndAlarmPriority:   filter.EndAlarmPriority,
			AlarmMethod:        filter.AlarmMethod,
			AlarmType:          filter.AlarmType,
			StartAlarmTime:     filter.StartAlarmTime,
			EndAlarmTime:       filter.EndAlarmTime,
			StartTime:          filter.CatalogStartTime,
			EndTime:            filter.CatalogEndTime,
		}
		if interval > 0 {
			request.Interval = &interval
		}
		identityCtx := withMonitorUserIdentityRoute(ctx, identity, localGatewayID)
		desired, err := g.desiredCascadeDownstreamSubscriptions(identityCtx, worker, request, cmdType, deviceID, expires)
		if err != nil {
			slog.Warn("reconcile cascade downstream subscriptions failed", "upstream", worker.platform.name, "event", cmdType, "err", err)
			return true
		}
		keys, err := g.syncCascadeDownstreamSubscriptionsMode(identityCtx, previous, desired, false)
		if err != nil {
			slog.Warn("reconcile cascade downstream subscriptions failed", "upstream", worker.platform.name, "event", cmdType, "err", err)
			return true
		}
		sub.mu.Lock()
		sub.DownstreamKeys = append(sub.DownstreamKeys[:0], keys...)
		sub.mu.Unlock()
		return true
	})
}

func (g *GB28181API) releaseCascadeDownstreamSubscriptions(ctx context.Context, keys []string) {
	for _, key := range keys {
		g.releaseCascadeDownstreamSubscription(ctx, key)
	}
}

func (g *GB28181API) releaseCascadeDownstreamSubscription(ctx context.Context, key string) {
	if ctx == nil {
		ctx = context.Background()
	}
	unlock, err := g.lockCascadeSubscriptionOperation(ctx, key)
	if err != nil {
		return
	}
	defer unlock()
	g.cascadeSubscriptionMu.Lock()
	state := g.cascadeSubscriptions[key]
	if state == nil {
		g.cascadeSubscriptionMu.Unlock()
		return
	}
	state.Refs--
	if state.Refs > 0 {
		g.cascadeSubscriptionMu.Unlock()
		return
	}
	delete(g.cascadeSubscriptions, key)
	cancel := state.Input
	identity := state.Identity.clone()
	localGatewayID := state.LocalGatewayID
	g.cascadeSubscriptionMu.Unlock()
	cancel.Cancel = true
	cancel.Expires = 0
	ctx = withMonitorUserIdentityRoute(ctx, identity, localGatewayID)
	if err = g.invokeCascadeSubscribe(ctx, &cancel); err != nil {
		slog.Warn("cancel downstream cascade subscription failed", "device_id", cancel.DeviceID, "target_id", cancel.TargetID, "event", cancel.Event, "err", err)
	}
}

func (g *GB28181API) closeCascadeDownstreamSubscriptions() {
	if g == nil {
		return
	}
	cleanupCtx := g.mediaPersistenceContext()
	for {
		g.cascadeSubscriptionMu.Lock()
		keys := make([]string, 0, len(g.cascadeSubscriptions))
		for key := range g.cascadeSubscriptions {
			keys = append(keys, key)
		}
		g.cascadeSubscriptionMu.Unlock()
		if len(keys) == 0 {
			return
		}
		for _, key := range keys {
			unlock, err := g.lockCascadeSubscriptionOperation(cleanupCtx, key)
			if err != nil {
				g.cascadeSubscriptionMu.Lock()
				delete(g.cascadeSubscriptions, key)
				g.cascadeSubscriptionMu.Unlock()
				g.loadAndDeleteOutgoingSubscription(key)
				slog.WarnContext(cleanupCtx, "wait downstream cascade subscription close lock failed", "key", key, "err", err)
				continue
			}
			g.cascadeSubscriptionMu.Lock()
			state := g.cascadeSubscriptions[key]
			delete(g.cascadeSubscriptions, key)
			g.cascadeSubscriptionMu.Unlock()
			if state != nil && state.Refs > 0 {
				input := state.Input
				input.Cancel = true
				input.Expires = 0
				identityCtx := withMonitorUserIdentityRoute(cleanupCtx, state.Identity, state.LocalGatewayID)
				if g.cascadeSubscribe != nil {
					if err := g.cascadeSubscribe(identityCtx, &input); err != nil {
						slog.WarnContext(cleanupCtx, "cancel downstream cascade subscription during close failed", "device_id", input.DeviceID, "target_id", input.TargetID, "event", input.Event, "err", err)
					}
				} else if value, loaded := g.loadAndDeleteOutgoingSubscription(key); loaded {
					dialog, _ := value.(*outgoingSubscriptionDialog)
					if dialog != nil {
						dialog.mu.Lock()
						if dialog.identity == nil {
							dialog.identity = state.Identity.clone()
							dialog.localGatewayID = state.LocalGatewayID
						}
						dialog.mu.Unlock()
						tx, _, sendErr := g.sendOutgoingSubscriptionCancellationContext(cleanupCtx, dialog, true)
						if sendErr == nil && tx != nil {
							responseCtx, cancel := context.WithTimeout(cleanupCtx, 3*time.Second)
							_, sendErr = sipResponseContext(responseCtx, tx)
							cancel()
						}
						if sendErr != nil && cleanupCtx.Err() == nil {
							slog.WarnContext(cleanupCtx, "cancel downstream cascade subscription during close failed", "device_id", input.DeviceID, "target_id", input.TargetID, "event", input.Event, "err", sendErr)
						}
					}
				}
			}
			unlock()
		}
	}
}

func (g *GB28181API) removeCascadeEventSubscriptions(worker *cascadeWorker) {
	g.removeCascadeEventSubscriptionsContext(context.Background(), worker)
}

func (g *GB28181API) removeCascadeEventSubscriptionsContext(ctx context.Context, worker *cascadeWorker) {
	if g == nil || worker == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	g.eventSubscribers.Range(func(rawKey, value any) bool {
		if ctx.Err() != nil {
			return false
		}
		key, ok := rawKey.(string)
		if !ok || strings.TrimSpace(key) == "" {
			return true
		}
		unlock, err := g.lockEventSubscriptionOperation(ctx, key)
		if err != nil {
			return ctx.Err() == nil
		}
		defer unlock()
		value, exists := g.eventSubscribers.Load(key)
		if !exists {
			return true
		}
		subscription, ok := value.(*eventSubscription)
		if !ok || subscription == nil {
			return true
		}
		subscription.mu.Lock()
		matches := subscription.Cascade == worker
		keys := append([]string(nil), subscription.DownstreamKeys...)
		subscription.mu.Unlock()
		if matches && g.eventSubscribers.CompareAndDelete(key, subscription) {
			g.releaseCascadeDownstreamSubscriptions(ctx, keys)
		}
		return ctx.Err() == nil
	})
}
