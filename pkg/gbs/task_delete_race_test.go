package gbs

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestDeviceDeleteSerializesFinalTaskNotifications(t *testing.T) {
	tests := []struct {
		name    string
		callID  string
		body    []byte
		setup   func(*GB28181API)
		remove  func(*GB28181API)
		exists  func(*GB28181API) bool
		handler func(*GB28181API, *sip.Context)
	}{
		{
			name:   "upgrade",
			callID: "upgrade-delete-race",
			body: []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>801</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><SessionID>upgrade-delete-race-session-00001</SessionID><UpgradeResult>OK</UpgradeResult>` +
				`<Firmware>V2.0.0</Firmware></Notify>`),
			setup: func(api *GB28181API) {
				api.storeUpgradeState(UpgradeState{
					DeviceID: gb10DeviceID, ChannelID: gb10DeviceID,
					SessionID: "upgrade-delete-race-session-00001", Status: "accepted",
				})
			},
			remove: func(api *GB28181API) {
				api.upgradeStateMu.Lock()
				delete(api.upgradeStates, upgradeStateKey(gb10DeviceID, "upgrade-delete-race-session-00001"))
				api.upgradeStateMu.Unlock()
			},
			exists: func(api *GB28181API) bool {
				_, ok := api.UpgradeState(gb10DeviceID, "upgrade-delete-race-session-00001")
				return ok
			},
			handler: func(api *GB28181API, ctx *sip.Context) {
				api.sipMessageDeviceUpgradeResult(ctx)
			},
		},
		{
			name:   "snapshot",
			callID: "snapshot-delete-race",
			body: []byte(`<Notify><CmdType>UploadSnapShotFinished</CmdType><SN>802</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><SessionID>snapshot-delete-race-session-0001</SessionID><SnapShotList>` +
				`<SnapShotFileID>` + gb10DeviceID + `022026082508150000001</SnapShotFileID>` +
				`</SnapShotList></Notify>`),
			setup: func(api *GB28181API) {
				api.storeSnapshotState(SnapshotState{
					DeviceID: gb10DeviceID, ChannelID: gb10DeviceID,
					SessionID: "snapshot-delete-race-session-0001", Status: "uploading", ExpectedCount: 1,
				})
			},
			remove: func(api *GB28181API) {
				api.snapshotStateMu.Lock()
				delete(api.snapshotStates, snapshotStateKey(gb10DeviceID, "snapshot-delete-race-session-0001"))
				api.snapshotStateMu.Unlock()
			},
			exists: func(api *GB28181API) bool {
				_, ok := api.SnapshotState(gb10DeviceID, "snapshot-delete-race-session-0001")
				return ok
			},
			handler: func(api *GB28181API, ctx *sip.Context) {
				api.sipMessageSnapshotFinished(ctx)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(GBVersion30)
			test.setup(api)

			unlockDelete := api.lockRegisterOperation(gb10DeviceID)
			connection := newFlowConnection()
			request := newFlowRequest(t, connection, sip.MethodMessage, test.callID, test.body)
			requestCtx := &sip.Context{
				Request: request, Tx: sip.NewTransaction(test.callID+"-tx", connection),
				DeviceID: gb10DeviceID, Source: connection.remote,
				To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
			}
			entered := make(chan struct{})
			done := make(chan struct{})
			go func() {
				close(entered)
				test.handler(api, requestCtx)
				close(done)
			}()
			<-entered
			select {
			case <-done:
				unlockDelete()
				t.Fatal("final task notification bypassed the device delete lock")
			case <-time.After(100 * time.Millisecond):
			}

			api.deviceDeletionTombstones.Store(gb10DeviceID, struct{}{})
			test.remove(api)
			unlockDelete()

			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("final task notification did not finish after device delete unlocked")
			}
			select {
			case payload := <-connection.writes:
				if !strings.Contains(string(payload), "SIP/2.0 403") {
					t.Fatalf("notification after device delete response = %s", payload)
				}
			case <-time.After(time.Second):
				t.Fatal("notification after device delete response timeout")
			}
			if test.exists(api) {
				t.Fatal("final task notification recreated state after device delete")
			}
		})
	}
}

func TestDeviceDeleteSerializesAlarmInboxPersistence(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	t.Cleanup(func() { config = previousConfig })

	store := &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion20)}
	server := &Server{memoryStorer: store}
	api := &GB28181API{svr: server}
	server.gb = api
	body := readGB10Fixture(t, "alarm-notify.xml")
	deliveryID := alarmDeliveryID(gb10DeviceID, sip.MethodMessage, body)

	unlockDelete := api.lockRegisterOperation(gb10DeviceID)
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodMessage, "alarm-delete-race", body)
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("alarm-delete-race-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
		To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
	}
	entered := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(entered)
		api.sipMessageAlarm(requestCtx)
		close(done)
	}()
	<-entered

	var earlyResponse []byte
	select {
	case earlyResponse = <-connection.writes:
	case <-time.After(100 * time.Millisecond):
	}

	// 模拟持有同一 REGISTER 锁的删除事务：先立墓碑并清理任务状态，再允许迟到请求继续。
	api.deviceDeletionTombstones.Store(gb10DeviceID, struct{}{})
	store.mu.Lock()
	clear(store.records)
	store.mu.Unlock()
	unlockDelete()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Alarm notification did not finish after device delete unlocked")
	}
	response := earlyResponse
	if len(response) == 0 {
		select {
		case response = <-connection.writes:
		case <-time.After(time.Second):
			t.Fatal("Alarm notification after device delete response timeout")
		}
	}
	if !strings.Contains(string(response), "SIP/2.0 403") {
		t.Fatalf("Alarm notification after device delete response = %s", response)
	}
	if _, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmInbox, gb10DeviceID, deliveryID); err != nil || ok {
		t.Fatalf("Alarm inbox after device delete = ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmReceipt, gb10DeviceID, deliveryID); err != nil || ok {
		t.Fatalf("Alarm receipt after device delete = ok=%v err=%v", ok, err)
	}
}

func TestDeviceDeletePreventsLateVideoUploadCascadeForward(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(testCascadeChannelID, &Channel{
		ChannelID: testCascadeChannelID,
		device:    memory.runtime,
	})
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	worker.effective = GBVersion30
	worker.updateStatus(func(status *CascadePlatformStatus) { status.Registered = true })
	forwarded := make(chan struct{}, 1)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		forwarded <- struct{}{}
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	manager := NewCascadeManager(nil)
	manager.items[platform.name] = worker
	server := &Server{memoryStorer: memory, cascade: manager}
	api := &GB28181API{svr: server}
	server.gb = api

	unlockDelete := api.lockRegisterOperation(gb10DeviceID)
	connection := newFlowConnection()
	body := []byte(`<Notify><CmdType>VideoUploadNotify</CmdType><SN>803</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><Time>2026-08-26T20:00:00</Time><Longitude>120.12</Longitude><Latitude>30.28</Latitude></Notify>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "video-upload-delete-race", body)
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("video-upload-delete-race-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
		To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
	}
	done := make(chan struct{})
	go func() {
		api.sipMessageVideoUploadNotify(requestCtx)
		close(done)
	}()

	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200") {
			unlockDelete()
			t.Fatalf("VideoUploadNotify response = %s", payload)
		}
	case <-time.After(time.Second):
		unlockDelete()
		t.Fatal("VideoUploadNotify response timeout")
	}
	api.deviceDeletionTombstones.Store(gb10DeviceID, struct{}{})
	unlockDelete()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("VideoUploadNotify handler did not finish after device delete unlocked")
	}
	api.lifecycleWG.Wait()
	select {
	case <-forwarded:
		t.Fatal("VideoUploadNotify was forwarded after device delete completed")
	default:
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.VideoUpload != nil {
		t.Fatalf("VideoUploadNotify recreated deleted query state: %+v", state.VideoUpload)
	}
}

