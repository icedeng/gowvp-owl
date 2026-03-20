package ipc

import (
	"context"
	"strings"

	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/reason"
)

// resolveDevice 按“内部 id -> device_id”顺序解析设备，兼容外部传入国标设备编号。
func (c Core) resolveDevice(ctx context.Context, id string) (*Device, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, reason.ErrBadRequest.SetMsg("device id is required")
	}

	var out Device
	if err := c.store.Device().Get(ctx, &out, orm.Where("id=?", id)); err == nil {
		return &out, nil
	} else if !orm.IsErrRecordNotFound(err) {
		return nil, reason.ErrDB.Withf(`Get err[%s]`, err.Error())
	}

	if err := c.store.Device().Get(ctx, &out, orm.Where("device_id=?", id)); err != nil {
		if orm.IsErrRecordNotFound(err) {
			return nil, reason.ErrNotFound.Withf(`Get err[%s]`, err.Error())
		}
		return nil, reason.ErrDB.Withf(`Get err[%s]`, err.Error())
	}
	return &out, nil
}
