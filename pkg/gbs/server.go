package gbs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/netpulse/ip"
)

// defaultSIPServer 仅供保留的旧版包级停止接口使用；核心流程均使用 Server 实例。
var defaultSIPServer atomic.Pointer[sip.Server]

type MemoryStorer interface {
	LoadOrStore(deviceID string, value *Device)
	LoadDeviceToMemory(conn sip.Connection) error         // 加载设备到内存
	RangeDevices(fn func(key string, value *Device) bool) // 遍历设备

	Change(deviceID string, changeFn func(*ipc.Device) error, changeFn2 func(*Device)) error // 登出设备

	Load(deviceID string) (*Device, bool)
	Store(deviceID string, value *Device)
	GetChannel(deviceID, channelID string) (*Channel, bool)

	// Change(deviceID string, changeFn func(*ipc.Device)) // 修改设备
}

type Server struct {
	*sip.Server
	gb           *GB28181API
	mediaService cascadeMediaServerResolver
	cascade      *CascadeManager

	fromAddress  sip.Address
	memoryStorer MemoryStorer
}

// RefreshDeviceVersion 将持久化的协议档案同步到在线设备会话。
// 设备编辑接口会调用该方法，使手动版本覆盖无需等待重启或重新注册。
func (s *Server) RefreshDeviceVersion(device *ipc.Device) {
	if s == nil || device == nil {
		return
	}
	version := deviceProtocolVersion(device.Ext)
	device.Ext.GBVersionCapabilities = effectiveCapabilityNames(version, device.Ext.GBDisabledCapabilities)
	if s.memoryStorer == nil {
		return
	}
	if current, ok := s.memoryStorer.Load(device.GetGB28181DeviceID()); ok {
		current.setGBProfile(version, device.Ext.GBDisabledCapabilities)
	}
}

// resolveHost 将配置的 Host 解析成可用于 SIP 头的地址。
// 保留域名解析失败时的原始值，避免因 DNS 临时异常阻断 INVITE 构造。
func resolveHost(host string) string {
	if host == "" {
		return ""
	}
	if net.ParseIP(host) != nil {
		return host
	}
	addrs, err := net.LookupHost(host)
	if err != nil || len(addrs) == 0 {
		slog.Warn("resolveHost failed, fallback to raw host", "host", host, "err", err)
		return host
	}
	return addrs[0]
}