func TestRecreatedDeviceRejectsPreviousGenerationVideoUploadNotify(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api
	_, oldBinding, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}

	// 模拟同编码设备已完成删除并创建新的运行态；旧请求此前已经通过访问控制。
	memory.runtime = &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	connection := newFlowConnection()
	body := []byte(`<Notify><CmdType>VideoUploadNotify</CmdType><SN>804</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Time>2026-08-26T20:01:00</Time><Longitude>120.13</Longitude><Latitude>30.29</Latitude></Notify>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "video-upload-old-generation", body)
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("video-upload-old-generation-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
		To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
	}
	requestCtx.Set(inboundRegistrationBindingContextKey, oldBinding)
	api.sipMessageVideoUploadNotify(requestCtx)
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200") {
			t.Fatalf("old-generation VideoUploadNotify response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("old-generation VideoUploadNotify response timeout")
	}
	api.lifecycleWG.Wait()
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.VideoUpload != nil {
		t.Fatalf("old-generation VideoUploadNotify polluted recreated device: %+v", state.VideoUpload)
	}
}

func TestRecreatedDeviceRejectsPreviousGenerationFinalTaskNotifications(t *testing.T) {
	tests := []struct {
		name      string
		callID    string
		body      []byte
		setup     func(*GB28181API)
		unchanged func(*GB28181API) bool
		handler   func(*GB28181API, *sip.Context)
	}{
		{
			name:   "upgrade",
			callID: "upgrade-old-generation",
			body: []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>805</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><SessionID>upgrade-new-generation-session-01</SessionID><UpgradeResult>OK</UpgradeResult>` +
				`<Firmware>old-generation-firmware</Firmware></Notify>`),
			setup: func(api *GB28181API) {
				api.storeUpgradeState(UpgradeState{
					DeviceID: gb10DeviceID, ChannelID: gb10DeviceID,
					SessionID: "upgrade-new-generation-session-01", Status: "accepted", Firmware: "new-generation-firmware",
				})
			},
			unchanged: func(api *GB28181API) bool {
				state, ok := api.UpgradeState(gb10DeviceID, "upgrade-new-generation-session-01")
				return ok && state.Status == "accepted" && state.Firmware == "new-generation-firmware"
			},
			handler: func(api *GB28181API, ctx *sip.Context) {
				api.sipMessageDeviceUpgradeResult(ctx)
			},
		},
		{
			name:   "snapshot",
			callID: "snapshot-old-generation",
			body: []byte(`<Notify><CmdType>UploadSnapShotFinished</CmdType><SN>806</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><SessionID>snapshot-new-generation-session-1</SessionID><SnapShotList>` +
				`<SnapShotFileID>` + gb10DeviceID + `022026082508160000001</SnapShotFileID>` +
				`</SnapShotList></Notify>`),
			setup: func(api *GB28181API) {
				api.storeSnapshotState(SnapshotState{
					DeviceID: gb10DeviceID, ChannelID: gb10DeviceID,
					SessionID: "snapshot-new-generation-session-1", Status: "uploading", ExpectedCount: 1,
				})
			},
			unchanged: func(api *GB28181API) bool {
				state, ok := api.SnapshotState(gb10DeviceID, "snapshot-new-generation-session-1")
				return ok && state.Status == "uploading" && len(state.FileIDs) == 0
			},
			handler: func(api *GB28181API, ctx *sip.Context) {
				api.sipMessageSnapshotFinished(ctx)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(GBVersion30)
			server := &Server{memoryStorer: memory}
			api := &GB28181API{svr: server}
			server.gb = api
			_, oldBinding, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
			if err != nil {
				t.Fatal(err)
			}

			// 新运行态创建的同会话任务不能被旧运行态已经通过门禁的通知完成。
			memory.runtime = &Device{IsOnline: true, gbVersion: string(GBVersion30)}
			test.setup(api)
			connection := newFlowConnection()
			request := newFlowRequest(t, connection, sip.MethodMessage, test.callID, test.body)
			requestCtx := &sip.Context{
				Request: request, Tx: sip.NewTransaction(test.callID+"-tx", connection),
				DeviceID: gb10DeviceID, Source: connection.remote,
				To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
			}
			requestCtx.Set(inboundRegistrationBindingContextKey, oldBinding)
			test.handler(api, requestCtx)
			select {
			case payload := <-connection.writes:
				if !strings.Contains(string(payload), "SIP/2.0 200") {
					t.Fatalf("old-generation notification response = %s", payload)
				}
			case <-time.After(time.Second):
				t.Fatal("old-generation notification response timeout")
			}
			if !test.unchanged(api) {
				t.Fatal("old-generation notification changed recreated device task state")
			}
		})
	}
}

