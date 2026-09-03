package gbs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	defaultLegacyRegisterExpires    = 3600
	defaultCascadeKeepaliveInterval = 60 * time.Second
	defaultCascadeRequestTimeout    = 8 * time.Second
	defaultCascadeCancelTimeout     = time.Second
	// RFC 3261 18.1.1 在路径 MTU 未知时要求大于 1300 字节的请求改用拥塞控制传输。
	cascadeReliableTransportThreshold = 1300
	maxCascadeRegisterRedirects       = 3
	maxCascadeRegisterMinRetries      = 1
)

var errCascadeWorkerStopping = errors.New("cascade worker is stopping")

// CascadePlatformStatus 是本平台向上级平台注册的运行状态快照。
type CascadePlatformStatus struct {
	Name              string    `json:"name"`
	ServerID          string    `json:"server_id"`
	Address           string    `json:"address"`
	ConfiguredVersion string    `json:"configured_version"`
	NegotiatedVersion string    `json:"negotiated_version,omitempty"`
	State             string    `json:"state"`
	Registered        bool      `json:"registered"`
	LastRegisterAt    time.Time `json:"last_register_at,omitempty"`
	LastKeepaliveAt   time.Time `json:"last_keepalive_at,omitempty"`
	ExpiresAt         time.Time `json:"expires_at,omitempty"`
	LastError         string    `json:"last_error,omitempty"`
}

type cascadePlatform struct {
	name                    string
	serverID                string
	remoteDomain            string
	remote                  net.Addr
	transport               string
	localID                 string
	localDomain             string
	localHost               string
	localPort               int
	localTLSPort            int
	localTLSEnabled         bool
	password                string
	signalDigestSeed        string
	version                 GBProtocolVersion
	expires                 int
	keepaliveInterval       time.Duration
	alarmDispatchEnabled    bool
	sharedChannels          []string
	channelIDMap            map[string]string
	exposedChannelMap       map[string]string
	mediaAllowedCIDRs       []*net.IPNet
	tlsConfig               *tls.Config
	registerCertificateAuth *cascadeRegisterCertificateAuthenticator
	monitorUserIdentity     *monitorUserIdentityPolicy
}

type cascadeTLSAddr struct {
	*net.TCPAddr
	serverName string
}

func (a *cascadeTLSAddr) Network() string { return "tls" }

func (p cascadePlatform) contactPort(transport string) int {
	if strings.EqualFold(strings.TrimSpace(transport), "tls") && p.localTLSPort > 0 {
		return p.localTLSPort
	}
	return p.localPort
}

func normalizeCascadePlatform(in conf.SIPUpstream, local conf.SIP, fallbackHost string) (cascadePlatform, error) {
	serverID := strings.TrimSpace(in.ServerID)
	if err := filterUnknowDevices(serverID); err != nil {
		return cascadePlatform{}, fmt.Errorf("upstream server_id: %w", err)
	}
	localID := strings.TrimSpace(in.LocalID)
	if localID == "" {
		localID = strings.TrimSpace(local.ID)
	}
	if err := filterUnknowDevices(localID); err != nil {
		return cascadePlatform{}, fmt.Errorf("upstream local_id: %w", err)
	}
	host := strings.TrimSpace(in.Host)
	if host == "" {
		return cascadePlatform{}, fmt.Errorf("upstream host is required")
	}
	transport := strings.ToLower(strings.TrimSpace(in.Transport))
	if transport == "" {
		transport = "udp"
	}
	port := in.Port
	if port == 0 {
		port = 5060
		if transport == "tls" {
			port = 5061
		}
	}
	if port < 1 || port > 65535 {
		return cascadePlatform{}, fmt.Errorf("upstream port must be between 1 and 65535")
	}
	var remote net.Addr
	tlsConfig, err := cascadeTLSClientConfig(in, host)
	if err != nil {
		return cascadePlatform{}, err
	}
	registerCertificateAuth, err := newCascadeRegisterCertificateAuthenticator(in.RegisterCertificateAuth)
	if err != nil {
		return cascadePlatform{}, fmt.Errorf("upstream certificate REGISTER authentication: %w", err)
	}
	monitorUserIdentity, err := newMonitorUserIdentityPolicy(in.MonitorUserIdentity)
	if err != nil {
		return cascadePlatform{}, fmt.Errorf("upstream Monitor-User-Identity: %w", err)
	}
	switch transport {
	case "udp":
		remote, err = net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port)))
	case "tcp":
		remote, err = net.ResolveTCPAddr("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	case "tls":
		var tcpAddr *net.TCPAddr
		tcpAddr, err = net.ResolveTCPAddr("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err == nil {
			remote = &cascadeTLSAddr{TCPAddr: tcpAddr, serverName: tlsConfig.ServerName}
		}
	default:
		return cascadePlatform{}, fmt.Errorf("upstream transport only supports udp/tcp/tls")
	}
	if err != nil {
		return cascadePlatform{}, fmt.Errorf("resolve upstream %s address: %w", transport, err)
	}
	remoteDomain := strings.TrimSpace(in.Domain)
	if remoteDomain == "" {
		remoteDomain = serverID[:10]
	}
	localDomain := strings.TrimSpace(in.LocalDomain)
	if localDomain == "" {
		localDomain = strings.TrimSpace(local.Domain)
	}
	if localDomain == "" {
		localDomain = localID[:10]
	}
	version := GBVersion10
	if strings.TrimSpace(in.Version) != "" {
		var ok bool
		version, ok = ParseGBProtocolVersion(in.Version)
		if !ok {
			return cascadePlatform{}, fmt.Errorf("upstream version only supports 1.0/1.1/2.0/3.0")
		}
	}
	expires := in.Expires
	if expires == 0 {
		expires = defaultLegacyRegisterExpires
		if version.AtLeast(GBVersion11) {
			expires = defaultRegisterExpires
		}
	}
	minimumExpires := 60
	if version.AtLeast(GBVersion11) {
		minimumExpires = minimumStandardRegisterTTL
	}
	if expires < minimumExpires || int64(expires) > maximumRegisterExpires {
		return cascadePlatform{}, fmt.Errorf("upstream expires must be between %d and %d seconds for %s", minimumExpires, maximumRegisterExpires, version.StandardName())
	}
	keepalive := in.KeepaliveInterval.Duration()
	if keepalive == 0 {
		keepalive = defaultCascadeKeepaliveInterval
	}
	if keepalive < 5*time.Second || keepalive > time.Hour {
		return cascadePlatform{}, fmt.Errorf("upstream keepalive_interval must be between 5s and 1h")
	}
	localHost := strings.TrimSpace(in.LocalHost)
	if localHost == "" {
		localHost = strings.TrimSpace(local.Host)
	}
	if localHost == "" {
		localHost = strings.TrimSpace(fallbackHost)
	}
	if localHost == "" {
		return cascadePlatform{}, fmt.Errorf("upstream local_host cannot be determined")
	}
	if local.Port < 1 || local.Port > 65535 {
		return cascadePlatform{}, fmt.Errorf("local SIP port must be between 1 and 65535")
	}
	localTLSPort := local.TLSPort
	if localTLSPort == 0 {
		localTLSPort = local.Port
	}
	if in.LocalPort != 0 {
		localTLSPort = in.LocalPort
		if transport != "tls" {
			local.Port = in.LocalPort
		}
	}
	if localTLSPort < 1 || localTLSPort > 65535 || local.Port < 1 || local.Port > 65535 {
		return cascadePlatform{}, fmt.Errorf("upstream local_port must be between 1 and 65535")
	}
	if transport == "tls" && !local.EnableTLS {
		return cascadePlatform{}, fmt.Errorf("upstream TLS transport requires the local SIP-TLS listener")
	}
	if _, err := sip.ParseSipURI(fmt.Sprintf("sip:%s@%s", serverID, remoteDomain)); err != nil {
		return cascadePlatform{}, fmt.Errorf("upstream domain: %w", err)
	}
	if _, err := sip.ParseSipURI(fmt.Sprintf("sip:%s@%s", localID, localDomain)); err != nil {
		return cascadePlatform{}, fmt.Errorf("upstream local_domain: %w", err)
	}
	if _, err := sip.ParseSipURI(fmt.Sprintf("sip:%s@%s", localID, net.JoinHostPort(localHost, strconv.Itoa(local.Port)))); err != nil {
		return cascadePlatform{}, fmt.Errorf("upstream local_host: %w", err)
	}
	sharedChannels := make([]string, 0, len(in.SharedChannels))
	sharedSet := make(map[string]struct{}, len(in.SharedChannels))
	for _, channelID := range in.SharedChannels {
		channelID = strings.TrimSpace(channelID)
		if err := filterUnknowDevices(channelID); err != nil {
			return cascadePlatform{}, fmt.Errorf("shared channel %q: %w", channelID, err)
		}
		if _, exists := sharedSet[channelID]; exists {
			return cascadePlatform{}, fmt.Errorf("duplicate shared channel: %s", channelID)
		}
		sharedSet[channelID] = struct{}{}
		sharedChannels = append(sharedChannels, channelID)
	}
	normalizedMap := make(map[string]string, len(in.ChannelIDMap))
	for sourceID, exposedID := range in.ChannelIDMap {
		sourceID = strings.TrimSpace(sourceID)
		if _, exists := sharedSet[sourceID]; !exists {
			return cascadePlatform{}, fmt.Errorf("channel_id_map source is not shared: %s", sourceID)
		}
		normalizedMap[sourceID] = strings.TrimSpace(exposedID)
	}
	channelIDMap := make(map[string]string, len(sharedChannels))
	exposedChannelMap := make(map[string]string, len(sharedChannels))
	exposedIDs := make(map[string]string, len(sharedChannels))
	for _, sourceID := range sharedChannels {
		exposedID := normalizedMap[sourceID]
		if exposedID == "" {
			exposedID = sourceID
		}
		if err := filterUnknowDevices(exposedID); err != nil {
			return cascadePlatform{}, fmt.Errorf("channel_id_map target %q: %w", exposedID, err)
		}
		if previous, exists := exposedIDs[exposedID]; exists && previous != sourceID {
			return cascadePlatform{}, fmt.Errorf("duplicate exposed channel id: %s", exposedID)
		}
		exposedIDs[exposedID] = sourceID
		channelIDMap[sourceID] = exposedID
		exposedChannelMap[exposedID] = sourceID
	}
	mediaAllowedCIDRs := make([]*net.IPNet, 0, len(in.MediaAllowedCIDRs))
	for _, value := range in.MediaAllowedCIDRs {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if ip := net.ParseIP(value); ip != nil {
			bits := 128
			if ip.To4() != nil {
				ip = ip.To4()
				bits = 32
			}
			mediaAllowedCIDRs = append(mediaAllowedCIDRs, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return cascadePlatform{}, fmt.Errorf("invalid media_allowed_cidrs entry %q: %w", value, err)
		}
		mediaAllowedCIDRs = append(mediaAllowedCIDRs, network)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = serverID
	}
	return cascadePlatform{
		name: name, serverID: serverID, remoteDomain: remoteDomain, remote: remote, transport: transport,
		localID: localID, localDomain: localDomain, localHost: localHost, localPort: local.Port, localTLSPort: localTLSPort, localTLSEnabled: local.EnableTLS,
		password: in.Password, signalDigestSeed: in.SignalDigestSeed,
		version: version, expires: expires, keepaliveInterval: keepalive, alarmDispatchEnabled: in.AlarmDispatchEnabled,
		sharedChannels: sharedChannels, channelIDMap: channelIDMap,
		exposedChannelMap: exposedChannelMap, mediaAllowedCIDRs: mediaAllowedCIDRs,
		tlsConfig: tlsConfig, registerCertificateAuth: registerCertificateAuth, monitorUserIdentity: monitorUserIdentity,
	}, nil
}

