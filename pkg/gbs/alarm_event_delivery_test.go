package gbs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func prepareAlarmEventTest(_ context.Context, e *AlarmEvent) error {
	e.InternalDeviceID, e.InternalChannelID = "original-device", "original-channel"
	return nil
}

func TestDurableAlarmFourVersionsAndMethods(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		for _, method := range []string{sip.MethodMessage, sip.MethodNotify} {
			t.Run(string(version)+method, func(t *testing.T) {
				store := &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(version)}
				api := &GB28181API{svr: &Server{memoryStorer: store}}
				api.core, _, _ = newCascadeMediaCore(t)
				delivered := 0
				deliver := func(_ context.Context, e *AlarmEvent) error {
					if e.InternalDeviceID != "original-device" || e.InternalChannelID != "original-channel" || e.ProtocolVersion != string(version) {
						t.Fatalf("wrong identity: %+v", e)
					}
					delivered++
					return nil
				}
				api.SetDurableAlarmHandler(prepareAlarmEventTest, deliver)
				body := readGBVersionFixture(t, string(version), "alarm-notify.xml")
				handler := api.sipMessageAlarm
				if method == sip.MethodNotify {
					handler = api.sipNotifyAlarm
				}
				assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, method, "durable-event", body, handler))
				id := alarmDeliveryID(gb10DeviceID, method, body)
				if _, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmEventInbox, gb10DeviceID, id); err != nil || !ok {
					t.Fatalf("not durably accepted: %v", err)
				}
				// 模拟确认后进程退出、设备离线；新实例只使用接纳快照，不等设备重传。
				store.device.IsOnline = false
				restarted := &GB28181API{svr: &Server{memoryStorer: store}}
				restarted.SetDurableAlarmHandler(func(context.Context, *AlarmEvent) error {
					t.Fatal("must not resolve current identity on restart")
					return nil
				}, deliver)
				restarted.processAlarmEventDeliveryBatch(time.Now())
				restarted.processAlarmEventDeliveryBatch(time.Now())
				if delivered != 1 {
					t.Fatalf("deliveries = %d", delivered)
				}
				if _, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmEventReceipt, gb10DeviceID, id); err != nil || !ok {
					t.Fatalf("receipt not saved: %v", err)
				}
			})
		}
	}
}

func TestDurableAlarmPersistenceFailureBeforeACK(t *testing.T) {
	store := &failingAlarmInboxMemory{alarmInboxTestMemory: &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	api.core, _, _ = newCascadeMediaCore(t)
	api.SetDurableAlarmHandler(prepareAlarmEventTest, func(context.Context, *AlarmEvent) error { t.Fatal("not accepted"); return nil })
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "durable-failure", readGBVersionFixture(t, "3.0", "alarm-notify.xml"), api.sipNotifyAlarm)
	if !strings.Contains(response, "SIP/2.0 500") {
		t.Fatal(response)
	}
}

func TestDurableAlarmPanicRetryAndRetransmission(t *testing.T) {
	store := &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	api.core, _, _ = newCascadeMediaCore(t)
	body := readGBVersionFixture(t, "3.0", "alarm-notify.xml")
	id := alarmDeliveryID(gb10DeviceID, sip.MethodNotify, body)
	calls := 0
	api.SetDurableAlarmHandler(prepareAlarmEventTest, func(context.Context, *AlarmEvent) error { calls++; panic("temporary handler failure") })
	assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "durable-panic", body, api.sipNotifyAlarm))
	api.processAlarmEventDeliveryBatch(time.Now())
	payload, ok, err := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmEventInbox, gb10DeviceID, id)
	if err != nil || !ok {
		t.Fatal("failed task lost", err)
	}
	var state alarmEventDeliveryState
	if err := json.Unmarshal(payload, &state); err != nil {
		t.Fatal(err)
	}
	if state.Attempts != 1 || state.LastError == "" || state.NextAttemptAt.IsZero() {
		t.Fatalf("retry missing: %+v", state)
	}
	// 重传不覆盖原始快照和退避，也不重新调用 resolver。
	api.SetDurableAlarmHandler(func(context.Context, *AlarmEvent) error { return fmt.Errorf("device has been recreated") }, func(_ context.Context, e *AlarmEvent) error {
		if e.InternalDeviceID != "original-device" {
			t.Fatal("identity changed")
		}
		calls++
		return nil
	})
	assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "durable-retransmit", body, api.sipNotifyAlarm))
	api.processAlarmEventDeliveryBatch(time.Now())
	if calls != 1 {
		t.Fatal("backoff bypassed")
	}
	api.processAlarmEventDeliveryBatch(state.NextAttemptAt.Add(time.Second))
	if calls != 2 {
		t.Fatal("retry not delivered")
	}
}

func TestDurableAlarmInvalidReceiptDoesNotSuppressEvent(t *testing.T) {
	store := &alarmInboxTestMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	api.core, _, _ = newCascadeMediaCore(t)
	api.SetDurableAlarmHandler(prepareAlarmEventTest, func(context.Context, *AlarmEvent) error { return nil })
	body := readGBVersionFixture(t, "3.0", "alarm-notify.xml")
	id := alarmDeliveryID(gb10DeviceID, sip.MethodNotify, body)
	if err := store.SaveGBTaskState(t.Context(), gbTaskKindAlarmEventReceipt, gb10DeviceID, id, []byte(`{}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "invalid-receipt", body, api.sipNotifyAlarm))
	if _, ok, _ := store.LoadGBTaskState(t.Context(), gbTaskKindAlarmEventInbox, gb10DeviceID, id); !ok {
		t.Fatal("event suppressed by corrupt receipt")
	}
}
