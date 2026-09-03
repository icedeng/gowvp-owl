package gbs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	// 附录多响应规则限制的是每条响应消息携带的记录数，不是整个查询的 SumNum。
	gbMultiResponseMaxItems = 10000
	// 聚合结果仍需要独立的内存安全边界；该值不作为协议 SumNum 上限。
	gbMultiResponseMaxCollectedItems = 100000
)

var errMultiResponseItemLimit = errors.New("GB28181 multi-response item limit exceeded")

func (g *GB28181API) multiResponseChunkExceedsLimit(ctx *sip.Context, count int) bool {
	if ctx == nil {
		return false
	}
	return g.getDeviceGBProtocolVersion(ctx.DeviceID).AtLeast(GBVersion11) && count > gbMultiResponseMaxItems
}

func buildMultiResponseKey(deviceID, cmdType string, sn int) string {
	return fmt.Sprintf("%s:%s:%d", deviceID, cmdType, sn)
}

type multiResponseResult[T any] struct {
	Items    []T
	Expected int
	Complete bool
	Err      error
}

type multiResponseEntry[T any] struct {
	expected  int
	items     []T
	seen      map[string]struct{}
	done      chan struct{}
	complete  bool
	cancelled bool
	err       error
}

// multiResponseCollector 聚合 Catalog、RecordInfo 等分包响应。
// 查询键必须包含设备、命令和 SN，避免同一设备并发查询串包。
type multiResponseCollector[T any] struct {
	mu      sync.Mutex
	entries map[string]*multiResponseEntry[T]
	keyFn   func(T) string
	closed  bool
}

func newMultiResponseCollector[T any](keyFn func(T) string) *multiResponseCollector[T] {
	return &multiResponseCollector[T]{
		entries: make(map[string]*multiResponseEntry[T]),
		keyFn:   keyFn,
	}
}

func (c *multiResponseCollector[T]) Start(key string) *multiResponseEntry[T] {
	entry := newMultiResponseEntry[T]()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.entries[key] = entry
	c.mu.Unlock()
	return entry
}

// TryStart 仅在 key 没有在途查询时创建新代次，供生产查询在 SN 回绕时跳过碰撞键。
func (c *multiResponseCollector[T]) TryStart(key string) (*multiResponseEntry[T], bool) {
	entry := newMultiResponseEntry[T]()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, false
	}
	if current, exists := c.entries[key]; exists {
		c.mu.Unlock()
		return current, false
	}
	c.entries[key] = entry
	c.mu.Unlock()
	return entry, true
}

func newMultiResponseEntry[T any]() *multiResponseEntry[T] {
	return &multiResponseEntry[T]{
		expected: -1,
		items:    make([]T, 0),
		seen:     make(map[string]struct{}),
		done:     make(chan struct{}),
	}
}

// Add 返回 false 表示没有对应的在途查询。
func (c *multiResponseCollector[T]) Add(key string, expected int, items []T) bool {
	return c.add(key, expected, items, nil)
}

// add 在条目写入和完成信号发出前执行 accepted，供调用方原子提交同一响应包的附加元数据。
// accepted 失败时当前包的条目不会部分写入，查询会立即以该错误结束。
func (c *multiResponseCollector[T]) add(key string, expected int, items []T, accepted func() error) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return false
	}
	if entry.complete || entry.cancelled || entry.err != nil {
		return false
	}
	// 厂商分包总数偶尔前后不一致，取观测到的最大非负值，避免过早完成。
	// 附录规定的 10000 是单包上限，不能据此拒绝更大的总数。
	if expected >= 0 && expected > entry.expected {
		entry.expected = expected
	}

	acceptedItems := make([]T, 0, len(items))
	acceptedKeys := make([]string, 0, len(items))
	chunkSeen := make(map[string]struct{}, len(items))
	for _, item := range items {
		itemKey := ""
		if c.keyFn != nil {
			itemKey = c.keyFn(item)
		}
		if itemKey != "" {
			if _, exists := entry.seen[itemKey]; exists {
				continue
			}
			if _, exists := chunkSeen[itemKey]; exists {
				continue
			}
			chunkSeen[itemKey] = struct{}{}
		}
		if len(entry.items)+len(acceptedItems) >= gbMultiResponseMaxCollectedItems {
			entry.err = fmt.Errorf("%w: received at least %d, safety limit %d", errMultiResponseItemLimit, len(entry.items)+len(acceptedItems)+1, gbMultiResponseMaxCollectedItems)
			close(entry.done)
			return true
		}
		acceptedItems = append(acceptedItems, item)
		acceptedKeys = append(acceptedKeys, itemKey)
	}
	if accepted != nil {
		if err := accepted(); err != nil {
			entry.err = err
			close(entry.done)
			return true
		}
	}
	for index, item := range acceptedItems {
		if acceptedKeys[index] != "" {
			entry.seen[acceptedKeys[index]] = struct{}{}
		}
		entry.items = append(entry.items, item)
	}
	if !entry.complete && entry.expected >= 0 && len(entry.items) >= entry.expected {
		entry.complete = true
		close(entry.done)
	}
	return true
}

