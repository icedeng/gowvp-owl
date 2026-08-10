package gbs

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
)

const ignorePassword = "#"

type GB28181API struct {
	cfg  *conf.SIP
	boot *conf.Bootstrap
	core ipc.Adapter

	catalogResponses *multiResponseCollector[Channels]
	recordResponses  *multiResponseCollector[RecordItem]
	// key=deviceID:SN，映射到 RecordInfo 的通道聚合键，兼容设备回写设备 ID。
	recordResponseAliases sync.Map

	// TODO: 待替换成 redis
	streams *conc.Map[string, *Streams]
	// key=deviceID:sn，用于等待 DeviceControl 的业务响应。
	pendingDeviceControl sync.Map
	// key=deviceID:cmdType:sn，用于等待设备查询响应。
	pendingDeviceQuery sync.Map
	// 报警回调，供上层业务（事件中心）注册。
	alarmHandlerMu sync.RWMutex
	alarmHandler   func(context.Context, *AlarmEvent)
	// 事件源侧订阅表（9.11），用于向订阅方发送 NOTIFY。
	eventSubscribers sync.Map
	// key=deviceID:sn，用于等待 DeviceConfig 业务应答（9.7/9.14）。
	pendingDeviceConfig sync.Map
	// key=TargetID:SN，用于等待 2014 Broadcast 业务应答。
	pendingBroadcast sync.Map
	// key=deviceID，保存结构化查询/状态结果（9.5/9.6/A.2.4）。
	queryStates sync.Map
	// key=Call-ID，保存入向 INVITE 会话状态（9.2 被叫侧会话）。
	inviteDialogs sync.Map
	// 设备控制命令全局序列号，避免 PTZ 与 DeviceControl 并发冲突。
	controlSN atomic.Uint32
	// 设备查询命令全局序列号，避免随机 SN 碰撞。
	querySN atomic.Uint32
	// directDownloads 管理 2014 附录 O 无 RTP 封装的 TCP 文件接收。
	directDownloads *DirectTCPDownloadManager
	directPolicyMu  sync.RWMutex
	directPolicy    directTCPRuntimePolicy
	metrics         GBMetrics

	svr *Server

	sms *sms.NodeManager
}

func NewGB28181API(cfg *conf.Bootstrap, store ipc.Adapter, sms *sms.NodeManager) *GB28181API {
	g := GB28181API{
		cfg:  &cfg.Sip,
		boot: cfg,
		core: store,
		sms:  sms,
		catalogResponses: newMultiResponseCollector(func(item Channels) string {
			return item.ChannelID
		}),
		recordResponses: newMultiResponseCollector(func(item RecordItem) string {
			return item.DeviceID + "\x00" + item.FilePath + "\x00" + item.StartTime + "\x00" + item.EndTime
		}),
		streams:         &conc.Map[string, *Streams]{},
		directDownloads: NewDirectTCPDownloadManager(directTCPDownloadOptions(cfg)),
	}
	g.controlSN.Store(uint32(sip.RandInt(100000, 999999)))
	g.querySN.Store(uint32(sip.RandInt(100000, 999999)))
	g.applyDirectTCPConfig(cfg.Sip.DirectTCPDownload)
	go g.startEventSubscriberCleaner()
	go g.startInviteDialogCleaner()
	return &g
}

type directTCPRuntimePolicy struct {
	Enabled   bool
	OfferPort int
	Allowlist map[string]struct{}
}

func (g *GB28181API) applyDirectTCPConfig(in conf.SIPDirectTCPDownload) {
	allowlist := make(map[string]struct{}, len(in.DeviceAllowlist))
	for _, deviceID := range in.DeviceAllowlist {
		if deviceID = strings.TrimSpace(deviceID); deviceID != "" {
			allowlist[deviceID] = struct{}{}
		}
	}
	port := in.OfferPort
	if port <= 0 || port > 65535 {
		port = 9
	}
	g.directPolicyMu.Lock()
	wasEnabled := g.directPolicy.Enabled
	g.directPolicy = directTCPRuntimePolicy{Enabled: in.Enabled, OfferPort: port, Allowlist: allowlist}
	g.directPolicyMu.Unlock()
	if wasEnabled && !in.Enabled && g.directDownloads != nil {
		g.directDownloads.CancelAll()
	}
	if g.directDownloads != nil && g.boot != nil {
		g.directDownloads.Reconfigure(directTCPDownloadOptions(g.boot))
	}
}

