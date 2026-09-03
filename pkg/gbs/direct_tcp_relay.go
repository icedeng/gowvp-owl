package gbs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

func (g *GB28181API) startCascadeDirectTCPRelay(session *cascadeMediaSession) {
	if g == nil || session == nil {
		return
	}
	relay := session.directRelaySnapshot()
	if relay == nil {
		return
	}
	relay.Start(func(result directTCPRelayResult) {
		if result.Err != nil && !errors.Is(result.Err, context.Canceled) {
			slog.WarnContext(g.mediaPersistenceContext(), "cascade direct TCP relay ended",
				"device_id", session.sourceDeviceID(), "channel_id", session.sourceChannelID(),
				"received", result.Received, "reason", result.Reason, "err", result.Err)
		}
		source := session.sourceSnapshot()
		if g.cascadeSourceMediaStatusFinished(source) {
			return
		}
		g.stopCascadeMediaSession(session, true, true)
	})
}

func (g *GB28181API) notifyCascadeDirectTCPRelaySenderFinished(stream *Streams) {
	if g == nil || stream == nil {
		return
	}
	g.inviteDialogs.Range(func(_, value any) bool {
		dialog, _ := value.(*inboundInviteDialog)
		if dialog == nil || dialog.Cascade == nil || dialog.Cascade.directRelaySnapshot() == nil ||
			!cascadeSourceReferencesStream(dialog.Cascade.sourceSnapshot(), stream) {
			return true
		}
		dialog.Cascade.directRelaySnapshot().NotifySenderFinished()
		return true
	})
}

func (g *GB28181API) cancelCascadeDirectTCPRelays() {
	if g == nil {
		return
	}
	g.inviteDialogs.Range(func(_, value any) bool {
		dialog, _ := value.(*inboundInviteDialog)
		if dialog != nil && dialog.Cascade != nil && dialog.Cascade.directRelaySnapshot() != nil {
			g.stopCascadeMediaSession(dialog.Cascade, true, false)
		}
		return true
	})
}

// directTCPRelayResult 描述一次附录 O 级联数据中继的终态。
// 裸 TCP 数据不落盘，因此这里只记录用于收敛和审计的字节数与原因。
type directTCPRelayResult struct {
	Received int64
	Reason   string
	Err      error
}

// directTCPRelay 在下级设备 TCP 服务端与上级平台 TCP 客户端之间流式转发原始 PS。
// 每条中继只允许一个已注册上级连接，且与设备直连下载共享并发额度。
type directTCPRelay struct {
	mu sync.Mutex

	ctx      context.Context
	cancel   context.CancelFunc
	listener *net.TCPListener

	deviceAddress      string
	deviceRegisteredIP net.IP
	upstreamAllowed    func(net.IP) bool
	opts               DirectTCPDownloadOptions
	fileSize           int64
	fileSizeKnown      bool

	sourceConn   net.Conn
	upstreamConn net.Conn
	release      func()
	onFinish     func(directTCPRelayResult)

	started        bool
	closed         bool
	finished       bool
	senderFinished bool
	result         directTCPRelayResult
	finishOnce     sync.Once
	done           chan struct{}
}

