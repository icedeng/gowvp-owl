package gbs

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/orm"
)

func TestCleanupQueryStatesExpiresAndBoundsSnapshots(t *testing.T) {
	now := time.Now()
	api := &GB28181API{}
	api.queryStates.Store("expired", &QueryState{UpdatedAt: now.Add(-queryStateTTL - time.Second)})
	api.queryStates.Store("invalid", "unexpected")
	for i := 0; i < maxQueryStateEntries+2; i++ {
		deviceID := fmt.Sprintf("device-%04d", i)
		api.queryStates.Store(deviceID, &QueryState{UpdatedAt: now.Add(time.Duration(i) * time.Nanosecond)})
	}

	api.cleanupQueryStates(now.Add(time.Second))

	if _, ok := api.queryStates.Load("expired"); ok {
		t.Fatal("expired query state was retained")
	}
	if _, ok := api.queryStates.Load("invalid"); ok {
		t.Fatal("invalid query state was retained")
	}
	count := 0
	api.queryStates.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != maxQueryStateEntries {
		t.Fatalf("query states = %d; want %d", count, maxQueryStateEntries)
	}
	if _, ok := api.queryStates.Load("device-0000"); ok {
		t.Fatal("oldest query state was retained")
	}
}

func TestDeviceStatusExtensionInfoVersionMatrix(t *testing.T) {
	legacyBody := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Reason> offline </Reason><Info> legacy </Info><Info>second</Info></Response>`)
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20} {
		if err := validateGenericQueryPayload(version, "DeviceStatus", legacyBody); err != nil {
			t.Fatalf("protocol %s rejected DeviceStatus Info: %v", version, err)
		}
		data := decodeDeviceStatusData(legacyBody)
		if data == nil || data.Reason != " offline " || len(data.Info) != 2 || data.Info[0] != " legacy " || data.Info[1] != "second" || len(data.ExtraInfo) != 0 {
			t.Fatalf("protocol %s DeviceStatus extension = %+v", version, data)
		}
	}
	if err := validateGenericQueryPayload(GBVersion30, "DeviceStatus", legacyBody); err == nil {
		t.Fatal("protocol 3.0 accepted legacy DeviceStatus Info")
	}
	structuredBody := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Info><doorType><DeviceID>` +
		gb10DeviceID + `</DeviceID></doorType></Info></Response>`)
	if err := validateGenericQueryPayload(GBVersion30, "DeviceStatus", structuredBody); err != nil {
		t.Fatalf("protocol 3.0 rejected structured Appendix A.4 Info: %v", err)
	}
	structuredData := decodeDeviceStatusData(structuredBody)
	if structuredData == nil || len(structuredData.Info) != 0 {
		t.Fatalf("structured Appendix A.4 Info leaked into legacy DeviceStatus Info: %+v", structuredData)
	}
	api, _ := newVersionGateAPI(GBVersion30)
	objects, err := api.validateAndDecodeAppendixA4(gb10DeviceID, "DeviceStatus", structuredBody)
	if err != nil || len(objects) != 1 || objects[0].Type != "doorType" {
		t.Fatalf("structured DeviceStatus Appendix A.4 = %+v, %v", objects, err)
	}
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20} {
		if err := validateGenericQueryPayload(version, "DeviceStatus", structuredBody); err == nil {
			t.Fatalf("protocol %s accepted structured DeviceStatus Info", version)
		}
	}
	unknownStructuredBody := []byte(strings.Replace(string(structuredBody),
		`<doorType><DeviceID>`+gb10DeviceID+`</DeviceID></doorType>`, `<VendorExtension/>`, 1))
	if err := validateGenericQueryPayload(GBVersion30, "DeviceStatus", unknownStructuredBody); err == nil {
		t.Fatal("protocol 3.0 accepted unknown structured DeviceStatus Info")
	}

	modernBody := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status>` +
		`<ExtraInfo>{"type":"doorType","DeviceID":"` + gb10DeviceID + `"}</ExtraInfo><ExtraInfo> plain </ExtraInfo></Response>`)
	if err := validateGenericQueryPayload(GBVersion30, "DeviceStatus", modernBody); err != nil {
		t.Fatalf("protocol 3.0 rejected DeviceStatus ExtraInfo: %v", err)
	}
	data := decodeDeviceStatusData(modernBody)
	if data == nil || len(data.ExtraInfo) != 2 || !strings.Contains(data.ExtraInfo[0], `"type":"doorType"`) || data.ExtraInfo[1] != " plain " || len(data.Info) != 0 {
		t.Fatalf("protocol 3.0 DeviceStatus extension = %+v", data)
	}
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20} {
		if err := validateGenericQueryPayload(version, "DeviceStatus", modernBody); err == nil {
			t.Fatalf("protocol %s accepted DeviceStatus ExtraInfo", version)
		}
	}

	tooLong := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Info>` + strings.Repeat("测", 1025) + `</Info></Response>`)
	if err := validateGenericQueryPayload(GBVersion20, "DeviceStatus", tooLong); err == nil {
		t.Fatal("DeviceStatus Info longer than 1024 characters was accepted")
	}

	state := &QueryState{DeviceStatus: data}
	clone := cloneQueryState(state)
	clone.DeviceStatus.ExtraInfo[0] = "changed"
	if state.DeviceStatus.ExtraInfo[0] == "changed" {
		t.Fatal("DeviceStatus ExtraInfo clone shares backing storage")
	}
}

func TestDeviceStatusResponseStructureVersionMatrix(t *testing.T) {
	valid := []struct {
		name    string
		version GBProtocolVersion
		alarm   string
		ext     string
	}{
		{name: "2011 schema status", version: GBVersion10, alarm: `<Alarmstatus Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Status>ONDUTY</Status></Item></Alarmstatus>`, ext: `<Info>legacy</Info>`},
		{name: "2011 example duty status", version: GBVersion10, alarm: `<Alarmstatus Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><DutyStatus>OFFDUTY</DutyStatus></Item></Alarmstatus>`},
		{name: "2014 supplement StatusDutyStatus", version: GBVersion11, alarm: `<Alarmstatus Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><StatusDutyStatus>ALARM</StatusDutyStatus></Item></Alarmstatus>`},
		{name: "2014 legacy DutyStatus", version: GBVersion11, alarm: `<Alarmstatus Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><DutyStatus>ALARM</DutyStatus></Item></Alarmstatus>`},
		{name: "2016 lower num", version: GBVersion20, alarm: `<Alarmstatus num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><DutyStatus>OFFDUTY</DutyStatus></Item></Alarmstatus>`, ext: `<Info>legacy</Info>`},
		{name: "2022 upper Num", version: GBVersion30, alarm: `<Alarmstatus Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><DutyStatus>ALARM</DutyStatus></Item></Alarmstatus>`, ext: `<ExtraInfo>modern</ExtraInfo>`},
	}
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Reason>reason</Reason>` +
				`<Encode>ON</Encode><Record>OFF</Record><DeviceTime>2026-08-29T12:00:00</DeviceTime>` + test.alarm + test.ext + `</Response>`)
			if err := validateGenericQueryPayload(test.version, "DeviceStatus", body); err != nil {
				t.Fatalf("valid DeviceStatus rejected: %v", err)
			}
		})
	}

	invalid := []struct {
		name    string
		version GBProtocolVersion
		fields  string
	}{
		{name: "duplicate result", version: GBVersion10, fields: `<Result>OK</Result><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status>`},
		{name: "unknown field", version: GBVersion10, fields: `<Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><VendorField>value</VendorField>`},
		{name: "out of order", version: GBVersion10, fields: `<Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><DeviceTime>2026-08-29T12:00:00</DeviceTime><Encode>ON</Encode>`},
		{name: "nested simple field", version: GBVersion10, fields: `<Result>OK</Result><Online><Value>ONLINE</Value></Online><Status>OK</Status>`},
		{name: "missing alarm count", version: GBVersion10, fields: `<Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Alarmstatus><Item/></Alarmstatus>`},
		{name: "2011 lower num", version: GBVersion10, fields: `<Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Alarmstatus num="0"/>`},
		{name: "2016 upper Num", version: GBVersion20, fields: `<Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Alarmstatus Num="0"/>`},
		{name: "unknown alarm item field", version: GBVersion30, fields: `<Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Alarmstatus Num="1"><Item><VendorField>value</VendorField></Item></Alarmstatus>`},
		{name: "2014 rejects deleted Status", version: GBVersion11, fields: `<Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Alarmstatus Num="1"><Item><Status>ONDUTY</Status></Item></Alarmstatus>`},
		{name: "2014 rejects duplicate status spellings", version: GBVersion11, fields: `<Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Alarmstatus Num="1"><Item><StatusDutyStatus>ONDUTY</StatusDutyStatus><DutyStatus>ONDUTY</DutyStatus></Item></Alarmstatus>`},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID>` + test.fields + `</Response>`)
			if err := validateGenericQueryPayload(test.version, "DeviceStatus", body); err == nil {
				t.Fatal("invalid DeviceStatus structure was accepted")
			}
		})
	}

	rootAttribute := []byte(`<Response vendor="x"><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status></Response>`)
	if err := validateGenericQueryPayload(GBVersion10, "DeviceStatus", rootAttribute); err == nil {
		t.Fatal("DeviceStatus root attribute was accepted")
	}
}

