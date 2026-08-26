package gbs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	cascadeTaskUpgrade   = "upgrade"
	cascadeTaskSnapshot  = "snapshot"
	cascadeTaskRouteTTL  = 7 * 24 * time.Hour
	maxCascadeTaskRoutes = 1024
)

type cascadeTaskRoute struct {
	notifyMu            sync.Mutex
	completed           bool
	startOnce           sync.Once
	startDone           chan struct{}
	startResult         string
	startErr            error
	kind                string
	worker              *cascadeWorker
	downstreamDeviceID  string
	downstreamTargetID  string
	exposedID           string
	upstreamSessionID   string
	downstreamSessionID string
	requestFingerprint  string
	identity            *monitorUserIdentity
	localGatewayID      string
	createdAt           time.Time
}

func cascadeTaskRouteKey(kind, deviceID, sessionID string) string {
	return "downstream:" + strings.TrimSpace(kind) + ":" + strings.TrimSpace(deviceID) + ":" + strings.TrimSpace(sessionID)
}

func cascadeTaskUpstreamRouteKey(kind string, worker *cascadeWorker, exposedID, sessionID string) string {
	name := ""
	if worker != nil {
		name = worker.platform.name
	}
	return "upstream:" + strings.TrimSpace(kind) + ":" + strings.TrimSpace(name) + ":" + strings.TrimSpace(exposedID) + ":" + strings.TrimSpace(sessionID)
}

func cascadeTaskFingerprint(value []byte) string {
	digest := sha256.Sum256(value)
	return fmt.Sprintf("%x", digest[:])
}

func (g *GB28181API) registerCascadeTaskRoute(ctx context.Context, kind string, worker *cascadeWorker, channel *ipc.Channel, exposedID, sessionID, fingerprint string) (*cascadeTaskRoute, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if g == nil || worker == nil || channel == nil || !worker.protocolVersion().AtLeast(GBVersion30) {
		return nil, false, fmt.Errorf("cascade task requires protocol 3.0")
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
		if !ok || existing == nil || existing.worker != worker || existing.downstreamTargetID != strings.TrimSpace(channel.ChannelID) ||
			existing.requestFingerprint != fingerprint {
			return nil, false, fmt.Errorf("cascade task session is already owned by another route")
		}
		return existing, false, nil
	}
	if g.cascadeTaskRouteCountLocked() >= maxCascadeTaskRoutes {
		return nil, false, fmt.Errorf("cascade task route capacity exceeded")
	}
	downstreamSessionID := sip.RandString(32)
	localGatewayID, _ := ctx.Value(monitorUserIdentityGatewayContextKey{}).(string)
	route := &cascadeTaskRoute{
		kind: kind, worker: worker, downstreamDeviceID: strings.TrimSpace(channel.DeviceID),
		downstreamTargetID: strings.TrimSpace(channel.ChannelID), exposedID: exposedID,
		upstreamSessionID: sessionID, downstreamSessionID: downstreamSessionID, requestFingerprint: fingerprint,
		startDone: make(chan struct{}), createdAt: time.Now(),
		identity: monitorUserIdentityFromContext(ctx), localGatewayID: strings.TrimSpace(localGatewayID),
	}
	downstreamKey := cascadeTaskRouteKey(kind, route.downstreamDeviceID, downstreamSessionID)
	g.cascadeTaskRoutes.Store(downstreamKey, route)
	actual, loaded := g.cascadeTaskRoutes.LoadOrStore(upstreamKey, route)
	if loaded {
		g.cascadeTaskRoutes.CompareAndDelete(downstreamKey, route)
		existing, ok := actual.(*cascadeTaskRoute)
		if !ok || existing == nil || existing.worker != worker || existing.downstreamTargetID != route.downstreamTargetID ||
			existing.requestFingerprint != fingerprint {
			return nil, false, fmt.Errorf("cascade task session is already owned by another route")
		}
		return existing, false, nil
	}
	return route, true, nil
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
	g.cascadeTaskRoutes.CompareAndDelete(cascadeTaskRouteKey(route.kind, route.downstreamDeviceID, route.downstreamSessionID), route)
	g.cascadeTaskRoutes.CompareAndDelete(cascadeTaskUpstreamRouteKey(route.kind, route.worker, route.exposedID, route.upstreamSessionID), route)
}

func (g *GB28181API) forwardCascadeTaskNotification(ctx context.Context, kind, deviceID, targetID, sessionID string, body []byte) (bool, error) {
	if g == nil {
		return false, nil
	}
	key := cascadeTaskRouteKey(kind, deviceID, sessionID)
	value, ok := g.cascadeTaskRoutes.Load(key)
	if !ok {
		return false, nil
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
		return true, nil
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
	route.completed = true
	g.cascadeTaskRouteMu.Lock()
	g.cascadeTaskRoutes.CompareAndDelete(cascadeTaskRouteKey(route.kind, route.downstreamDeviceID, route.downstreamSessionID), route)
	g.cascadeTaskRouteMu.Unlock()
	return true, nil
}

func rewriteCascadeTaskFields(body []byte, route *cascadeTaskRoute, snapshot bool) ([]byte, error) {
	if route == nil {
		return nil, fmt.Errorf("cascade task route is unavailable")
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
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
	return output.Bytes(), nil
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
		if !ok || route == nil || runtimeStateExpired(route.createdAt, now, cascadeTaskRouteTTL) {
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
	g.cascadeTaskRouteMu.Lock()
	defer g.cascadeTaskRouteMu.Unlock()
	g.cascadeTaskRoutes.Range(func(_, value any) bool {
		route, _ := value.(*cascadeTaskRoute)
		if route != nil && route.worker == worker {
			g.deleteCascadeTaskRouteLocked(route)
		}
		return true
	})
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
		rewritten, exposedID, err := rewriteCascadeEventBodyForDevice(worker.platform, body, sourceDeviceID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", worker.platform.name, err))
			continue
		}
		if exposedID == "" {
			continue
		}
		if err := worker.sendMessage(ctx, rewritten); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", worker.platform.name, err))
		}
	}
	return errors.Join(errs...)
}
