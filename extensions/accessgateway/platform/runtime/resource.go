package runtime

import (
	"context"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

// ResolveResource 按精确代次与原操作核对丢失的 open 回执，不能匹配新会话。
func (m *Manager) ResolveResource(ctx context.Context, pk string, ref protocol.ResourceRef, operation string) (*protocol.ResourceRef, error) {
	s := m.Snapshot(pk)
	if s.ConnectionState != "connected" {
		return nil, protocol.Failure(protocol.UpstreamTimeout, "网关离线，资源清理等待确认")
	}
	if s.Generation != ref.Generation || s.Epoch != ref.ConnectionEpoch {
		return nil, nil
	}
	if ref.ID != "" {
		return &ref, nil
	}
	var cursor *string
	for {
		r, e := m.CallExact(ctx, pk, ref, "resources.list", protocol.ResourcesList{ResourceType: ref.ResourceType, Size: 200, Cursor: cursor})
		if e != nil {
			return nil, e
		}
		page := r.(*protocol.ResourcesResult)
		for _, resource := range page.Items {
			if resource.OperationID != nil && *resource.OperationID == operation {
				found := resource.Ref
				return &found, nil
			}
		}
		if page.NextCursor == nil {
			return nil, nil
		}
		cursor = page.NextCursor
	}
}
