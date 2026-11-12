package onvifadapter

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gowvp/onvif"
	devicemodel "github.com/gowvp/onvif/device"
	media "github.com/gowvp/onvif/media"
	sdkdevice "github.com/gowvp/onvif/sdk/device"
	sdkmedia "github.com/gowvp/onvif/sdk/media"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/ixugo/goddd/pkg/orm"
)

var (
	_ ipc.DeviceDeleteLocker    = (*Adapter)(nil)
	_ ipc.DeviceEditCoordinator = (*Adapter)(nil)
	_ ipc.DeviceEditInitializer = (*Adapter)(nil)
)

type deviceConfig struct{ address, username, password string }

func configFor(device *ipc.Device) deviceConfig {
	return deviceConfig{net.JoinHostPort(device.IP, strconv.Itoa(device.Port)), device.GetUsername(), device.Password}
}

func (a *Adapter) deviceLock(id string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return &a.deviceLocks[h.Sum32()%uint32(len(a.deviceLocks))]
}

func (a *Adapter) lockDevice(id string) func() {
	mu := a.deviceLock(id)
	mu.Lock()
	return mu.Unlock
}

func (a *Adapter) LockDeviceDelete(device *ipc.Device) func() { return a.lockDevice(device.ID) }
func (a *Adapter) LockDeviceEdit(device *ipc.Device) func()   { return a.lockDevice(device.ID) }

// DeviceEdited 只在凭据事务提交后废弃旧连接；事务失败保留原连接。
func (a *Adapter) DeviceEdited(_ context.Context, before, after *ipc.Device) error {
	if configFor(before) != configFor(after) {
		a.devices.Delete(after.ID)
	}
	return nil
}

// loadDeviceLocked 读取当前持久配置，异步请求携带的旧对象不能重建已删除设备或恢复旧凭据。
func (a *Adapter) loadDeviceLocked(ctx context.Context, id string) (*ipc.Device, *Device, error) {
	var current ipc.Device
	if err := a.adapter.Store().Device().Get(ctx, &current, orm.Where("id=?", id)); err != nil {
		if orm.IsErrRecordNotFound(err) {
			a.devices.Delete(id)
		}
		return nil, nil, err
	}
	if !current.IsOnvif() {
		return nil, nil, fmt.Errorf("device is not ONVIF")
	}
	config := configFor(&current)
	runtime, ok := a.devices.Load(id)
	if !ok || runtime.config != config {
		runtime = &Device{config: config, IsOnline: current.IsOnline, profilesPending: true}
		a.devices.Store(id, runtime)
	}
	return &current, runtime, nil
}

func (a *Adapter) connect(params onvif.DeviceParams) (*onvif.Device, error) {
	if a.connectFn != nil {
		return a.connectFn(params)
	}
	return onvif.NewDevice(params)
}
func (a *Adapter) deviceInfo(ctx context.Context, device *onvif.Device) (devicemodel.GetDeviceInformationResponse, error) {
	if a.infoFn != nil {
		return a.infoFn(ctx, device)
	}
	return sdkdevice.Call_GetDeviceInformation(ctx, device, devicemodel.GetDeviceInformation{})
}
func (a *Adapter) getProfiles(ctx context.Context, device *onvif.Device) (media.GetProfilesResponse, error) {
	if a.profilesFn != nil {
		return a.profilesFn(ctx, device)
	}
	return sdkmedia.Call_GetProfiles(ctx, device, media.GetProfiles{})
}

func (a *Adapter) initDeviceLocked(ctx context.Context, id string, forceProfiles bool) error {
	current, runtime, err := a.loadDeviceLocked(ctx, id)
	if err != nil {
		return err
	}
	if forceProfiles {
		runtime.profilesPending = true
	}
	return a.refreshDeviceLocked(ctx, current, runtime)
}

func (a *Adapter) refreshDeviceLocked(ctx context.Context, current *ipc.Device, runtime *Device) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if runtime.Device == nil {
		connection, err := a.connect(onvif.DeviceParams{Xaddr: runtime.config.address, Username: runtime.config.username, Password: runtime.config.password, HttpClient: a.client})
		if err != nil {
			// 不缓存 SDK 返回的半初始化连接，下轮重新发现服务端点。
			return err
		}
		runtime.Device = connection
	}
	if _, err := a.deviceInfo(ctx, runtime.Device); err != nil {
		if time.Since(runtime.KeepaliveAt.Time) >= heartbeatTimeout {
			runtime.Device = nil
			runtime.profilesPending = true
		}
		return err
	}
	runtime.KeepaliveAt = orm.Now()
	if runtime.profilesPending {
		return a.queryAndSaveProfiles(ctx, current, runtime)
	}
	return nil
}

// SDK 部分调用未透传 Context，这里将所有 HTTP 请求绑定宿主取消信号。
// 保留 Client.Timeout 和请求自身取消，响应体读完或关闭前不提前取消。
type lifecycleTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t lifecycleTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(request.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	context.AfterFunc(ctx, func() { stop() })
	if t.ctx.Err() != nil {
		cancel()
	}
	response, err := t.base.RoundTrip(request.Clone(ctx))
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	response.Body = &cancelBody{ReadCloser: response.Body, cancel: func() { stop(); cancel() }}
	return response, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error { defer b.cancel(); return b.ReadCloser.Close() }
