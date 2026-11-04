package ipc

import (
	"context"
	"sync"
)

type ptzBypassKey struct{}

var ptzCoordinator struct {
	sync.RWMutex
	guard   func(context.Context, string, func(context.Context) error) error
	resolve func(context.Context, string, string) (string, error)
}

// SetPTZCoordinator 将旧管理与级联入口接到同一通道租约协调器。仅首期单实例使用。
func SetPTZCoordinator(guard func(context.Context, string, func(context.Context) error) error, resolve func(context.Context, string, string) (string, error)) {
	ptzCoordinator.Lock()
	defer ptzCoordinator.Unlock()
	ptzCoordinator.guard = guard
	ptzCoordinator.resolve = resolve
}
func PTZBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, ptzBypassKey{}, true)
}
func CoordinatePTZ(ctx context.Context, id string, fn func(context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if ctx.Value(ptzBypassKey{}) == true {
		return fn(ctx)
	}
	ptzCoordinator.RLock()
	guard := ptzCoordinator.guard
	ptzCoordinator.RUnlock()
	if guard == nil {
		return fn(ctx)
	}
	return guard(ctx, id, func(c context.Context) error { return fn(PTZBypass(c)) })
}
func CoordinateGBPTZ(ctx context.Context, device, channel string, fn func(context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if ctx.Value(ptzBypassKey{}) == true {
		return fn(ctx)
	}
	ptzCoordinator.RLock()
	resolve := ptzCoordinator.resolve
	ptzCoordinator.RUnlock()
	if resolve == nil {
		return fn(ctx)
	}
	id, err := resolve(ctx, device, channel)
	if err != nil {
		return err
	}
	return CoordinatePTZ(ctx, id, fn)
}
