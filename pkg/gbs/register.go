package gbs

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
)

const ignorePassword = "#"

const defaultRegisterExpires = 86400

const (
	registerNonceTTL   = 5 * time.Minute
	maxRegisterNonces  = 4096
	registerDigestAlgo = "MD5"
)

type registerNonceState struct {
	DeviceID            string
	SourceIP            string
	AcceptedFingerprint string
	IssuedAt            time.Time
	Expires             time.Time
}

type GB28181API struct {
	cfgMu sync.RWMutex
	cfg   *conf.SIP
	boot  *conf.Bootstrap
	core  ipc.Adapter

	catalogResponses *multiResponseCollector[Channels]
	recordResponses  *multiResponseCollector[RecordItem]
	// key=deviceID:SN，映射到 RecordInfo 的通道聚合键，兼容设备回写设备 ID。
	recordResponseAliases sync.Map
	// REGISTER Digest nonce 由服务端签发并绑定设备和源 IP，避免接受任意或永久可重放的 nonce。
	registerNonceMu sync.Mutex
	registerNonces  map[string]registerNonceState
	// MESSAGE/NOTIFY 兼容 Digest nonce 独立维护，按设备、源 IP 和 nc 防重放。
	messageNonceMu sync.Mutex
	messageNonces  map[string]messageNonceState

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
	// eventSubscriptionMu 串行化订阅创建、续订、取消和过期删除，避免续订与清理互相覆盖。
	eventSubscriptionMu sync.Mutex
	// 订阅方侧对话表，保证续订/取消复用 Call-ID、标签和递增 CSeq。
	outgoingSubscriptions sync.Map
	// 上级事件订阅到下级设备订阅的引用表；多个上级订阅可安全复用同一条下级对话。
	cascadeSubscriptionMu sync.Mutex
	cascadeSubscriptions  map[string]*cascadeDownstreamSubscription
	cascadeSubscribe      func(context.Context, *SubscribeInput) error
	// key=deviceID:sn，用于等待 DeviceConfig 业务应答（9.7/9.14）。
	pendingDeviceConfig sync.Map
	// 设备软件升级状态（2022 9.13/A.2.5.9），key=deviceID:sessionID。
	upgradeStateMu sync.RWMutex
	upgradeStates  map[string]UpgradeState
	// 图像抓拍状态（2022 9.14/A.2.5.7），key=deviceID:sessionID。
	snapshotStateMu sync.RWMutex
	snapshotStates  map[string]SnapshotState
	// key=TargetID:SN，用于等待 2014 Broadcast 业务应答。
	pendingBroadcast sync.Map
	// key=目标通道 ID/设备 ID，保存等待接收端主动 INVITE 的 2014 广播会话。
	broadcastSessions sync.Map
	// key=上游音频源 Call-ID，保存级联广播的上游 INVITE 对话。
	cascadeVoiceDialogs sync.Map
	// key=媒体接收流 ID，保存等待设备音频流建立的 2016/2022 对讲会话。
	talkSessions sync.Map
	// key=history:Download:deviceID:channelID，保存普通 RTP 下载进度快照。
	rtpDownloads sync.Map
	// key=deviceID，保存结构化查询/状态结果（9.5/9.6/A.2.4）。
	queryStateMu sync.RWMutex
	queryStates  sync.Map
	// key=Call-ID，保存入向 INVITE 会话状态（9.2 被叫侧会话）。
	inviteDialogs sync.Map
	// 级联实时点播复用同一通道的下级媒体源，并按会话引用计数释放。
	cascadeMediaMu         sync.Mutex
	cascadeSources         map[string]*cascadeSourceRef
	cascadePlay            func(*PlayInput) error
	cascadeStop            func(context.Context, *StopPlayInput) error
	cascadeHistory         func(context.Context, *HistoryInput) error
	cascadeStopHistory     func(context.Context, *StopHistoryInput) error
	cascadeControlHistory  func(context.Context, *ControlHistoryInput) error
	cascadeQueryRecords    func(context.Context, *RecordQueryInput) ([]RecordItem, error)
	cascadeDeviceQuery     func(context.Context, *DeviceQueryInput) (*DeviceQueryOutput, error)
	cascadeDeviceControl   func(context.Context, *ipc.Channel, *deviceControlA23Request) (string, error)
	cascadeBroadcastNotify func(context.Context, *Channel, *VoiceInput) error
	// 设备控制命令全局序列号，避免 PTZ 与 DeviceControl 并发冲突。
	controlSN atomic.Uint32
	// 设备查询命令全局序列号，避免随机 SN 碰撞。
	querySN atomic.Uint32
	// directDownloads 管理 2014 附录 O 无 RTP 封装的 TCP 文件接收。
	directDownloads *DirectTCPDownloadManager
	directPolicyMu  sync.RWMutex
	directPolicy    directTCPRuntimePolicy
	metrics         GBMetrics
	closeOnce       sync.Once
	lifecycleDone   chan struct{}

	svr *Server

	sms rtpMediaService
}

