package gbs

import (
	"context"
	"fmt"
	"sync"
)

func buildMultiResponseKey(deviceID, cmdType string, sn int) string {
	return fmt.Sprintf("%s:%s:%d", deviceID, cmdType, sn)
}

type multiResponseResult[T any] struct {
	Items    []T
	Expected int
	Complete bool
}

type multiResponseEntry[T any] struct {
	expected int
	items    []T
	seen     map[string]struct{}
	done     chan struct{}
	complete bool
}

// multiResponseCollector 聚合 Catalog、RecordInfo 等分包响应。
// 查询键必须包含设备、命令和 SN，避免同一设备并发查询串包。
type multiResponseCollector[T any] struct {
	mu      sync.Mutex
	entries map[string]*multiResponseEntry[T]
	keyFn   func(T) string
}

func newMultiResponseCollector[T any](keyFn func(T) string) *multiResponseCollector[T] {
	return &multiResponseCollector[T]{
		entries: make(map[string]*multiResponseEntry[T]),
		keyFn:   keyFn,
	}
}

func (c *multiResponseCollector[T]) Start(key string) {
	c.mu.Lock()
	c.entries[key] = &multiResponseEntry[T]{
		expected: -1,
		items:    make([]T, 0),
		seen:     make(map[string]struct{}),
		done:     make(chan struct{}),
	}
	c.mu.Unlock()
}

// Add 返回 false 表示没有对应的在途查询。
func (c *multiResponseCollector[T]) Add(key string, expected int, items []T) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return false
	}
	// 厂商分包总数偶尔前后不一致，取观测到的最大非负值，避免过早完成。
	if expected >= 0 && expected > entry.expected {
		entry.expected = expected
	}
	for _, item := range items {
		itemKey := ""
		if c.keyFn != nil {
			itemKey = c.keyFn(item)
		}
		if itemKey != "" {
			if _, exists := entry.seen[itemKey]; exists {
				continue
			}
			entry.seen[itemKey] = struct{}{}
		}
		entry.items = append(entry.items, item)
	}
	if !entry.complete && entry.expected >= 0 && len(entry.items) >= entry.expected {
		entry.complete = true
		close(entry.done)
	}
	return true
}

func (c *multiResponseCollector[T]) Wait(ctx context.Context, key string) multiResponseResult[T] {
	c.mu.Lock()
	entry, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		return multiResponseResult[T]{Expected: -1}
	}
	done := entry.done
	complete := entry.complete
	c.mu.Unlock()

	if !complete {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}

	c.mu.Lock()
	entry, ok = c.entries[key]
	if !ok {
		c.mu.Unlock()
		return multiResponseResult[T]{Expected: -1}
	}
	delete(c.entries, key)
	result := multiResponseResult[T]{
		Items:    append([]T(nil), entry.items...),
		Expected: entry.expected,
		Complete: entry.complete,
	}
	c.mu.Unlock()
	return result
}

func (c *multiResponseCollector[T]) Cancel(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}