func NewServer(cfg *conf.Bootstrap, store ipc.Adapter, sc sms.Core) (*Server, func(), error) {
	if cfg == nil {
		return nil, nil, fmt.Errorf("GB28181 configuration is unavailable")
	}
	if err := conf.ValidateSIPConfig(cfg.Sip); err != nil {
		return nil, nil, err
	}
	memoryStorer, ok := store.Store().(MemoryStorer)
	if !ok || memoryStorer == nil {
		return nil, nil, fmt.Errorf("GB28181 memory store is unavailable")
	}
	iip := ip.InternalIP()
	uri, err := sip.ParseSipURI(fmt.Sprintf("sip:%s@%s:%d", cfg.Sip.ID, iip, cfg.Sip.Port))
	if err != nil {
		return nil, nil, fmt.Errorf("build GB28181 server URI: %w", err)
	}
	from := sip.Address{
		DisplayName: sip.String{Str: "gowvp/owl"},
		URI:         &uri,
		Params:      sip.NewParams(),
	}

	sipTrafficLogger, err := sip.NewTrafficLogger(sip.TrafficLogConfig{
		Enabled:      cfg.Sip.Log.Enabled,
		Dir:          filepath.Join(cfg.ConfigDir, cfg.Sip.Log.Dir),
		MaxAge:       cfg.Sip.Log.MaxAge.Duration(),
		RotationTime: cfg.Sip.Log.RotationTime.Duration(),
		RotationSize: cfg.Sip.Log.RotationSize * 1024 * 1024,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("init SIP traffic logger: %w", err)
	}
	registerCertificateAuth, err := newRegisterCertificateAuthenticator(cfg.Sip.RegisterCertificateAuth)
	if err != nil {
		if sipTrafficLogger != nil {
			_ = sipTrafficLogger.Close()
		}
		return nil, nil, fmt.Errorf("init certificate REGISTER authentication: %w", err)
	}
	api := NewGB28181API(cfg, store, sc.NodeManager)
	api.registerCertificateAuth = registerCertificateAuth
	sipServer := sip.NewServer(&from)
	sipServer.Register(api.handlerRegister)
	msg := sipServer.Message(api.sipAccessControlMiddleware, api.sipCascadeMessageMiddleware)
	msg.Handle("Keepalive", api.sipMessageKeepalive)
	msg.Handle("Catalog", api.sipMessageCatalog)
	msg.Handle("DeviceInfo", api.sipMessageDeviceInfo)
	msg.Handle("ConfigDownload", api.sipMessageConfigDownload)
	msg.Handle("DeviceConfig", api.handleDeviceConfig)
	msg.Handle("DeviceControl", api.sipMessageDeviceControl)
	msg.Handle("RecordInfo", api.sipMessageRecordInfo)
	msg.Handle("MediaStatus", api.sipMessageMediaStatus)
	msg.Handle("DeviceUpgradeResult", api.sipMessageDeviceUpgradeResult)
	msg.Handle("UploadSnapShotFinished", api.sipMessageSnapshotFinished)
	msg.Handle("VideoUploadNotify", api.sipMessageVideoUploadNotify)

	// 报警既可能由 MESSAGE 上报，也可能由 NOTIFY 上报，二者均接入。
	notify := sipServer.Notify(api.sipAccessControlMiddleware, api.sipNotifySubscriptionState)
	notify.Handle("Alarm", api.sipNotifyAlarm)
	notify.Handle("Catalog", api.sipNotifyCatalog)
	notify.Handle("MobilePosition", api.sipNotifyMobilePosition)
	notify.Handle("PTZPosition", api.sipMessageQueryGeneric)
	notify.Handle("DeviceStatus", api.sipMessageQueryGeneric)
	notify.Handle("PresetQuery", api.sipMessageQueryGeneric)
	notify.Handle("PersetQuery", api.sipMessageQueryGeneric)
	notify.Handle("HomePositionQuery", api.sipMessageQueryGeneric)
	notify.Handle("CruiseTrackListQuery", api.sipMessageQueryGeneric)
	notify.Handle("CruiseTrackQuery", api.sipMessageQueryGeneric)
	notify.Handle("SDCardStatus", api.sipMessageQueryGeneric)
	notify.Handle("ConfigDownload", api.sipMessageQueryGeneric)
	notify.Handle("DeviceUpgradeResult", api.sipMessageDeviceUpgradeResult)
	notify.Handle("UploadSnapShotFinished", api.sipMessageSnapshotFinished)
	notify.Handle("VideoUploadNotify", api.sipMessageVideoUploadNotify)
	msg.Handle("Alarm", api.sipMessageAlarm)

	// 9.11 事件源侧：接收上级订阅请求（SUBSCRIBE）。
	sipServer.Subscribe(api.sipAccessControlMiddleware, api.sipSubscribeEvent)
	// 9.2 被叫侧会话兼容：接收入向 INVITE/BYE/ACK。
	sipServer.Handle(sip.MethodInvite, api.sipInviteGeneric)
	sipServer.Handle(sip.MethodCancel, api.sipCancelGeneric)
	sipServer.Handle(sip.MethodBYE, api.sipByeGeneric)
	sipServer.Handle(sip.MethodACK, api.sipAckGeneric)
	sipServer.Handle(sip.MethodInfo, api.sipInfoGeneric)
	// OPTIONS 探测（入向）兼容。
	sipServer.Handle(sip.MethodOptions, api.sipOptionsGeneric)

	// A.2.4 查询响应补齐：注册缺失查询命令响应处理。
	msg.Handle("DeviceStatus", api.sipMessageQueryGeneric)
	msg.Handle("PresetQuery", api.sipMessageQueryGeneric)
	msg.Handle("PersetQuery", api.sipMessageQueryGeneric)
	msg.Handle("HomePositionQuery", api.sipMessageQueryGeneric)
	msg.Handle("CruiseTrackListQuery", api.sipMessageQueryGeneric)
	msg.Handle("CruiseTrackQuery", api.sipMessageQueryGeneric)
	msg.Handle("PTZPosition", api.sipMessageQueryGeneric)
	msg.Handle("SDCardStatus", api.sipMessageQueryGeneric)
	msg.Handle("MobilePosition", api.sipMessageQueryGeneric)
	msg.Handle("Broadcast", api.sipMessageBroadcastResponse)

	c := Server{
		Server:       sipServer,
		mediaService: sc,
		fromAddress:  from,
		gb:           api,
		memoryStorer: memoryStorer,
	}
	api.svr = &c
	sipServer.SetRequestSecurityResolver(api.resolveSignalDigestSecurity)
	var (
		cleanupOnce     sync.Once
		loggerInstalled bool
	)
	cleanup := func() {
		cleanupOnce.Do(func() {
			c.Close()
			defaultSIPServer.CompareAndSwap(sipServer, nil)
			if loggerInstalled {
				if previous := sip.SetTrafficLogger(nil); previous != nil {
					_ = previous.Close()
				}
			} else if sipTrafficLogger != nil {
				_ = sipTrafficLogger.Close()
			}
		})
	}

	listenPlan := buildSIPListenPlan(cfg.Sip)
	listenAddr := fmt.Sprintf(":%d", cfg.Sip.Port)
	if err := sipServer.StartUDPServer(listenAddr); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("start SIP UDP listener: %w", err)
	}
	if listenPlan.PlainTCP {
		if err := sipServer.StartTCPServer(listenAddr); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("start SIP TCP listener: %w", err)
		}
	}
	if listenPlan.TLS {
		if err := sipServer.StartTLSServerWithOptions(fmt.Sprintf(":%d", listenPlan.TLSPort), sip.TLSListenerOptions{
			CertFile: cfg.Sip.TLSCert, KeyFile: cfg.Sip.TLSKey,
			ClientCAFile: cfg.Sip.TLSClientCA, RequireClientCert: cfg.Sip.TLSRequireClientCert,
		}); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("start SIP TLS listener: %w", err)
		}
	}
	previous := sip.SetTrafficLogger(sipTrafficLogger)
	loggerInstalled = true
	if previous != nil {
		_ = previous.Close()
	}
	go c.startTickerCheck()
	defaultSIPServer.Store(sipServer)
	if err := c.memoryStorer.LoadDeviceToMemory(sipServer.UDPConn()); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("load GB28181 devices into memory: %w", err)
	}
	c.cascade = NewCascadeManager(&c)
	if err := c.cascade.Apply(cfg.Sip, cfg.Sip.Upstreams); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("initialize GB28181 cascade upstreams: %w", err)
	}
	return &c, cleanup, nil
}

