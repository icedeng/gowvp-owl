package owl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs"
	"github.com/gowvp/owl/pkg/zlm"
	"gorm.io/gorm"
)

type mediaHost struct {
	host    *Host
	media   sms.Core
	bridges sync.Map
}
type mediaBridge struct {
	deviceCode    string
	host          *mediaHost
	row           store.Session
	lease         protocol.Lease
	ref           protocol.ResourceRef
	ctx           context.Context
	cancel        context.CancelFunc
	binary        *transport.Binary
	rtp, rtcp     net.Conn
	release       func()
	wg            sync.WaitGroup
	closeMu       sync.Mutex
	closed        bool
	failOnce      sync.Once
	firstPacket   sync.Once
	localMu       sync.Mutex
	localSendIP   string
	localListener net.Listener
}

func (h *Host) AttachMedia(media sms.Core) {
	h.media = &mediaHost{host: h, media: media}
	if h.sip != nil {
		h.sip.SetExternalMediaPort(h)
	}
}

func (h *Host) CheckNewBusiness(ctx context.Context, code string, mode int8, purpose string) (bool, error) {
	if mode < 0 || mode > 2 {
		return false, fmt.Errorf("传输模式无效")
	}
	var device ipc.Device
	if err := h.db.WithContext(ctx).Select("id").Where("device_id = ?", code).First(&device).Error; err != nil {
		return false, err
	}
	var migrating int64
	if err := h.db.WithContext(ctx).Table("access_gateway_migration_items AS i").Joins("JOIN access_gateway_migrations AS m ON m.id=i.migration_id").Where("i.subject_type = ? AND i.subject_id = ? AND i.active_subject_key IS NOT NULL AND m.expires_at > ? AND m.state IN ?", subjectKind(code), device.ID, time.Now().UTC(), []string{"draining", "waiting_register"}).Count(&migrating).Error; err != nil {
		return false, err
	}
	if migrating > 0 {
		return false, core.Fail(409, "MIGRATION_DRAINING", "对象正在迁移，禁止创建新业务")
	}
	binding, err := h.repo.Binding(ctx, device.ID)
	if store.IsNotFound(err) || (err == nil && binding.IngressType == "direct") {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	manager := h.manager()
	if manager == nil || binding.GatewayID == nil {
		return true, core.Fail(503, "GATEWAY_UNAVAILABLE", "网关未启用")
	}
	row, err := h.repo.GetPK(ctx, *binding.GatewayID)
	if err != nil {
		return true, err
	}
	snapshot := manager.Snapshot(row.ID)
	if row.AdminState != "enabled" || row.ConfigRevision != row.AppliedRevision || snapshot.HealthState != "ready" || !store.EffectiveBinding(binding, snapshot) || binding.LastFlowID == nil {
		return true, core.Fail(503, "GATEWAY_UNAVAILABLE", "网关或设备入口尚未就绪")
	}
	capability := "rtp_" + []string{"udp", "tcp_passive", "tcp_active"}[mode]
	if purpose == "raw_download" {
		capability = "raw_tcp_download"
	}
	if purpose == "snapshot_upload" || purpose == "upgrade_download" {
		capability = "https_transfer"
	}
	for _, capability := range []string{capability} {
		if !slices.Contains(snapshot.Capabilities, capability) {
			return true, core.Fail(409, "CAPABILITY_UNSUPPORTED", "网关缺少能力 "+capability)
		}
	}
	return true, nil
}
func (h *Host) SSRCAllowed(ctx context.Context, ssrc string) (bool, error) {
	var count int64
	err := h.db.WithContext(ctx).Model(&store.SSRCReservation{}).Where("ssrc = ? AND (state = ? OR quarantine_until > ?)", ssrc, "reserved", time.Now().UTC()).Count(&count).Error
	return count == 0, err
}
func (h *Host) PrepareReceive(ctx context.Context, in gbs.ExternalReceiveSpec) (gbs.ExternalMediaLease, error) {
	var err error
	if in.SubjectType != "platform" {
		external, e := h.CheckNewBusiness(ctx, in.DeviceID, in.WireMode, in.Purpose)
		if e != nil || !external {
			return nil, e
		}
	}
	mh, ok := h.media.(*mediaHost)
	if !ok {
		return nil, core.Fail(503, "DEPENDENCY_UNAVAILABLE", "媒体转接未装配")
	}
	if in.MediaServer == nil || in.MediaServer.Disabled || in.MediaServer.IP == "" {
		return nil, fmt.Errorf("缺少可达媒体节点")
	}
	if !in.Sending && in.WireMode == 0 && in.RTCPPort == 0 {
		return nil, core.Fail(409, "CAPABILITY_UNSUPPORTED", "媒体驱动未返回实际 UDP RTCP 端口，请更新媒体节点补丁")
	}
	var binding store.BindingFields
	subjectType, subjectID := "device", ""
	if in.SubjectType == "platform" {
		subjectType, subjectID = "platform", in.SubjectID
		var platform store.PlatformBinding
		if err = h.db.WithContext(ctx).Where("subject_id = ?", subjectID).First(&platform).Error; err != nil {
			return nil, err
		}
		binding = platform.BindingFields
		if binding.GatewayID == nil {
			return nil, core.Fail(409, "INGRESS_CONFLICT", "平台未绑定网关")
		}
		configured, e := h.repo.Get(ctx, in.GatewayID)
		if e != nil {
			return nil, e
		}
		if configured.ID != *binding.GatewayID {
			return nil, core.Conflict("级联入口配置与绑定不一致")
		}
	} else {
		var device ipc.Device
		if err = h.db.WithContext(ctx).Select("id,device_id").Where("device_id = ?", in.DeviceID).First(&device).Error; err != nil {
			return nil, err
		}
		subjectType, subjectID = subjectKind(device.DeviceID), device.ID
		deviceBinding, e := h.repo.Binding(ctx, device.ID)
		if e != nil {
			return nil, e
		}
		binding = deviceBinding.BindingFields
	}
	if binding.GatewayID == nil || binding.LastFlowID == nil {
		return nil, core.Fail(503, "GATEWAY_UNAVAILABLE", "对象尚未注册")
	}
	pk := *binding.GatewayID
	manager := h.manager()
	snapshot := manager.Snapshot(pk)
	if !store.EffectiveBinding(store.Binding{BindingFields: binding}, snapshot) {
		return nil, core.Fail(503, "GATEWAY_UNAVAILABLE", "对象需要在当前网关代次重新注册")
	}
	ownerID, ownerEpoch, owned := manager.OwnerLease(pk)
	if !owned {
		return nil, protocol.Failure(protocol.OwnerConflict, "当前实例不是网关 owner")
	}
	config, err := h.repo.GetPK(ctx, pk)
	if err != nil {
		return nil, err
	}
	mode := []string{"udp", "tcp_passive", "tcp_active"}[in.WireMode]
	localMode := mode
	if mode == "tcp_active" {
		localMode = "tcp_passive"
	}
	rtcp := "udp_separate"
	if in.WireMode != 0 {
		rtcp = "disabled"
		if in.TCPRTCP {
			rtcp = "tcp_mux"
		}
	}
	mediaJSON, _ := json.Marshal(in.MediaServer)
	if in.Sending {
		var data map[string]any
		_ = json.Unmarshal(mediaJSON, &data)
		data["gateway_send"] = true
		data["send_vhost"] = in.SendVHost
		data["send_app"] = in.SendApp
		data["send_stream"] = in.SendStream
		data["send_ssrc"] = in.SSRC
		mediaJSON, _ = json.Marshal(data)
	}
	channel := in.ChannelID
	direction := in.Direction
	if direction == "" {
		direction = "device_to_a"
	}
	recvSSRC := &in.SSRC
	if in.Sending {
		recvSSRC = nil
		direction = "a_to_device"
		in.SendSSRC = &in.SSRC
	}
	row := store.Session{Base: store.Base{ID: uuid.NewString()}, GatewayID: pk, ResourceType: "media", RemoteID: uuid.NewString(), Generation: snapshot.Generation, ConnectionEpoch: mustEpoch(snapshot.Epoch), OwnerEpoch: ownerEpoch, SubjectType: subjectType, SubjectID: subjectID, ChannelID: &channel, BindingRevision: binding.BindingRevision, ConfigRevision: config.ConfigRevision, Purpose: in.Purpose, WireMode: mode, LocalMode: localMode, Direction: direction, SendSSRC: in.SendSSRC, SSRC: recvSSRC, MediaServerID: &in.MediaServer.ID, StreamID: &in.StreamID, MediaNodeSnapshot: string(mediaJSON), State: "allocated"}
	err = h.repo.Transaction(ctx, func(tx *gorm.DB) error {
		if e := store.CheckOwner(tx, pk, ownerID, ownerEpoch); e != nil {
			return e
		}
		current, e := store.LockGateway(tx, pk)
		if e != nil {
			return e
		}
		if e = store.CheckBusinessBinding(tx, row.SubjectType, row.SubjectID, pk, row.BindingRevision); e != nil {
			return e
		}
		if current.AdminState != "enabled" || current.ConfigRevision != row.ConfigRevision {
			return protocol.Failure(protocol.ConfigConflict, "媒体准备时配置已改变")
		}
		if e = tx.Where("ssrc = ? AND state = ? AND quarantine_until <= ?", in.SSRC, "quarantined", time.Now().UTC()).Delete(&store.SSRCReservation{}).Error; e != nil {
			return e
		}
		if e = tx.Create(&store.SSRCReservation{SSRC: in.SSRC, SessionID: row.ID, State: "reserved", ReservedAt: time.Now().UTC()}).Error; e != nil {
			return e
		}
		if in.SendSSRC != nil && *in.SendSSRC != in.SSRC {
			if e = tx.Where("ssrc = ? AND state = ? AND quarantine_until <= ?", *in.SendSSRC, "quarantined", time.Now().UTC()).Delete(&store.SSRCReservation{}).Error; e != nil {
				return e
			}
			if e = tx.Create(&store.SSRCReservation{SSRC: *in.SendSSRC, SessionID: row.ID, State: "reserved", ReservedAt: time.Now().UTC()}).Error; e != nil {
				return e
			}
		}
		return tx.Create(&row).Error
	})
	if err != nil {
		return nil, err
	}
	bridgeCtx, cancel := context.WithCancel(context.Background())
	b := &mediaBridge{deviceCode: in.DeviceID, host: mh, row: row, ctx: bridgeCtx, cancel: cancel, ref: protocol.ResourceRef{ResourceType: "media", Generation: snapshot.Generation, ConnectionEpoch: snapshot.Epoch}}
	mh.bridges.Store(row.ID, b)
	// 在 open 前保存宿主身份，即使应答丢失也能按 operation_id 找回并精确关闭。
	result, err := manager.CallExact(ctx, pk, b.ref, "media.open", protocol.MediaOpen{OperationID: row.ID, Spec: protocol.MediaSpec{SubjectType: subjectType, SubjectID: subjectID, ChannelID: &channel, Purpose: in.Purpose, Direction: row.Direction, SendSSRC: in.SendSSRC, WireMode: mode, SSRC: recvSSRC, FlowID: *binding.LastFlowID, ConfigRevision: protocol.Number(config.ConfigRevision), SourceIPs: in.SourceIPs, RTCP: rtcp}})
	if err != nil {
		return b, err
	}
	b.lease = result.(*protocol.MediaOpenResult).Lease
	b.ref.ID = b.lease.SessionID
	b.row.RemoteID = b.ref.ID
	b.row.PublicEndpoint = store.Endpoints{RTP: b.lease.PublicRTP, RTCP: b.lease.PublicRTCP}
	if err = h.repo.SaveSession(ctx, &b.row, ownerID, ownerEpoch); err != nil {
		return b, err
	}
	b.release, err = manager.RegisterLease(pk, b.ref)
	if err != nil {
		return b, err
	}
	if in.Sending {
		if err = b.prepareLocalSender(ctx, in.MediaServer.IP, in.WireMode); err != nil {
			return b, err
		}
	} else {
		dialer := net.Dialer{Timeout: 5 * time.Second}
		network := "udp"
		if in.WireMode != 0 {
			network = "tcp"
		}
		b.rtp, err = dialer.DialContext(ctx, network, net.JoinHostPort(in.MediaServer.IP, fmt.Sprint(in.Port)))
		if err != nil {
			return b, err
		}
		if in.WireMode == 0 {
			b.rtcp, err = dialer.DialContext(ctx, "udp", net.JoinHostPort(in.MediaServer.IP, fmt.Sprint(in.RTCPPort)))
			if err != nil {
				return b, err
			}
		}
		b.row.LocalEndpoint.RTP = &protocol.Endpoint{IP: in.MediaServer.IP, Port: in.Port}
		if in.RTCPPort > 0 {
			b.row.LocalEndpoint.RTCP = &protocol.Endpoint{IP: in.MediaServer.IP, Port: in.RTCPPort}
		}
	}
	b.binary, err = manager.Attach(ctx, pk, b.ref, mode, b.row.Direction)
	if err != nil {
		return b, err
	}
	b.row.Timings = &core.PathTimings{WSSAttachedAt: timePointer(time.Now().UTC())}
	b.row.State = "attached"
	now := time.Now().UTC()
	expires := now.Add(time.Duration(b.lease.LeaseTTLMS) * time.Millisecond)
	b.row.LastRenewedAt = &now
	b.row.ExpiresAt = &expires
	if err = h.repo.SaveSession(ctx, &b.row, ownerID, ownerEpoch); err != nil {
		return b, err
	}
	if !in.Sending || in.WireMode == 0 {
		b.wg.Add(1)
		go b.fromGateway()
	}
	if in.Sending && in.WireMode != 0 {
		b.wg.Add(1)
		go b.acceptLocalSender()
	} else if in.WireMode != 0 {
		b.wg.Add(1)
		go b.toGateway(b.rtp, protocol.TCPBytes)
	} else {
		b.wg.Add(1)
		go b.toGateway(b.rtcp, protocol.UDPRTCP)
		if direction == "duplex" || in.Sending {
			b.wg.Add(1)
			go b.toGateway(b.rtp, protocol.UDPRTP)
		}
	}
	return b, nil
}
func mustEpoch(v protocol.Decimal) int64 { n, _ := v.Int64(); return n }
func (b *mediaBridge) AdvertisedEndpoint() (string, int) {
	if b.lease.PublicRTP == nil {
		return "", 0
	}
	return b.lease.PublicRTP.IP, b.lease.PublicRTP.Port
}
func (b *mediaBridge) ConnectRemote(ctx context.Context, ip string, port int) error {
	_, err := b.host.host.manager().CallExact(ctx, b.row.GatewayID, b.ref, "media.connect", protocol.MediaConnect{OperationID: uuid.NewString(), SessionID: b.ref.ID, Target: protocol.Endpoint{IP: ip, Port: port}})
	return err
}
func (b *mediaBridge) fail(err error) {
	if b.ctx.Err() != nil {
		return
	}
	b.failOnce.Do(func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			b.closeMu.Lock()
			b.row.Error = protocol.Failure(protocol.IOError, "媒体桥已中断，等待原资源回收")
			if manager := b.host.host.manager(); manager != nil {
				if owner, epoch, owned := manager.OwnerLease(b.row.GatewayID); owned {
					_ = b.host.host.repo.SaveSession(ctx, &b.row, owner, epoch)
				}
			}
			b.closeMu.Unlock()
			_ = b.host.StopGatewaySession(ctx, b.row.GatewayID, b.row.ID, true)
		}()
	})
}

