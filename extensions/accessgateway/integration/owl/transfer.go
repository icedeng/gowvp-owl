package owl

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs"
	"gorm.io/gorm"
)

type transferBridge struct {
	host     *Host
	row      store.Session
	ref      protocol.ResourceRef
	opened   protocol.TransferOpened
	binary   *transport.Binary
	ctx      context.Context
	cancel   context.CancelFunc
	release  func()
	mu       sync.Mutex
	closed   bool
	file     *os.File
	pipe     net.Conn
	pipeErr  error
	sendDone chan error
}

func (h *Host) SetSnapshotSink(sink func(context.Context, string, string, string, []byte) error) {
	h.mu.Lock()
	h.snapshotSink = sink
	h.mu.Unlock()
}

func (h *Host) PrepareTransfer(ctx context.Context, in gbs.ExternalTransferSpec) (gbs.ExternalTransferLease, error) {
	var binding store.BindingFields
	var err error
	subjectType, subjectID := "device", ""
	if in.SubjectType == "platform" {
		subjectType, subjectID = "platform", in.SubjectID
		var platform store.PlatformBinding
		if err = h.db.WithContext(ctx).Where("subject_id = ?", subjectID).First(&platform).Error; err != nil {
			return nil, err
		}
		binding = platform.BindingFields
		configured, e := h.repo.Get(ctx, in.GatewayID)
		if e != nil {
			return nil, e
		}
		if binding.GatewayID == nil || *binding.GatewayID != configured.ID {
			return nil, core.Conflict("平台入口不一致")
		}
	}
	if subjectType == "device" {
		external, e := h.CheckNewBusiness(ctx, in.DeviceID, 0, in.Kind)
		if e != nil || !external {
			return nil, e
		}
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
	if binding.GatewayID == nil {
		return nil, core.Fail(503, "GATEWAY_UNAVAILABLE", "缺少网关绑定")
	}
	pk := *binding.GatewayID
	manager := h.manager()
	snap := manager.Snapshot(pk)
	owner, epoch, owned := manager.OwnerLease(pk)
	if !owned {
		return nil, protocol.Failure(protocol.OwnerConflict, "网关 owner 不可用")
	}
	config, err := h.repo.GetPK(ctx, pk)
	if err != nil {
		return nil, err
	}
	if in.MaxBytes < 1 {
		in.MaxBytes = 1 << 30
	}
	if in.Kind == "snapshot_upload" {
		in.MaxBytes = 12 << 20
	}
	bridgeCtx, cancel := context.WithTimeout(context.Background(), time.Hour)
	b := &transferBridge{host: h, ctx: bridgeCtx, cancel: cancel, ref: protocol.ResourceRef{ResourceType: "transfer", Generation: snap.Generation, ConnectionEpoch: snap.Epoch}}
	spec := protocol.TransferSpec{SubjectType: subjectType, TaskID: uuid.NewString(), SubjectID: subjectID, Kind: in.Kind, Direction: "device_to_a", MaxBytes: protocol.Number(in.MaxBytes), TTLMS: 3600000, ContentType: "application/octet-stream"}
	if in.Kind == "snapshot_upload" {
		spec.ContentType = "image/jpeg"
	}
	if in.Kind == "raw_download" {
		spec.WireMode = "tcp_active"
		if in.WireMode != "" {
			spec.WireMode = in.WireMode
		}
		if in.Direction != "" {
			spec.Direction = in.Direction
		}
		spec.SourceIPs = []string{}
	}
	if in.Kind == "upgrade_download" {
		// 固件只允许从管理员明确提供的公网 HTTPS 文件源获取，逐次 DNS 校验且拒绝重定向。
		u, e := url.Parse(strings.TrimSpace(in.SourceURL))
		if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			cancel()
			return nil, core.Fail(400, "INVALID_FILE_URL", "升级文件须为可验证的公网 HTTPS 地址")
		}
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DialContext: transport.PublicDialer}, Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if e != nil {
			cancel()
			return nil, e
		}
		resp, e := client.Do(req)
		if e != nil {
			cancel()
			return nil, e
		}
		defer resp.Body.Close()
		defer client.CloseIdleConnections()
		if resp.StatusCode != 200 || resp.ContentLength < 0 || resp.ContentLength > in.MaxBytes {
			cancel()
			return nil, core.Fail(400, "INVALID_FILE_SOURCE", "固件源须返回有界完整文件及 Content-Length")
		}
		file, e := os.CreateTemp("", "owl-gateway-firmware-*.bin")
		if e != nil {
			cancel()
			return nil, e
		}
		b.file = file
		failed := true
		defer func() {
			if failed {
				file.Close()
				os.Remove(file.Name())
			}
		}()
		digest := sha256.New()
		n, e := io.Copy(io.MultiWriter(file, digest), io.LimitReader(resp.Body, in.MaxBytes+1))
		if e != nil || n != resp.ContentLength {
			cancel()
			return nil, fmt.Errorf("固件下载长度或传输校验失败")
		}
		if e = file.Sync(); e != nil {
			cancel()
			return nil, e
		}
		if _, e = file.Seek(0, io.SeekStart); e != nil {
			cancel()
			return nil, e
		}
		spec.Direction = "a_to_device"
		length := protocol.Number(n)
		zero := protocol.Number(0)
		hash := hex.EncodeToString(digest.Sum(nil))
		spec.ContentLength = &length
		spec.SHA256 = &hash
		spec.RangeOffset = &zero
		spec.RangeLength = &length
		spec.MaxBytes = length
		failed = false
	}
	b.row = store.Session{Base: store.Base{ID: uuid.NewString()}, GatewayID: pk, ResourceType: "transfer", RemoteID: uuid.NewString(), Generation: snap.Generation, ConnectionEpoch: mustEpoch(snap.Epoch), OwnerEpoch: epoch, SubjectType: subjectType, SubjectID: subjectID, BindingRevision: binding.BindingRevision, ConfigRevision: config.ConfigRevision, Purpose: in.Kind, WireMode: spec.WireMode, LocalMode: "file", Direction: spec.Direction, MediaNodeSnapshot: encodeTask(in), State: "allocated"}
	if err = h.repo.Transaction(ctx, func(tx *gorm.DB) error {
		if err := store.CheckOwner(tx, pk, owner, epoch); err != nil {
			return err
		}
		current, err := store.LockGateway(tx, pk)
		if err != nil {
			return err
		}
		if current.AdminState != "enabled" || current.ConfigRevision != b.row.ConfigRevision {
			return core.Conflict("网关配置已改变")
		}
		if err = store.CheckBusinessBinding(tx, subjectType, subjectID, pk, binding.BindingRevision); err != nil {
			return err
		}
		return tx.Create(&b.row).Error
	}); err != nil {
		b.dispose()
		return nil, err
	}
	h.transfers.Store(b.row.ID, b)
	result, err := manager.CallExact(ctx, pk, b.ref, "transfer.open", protocol.TransferOpen{OperationID: b.row.ID, Spec: spec})
	if err != nil {
		return b, err
	}
	b.opened = *result.(*protocol.TransferOpened)
	b.ref.ID = b.opened.TransferID
	b.row.RemoteID = b.ref.ID
	b.row.PublicEndpoint.RTP = b.opened.PublicTCP
	if err = h.repo.SaveSession(ctx, &b.row, owner, epoch); err != nil {
		return b, err
	}
	b.release, err = manager.RegisterLease(pk, b.ref)
	if err != nil {
		return b, err
	}
	b.binary, err = manager.Attach(ctx, pk, b.ref, "", spec.Direction)
	if err != nil {
		return b, err
	}
	b.row.State = "attached"
	if err = h.repo.SaveSession(ctx, &b.row, owner, epoch); err != nil {
		return b, err
	}
	if in.Kind == "snapshot_upload" {
		go b.receiveSnapshot(in)
	} else if in.Kind == "upgrade_download" {
		go b.sendUpgrade(spec)
	}
	return b, nil
}
func (b *transferBridge) AdvertisedEndpoint() (string, int) {
	if b.opened.PublicTCP == nil {
		return "", 0
	}
	return b.opened.PublicTCP.IP, b.opened.PublicTCP.Port
}
func (b *transferBridge) PublicURL() string { return value(b.opened.PublicURL) }
func (b *transferBridge) Connect(ctx context.Context, address string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.row.Direction == "a_to_device" {
		return b.rawSender(ctx)
	}
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		return nil, err
	}
	_, err = b.host.manager().CallExact(ctx, b.row.GatewayID, b.ref, "transfer.connect", protocol.TransferConnect{OperationID: uuid.NewString(), TransferID: b.ref.ID, Target: protocol.Endpoint{IP: host, Port: port}})
	if err != nil {
		return nil, err
	}
	reader, writer := net.Pipe()
	b.mu.Lock()
	if b.closed || b.pipe != nil {
		b.mu.Unlock()
		reader.Close()
		writer.Close()
		return nil, context.Canceled
	}
	b.pipe = writer
	b.mu.Unlock()
	go func() {
		defer writer.Close()
		for {
			msg, e := b.binary.Read(b.ctx)
			if e != nil {
				b.setPipeError(e)
				return
			}
			if msg.Kind == protocol.EOF {
				return
			}
			if msg.Kind != protocol.FileBytes {
				b.setPipeError(fmt.Errorf("网关返回非文件数据"))
				return
			}
			if _, e = writer.Write(msg.Payload); e != nil {
				b.setPipeError(e)
				return
			}
		}
	}()
	return &transferConn{Conn: reader, bridge: b}, nil
}
func (b *transferBridge) Commit(ctx context.Context, n int64, hash string) error {
	if b.row.Direction == "a_to_device" && b.row.Purpose == "raw_download" {
		b.mu.Lock()
		done := b.sendDone
		b.mu.Unlock()
		if done == nil {
			return fmt.Errorf("文件发送尚未建立")
		}
		select {
		case err := <-done:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-b.ctx.Done():
			return b.ctx.Err()
		}
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			result, err := b.host.manager().CallExact(ctx, b.row.GatewayID, b.ref, "transfer.query", protocol.TransferQuery{TransferID: b.ref.ID})
			if err != nil {
				return err
			}
			q := result.(*protocol.TransferQueryResult)
			if q.Phase == "waiting_commit" {
				break
			}
			if q.State == "closed" || q.Phase == "failed" {
				return fmt.Errorf("网关文件发送已失败或关闭")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}

	r, err := b.host.manager().CallExact(ctx, b.row.GatewayID, b.ref, "transfer.finish", protocol.TransferFinish{OperationID: uuid.NewString(), TransferID: b.ref.ID, ReceivedBytes: protocol.Number(n), SHA256: hash})
	if err != nil {
		return err
	}
	if !r.(*protocol.TransferFinished).Committed {
		return fmt.Errorf("网关未确认文件提交")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	manager := b.host.manager()
	owner, epoch, owned := manager.OwnerLease(b.row.GatewayID)
	if !owned {
		return protocol.Failure(protocol.OwnerConflict, "任务提交等待 owner")
	}
	b.row.CleanupSteps.TransferCommitted = true
	return b.host.repo.SaveSession(ctx, &b.row, owner, epoch)
}
func (b *transferBridge) receiveSnapshot(in gbs.ExternalTransferSpec) {
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = b.Close(ctx)
	}()
	body := []byte{}
	digest := sha256.New()
	for {
		msg, e := b.binary.Read(b.ctx)
		if e != nil {
			return
		}
		if msg.Kind == protocol.EOF {
			break
		}
		if msg.Kind != protocol.FileBytes || int64(len(body)+len(msg.Payload)) > in.MaxBytes {
			return
		}
		body = append(body, msg.Payload...)
		digest.Write(msg.Payload)
	}
	b.host.mu.RLock()
	sink := b.host.snapshotSink
	b.host.mu.RUnlock()
	if sink == nil {
		return
	}
	if sink(b.ctx, in.DeviceID, in.CoverKey, in.TaskID, body) != nil {
		return
	}
	_ = b.Commit(b.ctx, int64(len(body)), hex.EncodeToString(digest.Sum(nil)))
}
func (b *transferBridge) sendUpgrade(spec protocol.TransferSpec) {
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = b.Close(ctx)
	}()
	buffer := make([]byte, 65536)
	for {
		n, e := b.file.Read(buffer)
		if n > 0 {
			if err := b.binary.Send(b.ctx, protocol.FileBytes, buffer[:n]); err != nil {
				return
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return
		}
	}
	if b.binary.Send(b.ctx, protocol.EOF, nil) != nil {
		return
	}
	// B 的 sent 只说明 HTTP 写完，查询确认后才提交文件传送；设备升级结果仍归原 SIP 状态机。
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			r, e := b.host.manager().CallExact(b.ctx, b.row.GatewayID, b.ref, "transfer.query", protocol.TransferQuery{TransferID: b.ref.ID})
			if e != nil {
				return
			}
			q := r.(*protocol.TransferQueryResult)
			if q.Phase == "waiting_commit" {
				n, _ := spec.ContentLength.Int64()
				_ = b.Commit(b.ctx, n, *spec.SHA256)
				return
			}
		}
	}
}
func (b *transferBridge) dispose() {
	b.cancel()
	if b.binary != nil {
		b.binary.Close()
	}
	if b.file != nil {
		b.file.Close()
		os.Remove(b.file.Name())
	}
	if b.pipe != nil {
		b.pipe.Close()
	}
}
func (b *transferBridge) Close(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.dispose()
	if !b.row.CleanupSteps.TransferCommitted {
		if err := b.host.failTransferTask(ctx, b.row.MediaNodeSnapshot); err != nil {
			return err
		}
	}
	manager := b.host.manager()
	owner, epoch, owned := manager.OwnerLease(b.row.GatewayID)
	if !owned {
		return protocol.Failure(protocol.OwnerConflict, "任务等待 owner 清理")
	}
	ref, err := manager.ResolveResource(ctx, b.row.GatewayID, b.ref, b.row.ID)
	if err != nil {
		return err
	}
	if ref != nil {
		result, e := manager.CallExact(ctx, b.row.GatewayID, *ref, "resources.close", protocol.ResourceClose{OperationID: uuid.NewString(), Ref: *ref, Reason: "task_cleanup"})
		if e != nil {
			return e
		}
		if !result.(*protocol.ResourceCloseResult).Closed {
			return fmt.Errorf("任务清理待确认")
		}
	}
	now := time.Now().UTC()
	b.row.State = "closed"
	b.row.ClosedAt = &now
	b.row.CleanupSteps = store.CleanupSteps{TransferCommitted: b.row.CleanupSteps.TransferCommitted, BusinessInvalidated: true, RemoteClosed: true, BridgeClosed: true, MediaClosed: true, Committed: true}
	if err = b.host.repo.SaveSession(ctx, &b.row, owner, epoch); err != nil {
		return err
	}
	if b.release != nil {
		b.release()
	}
	b.closed = true
	b.host.transfers.Delete(b.row.ID)
	return nil
}

