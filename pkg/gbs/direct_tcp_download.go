package gbs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	directTCPStatusConnecting = "connecting"
	directTCPStatusReceiving  = "receiving"
	directTCPStatusCompleted  = "completed"
	directTCPStatusFailed     = "failed"
	directTCPStatusCancelled  = "cancelled"

	directTCPMaxTerminalStates = 4096
)

// DirectTCPDownloadOptions 是 2014 附录 O 下载管理器的资源与安全限制。
type DirectTCPDownloadOptions struct {
	StorageDir           string
	RetainDays           int
	MaxFileSize          int64
	GlobalConcurrency    int
	DeviceConcurrency    int
	DialTimeout          time.Duration
	FirstByteTimeout     time.Duration
	IdleTimeout          time.Duration
	TotalTimeout         time.Duration
	AllowAddressMismatch bool
	AllowedAddressCIDRs  []string
	// AllowUnsafeAddresses 仅供本地模拟器测试使用，生产配置不暴露该开关。
	AllowUnsafeAddresses bool
}

// DirectTCPDownloadRequest 描述一个由设备作为 TCP 服务端的文件下载任务。
type DirectTCPDownloadRequest struct {
	SessionID     string
	DeviceID      string
	ChannelID     string
	Address       string
	RegisteredIP  net.IP
	FileSize      int64
	FileSizeKnown bool
	OnFinish      func(DirectTCPDownloadState)
}

