package onvifadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gowvp/onvif"
	imaging "github.com/gowvp/onvif/Imaging"
	devicemodel "github.com/gowvp/onvif/device"
	m "github.com/gowvp/onvif/media"
	p "github.com/gowvp/onvif/ptz"
	sdk "github.com/gowvp/onvif/sdk"
	sdkdevice "github.com/gowvp/onvif/sdk/device"
	sdkmedia "github.com/gowvp/onvif/sdk/media"
	sdkptz "github.com/gowvp/onvif/sdk/ptz"
	"github.com/gowvp/onvif/xsd"
	xsdonvif "github.com/gowvp/onvif/xsd/onvif"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
)

var _ ipc.Protocoler = (*Adapter)(nil)

// Adapter ONVIF 协议适配器
//
// 设计说明:
// - 适配器实现 ipc.Protocol 接口（Port 在 ipc 包内）
// - 适配器直接依赖领域模型 (ipc.Device, ipc.Channel)
// - 适配器依赖 ipc.Adapter 来访问存储和通用功能
// - 这符合清晰架构: 外层（适配器）依赖内层（领域）
type Adapter struct {
	devices conc.Map[string, *Device] // ONVIF 设备连接缓存
	adapter ipc.Adapter               // 通用适配器，提供 SaveChannels 等方法
	client  *http.Client
	sms     sms.Core
}

// Device ONVIF 设备包装（内存状态 + ONVIF 连接）
type Device struct {
	*onvif.Device
	KeepaliveAt orm.Time // 最后心跳时间
	IsOnline    bool     // 在线状态（内存缓存）
}

// DeleteDevice implements ipc.Protocoler.
func (a *Adapter) DeleteDevice(ctx context.Context, device *ipc.Device) error {
	a.devices.Delete(device.ID)
	return nil
}

func NewAdapter(adapter ipc.Adapter, sms sms.Core) *Adapter {
	cli := *http.DefaultClient
	cli.Timeout = time.Millisecond * 3000
	a := Adapter{
		adapter: adapter,
		client:  &cli,
		sms:     sms,
	}
	a.init()

	// 启动健康检查
	go a.startHealthCheck(context.Background())

	return &a
}

func (a *Adapter) init() {
	devices, err := a.adapter.FindDevices(context.TODO())
	if err != nil {
		panic(err)
	}
	for _, device := range devices {
		if device.IsOnvif() {
			go func(device *ipc.Device) {
				onvifDev, err := onvif.NewDevice(onvif.DeviceParams{
					Xaddr:      fmt.Sprintf("%s:%d", device.IP, device.Port),
					Username:   device.GetUsername(),
					Password:   device.Password,
					HttpClient: a.client,
				})
				if err != nil {
					_ = a.adapter.Edit(device.ID, func(d *ipc.Device) {
						d.IsOnline = false
					})
					slog.Error("初始化 ONVIF 设备失败", "err", err, "device_id", device.ID)
				}
				if onvifDev == nil {
					return
				}
				a.devices.Store(device.ID, &Device{
					Device:   onvifDev,
					IsOnline: err == nil,
				})
			}(device)
		}
	}
}

// ValidateDevice 实现 ipc.Protocol 接口 - ONVIF 设备验证
func (a *Adapter) ValidateDevice(ctx context.Context, dev *ipc.Device) error {
	onvifDev, err := onvif.NewDevice(onvif.DeviceParams{
		Xaddr:      fmt.Sprintf("%s:%d", dev.IP, dev.Port),
		Username:   dev.GetUsername(),
		Password:   dev.Password,
		HttpClient: a.client,
	})
	if err != nil {
		return fmt.Errorf("IP 或 PORT 错误: %w", err)
	}

	// 获取设备信息并填充到领域模型
	resp, err := sdkdevice.Call_GetDeviceInformation(ctx, onvifDev, devicemodel.GetDeviceInformation{})
	if err != nil {
		return fmt.Errorf("账号或密码错误: %w", err)
	}
	dev.Transport = "tcp"
	dev.Ext.Firmware = resp.FirmwareVersion
	dev.Ext.Manufacturer = resp.Manufacturer
	dev.Ext.Model = resp.Model
	dev.IsOnline = true
	dev.Address = fmt.Sprintf("%s:%d", dev.IP, dev.Port)
	return nil
}