func NewGB28181API(cfg *conf.Bootstrap, store ipc.Adapter, sms *sms.NodeManager) *GB28181API {
	sipConfig := cfg.Sip
	g := GB28181API{
		cfg:  &sipConfig,
		boot: cfg,
		core: store,
		sms:  sms,
		catalogResponses: newMultiResponseCollector(func(item Channels) string {
			return item.ChannelID
		}),
		recordResponses: newMultiResponseCollector(func(item RecordItem) string {
			return item.DeviceID + "\x00" + item.FilePath + "\x00" + item.StartTime + "\x00" + item.EndTime
		}),
		registerNonces:       make(map[string]registerNonceState),
		messageNonces:        make(map[string]messageNonceState),
		streams:              &conc.Map[string, *Streams]{},
		cascadeSources:       make(map[string]*cascadeSourceRef),
		cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription),
		upgradeStates:        make(map[string]UpgradeState),
		snapshotStates:       make(map[string]SnapshotState),
		directDownloads:      NewDirectTCPDownloadManager(directTCPDownloadOptions(cfg.Sip.DirectTCPDownload)),
		lifecycleDone:        make(chan struct{}),
	}
	g.controlSN.Store(uint32(sip.RandInt(100000, 999999)))
	g.querySN.Store(uint32(sip.RandInt(100000, 999999)))
	g.applyDirectTCPConfig(cfg.Sip.DirectTCPDownload)
	go g.startEventSubscriberCleaner()
	go g.startInviteDialogCleaner()
	go g.startRuntimeStateCleaner()
	return &g
}

func (g *GB28181API) serviceDone() <-chan struct{} {
	if g == nil {
		return nil
	}
	return g.lifecycleDone
}

