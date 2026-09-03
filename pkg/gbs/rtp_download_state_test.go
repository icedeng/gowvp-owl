package gbs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
)

type rtpDownloadTaskMemory struct {
	*persistentTaskMemory
	metadataMu sync.Mutex
	updatedAt  map[string]time.Time
	failSaves  int
}

// blockingTaskStateLoadMemory 暂停恢复线程读取旧快照的时刻，
// 用于证明同键任务写入不会被恢复清理删除。
type blockingTaskStateLoadMemory struct {
	*rtpDownloadTaskMemory
	kind, deviceID, sessionID string
	entered, release          chan struct{}
	blockOnce                 sync.Once
}

func (m *blockingTaskStateLoadMemory) LoadGBTaskState(ctx context.Context, kind, deviceID, sessionID string) ([]byte, bool, error) {
	payload, found, err := m.rtpDownloadTaskMemory.LoadGBTaskState(ctx, kind, deviceID, sessionID)
	if err != nil || kind != m.kind || deviceID != m.deviceID || sessionID != m.sessionID {
		return payload, found, err
	}
	m.blockOnce.Do(func() {
		close(m.entered)
		select {
		case <-m.release:
		case <-ctx.Done():
		}
	})
	return payload, found, err
}

func newRTPDownloadTaskMemory(version GBProtocolVersion) *rtpDownloadTaskMemory {
	return &rtpDownloadTaskMemory{
		persistentTaskMemory: newPersistentTaskMemory(version),
		updatedAt:            make(map[string]time.Time),
	}
}

func (m *rtpDownloadTaskMemory) SaveGBTaskState(ctx context.Context, kind, deviceID, sessionID string, payload []byte, updatedAt time.Time) error {
	m.metadataMu.Lock()
	if m.failSaves > 0 {
		m.failSaves--
		m.metadataMu.Unlock()
		return errTaskStateSave
	}
	m.metadataMu.Unlock()
	if err := m.persistentTaskMemory.SaveGBTaskState(ctx, kind, deviceID, sessionID, payload, updatedAt); err != nil {
		return err
	}
	m.metadataMu.Lock()
	m.updatedAt[persistentTaskKey(kind, deviceID, sessionID)] = updatedAt
	m.metadataMu.Unlock()
	return nil
}

func (m *rtpDownloadTaskMemory) ListGBTaskStates(ctx context.Context, kind string, limit int) ([]ipc.GBTaskStateRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.metadataMu.Lock()
	records := make([]ipc.GBTaskStateRecord, 0)
	for key, payload := range m.records {
		parts := strings.Split(key, "\x00")
		if len(parts) != 3 || parts[0] != kind {
			continue
		}
		records = append(records, ipc.GBTaskStateRecord{
			Kind: parts[0], DeviceID: parts[1], SessionID: parts[2], Payload: string(payload),
			UpdatedAt: orm.Time{Time: m.updatedAt[key]},
		})
	}
	m.metadataMu.Unlock()
	m.mu.Unlock()
	sort.Slice(records, func(i, j int) bool { return records[i].UpdatedAt.Time.Before(records[j].UpdatedAt.Time) })
	if limit > 0 && len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}

func TestRTPDownloadProgressUsesFileSizeAndMediaBytes(t *testing.T) {
	media := &fakeRTPMediaService{mediaItems: []zlm.MediaItem{{
		TotalBytes: 500, BytesSpeed: 128, Tracks: []zlm.MediaTrack{{Duration: 90_000}},
	}}}
	api := &GB28181API{sms: media}
	start := time.Date(2026, 8, 29, 10, 0, 0, 0, time.Local)
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "download-stream", CallID: "download-dialog",
		FileSize: 1000, FileSizeKnown: true, S: start, E: start.Add(2 * time.Minute), mediaServer: &sms.MediaServer{},
	}
	api.registerRTPDownload(stream)
	state, ok := api.RTPDownloadByChannel(gb10DeviceID, gb10ChannelID)
	if !ok {
		t.Fatal("RTP download state not found")
	}
	if state.Received != 500 || state.BytesSpeed != 128 || !state.ProgressKnown || !state.Approximate || state.ProgressPercent != 50 {
		t.Fatalf("unexpected RTP download state: %+v", state)
	}
}