func TestRecreatedDeviceRejectsPreviousGenerationMobilePosition(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion20)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api
	_, oldBinding, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}

	memory.runtime = &Device{IsOnline: true, gbVersion: string(GBVersion20)}
	connection := newFlowConnection()
	body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>807</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Time>2026-08-30T10:00:00</Time><Longitude>120.1</Longitude><Latitude>30.1</Latitude></Notify>`)
	request := newFlowRequest(t, connection, sip.MethodNotify, "mobile-position-old-generation", body)
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("mobile-position-old-generation-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
		To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
	}
	requestCtx.Set(inboundRegistrationBindingContextKey, oldBinding)
	api.sipNotifyMobilePosition(requestCtx)
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200") {
			t.Fatalf("old-generation MobilePosition response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("old-generation MobilePosition response timeout")
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && (state.MobilePosition != nil || len(state.MobilePositions) != 0) {
		t.Fatalf("old-generation MobilePosition polluted recreated device: %+v", state)
	}
}

func TestRecreatedDeviceRejectsPreviousGenerationPTZPositionNotify(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api
	_, oldBinding, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}

	memory.runtime = &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	connection := newFlowConnection()
	body := []byte(`<Notify><CmdType>PTZPosition</CmdType><SN>810</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Pan>12.5</Pan><Tilt>-3.25</Tilt><Zoom>2</Zoom></Notify>`)
	request := newFlowRequest(t, connection, sip.MethodNotify, "ptz-position-old-generation", body)
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("ptz-position-old-generation-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
		To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
	}
	requestCtx.Set(inboundRegistrationBindingContextKey, oldBinding)
	api.sipMessageQueryGeneric(requestCtx)
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200") {
			t.Fatalf("old-generation PTZPosition response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("old-generation PTZPosition response timeout")
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.PTZPosition != nil {
		t.Fatalf("old-generation PTZPosition polluted recreated device: %+v", state.PTZPosition)
	}
}

func TestRecreatedDeviceRejectsPreviousGenerationDeviceConfigResponse(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api
	_, oldBinding, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}

	memory.runtime = &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	connection := newFlowConnection()
	body := []byte(`<Response><CmdType>DeviceConfig</CmdType><SN>811</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Result>OK</Result></Response>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "device-config-old-generation", body)
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("device-config-old-generation-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
		To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
	}
	requestCtx.Set(inboundRegistrationBindingContextKey, oldBinding)
	api.handleDeviceConfig(requestCtx)
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200") {
			t.Fatalf("old-generation DeviceConfig response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("old-generation DeviceConfig response timeout")
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.DeviceConfig != nil {
		t.Fatalf("old-generation DeviceConfig polluted recreated device: %+v", state.DeviceConfig)
	}
}