func TestChannelDeviceStatusResponseStoresStateUnderTargetFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(version)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{
				ChannelID: gb10ChannelID,
				device:    memory.runtime,
			})
			api := &GB28181API{}
			api.svr = &Server{gb: api, memoryStorer: memory}
			const sn = 1731
			pending := &pendingQueryWait{
				wait:     make(chan *DeviceQueryOutput, 1),
				targetID: gb10ChannelID,
			}
			api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "DeviceStatus", sn), pending)
			body := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>1731</SN><DeviceID>` + gb10ChannelID +
				`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status></Response>`)

			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
				"channel-device-status-"+string(version), body, api.sipMessageQueryGeneric)
			assertFlowOK(t, response)

			state, ok := api.GetQueryState(gb10ChannelID)
			if !ok || state.DeviceStatus == nil || state.DeviceStatus.DeviceID != gb10ChannelID {
				t.Fatalf("channel query state = %+v, %v", state, ok)
			}
			if _, ok := api.GetQueryState(gb10DeviceID); ok {
				t.Fatal("channel DeviceStatus response overwrote the parent device query state")
			}
			select {
			case output := <-pending.wait:
				status, valid := output.Data.(*DeviceStatusData)
				if !valid || status.DeviceID != gb10ChannelID {
					t.Fatalf("pending channel query output = %+v", output)
				}
			default:
				t.Fatal("channel DeviceStatus response did not resolve the pending query")
			}
		})
	}
}

func TestPresetQueryResponseStructureVersionMatrix(t *testing.T) {
	for _, test := range []struct {
		name    string
		version GBProtocolVersion
		cmdType string
		count   string
	}{
		{name: "2014 supplement", version: GBVersion11, cmdType: "PersetQuery"},
		{name: "2016", version: GBVersion20, cmdType: "PresetQuery"},
		{name: "2022", version: GBVersion30, cmdType: "PresetQuery", count: "<SumNum>1</SumNum>"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`<Response><CmdType>` + test.cmdType + `</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID>` +
				test.count + `<PresetList Num="1"><Item><PresetID>1</PresetID><PresetName>one</PresetName></Item></PresetList></Response>`)
			if err := validateGenericQueryPayload(test.version, "PresetQuery", body); err != nil {
				t.Fatalf("valid PresetQuery rejected: %v", err)
			}
		})
	}

	invalid := []struct {
		name    string
		version GBProtocolVersion
		payload string
	}{
		{name: "2014 invalid compatibility result", version: GBVersion11, payload: `<Result>SUCCESS</Result><SumNum>0</SumNum><PresetList Num="0"/>`},
		{name: "2016 compatibility fields out of order", version: GBVersion20, payload: `<SumNum>0</SumNum><Result>OK</Result><PresetList Num="0"/>`},
		{name: "2022 requires SumNum", version: GBVersion30, payload: `<PresetList Num="0"/>`},
		{name: "list requires Num", version: GBVersion30, payload: `<SumNum>0</SumNum><PresetList/>`},
		{name: "item field order", version: GBVersion30, payload: `<SumNum>1</SumNum><PresetList Num="1"><Item><PresetName>one</PresetName><PresetID>1</PresetID></Item></PresetList>`},
		{name: "unknown item field", version: GBVersion30, payload: `<SumNum>1</SumNum><PresetList Num="1"><Item><PresetID>1</PresetID><PresetName>one</PresetName><VendorField>value</VendorField></Item></PresetList>`},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`<Response><CmdType>PresetQuery</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID>` + test.payload + `</Response>`)
			if err := validateGenericQueryPayload(test.version, "PresetQuery", body); err == nil {
				t.Fatal("invalid PresetQuery structure was accepted")
			}
		})
	}
}

func TestPresetQueryRequiredPlainStringsAllowEmptyValues(t *testing.T) {
	for _, test := range []struct {
		version GBProtocolVersion
		cmdType string
		count   string
	}{
		{version: GBVersion11, cmdType: "PersetQuery"},
		{version: GBVersion20, cmdType: "PresetQuery"},
		{version: GBVersion30, cmdType: "PresetQuery", count: "<SumNum>1</SumNum>"},
	} {
		t.Run(string(test.version), func(t *testing.T) {
			body := []byte(`<Response><CmdType>` + test.cmdType + `</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID>` +
				test.count + `<PresetList Num="1"><Item><PresetID> </PresetID><PresetName/></Item></PresetList></Response>`)
			if err := validateGenericQueryPayload(test.version, "PresetQuery", body); err != nil {
				t.Fatalf("required empty plain strings rejected: %v", err)
			}
			presets := decodePresetQueryData(body)
			if len(presets) != 1 || presets[0].PresetID != " " || presets[0].PresetName != "" {
				t.Fatalf("PresetQuery empty strings = %+v", presets)
			}
		})
	}

	missingPresetID := []byte(`<Response><CmdType>PresetQuery</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><PresetList Num="1"><Item><PresetName/></Item></PresetList></Response>`)
	if err := validateGenericQueryPayload(GBVersion30, "PresetQuery", missingPresetID); err == nil {
		t.Fatal("PresetQuery accepted an item without required PresetID element")
	}
}

func TestHomePositionQueryResetTimeUsesStandardIntegerRange(t *testing.T) {
	body := []byte(`<Response><CmdType>HomePositionQuery</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><HomePosition><Enabled>1</Enabled><ResetTime>-1</ResetTime><PresetIndex>0</PresetIndex></HomePosition></Response>`)
	if err := validateGenericQueryPayload(GBVersion30, "HomePositionQuery", body); err != nil {
		t.Fatalf("standard integer ResetTime rejected: %v", err)
	}
}

func TestQueryStatePreservesPlainStringWhitespace(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20, GBVersion30} {
		count := ""
		if version == GBVersion30 {
			count = "<SumNum>1</SumNum>"
		}
		presetBody := []byte(`<Response><CmdType>PresetQuery</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID>` + count + `<PresetList Num="1"><Item><PresetID> 1 </PresetID><PresetName> entrance </PresetName></Item></PresetList></Response>`)
		if err := validateGenericQueryPayload(version, "PresetQuery", presetBody); err != nil {
			t.Fatalf("protocol %s rejected PresetQuery strings: %v", version, err)
		}
		presets := decodePresetQueryData(presetBody)
		if len(presets) != 1 || presets[0].PresetID != " 1 " || presets[0].PresetName != " entrance " {
			t.Fatalf("protocol %s PresetQuery strings = %+v", version, presets)
		}
	}

	cruiseList := decodeCruiseTrackListData([]byte(`<Response><SumNum>1</SumNum><CruiseTrackList Num="1"><CruiseTrack><Number>0</Number><Name> day </Name></CruiseTrack></CruiseTrackList></Response>`))
	if len(cruiseList) != 1 || cruiseList[0].Name != " day " {
		t.Fatalf("CruiseTrackListQuery Name = %+v", cruiseList)
	}

	cruise := decodeCruiseTrackData([]byte(`<Response><Number>0</Number><Name> night </Name><SumNum>0</SumNum></Response>`))
	if cruise == nil || cruise.Name != " night " {
		t.Fatalf("CruiseTrackQuery Name = %+v", cruise)
	}

	sdCards := decodeSDCardStatusData([]byte(`<Response><SumNum>1</SumNum><SDCardStatusInfo Num="1"><Item><ID>0</ID><HddName> card </HddName><Status> ok </Status><Capacity>10</Capacity><FreeSpace>5</FreeSpace></Item></SDCardStatusInfo></Response>`))
	if len(sdCards) != 1 || sdCards[0].HddName != " card " || sdCards[0].Status != "ok" {
		t.Fatalf("SDCardStatus strings = %+v", sdCards)
	}
}

func TestGetQueryStateReturnsDeepSnapshot(t *testing.T) {
	api := &GB28181API{}
	videoConfigCount := 1
	api.queryStates.Store(gb10DeviceID, &QueryState{
		DeviceStatus: &DeviceStatusData{Online: "ONLINE", FaultDeviceIDs: []string{gb10ChannelID}},
		CruiseTracks: []CruiseTrackData{{Number: 1, Points: []CruisePointData{{PresetIndex: 7}}}},
		AppendixA4:   []AppendixA4Object{{Type: "doorType", Fields: map[string]string{"DeviceID": gb10DeviceID}}},
		ConfigDownload: &ConfigDownloadState{VideoParamConfig: &VideoParamConfig{
			Num: &videoConfigCount, Attributes: []xml.Attr{{Name: xml.Name{Local: "test"}, Value: "original"}},
		}},
	})

	snapshot, ok := api.GetQueryState(gb10DeviceID)
	if !ok {
		t.Fatal("query state snapshot not found")
	}
	snapshot.DeviceStatus.Online = "OFFLINE"
	snapshot.DeviceStatus.FaultDeviceIDs[0] = "mutated"
	snapshot.CruiseTracks[0].Points[0].PresetIndex = 99
	snapshot.AppendixA4[0].Fields["DeviceID"] = "mutated"
	*snapshot.ConfigDownload.VideoParamConfig.Num = 99
	snapshot.ConfigDownload.VideoParamConfig.Attributes[0].Value = "mutated"

	current, ok := api.GetQueryState(gb10DeviceID)
	if !ok {
		t.Fatal("query state disappeared")
	}
	if current.DeviceStatus.Online != "ONLINE" || current.DeviceStatus.FaultDeviceIDs[0] != gb10ChannelID ||
		current.CruiseTracks[0].Points[0].PresetIndex != 7 || current.AppendixA4[0].Fields["DeviceID"] != gb10DeviceID ||
		*current.ConfigDownload.VideoParamConfig.Num != 1 || current.ConfigDownload.VideoParamConfig.Attributes[0].Value != "original" {
		t.Fatalf("GetQueryState leaked internal state: %+v", current)
	}
}

func TestApplyDeviceStatusUsesOnlineFieldOnly(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.persistent.IsOnline = true
	memory.runtime.IsOnline = true
	api := &GB28181API{svr: &Server{memoryStorer: memory}}

	api.persistDecodedQuery(gb10DeviceID, "DeviceStatus", decodedDeviceQuery{data: &DeviceStatusData{
		DeviceID: gb10DeviceID, Result: "OK", Online: "OFFLINE", Status: "OK",
	}})
	if memory.persistent.IsOnline || memory.runtime.IsOnlineNow() {
		t.Fatal("DeviceStatus Online=OFFLINE was overwritten by Status=OK")
	}
	if memory.persistent.Ext.GBRegistrationClosed == nil || *memory.persistent.Ext.GBRegistrationClosed {
		t.Fatalf("DeviceStatus OFFLINE closed persisted REGISTER binding: %+v", memory.persistent.Ext)
	}

	api.persistDecodedQuery(gb10DeviceID, "DeviceStatus", decodedDeviceQuery{data: &DeviceStatusData{
		DeviceID: gb10DeviceID, Result: "OK", Online: "ONLINE", Status: "ERROR",
	}})
	if !memory.persistent.IsOnline || !memory.runtime.IsOnlineNow() {
		t.Fatal("DeviceStatus Online=ONLINE was overwritten by Status=ERROR")
	}

	if err := api.logout(gb10DeviceID, func(device *ipc.Device) error {
		device.IsOnline = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if memory.persistent.Ext.GBRegistrationClosed == nil || !*memory.persistent.Ext.GBRegistrationClosed {
		t.Fatalf("logout did not persist closed REGISTER binding: %+v", memory.persistent.Ext)
	}
	api.persistDecodedQuery(gb10DeviceID, "DeviceStatus", decodedDeviceQuery{data: &DeviceStatusData{
		DeviceID: gb10DeviceID, Result: "OK", Online: "ONLINE", Status: "OK",
	}})
	if memory.persistent.IsOnline || memory.runtime.IsOnlineNow() || !memory.runtime.runtimeSnapshot().RegistrationClosed {
		t.Fatal("late DeviceStatus restored a closed REGISTER binding")
	}
}

func TestDeviceStatusPersistenceFailureRetriesWithoutClosingRegistration(t *testing.T) {
	persistErr := errors.New("device status persistence unavailable")
	registeredAt := time.Now().Add(-time.Minute)
	memory := &deviceStatusFailureMemory{
		flowMemory: newFlowMemory(gb10DeviceID),
		changeErr:  persistErr,
	}
	memory.persistent.IsOnline = true
	memory.persistent.RegisteredAt = orm.Time{Time: registeredAt}
	memory.persistent.Expires = 3600
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.LastRegisterAt = registeredAt
		device.Expires = 3600
	})
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api

	err := api.applyDeviceStatus(gb10DeviceID, &DeviceStatusData{
		DeviceID: gb10DeviceID,
		Result:   "OK",
		Online:   "OFFLINE",
		Status:   "OK",
	})
	if !errors.Is(err, persistErr) {
		t.Fatalf("applyDeviceStatus error = %v, want %v", err, persistErr)
	}
	state := memory.runtime.runtimeSnapshot()
	if !memory.persistent.IsOnline || !state.IsOnline {
		t.Fatalf("failed persistence committed state: persistent=%t runtime=%+v", memory.persistent.IsOnline, state)
	}
	if !state.DeviceStatusPersistencePending || state.PendingDeviceStatusOnline || state.PendingDeviceStatusAt.IsZero() {
		t.Fatalf("pending DeviceStatus state = %+v", state)
	}
	if state.RegistrationClosed || state.OfflinePersistencePending {
		t.Fatalf("DeviceStatus failure closed REGISTER binding: %+v", state)
	}

	memory.changeErr = nil
	server.checkOfflineDevices(time.Now())
	state = memory.runtime.runtimeSnapshot()
	if memory.persistent.IsOnline || state.IsOnline || state.DeviceStatusPersistencePending {
		t.Fatalf("retried DeviceStatus state = persistent:%t runtime:%+v", memory.persistent.IsOnline, state)
	}
	if state.RegistrationClosed || state.OfflinePersistencePending {
		t.Fatalf("retried DeviceStatus closed REGISTER binding: %+v", state)
	}
}

func TestPendingDeviceStatusCannotOverwriteNewRegistration(t *testing.T) {
	persistErr := errors.New("device status persistence unavailable")
	registeredAt := time.Now().Add(-time.Minute)
	memory := &deviceStatusFailureMemory{
		flowMemory: newFlowMemory(gb10DeviceID),
		changeErr:  persistErr,
	}
	memory.persistent.IsOnline = true
	memory.persistent.RegisteredAt = orm.Time{Time: registeredAt}
	memory.persistent.Expires = 3600
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.LastRegisterAt = registeredAt
		device.Expires = 3600
	})
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api

	if err := api.applyDeviceStatus(gb10DeviceID, &DeviceStatusData{Online: "OFFLINE"}); !errors.Is(err, persistErr) {
		t.Fatalf("applyDeviceStatus error = %v, want %v", err, persistErr)
	}
	pending := memory.runtime.runtimeSnapshot()
	if !pending.DeviceStatusPersistencePending {
		t.Fatalf("pending DeviceStatus state = %+v", pending)
	}

	newRegisteredAt := time.Now()
	memory.changeErr = nil
	memory.persistent.IsOnline = true
	memory.persistent.RegisteredAt = orm.Time{Time: newRegisteredAt}
	memory.persistent.Expires = 7200
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.LastRegisterAt = newRegisteredAt
		device.Expires = 7200
		clearPendingDeviceStatusLocked(device)
	})
	changed, err := api.retryPendingDeviceStatus(gb10DeviceID, pending)
	if err != nil || changed {
		t.Fatalf("stale DeviceStatus retry = changed %t, err %v", changed, err)
	}
	state := memory.runtime.runtimeSnapshot()
	if !memory.persistent.IsOnline || !state.IsOnline || state.DeviceStatusPersistencePending ||
		!state.LastRegisterAt.Equal(newRegisteredAt) || state.Expires != 7200 {
		t.Fatalf("new registration overwritten: persistent=%+v runtime=%+v", memory.persistent, state)
	}
}

func TestAdmittedDeviceStatusCannotOverwriteNewRegistration(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			testAdmittedDeviceStatusCannotOverwriteNewRegistration(t, version)
		})
	}
}

func testAdmittedDeviceStatusCannotOverwriteNewRegistration(t *testing.T, version GBProtocolVersion) {
	oldRegisteredAt := time.Now().Add(-time.Minute)
	newRegisteredAt := time.Now()
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(version)
	memory.persistent.IsOnline = true
	memory.persistent.RegisteredAt = orm.Time{Time: oldRegisteredAt}
	memory.persistent.Expires = 3600
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.LastRegisterAt = oldRegisteredAt
		device.Expires = 3600
	})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}

	operation := newPendingDeviceOperation(t.Context(), gb10DeviceID, gb10DeviceID)
	defer operation.Cancel(nil)
	pending := &pendingQueryWait{
		wait:      make(chan *DeviceQueryOutput, 1),
		targetID:  gb10DeviceID,
		operation: operation,
	}
	const sn = 93
	waitKey := buildPendingQueryKey(gb10DeviceID, "DeviceStatus", sn)
	api.pendingDeviceQuery.Store(waitKey, pending)
	defer api.pendingDeviceQuery.Delete(waitKey)

	conn := newFlowConnection()
	body := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>93</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Result>OK</Result><Online>OFFLINE</Online><Status>OK</Status></Response>`)
	request := newFlowRequest(t, conn, sip.MethodMessage, "device-status-old-registration", body)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("device-status-old-registration-tx", conn),
		DeviceID: gb10DeviceID, Source: conn.remote, Log: slog.Default(),
	}

	// 旧 REGISTER 绑定已经通过入向门禁，但业务处理尚未开始。
	api.sipAccessControlMiddleware(ctx)
	if ctx.IsAborted() {
		t.Fatal("active old registration was rejected")
	}

	// 新 REGISTER 在旧 handler 继续执行前提交。
	memory.persistent.IsOnline = true
	memory.persistent.RegisteredAt = orm.Time{Time: newRegisteredAt}
	memory.persistent.Expires = 7200
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.LastRegisterAt = newRegisteredAt
		device.Expires = 7200
	})

	api.sipMessageQueryGeneric(ctx)
	select {
	case payload := <-conn.writes:
		assertFlowOK(t, string(payload))
	default:
		t.Fatal("late DeviceStatus was not acknowledged")
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok {
		t.Fatalf("late DeviceStatus updated QueryState: %+v", state)
	}
	select {
	case output := <-pending.wait:
		t.Fatalf("late DeviceStatus resolved a new-registration query: %+v", output)
	default:
	}
	state := memory.runtime.runtimeSnapshot()
	if !memory.persistent.IsOnline || !state.IsOnline ||
		!memory.persistent.RegisteredAt.Time.Equal(newRegisteredAt) ||
		!state.LastRegisterAt.Equal(newRegisteredAt) ||
		memory.persistent.Expires != 7200 || state.Expires != 7200 {
		t.Fatalf("late DeviceStatus overwrote new registration: persistent=%+v runtime=%+v", memory.persistent, state)
	}
}

