package gbs

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
)

var (
	errAuthHeaderMissing              = errors.New("authorization header required")
	errInboundDeviceNotRegistered     = errors.New("GB28181 device is not registered")
	errInboundDeviceGenerationChanged = errors.New("GB28181 device registration generation changed")
)

const (
	cascadeWorkerContextKey              = "gb28181.cascade.worker"
	inboundRegistrationBindingContextKey = "gb28181.inbound.registration-binding"
)

const (
	messageNonceTTL  = 5 * time.Minute
	maxMessageNonces = 4096
)

type messageNonceState struct {
	DeviceID            string
	SourceIP            string
	AcceptedFingerprint string
	HighestNC           uint32
	IssuedAt            time.Time
	Expires             time.Time
}

type inboundRegistrationBinding struct {
	device         *Device
	lastRegisterAt time.Time
	expires        int
}

// sipAccessControlMiddleware 为 MESSAGE/NOTIFY 提供访问控制中间件。
// 控制项：
// 1) strict_source_check：校验上报源 IP 是否与注册源一致；
// 2) require_message_auth：要求 MESSAGE/NOTIFY 携带 Digest 鉴权。
func (g *GB28181API) sipAccessControlMiddleware(ctx *sip.Context) {
	if g != nil && g.annexG != nil && ctx != nil && annexGCommandFromRequest(ctx.Request) != "" {
		g.sipAnnexGAccessControlMiddleware(ctx)
		return
	}
	if g != nil && g.svr != nil && g.svr.cascade != nil {
		if worker, ok := g.svr.cascade.matchRegistered(ctx.DeviceID, ctx.Source, ctx.Request.GetConnection()); ok {
			ctx.Set(cascadeWorkerContextKey, worker)
			ctx.XGBVer = string(worker.protocolVersion())
			ctx.XGBVerRaw = ctx.XGBVer
			ctx.Next()
			return
		}
	}
	device, binding, err := g.ensureRegisteredInboundDeviceWithBinding(ctx.DeviceID)
	if err != nil {
		if errors.Is(err, errInboundDeviceNotRegistered) {
			ctx.AbortString(403, "unregistered GB28181 device")
			return
		}
		ctx.Log.Error("validate inbound GB28181 device", "err", err)
		ctx.AbortString(503, "GB28181 device store unavailable")
		return
	}
	ctx.XGBVer = string(GBVersion10)
	if version, ok := ParseGBProtocolVersion(device.GBVersion()); ok {
		ctx.XGBVer = string(version)
	}
	if err := g.checkSourceAddress(ctx); err != nil {
		ctx.AbortString(403, err.Error())
		return
	}
	if err := g.checkDigestAuth(ctx); err != nil {
		g.respondMessageDigestChallenge(ctx)
		return
	}
	ctx.Set(inboundRegistrationBindingContextKey, binding)
	ctx.Next()
}

func admittedInboundRegistrationBinding(ctx *sip.Context) (inboundRegistrationBinding, bool) {
	if ctx == nil {
		return inboundRegistrationBinding{}, false
	}
	value, ok := ctx.Get(inboundRegistrationBindingContextKey)
	if !ok {
		return inboundRegistrationBinding{}, false
	}
	binding, ok := value.(inboundRegistrationBinding)
	return binding, ok
}

// inboundRegistrationBindingMatchesLocked 仅在持有同设备 REGISTER 操作锁时调用。
func (g *GB28181API) inboundRegistrationBindingMatchesLocked(deviceID string, expected inboundRegistrationBinding) bool {
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return false
	}
	device, ok := g.svr.memoryStorer.Load(strings.TrimSpace(deviceID))
	if !ok || device == nil {
		return false
	}
	if expected.device != nil && device != expected.device {
		return false
	}
	current := device.runtimeSnapshot()
	return runtimeRegistrationBindingActive(current, time.Now()) &&
		current.Expires == expected.expires && current.LastRegisterAt.Equal(expected.lastRegisterAt)
}

