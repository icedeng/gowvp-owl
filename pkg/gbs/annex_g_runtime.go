package gbs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/annexg"
	"github.com/gowvp/owl/pkg/gbs/annexg/gormstore"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"golang.org/x/time/rate"
	"gorm.io/gorm"
)

const (
	annexGSystemContextKey       = "gb28181.annex_g.system"
	defaultAnnexGPendingTTL      = 24 * time.Hour
	defaultAnnexGMaxPending      = 4096
	defaultAnnexGInboundRate     = 50
	defaultAnnexGInboundBurst    = 100
	annexGPendingCleanupInterval = time.Hour
	annexGOutboundRetryInterval  = 5 * time.Second
	annexGOutboundRetryMax       = time.Minute
	annexGOutboundRetryBatch     = 8
	annexGStoredResponseBatch    = 8
)

type annexGSystem struct {
	id                     string
	role                   annexg.SystemRole
	version                annexg.Version
	profileFingerprint     string
	password               string
	signalDigestSeed       string
	realm                  string
	transport              string
	targetURI              *sip.URI
	target                 net.Addr
	tlsConfig              *tls.Config
	sourceNetworks         []*net.IPNet
	allowInsecureTransport bool
	inboundLimiter         *rate.Limiter
	securityMu             sync.RWMutex
	signalSecurity         sip.MessageSecurity

	connMu  sync.Mutex
	conn    sip.Connection
	connKey string
	closed  bool
	dialTCP func(context.Context, string) (net.Conn, error)
	dialTLS func(context.Context, string, *tls.Config) (net.Conn, error)
}

type annexGResponseSender func(*sip.Context, *annexGSystem, annexg.Version, annexg.Message) error

type annexGPendingKey struct {
	systemID string
	command  annexg.Command
	sn       int
}

type annexGPendingResult struct {
	message annexg.Message
	err     error
}

type annexGPendingExchange struct {
	request   annexg.Message
	response  annexg.Message
	result    chan annexGPendingResult
	expiresAt time.Time
	delivered bool
	needsSend bool
	sending   bool
	attempts  int
	nextSend  time.Time
}

type annexGService struct {
	localID  string
	realm    string
	systems  map[string]*annexGSystem
	store    *gormstore.Store
	adapter  annexg.Adapter
	send     annexGResponseSender
	outbound func(context.Context, *sip.Server, *annexGSystem, *sip.Request) error

	pendingMu     sync.Mutex
	pending       map[annexGPendingKey]*annexGPendingExchange
	pendingTTL    time.Duration
	maxPending    int
	closed        bool
	closeOnce     sync.Once
	cleanerCancel context.CancelFunc
	cleanerWG     sync.WaitGroup
	server        *Server
	retryWake     chan struct{}
}

func newAnnexGService(cfg conf.SIP, db *gorm.DB) (*annexGService, error) {
	return newAnnexGServiceContext(context.Background(), cfg, db)
}

func newAnnexGServiceContext(ctx context.Context, cfg conf.SIP, db *gorm.DB) (*annexGService, error) {
	if !cfg.AnnexG.Enabled {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := conf.ValidateSIPAnnexGConfig(cfg.AnnexG, cfg.ID, cfg.EnableTLS); err != nil {
		return nil, err
	}
	pendingTTL := cfg.AnnexG.PendingTTL.Duration()
	if pendingTTL <= 0 {
		pendingTTL = defaultAnnexGPendingTTL
	}
	maxPending := cfg.AnnexG.MaxPending
	if maxPending <= 0 {
		maxPending = defaultAnnexGMaxPending
	}
	inboundRate := cfg.AnnexG.InboundRate
	if inboundRate <= 0 {
		inboundRate = defaultAnnexGInboundRate
	}
	inboundBurst := cfg.AnnexG.InboundBurst
	if inboundBurst <= 0 {
		inboundBurst = defaultAnnexGInboundBurst
	}
	store, err := gormstore.New(db, gormstore.Options{
		MaxSendRecords: cfg.AnnexG.MaxSendRecords,
		PendingTTL:     pendingTTL,
		MaxPending:     maxPending,
	})
	if err != nil {
		return nil, err
	}
	migrateCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := store.Migrate(migrateCtx); err != nil {
		return nil, fmt.Errorf("migrate Annex G store: %w", err)
	}

	service := &annexGService{
		localID:    strings.TrimSpace(cfg.ID),
		realm:      cfg.GetDomain(),
		systems:    make(map[string]*annexGSystem, len(cfg.AnnexG.Systems)),
		store:      store,
		pending:    make(map[annexGPendingKey]*annexGPendingExchange),
		pendingTTL: pendingTTL,
		maxPending: maxPending,
		retryWake:  make(chan struct{}, 1),
		outbound:   sendAnnexGSIPRequest,
	}
	localID := strings.TrimSpace(cfg.ID)
	for _, configured := range cfg.AnnexG.Systems {
		system, err := buildAnnexGSystem(configured, localID, cfg.SignalDigest.Seed)
		if err != nil {
			return nil, err
		}
		security, err := newSignalDigestSecurity(&cfg, annexGSignalDigestSeed(
			configured.SignalDigestSeed, configured.Password, cfg.SignalDigest.Seed,
		))
		if err != nil {
			return nil, fmt.Errorf("initialize Annex G signal Digest for system %s: %w", system.id, err)
		}
		system.setSignalSecurity(security)
		system.inboundLimiter = rate.NewLimiter(rate.Limit(inboundRate), inboundBurst)
		service.systems[system.id] = system
	}
	consumer := annexg.DomainConsumer{
		MPAlarmSink: store, MPAlarmQuerier: store,
		ECSAlarmSink: store, ECSAlarmQuerier: store,
		TGSAlarmSink: store, TGSAlarmQuerier: store,
		Defence: store,
	}
	service.adapter = annexg.Adapter{Authorizer: service, Consumer: consumer}
	if err := service.restorePending(migrateCtx); err != nil {
		return nil, fmt.Errorf("restore Annex G pending exchanges: %w", err)
	}
	service.startPendingCleaner()
	return service, nil
}

func (service *annexGService) restorePending(ctx context.Context) error {
	if service == nil || service.store == nil {
		return errors.New("Annex G store is unavailable")
	}
	items, err := service.store.LoadPendingExchanges(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		system := service.systems[strings.TrimSpace(item.SystemID)]
		key, err := annexGPendingKeyFor(item.SystemID, item.Request)
		if err != nil {
			return err
		}
		if system == nil || system.version != item.Version || item.ProfileFingerprint == "" || item.ProfileFingerprint != system.profileFingerprint {
			if err := service.store.DeletePendingExchange(ctx, key.systemID, key.command, key.sn); err != nil {
				return fmt.Errorf("discard stale Annex G pending exchange %s/%d for system %s: %w", key.command, key.sn, key.systemID, err)
			}
			slog.Warn("discard stale Annex G pending exchange for changed or unavailable system profile", "system_id", item.SystemID, "version", item.Version)
			continue
		}
		if len(service.pending) >= service.maxPending {
			return fmt.Errorf("Annex G restored pending exchange limit %d exceeded", service.maxPending)
		}
		if _, exists := service.pending[key]; exists {
			return fmt.Errorf("duplicate stored Annex G pending exchange %s/%d for system %s", key.command, key.sn, key.systemID)
		}
		service.pending[key] = &annexGPendingExchange{
			request: item.Request, response: item.Response,
			result: make(chan annexGPendingResult, 1), expiresAt: item.ExpiresAt,
			needsSend: item.Response == nil, nextSend: time.Now(),
		}
	}
	service.retryStoredResponses(ctx)
	return nil
}

func (service *annexGService) startPendingCleaner() {
	if service == nil || service.store == nil {
		return
	}
	interval := annexGPendingCleanupInterval
	if halfTTL := service.pendingTTL / 2; halfTTL > 0 && halfTTL < interval {
		interval = halfTTL
	}
	if interval < time.Minute {
		interval = time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	service.cleanerCancel = cancel
	service.cleanerWG.Add(1)
	go func() {
		defer service.cleanerWG.Done()
		ticker := time.NewTicker(interval)
		retryTicker := time.NewTicker(annexGOutboundRetryInterval)
		defer ticker.Stop()
		defer retryTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-service.retryWake:
				service.retryPendingRequests(ctx, time.Now())
			case now := <-retryTicker.C:
				service.retryPendingRequests(ctx, now)
				service.retryStoredResponses(ctx)
			case now := <-ticker.C:
				service.retryPendingRequests(ctx, now)
				service.retryStoredResponses(ctx)
				service.cleanupExpiredPending(ctx, now)
			}
		}
	}()
}