// InitDevice 实现 ipc.Protocol 接口 - 初始化 ONVIF 设备
// ONVIF 设备初始化时，自动查询 Profiles 并创建为通道
func (a *Adapter) InitDevice(ctx context.Context, dev *ipc.Device) error {
	// 创建 ONVIF 连接
	onvifDev, err := onvif.NewDevice(onvif.DeviceParams{
		Xaddr:      fmt.Sprintf("%s:%d", dev.IP, dev.Port),
		Username:   dev.GetUsername(),
		Password:   dev.Password,
		HttpClient: a.client,
	})
	if err != nil {
		return err
	}

	// 缓存设备连接
	d := Device{
		Device:   onvifDev,
		IsOnline: true,
	}
	a.devices.Store(dev.ID, &d)

	// 自动查询 Profiles 作为通道
	return a.queryAndSaveProfiles(ctx, dev, &d)
}

// QueryCatalog 实现 ipc.Protocol 接口 - ONVIF 查询 Profiles
func (a *Adapter) QueryCatalog(ctx context.Context, dev *ipc.Device) error {
	onvifDev, ok := a.devices.Load(dev.ID)
	if !ok {
		// 设备连接不在缓存中，尝试重新连接
		var err error
		d, err := onvif.NewDevice(onvif.DeviceParams{
			Xaddr:    fmt.Sprintf("%s:%d", dev.IP, dev.Port),
			Username: dev.GetUsername(),
			Password: dev.Password,
		})
		if err != nil {
			return fmt.Errorf("ONVIF 设备未初始化: %w", err)
		}
		onvifDev = &Device{
			Device:   d,
			IsOnline: true,
		}
		a.devices.Store(dev.ID, onvifDev)
	}

	return a.queryAndSaveProfiles(ctx, dev, onvifDev)
}

// StartPlay 实现 ipc.Protocol 接口 - ONVIF 播放
func (a *Adapter) StartPlay(ctx context.Context, dev *ipc.Device, ch *ipc.Channel) (*ipc.PlayResponse, error) {
	onvifDev, ok := a.devices.Load(dev.ID)
	if !ok {
		return nil, fmt.Errorf("ONVIF 设备未初始化")
	}

	// 获取 RTSP 地址
	streamURI, err := a.getStreamURI(ctx, onvifDev, ch.ChannelID)
	if err != nil {
		return nil, err
	}

	return &ipc.PlayResponse{
		RTSP: streamURI,
	}, nil
}

// StopPlay 实现 ipc.Protocol 接口 - ONVIF 停止播放
func (a *Adapter) StopPlay(ctx context.Context, dev *ipc.Device, ch *ipc.Channel) error {
	// ONVIF 通常不需要显式停止播放
	return nil
}

