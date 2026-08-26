package gbs

import (
	"context"
	"encoding/xml"
	"strings"
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestKeepaliveRejectsInvalidEnvelopeBeforeLoadingOrState(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong root", body: `<Response><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Response>`},
		{name: "wrong command", body: `<Notify><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`},
		{name: "non-positive SN", body: `<Notify><CmdType>Keepalive</CmdType><SN>0</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`},
		{name: "device mismatch", body: `<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>34020000001320000009</DeviceID><Status>OK</Status></Notify>`},
		{name: "invalid status", body: `<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>ONLINE</Status></Notify>`},
		{name: "invalid fault device", body: `<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status><Info><DeviceID>bad</DeviceID></Info></Notify>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := &flowMemory{persistent: &ipc.Device{DeviceID: gb10DeviceID}}
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "keepalive-invalid-"+test.name, []byte(test.body), api.sipMessageKeepalive)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid Keepalive response = %s", response)
			}
			if memory.runtime != nil || memory.persistent.IsOnline || !memory.persistent.KeepaliveAt.IsZero() {
				t.Fatalf("invalid Keepalive changed device state: persistent=%+v runtime=%+v", memory.persistent, memory.runtime)
			}
			if state, ok := api.GetQueryState(gb10DeviceID); ok && state.DeviceStatus != nil {
				t.Fatalf("invalid Keepalive changed query state: %+v", state.DeviceStatus)
			}
		})
	}
}

func TestKeepalivePreservesDocumentedVendorStatusCompatibility(t *testing.T) {
	for _, status := range []string{"", "ON", "OFF"} {
		memory := newFlowMemory(gb10DeviceID)
		api := &GB28181API{svr: &Server{memoryStorer: memory}}
		body := `<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID>`
		if status != "" {
			body += `<Status>` + status + `</Status>`
		}
		body += `</Notify>`
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "keepalive-compatible-"+status, []byte(body), api.sipMessageKeepalive)
		assertFlowOK(t, response)
		if memory.persistent.IsOnline != (status == "" || status == "ON") {
			t.Fatalf("Keepalive status %q online = %v", status, memory.persistent.IsOnline)
		}
	}
}

func TestAlarmRejectsInvalidEnvelopeBeforeStateAndCallback(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	callbacks := make(chan *AlarmEvent, 1)
	api.SetAlarmHandler(func(context.Context, *AlarmEvent) { callbacks <- &AlarmEvent{} })
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong root", body: `<Response><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Response>`},
		{name: "wrong command", body: `<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Notify>`},
		{name: "non-positive SN", body: `<Notify><CmdType>Alarm</CmdType><SN>0</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Notify>`},
		{name: "unknown target", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>34020000001320000009</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Notify>`},
		{name: "invalid priority", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>5</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Notify>`},
		{name: "invalid method", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>8</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Notify>`},
		{name: "invalid time", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>bad</AlarmTime></Notify>`},
		{name: "invalid coordinate", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><Longitude>NaN</Longitude></Notify>`},
		{name: "longitude out of range", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><Longitude>181</Longitude></Notify>`},
		{name: "latitude out of range", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><Latitude>-91</Latitude></Notify>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "alarm-invalid-"+test.name, []byte(test.body), api.sipMessageAlarm)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid Alarm response = %s", response)
			}
		})
	}
	select {
	case event := <-callbacks:
		t.Fatalf("invalid Alarm invoked callback: %+v", event)
	default:
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && len(state.AppendixA4) > 0 {
		t.Fatalf("invalid Alarm changed Appendix A.4 state: %+v", state.AppendixA4)
	}
}

func TestAlarmTypeAndEventTypeRulesByVersion(t *testing.T) {
	tests := []struct {
		name      string
		version   GBProtocolVersion
		method    string
		alarmType string
		eventType *int
		wantErr   bool
	}{
		{name: "2011 ignores later type extension", version: GBVersion10, method: "2", alarmType: "vendor"},
		{name: "2016 device alarm boundary", version: GBVersion20, method: "2", alarmType: "5"},
		{name: "2016 invalid device alarm type", version: GBVersion20, method: "2", alarmType: "6", wantErr: true},
		{name: "2016 video alarm boundary", version: GBVersion20, method: "5", alarmType: "12"},
		{name: "2016 rejects 2022 video content type", version: GBVersion20, method: "5", alarmType: "13", wantErr: true},
		{name: "2022 video content type", version: GBVersion30, method: "5", alarmType: "13"},
		{name: "type requires typed method", version: GBVersion30, method: "1", alarmType: "1", wantErr: true},
		{name: "intrusion entry event", version: GBVersion20, method: "5", alarmType: "6", eventType: intPointer(1)},
		{name: "intrusion exit event", version: GBVersion30, method: "5", alarmType: "6", eventType: intPointer(2)},
		{name: "invalid intrusion event", version: GBVersion30, method: "5", alarmType: "6", eventType: intPointer(3), wantErr: true},
		{name: "event on non-intrusion alarm", version: GBVersion30, method: "5", alarmType: "5", eventType: intPointer(1), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(test.version)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			msg := &messageAlarm{
				XMLName: xml.Name{Local: "Notify"}, CmdType: "Alarm", SN: 1, DeviceID: gb10ChannelID,
				AlarmPriority: "1", AlarmMethod: test.method, AlarmTime: "2026-08-26T01:00:00",
			}
			msg.Info.AlarmType = test.alarmType
			msg.Info.AlarmTypeParam.EventType = test.eventType
			err := api.validateAlarmEnvelope(&sip.Context{DeviceID: gb10DeviceID}, msg)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateAlarmEnvelope() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestMediaStatusRejectsInvalidEnvelopeAndTargetBeforeSessionStop(t *testing.T) {
	streams := &conc.Map[string, *Streams]{}
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "media-status-invalid"}
	streams.Store(key, stream)
	api := &GB28181API{streams: streams}
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong root", body: `<Response><CmdType>MediaStatus</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Response>`},
		{name: "wrong command", body: `<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Notify>`},
		{name: "non-positive SN", body: `<Notify><CmdType>MediaStatus</CmdType><SN>0</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Notify>`},
		{name: "missing type", body: `<Notify><CmdType>MediaStatus</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID></Notify>`},
		{name: "unknown target", body: `<Notify><CmdType>MediaStatus</CmdType><SN>1</SN><DeviceID>34020000001320000009</DeviceID><NotifyType>121</NotifyType></Notify>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "media-status-invalid", []byte(test.body), api.sipMessageMediaStatus)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid MediaStatus response = %s", response)
			}
			if current, ok := streams.Load(key); !ok || current != stream || stream.Stop {
				t.Fatal("invalid MediaStatus stopped history session")
			}
		})
	}
}
