package openplatform

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
)

type playbackTestHost struct {
	Host
	starts, stops, controls int
	failStart, failStop     bool
	duringStart             func()
	lastStopped             Playback
}

func (h *playbackTestHost) PreparePlayback(_ context.Context, row Playback) (Source, error) {
	return Source{MediaServerID: "bound-node", App: "rtp", Stream: row.ID, BaseURL: "http://private.invalid"}, nil
}
func (h *playbackTestHost) StartPlayback(context.Context, Playback) error {
	h.starts++
	if h.duringStart != nil {
		h.duringStart()
	}
	if h.failStart {
		return errors.New("private detail")
	}
	return nil
}
func (h *playbackTestHost) StopPlayback(_ context.Context, row Playback) error {
	h.stops++
	h.lastStopped = row
	if h.failStop {
		return errors.New("failed")
	}
	return nil
}
func (h *playbackTestHost) ControlPlayback(context.Context, Playback, PlaybackControl) error {
	h.controls++
	return nil
}
func (h *playbackTestHost) PlaybackActive(Playback) bool { return true }
func playbackFixture(t *testing.T) (*Service, Principal, *playbackTestHost) {
	t.Helper()
	s, p, host := fixture(t)
	h := &playbackTestHost{Host: host}
	s.Host = h
	if err := s.DB.Model(&ipc.Device{}).Where("id = ?", "d1").Updates(map[string]any{"type": ipc.TypeGB28181, "is_online": true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Model(&ipc.Channel{}).Where("id = ?", "c1").Update("channel_id", "001-channel").Error; err != nil {
		t.Fatal(err)
	}
	return s, p, h
}
func playbackInput() PlaybackInput { return PlaybackInput{StartAt: 1700000000, EndAt: 1700000060} }
func TestPlaybackLifecycleTicketsAndControls(t *testing.T) {
	s, p, h := playbackFixture(t)
	row, err := s.CreatePlayback(t.Context(), p, "c1", playbackInput())
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "preparing" || row.DeviceCode != "gb1" || row.ChannelCode != "001-channel" || h.starts != 0 {
		t.Fatal(row)
	}
	if _, _, err = s.IssueTicket(p, "playback", row.ID); AsError(err).Code != "PLAYBACK_NOT_READY" {
		t.Fatal(err)
	}
	s.StartPlayback(row.ID)
	s.StartPlayback(row.ID)
	if h.starts != 1 {
		t.Fatal("duplicate start", h.starts)
	}
	raw, ticket, err := s.IssueTicket(p, "playback", row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CheckTicket(raw, "playback", row.ID); err != nil {
		t.Fatal(err)
	}
	for _, in := range []PlaybackControl{{Action: "seek", SeekAt: row.EndAt}, {Action: "speed", Scale: 8}, {Action: "pause", Scale: 2}, {Action: "stop"}} {
		if _, err = s.ControlPlayback(t.Context(), p, row.ID, in); AsError(err).Status != 400 {
			t.Fatal(in, err)
		}
	}
	if h.controls != 0 {
		t.Fatal("invalid control reached host")
	}
	row, err = s.ControlPlayback(t.Context(), p, row.ID, PlaybackControl{Action: "pause"})
	if err != nil || row.State != "paused" {
		t.Fatal(row, err)
	}
	if err = s.CheckActive(ticket); err != nil {
		t.Fatal("pause revoked media", err)
	}
	row, err = s.ControlPlayback(t.Context(), p, row.ID, PlaybackControl{Action: "seek", SeekAt: row.StartAt + 10})
	if err != nil || row.State != "ready" {
		t.Fatal(row, err)
	}
	// 轮换到第二凭据可换票；撤销原创建凭据仍必须结束会话。
	c, _, err := s.CreateCredential(p.App.ID, nil, "admin")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Current(p.App.ID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.IssueTicket(other, "playback", row.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Revoke(p.App.ID, p.Credential.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckActive(ticket); err == nil {
		t.Fatal("revoked connection survived")
	}
	if _, _, err = s.IssueTicket(other, "playback", row.ID); err == nil {
		t.Fatal("revoked creator revived")
	}
	s.maintainPlaybacks(t.Context())
	if h.stops != 1 {
		t.Fatal(h.stops)
	}
}
func TestPlaybackIsolationSharedQuotaAndStopRetry(t *testing.T) {
	s, p, h := playbackFixture(t)
	row, err := s.CreatePlayback(t.Context(), p, "c1", playbackInput())
	if err != nil {
		t.Fatal(err)
	}
	s.StartPlayback(row.ID)
	live, err := s.CreateSession(t.Context(), p, "c1", "hls")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreatePlayback(t.Context(), p, "c1", playbackInput()); AsError(err).Status != 429 {
		t.Fatal("shared channel quota", err)
	}
	stranger := p
	stranger.App.ID = "other-app"
	if _, err = s.Playback(stranger, row.ID); AsError(err).Status != 404 {
		t.Fatal("app leak", err)
	}
	h.failStop = true
	if err = s.StopPlayback(p, row.ID); AsError(err).Code != "PLAYBACK_STOP_PENDING" {
		t.Fatal(err)
	}
	var stopped Playback
	s.DB.First(&stopped, "id = ?", row.ID)
	if stopped.State != "stopped" || !stopped.StopPending {
		t.Fatal(stopped)
	}
	if _, _, err = s.IssueTicket(p, "playback", row.ID); err == nil {
		t.Fatal("stopped ticket")
	}
	if _, _, err = s.IssueTicket(p, "live", live.ID); err != nil {
		t.Fatal("live affected", err)
	}
	// 重试必须使用原绑定，不受设备编码、节点配置后续修改影响。
	s.DB.Model(&ipc.Channel{}).Where("id = ?", "c1").Update("channel_id", "replacement")
	h.failStop = false
	s.DB.Model(&stopped).Update("next_stop_at", time.Now().Add(-time.Second))
	s.maintainPlaybacks(t.Context())
	s.DB.First(&stopped, "id = ?", row.ID)
	if stopped.StopPending || h.stops != 2 || h.lastStopped.ChannelCode != "001-channel" || h.lastStopped.Source.MediaServerID != "bound-node" {
		t.Fatal(stopped, h.lastStopped, h.stops)
	}
}
func TestPlaybackFailedStartRevocationExpiryAndRestart(t *testing.T) {
	for _, mode := range []string{"failure", "revoke", "expiry", "restart", "abandoned"} {
		t.Run(mode, func(t *testing.T) {
			s, p, h := playbackFixture(t)
			row, err := s.CreatePlayback(t.Context(), p, "c1", playbackInput())
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "failure":
				h.failStart = true
				s.StartPlayback(row.ID)
			case "revoke":
				h.duringStart = func() {
					if err := s.Revoke(p.App.ID, p.Credential.ID, "admin"); err != nil {
						t.Fatal(err)
					}
				}
				s.StartPlayback(row.ID)
			case "expiry":
				s.StartPlayback(row.ID)
				s.DB.Model(&row).Update("expires_at", time.Now().Add(-time.Second))
				s.maintainPlaybacks(t.Context())
			case "restart":
				s.StartPlayback(row.ID)
				if err = Migrate(s.DB); err != nil {
					t.Fatal(err)
				}
				s, err = New(s.DB, s.Config, h)
				if err != nil {
					t.Fatal(err)
				}
				s.maintainPlaybacks(t.Context())
			case "abandoned":
				s.DB.Model(&row).Update("created_at", time.Now().Add(-2*time.Minute))
				s.maintainPlaybacks(t.Context())
			}
			if err = s.DB.First(&row, "id = ?", row.ID).Error; err != nil {
				t.Fatal(err)
			}
			if Has([]string{"preparing", "ready", "paused"}, row.State) || row.StopPending || h.stops != 1 {
				t.Fatal(row, h.stops)
			}
			if mode == "failure" && row.Error != "PLAYBACK_START_FAILED" {
				t.Fatal("unsafe error", row.Error)
			}
		})
	}
}
func TestPlaybackAuthorizationBeforeHost(t *testing.T) {
	s, p, h := playbackFixture(t)
	if _, err := s.CreatePlayback(t.Context(), p, "c2", playbackInput()); AsError(err).Status != 404 {
		t.Fatal(err)
	}
	s.DB.Model(&App{}).Where("id = ?", p.App.ID).Update("scopes", `["recording:read"]`)
	if _, err := s.CreatePlayback(t.Context(), p, "c1", playbackInput()); AsError(err).Status != 403 {
		t.Fatal(err)
	}
	if h.starts != 0 || h.controls != 0 {
		t.Fatal("unauthorized side effect")
	}
}
