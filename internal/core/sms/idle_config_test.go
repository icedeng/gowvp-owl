package sms

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestStreamNoneReaderDelayAppliesAndPreservesPreviousOnFailure(t *testing.T) {
	var applied string
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if fail {
			json.NewEncoder(w).Encode(map[string]any{"code": -1, "msg": "unavailable"})
			return
		}
		applied, _ = body["general.streamNoneReaderDelayMS"].(string)
		json.NewEncoder(w).Encode(map[string]any{"code": 0})
	}))
	defer server.Close()
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	portNumber, _ := strconv.Atoi(port)
	n := NewNodeManager(nil)
	defer n.Close()
	n.cacheServers.Store("local", &WarpMediaServer{Config: &MediaServer{
		ID: "local", Type: ProtocolZLMediaKit, IP: host, Ports: MediaServerPorts{HTTP: portNumber},
	}})
	driver := n.drivers[ProtocolZLMediaKit].(*ZLMDriver)
	if got := driver.noneReaderDelay(); got != 180 {
		t.Fatalf("default = %d", got)
	}
	if err := n.SetStreamNoneReaderDelay(240); err != nil {
		t.Fatal(err)
	}
	if applied != "240000" || driver.noneReaderDelay() != 240 {
		t.Fatalf("下发或重连配置不一致: %q, %d", applied, driver.noneReaderDelay())
	}
	fail = true
	if err := n.SetStreamNoneReaderDelay(60); err == nil || driver.noneReaderDelay() != 240 {
		t.Fatal("下发失败时未保留原设置")
	}
	if err := n.SetStreamNoneReaderDelay(0); err == nil {
		t.Fatal("接受了非法的零秒配置")
	}
}