// Close 先注销并停止上级级联，再关闭底层 SIP 监听器。
func (s *Server) Close() {
	if s == nil {
		return
	}
	if s.gb != nil {
		s.gb.close()
	}
	if s.cascade != nil {
		s.cascade.Close()
	}
	if s.Server != nil {
		s.Server.Close()
	}
}

type sipListenPlan struct {
	PlainTCP bool
	TLS      bool
	TLSPort  int
}

func buildSIPListenPlan(cfg conf.SIP) sipListenPlan {
	plan := sipListenPlan{PlainTCP: true}
	if !cfg.EnableTLS {
		return plan
	}
	plan.TLS = true
	plan.TLSPort = cfg.TLSPort
	if plan.TLSPort <= 0 {
		plan.TLSPort = cfg.Port
	}
	// 同一 TCP 端口不能同时监听明文 SIP 和 TLS；TLS 占用主端口时保留 UDP、关闭明文 TCP。
	plan.PlainTCP = plan.TLSPort != cfg.Port
	return plan
}

// SetConfig 热更新不依赖监听器和平台身份重建的 SIP 配置。
func (s *Server) SetConfig(cfg conf.SIP) {
	if s == nil || s.gb == nil {
		return
	}
	s.gb.setConfig(cfg)
	s.gb.applyDirectTCPConfig(cfg.DirectTCPDownload)
	if s.gb.directDownloads != nil {
		s.gb.directDownloads.Reconfigure(directTCPDownloadOptions(cfg.DirectTCPDownload))
	}
	if s.cascade != nil {
		if err := s.cascade.Apply(cfg, cfg.Upstreams); err != nil {
			slog.Error("reload GB28181 cascade upstreams failed", "err", err)
		}
	}
}

