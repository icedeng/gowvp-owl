package sip

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ixugo/goddd/pkg/conc"
)

// ObserverHandler 返回 true 表示完成任务
type ObserverHandler func(deviceID string, args ...string) bool

type observerNotification struct {
	deviceID string
	args     []string
}

type observerRegistration struct {
	handler ObserverHandler
	done    chan struct{}
	mu      sync.Mutex
	active  bool
	running bool
	pending []observerNotification
}

func newObserverRegistration(handler ObserverHandler) *observerRegistration {
	registration := &observerRegistration{
		handler: handler,
		done:    make(chan struct{}),
		active:  true,
	}
	return registration
}

func (r *observerRegistration) notify(deviceID string, args ...string) bool {
	if r == nil || r.handler == nil {
		return false
	}
	notification := observerNotification{deviceID: deviceID, args: append([]string(nil), args...)}
	r.mu.Lock()
	if !r.active {
		r.mu.Unlock()
		return false
	}
	if r.running {
		r.pending = append(r.pending, notification)
		r.mu.Unlock()
		return false
	}
	r.running = true
	r.mu.Unlock()

	for {
		matched := r.handler(notification.deviceID, notification.args...)
		r.mu.Lock()
		if !r.active {
			r.running = false
			r.pending = nil
			r.mu.Unlock()
			return false
		}
		if matched {
			r.active = false
			r.running = false
			r.pending = nil
			close(r.done)
			r.mu.Unlock()
			return true
		}
		if len(r.pending) == 0 {
			r.running = false
			r.mu.Unlock()
			return false
		}
		notification = r.pending[0]
		r.pending[0] = observerNotification{}
		r.pending = r.pending[1:]
		r.mu.Unlock()
	}
}

func (r *observerRegistration) deactivate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.active {
		r.active = false
		r.pending = nil
		close(r.done)
	}
	r.mu.Unlock()
}

// Observer 观察者
type Observer struct {
	data     conc.Map[string, *observerRegistration]
	sequence atomic.Uint64
}

// NewObserver 创建观察者
func NewObserver() *Observer {
	return &Observer{}
}

// Register 同步等待观察者完成任务
func (o *Observer) Register(deviceID string, duration time.Duration, fn ObserverHandler) {
	if o == nil || fn == nil {
		return
	}
	registration := newObserverRegistration(fn)
	if previous, replaced := o.data.Swap(deviceID, registration); replaced {
		previous.deactivate()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	// 等待通知或超时
	select {
	// 收到通知
	case <-registration.done:
	// 超时
	case <-timer.C:
	}
	o.data.CompareAndDelete(deviceID, registration)
	registration.deactivate()
}

// DefaultRegister 默认的注册行为
func (o *Observer) DefaultRegister(deviceID string) {
	key := fmt.Sprintf("%s:%d", deviceID, o.sequence.Add(1))
	o.Register(key, 7*time.Second, func(did string, _ ...string) bool {
		return deviceID == did
	})
}

// RegisterWithTimeout 自定义等待时间
func (o *Observer) RegisterWithTimeout(deviceID string, duration time.Duration) {
	key := fmt.Sprintf("%s:%d", deviceID, o.sequence.Add(1))
	o.Register(key, duration, func(did string, _ ...string) bool {
		return deviceID == did
	})
}

// Notify 通知观察者
func (o *Observer) Notify(deviceID string, args ...string) {
	if o == nil {
		return
	}
	o.data.Range(func(key string, registration *observerRegistration) bool {
		current, ok := o.data.Load(key)
		if !ok || current != registration {
			return true
		}
		if registration.notify(deviceID, args...) {
			o.data.CompareAndDelete(key, registration)
		}
		return true
	})
}
