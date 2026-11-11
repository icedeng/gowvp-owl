package owl

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"gorm.io/gorm"
)

type connection struct {
	platformSubject string
	host            *Host
	ingress         core.IngressRoute
	mu              sync.RWMutex
	route           protocol.Route
	closed          bool
	writeDeadline   time.Time
}

func newConnection(host *Host, in core.SignalIngress) *connection {
	return &connection{host: host, ingress: in.Ingress, route: in.Route}
}
func (c *connection) RoutingScope() string {
	return scopePrefix(c.ingress.GatewayPK, c.ingress.Generation, c.ingress.ConnectionEpoch) + c.ingress.FlowID
}
func (c *connection) AdvertisedAddr() net.Addr {
	return endpointAddress(c.ingress.SIPAdvertise, c.Network())
}
func (c *connection) Network() string            { c.mu.RLock(); defer c.mu.RUnlock(); return c.route.Transport }
func (c *connection) SignalingTransport() string { return strings.ToUpper(c.Network()) }
func endpointAddress(e protocol.Endpoint, network string) net.Addr {
	if network == "udp" {
		return &net.UDPAddr{IP: net.ParseIP(e.IP), Port: e.Port}
	}
	return &net.TCPAddr{IP: net.ParseIP(e.IP), Port: e.Port}
}
func (c *connection) LocalAddr() net.Addr {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return endpointAddress(c.route.Listener, c.route.Transport)
}
func (c *connection) RemoteAddr() net.Addr {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return endpointAddress(c.route.Peer, c.route.Transport)
}
func (c *connection) Read([]byte) (int, error) {
	return 0, fmt.Errorf("外部信令通过受控消息入口读取")
}
func (c *connection) ReadFrom([]byte) (int, net.Addr, error) {
	return 0, nil, fmt.Errorf("外部信令通过受控消息入口读取")
}
func (c *connection) SetReadDeadline(time.Time) error { return nil }
func (c *connection) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = deadline
	return nil
}
func (c *connection) SetDeadline(deadline time.Time) error { return c.SetWriteDeadline(deadline) }
func (c *connection) Write(data []byte) (int, error)       { return c.WriteTo(data, c.RemoteAddr()) }
func (c *connection) WriteTo(data []byte, target net.Addr) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.WriteToContext(ctx, data, target)
}
func (c *connection) WriteToContext(ctx context.Context, data []byte, target net.Addr) (int, error) {
	c.mu.RLock()
	route, closed, deadline := c.route, c.closed, c.writeDeadline
	c.mu.RUnlock()
	if closed {
		return 0, net.ErrClosed
	}
	if target != nil && target.String() != route.Peer.String() {
		return 0, &ipc.IngressDenied{Message: "目标不属于当前 SIP flow"}
	}
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	manager := c.host.manager()
	if manager == nil {
		return 0, net.ErrClosed
	}
	if e := manager.SendSignal(ctx, c.ingress.GatewayPK, c.ingress.Generation, c.ingress.ConnectionEpoch, route, data); e != nil {
		return 0, e
	}
	return len(data), nil
}
func (c *connection) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	flow := c.route.FlowID
	c.mu.Unlock()
	manager := c.host.manager()
	if manager == nil {
		return nil
	}
	snapshot := manager.Snapshot(c.ingress.GatewayPK)
	if snapshot.Generation != c.ingress.Generation || snapshot.Epoch != c.ingress.ConnectionEpoch {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, e := manager.Call(ctx, c.ingress.GatewayPK, "sip.flow.close", protocol.FlowClose{OperationID: uuid.NewString(), FlowID: flow, Reason: "host_connection_closed"})
	return e
}
func (c *connection) RegistrationCommitted(ctx context.Context, deviceCode string, expires int) error {
	var device ipc.Device
	if e := c.host.db.WithContext(ctx).Select("id").Where("device_id = ?", deviceCode).First(&device).Error; e != nil {
		return e
	}
	binding, e := c.host.repo.Binding(ctx, device.ID)
	if e != nil {
		return e
	}
	if binding.GatewayID == nil || *binding.GatewayID != c.ingress.GatewayPK || binding.LastGeneration == nil || *binding.LastGeneration != c.ingress.Generation || binding.LastConnectionEpoch == nil || protocol.Number(*binding.LastConnectionEpoch) != c.ingress.ConnectionEpoch {
		return &ipc.IngressDenied{Message: "注册绑定不属于此网关代次"}
	}
	action := "bind"
	if expires == 0 {
		action = "unbind"
	}
	manager := c.host.manager()
	if manager == nil {
		return net.ErrClosed
	}
	response, e := manager.Call(ctx, c.ingress.GatewayPK, "sip.binding.update", protocol.BindingUpdate{OperationID: uuid.NewString(), Action: action, Binding: protocol.Binding{SubjectType: subjectKind(deviceCode), SubjectID: device.ID, FlowID: c.ingress.FlowID, BindingRevision: protocol.Number(binding.BindingRevision), ExpiresInMS: expires * 1000}})
	if e != nil {
		return e
	}
	if !response.(*protocol.BindingResult).Applied {
		return &ipc.IngressDenied{Message: "B 尚未应用注册绑定"}
	}
	c.mu.Lock()
	revision := protocol.Number(binding.BindingRevision)
	c.route.BindingRevision = &revision
	c.mu.Unlock()
	return nil
}
func (c *connection) OpenSignalingTransport(ctx context.Context, network string, target net.Addr) (sip.Connection, error) {
	if network != "tcp" || target == nil {
		return nil, &ipc.IngressDenied{Message: "仅允许已认证设备的 SIP/TCP 升级"}
	}
	c.mu.RLock()
	route := c.route
	c.mu.RUnlock()
	if route.BindingRevision == nil {
		return nil, &ipc.IngressDenied{Message: "尚未认证的 flow 不能主动建连"}
	}
	var bindingSubject string
	if e := c.host.repo.Read(ctx, func(db *gorm.DB) error {
		return db.Table("access_gateway_bindings").Select("subject_id").Where("gateway_id = ? AND binding_revision = ? AND last_generation = ? AND last_connection_epoch = ? AND last_flow_id = ?", c.ingress.GatewayPK, *route.BindingRevision, c.ingress.Generation, c.ingress.ConnectionEpoch, c.ingress.FlowID).Scan(&bindingSubject).Error
	}); e != nil {
		return nil, e
	}
	kind := "device"
	if bindingSubject == "" {
		kind = "platform"
		if e := c.host.repo.Read(ctx, func(db *gorm.DB) error {
			return db.Table("access_gateway_platform_bindings").Select("subject_id").Where("gateway_id = ? AND binding_revision = ? AND last_generation = ? AND last_connection_epoch = ? AND last_flow_id = ?", c.ingress.GatewayPK, *route.BindingRevision, c.ingress.Generation, c.ingress.ConnectionEpoch, c.ingress.FlowID).Scan(&bindingSubject).Error
		}); e != nil {
			return nil, e
		}
	}
	if bindingSubject == "" {
		return nil, &ipc.IngressDenied{Message: "无法确定此 flow 的已认证对象归属"}
	}
	host, port, e := net.SplitHostPort(target.String())
	if e != nil {
		return nil, e
	}
	number, e := strconv.Atoi(port)
	if e != nil {
		return nil, e
	}
	manager := c.host.manager()
	if manager == nil {
		return nil, net.ErrClosed
	}
	response, e := manager.Call(ctx, c.ingress.GatewayPK, "sip.flow.open", protocol.FlowOpen{OperationID: uuid.NewString(), SubjectType: kind, SubjectID: bindingSubject, Target: protocol.Endpoint{IP: host, Port: number}, Transport: "tcp", BindingRevision: *route.BindingRevision})
	if e != nil {
		return nil, e
	}
	next := response.(*protocol.FlowOpenResult).Route
	ingress := c.ingress
	ingress.FlowID = next.FlowID
	upgraded := newConnection(c.host, core.SignalIngress{Ingress: ingress, Route: next})
	upgraded.platformSubject = c.platformSubject
	return upgraded, nil
}

var _ sip.ExternalRoutingConnection = (*connection)(nil)