// CascadeStatuses 返回本平台向所有已启用上级平台注册的运行状态。
func (s *Server) CascadeStatuses() []CascadePlatformStatus {
	if s == nil || s.cascade == nil {
		return []CascadePlatformStatus{}
	}
	return s.cascade.Statuses()
}

// ValidateCascadeConfig 在写入配置前校验所有已启用的上级平台参数。
func (s *Server) ValidateCascadeConfig(cfg conf.SIP) error {
	fallbackHost := ""
	if s != nil && s.fromAddress.URI != nil {
		fallbackHost = s.fromAddress.URI.Host()
	}
	_, err := normalizeCascadePlatforms(cfg, cfg.Upstreams, fallbackHost)
	return err
}

// startTickerCheck 定时检查离线，通过心跳超时判断设备是否离线
func (s *Server) startTickerCheck() {
	if s == nil || s.gb == nil || s.gb.lifecycleDone == nil {
		return
	}
	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.gb.lifecycleDone:
			return
		case <-timer.C:
		}
		s.checkOfflineDevices(time.Now())
		timer.Reset(time.Second)
	}
}

var errOfflineSnapshotStale = errors.New("device state changed during offline check")

func (s *Server) checkOfflineDevices(now time.Time) {
	if s == nil || s.gb == nil || s.memoryStorer == nil {
		return
	}
	s.memoryStorer.RangeDevices(func(key string, dev *Device) bool {
		state := dev.runtimeSnapshot()
		if !state.IsOnline {
			return true
		}
		if len(key) < 18 {
			return true
		}

		// 计算超时时间：心跳间隔 * 超时次数
		// 默认心跳间隔 60s，超时次数 3 次，即 3 分钟无心跳判定离线
		interval := state.KeepaliveInterval
		if interval == 0 {
			interval = 60
		}
		timeoutCount := state.KeepaliveTimeout
		if timeoutCount == 0 {
			timeoutCount = 3
		}
		timeout := time.Duration(interval) * time.Duration(timeoutCount) * time.Second

		// 跳过未收到过心跳的设备（LastKeepaliveAt 为零值），这类设备依赖注册超时处理
		if state.LastKeepaliveAt.IsZero() {
			// 如果注册时间也超过了超时时间，则判定离线
			if !state.LastRegisterAt.IsZero() && now.Sub(state.LastRegisterAt) >= timeout {
				if _, err := s.logoutDeviceIfCurrent(key, state); err != nil {
					slog.Error("logout device failed", "device_id", key, "err", err)
				}
			}
			return true
		}

		// 心跳超时或连接丢失，判定设备离线
		if sub := now.Sub(state.LastKeepaliveAt); sub >= timeout || state.Conn == nil {
			// 对 TCP/TLS 设备在离线判定前先做一次 OPTIONS 探测，避免瞬时抖动误判离线。
			if sub >= timeout && state.Conn != nil && state.Source != nil && state.Source.Network() != "udp" {
				if err := s.gb.ProbeOptions(context.Background(), &OptionsProbeInput{
					DeviceID: key,
					Timeout:  3 * time.Second,
				}); err == nil {
					return true
				}
			}
			changed, err := s.logoutDeviceIfCurrent(key, state)
			if err != nil {
				slog.Error("logout device failed", "device_id", key, "err", err)
			} else if changed {
				slog.Info("device offline detected",
					"device_id", key,
					"last_keepalive", state.LastKeepaliveAt,
					"timeout", timeout,
					"elapsed", sub,
					"conn_nil", state.Conn == nil,
				)
			}
		}
		return true
	})
}