func TestRecreatedDeviceRejectsPreviousGenerationMediaStatus(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	streams := &conc.Map[string, *Streams]{}
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server, streams: streams}
	server.gb = api
	_, oldBinding, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}

	memory.runtime = &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	const callID = "media-status-new-generation"
	key := historyKey(historyModePlayback, gb10DeviceID, gb10DeviceID)
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10DeviceID, CallID: callID}
	streams.Store(key, stream)
	connection := newFlowConnection()
	body := []byte(`<Notify><CmdType>MediaStatus</CmdType><SN>812</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><NotifyType>121</NotifyType></Notify>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, callID, body)
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("media-status-old-generation-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
		To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
	}
	requestCtx.Set(inboundRegistrationBindingContextKey, oldBinding)
	api.sipMessageMediaStatus(requestCtx)
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200") {
			t.Fatalf("old-generation MediaStatus response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("old-generation MediaStatus response timeout")
	}
	if current, ok := streams.Load(key); !ok || current != stream || stream.Stop {
		t.Fatalf("old-generation MediaStatus stopped recreated device stream: present=%v stop=%v", ok, stream.Stop)
	}
}

func TestRecreatedDeviceRejectsPreviousGenerationAlarmSideEffects(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	t.Cleanup(func() { config = previousConfig })
	store := &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion20)}
	server := &Server{memoryStorer: store}
	api := &GB28181API{svr: server}
	server.gb = api
	_, oldBinding, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}

	store.device = &Device{IsOnline: true, gbVersion: string(GBVersion20)}
	called := false
	api.SetAlarmHandler(func(context.Context, *AlarmEvent) { called = true })
	connection := newFlowConnection()
	body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>808</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod>` +
		`<AlarmTime>2026-08-30T10:00:01</AlarmTime><AlarmDescription>old generation</AlarmDescription>` +
		`<Longitude>120.1</Longitude><Latitude>30.1</Latitude></Notify>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "alarm-old-generation", body)
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("alarm-old-generation-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
		To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
	}
	requestCtx.Set(inboundRegistrationBindingContextKey, oldBinding)
	api.sipMessageAlarm(requestCtx)
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200") {
			t.Fatalf("old-generation Alarm response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("old-generation Alarm response timeout")
	}
	if called {
		t.Fatal("old-generation Alarm reached recreated device business callback")
	}
	deliveryID := alarmDeliveryID(gb10DeviceID, sip.MethodMessage, body)
	if _, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmInbox, gb10DeviceID, deliveryID); err != nil || ok {
		t.Fatalf("old-generation Alarm inbox = ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmReceipt, gb10DeviceID, deliveryID); err != nil || ok {
		t.Fatalf("old-generation Alarm receipt = ok=%v err=%v", ok, err)
	}
}

func TestRecreatedDeviceRejectsQueuedPreviousGenerationAlarmCascade(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion20)
	platform := testSharedCascadePlatform(t)
	platform.alarmDispatchEnabled = true
	worker := newCascadeWorker(nil, platform)
	t.Cleanup(worker.cancel)
	worker.effective = GBVersion20
	worker.updateStatus(func(status *CascadePlatformStatus) {
		status.Registered = true
		status.State = "registered"
	})
	forwarded := make(chan struct{}, 1)
	worker.exchange = func(context.Context, *sip.Request) (*sip.Response, error) {
		forwarded <- struct{}{}
		return nil, context.Canceled
	}
	manager := NewCascadeManager(nil)
	manager.items[platform.name] = worker
	server := &Server{memoryStorer: memory, cascade: manager}
	api := &GB28181API{svr: server}
	server.gb = api
	_, oldBinding, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}

	unlockGeneration := api.lockRegisterOperation(gb10DeviceID)
	api.dispatchAlarmToCascadeTargets(gb10DeviceID, cascadeAlarmPayload(), oldBinding)
	memory.runtime = &Device{IsOnline: true, gbVersion: string(GBVersion20)}
	unlockGeneration()
	api.lifecycleWG.Wait()
	select {
	case <-forwarded:
		t.Fatal("queued old-generation Alarm was forwarded after recreated device became current")
	default:
	}
}

func TestRecreatedDeviceRejectsPreviousGenerationCatalogRefresh(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	blockedCatalog := &keyedOperationLock{refs: 1}
	blockedCatalog.mutex.Lock()
	server := &Server{memoryStorer: memory}
	api := &GB28181API{
		svr: server, lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		catalogOperations: map[string]*keyedOperationLock{gb10DeviceID: blockedCatalog},
	}
	server.gb = api
	t.Cleanup(func() {
		lifecycleCancel()
		blockedCatalog.mutex.Unlock()
		api.lifecycleWG.Wait()
	})
	_, oldBinding, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}

	memory.runtime = &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	connection := newFlowConnection()
	body := []byte(`<Notify><CmdType>Catalog</CmdType><SN>809</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SumNum>0</SumNum><DeviceList Num="0"></DeviceList></Notify>`)
	request := newFlowRequest(t, connection, sip.MethodNotify, "catalog-old-generation", body)
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("catalog-old-generation-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
		To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
	}
	requestCtx.Set(inboundRegistrationBindingContextKey, oldBinding)
	api.sipNotifyCatalog(requestCtx)
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200") {
			t.Fatalf("old-generation Catalog response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("old-generation Catalog response timeout")
	}
	api.catalogRefreshMu.Lock()
	refreshCount := len(api.catalogRefreshes)
	api.catalogRefreshMu.Unlock()
	if refreshCount != 0 {
		t.Fatalf("old-generation Catalog scheduled %d refreshes for recreated device", refreshCount)
	}
}

func TestRecreatedAlarmReceiverRejectsPreviousGenerationBusinessResponse(t *testing.T) {
	memory := newFlowMemory(testAlarmReceiverID)
	memory.runtime.setGBVersion(GBVersion20)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api
	_, oldBinding, err := api.ensureRegisteredInboundDeviceWithBinding(testAlarmReceiverID)
	if err != nil {
		t.Fatal(err)
	}

	// 新代次接警终端复用了相同编码和业务 SN，旧代次响应不能完成新等待任务。
	memory.runtime = &Device{IsOnline: true, gbVersion: string(GBVersion20)}
	operation := newPendingDeviceOperation(t.Context(), testAlarmReceiverID, testCascadeChannelID)
	defer operation.Cancel(nil)
	key := pendingLocalAlarmDispatchKey{receiverID: testAlarmReceiverID, sn: 812, deviceID: testCascadeChannelID}
	pending := &pendingLocalAlarmDispatch{wait: make(chan alarmBusinessResponse, 1), operation: operation}
	api.pendingLocalAlarmDispatch.Store(key, pending)
	defer api.pendingLocalAlarmDispatch.Delete(key)

	connection := newFlowConnection()
	body := []byte(`<Response><CmdType>Alarm</CmdType><SN>812</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><Result>OK</Result></Response>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "alarm-receiver-old-generation", body)
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("alarm-receiver-old-generation-tx", connection),
		DeviceID: testAlarmReceiverID, Source: connection.remote,
		To: mustFlowAddress(t, "sip:"+testAlarmReceiverID+"@3402000000"), Log: slog.Default(),
	}
	requestCtx.Set(inboundRegistrationBindingContextKey, oldBinding)
	api.handleLocalAlarmBusinessResponse(requestCtx)
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200") {
			t.Fatalf("old-generation Alarm business response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("old-generation Alarm business response timeout")
	}
	select {
	case response := <-pending.wait:
		t.Fatalf("old-generation Alarm business response completed recreated receiver task: %+v", response)
	default:
	}
}

