package gbs

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestGenericQueryPayloadRejectsInvalidDataBeforeSideEffects(t *testing.T) {
	tests := []struct {
		name    string
		cmdType string
		payload string
	}{
		{name: "preset missing sum", cmdType: "PresetQuery", payload: `<PresetList Num="0"/>`},
		{name: "preset count mismatch", cmdType: "PresetQuery", payload: `<SumNum>1</SumNum><PresetList Num="0"/>`},
		{name: "preset missing item name", cmdType: "PresetQuery", payload: `<SumNum>1</SumNum><PresetList Num="1"><Item><PresetID>1</PresetID></Item></PresetList>`},
		{name: "preset duplicate sum", cmdType: "PresetQuery", payload: `<SumNum>1</SumNum><SumNum>1</SumNum><PresetList Num="1"><Item><PresetID>1</PresetID><PresetName>one</PresetName></Item></PresetList>`},
		{name: "preset unknown field", cmdType: "PresetQuery", payload: `<SumNum>0</SumNum><VendorField>value</VendorField><PresetList Num="0"/>`},
		{name: "preset out of order", cmdType: "PresetQuery", payload: `<PresetList Num="0"/><SumNum>0</SumNum>`},
		{name: "preset nested name", cmdType: "PresetQuery", payload: `<SumNum>1</SumNum><PresetList Num="1"><Item><PresetID>1</PresetID><PresetName><Value>one</Value></PresetName></Item></PresetList>`},
		{name: "home missing enabled", cmdType: "HomePositionQuery", payload: `<HomePosition><PresetIndex>1</PresetIndex></HomePosition>`},
		{name: "home invalid enabled", cmdType: "HomePositionQuery", payload: `<HomePosition><Enabled>2</Enabled></HomePosition>`},
		{name: "home invalid preset", cmdType: "HomePositionQuery", payload: `<HomePosition><Enabled>1</Enabled><PresetIndex>256</PresetIndex></HomePosition>`},
		{name: "home duplicate enabled", cmdType: "HomePositionQuery", payload: `<HomePosition><Enabled>1</Enabled><Enabled>1</Enabled></HomePosition>`},
		{name: "home unknown field", cmdType: "HomePositionQuery", payload: `<HomePosition><Enabled>1</Enabled><VendorField>value</VendorField></HomePosition>`},
		{name: "home out of order", cmdType: "HomePositionQuery", payload: `<HomePosition><Enabled>1</Enabled><PresetIndex>1</PresetIndex><ResetTime>1</ResetTime></HomePosition>`},
		{name: "cruise list missing sum", cmdType: "CruiseTrackListQuery", payload: `<CruiseTrackList Num="0"/>`},
		{name: "cruise list missing number", cmdType: "CruiseTrackListQuery", payload: `<SumNum>1</SumNum><CruiseTrackList Num="1"><CruiseTrack/></CruiseTrackList>`},
		{name: "cruise list invalid number", cmdType: "CruiseTrackListQuery", payload: `<SumNum>1</SumNum><CruiseTrackList Num="1"><CruiseTrack><Number>2</Number></CruiseTrack></CruiseTrackList>`},
		{name: "cruise list long name", cmdType: "CruiseTrackListQuery", payload: `<SumNum>1</SumNum><CruiseTrackList Num="1"><CruiseTrack><Number>0</Number><Name>` + strings.Repeat("a", 33) + `</Name></CruiseTrack></CruiseTrackList>`},
		{name: "cruise list unknown field", cmdType: "CruiseTrackListQuery", payload: `<SumNum>1</SumNum><CruiseTrackList Num="1"><CruiseTrack><Number>0</Number><VendorField>value</VendorField></CruiseTrack></CruiseTrackList>`},
		{name: "cruise list nested number", cmdType: "CruiseTrackListQuery", payload: `<SumNum>1</SumNum><CruiseTrackList Num="1"><CruiseTrack><Number><Value>0</Value></Number></CruiseTrack></CruiseTrackList>`},
		{name: "cruise missing point speed", cmdType: "CruiseTrackQuery", payload: `<Number>0</Number><SumNum>1</SumNum><CruisePointList Num="1"><CruisePoint><PresetIndex>1</PresetIndex><StayTime>1</StayTime></CruisePoint></CruisePointList>`},
		{name: "cruise invalid speed", cmdType: "CruiseTrackQuery", payload: `<Number>0</Number><SumNum>1</SumNum><CruisePointList Num="1"><CruisePoint><PresetIndex>1</PresetIndex><StayTime>1</StayTime><Speed>16</Speed></CruisePoint></CruisePointList>`},
		{name: "cruise point out of order", cmdType: "CruiseTrackQuery", payload: `<Number>0</Number><SumNum>1</SumNum><CruisePointList Num="1"><CruisePoint><StayTime>1</StayTime><PresetIndex>1</PresetIndex><Speed>1</Speed></CruisePoint></CruisePointList>`},
		{name: "cruise duplicate number", cmdType: "CruiseTrackQuery", payload: `<Number>0</Number><Number>0</Number><SumNum>0</SumNum>`},
		{name: "ptz non finite", cmdType: "PTZPosition", payload: `<Pan>NaN</Pan>`},
		{name: "ptz duplicate pan", cmdType: "PTZPosition", payload: `<Pan>1</Pan><Pan>2</Pan>`},
		{name: "ptz unknown field", cmdType: "PTZPosition", payload: `<VendorField>value</VendorField>`},
		{name: "ptz out of order", cmdType: "PTZPosition", payload: `<Zoom>1</Zoom><Tilt>1</Tilt>`},
		{name: "sd missing sum", cmdType: "SDCardStatus", payload: `<SDCardStatusInfo Num="0"/>`},
		{name: "sd missing name", cmdType: "SDCardStatus", payload: `<SumNum>1</SumNum><SDCardStatusInfo Num="1"><Item><ID>1</ID><Status>ok</Status><Capacity>10</Capacity><FreeSpace>5</FreeSpace></Item></SDCardStatusInfo>`},
		{name: "sd invalid status", cmdType: "SDCardStatus", payload: `<SumNum>1</SumNum><SDCardStatusInfo Num="1"><Item><ID>1</ID><HddName>card</HddName><Status>ready</Status><Capacity>10</Capacity><FreeSpace>5</FreeSpace></Item></SDCardStatusInfo>`},
		{name: "sd missing Num", cmdType: "SDCardStatus", payload: `<SumNum>0</SumNum><SDCardStatusInfo/>`},
		{name: "sd out of order", cmdType: "SDCardStatus", payload: `<SumNum>1</SumNum><SDCardStatusInfo Num="1"><Item><ID>1</ID><Status>ok</Status><HddName>card</HddName><Capacity>10</Capacity><FreeSpace>5</FreeSpace></Item></SDCardStatusInfo>`},
		{name: "sd unknown item field", cmdType: "SDCardStatus", payload: `<SumNum>1</SumNum><SDCardStatusInfo Num="1"><Item><ID>1</ID><HddName>card</HddName><Status>ok</Status><Capacity>10</Capacity><FreeSpace>5</FreeSpace><VendorField>value</VendorField></Item></SDCardStatusInfo>`},
		{name: "device status invalid encode", cmdType: "DeviceStatus", payload: `<Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Encode>YES</Encode>`},
		{name: "device status invalid record", cmdType: "DeviceStatus", payload: `<Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Record>YES</Record>`},
		{name: "device status invalid time", cmdType: "DeviceStatus", payload: `<Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><DeviceTime>invalid</DeviceTime>`},
		{name: "device status missing alarm count", cmdType: "DeviceStatus", payload: `<Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Alarmstatus/>`},
		{name: "device status alarm count mismatch", cmdType: "DeviceStatus", payload: `<Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Alarmstatus Num="2"><Item/></Alarmstatus>`},
		{name: "device status invalid alarm id", cmdType: "DeviceStatus", payload: `<Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Alarmstatus Num="1"><Item><DeviceID>bad</DeviceID><DutyStatus>ONDUTY</DutyStatus></Item></Alarmstatus>`},
		{name: "device status invalid duty status", cmdType: "DeviceStatus", payload: `<Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Alarmstatus Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><DutyStatus>READY</DutyStatus></Item></Alarmstatus>`},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(GBVersion30)
			sn := 900 + index
			pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
			api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, test.cmdType, sn), pending)
			body := []byte(fmt.Sprintf(`<Response><CmdType>%s</CmdType><SN>%d</SN><DeviceID>%s</DeviceID>%s</Response>`, test.cmdType, sn, gb10DeviceID, test.payload))
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "query-payload-invalid", body, api.sipMessageQueryGeneric)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid %s response = %s", test.cmdType, response)
			}
			select {
			case output := <-pending.wait:
				t.Fatalf("invalid payload resolved pending query: %+v", output)
			default:
			}
			if _, ok := api.GetQueryState(gb10DeviceID); ok {
				t.Fatal("invalid payload changed query state")
			}
		})
	}
}

