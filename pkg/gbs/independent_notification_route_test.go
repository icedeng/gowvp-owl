package gbs

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestIndependentNotificationHandlersRequireMessage(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	tests := []struct {
		cmdType string
		handler sip.HandlerFunc
	}{
		{cmdType: "DeviceUpgradeResult", handler: api.sipMessageDeviceUpgradeResult},
		{cmdType: "UploadSnapShotFinished", handler: api.sipMessageSnapshotFinished},
		{cmdType: "VideoUploadNotify", handler: api.sipMessageVideoUploadNotify},
	}
	for _, test := range tests {
		t.Run(test.cmdType, func(t *testing.T) {
			body := []byte(`<Notify><CmdType>` + test.cmdType + `</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Notify>`)
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "wrong-method-"+test.cmdType, body, test.handler)
			if !strings.Contains(response, "SIP/2.0 400") || !strings.Contains(response, "requires MESSAGE") {
				t.Fatalf("%s NOTIFY response = %s", test.cmdType, response)
			}
		})
	}
}

func TestIndependentNotificationProductionRoutesOnlyMessage(t *testing.T) {
	for _, cmdType := range []string{"DeviceUpgradeResult", "UploadSnapShotFinished", "VideoUploadNotify"} {
		t.Run(cmdType+" MESSAGE", func(t *testing.T) {
			response := sendIndependentNotificationRouteRequest(t, sip.MethodMessage, cmdType)
			if !strings.Contains(response, "SIP/2.0 400") || strings.Contains(response, "SIP/2.0 405") {
				t.Fatalf("%s MESSAGE route response = %s", cmdType, response)
			}
		})
		t.Run(cmdType+" NOTIFY", func(t *testing.T) {
			response := sendIndependentNotificationRouteRequest(t, sip.MethodNotify, cmdType)
			if !strings.Contains(response, "SIP/2.0 400 Bad Request") || strings.Contains(strings.ToLower(response), "\r\nallow:") {
				t.Fatalf("%s NOTIFY route response = %s", cmdType, response)
			}
		})
	}
}

func sendIndependentNotificationRouteRequest(t *testing.T, method, cmdType string) string {
	t.Helper()
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion20)
	api := &GB28181API{}
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	sipServer := sip.NewServer(local)
	server := &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	api.svr = server
	registerIndependentMessageNotificationRoutes(sipServer.Message(), api)

	serverConn, clientConn := net.Pipe()
	go sipServer.ProcessTCPConnection(sip.NewTCPConnection(serverConn))
	t.Cleanup(func() {
		_ = clientConn.Close()
		sipServer.Close()
	})

	body := `<Notify><CmdType>` + cmdType + `</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Notify>`
	request := fmt.Sprintf("%s sip:%s@3402000000 SIP/2.0\r\nVia: SIP/2.0/TCP 192.0.2.10:5060;branch=z9hG4bK-%s\r\nFrom: <sip:%s@3402000000>;tag=%s\r\nTo: <sip:%s@3402000000>\r\nCall-ID: independent-%s-%s\r\nCSeq: 1 %s\r\nMax-Forwards: 70\r\nContent-Type: Application/MANSCDP+xml\r\nContent-Length: %d\r\n\r\n%s",
		method, gb10PlatformID, strings.ToLower(cmdType), gb10DeviceID, strings.ToLower(cmdType), gb10PlatformID,
		strings.ToLower(method), strings.ToLower(cmdType), method, len(body), body)
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
	return string(response[:n])
}