// queryAndSaveProfiles 查询 ONVIF Profiles 并保存为通道
//
// 使用统一的 SaveChannels 方法，自动处理增量更新和删除
func (a *Adapter) queryAndSaveProfiles(ctx context.Context, device *ipc.Device, onvifDev *Device) error {
	resp, err := sdkmedia.Call_GetProfiles(ctx, onvifDev.Device, m.GetProfiles{})
	if err != nil {
		return fmt.Errorf("账号或密码错误: %w", err)
	}

	// 将 Profiles 转换为通道列表
	channels := make([]*ipc.Channel, 0, len(resp.Profiles))
	for _, profile := range resp.Profiles {
		channel := &ipc.Channel{
			DeviceID:  device.ID,
			ChannelID: string(profile.Token),
			Name:      string(profile.Name),
			DID:       device.ID,
			IsOnline:  true,
			Type:      ipc.TypeOnvif,
		}
		channels = append(channels, channel)
	}
	if len(channels) == 0 {
		return fmt.Errorf("没有找到 ONVIF 通道")
	}

	// 使用统一的 SaveChannels 方法保存（自动处理增删改）
	if err := a.adapter.SaveChannels(channels); err != nil {
		return fmt.Errorf("保存 ONVIF 通道失败: %w", err)
	}

	slog.InfoContext(ctx, "ONVIF Profiles 同步完成",
		"device_id", device.ID,
		"profile_count", len(channels))

	return nil
}

// getStreamURI 获取 RTSP 流地址
func (a *Adapter) getStreamURI(ctx context.Context, dev *Device, profileToken string) (string, error) {
	var param m.GetStreamUri
	param.StreamSetup.Transport.Protocol = "RTSP"
	param.StreamSetup.Stream = "RTP-Unicast"
	param.ProfileToken = xsdonvif.ReferenceToken(profileToken)
	resp, err := sdkmedia.Call_GetStreamUri(ctx, dev.Device, param)
	if err != nil {
		return "", err
	}
	playURL := buildPlayURL(string(resp.MediaUri.Uri), dev.Device.GetDeviceParams().Username, dev.Device.GetDeviceParams().Password)
	return playURL, nil
}

func buildPlayURL(rawurl, username, password string) string {
	if username != "" && password != "" {
		return strings.Replace(rawurl, "rtsp://", fmt.Sprintf("rtsp://%s:%s@", username, password), 1)
	}
	return rawurl
}

