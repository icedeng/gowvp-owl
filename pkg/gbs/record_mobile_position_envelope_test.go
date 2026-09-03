package gbs

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestRecordInfoRejectsInvalidEnvelopeBeforeCollector(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong root", body: `<Notify><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID></Item></RecordList></Notify>`},
		{name: "wrong command", body: `<Response><CmdType>Catalog</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID></Item></RecordList></Response>`},
		{name: "non-positive SN", body: `<Response><CmdType>RecordInfo</CmdType><SN>0</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID></Item></RecordList></Response>`},
		{name: "missing name", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID></Item></RecordList></Response>`},
		{name: "missing sum", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><RecordList Num="0"></RecordList></Response>`},
		{name: "negative sum", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>-1</SumNum><RecordList Num="0"></RecordList></Response>`},
		{name: "missing record list", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>0</SumNum></Response>`},
		{name: "missing list count", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList><Item><DeviceID>` + gb10ChannelID + `</DeviceID></Item></RecordList></Response>`},
		{name: "list count mismatch", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="0"><Item><DeviceID>` + gb10ChannelID + `</DeviceID></Item></RecordList></Response>`},
		{name: "unknown target", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>34020000001320000009</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>34020000001320000009</DeviceID></Item></RecordList></Response>`},
		{name: "item target mismatch", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10DeviceID + `</DeviceID></Item></RecordList></Response>`},
		{name: "missing item name", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Secrecy>0</Secrecy></Item></RecordList></Response>`},
		{name: "missing item secrecy", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Name>record</Name></Item></RecordList></Response>`},
		{name: "invalid item secrecy", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Name>record</Name><Secrecy>2</Secrecy></Item></RecordList></Response>`},
		{name: "2011 Appendix A.4 ExtraInfo", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>0</SumNum><RecordList Num="0"></RecordList><ExtraInfo>{"type":"doorType","DeviceID":"` + gb10ChannelID + `"}</ExtraInfo></Response>`},
		{name: "duplicate sum", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><SumNum>0</SumNum></Response>`},
		{name: "unknown top-level field", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>0</SumNum><Vendor>1</Vendor></Response>`},
		{name: "top-level field out of order", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><Name>camera</Name><DeviceID>` + gb10ChannelID + `</DeviceID><SumNum>0</SumNum></Response>`},
		{name: "root attribute", body: `<Response vendor="1"><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>0</SumNum></Response>`},
		{name: "simple field attribute", body: `<Response><CmdType>RecordInfo</CmdType><SN vendor="1">3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>0</SumNum></Response>`},
		{name: "nested simple field", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name><Value>camera</Value></Name><SumNum>0</SumNum></Response>`},
		{name: "unknown list attribute", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1" vendor="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Name>record</Name><Secrecy>0</Secrecy></Item></RecordList></Response>`},
		{name: "duplicate item device", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><DeviceID>` + gb10ChannelID + `</DeviceID><Name>record</Name><Secrecy>0</Secrecy></Item></RecordList></Response>`},
		{name: "unknown item field", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Name>record</Name><Secrecy>0</Secrecy><Vendor>1</Vendor></Item></RecordList></Response>`},
		{name: "item field out of order", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><Name>record</Name><DeviceID>` + gb10ChannelID + `</DeviceID><Secrecy>0</Secrecy></Item></RecordList></Response>`},
		{name: "legacy Info too long", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>0</SumNum><RecordList Num="0"></RecordList><Info>` + strings.Repeat("中", 1025) + `</Info></Response>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			collector := newMultiResponseCollector(func(item RecordItem) string { return item.DeviceID + item.FilePath })
			key := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 3)
			collector.Start(key)
			api := &GB28181API{svr: &Server{memoryStorer: memory}, recordResponses: collector}
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "record-invalid-"+test.name, []byte(test.body), api.sipMessageRecordInfo)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid RecordInfo response = %s", response)
			}
			waitCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			result := collector.Wait(waitCtx, key)
			cancel()
			if result.Complete || len(result.Items) != 0 {
				t.Fatalf("invalid RecordInfo changed collector: %+v", result)
			}
		})
	}
}

func TestRecordInfo2022CollectsAppendixA4ExtraInfo(t *testing.T) {
	base, _, _ := newCascadeMediaCore(t)
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	collector := newMultiResponseCollector(func(item RecordItem) string { return item.DeviceID + item.FilePath })
	key := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 8)
	collector.Start(key)
	api := &GB28181API{core: base, svr: &Server{memoryStorer: memory}, recordResponses: collector}
	generation := api.startRecordResponseExtra(key)
	body := `<Response><CmdType>RecordInfo</CmdType><SN>8</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>0</SumNum><ExtraInfo>{"type":"doorType","DeviceID":"` + gb10ChannelID + `"}</ExtraInfo></Response>`
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "record-2022-extra-info", []byte(body), api.sipMessageRecordInfo)
	assertFlowOK(t, response)
	result := collector.Wait(t.Context(), key)
	if !result.Complete || result.Expected != 0 {
		t.Fatalf("RecordInfo collector result = %+v", result)
	}
	metadata := api.takeRecordResponseMetadata(key, generation)
	if len(metadata.ExtraInfo) != 1 || !strings.Contains(metadata.ExtraInfo[0], `"type":"doorType"`) {
		t.Fatalf("RecordInfo ExtraInfo = %+v", metadata.ExtraInfo)
	}
	if len(metadata.ResponseXML) != 1 || metadata.ResponseXML[0] != body {
		t.Fatalf("RecordInfo response XML = %+v", metadata.ResponseXML)
	}
	if len(metadata.AppendixA4) != 1 || metadata.AppendixA4[0].Type != "doorType" {
		t.Fatalf("RecordInfo Appendix A.4 metadata = %+v", metadata.AppendixA4)
	}
	state, ok := api.GetQueryState(gb10ChannelID)
	if !ok || len(state.AppendixA4) != 1 || state.AppendixA4[0].Type != "doorType" {
		t.Fatalf("RecordInfo Appendix A.4 state = %+v", state)
	}
	if parentState, ok := api.GetQueryState(gb10DeviceID); ok && len(parentState.AppendixA4) != 0 {
		t.Fatalf("RecordInfo channel Appendix A.4 overwrote parent runtime state: %+v", parentState.AppendixA4)
	}
}

func TestRecordInfoMetadataLimitAbortsCollectorWithoutPartialChunk(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	collector := newMultiResponseCollector(func(item RecordItem) string {
		return item.DeviceID + "\x00" + item.FilePath + "\x00" + item.StartTime + "\x00" + item.EndTime
	})
	key := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 18)
	collector.Start(key)
	api := &GB28181API{svr: &Server{memoryStorer: memory}, recordResponses: collector}
	generation := api.startRecordResponseExtra(key)
	api.recordResponseExtraMu.Lock()
	api.recordResponseMetadata[key].bytes = gbRecordResponseMaxMetadataBytes
	api.recordResponseExtraMu.Unlock()

	body := `<Response><CmdType>RecordInfo</CmdType><SN>18</SN><DeviceID>` + gb10ChannelID +
		`</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID +
		`</DeviceID><Name>record</Name><FilePath>/record/overflow.ps</FilePath><Secrecy>0</Secrecy></Item></RecordList></Response>`
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "record-metadata-limit", []byte(body), api.sipMessageRecordInfo)
	assertFlowOK(t, response)

	result := collector.Wait(t.Context(), key)
	if !errors.Is(result.Err, errRecordResponseMetadataLimit) || result.Complete || len(result.Items) != 0 {
		t.Fatalf("RecordInfo metadata overflow result = %+v", result)
	}
	metadata := api.takeRecordResponseMetadata(key, generation)
	if len(metadata.ResponseXML) != 0 || len(metadata.ExtraInfo) != 0 || len(metadata.AppendixA4) != 0 {
		t.Fatalf("overflowing RecordInfo metadata was partially committed: %+v", metadata)
	}
}

