package api

import (
	"net"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs"
)

// historyPlaybackOutput 仅为已成功建立的历史会话签发播放地址，不调用直播接口。
func (a IPCAPI) historyPlaybackOutput(c *gin.Context, channelID string) (*playOutput, error) {
	ch, err := a.ipc.GetChannel(c.Request.Context(), channelID)
	if err != nil {
		return nil, err
	}
	mediaServerID := channelMediaServerID(ch)
	svr, err := a.uc.SMSAPI.smsCore.GetMediaServer(c.Request.Context(), mediaServerID)
	if err != nil {
		return nil, err
	}
	stream := gbs.HistoryStreamID("playback", ch.ID)
	token, err := a.uc.newPlayToken(c, map[string]any{"stream": stream, "app": "rtp", "media_server_id": mediaServerID})
	if err != nil {
		return nil, err
	}
	host := c.Request.Host
	if value, _, splitErr := net.SplitHostPort(host); splitErr == nil {
		host = value
	}
	prefix := c.Request.Header.Get("X-Forwarded-Prefix")
	if prefix == "" {
		prefix = "http://" + net.JoinHostPort(host, strconv.Itoa(a.uc.Conf.Server.HTTP.Port))
	}
	if forwarded := strings.TrimSpace(c.Request.Header.Get("X-Forwarded-Host")); forwarded != "" {
		host = forwarded
	}
	address := a.uc.SMSAPI.smsCore.GetStreamLiveAddr(svr, prefix, host, "rtp", stream, token)
	return &playOutput{App: "rtp", Stream: stream, Items: []sms.StreamLiveAddr{address}}, nil
}