func TestRecreatedDeviceRejectsPreviousGenerationOutboundBYE(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server, streams: &conc.Map[string, *Streams]{}}
	server.gb = api
	_, oldBinding, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}

	// 旧代次 BYE 已通过入口准入后，设备被删除并以相同编码重建；即使新会话复用
	// Call-ID，旧请求也只能得到协议应答，不能结束新代次媒体。
	memory.runtime = &Device{IsOnline: true, gbVersion: string(GBVersion20)}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		StreamID: "recreated-device-stream", CallID: "recreated-device-outbound-bye",
	}
	key := "play:" + gb10DeviceID + ":" + gb10ChannelID
	api.streams.Store(key, stream)

	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodBYE, stream.CallID, nil)
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("recreated-device-outbound-bye-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
		To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
	}
	requestCtx.Set(inboundRegistrationBindingContextKey, oldBinding)
	api.sipByeGeneric(requestCtx)

	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200") {
			t.Fatalf("old-generation outbound BYE response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("old-generation outbound BYE response timeout")
	}
	if current, ok := api.streams.Load(key); !ok || current != stream || stream.Stop {
		t.Fatal("old-generation outbound BYE stopped recreated device stream")
	}
}

func TestRenewedRegistrationRejectsPreviousGenerationOutboundBYE(t *testing.T) {
	oldRegisteredAt := time.Now().Add(-time.Minute)
	newRegisteredAt := time.Now()
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.LastRegisterAt = oldRegisteredAt
		device.Expires = 3600
	})
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server, streams: &conc.Map[string, *Streams]{}}
	server.gb = api
	_, oldBinding, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}

	// 正常续注册复用运行态对象，但 LastRegisterAt/Expires 已形成新绑定；旧周期已准入
	// 的 BYE 不能终止新周期复用的媒体键。
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.LastRegisterAt = newRegisteredAt
		device.Expires = 7200
	})
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		StreamID: "renewed-registration-stream", CallID: "renewed-registration-outbound-bye",
	}
	key := "play:" + gb10DeviceID + ":" + gb10ChannelID
	api.streams.Store(key, stream)

	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodBYE, stream.CallID, nil)
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("renewed-registration-outbound-bye-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
		To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
	}
	requestCtx.Set(inboundRegistrationBindingContextKey, oldBinding)
	api.sipByeGeneric(requestCtx)

	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200") {
			t.Fatalf("previous-registration outbound BYE response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("previous-registration outbound BYE response timeout")
	}
	if current, ok := api.streams.Load(key); !ok || current != stream || stream.Stop {
		t.Fatal("previous-registration outbound BYE stopped renewed-registration stream")
	}
}