// DirectTCPDownloadState 是可通过管理接口查询的下载快照。
type DirectTCPDownloadState struct {
	SessionID     string    `json:"session_id"`
	DeviceID      string    `json:"device_id"`
	ChannelID     string    `json:"channel_id"`
	Status        string    `json:"status"`
	Received      int64     `json:"received"`
	FileSize      int64     `json:"file_size"`
	FileSizeKnown bool      `json:"file_size_known"`
	SizeVerified  bool      `json:"size_verified"`
	Output        string    `json:"output,omitempty"`
	SHA256        string    `json:"sha256,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	CompletedAt   time.Time `json:"completed_at,omitempty"`
	EndReason     string    `json:"end_reason,omitempty"`
	Error         string    `json:"error,omitempty"`
}

type directTCPDownloadSession struct {
	request DirectTCPDownloadRequest
	opts    DirectTCPDownloadOptions
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}

	mu             sync.Mutex
	conn           net.Conn
	senderFinished bool
	tempPath       string
}

func (s *directTCPDownloadSession) setConn(conn net.Conn) {
	s.mu.Lock()
	s.conn = conn
	if s.senderFinished {
		_ = conn.Close()
	}
	s.mu.Unlock()
}

func (s *directTCPDownloadSession) closeConn() {
	s.mu.Lock()
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.mu.Unlock()
}

func (s *directTCPDownloadSession) signalSenderFinished() {
	s.mu.Lock()
	s.senderFinished = true
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.mu.Unlock()
}

func (s *directTCPDownloadSession) requestCancel() {
	s.mu.Lock()
	s.cancel()
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.mu.Unlock()
}

func (s *directTCPDownloadSession) wasSenderFinished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.senderFinished
}

func (s *directTCPDownloadSession) setTempPath(path string) {
	s.mu.Lock()
	s.tempPath = path
	s.mu.Unlock()
}

func (s *directTCPDownloadSession) tempPathSnapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tempPath
}

// DirectTCPDownloadManager 管理裸 TCP 文件接收、进度、取消和资源限制。
type DirectTCPDownloadManager struct {
	mu          sync.RWMutex
	opts        DirectTCPDownloadOptions
	states      map[string]DirectTCPDownloadState
	active      map[string]*directTCPDownloadSession
	deviceCount map[string]int
	activeCount int
	pendingOpts *DirectTCPDownloadOptions
}

func NewDirectTCPDownloadManager(opts DirectTCPDownloadOptions) *DirectTCPDownloadManager {
	opts = normalizeDirectTCPDownloadOptions(opts)
	return &DirectTCPDownloadManager{
		opts:        opts,
		states:      make(map[string]DirectTCPDownloadState),
		active:      make(map[string]*directTCPDownloadSession),
		deviceCount: make(map[string]int),
	}
}

func normalizeDirectTCPDownloadOptions(opts DirectTCPDownloadOptions) DirectTCPDownloadOptions {
	opts.AllowedAddressCIDRs = append([]string(nil), opts.AllowedAddressCIDRs...)
	if strings.TrimSpace(opts.StorageDir) == "" {
		opts.StorageDir = "./configs/downloads/gb28181"
	}
	if opts.RetainDays <= 0 {
		opts.RetainDays = 7
	}
	if opts.MaxFileSize <= 0 {
		opts.MaxFileSize = 10 << 30
	}
	if opts.GlobalConcurrency <= 0 {
		opts.GlobalConcurrency = 4
	}
	if opts.DeviceConcurrency <= 0 {
		opts.DeviceConcurrency = 1
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 5 * time.Second
	}
	if opts.FirstByteTimeout <= 0 {
		opts.FirstByteTimeout = 15 * time.Second
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 30 * time.Second
	}
	if opts.TotalTimeout <= 0 {
		opts.TotalTimeout = 2 * time.Hour
	}
	return opts
}

// Reconfigure 热更新后续下载使用的限制；活动任务继续使用启动时快照。
func (m *DirectTCPDownloadManager) Reconfigure(opts DirectTCPDownloadOptions) {
	if m == nil {
		return
	}
	opts = normalizeDirectTCPDownloadOptions(opts)
	m.mu.Lock()
	if m.activeCount > 0 {
		m.pendingOpts = &opts
	} else {
		m.opts = opts
		m.pendingOpts = nil
	}
	m.mu.Unlock()
}

// Start 启动异步下载。任务建立后可通过 State/Wait 查询终态。
func (m *DirectTCPDownloadManager) Start(parent context.Context, req DirectTCPDownloadRequest) error {
	if m == nil {
		return errors.New("direct TCP download manager is unavailable")
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.DeviceID = strings.TrimSpace(req.DeviceID)
	req.ChannelID = strings.TrimSpace(req.ChannelID)
	req.Address = strings.TrimSpace(req.Address)
	if req.SessionID == "" || req.DeviceID == "" || req.ChannelID == "" {
		return errors.New("direct TCP download requires session, device and channel IDs")
	}
	if req.FileSizeKnown && req.FileSize < 0 {
		return errors.New("direct TCP download file size must not be negative")
	}
	m.mu.RLock()
	opts := m.opts
	m.mu.RUnlock()
	if req.FileSizeKnown && req.FileSize > opts.MaxFileSize {
		return fmt.Errorf("direct TCP download file size %d exceeds limit %d", req.FileSize, opts.MaxFileSize)
	}
	if err := validateDirectTCPAddress(req.Address, req.RegisteredIP, opts); err != nil {
		return err
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, opts.TotalTimeout)
	now := time.Now()
	session := &directTCPDownloadSession{request: req, opts: opts, ctx: ctx, cancel: cancel, done: make(chan struct{})}

	m.mu.Lock()
	if _, exists := m.active[req.SessionID]; exists {
		m.mu.Unlock()
		cancel()
		return fmt.Errorf("direct TCP download session already active: %s", req.SessionID)
	}
	if m.activeCount >= opts.GlobalConcurrency {
		m.mu.Unlock()
		cancel()
		return fmt.Errorf("direct TCP download global concurrency limit reached: %d", opts.GlobalConcurrency)
	}
	if m.deviceCount[req.DeviceID] >= opts.DeviceConcurrency {
		m.mu.Unlock()
		cancel()
		return fmt.Errorf("direct TCP download device concurrency limit reached: %d", opts.DeviceConcurrency)
	}
	m.active[req.SessionID] = session
	m.activeCount++
	m.deviceCount[req.DeviceID]++
	m.states[req.SessionID] = DirectTCPDownloadState{
		SessionID:     req.SessionID,
		DeviceID:      req.DeviceID,
		ChannelID:     req.ChannelID,
		Status:        directTCPStatusConnecting,
		FileSize:      req.FileSize,
		FileSizeKnown: req.FileSizeKnown,
		StartedAt:     now,
		UpdatedAt:     now,
	}
	m.mu.Unlock()

	go m.run(session)
	return nil
}

// State 返回下载状态快照。
func (m *DirectTCPDownloadManager) State(sessionID string) (DirectTCPDownloadState, bool) {
	if m == nil {
		return DirectTCPDownloadState{}, false
	}
	m.mu.RLock()
	state, ok := m.states[strings.TrimSpace(sessionID)]
	m.mu.RUnlock()
	return state, ok
}

// FindByChannel 返回通道最近启动的下载状态。
func (m *DirectTCPDownloadManager) FindByChannel(deviceID, channelID string) (DirectTCPDownloadState, bool) {
	if m == nil {
		return DirectTCPDownloadState{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var latest DirectTCPDownloadState
	found := false
	for _, state := range m.states {
		if state.DeviceID != deviceID || state.ChannelID != channelID {
			continue
		}
		if !found || state.StartedAt.After(latest.StartedAt) {
			latest = state
			found = true
		}
	}
	return latest, found
}

// Wait 等待下载终态，主要供自动化验证和内部编排使用。
func (m *DirectTCPDownloadManager) Wait(ctx context.Context, sessionID string) (DirectTCPDownloadState, error) {
	if m == nil {
		return DirectTCPDownloadState{}, errors.New("direct TCP download manager is unavailable")
	}
	m.mu.RLock()
	session, active := m.active[strings.TrimSpace(sessionID)]
	state, exists := m.states[strings.TrimSpace(sessionID)]
	m.mu.RUnlock()
	if !exists {
		return DirectTCPDownloadState{}, fmt.Errorf("direct TCP download session not found: %s", sessionID)
	}
	if !active {
		return state, nil
	}
	select {
	case <-session.done:
		state, _ = m.State(sessionID)
		return state, nil
	case <-ctx.Done():
		return DirectTCPDownloadState{}, ctx.Err()
	}
}

// Cancel 取消活动下载并关闭连接。
func (m *DirectTCPDownloadManager) Cancel(sessionID string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	session, ok := m.active[strings.TrimSpace(sessionID)]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	session.requestCancel()
	return true
}

// CancelAll 用于关闭功能或回滚时收敛全部活动任务。
func (m *DirectTCPDownloadManager) CancelAll() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	sessions := make([]*directTCPDownloadSession, 0, len(m.active))
	for _, session := range m.active {
		sessions = append(sessions, session)
	}
	m.mu.RUnlock()
	for _, session := range sessions {
		session.requestCancel()
	}
	return len(sessions)
}

// CancelDevice 取消指定设备的全部活动下载，其他设备不受影响。
func (m *DirectTCPDownloadManager) CancelDevice(deviceID string) int {
	if m == nil {
		return 0
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return 0
	}
	m.mu.RLock()
	sessions := make([]*directTCPDownloadSession, 0, m.deviceCount[deviceID])
	for _, session := range m.active {
		if session != nil && session.request.DeviceID == deviceID {
			sessions = append(sessions, session)
		}
	}
	m.mu.RUnlock()
	for _, session := range sessions {
		session.requestCancel()
	}
	return len(sessions)
}

// NotifySenderFinished 处理 MediaStatus/121；未知大小时以该通知收敛传输。
func (m *DirectTCPDownloadManager) NotifySenderFinished(sessionID string) bool {
	return m.NotifySenderFinishedForDevice(sessionID, "")
}

// NotifySenderFinishedForDevice 仅允许会话所属设备通过 MediaStatus/121 收敛传输。
// deviceID 为空保留内部旧调用兼容；协议入口必须传入已鉴权的设备编号。
func (m *DirectTCPDownloadManager) NotifySenderFinishedForDevice(sessionID, deviceID string) bool {
	if m == nil {
		return false
	}
	deviceID = strings.TrimSpace(deviceID)
	m.mu.RLock()
	session, ok := m.active[strings.TrimSpace(sessionID)]
	m.mu.RUnlock()
	if !ok || deviceID != "" && session.request.DeviceID != deviceID {
		return false
	}
	session.signalSenderFinished()
	return true
}

func (m *DirectTCPDownloadManager) run(session *directTCPDownloadSession) {
	req := session.request
	opts := session.opts
	dialer := net.Dialer{Timeout: opts.DialTimeout}
	conn, err := dialer.DialContext(session.ctx, "tcp", req.Address)
	if err != nil {
		status, reason := directTCPStatusFailed, "connect_failed"
		if errors.Is(session.ctx.Err(), context.Canceled) {
			status, reason = directTCPStatusCancelled, "cancelled"
		} else if errors.Is(session.ctx.Err(), context.DeadlineExceeded) {
			reason = "total_timeout"
		}
		m.finish(session, status, reason, "", "", false, err)
		return
	}
	session.setConn(conn)
	defer conn.Close()

	watchDone := make(chan struct{})
	go func() {
		select {
		case <-session.ctx.Done():
			session.closeConn()
		case <-watchDone:
		}
	}()
	defer close(watchDone)

	root, err := filepath.Abs(opts.StorageDir)
	if err != nil {
		m.finish(session, directTCPStatusFailed, "invalid_storage", "", "", false, err)
		return
	}
	if err := ensureDirectTCPStorageRoot(root); err != nil {
		m.finish(session, directTCPStatusFailed, "create_storage_failed", "", "", false, err)
		return
	}
	name := directTCPDownloadFilename(req)
	finalPath := filepath.Join(root, name)
	if !pathWithinRoot(root, finalPath) {
		m.finish(session, directTCPStatusFailed, "invalid_output_path", "", "", false, errors.New("download output escapes storage root"))
		return
	}
	tmp, err := os.CreateTemp(root, "."+name+"-*.part")
	if err != nil {
		m.finish(session, directTCPStatusFailed, "create_file_failed", "", "", false, err)
		return
	}
	tmpPath := tmp.Name()
	session.setTempPath(tmpPath)
	keep := false
	defer func() {
		session.setTempPath("")
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		m.finish(session, directTCPStatusFailed, "secure_file_failed", "", "", false, err)
		return
	}

	m.updateProgress(req.SessionID, directTCPStatusReceiving, 0)
	received, digest, verified, reason, err := m.receive(session, conn, tmp, sha256.New())
	if err != nil {
		status := directTCPStatusFailed
		if errors.Is(session.ctx.Err(), context.Canceled) {
			status = directTCPStatusCancelled
			reason = "cancelled"
		} else if errors.Is(session.ctx.Err(), context.DeadlineExceeded) {
			reason = "total_timeout"
		}
		m.updateProgress(req.SessionID, status, received)
		m.finish(session, status, reason, "", "", false, err)
		return
	}
	if err := tmp.Sync(); err != nil {
		m.finish(session, directTCPStatusFailed, "sync_file_failed", "", "", false, err)
		return
	}
	if err := tmp.Close(); err != nil {
		m.finish(session, directTCPStatusFailed, "close_file_failed", "", "", false, err)
		return
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		m.finish(session, directTCPStatusFailed, "commit_file_failed", "", "", false, err)
		return
	}
	keep = true
	m.updateProgress(req.SessionID, directTCPStatusCompleted, received)
	m.finish(session, directTCPStatusCompleted, reason, name, digest, verified, nil)
}

func (m *DirectTCPDownloadManager) receive(session *directTCPDownloadSession, conn net.Conn, dst io.Writer, digest hash.Hash) (int64, string, bool, string, error) {
	if session.request.FileSizeKnown && session.request.FileSize == 0 {
		return 0, hex.EncodeToString(digest.Sum(nil)), true, "size_reached", nil
	}
	buffer := make([]byte, 64*1024)
	var received int64
	for {
		readTimeout := session.opts.IdleTimeout
		if received == 0 {
			readTimeout = session.opts.FirstByteTimeout
		}
		if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			return received, "", false, "set_deadline_failed", err
		}
		n, err := conn.Read(buffer)
		if n > 0 {
			next := received + int64(n)
			if next > session.opts.MaxFileSize {
				return received, "", false, "size_limit_exceeded", fmt.Errorf("received file exceeds limit %d", session.opts.MaxFileSize)
			}
			if session.request.FileSizeKnown && next > session.request.FileSize {
				return received, "", false, "declared_size_exceeded", fmt.Errorf("received %d bytes exceeds declared size %d", next, session.request.FileSize)
			}
			if _, writeErr := dst.Write(buffer[:n]); writeErr != nil {
				return received, "", false, "write_failed", writeErr
			}
			_, _ = digest.Write(buffer[:n])
			received = next
			m.updateProgress(session.request.SessionID, directTCPStatusReceiving, received)
			if session.request.FileSizeKnown && received == session.request.FileSize {
				return received, hex.EncodeToString(digest.Sum(nil)), true, "size_reached", nil
			}
		}
		if err == nil {
			continue
		}
		if session.ctx.Err() != nil {
			return received, "", false, "context_done", session.ctx.Err()
		}
		if session.wasSenderFinished() {
			if session.request.FileSizeKnown && received != session.request.FileSize {
				return received, "", false, "early_finish", fmt.Errorf("sender finished at %d of %d bytes", received, session.request.FileSize)
			}
			return received, hex.EncodeToString(digest.Sum(nil)), session.request.FileSizeKnown, "media_status", nil
		}
		if errors.Is(err, io.EOF) {
			if session.request.FileSizeKnown && received != session.request.FileSize {
				return received, "", false, "early_eof", fmt.Errorf("unexpected EOF at %d of %d bytes", received, session.request.FileSize)
			}
			return received, hex.EncodeToString(digest.Sum(nil)), session.request.FileSizeKnown, "eof", nil
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			reason := "idle_timeout"
			if received == 0 {
				reason = "first_byte_timeout"
			}
			return received, "", false, reason, err
		}
		return received, "", false, "read_failed", err
	}
}

func (m *DirectTCPDownloadManager) updateProgress(sessionID, status string, received int64) {
	m.mu.Lock()
	state, ok := m.states[sessionID]
	if ok {
		state.Status = status
		state.Received = received
		state.UpdatedAt = time.Now()
		m.states[sessionID] = state
	}
	m.mu.Unlock()
}

func (m *DirectTCPDownloadManager) finish(session *directTCPDownloadSession, status, reason, output, digest string, verified bool, err error) {
	now := time.Now()
	m.mu.Lock()
	state := m.states[session.request.SessionID]
	state.Status = status
	state.UpdatedAt = now
	state.CompletedAt = now
	state.EndReason = reason
	state.Output = output
	state.SHA256 = digest
	state.SizeVerified = verified
	if err != nil {
		state.Error = err.Error()
	}
	m.states[session.request.SessionID] = state
	if active, ok := m.active[session.request.SessionID]; ok && active == session {
		delete(m.active, session.request.SessionID)
		m.activeCount--
		m.deviceCount[session.request.DeviceID]--
		if m.deviceCount[session.request.DeviceID] <= 0 {
			delete(m.deviceCount, session.request.DeviceID)
		}
		if m.activeCount == 0 && m.pendingOpts != nil {
			m.opts = *m.pendingOpts
			m.pendingOpts = nil
		}
	}
	m.cleanupTerminalStatesLocked(now, m.opts)
	m.mu.Unlock()
	session.cancel()
	close(session.done)
	if session.request.OnFinish != nil {
		session.request.OnFinish(state)
	}
}

type directTCPTerminalEntry struct {
	sessionID string
	completed time.Time
	started   time.Time
}

func (m *DirectTCPDownloadManager) cleanupTerminalStatesLocked(now time.Time, opts DirectTCPDownloadOptions) {
	cutoff := now.Add(-time.Duration(opts.RetainDays) * 24 * time.Hour)
	terminals := make([]directTCPTerminalEntry, 0, len(m.states))
	for sessionID, state := range m.states {
		if _, active := m.active[sessionID]; active || state.CompletedAt.IsZero() {
			continue
		}
		if !state.CompletedAt.After(cutoff) {
			delete(m.states, sessionID)
			continue
		}
		terminals = append(terminals, directTCPTerminalEntry{
			sessionID: sessionID, completed: state.CompletedAt, started: state.StartedAt,
		})
	}
	if len(terminals) <= directTCPMaxTerminalStates {
		return
	}
	sort.Slice(terminals, func(i, j int) bool {
		if terminals[i].completed.Equal(terminals[j].completed) {
			if terminals[i].started.Equal(terminals[j].started) {
				return terminals[i].sessionID < terminals[j].sessionID
			}
			return terminals[i].started.Before(terminals[j].started)
		}
		return terminals[i].completed.Before(terminals[j].completed)
	})
	for _, entry := range terminals[:len(terminals)-directTCPMaxTerminalStates] {
		delete(m.states, entry.sessionID)
	}
}

// Cleanup 回收下载终态和管理器生成的过期文件；活动任务不受影响。
func (m *DirectTCPDownloadManager) Cleanup(now time.Time) {
	if m == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	m.mu.Lock()
	opts := m.opts
	m.cleanupTerminalStatesLocked(now, opts)
	active := make([]*directTCPDownloadSession, 0, len(m.active))
	for _, session := range m.active {
		active = append(active, session)
	}
	m.mu.Unlock()
	protected := make(map[string]struct{}, len(active))
	for _, session := range active {
		if path := session.tempPathSnapshot(); path != "" {
			protected[path] = struct{}{}
		}
	}
	m.cleanupExpiredFiles(now, opts, protected)
}

func validateDirectTCPAddress(address string, registeredIP net.IP, opts DirectTCPDownloadOptions) error {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return fmt.Errorf("invalid direct TCP download address: %w", err)
	}
	port, err := net.LookupPort("tcp", portText)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("invalid direct TCP download port: %s", portText)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("direct TCP download requires an IPv4 address: %s", host)
	}
	if !opts.AllowUnsafeAddresses && (ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast()) {
		return fmt.Errorf("unsafe direct TCP download address is forbidden: %s", ip)
	}
	registered := registeredIP.To4()
	if registered != nil && ip.Equal(registered) {
		return nil
	}
	if !opts.AllowAddressMismatch {
		return fmt.Errorf("direct TCP download address %s differs from registered device address %s", ip, registeredIP)
	}
	if !ipAllowedByCIDRs(ip, opts.AllowedAddressCIDRs) {
		return fmt.Errorf("direct TCP download address %s is not in allowed address CIDRs", ip)
	}
	return nil
}

func ipAllowedByCIDRs(ip net.IP, cidrs []string) bool {
	for _, value := range cidrs {
		_, network, err := net.ParseCIDR(strings.TrimSpace(value))
		if err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func ensureDirectTCPStorageRoot(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("direct TCP storage root must be a real directory: %s", root)
	}
	return os.Chmod(root, 0o700)
}

func directTCPDownloadFilename(req DirectTCPDownloadRequest) string {
	sum := sha256.Sum256([]byte(req.SessionID))
	deviceID := safeFilenamePart(req.DeviceID)
	channelID := safeFilenamePart(req.ChannelID)
	return fmt.Sprintf("%s-%s-%d-%x.ps", deviceID, channelID, time.Now().UnixNano(), sum[:6])
}

func safeFilenamePart(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func pathWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (m *DirectTCPDownloadManager) cleanupExpiredFiles(now time.Time, opts DirectTCPDownloadOptions, protected map[string]struct{}) {
	root, err := filepath.Abs(opts.StorageDir)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := now.Add(-time.Duration(opts.RetainDays) * 24 * time.Hour)
	for _, entry := range entries {
		name := entry.Name()
		managedFile := strings.HasSuffix(name, ".ps") || strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".part")
		if entry.IsDir() || !managedFile {
			continue
		}
		path := filepath.Join(root, name)
		if _, active := protected[path]; active {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
	}
}