func cascadeTLSClientConfig(in conf.SIPUpstream, host string) (*tls.Config, error) {
	certFile := strings.TrimSpace(in.TLSCert)
	keyFile := strings.TrimSpace(in.TLSKey)
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("upstream TLS client certificate and key must be configured together")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: strings.TrimSpace(in.TLSServerName)}
	if config.ServerName == "" {
		config.ServerName = strings.TrimSpace(host)
	}
	if caFile := strings.TrimSpace(in.TLSCA); caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read upstream TLS CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("upstream TLS CA does not contain a valid certificate")
		}
		config.RootCAs = roots
	}
	if certFile != "" {
		certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load upstream TLS client certificate: %w", err)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	return config, nil
}

type cascadeExchange func(context.Context, *sip.Request) (*sip.Response, error)
type cascadeSend func(*sip.Request) error
type cascadeTCPDial func(context.Context, string) (net.Conn, error)
type cascadeTLSDial func(context.Context, string, string) (net.Conn, error)

type cascadeWorker struct {
	server   *Server
	platform cascadePlatform
	exchange cascadeExchange
	send     cascadeSend

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	// operationsCtx 只约束该上级承载的业务任务。热更新先取消业务，再保留注册上下文完成退订、BYE 和注销收尾。
	operationsCtx    context.Context
	cancelOperations context.CancelFunc

	mu        sync.RWMutex
	stopping  bool
	status    CascadePlatformStatus
	callID    sip.CallID
	fromTag   string
	cseq      uint32
	effective GBProtocolVersion
	accepted  int
	targetURI *sip.URI
	target    net.Addr

	connMu    sync.Mutex
	tcpConn   sip.Connection
	tcpRemote string
	dialTCP   cascadeTCPDial
	dialTLS   cascadeTLSDial

	inviteTxMu sync.Mutex
	inviteTx   map[string]*sip.Transaction

	responseTaskMu      sync.Mutex
	responseTasks       sync.WaitGroup
	responseTasksClosed bool
}

func newCascadeWorker(server *Server, platform cascadePlatform) *cascadeWorker {
	ctx, cancel := context.WithCancel(context.Background())
	operationsCtx, cancelOperations := context.WithCancel(ctx)
	scheme := "sip"
	if platform.transport == "tls" {
		scheme = "sips"
	}
	remoteURI, _ := sip.ParseSipURI(fmt.Sprintf("%s:%s@%s", scheme, platform.serverID, platform.remoteDomain))
	setCascadeURITransport(&remoteURI, platform.transport)
	w := &cascadeWorker{
		server: server, platform: platform, ctx: ctx, cancel: cancel, done: make(chan struct{}),
		operationsCtx: operationsCtx, cancelOperations: cancelOperations,
		callID: sip.CallID("cascade-register-" + sip.RandString(24)), fromTag: sip.RandString(24),
		effective: platform.version, accepted: platform.expires,
		targetURI: &remoteURI, target: cloneCascadeAddr(platform.remote),
		status: CascadePlatformStatus{
			Name: platform.name, ServerID: platform.serverID, Address: platform.remote.String(),
			ConfiguredVersion: string(platform.version), State: "starting",
		},
	}
	w.dialTCP = func(ctx context.Context, address string) (net.Conn, error) {
		return (&net.Dialer{Timeout: defaultCascadeRequestTimeout}).DialContext(ctx, "tcp", address)
	}
	w.dialTLS = func(ctx context.Context, address, serverName string) (net.Conn, error) {
		config := platform.tlsConfig
		if config == nil {
			return nil, fmt.Errorf("cascade SIP/TLS configuration is unavailable")
		}
		config = config.Clone()
		if strings.TrimSpace(serverName) != "" {
			config.ServerName = strings.TrimSpace(serverName)
		}
		dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: defaultCascadeRequestTimeout}, Config: config}
		return dialer.DialContext(ctx, "tcp", address)
	}
	w.exchange = w.exchangeRequest
	w.send = w.sendRequest
	return w
}

func (w *cascadeWorker) start() { go w.run() }

func (w *cascadeWorker) stop() {
	w.stopOperations()
	w.beginStop()
	w.responseTaskMu.Lock()
	w.responseTasksClosed = true
	w.responseTaskMu.Unlock()
	w.cancel()
	<-w.done
	w.responseTasks.Wait()
	w.closeInviteTransactions()
}

func (w *cascadeWorker) beginStop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if !w.stopping {
		w.stopping = true
		w.status.State = "stopping"
		w.status.Registered = false
	}
	w.mu.Unlock()
}

func (w *cascadeWorker) isStopping() bool {
	if w == nil {
		return true
	}
	w.mu.RLock()
	stopping := w.stopping
	w.mu.RUnlock()
	return stopping
}

func (w *cascadeWorker) stopOperations() {
	if w != nil && w.cancelOperations != nil {
		w.cancelOperations()
	}
}

func (w *cascadeWorker) operationContext() context.Context {
	if w == nil {
		return context.Background()
	}
	if w.operationsCtx != nil {
		return w.operationsCtx
	}
	if w.ctx != nil {
		return w.ctx
	}
	return context.Background()
}

// startResponseTask 跟踪已经写出的清理请求响应。业务任务停止后它仍可完成认证应答，
// worker 完整停止时则由 w.ctx 取消并等待退出，避免留下游离 goroutine。
func (w *cascadeWorker) startResponseTask(task func(context.Context)) bool {
	if w == nil || task == nil || w.ctx == nil {
		return false
	}
	w.responseTaskMu.Lock()
	if w.responseTasksClosed || w.ctx.Err() != nil {
		w.responseTaskMu.Unlock()
		return false
	}
	w.responseTasks.Add(1)
	w.responseTaskMu.Unlock()
	go func() {
		defer w.responseTasks.Done()
		task(w.ctx)
	}()
	return true
}

func withCascadeWorkerOperation(parent context.Context, worker *cascadeWorker) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	stopWorkerCancel := func() bool { return false }
	if worker != nil {
		stopWorkerCancel = context.AfterFunc(worker.operationContext(), cancel)
	}
	return ctx, func() {
		stopWorkerCancel()
		cancel()
	}
}

func (w *cascadeWorker) snapshot() CascadePlatformStatus {
	w.mu.RLock()
	state := w.status
	w.mu.RUnlock()
	return state
}

func cascadeRegistrationActive(state CascadePlatformStatus, now time.Time) bool {
	return state.Registered && (state.ExpiresAt.IsZero() || now.Before(state.ExpiresAt))
}

func (w *cascadeWorker) registrationActive(now time.Time) bool {
	return w != nil && cascadeRegistrationActive(w.snapshot(), now)
}

func (w *cascadeWorker) updateStatus(fn func(*CascadePlatformStatus)) {
	w.mu.Lock()
	fn(&w.status)
	w.mu.Unlock()
}