// ensureRegisteredInboundDevice 只允许已有注册运行态的设备进入普通业务路由。
// 附录 G 外部系统和已注册上级平台在调用前已由各自身份门禁分流。
func (g *GB28181API) ensureRegisteredInboundDevice(deviceID string) (*Device, error) {
	device, _, err := g.ensureRegisteredInboundDeviceWithBinding(deviceID)
	return device, err
}

func (g *GB28181API) ensureRegisteredInboundDeviceWithBinding(deviceID string) (*Device, inboundRegistrationBinding, error) {
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return nil, inboundRegistrationBinding{}, fmt.Errorf("GB28181 memory store unavailable")
	}
	if device, ok := g.svr.memoryStorer.Load(deviceID); ok && device != nil {
		state := device.runtimeSnapshot()
		if runtimeRegistrationBindingActive(state, time.Now()) {
			return device, inboundRegistrationBinding{
				device:         device,
				lastRegisterAt: state.LastRegisterAt,
				expires:        state.Expires,
			}, nil
		}
	}
	return nil, inboundRegistrationBinding{}, errInboundDeviceNotRegistered
}

func (g *GB28181API) respondMessageDigestChallenge(ctx *sip.Context) {
	cfg := g.configSnapshot()
	domain := ""
	if cfg != nil {
		domain = cfg.GetDomain()
	}
	nonce := g.issueMessageNonce(ctx.DeviceID, parseAddressIP(addrString(ctx.Source)))
	resp := sip.NewResponseFromRequest("", ctx.Request, 401, "Unauthorized", nil)
	resp.AppendHeader(&sip.GenericHeader{
		HeaderName: "WWW-Authenticate",
		Contents:   fmt.Sprintf(`Digest realm="%s",qop="auth",nonce="%s"`, domain, nonce),
	})
	_ = ctx.Tx.Respond(resp)
	ctx.Abort()
}

// checkSourceAddress 校验源地址（仅比较 IP，忽略端口变化）。
func (g *GB28181API) checkSourceAddress(ctx *sip.Context) error {
	cfg := g.configSnapshot()
	if cfg == nil || !cfg.StrictSourceCheck {
		return nil
	}
	srcIP := parseComparableIP(addrString(ctx.Source))
	if srcIP == nil {
		return fmt.Errorf("source ip is unavailable or invalid")
	}
	cred, err := g.lookupDeviceCredential(ctx.DeviceID)
	if err != nil {
		return fmt.Errorf("device not found")
	}
	expectedIP := parseComparableIP(cred.Address)
	if expectedIP == nil {
		return fmt.Errorf("registered source ip is unavailable or invalid")
	}
	if !srcIP.Equal(expectedIP) {
		return fmt.Errorf("source ip mismatch")
	}
	return nil
}

// checkDigestAuth 校验 Digest 鉴权。
func (g *GB28181API) checkDigestAuth(ctx *sip.Context) error {
	cfg := g.configSnapshot()
	if cfg == nil || !cfg.RequireMessageAuth {
		return nil
	}
	cred, err := g.lookupDeviceCredential(ctx.DeviceID)
	if err != nil {
		return fmt.Errorf("device not found")
	}
	password := cred.Password
	if password == "" {
		password = cfg.Password
	}
	// ignorePassword 表示免鉴权，保持与 REGISTER 逻辑一致。
	if password == "" || password == ignorePassword {
		return nil
	}
	return g.checkMessageDigestCredential(ctx, cred.DeviceID, password, cfg.GetDomain())
}

