package gbs

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
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
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
)

const ignorePassword = "#"

const (
	defaultRegisterExpires           = 86400
	minimumStandardRegisterTTL       = 3600
	maximumRegisterExpires     int64 = 1<<32 - 1
	statusIntervalTooBrief           = 423
)

const (
	registerNonceTTL   = 5 * time.Minute
	maxRegisterNonces  = 4096
	registerDigestAlgo = "MD5"
	registerResultTTL  = time.Minute
	maxRegisterResults = 4096
	// gbShutdownPersistenceTimeout 限制停服阶段媒体状态最佳努力落库的总窗口。
	gbShutdownPersistenceTimeout = 5 * time.Second
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
	Pending   bool
}

var errRegisterCSeqOutOfOrder = errors.New("REGISTER CSeq is out of order")

type GB28181API struct {
	cfgMu sync.RWMutex
	cfg   *conf.SIP
	boot  *conf.Bootstrap
	core  ipc.Adapter
	// recordingStore 提供平台中心录像目录，供上级 RecordInfo 查询使用。
	recordingStore recording.RecordingStorer

	catalogResponses *multiResponseCollector[Channels]
	recordResponses  *multiResponseCollector[RecordItem]
	// catalogOperations 按设备串行化完整 Catalog 查询，避免较早请求迟到后覆盖较新快照。
	catalogOperationMu sync.Mutex
	catalogOperations  map[string]*keyedOperationLock
	// catalogRefreshes 合并同一设备触发的完整刷新任务。
	catalogRefreshMu sync.Mutex
	catalogRefreshes map[string]*catalogRefreshState
	// key=deviceID:SN，映射到 RecordInfo 的具体查询代次，兼容设备回写设备 ID。
	recordResponseAliases sync.Map
	// recordResponseExtra/recordResponseXML/recordResponseAppendixA4 保存同一 RecordInfo 分包查询的响应元数据。
	recordResponseExtraMu     sync.Mutex
	recordResponseGenerations map[string]*recordResponseGeneration
	recordResponseExtra       map[string][]string
	recordResponseXML         map[string][]string
	recordResponseAppendixA4  map[string][]AppendixA4Object
	recordResponseMetadata    map[string]*recordResponseMetadataAccumulator
	// REGISTER Digest nonce 由服务端签发并绑定设备和源 IP，避免接受任意或永久可重放的 nonce。
	registerNonceMu sync.Mutex
	registerNonces  map[string]registerNonceState
	// registerCertificateAuth 实现 2011/2014/2016 Capability/Asymmetric REGISTER 认证。
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
	// annexG 是默认关闭的附录 G 静态外部系统运行时。
	annexG *annexGService

	// TODO: 待替换成 redis
	streams *conc.Map[string, *Streams]
	// key=deviceID:sn，用于等待 DeviceControl 的业务响应。
	pendingDeviceControl sync.Map
	// key=deviceID:cmdType:sn，用于等待设备查询响应。
	pendingDeviceQuery sync.Map
	// key=deviceID:cmdType:sn，用于取消 Catalog/RecordInfo 的 SIP 确认及分包等待。
	pendingMultiResponse sync.Map
	// key=*pendingDeviceOperation，跟踪仅等待 SIP 最终响应的设备请求。
	pendingDeviceRequests sync.Map
	// key=上级平台+目标编码，保存 MobilePosition Query 触发的级联通知路由。
	cascadeMobilePositionQueries sync.Map
	// 报警回调，供上层业务（事件中心）注册。
	alarmHandlerMu        sync.RWMutex
	alarmHandler          func(context.Context, *AlarmEvent) error
	alarmInboxWake        chan struct{}
	alarmInboxWorkerOnce  sync.Once
	alarmInboxOperationMu sync.Mutex
	alarmInboxOperations  map[string]*keyedOperationLock
	// 2022 VideoUploadNotify 先进入持久化 outbox，再由后台工作器转发到 3.0 上级。
	videoUploadOutboxWake       chan struct{}
	videoUploadOutboxWorkerOnce sync.Once
	// key=源设备+原始 VideoUploadNotify+上级平台的稳定投递键，防止换事务重传重复转发。
	videoUploadReceiptMu       sync.Mutex
	videoUploadReceipts        map[string]time.Time
	videoUploadPendingReceipts map[string]time.Time
	// key=上级 worker+转发 SN+上级可见通道 ID，用于等待 9.4 Alarm 业务应答。
	pendingAlarmDispatch sync.Map
	// key=本域接警终端 ID+转发 SN+报警源 ID，用于等待 9.4 Alarm 业务应答。
	pendingLocalAlarmDispatch sync.Map
	// 事件源侧订阅表（9.11），用于向订阅方发送 NOTIFY。
	eventSubscribers sync.Map
	// eventNotifyRetryWait 仅供确定性测试压缩重试等待，生产为 nil 时使用标准退避/Retry-After。
	eventNotifyRetryWait func(int, *sip.Response) time.Duration
	// eventSubscriptionOps 按订阅键串行化创建、续订、取消和过期删除，无关对话可并行。
	eventSubscriptionMu  sync.Mutex
	eventSubscriptionOps map[string]*keyedOperationLock
	// 订阅方侧对话表，保证续订/取消复用 Call-ID、标签和递增 CSeq。
	outgoingSubscriptions      sync.Map
	manualSubscriptionIntentMu sync.Mutex
	// taskStateOperations 串行化同一长周期任务持久键的保存、删除与启动恢复，
	// 避免恢复列表的旧快照覆盖服务启动后已经写入的新会话状态。
	taskStateOperationMu sync.Mutex
	taskStateOperations  map[string]*keyedOperationLock
	// key=人工订阅持久化键；终止策略落库失败时阻止旧意图被恢复，并由恢复线程重试。
	pendingManualSubscriptionTerminations sync.Map
	manualSubscriptionRecoveryWake        chan string
	// 上级事件订阅到下级设备订阅的引用表；多个上级订阅可安全复用同一条下级对话。
	cascadeSubscriptionMu   sync.Mutex
	cascadeSubscriptions    map[string]*cascadeDownstreamSubscription
	cascadeSubscriptionOpMu sync.Mutex
	cascadeSubscriptionOps  map[string]*keyedOperationLock
	cascadeSubscribe        func(context.Context, *SubscribeInput) error
	manualSubscribeRefresh  func(context.Context, *SubscribeInput) error
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
	// key=*cascadeVoiceSourceSession，保存尚未建立 Call-ID 或已脱离活动对话、但 RTP/BYE 清理未完成的级联语音源。
	pendingCascadeVoiceCleanups sync.Map
	// key=媒体接收流 ID，保存等待设备音频流建立的 2016/2022 对讲会话。
	talkSessions sync.Map
	// key=history:Download:deviceID:channelID，保存普通 RTP 下载进度快照。
	rtpDownloads sync.Map
	// key=deviceID，保存结构化查询/状态结果（9.5/9.6/A.2.4）。
	queryStateMu             sync.RWMutex
	queryStates              sync.Map
	deviceDeletionTombstones sync.Map
	deviceOfflineTombstones  sync.Map
	// key=Call-ID，保存入向 INVITE 会话状态（9.2 被叫侧会话）。
	inviteDialogs sync.Map
	// key=*inboundInviteDialog，保存已经从活动索引摘除、但 BYE 尚未成功发出的终态对话。
	pendingInboundDialogCleanups sync.Map
	// key=*cascadeMediaSession，保存 StopSendRTP 尚未成功的级联媒体发送会话。
	pendingCascadeMediaCleanups sync.Map
	cascadeInviteMu             sync.Mutex
	// 级联实时点播复用同一通道的下级媒体源，并按会话引用计数释放。
	cascadeMediaMu         sync.Mutex
	cascadeSources         map[string]*cascadeSourceRef
	cascadePlay            func(context.Context, *PlayInput) error
	cascadeStop            func(context.Context, *StopPlayInput) error
	cascadeHistory         func(context.Context, *HistoryInput) error
	cascadeStopHistory     func(context.Context, *StopHistoryInput) error
	cascadeControlHistory  func(context.Context, *ControlHistoryInput) error
	cascadeQueryRecords    func(context.Context, *RecordQueryInput) ([]RecordItem, error)
	cascadeRecordResult    func(context.Context, *RecordQueryInput) (recordQueryResult, error)
	cascadeDeviceQuery     func(context.Context, *DeviceQueryInput) (*DeviceQueryOutput, error)
	cascadeDeviceControl   func(context.Context, *ipc.Channel, *deviceControlA23Request) (string, error)
	cascadeDeviceConfig    func(context.Context, *ipc.Channel, *DeviceConfigRequest) (string, error)
	cascadeBroadcastNotify func(context.Context, *Channel, *VoiceInput) error
	// 2022 升级/抓拍的级联发起会话，key=kind:downstreamDeviceID:sessionID。
	cascadeTaskRouteMu sync.Mutex
	cascadeTaskRoutes  sync.Map
	// 设备控制命令全局序列号，避免 PTZ 与 DeviceControl 并发冲突。
	controlSN atomic.Uint32
	// 设备查询命令全局序列号，避免随机 SN 碰撞。
	querySN atomic.Uint32
	// directDownloads 管理 2014 附录 O 无 RTP 封装的 TCP 文件接收。
	directDownloads             *DirectTCPDownloadManager
	directDownloadPersistenceMu sync.Mutex
	directDownloadPersistence   map[string]*directTCPDownloadPersistenceSlot
	directPolicyMu              sync.RWMutex
	directPolicy                directTCPRuntimePolicy
	metrics                     GBMetrics
	closeBeginOnce              sync.Once
	closeOnce                   sync.Once
	lifecycleMu                 sync.Mutex
	lifecycleDone               chan struct{}
	lifecycleCtx                context.Context
	lifecycleCancel             context.CancelFunc
	// shutdownPersistenceCtx 与已取消的业务 context 分离，允许停服时有界清理持久状态。
	shutdownPersistenceCtx    context.Context
	shutdownPersistenceCancel context.CancelFunc
	lifecycleClosed           bool
	lifecycleWG               sync.WaitGroup
	requestWG                 sync.WaitGroup
	backgroundWorkersOnce     sync.Once
	// startupReady 在生产 Server 打开监听器后、设备/级联/任务恢复完成前阻断入向业务。
	// 旧测试和独立构造未设置该通道时保持原有立即可用语义。
	startupReady     chan struct{}
	startupReadyOnce sync.Once

	svr *Server

	sms rtpMediaService
}