func TestDeviceStatusPayloadUsesVersionSpecificAlarmField(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		count   string
		field   string
	}{
		{name: "2011 schema", version: GBVersion10, count: "Num", field: "Status"},
		{name: "2011 normative sample", version: GBVersion10, count: "Num", field: "DutyStatus"},
		{name: "2014 supplement", version: GBVersion11, count: "Num", field: "StatusDutyStatus"},
		{name: "2014 legacy vendor spelling", version: GBVersion11, count: "Num", field: "DutyStatus"},
		{name: "2016", version: GBVersion20, count: "num", field: "DutyStatus"},
		{name: "2022", version: GBVersion30, count: "Num", field: "DutyStatus"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Encode>ON</Encode><Record>OFF</Record>` +
				`<DeviceTime>2026-08-25T10:00:00+08:00</DeviceTime><Alarmstatus ` + test.count + `="1"><Item><DeviceID>` + gb10ChannelID +
				`</DeviceID><` + test.field + `>ONDUTY</` + test.field + `></Item></Alarmstatus></Response>`)
			if err := validateGenericQueryPayload(test.version, "DeviceStatus", body); err != nil {
				t.Fatalf("valid %s DeviceStatus rejected: %v", test.version, err)
			}
			data := decodeDeviceStatusData(body)
			if data == nil || data.Encode != "ON" || data.Record != "OFF" || data.DeviceTime == "" ||
				len(data.AlarmStatuses) != 1 || data.AlarmStatuses[0].DeviceID != gb10ChannelID || data.AlarmStatuses[0].DutyStatus != "ONDUTY" {
				t.Fatalf("decoded %s DeviceStatus = %+v", test.version, data)
			}
		})
	}

	invalid := []struct {
		name    string
		version GBProtocolVersion
		count   string
		fields  string
	}{
		{name: "2011 rejects duplicate standard spellings", version: GBVersion10, fields: `<Status>ONDUTY</Status><DutyStatus>ONDUTY</DutyStatus>`},
		{name: "2011 rejects merged supplement text", version: GBVersion10, fields: `<StatusDutyStatus>ONDUTY</StatusDutyStatus>`},
		{name: "2014 rejects deleted Status", version: GBVersion11, fields: `<Status>ONDUTY</Status>`},
		{name: "2014 rejects duplicate status spellings", version: GBVersion11, fields: `<StatusDutyStatus>ONDUTY</StatusDutyStatus><DutyStatus>ONDUTY</DutyStatus>`},
		{name: "2016 rejects legacy Status", version: GBVersion20, fields: `<Status>ONDUTY</Status>`},
		{name: "2022 rejects legacy Status", version: GBVersion30, fields: `<Status>ONDUTY</Status>`},
		{name: "2011 rejects lowercase count", version: GBVersion10, count: "num", fields: `<Status>ONDUTY</Status>`},
		{name: "2014 rejects lowercase count", version: GBVersion11, count: "num", fields: `<DutyStatus>ONDUTY</DutyStatus>`},
		{name: "2016 rejects uppercase count", version: GBVersion20, count: "Num", fields: `<DutyStatus>ONDUTY</DutyStatus>`},
		{name: "2022 rejects lowercase count", version: GBVersion30, count: "num", fields: `<DutyStatus>ONDUTY</DutyStatus>`},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			count := test.count
			if count == "" && test.version == GBVersion20 {
				count = "num"
			}
			if count == "" {
				count = "Num"
			}
			body := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Alarmstatus ` + count + `="1"><Item>` +
				`<DeviceID>` + gb10ChannelID + `</DeviceID>` + test.fields + `</Item></Alarmstatus></Response>`)
			if err := validateGenericQueryPayload(test.version, "DeviceStatus", body); err == nil {
				t.Fatal("invalid DeviceStatus alarm field was accepted")
			}
		})
	}

	t.Run("sparse optional item remains compatible", func(t *testing.T) {
		body := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID +
			`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Alarmstatus Num="1"><Item/></Alarmstatus></Response>`)
		if err := validateGenericQueryPayload(GBVersion30, "DeviceStatus", body); err != nil {
			t.Fatalf("sparse DeviceStatus alarm item rejected: %v", err)
		}
	})
}

