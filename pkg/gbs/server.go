package gbs

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/netpulse/ip"
)

type MemoryStorer interface {
	LoadOrStore(deviceID string, value *Device)
	LoadDeviceToMemory(conn sip.Connection)               // 加载设备到内存
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
	mediaService sms.Core

	fromAddress  sip.Address
	memoryStorer MemoryStorer
}

func NewServer(cfg *conf.Bootstrap, store ipc.Adapter, sc sms.Core) (*Server, func()) {
	api := NewGB28181API(cfg, store, sc.NodeManager)

	iip := ip.InternalIP()
	uri, _ := sip.ParseSipURI(fmt.Sprintf("sip:%s@%s:%d", cfg.Sip.ID, iip, cfg.Sip.Port))
	from := sip.Address{
		DisplayName: sip.String{Str: "gowvp/owl"},
		URI:         &uri,
		Params:      sip.NewParams(),
	}

	svr = sip.NewServer(&from)
	sipTrafficLogger, err := sip.NewTrafficLogger(sip.TrafficLogConfig{
		Enabled:      cfg.Sip.Log.Enabled,
		Dir:          filepath.Join(cfg.ConfigDir, cfg.Sip.Log.Dir),
		MaxAge:       cfg.Sip.Log.MaxAge.Duration(),
		RotationTime: cfg.Sip.Log.RotationTime.Duration(),
		RotationSize: cfg.Sip.Log.RotationSize * 1024 * 1024,
	})
	if err != nil {
		slog.Error("init sip traffic logger failed", "err", err)
	} else {
		previous := sip.SetTrafficLogger(sipTrafficLogger)
		if previous != nil {
			_ = previous.Close()
		}
	}
	svr.Register(api.handlerRegister)
	msg := svr.Message(api.sipAccessControlMiddleware)
	msg.Handle("Keepalive", api.sipMessageKeepalive)
	msg.Handle("Catalog", api.sipMessageCatalog)
	msg.Handle("DeviceInfo", api.sipMessageDeviceInfo)
	msg.Handle("ConfigDownload", api.sipMessageConfigDownload)
	msg.Handle("DeviceConfig", api.handleDeviceConfig)
	msg.Handle("DeviceControl", api.sipMessageDeviceControl)
	msg.Handle("RecordInfo", api.sipMessageRecordInfo)

	// 报警既可能由 MESSAGE 上报，也可能由 NOTIFY 上报，二者均接入。
	notify := svr.Notify(api.sipAccessControlMiddleware)
	notify.Handle("Alarm", api.sipNotifyAlarm)
	notify.Handle("Catalog", api.sipNotifyCatalog)
	notify.Handle("MobilePosition", api.sipNotifyMobilePosition)
	notify.Handle("PTZPosition", api.sipMessageQueryGeneric)
	notify.Handle("DeviceStatus", api.sipMessageQueryGeneric)
	notify.Handle("PresetQuery", api.sipMessageQueryGeneric)
	notify.Handle("HomePositionQuery", api.sipMessageQueryGeneric)
	notify.Handle("SDCardStatus", api.sipMessageQueryGeneric)
	notify.Handle("ConfigDownload", api.sipMessageQueryGeneric)
	msg.Handle("Alarm", api.sipMessageAlarm)

	// 9.11 事件源侧：接收上级订阅请求（SUBSCRIBE）。
	svr.Subscribe(api.sipSubscribeEvent)
	// 9.2 被叫侧会话兼容：接收入向 INVITE/BYE/ACK。
	svr.Handle(sip.MethodInvite, api.sipInviteGeneric)
	svr.Handle(sip.MethodBYE, api.sipByeGeneric)
	svr.Handle(sip.MethodACK, api.sipAckGeneric)
	// OPTIONS 探测（入向）兼容。
	svr.Handle(sip.MethodOptions, api.sipOptionsGeneric)

	// A.2.4 查询响应补齐：注册缺失查询命令响应处理。
	msg.Handle("DeviceStatus", api.sipMessageQueryGeneric)
	msg.Handle("PresetQuery", api.sipMessageQueryGeneric)
	msg.Handle("HomePositionQuery", api.sipMessageQueryGeneric)
	msg.Handle("PTZPosition", api.sipMessageQueryGeneric)
	msg.Handle("SDCardStatus", api.sipMessageQueryGeneric)
	msg.Handle("MobilePosition", api.sipMessageQueryGeneric)
	msg.Handle("Broadcast", api.sipMessageQueryGeneric)

	c := Server{
		Server:       svr,
		mediaService: sc,
		fromAddress:  from,
		gb:           api,
		memoryStorer: store.Store().(MemoryStorer),
	}
	api.svr = &c

	go svr.ListenUDPServer(fmt.Sprintf(":%d", cfg.Sip.Port))
	go svr.ListenTCPServer(fmt.Sprintf(":%d", cfg.Sip.Port))
	if cfg.Sip.EnableTLS {
		tlsPort := cfg.Sip.TLSPort
		if tlsPort <= 0 {
			tlsPort = cfg.Sip.Port
		}
		go func() {
			if err := svr.ListenTLSServer(fmt.Sprintf(":%d", tlsPort), cfg.Sip.TLSCert, cfg.Sip.TLSKey); err != nil {
				slog.Error("listen tls server failed", "port", tlsPort, "err", err)
			}
		}()
	}
	go c.startTickerCheck()
	// 等待 UDP 连接
	for {
		time.Sleep(50 * time.Millisecond)
		if svr.UDPConn() != nil {
			c.memoryStorer.LoadDeviceToMemory(svr.UDPConn())
			break
		}
	}
	return &c, func() {
		c.Close()
		if previous := sip.SetTrafficLogger(nil); previous != nil {
			_ = previous.Close()
		}
	}
}