func TestRTPDownloadProgressUsesMediaDurationWithoutFileSize(t *testing.T) {
	media := &fakeRTPMediaService{mediaItems: []zlm.MediaItem{
		{TotalBytes: 500, BytesSpeed: 128, Tracks: []zlm.MediaTrack{{Duration: 30_000}, {Duration: 45_000}}},
		{Tracks: []zlm.MediaTrack{{Duration: 40_000}}},
	}}
	api := &GB28181API{sms: media}
	start := time.Date(2026, 8, 29, 10, 0, 0, 0, time.Local)
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "duration-download-stream", CallID: "duration-download-dialog",
		S: start, E: start.Add(2 * time.Minute), mediaServer: &sms.MediaServer{},
	}
	api.registerRTPDownload(stream)
	state, ok := api.RTPDownloadByChannel(gb10DeviceID, gb10ChannelID)
	if !ok {
		t.Fatal("RTP download state not found")
	}
	if state.Received != 500 || state.BytesSpeed != 128 || !state.ProgressKnown || state.Approximate || state.ProgressPercent != 37.5 {
		t.Fatalf("unexpected media-duration progress: %+v", state)
	}
}

func TestRTPDownloadProgressStaysUnknownWithoutMediaDuration(t *testing.T) {
	media := &fakeRTPMediaService{mediaItems: []zlm.MediaItem{{
		TotalBytes: 500, BytesSpeed: 128, Tracks: []zlm.MediaTrack{{CodecIDName: "H264"}},
	}}}
	api := &GB28181API{sms: media}
	start := time.Date(2026, 8, 29, 10, 0, 0, 0, time.Local)
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "unknown-duration-stream", CallID: "unknown-duration-dialog",
		S: start, E: start.Add(2 * time.Minute), mediaServer: &sms.MediaServer{},
	}
	api.registerRTPDownload(stream)
	state, ok := api.RTPDownloadByChannel(gb10DeviceID, gb10ChannelID)
	if !ok {
		t.Fatal("RTP download state not found")
	}
	if state.ProgressKnown || state.ProgressPercent != 0 || state.Approximate {
		t.Fatalf("progress without media duration should stay unknown: %+v", state)
	}
}

func TestRemoteBYEFinishesOutboundRTPDownload(t *testing.T) {
	conn := newFlowConnection()
	media := &fakeRTPMediaService{mediaItems: []zlm.MediaItem{{TotalBytes: 1000}}}
	api := &GB28181API{
		cfg: &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"}, sms: media,
		streams: &conc.Map[string, *Streams]{},
	}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "download-stream", CallID: "remote-download-bye",
		FileSize: 1000, FileSizeKnown: true, mediaServer: &sms.MediaServer{},
	}
	key := historyKey(historyModeDownload, gb10DeviceID, gb10ChannelID)
	api.streams.Store(key, stream)
	api.registerRTPDownload(stream)

	response := runFlowHandler(t, conn, api, sip.MethodBYE, stream.CallID, nil, api.sipByeGeneric)
	assertFlowOK(t, response)
	if _, ok := api.streams.Load(key); ok {
		t.Fatal("remote BYE did not remove outbound stream")
	}
	state, ok := api.RTPDownloadByChannel(gb10DeviceID, gb10ChannelID)
	if !ok || state.Status != rtpDownloadCompleted || state.EndReason != "remote_bye" || state.CompletedAt.IsZero() {
		t.Fatalf("unexpected terminal state: %+v", state)
	}
	if time.Since(state.CompletedAt) > time.Second {
		t.Fatalf("unexpected completion time: %s", state.CompletedAt)
	}
	media.mu.Lock()
	closed := media.closed
	media.mu.Unlock()
	if closed.StreamID != stream.StreamID {
		t.Fatalf("RTP receiver was not closed: %+v", closed)
	}
}