func TestRecordQueryInputFromDeviceQueryPreservesFilters(t *testing.T) {
	streamNumber := 2
	secrecy := 1
	in := &DeviceQueryInput{
		Start: 1, End: 2, Timeout: time.Second, Type: "all",
		FilePath: "/record/front-gate.ps", Address: "front-gate", Secrecy: &secrecy, RecorderID: gb10DeviceID,
		StreamNumber: &streamNumber, AlarmMethod: "2/5", AlarmType: "2",
	}
	out := recordQueryInputFromDeviceQuery(gb10DeviceID, gb10ChannelID, in)
	if out.DeviceID != gb10DeviceID || out.ChannelID != gb10ChannelID || out.Start != 1 || out.End != 2 || out.Timeout != time.Second ||
		out.FilePath != "/record/front-gate.ps" || out.Address != "front-gate" || out.Secrecy == nil || *out.Secrecy != 1 ||
		out.Type != "all" || out.RecorderID != gb10DeviceID || out.StreamNumber == nil || *out.StreamNumber != 2 ||
		out.AlarmMethod != "2/5" || out.AlarmType != "2" {
		t.Fatalf("RecordInfo query input = %+v", out)
	}
}

func TestGenericQueryPayloadAcceptsValidBoundaryData(t *testing.T) {
	tests := []struct {
		cmdType string
		payload string
	}{
		{cmdType: "PresetQuery", payload: `<SumNum>1</SumNum><PresetList Num="1"><Item><PresetID>1</PresetID><PresetName>门口</PresetName></Item></PresetList>`},
		{cmdType: "HomePositionQuery", payload: `<HomePosition><Enabled>1</Enabled><ResetTime>0</ResetTime><PresetIndex>255</PresetIndex></HomePosition>`},
		{cmdType: "CruiseTrackListQuery", payload: `<SumNum>1</SumNum><CruiseTrackList Num="1"><CruiseTrack><Number>1</Number><Name>` + strings.Repeat("a", 32) + `</Name></CruiseTrack></CruiseTrackList>`},
		{cmdType: "CruiseTrackQuery", payload: `<Number>0</Number><SumNum>1</SumNum><CruisePointList Num="1"><CruisePoint><PresetIndex>255</PresetIndex><StayTime>4095</StayTime><Speed>15</Speed></CruisePoint></CruisePointList>`},
		{cmdType: "PTZPosition", payload: `<Pan>-3.25</Pan>`},
		{cmdType: "PTZPosition"},
		{cmdType: "SDCardStatus", payload: `<SumNum>1</SumNum><SDCardStatusInfo Num="1"><Item><ID>0</ID><HddName>card</HddName><Status>formatting</Status><FormatProgress>100</FormatProgress><Capacity>10</Capacity><FreeSpace>0</FreeSpace></Item></SDCardStatusInfo>`},
	}
	for _, test := range tests {
		t.Run(test.cmdType, func(t *testing.T) {
			body := []byte(`<Response><CmdType>` + test.cmdType + `</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID>` + test.payload + `</Response>`)
			if err := validateGenericQueryPayload(GBVersion30, test.cmdType, body); err != nil {
				t.Fatalf("valid payload rejected: %v", err)
			}
		})
	}
}

