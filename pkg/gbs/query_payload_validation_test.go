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
		{name: "home missing enabled", cmdType: "HomePositionQuery", payload: `<HomePosition><PresetIndex>1</PresetIndex></HomePosition>`},
		{name: "home invalid enabled", cmdType: "HomePositionQuery", payload: `<HomePosition><Enabled>2</Enabled></HomePosition>`},
		{name: "home negative reset", cmdType: "HomePositionQuery", payload: `<HomePosition><Enabled>1</Enabled><ResetTime>-1</ResetTime></HomePosition>`},
		{name: "home invalid preset", cmdType: "HomePositionQuery", payload: `<HomePosition><Enabled>1</Enabled><PresetIndex>256</PresetIndex></HomePosition>`},
		{name: "cruise list missing sum", cmdType: "CruiseTrackListQuery", payload: `<CruiseTrackList Num="0"/>`},
		{name: "cruise list missing number", cmdType: "CruiseTrackListQuery", payload: `<SumNum>1</SumNum><CruiseTrackList Num="1"><CruiseTrack/></CruiseTrackList>`},
		{name: "cruise list invalid number", cmdType: "CruiseTrackListQuery", payload: `<SumNum>1</SumNum><CruiseTrackList Num="1"><CruiseTrack><Number>2</Number></CruiseTrack></CruiseTrackList>`},
		{name: "cruise list long name", cmdType: "CruiseTrackListQuery", payload: `<SumNum>1</SumNum><CruiseTrackList Num="1"><CruiseTrack><Number>0</Number><Name>` + strings.Repeat("a", 33) + `</Name></CruiseTrack></CruiseTrackList>`},
		{name: "cruise missing point speed", cmdType: "CruiseTrackQuery", payload: `<Number>0</Number><SumNum>1</SumNum><CruisePointList Num="1"><CruisePoint><PresetIndex>1</PresetIndex><StayTime>1</StayTime></CruisePoint></CruisePointList>`},
		{name: "cruise invalid speed", cmdType: "CruiseTrackQuery", payload: `<Number>0</Number><SumNum>1</SumNum><CruisePointList Num="1"><CruisePoint><PresetIndex>1</PresetIndex><StayTime>1</StayTime><Speed>16</Speed></CruisePoint></CruisePointList>`},
		{name: "ptz non finite", cmdType: "PTZPosition", payload: `<Pan>NaN</Pan>`},
		{name: "sd missing sum", cmdType: "SDCardStatus", payload: `<SDCardStatusInfo Num="0"/>`},
		{name: "sd invalid status", cmdType: "SDCardStatus", payload: `<SumNum>1</SumNum><SDCardStatusInfo Num="1"><Item><ID>1</ID><HddName>card</HddName><Status>ready</Status><Capacity>10</Capacity><FreeSpace>5</FreeSpace></Item></SDCardStatusInfo>`},
		{name: "sd invalid capacity", cmdType: "SDCardStatus", payload: `<SumNum>1</SumNum><SDCardStatusInfo Num="1"><Item><ID>1</ID><HddName>card</HddName><Status>ok</Status><Capacity>10</Capacity><FreeSpace>11</FreeSpace></Item></SDCardStatusInfo>`},
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
		version GBProtocolVersion
		count   string
		field   string
	}{
		{version: GBVersion10, count: "Num", field: "Status"},
		{version: GBVersion11, count: "Num", field: "StatusDutyStatus"},
		{version: GBVersion20, count: "num", field: "DutyStatus"},
		{version: GBVersion30, count: "Num", field: "DutyStatus"},
	}
	for _, test := range tests {
		t.Run(string(test.version), func(t *testing.T) {
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
			wrong := strings.Replace(string(body), "<"+test.field+">ONDUTY</"+test.field+">", "<DutyStatus>ONDUTY</DutyStatus>", 1)
			if test.field != "DutyStatus" && validateGenericQueryPayload(test.version, "DeviceStatus", []byte(wrong)) == nil {
				t.Fatalf("%s accepted DutyStatus from another protocol profile", test.version)
			}
		})
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
		{cmdType: "CruiseTrackQuery", payload: `<Number>0</Number><SumNum>1</SumNum><CruisePointList Num="1"><CruisePoint><PresetIndex>0</PresetIndex><StayTime>0</StayTime><Speed>15</Speed></CruisePoint></CruisePointList>`},
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
