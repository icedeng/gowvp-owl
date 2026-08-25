package gbs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestCatalog11ExtensionMapping(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.1", "catalog-response.xml"))
	if err != nil {
		t.Fatal(err)
	}
	var response MessageDeviceListResponse
	if err := sip.XMLDecode(body, &response); err != nil {
		t.Fatalf("decode Catalog: %v", err)
	}
	if len(response.Item) != 1 {
		t.Fatalf("Catalog items = %d", len(response.Item))
	}
	item := response.Item[0]
	if item.ParentID != gb10DeviceID || item.IPAddress != "192.0.2.11" || item.Port != 5060 {
		t.Fatalf("Catalog network fields not decoded: %+v", item)
	}
	if item.Info.PTZType != 1 || item.Info.BusinessGroupID != "34020000002150000001" || item.Info.Resolution != "5/6" {
		t.Fatalf("Catalog Info not decoded: %+v", item.Info)
	}
	if !strings.Contains(item.RawXML, "<VendorExtension>fixture-value</VendorExtension>") ||
		!strings.Contains(item.Info.RawXML, "<VendorCapability>enabled</VendorCapability>") {
		t.Fatalf("unknown Catalog XML was not retained: item=%q info=%q", item.RawXML, item.Info.RawXML)
	}

	ext := catalogChannelExt(item)
	if ext.GBCatalog == nil || ext.GBCatalog.ParentID != item.ParentID || ext.GBCatalog.Kind != GBCatalogItemDevice {
		t.Fatalf("Catalog extension mapping = %+v", ext.GBCatalog)
	}
	encoded, err := json.Marshal(ext)
	if err != nil {
		t.Fatal(err)
	}
	var restored ipc.DeviceExt
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.GBCatalog == nil || !strings.Contains(restored.GBCatalog.RawXML, "VendorExtension") {
		t.Fatalf("Catalog extension JSON round trip lost raw XML: %s", encoded)
	}
}

func TestCatalog11TreeItemKinds(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.1", "catalog-tree-response.xml"))
	if err != nil {
		t.Fatal(err)
	}
	var response MessageDeviceListResponse
	if err := sip.XMLDecode(body, &response); err != nil {
		t.Fatal(err)
	}
	want := []GBCatalogItemKind{
		GBCatalogItemAdministrative,
		GBCatalogItemSystem,
		GBCatalogItemBusinessGroup,
		GBCatalogItemVirtualOrganization,
		GBCatalogItemDevice,
	}
	if len(response.Item) != len(want) {
		t.Fatalf("Catalog tree items = %d, want %d", len(response.Item), len(want))
	}
	for index, item := range response.Item {
		if got := classifyGBCatalogItem(item.ChannelID); got != want[index] {
			t.Errorf("classifyGBCatalogItem(%q) = %q, want %q", item.ChannelID, got, want[index])
		}
	}
	if response.Item[3].ParentID != response.Item[1].ChannelID ||
		response.Item[4].ParentID != response.Item[3].ChannelID {
		t.Fatal("Catalog parent relationships were not decoded")
	}
}

func TestCatalogNotify11AcceptsNotifyRootAndEvent(t *testing.T) {
	body := []byte(`<?xml version="1.0"?><Notify><CmdType>Catalog</CmdType><SN>62</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Event>OFF</Event></Item></DeviceList></Notify>`)
	var notify MessageDeviceListResponse
	if err := sip.XMLDecode(body, &notify); err != nil {
		t.Fatalf("decode Catalog NOTIFY: %v", err)
	}
	if notify.XMLName.Local != "Notify" || len(notify.Item) != 1 || notify.Item[0].Event != "OFF" {
		t.Fatalf("Catalog NOTIFY = %+v", notify)
	}

	api := &GB28181API{}
	conn := newFlowConnection()
	response := runFlowHandler(t, conn, api, sip.MethodNotify, "catalog-notify-1", body, api.sipNotifyCatalog)
	assertFlowOK(t, response)
}

func TestCatalogResponseRejectsInvalidEnvelopeBeforeAggregation(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion10)
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "Catalog", 9), pending)
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong command", body: `<Response><CmdType>DeviceStatus</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>0</SumNum></Response>`},
		{name: "non-positive SN", body: `<Response><CmdType>Catalog</CmdType><SN>0</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>0</SumNum></Response>`},
		{name: "missing device", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><SumNum>0</SumNum></Response>`},
		{name: "negative sum", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>-1</SumNum></Response>`},
		{name: "invalid target", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>invalid</DeviceID><SumNum>0</SumNum></Response>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "catalog-invalid-"+test.name, []byte(test.body), api.sipMessageCatalog)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid Catalog response = %s", response)
			}
		})
	}
	select {
	case out := <-pending.wait:
		t.Fatalf("invalid Catalog resolved pending query: %+v", out)
	default:
	}
}

func TestCatalogPartialResultDoesNotReplaceSnapshot(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Channels.Store("existing-channel", &Channel{ChannelID: "existing-channel", device: memory.runtime})
	api := &GB28181API{cfg: &conf.SIP{Domain: "3402000000"}, svr: &Server{memoryStorer: memory}}

	api.persistCatalogResult(gb10DeviceID, multiResponseResult[Channels]{
		Items:    []Channels{{ChannelID: "partial-channel", Status: "ON"}},
		Expected: 2,
		Complete: false,
	})
	if _, ok := memory.runtime.Channels.Load("existing-channel"); !ok {
		t.Fatal("partial Catalog removed existing channel")
	}
	if _, ok := memory.runtime.Channels.Load("partial-channel"); ok {
		t.Fatal("partial Catalog replaced snapshot")
	}

	api.persistCatalogResult(gb10DeviceID, multiResponseResult[Channels]{
		Items:    []Channels{{ChannelID: "complete-channel", Status: "ON"}},
		Expected: 1,
		Complete: true,
	})
	if _, ok := memory.runtime.Channels.Load("existing-channel"); ok {
		t.Fatal("complete Catalog kept missing channel in runtime snapshot")
	}
	if _, ok := memory.runtime.Channels.Load("complete-channel"); !ok {
		t.Fatal("complete Catalog did not replace runtime snapshot")
	}
}