func TestMediaStatusAfterRemoteBYEUpgradesRTPDownloadCompletion(t *testing.T) {
	conn := newFlowConnection()
	api := &GB28181API{streams: &conc.Map[string, *Streams]{}}
	setMediaStatusTestVersion(t, api, GBVersion11)
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "download-race-stream", CallID: "download-race-dialog",
		mediaServer: &sms.MediaServer{},
	}
	key := historyKey(historyModeDownload, gb10DeviceID, gb10ChannelID)
	api.streams.Store(key, stream)
	api.registerRTPDownload(stream)

	response := runFlowHandler(t, conn, api, sip.MethodBYE, stream.CallID, nil, api.sipByeGeneric)
	assertFlowOK(t, response)
	state, ok := api.RTPDownloadByChannel(gb10DeviceID, gb10ChannelID)
	if !ok || state.Status != rtpDownloadStopped || state.EndReason != "remote_bye" {
		t.Fatalf("state after leading BYE = %+v", state)
	}

	body := []byte(`<?xml version="1.0"?><Notify><CmdType>MediaStatus</CmdType><SN>42</SN><DeviceID>` + gb10DeviceID + `</DeviceID><NotifyType>121</NotifyType></Notify>`)
	response = runFlowHandler(t, conn, api, sip.MethodMessage, stream.CallID, body, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	state, ok = api.RTPDownloadByChannel(gb10DeviceID, gb10ChannelID)
	if !ok || state.Status != rtpDownloadCompleted || state.EndReason != "media_status" || state.CompletedAt.IsZero() {
		t.Fatalf("state after trailing MediaStatus = %+v", state)
	}
}

func TestLateMediaStatusDoesNotOverrideUserStoppedRTPDownload(t *testing.T) {
	conn := newFlowConnection()
	api := &GB28181API{streams: &conc.Map[string, *Streams]{}}
	setMediaStatusTestVersion(t, api, GBVersion11)
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "download-user-stop-stream", CallID: "download-user-stop-dialog",
		mediaServer: &sms.MediaServer{},
	}
	api.registerRTPDownload(stream)
	api.finishRTPDownload(stream, rtpDownloadStopped, "stopped_by_user")

	body := []byte(`<?xml version="1.0"?><Notify><CmdType>MediaStatus</CmdType><SN>43</SN><DeviceID>` + gb10DeviceID + `</DeviceID><NotifyType>121</NotifyType></Notify>`)
	response := runFlowHandler(t, conn, api, sip.MethodMessage, stream.CallID, body, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	state, ok := api.RTPDownloadByChannel(gb10DeviceID, gb10ChannelID)
	if !ok || state.Status != rtpDownloadStopped || state.EndReason != "stopped_by_user" {
		t.Fatalf("late MediaStatus changed user-stopped state: %+v", state)
	}
}

func TestRTPDownloadMediaStatusCompletionIsNotDowngradedByTrailingBYE(t *testing.T) {
	api := &GB28181API{}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "download-trailing-bye-stream", CallID: "download-trailing-bye-dialog",
		mediaServer: &sms.MediaServer{},
	}
	api.registerRTPDownload(stream)
	if !api.finishRTPDownloadByCallID(stream.DeviceID, stream.ChannelID, stream.CallID, rtpDownloadCompleted, "media_status") {
		t.Fatal("MediaStatus did not match retained RTP download")
	}

	// 模拟 BYE 已抢先删除流索引，但其较慢的媒体清理晚于 MediaStatus 才写终态。
	api.finishRTPDownload(stream, rtpDownloadStopped, "remote_bye")
	state, ok := api.RTPDownloadByChannel(stream.DeviceID, stream.ChannelID)
	if !ok || state.Status != rtpDownloadCompleted || state.EndReason != "media_status" {
		t.Fatalf("trailing BYE downgraded MediaStatus completion: %+v", state)
	}
}

func TestOldRTPDownloadCannotFinishReplacementSession(t *testing.T) {
	api := &GB28181API{}
	oldStream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		StreamID: "old-download-stream", CallID: "old-download-dialog", mediaServer: &sms.MediaServer{},
	}
	replacement := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		StreamID: "replacement-download-stream", CallID: "replacement-download-dialog", mediaServer: &sms.MediaServer{},
	}
	api.registerRTPDownload(oldStream)
	api.registerRTPDownload(replacement)

	api.finishRTPDownload(oldStream, rtpDownloadStopped, "remote_bye")

	state, ok := api.RTPDownloadByChannel(replacement.DeviceID, replacement.ChannelID)
	if !ok {
		t.Fatal("replacement RTP download state not found")
	}
	if state.SessionID != replacement.CallID || state.Status != rtpDownloadWaiting || !state.CompletedAt.IsZero() || state.EndReason != "" {
		t.Fatalf("old RTP download changed replacement state: %+v", state)
	}
}

