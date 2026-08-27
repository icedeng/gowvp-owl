package gbs

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if item.Info.PTZType != 1 || item.Info.PTZTypeList != "1" || item.Info.BusinessGroupID != "34020000002150000001" || item.Info.Resolution != "5/6" {
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

func TestCatalog30AcceptsMultiValuePTZType(t *testing.T) {
	body := []byte(`<Response><CmdType>Catalog</CmdType><SN>30</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><SecurityLevelCode>B</SecurityLevelCode><BusinessGroupID>34020000002150000001</BusinessGroupID><Info><PTZType>1/2</PTZType><PhotoelectricImagingType>1/9</PhotoelectricImagingType><CapturePositionType>0010100</CapturePositionType><SupplyLightType>9</SupplyLightType><StreamNumberList>0/1/2</StreamNumberList><DownloadSpeed>1/2/4</DownloadSpeed><SVCSpaceSupportMode>3</SVCSpaceSupportMode><SVCTimeSupportMode>2</SVCTimeSupportMode><SSVCRatioSupportList>4:3/2:1</SSVCRatioSupportList><MobileDeviceType>5</MobileDeviceType><HorizontalFieldAngle>120</HorizontalFieldAngle><VerticalFieldAngle>90</VerticalFieldAngle><MaxViewDistance>500</MaxViewDistance><GrassrootsCode>340200</GrassrootsCode><PointType>2</PointType><PointCommonName>North Gate</PointCommonName><MAC>AA-BB-CC-DD-EE-FF</MAC><FunctionType>01/99</FunctionType><EncodeType>H.265</EncodeType><InstallTime>2026-08-25T10:00:00+08:00</InstallTime><ManagementUnit>Unit A</ManagementUnit><ContactInfo>0571-12345678</ContactInfo><RecordSaveDays>30</RecordSaveDays><IndustrialClassification>A01</IndustrialClassification><VendorCapability>enabled</VendorCapability></Info></Item></DeviceList></Response>`)
	var response MessageDeviceListResponse
	if err := sip.XMLDecode(body, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Item) != 1 || response.Item[0].Info.PTZType != 1 || response.Item[0].Info.PTZTypeList != "1/2" || response.Item[0].BusinessGroupID != "34020000002150000001" {
		t.Fatalf("2022 Catalog PTZType = %+v", response.Item)
	}
	if response.Item[0].Info.StreamNumberList != "0/1/2" || response.Item[0].Info.FunctionType != "01/99" || !strings.Contains(response.Item[0].Info.RawXML, "VendorCapability") {
		t.Fatalf("2022 Catalog Info fields = %+v, raw=%q", response.Item[0].Info, response.Item[0].Info.RawXML)
	}
	if err := validateCatalogItemValues(response.Item[0], GBVersion30); err != nil {
		t.Fatalf("valid 2022 Catalog Info rejected: %v", err)
	}
	if err := validateCatalogItemValues(response.Item[0], GBVersion20); err == nil {
		t.Fatal("2016 accepted 2022 multi-value PTZType")
	}
	ext := catalogChannelExt(response.Item[0])
	if ext.GBCatalog == nil || ext.GBCatalog.PTZType != 1 || ext.GBCatalog.PTZTypeList != "1/2" || ext.GBCatalog.SecurityLevelCode != "B" || ext.GBCatalog.BusinessGroupID != response.Item[0].BusinessGroupID || ext.GBCatalog.StreamNumberList != "0/1/2" || ext.GBCatalog.RecordSaveDays != 30 {
		t.Fatalf("2022 Catalog extension = %+v", ext.GBCatalog)
	}
	for _, invalid := range []int{5, 6, 7, 8} {
		item := response.Item[0]
		item.Info.SupplyLightType = invalid
		if err := validateCatalogItemValues(item, GBVersion30); err == nil {
			t.Fatalf("2022 accepted invalid SupplyLightType %d", invalid)
		}
	}
	invalidInfo := response.Item[0]
	invalidInfo.Info.FunctionType = "06"
	if err := validateCatalogItemValues(invalidInfo, GBVersion30); err == nil {
		t.Fatal("2022 accepted invalid FunctionType")
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

	api, _ := newVersionGateAPI(GBVersion11)
	api.lifecycleClosed = true
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
		{name: "missing sum", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Response>`},
		{name: "negative sum", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>-1</SumNum></Response>`},
		{name: "invalid target", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>invalid</DeviceID><SumNum>0</SumNum></Response>`},
		{name: "unknown target", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>34020000001320000009</DeviceID><SumNum>0</SumNum></Response>`},
		{name: "missing list", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum></Response>`},
		{name: "missing list count", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList><Item><DeviceID>` + gb10ChannelID + `</DeviceID></Item></DeviceList></Response>`},
		{name: "list count mismatch", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList Num="0"><Item><DeviceID>` + gb10ChannelID + `</DeviceID></Item></DeviceList></Response>`},
		{name: "invalid item", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>bad</DeviceID></Item></DeviceList></Response>`},
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

func TestCatalogRejectsChunkOverStandardLimit(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	count := gbMultiResponseMaxItems + 1
	items := make([]Channels, count)
	for index := range items {
		items[index].ChannelID = gb10ChannelID
	}
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	msg := MessageDeviceListResponse{
		XMLName: xml.Name{Local: "Response"}, CmdType: "Catalog", SN: 10, DeviceID: gb10DeviceID,
		SumNum: count, HasSumNum: true, HasList: true, ListNum: &count, Item: items,
	}
	if err := api.validateCatalogEnvelope(&sip.Context{DeviceID: gb10DeviceID}, msg, false); err == nil {
		t.Fatal("Catalog chunk above the 10000-item standard limit was accepted")
	}
}

func TestCatalogRejectsInvalidItemValuesBeforeAggregation(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion11)
	valid := Channels{ChannelID: gb10ChannelID, Status: "ON"}
	tests := []struct {
		name   string
		mutate func(*Channels)
	}{
		{name: "status", mutate: func(item *Channels) { item.Status = "OK" }},
		{name: "parental", mutate: func(item *Channels) { item.Parental = 2 }},
		{name: "safety way", mutate: func(item *Channels) { item.SafetyWay = 1 }},
		{name: "register way", mutate: func(item *Channels) { item.RegisterWay = 4 }},
		{name: "certifiable", mutate: func(item *Channels) { item.Certifiable = 2 }},
		{name: "error code", mutate: func(item *Channels) { item.ErrCode = -1 }},
		{name: "secrecy", mutate: func(item *Channels) { item.Secrecy = 2 }},
		{name: "port", mutate: func(item *Channels) { item.Port = 65536 }},
		{name: "longitude", mutate: func(item *Channels) { item.Longitude = math.NaN() }},
		{name: "end time", mutate: func(item *Channels) { item.EndTime = "invalid" }},
		{name: "info", mutate: func(item *Channels) { item.Info.PTZType = -1 }},
		{name: "info upper bound", mutate: func(item *Channels) { item.Info.DirectionType = 9 }},
		{name: "business group", mutate: func(item *Channels) { item.Info.BusinessGroupID = "bad" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := valid
			test.mutate(&item)
			count := 1
			msg := MessageDeviceListResponse{
				XMLName: xml.Name{Local: "Response"}, CmdType: "Catalog", SN: 10, DeviceID: gb10DeviceID,
				SumNum: 1, HasSumNum: true, HasList: true, ListNum: &count, Item: []Channels{item},
			}
			if err := api.validateCatalogEnvelope(&sip.Context{DeviceID: gb10DeviceID}, msg, false); err == nil {
				t.Fatalf("invalid Catalog item accepted: %+v", item)
			}
		})
	}
}

func TestCatalog10RejectsCatalogInfo(t *testing.T) {
	for _, info := range []CatalogItemInfo{{PTZType: 1}, {RawXML: "<PTZType>0</PTZType>"}} {
		item := Channels{ChannelID: gb10ChannelID, Status: "ON", Info: info}
		if err := validateCatalogItemValues(item, GBVersion10); err == nil {
			t.Fatalf("protocol 1.0 accepted Catalog Info introduced by protocol 1.1: %+v", info)
		}
	}
}

func TestCatalog10RejectsEmptyCatalogInfoFromXML(t *testing.T) {
	body := []byte(`<?xml version="1.0"?><Response><CmdType>Catalog</CmdType><SN>10</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Info/></Item></DeviceList></Response>`)
	var msg MessageDeviceListResponse
	if err := sip.XMLDecode(body, &msg); err != nil {
		t.Fatalf("decode Catalog: %v", err)
	}
	if len(msg.Item) != 1 || msg.Item[0].Info.XMLName.Local != "Info" {
		t.Fatalf("Catalog Info presence was not retained: %+v", msg.Item)
	}
	api, _ := newVersionGateAPI(GBVersion10)
	if err := api.validateCatalogEnvelope(&sip.Context{DeviceID: gb10DeviceID}, msg, false); err == nil {
		t.Fatal("protocol 1.0 accepted empty Catalog Info introduced by protocol 1.1")
	}
}

func TestCatalogAcceptsValidOptionalItemValuesByVersion(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		item := Channels{
			ChannelID: gb10ChannelID, Status: "off", Parental: 1, RegisterWay: 3,
			Secrecy: 1, Port: 65535, Longitude: 120.1, Latitude: 30.2,
		}
		if version != GBVersion30 {
			item.SafetyWay = 4
			item.Certifiable = 1
			item.ErrCode = 1
			item.EndTime = "2026-08-25T10:00:00+08:00"
		}
		if version == GBVersion11 || version == GBVersion20 {
			item.Info = CatalogItemInfo{PTZType: 1, PositionType: 1, BusinessGroupID: gb10DeviceID}
		} else if version == GBVersion30 {
			item.Info = CatalogItemInfo{PTZType: 1}
			item.BusinessGroupID = gb10DeviceID
		}
		if err := validateCatalogItemValues(item, version); err != nil {
			t.Fatalf("valid %s Catalog item rejected: %v", version, err)
		}
	}
}

func TestCatalog30UsesVersionSpecificInfoEnums(t *testing.T) {
	for _, value := range []string{"1", "7", "1/7"} {
		if err := validateCatalogItemValues(Channels{Info: CatalogItemInfo{PTZTypeList: value}}, GBVersion30); err != nil {
			t.Fatalf("2022 PTZType %q rejected: %v", value, err)
		}
	}
	if err := validateCatalogItemValues(Channels{Info: CatalogItemInfo{PTZTypeList: "7"}}, GBVersion20); err == nil {
		t.Fatal("2016 accepted PTZType 7")
	}
	for _, value := range []int{1, 2, 3, 4, 9} {
		if err := validateCatalogItemValues(Channels{Info: CatalogItemInfo{SupplyLightType: value}}, GBVersion30); err != nil {
			t.Fatalf("2022 SupplyLightType %d rejected: %v", value, err)
		}
	}
	if err := validateCatalogItemValues(Channels{Info: CatalogItemInfo{SupplyLightType: 5}}, GBVersion30); err == nil {
		t.Fatal("2022 accepted undefined SupplyLightType 5")
	}
	for _, info := range []CatalogItemInfo{
		{CapturePositionType: "001010"}, {CapturePositionType: "001010A"},
		{GrassrootsCode: "34020"}, {GrassrootsCode: "34020A"},
	} {
		if err := validateCatalogItemValues(Channels{Info: info}, GBVersion30); err == nil {
			t.Fatalf("2022 accepted invalid position code: %+v", info)
		}
	}
	if err := validateCatalogItemValues(Channels{Info: CatalogItemInfo{CapturePositionType: "0010100", GrassrootsCode: "340200"}}, GBVersion30); err != nil {
		t.Fatalf("2022 rejected valid position codes: %v", err)
	}
}

func TestCatalogRejectsFieldsOutsideVersionSchema(t *testing.T) {
	decodeItem := func(t *testing.T, fields string) Channels {
		t.Helper()
		body := []byte(`<Response><CmdType>Catalog</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID>` + fields + `</Item></DeviceList></Response>`)
		var response MessageDeviceListResponse
		if err := sip.XMLDecode(body, &response); err != nil || len(response.Item) != 1 {
			t.Fatalf("decode Catalog item: items=%d err=%v", len(response.Item), err)
		}
		return response.Item[0]
	}

	for _, field := range []string{
		`<Owner></Owner>`, `<SafetyWay>0</SafetyWay>`, `<CertNum></CertNum>`, `<Certifiable>0</Certifiable>`,
		`<ErrCode>0</ErrCode>`, `<EndTime></EndTime>`, `<Info><PositionType>0</PositionType></Info>`,
		`<Info><UseType>0</UseType></Info>`, `<Info><BusinessGroupID></BusinessGroupID></Info>`,
	} {
		if err := validateCatalogItemValues(decodeItem(t, field), GBVersion30); err == nil {
			t.Fatalf("2022 accepted removed Catalog field %s", field)
		}
	}
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20} {
		for _, field := range []string{`<SecurityLevelCode></SecurityLevelCode>`, `<BusinessGroupID></BusinessGroupID>`} {
			if err := validateCatalogItemValues(decodeItem(t, field), version); err == nil {
				t.Fatalf("%s accepted 2022 Catalog field %s", version, field)
			}
		}
	}
}

