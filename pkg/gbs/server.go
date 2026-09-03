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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/annexg/gormstore"
	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"gorm.io/gorm"
)

// defaultSIPServer 仅供保留的旧版包级停止接口使用；核心流程均显式使用 Server 实例。
// 保存业务层 Server，确保兼容入口同样应用目标解析、跨域身份和 Date+Note 安全策略。
var defaultSIPServer atomic.Pointer[Server]

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

// contextMemoryStorer 是状态提交的可选上下文能力。
// 保留 MemoryStorer.Change，避免破坏既有存储实现和测试桩。
type contextMemoryStorer interface {
	ChangeContext(context.Context, string, func(*ipc.Device) error, func(*Device)) error
}

// contextMemoryLoader 是启动恢复的可选上下文能力。
// 保留 MemoryStorer.LoadDeviceToMemory，避免破坏既有存储实现和测试桩。
type contextMemoryLoader interface {
	LoadDeviceToMemoryContext(context.Context, sip.Connection) error
}

// deviceChannelMemoryLoader 是设备重新建立运行态时可选的持久通道恢复能力。
// 保持为可选接口，避免破坏既有 MemoryStorer 实现和测试桩。
type deviceChannelMemoryLoader interface {
	LoadDeviceChannelsContext(context.Context, string, *Device) error
}

type Server struct {
	*sip.Server
	gb           *GB28181API
	mediaService cascadeMediaServerResolver
	cascade      *CascadeManager

	fromAddress   sip.Address
	memoryStorer  MemoryStorer
	dialDeviceTCP func(context.Context, string) (net.Conn, error)
}

func (s *Server) changeMemory(
	ctx context.Context,
	deviceID string,
	changePersistent func(*ipc.Device) error,
	changeRuntime func(*Device),
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if store, ok := s.memoryStorer.(contextMemoryStorer); ok {
		return store.ChangeContext(ctx, deviceID, changePersistent, changeRuntime)
	}
	return s.memoryStorer.Change(deviceID, changePersistent, changeRuntime)
}

func (s *Server) loadDeviceMemory(ctx context.Context, conn sip.Connection) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if loader, ok := s.memoryStorer.(contextMemoryLoader); ok {
		return loader.LoadDeviceToMemoryContext(ctx, conn)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.memoryStorer.LoadDeviceToMemory(conn)
}

// loadOrStoreDeviceMemory 在首次发布设备运行态前恢复已有通道。
// TCP/TLS 设备重启后不会复用旧连接；其重新 REGISTER 时必须立即恢复持久通道，
// 不能等注册后的 Catalog 查询完成才允许播放。
func (s *Server) loadOrStoreDeviceMemory(ctx context.Context, deviceID string, device *Device) error {
	if err := s.prepareDeviceMemory(ctx, deviceID, device); err != nil {
		return err
	}
	s.memoryStorer.LoadOrStore(deviceID, device)
	return nil
}

