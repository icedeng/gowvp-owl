package gbs

import (
	"bytes"
	"context"
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
	cascadeTaskUpgrade  = "upgrade"
	cascadeTaskSnapshot = "snapshot"
	cascadeTaskRouteTTL = 7 * 24 * time.Hour
)

type cascadeTaskRoute struct {
	mu                  sync.Mutex
	completed           bool
	kind                string
	worker              *cascadeWorker
	downstreamDeviceID  string
	downstreamTargetID  string
	exposedID           string
	upstreamSessionID   string
	downstreamSessionID string
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

func (g *GB28181API) registerCascadeTaskRoute(ctx context.Context, kind string, worker *cascadeWorker, channel *ipc.Channel, exposedID, sessionID string) (*cascadeTaskRoute, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if g == nil || worker == nil || channel == nil || !worker.protocolVersion().AtLeast(GBVersion30) {
		return nil, false, fmt.Errorf("cascade task requires protocol 3.0")
	}
	exposedID = strings.TrimSpace(exposedID)
	sessionID = strings.TrimSpace(sessionID)
	if !isGBDeviceIdentifier(exposedID) || validateGBSessionID(sessionID) != nil ||
		worker.platform.channelIDMap[strings.TrimSpace(channel.ChannelID)] != exposedID {
		return nil, false, fmt.Errorf("invalid cascade task route")
	}
	upstreamKey := cascadeTaskUpstreamRouteKey(kind, worker, exposedID, sessionID)
	if actual, loaded := g.cascadeTaskRoutes.Load(upstreamKey); loaded {
		existing, ok := actual.(*cascadeTaskRoute)
		if !ok || existing == nil || existing.worker != worker || existing.downstreamTargetID != strings.TrimSpace(channel.ChannelID) {
			return nil, false, fmt.Errorf("cascade task session is already owned by another route")
		}
		return existing, false, nil
	}
	downstreamSessionID := sip.RandString(32)
	localGatewayID, _ := ctx.Value(monitorUserIdentityGatewayContextKey{}).(string)
	route := &cascadeTaskRoute{
		kind: kind, worker: worker, downstreamDeviceID: strings.TrimSpace(channel.DeviceID),
		downstreamTargetID: strings.TrimSpace(channel.ChannelID), exposedID: exposedID,
		upstreamSessionID: sessionID, downstreamSessionID: downstreamSessionID, createdAt: time.Now(),
		identity: monitorUserIdentityFromContext(ctx), localGatewayID: strings.TrimSpace(localGatewayID),
	}
	downstreamKey := cascadeTaskRouteKey(kind, route.downstreamDeviceID, downstreamSessionID)
	g.cascadeTaskRoutes.Store(downstreamKey, route)
	actual, loaded := g.cascadeTaskRoutes.LoadOrStore(upstreamKey, route)
	if loaded {
		g.cascadeTaskRoutes.CompareAndDelete(downstreamKey, route)
		existing, ok := actual.(*cascadeTaskRoute)
		if !ok || existing == nil || existing.worker != worker || existing.downstreamTargetID != route.downstreamTargetID {
			return nil, false, fmt.Errorf("cascade task session is already owned by another route")
		}
		return existing, false, nil
	}
	return route, true, nil
}

func (g *GB28181API) removeCascadeTaskRoute(route *cascadeTaskRoute, created bool) {
	if g == nil || route == nil || !created {
		return
	}
	g.deleteCascadeTaskRoute(route)
}

func (g *GB28181API) deleteCascadeTaskRoute(route *cascadeTaskRoute) {
	if g == nil || route == nil {
		return
	}
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
		g.cascadeTaskRoutes.Delete(key)
		return false, nil
	}
	route.mu.Lock()
	defer route.mu.Unlock()
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
	g.deleteCascadeTaskRoute(route)
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
	if now.IsZero() {
		now = time.Now()
	}
	g.cascadeTaskRoutes.Range(func(key, value any) bool {
		route, ok := value.(*cascadeTaskRoute)
		if !ok || route == nil || runtimeStateExpired(route.createdAt, now, cascadeTaskRouteTTL) {
			if route == nil {
				g.cascadeTaskRoutes.CompareAndDelete(key, value)
			} else {
				g.deleteCascadeTaskRoute(route)
			}
		}
		return true
	})
}

func (g *GB28181API) removeCascadeTaskRoutes(worker *cascadeWorker) {
	if g == nil || worker == nil {
		return
	}
	g.cascadeTaskRoutes.Range(func(_, value any) bool {
		route, _ := value.(*cascadeTaskRoute)
		if route != nil && route.worker == worker {
			g.deleteCascadeTaskRoute(route)
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