func TestDeviceStatusOfflineRegistrationStillExpires(t *testing.T) {
	registeredAt := time.Now()
	memory := newFlowMemory(gb10DeviceID)
	memory.persistent.IsOnline = true
	memory.persistent.RegisteredAt = orm.Time{Time: registeredAt}
	memory.persistent.Expires = 10
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.LastRegisterAt = registeredAt
		device.Expires = 10
	})
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api

	if err := api.applyDeviceStatus(gb10DeviceID, &DeviceStatusData{Online: "OFFLINE"}); err != nil {
		t.Fatal(err)
	}
	state := memory.runtime.runtimeSnapshot()
	if state.IsOnline || state.RegistrationClosed {
		t.Fatalf("DeviceStatus offline state = %+v", state)
	}

	server.checkOfflineDevices(registeredAt.Add(10 * time.Second))
	state = memory.runtime.runtimeSnapshot()
	if state.IsOnline || !state.RegistrationClosed || memory.persistent.Ext.GBRegistrationClosed == nil ||
		!*memory.persistent.Ext.GBRegistrationClosed {
		t.Fatalf("expired DeviceStatus offline binding = persistent:%+v runtime:%+v", memory.persistent, state)
	}
}

func TestQueryStateConcurrentSnapshotsAreIsolated(t *testing.T) {
	api := &GB28181API{}
	api.storeQueryState(gb10DeviceID, "DeviceStatus", &DeviceStatusData{Online: "ONLINE", FaultDeviceIDs: []string{gb10ChannelID}})
	api.storeAppendixA4State(gb10DeviceID, []AppendixA4Object{{Type: "doorType", Fields: map[string]string{"DeviceID": gb10DeviceID}}})

	var group sync.WaitGroup
	group.Add(3)
	go func() {
		defer group.Done()
		for index := 0; index < 500; index++ {
			api.storeQueryState(gb10DeviceID, "DeviceStatus", &DeviceStatusData{
				Online: "ONLINE", FaultDeviceIDs: []string{fmt.Sprintf("fault-%d", index)},
			})
			api.storeAppendixA4State(gb10DeviceID, []AppendixA4Object{{
				Type: "doorType", Fields: map[string]string{"DeviceID": fmt.Sprintf("device-%d", index)},
			}})
		}
	}()
	for range 2 {
		go func() {
			defer group.Done()
			for index := 0; index < 500; index++ {
				state, ok := api.GetQueryState(gb10DeviceID)
				if !ok {
					continue
				}
				if state.DeviceStatus != nil && len(state.DeviceStatus.FaultDeviceIDs) > 0 {
					state.DeviceStatus.FaultDeviceIDs[0] = "reader mutation"
				}
				if len(state.AppendixA4) > 0 {
					state.AppendixA4[0].Fields["DeviceID"] = "reader mutation"
				}
			}
		}()
	}
	group.Wait()
}

func TestCleanupDevicePreventsLateQueryStateCommit(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api

	api.storeQueryStateForOwner(gb10DeviceID, gb10DeviceID, "DeviceStatus", &DeviceStatusData{Online: "ONLINE"})
	api.storeAppendixA4StateForOwner(gb10DeviceID, gb10ChannelID, []AppendixA4Object{{
		Type: "doorType", Fields: map[string]string{"DeviceID": gb10ChannelID},
	}})

	// 模拟 Core.DelDevice：删除锁覆盖协议清理、持久化删除和内存设备移除。
	unlockDelete := api.lockRegisterOperation(gb10DeviceID)
	api.queryStateMu.Lock()
	writerStarted := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		close(writerStarted)
		api.storeQueryStateForOwner(gb10DeviceID, gb10ChannelID, "PTZPosition", &PTZPositionData{})
	}()
	<-writerStarted

	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- api.cleanupDevice(context.Background(), gb10DeviceID)
	}()
	// 删除仍持有注册操作锁时，内存设备会在协议清理后从缓存移除。
	memory.runtime = nil
	api.queryStateMu.Unlock()
	if err := <-cleanupDone; err != nil {
		unlockDelete()
		t.Fatal(err)
	}
	unlockDelete()
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("late query state writer did not finish")
	}

	if _, ok := api.GetQueryState(gb10DeviceID); ok {
		t.Fatal("deleted parent query state was revived")
	}
	if _, ok := api.GetQueryState(gb10ChannelID); ok {
		t.Fatal("deleted child query state was revived")
	}
}

