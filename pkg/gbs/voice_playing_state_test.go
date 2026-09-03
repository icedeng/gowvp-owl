package gbs

import (
	"bufio"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
)

func TestPersistChannelIdleSkipsActiveSiblingStream(t *testing.T) {
	updates := make(chan bool, 1)
	api := newPlayingStateTestAPI(updates, nil)
	api.streams.Store("play:active", &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "live", Status: 0,
	})

	if err := api.persistChannelIdleIfNoActive(context.Background(), gb10DeviceID, gb10ChannelID); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-updates:
		t.Fatalf("active sibling stream persisted playing=%v, want no idle update", value)
	default:
	}
}

func TestPersistChannelActiveReturnsPersistenceError(t *testing.T) {
	wantErr := errors.New("persist playing state")
	api := newPlayingStateTestAPI(make(chan bool, 1), wantErr)

	if err := api.persistChannelActive(context.Background(), gb10DeviceID, gb10ChannelID); !errors.Is(err, wantErr) {
		t.Fatalf("persist active error = %v, want %v", err, wantErr)
	}
}

func TestStopBroadcastKeepsPlayingWhileSiblingStreamActive(t *testing.T) {
	updates := make(chan bool, 1)
	api := newPlayingStateTestAPI(updates, nil)
	api.streams.Store("play:active", &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "live", Status: 0,
	})
	voiceStream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "voice", Status: 0}
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, Stream: voiceStream, ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(gb10ChannelID, session)
	api.streams.Store(voiceKey(voiceModeBroadcast, gb10DeviceID, gb10ChannelID), voiceStream)

	if err := api.stopBroadcastSession(session, false); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-updates:
		t.Fatalf("stopping Broadcast persisted playing=%v while live stream remained active", value)
	default:
	}
	if _, ok := api.streams.Load("play:active"); !ok {
		t.Fatal("stopping Broadcast removed active sibling stream")
	}
}

func TestStopBroadcastReturnsIdlePersistenceError(t *testing.T) {
	wantErr := errors.New("persist playing state")
	api := newPlayingStateTestAPI(make(chan bool, 1), wantErr)
	voiceStream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "voice", Status: 0}
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, Stream: voiceStream, ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(gb10ChannelID, session)
	api.streams.Store(voiceKey(voiceModeBroadcast, gb10DeviceID, gb10ChannelID), voiceStream)

	if err := api.stopBroadcastSession(session, false); !errors.Is(err, wantErr) {
		t.Fatalf("stop Broadcast error = %v, want persistence error", err)
	}
}

func TestStopStandardTalkPersistsIdleAfterBothFlowsStop(t *testing.T) {
	updates := make(chan bool, 2)
	api := newPlayingStateTestAPI(updates, nil)
	voiceStream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "voice", Status: 0}
	const playKey = "voice:TalkUpstream:test"
	playStream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "talk-upstream", Status: 0}
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, Stream: voiceStream,
		StandardTalkPlayKey: playKey, ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(gb10ChannelID, session)
	api.streams.Store(voiceKey(voiceModeBroadcast, gb10DeviceID, gb10ChannelID), voiceStream)
	api.streams.Store(playKey, playStream)

	if err := api.stopBroadcastSession(session, false); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-updates:
		if value {
			t.Fatalf("stopping standard Talk persisted playing=%v, want false", value)
		}
	default:
		t.Fatal("stopping standard Talk did not persist idle after both media flows ended")
	}
	if api.hasActiveChannelStream(gb10DeviceID, gb10ChannelID) {
		t.Fatal("stopping standard Talk retained an active media flow")
	}
}

func TestFinishDirectTCPHistoryKeepsPlayingWhileSiblingStreamActive(t *testing.T) {
	updates := make(chan bool, 1)
	api := newPlayingStateTestAPI(updates, nil)
	api.streams.Store("play:active", &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "live", Status: 0,
	})
	download := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "download", DirectTCP: true, Status: 0,
	}
	const downloadKey = "history:Download:direct"
	api.streams.Store(downloadKey, download)

	api.finishDirectTCPHistory(downloadKey, nil, download, DirectTCPDownloadState{Status: directTCPStatusCompleted})

	select {
	case value := <-updates:
		t.Fatalf("finishing direct TCP download persisted playing=%v while live stream remained active", value)
	default:
	}
	if _, ok := api.streams.Load("play:active"); !ok {
		t.Fatal("finishing direct TCP download removed active sibling stream")
	}
}

func TestDirectTCPCompletionBeforeStartCommitDoesNotRestorePlaying(t *testing.T) {
	updates := make(chan bool, 2)
	api := newPlayingStateTestAPI(updates, nil)
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "download", DirectTCP: true, Status: 0,
	}
	const key = "history:Download:fast-complete"
	api.streams.Store(key, stream)

	api.finishDirectTCPHistory(key, nil, stream, DirectTCPDownloadState{Status: directTCPStatusCompleted})
	published, err := api.commitChannelStreamStart(context.Background(), key, stream)
	if err != nil {
		t.Fatal(err)
	}
	if published {
		t.Fatal("completed direct TCP download was republished by delayed start commit")
	}
	select {
	case value := <-updates:
		if value {
			t.Fatalf("completion persisted playing=%v, want false", value)
		}
	default:
		t.Fatal("direct TCP completion did not persist idle")
	}
	select {
	case value := <-updates:
		t.Fatalf("delayed start commit wrote a second playing state: %v", value)
	default:
	}
}