func TestRecreatedDeviceRejectsPreviousGenerationBroadcastINVITE(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	media := &fakeRTPMediaService{startPort: 30000}
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	server := &Server{memoryStorer: memory, fromAddress: *platform}
	api := &GB28181API{
		cfg: &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"},
		svr: server, sms: media, streams: &conc.Map[string, *Streams]{},
	}
	server.gb = api
	_, oldBinding, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}

	// 旧 INVITE 已通过媒体入口准入后才发生设备重建；新代次建立的同通道广播
	// 会话不能被旧报文认领并启动 RTP。
	memory.runtime = &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SourceID: gb10PlatformID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "recreated-microphone",
		SMS: &sms.MediaServer{ID: "local", SDPIP: "192.0.2.20"}, Version: GBVersion11,
		Stream: &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}, ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(gb10ChannelID, session)

	connection := newFlowConnection()
	body := []byte("v=0\r\n" +
		"o=" + gb10ChannelID + " 0 0 IN IP4 192.0.2.10\r\n" +
		"s=Play\r\nc=IN IP4 192.0.2.30\r\nt=0 0\r\n" +
		"m=audio 8000 RTP/AVP 96\r\na=recvonly\r\na=rtpmap:96 PS/90000\r\n")
	request := newFlowRequest(t, connection, sip.MethodInvite, "recreated-device-broadcast-invite", body)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: buildGBInviteSubject(gb10PlatformID, "voice-1", gb10ChannelID)})
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("recreated-device-broadcast-invite-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
		To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
	}
	requestCtx.Set(inboundRegistrationBindingContextKey, oldBinding)
	api.sipInviteGeneric(requestCtx)

	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 403") {
			t.Fatalf("old-generation Broadcast INVITE response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("old-generation Broadcast INVITE response timeout")
	}
	media.mu.Lock()
	startCalls := media.startCalls
	media.mu.Unlock()
	if startCalls != 0 {
		t.Fatalf("old-generation Broadcast INVITE started recreated session RTP %d times", startCalls)
	}
	if current, ok := api.broadcastSessions.Load(gb10ChannelID); !ok || current != session {
		t.Fatal("old-generation Broadcast INVITE removed recreated session")
	}
}