func TestQueryStateDeletionTombstoneRecoversWhenDeviceDeleteRollsBack(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	memory := newFlowMemory(persistentDevice.DeviceID)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	api.deviceDeletionTombstones.Store(persistentDevice.DeviceID, struct{}{})

	api.storeQueryState(persistentDevice.DeviceID, "DeviceStatus", &DeviceStatusData{Online: "ONLINE"})

	if _, deleted := api.deviceDeletionTombstones.Load(persistentDevice.DeviceID); deleted {
		t.Fatal("rolled-back device deletion retained query-state tombstone")
	}
	if _, ok := api.GetQueryState(persistentDevice.DeviceID); !ok {
		t.Fatal("rolled-back device deletion did not restore query-state writes")
	}
}

func TestDeviceDeleteLockReclaimsSettledDeletionTombstone(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	api := &GB28181API{core: adapter}
	server := &Server{gb: api}
	api.svr = server
	api.deviceDeletionTombstones.Store(persistentDevice.DeviceID, struct{}{})
	api.deviceOfflineTombstones.Store(persistentDevice.DeviceID, struct{}{})

	if err := adapter.Store().Device().Delete(t.Context(), persistentDevice, orm.Where("device_id = ?", persistentDevice.DeviceID)); err != nil {
		t.Fatal(err)
	}
	unlock := server.LockDeviceDelete(persistentDevice.DeviceID)
	unlock()
	if _, deleted := api.deviceDeletionTombstones.Load(persistentDevice.DeviceID); deleted {
		t.Fatal("settled device deletion retained tombstone")
	}
	if _, offline := api.deviceOfflineTombstones.Load(persistentDevice.DeviceID); offline {
		t.Fatal("settled device deletion retained offline tombstone")
	}
}

func TestDeviceDeleteLockKeepsOfflineTombstoneWhenDeleteRollsBack(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	api := &GB28181API{core: adapter}
	server := &Server{gb: api}
	api.svr = server
	api.deviceDeletionTombstones.Store(persistentDevice.DeviceID, struct{}{})
	api.deviceOfflineTombstones.Store(persistentDevice.DeviceID, struct{}{})

	unlock := server.LockDeviceDelete(persistentDevice.DeviceID)
	unlock()
	if _, deleted := api.deviceDeletionTombstones.Load(persistentDevice.DeviceID); deleted {
		t.Fatal("rolled-back device deletion retained deletion tombstone")
	}
	if _, offline := api.deviceOfflineTombstones.Load(persistentDevice.DeviceID); !offline {
		t.Fatal("rolled-back offline device deletion discarded offline tombstone")
	}
}

func TestDeviceDeleteUnlockIsIdempotentAcrossNewLockGeneration(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	api := &GB28181API{core: adapter}
	server := &Server{gb: api}
	api.svr = server

	unlockFirst := server.LockDeviceDelete(persistentDevice.DeviceID)
	unlockFirst()

	unlockSecond := server.LockDeviceDelete(persistentDevice.DeviceID)
	api.deviceDeletionTombstones.Store(persistentDevice.DeviceID, struct{}{})
	unlockFirst()
	if _, deleted := api.deviceDeletionTombstones.Load(persistentDevice.DeviceID); !deleted {
		unlockSecond()
		t.Fatal("stale unlock cleared a newer device deletion tombstone")
	}

	unlockSecond()
	if _, deleted := api.deviceDeletionTombstones.Load(persistentDevice.DeviceID); deleted {
		t.Fatal("current unlock retained rolled-back device deletion tombstone")
	}
}

func TestCleanupDeviceCannotMissLatePendingRequest(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api

	unlockDelete := api.lockRegisterOperation(gb10DeviceID)
	if err := api.cleanupDevice(context.Background(), gb10DeviceID); err != nil {
		unlockDelete()
		t.Fatal(err)
	}
	type trackedOperation struct {
		operation *pendingDeviceOperation
		release   func()
	}
	started := make(chan struct{})
	tracked := make(chan trackedOperation, 1)
	go func() {
		close(started)
		operation, release := api.trackPendingDeviceRequest(context.Background(), gb10DeviceID, gb10ChannelID)
		tracked <- trackedOperation{operation: operation, release: release}
	}()
	<-started
	unlockDelete()
	result := <-tracked
	defer result.release()

	if !errors.Is(result.operation.Cause(), ErrDeviceNotExist) {
		t.Fatalf("late pending request cause = %v; want %v", result.operation.Cause(), ErrDeviceNotExist)
	}
	if _, ok := api.pendingDeviceRequests.Load(result.operation); ok {
		t.Fatal("late pending request was published after device cleanup")
	}
}

func TestOfflineCleanupCannotMissLatePendingRequest(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api

	unlockOffline := api.lockRegisterOperation(gb10DeviceID)
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = false
		device.registrationClosed = true
	})
	api.cleanupOfflineDeviceRuntime(gb10DeviceID)
	type trackedOperation struct {
		operation *pendingDeviceOperation
		release   func()
	}
	started := make(chan struct{})
	tracked := make(chan trackedOperation, 1)
	go func() {
		close(started)
		operation, release := api.trackPendingDeviceRequest(context.Background(), gb10DeviceID, gb10ChannelID)
		tracked <- trackedOperation{operation: operation, release: release}
	}()
	<-started
	unlockOffline()
	result := <-tracked
	defer result.release()

	if !errors.Is(result.operation.Cause(), ErrDeviceOffline) {
		t.Fatalf("late pending request cause = %v; want %v", result.operation.Cause(), ErrDeviceOffline)
	}
	if _, ok := api.pendingDeviceRequests.Load(result.operation); ok {
		t.Fatal("late pending request was published after offline cleanup")
	}
}

func TestOnlineDeviceStatusClearsOfflineOperationTombstone(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	registeredAt := time.Now()
	memory.persistent.RegisteredAt = orm.Time{Time: registeredAt}
	memory.persistent.Expires = 3600
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = false
		device.LastRegisterAt = registeredAt
		device.Expires = 3600
		device.registrationClosed = false
	})
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api
	api.deviceOfflineTombstones.Store(gb10DeviceID, struct{}{})

	if err := api.applyDeviceStatus(gb10DeviceID, &DeviceStatusData{Online: "ONLINE"}); err != nil {
		t.Fatal(err)
	}
	if _, offline := api.deviceOfflineTombstones.Load(gb10DeviceID); offline {
		t.Fatal("online DeviceStatus retained offline-operation tombstone")
	}
}