func (s *Server) logoutDeviceIfCurrent(deviceID string, expected deviceRuntimeState) (bool, error) {
	err := s.gb.logout(deviceID, func(d *ipc.Device) error {
		if !d.IsOnline || d.Expires != expected.Expires ||
			!d.RegisteredAt.Time.Equal(expected.LastRegisterAt) ||
			!d.KeepaliveAt.Time.Equal(expected.LastKeepaliveAt) {
			return errOfflineSnapshotStale
		}
		d.IsOnline = false
		return nil
	})
	if errors.Is(err, errOfflineSnapshotStale) {
		return false, nil
	}
	return err == nil, err
}

// MODDEBUG MODDEBUG
var MODDEBUG = "DEBUG"

// ActiveDevices 记录当前活跃设备，请求播放时设备必须处于活跃状态
type ActiveDevices struct {
	sync.Map
}

// Get Get
func (a *ActiveDevices) Get(key string) (Devices, bool) {
	if v, ok := a.Load(key); ok {
		return v.(Devices), ok
	}
	return Devices{}, false
}

var _activeDevices ActiveDevices

// 系统运行信息
var (
	_sysinfo *m.SysInfo
	config   *m.Config
)

func LoadSYSInfo() {
	config = m.MConfig
	_activeDevices = ActiveDevices{sync.Map{}}

	StreamList = streamsList{&sync.Map{}, &sync.Map{}, 0}
	RecordList = apiRecordList{items: map[string]*apiRecordItem{}, l: sync.RWMutex{}}

	// init sysinfo
	// _sysinfo = &m.SysInfo{}
	// if err := db.Get(db.DBClient, _sysinfo); err != nil {
	// 	if db.RecordNotFound(err) {
	// 		//  初始不存在
	// 		_sysinfo = m.DefaultInfo()

	// 		if err = db.Create(db.DBClient, _sysinfo); err != nil {
	// 			// logrus.Fatalf("1 init sysinfo err:%v", err)
	// 		}
	// 	} else {
	// 		// logrus.Fatalf("2 init sysinfo err:%v", err)
	// 	}
	// }
	m.MConfig.GB28181 = _sysinfo

	// uri, _ := sip.ParseSipURI(fmt.Sprintf("sip:%s@%s", _sysinfo.LID, _sysinfo.Region))
	_serverDevices = Devices{
		DeviceID: _sysinfo.LID,
		// Region:   _sysinfo.Region,
		addr: &sip.Address{
			DisplayName: sip.String{Str: "sipserver"},
			// URI:         &uri,
			Params: sip.NewParams(),
		},
	}

	// init media
	url, err := url.Parse(config.Media.RTP)
	if err != nil {
		// logrus.Fatalf("media rtp url error,url:%s,err:%v", config.Media.RTP, err)
	}
	ipaddr, err := net.ResolveIPAddr("ip", url.Hostname())
	if err != nil {
		// logrus.Fatalf("media rtp url error,url:%s,err:%v", config.Media.RTP, err)
	}
	_sysinfo.MediaServerRtpIP = ipaddr.IP
	_sysinfo.MediaServerRtpPort, _ = strconv.Atoi(url.Port())
}

// zlm接收到的ssrc为16进制。发起请求的ssrc为10进制
func ssrc2stream(ssrc string) string {
	if ssrc[0:1] == "0" {
		ssrc = ssrc[1:]
	}
	num, _ := strconv.Atoi(ssrc)
	return fmt.Sprintf("%08X", num)
}

func sipResponse(tx *sip.Transaction) (*sip.Response, error) {
	return sipResponseContext(context.Background(), tx)
}