func (c *multiResponseCollector[T]) Has(key string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	_, ok := c.entries[key]
	c.mu.Unlock()
	return ok
}

func (c *multiResponseCollector[T]) Wait(ctx context.Context, key string) multiResponseResult[T] {
	c.mu.Lock()
	entry, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		return multiResponseResult[T]{Expected: -1}
	}
	c.mu.Unlock()
	return c.WaitEntry(ctx, key, entry)
}

// WaitEntry 只消费 Start 返回的查询代次，避免同键重建后旧等待者读取并删除新代次。
func (c *multiResponseCollector[T]) WaitEntry(ctx context.Context, key string, entry *multiResponseEntry[T]) multiResponseResult[T] {
	if c == nil || entry == nil {
		return multiResponseResult[T]{Expected: -1}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	done := entry.done
	complete := entry.complete
	cancelled := entry.cancelled
	c.mu.Unlock()

	if !complete && !cancelled {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}

	c.mu.Lock()
	if entry.cancelled {
		c.mu.Unlock()
		return multiResponseResult[T]{Expected: -1}
	}
	if current, ok := c.entries[key]; ok && current == entry {
		delete(c.entries, key)
	}
	result := multiResponseResult[T]{
		Items:    append([]T(nil), entry.items...),
		Expected: entry.expected,
		Complete: entry.complete,
		Err:      entry.err,
	}
	c.mu.Unlock()
	return result
}

func (c *multiResponseCollector[T]) Cancel(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	entry, ok := c.entries[key]
	if ok {
		c.cancelEntryLocked(key, entry)
	}
	c.mu.Unlock()
}

// CancelEntry 只取消指定查询代次，不会删除同键的新查询。
func (c *multiResponseCollector[T]) CancelEntry(key string, entry *multiResponseEntry[T]) {
	if c == nil || entry == nil {
		return
	}
	c.mu.Lock()
	c.cancelEntryLocked(key, entry)
	c.mu.Unlock()
}

func (c *multiResponseCollector[T]) cancelEntryLocked(key string, entry *multiResponseEntry[T]) {
	if current, ok := c.entries[key]; ok && current == entry {
		delete(c.entries, key)
	}
	if entry.cancelled {
		return
	}
	entry.cancelled = true
	if !entry.complete && entry.err == nil {
		close(entry.done)
	}
}

func (c *multiResponseCollector[T]) Abort(key string, cause error) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || entry == nil || entry.complete || entry.err != nil {
		return false
	}
	entry.err = cause
	close(entry.done)
	return true
}

func (c *multiResponseCollector[T]) AbortPrefix(prefix string, cause error) int {
	if c == nil || prefix == "" {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	aborted := 0
	for key, entry := range c.entries {
		if !strings.HasPrefix(key, prefix) || entry == nil || entry.complete || entry.err != nil {
			continue
		}
		entry.err = cause
		close(entry.done)
		aborted++
	}
	return aborted
}

func (c *multiResponseCollector[T]) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	entries := c.entries
	c.entries = make(map[string]*multiResponseEntry[T])
	for _, entry := range entries {
		if entry != nil && !entry.cancelled {
			entry.cancelled = true
		}
		if entry != nil && !entry.complete && entry.err == nil {
			close(entry.done)
		}
	}
	c.mu.Unlock()
}
