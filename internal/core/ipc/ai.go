package ipc

import (
	"context"

	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/web"
)

// ListAIChannelsAfter 按唯一 ID 游标扫描，避免在线状态变化引起 offset 分页漏项。
// 不过滤在线状态；停止任务前由调用方重新确认最新配置。
func (c *Core) ListAIChannelsAfter(ctx context.Context, after string, size int) ([]*Channel, error) {
	var items []*Channel
	query := orm.NewQuery(2)
	query.Where("id > ?", after)
	query.OrderBy("id ASC")
	_, err := c.store.Channel().List(ctx, &items, &web.PagerFilter{Page: 1, Size: size}, query.Encode()...)
	return items, err
}