func NewGB28181API(cfg *conf.Bootstrap, store ipc.Adapter, sms *sms.NodeManager) *GB28181API {
	api := newGB28181API(cfg, store, sms)
	api.startBackgroundWorkers()
	return api
}

func newGB28181API(cfg *conf.Bootstrap, store ipc.Adapter, sms *sms.NodeManager) *GB28181API {
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
		recordResponseGenerations:      make(map[string]*recordResponseGeneration),
		recordResponseExtra:            make(map[string][]string),
		recordResponseXML:              make(map[string][]string),
		recordResponseAppendixA4:       make(map[string][]AppendixA4Object),
		recordResponseMetadata:         make(map[string]*recordResponseMetadataAccumulator),
		registerNonces:                 make(map[string]registerNonceState),
		messageNonces:                  make(map[string]messageNonceState),
		streams:                        &conc.Map[string, *Streams]{},
		cascadeSources:                 make(map[string]*cascadeSourceRef),
		cascadeSubscriptions:           make(map[string]*cascadeDownstreamSubscription),
		upgradeStates:                  make(map[string]UpgradeState),
		snapshotStates:                 make(map[string]SnapshotState),
		videoUploadOutboxWake:          make(chan struct{}, 1),
		manualSubscriptionRecoveryWake: make(chan string, 64),
		directDownloads:                NewDirectTCPDownloadManager(directTCPDownloadOptions(cfg.Sip.DirectTCPDownload)),
		lifecycleDone:                  make(chan struct{}),
		lifecycleCtx:                   lifecycleCtx,
		lifecycleCancel:                lifecycleCancel,
	}
	g.controlSN.Store(uint32(sip.RandInt(100000, 999999)))
	g.querySN.Store(uint32(sip.RandInt(100000, 999999)))
	g.applyDirectTCPConfig(cfg.Sip.DirectTCPDownload)
	return &g
}