func (w *cascadeWorker) run() {
	defer close(w.done)
	defer w.closeTCPConnection()
	backoff := time.Second
	for {
		if err := w.register(w.ctx, w.platform.expires); err != nil {
			if w.ctx.Err() != nil || w.isStopping() {
				w.unregisterOnStop()
				return
			}
			w.updateStatus(func(state *CascadePlatformStatus) {
				state.State = "retrying"
				state.Registered = false
				state.LastError = err.Error()
			})
			if !waitCascade(w.ctx, backoff) {
				w.unregisterOnStop()
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		refreshAfter := time.Duration(w.accepted) * time.Second * 4 / 5
		refresh := time.NewTimer(refreshAfter)
		keepalive := time.NewTicker(w.platform.keepaliveInterval)
		registered := true
		for registered {
			select {
			case <-w.ctx.Done():
				if !refresh.Stop() {
					select {
					case <-refresh.C:
					default:
					}
				}
				keepalive.Stop()
				w.unregisterOnStop()
				return
			case <-refresh.C:
				keepalive.Stop()
				registered = false
			case <-keepalive.C:
				if err := w.keepalive(w.ctx); err != nil {
					if !refresh.Stop() {
						select {
						case <-refresh.C:
						default:
						}
					}
					keepalive.Stop()
					if w.ctx.Err() != nil || w.isStopping() {
						w.unregisterOnStop()
						return
					}
					w.updateStatus(func(state *CascadePlatformStatus) {
						state.State = "retrying"
						state.Registered = false
						state.LastError = err.Error()
					})
					registered = false
				}
			}
		}
	}
}

func waitCascade(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *cascadeWorker) unregisterOnStop() {
	state := w.snapshot()
	if !state.Registered && (state.ExpiresAt.IsZero() || !time.Now().Before(state.ExpiresAt)) {
		w.updateStatus(func(state *CascadePlatformStatus) { state.State = "stopped" })
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = w.register(ctx, 0)
	w.updateStatus(func(state *CascadePlatformStatus) {
		state.State = "stopped"
		state.Registered = false
		state.ExpiresAt = time.Time{}
	})
}

func (w *cascadeWorker) register(ctx context.Context, expires int) error {
	w.updateStatus(func(state *CascadePlatformStatus) {
		state.State = "registering"
		state.LastError = ""
	})
	requestedExpires := expires
	if requestedExpires > 0 {
		w.mu.RLock()
		if w.accepted > requestedExpires {
			requestedExpires = w.accepted
		}
		w.mu.RUnlock()
	}
	targetURI, target := w.remoteTarget()
	authorization := w.initialRegisterAuthorization()
	proxyAuthorization := ""
	request := w.newRegisterRequestWithCredentials(requestedExpires, authorization, proxyAuthorization)
	applyCascadeRequestTarget(request, targetURI, target)
	var response *sip.Response
	redirects := 0
	registrarAuthAttempts := 0
	proxyAuthAttempts := 0
	minimumRetries := 0
	certificateChallengeCompleted := false
	effective := w.protocolVersion()
	for {
		var err error
		response, err = w.exchange(ctx, request)
		if err != nil {
			return fmt.Errorf("cascade REGISTER: %w", err)
		}
		// 附录 I 要求 REGISTER 的成功和失败响应都携带版本号；认证重试必须立即使用已获知的较低版本。
		effective, err = negotiateCascadeVersion(effective, response)
		if err != nil {
			return fmt.Errorf("cascade REGISTER response protocol version: %w", err)
		}
		w.mu.Lock()
		w.effective = effective
		w.mu.Unlock()
		switch response.StatusCode() {
		case http.StatusMovedPermanently, http.StatusFound:
			if redirects >= maxCascadeRegisterRedirects {
				return fmt.Errorf("cascade REGISTER redirect limit exceeded")
			}
			targetURI, target, err = cascadeRegisterRedirectTarget(response, w.platform.serverID, cascadeTransportForAddr(target))
			if err != nil {
				return err
			}
			redirects++
			registrarAuthAttempts = 0
			proxyAuthAttempts = 0
			certificateChallengeCompleted = false
			authorization = w.initialRegisterAuthorization()
			proxyAuthorization = ""
			request = w.newRegisterRequestWithCredentials(requestedExpires, authorization, proxyAuthorization)
			applyCascadeRequestTarget(request, targetURI, target)
			continue
		case http.StatusUnauthorized:
			if registrarAuthAttempts > 0 {
				return fmt.Errorf("cascade REGISTER authentication failed after challenge")
			}
			scheme, challenge, err := cascadeRegisterChallenge(response, w.platform.registerCertificateAuth != nil)
			if err != nil {
				return err
			}
			switch scheme {
			case "asymmetric":
				if w.platform.registerCertificateAuth == nil {
					return fmt.Errorf("cascade REGISTER received Asymmetric challenge without certificate authentication configuration")
				}
				authorization, err = w.platform.registerCertificateAuth.asymmetricAuthorization(challenge)
				if err == nil {
					certificateChallengeCompleted = true
				}
			case "digest":
				if w.platform.registerCertificateAuth != nil && w.platform.registerCertificateAuth.required {
					return fmt.Errorf("cascade REGISTER refused certificate-to-Digest authentication downgrade")
				}
				var auth *sip.Authorization
				auth, err = cascadeDigestAuthorizationFromChallenge(challenge, request, w.platform.localID, w.platform.password)
				if err == nil {
					authorization = auth.String()
				}
			default:
				return fmt.Errorf("cascade REGISTER unsupported authentication challenge %q", scheme)
			}
			if err != nil {
				return err
			}
			registrarAuthAttempts++
			request = w.newRegisterRequestWithCredentials(requestedExpires, authorization, proxyAuthorization)
			applyCascadeRequestTarget(request, targetURI, target)
			continue
		case http.StatusProxyAuthRequired:
			if proxyAuthAttempts > 0 {
				return fmt.Errorf("cascade REGISTER proxy authentication failed after challenge")
			}
			headerName, auth, err := cascadeRequestDigestAuthorization(response, request, w.platform.localID, w.platform.password)
			if err != nil {
				return fmt.Errorf("cascade REGISTER proxy authentication: %w", err)
			}
			if headerName != "Proxy-Authorization" || auth == nil {
				return fmt.Errorf("cascade REGISTER proxy authentication returned invalid credentials")
			}
			proxyAuthorization = auth.String()
			proxyAuthAttempts++
			request = w.newRegisterRequestWithCredentials(requestedExpires, authorization, proxyAuthorization)
			applyCascadeRequestTarget(request, targetURI, target)
			continue
		case statusIntervalTooBrief:
			if requestedExpires == 0 {
				return fmt.Errorf("cascade unregister received 423 Interval Too Brief")
			}
			if minimumRetries >= maxCascadeRegisterMinRetries {
				return fmt.Errorf("cascade REGISTER minimum expiry negotiation limit exceeded")
			}
			minimum, err := cascadeRegisterMinimumExpires(response)
			if err != nil {
				return err
			}
			if minimum <= requestedExpires {
				return fmt.Errorf("cascade REGISTER Min-Expires %d does not exceed requested %d", minimum, requestedExpires)
			}
			requestedExpires = minimum
			minimumRetries++
			registrarAuthAttempts = 0
			proxyAuthAttempts = 0
			certificateChallengeCompleted = false
			authorization = w.initialRegisterAuthorization()
			proxyAuthorization = ""
			request = w.newRegisterRequestWithCredentials(requestedExpires, authorization, proxyAuthorization)
			applyCascadeRequestTarget(request, targetURI, target)
			continue
		}
		break
	}
	if response.StatusCode() < 200 || response.StatusCode() >= 300 {
		return fmt.Errorf("cascade REGISTER failed: %d %s", response.StatusCode(), response.Reason())
	}
	if w.platform.registerCertificateAuth != nil && w.platform.registerCertificateAuth.required && !certificateChallengeCompleted {
		return fmt.Errorf("cascade REGISTER certificate authentication required but upstream returned success without Asymmetric challenge")
	}
	accepted, err := validateCascadeRegisterSuccess(response, request, effective, requestedExpires)
	if err != nil {
		return err
	}
	now := time.Now()
	w.mu.Lock()
	w.effective = effective
	w.accepted = accepted
	w.targetURI = targetURI.Clone()
	w.target = cloneCascadeAddr(target)
	state := &w.status
	state.Address = target.String()
	state.NegotiatedVersion = string(effective)
	state.LastRegisterAt = now
	state.LastError = ""
	if requestedExpires == 0 {
		state.State = "stopped"
		state.Registered = false
		state.ExpiresAt = time.Time{}
		w.mu.Unlock()
		return nil
	}
	state.ExpiresAt = now.Add(time.Duration(accepted) * time.Second)
	if w.stopping {
		// 上级可能在本地停止后才返回成功。保留远端绑定的目标和有效期，
		// 让停止流程能够发送 Expires: 0，但绝不能重新暴露为已注册。
		state.State = "stopping"
		state.Registered = false
		w.mu.Unlock()
		return errCascadeWorkerStopping
	}
	state.State = "registered"
	state.Registered = true
	w.mu.Unlock()
	return nil
}

func (w *cascadeWorker) keepalive(ctx context.Context) error {
	body, err := sip.XMLEncode(struct {
		XMLName  xml.Name `xml:"Notify"`
		CmdType  string   `xml:"CmdType"`
		SN       int      `xml:"SN"`
		DeviceID string   `xml:"DeviceID"`
		Status   string   `xml:"Status"`
	}{CmdType: "Keepalive", SN: sip.RandInt(100000, 999999), DeviceID: w.platform.localID, Status: "OK"})
	if err != nil {
		return err
	}
	request := w.newKeepaliveRequest(body)
	if err := w.platform.monitorUserIdentity.apply(ctx, request); err != nil {
		return fmt.Errorf("cascade Keepalive Monitor-User-Identity: %w", err)
	}
	response, err := w.exchangeMessageWithDigest(ctx, request)
	if err != nil {
		return fmt.Errorf("cascade Keepalive: %w", err)
	}
	if response.StatusCode() != http.StatusOK {
		return fmt.Errorf("cascade Keepalive failed: %d %s", response.StatusCode(), response.Reason())
	}
	now := time.Now()
	w.mu.Lock()
	state := &w.status
	if !w.stopping {
		state.LastKeepaliveAt = now
		state.LastError = ""
		if !state.ExpiresAt.IsZero() && !now.Before(state.ExpiresAt) {
			// Keepalive 不能延长 REGISTER 绑定；响应跨过注册有效期时不得复活绑定。
			state.State = "expired"
			state.Registered = false
		} else {
			state.State = "registered"
			state.Registered = true
		}
	}
	w.mu.Unlock()
	return nil
}

func (w *cascadeWorker) newRegisterRequest(expires int, auth *sip.Authorization) *sip.Request {
	w.mu.Lock()
	next, err := sip.NextCSeq(w.cseq)
	if err != nil {
		// REGISTER 不在 SIP 对话内；序号耗尽时以新 Call-ID 建立新的注册序列。
		w.callID = sip.CallID("cascade-register-" + sip.RandString(24))
		next = 1
	}
	w.cseq = next
	callID := w.callID
	cseq := w.cseq
	w.mu.Unlock()
	return w.newRequest(sip.MethodRegister, nil, nil, &callID, cseq, expires, auth)
}

func (w *cascadeWorker) newRegisterRequestWithCredentials(expires int, authorization, proxyAuthorization string) *sip.Request {
	request := w.newRegisterRequest(expires, nil)
	if authorization != "" {
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Authorization", Contents: authorization})
	}
	if proxyAuthorization != "" {
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Proxy-Authorization", Contents: proxyAuthorization})
	}
	return request
}

func (w *cascadeWorker) initialRegisterAuthorization() string {
	if w.platform.registerCertificateAuth == nil {
		return ""
	}
	return w.platform.registerCertificateAuth.capabilityAuthorization()
}

func (w *cascadeWorker) newKeepaliveRequest(body []byte) *sip.Request {
	callID := sip.CallID("cascade-keepalive-" + sip.RandString(24))
	return w.newRequest(sip.MethodMessage, &sip.ContentTypeXML, body, &callID, 1, -1, nil)
}

func (w *cascadeWorker) contactAddress() *sip.Address {
	transport := cascadeTransportForAddr(w.remoteDestination())
	uri, err := sip.ParseSipURI(fmt.Sprintf("sip:%s@%s", w.platform.localID, net.JoinHostPort(w.platform.localHost, strconv.Itoa(w.platform.contactPort(transport)))))
	if err != nil {
		return nil
	}
	setCascadeURITransport(&uri, transport)
	return &sip.Address{URI: &uri, Params: sip.NewParams()}
}

func (w *cascadeWorker) protocolVersion() GBProtocolVersion {
	w.mu.RLock()
	version := w.effective
	w.mu.RUnlock()
	return version
}

func (w *cascadeWorker) remoteTarget() (*sip.URI, net.Addr) {
	w.mu.RLock()
	uri := w.targetURI
	remote := w.target
	w.mu.RUnlock()
	if uri == nil {
		scheme := "sip"
		if w.platform.transport == "tls" {
			scheme = "sips"
		}
		fallback, _ := sip.ParseSipURI(fmt.Sprintf("%s:%s@%s", scheme, w.platform.serverID, w.platform.remoteDomain))
		setCascadeURITransport(&fallback, w.platform.transport)
		uri = &fallback
	}
	if remote == nil {
		remote = w.platform.remote
	}
	return uri.Clone(), cloneCascadeAddr(remote)
}

func (w *cascadeWorker) remoteDestination() net.Addr {
	_, remote := w.remoteTarget()
	return remote
}

func (w *cascadeWorker) remoteAddressMatches(source net.Addr) bool {
	if source == nil {
		return false
	}
	remoteIP := addressIP(w.remoteDestination())
	return remoteIP != nil && remoteIP.Equal(addressIP(source))
}

func (w *cascadeWorker) targetURIForUser(user string) *sip.URI {
	uri, _ := w.remoteTarget()
	uri.FUser = sip.String{Str: strings.TrimSpace(user)}
	uri.FPassword = nil
	return uri
}

func (w *cascadeWorker) sendMessage(ctx context.Context, body []byte) error {
	ctx, cancel := withCascadeWorkerOperation(ctx, w)
	defer cancel()
	request := w.newKeepaliveRequest(body)
	if err := w.platform.monitorUserIdentity.apply(ctx, request); err != nil {
		return fmt.Errorf("cascade MESSAGE Monitor-User-Identity: %w", err)
	}
	response, err := w.exchangeMessageWithDigest(ctx, request)
	if err != nil {
		return fmt.Errorf("cascade MESSAGE: %w", err)
	}
	if response.StatusCode() != http.StatusOK {
		return fmt.Errorf("cascade MESSAGE failed: %d %s", response.StatusCode(), response.Reason())
	}
	return nil
}

func (w *cascadeWorker) exchangeMessageWithDigest(ctx context.Context, request *sip.Request) (*sip.Response, error) {
	return w.exchangeMessageWithDigestPrepared(ctx, request, nil)
}

func (w *cascadeWorker) exchangeMessageWithDigestPrepared(ctx context.Context, request *sip.Request, prepareRetry func(*sip.Request) error) (*sip.Response, error) {
	return w.exchangeRequestWithDigestPrepared(ctx, request, prepareRetry)
}

func (w *cascadeWorker) exchangeRequestWithDigest(ctx context.Context, request *sip.Request) (*sip.Response, error) {
	return w.exchangeRequestWithDigestPrepared(ctx, request, nil)
}

func (w *cascadeWorker) exchangeRequestWithDigestPrepared(ctx context.Context, request *sip.Request, prepareRetry func(*sip.Request) error) (*sip.Response, error) {
	method := "request"
	if request != nil && strings.TrimSpace(request.Method()) != "" {
		method = strings.ToUpper(strings.TrimSpace(request.Method()))
	}
	response, err := w.exchange(ctx, request)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, fmt.Errorf("cascade %s response is unavailable", method)
	}
	if response.StatusCode() != http.StatusUnauthorized && response.StatusCode() != http.StatusProxyAuthRequired {
		return response, nil
	}

	authorizationHeader, authorization, err := cascadeRequestDigestAuthorization(
		response, request, w.platform.localID, w.platform.password,
	)
	if err != nil {
		return response, fmt.Errorf("cascade %s Digest challenge: %w", method, err)
	}
	retry, err := buildCascadeRequestDigestRetry(request, authorizationHeader, authorization)
	if err != nil {
		return response, err
	}
	if prepareRetry != nil {
		if err := prepareRetry(retry); err != nil {
			return response, err
		}
	}
	response, err = w.exchange(ctx, retry)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, fmt.Errorf("cascade %s authentication response is unavailable", method)
	}
	return response, nil
}

func buildCascadeRequestDigestRetry(request *sip.Request, authorizationHeader string, authorization *sip.Authorization) (*sip.Request, error) {
	method := "request"
	if request != nil && strings.TrimSpace(request.Method()) != "" {
		method = strings.ToUpper(strings.TrimSpace(request.Method()))
	}
	if request == nil || authorization == nil ||
		(authorizationHeader != "Authorization" && authorizationHeader != "Proxy-Authorization") {
		return nil, fmt.Errorf("cascade %s Digest credentials are unavailable", method)
	}
	retry, ok := request.Clone().(*sip.Request)
	if !ok || retry == nil {
		return nil, fmt.Errorf("clone cascade %s request failed", method)
	}
	cseq, ok := retry.CSeq()
	if !ok || cseq == nil || !strings.EqualFold(cseq.MethodName, method) {
		return nil, fmt.Errorf("cascade %s CSeq is invalid", method)
	}
	next, err := sip.NextCSeq(cseq.SeqNo)
	if err != nil {
		return nil, fmt.Errorf("cascade %s Digest retry: %w", method, err)
	}
	cseq.SeqNo = next
	via, ok := retry.ViaHop()
	if !ok || via == nil {
		return nil, fmt.Errorf("cascade %s Via is unavailable", method)
	}
	params := sip.NewParams()
	if via.Params != nil {
		for _, key := range via.Params.Keys() {
			if strings.EqualFold(key, "branch") {
				continue
			}
			if value, exists := via.Params.Items()[key]; exists {
				params.Add(key, value)
			}
		}
	}
	via.Params = params.Add("branch", sip.String{Str: sip.GenerateBranch()})
	retry.RemoveHeader("Authorization")
	retry.RemoveHeader("Proxy-Authorization")
	retry.AppendHeader(&sip.GenericHeader{HeaderName: authorizationHeader, Contents: authorization.String()})
	return retry, nil
}

func (w *cascadeWorker) newRequest(method string, contentType *sip.ContentType, body []byte, callID *sip.CallID, cseq uint32, expires int, auth *sip.Authorization) *sip.Request {
	remoteURI, remote := w.remoteTarget()
	transport := cascadeTransportForAddr(remote)
	localPort := w.platform.contactPort(transport)
	localURI, _ := sip.ParseSipURI(fmt.Sprintf("sip:%s@%s", w.platform.localID, w.platform.localDomain))
	contactURI, _ := sip.ParseSipURI(fmt.Sprintf("sip:%s@%s", w.platform.localID, net.JoinHostPort(w.platform.localHost, strconv.Itoa(localPort))))
	setCascadeURITransport(&contactURI, transport)
	from := &sip.Address{URI: &localURI, Params: sip.NewParams().Add("tag", sip.String{Str: w.fromTag})}
	toURI := remoteURI.Clone()
	if method == sip.MethodRegister {
		toURI = localURI.Clone()
	}
	to := &sip.Address{URI: toURI, Params: sip.NewParams()}
	contact := &sip.Address{URI: &contactURI, Params: sip.NewParams()}
	headers := sip.NewHeaderBuilder().
		SetFrom(from).
		SetTo(to).
		SetContact(contact).
		SetContentType(contentType).
		SetMethod(method).
		SetSeqNo(uint(cseq)).
		SetCallID(callID).
		SetXGBVerValue(string(w.protocolVersion())).
		AddVia(&sip.ViaHop{Host: w.platform.localHost, Port: sip.NewPort(localPort), Transport: strings.ToUpper(transport), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}).Add("rport", nil)}).
		Build()
	if expires >= 0 {
		headers = append(headers, &sip.GenericHeader{HeaderName: "Expires", Contents: strconv.Itoa(expires)})
	}
	if auth != nil {
		headers = append(headers, &sip.GenericHeader{HeaderName: "Authorization", Contents: auth.String()})
	}
	if len(body) == 0 {
		length := sip.ContentLength(0)
		headers = append(headers, &length)
	}
	request := sip.NewRequest("", method, remoteURI, sip.DefaultSipVersion, headers, body)
	request.SetDestination(remote)
	if transport == "udp" && w.server != nil && w.server.Server != nil && w.server.UDPConn() != nil {
		request.SetConnection(w.server.UDPConn())
		request.SetSource(w.server.UDPConn().LocalAddr())
	}
	return request
}