func TestRTPTerminationBeforeStartCommitDoesNotRestorePlayingFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		for _, session := range []struct {
			name   string
			key    string
			typeID int
		}{
			{name: "Play", key: resolvePlaySessionKey(gb10DeviceID, gb10ChannelID, ""), typeID: 0},
			{name: "HistoryPlayback", key: historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID), typeID: 1},
		} {
			t.Run(string(version)+"/"+session.name, func(t *testing.T) {
				updates := make(chan bool, 2)
				api := newPlayingStateTestAPI(updates, nil)
				stream := &Streams{
					T: session.typeID, DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
					StreamID: "rtp-" + string(version), Status: 0,
				}
				api.streams.Store(session.key, stream)

				if !api.compareAndDeleteChannelStream(session.key, stream) {
					t.Fatal("RTP terminal path did not remove the active stream")
				}
				if err := api.persistChannelIdleIfNoActive(context.Background(), stream.DeviceID, stream.ChannelID); err != nil {
					t.Fatal(err)
				}
				published, err := api.commitChannelStreamStart(context.Background(), session.key, stream)
				if err != nil {
					t.Fatal(err)
				}
				if published {
					t.Fatal("terminated RTP stream was republished by delayed start commit")
				}
				select {
				case value := <-updates:
					if value {
						t.Fatalf("RTP termination persisted playing=%v, want false", value)
					}
				default:
					t.Fatal("RTP termination did not persist idle")
				}
				select {
				case value := <-updates:
					t.Fatalf("delayed RTP start commit wrote a second playing state: %v", value)
				default:
				}
			})
		}
	}
}

func TestPlayPersistenceFailureRollsBackFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			fixture := newCascadeDownstreamSIPFixture(t, version)
			_, _, inputChannel := newCascadeMediaCore(t)
			wantErr := errors.New("persist playing state")
			fixture.api.core = newPlayingStateTestAPI(make(chan bool, 1), wantErr).core
			media := &fakeRTPMediaService{openPort: 30000}
			fixture.api.sms = media
			fixture.api.streams = &conc.Map[string, *Streams]{}
			mediaServer := &sms.MediaServer{ID: "local", Type: sms.ProtocolZLMediaKit, SDPIP: "192.0.2.20"}
			observed := make(chan error, 1)

			go func() {
				_ = fixture.peer.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(fixture.peer)
				invite, err := readAnnexGTestSIPFrame(reader)
				if err != nil {
					observed <- err
					return
				}
				ssrc := directTCPSDPLineValue([]byte(invite), "y")
				body := "v=0\r\n" +
					"o=" + gb10DeviceID + " 0 0 IN IP4 192.0.2.30\r\n" +
					"s=Play\r\n" +
					"c=IN IP4 192.0.2.30\r\n" +
					"t=0 0\r\n" +
					"m=video 9000 RTP/AVP 96\r\n" +
					"a=sendonly\r\n" +
					"a=rtpmap:96 PS/90000\r\n" +
					"y=" + ssrc + "\r\n"
				if _, err = io.WriteString(fixture.peer, directTCPTestResponse(invite, body, "Application/SDP")); err != nil {
					observed <- err
					return
				}
				if _, err = readAnnexGTestSIPFrame(reader); err != nil { // ACK
					observed <- err
					return
				}
				bye, err := readAnnexGTestSIPFrame(reader)
				if err == nil && !sipFrameStartsWith(bye, "BYE ") {
					err = errors.New("rollback did not send BYE")
				}
				observed <- err
			}()

			err := fixture.api.PlayContext(t.Context(), &PlayInput{Channel: inputChannel, SMS: mediaServer})
			if !errors.Is(err, wantErr) {
				t.Fatalf("Play persistence error = %v, want %v", err, wantErr)
			}
			if err := <-observed; err != nil {
				t.Fatal(err)
			}
			media.mu.Lock()
			openCalls, closeCalls := media.openCalls, media.closeCalls
			media.mu.Unlock()
			if openCalls != 1 || closeCalls != 1 {
				t.Fatalf("RTP rollback calls = open:%d close:%d", openCalls, closeCalls)
			}
			if fixture.api.hasActiveChannelStream(gb10DeviceID, testCascadeChannelID) {
				t.Fatal("failed Play persistence retained active stream")
			}
		})
	}
}

func sipFrameStartsWith(frame, prefix string) bool {
	return len(frame) >= len(prefix) && frame[:len(prefix)] == prefix
}

func newPlayingStateTestAPI(updates chan<- bool, updateErr error) *GB28181API {
	channel := &playingStateChannelStore{updates: updates, updateErr: updateErr}
	store := &playingStateStore{channel: channel}
	return &GB28181API{
		core:    ipc.NewAdapter(store, uniqueid.Core{}),
		streams: &conc.Map[string, *Streams]{},
	}
}

type playingStateStore struct {
	ipc.Storer
	channel ipc.ChannelStorer
}

func (s *playingStateStore) Channel() ipc.ChannelStorer { return s.channel }

type playingStateChannelStore struct {
	ipc.ChannelStorer
	updates   chan<- bool
	updateErr error
}

func (s *playingStateChannelStore) Update(_ context.Context, channel *ipc.Channel, change func(*ipc.Channel) error, _ ...orm.QueryOption) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	if err := change(channel); err != nil {
		return err
	}
	s.updates <- channel.IsPlaying
	return nil
}