func (b *mediaBridge) receivedPacket() {
	b.firstPacket.Do(func() {
		go func() {
			b.closeMu.Lock()
			defer b.closeMu.Unlock()
			if b.ctx.Err() != nil || b.row.State != "attached" {
				return
			}
			ctx, cancel := context.WithTimeout(b.ctx, 3*time.Second)
			defer cancel()
			manager := b.host.host.manager()
			if manager == nil {
				return
			}
			owner, epoch, owned := manager.OwnerLease(b.row.GatewayID)
			if !owned {
				return
			}
			if b.row.Timings == nil {
				b.row.Timings = &core.PathTimings{}
			}
			b.row.Timings.AForwardedAt = timePointer(time.Now().UTC())
			b.row.State = "receiving"
			_ = b.host.host.repo.SaveSession(ctx, &b.row, owner, epoch)
		}()
	})
}
func value(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
func (b *mediaBridge) fromGateway() {
	defer b.wg.Done()
	for {
		msg, err := b.binary.Read(b.ctx)
		if err != nil {
			b.fail(err)
			return
		}
		conn := b.rtp
		if msg.Kind == protocol.UDPRTCP {
			conn = b.rtcp
		}
		if conn == nil {
			b.fail(fmt.Errorf("未协商的本地媒体方向"))
			return
		}
		if msg.Kind == protocol.EOF {
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			}
			return
		}
		if msg.Kind == protocol.UDPRTP {
			if err = protocol.ValidateRTP(msg.Payload, b.row.SSRC); err != nil {
				b.fail(err)
				return
			}
		}
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		data := msg.Payload
		for len(data) > 0 {
			n, e := conn.Write(data)
			if e != nil {
				b.fail(e)
				return
			}
			if n == 0 {
				b.fail(io.ErrShortWrite)
				return
			}
			data = data[n:]
		}
		if msg.Kind == protocol.UDPRTP || msg.Kind == protocol.TCPBytes {
			b.receivedPacket()
		}
	}
}
func (b *mediaBridge) toGateway(conn net.Conn, kind byte) {
	defer b.wg.Done()
	buffer := make([]byte, 65536)
	for {
		n, err := conn.Read(buffer)
		if n > 0 {
			if kind == protocol.UDPRTP && protocol.ValidateRTP(buffer[:n], b.row.SendSSRC) != nil {
				b.fail(fmt.Errorf("本地媒体节点返回非预期 RTP"))
				return
			}
			if kind == protocol.UDPRTCP && protocol.ValidateRTCP(buffer[:n]) != nil {
				b.fail(fmt.Errorf("本地媒体节点返回非法 RTCP"))
				return
			}
			if e := b.binary.Send(b.ctx, kind, buffer[:n]); e != nil {
				b.fail(e)
				return
			}
			if kind == protocol.UDPRTP || kind == protocol.TCPBytes {
				b.receivedPacket()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && kind == protocol.TCPBytes {
				_ = b.binary.Send(b.ctx, protocol.EOF, nil)
				return
			}
			b.fail(err)
			return
		}
	}
}
func (b *mediaBridge) Close(ctx context.Context) error {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	if b.closed {
		return nil
	}
	b.closeLocal()
	manager := b.host.host.manager()
	owner, epoch, owned := manager.OwnerLease(b.row.GatewayID)
	if !owned {
		return protocol.Failure(protocol.OwnerConflict, "清理等待网关 owner")
	}
	resolved, err := manager.ResolveResource(ctx, b.row.GatewayID, b.ref, b.row.ID)
	if err != nil {
		return err
	}
	if resolved != nil {
		result, e := manager.CallExact(ctx, b.row.GatewayID, *resolved, "resources.close", protocol.ResourceClose{OperationID: uuid.NewString(), Ref: *resolved, Reason: "host_cleanup"})
		if e != nil {
			return e
		}
		if !result.(*protocol.ResourceCloseResult).Closed {
			return fmt.Errorf("B 资源尚未关闭")
		}
	}
	b.row.State = "closing"
	b.row.CleanupSteps = store.CleanupSteps{BusinessInvalidated: true, RemoteClosed: true, BridgeClosed: true, MediaClosed: false, Committed: false}
	if err := b.host.host.repo.SaveSession(ctx, &b.row, owner, epoch); err != nil {
		return err
	}
	b.closed = true
	return nil
}
func (b *mediaBridge) FinishCleanup(ctx context.Context, dialog, media bool) error {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	manager := b.host.host.manager()
	owner, epoch, owned := manager.OwnerLease(b.row.GatewayID)
	if !owned {
		return protocol.Failure(protocol.OwnerConflict, "会话终态等待 owner")
	}
	b.row.CleanupSteps.DialogClosed = dialog
	b.row.CleanupSteps.MediaClosed = media
	if !b.closed || !dialog || !media {
		return b.host.host.repo.SaveSession(ctx, &b.row, owner, epoch)
	}
	now := time.Now().UTC()
	b.row.State = "closed"
	b.row.ClosedAt = &now
	b.row.CleanupSteps.Committed = true
	if err := b.host.host.repo.Transaction(ctx, func(tx *gorm.DB) error {
		if err := store.CheckOwner(tx, b.row.GatewayID, owner, epoch); err != nil {
			return err
		}
		if err := tx.Save(&b.row).Error; err != nil {
			return err
		}
		return tx.Model(&store.SSRCReservation{}).Where("session_id = ?", b.row.ID).Updates(map[string]any{"state": "quarantined", "quarantine_until": now.Add(24 * time.Hour)}).Error
	}); err != nil {
		return err
	}
	if b.release != nil {
		b.release()
	}
	b.host.bridges.Delete(b.row.ID)
	return nil
}

func (m *mediaHost) StopGatewaySession(ctx context.Context, pk, id string, force bool) error {
	row, err := m.host.repo.Session(ctx, pk, id)
	if err != nil {
		return err
	}
	if row.ResourceType == "transfer" {
		return m.host.closeTransfer(ctx, pk, id)
	}
	if row.State == "closed" {
		return nil
	}
	if row.SubjectType == "platform" && m.host.isUpstreamPlatform(ctx, row.SubjectID) {
		if m.host.sip != nil {
			if err = m.host.sip.StopExternalPlatformMedia(ctx, row.SubjectID, value(row.StreamID), value(row.SendSSRC)); err != nil {
				return err
			}
		}
	} else {
		var device ipc.Device
		if err = m.host.db.WithContext(ctx).Select("device_id").Where("id = ?", row.SubjectID).First(&device).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if m.host.sip != nil {
			if !force {
				if refs := m.host.sip.ExternalStreamReferences(device.DeviceID, value(row.StreamID)); refs > 0 {
					e := core.Fail(409, "SOURCE_IN_USE", "共享媒体源仍有业务引用")
					e.Details["references"] = refs
					return e
				}
			}
			if err = m.host.sip.StopExternalMediaStream(ctx, device.DeviceID, value(row.StreamID)); err != nil {
				return err
			}
		}
	}
	// 对话可能已丢失；仍须按持久化身份清理原 B 租约及原媒体节点，不能只以未找到对话为成功。
	current, err := m.host.repo.Session(ctx, pk, id)
	if err != nil {
		return err
	}
	if current.State == "closed" {
		return nil
	}
	recovery := core.RecoverySession{ID: id, GatewayPK: pk, Ref: protocol.ResourceRef{ResourceType: row.ResourceType, ID: row.RemoteID, Generation: row.Generation, ConnectionEpoch: protocol.Number(row.ConnectionEpoch)}, MediaNodeSnapshot: row.MediaNodeSnapshot, StreamID: row.StreamID}
	if b, ok := m.bridges.Load(id); ok {
		bridge := b.(*mediaBridge)
		if err = bridge.Close(ctx); err != nil {
			return err
		}
	}
	manager := m.host.manager()
	owner, epoch, owned := manager.OwnerLease(pk)
	if !owned {
		return protocol.Failure(protocol.OwnerConflict, "会话回收等待网关 owner")
	}
	resolved, err := manager.ResolveResource(ctx, pk, recovery.Ref, id)
	if err != nil {
		return err
	}
	if resolved != nil {
		result, err := manager.CallExact(ctx, pk, *resolved, "resources.close", protocol.ResourceClose{OperationID: uuid.NewString(), Ref: *resolved, Reason: "host_cleanup"})
		if err != nil {
			return err
		}
		if !result.(*protocol.ResourceCloseResult).Closed {
			return fmt.Errorf("B 资源尚未关闭")
		}
	}
	local, err := m.RecoverLocalSession(ctx, recovery)
	outcome := core.RecoveryOutcome{RemoteClosed: true, LocalClosed: local, DialogUnavailable: true}
	if err != nil {
		outcome.Error = protocol.Failure(protocol.IOError, "原媒体节点清理失败")
	}
	if saveErr := m.host.repo.SaveRecovery(ctx, pk, owner, epoch, recovery, outcome); saveErr != nil {
		return saveErr
	}
	return err
}

// closeLocal 与 Close/恢复共用；关闭 listener 后再等待 Accept/转发协程退出。
func (b *mediaBridge) closeLocal() {
	b.cancel()
	if b.binary != nil {
		_ = b.binary.Close()
	}
	b.localMu.Lock()
	if b.localListener != nil {
		_ = b.localListener.Close()
	}
	if b.rtp != nil {
		_ = b.rtp.Close()
	}
	if b.rtcp != nil {
		_ = b.rtcp.Close()
	}
	b.localMu.Unlock()
	b.wg.Wait()
}
func (m *mediaHost) RecoverLocalSession(ctx context.Context, in core.RecoverySession) (bool, error) {
	if in.Ref.ResourceType == "transfer" {
		if v, ok := m.host.transfers.Load(in.ID); ok {
			b := v.(*transferBridge)
			b.mu.Lock()
			b.dispose()
			if b.release != nil {
				b.release()
			}
			b.mu.Unlock()
			m.host.transfers.Delete(in.ID)
		}
		if !in.TransferCommitted {
			if err := m.host.failTransferTask(ctx, in.MediaNodeSnapshot); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	if b, ok := m.bridges.Load(in.ID); ok {
		bridge := b.(*mediaBridge)
		bridge.closeMu.Lock()
		bridge.closeLocal()
		if bridge.release != nil {
			bridge.release()
		}
		m.bridges.Delete(in.ID)
		bridge.closeMu.Unlock()
	}
	if in.StreamID == nil || in.MediaNodeSnapshot == "" {
		return true, nil
	}
	var server sms.MediaServer
	if err := json.Unmarshal([]byte(in.MediaNodeSnapshot), &server); err != nil {
		return false, err
	}
	var send struct {
		Sending bool   `json:"gateway_send"`
		Vhost   string `json:"send_vhost"`
		App     string `json:"send_app"`
		Stream  string `json:"send_stream"`
		SSRC    string `json:"send_ssrc"`
	}
	if err := json.Unmarshal([]byte(in.MediaNodeSnapshot), &send); err != nil {
		return false, err
	}
	if send.Sending {
		_, err := m.media.StopSendRTPContext(ctx, &server, zlm.StopSendRTPRequest{Vhost: send.Vhost, App: send.App, Stream: send.Stream, SSRC: send.SSRC})
		return err == nil, err
	}
	_, err := m.media.CloseRTPServerContext(ctx, &server, zlm.CloseRTPServerRequest{StreamID: *in.StreamID})
	return err == nil, err
}
func (m *mediaHost) ValidateMediaServer(ctx context.Context, id *string, capabilities []string) error {
	if id == nil {
		return nil
	}
	server, err := m.media.GetMediaServer(ctx, *id)
	if err != nil {
		return err
	}
	if server.Disabled {
		return core.Fail(409, "MEDIA_SERVER_DISABLED", "默认媒体节点已停用")
	}
	if server.Type != "" && server.Type != sms.ProtocolZLMediaKit {
		for _, c := range capabilities {
			if c == "rtcp_tcp_mux" || c == "voice_duplex" {
				return core.Fail(409, "CAPABILITY_UNSUPPORTED", "媒体驱动不支持 TCP RTCP/双向语音")
			}
		}
	}
	return nil
}
func (m *mediaHost) Capabilities(ctx context.Context, c core.DesiredConfig) ([]string, error) {
	return []string{"rtp_udp", "rtp_tcp_passive", "rtp_tcp_active", "rtcp_udp", "rtcp_tcp_mux", "playback", "rtp_download", "voice_duplex", "broadcast", "cascade", "raw_tcp_download", "https_transfer"}, m.ValidateMediaServer(ctx, c.DefaultMediaServerID, c.RequiredCapabilities)
}

var _ gbs.ExternalMediaPort = (*Host)(nil)

func timePointer(t time.Time) *time.Time { return &t }
