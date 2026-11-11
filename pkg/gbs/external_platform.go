package gbs

import (
	"context"
	"fmt"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"net"
	"strings"
	"time"
)

type ExternalPlatformSpec struct {
	GatewayID, SubjectID, ServerID, Name, Transport string
	Target                                          net.Addr
	Expires                                         int
	Register                                        bool
	Cleanup                                         bool
}
type ExternalPlatformPort interface {
	OpenPlatform(context.Context, ExternalPlatformSpec) (sip.Connection, error)
}

type ExternalPlatformBindingPort interface {
	AuthorizePlatform(context.Context, ExternalPlatformSpec) error
	CommitPlatformRegistration(context.Context, ExternalPlatformSpec, sip.Connection, int) error
}

func (s *Server) SetExternalPlatformPort(p ExternalPlatformPort) {
	s.externalMedia.Lock()
	s.externalMedia.platform = p
	s.externalMedia.Unlock()
}
func (s *Server) externalPlatformPort() ExternalPlatformPort {
	if s == nil {
		return nil
	}
	s.externalMedia.RLock()
	defer s.externalMedia.RUnlock()
	return s.externalMedia.platform
}

// AuthorizeExternalPlatform 保留原级联鉴权，先验证配置选择的入口，防止同 IP 的不同网关相互冒用。
func (s *Server) AuthorizeExternalPlatform(serverID string, conn sip.Connection) (bool, error) {
	if s == nil || s.cascade == nil {
		return false, nil
	}
	s.cascade.mu.RLock()
	defer s.cascade.mu.RUnlock()
	for _, w := range s.cascade.items {
		if w.platform.serverID != serverID {
			continue
		}
		if w.platform.gatewayID == "" {
			if sip.IsExternal(conn) {
				return true, fmt.Errorf("级联平台只允许配置的直连入口")
			}
			return true, nil
		}
		routed, ok := conn.(interface{ GatewayID() string })
		if !ok || routed.GatewayID() != w.platform.gatewayID {
			return true, fmt.Errorf("级联平台入口与配置不匹配")
		}
		w.mu.RLock()
		scope := w.externalRegisteredScope
		w.mu.RUnlock()
		// 大报文允许在同一已认证代次内切换 TCP flow；代次变化必须重新 REGISTER。
		currentScope := sip.RoutingScope(conn)
		boundary := strings.LastIndex(scope, "/")
		if boundary < 0 || !strings.HasPrefix(currentScope, scope[:boundary+1]) {
			return true, fmt.Errorf("级联平台尚未在当前网关代次重新注册")
		}
		if !w.remoteAddressMatches(conn.RemoteAddr()) || !w.registrationActive(time.Now()) {
			return true, fmt.Errorf("级联平台尚未在当前入口注册")
		}
		return true, nil
	}
	return false, nil
}

func (s *Server) ExternalPlatformSpec(serverID string) (ExternalPlatformSpec, bool) {
	if s == nil || s.cascade == nil {
		return ExternalPlatformSpec{}, false
	}
	s.cascade.mu.RLock()
	defer s.cascade.mu.RUnlock()
	for _, w := range s.cascade.items {
		if w.platform.serverID == serverID {
			return w.externalPlatformSpec(false), true
		}
	}
	return ExternalPlatformSpec{}, false
}

func (s *Server) StopExternalPlatformMedia(ctx context.Context, subject, stream, ssrc string) error {
	return s.stopExternalPlatformMedia(ctx, subject, stream, ssrc, false)
}

func (s *Server) DrainExternalPlatform(ctx context.Context, subject string) error {
	return s.stopExternalPlatformMedia(ctx, subject, "", "", true)
}

func (s *Server) stopExternalPlatformMedia(ctx context.Context, subject, stream, ssrc string, all bool) error {
	if s == nil || s.gb == nil {
		return nil
	}
	g := s.gb
	var result error
	g.inviteDialogs.Range(func(_, v any) bool {
		d, ok := v.(*inboundInviteDialog)
		if !ok || d.Cascade == nil {
			return true
		}
		session := d.Cascade
		if session.worker != nil && session.worker.platform.subjectID == subject && (all || session.stream == stream && session.ssrc == ssrc) {
			g.stopCascadeMediaSession(session, true, false)
			session.stopMu.Lock()
			complete := session.rtpStopped && session.sourceReleased
			session.stopMu.Unlock()
			if !complete {
				result = fmt.Errorf("级联网关媒体清理尚未完成")
			}
		}
		return true
	})
	g.cascadeVoiceDialogs.Range(func(_, v any) bool {
		source, ok := v.(*cascadeVoiceSourceSession)
		if ok && source.worker != nil && source.worker.platform.subjectID == subject && (all || source.streamID == stream) {
			if err := g.stopCascadeVoiceSource(source, true); err != nil {
				result = err
			}
		}
		return true
	})
	return result
}

func (w *cascadeWorker) externalPlatformSpec(register bool) ExternalPlatformSpec {
	return ExternalPlatformSpec{GatewayID: w.platform.gatewayID, SubjectID: w.platform.subjectID, ServerID: w.platform.serverID, Name: w.platform.name, Target: w.platform.remote, Transport: w.platform.transport, Expires: w.platform.expires, Register: register}
}
