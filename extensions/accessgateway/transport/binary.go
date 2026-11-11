package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

func readLimited(r io.Reader, limit int) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if e != nil {
		return nil, e
	}
	if len(b) > limit {
		return nil, fmt.Errorf("message limit exceeded")
	}
	return b, nil
}

type binaryWrite struct {
	kind   byte
	data   []byte
	queued time.Time
	result chan error
}
type Binary struct {
	conn     *websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
	wg       sync.WaitGroup
	queue    chan binaryWrite
	control  chan outgoing
	incoming chan protocol.BinaryMessage
	queued   atomic.Int64
	rxState  protocol.BinaryState
	txState  protocol.BinaryState
	mu       sync.Mutex
	terminal error
	counters *Counters
}

func NewBinary(parent context.Context, conn *websocket.Conn, wireMode, direction string, transfer, receiverToA bool, counters ...*Counters) *Binary {
	ctx, cancel := context.WithCancel(parent)
	b := &Binary{conn: conn, ctx: ctx, cancel: cancel, queue: make(chan binaryWrite, 128), control: make(chan outgoing, 8), incoming: make(chan protocol.BinaryMessage, 16), rxState: protocol.BinaryState{Transfer: transfer, WireMode: wireMode, Direction: direction, ToA: receiverToA}, txState: protocol.BinaryState{Transfer: transfer, WireMode: wireMode, Direction: direction, ToA: !receiverToA}}
	if len(counters) > 0 {
		b.counters = counters[0]
	}
	conn.SetReadLimit(protocol.MaxBinaryBytes)
	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(45 * time.Second)) })
	conn.SetPingHandler(func(data string) error {
		select {
		case b.control <- outgoing{kind: websocket.PongMessage, data: []byte(data)}:
			return nil
		default:
			return protocol.Failure(protocol.MediaBackpressure, "media control queue full")
		}
	})
	b.wg.Add(2)
	go b.readLoop()
	go b.writeLoop()
	return b
}
func (b *Binary) QueuedBytes() int64 {
	select {
	case <-b.ctx.Done():
		return 0
	default:
		return max(0, b.queued.Load())
	}
}

func (b *Binary) Done() <-chan struct{} { return b.ctx.Done() }
func (b *Binary) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.terminal != nil {
		return b.terminal
	}
	return context.Canceled
}
func (b *Binary) fail(err error) {
	b.once.Do(func() { b.mu.Lock(); b.terminal = err; b.mu.Unlock(); b.cancel(); _ = b.conn.Close() })
}
func (b *Binary) Close() error { b.fail(context.Canceled); b.wg.Wait(); return nil }
func (b *Binary) Send(ctx context.Context, kind byte, data []byte) error {
	// 拷贝数据使网络读取缓冲区可以安全复用；总预算覆盖队列和在途写入。
	if len(data) > 65536 {
		return protocol.Failure(protocol.InvalidArgument, "media chunk too large")
	}
	if b.queued.Add(int64(len(data))) > 1<<20 {
		b.queued.Add(-int64(len(data)))
		b.fail(protocol.Failure(protocol.MediaBackpressure, "media byte budget exceeded"))
		return b.Err()
	}
	item := binaryWrite{kind: kind, data: append([]byte(nil), data...), queued: time.Now(), result: make(chan error, 1)}
	select {
	case b.queue <- item:
	case <-b.ctx.Done():
		b.queued.Add(-int64(len(data)))
		return b.Err()
	case <-ctx.Done():
		b.queued.Add(-int64(len(data)))
		return ctx.Err()
	default:
		b.queued.Add(-int64(len(data)))
		b.fail(protocol.Failure(protocol.MediaBackpressure, "media queue full"))
		return b.Err()
	}
	select {
	case e := <-item.result:
		return e
	case <-ctx.Done():
		b.fail(ctx.Err())
		return ctx.Err()
	case <-b.ctx.Done():
		return b.Err()
	}
}
func (b *Binary) Read(ctx context.Context) (protocol.BinaryMessage, error) {
	select {
	case m := <-b.incoming:
		return m, nil
	case <-ctx.Done():
		return protocol.BinaryMessage{}, ctx.Err()
	case <-b.ctx.Done():
		return protocol.BinaryMessage{}, b.Err()
	}
}
func (b *Binary) readLoop() {
	defer b.wg.Done()
	defer b.fail(errors.New("media reader stopped"))
	for {
		kind, data, err := b.conn.ReadMessage()
		if err != nil {
			b.fail(err)
			return
		}
		if kind != websocket.BinaryMessage {
			b.fail(protocol.Failure(protocol.InvalidArgument, "media requires binary messages"))
			return
		}
		b.counters.Received(len(data))
		m, err := protocol.DecodeBinary(data)
		if err == nil {
			err = b.rxState.Accept(m)
		}
		if err != nil {
			b.fail(err)
			return
		}
		timer := time.NewTimer(time.Second)
		select {
		case b.incoming <- m:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			b.fail(protocol.Failure(protocol.MediaBackpressure, "media receiver stalled"))
			return
		case <-b.ctx.Done():
			timer.Stop()
			return
		}
	}
}
func (b *Binary) writeLoop() {
	defer b.wg.Done()
	defer b.fail(errors.New("media writer stopped"))
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		var item binaryWrite
		var ctl outgoing
		select {
		case ctl = <-b.control:
		default:
			select {
			case ctl = <-b.control:
			case item = <-b.queue:
			case <-ticker.C:
				ctl.kind = websocket.PingMessage
			case <-b.ctx.Done():
				return
			}
		}
		var err error
		if ctl.kind != 0 {
			_ = b.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err = b.conn.WriteMessage(ctl.kind, ctl.data)
		} else {
			if time.Since(item.queued) > time.Second {
				err = protocol.Failure(protocol.MediaBackpressure, "media queue delay exceeded")
			} else {
				m := protocol.BinaryMessage{Kind: item.kind, Sequence: b.txState.Sequence + 1, Payload: item.data}
				err = b.txState.Accept(m)
				if err == nil {
					var data []byte
					data, err = protocol.EncodeBinary(m)
					if err == nil {
						_ = b.conn.SetWriteDeadline(time.Now().Add(time.Second))
						err = b.conn.WriteMessage(websocket.BinaryMessage, data)
						if err == nil {
							b.counters.Sent(len(data))
						}
					}
				}
			}
			b.queued.Add(-int64(len(item.data)))
			item.result <- err
		}
		if err != nil {
			b.fail(err)
			return
		}
	}
}