// persistTransferClose 可用于进程重启后的恢复，任务字节不会在新代次自动续传。
func (h *Host) closeTransfer(ctx context.Context, pk, id string) error {
	if v, ok := h.transfers.Load(id); ok {
		return v.(*transferBridge).Close(ctx)
	}
	row, err := h.repo.Session(ctx, pk, id)
	if err != nil || row.State == "closed" {
		return err
	}
	b := &transferBridge{host: h, row: row, ref: protocol.ResourceRef{ResourceType: row.ResourceType, ID: row.RemoteID, Generation: row.Generation, ConnectionEpoch: protocol.Number(row.ConnectionEpoch)}, cancel: func() {}}
	return b.Close(ctx)
}

var _ gbs.ExternalTransferPort = (*Host)(nil)

func encodeTask(in gbs.ExternalTransferSpec) string { data, _ := json.Marshal(in); return string(data) }

func (h *Host) failTransferTask(ctx context.Context, snapshot string) error {
	var spec gbs.ExternalTransferSpec
	if err := json.Unmarshal([]byte(snapshot), &spec); err != nil {
		return err
	}
	if spec.Kind != "snapshot_upload" && spec.Kind != "upgrade_download" {
		return nil
	}
	if h.sip == nil {
		return core.Fail(503, "DEPENDENCY_UNAVAILABLE", "原任务状态等待国标服务恢复")
	}
	return h.sip.FailExternalTransfer(ctx, spec)
}
func (b *transferBridge) rawSender(ctx context.Context) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader, writer := net.Pipe()
	b.mu.Lock()
	if b.closed || b.pipe != nil {
		b.mu.Unlock()
		reader.Close()
		writer.Close()
		return nil, fmt.Errorf("传输已经连接或关闭")
	}
	b.pipe = reader
	b.sendDone = make(chan error, 1)
	done := b.sendDone
	b.mu.Unlock()
	go func() {
		defer reader.Close()
		var result error
		defer func() { b.setPipeError(result); done <- result; close(done) }()
		buf := make([]byte, 65536)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				if e := b.binary.Send(b.ctx, protocol.FileBytes, buf[:n]); e != nil {
					result = e
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					result = b.binary.Send(b.ctx, protocol.EOF, nil)
				} else {
					result = err
				}
				return
			}
		}
	}()
	return &transferConn{Conn: writer, bridge: b}, nil
}

// net.Pipe 的 EOF 不能掩盖 WSS 中断；否则未知长度的下载会被误判成功。
type transferConn struct {
	net.Conn
	bridge *transferBridge
}

func (b *transferBridge) setPipeError(err error) {
	if err == nil {
		return
	}
	b.mu.Lock()
	if b.pipeErr == nil {
		b.pipeErr = err
	}
	b.mu.Unlock()
}
func (c *transferConn) result(n int, err error) (int, error) {
	if err != nil {
		c.bridge.mu.Lock()
		cause := c.bridge.pipeErr
		c.bridge.mu.Unlock()
		if cause != nil {
			return n, cause
		}
	}
	return n, err
}
func (c *transferConn) Read(p []byte) (int, error) { n, err := c.Conn.Read(p); return c.result(n, err) }
func (c *transferConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	return c.result(n, err)
}
