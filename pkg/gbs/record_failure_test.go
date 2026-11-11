package gbs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func recordFailureAPI(version GBProtocolVersion) (*GB28181API, string) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(version)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	collector := newMultiResponseCollector(func(item RecordItem) string { return item.FilePath })
	key := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 181278)
	collector.Start(key)
	return &GB28181API{svr: &Server{memoryStorer: memory}, recordResponses: collector}, key
}

func TestRecordInfoDeviceFailureCompletesQueryByVersion(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			api, key := recordFailureAPI(version)
			body := readGBVersionFixture(t, "3.0", "device-compat-record-error.xml")
			assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "record-device-error", body, api.sipMessageRecordInfo))
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			result := api.recordResponses.Wait(ctx, key)
			var deviceErr *RecordQueryDeviceError
			if !errors.As(result.Err, &deviceErr) || deviceErr.Reason != "Cannot get record file" || deviceErr.SN != 181278 || result.Complete || len(result.Items) != 0 {
				t.Fatalf("failure was lost or reported as empty success: %+v", result)
			}
			if errors.Is(result.Err, ErrRecordQueryTimeout) {
				t.Fatal("device failure became a timeout")
			}
		})
	}
}

func TestRecordInfoDeviceFailureRejectsInvalidAndUnassociatedResponses(t *testing.T) {
	original := string(readGBVersionFixture(t, "3.0", "device-compat-record-error.xml"))
	for _, test := range []struct{ name, old, replacement, code string }{
		{"duplicate result", "<Result>ERROR</Result>", "<Result>ERROR</Result><Result>ERROR</Result>", "400"},
		{"duplicate reason", "<Reason>Cannot get record file</Reason>", "<Reason>a</Reason><Reason>b</Reason>", "400"},
		{"mixed success and error", "<Result>ERROR</Result>", "<SumNum>0</SumNum><Result>ERROR</Result>", "400"},
		{"success requires records envelope", "<Result>ERROR</Result>", "<Result>OK</Result>", "400"},
		{"unknown field", "</Response>", "<Vendor>value</Vendor></Response>", "400"},
		{"foreign target", gb10ChannelID, "34020000001320000999", "403"},
		{"unassociated SN", "181278", "181279", "200"},
		{"oversized reason", "Cannot get record file", strings.Repeat("中", 1025), "400"},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, key := recordFailureAPI(GBVersion30)
			body := []byte(strings.Replace(original, test.old, test.replacement, 1))
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "invalid-record-error", body, api.sipMessageRecordInfo)
			if !strings.Contains(response, "SIP/2.0 "+test.code) {
				t.Fatal(response)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
			defer cancel()
			result := api.recordResponses.Wait(ctx, key)
			if result.Err != nil || result.Complete {
				t.Fatalf("invalid error terminated query: %+v", result)
			}
		})
	}
}

func TestRecordInfoDeviceFailureParentAliasAndSiblingIsolation(t *testing.T) {
	for _, sibling := range []bool{false, true} {
		api, key := recordFailureAPI(GBVersion30)
		memory := api.svr.memoryStorer.(*flowMemory)
		siblingID := "34020000001320000003"
		memory.runtime.Channels.Store(siblingID, &Channel{ChannelID: siblingID, device: memory.runtime})
		api.recordResponseAliases.Store(buildMultiResponseKey(gb10DeviceID, "RecordInfo", 181278), &recordResponseAlias{recordKey: key, generation: &recordResponseGeneration{}})
		target := gb10DeviceID
		if sibling {
			target = siblingID
		}
		body := []byte(strings.Replace(string(readGBVersionFixture(t, "3.0", "device-compat-record-error.xml")), gb10ChannelID, target, 1))
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "record-parent-error", body, api.sipMessageRecordInfo)
		if sibling && !strings.Contains(response, "SIP/2.0 400") {
			t.Fatal(response)
		}
		if !sibling {
			assertFlowOK(t, response)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		result := api.recordResponses.Wait(ctx, key)
		cancel()
		if (result.Err == nil) != sibling {
			t.Fatalf("sibling=%v result=%+v", sibling, result)
		}
	}
}

func TestRecordInfoDeviceFailureRequiresSIPACKAndOriginalGeneration(t *testing.T) {
	for _, mode := range []string{"success", "write failure", "recreated query"} {
		t.Run(mode, func(t *testing.T) {
			api, key := recordFailureAPI(GBVersion30)
			old := api.recordResponses.Entry(key)
			var writeErr error
			if mode == "write failure" {
				writeErr = errors.New("write failed")
			}
			conn, done := startBlockingFlowHandler(t, api, sip.MethodMessage, "record-error-commit", readGBVersionFixture(t, "3.0", "device-compat-record-error.xml"), api.sipMessageRecordInfo, writeErr)
			api.recordResponses.mu.Lock()
			premature := old.err != nil
			api.recordResponses.mu.Unlock()
			if premature {
				t.Fatal("query ended before successful SIP acknowledgment")
			}
			current := old
			if mode == "recreated query" {
				api.recordResponses.CancelEntry(key, old)
				current = api.recordResponses.Start(key)
			}
			finishBlockingFlowHandler(t, conn, done)
			api.recordResponses.mu.Lock()
			aborted := current.err != nil
			api.recordResponses.mu.Unlock()
			if aborted != (mode == "success") {
				t.Fatalf("mode=%s aborted=%v", mode, aborted)
			}
		})
	}
}
