package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/wire"
	"github.com/gowvp/owl/internal/adapter/gbadapter"
	"github.com/gowvp/owl/internal/adapter/onvifadapter"
	"github.com/gowvp/owl/internal/adapter/rtmpadapter"
	"github.com/gowvp/owl/internal/adapter/rtspadapter"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipccache"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/gowvp/owl/internal/core/recording/adapter"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/internal/data"
	"github.com/gowvp/owl/pkg/gbs"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/domain/uniqueid/store/uniqueiddb"
	"github.com/ixugo/goddd/domain/version"
	"github.com/ixugo/goddd/domain/version/store/versiondb"
	"github.com/ixugo/goddd/domain/version/versionapi"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

var (
	ProviderVersionSet = wire.NewSet(versionapi.NewVersionCore)
	ProviderSet        = wire.NewSet(
		wire.Struct(new(Usecase), "*"),
		NewHTTPHandler,
		versionapi.New,
		NewSMSCore, NewSmsAPI,
		NewWebHookAPI,
		NewUniqueID,
		gbs.NewServer,
		NewIPCStore, NewGBAdapter,
		NewIPCCoreWithProtocols,
		NewIPCAPI,
		NewConfigAPI,
		NewUserAPI,
		NewAIWebhookAPIWithDeps,
		NewEventCore, NewEventAPI,
		// Recording: Store -> SMSProvider(adapter) -> Core -> API
		NewRecordingStore, NewSMSProviderAdapter, NewRecordingCore, NewRecordingAPI,
	)
)

type Usecase struct {
	Conf       *conf.Bootstrap
	DB         *gorm.DB
	Version    versionapi.API
	SMSAPI     SmsAPI
	WebHookAPI WebHookAPI
	UniqueID   uniqueid.Core
	GB28181API IPCAPI
	ConfigAPI  ConfigAPI

	SipServer    *gbs.Server
	UserAPI      UserAPI
	AIWebhookAPI AIWebhookAPI

	EventAPI EventAPI

	RecordingAPI RecordingAPI
}

// NewHTTPHandler 生成Gin框架路由内容
func NewHTTPHandler(uc *Usecase) http.Handler {
	cfg := uc.Conf.Server
	if cfg.HTTP.JwtSecret == "" {
		uc.Conf.Server.HTTP.JwtSecret = orm.GenerateRandomString(32)
	}
	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}
	g := gin.New()
	g.NoRoute(func(c *gin.Context) {
		c.JSON(404, "来到了无人的荒漠")
	})
	// 如果启用了 Pprof，设置 Pprof 监控
	if cfg.HTTP.PProf.Enabled {
		web.SetupPProf(g, &cfg.HTTP.PProf.AccessIps) // 设置 Pprof 监控
	}

	setupRouter(g, uc) // 设置路由处理函数
	// 将 SIP 层 GB 报警上报桥接到事件中心，统一落库到 events 表。
	setupGBAlarmBridge(uc)
	uc.Version.RecordVersion()
	return g // 返回配置好的 Gin 实例作为 http.Handler
}

// setupGBAlarmBridge 注册 GB 报警回调，并把报警转成标准事件记录。
func setupGBAlarmBridge(uc *Usecase) {
	if uc == nil || uc.SipServer == nil {
		return
	}

	uc.SipServer.SetAlarmHandler(func(ctx context.Context, in *gbs.AlarmEvent) {
		if in == nil || strings.TrimSpace(in.DeviceID) == "" {
			return
		}

		// 1) 设备国标编码 -> 内部设备ID。
		dev, err := uc.GB28181API.ipc.GetDeviceByDeviceID(ctx, in.DeviceID)
		if err != nil {
			slog.Warn("alarm: device not found", "deviceID", in.DeviceID, "err", err)
			return
		}

		var cid string
		if in.ChannelID != "" && in.ChannelID != in.DeviceID {
			// 2) 通道国标编码 -> 内部通道ID（查不到时允许为空）。
			ch, err := uc.GB28181API.ipc.GetChannelByDeviceChannelID(ctx, in.DeviceID, in.ChannelID)
			if err == nil {
				cid = ch.ID
			}
		}

		started := orm.Now()
		if t, ok := in.ParseTime(); ok {
			// 使用设备上报时间；解析失败时退化为服务端当前时间。
			started = orm.Time{Time: t}
		}

		label := "gb_alarm"
		if in.AlarmMethod != "" {
			// 按报警方式细分标签，如 gb_alarm_4。
			label = "gb_alarm_" + in.AlarmMethod
		}

		// 原始报警内容序列化到 zones 字段，便于审计和回溯。
		zones, _ := json.Marshal(in)
		if _, err := uc.EventAPI.eventCore.AddEvent(ctx, &event.AddEventInput{
			DID:       dev.ID,
			CID:       cid,
			StartedAt: started,
			EndedAt:   orm.Time{Time: started.Time.Add(1 * time.Second)},
			Label:     label,
			Score:     1,
			Zones:     string(zones),
			Model:     "GB28181",
		}); err != nil {
			slog.Warn("alarm: save event failed", "deviceID", in.DeviceID, "channelID", in.ChannelID, "err", err)
		}
	})
}

