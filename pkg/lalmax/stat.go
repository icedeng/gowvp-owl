package lalmax

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

const apiStatGroup = "/api/stat/group"

type StatSession struct {
	SessionID         string `json:"session_id"`
	Protocol          string `json:"protocol"`
	BaseType          string `json:"base_type"`
	RemoteAddr        string `json:"remote_addr"`
	StartTime         string `json:"start_time"`
	ReadBytesSum      uint64 `json:"read_bytes_sum"`
	WroteBytesSum     uint64 `json:"wrote_bytes_sum"`
	BitrateKbits      int    `json:"bitrate_kbits"`
	ReadBitrateKbits  int    `json:"read_bitrate_kbits"`
	WriteBitrateKbits int    `json:"write_bitrate_kbits"`
}

type StatRecordPerSecond struct {
	UnixSecond int64  `json:"unix_sec"`
	Value      uint32 `json:"v"`
}

type StatGroup struct {
	StreamName  string                `json:"stream_name"`
	AppName     string                `json:"app_name"`
	AudioCodec  string                `json:"audio_codec"`
	VideoCodec  string                `json:"video_codec"`
	VideoWidth  int                   `json:"video_width"`
	VideoHeight int                   `json:"video_height"`
	Pub         StatSession           `json:"pub"`
	Subs        []StatSession         `json:"subs"`
	Pull        StatSession           `json:"pull"`
	FPS         []StatRecordPerSecond `json:"in_frame_per_sec"`
}

type ApiStatGroupResp struct {
	CommonResp
	Data *StatGroup `json:"data"`
}

// ApiStatGroup 查询 LALMAX 聚合后的单流状态。
func (e *Engine) ApiStatGroup(ctx context.Context, appName, streamName string) (*ApiStatGroupResp, error) {
	streamName = strings.TrimSpace(streamName)
	if streamName == "" {
		return nil, fmt.Errorf("lalmax: stream_name is required")
	}
	query := url.Values{"stream_name": []string{streamName}}
	if appName = strings.TrimSpace(appName); appName != "" {
		query.Set("app_name", appName)
	}
	var resp ApiStatGroupResp
	if err := e.get(ctx, apiStatGroup+"?"+query.Encode(), &resp); err != nil {
		return nil, err
	}
	if err := resp.Err(); err != nil {
		return nil, err
	}
	if resp.Data == nil {
		return nil, fmt.Errorf("lalmax: empty stream status")
	}
	return &resp, nil
}