func (g *GB28181API) prepareCascadeDirectTCPRelay(
	ctx context.Context,
	policy directTCPRuntimePolicy,
	platform cascadePlatform,
	deviceID string,
	registeredIP net.IP,
	offer directTCPDownloadOffer,
) (*directTCPRelay, string, int, error) {
	if g == nil || g.directDownloads == nil {
		return nil, "", 0, fmt.Errorf("direct TCP relay resource manager is unavailable")
	}
	if !policy.CascadeRelayEnabled {
		return nil, "", 0, fmt.Errorf("2014 上级平台裸 TCP 下载中继未启用")
	}
	opts, release, err := g.directDownloads.reserveRelay(deviceID)
	if err != nil {
		return nil, "", 0, err
	}
	failed := true
	defer func() {
		if failed {
			release()
		}
	}()
	if offer.FileSizeKnown {
		if offer.FileSize < 0 {
			return nil, "", 0, fmt.Errorf("direct TCP relay file size must not be negative")
		}
		if offer.FileSize > opts.MaxFileSize {
			return nil, "", 0, fmt.Errorf("direct TCP relay file size %d exceeds limit %d", offer.FileSize, opts.MaxFileSize)
		}
	}
	if err := validateDirectTCPAddress(offer.Address, registeredIP, opts); err != nil {
		return nil, "", 0, err
	}
	listenIP := net.ParseIP(strings.TrimSpace(policy.RelayListenIP))
	if listenIP == nil || listenIP.IsMulticast() {
		return nil, "", 0, fmt.Errorf("invalid direct TCP relay listen IP")
	}
	advertiseIP := strings.TrimSpace(policy.RelayAdvertiseIP)
	if advertiseIP == "" && g.boot != nil {
		advertiseIP = strings.TrimSpace(g.boot.Media.SDPIP)
	}
	advertise, err := GetIP(advertiseIP)
	if err != nil {
		return nil, "", 0, fmt.Errorf("resolve direct TCP relay advertise IP: %w", err)
	}
	parsedAdvertise := net.ParseIP(advertise)
	if parsedAdvertise == nil || parsedAdvertise.IsUnspecified() || parsedAdvertise.IsMulticast() {
		return nil, "", 0, fmt.Errorf("invalid direct TCP relay advertise IP")
	}
	listener, err := listenDirectTCPRelay(listenIP, policy.RelayPortStart, policy.RelayPortEnd)
	if err != nil {
		return nil, "", 0, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	relayCtx, cancel := context.WithTimeout(ctx, opts.TotalTimeout)
	relay := &directTCPRelay{
		ctx: relayCtx, cancel: cancel, listener: listener,
		deviceAddress: offer.Address, deviceRegisteredIP: append(net.IP(nil), registeredIP...),
		upstreamAllowed: platform.allowsMediaDestination, opts: opts,
		fileSize: offer.FileSize, fileSizeKnown: offer.FileSizeKnown,
		release: release, done: make(chan struct{}),
	}
	go relay.finishOnContextDone()
	failed = false
	return relay, parsedAdvertise.String(), listener.Addr().(*net.TCPAddr).Port, nil
}

func listenDirectTCPRelay(ip net.IP, start, end int) (*net.TCPListener, error) {
	if ip == nil || start < 1 || end < start || end > 65535 {
		return nil, fmt.Errorf("invalid direct TCP relay listen range")
	}
	var lastErr error
	for port := start; port <= end; port++ {
		listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: ip, Port: port})
		if err == nil {
			return listener, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("direct TCP relay has no available port in %d-%d: %w", start, end, lastErr)
}

// Start 在上级 ACK 到达后启动中继。重复 ACK 不会重复建立 TCP 连接。
func (r *directTCPRelay) Start(onFinish func(directTCPRelayResult)) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return false
	}
	r.started = true
	if r.finished {
		result := r.result
		r.mu.Unlock()
		if onFinish != nil {
			go onFinish(result)
		}
		return false
	}
	if r.closed {
		r.mu.Unlock()
		return false
	}
	r.onFinish = onFinish
	r.mu.Unlock()
	go r.run()
	return true
}

// finishOnContextDone 只把外部取消或总超时提交为中继终态。显式 Close 已经由
// 会话状态机负责收敛，不能再从这里反向触发一次结束回调。
func (r *directTCPRelay) finishOnContextDone() {
	if r == nil || r.ctx == nil {
		return
	}
	<-r.ctx.Done()
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return
	}
	reason, err := directTCPRelayReason(r.ctx, "context_done", r.ctx.Err())
	r.finish(directTCPRelayResult{Reason: reason, Err: err})
}

func (r *directTCPRelay) run() {
	result := directTCPRelayResult{Reason: "completed"}
	upstream, err := r.acceptUpstream()
	if err != nil {
		result.Reason, result.Err = directTCPRelayReason(r.ctx, "accept_failed", err)
		r.finish(result)
		return
	}
	r.setUpstreamConn(upstream)

	dialer := net.Dialer{Timeout: r.opts.DialTimeout}
	source, err := dialer.DialContext(r.ctx, "tcp", r.deviceAddress)
	if err != nil {
		result.Reason, result.Err = directTCPRelayReason(r.ctx, "connect_device_failed", err)
		r.finish(result)
		return
	}
	r.setSourceConn(source)
	result.Received, result.Reason, result.Err = r.copy(source, upstream)
	r.finish(result)
}

func (r *directTCPRelay) acceptUpstream() (net.Conn, error) {
	if r == nil || r.listener == nil {
		return nil, fmt.Errorf("direct TCP relay listener is unavailable")
	}
	deadline := time.Now().Add(r.opts.FirstByteTimeout)
	if ctxDeadline, ok := r.ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := r.listener.SetDeadline(deadline); err != nil {
		return nil, err
	}
	for {
		conn, err := r.listener.AcceptTCP()
		if err != nil {
			return nil, err
		}
		remoteIP := addressIP(conn.RemoteAddr())
		if remoteIP != nil && r.upstreamAllowed != nil && r.upstreamAllowed(remoteIP) {
			_ = conn.SetDeadline(time.Time{})
			return conn, nil
		}
		_ = conn.Close()
		if err := r.ctx.Err(); err != nil {
			return nil, err
		}
	}
}