// NewUniqueID 唯一 id 生成器
func NewUniqueID(db *gorm.DB) uniqueid.Core {
	return uniqueid.NewCore(uniqueiddb.NewDB(db).AutoMigrate(orm.GetEnabledAutoMigrate()), 5)
}

// 需要迁移的版本阈值
const migrateVersionThreshold = "0.0.20"

func NewIPCStore(db *gorm.DB) ipc.Storer {
	store := ipccache.NewCache(ipcdb.NewDB(db).AutoMigrate(orm.GetEnabledAutoMigrate()))

	// 检查版本并执行 RTMP/RTSP 数据迁移到 channels 表
	if shouldMigrateStreamData(db) {
		slog.Info("检测到需要迁移 stream_push/stream_proxy 数据到 channels 表")
		uni := uniqueid.NewCore(uniqueiddb.NewDB(db), 5)
		if err := data.MigrateStreamData(db, uni); err != nil {
			slog.Error("数据迁移失败", "err", err)
			// 迁移失败不阻止程序启动，只记录错误
		}
	}

	return store
}

// shouldMigrateStreamData 检查是否需要迁移 stream_push/stream_proxy 数据
// 当数据库版本 < 0.0.20 且存在旧表时需要迁移
func shouldMigrateStreamData(db *gorm.DB) bool {
	// 检查是否存在旧表
	hasStreamPushs := db.Migrator().HasTable("stream_pushs")
	hasStreamProxys := db.Migrator().HasTable("stream_proxys")
	if !hasStreamPushs && !hasStreamProxys {
		return false
	}

	// 检查版本号
	vdb := versiondb.NewDB(db)
	var ver version.Version
	if err := vdb.First(&ver); err != nil {
		// 版本表不存在或为空，需要迁移
		slog.Debug("版本表不存在或为空，需要迁移")
		return true
	}

	// 比较版本号，< 0.0.20 需要迁移
	return compareVersion(ver.Version, migrateVersionThreshold) < 0
}

func NewGBAdapter(store ipc.Storer, uni uniqueid.Core) ipc.Adapter {
	return ipc.NewAdapter(
		store,
		uni,
	)
}

// IPCBundle 包含 ipc.Core 和 Protocols，用于解决循环依赖
type IPCBundle struct {
	Core      ipc.Core
	Protocols map[string]ipc.Protocoler
}

// NewIPCCoreWithProtocols 创建 IPC Core 和 Protocols
// 通过在函数内部分两步创建来解决：先创建不含 protocols 的 Core，再创建 Protocols，最后注入
func NewIPCCoreWithProtocols(store ipc.Storer, uni uniqueid.Core, adapter ipc.Adapter, smsCore sms.Core, gbsServer *gbs.Server, conf *conf.Bootstrap) IPCBundle {
	// 第一步：创建不含 protocols 的 ipc.Core
	ipcCore := ipc.NewCore(store, uni, nil)

	// 第二步：创建 protocols（需要 ipc.Core）
	protocols := make(map[string]ipc.Protocoler)
	protocols[ipc.TypeOnvif] = onvifadapter.NewAdapter(adapter, smsCore)
	protocols[ipc.TypeRTSP] = rtspadapter.NewAdapter(ipcCore, smsCore)
	protocols[ipc.TypeRTMP] = rtmpadapter.NewAdapter(ipcCore, conf)
	protocols[ipc.TypeGB28181] = gbadapter.NewAdapter(adapter, gbsServer, smsCore)

	// 第三步：将 protocols 注入到 ipc.Core
	ipcCore.SetProtocols(protocols)

	return IPCBundle{
		Core:      ipcCore,
		Protocols: protocols,
	}
}

// NewAIWebhookAPIWithDeps 创建带依赖的 AI Webhook API
func NewAIWebhookAPIWithDeps(conf *conf.Bootstrap, eventCore event.Core, ipcBundle IPCBundle) AIWebhookAPI {
	return NewAIWebhookAPI(conf, eventCore, ipcBundle.Core)
}

// NewSMSProviderAdapter 创建 SMS 适配器，将 sms.Core 适配为 recording.SMSProvider
// 通过接口解耦 recording 领域与 sms 领域，避免循环依赖
func NewSMSProviderAdapter(smsCore sms.Core) recording.SMSProvider {
	return adapter.NewSMSAdapter(smsCore)
}
