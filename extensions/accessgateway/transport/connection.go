// Package transport 实现 A/B 共用的 WSS 生命周期和有界消息投递。
package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

const (
	normalMessages   = 1024
	normalBytes      = 8 << 20
	priorityMessages = 128
	priorityBytes    = 1 << 20
)

type outgoing struct {
	kind     int
	data     []byte
	queued   time.Time
	result   chan error
	priority bool
}
type Incoming struct {
	Envelope protocol.Envelope
	Payload  any
	bytes    int
	priority bool
}
type Identity struct {
	GatewayID       string
	PlatformID      string
	Generation      *string
	ConnectionEpoch *protocol.Decimal
	OwnerEpoch      protocol.Decimal
}

type Peer struct {
	conn           *websocket.Conn
	ctx            context.Context
	cancel         context.CancelFunc
	once           sync.Once
	normal         chan outgoing
	priority       chan outgoing
	incoming       chan Incoming
	urgent         chan Incoming
	normalQueued   atomic.Int64
	priorityQueued atomic.Int64
	incomingBytes  atomic.Int64
	urgentBytes    atomic.Int64
	rx             atomic.Int64
	tx             atomic.Int64
	rtt            atomic.Int64
	pingAt         atomic.Int64
	mu             sync.Mutex
	pending        map[string]chan Incoming
	identity       Identity
	terminal       error
	wg             sync.WaitGroup
	counters       *Counters
}