func cascadeTransportForAddr(addr net.Addr) string {
	if addr != nil {
		network := strings.ToLower(strings.TrimSpace(addr.Network()))
		if strings.HasPrefix(network, "tls") {
			return "tls"
		}
		if strings.HasPrefix(network, "tcp") {
			return "tcp"
		}
	}
	return "udp"
}

func applyCascadeRequestTransport(request *sip.Request, transport string) {
	if request == nil {
		return
	}
	transport = strings.ToLower(strings.TrimSpace(transport))
	if transport == "" {
		transport = "udp"
	}
	if via, ok := request.ViaHop(); ok && via != nil {
		via.Transport = strings.ToUpper(transport)
	}
	if contact, ok := request.Contact(); ok && contact != nil && contact.Address != nil {
		setCascadeURITransport(contact.Address, transport)
	}
}

func applyCascadeRequestNextHopTransport(request *sip.Request, transport string) {
	if request == nil {
		return
	}
	setCascadeURITransport(request.NextHopURI(), transport)
}

func setCascadeURITransport(uri *sip.URI, transport string) {
	if uri == nil {
		return
	}
	params := sip.NewParams()
	if uri.FUriParams != nil {
		for key, value := range uri.FUriParams.Items() {
			if !strings.EqualFold(strings.TrimSpace(key), "transport") {
				params.Add(key, value)
			}
		}
	}
	transport = strings.ToLower(strings.TrimSpace(transport))
	uri.FIsEncrypted = transport == "tls"
	if transport == "tcp" || transport == "tls" {
		params.Add("transport", sip.String{Str: transport})
	}
	uri.FUriParams = params
}

