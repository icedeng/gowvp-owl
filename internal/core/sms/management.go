package sms

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/reason"
	"github.com/ixugo/goddd/pkg/web"
)

var mediaServerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
var mediaHostLabelPattern = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// MediaServerUsageStorer 将通道引用检查留在持久层，避免管理接口绕过保护。
type MediaServerUsageStorer interface {
	MediaServerUsage(context.Context, string) (bound, active int64, err error)
}

// LockMediaServerAssignment 避免通道绑定与节点删除交错执行。
func (n *NodeManager) LockMediaServerAssignment() func() {
	n.managementMu.RLock()
	return n.managementMu.RUnlock
}

func (n *NodeManager) beginMediaOperation(ctx context.Context, server *MediaServer) (func(), error) {
	unlock := n.LockMediaServerAssignment()
	if server == nil {
		unlock()
		return nil, fmt.Errorf("媒体节点不能为空")
	}
	var current MediaServer
	if err := n.storer.MediaServer().Get(ctx, &current, orm.Where("id=?", server.ID)); err != nil {
		unlock()
		return nil, err
	}
	if current.Disabled || server.Disabled {
		unlock()
		return nil, reason.ErrBadRequest.WithMsg("媒体节点已停用")
	}
	return unlock, nil
}

func validMediaHost(host string) bool {
	if host == "" || strings.ContainsAny(host, "/?#@ \\:\t\r\n") {
		return net.ParseIP(host) != nil
	}
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !mediaHostLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

type MediaServerRecordingStorer interface {
	MediaServerRecordingCount(context.Context, string) (int64, error)
}

func normalizeMediaServer(server *MediaServer) error {
	for _, dir := range []*string{&server.RecordPath, &server.RecordMountPath} {
		*dir = strings.TrimSpace(*dir)
		if *dir != "" {
			if !filepath.IsAbs(*dir) || filepath.Clean(*dir) == string(filepath.Separator) {
				return reason.ErrBadRequest.WithMsg("录像目录须为绝对路径，不能是文件系统根目录")
			}
			*dir = filepath.Clean(*dir)
		}
	}
	if server.RecordMountPath != "" && server.RecordPath == "" && server.ID != DefaultMediaServerID {
		return reason.ErrBadRequest.WithMsg("请同时填写 ZLM 录像目录")
	}

	server.IP, server.HookIP, server.SDPIP = strings.TrimSpace(server.IP), strings.TrimSpace(server.HookIP), strings.TrimSpace(server.SDPIP)
	server.Type = strings.ToLower(strings.TrimSpace(server.Type))
	if server.Type == "" {
		server.Type = ProtocolZLMediaKit
	}
	if server.Type != ProtocolZLMediaKit && server.Type != ProtocolLalmax {
		return reason.ErrBadRequest.WithMsg("不支持的媒体节点类型")
	}
	if !validMediaHost(server.IP) || !validMediaHost(server.HookIP) || (server.SDPIP != "" && !validMediaHost(server.SDPIP)) {
		return reason.ErrBadRequest.WithMsg("请填写有效的节点地址、回调地址和 SDP 地址（不含协议或端口）")
	}
	if server.Ports.HTTP < 1 || server.Ports.HTTP > 65535 {
		return reason.ErrBadRequest.WithMsg("API 端口须为 1–65535")
	}
	if strings.TrimSpace(server.Secret) == "" {
		return reason.ErrBadRequest.WithMsg("请填写节点 API 密钥")
	}
	parts := strings.Split(strings.TrimSpace(server.RTPPortRange), "-")
	if len(parts) != 2 {
		return reason.ErrBadRequest.WithMsg("RTP 端口范围格式应为 30000-50000")
	}
	start, e1 := strconv.Atoi(parts[0])
	end, e2 := strconv.Atoi(parts[1])
	if e1 != nil || e2 != nil || start < 1 || end > 65535 || start >= end {
		return reason.ErrBadRequest.WithMsg("RTP 端口范围须在 1–65535 之间且起始小于结束")
	}
	server.RTPPortRange = fmt.Sprintf("%d-%d", start, end)
	return nil
}

func (c Core) ensureUniqueEndpoint(ctx context.Context, server *MediaServer) error {
	items, _, err := c.listMediaServers(ctx, &FindMediaServerInput{PagerFilter: web.NewPagerFilterMaxSize()})
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ID != server.ID && strings.EqualFold(item.IP, server.IP) && item.Ports.HTTP == server.Ports.HTTP {
			return reason.ErrBadRequest.WithMsg("该 API 地址和端口已被其他媒体节点使用")
		}
	}
	return nil
}

