package transport

import "sync/atomic"

// Counters 累计同一 B 进程所有控制/媒体/任务 WSS 消息的负载字节，已关闭连接也保留。
type Counters struct {
	RX       atomic.Int64
	TX       atomic.Int64
	DeviceRX atomic.Int64
	DeviceTX atomic.Int64
}

func (c *Counters) DeviceReceived(n int) {
	if c != nil && n > 0 {
		addCounter(&c.DeviceRX, int64(n))
	}
}
func (c *Counters) DeviceSent(n int) {
	if c != nil && n > 0 {
		addCounter(&c.DeviceTX, int64(n))
	}
}

func addCounter(counter *atomic.Int64, n int64) {
	for {
		old := counter.Load()
		next := old + n
		if next < old {
			next = int64(1<<63 - 1)
		}
		if counter.CompareAndSwap(old, next) {
			return
		}
	}
}
func (c *Counters) Received(n int) {
	if c != nil && n > 0 {
		addCounter(&c.RX, int64(n))
	}
}
func (c *Counters) Sent(n int) {
	if c != nil && n > 0 {
		addCounter(&c.TX, int64(n))
	}
}
