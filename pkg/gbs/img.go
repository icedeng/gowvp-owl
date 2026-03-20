package gbs

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/netpulse/ip"
)

func (g *GB28181API) QuerySnapshot(deviceID, targetID, coverKey string) error {
	slog.Debug("QuerySnapshot", "deviceID", deviceID)
	if err := g.requireGBVersionAtLeast(deviceID, gbVersion2022, "图像抓拍(9.14)"); err != nil {
		return err
	}
	ipc, ok := g.svr.memoryStorer.Load(deviceID)
	if !ok {
		return ErrDeviceOffline
	}

	sn := int32(g.nextControlSN())
	sessionID := sip.RandString(32)
	body := NewDeviceConfig(targetID).SetSN(sn).SetSnapShotConfig(&SnapShot{
		SnapNum:   1,
		Interval:  1,
		UploadURL: g.buildSnapshotUploadURL(deviceID, coverKey, sessionID),
		SessionID: sessionID,
	}).Marshal()

	waitKey := buildPendingDeviceConfigKey(deviceID, int(sn))
	pending := &pendingDeviceConfig{wait: make(chan *DeviceConfigResponse, 1)}
	g.pendingDeviceConfig.Store(waitKey, pending)
	defer g.pendingDeviceConfig.Delete(waitKey)

	tx, err := g.svr.wrapRequest(ipc, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		return err
	}
	if _, err = sipResponse(tx); err != nil {
		return err
	}

	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	select {
	case resp := <-pending.wait:
		if strings.ToUpper(strings.TrimSpace(resp.Result)) == "OK" || strings.TrimSpace(resp.Result) == "" {
			return nil
		}
		return fmt.Errorf("snapshot config failed: %s", resp.Result)
	case <-timer.C:
		return fmt.Errorf("wait snapshot response timeout")
	}
}

// buildSnapshotUploadURL 生成抓拍回传地址，避免硬编码固定地址。
// 通过路径参数携带 device/coverKey/session，兼容部分设备不接受 query 参数的场景。
func (g *GB28181API) buildSnapshotUploadURL(deviceID, coverKey, sessionID string) string {
	path := fmt.Sprintf("/gb28181/snapshot/%s/%s/%s", deviceID, coverKey, sessionID)
	if g.boot != nil {
		baseURL := strings.TrimSpace(g.boot.Media.GBSnapshotBaseURL)
		if baseURL != "" {
			return strings.TrimRight(baseURL, "/") + path
		}
		host := strings.TrimSpace(g.boot.Media.WebHookIP)
		if host == "" {
			host = ip.InternalIP()
		}
		port := g.boot.Server.HTTP.Port
		if port <= 0 {
			port = 15123
		}
		if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
			return strings.TrimRight(host, "/") + path
		}
		return fmt.Sprintf("http://%s:%d%s", host, port, path)
	}
	return "http://127.0.0.1:15123" + path
}
