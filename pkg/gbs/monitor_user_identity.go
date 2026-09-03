package gbs

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	monitorUserIdentityHeaderName = "Monitor-User-Identity"
	monitorUserIdentityCacheKey   = "gb28181.monitor_user_identity"
	monitorUserIdentityMaxLength  = 2048
)

type monitorUserIdentityContextKey struct{}
type monitorUserIdentityGatewayContextKey struct{}

// monitorUserIdentity 按标准从右向左固定解析用户 ID、机构、类别、职级，
// 左侧保留一个或多个逐级前置的信令安全路由网关 ID。
type monitorUserIdentity struct {
	Gateways     []string
	UserID       string
	Organization string
	Category     string
	Rank         string
}

func (identity *monitorUserIdentity) clone() *monitorUserIdentity {
	if identity == nil {
		return nil
	}
	cloned := *identity
	cloned.Gateways = append([]string(nil), identity.Gateways...)
	return &cloned
}

func (identity *monitorUserIdentity) String() string {
	if identity == nil {
		return ""
	}
	parts := make([]string, 0, len(identity.Gateways)+4)
	parts = append(parts, identity.Gateways...)
	parts = append(parts, identity.UserID, identity.Organization, identity.Category, identity.Rank)
	return strings.Join(parts, "-")
}

type monitorUserIdentityPolicy struct {
	required       bool
	localGatewayID string
	remoteGateway  string
	local          *monitorUserIdentity
	trusted        map[string]struct{}
	allowedUsers   map[string]struct{}
	allowedOrgs    map[string]struct{}
	allowedClasses map[string]struct{}
	allowedRanks   map[string]struct{}
	maxHops        int
}

type monitorUserIdentityMessageSecurity struct {
	policy   *monitorUserIdentityPolicy
	mu       sync.RWMutex
	identity *monitorUserIdentity
}

func (security *monitorUserIdentityMessageSecurity) Verify(message sip.Message) error {
	if security == nil || security.policy == nil {
		return nil
	}
	value, present, err := monitorUserIdentityHeader(message)
	if err != nil {
		return err
	}
	if !present {
		if security.policy.required {
			return fmt.Errorf("Monitor-User-Identity is required")
		}
		security.mu.RLock()
		verified := security.identity != nil
		security.mu.RUnlock()
		if verified {
			return fmt.Errorf("Monitor-User-Identity changed after initial verification")
		}
		return nil
	}
	identity, err := parseMonitorUserIdentity(value)
	if err != nil {
		return err
	}
	security.mu.RLock()
	if security.identity != nil && security.identity.String() != value {
		security.mu.RUnlock()
		return fmt.Errorf("Monitor-User-Identity changed after initial verification")
	}
	security.mu.RUnlock()
	if err := security.policy.validateInbound(identity); err != nil {
		return err
	}
	security.mu.Lock()
	if security.identity != nil && security.identity.String() != value {
		security.mu.Unlock()
		return fmt.Errorf("Monitor-User-Identity changed after initial verification")
	}
	security.identity = identity
	security.mu.Unlock()
	return nil
}

func (security *monitorUserIdentityMessageSecurity) Sign(message sip.Message) error {
	if security == nil || security.policy == nil || message == nil {
		return nil
	}
	security.mu.RLock()
	verifiedIdentity := security.identity.clone()
	security.mu.RUnlock()
	identity, err := security.policy.outgoing(verifiedIdentity)
	if err != nil {
		return err
	}
	message.RemoveHeader(monitorUserIdentityHeaderName)
	message.AppendHeader(&sip.GenericHeader{HeaderName: monitorUserIdentityHeaderName, Contents: identity.String()})
	return nil
}

