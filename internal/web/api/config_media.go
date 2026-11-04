package api

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/reason"
)

type mediaPlaybackConfig struct {
	StreamNoneReaderDelaySeconds int `json:"stream_none_reader_delay_seconds" binding:"required,min=1,max=86400"`
}

func mediaNoneReaderDelay(media conf.Media) int {
	if media.StreamNoneReaderDelaySeconds > 0 {
		return media.StreamNoneReaderDelaySeconds
	}
	return 180
}

// updateMedia godoc
// @Summary 设置无人观看自动断流等待时间
// @Tags Config
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body mediaPlaybackConfig true "无人观看等待秒数，1–86400，默认180"
// @Success 200 {object} SwaggerMessageResponse
// @Router /configs/info/media [put]
func (a ConfigAPI) updateMedia(_ *gin.Context, in *mediaPlaybackConfig) (gin.H, error) {
	if a.conf == nil || a.uc == nil || a.uc.SMSAPI.smsCore.NodeManager == nil {
		return nil, reason.ErrServer.WithMsg("媒体配置不可用")
	}
	if in.StreamNoneReaderDelaySeconds < 1 || in.StreamNoneReaderDelaySeconds > 86400 {
		return nil, reason.ErrBadRequest.WithMsg("无人观看自动断流时间须为 1–86400 秒")
	}
	a.sipMu.Lock() // 与 SIP 配置写盘共用锁，避免并发覆盖配置文件。
	defer a.sipMu.Unlock()
	previous := *a.conf
	candidate := previous
	candidate.Media.StreamNoneReaderDelaySeconds = in.StreamNoneReaderDelaySeconds
	if err := conf.WriteConfig(&candidate, a.conf.ConfigPath); err != nil {
		return nil, reason.ErrServer.WithMsg(err.Error())
	}
	if err := a.uc.SMSAPI.smsCore.SetStreamNoneReaderDelay(in.StreamNoneReaderDelaySeconds); err != nil {
		if rollbackErr := conf.WriteConfig(&previous, a.conf.ConfigPath); rollbackErr != nil {
			return nil, reason.ErrServer.WithMsg(fmt.Sprintf("%v；回滚配置文件失败: %v", err, rollbackErr))
		}
		return nil, reason.ErrServer.WithMsg(err.Error())
	}
	a.conf.Media.StreamNoneReaderDelaySeconds = in.StreamNoneReaderDelaySeconds
	return gin.H{"msg": "ok"}, nil
}
