package gbs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestSIPResponseContextReturnsCallerCancellation(t *testing.T) {
	tx := sip.NewTransaction("context-cancel", nil)
	defer tx.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	if _, err := sipResponseContext(ctx, tx); !errors.Is(err, context.Canceled) {
		t.Fatalf("SIP response cancellation error = %v; want %v", err, context.Canceled)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("SIP response cancellation took %s", elapsed)
	}
	if response, err := tx.GetResponseContext(context.Background()); response != nil || err != nil {
		t.Fatalf("cancelled SIP transaction remained active: response=%v err=%v", response, err)
	}
}

func TestSIPResponseContextRejectsMissingTransaction(t *testing.T) {
	if _, err := sipResponseContext(context.Background(), nil); err == nil {
		t.Fatal("missing SIP transaction was accepted")
	}
}

func TestGBOperationsRejectCancelledContextBeforeSideEffects(t *testing.T) {
	api := &GB28181API{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		call func() error
	}{
		{name: "catalog", call: func() error { return api.QueryCatalogContext(ctx, gb10DeviceID) }},
		{name: "play", call: func() error { return api.PlayContext(ctx, nil) }},
		{name: "stop play", call: func() error { return api.StopPlay(ctx, nil) }},
		{name: "history", call: func() error { return api.StartHistory(ctx, nil) }},
		{name: "stop history", call: func() error { return api.StopHistory(ctx, nil) }},
		{name: "history control", call: func() error { return api.ControlHistory(ctx, nil) }},
		{name: "ptz", call: func() error { _, err := api.PTZContext(ctx, nil); return err }},
		{name: "device control", call: func() error { _, err := api.DeviceControl(ctx, nil); return err }},
		{name: "device query", call: func() error { _, err := api.DeviceQuery(ctx, nil); return err }},
		{name: "device config", call: func() error { _, err := api.SetDeviceConfig(ctx, nil); return err }},
		{name: "record query", call: func() error { _, err := api.QueryRecordList(ctx, nil); return err }},
		{name: "subscribe", call: func() error { return api.Subscribe(ctx, nil) }},
		{name: "time sync", call: func() error { return api.SyncTime(ctx, nil) }},
		{name: "options", call: func() error { return api.ProbeOptions(ctx, nil) }},
		{name: "upgrade", call: func() error { _, err := api.Upgrade(ctx, nil); return err }},
		{name: "snapshot", call: func() error { _, err := api.QuerySnapshotContext(ctx, "", "", ""); return err }},
		{name: "voice", call: func() error { return api.StartVoice(ctx, nil) }},
		{name: "stop voice", call: func() error { return api.StopVoice(ctx, nil) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled operation error = %v; want %v", err, context.Canceled)
			}
		})
	}
}