func (c Core) CreateMediaServer(ctx context.Context, in *AddMediaServerInput) (*MediaServer, error) {
	c.managementMu.Lock()
	defer c.managementMu.Unlock()
	id := strings.TrimSpace(in.ID)
	if !mediaServerIDPattern.MatchString(id) || strings.EqualFold(id, DefaultMediaServerID) {
		return nil, reason.ErrBadRequest.WithMsg("节点标识须为 1–64 位字母、数字、下划线或短横线，且不能使用 local")
	}
	var existing MediaServer
	if err := c.storer.MediaServer().Get(ctx, &existing, orm.Where("id=?", id)); err == nil {
		return nil, reason.ErrBadRequest.WithMsg("节点标识已存在")
	} else if !orm.IsErrRecordNotFound(err) {
		return nil, err
	}
	out := &MediaServer{ID: id, IP: in.IP, HookIP: in.HookIP, SDPIP: in.SDPIP, Type: in.Type, Secret: in.Secret, RTPPortRange: in.RTPPortRange}
	out.Ports.HTTP = in.HTTPPort
	if in.RecordPath != nil {
		out.RecordPath = *in.RecordPath
	}
	if in.RecordMountPath != nil {
		out.RecordMountPath = *in.RecordMountPath
	}
	if err := normalizeMediaServer(out); err != nil {
		return nil, err
	}
	if err := c.ensureUniqueEndpoint(ctx, out); err != nil {
		return nil, err
	}
	if err := c.storer.MediaServer().Create(ctx, out); err != nil {
		return nil, reason.ErrDB.WithMsg("保存媒体节点失败")
	}
	// 连接失败保留节点，便于修改地址或密钥；后台继续尝试重连。
	_ = c.connection(out, int(c.serverPort.Load()))
	return c.MediaServerSnapshot(ctx, id)
}

func (c Core) ensureUnused(ctx context.Context, server *MediaServer, deleting bool) error {
	store, ok := c.storer.MediaServer().(MediaServerUsageStorer)
	if !ok {
		return reason.ErrServer.WithMsg("媒体节点引用检查不可用")
	}
	bound, active, err := store.MediaServerUsage(ctx, server.ID)
	if err != nil {
		return err
	}
	if deleting && bound > 0 {
		return reason.ErrBadRequest.Withf("节点仍绑定 %d 个通道，请先调整通道绑定", bound)
	}
	if active > 0 {
		return reason.ErrBadRequest.WithMsg("节点存在活动通道，请先停止预览、回放、下载、语音和录像")
	}
	if !server.Disabled && c.IsOnline(server.ID) {
		// RTP 接收端也涵盖尚未出流的会话，不能只检查已生成的媒体流。
		if server.Type == "" || server.Type == ProtocolZLMediaKit {
			for _, resource := range []string{"streams", "rtp_servers"} {
				result, err := c.InspectMediaServer(ctx, server.ID, resource, nil)
				if err != nil || result.Status != "ok" {
					return reason.ErrBadRequest.WithMsg("无法确认节点是否空闲，请恢复节点查询后重试")
				}
				if rows, ok := result.Data.([]any); !ok || len(rows) > 0 {
					return reason.ErrBadRequest.WithMsg("节点仍有媒体流或 RTP 会话，请先停止相关业务")
				}
			}
		} else {
			result, err := c.GetMediaList(server)
			if err != nil || len(result.Data) > 0 {
				return reason.ErrBadRequest.WithMsg("节点仍有媒体流或无法确认空闲，请先停止相关业务")
			}
		}
	}
	return nil
}

