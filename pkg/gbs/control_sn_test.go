package gbs

import (
	"context"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestProtocolSNRemainsPositiveAndWrapsAtMaxInt32(t *testing.T) {
	api := &GB28181API{}
	api.querySN.Store(math.MaxInt32 - 1)
	if got := api.nextQuerySN(); got != math.MaxInt32 {
		t.Fatalf("query SN before wrap = %d", got)
	}
	if got := api.nextQuerySN(); got != 1 {
		t.Fatalf("query SN after wrap = %d", got)
	}
	api.controlSN.Store(math.MaxInt32)
	if got := api.nextControlSN(); got != 1 {
		t.Fatalf("control SN after wrap = %d", got)
	}
}

func TestConfigDownloadUsesUnifiedQuerySN(t *testing.T) {
	api := &GB28181API{}
	api.querySN.Store(40)
	first := string(api.newBasicParamRequest("34020000001320000001"))
	second := string(api.newBasicParamRequest("34020000001320000001"))
	for index, body := range []string{first, second} {
		want := 41 + index
		if !strings.Contains(body, "<SN>"+strconv.Itoa(want)+"</SN>") {
			t.Fatalf("ConfigDownload body does not contain SN %d: %s", want, body)
		}
	}
}

func TestDeviceQueryValidatesInputBeforeAllocatingSN(t *testing.T) {
	tests := []struct {
		name string
		in   DeviceQueryInput
	}{
		{
			name: "alarm filters",
			in: DeviceQueryInput{
				DeviceID: gb10DeviceID, Action: deviceQueryActionAlarm,
				StartAlarmPriority: "4", EndAlarmPriority: "1",
			},
		},
		{
			name: "cruise track number",
			in: DeviceQueryInput{
				DeviceID: gb10DeviceID, Action: deviceQueryActionCruiseTrack, Number: 2,
			},
		},
		{
			name: "catalog time range",
			in: DeviceQueryInput{
				DeviceID: gb10DeviceID, Action: deviceQueryActionCatalog, Start: 2, End: 1,
			},
		},
		{
			name: "unknown target channel",
			in: DeviceQueryInput{
				DeviceID: gb10DeviceID, TargetID: "34020000001320009999", Action: deviceQueryActionDeviceInfo,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(GBVersion30)
			api.querySN.Store(40)
			if _, err := api.DeviceQuery(t.Context(), &test.in); err == nil {
				t.Fatal("invalid DeviceQuery input was accepted")
			}
			if got := api.querySN.Load(); got != 40 {
				t.Fatalf("invalid DeviceQuery consumed query SN: got %d, want 40", got)
			}
		})
	}
}

func TestRecordQueryValidatesInputBeforeTrackingOperation(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	in := &RecordQueryInput{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, Start: 20, End: 10}
	if _, err := api.QueryRecordList(t.Context(), in); err == nil {
		t.Fatal("invalid record query input was accepted")
	}
	tracked := 0
	api.pendingDeviceRequests.Range(func(_, _ any) bool {
		tracked++
		return true
	})
	if tracked != 0 {
		t.Fatalf("invalid record query tracked %d pending operation(s)", tracked)
	}
}

func TestPendingBusinessResponseReservationsSkipActiveSequence(t *testing.T) {
	t.Run("AutomaticQuery", func(t *testing.T) {
		api := &GB28181API{}
		api.querySN.Store(0)
		oldKey := buildPendingQueryKey(gb10DeviceID, "DeviceInfo", 1)
		old := &pendingQueryWait{}
		api.pendingDeviceQuery.Store(oldKey, old)
		sn, cancel := api.reserveAutomaticQueryResponse(gb10DeviceID, "DeviceInfo", gb10DeviceID)
		if sn != 2 {
			t.Fatalf("reserved automatic query SN = %d", sn)
		}
		cancel()
		if current, ok := api.pendingDeviceQuery.Load(oldKey); !ok || current != old {
			t.Fatalf("old automatic query generation changed: %#v, exists=%v", current, ok)
		}
	})

	t.Run("DeviceControl", func(t *testing.T) {
		api := &GB28181API{}
		api.controlSN.Store(0)
		const oldKey = gb10DeviceID + ":1"
		old := &pendingDeviceControl{}
		api.pendingDeviceControl.Store(oldKey, old)
		operation := newPendingDeviceOperation(context.Background(), gb10DeviceID, gb10ChannelID)
		sn, key, pending := api.reservePendingDeviceControl(gb10DeviceID, gb10ChannelID, operation)
		if sn != 2 || key != gb10DeviceID+":2" {
			t.Fatalf("reserved DeviceControl SN/key = %d/%s", sn, key)
		}
		api.pendingDeviceControl.CompareAndDelete(key, pending)
		pending.operation.Cancel(nil)
		if current, ok := api.pendingDeviceControl.Load(oldKey); !ok || current != old {
			t.Fatalf("old DeviceControl generation changed: %#v, exists=%v", current, ok)
		}
	})

	t.Run("DeviceConfig", func(t *testing.T) {
		api := &GB28181API{}
		api.controlSN.Store(0)
		oldKey := buildPendingDeviceConfigKey(gb10DeviceID, 1)
		old := &pendingDeviceConfig{}
		api.pendingDeviceConfig.Store(oldKey, old)
		sn, key, pending, release := api.reservePendingDeviceConfig(context.Background(), gb10DeviceID, gb10ChannelID)
		if sn != 2 || key != buildPendingDeviceConfigKey(gb10DeviceID, 2) {
			t.Fatalf("reserved DeviceConfig SN/key = %d/%s", sn, key)
		}
		api.pendingDeviceConfig.CompareAndDelete(key, pending)
		release()
		if current, ok := api.pendingDeviceConfig.Load(oldKey); !ok || current != old {
			t.Fatalf("old DeviceConfig generation changed: %#v, exists=%v", current, ok)
		}
	})

	t.Run("Broadcast", func(t *testing.T) {
		api := &GB28181API{}
		api.controlSN.Store(0)
		oldKey := buildPendingBroadcastKey(gb10ChannelID, 1)
		old := &pendingBroadcastResponse{}
		api.pendingBroadcast.Store(oldKey, old)
		operation := newPendingDeviceOperation(context.Background(), gb10DeviceID, gb10ChannelID)
		sn, key, pending := api.reservePendingBroadcast(gb10ChannelID, operation)
		if sn != 2 || key != buildPendingBroadcastKey(gb10ChannelID, 2) {
			t.Fatalf("reserved Broadcast SN/key = %d/%s", sn, key)
		}
		api.pendingBroadcast.CompareAndDelete(key, pending)
		pending.operation.Cancel(nil)
		if current, ok := api.pendingBroadcast.Load(oldKey); !ok || current != old {
			t.Fatalf("old Broadcast generation changed: %#v, exists=%v", current, ok)
		}
	})
}

func TestRecordResponseGenerationCleanupDoesNotDeleteReplacement(t *testing.T) {
	api := &GB28181API{}
	recordKey := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 1)
	aliasKey := buildMultiResponseKey(gb10DeviceID, "RecordInfo", 1)

	oldGeneration := &recordResponseGeneration{}
	oldAlias := &recordResponseAlias{recordKey: recordKey, generation: oldGeneration}
	api.startRecordResponseExtraGeneration(recordKey, oldGeneration)
	api.recordResponseAliases.Store(aliasKey, oldAlias)

	// 设备清理先摘除旧代次，随后同一 SN 回绕并创建新查询。
	api.clearRecordResponseExtraGeneration(recordKey, oldGeneration)
	api.recordResponseAliases.CompareAndDelete(aliasKey, oldAlias)
	replacementGeneration := &recordResponseGeneration{}
	replacementAlias := &recordResponseAlias{recordKey: recordKey, generation: replacementGeneration}
	api.startRecordResponseExtraGeneration(recordKey, replacementGeneration)
	api.appendRecordResponseMetadata(recordKey, []byte("<Response>replacement</Response>"), nil)
	api.recordResponseAliases.Store(aliasKey, replacementAlias)

	// 旧查询延迟执行的 defer 只能清理旧对象，不能匹配字符串相同的新代次。
	api.recordResponseAliases.CompareAndDelete(aliasKey, oldAlias)
	api.clearRecordResponseExtraGeneration(recordKey, oldGeneration)
	if current, ok := api.recordResponseAliases.Load(aliasKey); !ok || current != replacementAlias {
		t.Fatalf("replacement RecordInfo alias was deleted: %#v, exists=%v", current, ok)
	}
	metadata := api.takeRecordResponseMetadata(recordKey, replacementGeneration)
	if len(metadata.ResponseXML) != 1 || metadata.ResponseXML[0] != "<Response>replacement</Response>" {
		t.Fatalf("replacement RecordInfo metadata = %+v", metadata)
	}
}