func (service *annexGService) bindServer(server *Server) {
	if service == nil || server == nil {
		return
	}
	service.pendingMu.Lock()
	if !service.closed {
		service.server = server
	}
	service.pendingMu.Unlock()
	service.wakePendingRetry()
}

func (service *annexGService) wakePendingRetry() {
	if service == nil || service.retryWake == nil {
		return
	}
	select {
	case service.retryWake <- struct{}{}:
	default:
	}
}

func annexGOutboundRetryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := annexGOutboundRetryInterval
	for index := 1; index < attempts && delay < annexGOutboundRetryMax; index++ {
		delay *= 2
		if delay > annexGOutboundRetryMax {
			delay = annexGOutboundRetryMax
		}
	}
	return delay
}

func (service *annexGService) finishPendingSend(key annexGPendingKey, pending *annexGPendingExchange, sendErr error, now time.Time) {
	if service == nil || pending == nil {
		return
	}
	service.pendingMu.Lock()
	if service.pending[key] == pending {
		pending.sending = false
		if sendErr == nil {
			pending.needsSend = false
			pending.nextSend = time.Time{}
		} else if !pending.delivered && pending.response == nil {
			pending.needsSend = true
			pending.attempts++
			pending.nextSend = now.Add(annexGOutboundRetryDelay(pending.attempts))
		}
	}
	service.pendingMu.Unlock()
}

// retryPendingRequests 重放已持久化但尚未取得 SIP 成功确认的出向业务请求。
// 相同系统、命令、SN 和 XML 保持不变，由外部系统据此幂等去重。
func (service *annexGService) retryPendingRequests(ctx context.Context, now time.Time) {
	if service == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	type retry struct {
		key     annexGPendingKey
		pending *annexGPendingExchange
		system  *annexGSystem
		server  *Server
	}
	retries := make([]retry, 0)
	service.pendingMu.Lock()
	server := service.server
	if !service.closed && server != nil && server.Server != nil {
		for key, pending := range service.pending {
			if pending == nil || !pending.needsSend || pending.sending || pending.delivered || pending.response != nil ||
				!pending.expiresAt.After(now) || !pending.nextSend.IsZero() && pending.nextSend.After(now) {
				continue
			}
			system := service.systems[key.systemID]
			if system == nil {
				continue
			}
			retries = append(retries, retry{key: key, pending: pending, system: system, server: server})
		}
		sort.Slice(retries, func(left, right int) bool {
			leftPending, rightPending := retries[left].pending, retries[right].pending
			if !leftPending.nextSend.Equal(rightPending.nextSend) {
				return leftPending.nextSend.Before(rightPending.nextSend)
			}
			if retries[left].key.systemID != retries[right].key.systemID {
				return retries[left].key.systemID < retries[right].key.systemID
			}
			if retries[left].key.command != retries[right].key.command {
				return retries[left].key.command < retries[right].key.command
			}
			return retries[left].key.sn < retries[right].key.sn
		})
		if len(retries) > annexGOutboundRetryBatch {
			retries = retries[:annexGOutboundRetryBatch]
		}
		for _, item := range retries {
			item.pending.sending = true
		}
	}
	service.pendingMu.Unlock()

	for _, item := range retries {
		body, err := annexg.Encode(item.system.version, item.pending.request)
		if err == nil {
			var request *sip.Request
			request, err = buildAnnexGOutboundRequest(item.server, service, item.system, body)
			if err == nil {
				err = item.system.prepareRequestConnection(ctx, item.server, request)
			}
			if err == nil {
				if service.outbound == nil {
					err = errors.New("Annex G outbound SIP sender is unavailable")
				} else {
					err = service.outbound(ctx, item.server.Server, item.system, request)
				}
			}
		}
		service.finishPendingSend(item.key, item.pending, err, time.Now())
		if err != nil && ctx.Err() == nil {
			slog.Warn("retry Annex G outbound request failed", "system_id", item.key.systemID, "cmd_type", item.key.command, "sn", item.key.sn, "err", err)
		}
	}
}

func (service *annexGService) retryStoredResponses(ctx context.Context) {
	if service == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	type retry struct {
		key     annexGPendingKey
		system  *annexGSystem
		pending *annexGPendingExchange
		message annexg.Message
	}
	retries := make([]retry, 0)
	service.pendingMu.Lock()
	for key, pending := range service.pending {
		if pending == nil || pending.response == nil || pending.delivered {
			continue
		}
		system := service.systems[key.systemID]
		if system == nil {
			continue
		}
		retries = append(retries, retry{key: key, system: system, pending: pending, message: pending.response})
	}
	sort.Slice(retries, func(left, right int) bool {
		if retries[left].key.systemID != retries[right].key.systemID {
			return retries[left].key.systemID < retries[right].key.systemID
		}
		if retries[left].key.command != retries[right].key.command {
			return retries[left].key.command < retries[right].key.command
		}
		return retries[left].key.sn < retries[right].key.sn
	})
	if len(retries) > annexGStoredResponseBatch {
		retries = retries[:annexGStoredResponseBatch]
	}
	for _, item := range retries {
		item.pending.delivered = true
	}
	service.pendingMu.Unlock()
	for _, item := range retries {
		prepared := annexg.Exchange{
			SourceID: item.system.id, SourceRole: item.system.role,
			DestinationID: service.localID, DestinationRole: annexg.RoleManagementPlatform,
			Message: item.message,
		}
		if err := service.consumeClaimedPendingResponse(ctx, item.system, item.pending, prepared); err != nil && ctx.Err() == nil {
			slog.Error("replay stored Annex G business response failed", "system_id", item.system.id, "cmd_type", item.message.CommandType(), "err", err)
		}
	}
}

func (service *annexGService) cleanupExpiredPending(ctx context.Context, now time.Time) {
	if service == nil {
		return
	}
	expired := make([]*annexGPendingExchange, 0)
	service.pendingMu.Lock()
	for key, pending := range service.pending {
		if pending != nil && !pending.expiresAt.IsZero() && !pending.expiresAt.After(now) {
			expired = append(expired, pending)
			delete(service.pending, key)
		}
	}
	service.pendingMu.Unlock()
	for _, pending := range expired {
		select {
		case pending.result <- annexGPendingResult{err: context.DeadlineExceeded}:
		default:
		}
	}
	if service.store != nil {
		if _, err := service.store.DeleteExpiredPendingExchanges(ctx, now); err != nil && ctx.Err() == nil {
			slog.Error("cleanup expired Annex G pending exchanges failed", "err", err)
		}
	}
}

func (service *annexGService) pendingCount() int {
	if service == nil {
		return 0
	}
	service.pendingMu.Lock()
	defer service.pendingMu.Unlock()
	return len(service.pending)
}