func applyCascadeRequestTarget(request *sip.Request, uri *sip.URI, remote net.Addr) {
	if request == nil || uri == nil || remote == nil {
		return
	}
	request.SetRecipient(uri.Clone())
	request.SetDestination(cloneCascadeAddr(remote))
	applyCascadeRequestTransport(request, cascadeTransportForAddr(remote))
}

func cascadeRegisterRedirectTarget(response *sip.Response, serverID, defaultTransport string) (*sip.URI, net.Addr, error) {
	contact, ok := response.Contact()
	if !ok || contact == nil || contact.Address == nil {
		return nil, nil, fmt.Errorf("cascade REGISTER redirect missing Contact")
	}
	uri := contact.Address.Clone()
	if uri.FPassword != nil {
		return nil, nil, fmt.Errorf("cascade REGISTER redirect Contact must not contain password")
	}
	if user := uri.User(); user != nil && strings.TrimSpace(user.String()) != "" && strings.TrimSpace(user.String()) != strings.TrimSpace(serverID) {
		return nil, nil, fmt.Errorf("cascade REGISTER redirect server ID mismatch")
	}
	if uri.User() == nil || strings.TrimSpace(uri.User().String()) == "" {
		uri.FUser = sip.String{Str: strings.TrimSpace(serverID)}
	}
	transport := strings.ToLower(strings.TrimSpace(defaultTransport))
	if transport == "" {
		transport = "udp"
	}
	explicitTransport := false
	if uri.FUriParams != nil {
		if value, exists := uri.FUriParams.Get("transport"); exists && value != nil {
			transport = strings.ToLower(strings.TrimSpace(value.String()))
			explicitTransport = true
		}
	}
	if uri.FIsEncrypted {
		if explicitTransport && transport != "tls" {
			return nil, nil, fmt.Errorf("cascade REGISTER SIPS redirect has conflicting transport")
		}
		transport = "tls"
	}
	if strings.EqualFold(strings.TrimSpace(defaultTransport), "tls") && transport != "tls" {
		return nil, nil, fmt.Errorf("cascade REGISTER redirect must not downgrade SIPS transport")
	}
	if transport != "udp" && transport != "tcp" && transport != "tls" {
		return nil, nil, fmt.Errorf("cascade REGISTER redirect only supports UDP/TCP/TLS transport")
	}
	host := strings.TrimSpace(uri.Host())
	if host == "" {
		return nil, nil, fmt.Errorf("cascade REGISTER redirect Contact host is empty")
	}
	port := 5060
	if transport == "tls" {
		port = 5061
	}
	if uri.FPort != nil {
		port = int(*uri.FPort)
	}
	var remote net.Addr
	var err error
	if transport == "tcp" {
		remote, err = net.ResolveTCPAddr("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	} else if transport == "tls" {
		var tcpAddr *net.TCPAddr
		tcpAddr, err = net.ResolveTCPAddr("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err == nil {
			remote = &cascadeTLSAddr{TCPAddr: tcpAddr, serverName: host}
		}
	} else {
		remote, err = net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port)))
	}
	if err != nil {
		return nil, nil, fmt.Errorf("resolve cascade REGISTER redirect: %w", err)
	}
	setCascadeURITransport(uri, transport)
	return uri, remote, nil
}

func cloneCascadeAddr(value net.Addr) net.Addr {
	switch addr := value.(type) {
	case *net.UDPAddr:
		if addr == nil {
			return nil
		}
		clone := *addr
		clone.IP = append(net.IP(nil), addr.IP...)
		return &clone
	case *net.TCPAddr:
		if addr == nil {
			return nil
		}
		clone := *addr
		clone.IP = append(net.IP(nil), addr.IP...)
		return &clone
	case *cascadeTLSAddr:
		if addr == nil || addr.TCPAddr == nil {
			return nil
		}
		clone := *addr.TCPAddr
		clone.IP = append(net.IP(nil), addr.IP...)
		return &cascadeTLSAddr{TCPAddr: &clone, serverName: addr.serverName}
	default:
		return value
	}
}

func cascadeDigestAuthorization(response *sip.Response, request *sip.Request, username, password string) (*sip.Authorization, error) {
	headerName, auth, err := cascadeRequestDigestAuthorization(response, request, username, password)
	if err != nil {
		return nil, err
	}
	if headerName != "Authorization" {
		return nil, fmt.Errorf("cascade REGISTER expected Authorization credentials")
	}
	return auth, nil
}

func cascadeRequestDigestAuthorization(response *sip.Response, request *sip.Request, username, password string) (string, *sip.Authorization, error) {
	if response == nil || request == nil || request.Recipient() == nil {
		return "", nil, fmt.Errorf("Digest challenge is incomplete")
	}
	challengeHeader := ""
	authorizationHeader := ""
	switch response.StatusCode() {
	case http.StatusUnauthorized:
		challengeHeader = "WWW-Authenticate"
		authorizationHeader = "Authorization"
	case http.StatusProxyAuthRequired:
		challengeHeader = "Proxy-Authenticate"
		authorizationHeader = "Proxy-Authorization"
	default:
		return "", nil, fmt.Errorf("response %d is not an authentication challenge", response.StatusCode())
	}
	headers := response.GetHeaders(challengeHeader)
	if len(headers) != 1 || headers[0] == nil {
		return "", nil, fmt.Errorf("response must contain exactly one %s header", challengeHeader)
	}
	auth, err := sip.AuthFromValueChecked(headers[0].String())
	if err != nil {
		return "", nil, fmt.Errorf("invalid Digest challenge: %w", err)
	}
	if auth == nil || auth.Get("realm") == "" || auth.Get("nonce") == "" {
		return "", nil, fmt.Errorf("invalid Digest challenge")
	}
	auth.SetUsername(username).
		SetPassword(password).
		SetMethod(request.Method()).
		SetURI(request.Recipient().String())
	if auth.QOP() == "auth" {
		auth.SetClientNonce("00000001", sip.RandString(24))
	}
	if _, err := auth.CalcResponseChecked(); err != nil {
		return "", nil, fmt.Errorf("unsupported Digest challenge: %w", err)
	}
	return authorizationHeader, auth, nil
}

func cascadeDigestAuthorizationFromChallenge(auth *sip.Authorization, request *sip.Request, username, password string) (*sip.Authorization, error) {
	if auth == nil || request == nil || request.Recipient() == nil {
		return nil, fmt.Errorf("cascade REGISTER invalid Digest challenge")
	}
	if auth.Get("realm") == "" || auth.Get("nonce") == "" {
		return nil, fmt.Errorf("cascade REGISTER invalid Digest challenge")
	}
	auth.SetUsername(username).
		SetPassword(password).
		SetMethod(request.Method()).
		SetURI(request.Recipient().String())
	if auth.QOP() == "auth" {
		auth.SetClientNonce("00000001", sip.RandString(24))
	}
	if _, err := auth.CalcResponseChecked(); err != nil {
		return nil, fmt.Errorf("cascade REGISTER unsupported Digest challenge: %w", err)
	}
	return auth, nil
}

