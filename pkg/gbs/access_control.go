package gbs

import (
	"context"
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
	errAuthHeaderMissing = errors.New("authorization header required")
)

const cascadeWorkerContextKey = "gb28181.cascade.worker"

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

// sipAccessControlMiddleware 为 MESSAGE/NOTIFY 提供访问控制中间件。
// 控制项：
// 1) strict_source_check：校验上报源 IP 是否与注册源一致；
// 2) require_message_auth：要求 MESSAGE/NOTIFY 携带 Digest 鉴权。
func (g *GB28181API) sipAccessControlMiddleware(ctx *sip.Context) {
	if g != nil && g.svr != nil && g.svr.cascade != nil {
		if worker, ok := g.svr.cascade.matchRegistered(ctx.DeviceID, ctx.Source); ok {
			ctx.Set(cascadeWorkerContextKey, worker)
			ctx.XGBVer = string(worker.protocolVersion())
			ctx.XGBVerRaw = ctx.XGBVer
			ctx.Next()
			return
		}
	}
	if err := g.checkSourceAddress(ctx); err != nil {
		ctx.AbortString(403, err.Error())
		return
	}
	if err := g.checkDigestAuth(ctx); err != nil {
		g.respondMessageDigestChallenge(ctx)
		return
	}
	ctx.Next()
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
	srcIP := parseAddressIP(addrString(ctx.Source))
	if srcIP == "" {
		return nil
	}
	cred, err := g.lookupDeviceCredential(ctx.DeviceID)
	if err != nil {
		return fmt.Errorf("device not found")
	}
	expectedIP := parseAddressIP(cred.Address)
	if expectedIP == "" {
		return nil
	}
	if srcIP != expectedIP {
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
	password := strings.TrimSpace(cred.Password)
	if password == "" {
		password = strings.TrimSpace(cfg.Password)
	}
	// ignorePassword 表示免鉴权，保持与 REGISTER 逻辑一致。
	if password == "" || password == ignorePassword {
		return nil
	}
	hdrs := ctx.Request.GetHeaders("Authorization")
	if len(hdrs) == 0 {
		return errAuthHeaderMissing
	}
	h, ok := hdrs[0].(*sip.GenericHeader)
	if !ok {
		return fmt.Errorf("invalid authorization header")
	}
	auth := sip.AuthFromValue(h.Contents)
	if auth.Get("realm") != cfg.GetDomain() {
		return fmt.Errorf("digest realm mismatch")
	}
	if !strings.EqualFold(auth.Algorithm(), registerDigestAlgo) {
		return fmt.Errorf("unsupported Digest algorithm %q", auth.Algorithm())
	}
	if auth.Get("username") != cred.DeviceID {
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
	if err := g.validateMessageNonce(nonce, cred.DeviceID, parseAddressIP(addrString(ctx.Source))); err != nil {
		return err
	}
	provided := strings.ToLower(strings.TrimSpace(auth.Get("response")))
	if provided == "" {
		return fmt.Errorf("digest response is missing")
	}
	auth.SetPassword(password)
	auth.SetUsername(cred.DeviceID)
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
			return &deviceCredential{
				DeviceID: strings.TrimSpace(deviceID),
				Password: strings.TrimSpace(dev.Password),
				Address:  strings.TrimSpace(dev.Address),
			}, nil
		}
	}
	var dev ipc.Device
	if err := g.core.Store().Device().Get(context.TODO(), &dev, orm.Where("device_id=?", deviceID)); err != nil {
		return nil, err
	}
	return &deviceCredential{
		DeviceID: strings.TrimSpace(dev.GetGB28181DeviceID()),
		Password: strings.TrimSpace(dev.Password),
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

func addrString(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	return addr.String()
}