func TestBroadcastINVITERechecksDeviceGenerationBeforePublishingDialog(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	startEntered := make(chan struct{})
	startRelease := make(chan struct{})
	media := &fakeRTPMediaService{startPort: 30000, startEntered: startEntered, startRelease: startRelease}
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	server := &Server{memoryStorer: memory, fromAddress: *platform}
	api := &GB28181API{
		cfg: &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"},
		svr: server, sms: media, streams: &conc.Map[string, *Streams]{},
	}
	server.gb = api
	_, admitted, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SourceID: gb10PlatformID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "generation-race-microphone",
		SMS: &sms.MediaServer{ID: "local", SDPIP: "192.0.2.20"}, Version: GBVersion11,
		Stream: &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}, ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(gb10ChannelID, session)

	connection := newFlowConnection()
	body := []byte("v=0\r\n" +
		"o=" + gb10ChannelID + " 0 0 IN IP4 192.0.2.10\r\n" +
		"s=Play\r\nc=IN IP4 192.0.2.30\r\nt=0 0\r\n" +
		"m=audio 8000 RTP/AVP 96\r\na=recvonly\r\na=rtpmap:96 PS/90000\r\n")
	request := newFlowRequest(t, connection, sip.MethodInvite, "broadcast-generation-commit-race", body)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: buildGBInviteSubject(gb10PlatformID, "voice-1", gb10ChannelID)})
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("broadcast-generation-commit-race-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
		To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
	}
	requestCtx.Set(inboundRegistrationBindingContextKey, admitted)
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.sipInviteGeneric(requestCtx)
	}()

	select {
	case <-startEntered:
	case <-time.After(time.Second):
		close(startRelease)
		t.Fatal("Broadcast RTP start was not reached")
	}
	memory.runtime = &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	close(startRelease)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Broadcast INVITE did not finish after RTP start resumed")
	}
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 487") {
			t.Fatalf("generation-changed Broadcast INVITE response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("generation-changed Broadcast INVITE response timeout")
	}
	media.mu.Lock()
	startCalls, stopCalls := media.startCalls, media.stopCalls
	media.mu.Unlock()
	if startCalls != 1 || stopCalls != 1 {
		t.Fatalf("generation-changed Broadcast RTP lifecycle = starts:%d stops:%d", startCalls, stopCalls)
	}
	if _, ok := api.inviteDialogs.Load("broadcast-generation-commit-race"); ok {
		t.Fatal("generation-changed Broadcast INVITE published a dialog")
	}
	session.mu.Lock()
	rtpStarted, dialog := session.rtpStarted, session.Dialog
	session.mu.Unlock()
	if rtpStarted || dialog != nil {
		t.Fatal("generation-changed Broadcast INVITE published session state")
	}
}