func (r *directTCPRelay) copy(source, destination net.Conn) (int64, string, error) {
	buffer := make([]byte, 64*1024)
	var received int64
	for {
		readTimeout := r.opts.IdleTimeout
		if received == 0 {
			readTimeout = r.opts.FirstByteTimeout
		}
		if err := source.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			return received, "set_source_deadline_failed", err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			next := received + int64(n)
			if next > r.opts.MaxFileSize {
				return received, "size_limit_exceeded", fmt.Errorf("relayed file exceeds limit %d", r.opts.MaxFileSize)
			}
			if r.fileSizeKnown && next > r.fileSize {
				return received, "declared_size_exceeded", fmt.Errorf("relayed %d bytes exceeds declared size %d", next, r.fileSize)
			}
			if err := destination.SetWriteDeadline(time.Now().Add(r.opts.IdleTimeout)); err != nil {
				return received, "set_destination_deadline_failed", err
			}
			written := 0
			for written < n {
				count, err := destination.Write(buffer[written:n])
				if err != nil {
					return received, "write_upstream_failed", err
				}
				if count <= 0 {
					return received, "write_upstream_failed", io.ErrShortWrite
				}
				written += count
			}
			received = next
		}
		if readErr == nil {
			continue
		}
		if err := r.ctx.Err(); err != nil {
			reason, cause := directTCPRelayReason(r.ctx, "context_done", err)
			return received, reason, cause
		}
		if r.senderFinishedSnapshot() {
			if r.fileSizeKnown && received != r.fileSize {
				return received, "early_finish", fmt.Errorf("sender finished at %d of %d bytes", received, r.fileSize)
			}
			return received, "media_status", nil
		}
		if errors.Is(readErr, io.EOF) {
			if r.fileSizeKnown && received != r.fileSize {
				return received, "early_eof", fmt.Errorf("unexpected EOF at %d of %d bytes", received, r.fileSize)
			}
			return received, "eof", nil
		}
		var netErr net.Error
		if errors.As(readErr, &netErr) && netErr.Timeout() {
			if r.fileSizeKnown && received == r.fileSize {
				return received, "size_reached", nil
			}
			reason := "idle_timeout"
			if received == 0 {
				reason = "first_byte_timeout"
			}
			return received, reason, readErr
		}
		return received, "read_device_failed", readErr
	}
}

func directTCPRelayReason(ctx context.Context, fallback string, err error) (string, error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return "cancelled", context.Canceled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "total_timeout", context.DeadlineExceeded
	}
	return fallback, err
}

func (r *directTCPRelay) setSourceConn(conn net.Conn) {
	r.mu.Lock()
	if r.closed {
		_ = conn.Close()
	} else {
		r.sourceConn = conn
		if r.senderFinished {
			_ = conn.Close()
		}
	}
	r.mu.Unlock()
}

func (r *directTCPRelay) setUpstreamConn(conn net.Conn) {
	r.mu.Lock()
	if r.closed {
		_ = conn.Close()
	} else {
		r.upstreamConn = conn
	}
	r.mu.Unlock()
}

func (r *directTCPRelay) senderFinishedSnapshot() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.senderFinished
}

// NotifySenderFinished 处理下级 MediaStatus/121，并打断可能阻塞的设备读取。
func (r *directTCPRelay) NotifySenderFinished() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.senderFinished = true
	if r.sourceConn != nil {
		_ = r.sourceConn.Close()
	}
	r.mu.Unlock()
}

func (r *directTCPRelay) finish(result directTCPRelayResult) {
	r.finishOnce.Do(func() {
		r.mu.Lock()
		r.finished = true
		r.result = result
		onFinish := r.onFinish
		r.mu.Unlock()
		r.Close()
		if onFinish != nil {
			onFinish(result)
		}
		close(r.done)
	})
}

// Close 立即关闭监听和双侧连接并释放共享并发额度；可重复调用。
func (r *directTCPRelay) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	if r.cancel != nil {
		r.cancel()
	}
	listener := r.listener
	source := r.sourceConn
	upstream := r.upstreamConn
	release := r.release
	r.release = nil
	r.mu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
	if source != nil {
		_ = source.Close()
	}
	if upstream != nil {
		_ = upstream.Close()
	}
	if release != nil {
		release()
	}
}
