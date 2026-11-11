// Package owl 是扩展与原国标业务的唯一适配边界。
package owl

import (
	"context"
	"fmt"
	"sync"

	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/runtime"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs"
	"gorm.io/gorm"
)

// MediaHost 只能操作会话保存的原节点/流身份，不能按当前通道配置猜测恢复资源。
type MediaHost interface {
	RecoverLocalSession(context.Context, core.RecoverySession) (bool, error)
	StopGatewaySession(context.Context, string, string, bool) error
	ValidateMediaServer(context.Context, *string, []string) error
	Capabilities(context.Context, core.DesiredConfig) ([]string, error)
}
type Host struct {
	platformMu   sync.Mutex
	db           *gorm.DB
	repo         *store.Repository
	sip          *gbs.Server
	media        MediaHost
	mu           sync.RWMutex
	runtime      *runtime.Manager
	transfers    sync.Map
	snapshotSink func(context.Context, string, string, string, []byte) error
}

func New(db *gorm.DB, server *gbs.Server, media MediaHost) *Host {
	return &Host{db: db, repo: store.New(db), sip: server, media: media}
}
func (h *Host) BindRuntime(manager *runtime.Manager) { h.mu.Lock(); h.runtime = manager; h.mu.Unlock() }

// AttachSIP 在管理路由开放前安装历史入口保护；模块关闭时也必须阻止跨入口直连。
func (h *Host) AttachSIP(server *gbs.Server) {
	h.sip = server
	if server != nil {
		server.SetRequestIngressGuard(h.AuthorizeInbound)
		server.SetExternalPlatformPort(h)
	}
}
func (h *Host) manager() *runtime.Manager { h.mu.RLock(); defer h.mu.RUnlock(); return h.runtime }
func scopePrefix(pk, generation string, epoch protocol.Decimal) string {
	return fmt.Sprintf("%s/%s/%s/", pk, generation, epoch)
}
func (h *Host) ReceiveSignal(ctx context.Context, in core.SignalIngress) error {
	if h.sip == nil || h.manager() == nil {
		return core.Fail(503, "DEPENDENCY_UNAVAILABLE", "国标信令适配尚未就绪")
	}
	conn := newConnection(h, in)
	return h.sip.InjectExternalPacket(ctx, conn, in.Raw)
}
func (h *Host) InvalidateIngress(ctx context.Context, pk, generation string, epoch protocol.Decimal) error {
	if h.sip == nil {
		return nil
	}
	return h.sip.InvalidateExternalIngress(ctx, scopePrefix(pk, generation, epoch))
}
func (h *Host) FlowClosed(ctx context.Context, pk string, ref protocol.ResourceRef) error {
	if h.sip == nil {
		return nil
	}
	return h.sip.InvalidateExternalIngress(ctx, scopePrefix(pk, ref.Generation, ref.ConnectionEpoch)+ref.ID)
}
func (h *Host) ResourceFailed(ctx context.Context, pk string, ref protocol.ResourceRef, failure protocol.Error) error {
	var sessions []store.Session
	if e := h.repo.Read(ctx, func(db *gorm.DB) error {
		return db.Where("gateway_id = ? AND remote_id = ? AND resource_type = ? AND generation = ? AND connection_epoch = ? AND state <> ?", pk, ref.ID, ref.ResourceType, ref.Generation, ref.ConnectionEpoch, "closed").Find(&sessions).Error
	}); e != nil {
		return e
	}
	for _, session := range sessions {
		if h.media == nil {
			return core.Fail(503, "DEPENDENCY_UNAVAILABLE", "原媒体资源等待清理适配")
		}
		if e := h.media.StopGatewaySession(ctx, pk, session.ID, true); e != nil {
			return e
		}
	}
	return nil
}
func (h *Host) RecoverLocalSession(ctx context.Context, session core.RecoverySession) (bool, error) {
	if h.media == nil {
		return false, core.Fail(503, "DEPENDENCY_UNAVAILABLE", "未装配原媒体节点清理能力")
	}
	return h.media.RecoverLocalSession(ctx, session)
}
func (h *Host) SupportedGatewayCapabilities(ctx context.Context, config core.DesiredConfig) ([]string, error) {
	caps := []string{"sip_udp", "sip_tcp"}
	if h.media != nil {
		supported, e := h.media.Capabilities(ctx, config)
		if e != nil {
			return nil, e
		}
		caps = append(caps, supported...)
	}
	return caps, nil
}
func (h *Host) ValidateMediaServer(ctx context.Context, id *string, capabilities []string) error {
	if h.media != nil {
		return h.media.ValidateMediaServer(ctx, id, capabilities)
	}
	if id != nil || len(capabilities) > 0 {
		for _, name := range capabilities {
			if name != "sip_udp" && name != "sip_tcp" {
				return core.Fail(409, "CAPABILITY_UNSUPPORTED", "媒体驱动能力尚未装配")
			}
		}
		if id != nil {
			return core.Fail(409, "CAPABILITY_UNSUPPORTED", "媒体节点适配尚未装配")
		}
	}
	return nil
}
func (h *Host) StopGatewaySession(ctx context.Context, pk, id string, force bool) error {
	if h.media == nil {
		return core.Fail(503, "DEPENDENCY_UNAVAILABLE", "宿主会话清理尚未装配")
	}
	return h.media.StopGatewaySession(ctx, pk, id, force)
}

var _ core.SignalHost = (*Host)(nil)
var _ core.ManagementHost = (*Host)(nil)
var _ ipc.IngressGuard = (*Host)(nil)
