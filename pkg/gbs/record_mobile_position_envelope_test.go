package gbs

import (
	"context"
	"encoding/xml"
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
		{name: "missing list count", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList><Item><DeviceID>` + gb10ChannelID + `</DeviceID></Item></RecordList></Response>`},
		{name: "list count mismatch", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="0"><Item><DeviceID>` + gb10ChannelID + `</DeviceID></Item></RecordList></Response>`},
		{name: "unknown target", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>34020000001320000009</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>34020000001320000009</DeviceID></Item></RecordList></Response>`},
		{name: "item target mismatch", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10DeviceID + `</DeviceID></Item></RecordList></Response>`},
		{name: "missing item name", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Secrecy>0</Secrecy></Item></RecordList></Response>`},
		{name: "empty item name", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Name> </Name><Secrecy>0</Secrecy></Item></RecordList></Response>`},
		{name: "missing item secrecy", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Name>record</Name></Item></RecordList></Response>`},
		{name: "invalid item secrecy", body: `<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Name>record</Name><Secrecy>2</Secrecy></Item></RecordList></Response>`},
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
			if !result.Complete || len(result.Items) != 1 || result.Items[0].Name != "record" || !result.Items[0].HasName || !result.Items[0].HasSecrecy {
				t.Fatalf("RecordInfo required fields = %+v", result)
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
		{name: "invalid 2022 stream number", version: GBVersion30, body: strings.Replace(valid, "</Item>", "<StreamNumber>3</StreamNumber></Item>", 1)},
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
		})
	}
}

func TestRecordInfoAcceptsEmptyResultWithoutRecordListForAllVersions(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(version)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			collector := newMultiResponseCollector(func(item RecordItem) string { return item.DeviceID + item.FilePath })
			key := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 4)
			collector.Start(key)
			api := &GB28181API{svr: &Server{memoryStorer: memory}, recordResponses: collector}
			body := `<Response><CmdType>RecordInfo</CmdType><SN>4</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>0</SumNum></Response>`
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
		SumNum: count, HasSumNum: true, HasList: true, ListNum: &count, Item: make([]RecordItem, count),
	}
	for index := range message.Item {
		message.Item[index].DeviceID = gb10ChannelID
	}
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	if err := api.validateRecordInfoEnvelope(&sip.Context{DeviceID: gb10DeviceID}, message, gb10ChannelID); err == nil {
		t.Fatal("RecordInfo chunk above the 10000-item standard limit was accepted")
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
		{name: "invalid direction", version: GBVersion20, body: strings.Replace(valid, "</Notify>", "<Direction>360</Direction></Notify>", 1)},
		{name: "2016 batch", version: GBVersion20, body: strings.Replace(valid, "</Notify>", `<SumNum>0</SumNum><DeviceList Num="0"></DeviceList></Notify>`, 1)},
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