func TestGenericQueryAcknowledgesBeforeSinglePersistence(t *testing.T) {
	base, _, _ := newCascadeMediaCore(t)
	deviceStore := &countingQueryDeviceStore{DeviceStorer: base.Store().Device()}
	store := &queryTestStore{Storer: base.Store(), device: deviceStore}
	api := &GB28181API{core: ipc.NewAdapter(store, uniqueid.Core{})}
	memory := &blockingQueryMemory{
		flowMemory: newFlowMemory(gb10DeviceID),
		queryMu:    &api.queryStateMu,
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	memory.runtime.setGBVersion(GBVersion30)
	api.svr = &Server{memoryStorer: memory}
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "DeviceStatus", 91), pending)

	conn := newFlowConnection()
	body := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>91</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status>` +
		`<Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID><DoorID>` + gb10DeviceID +
		`</DoorID></doorType></Info></Response>`)
	request := newFlowRequest(t, conn, sip.MethodMessage, "device-status-single-persistence", body)
	to := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.sipMessageQueryGeneric(&sip.Context{
			Request:  request,
			Tx:       sip.NewTransaction("device-status-single-persistence-tx", conn),
			DeviceID: gb10DeviceID,
			Source:   conn.remote,
			To:       to,
			Log:      slog.Default(),
		})
	}()
	release := func() {
		select {
		case <-memory.release:
		default:
			close(memory.release)
		}
	}
	defer release()

	select {
	case <-memory.entered:
	case <-time.After(time.Second):
		t.Fatal("DeviceStatus persistence was not reached")
	}
	select {
	case payload := <-conn.writes:
		if response := string(payload); !strings.Contains(response, "SIP/2.0 200 OK") {
			t.Fatalf("unexpected SIP response:\n%s", response)
		}
	default:
		release()
		<-done
		t.Fatal("DeviceStatus persistence delayed SIP 200 OK")
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("DeviceStatus handler did not finish")
	}

	if got := memory.changes.Load(); got != 1 {
		t.Fatalf("DeviceStatus changes = %d, want 1", got)
	}
	if memory.queryLockHeld.Load() {
		t.Fatal("DeviceStatus persistence ran while queryStateMu was held")
	}
	if got := deviceStore.updates.Load(); got != 1 {
		t.Fatalf("Appendix A.4 persistence updates = %d, want 1", got)
	}
	select {
	case output := <-pending.wait:
		status, ok := output.Data.(*DeviceStatusData)
		if !ok || len(output.AppendixA4) != 1 {
			t.Fatalf("pending DeviceStatus output = %+v", output)
		}
		status.Online = "OFFLINE"
		output.AppendixA4[0].Fields["DeviceID"] = "mutated"
	default:
		t.Fatal("DeviceStatus response did not resolve pending query")
	}
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.DeviceStatus == nil || state.DeviceStatus.Online != "ONLINE" ||
		len(state.AppendixA4) != 1 || state.AppendixA4[0].Fields["DeviceID"] != gb10DeviceID {
		t.Fatalf("DeviceQuery output leaked internal state: %+v", state)
	}
}

func TestHomePositionQuery2022ProductionMessageRoute(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	api := &GB28181API{}
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	sipServer := sip.NewServer(local)
	server := &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	api.svr = server
	sipServer.Message(api.sipAccessControlMiddleware, api.sipCascadeMessageMiddleware).
		Handle("HomePositionQuery", api.sipMessageQueryGeneric)

	operation := newPendingDeviceOperation(t.Context(), gb10DeviceID, gb10DeviceID)
	defer operation.Cancel(nil)
	pending := &pendingQueryWait{
		wait:      make(chan *DeviceQueryOutput, 1),
		targetID:  gb10DeviceID,
		operation: operation,
	}
	const sn = 92
	waitKey := buildPendingQueryKey(gb10DeviceID, "HomePositionQuery", sn)
	api.pendingDeviceQuery.Store(waitKey, pending)
	defer api.pendingDeviceQuery.Delete(waitKey)

	serverConn, clientConn := net.Pipe()
	go sipServer.ProcessTCPConnection(sip.NewTCPConnection(serverConn))
	t.Cleanup(func() {
		_ = clientConn.Close()
		sipServer.Close()
	})
	body := `<Response><CmdType>HomePositionQuery</CmdType><SN>92</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><HomePosition><Enabled>1</Enabled><ResetTime>60</ResetTime>` +
		`<PresetIndex>7</PresetIndex></HomePosition></Response>`
	request := fmt.Sprintf("MESSAGE sip:%s@3402000000 SIP/2.0\r\nVia: SIP/2.0/TCP 192.0.2.10:5060;branch=z9hG4bK-home-position-query-2022\r\nFrom: <sip:%s@3402000000>;tag=home-position-query-2022\r\nTo: <sip:%s@3402000000>\r\nCall-ID: home-position-query-2022\r\nCSeq: 1 MESSAGE\r\nMax-Forwards: 70\r\nX-GB-Ver: 3.0\r\nContent-Type: Application/MANSCDP+xml\r\nContent-Length: %d\r\n\r\n%s",
		gb10PlatformID, gb10DeviceID, gb10PlatformID, len(body), body)
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
	assertFlowOK(t, string(response[:n]))

	select {
	case output := <-pending.wait:
		data, ok := output.Data.(*HomePositionData)
		if !ok || data.Enabled == nil || *data.Enabled != 1 || data.ResetTime == nil || *data.ResetTime != 60 ||
			data.PresetIndex == nil || *data.PresetIndex != 7 {
			t.Fatalf("pending HomePositionQuery output = %+v", output)
		}
	case <-time.After(time.Second):
		t.Fatal("HomePositionQuery response did not resolve pending query")
	}

	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.HomePosition == nil || state.HomePosition.Enabled == nil || *state.HomePosition.Enabled != 1 ||
		state.HomePosition.ResetTime == nil || *state.HomePosition.ResetTime != 60 ||
		state.HomePosition.PresetIndex == nil || *state.HomePosition.PresetIndex != 7 {
		t.Fatalf("HomePositionQuery state = %+v", state)
	}
}

func TestConfigDownloadAcknowledgesWhenRuntimeDisappears(t *testing.T) {
	tests := []struct {
		version            GBProtocolVersion
		param              string
		extra              string
		loadsBeforeMissing int32
		wantAppendixA4     bool
	}{
		{
			version:            GBVersion11,
			loadsBeforeMissing: 2,
			param: `<BasicParam><Name>IPC</Name><DeviceID>` + gb10DeviceID + `</DeviceID>` +
				`<SIPServerID>` + gb10PlatformID + `</SIPServerID><SIPServerIP>192.0.2.20</SIPServerIP>` +
				`<SIPServerPort>5060</SIPServerPort><DomainName>3402000000</DomainName><Expiration>3600</Expiration>` +
				`<Password>secret</Password><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`,
		},
		{
			version:            GBVersion20,
			loadsBeforeMissing: 2,
			param: `<BasicParam><Name>IPC</Name><Expiration>3600</Expiration>` +
				`<HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`,
		},
		{
			version:            GBVersion30,
			loadsBeforeMissing: 3,
			param:              `<BasicParam><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`,
			extra:              `<ExtraInfo>{"type":"doorType","DeviceID":"` + gb10DeviceID + `"}</ExtraInfo>`,
			wantAppendixA4:     true,
		},
	}

	for _, test := range tests {
		t.Run(string(test.version), func(t *testing.T) {
			adapter, _, _ := newCascadeMediaCore(t)
			memory := &disappearingConfigMemory{
				versionGateMemory:  &versionGateMemory{device: &Device{IsOnline: true, gbVersion: string(test.version)}},
				loadsBeforeMissing: test.loadsBeforeMissing,
			}
			api := &GB28181API{svr: &Server{memoryStorer: memory}, core: adapter}
			pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
			api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "ConfigDownload", 92), pending)
			body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>92</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>OK</Result>` + test.param + test.extra + `</Response>`)

			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-runtime-disappears", body, api.sipMessageConfigDownload)
			assertFlowOK(t, response)
			select {
			case output := <-pending.wait:
				if output == nil || output.Result != "OK" || output.CmdType != "ConfigDownload" {
					t.Fatalf("ConfigDownload output = %+v", output)
				}
			default:
				t.Fatal("ConfigDownload runtime disappearance left query waiting")
			}
			state, ok := api.GetQueryState(gb10DeviceID)
			if !ok {
				t.Fatal("ConfigDownload runtime disappearance discarded validated response state")
			}
			if test.wantAppendixA4 && (len(state.AppendixA4) != 1 || state.AppendixA4[0].Type != "doorType") {
				t.Fatalf("ConfigDownload runtime disappearance lost validated Appendix A.4: %+v", state.AppendixA4)
			}
		})
	}
}

func TestConfigDownloadErrorDoesNotPolluteSuccessfulQueryState(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		payload string
	}{
		{
			name:    "2014",
			version: GBVersion11,
			payload: `<BasicParam><Name>poison</Name><DeviceID>` + gb10DeviceID + `</DeviceID>` +
				`<SIPServerID>` + gb10PlatformID + `</SIPServerID><SIPServerIP>192.0.2.20</SIPServerIP>` +
				`<SIPServerPort>5060</SIPServerPort><DomainName>3402000000</DomainName><Expiration>3600</Expiration>` +
				`<Password>secret</Password><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`,
		},
		{
			name:    "2016",
			version: GBVersion20,
			payload: `<BasicParam><Name>poison</Name><Expiration>3600</Expiration>` +
				`<HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`,
		},
		{
			name:    "2022",
			version: GBVersion30,
			payload: `<BasicParam><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>` +
				`<ExtraInfo>{"type":"doorType","DeviceID":"` + gb10DeviceID + `"}</ExtraInfo>`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(test.version)
			adapter, _, _ := newCascadeMediaCore(t)
			api.core = adapter
			api.queryStates.Store(gb10DeviceID, &QueryState{
				ConfigDownload: &ConfigDownloadState{
					CmdType: "ConfigDownload", SN: 91, DeviceID: gb10DeviceID, Result: "OK",
					BasicParam: &BasicParam{Name: "stable"},
				},
				AppendixA4: []AppendixA4Object{{Type: "stableType", Fields: map[string]string{"value": "stable"}}},
			})
			pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
			api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "ConfigDownload", 92), pending)
			body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>92</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>ERROR</Result>` + test.payload + `</Response>`)

			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-error-state-"+test.name, body, api.sipMessageConfigDownload)
			assertFlowOK(t, response)
			select {
			case output := <-pending.wait:
				if output == nil || output.Result != "ERROR" || output.Data != nil || len(output.AppendixA4) != 0 {
					t.Fatalf("ConfigDownload ERROR output = %+v", output)
				}
			case <-time.After(time.Second):
				t.Fatal("ConfigDownload ERROR did not resolve pending query")
			}

			state, ok := api.GetQueryState(gb10DeviceID)
			if !ok || state.ConfigDownload == nil || state.ConfigDownload.Result != "OK" ||
				state.ConfigDownload.SN != 91 || state.ConfigDownload.BasicParam == nil || state.ConfigDownload.BasicParam.Name != "stable" {
				t.Fatalf("ConfigDownload ERROR changed successful state: %+v", state)
			}
			if len(state.AppendixA4) != 1 || state.AppendixA4[0].Type != "stableType" {
				t.Fatalf("ConfigDownload ERROR changed Appendix A.4 state: %+v", state.AppendixA4)
			}
		})
	}
}

func TestDeviceInfoErrorDoesNotPolluteAppendixA4State(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	adapter, _, _ := newCascadeMediaCore(t)
	api.core = adapter
	api.queryStates.Store(gb10DeviceID, &QueryState{
		AppendixA4: []AppendixA4Object{{Type: "stableType", Fields: map[string]string{"value": "stable"}}},
	})
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "DeviceInfo", 93), pending)
	body := []byte(`<Response><CmdType>DeviceInfo</CmdType><SN>93</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Result>ERROR</Result><Info><doorType><DeviceID>` + gb10DeviceID +
		`</DeviceID></doorType></Info></Response>`)

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "device-info-error-state", body, api.sipMessageDeviceInfo)
	assertFlowOK(t, response)
	select {
	case output := <-pending.wait:
		if output == nil || output.Result != "ERROR" || output.Data != nil || len(output.AppendixA4) != 0 {
			t.Fatalf("DeviceInfo ERROR output = %+v", output)
		}
	case <-time.After(time.Second):
		t.Fatal("DeviceInfo ERROR did not resolve pending query")
	}

	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || len(state.AppendixA4) != 1 || state.AppendixA4[0].Type != "stableType" {
		t.Fatalf("DeviceInfo ERROR changed Appendix A.4 state: %+v", state)
	}
}

func TestDeviceStatusErrorDoesNotPolluteSuccessfulState(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		extra   string
	}{
		{name: "2011", version: GBVersion10, extra: `<Info>failed legacy state</Info>`},
		{name: "2014", version: GBVersion11, extra: `<Info>failed legacy state</Info>`},
		{name: "2016", version: GBVersion20, extra: `<Info>failed legacy state</Info>`},
		{
			name:    "2022",
			version: GBVersion30,
			extra: `<Info><doorType><DeviceID>` + gb10DeviceID +
				`</DeviceID></doorType></Info><ExtraInfo>failed modern state</ExtraInfo>`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, persistentDevice, _ := newCascadeMediaCore(t)
			memory := newFlowMemory(persistentDevice.DeviceID)
			memory.runtime.setGBVersion(test.version)
			memory.runtime.UpdateRuntime(func(device *Device) { device.IsOnline = false })
			api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
			api.queryStates.Store(gb10DeviceID, &QueryState{
				DeviceStatus: &DeviceStatusData{
					CmdType: "DeviceStatus", SN: 93, DeviceID: gb10DeviceID,
					Result: "OK", Online: "OFFLINE", Status: "OK", Reason: "stable",
				},
				AppendixA4: []AppendixA4Object{{Type: "stableType", Fields: map[string]string{"value": "stable"}}},
			})
			pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
			api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "DeviceStatus", 94), pending)
			body := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>94</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>ERROR</Result><Online>ONLINE</Online><Status>ERROR</Status>` + test.extra + `</Response>`)

			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
				"device-status-error-state-"+test.name, body, api.sipMessageQueryGeneric)
			assertFlowOK(t, response)
			select {
			case output := <-pending.wait:
				if output == nil || output.Result != "ERROR" || output.Data != nil || len(output.AppendixA4) != 0 {
					t.Fatalf("DeviceStatus ERROR output = %+v", output)
				}
			case <-time.After(time.Second):
				t.Fatal("DeviceStatus ERROR did not resolve pending query")
			}

			state, ok := api.GetQueryState(gb10DeviceID)
			if !ok || state.DeviceStatus == nil || state.DeviceStatus.Result != "OK" ||
				state.DeviceStatus.SN != 93 || state.DeviceStatus.Online != "OFFLINE" || state.DeviceStatus.Reason != "stable" {
				t.Fatalf("DeviceStatus ERROR changed successful state: %+v", state)
			}
			if len(state.AppendixA4) != 1 || state.AppendixA4[0].Type != "stableType" {
				t.Fatalf("DeviceStatus ERROR changed Appendix A.4 state: %+v", state.AppendixA4)
			}
			if memory.runtime.runtimeSnapshot().IsOnline {
				t.Fatal("DeviceStatus ERROR changed device runtime")
			}

			var stored ipc.Device
			if err := adapter.Store().Device().Get(t.Context(), &stored,
				orm.Where("device_id = ?", persistentDevice.DeviceID)); err != nil {
				t.Fatal(err)
			}
			if len(stored.Ext.GBAppendixA4) != 0 {
				t.Fatalf("DeviceStatus ERROR persisted Appendix A.4: %+v", stored.Ext.GBAppendixA4)
			}
		})
	}
}