func newMonitorUserIdentityPolicy(config conf.SIPMonitorUserIdentity) (*monitorUserIdentityPolicy, error) {
	if !config.Active() {
		return nil, nil
	}
	if err := conf.ValidateMonitorUserIdentityConfig(config); err != nil {
		return nil, err
	}
	maxHops := config.MaxHops
	if maxHops == 0 {
		maxHops = 8
	}
	policy := &monitorUserIdentityPolicy{
		required:       config.Required,
		localGatewayID: strings.TrimSpace(config.LocalGatewayID),
		remoteGateway:  strings.TrimSpace(config.RemoteGatewayID),
		local: &monitorUserIdentity{
			Gateways:     []string{strings.TrimSpace(config.LocalGatewayID)},
			UserID:       strings.TrimSpace(config.LocalUserID),
			Organization: strings.TrimSpace(config.LocalOrganization),
			Category:     strings.TrimSpace(config.LocalCategory),
			Rank:         strings.TrimSpace(config.LocalRank),
		},
		trusted:        stringSet(config.TrustedGatewayIDs),
		allowedUsers:   stringSet(config.AllowedUserIDs),
		allowedOrgs:    stringSet(config.AllowedOrganizations),
		allowedClasses: stringSet(config.AllowedCategories),
		allowedRanks:   stringSet(config.AllowedRanks),
		maxHops:        maxHops,
	}
	return policy, nil
}

func stringSet(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[strings.TrimSpace(value)] = struct{}{}
	}
	return set
}

func parseMonitorUserIdentity(value string) (*monitorUserIdentity, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("Monitor-User-Identity is empty")
	}
	if len(value) > monitorUserIdentityMaxLength || !utf8.ValidString(value) {
		return nil, fmt.Errorf("Monitor-User-Identity is too long or not valid UTF-8")
	}
	parts := strings.Split(value, "-")
	if len(parts) < 5 {
		return nil, fmt.Errorf("Monitor-User-Identity requires gateway, user, organization, category and rank segments")
	}
	for _, part := range parts {
		if part == "" || strings.TrimSpace(part) != part || containsSIPHeaderControl(part) {
			return nil, fmt.Errorf("Monitor-User-Identity contains an invalid segment")
		}
	}
	identityIndex := len(parts) - 4
	identity := &monitorUserIdentity{
		Gateways:     append([]string(nil), parts[:identityIndex]...),
		UserID:       parts[identityIndex],
		Organization: parts[identityIndex+1],
		Category:     parts[identityIndex+2],
		Rank:         parts[identityIndex+3],
	}
	for _, gatewayID := range identity.Gateways {
		if !isMonitorIdentityCodeType(gatewayID, 211, 211) {
			return nil, fmt.Errorf("Monitor-User-Identity gateway %q is not a type 211 GB code", gatewayID)
		}
	}
	if !isMonitorIdentityCodeType(identity.UserID, 300, 499) {
		return nil, fmt.Errorf("Monitor-User-Identity user %q is not a type 300-499 GB code", identity.UserID)
	}
	for _, attribute := range []string{identity.Organization, identity.Category, identity.Rank} {
		if len(attribute) > 64 {
			return nil, fmt.Errorf("Monitor-User-Identity attribute is too long")
		}
	}
	return identity, nil
}

func containsSIPHeaderControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func isMonitorIdentityCodeType(value string, minimum, maximum int) bool {
	if len(value) != 20 {
		return false
	}
	for index := range value {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	deviceType, err := strconv.Atoi(value[10:13])
	return err == nil && deviceType >= minimum && deviceType <= maximum
}

func (policy *monitorUserIdentityPolicy) validateInbound(identity *monitorUserIdentity) error {
	if policy == nil || identity == nil {
		return fmt.Errorf("Monitor-User-Identity policy or value is unavailable")
	}
	if len(identity.Gateways) == 0 || len(identity.Gateways) > policy.maxHops {
		return fmt.Errorf("Monitor-User-Identity gateway hop count is invalid")
	}
	if identity.Gateways[0] != policy.remoteGateway {
		return fmt.Errorf("Monitor-User-Identity immediate gateway mismatch")
	}
	seen := make(map[string]struct{}, len(identity.Gateways))
	for index, gatewayID := range identity.Gateways {
		if gatewayID == policy.localGatewayID {
			return fmt.Errorf("Monitor-User-Identity routing loop detected")
		}
		if _, exists := seen[gatewayID]; exists {
			return fmt.Errorf("Monitor-User-Identity contains a repeated gateway")
		}
		seen[gatewayID] = struct{}{}
		if index > 0 {
			if _, trusted := policy.trusted[gatewayID]; !trusted {
				return fmt.Errorf("Monitor-User-Identity contains an untrusted gateway")
			}
		}
	}
	if !allowedMonitorIdentityValue(policy.allowedUsers, identity.UserID) {
		return fmt.Errorf("Monitor-User-Identity user is not allowed")
	}
	if !allowedMonitorIdentityValue(policy.allowedOrgs, identity.Organization) {
		return fmt.Errorf("Monitor-User-Identity organization is not allowed")
	}
	if !allowedMonitorIdentityValue(policy.allowedClasses, identity.Category) {
		return fmt.Errorf("Monitor-User-Identity category is not allowed")
	}
	if !allowedMonitorIdentityValue(policy.allowedRanks, identity.Rank) {
		return fmt.Errorf("Monitor-User-Identity rank is not allowed")
	}
	return nil
}

func allowedMonitorIdentityValue(allowed map[string]struct{}, value string) bool {
	if len(allowed) == 0 {
		return true
	}
	_, ok := allowed[value]
	return ok
}

func (policy *monitorUserIdentityPolicy) outgoing(identity *monitorUserIdentity) (*monitorUserIdentity, error) {
	if policy == nil {
		return nil, nil
	}
	if identity == nil {
		return policy.local.clone(), nil
	}
	if err := policy.validateInbound(identity); err != nil {
		return nil, err
	}
	if len(identity.Gateways)+1 > policy.maxHops {
		return nil, fmt.Errorf("Monitor-User-Identity exceeds configured gateway hop limit")
	}
	forwarded := identity.clone()
	forwarded.Gateways = append([]string{policy.localGatewayID}, forwarded.Gateways...)
	return forwarded, nil
}

func monitorUserIdentityHeader(message sip.Message) (string, bool, error) {
	if message == nil {
		return "", false, fmt.Errorf("nil SIP message")
	}
	headers := message.GetHeaders(monitorUserIdentityHeaderName)
	if len(headers) == 0 {
		return "", false, nil
	}
	if len(headers) != 1 || headers[0] == nil {
		return "", false, fmt.Errorf("exactly one Monitor-User-Identity header is required")
	}
	value := headers[0].String()
	if _, after, ok := strings.Cut(value, ":"); ok {
		value = after
	}
	return strings.TrimSpace(value), true, nil
}

func withMonitorUserIdentity(ctx context.Context, identity *monitorUserIdentity) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if identity == nil {
		return ctx
	}
	return context.WithValue(ctx, monitorUserIdentityContextKey{}, identity.clone())
}

func withMonitorUserIdentityRoute(ctx context.Context, identity *monitorUserIdentity, localGatewayID string) context.Context {
	ctx = withMonitorUserIdentity(ctx, identity)
	localGatewayID = strings.TrimSpace(localGatewayID)
	if identity == nil || localGatewayID == "" {
		return ctx
	}
	return context.WithValue(ctx, monitorUserIdentityGatewayContextKey{}, localGatewayID)
}

func monitorUserIdentityFromContext(ctx context.Context) *monitorUserIdentity {
	if ctx == nil {
		return nil
	}
	identity, _ := ctx.Value(monitorUserIdentityContextKey{}).(*monitorUserIdentity)
	return identity.clone()
}

func monitorUserIdentityContext(ctx *sip.Context) context.Context {
	return monitorUserIdentityContextWithParent(context.Background(), ctx)
}