func TestRTPDownloadMediaStatusAndBYEConvergeConcurrently(t *testing.T) {
	for iteration := 0; iteration < 256; iteration++ {
		api := &GB28181API{}
		stream := &Streams{
			DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
			StreamID:    fmt.Sprintf("download-concurrent-stream-%d", iteration),
			CallID:      fmt.Sprintf("download-concurrent-dialog-%d", iteration),
			mediaServer: &sms.MediaServer{},
		}
		api.registerRTPDownload(stream)

		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			api.finishRTPDownload(stream, rtpDownloadStopped, "remote_bye")
		}()
		go func() {
			defer workers.Done()
			<-start
			api.finishRTPDownloadByCallID(stream.DeviceID, stream.ChannelID, stream.CallID, rtpDownloadCompleted, "media_status")
		}()
		close(start)
		workers.Wait()

		state, ok := api.RTPDownloadByChannel(stream.DeviceID, stream.ChannelID)
		if !ok || state.Status != rtpDownloadCompleted || state.EndReason != "media_status" {
			t.Fatalf("iteration %d did not converge to MediaStatus completion: %+v", iteration, state)
		}
	}
}

func TestRTPDownloadTerminalStatePersistsAcrossRestartVersionMatrix(t *testing.T) {
	versions := []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30}
	for _, version := range versions {
		t.Run(string(version), func(t *testing.T) {
			store := newRTPDownloadTaskMemory(version)
			first := &GB28181API{svr: &Server{memoryStorer: store}}
			stream := &Streams{
				DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
				StreamID: "persistent-download-" + string(version), CallID: "persistent-dialog-" + string(version),
				mediaServer: &sms.MediaServer{},
			}
			first.registerRTPDownload(stream)
			first.finishRTPDownload(stream, rtpDownloadStopped, "stopped_by_user")

			payload, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindRTPDownload, stream.DeviceID, stream.ChannelID)
			if err != nil || !found {
				t.Fatalf("persisted RTP download state: found=%v err=%v", found, err)
			}
			var persisted rtpDownloadPersistentState
			if err := json.Unmarshal(payload, &persisted); err != nil {
				t.Fatalf("decode persisted RTP download state: %v", err)
			}
			if persisted.State.Status != rtpDownloadStopped || persisted.State.EndReason != "stopped_by_user" {
				t.Fatalf("persisted RTP download state = %+v", persisted.State)
			}

			restarted := &GB28181API{svr: &Server{memoryStorer: store}}
			if err := restarted.restoreRTPDownloadStates(t.Context()); err != nil {
				t.Fatalf("restore RTP download states: %v", err)
			}
			state, ok := restarted.RTPDownloadByChannel(stream.DeviceID, stream.ChannelID)
			if !ok || state.SessionID != stream.CallID || state.Status != rtpDownloadStopped || state.EndReason != "stopped_by_user" {
				t.Fatalf("restored RTP download state = %+v, found=%v", state, ok)
			}
		})
	}
}

func TestRTPDownloadPersistenceKeepsMediaStatusUpgradeMonotonic(t *testing.T) {
	store := newRTPDownloadTaskMemory(GBVersion11)
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		StreamID: "persistent-media-status-stream", CallID: "persistent-media-status-dialog",
		mediaServer: &sms.MediaServer{},
	}
	api.registerRTPDownload(stream)
	api.finishRTPDownload(stream, rtpDownloadStopped, "remote_bye")
	if !api.finishRTPDownloadByCallID(stream.DeviceID, stream.ChannelID, stream.CallID, rtpDownloadCompleted, "media_status") {
		t.Fatal("MediaStatus did not match retained RTP download")
	}
	api.finishRTPDownload(stream, rtpDownloadStopped, "remote_bye")

	payload, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindRTPDownload, stream.DeviceID, stream.ChannelID)
	if err != nil || !found {
		t.Fatalf("persisted RTP download state: found=%v err=%v", found, err)
	}
	var persisted rtpDownloadPersistentState
	if err := json.Unmarshal(payload, &persisted); err != nil {
		t.Fatalf("decode persisted RTP download state: %v", err)
	}
	if persisted.State.Status != rtpDownloadCompleted || persisted.State.EndReason != "media_status" {
		t.Fatalf("trailing BYE downgraded persisted state: %+v", persisted.State)
	}
}