func TestRecreatedDeviceRejectsPreviousGenerationInboundSUBSCRIBE(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api
	_, oldBinding, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}

	// SUBSCRIBE 已通过访问控制后发生同编码重建；旧请求不能在新代次名下创建订阅。
	memory.runtime = &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	connection := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>813</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	request := newFlowRequest(t, connection, sip.MethodSubscribe, "inbound-subscribe-old-generation", body)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("inbound-subscribe-old-generation-tx", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
		To:     mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
		XGBVer: string(GBVersion11), Log: slog.Default(),
	}
	requestCtx.Set(inboundRegistrationBindingContextKey, oldBinding)
	api.sipSubscribeEvent(requestCtx)

	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 403") {
			t.Fatalf("old-generation inbound SUBSCRIBE response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("old-generation inbound SUBSCRIBE response timeout")
	}
	stored := false
	api.eventSubscribers.Range(func(_, _ any) bool {
		stored = true
		return false
	})
	if stored {
		t.Fatal("old-generation inbound SUBSCRIBE created recreated-device subscription")
	}
}

func TestInboundSUBSCRIBERechecksDeviceGenerationAfterSIPOK(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api
	_, admitted, err := api.ensureRegisteredInboundDeviceWithBinding(gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}

	base := newFlowConnection()
	connection := &blockingFlowResponseConnection{
		flowConnection: base, started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>814</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	request := newFlowRequest(t, base, sip.MethodSubscribe, "inbound-subscribe-generation-commit-race", body)
	request.SetConnection(connection)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
	requestCtx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("inbound-subscribe-generation-commit-race-tx", connection),
		DeviceID: gb10DeviceID, Source: base.remote,
		To:     mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
		XGBVer: string(GBVersion11), Log: slog.Default(),
	}
	requestCtx.Set(inboundRegistrationBindingContextKey, admitted)
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.sipSubscribeEvent(requestCtx)
	}()

	select {
	case <-connection.started:
	case <-time.After(time.Second):
		close(connection.release)
		t.Fatal("inbound SUBSCRIBE SIP response write was not reached")
	}
	memory.runtime = &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	close(connection.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("inbound SUBSCRIBE did not finish after SIP response resumed")
	}
	select {
	case payload := <-base.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200") {
			t.Fatalf("generation-changed inbound SUBSCRIBE response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("generation-changed inbound SUBSCRIBE response timeout")
	}
	stored := false
	api.eventSubscribers.Range(func(_, _ any) bool {
		stored = true
		return false
	})
	if stored {
		t.Fatal("generation-changed inbound SUBSCRIBE committed recreated-device subscription")
	}
}
