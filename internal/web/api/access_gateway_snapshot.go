package api

import (
	"context"
	"fmt"
	"github.com/google/uuid"
)

// 网关任务使用原抓拍权限、图片校验、落盘与计数，不在 B 建立另一套业务状态。
func (a IPCAPI) persistGatewaySnapshot(ctx context.Context, device, cover, session string, payload []byte) error {
	if a.uc == nil || a.uc.SipServer == nil {
		return fmt.Errorf("抓拍服务未就绪")
	}
	if err := a.uc.SipServer.ValidateSnapshotUploadContext(ctx, device, cover, session); err != nil {
		return err
	}
	if err := validateLegacySnapshotUpload(payload, cover); err != nil {
		return err
	}
	if _, err := uuid.Parse(cover); err == nil {
		if a.captures == nil {
			return fmt.Errorf("抓拍服务未就绪")
		}
		row, err := a.captures.Get(ctx, cover)
		if err != nil {
			return err
		}
		ch, err := a.ipc.GetChannel(ctx, row.ChannelID)
		if err != nil {
			return err
		}
		if ch.DeviceID != device {
			return fmt.Errorf("抓拍设备已改变")
		}
		if err = a.captures.SetSession(ctx, cover, session); err != nil {
			return err
		}
		if _, err = a.captures.Complete(ctx, cover, payload, "gb28181_device_upload"); err != nil {
			return err
		}
	} else if err := writeCover(a.uc.Conf.ConfigDir, cover, payload); err != nil {
		return err
	}
	return a.uc.SipServer.CommitSnapshotUpload(device, session)
}
