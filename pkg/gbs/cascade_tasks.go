package gbs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	cascadeTaskUpgrade   = "upgrade"
	cascadeTaskSnapshot  = "snapshot"
	cascadeTaskRouteTTL  = 7 * 24 * time.Hour
	maxCascadeTaskRoutes = 1024

	gbTaskKindVideoUploadOutbox  = "video_upload_outbox"
	gbTaskKindVideoUploadReceipt = "video_upload_receipt"
	videoUploadOutboxRetention   = 30 * 24 * time.Hour
	videoUploadReceiptRetention  = 30 * 24 * time.Hour
	maxVideoUploadOutboxStates   = 10000
	maxVideoUploadReceipts       = 10000
)

type videoUploadOutboxState struct {
	SourceDeviceID      string    `json:"source_device_id"`
	Body                []byte    `json:"body"`
	Platforms           []string  `json:"platforms"`
	ReceivedAt          time.Time `json:"received_at"`
	BindingRegisteredAt time.Time `json:"binding_registered_at,omitempty"`
	BindingExpires      int       `json:"binding_expires,omitempty"`
	HasBinding          bool      `json:"has_binding,omitempty"`
	Attempt             int       `json:"attempt,omitempty"`
	NextAttemptAt       time.Time `json:"next_attempt_at,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
}

type cascadeTaskRoute struct {
	notifyMu            sync.Mutex
	completed           bool
	completionPending   bool
	workerDetached      atomic.Bool
	retired             atomic.Bool
	startOnce           sync.Once
	startDone           chan struct{}
	startResult         string
	startErr            error
	kind                string
	worker              *cascadeWorker
	upstreamPlatform    string
	downstreamDeviceID  string
	downstreamTargetID  string
	exposedID           string
	upstreamSessionID   string
	downstreamSessionID string
	requestFingerprint  string
	identity            *monitorUserIdentity
	localGatewayID      string
	createdAt           time.Time
	updatedAtUnixNano   atomic.Int64
}

func (route *cascadeTaskRoute) setUpdatedAt(value time.Time) {
	if route != nil && !value.IsZero() {
		route.updatedAtUnixNano.Store(value.UnixNano())
	}
}

func (route *cascadeTaskRoute) lastUpdatedAt() time.Time {
	if route == nil {
		return time.Time{}
	}
	if value := route.updatedAtUnixNano.Load(); value > 0 {
		return time.Unix(0, value)
	}
	return route.createdAt
}

func cascadeTaskRouteKey(kind, deviceID, sessionID string) string {
	return "downstream:" + strings.TrimSpace(kind) + ":" + strings.TrimSpace(deviceID) + ":" + strings.TrimSpace(sessionID)
}

func cascadeTaskUpstreamRouteKey(kind string, worker *cascadeWorker, exposedID, sessionID string) string {
	name := ""
	if worker != nil {
		name = worker.platform.name
	}
	return cascadeTaskUpstreamRouteKeyByName(kind, name, exposedID, sessionID)
}

func cascadeTaskUpstreamRouteKeyByName(kind, platformName, exposedID, sessionID string) string {
	return "upstream:" + strings.TrimSpace(kind) + ":" + strings.TrimSpace(platformName) + ":" + strings.TrimSpace(exposedID) + ":" + strings.TrimSpace(sessionID)
}

func cascadeTaskFingerprint(value []byte) string {
	digest := sha256.Sum256(value)
	return fmt.Sprintf("%x", digest[:])
}

func (g *GB28181API) registerCascadeTaskRoute(ctx context.Context, kind string, worker *cascadeWorker, channel *ipc.Channel, exposedID, sessionID, fingerprint string, initialState ...any) (*cascadeTaskRoute, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if g == nil || worker == nil || channel == nil || !worker.protocolVersion().AtLeast(GBVersion30) {
		return nil, false, fmt.Errorf("cascade task requires protocol 3.0")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if !g.cascadeWorkerAvailable(worker) {
		return nil, false, context.Canceled
	}
	exposedID = strings.TrimSpace(exposedID)
	sessionID = strings.TrimSpace(sessionID)
	fingerprint = strings.TrimSpace(fingerprint)
	if !isGBDeviceIdentifier(exposedID) || validateGBSessionID(sessionID) != nil ||
		fingerprint == "" || worker.platform.channelIDMap[strings.TrimSpace(channel.ChannelID)] != exposedID {
		return nil, false, fmt.Errorf("invalid cascade task route")
	}
	g.cascadeTaskRouteMu.Lock()
	defer g.cascadeTaskRouteMu.Unlock()
	g.cleanupCascadeTaskRoutesLocked(time.Now())
	upstreamKey := cascadeTaskUpstreamRouteKey(kind, worker, exposedID, sessionID)
	if actual, loaded := g.cascadeTaskRoutes.Load(upstreamKey); loaded {
		existing, ok := actual.(*cascadeTaskRoute)
		if !ok || !existing.matchesUpstream(worker, channel, fingerprint) {
			return nil, false, fmt.Errorf("cascade task session is already owned by another route")
		}
		return existing, false, nil
	}
	if restored, found, err := g.restoreCascadeTaskRouteByUpstreamLocked(ctx, kind, worker, exposedID, sessionID); err != nil {
		return nil, false, err
	} else if found {
		if !restored.matchesUpstream(worker, channel, fingerprint) {
			return nil, false, fmt.Errorf("cascade task session is already owned by another route")
		}
		return restored, false, nil
	}
	if g.cascadeTaskRouteCountLocked() >= maxCascadeTaskRoutes {
		return nil, false, fmt.Errorf("cascade task route capacity exceeded")
	}
	downstreamSessionID := sip.RandString(32)
	localGatewayID, _ := ctx.Value(monitorUserIdentityGatewayContextKey{}).(string)
	createdAt := time.Now()
	route := &cascadeTaskRoute{
		kind: kind, worker: worker, upstreamPlatform: worker.platform.name, downstreamDeviceID: strings.TrimSpace(channel.DeviceID),
		downstreamTargetID: strings.TrimSpace(channel.ChannelID), exposedID: exposedID,
		upstreamSessionID: sessionID, downstreamSessionID: downstreamSessionID, requestFingerprint: fingerprint,
		startDone: make(chan struct{}), createdAt: createdAt,
		identity: monitorUserIdentityFromContext(ctx), localGatewayID: strings.TrimSpace(localGatewayID),
	}
	route.setUpdatedAt(createdAt)
	downstreamKey := cascadeTaskRouteKey(kind, route.downstreamDeviceID, downstreamSessionID)
	if err := g.storeCascadeTaskState(ctx, route, initialState...); err != nil {
		cleanupErr := g.deleteCascadeTaskStateContext(g.taskPersistenceContext(), route)
		return nil, false, errors.Join(err, cleanupErr)
	}
	if err := g.persistCascadeTaskRoute(ctx, route); err != nil {
		cleanupCtx := g.taskPersistenceContext()
		stateErr := g.deleteCascadeTaskStateContext(cleanupCtx, route)
		routeErr := g.deletePersistedCascadeTaskRoute(cleanupCtx, route)
		return nil, false, errors.Join(err, stateErr, routeErr)
	}
	if err := ctx.Err(); err != nil || !g.cascadeWorkerAvailable(worker) {
		if err == nil {
			err = context.Canceled
		}
		cleanupCtx := g.taskPersistenceContext()
		stateErr := g.deleteCascadeTaskStateContext(cleanupCtx, route)
		routeErr := g.deletePersistedCascadeTaskRoute(cleanupCtx, route)
		return nil, false, errors.Join(err, stateErr, routeErr)
	}
	// 路由及下游任务状态全部可靠落库后再发布内存索引，避免最终通知观察到半初始化路由。
	g.cascadeTaskRoutes.Store(downstreamKey, route)
	g.cascadeTaskRoutes.Store(upstreamKey, route)
	return route, true, nil
}

func (route *cascadeTaskRoute) matchesUpstream(worker *cascadeWorker, channel *ipc.Channel, fingerprint string) bool {
	if route == nil || worker == nil || channel == nil {
		return false
	}
	if route.upstreamPlatform != worker.platform.name || route.downstreamTargetID != strings.TrimSpace(channel.ChannelID) ||
		route.requestFingerprint != strings.TrimSpace(fingerprint) {
		return false
	}
	// worker 为空仅用于已完成且从持久化恢复的幂等路由，不再依赖已删除的上级配置。
	return route.worker == nil || route.worker == worker || route.workerDetached.Load()
}

func (g *GB28181API) deleteCascadeTaskStateContext(ctx context.Context, route *cascadeTaskRoute) error {
	if g == nil || route == nil {
		return nil
	}
	switch route.kind {
	case cascadeTaskUpgrade:
		return g.deleteUpgradeStateContext(ctx, route.downstreamDeviceID, route.downstreamSessionID)
	case cascadeTaskSnapshot:
		return g.deleteSnapshotStateContext(ctx, route.downstreamDeviceID, route.downstreamSessionID)
	}
	return nil
}

func (g *GB28181API) finishCascadeTaskState(ctx context.Context, route *cascadeTaskRoute, result string, taskErr error) error {
	if g == nil || route == nil {
		return nil
	}
	accepted := taskErr == nil && strings.EqualFold(strings.TrimSpace(result), "OK")
	switch route.kind {
	case cascadeTaskUpgrade:
		state, ok, err := g.loadUpgradeState(ctx, route.downstreamDeviceID, route.downstreamSessionID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("cascade upgrade task state not found")
		}
		state.Result = strings.ToUpper(strings.TrimSpace(result))
		if accepted {
			state.Status = "accepted"
		} else {
			state.Status = "rejected"
		}
		state.UpdatedAt = time.Now()
		return g.storeUpgradeStateContext(ctx, state)
	case cascadeTaskSnapshot:
		status := "rejected"
		if accepted {
			status = "accepted"
		}
		_, ok, err := g.transitionSnapshotStateContext(ctx, route.downstreamDeviceID, route.downstreamSessionID, status)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("cascade snapshot task state not found")
		}
		return nil
	default:
		return nil
	}
}

func (g *GB28181API) storeCascadeTaskState(ctx context.Context, route *cascadeTaskRoute, initialState ...any) error {
	if g == nil || route == nil {
		return fmt.Errorf("cascade task state is unavailable")
	}
	switch route.kind {
	case cascadeTaskUpgrade:
		state := UpgradeState{}
		if len(initialState) > 0 {
			state, _ = initialState[0].(UpgradeState)
		}
		state.DeviceID = route.downstreamDeviceID
		state.ChannelID = route.downstreamTargetID
		state.SessionID = route.downstreamSessionID
		state.Status = "pending"
		state.UpdatedAt = route.createdAt
		return g.storeUpgradeStateContext(ctx, state)
	case cascadeTaskSnapshot:
		state := SnapshotState{}
		if len(initialState) > 0 {
			state, _ = initialState[0].(SnapshotState)
		}
		state.DeviceID = route.downstreamDeviceID
		state.ChannelID = route.downstreamTargetID
		state.SessionID = route.downstreamSessionID
		state.Status = "pending"
		state.UpdatedAt = route.createdAt
		return g.storeSnapshotStateContext(ctx, state)
	default:
		return fmt.Errorf("unsupported cascade task kind %q", route.kind)
	}
}

func (g *GB28181API) cascadeTaskRouteCount() int {
	if g == nil {
		return 0
	}
	g.cascadeTaskRouteMu.Lock()
	defer g.cascadeTaskRouteMu.Unlock()
	return g.cascadeTaskRouteCountLocked()
}

func (g *GB28181API) cascadeTaskRouteCountLocked() int {
	unique := make(map[*cascadeTaskRoute]struct{})
	g.cascadeTaskRoutes.Range(func(_, value any) bool {
		if route, ok := value.(*cascadeTaskRoute); ok && route != nil {
			unique[route] = struct{}{}
		}
		return len(unique) < maxCascadeTaskRoutes
	})
	return len(unique)
}

func (route *cascadeTaskRoute) finishStart(result string, err error) (string, error) {
	if route == nil {
		return result, err
	}
	route.notifyMu.Lock()
	defer route.notifyMu.Unlock()
	if route.completed {
		result, err = "OK", nil
	}
	route.startOnce.Do(func() {
		route.startResult = result
		route.startErr = err
		close(route.startDone)
	})
	return route.startResult, route.startErr
}

func (route *cascadeTaskRoute) waitStart(ctx context.Context) (string, error) {
	if route == nil || route.startDone == nil {
		return "ERROR", fmt.Errorf("cascade task route is unavailable")
	}
	if route.isCompleted() {
		return "OK", nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-route.startDone:
		if route.isCompleted() {
			return "OK", nil
		}
		return route.startResult, route.startErr
	case <-ctx.Done():
		return "ERROR", ctx.Err()
	}
}

func (route *cascadeTaskRoute) isCompleted() bool {
	if route == nil {
		return false
	}
	route.notifyMu.Lock()
	completed := route.completed
	route.notifyMu.Unlock()
	return completed
}

func (g *GB28181API) deleteCascadeTaskRoute(route *cascadeTaskRoute) {
	if g == nil || route == nil {
		return
	}
	g.cascadeTaskRouteMu.Lock()
	defer g.cascadeTaskRouteMu.Unlock()
	g.deleteCascadeTaskRouteLocked(route)
}

func (g *GB28181API) deleteCascadeTaskRouteLocked(route *cascadeTaskRoute) {
	route.retired.Store(true)
	g.cascadeTaskRoutes.CompareAndDelete(cascadeTaskRouteKey(route.kind, route.downstreamDeviceID, route.downstreamSessionID), route)
	g.cascadeTaskRoutes.CompareAndDelete(cascadeTaskUpstreamRouteKeyByName(route.kind, route.upstreamPlatform, route.exposedID, route.upstreamSessionID), route)
}

func (g *GB28181API) forwardCascadeTaskNotification(ctx context.Context, kind, deviceID, targetID, sessionID string, body []byte) (bool, error) {
	if g == nil {
		return false, nil
	}
	key := cascadeTaskRouteKey(kind, deviceID, sessionID)
	value, ok := g.cascadeTaskRoutes.Load(key)
	if !ok {
		route, found, err := g.restoreCascadeTaskRouteByDownstream(ctx, kind, deviceID, sessionID)
		if err != nil || !found {
			return found, err
		}
		value = route
	}
	route, ok := value.(*cascadeTaskRoute)
	if !ok || route == nil {
		g.cascadeTaskRouteMu.Lock()
		g.cascadeTaskRoutes.CompareAndDelete(key, value)
		g.cascadeTaskRouteMu.Unlock()
		return false, nil
	}
	route.notifyMu.Lock()
	defer route.notifyMu.Unlock()
	if route.completed {
		return true, g.persistCompletedCascadeTaskRouteLocked(ctx, route)
	}
	if route.downstreamTargetID != strings.TrimSpace(targetID) {
		return true, fmt.Errorf("cascade task notification target mismatch")
	}
	rewritten, exposedID, err := rewriteCascadeEventBodyForDevice(route.worker.platform, body, route.downstreamDeviceID)
	if err != nil {
		return true, err
	}
	if exposedID != route.exposedID {
		return true, fmt.Errorf("cascade task exposed target mismatch")
	}
	rewritten, err = rewriteCascadeTaskFields(rewritten, route, kind == cascadeTaskSnapshot)
	if err != nil {
		return true, err
	}
	identityCtx := withMonitorUserIdentityRoute(ctx, route.identity, route.localGatewayID)
	if err := route.worker.sendMessage(identityCtx, rewritten); err != nil {
		return true, err
	}
	// 上级已确认后先在内存中记忆“已发送”。墓碑落库失败时设备重传
	// 只重试持久化，不能再次发送相同的最终业务通知。
	route.completed = true
	if err := g.persistCompletedCascadeTaskRouteLocked(ctx, route); err != nil {
		return true, err
	}
	return true, nil
}

func (g *GB28181API) persistCompletedCascadeTaskRouteLocked(ctx context.Context, route *cascadeTaskRoute) error {
	if g == nil || route == nil || !route.completed {
		return nil
	}
	persisted := route.persistentStateLocked()
	persisted.Completed = true
	persisted.UpdatedAt = time.Now()
	if err := g.persistCascadeTaskRouteState(ctx, persisted); err != nil {
		route.completionPending = true
		return err
	}
	route.completionPending = false
	route.setUpdatedAt(persisted.UpdatedAt)
	g.cascadeTaskRouteMu.Lock()
	g.cascadeTaskRoutes.CompareAndDelete(cascadeTaskRouteKey(route.kind, route.downstreamDeviceID, route.downstreamSessionID), route)
	g.cascadeTaskRouteMu.Unlock()
	return nil
}

// retryPendingCompletedCascadeTaskRoutes 补写已经获得上级 SIP 成功、但完成墓碑
// 首次落库失败的路由。每轮限制数量，避免数据库恢复后维护循环被大量记录阻塞。
func (g *GB28181API) retryPendingCompletedCascadeTaskRoutes() {
	if g == nil {
		return
	}
	const batchSize = 8
	routes := make([]*cascadeTaskRoute, 0, batchSize)
	seen := make(map[*cascadeTaskRoute]struct{}, batchSize)
	g.cascadeTaskRoutes.Range(func(_, value any) bool {
		route, ok := value.(*cascadeTaskRoute)
		if !ok || route == nil {
			return true
		}
		if _, exists := seen[route]; exists {
			return true
		}
		route.notifyMu.Lock()
		pending := route.completed && route.completionPending
		route.notifyMu.Unlock()
		if !pending {
			return true
		}
		seen[route] = struct{}{}
		routes = append(routes, route)
		return len(routes) < batchSize
	})
	for _, route := range routes {
		unlock := g.lockRegisterOperation(route.downstreamDeviceID)
		route.notifyMu.Lock()
		if !route.retired.Load() && route.completed && route.completionPending {
			if err := g.persistCompletedCascadeTaskRouteLocked(g.taskPersistenceContext(), route); err != nil && !g.serviceStopped() {
				slog.Warn("retry cascade task completion persistence failed",
					"kind", route.kind,
					"device_id", route.downstreamDeviceID,
					"session_id", route.downstreamSessionID,
					"err", err,
				)
			}
		}
		route.notifyMu.Unlock()
		unlock()
	}
}

func (g *GB28181API) restoreCascadeTaskRouteByDownstream(ctx context.Context, kind, deviceID, sessionID string) (*cascadeTaskRoute, bool, error) {
	if g == nil {
		return nil, false, nil
	}
	g.cascadeTaskRouteMu.Lock()
	defer g.cascadeTaskRouteMu.Unlock()
	key := cascadeTaskRouteKey(kind, deviceID, sessionID)
	if value, ok := g.cascadeTaskRoutes.Load(key); ok {
		route, valid := value.(*cascadeTaskRoute)
		if !valid || route == nil {
			g.cascadeTaskRoutes.CompareAndDelete(key, value)
			return nil, false, nil
		}
		return route, true, nil
	}
	state, found, err := g.loadCascadeTaskRouteStateByDownstream(ctx, kind, deviceID, sessionID)
	if err != nil || !found {
		return nil, found, err
	}
	if state.Kind != strings.TrimSpace(kind) || state.DownstreamDeviceID != strings.TrimSpace(deviceID) ||
		state.DownstreamSessionID != strings.TrimSpace(sessionID) {
		invalid := fmt.Errorf("persisted cascade task downstream identity mismatch")
		if deleteErr := g.deletePersistedCascadeTaskRoute(ctx, &cascadeTaskRoute{
			kind: strings.TrimSpace(kind), downstreamDeviceID: strings.TrimSpace(deviceID), downstreamSessionID: strings.TrimSpace(sessionID),
		}); deleteErr != nil {
			return nil, true, errors.Join(invalid, deleteErr)
		}
		slog.Warn("removed mismatched persisted cascade task route", "direction", "downstream", "kind", kind, "device_id", deviceID, "session_id", sessionID)
		return nil, false, nil
	}
	return g.restoreCascadeTaskRouteStateLocked(ctx, state)
}

func (g *GB28181API) restoreCascadeTaskRouteByUpstreamLocked(ctx context.Context, kind string, worker *cascadeWorker, exposedID, sessionID string) (*cascadeTaskRoute, bool, error) {
	if g == nil || worker == nil {
		return nil, false, nil
	}
	state, found, err := g.loadCascadeTaskRouteStateByUpstream(ctx, kind, worker.platform.name, exposedID, sessionID)
	if err != nil || !found {
		return nil, found, err
	}
	if state.Kind != strings.TrimSpace(kind) || state.PlatformName != worker.platform.name ||
		state.ExposedID != strings.TrimSpace(exposedID) || state.UpstreamSessionID != strings.TrimSpace(sessionID) {
		invalid := fmt.Errorf("persisted cascade task upstream identity mismatch")
		if deleteErr := g.deletePersistedCascadeTaskRouteByUpstream(ctx, kind, worker.platform.name, exposedID, sessionID); deleteErr != nil {
			return nil, true, errors.Join(invalid, deleteErr)
		}
		slog.Warn("removed mismatched persisted cascade task route", "direction", "upstream", "kind", kind, "platform", worker.platform.name, "session_id", sessionID)
		return nil, false, nil
	}
	return g.restoreCascadeTaskRouteStateLocked(ctx, state)
}

// restoreCascadeTaskRouteStateLocked 在 cascadeTaskRouteMu 持有期间恢复双向索引。
func (g *GB28181API) restoreCascadeTaskRouteStateLocked(ctx context.Context, state cascadeTaskRoutePersistentState) (*cascadeTaskRoute, bool, error) {
	updatedAt := state.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = state.CreatedAt
	}
	if runtimeStateExpired(updatedAt, time.Now(), cascadeTaskRouteTTL) {
		route := &cascadeTaskRoute{
			kind: state.Kind, downstreamDeviceID: state.DownstreamDeviceID, downstreamSessionID: state.DownstreamSessionID,
		}
		if err := g.deletePersistedCascadeTaskRoute(ctx, route); err != nil {
			return nil, true, err
		}
		return nil, false, nil
	}
	identity := state.Identity.clone()
	localGatewayID := strings.TrimSpace(state.LocalGatewayID)
	var worker *cascadeWorker
	if !state.Completed {
		if g.svr == nil || g.svr.cascade == nil {
			return nil, true, fmt.Errorf("cascade manager is unavailable for persisted task route")
		}
		var ok bool
		worker, ok = g.svr.cascade.workerByName(state.PlatformName)
		if !ok {
			return nil, true, fmt.Errorf("persisted cascade task upstream %q is unavailable", state.PlatformName)
		}
		if !worker.protocolVersion().AtLeast(GBVersion30) {
			return nil, true, fmt.Errorf("persisted cascade task upstream no longer supports protocol 3.0")
		}
		if worker.platform.channelIDMap[state.DownstreamTargetID] != state.ExposedID {
			return nil, true, fmt.Errorf("persisted cascade task channel mapping changed")
		}
		if identity != nil {
			policy := worker.platform.monitorUserIdentity
			if policy == nil {
				return nil, true, fmt.Errorf("persisted cascade task identity policy is unavailable")
			}
			if err := policy.validateInbound(identity); err != nil {
				return nil, true, fmt.Errorf("persisted cascade task identity is no longer authorized: %w", err)
			}
			localGatewayID = policy.localGatewayID
		}
	}
	route := &cascadeTaskRoute{
		kind: state.Kind, worker: worker, upstreamPlatform: state.PlatformName, downstreamDeviceID: state.DownstreamDeviceID,
		downstreamTargetID: state.DownstreamTargetID, exposedID: state.ExposedID,
		upstreamSessionID: state.UpstreamSessionID, downstreamSessionID: state.DownstreamSessionID,
		requestFingerprint: state.RequestFingerprint, identity: identity, localGatewayID: localGatewayID,
		createdAt: state.CreatedAt, completed: state.Completed, startDone: make(chan struct{}),
		startResult: state.StartResult,
	}
	route.setUpdatedAt(updatedAt)
	if state.StartError != "" {
		route.startErr = errors.New(state.StartError)
	}
	if state.StartFinished {
		close(route.startDone)
	} else {
		route.startResult = "ERROR"
		route.startErr = errors.New("cascade task start was interrupted by restart")
		close(route.startDone)
	}
	downstreamKey := cascadeTaskRouteKey(route.kind, route.downstreamDeviceID, route.downstreamSessionID)
	upstreamKey := cascadeTaskUpstreamRouteKeyByName(route.kind, route.upstreamPlatform, route.exposedID, route.upstreamSessionID)
	downstreamLoaded := false
	if actual, loaded := g.cascadeTaskRoutes.LoadOrStore(downstreamKey, route); loaded {
		downstreamLoaded = true
		existing, valid := actual.(*cascadeTaskRoute)
		if !valid || existing == nil {
			return nil, true, fmt.Errorf("invalid restored cascade task downstream route")
		}
		if existing.upstreamPlatform != state.PlatformName || existing.exposedID != state.ExposedID ||
			existing.upstreamSessionID != state.UpstreamSessionID || existing.requestFingerprint != state.RequestFingerprint {
			return nil, true, fmt.Errorf("persisted cascade task downstream route conflicts with runtime state")
		}
		route = existing
	}
	if actual, loaded := g.cascadeTaskRoutes.LoadOrStore(upstreamKey, route); loaded && actual != route {
		if !downstreamLoaded {
			g.cascadeTaskRoutes.CompareAndDelete(downstreamKey, route)
		}
		return nil, true, fmt.Errorf("persisted cascade task upstream route conflicts with runtime state")
	}
	return route, true, nil
}

func rewriteCascadeTaskFields(body []byte, route *cascadeTaskRoute, snapshot bool) ([]byte, error) {
	if route == nil {
		return nil, fmt.Errorf("cascade task route is unavailable")
	}
	decoder := sip.NewGBXMLDecoder(body)
	var output bytes.Buffer
	encoder := xml.NewEncoder(&output)
	stack := make([]xml.Name, 0, 8)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode cascade task notification: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			stack = append(stack, value.Name)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) == 0 {
				break
			}
			text := strings.TrimSpace(string(value))
			switch stack[len(stack)-1].Local {
			case "SessionID":
				if text == route.downstreamSessionID {
					token = xml.CharData([]byte(route.upstreamSessionID))
				}
			case "SnapShotFileID":
				if snapshot && strings.HasPrefix(text, route.downstreamTargetID+"02") {
					token = xml.CharData([]byte(route.exposedID + strings.TrimPrefix(text, route.downstreamTargetID)))
				}
			}
		}
		if err := encoder.EncodeToken(token); err != nil {
			return nil, fmt.Errorf("encode cascade task notification: %w", err)
		}
	}
	if err := encoder.Flush(); err != nil {
		return nil, fmt.Errorf("flush cascade task notification: %w", err)
	}
	encoded, err := sip.EncodeGBXMLDocument(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("encode cascade task notification as GB2312: %w", err)
	}
	return encoded, nil
}

func (g *GB28181API) cleanupCascadeTaskRoutes(now time.Time) {
	if g == nil {
		return
	}
	g.cascadeTaskRouteMu.Lock()
	defer g.cascadeTaskRouteMu.Unlock()
	g.cleanupCascadeTaskRoutesLocked(now)
}

func (g *GB28181API) cleanupCascadeTaskRoutesLocked(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	g.cascadeTaskRoutes.Range(func(key, value any) bool {
		route, ok := value.(*cascadeTaskRoute)
		if !ok || route == nil || runtimeStateExpired(route.lastUpdatedAt(), now, cascadeTaskRouteTTL) {
			if route == nil {
				g.cascadeTaskRoutes.CompareAndDelete(key, value)
			} else {
				g.deleteCascadeTaskRouteLocked(route)
			}
		}
		return true
	})
}

func (g *GB28181API) removeCascadeTaskRoutes(worker *cascadeWorker) {
	if g == nil || worker == nil {
		return
	}
	// 先快照候选路由，再逐个取得 notifyMu。不能在持有
	// cascadeTaskRouteMu 时获取 notifyMu，否则与最终通知路径
	// （notifyMu -> persist -> cascadeTaskRouteMu）形成锁顺序反转。
	candidates := make([]*cascadeTaskRoute, 0)
	g.cascadeTaskRouteMu.Lock()
	g.cascadeTaskRoutes.Range(func(_, value any) bool {
		route, _ := value.(*cascadeTaskRoute)
		if route != nil && route.worker == worker {
			candidates = append(candidates, route)
		}
		return true
	})
	g.cascadeTaskRouteMu.Unlock()
	for _, route := range candidates {
		route.notifyMu.Lock()
		completed := route.completed
		if completed {
			// 最终通知已发给上级但完成墓碑可能尚未落库。
			// 保留幂等路由并标记旧 worker 已脱离，避免热更新后设备重传再次发网。
			route.workerDetached.Store(true)
		}
		if completed {
			route.notifyMu.Unlock()
			continue
		}
		g.cascadeTaskRouteMu.Lock()
		// notifyMu 仍由当前协程持有，防止并发完成通知或新 worker 替换误删路由。
		if route.worker == worker && !route.completed {
			g.deleteCascadeTaskRouteLocked(route)
		}
		g.cascadeTaskRouteMu.Unlock()
		route.notifyMu.Unlock()
	}
}

func (g *GB28181API) removeCascadeTaskRoutesForDevice(deviceID string) {
	if g == nil {
		return
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return
	}
	g.cascadeTaskRouteMu.Lock()
	defer g.cascadeTaskRouteMu.Unlock()
	g.cascadeTaskRoutes.Range(func(_, value any) bool {
		route, _ := value.(*cascadeTaskRoute)
		if route != nil && route.downstreamDeviceID == deviceID {
			g.deleteCascadeTaskRouteLocked(route)
		}
		return true
	})
}

func (g *GB28181API) forwardCascadeVideoUploadNotify(ctx context.Context, sourceDeviceID string, body []byte) error {
	if g == nil || g.svr == nil || g.svr.cascade == nil {
		return nil
	}
	var errs []error
	for _, worker := range g.svr.cascade.registeredWorkers(GBVersion30) {
		if _, err := g.forwardCascadeVideoUploadNotifyToWorker(ctx, sourceDeviceID, body, worker); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// forwardCascadeVideoUploadNotifyToWorker 返回 true 表示该目标已完成或当前共享规则无需投递。
func (g *GB28181API) forwardCascadeVideoUploadNotifyToWorker(ctx context.Context, sourceDeviceID string, body []byte, worker *cascadeWorker) (bool, error) {
	if worker == nil {
		return false, fmt.Errorf("VideoUploadNotify upstream is unavailable")
	}
	platformName := strings.TrimSpace(worker.platform.name)
	receiptID := videoUploadReceiptID(sourceDeviceID, platformName, body)
	unlock, err := g.lockAlarmInboxOperation(ctx, "video-upload:"+receiptID)
	if err != nil {
		return false, fmt.Errorf("%s: lock VideoUploadNotify delivery: %w", platformName, err)
	}
	defer unlock()
	completed, err := g.videoUploadReceiptExists(ctx, sourceDeviceID, receiptID)
	if err != nil {
		return false, fmt.Errorf("%s: %w", platformName, err)
	}
	if completed {
		return true, nil
	}
	rewritten, exposedID, err := rewriteCascadeEventBodyForDevice(worker.platform, body, sourceDeviceID)
	if err != nil {
		return false, fmt.Errorf("%s: %w", platformName, err)
	}
	if exposedID == "" {
		return true, nil
	}
	if err := worker.sendMessage(ctx, rewritten); err != nil {
		return false, fmt.Errorf("%s: %w", platformName, err)
	}
	completedAt := time.Now()
	// 上级已经给出 SIP 成功确认后，后续只能重试本地回执落库，不能因为数据库
	// 瞬时失败再次发送同一业务通知。
	g.recordPendingVideoUploadReceipt(receiptID, completedAt)
	if err := g.saveTaskState(g.taskPersistenceContext(), gbTaskKindVideoUploadReceipt, sourceDeviceID, receiptID, []byte(`{"completed":true}`), completedAt); err != nil {
		return false, fmt.Errorf("%s: persist VideoUploadNotify receipt: %w", platformName, err)
	}
	g.recordVideoUploadReceipt(receiptID, completedAt)
	return true, nil
}

func videoUploadReceiptID(sourceDeviceID, platformName string, body []byte) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(strings.TrimSpace(sourceDeviceID)))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(strings.TrimSpace(platformName)))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(body)
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func (g *GB28181API) videoUploadReceiptExists(ctx context.Context, sourceDeviceID, receiptID string) (bool, error) {
	if g == nil {
		return false, nil
	}
	now := time.Now()
	g.videoUploadReceiptMu.Lock()
	completedAt, ok := g.videoUploadReceipts[receiptID]
	if ok && now.Sub(completedAt) <= videoUploadReceiptRetention {
		g.videoUploadReceiptMu.Unlock()
		return true, nil
	}
	if ok {
		delete(g.videoUploadReceipts, receiptID)
	}
	pendingAt, pending := g.videoUploadPendingReceipts[receiptID]
	if pending && now.Sub(pendingAt) > videoUploadReceiptRetention {
		delete(g.videoUploadPendingReceipts, receiptID)
		pending = false
	}
	g.videoUploadReceiptMu.Unlock()
	if pending {
		if err := g.saveTaskState(ctx, gbTaskKindVideoUploadReceipt, sourceDeviceID, receiptID, []byte(`{"completed":true}`), pendingAt); err != nil {
			return false, err
		}
		g.recordVideoUploadReceipt(receiptID, pendingAt)
		return true, nil
	}
	payload, found, err := g.loadTaskState(ctx, gbTaskKindVideoUploadReceipt, sourceDeviceID, receiptID)
	if err != nil || !found {
		return found, err
	}
	var receipt struct {
		Completed bool `json:"completed"`
	}
	if err := json.Unmarshal(payload, &receipt); err != nil || !receipt.Completed {
		if err == nil {
			err = errors.New("VideoUploadNotify receipt is not completed")
		}
		if deleteErr := g.deleteTaskState(ctx, gbTaskKindVideoUploadReceipt, sourceDeviceID, receiptID); deleteErr != nil {
			return false, errors.Join(err, deleteErr)
		}
		return false, nil
	}
	g.recordVideoUploadReceipt(receiptID, now)
	return true, nil
}

func (g *GB28181API) recordPendingVideoUploadReceipt(receiptID string, completedAt time.Time) {
	if g == nil || strings.TrimSpace(receiptID) == "" {
		return
	}
	g.videoUploadReceiptMu.Lock()
	if g.videoUploadPendingReceipts == nil {
		g.videoUploadPendingReceipts = make(map[string]time.Time)
	}
	if len(g.videoUploadPendingReceipts) >= maxVideoUploadReceipts {
		oldestKey := ""
		var oldestAt time.Time
		for key, candidate := range g.videoUploadPendingReceipts {
			if oldestKey == "" || candidate.Before(oldestAt) {
				oldestKey, oldestAt = key, candidate
			}
		}
		delete(g.videoUploadPendingReceipts, oldestKey)
	}
	g.videoUploadPendingReceipts[receiptID] = completedAt
	g.videoUploadReceiptMu.Unlock()
}

func (g *GB28181API) recordVideoUploadReceipt(receiptID string, completedAt time.Time) {
	if g == nil || strings.TrimSpace(receiptID) == "" {
		return
	}
	g.videoUploadReceiptMu.Lock()
	if g.videoUploadReceipts == nil {
		g.videoUploadReceipts = make(map[string]time.Time)
	}
	if len(g.videoUploadReceipts) >= maxVideoUploadReceipts {
		oldestKey := ""
		var oldestAt time.Time
		for key, candidate := range g.videoUploadReceipts {
			if oldestKey == "" || candidate.Before(oldestAt) {
				oldestKey, oldestAt = key, candidate
			}
		}
		delete(g.videoUploadReceipts, oldestKey)
	}
	g.videoUploadReceipts[receiptID] = completedAt
	delete(g.videoUploadPendingReceipts, receiptID)
	g.videoUploadReceiptMu.Unlock()
}

func (g *GB28181API) cleanupVideoUploadReceipts(now time.Time) {
	if g == nil {
		return
	}
	cutoff := now.Add(-videoUploadReceiptRetention)
	g.videoUploadReceiptMu.Lock()
	for key, completedAt := range g.videoUploadReceipts {
		if completedAt.Before(cutoff) {
			delete(g.videoUploadReceipts, key)
		}
	}
	for key, completedAt := range g.videoUploadPendingReceipts {
		if completedAt.Before(cutoff) {
			delete(g.videoUploadPendingReceipts, key)
		}
	}
	g.videoUploadReceiptMu.Unlock()
}