func (g *GB28181API) serviceStopped() bool {
	done := g.serviceDone()
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func (g *GB28181API) configSnapshot() *conf.SIP {
	if g == nil {
		return nil
	}
	g.cfgMu.RLock()
	defer g.cfgMu.RUnlock()
	if g.cfg == nil {
		return nil
	}
	cfg := *g.cfg
	return &cfg
}

func (g *GB28181API) setConfig(cfg conf.SIP) {
	if g == nil {
		return
	}
	g.cfgMu.Lock()
	g.cfg = &cfg
	g.cfgMu.Unlock()
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
}

func (g *GB28181API) directTCPPolicySnapshot() directTCPRuntimePolicy {
	g.directPolicyMu.RLock()
	policy := g.directPolicy
	g.directPolicyMu.RUnlock()
	return policy
}

func directTCPDownloadOptions(in conf.SIPDirectTCPDownload) DirectTCPDownloadOptions {
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
	for i := 0; i < len(deviceID); i++ {
		if deviceID[i] < '0' || deviceID[i] > '9' {
			return fmt.Errorf("device id must be all numbers")
		}
	}
	return nil
}

func (g *GB28181API) respondRegisterChallenge(ctx *sip.Context) {
	cfg := g.configSnapshot()
	domain := ""
	if cfg != nil {
		domain = cfg.GetDomain()
	}
	nonce := g.issueRegisterNonce(ctx.DeviceID, registerNonceSourceIP(ctx))
	resp := g.newRegisterResponse(ctx, http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
	resp.AppendHeader(&sip.GenericHeader{
		HeaderName: "WWW-Authenticate",
		Contents:   fmt.Sprintf(`Digest realm="%s",qop="auth",nonce="%s"`, domain, nonce),
	})
	_ = ctx.Tx.Respond(resp)
}

func (g *GB28181API) issueRegisterNonce(deviceID, sourceIP string) string {
	now := time.Now()
	g.registerNonceMu.Lock()
	defer g.registerNonceMu.Unlock()
	if g.registerNonces == nil {
		g.registerNonces = make(map[string]registerNonceState)
	}
	var oldestKey string
	var oldest time.Time
	for nonce, state := range g.registerNonces {
		if !state.Expires.After(now) {
			delete(g.registerNonces, nonce)
			continue
		}
		if oldestKey == "" || state.IssuedAt.Before(oldest) {
			oldestKey = nonce
			oldest = state.IssuedAt
		}
	}
	if len(g.registerNonces) >= maxRegisterNonces && oldestKey != "" {
		delete(g.registerNonces, oldestKey)
	}
	for {
		nonce := sip.RandString(32)
		if _, exists := g.registerNonces[nonce]; exists {
			continue
		}
		g.registerNonces[nonce] = registerNonceState{
			DeviceID: strings.TrimSpace(deviceID),
			SourceIP: strings.TrimSpace(sourceIP),
			IssuedAt: now,
			Expires:  now.Add(registerNonceTTL),
		}
		return nonce
	}
}

func (g *GB28181API) validateRegisterNonce(nonce, deviceID, sourceIP string) error {
	now := time.Now()
	g.registerNonceMu.Lock()
	defer g.registerNonceMu.Unlock()
	state, ok := g.registerNonces[nonce]
	if !ok {
		return fmt.Errorf("Digest nonce was not issued by this server")
	}
	if !state.Expires.After(now) {
		delete(g.registerNonces, nonce)
		return fmt.Errorf("Digest nonce expired")
	}
	if state.DeviceID != strings.TrimSpace(deviceID) {
		return fmt.Errorf("Digest nonce device mismatch")
	}
	if state.SourceIP != "" && state.SourceIP != strings.TrimSpace(sourceIP) {
		return fmt.Errorf("Digest nonce source mismatch")
	}
	return nil
}

func (g *GB28181API) acceptRegisterNonce(nonce, fingerprint string) error {
	g.registerNonceMu.Lock()
	defer g.registerNonceMu.Unlock()
	state, ok := g.registerNonces[nonce]
	if !ok || !state.Expires.After(time.Now()) {
		delete(g.registerNonces, nonce)
		return fmt.Errorf("Digest nonce expired")
	}
	if state.AcceptedFingerprint != "" && state.AcceptedFingerprint != fingerprint {
		return fmt.Errorf("Digest nonce replay detected")
	}
	state.AcceptedFingerprint = fingerprint
	g.registerNonces[nonce] = state
	return nil
}

func registerNonceSourceIP(ctx *sip.Context) string {
	if ctx == nil {
		return ""
	}
	return parseAddressIP(addrString(ctx.Source))
}

func (g *GB28181API) validateRegisterAuthorization(ctx *sip.Context, header sip.Header, username, password string) error {
	if ctx == nil || ctx.Request == nil {
		return fmt.Errorf("invalid REGISTER request")
	}
	generic, ok := header.(*sip.GenericHeader)
	if !ok || generic == nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(generic.Contents)), "digest ") {
		return fmt.Errorf("invalid Authorization header")
	}
	cfg := g.configSnapshot()
	if cfg == nil {
		return fmt.Errorf("SIP configuration is unavailable")
	}
	auth := sip.AuthFromValue(generic.Contents)
	if auth.Get("realm") != cfg.GetDomain() {
		return fmt.Errorf("Digest realm mismatch")
	}
	if !strings.EqualFold(auth.Algorithm(), registerDigestAlgo) {
		return fmt.Errorf("unsupported REGISTER Digest algorithm %q", auth.Algorithm())
	}
	if auth.Get("username") != username {
		return fmt.Errorf("Digest username mismatch")
	}
	if ctx.Request.Recipient() == nil {
		return fmt.Errorf("REGISTER request URI is missing")
	}
	requestURI := ctx.Request.Recipient().String()
	if auth.Get("uri") != requestURI {
		return fmt.Errorf("Digest uri mismatch")
	}
	if auth.QOP() == "auth" {
		if len(auth.Get("nc")) != 8 || strings.TrimSpace(auth.Get("cnonce")) == "" {
			return fmt.Errorf("REGISTER Digest qop=auth requires 8-digit nc and cnonce")
		}
		if _, err := hex.DecodeString(auth.Get("nc")); err != nil {
			return fmt.Errorf("invalid Digest nc")
		}
	} else if auth.QOP() != "" {
		return fmt.Errorf("unsupported REGISTER Digest qop %q", auth.QOP())
	}
	if err := g.validateRegisterNonce(auth.Get("nonce"), username, registerNonceSourceIP(ctx)); err != nil {
		return err
	}
	provided := strings.ToLower(strings.TrimSpace(auth.Get("response")))
	if provided == "" {
		return fmt.Errorf("Digest response is missing")
	}
	auth.SetPassword(password).SetUsername(username).SetMethod(ctx.Request.Method()).SetURI(requestURI)
	calculated, err := auth.CalcResponseChecked()
	if err != nil {
		return err
	}
	if len(provided) != len(calculated) || subtle.ConstantTimeCompare([]byte(provided), []byte(calculated)) != 1 {
		return fmt.Errorf("Digest response mismatch")
	}
	return g.acceptRegisterNonce(auth.Get("nonce"), registerRequestFingerprint(ctx.Request, provided))
}