func TestGenericQueryHandlerAcceptsUnrestrictedStandardIntegerFields(t *testing.T) {
	tests := []struct {
		name     string
		cmdType  string
		payload  string
		validate func(*testing.T, *QueryState)
	}{
		{
			name:    "home reset time",
			cmdType: "HomePositionQuery",
			payload: `<HomePosition><Enabled>1</Enabled><ResetTime>-1</ResetTime></HomePosition>`,
			validate: func(t *testing.T, state *QueryState) {
				if state.HomePosition == nil || state.HomePosition.ResetTime == nil || *state.HomePosition.ResetTime != -1 {
					t.Fatalf("HomePosition state = %+v", state.HomePosition)
				}
			},
		},
		{
			name:    "cruise preset and stay time",
			cmdType: "CruiseTrackQuery",
			payload: `<Number>0</Number><SumNum>1</SumNum><CruisePointList Num="1"><CruisePoint><PresetIndex>256</PresetIndex><StayTime>4096</StayTime><Speed>15</Speed></CruisePoint></CruisePointList>`,
			validate: func(t *testing.T, state *QueryState) {
				if state.CruiseTrack == nil || len(state.CruiseTrack.Points) != 1 || state.CruiseTrack.Points[0].PresetIndex != 256 || state.CruiseTrack.Points[0].StayTime != 4096 {
					t.Fatalf("CruiseTrack state = %+v", state.CruiseTrack)
				}
			},
		},
		{
			name:    "sd free space relation",
			cmdType: "SDCardStatus",
			payload: `<SumNum>1</SumNum><SDCardStatusInfo Num="1"><Item><ID>1</ID><HddName>card</HddName><Status>ok</Status><Capacity>10</Capacity><FreeSpace>11</FreeSpace></Item></SDCardStatusInfo>`,
			validate: func(t *testing.T, state *QueryState) {
				if len(state.SDCards) != 1 || state.SDCards[0].Capacity == nil || *state.SDCards[0].Capacity != 10 || state.SDCards[0].FreeSpace == nil || *state.SDCards[0].FreeSpace != 11 {
					t.Fatalf("SDCard state = %+v", state.SDCards)
				}
			},
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(GBVersion30)
			sn := 980 + index
			pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
			api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, test.cmdType, sn), pending)
			body := []byte(fmt.Sprintf(`<Response><CmdType>%s</CmdType><SN>%d</SN><DeviceID>%s</DeviceID>%s</Response>`, test.cmdType, sn, gb10DeviceID, test.payload))

			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "query-standard-integer", body, api.sipMessageQueryGeneric)
			if !strings.Contains(response, "SIP/2.0 200") {
				t.Fatalf("valid %s response = %s", test.cmdType, response)
			}
			select {
			case <-pending.wait:
			default:
				t.Fatal("valid payload did not resolve pending query")
			}
			state, ok := api.GetQueryState(gb10DeviceID)
			if !ok {
				t.Fatal("valid payload did not update query state")
			}
			test.validate(t, state)
		})
	}
}