func (a *Adapter) PTZControl(ctx context.Context, dev *ipc.Device, ch *ipc.Channel, in *ipc.PTZControlInput) error {
	onvifDev, ok := a.devices.Load(dev.ID)
	if !ok {
		return fmt.Errorf("ONVIF 设备未初始化")
	}
	action := strings.ToLower(strings.TrimSpace(in.Action))
	if in.Speed == 0 {
		in.Speed = 40
	}
	if in.Timeout <= 0 {
		in.Timeout = 6
	}
	profileToken := xsdonvif.ReferenceToken(ch.ChannelID)

	switch action {
	case "preset_set", "set_preset":
		if in.Preset < 1 || in.Preset > 255 {
			return fmt.Errorf("preset must be in [1,255]")
		}
		_, err := sdkptz.Call_SetPreset(ctx, onvifDev.Device, p.SetPreset{
			ProfileToken: profileToken,
			PresetName:   xsd.String(fmt.Sprintf("Preset-%d", in.Preset)),
			PresetToken:  xsdonvif.ReferenceToken(fmt.Sprintf("%d", in.Preset)),
		})
		return err
	case "preset_call", "goto_preset":
		if in.Preset < 1 || in.Preset > 255 {
			return fmt.Errorf("preset must be in [1,255]")
		}
		_, err := sdkptz.Call_GotoPreset(ctx, onvifDev.Device, p.GotoPreset{
			ProfileToken: profileToken,
			PresetToken:  xsdonvif.ReferenceToken(fmt.Sprintf("%d", in.Preset)),
		})
		return err
	case "preset_delete", "del_preset", "remove_preset":
		if in.Preset < 1 || in.Preset > 255 {
			return fmt.Errorf("preset must be in [1,255]")
		}
		_, err := sdkptz.Call_RemovePreset(ctx, onvifDev.Device, p.RemovePreset{
			ProfileToken: profileToken,
			PresetToken:  xsdonvif.ReferenceToken(fmt.Sprintf("%d", in.Preset)),
		})
		return err
	case "focus_add", "focus_near", "focus_plus":
		return a.imagingFocusMove(ctx, onvifDev.Device, profileToken, float64(in.Speed)/255.0)
	case "focus_sub", "focus_far", "focus_minus":
		return a.imagingFocusMove(ctx, onvifDev.Device, profileToken, -float64(in.Speed)/255.0)
	case "iris_add", "iris_open", "aperture_add":
		return a.imagingIrisAdjust(ctx, onvifDev.Device, profileToken, float64(in.Speed)/255.0)
	case "iris_sub", "iris_close", "aperture_sub":
		return a.imagingIrisAdjust(ctx, onvifDev.Device, profileToken, -float64(in.Speed)/255.0)
	}

	if action == "stop" {
		_, err := sdkptz.Call_Stop(ctx, onvifDev.Device, p.Stop{
			ProfileToken: profileToken,
			PanTilt:      true,
			Zoom:         true,
		})
		return err
	}

	normalizedSpeed := math.Min(1.0, math.Max(0.0, float64(in.Speed)/255.0))
	cmd := p.ContinuousMove{
		ProfileToken: profileToken,
		Velocity: xsdonvif.PTZSpeed{
			PanTilt: xsdonvif.Vector2D{},
			Zoom:    xsdonvif.Vector1D{},
		},
		Timeout: xsd.Duration("PT1S"),
	}

	switch action {
	case "left":
		cmd.Velocity.PanTilt.X = -normalizedSpeed
	case "right":
		cmd.Velocity.PanTilt.X = normalizedSpeed
	case "up":
		cmd.Velocity.PanTilt.Y = normalizedSpeed
	case "down":
		cmd.Velocity.PanTilt.Y = -normalizedSpeed
	case "left_up":
		cmd.Velocity.PanTilt.X = -normalizedSpeed
		cmd.Velocity.PanTilt.Y = normalizedSpeed
	case "left_down":
		cmd.Velocity.PanTilt.X = -normalizedSpeed
		cmd.Velocity.PanTilt.Y = -normalizedSpeed
	case "right_up":
		cmd.Velocity.PanTilt.X = normalizedSpeed
		cmd.Velocity.PanTilt.Y = normalizedSpeed
	case "right_down":
		cmd.Velocity.PanTilt.X = normalizedSpeed
		cmd.Velocity.PanTilt.Y = -normalizedSpeed
	case "zoom_in":
		cmd.Velocity.Zoom.X = normalizedSpeed
	case "zoom_out":
		cmd.Velocity.Zoom.X = -normalizedSpeed
	default:
		return fmt.Errorf("unsupported ptz action: %s", action)
	}

	_, err := sdkptz.Call_ContinuousMove(ctx, onvifDev.Device, cmd)
	return err
}

func (a *Adapter) imagingFocusMove(ctx context.Context, dev *onvif.Device, profileToken xsdonvif.ReferenceToken, speed float64) error {
	token, err := a.getVideoSourceToken(ctx, dev, profileToken)
	if err != nil {
		return err
	}
	req := imaging.Move{
		VideoSourceToken: token,
		Focus: xsdonvif.FocusMove{
			Continuous: xsdonvif.ContinuousFocus{Speed: xsd.Float(speed)},
		},
	}

	httpReply, err := dev.CallMethod(req)
	if err != nil {
		return err
	}
	type envelope struct {
		Header struct{}
		Body   struct {
			MoveResponse struct{} `xml:"MoveResponse"`
		}
	}
	var reply envelope
	return sdk.ReadAndParse(ctx, httpReply, &reply, "Move")
}