// prepareDeviceMemory 只填充尚未发布的候选运行态，不提前提交到内存存储。
// Keepalive 依赖该边界，确保 SIP 200 写失败时不会留下半初始化设备。
func (s *Server) prepareDeviceMemory(ctx context.Context, deviceID string, device *Device) error {
	if s == nil || s.memoryStorer == nil {
		return fmt.Errorf("GB28181 memory store unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, loaded := s.memoryStorer.Load(deviceID); !loaded {
		if loader, ok := s.memoryStorer.(deviceChannelMemoryLoader); ok {
			if err := loader.LoadDeviceChannelsContext(ctx, deviceID, device); err != nil {
				return err
			}
		}
	}
	return nil
}

// RefreshDeviceVersion 将持久化的协议档案同步到在线设备会话。
// 设备编辑接口会调用该方法，使手动版本覆盖无需等待重启或重新注册。
func (s *Server) RefreshDeviceVersion(device *ipc.Device) {
	if device == nil {
		return
	}
	version := ApplyGBVersionProfile(device)
	if s == nil {
		return
	}
	if s.memoryStorer == nil {
		return
	}
	if current, ok := s.memoryStorer.Load(device.GetGB28181DeviceID()); ok {
		current.setGBProfile(version, device.Ext.GBDisabledCapabilities)
	}
}

// LockChannelMedia 将管理端媒体节点绑定与同通道媒体会话串行化。
// 离线设备没有可启动的运行态媒体会话，因此允许直接预配置绑定。
func (s *Server) LockChannelMedia(ctx context.Context, deviceID, channelID string) (func(), error) {
	if s == nil || s.memoryStorer == nil {
		return nil, fmt.Errorf("GB28181 memory store unavailable")
	}
	deviceID = strings.TrimSpace(deviceID)
	channelID = strings.TrimSpace(channelID)
	if deviceID == "" || channelID == "" {
		return nil, fmt.Errorf("GB28181 media channel is unavailable")
	}
	device, ok := s.memoryStorer.Load(deviceID)
	if !ok || device == nil {
		return func() {}, nil
	}
	return device.lockMediaContext(ctx, channelID)
}

// ApplyGBVersionProfile 只更新待持久化设备模型的有效能力快照，不发布在线运行态。
// 设备编辑校验发生在数据库事务提交前，必须与 RefreshDeviceVersion 分离，避免回滚后运行态提前生效。
func ApplyGBVersionProfile(device *ipc.Device) GBProtocolVersion {
	if device == nil {
		return GBVersion10
	}
	version := deviceProtocolVersion(device.Ext)
	device.Ext.GBVersionCapabilities = effectiveCapabilityNames(version, device.Ext.GBDisabledCapabilities)
	return version
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

// NewServer 保留原有构造签名，兼容直接集成 gbs 包的调用方。
func NewServer(cfg *conf.Bootstrap, store ipc.Adapter, sc sms.Core) (*Server, func(), error) {
	return newServer(context.Background(), cfg, store, sc, nil, nil)
}

// NewServerWithStores 注入数据库和中心录像存储，供应用依赖装配使用。
func NewServerWithStores(cfg *conf.Bootstrap, store ipc.Adapter, sc sms.Core, db *gorm.DB, recordingStore recording.Storer) (*Server, func(), error) {
	return newServer(context.Background(), cfg, store, sc, db, recordingStore)
}

// NewServerWithStoresContext 注入应用生命周期，使启动恢复可被关闭信号取消。
func NewServerWithStoresContext(ctx context.Context, cfg *conf.Bootstrap, store ipc.Adapter, sc sms.Core, db *gorm.DB, recordingStore recording.Storer) (*Server, func(), error) {
	return newServer(ctx, cfg, store, sc, db, recordingStore)
}

func newServer(ctx context.Context, cfg *conf.Bootstrap, store ipc.Adapter, sc sms.Core, db *gorm.DB, recordingStore recording.Storer) (*Server, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil {
		return nil, nil, fmt.Errorf("GB28181 configuration is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := conf.ValidateSIPConfig(cfg.Sip); err != nil {
		return nil, nil, err
	}
	memoryStorer, ok := store.Store().(MemoryStorer)
	if !ok || memoryStorer == nil {
		return nil, nil, fmt.Errorf("GB28181 memory store is unavailable")
	}
	advertiseHost, err := resolveSIPAdvertiseHost(cfg.Sip.Host, cfg.Media.SDPIP, sip.ResolveSelfIP)
	if err != nil {
		return nil, nil, err
	}
	uri, err := sip.ParseSipURI(fmt.Sprintf("sip:%s@%s", cfg.Sip.ID, net.JoinHostPort(advertiseHost, strconv.Itoa(cfg.Sip.Port))))
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
	// 后台清理器必须等 Server、内存存储和级联管理器全部装配完成后再启动。
	api := newGB28181API(cfg, store, sc.NodeManager)
	// SIP socket 必须先打开才能把 UDP 连接装载到设备运行态；在恢复完成前用中间件
	// 拒绝新业务，避免数据库旧快照覆盖启动期间刚完成的 REGISTER/Keepalive。
	api.startupReady = make(chan struct{})
	if recordingStore != nil {
		api.recordingStore = recordingStore.Recording()
	}
	api.registerCertificateAuth = registerCertificateAuth
	api.annexG, err = newAnnexGServiceContext(ctx, cfg.Sip, db)
	if err != nil {
		if sipTrafficLogger != nil {
			_ = sipTrafficLogger.Close()
		}
		return nil, nil, fmt.Errorf("initialize GB28181 Annex G: %w", err)
	}
	if api.annexG != nil {
		api.annexG.send = api.sendAnnexGResponse
	}
	sipServer := sip.NewServer(&from)
	sipServer.Use(api.sipLifecycleMiddleware)
	sipServer.Use(api.sipMonitorUserIdentityMiddleware)
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
	registerIndependentMessageNotificationRoutes(msg, api)
	registerMobilePositionMessageRoute(msg, api)

	// 报警既可能由 MESSAGE 上报，也可能由 NOTIFY 上报，二者均接入。
	notify := sipServer.Notify(api.sipAccessControlMiddleware, api.sipNotifySubscriptionState)
	notify.Handle("Alarm", api.sipNotifyAlarm)
	notify.Handle("Catalog", api.sipNotifyCatalog)
	notify.Handle("MobilePosition", api.sipNotifyMobilePosition)
	notify.Handle("PTZPosition", api.sipMessageQueryGeneric)
	msg.Handle("Alarm", api.sipMessageAlarm)

	// 9.11 事件源侧：接收上级订阅请求（SUBSCRIBE）。
	sipServer.Subscribe(api.sipAccessControlMiddleware, api.sipSubscribeEvent)
	// 9.2 被叫侧会话兼容：接收入向 INVITE/BYE/ACK。
	sipServer.Handle(sip.MethodInvite, api.sipMediaRegistrationBindingMiddleware, api.sipInviteGeneric)
	sipServer.Handle(sip.MethodCancel, api.sipMediaRegistrationBindingMiddleware, api.sipCancelGeneric)
	sipServer.Handle(sip.MethodBYE, api.sipMediaRegistrationBindingMiddleware, api.sipByeGeneric)
	sipServer.Handle(sip.MethodACK, api.sipMediaRegistrationBindingMiddleware, api.sipAckGeneric)
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
	msg.Handle("Broadcast", api.sipMessageBroadcastResponse)
	if api.annexG != nil {
		registerAnnexGRoutes(msg, api.sipAnnexGMessage)
	}

	c := Server{
		Server:       sipServer,
		mediaService: sc,
		fromAddress:  from,
		gb:           api,
		memoryStorer: memoryStorer,
	}
	api.svr = &c
	sipServer.SetRequestSecurityResolver(api.resolveRequestSecurity)
	var (
		cleanupOnce     sync.Once
		loggerInstalled bool
	)
	cleanup := func() {
		cleanupOnce.Do(func() {
			c.Close()
			defaultSIPServer.CompareAndSwap(&c, nil)
			if loggerInstalled {
				sip.CompareAndSwapTrafficLogger(sipTrafficLogger, nil)
				if sipTrafficLogger != nil {
					_ = sipTrafficLogger.Close()
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
	if api.annexG != nil {
		api.annexG.bindServer(&c)
	}
	previous := sip.SetTrafficLogger(sipTrafficLogger)
	loggerInstalled = true
	if previous != nil {
		_ = previous.Close()
	}
	defaultSIPServer.Store(&c)
	if err := c.loadDeviceMemory(ctx, sipServer.UDPConn()); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("load GB28181 devices into memory: %w", err)
	}
	// 恢复完成后立即收敛已过期的 UDP REGISTER 绑定，避免等首次周期扫描才更新数据库和通道状态。
	c.checkOfflineDevices(time.Now())
	api.startLifecycleWorker(c.startTickerCheck)
	c.cascade = NewCascadeManager(&c)
	if err := c.cascade.Apply(cfg.Sip, cfg.Sip.Upstreams); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("initialize GB28181 cascade upstreams: %w", err)
	}
	api.startBackgroundWorkers()
	api.markStartupReady()
	return &c, cleanup, nil
}

func registerIndependentMessageNotificationRoutes(msg *sip.RouteGroup, api *GB28181API) {
	msg.Handle("DeviceUpgradeResult", api.sipMessageDeviceUpgradeResult)
	msg.Handle("UploadSnapShotFinished", api.sipMessageSnapshotFinished)
	msg.Handle("VideoUploadNotify", api.sipMessageVideoUploadNotify)
}

func requireMessageNotification(ctx *sip.Context, cmdType string) bool {
	if ctx == nil || ctx.Request == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(ctx.Request.Method()), sip.MethodMessage) {
		ctx.String(400, cmdType+" notification requires MESSAGE")
		return false
	}
	return true
}

// registerMobilePositionMessageRoute 注册 A.2.4 查询后通过 MESSAGE/Notify 上报的位置通知。
// 订阅产生的 SIP NOTIFY 仍由 Notify 路由使用同一个业务处理器。
func registerMobilePositionMessageRoute(group *sip.RouteGroup, api *GB28181API) {
	group.Handle("MobilePosition", api.sipNotifyMobilePosition)
}

func resolveSIPAdvertiseHost(configured, fallback string, resolver func() (net.IP, error)) (string, error) {
	host := strings.TrimSpace(configured)
	if host == "" {
		if selfIP, err := resolver(); err == nil {
			host = selfIP.String()
		} else {
			host = strings.TrimSpace(fallback)
			if host == "" {
				return "", fmt.Errorf("resolve GB28181 SIP advertise address: %w", err)
			}
		}
	}
	if strings.HasPrefix(host, "[") || strings.HasSuffix(host, "]") {
		if !strings.HasPrefix(host, "[") || !strings.HasSuffix(host, "]") {
			return "", fmt.Errorf("GB28181 SIP advertise address has mismatched IPv6 brackets: %s", host)
		}
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		if ip := net.ParseIP(host); ip == nil || ip.To4() != nil {
			return "", fmt.Errorf("GB28181 SIP advertise address is not a valid bracketed IPv6 address: %s", host)
		}
	}
	return host, nil
}

// Close 先收敛 GB28181 业务任务，再注销上级级联并关闭底层 SIP 监听器。
func (s *Server) Close() {
	if s == nil {
		return
	}
	if s.gb != nil {
		s.gb.beginClose()
	}
	if s.cascade != nil {
		s.cascade.Close()
	}
	if s.gb != nil && s.gb.annexG != nil {
		s.gb.annexG.close()
	}
	if s.gb != nil {
		s.gb.close()
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
	if err := s.ApplyConfig(cfg); err != nil {
		slog.Error("reload GB28181 SIP config failed", "err", err)
	}
}

// ApplyConfig 原子应用可热更新的 SIP 运行配置，并将失败返回给管理接口。
func (s *Server) ApplyConfig(cfg conf.SIP) error {
	if s == nil || s.gb == nil {
		return fmt.Errorf("GB28181 server is unavailable")
	}
	// 级联配置可能需要加载证书等外部资源；必须先成功替换 worker，再提交其余运行态，
	// 避免形成“新通用配置 + 旧级联配置”的部分热更新。
	if s.cascade != nil {
		if err := s.cascade.Apply(cfg, cfg.Upstreams); err != nil {
			return fmt.Errorf("reload GB28181 cascade upstreams: %w", err)
		}
	}
	s.gb.setConfig(cfg)
	s.gb.applyDirectTCPConfig(cfg.DirectTCPDownload)
	if s.gb.directDownloads != nil {
		s.gb.directDownloads.Reconfigure(directTCPDownloadOptions(cfg.DirectTCPDownload))
	}
	return nil
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
		if state.KeepalivePersistencePending {
			if _, err := s.gb.retryPendingKeepalive(key, state); err != nil {
				slog.Error("retry persisting Keepalive failed", "device_id", key, "err", err)
			}
			return true
		}
		if state.DeviceStatusPersistencePending {
			if _, err := s.gb.retryPendingDeviceStatus(key, state); err != nil {
				slog.Error("retry persisting DeviceStatus failed", "device_id", key, "err", err)
			}
			return true
		}
		registrationExpired := registrationBindingExpired(state.LastRegisterAt, state.Expires, now)
		if !state.IsOnline {
			if state.OfflinePersistencePending {
				if _, err := s.logoutDeviceIfCurrent(key, state); err != nil {
					slog.Error("retry persisting device offline state failed", "device_id", key, "err", err)
				}
			} else if !state.RegistrationClosed && registrationExpired {
				if _, err := s.logoutDeviceIfCurrent(key, state); err != nil {
					slog.Error("logout DeviceStatus offline device after registration expiry failed", "device_id", key, "err", err)
				}
			}
			return true
		}
		if len(key) < 18 {
			return true
		}

		// REGISTER 绑定到期后必须离线；Keepalive 和 OPTIONS 只证明传输可达，不能续期注册。
		if registrationExpired {
			changed, err := s.logoutDeviceIfCurrent(key, state)
			if err != nil {
				slog.Error("logout device after registration expiry failed", "device_id", key, "err", err)
			} else if changed {
				slog.Info("device registration expired",
					"device_id", key,
					"registered_at", state.LastRegisterAt,
					"expires", state.Expires,
				)
			}
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
				if err := s.gb.ProbeOptions(s.gb.serviceContext(), &OptionsProbeInput{
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
	unlock := s.gb.lockRegisterOperation(deviceID)
	defer unlock()
	err := s.gb.logout(deviceID, func(d *ipc.Device) error {
		if d.Expires != expected.Expires ||
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
	if err == nil {
		return true, nil
	}
	dev, ok := s.memoryStorer.Load(deviceID)
	if !ok || dev == nil {
		return false, err
	}
	marked := false
	dev.UpdateRuntime(func(current *Device) {
		if sameOfflineSnapshotLocked(current, expected) {
			current.IsOnline = false
			current.offlinePersistencePending = true
			current.registrationClosed = true
			clearPendingDeviceStatusLocked(current)
			clearPendingKeepaliveLocked(current)
			marked = true
		}
	})
	if marked {
		s.gb.cleanupOfflineDeviceRuntime(deviceID)
	}
	return marked, err
}

// sameOfflineSnapshotLocked 必须在持有 current.stateMu 写锁时调用。
func sameOfflineSnapshotLocked(current *Device, expected deviceRuntimeState) bool {
	return current != nil && current.Expires == expected.Expires &&
		current.LastRegisterAt.Equal(expected.LastRegisterAt) &&
		current.LastKeepaliveAt.Equal(expected.LastKeepaliveAt)
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

	StreamList = streamsList{Response: &sync.Map{}, Succ: &sync.Map{}}
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
	return sipResponseContextAccepted(ctx, tx, func(status int) bool {
		return status == http.StatusOK
	})
}

// sipInviteResponseContext 等待 INVITE 最终响应。RFC 3261 将全部 2xx 视为成功响应，
// 具体媒体语义由调用方在发送 ACK 后继续校验。
func sipInviteResponseContext(ctx context.Context, tx *sip.Transaction) (*sip.Response, error) {
	return sipResponseContextAcceptedKeepTransaction(ctx, tx, func(status int) bool {
		return status >= http.StatusOK && status < http.StatusMultipleChoices
	})
}

// sipResponseContextAccepted 用于完成后不再复用客户端事务的非 INVITE 请求。
// INVITE 的 2xx ACK 仍需原事务连接、安全器和缓存，因此必须走 sipInviteResponseContext。
func sipResponseContextAccepted(ctx context.Context, tx *sip.Transaction, accepted func(int) bool) (*sip.Response, error) {
	if tx != nil {
		defer tx.Close()
	}
	return sipResponseContextAcceptedKeepTransaction(ctx, tx, accepted)
}

func sipResponseContextAcceptedKeepTransaction(ctx context.Context, tx *sip.Transaction, accepted func(int) bool) (*sip.Response, error) {
	if tx == nil {
		return nil, sip.NewError(nil, "SIP transaction is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	response, err := tx.GetResponseContext(ctx)
	if err != nil {
		cancelSent, cancelErr := tx.CancelInviteDetached()
		if !cancelSent && cancelErr == nil {
			tx.Close()
		}
		return nil, errors.Join(err, cancelErr)
	}
	if response == nil {
		cancelSent, cancelErr := tx.CancelInviteDetached()
		if !cancelSent && cancelErr == nil {
			tx.Close()
		}
		return nil, errors.Join(sip.NewError(nil, "response timeout", "tx key:", tx.Key()), cancelErr)
	}
	if accepted == nil || !accepted(response.StatusCode()) {
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

// SetReliableAlarmHandler 注册带错误返回的报警回调，启用持久收件箱重放语义。
func (s *Server) SetReliableAlarmHandler(fn func(context.Context, *AlarmEvent) error) {
	s.gb.SetReliableAlarmHandler(fn)
}

// Upgrade 执行设备软件升级（GB/T 28181-2022 9.13）。
func (s *Server) Upgrade(ctx context.Context, in *UpgradeInput) (*UpgradeOutput, error) {
	return s.gb.Upgrade(ctx, in)
}

// UpgradeState 返回 2022 设备软件升级会话的最新状态。
func (s *Server) UpgradeState(deviceID, sessionID string) (UpgradeState, bool) {
	state, ok, err := s.UpgradeStateContext(context.Background(), deviceID, sessionID)
	if err != nil {
		slog.Error("load upgrade state", "device_id", deviceID, "session_id", sessionID, "err", err)
		return UpgradeState{}, false
	}
	return state, ok
}

func (s *Server) UpgradeStateContext(ctx context.Context, deviceID, sessionID string) (UpgradeState, bool, error) {
	if s == nil || s.gb == nil {
		return UpgradeState{}, false, fmt.Errorf("GB28181 server is unavailable")
	}
	return s.gb.UpgradeStateContext(ctx, deviceID, sessionID)
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
	snapshot := s.gb.metrics.Snapshot()
	if s.gb.annexG != nil {
		snapshot.AnnexGPending = uint64(s.gb.annexG.pendingCount())
	}
	return snapshot
}

// AnnexGAlarmAudits 查询附录 G 三类报警的业务审计记录。
func (s *Server) AnnexGAlarmAudits(ctx context.Context, query gormstore.AlarmAuditQuery) (gormstore.AlarmAuditPage, error) {
	store, err := s.annexGAuditStore()
	if err != nil {
		return gormstore.AlarmAuditPage{}, err
	}
	return store.ListAlarmAudits(ctx, query)
}

// AnnexGDefenceStates 查询卡口布控当前状态。
func (s *Server) AnnexGDefenceStates(ctx context.Context, query gormstore.DefenceAuditQuery) (gormstore.DefenceStatePage, error) {
	store, err := s.annexGAuditStore()
	if err != nil {
		return gormstore.DefenceStatePage{}, err
	}
	return store.ListDefenceStates(ctx, query)
}

// AnnexGDefenceAudits 查询不可变的布控和撤控历史。
func (s *Server) AnnexGDefenceAudits(ctx context.Context, query gormstore.DefenceAuditQuery) (gormstore.DefenceAuditPage, error) {
	store, err := s.annexGAuditStore()
	if err != nil {
		return gormstore.DefenceAuditPage{}, err
	}
	return store.ListDefenceAudits(ctx, query)
}

func (s *Server) annexGAuditStore() (*gormstore.Store, error) {
	if s == nil || s.gb == nil || s.gb.annexG == nil || s.gb.annexG.store == nil {
		return nil, errors.New("GB28181 Annex G is disabled")
	}
	return s.gb.annexG.store, nil
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

func (s *Server) OutgoingSubscriptionStates(ctx context.Context, deviceID string) ([]OutgoingSubscriptionState, error) {
	return s.gb.OutgoingSubscriptionStates(ctx, deviceID)
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
	state, ok, err := s.SnapshotStateContext(context.Background(), deviceID, sessionID)
	if err != nil {
		slog.Error("load snapshot state", "device_id", deviceID, "session_id", sessionID, "err", err)
		return SnapshotState{}, false
	}
	return state, ok
}

func (s *Server) SnapshotStateContext(ctx context.Context, deviceID, sessionID string) (SnapshotState, bool, error) {
	if s == nil || s.gb == nil {
		return SnapshotState{}, false, fmt.Errorf("GB28181 server is unavailable")
	}
	return s.gb.SnapshotStateContext(ctx, deviceID, sessionID)
}

func (s *Server) ValidateSnapshotUpload(deviceID, coverKey, sessionID string) error {
	return s.ValidateSnapshotUploadContext(context.Background(), deviceID, coverKey, sessionID)
}

func (s *Server) ValidateSnapshotUploadContext(ctx context.Context, deviceID, coverKey, sessionID string) error {
	if s == nil || s.gb == nil {
		return fmt.Errorf("GB28181 server is unavailable")
	}
	return s.gb.ValidateSnapshotUploadContext(ctx, deviceID, coverKey, sessionID)
}

func (s *Server) MarkSnapshotUploaded(deviceID, sessionID string) {
	if err := s.CommitSnapshotUpload(deviceID, sessionID); err != nil {
		slog.Error("mark snapshot uploaded", "device_id", deviceID, "session_id", sessionID, "err", err)
	}
}

// CommitSnapshotUpload 使用任务持久化生命周期提交上传计数，并将错误返回给回调接口。
func (s *Server) CommitSnapshotUpload(deviceID, sessionID string) error {
	if s == nil || s.gb == nil {
		return fmt.Errorf("GB28181 server is unavailable")
	}
	return s.gb.MarkSnapshotUploadedContext(s.gb.taskPersistenceContext(), deviceID, sessionID)
}