func TestRTPDownloadPersistenceFailureRetriesDuringCleanup(t *testing.T) {
	store := newRTPDownloadTaskMemory(GBVersion20)
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		StreamID: "retry-persistence-stream", CallID: "retry-persistence-dialog",
		mediaServer: &sms.MediaServer{},
	}
	api.registerRTPDownload(stream)
	store.metadataMu.Lock()
	store.failSaves = 1
	store.metadataMu.Unlock()
	api.finishRTPDownload(stream, rtpDownloadCompleted, "media_status")
	beforeRetry, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindRTPDownload, stream.DeviceID, stream.ChannelID)
	if err != nil || !found {
		t.Fatalf("active supersession marker missing after failed terminal save: found=%v err=%v", found, err)
	}
	var activeMarker rtpDownloadPersistentState
	if err := json.Unmarshal(beforeRetry, &activeMarker); err != nil || activeMarker.Terminal {
		t.Fatalf("failed terminal save replaced active marker: terminal=%v err=%v", activeMarker.Terminal, err)
	}

	api.cleanupRTPDownloads(time.Now())
	payload, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindRTPDownload, stream.DeviceID, stream.ChannelID)
	if err != nil || !found {
		t.Fatalf("cleanup retry did not persist state: found=%v err=%v", found, err)
	}
	var persisted rtpDownloadPersistentState
	if err := json.Unmarshal(payload, &persisted); err != nil || persisted.State.Status != rtpDownloadCompleted {
		t.Fatalf("retried persisted state = %+v, err=%v", persisted.State, err)
	}
}

func TestRTPDownloadRegistrationRejectsUndurableSupersessionMarker(t *testing.T) {
	store := newRTPDownloadTaskMemory(GBVersion20)
	store.failSaves = 1
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		StreamID: "undurable-marker-stream", CallID: "undurable-marker-dialog",
		mediaServer: &sms.MediaServer{},
	}
	if err := api.registerRTPDownload(stream); !errors.Is(err, errTaskStateSave) {
		t.Fatalf("register RTP download error = %v", err)
	}
	if _, ok := api.RTPDownloadByChannel(stream.DeviceID, stream.ChannelID); ok {
		t.Fatal("RTP download became visible without a durable supersession marker")
	}
}

func TestRTPDownloadShutdownRetriesTerminalPersistence(t *testing.T) {
	store := newRTPDownloadTaskMemory(GBVersion20)
	api := &GB28181API{
		svr: &Server{memoryStorer: store}, streams: &conc.Map[string, *Streams]{},
		lifecycleDone: make(chan struct{}), sms: &fakeRTPMediaService{},
	}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		StreamID: "shutdown-persistence-stream", CallID: "shutdown-persistence-dialog",
		mediaServer: &sms.MediaServer{},
	}
	key := historyKey(historyModeDownload, stream.DeviceID, stream.ChannelID)
	api.streams.Store(key, stream)
	api.registerRTPDownload(stream)
	store.metadataMu.Lock()
	store.failSaves = 1
	store.metadataMu.Unlock()

	api.close()
	state, ok := api.RTPDownloadByChannel(stream.DeviceID, stream.ChannelID)
	if !ok || state.Status != rtpDownloadStopped || state.EndReason != "service_stopped" {
		t.Fatalf("shutdown RTP terminal state = %+v, found=%v", state, ok)
	}
	if _, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindRTPDownload, stream.DeviceID, stream.ChannelID); err != nil || !found {
		t.Fatalf("shutdown retry did not persist RTP terminal state: found=%v err=%v", found, err)
	}
}

