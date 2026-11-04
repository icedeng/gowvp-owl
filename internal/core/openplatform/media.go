package openplatform

import (
	"context"
	"golang.org/x/time/rate"
	"time"
)

func (s *Service) AcquireConnection(p Principal, kind string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connections == nil {
		s.connections = map[string]int{}
	}
	key := p.App.ID + ":" + kind
	limit := p.App.LiveLimit * 4
	if kind == "download" || kind == "recording" {
		limit = p.App.DownloadLimit
	}
	if kind == "event" || kind == "snapshot" {
		limit = 10
	}
	if s.connections[key] >= limit {
		return nil, Fail(429, "CONCURRENT_LIMIT", "媒体连接超过配额")
	}
	s.connections[key]++
	return func() {
		s.mu.Lock()
		s.connections[key]--
		if s.connections[key] == 0 {
			delete(s.connections, key)
		}
		s.mu.Unlock()
	}, nil
}
func (s *Service) WaitBytes(ctx context.Context, app string, n int) error {
	s.mu.Lock()
	if s.bandwidth == nil {
		s.bandwidth = map[string]*rate.Limiter{}
	}
	limiter := s.bandwidth[app]
	if limiter == nil {
		if len(s.bandwidth) > 10000 {
			s.mu.Unlock()
			return ErrUnavailable
		}
		limiter = rate.NewLimiter(10*1024*1024, 1024*1024)
		s.bandwidth[app] = limiter
	}
	s.mu.Unlock()
	return limiter.WaitN(ctx, n)
}
func (s *Service) CheckActive(t Ticket) error {
	var current Ticket
	if e := s.DB.First(&current, "hash = ? AND revoked = ?", t.Hash, false).Error; e != nil {
		return ErrAuth
	}
	p, e := s.Current(t.AppID, t.CredentialID)
	if e != nil || p.App.Revision != t.Revision {
		return ErrAuth
	}
	if t.ChannelID != "" {
		if _, e = s.Channel(p.App, t.ChannelID); e != nil {
			return e
		}
	} else if !s.AllowedDevice(p.App, t.DeviceID) {
		return ErrAuth
	}
	if t.Kind == "snapshot" {
		var row Snapshot
		if s.DB.First(&row, "id = ? AND state = ? AND expires_at > ?", t.ResourceID, "succeeded", time.Now()).Error != nil {
			return ErrAuth
		}
		if _, e = s.Current(row.AppID, row.CredentialID); e != nil {
			return e
		}
	}
	if t.Kind == "live" {
		var row Session
		if s.DB.First(&row, "id = ? AND state = ? AND expires_at > ?", t.SessionID, "ready", time.Now()).Error != nil {
			return ErrAuth
		}
		if _, e = s.Current(row.AppID, row.CredentialID); e != nil {
			return e
		}
	}
	return nil
}
