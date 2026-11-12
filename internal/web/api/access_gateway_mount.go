package api

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"encoding/json"
	"github.com/gin-gonic/gin"
	agowl "github.com/gowvp/owl/extensions/accessgateway/integration/owl"
	agplatform "github.com/gowvp/owl/extensions/accessgateway/platform"
	agcore "github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/httpapi"
	"github.com/gowvp/owl/extensions/accessgateway/platform/migrations"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipccache"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs"
	"github.com/ixugo/goddd/domain/version"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
	"time"
)

func NewAccessGatewayHost(db *gorm.DB) *agowl.Host { return agowl.New(db, nil, nil) }

// version.Core 确保先完成宿主版本判定；失败时禁止设备恢复和版本成功记录。
func NewIPCStoreWithGateway(db *gorm.DB, cfg *conf.Bootstrap, _ version.Core, host *agowl.Host) (ipc.Storer, error) {
	var e error
	if orm.GetEnabledAutoMigrate() {
		e = migrations.Apply(db)
	} else {
		e = migrations.Verify(db)
	}
	if e != nil {
		return nil, fmt.Errorf("国标公网接入网关迁移/校验失败: %w", e)
	}
	cache := NewIPCStore(db, cfg)
	value, ok := cache.(*ipccache.Cache)
	if !ok {
		return nil, fmt.Errorf("国标设备缓存不支持入口事务")
	}
	value.SetIngressGuard(host)
	return cache, nil
}
func NewAccessGatewayModule(ctx context.Context, cfg *conf.Bootstrap, db *gorm.DB, server *gbs.Server, host *agowl.Host, media sms.Core) (*agplatform.Module, func(), error) {
	host.AttachSIP(server)
	host.AttachMedia(media)
	c := cfg.AccessGateway
	dir := c.CredentialDir
	if dir == "" {
		dir = "access-gateway/credentials"
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cfg.ConfigDir, dir)
	}
	config := agcore.Config{Enabled: c.Enabled, PlatformID: c.PlatformID, CredentialDir: dir, MaxConnections: c.MaxConnections, MaxMediaSessions: c.MaxMediaSessions, MaxTransferSessions: c.MaxTransferSessions}
	m, e := agplatform.New(ctx, config, db, host, host, host.BindRuntime)
	if e != nil {
		return nil, nil, e
	}
	return m, func() { _ = m.Close() }, nil
}
func mountAccessGateway(router gin.IRouter, uc *Usecase) {
	uc.AccessGatewayHost.SetFaultSink(func(ctx context.Context, id, name, incident, label string, recovered bool) error {
		key := "gateway-fault:" + incident
		if !recovered {
			body, _ := json.Marshal(map[string]string{"gateway_id": id, "gateway_name": name, "description": label})
			_, err := uc.EventAPI.eventCore.CreateEventAndNotify(ctx, &event.AddEventInput{StartedAt: orm.Now(), EndedAt: orm.Now(), Label: label, Score: 1, Model: "AccessGateway", SourceKey: &key, Zones: string(body), GatewayID: id, GatewayName: name})
			return err
		}
		var e event.Event
		if err := uc.EventAPI.eventCore.Workflow().DBEventBySource(ctx, key, &e); err != nil {
			return err
		}
		if e.Status == "resolved" || e.Status == "ignored" {
			return nil
		}
		return uc.EventAPI.eventCore.Workflow().Process(ctx, event.ProcessInput{Items: []event.ProcessTarget{{ID: e.ID, Revision: e.Revision}}, Status: "resolved", Note: "系统检测到网关故障已恢复"}, "system:gateway")
	})
	uc.AccessGatewayHost.SetAlarmSink(func(ctx context.Context, a protocol.AlarmRecord, did, cid string) error {
		received, err := time.Parse(time.RFC3339Nano, a.ReceivedAt)
		if err != nil {
			return err
		}
		alarm := gbs.AlarmEvent{AlarmTime: a.AlarmTime}
		started := received
		if t, ok := alarm.ParseTime(); ok {
			started = t
		}
		body, err := json.Marshal(a)
		if err != nil {
			return err
		}
		key := "gateway-alarm:" + a.ID
		_, err = uc.EventAPI.eventCore.CreateAlarmAndNotify(ctx, &event.AddEventInput{DID: did, CID: cid, StartedAt: orm.Time{Time: started}, EndedAt: orm.Time{Time: started}, Label: "gb_alarm_" + a.Method, Score: 1, Zones: string(body), Model: "GB28181", SourceKey: &key, Historical: a.Historical, ReceivedAt: &received}, event.AlarmIdentity{DeviceCode: a.DeviceCode, ChannelCode: a.ChannelCode, Method: a.Method, Type: a.Type, Priority: a.Priority, Description: a.Description, EventType: a.EventType, Longitude: a.Longitude, Latitude: a.Latitude})
		return err
	})
	uc.AccessGatewayHost.SetSnapshotSink(uc.GB28181API.persistGatewaySnapshot)
	uc.AccessGateway.Register(router, func(c *gin.Context) {
		deny := func() {
			httpapi.Failure(c, agcore.Fail(401, "ADMIN_REQUIRED", "需要有效的平台管理员 Bearer 身份"))
		}
		query := c.Request.URL.Query()
		if _, ok := query["token"]; ok {
			deny()
			return
		}
		if _, ok := query["access_token"]; ok {
			deny()
			return
		}
		auth := c.GetHeader("Authorization")
		if len(c.Request.Header.Values("Authorization")) != 1 || !strings.HasPrefix(auth, "Bearer ") {
			deny()
			return
		}
		claims, e := web.ParseToken(strings.TrimPrefix(auth, "Bearer "), uc.Conf.Server.HTTP.JwtSecret)
		if e != nil || claims.Valid() != nil || claims.ExpiresAt == nil {
			deny()
			return
		}
		if !uc.validAdminSession(claims) {
			deny()
			return
		}
		c.Set("access_gateway_admin", claims.Data[web.KeyUsername])
		c.Next()
	}, func(c *gin.Context) string { return c.GetString("access_gateway_admin") })
}

type accessGatewayHandler struct {
	http.Handler
	module *agplatform.Module
}

func (h *accessGatewayHandler) Close() error {
	e := h.module.Close()
	if closer, ok := h.Handler.(interface{ Close() error }); ok {
		if closeErr := closer.Close(); e == nil {
			e = closeErr
		}
	}
	return e
}