func TestPresetQueryErrorDoesNotPolluteSuccessfulState(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20} {
		t.Run(version.StandardName(), func(t *testing.T) {
			api, _ := newVersionGateAPI(version)
			api.queryStates.Store(gb10DeviceID, &QueryState{
				Presets: []PresetItemData{{PresetID: "1", PresetName: "stable"}},
			})
			pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
			api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "PresetQuery", 95), pending)
			body := []byte(`<Response><CmdType>PresetQuery</CmdType><SN>95</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>ERROR</Result><SumNum>1</SumNum><PresetList Num="1"><Item>` +
				`<PresetID>2</PresetID><PresetName>failed</PresetName></Item></PresetList></Response>`)

			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
				"preset-query-error-state-"+string(version), body, api.sipMessageQueryGeneric)
			assertFlowOK(t, response)
			select {
			case output := <-pending.wait:
				if output == nil || output.Result != "ERROR" || output.Data != nil || len(output.AppendixA4) != 0 {
					t.Fatalf("PresetQuery ERROR output = %+v", output)
				}
			case <-time.After(time.Second):
				t.Fatal("PresetQuery ERROR did not resolve pending query")
			}

			state, ok := api.GetQueryState(gb10DeviceID)
			if !ok || len(state.Presets) != 1 || state.Presets[0].PresetID != "1" || state.Presets[0].PresetName != "stable" {
				t.Fatalf("PresetQuery ERROR changed successful state: %+v", state)
			}
		})
	}
}

func TestDeviceConfigErrorDoesNotPolluteSuccessfulState(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		extra   string
	}{
		{name: "2014", version: GBVersion11},
		{name: "2016", version: GBVersion20},
		{
			name:    "2022",
			version: GBVersion30,
			extra:   `<Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID></doorType></Info>`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, persistentDevice, _ := newCascadeMediaCore(t)
			memory := newFlowMemory(persistentDevice.DeviceID)
			memory.runtime.setGBVersion(test.version)
			api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
			api.queryStates.Store(gb10DeviceID, &QueryState{
				DeviceConfig: &DeviceConfigState{
					CmdType: "DeviceConfig", SN: 94, DeviceID: gb10DeviceID,
					Result: "OK", RawXML: "stable response",
				},
				AppendixA4: []AppendixA4Object{{Type: "stableType", Fields: map[string]string{"value": "stable"}}},
			})
			pending := &pendingDeviceConfig{
				wait: make(chan *DeviceConfigResponse, 1), targetID: gb10DeviceID,
			}
			api.pendingDeviceConfig.Store(buildPendingDeviceConfigKey(gb10DeviceID, 95), pending)
			body := []byte(`<Response><CmdType>DeviceConfig</CmdType><SN>95</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>ERROR</Result>` + test.extra + `</Response>`)

			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
				"device-config-error-state-"+test.name, body, api.handleDeviceConfig)
			assertFlowOK(t, response)
			select {
			case output := <-pending.wait:
				if output == nil || output.Result != "ERROR" || output.RawXML != string(body) {
					t.Fatalf("DeviceConfig ERROR output = %+v", output)
				}
			case <-time.After(time.Second):
				t.Fatal("DeviceConfig ERROR did not resolve pending request")
			}

			state, ok := api.GetQueryState(gb10DeviceID)
			if !ok || state.DeviceConfig == nil || state.DeviceConfig.Result != "OK" ||
				state.DeviceConfig.SN != 94 || state.DeviceConfig.RawXML != "stable response" {
				t.Fatalf("DeviceConfig ERROR changed successful state: %+v", state)
			}
			if len(state.AppendixA4) != 1 || state.AppendixA4[0].Type != "stableType" {
				t.Fatalf("DeviceConfig ERROR changed Appendix A.4 state: %+v", state.AppendixA4)
			}

			var stored ipc.Device
			if err := adapter.Store().Device().Get(t.Context(), &stored,
				orm.Where("device_id = ?", persistentDevice.DeviceID)); err != nil {
				t.Fatal(err)
			}
			if len(stored.Ext.GBAppendixA4) != 0 {
				t.Fatalf("DeviceConfig ERROR persisted Appendix A.4: %+v", stored.Ext.GBAppendixA4)
			}
		})
	}
}

func TestQueryResponsesCommitOnlyAfterSuccessfulSIPOK(t *testing.T) {
	tests := []struct {
		name        string
		version     GBProtocolVersion
		cmdType     string
		body        string
		handler     func(*GB28181API, *sip.Context)
		expectState bool
	}{
		{
			name: "DeviceInfo error", version: GBVersion11, cmdType: "DeviceInfo",
			body:    `<Response><CmdType>DeviceInfo</CmdType><SN>97</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>ERROR</Result></Response>`,
			handler: func(api *GB28181API, ctx *sip.Context) { api.sipMessageDeviceInfo(ctx) },
		},
		{
			name: "ConfigDownload error", version: GBVersion11, cmdType: "ConfigDownload",
			body:    `<Response><CmdType>ConfigDownload</CmdType><SN>97</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>ERROR</Result></Response>`,
			handler: func(api *GB28181API, ctx *sip.Context) { api.sipMessageConfigDownload(ctx) },
		},
		{
			name: "DeviceStatus", version: GBVersion11, cmdType: "DeviceStatus", expectState: true,
			body:    `<Response><CmdType>DeviceStatus</CmdType><SN>97</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status></Response>`,
			handler: func(api *GB28181API, ctx *sip.Context) { api.sipMessageQueryGeneric(ctx) },
		},
		{
			name: "Catalog", version: GBVersion11, cmdType: "Catalog",
			body:    `<Response><CmdType>Catalog</CmdType><SN>97</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>0</SumNum><DeviceList Num="0"></DeviceList></Response>`,
			handler: func(api *GB28181API, ctx *sip.Context) { api.sipMessageCatalog(ctx) },
		},
	}
	for _, test := range tests {
		for _, writeFailure := range []bool{false, true} {
			name := "success"
			var writeErr error
			if writeFailure {
				name = "write failure"
				writeErr = errors.New("write failed")
			}
			t.Run(test.name+"/"+name, func(t *testing.T) {
				api, _ := newVersionGateAPI(test.version)
				pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID}
				api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, test.cmdType, 97), pending)
				conn, done := startBlockingFlowHandler(t, api, sip.MethodMessage, "query-commit-"+test.name+"-"+name, []byte(test.body), func(ctx *sip.Context) {
					test.handler(api, ctx)
				}, writeErr)

				completedBeforeSIP := false
				select {
				case <-pending.wait:
					completedBeforeSIP = true
				default:
				}
				_, stateBeforeSIP := api.GetQueryState(gb10DeviceID)
				finishBlockingFlowHandler(t, conn, done)
				if completedBeforeSIP || stateBeforeSIP {
					t.Fatal("query response committed before SIP 200 was written")
				}

				completed := false
				select {
				case <-pending.wait:
					completed = true
				default:
				}
				if completed != !writeFailure {
					t.Fatalf("query wait completed = %v, want %v", completed, !writeFailure)
				}
				_, stateAfterSIP := api.GetQueryState(gb10DeviceID)
				if stateAfterSIP != (!writeFailure && test.expectState) {
					t.Fatalf("query state stored = %v, want %v", stateAfterSIP, !writeFailure && test.expectState)
				}
			})
		}
	}
}

func TestUnassociatedDeviceStatusResponseDoesNotChangeState(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(version)
			memory.persistent.IsOnline = true
			memory.runtime.UpdateRuntime(func(device *Device) {
				device.IsOnline = true
			})
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			body := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>7931</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>OK</Result><Online>OFFLINE</Online><Status>OK</Status></Response>`)

			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
				"unassociated-device-status-"+string(version), body, api.sipMessageQueryGeneric)
			assertFlowOK(t, response)
			if !memory.persistent.IsOnline || !memory.runtime.IsOnlineNow() {
				t.Fatal("unassociated DeviceStatus changed device online state")
			}
			if state, ok := api.GetQueryState(gb10DeviceID); ok && state.DeviceStatus != nil {
				t.Fatalf("unassociated DeviceStatus changed query state: %+v", state.DeviceStatus)
			}
		})
	}
}

func TestUnassociatedConfigDownloadResponseDoesNotChangeState(t *testing.T) {
	tests := []struct {
		version GBProtocolVersion
		payload string
	}{
		{
			version: GBVersion11,
			payload: `<BasicParam><Name>IPC</Name><DeviceID>` + gb10DeviceID + `</DeviceID>` +
				`<SIPServerID>` + gb10PlatformID + `</SIPServerID><SIPServerIP>192.0.2.20</SIPServerIP>` +
				`<SIPServerPort>5060</SIPServerPort><DomainName>3402000000</DomainName><Expiration>3600</Expiration>` +
				`<Password>secret</Password><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`,
		},
		{
			version: GBVersion20,
			payload: `<BasicParam><Name>IPC</Name><Expiration>3600</Expiration>` +
				`<HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`,
		},
		{
			version: GBVersion30,
			payload: `<BasicParam><HeartBeatInterval>60</HeartBeatInterval>` +
				`<HeartBeatCount>3</HeartBeatCount></BasicParam>`,
		},
	}
	for _, test := range tests {
		t.Run(string(test.version), func(t *testing.T) {
			api, _ := newVersionGateAPI(test.version)
			body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>7932</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>OK</Result>` + test.payload + `</Response>`)

			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
				"unassociated-config-download-"+string(test.version), body, api.sipMessageConfigDownload)
			assertFlowOK(t, response)
			if state, ok := api.GetQueryState(gb10DeviceID); ok && state.ConfigDownload != nil {
				t.Fatalf("unassociated ConfigDownload changed query state: %+v", state.ConfigDownload)
			}
		})
	}
}

