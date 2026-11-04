package api

import (
	"context"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/zlm"
)

// channelStreamStatus 仅用于列表响应，不将媒体节点的瞬时状态持久化。
type channelStreamStatus struct {
	*ipc.Channel
	StreamStatus string `json:"stream_status,omitempty" enums:"streaming,idle,unknown"` // 仅 include_stream_status=true 返回；直播推流中、未推流或未知
}

type liveStreamKey struct{ app, stream string }
type channelStreamKey struct {
	node string
	liveStreamKey
}

func liveChannelStream(ch *ipc.Channel) (channelStreamKey, bool) {
	key := channelStreamKey{node: channelMediaServerID(ch)}
	switch ch.GetType() {
	case ipc.TypeGB28181:
		key.app, key.stream = "rtp", ch.ID
	case ipc.TypeRTMP:
		key.app, key.stream = ch.App, ch.Stream
	case ipc.TypeRTSP:
		key.app, key.stream = ch.App, ch.Stream
		if key.app == "" {
			key.app = "live"
		}
	case ipc.TypeOnvif:
		key.node, key.app, key.stream = sms.DefaultMediaServerID, "live", ch.ID
	default:
		return key, false
	}
	return key, key.app != "" && key.stream != ""
}

func (a IPCAPI) channelStreamStatuses(ctx context.Context, items []*ipc.Channel) []*channelStreamStatus {
	ctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	return queryChannelStreamStatuses(ctx, items, func(ctx context.Context, id string) (*zlm.GetMediaListResponse, error) {
		if a.uc == nil || a.uc.SMSAPI.smsCore.NodeManager == nil {
			return nil, nil
		}
		server, err := a.uc.SMSAPI.smsCore.GetMediaServer(ctx, id)
		if err != nil {
			return nil, err
		}
		return a.uc.SMSAPI.smsCore.GetMediaListContext(ctx, server)
	})
}

// 每个节点只查一次；节点失败保留 unknown，不能把无法查询误报为未推流。
func queryChannelStreamStatuses(ctx context.Context, items []*ipc.Channel, fetch func(context.Context, string) (*zlm.GetMediaListResponse, error)) []*channelStreamStatus {
	out := make([]*channelStreamStatus, len(items))
	groups := make(map[string][]int)
	keys := make([]channelStreamKey, len(items))
	for i, ch := range items {
		out[i] = &channelStreamStatus{Channel: ch, StreamStatus: "unknown"}
		if key, ok := liveChannelStream(ch); ok {
			keys[i] = key
			groups[key.node] = append(groups[key.node], i)
		}
	}
	jobs := make(chan string, len(groups))
	for node := range groups {
		jobs <- node
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(4, len(groups)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for node := range jobs {
				if ctx.Err() != nil {
					return
				}
				response, err := fetch(ctx, node)
				if err != nil || response == nil || response.Code != zlm.Success {
					continue
				}
				streams := make(map[liveStreamKey]bool, len(response.Data))
				for _, stream := range response.Data {
					streams[liveStreamKey{stream.App, stream.Stream}] = true
				}
				// 各节点对应互不重叠的下标；不按观看人数或历史会话判断。
				for _, i := range groups[node] {
					out[i].StreamStatus = "idle"
					if streams[keys[i].liveStreamKey] {
						out[i].StreamStatus = "streaming"
					}
				}
			}
		}()
	}
	workers.Wait()
	return out
}