func TestRestoreRTPDownloadStatesRejectsActiveInvalidAndMismatchedRecords(t *testing.T) {
	store := newRTPDownloadTaskMemory(GBVersion30)
	now := time.Now()
	validState := RTPDownloadState{
		SessionID: "restore-valid-dialog", DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		Status: rtpDownloadCompleted, EndReason: "media_status",
		StartedAt: now.Add(-time.Minute), UpdatedAt: now, CompletedAt: now,
	}
	validPayload, err := json.Marshal(rtpDownloadPersistentState{
		Key: historyKey(historyModeDownload, validState.DeviceID, validState.ChannelID), Terminal: true, State: validState,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindRTPDownload, validState.DeviceID, validState.ChannelID, validPayload, now); err != nil {
		t.Fatal(err)
	}

	active := validState
	active.ChannelID = "34020000001320000023"
	active.Status = rtpDownloadReceiving
	active.CompletedAt = time.Time{}
	active.EndReason = ""
	activePayload, _ := json.Marshal(rtpDownloadPersistentState{
		Key: historyKey(historyModeDownload, active.DeviceID, active.ChannelID), State: active,
	})
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindRTPDownload, active.DeviceID, active.ChannelID, activePayload, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindRTPDownload, "34020000001320009999", "34020000001320009998", validPayload, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindRTPDownload, gb10DeviceID, "34020000001320009997", []byte("not-json"), now); err != nil {
		t.Fatal(err)
	}
	future := validState
	future.ChannelID = "34020000001320009996"
	future.UpdatedAt = now.Add(24 * time.Hour)
	futurePayload, _ := json.Marshal(rtpDownloadPersistentState{
		Key: historyKey(historyModeDownload, future.DeviceID, future.ChannelID), Terminal: true, State: future,
	})
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindRTPDownload, future.DeviceID, future.ChannelID, futurePayload, future.UpdatedAt); err != nil {
		t.Fatal(err)
	}

	api := &GB28181API{svr: &Server{memoryStorer: store}}
	if err := api.restoreRTPDownloadStates(t.Context()); err != nil {
		t.Fatalf("restore RTP download states: %v", err)
	}
	state, ok := api.RTPDownloadByChannel(validState.DeviceID, validState.ChannelID)
	if !ok || state.SessionID != validState.SessionID {
		t.Fatalf("valid state was not restored: %+v, found=%v", state, ok)
	}
	if _, ok := api.RTPDownloadByChannel(active.DeviceID, active.ChannelID); ok {
		t.Fatal("active persisted state was restored as a live download")
	}
	if _, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindRTPDownload, active.DeviceID, active.ChannelID); err != nil || found {
		t.Fatalf("interrupted active marker was not removed: found=%v err=%v", found, err)
	}
	if _, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindRTPDownload, "34020000001320009999", "34020000001320009998"); err != nil || found {
		t.Fatalf("mismatched RTP download record was not removed: found=%v err=%v", found, err)
	}
	if _, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindRTPDownload, gb10DeviceID, "34020000001320009997"); err != nil || found {
		t.Fatalf("malformed RTP download record was not removed: found=%v err=%v", found, err)
	}
	if _, ok := api.RTPDownloadByChannel(future.DeviceID, future.ChannelID); ok {
		t.Fatal("RTP download record with a future update time was restored")
	}
	if _, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindRTPDownload, future.DeviceID, future.ChannelID); err != nil || found {
		t.Fatalf("RTP download record with a future update time was not removed: found=%v err=%v", found, err)
	}
}