func monitorUserIdentityContextWithParent(parent context.Context, ctx *sip.Context) context.Context {
	if parent == nil {
		parent = context.Background()
	}
	if ctx == nil {
		return parent
	}
	value, ok := ctx.Get(monitorUserIdentityCacheKey)
	if !ok {
		return parent
	}
	identity, _ := value.(*monitorUserIdentity)
	localGatewayID := ""
	if workerValue, exists := ctx.Get(cascadeWorkerContextKey); exists {
		if worker, valid := workerValue.(*cascadeWorker); valid && worker != nil && worker.platform.monitorUserIdentity != nil {
			localGatewayID = worker.platform.monitorUserIdentity.localGatewayID
		}
	}
	return withMonitorUserIdentityRoute(parent, identity, localGatewayID)
}

// applyForwardedMonitorUserIdentity 将已验证的跨域用户身份继续传给下行目标，
// 并按 8.3 要求只在原值最前面追加当前安全路由网关 ID。
func applyForwardedMonitorUserIdentity(ctx context.Context, request *sip.Request) error {
	identity := monitorUserIdentityFromContext(ctx)
	if identity == nil || request == nil {
		return nil
	}
	localGatewayID, _ := ctx.Value(monitorUserIdentityGatewayContextKey{}).(string)
	localGatewayID = strings.TrimSpace(localGatewayID)
	if localGatewayID == "" {
		return fmt.Errorf("Monitor-User-Identity forwarding gateway is unavailable")
	}
	for _, gatewayID := range identity.Gateways {
		if gatewayID == localGatewayID {
			return fmt.Errorf("Monitor-User-Identity routing loop detected")
		}
	}
	identity.Gateways = append([]string{localGatewayID}, identity.Gateways...)
	request.RemoveHeader(monitorUserIdentityHeaderName)
	request.AppendHeader(&sip.GenericHeader{HeaderName: monitorUserIdentityHeaderName, Contents: identity.String()})
	return nil
}

func (policy *monitorUserIdentityPolicy) apply(ctx context.Context, request *sip.Request) error {
	if policy == nil || request == nil {
		return nil
	}
	identity, err := policy.outgoing(monitorUserIdentityFromContext(ctx))
	if err != nil {
		return err
	}
	request.RemoveHeader(monitorUserIdentityHeaderName)
	request.AppendHeader(&sip.GenericHeader{HeaderName: monitorUserIdentityHeaderName, Contents: identity.String()})
	return nil
}

// sipMonitorUserIdentityMiddleware 在已注册上级平台的信令边界验证身份路径和访问属性。
// 非级联设备及未启用该能力的平台保持原有行为。
func (g *GB28181API) sipMonitorUserIdentityMiddleware(ctx *sip.Context) {
	if ctx == nil {
		return
	}
	if g == nil || ctx.Request == nil || g.svr == nil || g.svr.cascade == nil {
		ctx.Next()
		return
	}
	worker, ok := g.svr.cascade.matchRegistered(ctx.DeviceID, ctx.Source, ctx.Request.GetConnection())
	if !ok || worker == nil || worker.platform.monitorUserIdentity == nil {
		ctx.Next()
		return
	}
	policy := worker.platform.monitorUserIdentity
	value, present, err := monitorUserIdentityHeader(ctx.Request)
	if err != nil {
		ctx.AbortString(http.StatusBadRequest, err.Error())
		return
	}
	if !present {
		if policy.required {
			ctx.AbortString(http.StatusForbidden, "Monitor-User-Identity is required")
			return
		}
		ctx.Next()
		return
	}
	identity, err := parseMonitorUserIdentity(value)
	if err == nil {
		err = policy.validateInbound(identity)
	}
	if err != nil {
		ctx.AbortString(http.StatusForbidden, err.Error())
		return
	}
	ctx.Set(monitorUserIdentityCacheKey, identity)
	ctx.Set(cascadeWorkerContextKey, worker)
	ctx.Next()
}