// checkMessageDigestCredential 使用服务端签发且绑定来源的 nonce 校验一组明确凭据。
// 普通设备和附录 G 静态外部系统共用同一套 qop/nc/重放语义。
func (g *GB28181API) checkMessageDigestCredential(ctx *sip.Context, username, password, realm string) error {
	if ctx == nil || ctx.Request == nil {
		return fmt.Errorf("request is unavailable")
	}
	username = strings.TrimSpace(username)
	realm = strings.TrimSpace(realm)
	if username == "" || password == "" || realm == "" {
		return fmt.Errorf("Digest credential is incomplete")
	}
	hdrs := ctx.Request.GetHeaders("Authorization")
	if len(hdrs) == 0 {
		return errAuthHeaderMissing
	}
	if len(hdrs) != 1 {
		return fmt.Errorf("request must contain exactly one Authorization header")
	}
	h, ok := hdrs[0].(*sip.GenericHeader)
	if !ok {
		return fmt.Errorf("invalid authorization header")
	}
	auth := sip.AuthFromValue(h.Contents)
	if auth.Get("realm") != realm {
		return fmt.Errorf("digest realm mismatch")
	}
	if !strings.EqualFold(auth.Algorithm(), registerDigestAlgo) {
		return fmt.Errorf("unsupported Digest algorithm %q", auth.Algorithm())
	}
	if auth.Get("username") != username {
		return fmt.Errorf("digest username mismatch")
	}
	if ctx.Request.Recipient() == nil {
		return fmt.Errorf("request URI is missing")
	}
	requestURI := ctx.Request.Recipient().String()
	if auth.Get("uri") != requestURI {
		return fmt.Errorf("digest uri mismatch")
	}
	if auth.QOP() == "auth" {
		if len(auth.Get("nc")) != 8 || strings.TrimSpace(auth.Get("cnonce")) == "" {
			return fmt.Errorf("Digest qop=auth requires 8-digit nc and cnonce")
		}
		decoded, err := hex.DecodeString(auth.Get("nc"))
		if err != nil || len(decoded) != 4 {
			return fmt.Errorf("invalid Digest nc")
		}
	} else if auth.QOP() != "" {
		return fmt.Errorf("unsupported Digest qop %q", auth.QOP())
	}
	nonce := auth.Get("nonce")
	if err := g.validateMessageNonce(nonce, username, parseAddressIP(addrString(ctx.Source))); err != nil {
		return err
	}
	provided := strings.ToLower(strings.TrimSpace(auth.Get("response")))
	if provided == "" {
		return fmt.Errorf("digest response is missing")
	}
	auth.SetPassword(password)
	auth.SetUsername(username)
	auth.SetMethod(ctx.Request.Method())
	auth.SetURI(requestURI)
	calculated, err := auth.CalcResponseChecked()
	if err != nil {
		return err
	}
	if len(provided) != len(calculated) || subtle.ConstantTimeCompare([]byte(provided), []byte(calculated)) != 1 {
		return fmt.Errorf("digest auth failed")
	}
	return g.acceptMessageNonce(nonce, auth.Get("nc"), registerRequestFingerprint(ctx.Request, provided))
}

func (g *GB28181API) issueMessageNonce(deviceID, sourceIP string) string {
	now := time.Now()
	g.messageNonceMu.Lock()
	defer g.messageNonceMu.Unlock()
	if g.messageNonces == nil {
		g.messageNonces = make(map[string]messageNonceState)
	}
	var oldestKey string
	var oldest time.Time
	for nonce, state := range g.messageNonces {
		if !state.Expires.After(now) {
			delete(g.messageNonces, nonce)
			continue
		}
		if oldestKey == "" || state.IssuedAt.Before(oldest) {
			oldestKey = nonce
			oldest = state.IssuedAt
		}
	}
	if len(g.messageNonces) >= maxMessageNonces && oldestKey != "" {
		delete(g.messageNonces, oldestKey)
	}
	for {
		nonce := sip.RandString(32)
		if _, exists := g.messageNonces[nonce]; exists {
			continue
		}
		g.messageNonces[nonce] = messageNonceState{
			DeviceID: strings.TrimSpace(deviceID), SourceIP: strings.TrimSpace(sourceIP),
			IssuedAt: now, Expires: now.Add(messageNonceTTL),
		}
		return nonce
	}
}