// startBackgroundWorkers 在 Server 及持久化依赖装配完成后一次性启动后台清理任务。
func (g *GB28181API) startBackgroundWorkers() bool {
	if g == nil {
		return false
	}
	started := false
	g.backgroundWorkersOnce.Do(func() {
		started = true
		if err := g.restoreRTPDownloadStates(g.serviceContext()); err != nil {
			slog.Error("restore RTP download task states", "err", err)
		}
		if err := g.restoreDirectTCPDownloadStates(g.serviceContext()); err != nil {
			slog.Error("restore direct TCP download task states", "err", err)
		}
		g.startLifecycleWorker(g.startEventSubscriberCleaner)
		g.startLifecycleWorker(g.runManualSubscriptionRecoveryWorker)
		g.startLifecycleWorker(g.startInviteDialogCleaner)
		g.startLifecycleWorker(g.startRuntimeStateCleaner)
		g.startVideoUploadOutboxWorker()
	})
	return started
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

func (g *GB28181API) newSIPLifecycleUnavailableResponse(ctx *sip.Context, reason string) *sip.Response {
	if ctx.Request != nil && strings.EqualFold(strings.TrimSpace(ctx.Request.Method()), sip.MethodRegister) {
		return g.newRegisterResponse(ctx, http.StatusServiceUnavailable, reason)
	}
	return sip.NewResponseFromRequest("", ctx.Request, http.StatusServiceUnavailable, reason, nil)
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
		_ = ctx.Tx.Respond(g.newSIPLifecycleUnavailableResponse(ctx, ErrServiceStopped.Error()))
		ctx.Abort()
		return
	}
	defer done()
	if !g.startupCompleted() {
		// ACK 不能发送错误响应；设备会按原事务或注册周期重试其他请求。
		if ctx.Request != nil && ctx.Request.Method() == sip.MethodACK {
			ctx.Abort()
			return
		}
		response := g.newSIPLifecycleUnavailableResponse(ctx, "Service Unavailable")
		response.AppendHeader(&sip.GenericHeader{HeaderName: "Retry-After", Contents: "1"})
		_ = ctx.Tx.Respond(response)
		ctx.Abort()
		return
	}
	ctx.Next()
}