func buildAnnexGSystem(configured conf.SIPAnnexGSystem, buildContext ...string) (*annexGSystem, error) {
	var localID, globalSignalDigestSeed string
	if len(buildContext) > 0 {
		localID = buildContext[0]
	}
	if len(buildContext) > 1 {
		globalSignalDigestSeed = buildContext[1]
	}
	if len(buildContext) > 2 {
		return nil, errors.New("at most two Annex G system context values are allowed")
	}
	id := strings.TrimSpace(configured.ID)
	if len(id) < 10 {
		return nil, errors.New("Annex G system ID must contain at least 10 characters")
	}
	version, ok := annexg.ParseVersion(configured.Version)
	if !ok || version == annexg.Version2022 {
		return nil, fmt.Errorf("unsupported Annex G version %q", configured.Version)
	}
	role := annexg.SystemRole(strings.TrimSpace(configured.Role))
	realm := strings.TrimSpace(configured.Realm)
	if realm == "" {
		realm = id[:10]
	}
	transport := strings.ToLower(strings.TrimSpace(configured.Transport))
	if transport == "" {
		transport = "tls"
	}
	targetURI, target, host, err := buildAnnexGTarget(id, configured.Address, transport)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := annexGTLSClientConfig(configured, host)
	if err != nil {
		return nil, err
	}
	effectiveSignalDigestSeed := annexGSignalDigestSeed(
		configured.SignalDigestSeed, configured.Password, globalSignalDigestSeed,
	)
	system := &annexGSystem{
		id: id, role: role, version: version,
		profileFingerprint: annexGSystemProfileFingerprint(configured, localID, globalSignalDigestSeed, id, version, realm, transport, effectiveSignalDigestSeed),
		password:           configured.Password, signalDigestSeed: configured.SignalDigestSeed,
		realm: realm, transport: transport,
		targetURI: targetURI, target: target, tlsConfig: tlsConfig,
		allowInsecureTransport: configured.AllowInsecureTransport,
	}
	system.dialTCP = func(ctx context.Context, address string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	}
	system.dialTLS = func(ctx context.Context, address string, config *tls.Config) (net.Conn, error) {
		dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: config}
		return dialer.DialContext(ctx, "tcp", address)
	}
	for _, source := range configured.SourceCIDRs {
		network, err := parseAnnexGSourceNetwork(source)
		if err != nil {
			return nil, fmt.Errorf("parse Annex G source %q: %w", source, err)
		}
		system.sourceNetworks = append(system.sourceNetworks, network)
	}
	return system, nil
}

