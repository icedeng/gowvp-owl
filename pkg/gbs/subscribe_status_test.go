package gbs

import (
	"context"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestOutgoingSubscriptionStatesExposeEstablishedDialogWithoutInternalKey(t *testing.T) {
	api := &GB28181API{}
	now := time.Now().UTC().Truncate(time.Second)
	body, err := sip.XMLEncode(subscribeEventRequest{
		CmdType:            "Alarm",
		SN:                 1001,
		DeviceID:           gb10DeviceID,
		StartAlarmPriority: "1",
		EndAlarmPriority:   "4",
		AlarmMethod:        "2/5",
		AlarmType:          "2",
		StartAlarmTime:     "2026-08-31T08:00:00",
		EndAlarmTime:       "2026-08-31T09:00:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	dialog := &outgoingSubscriptionDialog{
		response:    &sip.Response{},
		requestBody: body,
		deviceID:    gb10DeviceID,
		targetID:    gb10DeviceID,
		expires:     3600,
		expiresAt:   now.Add(time.Hour),
		refreshAt:   now.Add(54 * time.Minute),
	}
	dialog.notify = outgoingSubscriptionNotifyDialog{
		cmdType:           "Alarm",
		deviceID:          gb10DeviceID,
		targetID:          gb10DeviceID,
		expiresAt:         now.Add(55 * time.Minute),
		reportedExpiresAt: now.Add(55 * time.Minute),
		cseq:              7,
	}
	api.outgoingSubscriptions.Store("internal-key-must-not-leak", dialog)
	api.outgoingSubscriptions.Store("pending-dialog", &outgoingSubscriptionDialog{deviceID: gb10DeviceID})
	api.outgoingSubscriptions.Store("other-device", &outgoingSubscriptionDialog{
		response: &sip.Response{}, deviceID: "34020000002000000099", targetID: "34020000002000000099",
		expiresAt: now.Add(time.Hour), requestBody: body,
	})

	states, err := api.OutgoingSubscriptionStates(context.Background(), gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("subscription states = %+v, want one established dialog", states)
	}
	state := states[0]
	if state.DeviceID != gb10DeviceID || state.TargetID != gb10DeviceID || state.Event != "alarm" || state.Status != "active" {
		t.Fatalf("subscription identity/status = %+v", state)
	}
	if state.Expires != 3600 || !state.ExpiresAt.Equal(dialog.expiresAt) || !state.RefreshAt.Equal(dialog.refreshAt) || state.NotifyCSeq != 7 || !state.NotifyExpiresAt.Equal(dialog.notify.expiresAt) {
		t.Fatalf("subscription lifecycle = %+v", state)
	}
	if state.StartAlarmPriority != "1" || state.EndAlarmPriority != "4" || state.AlarmMethod != "2/5" || state.AlarmType != "2" ||
		state.StartAlarmTime != "2026-08-31T08:00:00" || state.EndAlarmTime != "2026-08-31T09:00:00" {
		t.Fatalf("subscription filters = %+v", state)
	}
}

func TestOutgoingSubscriptionStatesReportCancellationAndHonorContext(t *testing.T) {
	api := &GB28181API{}
	body, err := sip.XMLEncode(subscribeEventRequest{CmdType: "MobilePosition", DeviceID: gb10DeviceID, Interval: subscriptionStatusIntPointer(5)})
	if err != nil {
		t.Fatal(err)
	}
	dialog := &outgoingSubscriptionDialog{
		response: &sip.Response{}, requestBody: body, deviceID: gb10DeviceID, targetID: gb10DeviceID,
		expiresAt: time.Now().Add(time.Second),
	}
	dialog.cancelPending.Store(true)
	api.outgoingSubscriptions.Store("cancelled", dialog)

	states, err := api.OutgoingSubscriptionStates(context.Background(), gb10DeviceID)
	if err != nil || len(states) != 1 || states[0].Status != "terminating" || !states[0].CancelPending || states[0].Interval != 5 {
		t.Fatalf("terminating subscription states = %+v, err = %v", states, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := api.OutgoingSubscriptionStates(ctx, gb10DeviceID); err == nil {
		t.Fatal("cancelled context was ignored")
	}
}

func subscriptionStatusIntPointer(value int) *int { return &value }