func TestRecordQueryFiltersByVersion(t *testing.T) {
	stream0, stream3, streamNegative := 0, 3, -1
	indistinct0, indistinct1, indistinctInvalid := 0, 1, 2
	secrecy0, secrecy1, secrecyInvalid := 0, 1, 2
	tests := []struct {
		name    string
		version GBProtocolVersion
		input   RecordQueryInput
		wantErr bool
	}{
		{name: "legacy query unchanged", version: GBVersion10},
		{name: "2011 all type", version: GBVersion10, input: RecordQueryInput{Type: "all"}},
		{name: "2014 all type", version: GBVersion11, input: RecordQueryInput{Type: "all"}},
		{name: "2016 all type", version: GBVersion20, input: RecordQueryInput{Type: "all"}},
		{name: "2022 all type", version: GBVersion30, input: RecordQueryInput{Type: "ALL"}},
		{name: "alarm type", version: GBVersion20, input: RecordQueryInput{Type: "alarm"}},
		{name: "manual type", version: GBVersion30, input: RecordQueryInput{Type: "MANUAL"}},
		{name: "rejects blank type", version: GBVersion30, input: RecordQueryInput{Type: "  "}, wantErr: true},
		{name: "rejects unknown record type", version: GBVersion30, input: RecordQueryInput{Type: "other"}, wantErr: true},
		{name: "2011 rejects indistinct query", version: GBVersion10, input: RecordQueryInput{IndistinctQuery: &indistinct0}, wantErr: true},
		{name: "2014 supports indistinct query", version: GBVersion11, input: RecordQueryInput{IndistinctQuery: &indistinct1}},
		{name: "2016 supports indistinct query", version: GBVersion20, input: RecordQueryInput{IndistinctQuery: &indistinct0}},
		{name: "rejects invalid indistinct query", version: GBVersion30, input: RecordQueryInput{IndistinctQuery: &indistinctInvalid}, wantErr: true},
		{name: "secrecy public", version: GBVersion10, input: RecordQueryInput{Secrecy: &secrecy0}},
		{name: "secrecy secret", version: GBVersion30, input: RecordQueryInput{Secrecy: &secrecy1}},
		{name: "rejects invalid secrecy", version: GBVersion30, input: RecordQueryInput{Secrecy: &secrecyInvalid}, wantErr: true},
		{name: "2011 recorder string", version: GBVersion10, input: RecordQueryInput{RecorderID: "recorder-main"}},
		{name: "2014 recorder string", version: GBVersion11, input: RecordQueryInput{RecorderID: "recorder-main"}},
		{name: "2016 recorder string", version: GBVersion20, input: RecordQueryInput{RecorderID: "recorder-main"}},
		{name: "2022 recorder string", version: GBVersion30, input: RecordQueryInput{RecorderID: "recorder-main"}},
		{name: "20 digit recorder remains compatible", version: GBVersion30, input: RecordQueryInput{RecorderID: gb10DeviceID}},
		{name: "2016 rejects stream filter", version: GBVersion20, input: RecordQueryInput{StreamNumber: &stream0}, wantErr: true},
		{name: "2022 stream boundary", version: GBVersion30, input: RecordQueryInput{StreamNumber: &stream0}},
		{name: "2022 additional substream", version: GBVersion30, input: RecordQueryInput{StreamNumber: &stream3}},
		{name: "2022 rejects negative stream", version: GBVersion30, input: RecordQueryInput{StreamNumber: &streamNegative}, wantErr: true},
		{name: "2022 alarm filter", version: GBVersion30, input: RecordQueryInput{AlarmMethod: "5", AlarmType: "13"}},
		{name: "2022 slash alarm methods", version: GBVersion30, input: RecordQueryInput{AlarmMethod: "2/5", AlarmType: "2"}},
		{name: "2022 rejects type without method", version: GBVersion30, input: RecordQueryInput{AlarmType: "1"}, wantErr: true},
		{name: "2022 rejects invalid type for method", version: GBVersion30, input: RecordQueryInput{AlarmMethod: "6", AlarmType: "3"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateRecordQueryFilters(test.version, &test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateRecordQueryFilters() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestRecordInfoParentAliasPreservesChannelTarget(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	collector := newMultiResponseCollector(func(item RecordItem) string { return item.DeviceID + item.FilePath })
	key := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 3)
	collector.Start(key)
	api := &GB28181API{svr: &Server{memoryStorer: memory}, recordResponses: collector}
	api.recordResponseAliases.Store(buildMultiResponseKey(gb10DeviceID, "RecordInfo", 3), key)
	body := `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Name>record</Name><FilePath>/record.ps</FilePath><Secrecy>0</Secrecy></Item></RecordList></Response>`
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "record-parent-alias", []byte(body), api.sipMessageRecordInfo)
	assertFlowOK(t, response)
	result := collector.Wait(context.Background(), key)
	if !result.Complete || len(result.Items) != 1 || result.Items[0].DeviceID != gb10ChannelID {
		t.Fatalf("parent alias result = %+v", result)
	}
}

func TestRecordInfoParentAliasStoresAppendixA4UnderRequestedChannel(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	collector := newMultiResponseCollector(func(item RecordItem) string { return item.DeviceID + item.FilePath })
	key := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 4)
	collector.Start(key)
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}, recordResponses: collector}
	api.recordResponseAliases.Store(buildMultiResponseKey(gb10DeviceID, "RecordInfo", 4), key)
	body := `<Response><CmdType>RecordInfo</CmdType><SN>4</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Name>camera</Name><SumNum>0</SumNum><ExtraInfo>{"type":"doorType","DeviceID":"` +
		gb10ChannelID + `"}</ExtraInfo></Response>`

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
		"record-parent-alias-a4", []byte(body), api.sipMessageRecordInfo)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10ChannelID)
	if !ok || len(state.AppendixA4) != 1 || state.AppendixA4[0].Type != "doorType" {
		t.Fatalf("parent-alias RecordInfo channel state = %+v", state)
	}
	if parentState, ok := api.GetQueryState(gb10DeviceID); ok && len(parentState.AppendixA4) != 0 {
		t.Fatalf("parent-alias RecordInfo overwrote parent runtime state: %+v", parentState.AppendixA4)
	}
}

