package gbs

import (
	"context"
	"crypto/sha256"
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
	registerResultTTL  = time.Minute
	maxRegisterResults = 4096
)

type registerNonceState struct {
	DeviceID            string
	SourceIP            string
	AcceptedFingerprint string
	IssuedAt            time.Time
	Expires             time.Time
}

type registerResultState struct {
	DeviceID  string
	Date      string
	ExpiresAt time.Time
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
	// registerCertificateAuth 实现 GB/T 28181-2016 Capability/Asymmetric REGISTER 认证。
	// 默认 nil，避免改变现有 Digest/免密码注册行为。
	registerCertificateAuth *registerCertificateAuthenticator
	// registerOperations 按设备串行化 REGISTER 的查询、自动建档和状态提交，不同设备仍可并行。
	registerOperationMu sync.Mutex
	registerOperations  map[string]*keyedOperationLock
	registerResultMu    sync.Mutex
	registerResults     map[[sha256.Size]byte]registerResultState
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
	// eventSubscriptionOps 按订阅键串行化创建、续订、取消和过期删除，无关对话可并行。
	eventSubscriptionMu  sync.Mutex
	eventSubscriptionOps map[string]*keyedOperationLock
	// 订阅方侧对话表，保证续订/取消复用 Call-ID、标签和递增 CSeq。
	outgoingSubscriptions sync.Map
	// 上级事件订阅到下级设备订阅的引用表；多个上级订阅可安全复用同一条下级对话。
	cascadeSubscriptionMu   sync.Mutex
	cascadeSubscriptions    map[string]*cascadeDownstreamSubscription
	cascadeSubscriptionOpMu sync.Mutex
	cascadeSubscriptionOps  map[string]*keyedOperationLock
	cascadeSubscribe        func(context.Context, *SubscribeInput) error
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
	cascadePlay            func(context.Context, *PlayInput) error
	cascadeStop            func(context.Context, *StopPlayInput) error
	cascadeHistory         func(context.Context, *HistoryInput) error
	cascadeStopHistory     func(context.Context, *StopHistoryInput) error
	cascadeControlHistory  func(context.Context, *ControlHistoryInput) error
	cascadeQueryRecords    func(context.Context, *RecordQueryInput) ([]RecordItem, error)
	cascadeDeviceQuery     func(context.Context, *DeviceQueryInput) (*DeviceQueryOutput, error)
	cascadeDeviceControl   func(context.Context, *ipc.Channel, *deviceControlA23Request) (string, error)
	cascadeDeviceConfig    func(context.Context, *ipc.Channel, *DeviceConfigRequest) (string, error)
	cascadeBroadcastNotify func(context.Context, *Channel, *VoiceInput) error
	// 2022 升级/抓拍的级联发起会话，key=kind:downstreamDeviceID:sessionID。
	cascadeTaskRoutes sync.Map
	// 设备控制命令全局序列号，避免 PTZ 与 DeviceControl 并发冲突。
	controlSN atomic.Uint32
	// 设备查询命令全局序列号，避免随机 SN 碰撞。
	querySN atomic.Uint32
	// directDownloads 管理 2014 附录 O 无 RTP 封装的 TCP 文件接收。
	directDownloads *DirectTCPDownloadManager
	directPolicyMu  sync.RWMutex
	directPolicy    directTCPRuntimePolicy
	metrics         GBMetrics
	closeBeginOnce  sync.Once
	closeOnce       sync.Once
	lifecycleMu     sync.Mutex
	lifecycleDone   chan struct{}
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	lifecycleClosed bool
	lifecycleWG     sync.WaitGroup
	requestWG       sync.WaitGroup

	svr *Server

	sms rtpMediaService
}

func NewGB28181API(cfg *conf.Bootstrap, store ipc.Adapter, sms *sms.NodeManager) *GB28181API {
	sipConfig := cfg.Sip
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
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
		lifecycleCtx:         lifecycleCtx,
		lifecycleCancel:      lifecycleCancel,
	}
	g.controlSN.Store(uint32(sip.RandInt(100000, 999999)))
	g.querySN.Store(uint32(sip.RandInt(100000, 999999)))
	g.applyDirectTCPConfig(cfg.Sip.DirectTCPDownload)
	g.startLifecycleWorker(g.startEventSubscriberCleaner)
	g.startLifecycleWorker(g.startInviteDialogCleaner)
	g.startLifecycleWorker(g.startRuntimeStateCleaner)
	return &g
}

func (g *GB28181API) startLifecycleWorker(worker func()) bool {
	if g == nil || worker == nil {
		return false
	}
	g.lifecycleMu.Lock()
	if g.lifecycleClosed {
		g.lifecycleMu.Unlock()
		return false
	}
	g.lifecycleWG.Add(1)
	g.lifecycleMu.Unlock()
	go func() {
		defer g.lifecycleWG.Done()
		worker()
	}()
	return true
}

func (g *GB28181API) beginLifecycleRequest() (func(), bool) {
	if g == nil {
		return nil, false
	}
	g.lifecycleMu.Lock()
	if g.lifecycleClosed {
		g.lifecycleMu.Unlock()
		return nil, false
	}
	g.requestWG.Add(1)
	g.lifecycleMu.Unlock()
	var once sync.Once
	return func() { once.Do(g.requestWG.Done) }, true
}

