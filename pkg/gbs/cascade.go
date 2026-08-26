package gbs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/xml"
	"errors"
	"fmt"
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
	defaultCascadeRegisterExpires   = 3600
	defaultCascadeKeepaliveInterval = 60 * time.Second
	defaultCascadeRequestTimeout    = 8 * time.Second
	maxCascadeRegisterRedirects     = 3
)

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
		expires = defaultCascadeRegisterExpires
	}
	if expires < 60 || expires > 86400 {
		return cascadePlatform{}, fmt.Errorf("upstream expires must be between 60 and 86400 seconds")
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
		password: in.Password, signalDigestSeed: strings.TrimSpace(in.SignalDigestSeed),
		version: version, expires: expires, keepaliveInterval: keepalive,
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

	mu        sync.RWMutex
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
}

func newCascadeWorker(server *Server, platform cascadePlatform) *cascadeWorker {
	ctx, cancel := context.WithCancel(context.Background())
	scheme := "sip"
	if platform.transport == "tls" {
		scheme = "sips"
	}
	remoteURI, _ := sip.ParseSipURI(fmt.Sprintf("%s:%s@%s", scheme, platform.serverID, platform.remoteDomain))
	setCascadeURITransport(&remoteURI, platform.transport)
	w := &cascadeWorker{
		server: server, platform: platform, ctx: ctx, cancel: cancel, done: make(chan struct{}),
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
	w.cancel()
	<-w.done
}

func (w *cascadeWorker) snapshot() CascadePlatformStatus {
	w.mu.RLock()
	state := w.status
	w.mu.RUnlock()
	return state
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
	if !w.snapshot().Registered {
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
	targetURI, target := w.remoteTarget()
	request := w.newInitialRegisterRequest(expires)
	applyCascadeRequestTarget(request, targetURI, target)
	var response *sip.Response
	redirects := 0
	authAttempts := 0
	certificateChallengeCompleted := false
	for {
		var err error
		response, err = w.exchange(ctx, request)
		if err != nil {
			return fmt.Errorf("cascade REGISTER: %w", err)
		}
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
			authAttempts = 0
			certificateChallengeCompleted = false
			request = w.newInitialRegisterRequest(expires)
			applyCascadeRequestTarget(request, targetURI, target)
			continue
		case http.StatusUnauthorized:
			if authAttempts > 0 {
				return fmt.Errorf("cascade REGISTER authentication failed after challenge")
			}
			scheme, challenge, err := cascadeRegisterChallenge(response, w.platform.registerCertificateAuth != nil)
			if err != nil {
				return err
			}
			var authorization string
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
			authAttempts++
			request = w.newRegisterRequestWithAuthorization(expires, authorization)
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
	effective := negotiateCascadeVersion(w.platform.version, response)
	accepted, err := cascadeAcceptedExpires(response, expires)
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
	if expires == 0 {
		state.State = "stopped"
		state.Registered = false
		state.ExpiresAt = time.Time{}
		w.mu.Unlock()
		return nil
	}
	state.State = "registered"
	state.Registered = true
	state.ExpiresAt = now.Add(time.Duration(accepted) * time.Second)
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
	response, err := w.exchange(ctx, request)
	if err != nil {
		return fmt.Errorf("cascade Keepalive: %w", err)
	}
	if response.StatusCode() != http.StatusOK {
		return fmt.Errorf("cascade Keepalive failed: %d %s", response.StatusCode(), response.Reason())
	}
	w.updateStatus(func(state *CascadePlatformStatus) {
		state.State = "registered"
		state.Registered = true
		state.LastKeepaliveAt = time.Now()
		state.LastError = ""
	})
	return nil
}

func (w *cascadeWorker) newRegisterRequest(expires int, auth *sip.Authorization) *sip.Request {
	w.cseq++
	return w.newRequest(sip.MethodRegister, nil, nil, &w.callID, w.cseq, expires, auth)
}

func (w *cascadeWorker) newRegisterRequestWithAuthorization(expires int, authorization string) *sip.Request {
	request := w.newRegisterRequest(expires, nil)
	if authorization != "" {
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Authorization", Contents: authorization})
	}
	return request
}

func (w *cascadeWorker) newInitialRegisterRequest(expires int) *sip.Request {
	if w.platform.registerCertificateAuth == nil {
		return w.newRegisterRequest(expires, nil)
	}
	return w.newRegisterRequestWithAuthorization(expires, w.platform.registerCertificateAuth.capabilityAuthorization())
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
	request := w.newKeepaliveRequest(body)
	if err := w.platform.monitorUserIdentity.apply(ctx, request); err != nil {
		return fmt.Errorf("cascade MESSAGE Monitor-User-Identity: %w", err)
	}
	response, err := w.exchange(ctx, request)
	if err != nil {
		return fmt.Errorf("cascade MESSAGE: %w", err)
	}
	if response.StatusCode() != http.StatusOK {
		return fmt.Errorf("cascade MESSAGE failed: %d %s", response.StatusCode(), response.Reason())
	}
	return nil
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
		AddVia(&sip.ViaHop{Host: w.platform.localHost, Port: sip.NewPort(localPort), Transport: strings.ToUpper(transport), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).
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
	if uri.FPassword != nil && strings.TrimSpace(uri.FPassword.String()) != "" {
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
	headers := response.GetHeaders("WWW-Authenticate")
	if len(headers) == 0 {
		return nil, fmt.Errorf("cascade REGISTER 401 missing WWW-Authenticate")
	}
	auth := sip.AuthFromValue(headers[0].String())
	return cascadeDigestAuthorizationFromChallenge(auth, request, username, password)
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

func negotiateCascadeVersion(configured GBProtocolVersion, response *sip.Response) GBProtocolVersion {
	for _, header := range response.GetHeaders("X-GB-Ver") {
		value := header.String()
		if _, after, ok := strings.Cut(value, ":"); ok {
			value = after
		}
		if remote, ok := ParseGBProtocolVersion(strings.TrimSpace(value)); ok && remote.rank() < configured.rank() {
			return remote
		}
	}
	return configured
}

func cascadeAcceptedExpires(response *sip.Response, requested int) (int, error) {
	if requested == 0 {
		return 0, nil
	}
	value := ""
	if headers := response.GetHeaders("Expires"); len(headers) > 0 {
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
	accepted, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || accepted <= 0 {
		return 0, fmt.Errorf("cascade REGISTER invalid response expires: %s", strings.TrimSpace(value))
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
	if err := w.prepareRequestConnection(ctx, request); err != nil {
		return nil, err
	}
	security, err := w.signalDigestSecurity()
	if err != nil {
		return nil, err
	}
	tx, err := w.server.RequestWithSecurity(request, security)
	if err != nil {
		w.invalidateTCPConnection(request.GetConnection())
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, defaultCascadeRequestTimeout)
	defer cancel()
	response, err := tx.GetResponseContext(waitCtx)
	if err != nil {
		_, _ = tx.CancelInvite()
		tx.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("response timeout")
	}
	if response == nil {
		_, cancelErr := tx.CancelInvite()
		tx.Close()
		return nil, errors.Join(fmt.Errorf("response timeout"), cancelErr)
	}
	return response, nil
}

func (w *cascadeWorker) sendRequest(request *sip.Request) error {
	if err := w.prepareRequestConnection(w.ctx, request); err != nil {
		return err
	}
	security, err := w.signalDigestSecurity()
	if err != nil {
		return err
	}
	_, err = w.server.RequestWithSecurity(request, security)
	if err != nil {
		w.invalidateTCPConnection(request.GetConnection())
	}
	return err
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

func (w *cascadeWorker) prepareRequestConnection(ctx context.Context, request *sip.Request) error {
	if w == nil || w.server == nil || w.server.Server == nil || request == nil {
		return fmt.Errorf("SIP server is unavailable")
	}
	remote := request.Destination()
	if remote == nil {
		_, remote = w.remoteTarget()
		request.SetDestination(remote)
	}
	transport := cascadeTransportForAddr(remote)
	if transport == "tls" && !w.platform.localTLSEnabled {
		return fmt.Errorf("cascade SIP/TLS requires the local SIP-TLS listener")
	}
	if transport == "udp" {
		w.closeTCPConnection()
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
	conn := sip.NewTCPConnection(raw)
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
		if m.server != nil && m.server.gb != nil {
			m.server.gb.removeCascadeEventSubscriptions(worker)
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

	m.mu.Lock()
	items := m.items
	m.items = make(map[string]*cascadeWorker)
	m.mu.Unlock()
	for _, worker := range items {
		if m.server != nil && m.server.gb != nil {
			m.server.gb.removeCascadeEventSubscriptions(worker)
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
	for _, worker := range items {
		out = append(out, worker.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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
		if !worker.snapshot().Registered {
			return nil, false
		}
		return worker, true
	}
	return nil, false
}