// annexGSystemProfileFingerprint 计算外部系统档案指纹，防止重启后把旧在途业务发送到已变更的身份或地址。
// 密码和证书路径只进入摘要，不会写入持久化记录或日志。
func annexGSystemProfileFingerprint(configured conf.SIPAnnexGSystem, localID, globalSignalDigestSeed, id string, version annexg.Version, realm, transport, effectiveSignalDigestSeed string) string {
	sources := make([]string, 0, len(configured.SourceCIDRs))
	for _, source := range configured.SourceCIDRs {
		sources = append(sources, strings.TrimSpace(source))
	}
	sort.Strings(sources)
	payload := struct {
		LocalID                string   `json:"local_id"`
		ID                     string   `json:"id"`
		Role                   string   `json:"role"`
		Version                string   `json:"version"`
		Password               string   `json:"password"`
		SignalDigestSeed       string   `json:"signal_digest_seed"`
		GlobalSignalDigestSeed string   `json:"global_signal_digest_seed"`
		Realm                  string   `json:"realm"`
		Address                string   `json:"address"`
		Transport              string   `json:"transport"`
		SourceCIDRs            []string `json:"source_cidrs"`
		AllowInsecureTransport bool     `json:"allow_insecure_transport"`
		TLSCA                  string   `json:"tls_ca"`
		// omitempty 保持未配置 CRL 的既有档案指纹不变，避免升级后丢弃合法在途关联。
		TLSCRL        string `json:"tls_crl,omitempty"`
		TLSServerName string `json:"tls_server_name"`
		TLSCert       string `json:"tls_cert"`
		TLSKey        string `json:"tls_key"`
	}{
		LocalID: strings.TrimSpace(localID), ID: id, Role: strings.TrimSpace(configured.Role), Version: string(version),
		Password: configured.Password, SignalDigestSeed: effectiveSignalDigestSeed,
		GlobalSignalDigestSeed: globalSignalDigestSeed,
		Realm:                  realm, Address: strings.TrimSpace(configured.Address), Transport: transport,
		SourceCIDRs: sources, AllowInsecureTransport: configured.AllowInsecureTransport,
		TLSCA: strings.TrimSpace(configured.TLSCA), TLSCRL: strings.TrimSpace(configured.TLSCRL),
		TLSServerName: strings.TrimSpace(configured.TLSServerName),
		TLSCert:       strings.TrimSpace(configured.TLSCert), TLSKey: strings.TrimSpace(configured.TLSKey),
	}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (system *annexGSystem) setSignalSecurity(security sip.MessageSecurity) {
	if system == nil {
		return
	}
	system.securityMu.Lock()
	system.signalSecurity = security
	system.securityMu.Unlock()
}

func (system *annexGSystem) messageSecurity() sip.MessageSecurity {
	if system == nil {
		return nil
	}
	system.securityMu.RLock()
	defer system.securityMu.RUnlock()
	return system.signalSecurity
}

func (service *annexGService) updateSignalDigestSecurity(cfg *conf.SIP) error {
	if service == nil || cfg == nil {
		return nil
	}
	type update struct {
		system   *annexGSystem
		security sip.MessageSecurity
	}
	updates := make([]update, 0, len(service.systems))
	for _, system := range service.systems {
		security, err := newSignalDigestSecurity(cfg, annexGSignalDigestSeed(
			system.signalDigestSeed, system.password, cfg.SignalDigest.Seed,
		))
		if err != nil {
			return fmt.Errorf("update Annex G signal Digest for system %s: %w", system.id, err)
		}
		updates = append(updates, update{system: system, security: security})
	}
	for _, item := range updates {
		item.system.setSignalSecurity(item.security)
	}
	return nil
}

type annexGTLSAddr struct {
	*net.TCPAddr
}

func (*annexGTLSAddr) Network() string { return "tls" }

func buildAnnexGTarget(systemID, address, transport string) (*sip.URI, net.Addr, string, error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return nil, nil, "", fmt.Errorf("parse Annex G target address: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, nil, "", errors.New("invalid Annex G target port")
	}
	scheme := "sip"
	if transport == "tls" {
		scheme = "sips"
	}
	uri, err := sip.ParseSipURI(fmt.Sprintf("%s:%s@%s", scheme, strings.TrimSpace(systemID), net.JoinHostPort(host, portText)))
	if err != nil {
		return nil, nil, "", fmt.Errorf("parse Annex G target URI: %w", err)
	}
	setCascadeURITransport(&uri, transport)
	joined := net.JoinHostPort(host, portText)
	if transport == "udp" {
		resolved, resolveErr := net.ResolveUDPAddr("udp", joined)
		return &uri, resolved, host, resolveErr
	}
	resolved, err := net.ResolveTCPAddr("tcp", joined)
	if err != nil {
		return nil, nil, "", err
	}
	if transport == "tls" {
		return &uri, &annexGTLSAddr{TCPAddr: resolved}, host, nil
	}
	return &uri, resolved, host, nil
}

func annexGTLSClientConfig(configured conf.SIPAnnexGSystem, host string) (*tls.Config, error) {
	transport := strings.ToLower(strings.TrimSpace(configured.Transport))
	if transport != "" && transport != "tls" {
		return nil, nil
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: strings.TrimSpace(configured.TLSServerName)}
	if config.ServerName == "" {
		config.ServerName = strings.TrimSpace(host)
	}
	var authorities []*x509.Certificate
	if caFile := strings.TrimSpace(configured.TLSCA); caFile != "" {
		contents, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read Annex G TLS CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(contents) {
			return nil, errors.New("Annex G TLS CA does not contain a valid certificate")
		}
		authorities, err = loadX509Certificates(caFile)
		if err != nil {
			return nil, fmt.Errorf("load Annex G TLS CA: %w", err)
		}
		config.RootCAs = roots
	}
	if crlFile := strings.TrimSpace(configured.TLSCRL); crlFile != "" {
		if len(authorities) == 0 {
			return nil, errors.New("Annex G TLS CRL requires a configured TLS CA")
		}
		lists, err := loadX509RevocationLists(crlFile)
		if err != nil {
			return nil, fmt.Errorf("load Annex G TLS CRL: %w", err)
		}
		if err := validateX509RevocationLists(lists, authorities, time.Now()); err != nil {
			return nil, fmt.Errorf("validate Annex G TLS CRL: %w", err)
		}
		config.VerifyConnection = func(state tls.ConnectionState) error {
			if err := checkAnnexGTLSCertificateChainRevocation(state, lists, authorities, time.Now()); err != nil {
				return fmt.Errorf("verify Annex G TLS certificate revocation: %w", err)
			}
			return nil
		}
	}
	if certFile := strings.TrimSpace(configured.TLSCert); certFile != "" {
		certificate, err := tls.LoadX509KeyPair(certFile, strings.TrimSpace(configured.TLSKey))
		if err != nil {
			return nil, fmt.Errorf("load Annex G TLS client certificate: %w", err)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	return config, nil
}

func checkAnnexGTLSCertificateChainRevocation(state tls.ConnectionState, lists []*x509.RevocationList, authorities []*x509.Certificate, now time.Time) error {
	if len(state.PeerCertificates) == 0 {
		return errors.New("Annex G TLS peer certificate is unavailable")
	}
	// 直接调用 VerifyConnection 的兼容入口没有 VerifiedChains，仍至少校验叶证书。
	if len(state.VerifiedChains) == 0 {
		return checkX509CertificateRevocation(state.PeerCertificates[0], lists, authorities, now)
	}
	var chainErrors []error
	for _, chain := range state.VerifiedChains {
		if len(chain) == 0 {
			continue
		}
		if err := checkX509CertificateRevocation(chain[0], lists, authorities, now); err != nil {
			chainErrors = append(chainErrors, err)
			continue
		}
		chainValid := true
		// 只跳过真正自签的根证书。配置为信任锚但由上级 CA 签发的中间证书
		// 仍必须检查撤销状态，不能因为验证链在该证书处结束而绕过 CRL。
		for index := 1; index < len(chain); index++ {
			certificate := chain[index]
			if certificate != nil && bytes.Equal(certificate.RawIssuer, certificate.RawSubject) && certificate.CheckSignatureFrom(certificate) == nil {
				continue
			}
			applicable := false
			for _, list := range lists {
				if certificate != nil && list != nil && bytes.Equal(list.RawIssuer, certificate.RawIssuer) {
					applicable = true
					break
				}
			}
			if !applicable {
				continue
			}
			if err := checkX509CertificateRevocation(certificate, lists, authorities, now); err != nil {
				chainErrors = append(chainErrors, err)
				chainValid = false
				break
			}
		}
		if chainValid {
			return nil
		}
	}
	if len(chainErrors) == 0 {
		return errors.New("Annex G TLS verified certificate chain is unavailable")
	}
	return errors.Join(chainErrors...)
}

func parseAnnexGSourceNetwork(value string) (*net.IPNet, error) {
	value = strings.TrimSpace(value)
	if ip := net.ParseIP(value); ip != nil {
		bits := 128
		if ip4 := ip.To4(); ip4 != nil {
			ip, bits = ip4, 32
		}
		return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, nil
	}
	_, network, err := net.ParseCIDR(value)
	return network, err
}

// AuthorizeAnnexG 确保交换的一端是本管理平台，另一端来自静态外部系统档案。
func (service *annexGService) AuthorizeAnnexG(_ context.Context, exchange annexg.Exchange) error {
	if service == nil {
		return annexg.ErrAdapterDisabled
	}
	if exchange.SourceID == service.localID && exchange.SourceRole == annexg.RoleManagementPlatform {
		system, ok := service.systems[strings.TrimSpace(exchange.DestinationID)]
		if !ok || system.role != exchange.DestinationRole {
			return errors.New("Annex G destination is not authorized")
		}
		return nil
	}
	if exchange.DestinationID == service.localID && exchange.DestinationRole == annexg.RoleManagementPlatform {
		system, ok := service.systems[strings.TrimSpace(exchange.SourceID)]
		if !ok || system.role != exchange.SourceRole {
			return errors.New("Annex G source is not authorized")
		}
		return nil
	}
	return errors.New("Annex G exchange does not include the local management platform")
}

func annexGCommandFromBody(body []byte) annexg.Command {
	_, command, err := annexg.ClassifyEnvelope(body)
	if err != nil {
		return ""
	}
	return command
}

func annexGCommandFromRequest(request *sip.Request) annexg.Command {
	if request == nil || !strings.EqualFold(strings.TrimSpace(request.Method()), sip.MethodMessage) {
		return ""
	}
	return annexGCommandFromBody(request.Body())
}

func (g *GB28181API) sipAnnexGAccessControlMiddleware(ctx *sip.Context) {
	if ctx == nil {
		return
	}
	if g == nil {
		ctx.AbortString(403, "Annex G is disabled")
		return
	}
	g.metrics.annexGRequests.Add(1)
	accepted := false
	defer func() {
		if !accepted {
			g.metrics.annexGRejected.Add(1)
		}
	}()
	if g.annexG == nil || ctx.Request == nil {
		ctx.AbortString(403, "Annex G is disabled")
		return
	}
	system, ok := g.annexG.systems[strings.TrimSpace(ctx.DeviceID)]
	if !ok {
		ctx.AbortString(403, "Annex G system is not authorized")
		return
	}
	if recipient := ctx.Request.Recipient(); recipient == nil || recipient.User() == nil || strings.TrimSpace(recipient.User().String()) != g.annexG.localID {
		ctx.AbortString(403, "Annex G destination mismatch")
		return
	}
	if !annexGSourceAllowed(system, ctx.Source) {
		ctx.AbortString(403, "Annex G source is not allowed")
		return
	}
	transport := sip.SignalingTransport(ctx.Request.GetConnection())
	if transport != "TLS" && !system.allowInsecureTransport {
		ctx.AbortString(403, "Annex G system requires SIP-TLS")
		return
	}
	if headers := ctx.Request.GetHeaders("X-GB-Ver"); len(headers) != 1 || strings.TrimSpace(ctx.XGBVerRaw) != string(system.version) {
		ctx.AbortString(400, "Annex G X-GB-Ver does not match the system profile")
		return
	}
	if err := g.checkMessageDigestCredential(ctx, system.id, system.password, g.annexG.realm); err != nil {
		g.respondMessageDigestChallenge(ctx)
		return
	}
	if system.inboundLimiter != nil && !system.inboundLimiter.Allow() {
		g.metrics.annexGRateLimited.Add(1)
		response := sip.NewResponseFromRequest("", ctx.Request, 503, "Service Unavailable", nil)
		response.AppendHeader(&sip.GenericHeader{HeaderName: "Retry-After", Contents: "1"})
		_ = ctx.Tx.Respond(response)
		ctx.Abort()
		return
	}
	ctx.XGBVer = string(system.version)
	ctx.Set(annexGSystemContextKey, system)
	accepted = true
	g.metrics.annexGAccepted.Add(1)
	ctx.Next()
}

func annexGSourceAllowed(system *annexGSystem, source net.Addr) bool {
	if system == nil || source == nil {
		return false
	}
	var ip net.IP
	switch value := source.(type) {
	case *net.UDPAddr:
		ip = value.IP
	case *net.TCPAddr:
		ip = value.IP
	default:
		host := parseAddressIP(source.String())
		if zoneAt := strings.LastIndexByte(host, '%'); zoneAt >= 0 {
			host = host[:zoneAt]
		}
		ip = net.ParseIP(host)
	}
	if ip == nil {
		return false
	}
	for _, network := range system.sourceNetworks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// AnnexGExchange 以管理平台身份向已配置的附录 G 外部系统发送通知或查询，
// 并等待对方通过独立 SIP MESSAGE 返回严格关联的业务响应。
func (s *Server) AnnexGExchange(ctx context.Context, systemID string, request annexg.Message) (annexg.Message, error) {
	if s == nil || s.gb == nil || s.gb.annexG == nil || s.Server == nil {
		return nil, errors.New("Annex G runtime is unavailable")
	}
	if ctx == nil {
		return nil, errors.New("nil Annex G exchange context")
	}
	done, ok := s.gb.beginLifecycleRequest()
	if !ok {
		return nil, errors.New("GB28181 server is closing")
	}
	defer done()
	return s.gb.annexG.exchange(ctx, s.gb.lifecycleCtx, s, systemID, request)
}

func (service *annexGService) exchange(ctx, lifecycleCtx context.Context, server *Server, systemID string, request annexg.Message) (annexg.Message, error) {
	if service == nil || server == nil || server.Server == nil {
		return nil, errors.New("Annex G runtime is unavailable")
	}
	if ctx == nil {
		return nil, errors.New("nil Annex G exchange context")
	}
	if request == nil {
		return nil, errors.New("Annex G request is unavailable")
	}
	system, ok := service.systems[strings.TrimSpace(systemID)]
	if !ok || system == nil {
		return nil, errors.New("Annex G destination system is not configured")
	}
	body, err := annexg.Encode(system.version, request)
	if err != nil {
		return nil, err
	}
	exchange := annexg.Exchange{
		SourceID: service.localID, SourceRole: annexg.RoleManagementPlatform,
		DestinationID: system.id, DestinationRole: system.role,
	}
	prepared, err := service.adapter.Prepare(ctx, system.version, exchange, body)
	if err != nil {
		return nil, err
	}
	if prepared.Message.RootName() == "Response" {
		return nil, errors.New("Annex G platform exchange only accepts outbound Notify or Query messages")
	}
	key, err := annexGPendingKeyFor(system.id, prepared.Message)
	if err != nil {
		return nil, err
	}
	pending, err := service.registerPending(key, prepared.Message)
	if err != nil {
		return nil, err
	}

	if err := service.store.SavePendingExchange(ctx, system.id, system.version, prepared.Message, system.profileFingerprint); err != nil {
		service.removePending(key, pending)
		return nil, fmt.Errorf("persist pending Annex G request: %w", err)
	}
	if err := service.persistOutboundBeforeSend(ctx, prepared.Message); err != nil {
		discardErr := service.discardPending(key, pending)
		if discardErr != nil {
			err = errors.Join(err, discardErr)
		}
		return nil, fmt.Errorf("persist outbound Annex G request: %w", err)
	}
	sipRequest, err := buildAnnexGOutboundRequest(server, service, system, body)
	if err != nil {
		return nil, service.discardPendingAfterError(key, pending, err)
	}
	if err := system.prepareRequestConnection(ctx, server, sipRequest); err != nil {
		return nil, service.discardPendingAfterError(key, pending, err)
	}
	if service.outbound == nil {
		return nil, service.discardPendingAfterError(key, pending, errors.New("Annex G outbound SIP sender is unavailable"))
	}
	if err := service.outbound(ctx, server.Server, system, sipRequest); err != nil {
		// 请求可能已经写出但 SIP 确认丢失，保留关联以接收迟到业务响应。
		service.finishPendingSend(key, pending, err, time.Now())
		service.wakePendingRetry()
		return nil, err
	}
	service.finishPendingSend(key, pending, nil, time.Now())

	var lifecycleDone <-chan struct{}
	if lifecycleCtx != nil {
		lifecycleDone = lifecycleCtx.Done()
	}
	select {
	case result := <-pending.result:
		if result.err != nil {
			return result.message, result.err
		}
		return result.message, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-lifecycleDone:
		return nil, errors.New("GB28181 server is closing")
	}
}

func annexGPendingKeyFor(systemID string, message annexg.Message) (annexGPendingKey, error) {
	sn, ok := annexg.MessageSequence(message)
	if !ok || sn <= 0 || message == nil || message.CommandType() == "" {
		return annexGPendingKey{}, errors.New("Annex G message correlation fields are unavailable")
	}
	return annexGPendingKey{systemID: strings.TrimSpace(systemID), command: message.CommandType(), sn: sn}, nil
}

func (service *annexGService) registerPending(key annexGPendingKey, request annexg.Message) (*annexGPendingExchange, error) {
	service.pendingMu.Lock()
	defer service.pendingMu.Unlock()
	if service.closed {
		return nil, errors.New("Annex G runtime is closed")
	}
	if service.pending == nil {
		service.pending = make(map[annexGPendingKey]*annexGPendingExchange)
	}
	now := time.Now()
	for existingKey, existing := range service.pending {
		if existing != nil && !existing.expiresAt.IsZero() && !existing.expiresAt.After(now) {
			delete(service.pending, existingKey)
			select {
			case existing.result <- annexGPendingResult{err: context.DeadlineExceeded}:
			default:
			}
		}
	}
	if _, exists := service.pending[key]; exists {
		return nil, fmt.Errorf("Annex G request %s/%d is already pending for system %s", key.command, key.sn, key.systemID)
	}
	if len(service.pending) >= service.maxPending {
		return nil, fmt.Errorf("Annex G pending exchange limit %d reached", service.maxPending)
	}
	pending := &annexGPendingExchange{
		request: request, result: make(chan annexGPendingResult, 1), expiresAt: now.Add(service.pendingTTL),
		needsSend: true, sending: true, nextSend: now,
	}
	service.pending[key] = pending
	return pending, nil
}

func (service *annexGService) removePending(key annexGPendingKey, pending *annexGPendingExchange) {
	if service == nil || pending == nil {
		return
	}
	service.pendingMu.Lock()
	if service.pending[key] == pending {
		delete(service.pending, key)
	}
	service.pendingMu.Unlock()
}

func (service *annexGService) discardPending(key annexGPendingKey, pending *annexGPendingExchange) error {
	if service == nil || service.store == nil {
		return errors.New("Annex G store is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := service.store.DeletePendingExchange(ctx, key.systemID, key.command, key.sn); err != nil {
		return fmt.Errorf("delete Annex G pending exchange: %w", err)
	}
	service.removePending(key, pending)
	return nil
}

func (service *annexGService) discardPendingAfterError(key annexGPendingKey, pending *annexGPendingExchange, cause error) error {
	if discardErr := service.discardPending(key, pending); discardErr != nil {
		return errors.Join(cause, discardErr)
	}
	return cause
}

func (service *annexGService) releasePendingResponse(key annexGPendingKey, pending *annexGPendingExchange) {
	if service == nil || pending == nil {
		return
	}
	service.pendingMu.Lock()
	if service.pending[key] == pending {
		pending.delivered = false
	}
	service.pendingMu.Unlock()
}

func (service *annexGService) setPendingResponse(key annexGPendingKey, pending *annexGPendingExchange, response annexg.Message) bool {
	if service == nil || pending == nil || response == nil {
		return false
	}
	service.pendingMu.Lock()
	defer service.pendingMu.Unlock()
	if service.pending[key] != pending {
		return false
	}
	pending.response = response
	return true
}

func (service *annexGService) claimPendingResponse(systemID string, response annexg.Message) (*annexGPendingExchange, error) {
	if service == nil || response == nil || response.RootName() != "Response" {
		return nil, errors.New("Annex G business response is invalid")
	}
	key, err := annexGPendingKeyFor(systemID, response)
	if err != nil {
		return nil, err
	}
	service.pendingMu.Lock()
	defer service.pendingMu.Unlock()
	pending := service.pending[key]
	if pending == nil {
		return nil, fmt.Errorf("Annex G response %s/%d is not associated with an active request", key.command, key.sn)
	}
	if !pending.expiresAt.IsZero() && !pending.expiresAt.After(time.Now()) {
		delete(service.pending, key)
		return nil, fmt.Errorf("Annex G response %s/%d association expired", key.command, key.sn)
	}
	if pending.delivered {
		return nil, fmt.Errorf("Annex G response %s/%d was already delivered", key.command, key.sn)
	}
	if pending.request == nil || pending.request.CommandType() != response.CommandType() {
		return nil, errors.New("Annex G response does not match the active request")
	}
	pending.delivered = true
	return pending, nil
}

func (service *annexGService) consumeClaimedPendingResponse(
	ctx context.Context,
	system *annexGSystem,
	pending *annexGPendingExchange,
	prepared annexg.Exchange,
) error {
	if service == nil || system == nil || pending == nil || prepared.Message == nil {
		return errors.New("Annex G pending response context is unavailable")
	}
	key, err := annexGPendingKeyFor(system.id, prepared.Message)
	if err != nil {
		return err
	}
	_, consumeErr := service.adapter.Consume(ctx, system.version, prepared)
	terminal := false
	if consumeErr == nil {
		result, ok := annexGBusinessResult(prepared.Message)
		if !ok {
			consumeErr = errors.New("Annex G business response result is unavailable")
		} else if result != annexg.ResultOK {
			consumeErr = fmt.Errorf("Annex G peer returned business result %s", result)
			terminal = true
		} else if consumeErr = service.persistOutboundResponse(ctx, pending.request, prepared.Message); consumeErr == nil {
			terminal = true
		}
	}
	if terminal {
		if err := service.store.DeletePendingExchange(ctx, key.systemID, key.command, key.sn); err != nil {
			consumeErr = fmt.Errorf("delete completed Annex G pending exchange: %w", err)
			terminal = false
		}
	}
	if terminal {
		service.removePending(key, pending)
	} else {
		service.releasePendingResponse(key, pending)
	}
	select {
	case pending.result <- annexGPendingResult{message: prepared.Message, err: consumeErr}:
	default:
		slog.Error("deliver Annex G business response failed", "system_id", system.id, "cmd_type", prepared.Message.CommandType())
	}
	return consumeErr
}

func (service *annexGService) persistOutboundBeforeSend(ctx context.Context, request annexg.Message) error {
	if service == nil || service.store == nil {
		return errors.New("Annex G store is unavailable")
	}
	if notify, ok := request.(*annexg.MPAlarmNotify); ok {
		return service.store.SaveMPAlarmRecord(ctx, notify.AlarmContent)
	}
	return nil
}

func (service *annexGService) persistOutboundResponse(ctx context.Context, request, response annexg.Message) error {
	if service == nil || service.store == nil {
		return errors.New("Annex G store is unavailable")
	}
	result, ok := annexGBusinessResult(response)
	if !ok {
		return errors.New("Annex G business response result is unavailable")
	}
	if result != annexg.ResultOK {
		return fmt.Errorf("Annex G peer returned business result %s", result)
	}
	switch original := request.(type) {
	case *annexg.ConfigDefenceNotify:
		localResponse, err := service.store.ApplyConfigDefence(ctx, *original)
		if err != nil {
			return fmt.Errorf("persist acknowledged Annex G defence state: %w", err)
		}
		if localResponse.Result != annexg.ResultOK {
			return errors.New("persist acknowledged Annex G defence state failed")
		}
	case *annexg.AlarmRecordQuery:
		switch records := response.(type) {
		case *annexg.MPAlarmRecordListResponse:
			for _, record := range records.RecordList.AlarmRecords {
				if err := service.store.SaveMPAlarmRecord(ctx, record); err != nil {
					return fmt.Errorf("persist MP alarm query response: %w", err)
				}
			}
		case *annexg.ECSAlarmRecordListResponse:
			for _, record := range records.RecordList.AlarmRecords {
				if err := service.store.SaveECSAlarmRecord(ctx, record); err != nil {
					return fmt.Errorf("persist ECS alarm query response: %w", err)
				}
			}
		case *annexg.TGSAlarmRecordListResponse:
			for _, record := range records.RecordList.AlarmRecords {
				if err := service.store.SaveTGSAlarmRecord(ctx, record); err != nil {
					return fmt.Errorf("persist TGS alarm query response: %w", err)
				}
			}
		default:
			return fmt.Errorf("unexpected Annex G query response %T for %s", response, original.CmdType)
		}
	}
	return nil
}

func annexGBusinessResult(message annexg.Message) (annexg.Result, bool) {
	switch value := message.(type) {
	case *annexg.NotificationResponse:
		return value.Result, true
	case *annexg.MPAlarmRecordListResponse:
		return value.Result, true
	case *annexg.ECSAlarmRecordListResponse:
		return value.Result, true
	case *annexg.TGSAlarmRecordListResponse:
		return value.Result, true
	default:
		return "", false
	}
}

func buildAnnexGOutboundRequest(server *Server, service *annexGService, system *annexGSystem, body []byte) (*sip.Request, error) {
	if server == nil || service == nil || system == nil || system.targetURI == nil || system.target == nil {
		return nil, errors.New("Annex G outbound target is incomplete")
	}
	localURI, err := sip.ParseSipURI(fmt.Sprintf("sip:%s@%s", service.localID, service.realm))
	if err != nil {
		return nil, fmt.Errorf("build Annex G local URI: %w", err)
	}
	localHost := ""
	if server.fromAddress.URI != nil {
		localHost = strings.TrimSpace(server.fromAddress.URI.Host())
	}
	if localHost == "" {
		return nil, errors.New("Annex G local SIP advertise host is unavailable")
	}
	from := &sip.Address{URI: &localURI, Params: sip.NewParams().Add("tag", sip.String{Str: sip.RandString(16)})}
	to := &sip.Address{URI: system.targetURI.Clone(), Params: sip.NewParams()}
	callID := sip.CallID("annex-g-" + sip.RandString(24))
	var viaPort *sip.Port
	if server.fromAddress.URI != nil && server.fromAddress.URI.FPort != nil {
		port := *server.fromAddress.URI.FPort
		viaPort = &port
	}
	headers := sip.NewHeaderBuilder().
		SetFrom(from).
		SetTo(to).
		SetContentType(&sip.ContentTypeXML).
		SetMethod(sip.MethodMessage).
		SetSeqNo(1).
		SetCallID(&callID).
		SetXGBVerValue(string(system.version)).
		AddVia(&sip.ViaHop{
			Host: localHost, Port: viaPort, Transport: strings.ToUpper(system.transport),
			Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}).Add("rport", nil),
		}).Build()
	request := sip.NewRequest("", sip.MethodMessage, system.targetURI.Clone(), sip.DefaultSipVersion, headers, body)
	request.SetDestination(system.target)
	return request, nil
}

func (system *annexGSystem) prepareRequestConnection(ctx context.Context, server *Server, request *sip.Request) error {
	if system == nil || server == nil || server.Server == nil || request == nil || system.target == nil {
		return errors.New("Annex G SIP connection target is unavailable")
	}
	if system.transport == "udp" {
		wireLength, err := signedSIPRequestLength(request, system.messageSecurity())
		if err != nil {
			return err
		}
		if wireLength > cascadeReliableTransportThreshold {
			reliableTarget, err := cascadeTCPDestination(system.target)
			if err != nil {
				return fmt.Errorf("resolve oversized Annex G SIP/TCP target: %w", err)
			}
			conn, err := system.ensureConnectionTarget(ctx, server, "tcp", reliableTarget)
			if err != nil {
				return err
			}
			request.SetConnection(conn)
			request.SetSource(conn.LocalAddr())
			request.SetDestination(reliableTarget)
			setCascadeURITransport(request.Recipient(), "tcp")
			applyCascadeRequestTransport(request, "tcp")
			return nil
		}
		conn := server.UDPConn()
		if conn == nil {
			return errors.New("SIP UDP listener is unavailable")
		}
		request.SetConnection(conn)
		request.SetSource(conn.LocalAddr())
		return nil
	}
	conn, err := system.ensureConnection(ctx, server)
	if err != nil {
		return err
	}
	request.SetConnection(conn)
	request.SetSource(conn.LocalAddr())
	return nil
}

func (system *annexGSystem) ensureConnection(ctx context.Context, server *Server) (sip.Connection, error) {
	return system.ensureConnectionTarget(ctx, server, system.transport, system.target)
}

func (system *annexGSystem) ensureConnectionTarget(ctx context.Context, server *Server, transport string, target net.Addr) (sip.Connection, error) {
	if system == nil || server == nil || server.Server == nil || system.target == nil {
		return nil, errors.New("Annex G SIP/TCP target is unavailable")
	}
	transport = strings.ToLower(strings.TrimSpace(transport))
	if target == nil || (transport != "tcp" && transport != "tls") {
		return nil, errors.New("Annex G SIP/TCP target is unavailable")
	}
	address := target.String()
	connectionKey := transport + "|" + address
	system.connMu.Lock()
	if system.closed {
		system.connMu.Unlock()
		return nil, errors.New("Annex G external system connection is closed")
	}
	currentKey := system.connKey
	if currentKey == "" && system.target != nil {
		currentKey = system.transport + "|" + system.target.String()
	}
	if system.conn != nil && currentKey == connectionKey {
		conn := system.conn
		system.connMu.Unlock()
		return conn, nil
	}
	dialTCP, dialTLS := system.dialTCP, system.dialTLS
	system.connMu.Unlock()

	var raw net.Conn
	var err error
	if transport == "tls" {
		if dialTLS == nil || system.tlsConfig == nil {
			return nil, errors.New("Annex G SIP-TLS dialer is unavailable")
		}
		raw, err = dialTLS(ctx, address, system.tlsConfig.Clone())
	} else {
		if transport != "tcp" || dialTCP == nil {
			return nil, errors.New("Annex G SIP/TCP dialer is unavailable")
		}
		raw, err = dialTCP(ctx, address)
	}
	if err != nil {
		return nil, fmt.Errorf("dial Annex G SIP/%s %s: %w", strings.ToUpper(transport), address, err)
	}
	var conn sip.Connection
	if transport == "tls" {
		conn = sip.NewTLSConnection(raw)
	} else {
		conn = sip.NewTCPConnection(raw)
	}
	system.connMu.Lock()
	if system.closed {
		system.connMu.Unlock()
		_ = conn.Close()
		return nil, errors.New("Annex G external system connection is closed")
	}
	currentKey = system.connKey
	if currentKey == "" && system.target != nil {
		currentKey = system.transport + "|" + system.target.String()
	}
	if system.conn != nil && currentKey == connectionKey {
		existing := system.conn
		system.connMu.Unlock()
		_ = conn.Close()
		return existing, nil
	}
	previous := system.conn
	system.conn = conn
	system.connKey = connectionKey
	system.connMu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	go func() {
		server.Server.ProcessTCPConnection(conn)
		system.connMu.Lock()
		if system.conn == conn {
			system.conn = nil
			system.connKey = ""
		}
		system.connMu.Unlock()
	}()
	return conn, nil
}

func (system *annexGSystem) invalidateConnection(conn sip.Connection) {
	if system == nil || conn == nil {
		return
	}
	system.connMu.Lock()
	if system.conn != conn {
		system.connMu.Unlock()
		return
	}
	system.conn = nil
	system.connKey = ""
	system.connMu.Unlock()
	_ = conn.Close()
}

func (system *annexGSystem) closeConnection() {
	if system == nil {
		return
	}
	system.connMu.Lock()
	system.closed = true
	conn := system.conn
	system.conn = nil
	system.connKey = ""
	system.connMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (service *annexGService) close() {
	if service == nil {
		return
	}
	service.closeOnce.Do(func() {
		service.pendingMu.Lock()
		service.closed = true
		service.server = nil
		service.pendingMu.Unlock()
		if service.cleanerCancel != nil {
			service.cleanerCancel()
		}
		service.cleanerWG.Wait()
		service.pendingMu.Lock()
		pending := make([]*annexGPendingExchange, 0, len(service.pending))
		for key, exchange := range service.pending {
			pending = append(pending, exchange)
			delete(service.pending, key)
		}
		service.pendingMu.Unlock()
		for _, exchange := range pending {
			select {
			case exchange.result <- annexGPendingResult{err: errors.New("Annex G runtime is closed")}:
			default:
			}
		}
		for _, system := range service.systems {
			system.closeConnection()
		}
	})
}

func sendAnnexGSIPRequest(ctx context.Context, server *sip.Server, system *annexGSystem, request *sip.Request) error {
	if server == nil || system == nil || request == nil {
		return errors.New("Annex G SIP sender is unavailable")
	}
	response, err := exchangeAnnexGSIPRequest(ctx, server, system, request)
	if err != nil {
		system.invalidateConnection(request.GetConnection())
		return err
	}
	if response.StatusCode() >= 200 && response.StatusCode() < 300 {
		return nil
	}
	if response.StatusCode() != 401 && response.StatusCode() != 407 {
		return fmt.Errorf("Annex G SIP response status %d", response.StatusCode())
	}
	retry, err := buildAnnexGDigestRetry(request, response, system, system.realm)
	if err != nil {
		return err
	}
	retryResponse, err := exchangeAnnexGSIPRequest(ctx, server, system, retry)
	if err != nil {
		system.invalidateConnection(retry.GetConnection())
		return err
	}
	if retryResponse.StatusCode() < 200 || retryResponse.StatusCode() >= 300 {
		return fmt.Errorf("Annex G authenticated SIP response status %d", retryResponse.StatusCode())
	}
	return nil
}

func exchangeAnnexGSIPRequest(ctx context.Context, server *sip.Server, system *annexGSystem, request *sip.Request) (*sip.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := server.RequestWithSecurityContext(attemptCtx, request, system.messageSecurity())
	if err != nil {
		return nil, err
	}
	defer tx.Close()
	return waitAnnexGSIPResponse(attemptCtx, tx)
}

func (g *GB28181API) sipAnnexGMessage(ctx *sip.Context) {
	value, ok := ctx.Get(annexGSystemContextKey)
	system, typeOK := value.(*annexGSystem)
	if !ok || !typeOK || system == nil || g == nil || g.annexG == nil {
		if g != nil {
			g.metrics.annexGBusinessFailures.Add(1)
		}
		ctx.AbortString(403, "Annex G authentication context is unavailable")
		return
	}
	if !annexGXMLContentType(ctx.Request) {
		g.metrics.annexGBusinessFailures.Add(1)
		ctx.AbortString(415, "Content-Type must be Application/MANSCDP+xml")
		return
	}
	exchange := annexg.Exchange{
		SourceID: system.id, SourceRole: system.role,
		DestinationID: g.annexG.localID, DestinationRole: annexg.RoleManagementPlatform,
	}
	parent := g.lifecycleCtx
	if parent == nil {
		parent = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	prepared, err := g.annexG.adapter.Prepare(requestCtx, system.version, exchange, ctx.Request.Body())
	if err != nil {
		g.metrics.annexGBusinessFailures.Add(1)
		ctx.AbortString(400, err.Error())
		return
	}
	if prepared.Message.RootName() == "Response" {
		key, err := annexGPendingKeyFor(system.id, prepared.Message)
		if err != nil {
			g.metrics.annexGBusinessFailures.Add(1)
			ctx.AbortString(400, err.Error())
			return
		}
		pending, err := g.annexG.claimPendingResponse(system.id, prepared.Message)
		if err != nil {
			g.metrics.annexGBusinessFailures.Add(1)
			ctx.AbortString(409, err.Error())
			return
		}
		if err := g.annexG.store.SavePendingResponse(requestCtx, system.id, system.version, prepared.Message); err != nil {
			g.metrics.annexGBusinessFailures.Add(1)
			g.annexG.releasePendingResponse(key, pending)
			ctx.AbortString(503, "persist Annex G business response failed")
			return
		}
		if !g.annexG.setPendingResponse(key, pending, prepared.Message) {
			g.metrics.annexGBusinessFailures.Add(1)
			ctx.AbortString(409, "Annex G pending exchange changed while storing response")
			return
		}
		respondErr := ctx.RespondString(200, "OK")
		ctx.Abort()
		if respondErr != nil {
			g.metrics.annexGBusinessFailures.Add(1)
			g.annexG.releasePendingResponse(key, pending)
			slog.Error("acknowledge Annex G business response failed", "system_id", system.id, "cmd_type", prepared.Message.CommandType(), "err", respondErr)
			return
		}
		if consumeErr := g.annexG.consumeClaimedPendingResponse(requestCtx, system, pending, prepared); consumeErr != nil {
			g.metrics.annexGBusinessFailures.Add(1)
			slog.Error("consume Annex G business response failed", "system_id", system.id, "cmd_type", prepared.Message.CommandType(), "err", consumeErr)
		}
		return
	}

	// SIP 事务确认必须先于可能涉及数据库的业务消费和独立 MESSAGE 业务应答。
	respondErr := ctx.RespondString(200, "OK")
	ctx.Abort()
	if respondErr != nil {
		g.metrics.annexGBusinessFailures.Add(1)
		slog.Error("acknowledge Annex G request failed", "system_id", system.id, "cmd_type", prepared.Message.CommandType(), "err", respondErr)
		return
	}
	response, err := g.annexG.adapter.Consume(requestCtx, system.version, prepared)
	if err != nil {
		g.metrics.annexGBusinessFailures.Add(1)
		response = annexg.ErrorResponse(prepared.Message)
		if response == nil {
			slog.Error("consume Annex G response failed", "system_id", system.id, "err", err)
			return
		}
		slog.Error("consume Annex G request failed", "system_id", system.id, "cmd_type", prepared.Message.CommandType(), "err", err)
	}
	if response == nil {
		return
	}
	if g.annexG.send == nil {
		g.metrics.annexGBusinessFailures.Add(1)
		slog.Error("send Annex G business response failed", "system_id", system.id, "err", "sender is unavailable")
		return
	}
	if sendErr := g.annexG.send(ctx, system, system.version, response); sendErr != nil {
		g.metrics.annexGBusinessFailures.Add(1)
		slog.Error("send Annex G business response failed", "system_id", system.id, "cmd_type", response.CommandType(), "err", sendErr)
	}
}

func annexGXMLContentType(request *sip.Request) bool {
	if request == nil || len(request.GetHeaders("Content-Type")) != 1 {
		return false
	}
	contentType, ok := request.ContentType()
	if !ok || contentType == nil {
		return false
	}
	mediaType, _, _ := strings.Cut(string(*contentType), ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), string(sip.ContentTypeXML))
}

func (g *GB28181API) sendAnnexGResponse(ctx *sip.Context, system *annexGSystem, version annexg.Version, response annexg.Message) error {
	if g == nil || g.svr == nil || g.svr.Server == nil || ctx == nil || ctx.Request == nil || system == nil {
		return errors.New("Annex G SIP sender is unavailable")
	}
	body, err := annexg.Encode(version, response)
	if err != nil {
		return err
	}
	request, err := buildAnnexGResponseRequest(ctx, version, body)
	if err != nil {
		return err
	}
	return sendAnnexGSIPRequest(g.lifecycleCtx, g.svr.Server, system, request)
}

func buildAnnexGResponseRequest(ctx *sip.Context, version annexg.Version, body []byte) (*sip.Request, error) {
	if ctx == nil || ctx.Request == nil || ctx.To == nil || ctx.To.URI == nil || ctx.From == nil || ctx.From.URI == nil || ctx.Source == nil {
		return nil, errors.New("Annex G response target is incomplete")
	}
	headers := sip.NewHeaderBuilder().
		SetTo(ctx.To).
		SetFrom(ctx.From).
		AddVia(&sip.ViaHop{Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).
		SetContentType(&sip.ContentTypeXML).
		SetXGBVerValue(string(version)).
		SetMethod(sip.MethodMessage).
		Build()
	request := sip.NewRequest("", sip.MethodMessage, ctx.To.URI.Clone(), sip.DefaultSipVersion, headers, body)
	request.SetDestination(ctx.Source)
	request.SetConnection(ctx.Request.GetConnection())
	return request, nil
}

func waitAnnexGSIPResponse(parent context.Context, tx *sip.Transaction) (*sip.Response, error) {
	if parent == nil {
		parent = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	response, err := tx.GetResponseContext(waitCtx)
	if err != nil {
		return nil, fmt.Errorf("wait Annex G SIP response: %w", err)
	}
	if response == nil {
		return nil, errors.New("Annex G SIP response is unavailable")
	}
	return response, nil
}

func buildAnnexGDigestRetry(request *sip.Request, challengeResponse *sip.Response, system *annexGSystem, expectedRealm string) (*sip.Request, error) {
	if request == nil || challengeResponse == nil || system == nil {
		return nil, errors.New("Annex G Digest challenge is incomplete")
	}
	challengeHeader, authorizeHeader := "", ""
	switch challengeResponse.StatusCode() {
	case 401:
		challengeHeader, authorizeHeader = "WWW-Authenticate", "Authorization"
	case 407:
		challengeHeader, authorizeHeader = "Proxy-Authenticate", "Proxy-Authorization"
	default:
		return nil, fmt.Errorf("Annex G Digest challenge status %d is unsupported", challengeResponse.StatusCode())
	}
	headers := challengeResponse.GetHeaders(challengeHeader)
	if len(headers) != 1 {
		return nil, fmt.Errorf("Annex G Digest challenge must contain exactly one %s header", challengeHeader)
	}
	challenge, err := sip.AuthFromValueChecked(headers[0].String())
	if err != nil {
		return nil, fmt.Errorf("parse Annex G Digest challenge: %w", err)
	}
	if challenge.Get("realm") == "" || challenge.Get("realm") != strings.TrimSpace(expectedRealm) || challenge.Get("nonce") == "" {
		return nil, errors.New("Annex G Digest challenge is invalid")
	}
	if request.Recipient() == nil {
		return nil, errors.New("Annex G request URI is unavailable")
	}
	challenge.SetUsername(system.id).
		SetPassword(system.password).
		SetMethod(sip.MethodMessage).
		SetURI(request.Recipient().String())
	if challenge.QOP() == "auth" {
		challenge.SetClientNonce("00000001", sip.RandString(32))
	}
	if _, err := challenge.CalcResponseChecked(); err != nil {
		return nil, fmt.Errorf("calculate Annex G Digest response: %w", err)
	}

	retry, ok := request.Clone().(*sip.Request)
	if !ok || retry == nil {
		return nil, errors.New("clone Annex G request failed")
	}
	cseq, ok := retry.CSeq()
	if !ok || cseq == nil {
		return nil, errors.New("Annex G request CSeq is unavailable")
	}
	next, err := sip.NextCSeq(cseq.SeqNo)
	if err != nil {
		return nil, fmt.Errorf("Annex G Digest retry: %w", err)
	}
	retry.RemoveHeader("CSeq")
	retry.AppendHeader(&sip.CSeq{SeqNo: next, MethodName: sip.MethodMessage})
	retry.RemoveHeader("Via")
	retry.AppendHeader(sip.ViaHeader{&sip.ViaHop{
		ProtocolName: "SIP", ProtocolVersion: "2.0",
		Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
	}})
	retry.RemoveHeader("Authorization")
	retry.RemoveHeader("Proxy-Authorization")
	retry.AppendHeader(&sip.GenericHeader{HeaderName: authorizeHeader, Contents: challenge.String()})
	return retry, nil
}

func registerAnnexGRoutes(group *sip.RouteGroup, handler sip.HandlerFunc) {
	if group == nil || handler == nil {
		return
	}
	for _, command := range []annexg.Command{
		annexg.CommandMPAlarm, annexg.CommandECSAlarm, annexg.CommandTGSAlarm,
		annexg.CommandConfigDefence, annexg.CommandMPAlarmRecordList,
		annexg.CommandECSAlarmRecordList, annexg.CommandTGSAlarmRecordList,
	} {
		group.Handle(string(command), handler)
	}
}