func TestCatalogRegisterWayByVersion(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			for _, value := range []int{0, 1, 2, 3} {
				if err := validateCatalogItemValues(Channels{RegisterWay: value}, version); err != nil {
					t.Fatalf("RegisterWay %d rejected: %v", value, err)
				}
			}
			if err := validateCatalogItemValues(Channels{RegisterWay: 4}, version); (err == nil) != (version == GBVersion30) {
				t.Fatalf("RegisterWay 4 validation error = %v", err)
			}
			for _, value := range []int{-1, 5} {
				if err := validateCatalogItemValues(Channels{RegisterWay: value}, version); err == nil {
					t.Fatalf("invalid RegisterWay %d accepted", value)
				}
			}
		})
	}
}

func TestCatalogResponseRejectsSiblingPendingTarget(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	firstChannelID := gb10ChannelID
	secondChannelID := "34020000001320000003"
	memory.runtime.Channels.Store(firstChannelID, &Channel{ChannelID: firstChannelID, device: memory.runtime})
	memory.runtime.Channels.Store(secondChannelID, &Channel{ChannelID: secondChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: firstChannelID}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "Catalog", 63), pending)
	body := []byte(`<Response><CmdType>Catalog</CmdType><SN>63</SN><DeviceID>` + secondChannelID + `</DeviceID><SumNum>0</SumNum></Response>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "catalog-sibling-target", body, api.sipMessageCatalog)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("sibling Catalog response = %s", response)
	}
	select {
	case out := <-pending.wait:
		t.Fatalf("sibling Catalog response resolved pending query: %+v", out)
	default:
	}
}

func TestCatalogResponseRejectsSiblingAggregateTarget(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	secondChannelID := "34020000001320000003"
	memory.runtime.Channels.Store(secondChannelID, &Channel{ChannelID: secondChannelID, device: memory.runtime})
	collector := newMultiResponseCollector(func(item Channels) string { return item.ChannelID })
	key := buildMultiResponseKey(gb10DeviceID, "Catalog", 65)
	collector.Start(key)
	api := &GB28181API{svr: &Server{memoryStorer: memory}, catalogResponses: collector}
	body := []byte(`<Response><CmdType>Catalog</CmdType><SN>65</SN><DeviceID>` + secondChannelID + `</DeviceID><SumNum>0</SumNum></Response>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "catalog-sibling-aggregate", body, api.sipMessageCatalog)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("sibling aggregate Catalog response = %s", response)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	result := collector.Wait(waitCtx, key)
	cancel()
	if result.Complete || len(result.Items) != 0 {
		t.Fatalf("sibling Catalog response changed collector: %+v", result)
	}
}

