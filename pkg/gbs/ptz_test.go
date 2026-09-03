package gbs

import (
	"errors"
	"strings"
	"testing"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestDeviceControlResponseRejectsInvalidEnvelopeBeforeWait(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	pending := &pendingDeviceControl{wait: make(chan *deviceControlResponse, 1), targetID: gb10ChannelID}
	api.pendingDeviceControl.Store(gb10DeviceID+":61", pending)
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong root", body: `<Notify><CmdType>DeviceControl</CmdType><SN>61</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result></Notify>`},
		{name: "wrong command", body: `<Response><CmdType>DeviceConfig</CmdType><SN>61</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "non-positive SN", body: `<Response><CmdType>DeviceControl</CmdType><SN>0</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "invalid result", body: `<Response><CmdType>DeviceControl</CmdType><SN>61</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>SUCCESS</Result></Response>`},
		{name: "unknown target", body: `<Response><CmdType>DeviceControl</CmdType><SN>61</SN><DeviceID>34020000001320000009</DeviceID><Result>OK</Result></Response>`},
		{name: "other owned target", body: `<Response><CmdType>DeviceControl</CmdType><SN>61</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "duplicate result", body: `<Response><CmdType>DeviceControl</CmdType><SN>61</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>ERROR</Result><Result>OK</Result></Response>`},
		{name: "unknown field", body: `<Response><CmdType>DeviceControl</CmdType><SN>61</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result><VendorField>value</VendorField></Response>`},
		{name: "out of order", body: `<Response><CmdType>DeviceControl</CmdType><DeviceID>` + gb10ChannelID + `</DeviceID><SN>61</SN><Result>OK</Result></Response>`},
		{name: "nested result", body: `<Response><CmdType>DeviceControl</CmdType><SN>61</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result><Value>OK</Value></Result></Response>`},
		{name: "root attribute", body: `<Response vendor="x"><CmdType>DeviceControl</CmdType><SN>61</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result></Response>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "device-control-invalid-"+test.name, []byte(test.body), api.sipMessageDeviceControl)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid DeviceControl response = %s", response)
			}
		})
	}
	select {
	case result := <-pending.wait:
		t.Fatalf("invalid DeviceControl resolved pending request: %+v", result)
	default:
	}
}

func TestDeviceControlResponseWithoutPendingIsAcknowledged(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	body := `<Response><CmdType>DeviceControl</CmdType><SN>62</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result></Response>`
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "device-control-late-response", []byte(body), api.sipMessageDeviceControl)
	if !strings.Contains(response, "SIP/2.0 200") {
		t.Fatalf("late DeviceControl response = %s", response)
	}
	if countSyncMap(&api.pendingDeviceControl) != 0 {
		t.Fatal("late DeviceControl response created pending state")
	}
}

func TestDeviceControlResponseCompletesOnlyAfterSuccessfulSIPOK(t *testing.T) {
	tests := []struct {
		name     string
		writeErr error
		complete bool
	}{
		{name: "success", complete: true},
		{name: "write failure", writeErr: errors.New("write failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			pending := &pendingDeviceControl{wait: make(chan *deviceControlResponse, 1), targetID: gb10ChannelID}
			api.pendingDeviceControl.Store(gb10DeviceID+":63", pending)
			body := []byte(`<Response><CmdType>DeviceControl</CmdType><SN>63</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result></Response>`)
			conn, done := startBlockingFlowHandler(t, api, sip.MethodMessage, "device-control-commit-"+test.name, body, api.sipMessageDeviceControl, test.writeErr)
			completedBeforeSIP := false
			select {
			case <-pending.wait:
				completedBeforeSIP = true
			default:
			}
			finishBlockingFlowHandler(t, conn, done)
			if completedBeforeSIP {
				t.Fatal("DeviceControl response completed before SIP 200 was written")
			}
			completed := false
			select {
			case <-pending.wait:
				completed = true
			default:
			}
			if completed != test.complete {
				t.Fatalf("DeviceControl completed = %v, want %v", completed, test.complete)
			}
		})
	}
}

func TestDeviceControlRequestUsesStandardExtraInfoElement(t *testing.T) {
	body, err := sip.XMLEncode(deviceControlRequest{
		CmdType:   ptzCmdTypeDeviceControl,
		SN:        1,
		DeviceID:  gb10ChannelID,
		PTZCmd:    "A50F0100000000B5",
		ExtraInfo: "vendor",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "<ExtraInfo>vendor</ExtraInfo>") || strings.Contains(text, "<ExtralInfo>") {
		t.Fatalf("DeviceControl ExtraInfo element = %s", text)
	}
}

func TestEncodePTZCommandZoomBits(t *testing.T) {
	tests := []struct {
		name string
		in   *PTZInput
		want string
	}{
		{
			name: "zoom in uses bit5",
			in: &PTZInput{
				Action: PTZActionZoomIn,
				Speed:  40,
			},
			want: "A50F01202828F015",
		},
		{
			name: "zoom out uses bit4",
			in: &PTZInput{
				Action: PTZActionZoomOut,
				Speed:  40,
			},
			want: "A50F01102828F005",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := encodePTZCommand(tt.in)
			if err != nil {
				t.Fatalf("encodePTZCommand() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("encodePTZCommand() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNormalizeZoomSpeed(t *testing.T) {
	tests := []struct {
		name  string
		speed uint8
		want  uint8
	}{
		{name: "stop remains zero", speed: 0, want: 0},
		{name: "protocol range unchanged", speed: 8, want: 8},
		{name: "large api speed clamps to four bit max", speed: 40, want: 15},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeZoomSpeed(tt.speed); got != tt.want {
				t.Fatalf("normalizeZoomSpeed(%d) = %d, want %d", tt.speed, got, tt.want)
			}
		})
	}
}