func (g *GB28181API) startupCompleted() bool {
	if g == nil || g.startupReady == nil {
		return true
	}
	select {
	case <-g.startupReady:
		return true
	default:
		return false
	}
}

func (g *GB28181API) markStartupReady() {
	if g == nil || g.startupReady == nil {
		return
	}
	g.startupReadyOnce.Do(func() { close(g.startupReady) })
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

func (g *GB28181API) serviceContext() context.Context {
	if g == nil {
		return context.Background()
	}
	g.lifecycleMu.Lock()
	ctx := g.lifecycleCtx
	closed := g.lifecycleClosed
	g.lifecycleMu.Unlock()
	if ctx != nil {
		return ctx
	}
	if !closed {
		return context.Background()
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	return cancelled
}

// initializedServiceContext 返回已装配服务的生命周期 context。
// 旧测试和内部兼容入口可能手工构造未初始化生命周期的 API，此时保持 Background 语义。
func (g *GB28181API) initializedServiceContext() context.Context {
	if g == nil {
		return context.Background()
	}
	g.lifecycleMu.Lock()
	ctx := g.lifecycleCtx
	g.lifecycleMu.Unlock()
	if ctx != nil {
		return ctx
	}
	return context.Background()
}

func (g *GB28181API) mediaPersistenceContext() context.Context {
	if g == nil {
		return context.Background()
	}
	g.lifecycleMu.Lock()
	ctx := g.lifecycleCtx
	if g.lifecycleClosed && g.shutdownPersistenceCtx != nil {
		ctx = g.shutdownPersistenceCtx
	}
	g.lifecycleMu.Unlock()
	if ctx != nil {
		return ctx
	}
	return context.Background()
}

// taskPersistenceContext 允许调用方取消后可靠保存任务终态，同时受服务停服收尾窗口约束。
func (g *GB28181API) taskPersistenceContext() context.Context {
	return g.mediaPersistenceContext()
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
	if g.annexG != nil {
		if err := g.annexG.updateSignalDigestSecurity(&cfg); err != nil {
			slog.Error("reload Annex G signal Digest security failed", "err", err)
		}
	}
	g.cfgMu.Lock()
	g.cfg = &cfg
	g.cfgMu.Unlock()
}

type directTCPRuntimePolicy struct {
	Enabled             bool
	CascadeRelayEnabled bool
	OfferPort           int
	RelayListenIP       string
	RelayAdvertiseIP    string
	RelayPortStart      int
	RelayPortEnd        int
	Allowlist           map[string]struct{}
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
	wasCascadeRelayEnabled := g.directPolicy.CascadeRelayEnabled
	g.directPolicy = directTCPRuntimePolicy{
		Enabled: in.Enabled, CascadeRelayEnabled: in.CascadeRelayEnabled,
		OfferPort: port, RelayListenIP: strings.TrimSpace(in.RelayListenIP),
		RelayAdvertiseIP: strings.TrimSpace(in.RelayAdvertiseIP),
		RelayPortStart:   in.RelayPortStart, RelayPortEnd: in.RelayPortEnd,
		Allowlist: allowlist,
	}
	g.directPolicyMu.Unlock()
	if wasEnabled && !in.Enabled && g.directDownloads != nil {
		g.directDownloads.CancelAll()
	}
	if wasCascadeRelayEnabled && !in.CascadeRelayEnabled {
		g.cancelCascadeDirectTCPRelays()
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
	if err := validateRegisterXGBVersion(ctx); err != nil {
		g.respondRegister(ctx, http.StatusBadRequest, err.Error())
		return
	}
	if err := filterUnknowDevices(ctx.DeviceID); err != nil {
		slog.Error("过滤设备，拒绝注册", "device_id", ctx.DeviceID, "err", err)
		g.respondRegister(ctx, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateRegisterContact(ctx); err != nil {
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
	if declaredVersion, ok := ParseGBProtocolVersion(ctx.XGBVer); ok && registerExpiresTooBrief(declaredVersion, expires) {
		g.respondRegisterIntervalTooBrief(ctx)
		return
	}

	// 9.1.2.3 注册重定向。目标只读取服务端配置，不能信任设备请求头，避免形成开放重定向。
	// 示例值：sip:34020000002000000001@10.0.0.8:5060
	if redirect := strings.TrimSpace(cfg.RegisterRedirect); redirect != "" && ctx.XGBVer == string(GBVersion30) && expires > 0 {
		if err := conf.ValidateSIPRegisterRedirect(redirect, cfg.ID); err != nil {
			g.respondRegister(ctx, http.StatusBadRequest, "invalid redirect uri")
			return
		}
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
		resp.AppendHeader(&sip.GenericHeader{
			HeaderName: "Expires",
			Contents:   strconv.Itoa(expires),
		})
		_ = ctx.Tx.Respond(resp)
		return
	}
	unlockRegister := g.lockRegisterOperation(ctx.DeviceID)
	defer unlockRegister()
	requestKey := registerResultKey(ctx)
	respondOK := func(date string) error {
		resp := g.newRegisterSuccessResponse(ctx, expires, date)
		if err := ctx.Tx.Respond(resp); err != nil {
			ctx.Log.Error("respond REGISTER success", "err", err)
			return err
		}
		if expires > 0 {
			g.deviceDeletionTombstones.Delete(ctx.DeviceID)
			g.deviceOfflineTombstones.Delete(ctx.DeviceID)
		}
		g.metrics.registerSuccess.Add(1)
		return nil
	}
	if cached, ok := g.loadRegisterResult(requestKey, time.Now()); ok {
		_ = respondOK(cached.Date)
		return
	}
	responseDate := sip.FormatGBTime(time.Now(), "2006-01-02T15:04:05.000")
	requestFingerprint := hex.EncodeToString(requestKey[:])
	respFn := func(date string) (registerResultState, bool) {
		now := time.Now()
		state := registerResultState{
			DeviceID:  ctx.DeviceID,
			Date:      date,
			ExpiresAt: registerResultCacheExpiry(now, expires),
		}
		if err := respondOK(state.Date); err != nil {
			state.Pending = true
			g.storeRegisterResult(requestKey, state, now)
			return state, false
		}
		return state, true
	}

	var (
		dev      ipc.Device
		isNewDev bool
	)
	if err := g.core.Store().Device().Get(g.serviceContext(), &dev, orm.Where("device_id=?", ctx.DeviceID)); err != nil {
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
	effectiveVersion := applyGBProtocolVersion(&dev.Ext, ctx.XGBVer)
	ctx.Log = ctx.Log.With(
		"declared_version", ctx.XGBVerRaw,
		"effective_version", effectiveVersion,
		"version_source", dev.Ext.GBVersionSource,
	)
	if ctx.XGBVerRaw != "" && ctx.XGBVer == "" {
		ctx.Log.Warn("设备声明了未知协议版本，使用保守或已配置档案")
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
	if registerExpiresTooBrief(effectiveVersion, expires) {
		g.respondRegisterIntervalTooBrief(ctx)
		return
	}
	// 注销不存在的绑定应在鉴权通过后保持幂等，不能绕过鉴权或反向创建设备档案。
	if isNewDev && expires == 0 {
		ctx.Log.Info("忽略未知设备注销")
		g.deleteRegisterResultsForDevice(ctx.DeviceID)
		if state, ok := respFn(responseDate); ok {
			g.storeRegisterResult(requestKey, state, time.Now())
		}
		return
	}

	// 鉴权通过后，未知设备才自动建档
	if isNewDev {
		d, err := g.core.GetDeviceByDeviceIDContext(g.serviceContext(), ctx.DeviceID)
		if err != nil {
			ctx.Log.Error("create device by device_id failed", "err", err)
			g.respondRegister(ctx, http.StatusInternalServerError, "server db error")
			return
		}
		dev = *d
	}

	// 仅在通过校验后更新内存状态，避免未授权请求污染内存。
	// 重启后 TCP/TLS 设备的旧连接不可复用，但重新注册成功前必须恢复持久化通道，
	// 避免等待后续 Catalog 查询时立即播放报 channel not exist。
	runtimeDevice := &Device{
		conn:    ctx.Request.GetConnection(),
		source:  ctx.Source,
		to:      ctx.To,
		Address: ctx.Source.String(),
	}
	if expires == 0 {
		g.svr.memoryStorer.LoadOrStore(ctx.DeviceID, runtimeDevice)
	} else if err := g.svr.loadOrStoreDeviceMemory(g.serviceContext(), ctx.DeviceID, runtimeDevice); err != nil {
		g.respondRegisterStateError(ctx, "恢复设备通道失败", err)
		return
	}

	if expires == 0 {
		ctx.Log.Info("设备注销")
		var (
			replayDate      string
			replayConfirmed bool
		)
		if err := g.logout(ctx.DeviceID, func(b *ipc.Device) error {
			replay, confirmed, err := applyRegisterBindingSequence(&b.Ext, ctx.Request, requestFingerprint, responseDate)
			if err != nil {
				return err
			}
			if replay {
				replayDate = b.Ext.GBRegisterResponseDate
				replayConfirmed = confirmed
				return nil
			}
			b.IsOnline = false
			b.Address = ctx.Source.String()
			if ctx.XGBVer != "" {
				applyGBProtocolVersion(&b.Ext, ctx.XGBVer)
			}
			return nil
		}); err != nil {
			g.respondRegisterStateError(ctx, "设备注销状态持久化失败", err)
			return
		}
		if replayDate == "" {
			// 新绑定状态已提交，旧成功响应不能再代表当前状态。
			g.deleteRegisterResultsForDevice(ctx.DeviceID)
			replayDate = responseDate
		}
		state, ok := respFn(replayDate)
		if !ok {
			return
		}
		if !replayConfirmed {
			confirmed, _, confirmErr := g.confirmRegisterResponse(ctx.DeviceID, requestFingerprint)
			if confirmErr != nil {
				ctx.Log.Error("持久化设备注销响应确认失败", "err", confirmErr)
				state.Pending = true
				g.storeRegisterResult(requestKey, state, time.Now())
				return
			}
			if !confirmed {
				return
			}
		}
		g.storeRegisterResult(requestKey, state, time.Now())
		return
	}

	var (
		replayDate      string
		replayConfirmed bool
	)
	if err := g.login(ctx, effectiveVersion, dev.Ext.GBDisabledCapabilities, func(b *ipc.Device) error {
		replay, confirmed, err := applyRegisterBindingSequence(&b.Ext, ctx.Request, requestFingerprint, responseDate)
		if err != nil {
			return err
		}
		if replay {
			replayDate = b.Ext.GBRegisterResponseDate
			replayConfirmed = confirmed
			return nil
		}
		b.IsOnline = true
		b.RegisteredAt = orm.Now()
		b.KeepaliveAt = orm.Now()
		b.Expires = expires
		b.Address = ctx.Source.String()
		b.Transport = requestSignalingTransport(ctx)
		applyGBProtocolVersion(&b.Ext, ctx.XGBVer)
		return nil
	}); err != nil {
		g.respondRegisterStateError(ctx, "设备注册状态持久化失败", err)
		return
	}
	if replayConfirmed {
		if state, ok := respFn(replayDate); ok {
			g.storeRegisterResult(requestKey, state, time.Now())
		}
		return
	}
	if replayDate == "" {
		// 刷新注册可能改变有效期、地址或版本；只保留本次成功响应的幂等结果。
		g.deleteRegisterResultsForDevice(ctx.DeviceID)
		replayDate = responseDate
	}

	// conn := ctx.Request.GetConnection()
	// fmt.Printf(">>> %p\n", conn

	ctx.Log.Info("设备注册成功")
	// ctx.Log.Debug("device info", "source", ctx.Source, "host", ctx.Host)

	state, ok := respFn(replayDate)
	if !ok {
		return
	}
	confirmed, claimed, confirmErr := g.confirmRegisterResponse(ctx.DeviceID, requestFingerprint)
	if confirmErr != nil {
		ctx.Log.Error("持久化设备注册响应确认失败", "err", confirmErr)
		state.Pending = true
		g.storeRegisterResult(requestKey, state, time.Now())
		return
	}
	if !confirmed {
		return
	}
	g.storeRegisterResult(requestKey, state, time.Now())
	if !claimed {
		return
	}
	// 注册历史不是 REGISTER 成功事务的一部分，避免其查询、写入和清理延迟 200 OK。
	if history := g.core.DeviceHistory(); history != nil {
		if err := history.Record(g.serviceContext(), ctx.DeviceID, ipc.DeviceHistoryRegister, ctx.Source.String(), "online", time.Now()); err != nil {
			ctx.Log.Error("持久化设备注册历史失败", "err", err)
		}
	}
	// 历史必须与设备删除使用同一活动锁串行；否则删除提交后可能再次插入孤立历史。
	unlockRegister()
	g.signalManualSubscriptionRecovery(ctx.DeviceID)

	ctx.XGBVer = string(effectiveVersion)
	g.QueryDeviceInfo(ctx)
	if err := g.QueryCatalogContext(g.serviceContext(), dev.GetGB28181DeviceID()); err != nil {
		ctx.Log.Warn("注册后目录查询失败", "err", err)
		if g.scheduleCatalogRefreshAfterFailure(dev.GetGB28181DeviceID(), err) {
			ctx.Log.Info("已安排注册后 Catalog 补查")
		}
	}
	if g.deviceSupportsGBFeature(dev.GetGB28181DeviceID(), "config_query", effectiveVersion, func(c GBCapabilities) bool { return c.ConfigQuery }) {
		if err := g.QueryConfigDownloadBasicContext(g.serviceContext(), dev.GetGB28181DeviceID()); err != nil {
			ctx.Log.Warn("注册后基础配置查询失败", "err", err)
		}
	}
}

func validateRegisterXGBVersion(ctx *sip.Context) error {
	if ctx == nil || ctx.Request == nil {
		return fmt.Errorf("invalid REGISTER request")
	}
	raw, version, present, err := parseXGBVersionHeader(ctx.Request)
	if err != nil {
		return fmt.Errorf("invalid REGISTER protocol version: %w", err)
	}
	ctx.XGBVerRaw = ""
	ctx.XGBVer = ""
	if !present {
		return nil
	}
	ctx.XGBVerRaw = raw
	if version.Valid() {
		ctx.XGBVer = string(version)
	}
	return nil
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
	if state.Pending {
		return registerResultState{}, false
	}
	return state, true
}

func applyRegisterBindingSequence(ext *ipc.DeviceExt, request *sip.Request, fingerprint, responseDate string) (bool, bool, error) {
	if ext == nil || request == nil {
		return false, false, fmt.Errorf("REGISTER sequence context is unavailable")
	}
	callID, callIDOK := request.CallID()
	cseq, cseqOK := request.CSeq()
	if !callIDOK || callID == nil || cseq == nil || !cseqOK {
		return false, false, fmt.Errorf("REGISTER sequence headers are unavailable")
	}
	value := strings.TrimSpace(string(*callID))
	if value == "" || !strings.EqualFold(strings.TrimSpace(cseq.MethodName), sip.MethodRegister) {
		return false, false, fmt.Errorf("REGISTER sequence headers are invalid")
	}
	fingerprint = strings.TrimSpace(fingerprint)
	responseDate = strings.TrimSpace(responseDate)
	if ext.GBRegisterCallID == value {
		if cseq.SeqNo < ext.GBRegisterCSeq {
			return false, false, errRegisterCSeqOutOfOrder
		}
		if cseq.SeqNo == ext.GBRegisterCSeq {
			if fingerprint != "" && ext.GBRegisterRequestFingerprint == fingerprint && ext.GBRegisterResponseDate != "" {
				return true, ext.GBRegisterResponseConfirmed, nil
			}
			return false, false, errRegisterCSeqOutOfOrder
		}
	}
	ext.GBRegisterCallID = value
	ext.GBRegisterCSeq = cseq.SeqNo
	ext.GBRegisterRequestFingerprint = fingerprint
	ext.GBRegisterResponseDate = responseDate
	ext.GBRegisterResponseConfirmed = false
	return false, false, nil
}

func (g *GB28181API) confirmRegisterResponse(deviceID, fingerprint string) (confirmed, claimed bool, err error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return false, false, fmt.Errorf("REGISTER request fingerprint is unavailable")
	}
	err = g.svr.changeMemory(g.serviceContext(), deviceID, func(device *ipc.Device) error {
		if device.Ext.GBRegisterRequestFingerprint != fingerprint {
			return nil
		}
		confirmed = true
		if !device.Ext.GBRegisterResponseConfirmed {
			device.Ext.GBRegisterResponseConfirmed = true
			claimed = true
		}
		return nil
	}, nil)
	return confirmed, claimed, err
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

// registerResultCacheExpiry 不让成功响应缓存活得比短周期注册绑定更久。
// Expires=0 是注销事务，不对应活动绑定，仍使用常规幂等窗口。
func registerResultCacheExpiry(now time.Time, expires int) time.Time {
	expiresAt := now.Add(registerResultTTL)
	if expires <= 0 {
		return expiresAt
	}
	bindingExpiresAt := now.Add(time.Duration(expires) * time.Second)
	if bindingExpiresAt.Before(expiresAt) {
		return bindingExpiresAt
	}
	return expiresAt
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
	if headers := ctx.Request.GetHeaders("Expires"); len(headers) > 1 {
		return 0, fmt.Errorf("REGISTER must contain at most one Expires header")
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
	if err != nil || expires < 0 || int64(expires) > maximumRegisterExpires {
		return 0, fmt.Errorf("invalid REGISTER expires: %s", value)
	}
	return expires, nil
}

func registerExpiresTooBrief(version GBProtocolVersion, expires int) bool {
	return expires > 0 && version.AtLeast(GBVersion11) && expires < minimumStandardRegisterTTL
}

func validateRegisterContact(ctx *sip.Context) error {
	if ctx == nil || ctx.Request == nil {
		return fmt.Errorf("invalid REGISTER request")
	}
	headers := ctx.Request.GetHeaders("Contact")
	if len(headers) != 1 {
		return fmt.Errorf("REGISTER must contain exactly one Contact header")
	}
	contact, ok := ctx.Request.Contact()
	if !ok || contact == nil || contact.Address == nil || strings.TrimSpace(contact.Address.Host()) == "" {
		return fmt.Errorf("REGISTER Contact header is invalid")
	}
	user := contact.Address.User()
	if user == nil || strings.TrimSpace(user.String()) == "" {
		return fmt.Errorf("REGISTER Contact device id is missing")
	}
	if user.String() != ctx.DeviceID {
		return fmt.Errorf("REGISTER Contact device id does not match From device id")
	}
	return nil
}

func (g *GB28181API) login(ctx *sip.Context, version GBProtocolVersion, disabledCapabilities []string, fn func(d *ipc.Device) error) error {
	slog.Info("status change 设备上线", "device_id", ctx.DeviceID)
	var committed ipc.Device
	return g.svr.changeMemory(g.serviceContext(), ctx.DeviceID, func(device *ipc.Device) error {
		if fn != nil {
			if err := fn(device); err != nil {
				return err
			}
		}
		setPersistedRegistrationClosed(device, false)
		committed = *device
		return nil
	}, func(d *Device) {
		SyncRegistrationBindingRuntime(d, &committed)
		d.conn = ctx.Request.GetConnection()
		d.source = ctx.Source
		d.to = ctx.To
		d.offlinePersistencePending = false
		d.registrationClosed = false
		clearPendingDeviceStatusLocked(d)
		clearPendingKeepaliveLocked(d)
		d.setGBProfile(version, disabledCapabilities)
	})
}

func (g *GB28181API) newRegisterResponse(ctx *sip.Context, status int, reason string) *sip.Response {
	resp := sip.NewResponseFromRequest("", ctx.Request, status, reason, nil)
	version := sip.XGBVer(platformMaxGBVersion)
	resp.AppendHeader(&version)
	return resp
}

func (g *GB28181API) newRegisterSuccessResponse(ctx *sip.Context, expires int, date string) *sip.Response {
	resp := g.newRegisterResponse(ctx, http.StatusOK, "OK")
	sip.CopyHeaders("Contact", ctx.Request, resp)
	acceptedExpires := sip.Expires(expires)
	resp.AppendHeader(&acceptedExpires)
	resp.AppendHeader(&sip.GenericHeader{
		HeaderName: "Date",
		Contents:   date,
	})
	return resp
}

func (g *GB28181API) respondRegister(ctx *sip.Context, status int, reason string) {
	if status >= http.StatusBadRequest {
		g.metrics.registerFailures.Add(1)
	}
	_ = ctx.Tx.Respond(g.newRegisterResponse(ctx, status, reason))
}

func (g *GB28181API) respondRegisterStateError(ctx *sip.Context, logMessage string, err error) {
	if errors.Is(err, errRegisterCSeqOutOfOrder) {
		ctx.Log.Warn(logMessage, "err", err)
		g.metrics.registerFailures.Add(1)
		resp := g.newRegisterResponse(ctx, http.StatusInternalServerError, errRegisterCSeqOutOfOrder.Error())
		resp.AppendHeader(&sip.GenericHeader{HeaderName: "Retry-After", Contents: "0"})
		_ = ctx.Tx.Respond(resp)
		return
	}
	ctx.Log.Error(logMessage, "err", err)
	g.respondRegister(ctx, http.StatusInternalServerError, "server db error")
}

func (g *GB28181API) respondRegisterIntervalTooBrief(ctx *sip.Context) {
	g.metrics.registerFailures.Add(1)
	resp := g.newRegisterResponse(ctx, statusIntervalTooBrief, "Interval Too Brief")
	resp.AppendHeader(&sip.GenericHeader{
		HeaderName: "Min-Expires",
		Contents:   strconv.Itoa(minimumStandardRegisterTTL),
	})
	_ = ctx.Tx.Respond(resp)
}

func (g *GB28181API) logout(deviceID string, changeFn func(*ipc.Device) error) error {
	var committed ipc.Device
	err := g.svr.changeMemory(g.serviceContext(), deviceID, func(device *ipc.Device) error {
		if changeFn != nil {
			if err := changeFn(device); err != nil {
				return err
			}
		}
		setPersistedRegistrationClosed(device, true)
		committed = *device
		return nil
	}, func(d *Device) {
		SyncRegistrationBindingRuntime(d, &committed)
		d.Expires = 0
		d.IsOnline = false
		d.offlinePersistencePending = false
		d.registrationClosed = true
		clearPendingDeviceStatusLocked(d)
		clearPendingKeepaliveLocked(d)
	})
	if err == nil {
		slog.Info("status change 设备离线", "device_id", deviceID)
		g.cleanupOfflineDeviceRuntime(deviceID)
	}
	return err
}

func (g *GB28181API) cleanupOfflineDeviceRuntime(deviceID string) {
	g.deviceOfflineTombstones.Store(strings.TrimSpace(deviceID), struct{}{})
	g.cancelPendingDeviceOperations(deviceID, ErrDeviceOffline)
	g.releaseInboundEventSubscriptionsOwnedByDeviceContext(g.mediaPersistenceContext(), deviceID)
	g.removeCascadeMobilePositionQueriesForDevice(deviceID)
	g.releaseOutgoingSubscriptionsForDeviceContext(g.mediaPersistenceContext(), deviceID)
	g.terminateDeviceMediaSessions(g.mediaPersistenceContext(), deviceID, "device_offline")
}