func (g *GB28181API) directTCPPolicySnapshot() directTCPRuntimePolicy {
	g.directPolicyMu.RLock()
	policy := g.directPolicy
	g.directPolicyMu.RUnlock()
	return policy
}

func directTCPDownloadOptions(cfg *conf.Bootstrap) DirectTCPDownloadOptions {
	in := cfg.Sip.DirectTCPDownload
	return DirectTCPDownloadOptions{
		StorageDir:           in.StorageDir,
		RetainDays:           in.RetainDays,
		MaxFileSize:          in.MaxFileSize,
		GlobalConcurrency:    in.GlobalConcurrency,
		DeviceConcurrency:    in.DeviceConcurrency,
		DialTimeout:          time.Duration(in.DialTimeout),
		FirstByteTimeout:     time.Duration(in.FirstByteTimeout),
		IdleTimeout:          time.Duration(in.IdleTimeout),
		TotalTimeout:         time.Duration(in.TotalTimeout),
		AllowAddressMismatch: in.AllowAddressMismatch,
		AllowedAddressCIDRs:  append([]string(nil), in.AllowedAddressCIDRs...),
	}
}

// filterUnknowDevices 国标 ID 校验，正常是长度为 20 的纯数字字符串
func filterUnknowDevices(deviceID string) error {
	if len(deviceID) != 20 {
		return fmt.Errorf("device id must be 20 digits")
	}
	// 验证必须全是数字
	for _, ch := range deviceID {
		if !unicode.IsNumber(ch) {
			return fmt.Errorf("device id must be all numbers")
		}
	}
	return nil
}