func NewPeer(parent context.Context, conn *websocket.Conn, identity Identity, counters ...*Counters) *Peer {
	ctx, cancel := context.WithCancel(parent)
	p := &Peer{conn: conn, ctx: ctx, cancel: cancel, normal: make(chan outgoing, normalMessages), priority: make(chan outgoing, priorityMessages), incoming: make(chan Incoming, normalMessages), urgent: make(chan Incoming, priorityMessages), identity: identity, pending: map[string]chan Incoming{}}
	if len(counters) > 0 {
		p.counters = counters[0]
	}
	conn.SetReadLimit(protocol.MaxControlBytes)
	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
		if sent := p.pingAt.Load(); sent > 0 {
			p.rtt.Store(time.Now().UnixNano() - sent)
		}
		return nil
	})
	conn.SetPingHandler(func(s string) error { return p.enqueueControl(websocket.PongMessage, []byte(s)) })
	p.wg.Add(2)
	go p.readLoop()
	go p.writeLoop()
	return p
}
func (p *Peer) Done() <-chan struct{} { return p.ctx.Done() }
func (p *Peer) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.terminal != nil {
		return p.terminal
	}
	return context.Canceled
}
func (p *Peer) Close() error { p.fail(context.Canceled); p.wg.Wait(); return nil }
func (p *Peer) fail(err error) {
	p.once.Do(func() { p.mu.Lock(); p.terminal = err; p.mu.Unlock(); p.cancel(); _ = p.conn.Close() })
}
func (p *Peer) SetIdentity(id Identity) { p.mu.Lock(); p.identity = id; p.mu.Unlock() }
func (p *Peer) Identity() Identity      { p.mu.Lock(); defer p.mu.Unlock(); return p.identity }
func (p *Peer) Stats() (rx, tx int64, rtt time.Duration) {
	return p.rx.Load(), p.tx.Load(), time.Duration(p.rtt.Load())
}
func priorityType(s string) bool {
	return s == "heartbeat" || s == "heartbeat.ack" || s == "resources.close" || s == "resources.close.result" || s == "gateway.drain" || s == "gateway.drain.result" || s == "sip.packet" || s == "media.close" || s == "media.close.result" || s == "transfer.close" || s == "transfer.close.result"
}
func (p *Peer) enqueueControl(kind int, data []byte) error {
	select {
	case p.priority <- outgoing{kind: kind, data: data, queued: time.Now(), priority: true}:
		return nil
	case <-p.ctx.Done():
		return p.Err()
	default:
		return fmt.Errorf("control queue full")
	}
}
func (p *Peer) Send(ctx context.Context, env protocol.Envelope) error {
	data := protocol.Payload(env)
	if len(data) > protocol.MaxControlBytes {
		return fmt.Errorf("control message too large")
	}
	item := outgoing{kind: websocket.TextMessage, data: data, queued: time.Now(), result: make(chan error, 1), priority: priorityType(env.Type)}
	queue, budget, limit := p.normal, &p.normalQueued, int64(normalBytes)
	if item.priority {
		queue, budget, limit = p.priority, &p.priorityQueued, priorityBytes
	}
	if budget.Add(int64(len(data))) > limit {
		budget.Add(-int64(len(data)))
		return protocol.Failure(protocol.Exhausted, "control byte budget exceeded")
	}
	select {
	case queue <- item:
	case <-ctx.Done():
		budget.Add(-int64(len(data)))
		return ctx.Err()
	case <-p.ctx.Done():
		budget.Add(-int64(len(data)))
		return p.Err()
	default:
		budget.Add(-int64(len(data)))
		return protocol.Failure(protocol.Exhausted, "control queue full")
	}
	select {
	case err := <-item.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return p.Err()
	}
}
func (p *Peer) Next(ctx context.Context) (Incoming, error) {
	var item Incoming
	select {
	case item = <-p.urgent:
	default:
		select {
		case item = <-p.urgent:
		case item = <-p.incoming:
		case <-ctx.Done():
			return item, ctx.Err()
		case <-p.ctx.Done():
			return item, p.Err()
		}
	}
	if item.priority {
		p.urgentBytes.Add(-int64(item.bytes))
	} else {
		p.incomingBytes.Add(-int64(item.bytes))
	}
	return item, nil
}
func (p *Peer) Event(ctx context.Context, name string, payload any) error {
	id := p.Identity()
	return p.Send(ctx, protocol.Envelope{Version: protocol.Version, Type: name, Kind: "event", RequestID: uuid.NewString(), GatewayID: id.GatewayID, PlatformID: id.PlatformID, Generation: id.Generation, ConnectionEpoch: id.ConnectionEpoch, OwnerEpoch: id.OwnerEpoch, Payload: protocol.Payload(payload)})
}
func (p *Peer) Reply(ctx context.Context, request protocol.Envelope, payload any, rpcErr *protocol.Error) error {
	id := p.Identity()
	body := protocol.Payload(payload)
	if rpcErr != nil {
		body = protocol.Payload(map[string]any{})
	}
	return p.Send(ctx, protocol.Envelope{Version: protocol.Version, Type: protocol.ResponseType(request.Type), Kind: "response", RequestID: uuid.NewString(), ReplyTo: request.RequestID, GatewayID: id.GatewayID, PlatformID: id.PlatformID, Generation: id.Generation, ConnectionEpoch: id.ConnectionEpoch, OwnerEpoch: id.OwnerEpoch, Payload: body, Error: rpcErr})
}
func (p *Peer) Call(ctx context.Context, name string, payload any) (Incoming, error) {
	if protocol.ResponseType(name) == "" {
		return Incoming{}, fmt.Errorf("unknown RPC %q", name)
	}
	id := p.Identity()
	request := protocol.Envelope{Version: protocol.Version, Type: name, Kind: "request", RequestID: uuid.NewString(), GatewayID: id.GatewayID, PlatformID: id.PlatformID, Generation: id.Generation, ConnectionEpoch: id.ConnectionEpoch, OwnerEpoch: id.OwnerEpoch, Payload: protocol.Payload(payload)}
	result := make(chan Incoming, 1)
	p.mu.Lock()
	if len(p.pending) >= normalMessages {
		p.mu.Unlock()
		return Incoming{}, protocol.Failure(protocol.Exhausted, "RPC limit")
	}
	p.pending[request.RequestID] = result
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, request.RequestID); p.mu.Unlock() }()
	if err := p.Send(ctx, request); err != nil {
		return Incoming{}, err
	}
	select {
	case in := <-result:
		if in.Envelope.Type != protocol.ResponseType(name) {
			return in, fmt.Errorf("RPC response type mismatch")
		}
		if in.Envelope.Error != nil {
			return in, in.Envelope.Error
		}
		return in, nil
	case <-ctx.Done():
		return Incoming{}, ctx.Err()
	case <-p.ctx.Done():
		return Incoming{}, p.Err()
	}
}
func (p *Peer) readLoop() {
	defer p.wg.Done()
	defer p.fail(errors.New("WSS reader stopped"))
	for {
		kind, data, err := p.conn.ReadMessage()
		if err != nil {
			p.fail(err)
			return
		}
		p.rx.Add(int64(len(data)))
		p.counters.Received(len(data))
		if kind != websocket.TextMessage {
			p.fail(fmt.Errorf("control requires text messages"))
			return
		}
		env, payload, err := protocol.DecodeEnvelope(data)
		if err != nil {
			p.fail(err)
			return
		}
		id := p.Identity()
		if id.Generation == nil {
			p.mu.Lock()
			pending := p.pending[env.ReplyTo]
			p.mu.Unlock()
			if env.Type != "hello.ack" || env.Kind != "response" || pending == nil {
				p.fail(protocol.Failure(protocol.IdentityMismatch, "hello acknowledgement required before control traffic"))
				return
			}
			if env.Error == nil {
				if env.Generation == nil || env.ConnectionEpoch == nil {
					p.fail(protocol.Failure(protocol.StaleEpoch, "missing hello epochs"))
					return
				}
				id.Generation = env.Generation
				id.ConnectionEpoch = env.ConnectionEpoch
				p.SetIdentity(id)
			}
		}
		if env.GatewayID != id.GatewayID || env.PlatformID != id.PlatformID || env.OwnerEpoch != id.OwnerEpoch {
			p.fail(protocol.Failure(protocol.IdentityMismatch, "control identity mismatch"))
			return
		}
		if id.Generation != nil && (env.Generation == nil || *env.Generation != *id.Generation || env.ConnectionEpoch == nil || id.ConnectionEpoch == nil || *env.ConnectionEpoch != *id.ConnectionEpoch) {
			p.fail(protocol.Failure(protocol.StaleEpoch, "control epoch mismatch"))
			return
		}
		in := Incoming{Envelope: env, Payload: payload, bytes: len(data), priority: priorityType(env.Type)}
		if env.Kind == "response" {
			p.mu.Lock()
			response := p.pending[env.ReplyTo]
			p.mu.Unlock()
			if response != nil {
				select {
				case response <- in:
				default:
				}
			}
			continue
		}
		queue, budget, limit := p.incoming, &p.incomingBytes, int64(normalBytes)
		if in.priority {
			queue, budget, limit = p.urgent, &p.urgentBytes, priorityBytes
		}
		if budget.Add(int64(len(data))) > limit {
			budget.Add(-int64(len(data)))
			p.fail(protocol.Failure(protocol.Exhausted, "incoming control byte budget exceeded"))
			return
		}
		select {
		case queue <- in:
		case <-p.ctx.Done():
			return
		default:
			budget.Add(-int64(len(data)))
			p.fail(protocol.Failure(protocol.Exhausted, "incoming control queue full"))
			return
		}
	}
}
func (p *Peer) writeLoop() {
	defer p.wg.Done()
	defer p.fail(errors.New("WSS writer stopped"))
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		var item outgoing
		select {
		case item = <-p.priority:
		default:
			select {
			case item = <-p.priority:
			case item = <-p.normal:
			case <-ticker.C:
				item = outgoing{kind: websocket.PingMessage, queued: time.Now()}
				p.pingAt.Store(time.Now().UnixNano())
			case <-p.ctx.Done():
				return
			}
		}
		if item.result != nil {
			if item.priority {
				p.priorityQueued.Add(-int64(len(item.data)))
			} else {
				p.normalQueued.Add(-int64(len(item.data)))
			}
		}
		_ = p.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		err := p.conn.WriteMessage(item.kind, item.data)
		if item.result != nil {
			item.result <- err
		}
		if err != nil {
			p.fail(err)
			return
		}
		p.tx.Add(int64(len(item.data)))
		p.counters.Sent(len(item.data))
	}
}