func negotiateCascadeVersion(configured GBProtocolVersion, response *sip.Response) (GBProtocolVersion, error) {
	_, remote, present, err := parseXGBVersionHeader(response)
	if err != nil {
		return "", err
	}
	if !present {
		// 兼容未实现 2022 附录 I 的存量 2011/2014/2016 上级，沿用已配置或已协商档案。
		return configured, nil
	}
	if !remote.Valid() {
		// 语法合法但本平台未知的扩展版本不能静默启用高版本能力。
		return GBVersion10, nil
	}
	if remote.rank() < configured.rank() {
		return remote, nil
	}
	return configured, nil
}

func cascadeAcceptedExpires(response *sip.Response, requested int) (int, error) {
	if requested == 0 {
		return 0, nil
	}
	value := ""
	if headers := response.GetHeaders("Expires"); len(headers) > 1 {
		return 0, fmt.Errorf("cascade REGISTER response contains multiple Expires headers")
	} else if len(headers) == 1 {
		value = headers[0].String()
		if _, after, ok := strings.Cut(value, ":"); ok {
			value = after
		}
	}
	if value == "" {
		if contact, ok := response.Contact(); ok && contact != nil && contact.Params != nil {
			if expires, ok := contact.Params.Get("expires"); ok && expires != nil {
				value = expires.String()
			}
		}
	}
	if strings.TrimSpace(value) == "" {
		return requested, nil
	}
	return parseCascadeAcceptedExpires(value, requested)
}

func parseCascadeAcceptedExpires(value string, requested int) (int, error) {
	accepted, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || accepted <= 0 || int64(accepted) > maximumRegisterExpires {
		return 0, fmt.Errorf("cascade REGISTER invalid response expires: %s", strings.TrimSpace(value))
	}
	if accepted > requested {
		return 0, fmt.Errorf("cascade REGISTER response expires %d exceeds requested %d", accepted, requested)
	}
	return accepted, nil
}

func cascadeRegisterMinimumExpires(response *sip.Response) (int, error) {
	value, err := singleSIPHeaderValue(response, "Min-Expires")
	if err != nil {
		return 0, fmt.Errorf("cascade REGISTER invalid 423 response: %w", err)
	}
	minimum, parseErr := strconv.Atoi(value)
	if parseErr != nil || minimum <= 0 || int64(minimum) > maximumRegisterExpires {
		return 0, fmt.Errorf("cascade REGISTER invalid Min-Expires: %s", value)
	}
	return minimum, nil
}

func validateCascadeRegisterSuccess(response *sip.Response, request *sip.Request, version GBProtocolVersion, requested int) (int, error) {
	date, err := singleSIPHeaderValue(response, "Date")
	if err != nil {
		return 0, fmt.Errorf("cascade REGISTER invalid Date header: %w", err)
	}
	if _, err = sip.ParseGBTime("2006-01-02T15:04:05.000", date); err != nil {
		return 0, fmt.Errorf("cascade REGISTER invalid Date: %s", date)
	}
	if requested == 0 {
		if value, headerErr := singleSIPHeaderValue(response, "Expires"); headerErr != nil {
			return 0, fmt.Errorf("cascade unregister invalid Expires header: %w", headerErr)
		} else if value != "" && value != "0" {
			return 0, fmt.Errorf("cascade unregister response expires must be 0")
		}
		return 0, nil
	}

	requestContact, ok := request.Contact()
	if !ok || requestContact == nil || requestContact.Address == nil {
		return 0, fmt.Errorf("cascade REGISTER request Contact is unavailable")
	}
	var matched *sip.ContactHeader
	for _, header := range response.GetHeaders("Contact") {
		contact, valid := header.(*sip.ContactHeader)
		if !valid || contact == nil || contact.Address == nil {
			return 0, fmt.Errorf("cascade REGISTER response Contact is invalid")
		}
		if contact.Address.Equals(requestContact.Address) {
			if matched != nil {
				return 0, fmt.Errorf("cascade REGISTER response contains duplicate binding Contact")
			}
			matched = contact
		}
	}
	if matched == nil {
		return 0, fmt.Errorf("cascade REGISTER response is missing the requested Contact binding")
	}

	topLevelExpires, err := singleSIPHeaderValue(response, "Expires")
	if err != nil {
		return 0, fmt.Errorf("cascade REGISTER invalid Expires header: %w", err)
	}
	acceptedValue := topLevelExpires
	if matched.Params != nil {
		if contactExpires, exists := matched.Params.Get("expires"); exists && contactExpires != nil {
			acceptedValue = contactExpires.String()
		}
	}
	if strings.TrimSpace(acceptedValue) == "" {
		return 0, fmt.Errorf("cascade REGISTER response is missing accepted Expires")
	}
	accepted, err := parseCascadeAcceptedExpires(acceptedValue, requested)
	if err != nil {
		return 0, err
	}
	if registerExpiresTooBrief(version, accepted) {
		return 0, fmt.Errorf("cascade REGISTER response expires %d is below %s minimum %d", accepted, version.StandardName(), minimumStandardRegisterTTL)
	}
	return accepted, nil
}

func (w *cascadeWorker) exchangeRequest(ctx context.Context, request *sip.Request) (*sip.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, defaultCascadeRequestTimeout)
	defer cancel()
	security, err := w.signalDigestSecurity()
	if err != nil {
		return nil, err
	}
	if err := w.prepareRequestConnection(waitCtx, request, security); err != nil {
		return nil, err
	}
	tx, err := w.server.RequestWithSecurityContext(waitCtx, request, security)
	if err != nil {
		w.invalidateTCPConnection(request.GetConnection())
		return nil, err
	}
	response, err := tx.GetResponseContext(waitCtx)
	if err != nil {
		cancelCtx, cancelCancel := context.WithTimeout(context.WithoutCancel(ctx), defaultCascadeCancelTimeout)
		cancelSent, cancelErr := tx.CancelInviteDetachedContext(cancelCtx)
		cancelCancel()
		if !cancelSent && cancelErr == nil {
			tx.Close()
		}
		w.invalidateTCPConnection(request.GetConnection())
		if ctx.Err() != nil {
			return nil, errors.Join(ctx.Err(), cancelErr)
		}
		return nil, errors.Join(fmt.Errorf("response timeout"), cancelErr)
	}
	if response == nil {
		cancelCtx, cancelCancel := context.WithTimeout(context.WithoutCancel(ctx), defaultCascadeCancelTimeout)
		cancelSent, cancelErr := tx.CancelInviteDetachedContext(cancelCtx)
		cancelCancel()
		if !cancelSent && cancelErr == nil {
			tx.Close()
		}
		w.invalidateTCPConnection(request.GetConnection())
		return nil, errors.Join(fmt.Errorf("response timeout"), cancelErr)
	}
	w.completeExchangeTransaction(request, response, tx)
	return response, nil
}

// completeExchangeTransaction 在最终响应后立即回收普通客户端事务。
// INVITE 事务继续保留：2xx 需要原事务发送 ACK，非 2xx 需要在完成窗口内 ACK 重传响应。
func (w *cascadeWorker) completeExchangeTransaction(request *sip.Request, response *sip.Response, tx *sip.Transaction) {
	if tx == nil {
		return
	}
	if request != nil && strings.EqualFold(strings.TrimSpace(request.Method()), sip.MethodInvite) {
		w.rememberInviteTransaction(response, tx)
		return
	}
	tx.Close()
}

func (w *cascadeWorker) sendRequest(request *sip.Request) error {
	return w.sendRequestContext(w.ctx, request)
}

func (w *cascadeWorker) sendRequestContext(ctx context.Context, request *sip.Request) error {
	_, err := w.sendRequestTransactionContext(ctx, request)
	return err
}

// sendRequestWithDigestAsyncContext 同步完成初次写出，再由 worker 跟踪最终响应。
// 清理调用方无需等待上级响应；若收到 401/407，后台最多补发一次认证请求。
func (w *cascadeWorker) sendRequestWithDigestAsyncContext(ctx context.Context, request *sip.Request) error {
	return w.sendRequestWithDigestAsyncPreparedContext(ctx, request, nil)
}

func (w *cascadeWorker) sendRequestWithDigestAsyncPreparedContext(ctx context.Context, request *sip.Request, prepareRetry func(*sip.Request) error) error {
	tx, err := w.sendRequestTransactionContext(ctx, request)
	if err != nil {
		return err
	}
	if tx == nil {
		return nil
	}
	if !w.startResponseTask(func(taskCtx context.Context) {
		w.handleAsyncDigestResponse(taskCtx, request, tx, prepareRetry)
	}) {
		tx.Close()
	}
	return nil
}

func (w *cascadeWorker) handleAsyncDigestResponse(ctx context.Context, request *sip.Request, tx *sip.Transaction, prepareRetry func(*sip.Request) error) {
	defer tx.Close()
	response, err := w.waitAsyncFinalResponse(ctx, request, tx)
	if err != nil {
		w.logAsyncResponseError(request, err)
		return
	}
	if response.StatusCode() != http.StatusUnauthorized && response.StatusCode() != http.StatusProxyAuthRequired {
		return
	}
	authorizationHeader, authorization, err := cascadeRequestDigestAuthorization(
		response, request, w.platform.localID, w.platform.password,
	)
	if err != nil {
		w.logAsyncResponseError(request, fmt.Errorf("Digest challenge: %w", err))
		return
	}
	retry, err := buildCascadeRequestDigestRetry(request, authorizationHeader, authorization)
	if err != nil {
		w.logAsyncResponseError(request, err)
		return
	}
	if prepareRetry != nil {
		if err := prepareRetry(retry); err != nil {
			w.logAsyncResponseError(request, err)
			return
		}
	}
	retryTX, err := w.sendRequestTransactionContext(ctx, retry)
	if err != nil {
		w.logAsyncResponseError(request, fmt.Errorf("Digest retry: %w", err))
		return
	}
	if retryTX == nil {
		return
	}
	defer retryTX.Close()
	if _, err := w.waitAsyncFinalResponse(ctx, retry, retryTX); err != nil {
		w.logAsyncResponseError(request, fmt.Errorf("Digest retry response: %w", err))
	}
}