func (g *GB28181API) handlerRegister(ctx *sip.Context) {
	g.metrics.registerRequests.Add(1)
	if err := filterUnknowDevices(ctx.DeviceID); err != nil {
		slog.Error("过滤设备，拒绝注册", "device_id", ctx.DeviceID, "err", err)
		g.respondRegister(ctx, http.StatusBadRequest, err.Error())
		return
	}

	// 9.1.2.3 注册重定向：当网关层注入 X-GB-Redirect 时返回 302。
	// 示例值：sip:34020000002000000001@10.0.0.8:5060
	if redirect := strings.TrimSpace(ctx.GetHeader("X-GB-Redirect")); redirect != "" {
		uri, err := sip.ParseSipURI(redirect)
		if err != nil {
			g.respondRegister(ctx, http.StatusBadRequest, "invalid redirect uri")
			return
		}
		resp := g.newRegisterResponse(ctx, http.StatusFound, "Moved Temporarily")
		resp.AppendHeader(&sip.ContactHeader{
			DisplayName: sip.String{Str: "redirect"},
			Address:     &uri,
			Params:      sip.NewParams(),
		})
		_ = ctx.Tx.Respond(resp)
		return
	}

	var (
		dev      ipc.Device
		isNewDev bool
	)
	if err := g.core.Store().Device().Get(context.TODO(), &dev, orm.Where("device_id=?", ctx.DeviceID)); err != nil {
		if !orm.IsErrRecordNotFound(err) {
			ctx.Log.Error("GetDeviceByDeviceID", "err", err)
			g.respondRegister(ctx, http.StatusInternalServerError, "server db error")
			return
		}
		isNewDev = true
	}

	password := g.cfg.Password
	if !isNewDev {
		password = dev.Password
		if password == "" {
			password = g.cfg.Password
		}
		// 免鉴权
		if dev.Password == ignorePassword {
			password = ""
		}
	}

	if password != "" {
		hdrs := ctx.Request.GetHeaders("Authorization")
		if len(hdrs) == 0 {
			resp := g.newRegisterResponse(ctx, http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
			resp.AppendHeader(&sip.GenericHeader{HeaderName: "WWW-Authenticate", Contents: fmt.Sprintf(`Digest realm="%s",qop="auth",nonce="%s"`, g.cfg.Domain, sip.RandString(32))})
			_ = ctx.Tx.Respond(resp)
			return
		}
		authenticateHeader := hdrs[0].(*sip.GenericHeader)
		auth := sip.AuthFromValue(authenticateHeader.Contents)
		auth.SetPassword(password)
		if !isNewDev {
			auth.SetUsername(dev.GetGB28181DeviceID())
		} else {
			auth.SetUsername(ctx.DeviceID)
		}
		auth.SetMethod(ctx.Request.Method())
		auth.SetURI(auth.Get("uri"))
		if auth.CalcResponse() != auth.Get("response") {
			ctx.Log.Info("设备注册鉴权失败")
			g.respondRegister(ctx, http.StatusUnauthorized, "wrong password")
			return
		}
	}

	// 鉴权通过后，未知设备才自动建档
	if isNewDev {
		d, err := g.core.GetDeviceByDeviceID(ctx.DeviceID)
		if err != nil {
			ctx.Log.Error("create device by device_id failed", "err", err)
			g.respondRegister(ctx, http.StatusInternalServerError, "server db error")
			return
		}
		dev = *d
	}

	// 仅在通过校验后更新内存状态，避免未授权请求污染内存
	g.svr.memoryStorer.LoadOrStore(ctx.DeviceID, &Device{
		conn:   ctx.Request.GetConnection(),
		source: ctx.Source,
		to:     ctx.To,
	})

	respFn := func() {
		g.metrics.registerSuccess.Add(1)
		resp := g.newRegisterResponse(ctx, http.StatusOK, "OK")
		resp.AppendHeader(&sip.GenericHeader{
			HeaderName: "Date",
			Contents:   time.Now().Format("2006-01-02T15:04:05.000"),
		})
		_ = ctx.Tx.Respond(resp)
	}

	expire := ctx.GetHeader("Expires")
	if expire == "0" {
		ctx.Log.Info("设备注销")
		g.logout(ctx.DeviceID, func(b *ipc.Device) error {
			b.IsOnline = false
			b.Address = ctx.Source.String()
			if ctx.XGBVer != "" {
				applyGBProtocolVersion(&b.Ext, ctx.XGBVer)
			}
			return nil
		})
		respFn()
		return
	}

	effectiveVersion := applyGBProtocolVersion(&dev.Ext, ctx.XGBVer)
	ctx.Log = ctx.Log.With(
		"declared_version", ctx.XGBVerRaw,
		"effective_version", effectiveVersion,
		"version_source", dev.Ext.GBVersionSource,
	)
	if ctx.XGBVerRaw != "" && ctx.XGBVer == "" {
		ctx.Log.Warn("设备声明了未知协议版本，使用保守或已配置档案")
	}

	g.login(ctx, effectiveVersion, func(b *ipc.Device) error {
		b.IsOnline = true
		b.RegisteredAt = orm.Now()
		b.KeepaliveAt = orm.Now()
		b.Expires, _ = strconv.Atoi(expire)
		b.Address = ctx.Source.String()
		b.Transport = ctx.Source.Network()
		applyGBProtocolVersion(&b.Ext, ctx.XGBVer)
		return nil
	})

	// conn := ctx.Request.GetConnection()
	// fmt.Printf(">>> %p\n", conn

	ctx.Log.Info("设备注册成功")
	// ctx.Log.Debug("device info", "source", ctx.Source, "host", ctx.Host)

	respFn()

	ctx.XGBVer = string(effectiveVersion)
	g.QueryDeviceInfo(ctx)
	_ = g.QueryCatalog(dev.GetGB28181DeviceID())
	if effectiveVersion.Capabilities().ConfigQuery {
		_ = g.QueryConfigDownloadBasic(dev.GetGB28181DeviceID())
	}
}

func (g *GB28181API) login(ctx *sip.Context, version GBProtocolVersion, fn func(d *ipc.Device) error) {
	slog.Info("status change 设备上线", "device_id", ctx.DeviceID)
	g.svr.memoryStorer.Change(ctx.DeviceID, fn, func(d *Device) {
		d.conn = ctx.Request.GetConnection()
		d.source = ctx.Source
		d.to = ctx.To
		d.setGBVersion(version)
	})
}

func (g *GB28181API) newRegisterResponse(ctx *sip.Context, status int, reason string) *sip.Response {
	resp := sip.NewResponseFromRequest("", ctx.Request, status, reason, nil)
	version := sip.XGBVer(platformMaxGBVersion)
	resp.AppendHeader(&version)
	return resp
}

func (g *GB28181API) respondRegister(ctx *sip.Context, status int, reason string) {
	if status >= http.StatusBadRequest {
		g.metrics.registerFailures.Add(1)
	}
	_ = ctx.Tx.Respond(g.newRegisterResponse(ctx, status, reason))
}

func (g *GB28181API) logout(deviceID string, changeFn func(*ipc.Device) error) error {
	slog.Info("status change 设备离线", "device_id", deviceID)
	return g.svr.memoryStorer.Change(deviceID, changeFn, func(d *Device) {
		d.Expires = 0
		d.IsOnline = false
	})
}
