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
	method, _ := normalizeAlarmMethodFilter(req.AlarmMethod)
	return eventSubscriptionFilter{
		StartAlarmPriority: strings.TrimSpace(req.StartAlarmPriority),
		EndAlarmPriority:   strings.TrimSpace(req.EndAlarmPriority),
		AlarmMethod:        method,
		AlarmType:          strings.TrimSpace(req.AlarmType),
		StartAlarmTime:     strings.TrimSpace(req.StartAlarmTime),
		EndAlarmTime:       strings.TrimSpace(req.EndAlarmTime),
	}
}

func validateSubscribeEventEnvelope(req subscribeEventRequest, cmdType string) error {
	if req.SN <= 0 {
		return fmt.Errorf("invalid subscription SN")
	}
	if !isGBDeviceIdentifier(strings.TrimSpace(req.DeviceID)) {
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
	if !strings.EqualFold(cmdType, "Alarm") {
		return nil
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
	startTime, hasStart, err := parseSubscriptionTime(req.StartAlarmTime)
	if err != nil {
		return fmt.Errorf("invalid StartAlarmTime: %w", err)
	}
	endTime, hasEnd, err := parseSubscriptionTime(req.EndAlarmTime)
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
	if method == "" {
		method = strings.TrimSpace(alarm.Info.AlarmMethod)
	}
	if wanted := strings.TrimSpace(filter.AlarmMethod); wanted != "" && wanted != "0" && !alarmMethodIntersects(wanted, method) {
		return false
	}
	alarmType := strings.TrimSpace(alarm.AlarmType)
	if alarmType == "" {
		alarmType = strings.TrimSpace(alarm.Info.AlarmType)
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
	method, _ := normalizeAlarmMethodFilter(req.AlarmMethod)
	return SubscribeInput{
		DeviceID: deviceID, TargetID: targetID, Event: cmdType, Expires: expires,
		StartAlarmPriority: strings.TrimSpace(req.StartAlarmPriority),
		EndAlarmPriority:   strings.TrimSpace(req.EndAlarmPriority),
		AlarmMethod:        method,
		AlarmType:          strings.TrimSpace(req.AlarmType),
		StartAlarmTime:     strings.TrimSpace(req.StartAlarmTime),
		EndAlarmTime:       strings.TrimSpace(req.EndAlarmTime),
		Interval:           subscribeRequestInterval(req),
	}
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

func (g *GB28181API) invokeCascadeSubscribe(ctx context.Context, input *SubscribeInput) error {
	if g.cascadeSubscribe != nil {
		return g.cascadeSubscribe(ctx, input)
	}
	return g.Subscribe(ctx, input)
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
			if input.Expires > state.Input.Expires {
				state.Input.Expires = input.Expires
			}
			if alreadyOwned {
				if !refreshOwned {
					g.cascadeSubscriptionMu.Unlock()
					unlock()
					continue
				}
				refresh := state.Input
				g.cascadeSubscriptionMu.Unlock()
				if err := g.invokeCascadeSubscribe(ctx, &refresh); err != nil {
					unlock()
					g.rollbackCascadeDownstreamSubscriptions(ctx, acquired)
					return nil, fmt.Errorf("renew downstream %s subscription: %w", input.Event, err)
				}
				unlock()
				continue
			}
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
	for index := len(keys) - 1; index >= 0; index-- {
		g.releaseCascadeDownstreamSubscription(ctx, keys[index])
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
	unlock, err := g.lockCascadeSubscriptionOperation(context.Background(), key)
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
			unlock, err := g.lockCascadeSubscriptionOperation(context.Background(), key)
			if err != nil {
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
				ctx := withMonitorUserIdentityRoute(context.Background(), state.Identity, state.LocalGatewayID)
				if err := g.invokeCascadeSubscribe(ctx, &input); err != nil {
					slog.Warn("cancel downstream cascade subscription during close failed", "device_id", input.DeviceID, "target_id", input.TargetID, "event", input.Event, "err", err)
				}
			}
			unlock()
		}
	}
}

func (g *GB28181API) removeCascadeEventSubscriptions(worker *cascadeWorker) {
	if g == nil || worker == nil {
		return
	}
	g.eventSubscribers.Range(func(rawKey, value any) bool {
		key, ok := rawKey.(string)
		if !ok || strings.TrimSpace(key) == "" {
			return true
		}
		unlock, err := g.lockEventSubscriptionOperation(context.Background(), key)
		if err != nil {
			return true
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
			g.releaseCascadeDownstreamSubscriptions(context.Background(), keys)
		}
		return true
	})
}