func TestUnassociatedDeviceConfigResponseDoesNotChangeOrPersistState(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	memory := newFlowMemory(persistentDevice.DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
	body := []byte(`<Response><CmdType>DeviceConfig</CmdType><SN>7937</SN><DeviceID>` + persistentDevice.DeviceID +
		`</DeviceID><Result>OK</Result><Info><doorType><DeviceID>` + persistentDevice.DeviceID +
		`</DeviceID></doorType></Info></Response>`)

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
		"unassociated-device-config", body, api.handleDeviceConfig)
	assertFlowOK(t, response)
	if state, ok := api.GetQueryState(persistentDevice.DeviceID); ok &&
		(state.DeviceConfig != nil || len(state.AppendixA4) != 0) {
		t.Fatalf("unassociated DeviceConfig changed query state: %+v", state)
	}
	var stored ipc.Device
	if err := adapter.Store().Device().Get(t.Context(), &stored,
		orm.Where("device_id = ?", persistentDevice.DeviceID)); err != nil {
		t.Fatal(err)
	}
	if len(stored.Ext.GBAppendixA4) != 0 {
		t.Fatalf("unassociated DeviceConfig persisted Appendix A.4: %+v", stored.Ext.GBAppendixA4)
	}
}

func TestAutomaticConfigDownloadExpectationIsSingleUse(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion30)
	const sn = 7936
	cancel := api.expectAutomaticQueryResponse(gb10DeviceID, CMDTypeConfigDownload, sn, gb10DeviceID)
	defer cancel()
	responseBody := func(interval int) []byte {
		return []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>7936</SN><DeviceID>` + gb10DeviceID +
			`</DeviceID><Result>OK</Result><BasicParam><HeartBeatInterval>` + strconv.Itoa(interval) +
			`</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam></Response>`)
	}

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
		"automatic-config-download-first", responseBody(60), api.sipMessageConfigDownload)
	assertFlowOK(t, response)
	if _, ok := api.pendingDeviceQuery.Load(buildPendingQueryKey(gb10DeviceID, CMDTypeConfigDownload, sn)); ok {
		t.Fatal("automatic ConfigDownload expectation was not consumed")
	}
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.ConfigDownload == nil || state.ConfigDownload.BasicParam == nil ||
		state.ConfigDownload.BasicParam.HeartBeatInterval != 60 || memory.device.runtimeSnapshot().KeepaliveInterval != 60 {
		t.Fatalf("automatic ConfigDownload response state = %+v", state)
	}

	response = runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
		"automatic-config-download-duplicate", responseBody(90), api.sipMessageConfigDownload)
	assertFlowOK(t, response)
	state, _ = api.GetQueryState(gb10DeviceID)
	if state.ConfigDownload.BasicParam.HeartBeatInterval != 60 || memory.device.runtimeSnapshot().KeepaliveInterval != 60 {
		t.Fatalf("duplicate automatic ConfigDownload changed state: %+v", state.ConfigDownload)
	}
}

func TestUnassociatedDeviceInfoResponseDoesNotPersistMetadata(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	memory := newFlowMemory(persistentDevice.DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
	body := []byte(`<Response><CmdType>DeviceInfo</CmdType><SN>7933</SN><DeviceID>` + persistentDevice.DeviceID +
		`</DeviceID><DeviceName>Unassociated NVR</DeviceName><Result>OK</Result>` +
		`<Manufacturer>Unassociated Vendor</Manufacturer></Response>`)

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
		"unassociated-device-info", body, api.sipMessageDeviceInfo)
	assertFlowOK(t, response)
	var stored ipc.Device
	if err := adapter.Store().Device().Get(t.Context(), &stored, orm.Where("device_id = ?", persistentDevice.DeviceID)); err != nil {
		t.Fatal(err)
	}
	if stored.Ext.Name != "" || stored.Ext.Manufacturer != "" {
		t.Fatalf("unassociated DeviceInfo changed persisted metadata: %+v", stored.Ext)
	}
}

func TestUnassociatedMultiResponseDoesNotPersistAppendixA4(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		handler func(*GB28181API, *sip.Context)
	}{
		{
			name: "Catalog",
			body: `<Response><CmdType>Catalog</CmdType><SN>7934</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><SumNum>0</SumNum><DeviceList Num="0"></DeviceList>` +
				`<ExtraInfo>{"type":"doorType","DeviceID":"` + gb10DeviceID + `"}</ExtraInfo></Response>`,
			handler: func(api *GB28181API, ctx *sip.Context) { api.sipMessageCatalog(ctx) },
		},
		{
			name: "RecordInfo",
			body: `<Response><CmdType>RecordInfo</CmdType><SN>7935</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Name>record</Name><SumNum>0</SumNum><RecordList Num="0"></RecordList>` +
				`<ExtraInfo>{"type":"doorType","DeviceID":"` + gb10DeviceID + `"}</ExtraInfo></Response>`,
			handler: func(api *GB28181API, ctx *sip.Context) { api.sipMessageRecordInfo(ctx) },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, _, _ := newCascadeMediaCore(t)
			api, _ := newVersionGateAPI(GBVersion30)
			api.core = adapter
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
				"unassociated-multi-response-"+test.name, []byte(test.body), func(ctx *sip.Context) {
					test.handler(api, ctx)
				})
			assertFlowOK(t, response)
			if state, ok := api.GetQueryState(gb10DeviceID); ok && len(state.AppendixA4) != 0 {
				t.Fatalf("unassociated %s changed Appendix A.4 state: %+v", test.name, state.AppendixA4)
			}
		})
	}
}

type disappearingConfigMemory struct {
	*versionGateMemory
	loads              atomic.Int32
	loadsBeforeMissing int32
}

func (m *disappearingConfigMemory) Load(deviceID string) (*Device, bool) {
	if m.loads.Add(1) > m.loadsBeforeMissing {
		return nil, false
	}
	return m.versionGateMemory.Load(deviceID)
}

func TestAppendixA4HandlersAcknowledgeBeforePersistence(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	defer func() { config = previousConfig }()

	tests := []struct {
		name    string
		method  string
		body    []byte
		handler func(*GB28181API, *sip.Context)
	}{
		{
			name:   "Alarm",
			method: sip.MethodMessage,
			body: []byte(`<Notify><CmdType>Alarm</CmdType><SN>92</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime>` +
				`<Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID></doorType></Info></Notify>`),
			handler: func(api *GB28181API, ctx *sip.Context) {
				api.sipMessageAlarm(ctx)
			},
		},
		{
			name:   "DeviceConfig",
			method: sip.MethodMessage,
			body: []byte(`<Response><CmdType>DeviceConfig</CmdType><SN>93</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>OK</Result><Info><doorType><DeviceID>` + gb10DeviceID +
				`</DeviceID></doorType></Info></Response>`),
			handler: func(api *GB28181API, ctx *sip.Context) {
				api.handleDeviceConfig(ctx)
			},
		},
		{
			name:   "RecordInfo",
			method: sip.MethodMessage,
			body: []byte(`<Response><CmdType>RecordInfo</CmdType><SN>94</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Name>camera</Name><SumNum>0</SumNum><RecordList Num="0"></RecordList><ExtraInfo>{"type":"doorType","DeviceID":"` + gb10DeviceID +
				`"}</ExtraInfo></Response>`),
			handler: func(api *GB28181API, ctx *sip.Context) {
				api.sipMessageRecordInfo(ctx)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base, _, _ := newCascadeMediaCore(t)
			deviceStore := &blockingQueryDeviceStore{
				DeviceStorer: base.Store().Device(),
				entered:      make(chan struct{}),
				release:      make(chan struct{}),
			}
			store := &queryTestStore{Storer: base.Store(), device: deviceStore}
			api, _ := newVersionGateAPI(GBVersion30)
			api.core = ipc.NewAdapter(store, uniqueid.Core{})
			if test.name == "DeviceConfig" {
				key := buildPendingDeviceConfigKey(gb10DeviceID, 93)
				api.pendingDeviceConfig.Store(key, &pendingDeviceConfig{
					wait:     make(chan *DeviceConfigResponse, 1),
					targetID: gb10DeviceID,
				})
				defer api.pendingDeviceConfig.Delete(key)
			}
			if test.name == "RecordInfo" {
				api.recordResponses = newMultiResponseCollector(func(item RecordItem) string { return item.FilePath })
				api.recordResponses.Start(buildMultiResponseKey(gb10DeviceID, "RecordInfo", 94))
			}
			conn := newFlowConnection()
			request := newFlowRequest(t, conn, test.method, "appendix-a4-ack-"+test.name, test.body)
			to := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
			done := make(chan struct{})
			go func() {
				defer close(done)
				test.handler(api, &sip.Context{
					Request: request, Tx: sip.NewTransaction("appendix-a4-ack-"+test.name+"-tx", conn),
					DeviceID: gb10DeviceID, Source: conn.remote, To: to, Log: slog.Default(),
				})
			}()
			release := func() {
				select {
				case <-deviceStore.release:
				default:
					close(deviceStore.release)
				}
			}
			defer release()

			select {
			case <-deviceStore.entered:
			case <-time.After(time.Second):
				t.Fatal("Appendix A.4 persistence was not reached")
			}
			select {
			case payload := <-conn.writes:
				if response := string(payload); !strings.Contains(response, "SIP/2.0 200 OK") {
					t.Fatalf("unexpected SIP response:\n%s", response)
				}
			default:
				release()
				<-done
				t.Fatal("Appendix A.4 persistence delayed SIP 200 OK")
			}
			release()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler did not finish")
			}
			if got := deviceStore.updates.Load(); got != 1 {
				t.Fatalf("Appendix A.4 persistence updates = %d, want 1", got)
			}
		})
	}
}

func TestAppendixA4PersistenceStopsWithService(t *testing.T) {
	base, _, _ := newCascadeMediaCore(t)
	deviceStore := &lifecycleQueryDeviceStore{
		DeviceStorer: base.Store().Device(),
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
	}
	store := &queryTestStore{Storer: base.Store(), device: deviceStore}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		core:            ipc.NewAdapter(store, uniqueid.Core{}),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		lifecycleDone:   make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.persistAppendixA4Objects(gb10DeviceID, []AppendixA4Object{{
			Type: "doorType", Fields: map[string]string{"DeviceID": gb10DeviceID},
		}})
	}()
	release := func() {
		select {
		case <-deviceStore.release:
		default:
			close(deviceStore.release)
		}
	}
	defer release()

	select {
	case <-deviceStore.entered:
	case <-time.After(time.Second):
		t.Fatal("Appendix A.4 persistence was not reached")
	}
	api.beginClose()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		release()
		<-done
		t.Fatal("Appendix A.4 persistence ignored service cancellation")
	}
	if !deviceStore.cancelled.Load() {
		t.Fatal("Appendix A.4 persistence did not receive the service cancellation context")
	}
}

