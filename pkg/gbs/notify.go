package gbs

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	// NotifyMethodUserActive 设备活跃状态通知
	NotifyMethodDevicesActive = "devices.active"
	// NotifyMethodUserRegister 设备注册通知
	NotifyMethodDevicesRegister = "devices.regiester"
	// NotifyMethodDeviceActive 通道活跃通知
	NotifyMethodChannelsActive = "channels.active"
	// NotifyMethodRecordStop 视频录制结束
	NotifyMethodRecordStop = "records.stop"
	// NotifyMethodAlarm 报警事件通知
	NotifyMethodAlarm = "events.alarm"
)

// Notify 消息通知结构
type Notify struct {
	Method string `json:"method"`
	Data   any    `json:"data"`
}

func notify(data *Notify) {
	if url, ok := config.NotifyMap[data.Method]; ok {
		res, err := sip.PostJSONRequest(url, data)
		if err != nil {
			// logrus.Warningln(data.Method, "send notify fail.", err)
		}
		if strings.ToUpper(string(res)) != "OK" {
			// logrus.Warningln(data.Method, "send notify resp fail.", string(res), "len:", len(res), config.Notify, data)
		} else {
			// logrus.Debug("notify send succ:", data.Method, data.Data)
		}
	} else {
		// logrus.Traceln("notify config not found", data.Method)
	}
}

func notifyDevicesAcitve(id, status string) *Notify {
	return &Notify{
		Method: NotifyMethodDevicesActive,
		Data: map[string]any{
			"deviceid": id,
			"status":   status,
			"time":     time.Now().Unix(),
		},
	}
}

func notifyDevicesRegister(u Devices) *Notify {
	u.Sys = *config.GB28181
	return &Notify{
		Method: NotifyMethodDevicesRegister,
		Data:   u,
	}
}

func notifyChannelsActive(d Channels) *Notify {
	return &Notify{
		Method: NotifyMethodChannelsActive,
		Data: map[string]any{
			"channelid": d.ChannelID,
			"status":    d.Status,
			"time":      time.Now().Unix(),
		},
	}
}

func notifyRecordStop(url string, req url.Values) *Notify {
	d := map[string]any{
		"url": fmt.Sprintf("%s/%s", config.Media.HTTP, url),
	}
	for k, v := range req {
		d[k] = v[0]
	}
	return &Notify{
		Method: NotifyMethodRecordStop,
		Data:   d,
	}
}

func notifyAlarm(event *AlarmEvent) *Notify {
	// 统一报警通知事件，便于外部系统订阅。
	return &Notify{
		Method: NotifyMethodAlarm,
		Data:   event,
	}
}