func TestRecordInfoRejectsSiblingChannelUsingPendingSN(t *testing.T) {
	siblingChannelID := "34020000001320000003"
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	memory.runtime.Channels.Store(siblingChannelID, &Channel{ChannelID: siblingChannelID, device: memory.runtime})
	collector := newMultiResponseCollector(func(item RecordItem) string { return item.DeviceID + item.FilePath })
	key := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 9)
	collector.Start(key)
	api := &GB28181API{svr: &Server{memoryStorer: memory}, recordResponses: collector}
	api.recordResponseAliases.Store(buildMultiResponseKey(gb10DeviceID, "RecordInfo", 9), key)

	body := `<Response><CmdType>RecordInfo</CmdType><SN>9</SN><DeviceID>` + siblingChannelID + `</DeviceID><Name>sibling</Name><SumNum>0</SumNum></Response>`
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "record-sibling-target", []byte(body), api.sipMessageRecordInfo)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("sibling RecordInfo response = %s", response)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	result := collector.Wait(waitCtx, key)
	cancel()
	if result.Complete || len(result.Items) != 0 {
		t.Fatalf("sibling RecordInfo changed collector: %+v", result)
	}
}

func TestRecordInfoAcceptsRequiredItemFieldsForAllVersions(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(version)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			collector := newMultiResponseCollector(func(item RecordItem) string { return item.DeviceID + item.FilePath })
			key := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 6)
			collector.Start(key)
			api := &GB28181API{svr: &Server{memoryStorer: memory}, recordResponses: collector}
			body := `<Response><CmdType>RecordInfo</CmdType><SN>6</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Name> record </Name><Secrecy>0</Secrecy></Item></RecordList></Response>`
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "record-required-"+string(version), []byte(body), api.sipMessageRecordInfo)
			assertFlowOK(t, response)
			result := collector.Wait(context.Background(), key)
			if !result.Complete || len(result.Items) != 1 || result.Items[0].Name != " record " || !result.Items[0].HasName || !result.Items[0].HasSecrecy {
				t.Fatalf("RecordInfo required fields = %+v", result)
			}
		})
	}
}