func TestGenericQueryPayloadAcceptsPresentEmptyPlainStringNames(t *testing.T) {
	tests := []struct {
		cmdType string
		payload string
	}{
		{cmdType: "PresetQuery", payload: `<SumNum>1</SumNum><PresetList Num="1"><Item><PresetID>1</PresetID><PresetName/></Item></PresetList>`},
		{cmdType: "SDCardStatus", payload: `<SumNum>1</SumNum><SDCardStatusInfo Num="1"><Item><ID>0</ID><HddName> </HddName><Status>ok</Status><Capacity>10</Capacity><FreeSpace>0</FreeSpace></Item></SDCardStatusInfo>`},
	}
	for _, test := range tests {
		t.Run(test.cmdType, func(t *testing.T) {
			body := []byte(`<Response><CmdType>` + test.cmdType + `</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID>` + test.payload + `</Response>`)
			if err := validateGenericQueryPayload(GBVersion30, test.cmdType, body); err != nil {
				t.Fatalf("present empty string rejected: %v", err)
			}
		})
	}
}

func TestPresetQueryLegacyVersionsDoNotRequireSumNum(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20} {
		body := []byte(`<Response><CmdType>PresetQuery</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><PresetList Num="0"/></Response>`)
		if err := validateGenericQueryPayload(version, "PresetQuery", body); err != nil {
			t.Fatalf("%s legacy PresetQuery rejected: %v", version, err)
		}
	}
}

func TestInvalidGenericQueryPayloadIsNotForwardedToSubscribers(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	notifyConn := newFlowConnection()
	api.eventSubscribers.Store("invalid-ptz", &eventSubscription{
		CmdType: "PTZPosition", DeviceID: gb10DeviceID,
		ExpiresAt: time.Now().Add(time.Minute),
		To:        mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
		Source:    notifyConn.remote, Conn: notifyConn,
	})
	body := []byte(`<Notify><CmdType>PTZPosition</CmdType><SN>7</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Pan>NaN</Pan></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "invalid-ptz-no-forward", body, api.sipMessageQueryGeneric)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("invalid PTZPosition response = %s", response)
	}
	select {
	case payload := <-notifyConn.writes:
		t.Fatalf("invalid payload was forwarded to subscriber: %s", payload)
	default:
	}
}