func sipResponseContext(ctx context.Context, tx *sip.Transaction) (*sip.Response, error) {
	if tx == nil {
		return nil, sip.NewError(nil, "SIP transaction is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	response, err := tx.GetResponseContext(ctx)
	if err != nil {
		tx.Close()
		return nil, err
	}
	if response == nil {
		return nil, sip.NewError(nil, "response timeout", "tx key:", tx.Key())
	}
	if response.StatusCode() != http.StatusOK {
		return response, sip.NewError(nil, "device: ", response.StatusCode(), " ", response.Reason())
	}
	return response, nil
}

// QueryCatalog 查询 catalog
func (s *Server) QueryCatalog(deviceID string) error {
	return s.gb.QueryCatalog(deviceID)
}

// QueryCatalogContext 查询目录，并允许调用方取消 SIP 及多响应等待。
func (s *Server) QueryCatalogContext(ctx context.Context, deviceID string) error {
	return s.gb.QueryCatalogContext(ctx, deviceID)
}

func (s *Server) Play(in *PlayInput) error {
	return s.PlayContext(context.Background(), in)
}

// PlayContext 启动实时点播并允许调用方取消 SIP INVITE 等待。
func (s *Server) PlayContext(ctx context.Context, in *PlayInput) error {
	s.gb.metrics.mediaRequests.Add(1)
	err := s.gb.PlayContext(ctx, in)
	if err != nil {
		s.gb.metrics.mediaFailures.Add(1)
	} else {
		s.gb.metrics.mediaSuccess.Add(1)
	}
	return err
}

func (s *Server) StopPlay(ctx context.Context, in *StopPlayInput) error {
	return s.gb.StopPlay(ctx, in)
}

func (s *Server) PTZ(ctx context.Context, in *PTZInput) (*PTZOutput, error) {
	return s.gb.PTZContext(ctx, in)
}

// DeviceControl 执行附录 A.2.3 设备控制命令。
func (s *Server) DeviceControl(ctx context.Context, in *DeviceControlInput) (*DeviceControlOutput, error) {
	return s.gb.DeviceControl(ctx, in)
}

// DeviceQuery 执行附录 A.2.4 设备查询命令。
func (s *Server) DeviceQuery(ctx context.Context, in *DeviceQueryInput) (*DeviceQueryOutput, error) {
	return s.gb.DeviceQuery(ctx, in)
}

func (s *Server) SetBasicParam(ctx context.Context, in *BasicParamConfigInput) (*DeviceConfigState, error) {
	return s.gb.SetBasicParam(ctx, in)
}

func (s *Server) SetDeviceConfig(ctx context.Context, in *DeviceConfigInput) (*DeviceConfigState, error) {
	return s.gb.SetDeviceConfig(ctx, in)
}

// QueryRecordList 查询设备录像目录（RecordInfo）。
func (s *Server) QueryRecordList(ctx context.Context, in *RecordQueryInput) (*Records, error) {
	return s.gb.QueryRecordList(ctx, in)
}

// SetAlarmHandler 注册报警回调。
func (s *Server) SetAlarmHandler(fn func(context.Context, *AlarmEvent)) {
	s.gb.SetAlarmHandler(fn)
}

// Upgrade 执行设备软件升级（GB/T 28181-2022 9.13）。
func (s *Server) Upgrade(ctx context.Context, in *UpgradeInput) (*UpgradeOutput, error) {
	return s.gb.Upgrade(ctx, in)
}

// UpgradeState 返回 2022 设备软件升级会话的最新状态。
func (s *Server) UpgradeState(deviceID, sessionID string) (UpgradeState, bool) {
	if s == nil || s.gb == nil {
		return UpgradeState{}, false
	}
	return s.gb.UpgradeState(deviceID, sessionID)
}

func (s *Server) StartHistory(ctx context.Context, in *HistoryInput) error {
	s.gb.metrics.mediaRequests.Add(1)
	err := s.gb.StartHistory(ctx, in)
	if err != nil {
		s.gb.metrics.mediaFailures.Add(1)
	} else {
		s.gb.metrics.mediaSuccess.Add(1)
	}
	return err
}

func (s *Server) StopHistory(ctx context.Context, in *StopHistoryInput) error {
	return s.gb.StopHistory(ctx, in)
}

func (s *Server) ControlHistory(ctx context.Context, in *ControlHistoryInput) error {
	return s.gb.ControlHistory(ctx, in)
}

func (s *Server) DirectTCPDownloadState(sessionID string) (DirectTCPDownloadState, bool) {
	if s == nil || s.gb == nil || s.gb.directDownloads == nil {
		return DirectTCPDownloadState{}, false
	}
	return s.gb.directDownloads.State(sessionID)
}

func (s *Server) DirectTCPDownloadByChannel(deviceID, channelID string) (DirectTCPDownloadState, bool) {
	if s == nil || s.gb == nil || s.gb.directDownloads == nil {
		return DirectTCPDownloadState{}, false
	}
	return s.gb.directDownloads.FindByChannel(deviceID, channelID)
}

func (s *Server) RTPDownloadByChannel(deviceID, channelID string) (RTPDownloadState, bool) {
	if s == nil || s.gb == nil {
		return RTPDownloadState{}, false
	}
	return s.gb.RTPDownloadByChannel(deviceID, channelID)
}

func (s *Server) CancelDirectTCPDownload(sessionID string) bool {
	return s != nil && s.gb != nil && s.gb.directDownloads != nil && s.gb.directDownloads.Cancel(sessionID)
}

func (s *Server) Metrics() GBMetricsSnapshot {
	if s == nil || s.gb == nil {
		return GBMetricsSnapshot{}
	}
	return s.gb.metrics.Snapshot()
}

func (s *Server) SyncTime(ctx context.Context, in *TimeSyncInput) error {
	return s.gb.SyncTime(ctx, in)
}

// ProbeOptions 发起 OPTIONS 探活。
func (s *Server) ProbeOptions(ctx context.Context, in *OptionsProbeInput) error {
	return s.gb.ProbeOptions(ctx, in)
}

func (s *Server) Subscribe(ctx context.Context, in *SubscribeInput) error {
	return s.gb.Subscribe(ctx, in)
}

func (s *Server) StartVoice(ctx context.Context, in *VoiceInput) error {
	return s.gb.StartVoice(ctx, in)
}

func (s *Server) StopVoice(ctx context.Context, in *StopVoiceInput) error {
	return s.gb.StopVoice(ctx, in)
}

// QuerySnapshot 厂商实现抓图的少，sip 层已实现，先搁置
func (s *Server) QuerySnapshot(deviceID, targetID, coverKey string) (*SnapshotState, error) {
	return s.gb.QuerySnapshot(deviceID, targetID, coverKey)
}

// QuerySnapshotContext 下发抓拍并允许调用方取消 SIP 及业务应答等待。
func (s *Server) QuerySnapshotContext(ctx context.Context, deviceID, targetID, coverKey string) (*SnapshotState, error) {
	return s.gb.QuerySnapshotContext(ctx, deviceID, targetID, coverKey)
}

func (s *Server) SnapshotState(deviceID, sessionID string) (SnapshotState, bool) {
	if s == nil || s.gb == nil {
		return SnapshotState{}, false
	}
	return s.gb.SnapshotState(deviceID, sessionID)
}

func (s *Server) ValidateSnapshotUpload(deviceID, coverKey, sessionID string) error {
	if s == nil || s.gb == nil {
		return fmt.Errorf("GB28181 server is unavailable")
	}
	return s.gb.ValidateSnapshotUpload(deviceID, coverKey, sessionID)
}

func (s *Server) MarkSnapshotUploaded(deviceID, sessionID string) {
	if s != nil && s.gb != nil {
		s.gb.MarkSnapshotUploaded(deviceID, sessionID)
	}
}