func TestRTPDownloadRestoreCannotDeleteConcurrentMarker(t *testing.T) {
	base := newRTPDownloadTaskMemory(GBVersion20)
	now := time.Now()
	old := RTPDownloadState{SessionID: "old-restore-marker", DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, Status: rtpDownloadReceiving, StartedAt: now.Add(-time.Minute), UpdatedAt: now}
	payload, err := json.Marshal(rtpDownloadPersistentState{Key: historyKey(historyModeDownload, old.DeviceID, old.ChannelID), State: old})
	if err != nil {
		t.Fatal(err)
	}
	if err := base.SaveGBTaskState(t.Context(), gbTaskKindRTPDownload, old.DeviceID, old.ChannelID, payload, now); err != nil {
		t.Fatal(err)
	}
	store := &blockingTaskStateLoadMemory{
		rtpDownloadTaskMemory: base, kind: gbTaskKindRTPDownload, deviceID: old.DeviceID, sessionID: old.ChannelID,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	restoreDone := make(chan error, 1)
	go func() { restoreDone <- api.restoreRTPDownloadStates(t.Context()) }()
	<-store.entered
	newStream := &Streams{DeviceID: old.DeviceID, ChannelID: old.ChannelID, StreamID: "new-restore-stream", CallID: "new-restore-dialog", mediaServer: &sms.MediaServer{}}
	persistDone := make(chan error, 1)
	go func() { persistDone <- api.registerRTPDownload(newStream) }()
	select {
	case err := <-persistDone:
		t.Fatalf("new RTP marker persisted before restore released: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(store.release)
	if err := <-restoreDone; err != nil {
		t.Fatal(err)
	}
	if err := <-persistDone; err != nil {
		t.Fatal(err)
	}
	finalPayload, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindRTPDownload, old.DeviceID, old.ChannelID)
	if err != nil || !found {
		t.Fatalf("new RTP marker missing after restore: found=%v err=%v", found, err)
	}
	var final rtpDownloadPersistentState
	if err := json.Unmarshal(finalPayload, &final); err != nil || final.Terminal || final.State.SessionID != newStream.CallID {
		t.Fatalf("restore removed or replaced new RTP marker: %+v err=%v", final, err)
	}
}

func TestCascadeRTPDownloadTerminalStateUsesIndependentPersistencePool(t *testing.T) {
	store := newRTPDownloadTaskMemory(GBVersion20)
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		StreamID: "cascade-persistent-stream", CallID: "cascade-persistent-dialog",
		sessionKey:  historyKey(historyModeDownload, gb10DeviceID, gb10ChannelID) + ":cascade:0123456789abcdef",
		mediaServer: &sms.MediaServer{},
	}
	api.registerRTPDownload(stream)
	api.finishRTPDownload(stream, rtpDownloadCompleted, "media_status")
	if _, found, err := store.LoadGBTaskState(t.Context(), gbTaskKindRTPDownloadSession, stream.DeviceID, stream.CallID); err != nil || !found {
		t.Fatalf("cascade RTP terminal state was not isolated: found=%v err=%v", found, err)
	}
}

func TestRemoteBYECannotStopAnotherDevicesOutboundSession(t *testing.T) {
	api := &GB28181API{streams: &conc.Map[string, *Streams]{}}
	conn := newFlowConnection()
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "cross-device-bye", StreamID: "cross-device-stream",
	}
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	api.streams.Store(key, stream)
	request := newFlowRequest(t, conn, sip.MethodBYE, stream.CallID, nil)
	api.sipByeGeneric(&sip.Context{
		Request: request, Tx: sip.NewTransaction("cross-device-bye-tx", conn),
		DeviceID: "34020000001320009999", Source: conn.remote,
	})
	select {
	case response := <-conn.writes:
		if !strings.Contains(string(response), "SIP/2.0 481") {
			t.Fatalf("cross-device BYE response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("cross-device BYE response timeout")
	}
	if current, ok := api.streams.Load(key); !ok || current != stream || stream.Stop {
		t.Fatal("cross-device BYE stopped another device's outbound session")
	}
}

func TestRemoteBYEAcknowledgesBeforeMediaCleanup(t *testing.T) {
	conn := newFlowConnection()
	media := &blockingCloseRTPMediaService{
		fakeRTPMediaService: &fakeRTPMediaService{},
		started:             make(chan struct{}),
		release:             make(chan struct{}),
	}
	api := &GB28181API{sms: media, streams: &conc.Map[string, *Streams]{}}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "slow-bye-stream", CallID: "slow-remote-bye",
		mediaServer: &sms.MediaServer{},
	}
	key := "play:" + gb10DeviceID + ":" + gb10ChannelID
	api.streams.Store(key, stream)
	request := newFlowRequest(t, conn, sip.MethodBYE, stream.CallID, nil)
	to := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.sipByeGeneric(&sip.Context{
			Request: request, Tx: sip.NewTransaction("slow-remote-bye-tx", conn),
			DeviceID: gb10DeviceID, Source: conn.remote, To: to,
		})
	}()
	release := func() {
		select {
		case <-media.release:
		default:
			close(media.release)
		}
	}
	defer release()

	select {
	case <-media.started:
	case <-time.After(time.Second):
		t.Fatal("remote BYE media cleanup was not reached")
	}
	select {
	case payload := <-conn.writes:
		if response := string(payload); !strings.Contains(response, "SIP/2.0 200 OK") {
			t.Fatalf("unexpected BYE response:\n%s", response)
		}
	default:
		release()
		<-done
		t.Fatal("media cleanup delayed BYE 200 OK")
	}
	if current, ok := api.streams.Load(key); !ok || current != stream || !api.mediaStreamStopping(stream) {
		t.Fatal("remote BYE did not retain a terminal stream while media cleanup was in progress")
	}
	if api.hasActiveChannelStream(stream.DeviceID, stream.ChannelID) {
		t.Fatal("terminal stream remained visible as active during media cleanup")
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("remote BYE handler did not finish")
	}
}

