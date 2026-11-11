package gbs

import (
	"context"
	"fmt"
	"sync"

	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/zlm"
)

// ExternalMediaPort 只表达宿主业务需要的传输职责，不依赖网关协议和持久化模型。
type ExternalMediaPort interface {
	CheckNewBusiness(context.Context, string, int8, string) (bool, error)
	PrepareReceive(context.Context, ExternalReceiveSpec) (ExternalMediaLease, error)
	SSRCAllowed(context.Context, string) (bool, error)
}
type ExternalReceiveSpec struct {
	SourceIPs                                    []string
	SendVHost, SendApp, SendStream               string
	Sending                                      bool
	SubjectType, SubjectID, GatewayID            string
	SendSSRC                                     *string
	Direction                                    string
	DeviceID, ChannelID, StreamID, Purpose, SSRC string
	WireMode                                     int8
	TCPRTCP                                      bool
	MediaServer                                  *sms.MediaServer
	Port, RTCPPort                               int
}
type ExternalSendLease interface {
	ExternalMediaLease
	LocalSendEndpoint() (string, int)
	SetLocalSender(context.Context, int, int) error
	ConnectDestination(context.Context, string, int, string, int) error
	PublicRTCP() (string, int)
}
type ExternalMediaLease interface {
	AdvertisedEndpoint() (string, int)
	ConnectRemote(context.Context, string, int) error
	Close(context.Context) error
}
type externalMediaState struct {
	sync.RWMutex
	port     ExternalMediaPort
	platform ExternalPlatformPort
}

func (s *Server) SetExternalMediaPort(port ExternalMediaPort) {
	s.externalMedia.Lock()
	s.externalMedia.port = port
	s.externalMedia.Unlock()
}
func (g *GB28181API) externalMediaPort() ExternalMediaPort {
	if g == nil || g.svr == nil {
		return nil
	}
	g.svr.externalMedia.RLock()
	defer g.svr.externalMedia.RUnlock()
	return g.svr.externalMedia.port
}
func (g *GB28181API) externalLocalMode(ctx context.Context, device string, mode int8, purpose string) (int8, error) {
	if p := g.externalMediaPort(); p != nil {
		external, err := p.CheckNewBusiness(ctx, device, mode, purpose)
		if err != nil {
			return mode, err
		}
		if external && mode == 2 {
			return 1, nil
		}
	}
	return mode, nil
}
func (g *GB28181API) prepareExternalReceive(ctx context.Context, stream *Streams, mode int8, purpose string, tcpRTCP bool, local *zlm.OpenRTPServerResponse) error {
	p := g.externalMediaPort()
	if p == nil {
		return nil
	}
	external, gateErr := p.CheckNewBusiness(ctx, stream.DeviceID, mode, purpose)
	if gateErr != nil {
		return gateErr
	}
	if !external {
		return nil
	}
	if tcpRTCP && (local.TCPRTCP == nil || !*local.TCPRTCP) {
		return fmt.Errorf("媒体节点未确认 TCP RTCP 复用补丁能力")
	}
	lease, err := p.PrepareReceive(ctx, ExternalReceiveSpec{SendSSRC: stream.externalSendSSRC, Direction: stream.externalDirection, DeviceID: stream.DeviceID, ChannelID: stream.ChannelID, StreamID: stream.StreamID, Purpose: purpose, SSRC: stream.ssrc, WireMode: mode, TCPRTCP: tcpRTCP, MediaServer: stream.mediaServer, Port: local.Port, RTCPPort: local.RTCPPort})
	if lease != nil {
		stream.externalMedia = lease
	}
	return err
}
func (g *GB28181API) mediaSDPEndpoint(stream *Streams, ip string, port int) (string, int) {
	if stream != nil && stream.externalMedia != nil {
		return stream.externalMedia.AdvertisedEndpoint()
	}
	return ip, port
}

// StopExternalMediaStream 以原节点和流快照定位，清理不会碰触相同通道后来创建的新会话。
func (s *Server) StopExternalMediaStream(ctx context.Context, device, streamID string) error {
	if s == nil || s.gb == nil || s.gb.streams == nil {
		return nil
	}
	var result error
	s.gb.talkSessions.Range(func(_, v any) bool {
		if session, ok := v.(*talkSession); ok && session.DeviceID == device && session.ReceiveStream == streamID {
			if e := s.gb.stopTalkSession(session, fmt.Errorf("gateway_stopped")); e != nil {
				result = e
			}
		}
		return true
	})
	s.gb.broadcastSessions.Range(func(_, v any) bool {
		if session, ok := v.(*broadcastSession); ok && session.DeviceID == device && session.Stream != nil && session.Stream.StreamID == streamID {
			if e := s.gb.stopBroadcastSession(session, true); e != nil {
				result = e
			}
		}
		return true
	})
	s.gb.streams.Range(func(key string, stream *Streams) bool {
		if stream != nil && stream.DeviceID == device && stream.StreamID == streamID {
			s.gb.terminateCascadeSessionsForStream(stream)
			s.gb.markMediaStreamStopped(stream, "gateway_stopped", false)
			complete, err := s.gb.cleanupMediaStreamContext(ctx, key, stream)
			if err != nil {
				result = err
			} else if !complete {
				result = fmt.Errorf("网关媒体清理尚未完成")
			}
		}
		return true
	})
	return result
}

// DrainExternalSubject 复用原设备收尾状态机；仍有索引即表示资源未确认释放。
func (s *Server) DrainExternalSubject(ctx context.Context, device string) (bool, error) {
	if s == nil || s.gb == nil {
		return false, fmt.Errorf("国标服务未就绪")
	}
	g := s.gb
	g.cancelPendingDeviceOperations(device, fmt.Errorf("设备正在迁移"))
	g.terminateMediaSessions(ctx, device, nil, "ingress_migration")
	pending := false
	if g.streams != nil {
		g.streams.Range(func(_ string, v *Streams) bool {
			if v != nil && v.DeviceID == device {
				pending = true
			}
			return true
		})
	}
	g.talkSessions.Range(func(_, v any) bool {
		if session, ok := v.(*talkSession); ok && session.DeviceID == device {
			pending = true
		}
		return true
	})
	g.broadcastSessions.Range(func(_, v any) bool {
		if session, ok := v.(*broadcastSession); ok && session.DeviceID == device {
			pending = true
		}
		return true
	})
	return !pending, nil
}

func (s *Server) ExternalStreamReferences(device, streamID string) int {
	if s == nil || s.gb == nil {
		return 0
	}
	g := s.gb
	g.cascadeMediaMu.Lock()
	defer g.cascadeMediaMu.Unlock()
	count := 0
	for _, source := range g.cascadeSources {
		if source != nil && source.stream != nil && source.stream.DeviceID == device && source.stream.StreamID == streamID {
			count += source.refs
		}
	}
	return count
}