func (c Core) UpdateMediaServer(ctx context.Context, in *EditMediaServerInput, id string, serverPort int) (*MediaServer, error) {
	c.managementMu.Lock()
	defer c.managementMu.Unlock()
	current, err := c.GetMediaServer(ctx, id)
	if err != nil {
		return nil, err
	}
	next := *current
	next.IP, next.HookIP, next.SDPIP = in.IP, in.HookIP, in.SDPIP
	if in.RecordPath != nil {
		next.RecordPath = *in.RecordPath
	}
	if in.RecordMountPath != nil {
		next.RecordMountPath = *in.RecordMountPath
	}
	if in.Type != "" {
		next.Type = in.Type
	}
	if in.Secret != "" {
		next.Secret = in.Secret
	}
	if in.HTTPPort != 0 {
		next.Ports.HTTP = in.HTTPPort
	}
	if in.RTPPortRange != "" {
		next.RTPPortRange = in.RTPPortRange
	}
	if err := normalizeMediaServer(&next); err != nil {
		return nil, err
	}
	if err := c.ensureUniqueEndpoint(ctx, &next); err != nil {
		return nil, err
	}
	if err := c.ensureUnused(ctx, current, false); err != nil {
		return nil, err
	}
	if next.RecordPath != current.RecordPath {
		if err := c.ensureNoRecordings(ctx, id); err != nil {
			return nil, err
		}
	}
	if err := c.storer.MediaServer().Update(ctx, &MediaServer{}, func(b *MediaServer) {
		b.IP, b.HookIP, b.SDPIP, b.Type, b.Secret, b.Ports, b.RTPPortRange = next.IP, next.HookIP, next.SDPIP, next.Type, next.Secret, next.Ports, next.RTPPortRange
		b.RecordPath, b.RecordMountPath = next.RecordPath, next.RecordMountPath
		b.Status = false
	}, orm.Where("id=?", id)); err != nil {
		return nil, err
	}
	_ = c.connection(&next, serverPort)
	return c.MediaServerSnapshot(ctx, id)
}

func (c Core) SetMediaServerDisabled(ctx context.Context, id string, disabled bool) (*MediaServer, error) {
	c.managementMu.Lock()
	defer c.managementMu.Unlock()
	current, err := c.GetMediaServer(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.Disabled == disabled {
		return c.MediaServerSnapshot(ctx, id)
	}
	if disabled {
		if err := c.ensureUnused(ctx, current, false); err != nil {
			return nil, err
		}
	}
	c.connectionMu.Lock()
	err = c.storer.MediaServer().Update(ctx, &MediaServer{}, func(b *MediaServer) {
		b.Disabled = disabled
		b.Status = false
	}, orm.Where("id=?", id))
	if err == nil {
		c.cacheServers.Delete(id)
	}
	c.connectionMu.Unlock()
	if err != nil {
		return nil, err
	}
	if !disabled {
		current.Disabled = false
		_ = c.connection(current, int(c.serverPort.Load()))
	}
	return c.MediaServerSnapshot(ctx, id)
}

func (c Core) DeleteMediaServer(ctx context.Context, id string) (*MediaServer, error) {
	c.managementMu.Lock()
	defer c.managementMu.Unlock()
	if id == DefaultMediaServerID {
		return nil, reason.ErrBadRequest.WithMsg("local 为默认媒体节点，不能删除")
	}
	current, err := c.GetMediaServer(ctx, id)
	if err != nil {
		return nil, err
	}
	if !current.Disabled {
		return nil, reason.ErrBadRequest.WithMsg("请先停用节点再删除")
	}
	if err := c.ensureNoRecordings(ctx, id); err != nil {
		return nil, err
	}
	if err := c.ensureUnused(ctx, current, true); err != nil {
		return nil, err
	}
	c.connectionMu.Lock()
	defer c.connectionMu.Unlock()
	if err := c.storer.MediaServer().Delete(ctx, &MediaServer{}, orm.Where("id=?", id)); err != nil {
		return nil, err
	}
	c.cacheServers.Delete(id)
	return current, nil
}

// 存在历史录像时不能删除节点或更换存储身份；挂载点可调整以恢复同一存储的访问。
func (c Core) ensureNoRecordings(ctx context.Context, id string) error {
	store, ok := c.storer.MediaServer().(MediaServerRecordingStorer)
	if !ok {
		return reason.ErrServer.WithMsg("录像引用检查不可用")
	}
	count, err := store.MediaServerRecordingCount(ctx, id)
	if err != nil {
		return err
	}
	if count > 0 {
		return reason.ErrBadRequest.WithMsg("节点仍有历史录像，不能删除节点或更换 ZLM 录像目录")
	}
	return nil
}