func (w *cascadeWorker) waitAsyncFinalResponse(ctx context.Context, request *sip.Request, tx *sip.Transaction) (*sip.Response, error) {
	if tx == nil {
		return nil, fmt.Errorf("SIP transaction is unavailable")
	}
	waitCtx, cancel := context.WithTimeout(ctx, defaultCascadeRequestTimeout)
	defer cancel()
	response, err := tx.GetResponseContext(waitCtx)
	if err != nil {
		if waitCtx.Err() != nil && ctx.Err() == nil && request != nil {
			w.invalidateTCPConnection(request.GetConnection())
		}
		return nil, err
	}
	if response == nil {
		return nil, fmt.Errorf("response is unavailable")
	}
	return response, nil
}

func (w *cascadeWorker) logAsyncResponseError(request *sip.Request, err error) {
	if err == nil || (w != nil && w.ctx != nil && w.ctx.Err() != nil) {
		return
	}
	method := "request"
	if request != nil && strings.TrimSpace(request.Method()) != "" {
		method = strings.ToUpper(strings.TrimSpace(request.Method()))
	}
	slog.Warn("cascade asynchronous SIP response failed", "method", method, "err", err)
}

func (w *cascadeWorker) sendRequestTransactionContext(ctx context.Context, request *sip.Request) (*sip.Transaction, error) {
	if ctx == nil {
		ctx = w.ctx
	}
	writeCtx, cancel := context.WithTimeout(ctx, defaultCascadeRequestTimeout)
	defer cancel()
	security, err := w.signalDigestSecurity()
	if err != nil {
		return nil, err
	}
	// 2xx ACK 必须复用原 INVITE 客户端事务，但仍属于需要 Date+Note 的出向 SIP 请求。
	// 在取走事务前签名，签名失败时保留事务供调用方重试。
	if request != nil && strings.EqualFold(strings.TrimSpace(request.Method()), sip.MethodACK) && security != nil {
		if err := signSIPMessageSafely(security, request); err != nil {
			return nil, err
		}
	}
	if tx := w.takeInviteTransaction(request); tx != nil {
		if err := tx.RequestContext(writeCtx, request); err != nil {
			tx.Close()
			w.invalidateTCPConnection(request.GetConnection())
			return nil, err
		}
		return nil, nil
	}
	if err := w.prepareRequestConnection(writeCtx, request, security); err != nil {
		return nil, err
	}
	tx, err := w.server.RequestWithSecurityContext(writeCtx, request, security)
	if err != nil {
		w.invalidateTCPConnection(request.GetConnection())
	}
	return tx, err
}

func cascadeInviteDialogKey(callID *sip.CallID, cseq *sip.CSeq, toTag, method string) string {
	if callID == nil || cseq == nil || normalizeCallID(callID) == "" || strings.TrimSpace(toTag) == "" ||
		!strings.EqualFold(strings.TrimSpace(cseq.MethodName), method) {
		return ""
	}
	return normalizeCallID(callID) + "\x00" + strconv.FormatUint(uint64(cseq.SeqNo), 10) + "\x00" + strings.TrimSpace(toTag)
}

func (w *cascadeWorker) rememberInviteTransaction(response *sip.Response, tx *sip.Transaction) {
	if w == nil || response == nil || tx == nil || response.StatusCode() < http.StatusOK || response.StatusCode() >= http.StatusMultipleChoices {
		return
	}
	callID, callIDOK := response.CallID()
	cseq, cseqOK := response.CSeq()
	if !callIDOK || !cseqOK {
		return
	}
	key := cascadeInviteDialogKey(callID, cseq, sipResponseToTag(response), sip.MethodInvite)
	if key == "" {
		return
	}
	w.inviteTxMu.Lock()
	if w.inviteTx == nil {
		w.inviteTx = make(map[string]*sip.Transaction)
	}
	previous := w.inviteTx[key]
	w.inviteTx[key] = tx
	w.inviteTxMu.Unlock()
	if previous != nil && previous != tx {
		previous.Close()
	}
}

func (w *cascadeWorker) takeInviteTransaction(request *sip.Request) *sip.Transaction {
	if w == nil || request == nil || !strings.EqualFold(strings.TrimSpace(request.Method()), sip.MethodACK) {
		return nil
	}
	callID, callIDOK := request.CallID()
	cseq, cseqOK := request.CSeq()
	if !callIDOK || !cseqOK {
		return nil
	}
	key := cascadeInviteDialogKey(callID, cseq, sipRequestToTag(request), sip.MethodACK)
	if key == "" {
		return nil
	}
	w.inviteTxMu.Lock()
	tx := w.inviteTx[key]
	delete(w.inviteTx, key)
	w.inviteTxMu.Unlock()
	return tx
}

func (w *cascadeWorker) discardInviteTransaction(response *sip.Response) {
	if w == nil || response == nil {
		return
	}
	callID, callIDOK := response.CallID()
	cseq, cseqOK := response.CSeq()
	if !callIDOK || !cseqOK {
		return
	}
	key := cascadeInviteDialogKey(callID, cseq, sipResponseToTag(response), sip.MethodInvite)
	if key == "" {
		return
	}
	w.inviteTxMu.Lock()
	tx := w.inviteTx[key]
	delete(w.inviteTx, key)
	w.inviteTxMu.Unlock()
	if tx != nil {
		tx.Close()
	}
}

func (w *cascadeWorker) closeInviteTransactions() {
	if w == nil {
		return
	}
	w.inviteTxMu.Lock()
	items := make([]*sip.Transaction, 0, len(w.inviteTx))
	for key, tx := range w.inviteTx {
		delete(w.inviteTx, key)
		if tx != nil {
			items = append(items, tx)
		}
	}
	w.inviteTxMu.Unlock()
	for _, tx := range items {
		tx.Close()
	}
}

func (w *cascadeWorker) signalDigestSecurity() (sip.MessageSecurity, error) {
	if w == nil || w.server == nil || w.server.gb == nil {
		return nil, nil
	}
	cfg := w.server.gb.configSnapshot()
	if cfg == nil {
		return nil, nil
	}
	return newSignalDigestSecurity(cfg, cascadeSignalDigestSeed(w.platform, cfg.SignalDigest.Seed))
}

func (w *cascadeWorker) prepareRequestConnection(ctx context.Context, request *sip.Request, security sip.MessageSecurity) error {
	if w == nil || w.server == nil || w.server.Server == nil || request == nil {
		return fmt.Errorf("SIP server is unavailable")
	}
	remote := request.Destination()
	if remote == nil {
		_, remote = w.remoteTarget()
		request.SetDestination(remote)
	}
	transport := cascadeTransportForAddr(remote)
	wireLength, err := signedSIPRequestLength(request, security)
	if err != nil {
		return err
	}
	if transport == "udp" && wireLength > cascadeReliableTransportThreshold {
		reliable, err := cascadeTCPDestination(remote)
		if err != nil {
			return fmt.Errorf("resolve oversized cascade SIP/TCP target: %w", err)
		}
		remote = reliable
		transport = "tcp"
		request.SetDestination(remote)
		applyCascadeRequestNextHopTransport(request, transport)
	}
	if transport == "tls" && !w.platform.localTLSEnabled {
		return fmt.Errorf("cascade SIP/TLS requires the local SIP-TLS listener")
	}
	if transport == "udp" {
		if w.server.UDPConn() == nil {
			return fmt.Errorf("SIP UDP listener is unavailable")
		}
		request.SetConnection(w.server.UDPConn())
		request.SetSource(w.server.UDPConn().LocalAddr())
		applyCascadeRequestTransport(request, "udp")
		return nil
	}
	conn, err := w.ensureTCPConnection(ctx, remote)
	if err != nil {
		return err
	}
	request.SetConnection(conn)
	request.SetSource(conn.LocalAddr())
	applyCascadeRequestTransport(request, transport)
	return nil
}

func cascadeTCPDestination(remote net.Addr) (*net.TCPAddr, error) {
	if remote == nil {
		return nil, fmt.Errorf("SIP destination is unavailable")
	}
	if udp, ok := remote.(*net.UDPAddr); ok && udp != nil {
		return &net.TCPAddr{IP: append(net.IP(nil), udp.IP...), Port: udp.Port, Zone: udp.Zone}, nil
	}
	return net.ResolveTCPAddr("tcp", remote.String())
}