func TestRemoteBYECommitsOnlyAfterSuccessfulSIPOK(t *testing.T) {
	base := newFlowConnection()
	conn := &blockingFlowResponseConnection{
		flowConnection: base,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
		writeErr:       errors.New("remote BYE SIP OK write failed"),
	}
	api := &GB28181API{streams: &conc.Map[string, *Streams]{}}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "failed-bye-stream", CallID: "failed-remote-bye",
	}
	key := "play:" + gb10DeviceID + ":" + gb10ChannelID
	api.streams.Store(key, stream)
	request := newFlowRequest(t, base, sip.MethodBYE, stream.CallID, nil)
	request.SetConnection(conn)
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.sipByeGeneric(&sip.Context{
			Request: request, Tx: sip.NewTransaction("failed-remote-bye-tx", conn),
			DeviceID: gb10DeviceID, Source: base.remote,
		})
	}()

	select {
	case <-conn.started:
	case <-time.After(time.Second):
		close(conn.release)
		t.Fatal("remote BYE SIP response write did not start")
	}
	if current, ok := api.streams.Load(key); !ok || current != stream || stream.Stop {
		close(conn.release)
		<-done
		t.Fatal("remote BYE committed before SIP OK completed")
	}
	close(conn.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("remote BYE handler did not return after SIP OK write failure")
	}
	if current, ok := api.streams.Load(key); !ok || current != stream || stream.Stop {
		t.Fatal("remote BYE committed after SIP OK write failure")
	}
}

func TestCleanupRTPDownloadsExpiresOnlyTerminalStates(t *testing.T) {
	now := time.Now()
	api := &GB28181API{}
	expiredKey := "cascade-download-expired"
	activeKey := "cascade-download-active"
	recentKey := historyKey(historyModeDownload, gb10DeviceID, gb10ChannelID)
	api.rtpDownloads.Store(expiredKey, testRTPDownloadSession(gb10DeviceID, gb10ChannelID, now.Add(-rtpDownloadTerminalTTL-time.Second)))
	api.rtpDownloads.Store(activeKey, testRTPDownloadSession(gb10DeviceID, gb10ChannelID, time.Time{}))
	api.rtpDownloads.Store(recentKey, testRTPDownloadSession(gb10DeviceID, gb10ChannelID, now.Add(-time.Minute)))
	api.rtpDownloads.Store("invalid", "unexpected")

	api.cleanupRTPDownloads(now)

	if _, ok := api.rtpDownloads.Load(expiredKey); ok {
		t.Fatal("expired RTP terminal state was retained")
	}
	if _, ok := api.rtpDownloads.Load(activeKey); !ok {
		t.Fatal("active RTP download was removed")
	}
	if _, ok := api.rtpDownloads.Load(recentKey); !ok {
		t.Fatal("recent channel RTP terminal state was removed")
	}
	if _, ok := api.rtpDownloads.Load("invalid"); ok {
		t.Fatal("invalid RTP download entry was retained")
	}
}

func TestCleanupRTPDownloadsBoundsIndependentSessionStates(t *testing.T) {
	now := time.Now()
	api := &GB28181API{}
	for i := 0; i < rtpDownloadMaxSessionTerminalStates+2; i++ {
		key := fmt.Sprintf("cascade-download-%04d", i)
		api.rtpDownloads.Store(key, testRTPDownloadSession(gb10DeviceID, gb10ChannelID, now.Add(time.Duration(i)*time.Nanosecond)))
	}
	activeKey := "cascade-download-active"
	api.rtpDownloads.Store(activeKey, testRTPDownloadSession(gb10DeviceID, gb10ChannelID, time.Time{}))

	api.cleanupRTPDownloads(now.Add(time.Second))

	terminalCount := 0
	api.rtpDownloads.Range(func(key, value any) bool {
		session, ok := value.(*rtpDownloadSession)
		if ok && !session.snapshot().CompletedAt.IsZero() {
			terminalCount++
		}
		return true
	})
	if terminalCount != rtpDownloadMaxSessionTerminalStates {
		t.Fatalf("RTP session terminal states = %d; want %d", terminalCount, rtpDownloadMaxSessionTerminalStates)
	}
	if _, ok := api.rtpDownloads.Load("cascade-download-0000"); ok {
		t.Fatal("oldest RTP session terminal state was retained")
	}
	if _, ok := api.rtpDownloads.Load(activeKey); !ok {
		t.Fatal("active RTP session was removed by capacity cleanup")
	}
}

func testRTPDownloadSession(deviceID, channelID string, completedAt time.Time) *rtpDownloadSession {
	state := RTPDownloadState{
		SessionID: "test-session", DeviceID: deviceID, ChannelID: channelID,
		Status: rtpDownloadReceiving, StartedAt: completedAt.Add(-time.Second), UpdatedAt: completedAt,
		CompletedAt: completedAt,
	}
	if completedAt.IsZero() {
		state.StartedAt = time.Now()
		state.UpdatedAt = state.StartedAt
	}
	return &rtpDownloadSession{state: state}
}