func (a *Adapter) imagingIrisAdjust(ctx context.Context, dev *onvif.Device, profileToken xsdonvif.ReferenceToken, delta float64) error {
	// 通过成像参数接口调节光圈，delta>0 放大，delta<0 缩小。
	token, err := a.getVideoSourceToken(ctx, dev, profileToken)
	if err != nil {
		return err
	}
	settings, err := a.getImagingSettings(ctx, dev, token)
	if err != nil {
		return err
	}
	if settings.Exposure == nil {
		settings.Exposure = &xsdonvif.Exposure20{}
	}

	// 光圈值通常为 0~1 区间，按速度做增量调整。
	step := math.Max(0.02, math.Min(0.3, math.Abs(delta)))
	next := settings.Exposure.Iris
	if delta >= 0 {
		next += step
	} else {
		next -= step
	}
	if next < 0 {
		next = 0
	}
	if next > 1 {
		next = 1
	}

	settings.Exposure.Mode = xsdonvif.ExposureMode("MANUAL")
	settings.Exposure.Iris = next
	return a.setImagingSettings(ctx, dev, token, settings)
}

// getImagingSettings 获取当前视频源成像参数（用于光圈增减前读取基线值）。
func (a *Adapter) getImagingSettings(ctx context.Context, dev *onvif.Device, token xsdonvif.ReferenceToken) (xsdonvif.ImagingSettings20, error) {
	httpReply, err := dev.CallMethod(imaging.GetImagingSettings{VideoSourceToken: token})
	if err != nil {
		return xsdonvif.ImagingSettings20{}, err
	}
	type envelope struct {
		Header struct{}
		Body   struct {
			GetImagingSettingsResponse struct {
				ImagingSettings xsdonvif.ImagingSettings20 `xml:"ImagingSettings"`
			} `xml:"GetImagingSettingsResponse"`
		}
	}
	var reply envelope
	if err := sdk.ReadAndParse(ctx, httpReply, &reply, "GetImagingSettings"); err != nil {
		return xsdonvif.ImagingSettings20{}, err
	}
	return reply.Body.GetImagingSettingsResponse.ImagingSettings, nil
}

// setImagingSettings 写回成像参数（光圈、曝光等）。
func (a *Adapter) setImagingSettings(ctx context.Context, dev *onvif.Device, token xsdonvif.ReferenceToken, settings xsdonvif.ImagingSettings20) error {
	httpReply, err := dev.CallMethod(imaging.SetImagingSettings{
		VideoSourceToken: token,
		ImagingSettings:  settings,
		ForcePersistence: xsd.Boolean(true),
	})
	if err != nil {
		return err
	}
	type envelope struct {
		Header struct{}
		Body   struct {
			SetImagingSettingsResponse struct{} `xml:"SetImagingSettingsResponse"`
		}
	}
	var reply envelope
	return sdk.ReadAndParse(ctx, httpReply, &reply, "SetImagingSettings")
}

// getVideoSourceToken 通过 ProfileToken 解析视频源 token。
func (a *Adapter) getVideoSourceToken(ctx context.Context, dev *onvif.Device, profileToken xsdonvif.ReferenceToken) (xsdonvif.ReferenceToken, error) {
	resp, err := sdkmedia.Call_GetProfile(ctx, dev, m.GetProfile{ProfileToken: profileToken})
	if err != nil {
		return "", err
	}
	if resp.Profile.VideoSourceConfiguration.SourceToken == "" {
		return "", fmt.Errorf("video source token is empty")
	}
	return resp.Profile.VideoSourceConfiguration.SourceToken, nil
}

func (a *Adapter) Discover(ctx context.Context, w io.Writer) error {
	recv, err := onvif.AllAvailableDevicesAtSpecificEthernetInterfaces()
	if err != nil {
		return err
	}

	for {
		select {
		case dev, ok := <-recv:
			if !ok {
				return nil
			}
			var exists bool
			a.devices.Range(func(key string, value *Device) bool {
				if value.GetDeviceParams().Xaddr == dev.GetDeviceParams().Xaddr {
					exists = true
					return false
				}
				return true
			})
			if exists {
				continue
			}
			b, _ := json.Marshal(toDiscoverResponse(dev))
			_, _ = w.Write(b)
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
			slog.DebugContext(ctx, "discover timeout")
			return nil
		}
	}
}