func TestRecordInfoAcceptsPresentEmptyAndWhitespaceNamesForAllVersions(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(version)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			collector := newMultiResponseCollector(func(item RecordItem) string { return item.DeviceID + item.FilePath })
			key := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 6)
			collector.Start(key)
			api := &GB28181API{svr: &Server{memoryStorer: memory}, recordResponses: collector}
			body := `<Response><CmdType>RecordInfo</CmdType><SN>6</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name/><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Name> </Name><Secrecy>0</Secrecy></Item></RecordList></Response>`
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "record-empty-name-"+string(version), []byte(body), api.sipMessageRecordInfo)
			assertFlowOK(t, response)
			result := collector.Wait(context.Background(), key)
			if !result.Complete || len(result.Items) != 1 || result.Items[0].Name != " " || !result.Items[0].HasName {
				t.Fatalf("RecordInfo empty string Name fields = %+v", result)
			}
		})
	}
}

func TestRecordInfoValidatesOptionalItemFieldsByVersion(t *testing.T) {
	valid := `<Response><CmdType>RecordInfo</CmdType><SN>7</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Name>record</Name><StartTime>2026-08-25T10:00:00</StartTime><EndTime>2026-08-25T10:01:00</EndTime><Secrecy>0</Secrecy><Type>time</Type></Item></RecordList></Response>`
	tests := []struct {
		name    string
		version GBProtocolVersion
		body    string
		wantOK  bool
	}{
		{name: "2011 all type", version: GBVersion10, body: strings.Replace(valid, "<Type>time</Type>", "<Type>all</Type>", 1), wantOK: true},
		{name: "2014 all type", version: GBVersion11, body: strings.Replace(valid, "<Type>time</Type>", "<Type>all</Type>", 1)},
		{name: "2016 all type", version: GBVersion20, body: strings.Replace(valid, "<Type>time</Type>", "<Type>all</Type>", 1)},
		{name: "uppercase type normalized", version: GBVersion20, body: strings.Replace(valid, "<Type>time</Type>", "<Type>ALARM</Type>", 1), wantOK: true},
		{name: "unknown type", version: GBVersion30, body: strings.Replace(valid, "<Type>time</Type>", "<Type>other</Type>", 1)},
		{name: "empty type", version: GBVersion20, body: strings.Replace(valid, "<Type>time</Type>", "<Type></Type>", 1)},
		{name: "empty optional time", version: GBVersion20, body: strings.Replace(valid, "2026-08-25T10:00:00", "", 1)},
		{name: "invalid start time", version: GBVersion20, body: strings.Replace(valid, "2026-08-25T10:00:00", "invalid", 1)},
		{name: "reversed time", version: GBVersion20, body: strings.Replace(valid, "2026-08-25T10:01:00", "2026-08-25T09:59:00", 1)},
		{name: "fractional timezone", version: GBVersion30, body: strings.Replace(strings.Replace(valid, "2026-08-25T10:00:00", "2026-08-25T10:00:00.123+08:00", 1), "2026-08-25T10:01:00", "2026-08-25T10:01:00.456+08:00", 1), wantOK: true},
		{name: "2014 file size", version: GBVersion11, body: strings.Replace(valid, "</Item>", "<FileSize>1024</FileSize></Item>", 1)},
		{name: "2016 file size", version: GBVersion20, body: strings.Replace(valid, "</Item>", "<FileSize>1024</FileSize></Item>", 1), wantOK: true},
		{name: "2016 record location", version: GBVersion20, body: strings.Replace(valid, "</Item>", "<RecordLocation>"+gb10DeviceID+"</RecordLocation></Item>", 1)},
		{name: "valid 2022 storage fields", version: GBVersion30, body: strings.Replace(valid, "</Item>", "<RecordLocation>"+gb10DeviceID+"</RecordLocation><StreamNumber>2</StreamNumber></Item>", 1), wantOK: true},
		{name: "empty 2022 record location", version: GBVersion30, body: strings.Replace(valid, "</Item>", "<RecordLocation></RecordLocation></Item>", 1)},
		{name: "invalid 2022 record location", version: GBVersion30, body: strings.Replace(valid, "</Item>", "<RecordLocation>bad</RecordLocation></Item>", 1)},
		{name: "additional 2022 substream", version: GBVersion30, body: strings.Replace(valid, "</Item>", "<StreamNumber>3</StreamNumber></Item>", 1), wantOK: true},
		{name: "invalid 2022 stream number", version: GBVersion30, body: strings.Replace(valid, "</Item>", "<StreamNumber>-1</StreamNumber></Item>", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(test.version)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			collector := newMultiResponseCollector(func(item RecordItem) string { return item.DeviceID + item.FilePath })
			key := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 7)
			collector.Start(key)
			api := &GB28181API{svr: &Server{memoryStorer: memory}, recordResponses: collector}
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "record-optional-"+test.name, []byte(test.body), api.sipMessageRecordInfo)
			if gotOK := strings.Contains(response, "SIP/2.0 200"); gotOK != test.wantOK {
				t.Fatalf("RecordInfo response = %s, want OK %v", response, test.wantOK)
			}
			if !test.wantOK {
				waitCtx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
				result := collector.Wait(waitCtx, key)
				cancel()
				if result.Complete || len(result.Items) != 0 {
					t.Fatalf("invalid RecordInfo changed collector: %+v", result)
				}
			}
		})
	}
}

func TestRecordInfoEmptyResponseListVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			withoutList := []byte(`<Response><CmdType>RecordInfo</CmdType><SN>4</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>0</SumNum></Response>`)
			if err := validateRecordInfoResponseStructure(withoutList, version); (err != nil) != (version == GBVersion10) {
				t.Fatalf("protocol %s missing RecordList validation error = %v", version, err)
			}

			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(version)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			collector := newMultiResponseCollector(func(item RecordItem) string { return item.DeviceID + item.FilePath })
			key := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 4)
			collector.Start(key)
			api := &GB28181API{svr: &Server{memoryStorer: memory}, recordResponses: collector}
			body := `<Response><CmdType>RecordInfo</CmdType><SN>4</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>0</SumNum><RecordList Num="0"></RecordList></Response>`
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "record-empty-"+string(version), []byte(body), api.sipMessageRecordInfo)
			assertFlowOK(t, response)
			result := collector.Wait(context.Background(), key)
			if !result.Complete || result.Expected != 0 || len(result.Items) != 0 {
				t.Fatalf("empty RecordInfo result = %+v", result)
			}
		})
	}
}

func TestRecordInfoRejectsChunkOverStandardLimit(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	count := gbMultiResponseMaxItems + 1
	message := &MessageRecordInfoResponse{
		XMLName: xml.Name{Local: "Response"}, CmdType: "RecordInfo", SN: 5, DeviceID: gb10ChannelID, Name: "camera",
		HasName: true, SumNum: count, HasSumNum: true, HasList: true, ListNum: &count, Item: make([]RecordItem, count),
	}
	for index := range message.Item {
		message.Item[index].DeviceID = gb10ChannelID
	}
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	if err := api.validateRecordInfoEnvelope(&sip.Context{DeviceID: gb10DeviceID}, message, gb10ChannelID); err == nil {
		t.Fatal("RecordInfo chunk above the 10000-item standard limit was accepted")
	}
}

func TestRecordInfoAcceptsDeclaredTotalAbovePerChunkLimit(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	one := 1
	message := &MessageRecordInfoResponse{
		XMLName: xml.Name{Local: "Response"}, CmdType: "RecordInfo", SN: 5, DeviceID: gb10ChannelID, Name: "camera",
		HasName: true, SumNum: gbMultiResponseMaxItems + 1, HasSumNum: true, HasList: true, ListNum: &one,
		Item: []RecordItem{{DeviceID: gb10ChannelID, Name: "record", HasName: true, Secrecy: 0, HasSecrecy: true}},
	}
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	if err := api.validateRecordInfoEnvelope(&sip.Context{DeviceID: gb10DeviceID}, message, gb10ChannelID); err != nil {
		t.Fatalf("RecordInfo declared total above the per-chunk limit was rejected: %v", err)
	}
}

func TestRecordInfoRequiresRecordLocationFor2022IndistinctQuery(t *testing.T) {
	tests := []struct {
		name                  string
		requireRecordLocation bool
		recordLocation        string
		hasRecordLocation     bool
		wantErr               bool
	}{
		{name: "fuzzy missing location", requireRecordLocation: true, wantErr: true},
		{name: "fuzzy valid location", requireRecordLocation: true, recordLocation: gb10DeviceID, hasRecordLocation: true},
		{name: "exact query missing location"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(GBVersion30)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			collector := newMultiResponseCollector(func(item RecordItem) string { return item.DeviceID + item.FilePath })
			sn := index + 6
			recordKey := buildMultiResponseKey(gb10ChannelID, "RecordInfo", sn)
			collector.Start(recordKey)
			api := &GB28181API{svr: &Server{memoryStorer: memory}, recordResponses: collector}
			api.recordResponseAliases.Store(buildMultiResponseKey(gb10DeviceID, "RecordInfo", sn), &recordResponseAlias{
				recordKey: recordKey, requireRecordLocation: test.requireRecordLocation,
			})
			one := 1
			message := &MessageRecordInfoResponse{
				XMLName: xml.Name{Local: "Response"}, CmdType: "RecordInfo", SN: sn, DeviceID: gb10ChannelID, Name: "camera",
				HasName: true, SumNum: 1, HasSumNum: true, HasList: true, ListNum: &one,
				Item: []RecordItem{{
					DeviceID: gb10ChannelID, Name: "record", HasName: true, Secrecy: 0, HasSecrecy: true, Type: "time",
					RecordLocation: test.recordLocation, HasRecordLocation: test.hasRecordLocation,
				}},
			}
			err := api.validateRecordInfoEnvelope(&sip.Context{DeviceID: gb10DeviceID}, message, gb10ChannelID)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateRecordInfoEnvelope() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestMobilePositionRejectsInvalidNotificationBeforeState(t *testing.T) {
	valid := `<Notify><CmdType>MobilePosition</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-26T12:00:00</Time><Longitude>120</Longitude><Latitude>30</Latitude></Notify>`
	tests := []struct {
		name    string
		version GBProtocolVersion
		body    string
	}{
		{name: "2011 version", version: GBVersion10, body: valid},
		{name: "2014 version", version: GBVersion11, body: valid},
		{name: "wrong root", version: GBVersion20, body: strings.Replace(valid, "Notify", "Response", 2)},
		{name: "wrong command", version: GBVersion20, body: strings.Replace(valid, "MobilePosition", "DeviceStatus", 1)},
		{name: "non-positive SN", version: GBVersion20, body: strings.Replace(valid, "<SN>1</SN>", "<SN>0</SN>", 1)},
		{name: "invalid target", version: GBVersion20, body: strings.ReplaceAll(valid, gb10DeviceID, "bad")},
		{name: "unknown target", version: GBVersion20, body: strings.ReplaceAll(valid, gb10DeviceID, "34020000001320000009")},
		{name: "missing time", version: GBVersion20, body: strings.Replace(valid, "<Time>2026-08-26T12:00:00</Time>", "", 1)},
		{name: "missing longitude", version: GBVersion20, body: strings.Replace(valid, "<Longitude>120</Longitude>", "", 1)},
		{name: "invalid longitude", version: GBVersion20, body: strings.Replace(valid, "120</Longitude>", "181</Longitude>", 1)},
		{name: "invalid latitude", version: GBVersion20, body: strings.Replace(valid, "30</Latitude>", "-91</Latitude>", 1)},
		{name: "invalid direction", version: GBVersion20, body: strings.Replace(valid, "</Notify>", "<Direction>361</Direction></Notify>", 1)},
		{name: "2016 height", version: GBVersion20, body: strings.Replace(valid, "</Notify>", "<Height>8</Height></Notify>", 1)},
		{name: "2016 batch", version: GBVersion20, body: strings.Replace(valid, "</Notify>", `<SumNum>0</SumNum><DeviceList Num="0"></DeviceList></Notify>`, 1)},
		{name: "2016 Appendix A.4 extension", version: GBVersion20, body: strings.Replace(valid, "</Notify>", `<Info><doorType><DeviceID>`+gb10DeviceID+`</DeviceID></doorType></Info></Notify>`, 1)},
		{name: "duplicate SN", version: GBVersion20, body: strings.Replace(valid, "</SN>", "</SN><SN>2</SN>", 1)},
		{name: "unknown field", version: GBVersion20, body: strings.Replace(valid, "</Notify>", "<VendorField>value</VendorField></Notify>", 1)},
		{name: "out of order field", version: GBVersion20, body: strings.Replace(valid, "<Time>2026-08-26T12:00:00</Time><Longitude>120</Longitude>", "<Longitude>120</Longitude><Time>2026-08-26T12:00:00</Time>", 1)},
		{name: "root attribute", version: GBVersion20, body: strings.Replace(valid, "<Notify>", `<Notify vendor="value">`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(test.version)
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "mobile-invalid-"+test.name, []byte(test.body), api.sipNotifyMobilePosition)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid MobilePosition response = %s", response)
			}
			if state, ok := api.GetQueryState(gb10DeviceID); ok && (state.MobilePosition != nil || len(state.MobilePositions) != 0) {
				t.Fatalf("invalid MobilePosition changed state: %+v", state)
			}
		})
	}
}

func TestGB20MobilePositionWithoutDeviceIDStoresStandardState(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion20)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>2</SN><Time>2026-08-26T12:00:00</Time><Longitude>120.1</Longitude><Latitude>30.1</Latitude></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "mobile-standard-2016", body, api.sipNotifyMobilePosition)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.MobilePosition == nil || state.MobilePosition.DeviceID != gb10DeviceID || state.MobilePosition.Longitude == nil || *state.MobilePosition.Longitude != 120.1 {
		t.Fatalf("standard 2016 MobilePosition state = %+v", state)
	}
}

