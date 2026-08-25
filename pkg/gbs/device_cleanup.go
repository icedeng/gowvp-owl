package gbs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gowvp/owl/pkg/zlm"
)

// CleanupDevice 在删除持久化设备前，释放该设备关联的媒体和协议运行态。
func (s *Server) CleanupDevice(ctx context.Context, deviceID string) error {
	if s == nil || s.gb == nil {
		return nil
	}
	return s.gb.cleanupDevice(ctx, deviceID)
}

func (g *GB28181API) cleanupDevice(ctx context.Context, deviceID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return fmt.Errorf("GB28181 device ID is required")
	}

	cause := fmt.Errorf("GB28181 device %s deleted", deviceID)
	g.talkSessions.Range(func(_, value any) bool {
		session, _ := value.(*talkSession)
		if session == nil || session.DeviceID != deviceID {
			return true
		}
		if err := g.stopTalkSession(session, cause); err != nil {
			slog.WarnContext(ctx, "停止已删除设备的语音对讲失败", "device_id", deviceID, "err", err)
		}
		if g.streams != nil && session.Stream != nil {
			g.streams.CompareAndDelete(voiceKey(voiceModeTalk, session.DeviceID, session.ChannelID), session.Stream)
		}
		return true
	})
	g.broadcastSessions.Range(func(_, value any) bool {
		session, _ := value.(*broadcastSession)
		if session == nil || session.DeviceID != deviceID {
			return true
		}
		session.mu.Lock()
		dialog := session.Dialog
		session.mu.Unlock()
		if err := g.stopBroadcastSession(session, true); err != nil {
			slog.WarnContext(ctx, "停止已删除设备的语音广播失败", "device_id", deviceID, "err", err)
		}
		if dialog != nil {
			g.inviteDialogs.CompareAndDelete(dialog.CallID, dialog)
		}
		return true
	})

	if g.directDownloads != nil {
		g.directDownloads.CancelDevice(deviceID)
	}
	if g.streams != nil {
		g.streams.Range(func(key string, stream *Streams) bool {
			if stream == nil || stream.DeviceID != deviceID || !g.streams.CompareAndDelete(key, stream) {
				return true
			}
			if stream.DirectTCP && g.directDownloads != nil {
				g.directDownloads.Cancel(stream.DirectSessionID)
			}
			if strings.HasPrefix(key, "history:"+historyModeDownload+":") && !stream.DirectTCP {
				g.finishRTPDownload(stream, rtpDownloadStopped, "device_deleted")
			}
			stream.Stop = true
			stream.Status = 1
			stream.EndReason = "device_deleted"
			g.terminateCascadeSessionsForStream(stream)
			g.sendStreamBYE(stream)
			if stream.mediaServer != nil && g.sms != nil && strings.TrimSpace(stream.StreamID) != "" {
				if _, err := g.sms.CloseRTPServer(stream.mediaServer, zlm.CloseRTPServerRequest{StreamID: stream.StreamID}); err != nil {
					slog.WarnContext(ctx, "关闭已删除设备的 RTP 接收端口失败", "device_id", deviceID, "stream_id", stream.StreamID, "err", err)
				}
			}
			return true
		})
	}

	g.queryStates.Delete(deviceID)
	g.rtpDownloads.Range(func(key, value any) bool {
		session, _ := value.(*rtpDownloadSession)
		if session != nil && session.snapshot().DeviceID == deviceID {
			g.rtpDownloads.CompareAndDelete(key, value)
		}
		return true
	})
	g.upgradeStateMu.Lock()
	for key, state := range g.upgradeStates {
		if state.DeviceID == deviceID {
			delete(g.upgradeStates, key)
		}
	}
	g.upgradeStateMu.Unlock()
	g.snapshotStateMu.Lock()
	for key, state := range g.snapshotStates {
		if state.DeviceID == deviceID {
			delete(g.snapshotStates, key)
		}
	}
	g.snapshotStateMu.Unlock()
	g.registerNonceMu.Lock()
	for nonce, state := range g.registerNonces {
		if state.DeviceID == deviceID {
			delete(g.registerNonces, nonce)
		}
	}
	g.registerNonceMu.Unlock()
	g.messageNonceMu.Lock()
	for nonce, state := range g.messageNonces {
		if state.DeviceID == deviceID {
			delete(g.messageNonces, nonce)
		}
	}
	g.messageNonceMu.Unlock()
	g.registerResultMu.Lock()
	for key, state := range g.registerResults {
		if state.DeviceID == deviceID {
			delete(g.registerResults, key)
		}
	}
	g.registerResultMu.Unlock()
	g.outgoingSubscriptions.Range(func(key, value any) bool {
		keyText, ok := key.(string)
		if ok && strings.HasPrefix(keyText, deviceID+"|") {
			g.outgoingSubscriptions.CompareAndDelete(key, value)
		}
		return true
	})
	g.cascadeSubscriptionMu.Lock()
	for key, state := range g.cascadeSubscriptions {
		if state != nil && state.Input.DeviceID == deviceID {
			delete(g.cascadeSubscriptions, key)
		}
	}
	g.cascadeSubscriptionMu.Unlock()
	return nil
}