func TestCatalogNotifyRejectsTargetAndCountBeforeRefresh(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	tests := []struct {
		name string
		body string
	}{
		{name: "source mismatch", body: `<Notify><CmdType>Catalog</CmdType><SN>64</SN><DeviceID>34020000001320000009</DeviceID><SumNum>0</SumNum></Notify>`},
		{name: "count exceeds total", body: `<Notify><CmdType>Catalog</CmdType><SN>64</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>0</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID></Item></DeviceList></Notify>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "catalog-notify-invalid-"+test.name, []byte(test.body), api.sipNotifyCatalog)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid Catalog NOTIFY response = %s", response)
			}
		})
	}
}

func TestCatalogNotifyAcceptsMultiResponseChunkCount(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	body := `<Notify><CmdType>Catalog</CmdType><SN>65</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>2</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID></Item></DeviceList></Notify>`
	var msg MessageDeviceListResponse
	if err := sip.XMLDecode([]byte(body), &msg); err != nil {
		t.Fatal(err)
	}
	if err := api.validateCatalogEnvelope(&sip.Context{DeviceID: gb10DeviceID}, msg, true); err != nil {
		t.Fatalf("valid multi-response Catalog NOTIFY chunk rejected: %v", err)
	}
}

func TestCatalogResponseAcceptsSameDomainOrganizationRootWithoutPendingQuery(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	for _, targetID := range []string{
		"34020000002000000001",
		"34020000002150000001",
		"34020000002160000001",
	} {
		body := []byte(`<Response><CmdType>Catalog</CmdType><SN>66</SN><DeviceID>` + targetID + `</DeviceID><SumNum>0</SumNum></Response>`)
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "catalog-organization-root-"+targetID, body, api.sipMessageCatalog)
		assertFlowOK(t, response)
	}
}

func TestCatalogEmptyResultListRulesByVersionAndMessageType(t *testing.T) {
	tests := []struct {
		name         string
		version      GBProtocolVersion
		root         string
		list         string
		notification bool
		wantErr      bool
	}{
		{name: "2011 response requires list", version: GBVersion10, root: "Response", wantErr: true},
		{name: "2011 response with list", version: GBVersion10, root: "Response", list: `<DeviceList Num="0"></DeviceList>`},
		{name: "2011 response rejects notify root", version: GBVersion10, root: "Notify", list: `<DeviceList Num="0"></DeviceList>`, wantErr: true},
		{name: "2011 notify uses response root", version: GBVersion10, root: "Response", list: `<DeviceList Num="0"></DeviceList>`, notification: true},
		{name: "2011 notify rejects notify root", version: GBVersion10, root: "Notify", list: `<DeviceList Num="0"></DeviceList>`, notification: true, wantErr: true},
		{name: "2014 response may omit list", version: GBVersion11, root: "Response"},
		{name: "2014 response rejects notify root", version: GBVersion11, root: "Notify", wantErr: true},
		{name: "2014 notify rejects response root", version: GBVersion11, root: "Response", list: `<DeviceList Num="0"></DeviceList>`, notification: true, wantErr: true},
		{name: "2014 notify requires list", version: GBVersion11, root: "Notify", notification: true, wantErr: true},
		{name: "2014 notify with list", version: GBVersion11, root: "Notify", list: `<DeviceList Num="0"></DeviceList>`, notification: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(test.version)
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			var msg MessageDeviceListResponse
			body := `<` + test.root + `><CmdType>Catalog</CmdType><SN>11</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>0</SumNum>` + test.list + `</` + test.root + `>`
			if err := sip.XMLDecode([]byte(body), &msg); err != nil {
				t.Fatal(err)
			}
			err := api.validateCatalogEnvelope(&sip.Context{DeviceID: gb10DeviceID}, msg, test.notification)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateCatalogEnvelope() error = %v, wantErr %v", err, test.wantErr)
			}
		})
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
