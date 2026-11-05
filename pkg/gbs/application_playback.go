package gbs

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// ApplicationPlaybackStreamID 使用平台生成的UUID隔离每次回放，不复用管理端的通道会话。
func ApplicationPlaybackStreamID(id string) string { return "open-playback-" + id }

func applicationPlaybackKey(id string) (string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", fmt.Errorf("invalid application playback session ID")
	}
	return "application-playback:" + id, nil
}

func (s *Server) StartApplicationPlayback(ctx context.Context, id string, in HistoryInput) error {
	if s == nil || s.gb == nil {
		return fmt.Errorf("GB28181 server is unavailable")
	}
	key, err := applicationPlaybackKey(id)
	if err != nil {
		return err
	}
	in.Mode, in.sessionKey, in.streamID = historyModePlayback, key, ApplicationPlaybackStreamID(id)
	in.Transport, in.DownloadSpeed = historyTransportRTP, 0
	return s.StartHistory(ctx, &in)
}

func (s *Server) StopApplicationPlayback(ctx context.Context, id string, in StopHistoryInput) error {
	if s == nil || s.gb == nil {
		return fmt.Errorf("GB28181 server is unavailable")
	}
	key, err := applicationPlaybackKey(id)
	if err != nil {
		return err
	}
	// 重启后内存会话已不存在；媒体接收端由宿主按持久化的独立流ID另外关闭。
	if _, ok := s.gb.streams.Load(key); !ok {
		return nil
	}
	in.Mode, in.sessionKey = historyModePlayback, key
	return s.StopHistory(ctx, &in)
}

func (s *Server) ControlApplicationPlayback(ctx context.Context, id string, in ControlHistoryInput) error {
	if s == nil || s.gb == nil {
		return fmt.Errorf("GB28181 server is unavailable")
	}
	key, err := applicationPlaybackKey(id)
	if err != nil {
		return err
	}
	in.Mode, in.sessionKey, in.Cmd = historyModePlayback, key, ""
	return s.ControlHistory(ctx, &in)
}

func (s *Server) ApplicationPlaybackActive(id string) bool {
	key, err := applicationPlaybackKey(id)
	if err != nil || s == nil || s.gb == nil {
		return false
	}
	stream, ok := s.gb.streams.Load(key)
	return ok && !s.gb.mediaStreamStopping(stream)
}