func TestGB20ChannelMobilePositionStoresStateUnderTarget(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion20)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>9</SN><DeviceID>` + gb10ChannelID +
		`</DeviceID><Time>2026-08-26T12:00:00</Time><Longitude>120.3</Longitude><Latitude>30.3</Latitude></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "mobile-channel-2016", body, api.sipNotifyMobilePosition)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10ChannelID)
	if !ok || state.MobilePosition == nil || state.MobilePosition.DeviceID != gb10ChannelID ||
		state.MobilePosition.Longitude == nil || *state.MobilePosition.Longitude != 120.3 {
		t.Fatalf("channel MobilePosition state = %+v", state)
	}
	if parent, ok := api.GetQueryState(gb10DeviceID); ok && parent.MobilePosition != nil {
		t.Fatalf("channel MobilePosition polluted parent state: %+v", parent.MobilePosition)
	}
}

func TestMobilePositionMessageRouteAcceptsQueryNotify(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion20)
	api := &GB28181API{}
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	sipServer := sip.NewServer(local)
	server := &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	api.svr = server
	registerMobilePositionMessageRoute(sipServer.Message(), api)

	serverConn, clientConn := net.Pipe()
	connection := sip.NewTCPConnection(serverConn)
	go sipServer.ProcessTCPConnection(connection)
	t.Cleanup(func() {
		_ = clientConn.Close()
		sipServer.Close()
	})

	body := `<Notify><CmdType>MobilePosition</CmdType><SN>8</SN><Time>2026-08-28T10:00:00</Time><Longitude>120.5</Longitude><Latitude>30.2</Latitude></Notify>`
	request := fmt.Sprintf("MESSAGE sip:%s@3402000000 SIP/2.0\r\nVia: SIP/2.0/TCP 192.0.2.10:5060;branch=z9hG4bK-mobile-route\r\nFrom: <sip:%s@3402000000>;tag=mobile-route\r\nTo: <sip:%s@3402000000>\r\nCall-ID: mobile-position-message-route\r\nCSeq: 1 MESSAGE\r\nMax-Forwards: 70\r\nContent-Type: Application/MANSCDP+xml\r\nContent-Length: %d\r\n\r\n%s", gb10PlatformID, gb10DeviceID, gb10PlatformID, len(body), body)
	if err := clientConn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := clientConn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 4096)
	n, err := clientConn.Read(response)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response[:n]), "SIP/2.0 200 OK") {
		t.Fatalf("MobilePosition MESSAGE response = %s", response[:n])
	}
	deadline := time.Now().Add(time.Second)
	for {
		state, ok := api.GetQueryState(gb10DeviceID)
		if ok && state.MobilePosition != nil && state.MobilePosition.DeviceID == gb10DeviceID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("MobilePosition MESSAGE state = %+v", state)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGB20MobilePositionUsesMatchedSubscriptionTarget(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion20)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	key := "mobile-position-dialog"
	api.outgoingSubscriptions.Store(key, &outgoingSubscriptionDialog{
		notify: outgoingSubscriptionNotifyDialog{targetID: gb10ChannelID},
	})
	ctx := &sip.Context{DeviceID: gb10DeviceID}
	ctx.Set(outgoingSubscriptionNotifyContextKey, key)
	var msg mobilePositionNotify
	body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>3</SN><Time>2026-08-26T12:00:00</Time><Longitude>120.2</Longitude><Latitude>30.2</Latitude></Notify>`)
	if err := sip.XMLDecode(body, &msg); err != nil {
		t.Fatal(err)
	}
	position, _, err := api.validateMobilePositionNotify(ctx, &msg)
	if err != nil {
		t.Fatal(err)
	}
	if position == nil || position.DeviceID != gb10ChannelID {
		t.Fatalf("matched subscription MobilePosition target = %+v", position)
	}
}

func TestGB30MobilePositionBatchStoresCompatibleState(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	body := `<Notify><CmdType>MobilePosition</CmdType><SN>2</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-26T12:00:00</Time><SumNum>2</SumNum><DeviceList Num="2"><Item><DeviceID>` + gb10DeviceID + `</DeviceID><CaptureTime>2026-08-26T11:59:58</CaptureTime><Longitude>120.1</Longitude><Latitude>30.1</Latitude><Speed>10</Speed></Item><Item><DeviceID>` + gb10ChannelID + `</DeviceID><CaptureTime>2026-08-26T11:59:59</CaptureTime><Longitude>120.2</Longitude><Latitude>30.2</Latitude><Direction>359.9</Direction><Height>5</Height></Item></DeviceList></Notify>`
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "mobile-batch", []byte(body), api.sipNotifyMobilePosition)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || len(state.MobilePositions) != 2 || state.MobilePosition == nil || state.MobilePosition.DeviceID != gb10ChannelID || state.MobilePosition.Height == nil || *state.MobilePosition.Height != 5 {
		t.Fatalf("batch MobilePosition state = %+v", state)
	}
	*state.MobilePositions[0].Longitude = 0
	again, _ := api.GetQueryState(gb10DeviceID)
	if again.MobilePositions[0].Longitude == nil || *again.MobilePositions[0].Longitude != 120.1 {
		t.Fatalf("batch MobilePosition state was not cloned: %+v", again.MobilePositions)
	}
	channelState, ok := api.GetQueryState(gb10ChannelID)
	if !ok || channelState.MobilePosition == nil || channelState.MobilePosition.DeviceID != gb10ChannelID ||
		channelState.MobilePosition.Longitude == nil || *channelState.MobilePosition.Longitude != 120.2 || len(channelState.MobilePositions) != 0 {
		t.Fatalf("batch channel MobilePosition state = %+v", channelState)
	}
}

func TestGB30MobilePositionBatchSelectsLatestCaptureTime(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	body := `<Notify><CmdType>MobilePosition</CmdType><SN>6</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Time>2026-08-26T12:00:00</Time><SumNum>2</SumNum><DeviceList Num="2">` +
		`<Item><DeviceID>` + gb10ChannelID + `</DeviceID><CaptureTime>2026-08-26T11:59:59</CaptureTime><Longitude>120.2</Longitude><Latitude>30.2</Latitude></Item>` +
		`<Item><DeviceID>` + gb10DeviceID + `</DeviceID><CaptureTime>2026-08-26T11:59:58</CaptureTime><Longitude>120.1</Longitude><Latitude>30.1</Latitude></Item>` +
		`</DeviceList></Notify>`
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "mobile-batch-latest-capture", []byte(body), api.sipNotifyMobilePosition)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.MobilePosition == nil || state.MobilePosition.DeviceID != gb10ChannelID ||
		state.MobilePosition.CaptureTime != "2026-08-26T11:59:59" {
		t.Fatalf("batch MobilePosition latest state = %+v", state)
	}
}

func TestGB30MobilePositionAcceptsZeroResultWithoutDeviceList(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	body := `<Notify><CmdType>MobilePosition</CmdType><SN>7</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Time>2026-08-26T12:00:00</Time><SumNum>0</SumNum></Notify>`
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "mobile-batch-zero", []byte(body), api.sipNotifyMobilePosition)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.MobilePosition != nil || len(state.MobilePositions) != 0 {
		t.Fatalf("zero-result MobilePosition state = %+v", state)
	}
}

func TestMobilePositionDirectionRangeUsesProtocolVersion(t *testing.T) {
	t.Run("2016 accepts 360 degrees", func(t *testing.T) {
		api, _ := newVersionGateAPI(GBVersion20)
		body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>10</SN><Time>2026-08-30T10:00:00</Time>` +
			`<Longitude>120.2</Longitude><Latitude>30.2</Latitude><Direction>360</Direction></Notify>`)
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify,
			"mobile-direction-2016", body, api.sipNotifyMobilePosition)
		assertFlowOK(t, response)
	})

	t.Run("2022 rejects 360 degrees", func(t *testing.T) {
		memory := newFlowMemory(gb10DeviceID)
		memory.runtime.setGBVersion(GBVersion30)
		api := &GB28181API{svr: &Server{memoryStorer: memory}}
		body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>11</SN><DeviceID>` + gb10DeviceID +
			`</DeviceID><Time>2026-08-30T10:00:00</Time><SumNum>1</SumNum><DeviceList Num="1"><Item>` +
			`<DeviceID>` + gb10DeviceID + `</DeviceID><CaptureTime>2026-08-30T09:59:59</CaptureTime>` +
			`<Longitude>120.2</Longitude><Latitude>30.2</Latitude><Direction>360</Direction></Item></DeviceList></Notify>`)
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify,
			"mobile-direction-2022", body, api.sipNotifyMobilePosition)
		if !strings.Contains(response, "SIP/2.0 400") {
			t.Fatalf("invalid 2022 MobilePosition direction response = %s", response)
		}
		if _, ok := api.GetQueryState(gb10DeviceID); ok {
			t.Fatal("invalid 2022 MobilePosition direction changed state")
		}
	})
}

func TestGB30MobilePositionRejectsInvalidStructureBeforeState(t *testing.T) {
	valid := `<Notify><CmdType>MobilePosition</CmdType><SN>12</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-26T12:00:00</Time><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><CaptureTime>2026-08-26T11:59:59</CaptureTime><Longitude>120.2</Longitude><Latitude>30.2</Latitude></Item></DeviceList></Notify>`
	tests := map[string]string{
		"duplicate SumNum":        strings.Replace(valid, "</SumNum>", "</SumNum><SumNum>1</SumNum>", 1),
		"unknown top-level field": strings.Replace(valid, "<SumNum>", "<VendorField>value</VendorField><SumNum>", 1),
		"out of order top-level field": strings.Replace(valid,
			"<Time>2026-08-26T12:00:00</Time><SumNum>1</SumNum>",
			"<SumNum>1</SumNum><Time>2026-08-26T12:00:00</Time>", 1),
		"duplicate item DeviceID": strings.Replace(valid, "</DeviceID><CaptureTime>", "</DeviceID><DeviceID>"+gb10ChannelID+"</DeviceID><CaptureTime>", 1),
		"unknown item field":      strings.Replace(valid, "</Latitude></Item>", "</Latitude><VendorField>value</VendorField></Item>", 1),
		"item attribute":          strings.Replace(valid, "<Item>", `<Item vendor="value">`, 1),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(GBVersion30)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "mobile-invalid-structure-"+name, []byte(body), api.sipNotifyMobilePosition)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid MobilePosition structure response = %s", response)
			}
			if _, ok := api.GetQueryState(gb10DeviceID); ok {
				t.Fatal("invalid MobilePosition structure changed state")
			}
		})
	}
}

func TestGB30MobilePositionRejectsInvalidBatchCount(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	body := `<Notify><CmdType>MobilePosition</CmdType><SN>2</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-26T12:00:00</Time><SumNum>1</SumNum><DeviceList Num="2"></DeviceList></Notify>`
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "mobile-invalid-count", []byte(body), api.sipNotifyMobilePosition)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("invalid batch MobilePosition response = %s", response)
	}
	if _, ok := api.GetQueryState(gb10DeviceID); ok {
		t.Fatal("invalid batch MobilePosition created state")
	}
}

func TestGB30MobilePositionRejectsBatchTotalMismatch(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	body := `<Notify><CmdType>MobilePosition</CmdType><SN>3</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-26T12:00:00</Time><SumNum>2</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><CaptureTime>2026-08-26T11:59:59</CaptureTime><Longitude>120.2</Longitude><Latitude>30.2</Latitude></Item></DeviceList></Notify>`
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "mobile-batch-total-mismatch", []byte(body), api.sipNotifyMobilePosition)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("invalid batch total response = %s", response)
	}
	if _, ok := api.GetQueryState(gb10DeviceID); ok {
		t.Fatal("invalid batch total changed MobilePosition state")
	}
}

func TestGB30MobilePositionAllowsOptionalDeviceListNum(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	body := `<Notify><CmdType>MobilePosition</CmdType><SN>4</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-26T12:00:00</Time><SumNum>1</SumNum><DeviceList><Item><DeviceID>` + gb10ChannelID + `</DeviceID><CaptureTime>2026-08-26T11:59:59</CaptureTime><Longitude>120.2</Longitude><Latitude>30.2</Latitude></Item></DeviceList></Notify>`
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "mobile-optional-list-num", []byte(body), api.sipNotifyMobilePosition)
	assertFlowOK(t, response)
}

func TestGB30MobilePositionRejectsSingleAndExtensions(t *testing.T) {
	baseBatch := `<Notify><CmdType>MobilePosition</CmdType><SN>5</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-26T12:00:00</Time><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10DeviceID + `</DeviceID><CaptureTime>2026-08-26T11:59:59</CaptureTime><Longitude>120.2</Longitude><Latitude>30.2</Latitude></Item></DeviceList>%s</Notify>`
	tests := map[string]string{
		"single structure": `<Notify><CmdType>MobilePosition</CmdType><SN>5</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-26T12:00:00</Time><Longitude>120.2</Longitude><Latitude>30.2</Latitude></Notify>`,
		"top-level Height": fmt.Sprintf(strings.Replace(baseBatch, "<SumNum>", "<Height>8</Height><SumNum>", 1), ""),
		"Info":             fmt.Sprintf(baseBatch, "<Info>vendor</Info>"),
		"ExtraInfo":        fmt.Sprintf(baseBatch, "<ExtraInfo>vendor</ExtraInfo>"),
		"Appendix A.4":     fmt.Sprintf(baseBatch, `<Info><doorType><DeviceID>`+gb10DeviceID+`</DeviceID></doorType></Info>`),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(GBVersion30)
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "mobile-invalid-2022-"+name, []byte(body), api.sipNotifyMobilePosition)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid 2022 MobilePosition response = %s", response)
			}
			if _, ok := api.GetQueryState(gb10DeviceID); ok {
				t.Fatal("invalid 2022 MobilePosition changed state")
			}
		})
	}
}