func TestLegacyAppendixA4QueryHandlersRejectBeforeStateAndWait(t *testing.T) {
	tests := []struct {
		name    string
		cmdType string
		method  string
		body    string
		handler func(*GB28181API, *sip.Context)
		wait    bool
	}{
		{
			name: "DeviceInfo", cmdType: "DeviceInfo", method: sip.MethodMessage, wait: true,
			body:    `<Response><CmdType>DeviceInfo</CmdType><SN>96</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>ERROR</Result><Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID></doorType></Info></Response>`,
			handler: func(api *GB28181API, ctx *sip.Context) { api.sipMessageDeviceInfo(ctx) },
		},
		{
			name: "ConfigDownload", cmdType: "ConfigDownload", method: sip.MethodMessage, wait: true,
			body:    `<Response><CmdType>ConfigDownload</CmdType><SN>96</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>ERROR</Result><Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID></doorType></Info></Response>`,
			handler: func(api *GB28181API, ctx *sip.Context) { api.sipMessageConfigDownload(ctx) },
		},
		{
			name: "Catalog response", cmdType: "Catalog", method: sip.MethodMessage, wait: true,
			body:    `<Response><CmdType>Catalog</CmdType><SN>96</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>0</SumNum><DeviceList Num="0"></DeviceList><Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID></doorType></Info></Response>`,
			handler: func(api *GB28181API, ctx *sip.Context) { api.sipMessageCatalog(ctx) },
		},
		{
			name: "Catalog notification", cmdType: "Catalog", method: sip.MethodNotify,
			body:    `<Notify><CmdType>Catalog</CmdType><SN>96</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>0</SumNum><DeviceList Num="0"></DeviceList><Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID></doorType></Info></Notify>`,
			handler: func(api *GB28181API, ctx *sip.Context) { api.sipNotifyCatalog(ctx) },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(GBVersion20)
			var pending *pendingQueryWait
			if test.wait {
				pending = &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
				api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, test.cmdType, 96), pending)
			}
			response := runFlowHandler(t, newFlowConnection(), api, test.method, "legacy-a4-"+test.name, []byte(test.body), func(ctx *sip.Context) {
				test.handler(api, ctx)
			})
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("legacy Appendix A.4 response = %s", response)
			}
			if _, ok := api.GetQueryState(gb10DeviceID); ok {
				t.Fatal("legacy Appendix A.4 changed query state")
			}
			if pending != nil {
				select {
				case output := <-pending.wait:
					t.Fatalf("legacy Appendix A.4 resolved query wait: %+v", output)
				default:
				}
			}
		})
	}
}

func TestKeepaliveAcknowledgesBeforePersistenceAndTreatsMissingStatusOnline(t *testing.T) {
	api := &GB28181API{}
	memory := &blockingKeepaliveMemory{
		flowMemory: newFlowMemory(gb10DeviceID),
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	api.svr = &Server{memoryStorer: memory}
	conn := newFlowConnection()
	body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>94</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Notify>`)
	request := newFlowRequest(t, conn, sip.MethodMessage, "keepalive-ack-before-persistence", body)
	to := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.sipMessageKeepalive(&sip.Context{
			Request: request, Tx: sip.NewTransaction("keepalive-ack-before-persistence-tx", conn),
			DeviceID: gb10DeviceID, Source: conn.remote, To: to, Log: slog.Default(), XGBVer: string(GBVersion10),
		})
	}()
	release := func() {
		select {
		case <-memory.release:
		default:
			close(memory.release)
		}
	}
	defer release()

	select {
	case <-memory.entered:
	case <-time.After(time.Second):
		t.Fatal("Keepalive persistence was not reached")
	}
	select {
	case payload := <-conn.writes:
		if response := string(payload); !strings.Contains(response, "SIP/2.0 200 OK") {
			t.Fatalf("unexpected SIP response:\n%s", response)
		}
	default:
		release()
		<-done
		t.Fatal("Keepalive persistence delayed SIP 200 OK")
	}
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.DeviceStatus == nil || state.DeviceStatus.Online != "ONLINE" {
		t.Fatalf("missing-status Keepalive state = %+v", state)
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Keepalive handler did not finish")
	}
	if !memory.persistent.IsOnline {
		t.Fatal("missing-status Keepalive persisted device as offline")
	}
}

func TestKeepaliveCommitsOnlyAfterSuccessfulSIPOK(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	base := newFlowConnection()
	conn := &blockingFlowResponseConnection{
		flowConnection: base,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
		writeErr:       errors.New("Keepalive SIP OK write failed"),
	}
	body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>95</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`)
	request := newFlowRequest(t, base, sip.MethodMessage, "keepalive-commit-after-sip-ok", body)
	request.SetConnection(conn)
	to := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.sipMessageKeepalive(&sip.Context{
			Request: request, Tx: sip.NewTransaction("keepalive-commit-after-sip-ok-tx", conn),
			DeviceID: gb10DeviceID, Source: base.remote, To: to, Log: slog.Default(), XGBVer: string(GBVersion10),
		})
	}()

	select {
	case <-conn.started:
	case <-time.After(time.Second):
		close(conn.release)
		t.Fatal("Keepalive SIP response write did not start")
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.DeviceStatus != nil {
		close(conn.release)
		<-done
		t.Fatalf("Keepalive committed query state before SIP OK: %+v", state.DeviceStatus)
	}
	if state := memory.runtime.runtimeSnapshot(); !state.LastKeepaliveAt.IsZero() {
		close(conn.release)
		<-done
		t.Fatalf("Keepalive committed runtime before SIP OK: %+v", state)
	}

	close(conn.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Keepalive handler did not return after SIP OK write failure")
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.DeviceStatus != nil {
		t.Fatalf("Keepalive committed query state after SIP OK write failure: %+v", state.DeviceStatus)
	}
	if state := memory.runtime.runtimeSnapshot(); !state.LastKeepaliveAt.IsZero() {
		t.Fatalf("Keepalive committed runtime after SIP OK write failure: %+v", state)
	}
	if !memory.persistent.KeepaliveAt.IsZero() {
		t.Fatalf("Keepalive persisted after SIP OK write failure: %+v", memory.persistent.KeepaliveAt)
	}
}

func TestDeviceInfoAcknowledgesBeforePersistence(t *testing.T) {
	base, _, _ := newCascadeMediaCore(t)
	deviceStore := &blockingQueryDeviceStore{
		DeviceStorer: base.Store().Device(),
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
	}
	store := &queryTestStore{Storer: base.Store(), device: deviceStore}
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	api := &GB28181API{core: ipc.NewAdapter(store, uniqueid.Core{}), svr: &Server{memoryStorer: memory}}
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "DeviceInfo", 95), pending)
	conn := newFlowConnection()
	body := []byte(`<Response><CmdType>DeviceInfo</CmdType><SN>95</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><DeviceName>Slow IPC</DeviceName><Result>OK</Result></Response>`)
	request := newFlowRequest(t, conn, sip.MethodMessage, "device-info-ack-before-persistence", body)
	to := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.sipMessageDeviceInfo(&sip.Context{
			Request: request, Tx: sip.NewTransaction("device-info-ack-before-persistence-tx", conn),
			DeviceID: gb10DeviceID, Source: conn.remote, To: to, Log: slog.Default(),
		})
	}()
	release := func() {
		select {
		case <-deviceStore.release:
		default:
			close(deviceStore.release)
		}
	}
	defer release()

	select {
	case <-deviceStore.entered:
	case <-time.After(time.Second):
		t.Fatal("DeviceInfo persistence was not reached")
	}
	select {
	case payload := <-conn.writes:
		if response := string(payload); !strings.Contains(response, "SIP/2.0 200 OK") {
			t.Fatalf("unexpected SIP response:\n%s", response)
		}
	default:
		release()
		<-done
		t.Fatal("DeviceInfo persistence delayed SIP 200 OK")
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("DeviceInfo handler did not finish")
	}
	if got := deviceStore.updates.Load(); got != 1 {
		t.Fatalf("DeviceInfo persistence updates = %d, want 1", got)
	}
}

type blockingQueryMemory struct {
	*flowMemory
	queryMu       *sync.RWMutex
	entered       chan struct{}
	release       chan struct{}
	changes       atomic.Int32
	queryLockHeld atomic.Bool
	enterOnce     sync.Once
}

type blockingKeepaliveMemory struct {
	*flowMemory
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
}

type deviceStatusFailureMemory struct {
	*flowMemory
	changeErr error
}

func (m *deviceStatusFailureMemory) Change(deviceID string, changePersistent func(*ipc.Device) error, changeRuntime func(*Device)) error {
	if m.changeErr != nil {
		return m.changeErr
	}
	return m.flowMemory.Change(deviceID, changePersistent, changeRuntime)
}

func (m *blockingKeepaliveMemory) Change(deviceID string, changePersistent func(*ipc.Device) error, changeRuntime func(*Device)) error {
	m.enterOnce.Do(func() { close(m.entered) })
	<-m.release
	return m.flowMemory.Change(deviceID, changePersistent, changeRuntime)
}

func (m *blockingQueryMemory) Change(deviceID string, changePersistent func(*ipc.Device) error, changeRuntime func(*Device)) error {
	m.changes.Add(1)
	if !m.queryMu.TryLock() {
		m.queryLockHeld.Store(true)
	} else {
		m.queryMu.Unlock()
	}
	m.enterOnce.Do(func() { close(m.entered) })
	<-m.release
	return m.flowMemory.Change(deviceID, changePersistent, changeRuntime)
}

type countingQueryDeviceStore struct {
	ipc.DeviceStorer
	updates atomic.Int32
}

type blockingQueryDeviceStore struct {
	ipc.DeviceStorer
	updates   atomic.Int32
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
}

type lifecycleQueryDeviceStore struct {
	ipc.DeviceStorer
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
	cancelled atomic.Bool
}

func (s *lifecycleQueryDeviceStore) Update(ctx context.Context, _ *ipc.Device, _ func(*ipc.Device) error, _ ...orm.QueryOption) error {
	s.enterOnce.Do(func() { close(s.entered) })
	select {
	case <-ctx.Done():
		s.cancelled.Store(true)
		return ctx.Err()
	case <-s.release:
		return nil
	}
}

func (s *blockingQueryDeviceStore) Update(ctx context.Context, device *ipc.Device, change func(*ipc.Device) error, opts ...orm.QueryOption) error {
	s.updates.Add(1)
	s.enterOnce.Do(func() { close(s.entered) })
	<-s.release
	return s.DeviceStorer.Update(ctx, device, change, opts...)
}

func (s *countingQueryDeviceStore) Update(ctx context.Context, device *ipc.Device, change func(*ipc.Device) error, opts ...orm.QueryOption) error {
	s.updates.Add(1)
	return s.DeviceStorer.Update(ctx, device, change, opts...)
}

type queryTestStore struct {
	ipc.Storer
	device ipc.DeviceStorer
}

func (s *queryTestStore) Device() ipc.DeviceStorer {
	return s.device
}
