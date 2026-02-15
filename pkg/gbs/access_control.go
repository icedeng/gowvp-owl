package gbs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
)

var (
	errAuthHeaderMissing = errors.New("authorization header required")
)

// sipAccessControlMiddleware 为 MESSAGE/NOTIFY 提供访问控制中间件。
// 控制项：
// 1) strict_source_check：校验上报源 IP 是否与注册源一致；
// 2) require_message_auth：要求 MESSAGE/NOTIFY 携带 Digest 鉴权。
func (g *GB28181API) sipAccessControlMiddleware(ctx *sip.Context) {
	if err := g.checkSourceAddress(ctx); err != nil {
		ctx.AbortString(403, err.Error())
		return
	}
	if err := g.checkDigestAuth(ctx); err != nil {
		if errors.Is(err, errAuthHeaderMissing) {
			resp := sip.NewResponseFromRequest("", ctx.Request, 401, "Unauthorized", nil)
			resp.AppendHeader(&sip.GenericHeader{
				HeaderName: "WWW-Authenticate",
				Contents:   fmt.Sprintf(`Digest realm="%s",qop="auth",nonce="%s"`, g.cfg.Domain, sip.RandString(32)),
			})
			_ = ctx.Tx.Respond(resp)
			ctx.Abort()
			return
		}
		ctx.AbortString(401, err.Error())
		return
	}
	ctx.Next()
}

// checkSourceAddress 校验源地址（仅比较 IP，忽略端口变化）。
func (g *GB28181API) checkSourceAddress(ctx *sip.Context) error {
	if g == nil || g.cfg == nil || !g.cfg.StrictSourceCheck {
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
	if g == nil || g.cfg == nil || !g.cfg.RequireMessageAuth {
		return nil
	}
	cred, err := g.lookupDeviceCredential(ctx.DeviceID)
	if err != nil {
		return fmt.Errorf("device not found")
	}
	password := strings.TrimSpace(cred.Password)
	if password == "" {
		password = strings.TrimSpace(g.cfg.Password)
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
	auth.SetPassword(password)
	auth.SetUsername(cred.DeviceID)
	auth.SetMethod(ctx.Request.Method())
	auth.SetURI(auth.Get("uri"))
	if auth.CalcResponse() != auth.Get("response") {
		return fmt.Errorf("digest auth failed")
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