func (g *GB28181API) validateMessageNonce(nonce, deviceID, sourceIP string) error {
	now := time.Now()
	g.messageNonceMu.Lock()
	defer g.messageNonceMu.Unlock()
	state, ok := g.messageNonces[nonce]
	if !ok {
		return fmt.Errorf("Digest nonce was not issued by this server")
	}
	if !state.Expires.After(now) {
		delete(g.messageNonces, nonce)
		return fmt.Errorf("Digest nonce expired")
	}
	if state.DeviceID != strings.TrimSpace(deviceID) {
		return fmt.Errorf("Digest nonce device mismatch")
	}
	if state.SourceIP != "" && state.SourceIP != strings.TrimSpace(sourceIP) {
		return fmt.Errorf("Digest nonce source mismatch")
	}
	return nil
}

func (g *GB28181API) acceptMessageNonce(nonce, nc, fingerprint string) error {
	g.messageNonceMu.Lock()
	defer g.messageNonceMu.Unlock()
	state, ok := g.messageNonces[nonce]
	if !ok || !state.Expires.After(time.Now()) {
		delete(g.messageNonces, nonce)
		return fmt.Errorf("Digest nonce expired")
	}
	if nc == "" {
		if state.AcceptedFingerprint != "" && state.AcceptedFingerprint != fingerprint {
			return fmt.Errorf("Digest nonce replay detected")
		}
		state.AcceptedFingerprint = fingerprint
		g.messageNonces[nonce] = state
		return nil
	}
	count, err := strconv.ParseUint(nc, 16, 32)
	if err != nil || count == 0 {
		return fmt.Errorf("invalid Digest nc")
	}
	if uint32(count) < state.HighestNC || uint32(count) == state.HighestNC && state.AcceptedFingerprint != fingerprint {
		return fmt.Errorf("Digest nonce replay detected")
	}
	if uint32(count) > state.HighestNC {
		state.HighestNC = uint32(count)
		state.AcceptedFingerprint = fingerprint
		g.messageNonces[nonce] = state
	}
	return nil
}

type deviceCredential struct {
	DeviceID string
	Password string
	Address  string
}

func (g *GB28181API) lookupDeviceCredential(deviceID string) (*deviceCredential, error) {
	if g != nil && g.svr != nil && g.svr.memoryStorer != nil {
		if dev, ok := g.svr.memoryStorer.Load(deviceID); ok && dev != nil {
			state := dev.runtimeSnapshot()
			return &deviceCredential{
				DeviceID: strings.TrimSpace(deviceID),
				Password: state.Password,
				Address:  strings.TrimSpace(state.Address),
			}, nil
		}
	}
	var dev ipc.Device
	if err := g.core.Store().Device().Get(g.serviceContext(), &dev, orm.Where("device_id=?", deviceID)); err != nil {
		return nil, err
	}
	return &deviceCredential{
		DeviceID: strings.TrimSpace(dev.GetGB28181DeviceID()),
		Password: dev.Password,
		Address:  strings.TrimSpace(dev.Address),
	}, nil
}

func parseAddressIP(address string) string {
	address = strings.TrimSpace(address)
	if address == "" {
		return ""
	}
	// 去除可能存在的协议前缀。
	address = strings.TrimPrefix(address, "udp://")
	address = strings.TrimPrefix(address, "tcp://")
	address = strings.TrimPrefix(address, "tls://")
	host, _, err := net.SplitHostPort(address)
	if err == nil {
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
		}
		return host
	}
	// 非 host:port 场景，退化为原值。
	return address
}

func parseComparableIP(address string) net.IP {
	value := parseAddressIP(address)
	if zone := strings.LastIndexByte(value, '%'); zone >= 0 {
		value = value[:zone]
	}
	return net.ParseIP(value)
}

func addrString(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	return addr.String()
}