// SetConfig 热更新 SIP 配置，用于配置变更时更新 from 地址而无需重启服务
func (s *Server) SetConfig() {
	cfg := s.gb.cfg
	iip := ip.InternalIP()
	uri, _ := sip.ParseSipURI(fmt.Sprintf("sip:%s@%s:%d", cfg.ID, iip, cfg.Port))
	from := sip.Address{
		DisplayName: sip.String{Str: "gowvp/owl"},
		URI:         &uri,
		Params:      sip.NewParams(),
	}
	s.fromAddress = from
	s.Server.SetFrom(&from)
}

// startTickerCheck 定时检查离线，通过心跳超时判断设备是否离线
func (s *Server) startTickerCheck() {
	conc.Timer(context.Background(), 60*time.Second, time.Second, func() {
		now := time.Now()
		s.memoryStorer.RangeDevices(func(key string, dev *Device) bool {
			if !dev.IsOnline {
				return true
			}
			if len(key) < 18 {
				return true
			}

			// 计算超时时间：心跳间隔 * 超时次数
			// 默认心跳间隔 60s，超时次数 3 次，即 3 分钟无心跳判定离线
			interval := dev.keepaliveInterval
			if interval == 0 {
				interval = 60
			}
			timeoutCount := dev.keepaliveTimeout
			if timeoutCount == 0 {
				timeoutCount = 3
			}
			timeout := time.Duration(interval) * time.Duration(timeoutCount) * time.Second

			// 跳过未收到过心跳的设备（LastKeepaliveAt 为零值），这类设备依赖注册超时处理
			if dev.LastKeepaliveAt.IsZero() {
				// 如果注册时间也超过了超时时间，则判定离线
				if !dev.LastRegisterAt.IsZero() && now.Sub(dev.LastRegisterAt) >= timeout {
					if err := s.gb.logout(key, func(d *ipc.Device) error {
						d.IsOnline = false
						return nil
					}); err != nil {
						slog.Error("logout device failed", "device_id", key, "err", err)
					}
				}
				return true
			}

			// 心跳超时或连接丢失，判定设备离线
			if sub := now.Sub(dev.LastKeepaliveAt); sub >= timeout || dev.conn == nil {
				// 对 TCP/TLS 设备在离线判定前先做一次 OPTIONS 探测，避免瞬时抖动误判离线。
				if sub >= timeout && dev.conn != nil && dev.source != nil && dev.source.Network() != "udp" {
					if err := s.gb.ProbeOptions(context.Background(), &OptionsProbeInput{
						DeviceID: key,
						Timeout:  3 * time.Second,
					}); err == nil {
						return true
					}
				}
				slog.Info("device offline detected",
					"device_id", key,
					"last_keepalive", dev.LastKeepaliveAt,
					"timeout", timeout,
					"elapsed", sub,
					"conn_nil", dev.conn == nil,
				)
				if err := s.gb.logout(key, func(d *ipc.Device) error {
					d.IsOnline = false
					return nil
				}); err != nil {
					slog.Error("logout device failed", "device_id", key, "err", err)
				}
			}
			return true
		})
	})
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
	ssrcLock = &sync.Mutex{}
	_recordList = &sync.Map{}
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
	response := tx.GetResponse()
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

func (s *Server) Play(in *PlayInput) error {
	return s.gb.Play(in)
}

func (s *Server) StopPlay(ctx context.Context, in *StopPlayInput) error {
	return s.gb.StopPlay(ctx, in)
}

func (s *Server) PTZ(ctx context.Context, in *PTZInput) (*PTZOutput, error) {
	return s.gb.PTZ(in)
}

// DeviceControl 执行附录 A.2.3 设备控制命令。
func (s *Server) DeviceControl(ctx context.Context, in *DeviceControlInput) (*DeviceControlOutput, error) {
	return s.gb.DeviceControl(ctx, in)
}

// DeviceQuery 执行附录 A.2.4 设备查询命令。
func (s *Server) DeviceQuery(ctx context.Context, in *DeviceQueryInput) (*DeviceQueryOutput, error) {
	return s.gb.DeviceQuery(ctx, in)
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

func (s *Server) StartHistory(ctx context.Context, in *HistoryInput) error {
	return s.gb.StartHistory(ctx, in)
}

func (s *Server) StopHistory(ctx context.Context, in *StopHistoryInput) error {
	return s.gb.StopHistory(ctx, in)
}

func (s *Server) ControlHistory(ctx context.Context, in *ControlHistoryInput) error {
	return s.gb.ControlHistory(ctx, in)
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
func (s *Server) QuerySnapshot(deviceID, targetID, coverKey string) error {
	return s.gb.QuerySnapshot(deviceID, targetID, coverKey)
}