// sipLifecycleMiddleware 将已接纳的入向 SIP 请求纳入 GB28181 关闭等待，
// 并在停服开始后拒绝新业务，避免清理完成后又写回会话或订阅状态。
func (g *GB28181API) sipLifecycleMiddleware(ctx *sip.Context) {
	if ctx == nil {
		return
	}
	done, ok := g.beginLifecycleRequest()
	if !ok {
		if ctx.Request != nil && ctx.Request.Method() == sip.MethodACK {
			ctx.Abort()
			return
		}
		ctx.AbortString(http.StatusServiceUnavailable, ErrServiceStopped.Error())
		return
	}
	defer done()
	ctx.Next()
}

// startLifecycleTask 启动随 GB28181 服务取消并由 close 等待的短任务。
// lifecycleMu 保证 WaitGroup.Add 不会和 close 中的 Wait 并发。
func (g *GB28181API) startLifecycleTask(parent context.Context, task func(context.Context)) bool {
	if g == nil || task == nil {
		return false
	}
	if parent == nil {
		parent = context.Background()
	}
	g.lifecycleMu.Lock()
	if g.lifecycleClosed {
		g.lifecycleMu.Unlock()
		return false
	}
	if g.lifecycleCtx == nil {
		g.lifecycleCtx, g.lifecycleCancel = context.WithCancel(context.Background())
	}
	lifecycleCtx := g.lifecycleCtx
	g.lifecycleWG.Add(1)
	g.lifecycleMu.Unlock()

	taskCtx, cancel := context.WithCancel(parent)
	stopLifecycleCancel := context.AfterFunc(lifecycleCtx, cancel)
	go func() {
		defer g.lifecycleWG.Done()
		defer stopLifecycleCancel()
		defer cancel()
		task(taskCtx)
	}()
	return true
}

func (g *GB28181API) serviceDone() <-chan struct{} {
	if g == nil {
		return nil
	}
	return g.lifecycleDone
}