func (w *cascadeWorker) ensureTCPConnection(ctx context.Context, remote net.Addr) (sip.Connection, error) {
	transport := cascadeTransportForAddr(remote)
	if remote == nil || transport == "udp" {
		return nil, fmt.Errorf("invalid cascade SIP/TCP or SIP/TLS target")
	}
	address := remote.String()
	connectionKey := transport + "|" + address
	w.connMu.Lock()
	if w.tcpConn != nil && w.tcpRemote == connectionKey {
		conn := w.tcpConn
		w.connMu.Unlock()
		return conn, nil
	}
	previous := w.tcpConn
	w.tcpConn = nil
	w.tcpRemote = ""
	dial := w.dialTCP
	dialTLS := w.dialTLS
	w.connMu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	var raw net.Conn
	var err error
	if transport == "tls" {
		if dialTLS == nil {
			return nil, fmt.Errorf("cascade SIP/TLS dialer is unavailable")
		}
		serverName := ""
		if target, ok := remote.(*cascadeTLSAddr); ok && target != nil {
			serverName = target.serverName
		}
		raw, err = dialTLS(ctx, address, serverName)
	} else {
		if dial == nil {
			return nil, fmt.Errorf("cascade SIP/TCP dialer is unavailable")
		}
		raw, err = dial(ctx, address)
	}
	if err != nil {
		return nil, fmt.Errorf("dial cascade SIP/%s %s: %w", strings.ToUpper(transport), address, err)
	}
	var conn sip.Connection
	if transport == "tls" {
		conn = sip.NewTLSConnection(raw)
	} else {
		conn = sip.NewTCPConnection(raw)
	}
	w.connMu.Lock()
	if w.tcpConn != nil && w.tcpRemote == connectionKey {
		existing := w.tcpConn
		w.connMu.Unlock()
		_ = conn.Close()
		return existing, nil
	}
	existing := w.tcpConn
	w.tcpConn = conn
	w.tcpRemote = connectionKey
	w.connMu.Unlock()
	if existing != nil {
		_ = existing.Close()
	}
	go func() {
		w.server.Server.ProcessTCPConnection(conn)
		w.connMu.Lock()
		if w.tcpConn == conn {
			w.tcpConn = nil
			w.tcpRemote = ""
		}
		w.connMu.Unlock()
	}()
	return conn, nil
}

func (w *cascadeWorker) invalidateTCPConnection(conn sip.Connection) {
	if conn == nil || conn.Network() != "tcp" {
		return
	}
	w.connMu.Lock()
	if w.tcpConn != conn {
		w.connMu.Unlock()
		return
	}
	w.tcpConn = nil
	w.tcpRemote = ""
	w.connMu.Unlock()
	_ = conn.Close()
}

func (w *cascadeWorker) closeTCPConnection() {
	if w == nil {
		return
	}
	w.connMu.Lock()
	conn := w.tcpConn
	w.tcpConn = nil
	w.tcpRemote = ""
	w.connMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// CascadeManager 管理多个上级平台的注册工作协程。
type CascadeManager struct {
	server *Server
	opMu   sync.Mutex
	closed bool
	mu     sync.RWMutex
	items  map[string]*cascadeWorker
}

func NewCascadeManager(server *Server) *CascadeManager {
	return &CascadeManager{server: server, items: make(map[string]*cascadeWorker)}
}

func normalizeCascadePlatforms(local conf.SIP, configs []conf.SIPUpstream, fallbackHost string) ([]cascadePlatform, error) {
	platforms := make([]cascadePlatform, 0, len(configs))
	names := make(map[string]struct{}, len(configs))
	peers := make(map[string]struct{}, len(configs))
	for _, item := range configs {
		if !item.Enabled {
			continue
		}
		platform, err := normalizeCascadePlatform(item, local, fallbackHost)
		if err != nil {
			return nil, fmt.Errorf("upstream %q: %w", item.Name, err)
		}
		if _, exists := names[platform.name]; exists {
			return nil, fmt.Errorf("duplicate upstream name: %s", platform.name)
		}
		peerKey := platform.serverID + "@" + platform.remote.String()
		if _, exists := peers[peerKey]; exists {
			return nil, fmt.Errorf("duplicate upstream platform endpoint: %s", peerKey)
		}
		names[platform.name] = struct{}{}
		peers[peerKey] = struct{}{}
		platforms = append(platforms, platform)
	}
	return platforms, nil
}

func (m *CascadeManager) Apply(local conf.SIP, configs []conf.SIPUpstream) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.closed {
		return ErrServiceStopped
	}

	fallbackHost := ""
	if m.server != nil && m.server.fromAddress.URI != nil {
		fallbackHost = m.server.fromAddress.URI.Host()
	}
	platforms, err := normalizeCascadePlatforms(local, configs, fallbackHost)
	if err != nil {
		return err
	}

	m.mu.Lock()
	previous := m.items
	m.items = make(map[string]*cascadeWorker, len(platforms))
	for _, platform := range platforms {
		m.items[platform.name] = newCascadeWorker(m.server, platform)
	}
	current := make([]*cascadeWorker, 0, len(m.items))
	for _, worker := range m.items {
		current = append(current, worker)
	}
	m.mu.Unlock()

	for _, worker := range previous {
		worker.stopOperations()
	}
	for _, worker := range previous {
		if m.server != nil && m.server.gb != nil {
			m.server.gb.removeCascadeAlarmDispatches(worker)
			m.server.gb.removeCascadeEventSubscriptionsContext(m.server.gb.serviceContext(), worker)
			m.server.gb.removeCascadeTaskRoutes(worker)
			m.server.gb.removeCascadeMobilePositionQueries(worker)
			m.server.gb.removeCascadeMediaSessions(worker)
			m.server.gb.removeCascadeVoiceSessions(worker)
		}
		worker.stop()
	}
	for _, worker := range current {
		worker.start()
	}
	return nil
}

func (m *CascadeManager) Close() {
	if m == nil {
		return
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.closed {
		return
	}
	m.closed = true

	m.mu.Lock()
	items := m.items
	m.items = make(map[string]*cascadeWorker)
	m.mu.Unlock()
	for _, worker := range items {
		worker.stopOperations()
	}
	for _, worker := range items {
		if m.server != nil && m.server.gb != nil {
			m.server.gb.removeCascadeAlarmDispatches(worker)
			m.server.gb.removeCascadeEventSubscriptionsContext(m.server.gb.serviceContext(), worker)
			m.server.gb.removeCascadeTaskRoutes(worker)
			m.server.gb.removeCascadeMobilePositionQueries(worker)
			m.server.gb.removeCascadeMediaSessions(worker)
			m.server.gb.removeCascadeVoiceSessions(worker)
		}
		worker.stop()
	}
}

func (m *CascadeManager) Statuses() []CascadePlatformStatus {
	if m == nil {
		return []CascadePlatformStatus{}
	}
	m.mu.RLock()
	items := make([]*cascadeWorker, 0, len(m.items))
	for _, worker := range m.items {
		items = append(items, worker)
	}
	m.mu.RUnlock()
	out := make([]CascadePlatformStatus, 0, len(items))
	now := time.Now()
	for _, worker := range items {
		state := worker.snapshot()
		if state.Registered && !cascadeRegistrationActive(state, now) {
			state.Registered = false
			state.State = "expired"
		}
		out = append(out, state)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *CascadeManager) registeredWorkers(minimum GBProtocolVersion) []*cascadeWorker {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	items := make([]*cascadeWorker, 0, len(m.items))
	now := time.Now()
	for _, worker := range m.items {
		if worker != nil && worker.registrationActive(now) && worker.protocolVersion().AtLeast(minimum) {
			items = append(items, worker)
		}
	}
	m.mu.RUnlock()
	return items
}

// configuredWorkers 返回所有当前配置的上级，不要求已经完成 REGISTER。
// 持久化业务 outbox 需要记录暂时离线的目标，待其重新注册后再投递。
func (m *CascadeManager) configuredWorkers(minimum GBProtocolVersion) []*cascadeWorker {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	items := make([]*cascadeWorker, 0, len(m.items))
	for _, worker := range m.items {
		if worker != nil && worker.protocolVersion().AtLeast(minimum) {
			items = append(items, worker)
		}
	}
	m.mu.RUnlock()
	return items
}

func (m *CascadeManager) workerByName(name string) (*cascadeWorker, bool) {
	if m == nil {
		return nil, false
	}
	name = strings.TrimSpace(name)
	m.mu.RLock()
	worker, ok := m.items[name]
	m.mu.RUnlock()
	return worker, ok && worker != nil
}

func (g *GB28181API) cascadeWorkerAvailable(worker *cascadeWorker) bool {
	if worker == nil {
		return false
	}
	operationCtx := worker.operationContext()
	if operationCtx != nil {
		select {
		case <-operationCtx.Done():
			return false
		default:
		}
	}
	if g == nil || g.svr == nil || g.svr.cascade == nil {
		return true
	}
	current, ok := g.svr.cascade.workerByName(worker.platform.name)
	return ok && current == worker
}

func (g *GB28181API) startCascadeLifecycleTask(parent context.Context, worker *cascadeWorker, task func(context.Context)) bool {
	if g == nil || worker == nil || task == nil || !g.cascadeWorkerAvailable(worker) {
		return false
	}
	return g.startLifecycleTask(parent, func(taskCtx context.Context) {
		operationCtx, cancel := withCascadeWorkerOperation(taskCtx, worker)
		defer cancel()
		if !g.cascadeWorkerAvailable(worker) {
			return
		}
		task(operationCtx)
	})
}

func (m *CascadeManager) matchRegistered(serverID string, source net.Addr, connections ...sip.Connection) (*cascadeWorker, bool) {
	if m == nil || strings.TrimSpace(serverID) == "" || source == nil {
		return nil, false
	}
	sourceIP := parseAddressIP(source.String())
	if sourceIP == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, worker := range m.items {
		if worker.platform.serverID != serverID || !worker.remoteAddressMatches(source) {
			continue
		}
		if cascadeTransportForAddr(worker.remoteDestination()) == "tls" {
			var connection sip.Connection
			if len(connections) > 0 {
				connection = connections[0]
			}
			worker.connMu.Lock()
			trustedConnection := worker.tcpConn
			worker.connMu.Unlock()
			if connection == nil || trustedConnection == nil || connection != trustedConnection {
				return nil, false
			}
		}
		if !worker.registrationActive(time.Now()) {
			return nil, false
		}
		return worker, true
	}
	return nil, false
}