func registerRequestFingerprint(request *sip.Request, response string) string {
	callID := ""
	if value, ok := request.CallID(); ok && value != nil {
		callID = value.String()
	}
	sequence := ""
	if value, ok := request.CSeq(); ok && value != nil {
		sequence = fmt.Sprintf("%d:%s", value.SeqNo, value.MethodName)
	}
	return callID + "\x00" + sequence + "\x00" + response
}

func (g *GB28181API) handlerRegister(ctx *sip.Context) {
	g.metrics.registerRequests.Add(1)
	cfg := g.configSnapshot()
	if cfg == nil {
		g.respondRegister(ctx, http.StatusServiceUnavailable, "SIP configuration is unavailable")
		return
	}
	if err := filterUnknowDevices(ctx.DeviceID); err != nil {
		slog.Error("过滤设备，拒绝注册", "device_id", ctx.DeviceID, "err", err)
		g.respondRegister(ctx, http.StatusBadRequest, err.Error())
		return
	}
	// Request-URI 的 user 应为平台 SIP ID。非空且不匹配时尽早拒绝，避免后续信令静默失败。
	if recipient := ctx.Request.Recipient(); recipient != nil {
		if user := recipient.User(); user != nil && user.String() != "" && user.String() != cfg.ID {
			g.respondRegister(ctx, http.StatusForbidden, fmt.Sprintf("server id mismatch, expect %s got %s", cfg.ID, user.String()))
			return
		}
	}
	expires, err := parseRegisterExpires(ctx)
	if err != nil {
		g.respondRegister(ctx, http.StatusBadRequest, err.Error())
		return
	}

	// 9.1.2.3 注册重定向。目标只读取服务端配置，不能信任设备请求头，避免形成开放重定向。
	// 示例值：sip:34020000002000000001@10.0.0.8:5060
	if redirect := strings.TrimSpace(cfg.RegisterRedirect); redirect != "" && ctx.XGBVer == string(GBVersion30) {
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

	password := cfg.Password
	if !isNewDev {
		password = dev.Password
		if password == "" {
			password = cfg.Password
		}
		// 免鉴权
		if dev.Password == ignorePassword {
			password = ""
		}
	}

	if password != "" {
		hdrs := ctx.Request.GetHeaders("Authorization")
		if len(hdrs) == 0 {
			g.respondRegisterChallenge(ctx)
			return
		}
		username := ctx.DeviceID
		if !isNewDev {
			username = dev.GetGB28181DeviceID()
		}
		if err := g.validateRegisterAuthorization(ctx, hdrs[0], username, password); err != nil {
			ctx.Log.Info("设备注册鉴权失败", "err", err)
			g.respondRegisterChallenge(ctx)
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

	if expires == 0 {
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

	g.login(ctx, effectiveVersion, dev.Ext.GBDisabledCapabilities, func(b *ipc.Device) error {
		b.IsOnline = true
		b.RegisteredAt = orm.Now()
		b.KeepaliveAt = orm.Now()
		b.Expires = expires
		b.Address = ctx.Source.String()
		b.Transport = ctx.Source.Network()
		applyGBProtocolVersion(&b.Ext, ctx.XGBVer)
		return nil
	})
	if history := g.core.DeviceHistory(); history != nil {
		if err := history.Record(context.TODO(), ctx.DeviceID, ipc.DeviceHistoryRegister, ctx.Source.String(), "online", time.Now()); err != nil {
			ctx.Log.Error("持久化设备注册历史失败", "err", err)
		}
	}

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

func parseRegisterExpires(ctx *sip.Context) (int, error) {
	if ctx == nil || ctx.Request == nil {
		return 0, fmt.Errorf("invalid REGISTER request")
	}
	// RFC 3261 10.2.1: Contact 的 expires 参数作用于该注册绑定，并覆盖 Expires 头的默认值。
	value := ""
	if contact, ok := ctx.Request.Contact(); ok && contact != nil && contact.Params != nil {
		if expires, ok := contact.Params.Get("expires"); ok && expires != nil {
			value = strings.TrimSpace(expires.String())
		}
	}
	if value == "" {
		value = strings.TrimSpace(ctx.GetHeader("Expires"))
	}
	if value == "" {
		return defaultRegisterExpires, nil
	}
	expires, err := strconv.Atoi(value)
	if err != nil || expires < 0 {
		return 0, fmt.Errorf("invalid REGISTER expires: %s", value)
	}
	return expires, nil
}

func (g *GB28181API) login(ctx *sip.Context, version GBProtocolVersion, disabledCapabilities []string, fn func(d *ipc.Device) error) {
	slog.Info("status change 设备上线", "device_id", ctx.DeviceID)
	g.svr.memoryStorer.Change(ctx.DeviceID, fn, func(d *Device) {
		d.conn = ctx.Request.GetConnection()
		d.source = ctx.Source
		d.to = ctx.To
		d.setGBProfile(version, disabledCapabilities)
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