func (g *GB28181API) serviceStopped() bool {
	if g == nil {
		return false
	}
	g.lifecycleMu.Lock()
	stopped := g.lifecycleClosed
	g.lifecycleMu.Unlock()
	return stopped
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
	hash := sha256.New()
	if request != nil {
		_, _ = hash.Write([]byte(request.String()))
	}
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(response))
	return string(hash.Sum(nil))
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
	unlockRegister := g.lockRegisterOperation(ctx.DeviceID)
	defer unlockRegister()
	requestKey := registerResultKey(ctx)
	respondOK := func(date string) {
		g.metrics.registerSuccess.Add(1)
		resp := g.newRegisterResponse(ctx, http.StatusOK, "OK")
		resp.AppendHeader(&sip.GenericHeader{
			HeaderName: "Date",
			Contents:   date,
		})
		_ = ctx.Tx.Respond(resp)
	}
	if cached, ok := g.loadRegisterResult(requestKey, time.Now()); ok {
		respondOK(cached.Date)
		return
	}
	respFn := func() {
		now := time.Now()
		state := registerResultState{
			DeviceID:  ctx.DeviceID,
			Date:      sip.FormatGBTime(now, "2006-01-02T15:04:05.000"),
			ExpiresAt: now.Add(registerResultTTL),
		}
		g.storeRegisterResult(requestKey, state, now)
		respondOK(state.Date)
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

	hdrs := ctx.Request.GetHeaders("Authorization")
	certificateAuthenticated, stop := g.authenticateRegisterCertificate(ctx, hdrs)
	if stop {
		return
	}
	if password != "" && !certificateAuthenticated {
		if len(hdrs) == 0 {
			g.respondRegisterChallenge(ctx)
			return
		}
		if len(hdrs) != 1 {
			g.respondRegister(ctx, http.StatusBadRequest, "REGISTER requires exactly one Authorization header")
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
	// 注销不存在的绑定应在鉴权通过后保持幂等，不能绕过鉴权或反向创建设备档案。
	if isNewDev && expires == 0 {
		ctx.Log.Info("忽略未知设备注销")
		respFn()
		return
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

	if expires == 0 {
		ctx.Log.Info("设备注销")
		if err := g.logout(ctx.DeviceID, func(b *ipc.Device) error {
			b.IsOnline = false
			b.Address = ctx.Source.String()
			if ctx.XGBVer != "" {
				applyGBProtocolVersion(&b.Ext, ctx.XGBVer)
			}
			return nil
		}); err != nil {
			ctx.Log.Error("设备注销状态持久化失败", "err", err)
			g.respondRegister(ctx, http.StatusInternalServerError, "server db error")
			return
		}
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

	if err := g.login(ctx, effectiveVersion, dev.Ext.GBDisabledCapabilities, func(b *ipc.Device) error {
		b.IsOnline = true
		b.RegisteredAt = orm.Now()
		b.KeepaliveAt = orm.Now()
		b.Expires = expires
		b.Address = ctx.Source.String()
		b.Transport = requestSignalingTransport(ctx)
		applyGBProtocolVersion(&b.Ext, ctx.XGBVer)
		return nil
	}); err != nil {
		ctx.Log.Error("设备注册状态持久化失败", "err", err)
		g.respondRegister(ctx, http.StatusInternalServerError, "server db error")
		return
	}

	// conn := ctx.Request.GetConnection()
	// fmt.Printf(">>> %p\n", conn

	ctx.Log.Info("设备注册成功")
	// ctx.Log.Debug("device info", "source", ctx.Source, "host", ctx.Host)

	respFn()
	unlockRegister()
	// 注册历史不是 REGISTER 成功事务的一部分，避免其查询、写入和清理延迟 200 OK。
	if history := g.core.DeviceHistory(); history != nil {
		if err := history.Record(context.TODO(), ctx.DeviceID, ipc.DeviceHistoryRegister, ctx.Source.String(), "online", time.Now()); err != nil {
			ctx.Log.Error("持久化设备注册历史失败", "err", err)
		}
	}

	ctx.XGBVer = string(effectiveVersion)
	g.QueryDeviceInfo(ctx)
	_ = g.QueryCatalog(dev.GetGB28181DeviceID())
	if g.deviceSupportsGBFeature(dev.GetGB28181DeviceID(), "config_query", effectiveVersion, func(c GBCapabilities) bool { return c.ConfigQuery }) {
		_ = g.QueryConfigDownloadBasic(dev.GetGB28181DeviceID())
	}
}

func requestSignalingTransport(ctx *sip.Context) string {
	if ctx == nil || ctx.Request == nil {
		return ""
	}
	if transport := strings.ToLower(sip.SignalingTransport(ctx.Request.GetConnection())); transport != "" {
		return transport
	}
	if ctx.Source != nil {
		return strings.ToLower(strings.TrimSpace(ctx.Source.Network()))
	}
	return ""
}

func registerResultKey(ctx *sip.Context) [sha256.Size]byte {
	hash := sha256.New()
	if ctx != nil && ctx.Request != nil {
		_, _ = hash.Write([]byte(ctx.Request.String()))
	}
	_, _ = hash.Write([]byte{0})
	if ctx != nil && ctx.Source != nil {
		_, _ = hash.Write([]byte(ctx.Source.String()))
	}
	var out [sha256.Size]byte
	copy(out[:], hash.Sum(nil))
	return out
}

func (g *GB28181API) loadRegisterResult(key [sha256.Size]byte, now time.Time) (registerResultState, bool) {
	g.registerResultMu.Lock()
	defer g.registerResultMu.Unlock()
	state, ok := g.registerResults[key]
	if !ok {
		return registerResultState{}, false
	}
	if !state.ExpiresAt.After(now) {
		delete(g.registerResults, key)
		return registerResultState{}, false
	}
	return state, true
}

func (g *GB28181API) storeRegisterResult(key [sha256.Size]byte, state registerResultState, now time.Time) {
	g.registerResultMu.Lock()
	defer g.registerResultMu.Unlock()
	if g.registerResults == nil {
		g.registerResults = make(map[[sha256.Size]byte]registerResultState)
	}
	if len(g.registerResults) >= maxRegisterResults {
		for currentKey, current := range g.registerResults {
			if !current.ExpiresAt.After(now) {
				delete(g.registerResults, currentKey)
			}
		}
	}
	if len(g.registerResults) >= maxRegisterResults {
		var oldestKey [sha256.Size]byte
		oldestAt := time.Time{}
		for currentKey, current := range g.registerResults {
			if oldestAt.IsZero() || current.ExpiresAt.Before(oldestAt) {
				oldestKey = currentKey
				oldestAt = current.ExpiresAt
			}
		}
		delete(g.registerResults, oldestKey)
	}
	g.registerResults[key] = state
}

func (g *GB28181API) lockRegisterOperation(deviceID string) func() {
	g.registerOperationMu.Lock()
	if g.registerOperations == nil {
		g.registerOperations = make(map[string]*keyedOperationLock)
	}
	entry := g.registerOperations[deviceID]
	if entry == nil {
		entry = &keyedOperationLock{}
		g.registerOperations[deviceID] = entry
	}
	entry.refs++
	g.registerOperationMu.Unlock()
	entry.mutex.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mutex.Unlock()
			g.registerOperationMu.Lock()
			if g.registerOperations[deviceID] == entry {
				entry.refs--
				if entry.refs == 0 {
					delete(g.registerOperations, deviceID)
				}
			}
			g.registerOperationMu.Unlock()
		})
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

func (g *GB28181API) login(ctx *sip.Context, version GBProtocolVersion, disabledCapabilities []string, fn func(d *ipc.Device) error) error {
	slog.Info("status change 设备上线", "device_id", ctx.DeviceID)
	return g.svr.memoryStorer.Change(ctx.DeviceID, fn, func(d *Device) {
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
	err := g.svr.memoryStorer.Change(deviceID, changeFn, func(d *Device) {
		d.Expires = 0
		d.IsOnline = false
	})
	if err == nil {
		slog.Info("status change 设备离线", "device_id", deviceID)
	}
	return err
}
