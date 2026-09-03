package gbs

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
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
	body := []byte(`<Response><CmdType>Catalog</CmdType><SN>30</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Name>Camera</Name><Manufacturer>Vendor</Manufacturer><Model>Model</Model><CivilCode>340200</CivilCode><Address>North Gate</Address><Parental>0</Parental><ParentID>` + gb10DeviceID + `</ParentID><RegisterWay>1</RegisterWay><Secrecy>0</Secrecy><Status>ON</Status><Longitude>120</Longitude><Latitude>30</Latitude><SecurityLevelCode>B</SecurityLevelCode><BusinessGroupID>34020000002150000001</BusinessGroupID><Info><PTZType>1/2</PTZType><PhotoelectricImagingType>1/9</PhotoelectricImagingType><CapturePositionType>0010100</CapturePositionType><SupplyLightType>9</SupplyLightType><StreamNumberList>0/1/2</StreamNumberList><DownloadSpeed>1/2/4</DownloadSpeed><SVCSpaceSupportMode>3</SVCSpaceSupportMode><SVCTimeSupportMode>2</SVCTimeSupportMode><SSVCRatioSupportList>4:3/2:1</SSVCRatioSupportList><MobileDeviceType>5</MobileDeviceType><HorizontalFieldAngle>120</HorizontalFieldAngle><VerticalFieldAngle>90</VerticalFieldAngle><MaxViewDistance>500</MaxViewDistance><GrassrootsCode>340200</GrassrootsCode><PointType>2</PointType><PointCommonName>North Gate</PointCommonName><MAC>AA-BB-CC-DD-EE-FF</MAC><FunctionType>01/99</FunctionType><EncodeType>H.265</EncodeType><InstallTime>2026-08-25T10:00:00+08:00</InstallTime><ManagementUnit>Unit A</ManagementUnit><ContactInfo>0571-12345678</ContactInfo><RecordSaveDays>30</RecordSaveDays><IndustrialClassification>A01</IndustrialClassification><VendorCapability>enabled</VendorCapability></Info></Item></DeviceList></Response>`)
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

func TestCatalog30InfoPreservesHumanReadableWhitespace(t *testing.T) {
	body := []byte(`<Info><PTZType> 1/2 </PTZType><PhotoelectricImagingType> 1/9 </PhotoelectricImagingType>` +
		`<StreamNumberList> 0/1/2 </StreamNumberList><GrassrootsCode> 340200 </GrassrootsCode>` +
		`<PointCommonName>  North Gate  </PointCommonName><MAC> AA-BB-CC-DD-EE-FF </MAC>` +
		`<FunctionType> 01/99 </FunctionType><ManagementUnit>  Unit A  </ManagementUnit>` +
		`<ContactInfo>  0571-12345678  </ContactInfo></Info>`)
	var info CatalogItemInfo
	if err := sip.XMLDecode(body, &info); err != nil {
		t.Fatal(err)
	}
	if info.PointCommonName != "  North Gate  " || info.ManagementUnit != "  Unit A  " || info.ContactInfo != "  0571-12345678  " {
		t.Fatalf("human-readable Catalog Info fields = %+v", info)
	}
	if info.PTZTypeList != "1/2" || info.PhotoelectricImagingType != "1/9" || info.StreamNumberList != "0/1/2" || info.GrassrootsCode != "340200" || info.MAC != "AA-BB-CC-DD-EE-FF" || info.FunctionType != "01/99" {
		t.Fatalf("normalized Catalog Info code fields = %+v", info)
	}
}

func TestCatalog30RequiresSchemaFieldsByItemKind(t *testing.T) {
	decodeItem := func(t *testing.T, body string) Channels {
		t.Helper()
		var item Channels
		if err := sip.XMLDecode([]byte(body), &item); err != nil {
			t.Fatalf("decode Catalog item: %v", err)
		}
		return item
	}
	remove := func(body, element string) string {
		start := strings.Index(body, "<"+element+">")
		endTag := "</" + element + ">"
		end := strings.Index(body, endTag)
		if start < 0 || end < start {
			return body
		}
		return body[:start] + body[end+len(endTag):]
	}

	validCamera := `<Item><DeviceID>` + gb10ChannelID + `</DeviceID><Name>Camera</Name><Manufacturer>Vendor</Manufacturer><Model>Model</Model><CivilCode>340200</CivilCode><Address>Address</Address><Parental>0</Parental><ParentID>` + gb10DeviceID + `</ParentID><RegisterWay>1</RegisterWay><Secrecy>0</Secrecy><Status>ON</Status><Longitude>0</Longitude><Latitude>0</Latitude><Info><GrassrootsCode>000000</GrassrootsCode><PointType>2</PointType></Info></Item>`
	if err := validateCatalogItemValues(decodeItem(t, validCamera), GBVersion30); err != nil {
		t.Fatalf("valid protocol 3.0 camera rejected: %v", err)
	}
	for _, element := range []string{"Name", "Manufacturer", "Model", "CivilCode", "Address", "Parental", "ParentID", "RegisterWay", "Secrecy", "Status", "Longitude", "Latitude", "GrassrootsCode", "PointType"} {
		t.Run("camera_missing_"+element, func(t *testing.T) {
			if err := validateCatalogItemValues(decodeItem(t, remove(validCamera, element)), GBVersion30); err == nil {
				t.Fatalf("protocol 3.0 camera accepted missing %s", element)
			}
		})
	}
	// 附录 J 的 132 设备目录项示例只列出顶层必填字段，允许整个 Info 缺省。
	if err := validateCatalogItemValues(decodeItem(t, remove(validCamera, "Info")), GBVersion30); err != nil {
		t.Fatalf("protocol 3.0 Annex J camera without optional Info rejected: %v", err)
	}
	multiCamera := strings.Replace(remove(validCamera, "Info"), gb10ChannelID, "34020000001220000001", 1)
	if err := validateCatalogItemValues(decodeItem(t, multiCamera), GBVersion30); err != nil {
		t.Fatalf("protocol 3.0 multi-camera device rejected as camera: %v", err)
	}

	classOne := strings.Replace(validCamera, "<PointType>2</PointType>", "<PointType>1</PointType><InstallTime>2026-08-28T10:00:00+08:00</InstallTime><ContactInfo>0571-12345678</ContactInfo><RecordSaveDays>0</RecordSaveDays>", 1)
	if err := validateCatalogItemValues(decodeItem(t, classOne), GBVersion30); err != nil {
		t.Fatalf("valid protocol 3.0 class I camera rejected: %v", err)
	}
	unrestrictedIntegers := strings.Replace(classOne, "<GrassrootsCode>", "<MaxViewDistance>-1.5</MaxViewDistance><GrassrootsCode>", 1)
	unrestrictedIntegers = strings.Replace(unrestrictedIntegers, "<RecordSaveDays>0</RecordSaveDays>", "<RecordSaveDays>-1</RecordSaveDays>", 1)
	if err := validateCatalogItemValues(decodeItem(t, unrestrictedIntegers), GBVersion30); err != nil {
		t.Fatalf("protocol 3.0 Catalog fields with unrestricted standard numeric ranges rejected: %v", err)
	}
	for _, element := range []string{"InstallTime", "ContactInfo", "RecordSaveDays"} {
		t.Run("class_one_missing_"+element, func(t *testing.T) {
			if err := validateCatalogItemValues(decodeItem(t, remove(classOne, element)), GBVersion30); err == nil {
				t.Fatalf("protocol 3.0 class I camera accepted missing %s", element)
			}
		})
	}
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "empty", body: strings.Replace(classOne, "<ContactInfo>0571-12345678</ContactInfo>", "<ContactInfo/>", 1)},
		{name: "whitespace", body: strings.Replace(classOne, "<ContactInfo>0571-12345678</ContactInfo>", "<ContactInfo>   </ContactInfo>", 1), want: "   "},
	} {
		t.Run("class_one_contact_info_"+test.name, func(t *testing.T) {
			item := decodeItem(t, test.body)
			if err := validateCatalogItemValues(item, GBVersion30); err != nil {
				t.Fatalf("protocol 3.0 class I camera rejected present ordinary string ContactInfo: %v", err)
			}
			if item.Info.ContactInfo != test.want || !item.Info.hasContactInfo {
				t.Fatalf("Catalog ContactInfo = %q, present = %v; want %q, true", item.Info.ContactInfo, item.Info.hasContactInfo, test.want)
			}
		})
	}

	items := []struct {
		name     string
		body     string
		required []string
	}{
		{name: "administrative", body: `<Item><DeviceID>11</DeviceID><Name>北京市</Name></Item>`, required: []string{"Name"}},
		{name: "system", body: `<Item><DeviceID>11010100002000000001</DeviceID><Name>平台</Name><Manufacturer>Vendor</Manufacturer><Model>Model</Model><CivilCode>110101</CivilCode><Address>Address</Address><RegisterWay>1</RegisterWay><Secrecy>0</Secrecy><Status>ON</Status></Item>`, required: []string{"Name", "Manufacturer", "Model", "CivilCode", "Address", "RegisterWay", "Secrecy", "Status"}},
		{name: "business_group", body: `<Item><DeviceID>11010100002150000001</DeviceID><Name>业务分组</Name><CivilCode>110101</CivilCode><ParentID>11010100002000000001</ParentID></Item>`, required: []string{"Name", "CivilCode", "ParentID"}},
		{name: "virtual_organization", body: `<Item><DeviceID>11010100002160000001</DeviceID><Name>虚拟组织</Name><BusinessGroupID>11010100002150000001</BusinessGroupID></Item>`, required: []string{"Name", "BusinessGroupID"}},
	}
	for _, test := range items {
		t.Run(test.name, func(t *testing.T) {
			if err := validateCatalogItemValues(decodeItem(t, test.body), GBVersion30); err != nil {
				t.Fatalf("valid protocol 3.0 %s rejected: %v", test.name, err)
			}
			for _, element := range test.required {
				if err := validateCatalogItemValues(decodeItem(t, remove(test.body, element)), GBVersion30); err == nil {
					t.Fatalf("protocol 3.0 %s accepted missing %s", test.name, element)
				}
			}
		})
	}
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "empty_name", body: `<Item><DeviceID>340200</DeviceID><Name/></Item>`},
		{name: "whitespace_name", body: `<Item><DeviceID>340200</DeviceID><Name>   </Name></Item>`, want: "   "},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := decodeItem(t, test.body)
			if err := validateCatalogItemValues(item, GBVersion30); err != nil {
				t.Fatalf("protocol 3.0 Catalog rejected present ordinary string Name: %v", err)
			}
			if item.Name != test.want || !item.hasName {
				t.Fatalf("Catalog Name = %q, hasName = %v; want %q, true", item.Name, item.hasName, test.want)
			}
		})
	}
	for _, element := range []string{"Manufacturer", "Model", "Address"} {
		t.Run("camera_empty_"+element, func(t *testing.T) {
			body := strings.Replace(validCamera, ">"+map[string]string{
				"Manufacturer": "Vendor", "Model": "Model", "Address": "Address",
			}[element]+"</"+element+">", `> </`+element+`>`, 1)
			item := decodeItem(t, body)
			if err := validateCatalogItemValues(item, GBVersion30); err != nil {
				t.Fatalf("protocol 3.0 Catalog rejected present ordinary string %s: %v", element, err)
			}
			switch element {
			case "Manufacturer":
				if item.Manufacturer != " " || !item.hasManufacturer {
					t.Fatalf("Catalog Manufacturer = %q, present = %v", item.Manufacturer, item.hasManufacturer)
				}
			case "Model":
				if item.Model != " " || !item.hasModel {
					t.Fatalf("Catalog Model = %q, present = %v", item.Model, item.hasModel)
				}
			case "Address":
				if item.Address != " " || !item.hasAddress {
					t.Fatalf("Catalog Address = %q, present = %v", item.Address, item.hasAddress)
				}
			}
		})
	}
	invalidVirtualParent := strings.Replace(items[3].body, "</Item>", "<ParentID>invalid</ParentID></Item>", 1)
	if err := validateCatalogItemValues(decodeItem(t, invalidVirtualParent), GBVersion30); err == nil {
		t.Fatal("protocol 3.0 virtual organization accepted invalid ParentID")
	}
	invalidSystemParent := strings.Replace(items[1].body, "</Item>", "<ParentID>invalid</ParentID></Item>", 1)
	if err := validateCatalogItemValues(decodeItem(t, invalidSystemParent), GBVersion30); err == nil {
		t.Fatal("protocol 3.0 system accepted invalid ParentID")
	}
	nonCameraInfo := strings.Replace(items[1].body, "</Item>", "<Info><PTZType>1</PTZType></Info></Item>", 1)
	if err := validateCatalogItemValues(decodeItem(t, nonCameraInfo), GBVersion30); err == nil {
		t.Fatal("protocol 3.0 Catalog Info accepted missing GrassrootsCode")
	}

	legacyMinimal := Channels{ChannelID: gb10ChannelID}
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20} {
		if err := validateCatalogItemValues(legacyMinimal, version); err != nil {
			t.Fatalf("legacy version %s rejected previously compatible sparse item: %v", version, err)
		}
	}
}

func TestCatalog30HandlerAcceptsPresentEmptyRequiredPlainStrings(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	body := []byte(`<Response><CmdType>Catalog</CmdType><SN>301</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID +
		`</DeviceID><Name/><Manufacturer/><Model>   </Model><CivilCode>340200</CivilCode><Address/>` +
		`<Parental>0</Parental><ParentID>` + gb10DeviceID +
		`</ParentID><RegisterWay>1</RegisterWay><Secrecy>0</Secrecy><Status>ON</Status>` +
		`<Longitude>0</Longitude><Latitude>0</Latitude><Info><GrassrootsCode>000000</GrassrootsCode>` +
		`<PointType>1</PointType><InstallTime>2026-08-28T10:00:00+08:00</InstallTime>` +
		`<ContactInfo/><RecordSaveDays>0</RecordSaveDays></Info></Item></DeviceList></Response>`)

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
		"catalog-present-empty-required-strings", body, api.sipMessageCatalog)
	assertFlowOK(t, response)
}

func TestCatalogHandlerRejectsExplicitInvalidTypedZero(t *testing.T) {
	legacyAPI, _ := newVersionGateAPI(GBVersion11)
	legacyBody := []byte(`<Response><CmdType>Catalog</CmdType><SN>302</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID +
		`</DeviceID><RegisterWay>0</RegisterWay></Item></DeviceList></Response>`)
	legacyResponse := runFlowHandler(t, newFlowConnection(), legacyAPI, sip.MethodMessage,
		"catalog-explicit-zero-register-way", legacyBody, legacyAPI.sipMessageCatalog)
	if !strings.Contains(legacyResponse, "SIP/2.0 400") {
		t.Fatalf("protocol 1.1 explicit RegisterWay=0 response = %s", legacyResponse)
	}

	modernAPI, _ := newVersionGateAPI(GBVersion30)
	modernBody := []byte(`<Response><CmdType>Catalog</CmdType><SN>303</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>34020000002000000001</DeviceID>` +
		`<Name>Platform</Name><Manufacturer>Vendor</Manufacturer><Model>Model</Model><CivilCode>340200</CivilCode>` +
		`<Address>Address</Address><RegisterWay>1</RegisterWay><Secrecy>0</Secrecy><Status>ON</Status>` +
		`<Info><MobileDeviceType>0</MobileDeviceType><GrassrootsCode>000000</GrassrootsCode></Info>` +
		`</Item></DeviceList></Response>`)
	modernResponse := runFlowHandler(t, newFlowConnection(), modernAPI, sip.MethodMessage,
		"catalog-explicit-zero-mobile-type", modernBody, modernAPI.sipMessageCatalog)
	if !strings.Contains(modernResponse, "SIP/2.0 400") {
		t.Fatalf("protocol 3.0 explicit MobileDeviceType=0 response = %s", modernResponse)
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
	if !strings.Contains(response, "Content-Length: 0") || strings.Contains(response, "<Response>") {
		t.Fatalf("2014 Catalog NOTIFY response should have an empty body:\n%s", response)
	}
}

func TestCatalogNotifyUsesVersionedTopLevelExtensions(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		tail    string
		wantOK  bool
	}{
		{name: "2014 status and Info", version: GBVersion11, tail: `<Info>vendor</Info>`, wantOK: true},
		{name: "2014 rejects ExtraInfo", version: GBVersion11, tail: `<ExtraInfo>vendor</ExtraInfo>`},
		{name: "2022 ExtraInfo", version: GBVersion30, tail: `<ExtraInfo>vendor</ExtraInfo>`, wantOK: true},
		{name: "2022 rejects legacy Info", version: GBVersion30, tail: `<Info>vendor</Info>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(test.version)
			api.lifecycleClosed = true
			body := `<Notify><CmdType>Catalog</CmdType><SN>63</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status><SumNum>0</SumNum><DeviceList Num="0"></DeviceList>` + test.tail + `</Notify>`
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "catalog-notify-versioned-"+test.name, []byte(body), api.sipNotifyCatalog)
			if test.wantOK {
				assertFlowOK(t, response)
			} else if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("cross-version Catalog NOTIFY response = %s", response)
			}
		})
	}
}

func TestCatalogNotifyRejectsDuplicateItemEvent(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion11)
	body := `<Notify><CmdType>Catalog</CmdType><SN>64</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Event>OFF</Event><Event>ON</Event></Item></DeviceList></Notify>`
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "catalog-notify-duplicate-event", []byte(body), api.sipNotifyCatalog)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("duplicate Catalog NOTIFY Event response = %s", response)
	}
}

func TestCatalogNotifyItemEventVersionMatrix(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		root    string
		event   string
		wantErr bool
	}{
		{name: "2011 query response item", version: GBVersion10, root: "Response"},
		{name: "2011 rejects later Event", version: GBVersion10, root: "Response", event: "OFF", wantErr: true},
		{name: "2014 requires Event", version: GBVersion11, root: "Notify", wantErr: true},
		{name: "2014 valid Event", version: GBVersion11, root: "Notify", event: "OFF"},
		{name: "2016 requires Event", version: GBVersion20, root: "Notify", wantErr: true},
		{name: "2016 valid Event", version: GBVersion20, root: "Notify", event: "UPDATE"},
		{name: "2022 requires Event", version: GBVersion30, root: "Notify", wantErr: true},
		{name: "2022 valid Event", version: GBVersion30, root: "Notify", event: "ADD"},
		{name: "2022 rejects empty Event", version: GBVersion30, root: "Notify", event: " ", wantErr: true},
		{name: "2022 rejects unknown Event", version: GBVersion30, root: "Notify", event: "READY", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := ""
			if test.event != "" {
				event = "<Event>" + test.event + "</Event>"
			}
			itemTail := ""
			if test.version == GBVersion30 {
				itemTail = `<Name>camera</Name><Manufacturer>vendor</Manufacturer><Model>model</Model><CivilCode>340200</CivilCode>` +
					`<Address>gate</Address><Parental>0</Parental><ParentID>` + gb10DeviceID +
					`</ParentID><RegisterWay>1</RegisterWay><Secrecy>0</Secrecy><Status>ON</Status>`
			}
			body := []byte("<" + test.root + "><CmdType>Catalog</CmdType><SN>1</SN><DeviceID>" + gb10DeviceID +
				"</DeviceID><SumNum>1</SumNum><DeviceList Num=\"1\"><Item><DeviceID>" + gb10ChannelID + "</DeviceID>" + event +
				itemTail + "</Item></DeviceList></" + test.root + ">")

			err := validateCatalogStructure(body, test.version, true)
			if err == nil {
				var msg MessageDeviceListResponse
				if decodeErr := sip.XMLDecode(body, &msg); decodeErr != nil {
					t.Fatalf("decode Catalog notification: %v", decodeErr)
				}
				api, _ := newVersionGateAPI(test.version)
				err = api.validateCatalogEnvelope(&sip.Context{DeviceID: gb10DeviceID}, msg, true)
			}
			if (err != nil) != test.wantErr {
				t.Fatalf("Catalog notification validation error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestCatalogNotifyEventValues(t *testing.T) {
	for _, event := range []string{"ON", "OFF", "VLOST", "DEFECT", "ADD", "DEL", "UPDATE"} {
		if !validCatalogNotifyEvent(event) {
			t.Errorf("standard Catalog notification Event %q was rejected", event)
		}
	}
	for _, event := range []string{"", " ", "READY", "ONLINE", "DELETE"} {
		if validCatalogNotifyEvent(event) {
			t.Errorf("invalid Catalog notification Event %q was accepted", event)
		}
	}
}

func TestCatalogResponseRejectsInvalidEnvelopeBeforeAggregation(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion10)
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "Catalog", 9), pending)
	collector := newMultiResponseCollector(func(item Channels) string { return item.ChannelID })
	collectorKey := buildMultiResponseKey(gb10DeviceID, "Catalog", 9)
	collector.Start(collectorKey)
	api.catalogResponses = collector
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
		{name: "duplicate sum", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><SumNum>0</SumNum></Response>`},
		{name: "unknown top-level field", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>0</SumNum><Vendor>1</Vendor></Response>`},
		{name: "top-level field out of order", body: `<Response><CmdType>Catalog</CmdType><DeviceID>` + gb10DeviceID + `</DeviceID><SN>9</SN><SumNum>0</SumNum></Response>`},
		{name: "root attribute", body: `<Response vendor="1"><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>0</SumNum></Response>`},
		{name: "simple field attribute", body: `<Response><CmdType>Catalog</CmdType><SN vendor="1">9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>0</SumNum></Response>`},
		{name: "nested simple field", body: `<Response><CmdType>Catalog</CmdType><SN><Value>9</Value></SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>0</SumNum></Response>`},
		{name: "unknown list attribute", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList Num="1" vendor="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID></Item></DeviceList></Response>`},
		{name: "unknown list child", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>0</SumNum><DeviceList Num="0"><Vendor/></DeviceList></Response>`},
		{name: "duplicate item device", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><DeviceID>` + gb10ChannelID + `</DeviceID></Item></DeviceList></Response>`},
		{name: "response event", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Event>ADD</Event></Item></DeviceList></Response>`},
		{name: "2011 item info", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Info/></Item></DeviceList></Response>`},
		{name: "legacy info too long", body: `<Response><CmdType>Catalog</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>0</SumNum><Info>one</Info><Info>` + strings.Repeat("中", 1025) + `</Info></Response>`},
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
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	result := collector.Wait(waitCtx, collectorKey)
	cancel()
	if result.Complete || len(result.Items) != 0 {
		t.Fatalf("invalid Catalog changed collector: %+v", result)
	}
}

func TestCatalogDeviceQueryWaitsForAllResponseChunks(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion11)
	pending := &pendingQueryWait{
		wait:     make(chan *DeviceQueryOutput, 1),
		targetID: gb10DeviceID,
	}
	key := buildPendingQueryKey(gb10DeviceID, "Catalog", 66)
	api.pendingDeviceQuery.Store(key, pending)
	defer api.pendingDeviceQuery.Delete(key)

	secondChannelID := "34020000001320000003"
	chunks := []string{
		`<Response><CmdType>Catalog</CmdType><SN>66</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>2</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Name>Camera 1</Name></Item></DeviceList></Response>`,
		`<Response><CmdType>Catalog</CmdType><SN>66</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>2</SumNum><DeviceList Num="1"><Item><DeviceID>` + secondChannelID + `</DeviceID><Name>Camera 2</Name></Item></DeviceList></Response>`,
	}

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "catalog-device-query-first", []byte(chunks[0]), api.sipMessageCatalog)
	assertFlowOK(t, response)
	select {
	case out := <-pending.wait:
		t.Fatalf("Catalog DeviceQuery completed after first response chunk: %+v", out)
	default:
	}
	response = runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "catalog-device-query-duplicate", []byte(chunks[0]), api.sipMessageCatalog)
	assertFlowOK(t, response)
	select {
	case out := <-pending.wait:
		t.Fatalf("duplicate Catalog chunk completed DeviceQuery: %+v", out)
	default:
	}

	response = runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "catalog-device-query-second", []byte(chunks[1]), api.sipMessageCatalog)
	assertFlowOK(t, response)
	select {
	case out := <-pending.wait:
		items, ok := out.Data.([]Channels)
		if !ok || len(items) != 2 || items[0].ChannelID != gb10ChannelID || items[1].ChannelID != secondChannelID {
			t.Fatalf("aggregated Catalog DeviceQuery data = %#v", out.Data)
		}
		if len(out.responseXML) != 2 {
			t.Fatalf("aggregated Catalog response XML count = %d, want 2", len(out.responseXML))
		}
	case <-time.After(time.Second):
		t.Fatal("Catalog DeviceQuery did not complete after all response chunks")
	}
}

func TestCatalogDeviceQueryAggregatesAppendixA4AcrossResponseChunks(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	pending := &pendingQueryWait{
		wait:     make(chan *DeviceQueryOutput, 1),
		targetID: gb10DeviceID,
	}
	key := buildPendingQueryKey(gb10DeviceID, "Catalog", 68)
	api.pendingDeviceQuery.Store(key, pending)
	defer api.pendingDeviceQuery.Delete(key)

	expected := 2
	api.resolvePendingDeviceQueryResult(gb10DeviceID, "Catalog", 68, "", []byte("<Response><DeviceList/></Response>"), gb10DeviceID, decodedDeviceQuery{
		data:            []Channels{{ChannelID: gb10ChannelID}},
		appendixA4:      []AppendixA4Object{{Type: "personType", RawXML: "<Info type=\"personType\"/>"}},
		catalogExpected: &expected,
	})
	api.resolvePendingDeviceQueryResult(gb10DeviceID, "Catalog", 68, "", []byte("<Response><DeviceList/></Response>"), gb10DeviceID, decodedDeviceQuery{
		data:            []Channels{{ChannelID: "34020000001320000003"}},
		appendixA4:      []AppendixA4Object{{Type: "rectType", RawXML: "<Info type=\"rectType\"/>"}},
		catalogExpected: &expected,
	})
	select {
	case out := <-pending.wait:
		if len(out.AppendixA4) != 2 || out.AppendixA4[0].Type != "personType" || out.AppendixA4[1].Type != "rectType" {
			t.Fatalf("aggregated Catalog Appendix A.4 = %+v", out.AppendixA4)
		}
	default:
		t.Fatal("Catalog query did not complete after all Appendix A.4 response chunks")
	}
}

func TestCatalogDeviceQueryAcceptsMatchedAdministrativeTarget(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion11)
	pending := &pendingQueryWait{
		wait:     make(chan *DeviceQueryOutput, 1),
		targetID: "340200",
	}
	key := buildPendingQueryKey(gb10DeviceID, "Catalog", 67)
	api.pendingDeviceQuery.Store(key, pending)
	defer api.pendingDeviceQuery.Delete(key)

	body := []byte(`<Response><CmdType>Catalog</CmdType><SN>67</SN><DeviceID>340200</DeviceID><SumNum>0</SumNum></Response>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "catalog-administrative-target", body, api.sipMessageCatalog)
	assertFlowOK(t, response)
	select {
	case out := <-pending.wait:
		items, ok := out.Data.([]Channels)
		if !ok || len(items) != 0 || out.DeviceID != "340200" {
			t.Fatalf("administrative Catalog output = %+v", out)
		}
	case <-time.After(time.Second):
		t.Fatal("administrative Catalog did not resolve matched DeviceQuery")
	}
}

func TestCatalogAdministrativeTargetPersistsAppendixA4OnParentDevice(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	memory := newFlowMemory(persistentDevice.DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: "340200"}
	key := buildPendingQueryKey(persistentDevice.DeviceID, "Catalog", 69)
	api.pendingDeviceQuery.Store(key, pending)
	defer api.pendingDeviceQuery.Delete(key)
	body := []byte(`<Response><CmdType>Catalog</CmdType><SN>69</SN><DeviceID>340200</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>340200</DeviceID><Name>Region</Name><Info><GrassrootsCode>340200</GrassrootsCode><doorType><DeviceID>` + persistentDevice.DeviceID +
		`</DeviceID></doorType></Info></Item></DeviceList></Response>`)

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
		"catalog-administrative-a4", body, api.sipMessageCatalog)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState("340200")
	if !ok || len(state.AppendixA4) != 1 || state.AppendixA4[0].Type != "doorType" {
		t.Fatalf("administrative Catalog Appendix A.4 state = %+v", state)
	}
	var parent ipc.Device
	if err := adapter.Store().Device().Get(t.Context(), &parent,
		orm.Where("device_id = ?", persistentDevice.DeviceID)); err != nil {
		t.Fatal(err)
	}
	if len(parent.Ext.GBAppendixA4) != 1 || parent.Ext.GBAppendixA4[0].Type != "doorType" {
		t.Fatalf("administrative Catalog Appendix A.4 was not persisted on parent: %+v", parent.Ext.GBAppendixA4)
	}
}

func TestCatalogResponseRejectsUnmatchedAdministrativeTarget(t *testing.T) {
	for _, test := range []struct {
		name           string
		pendingTarget  string
		responseTarget string
	}{
		{name: "no pending query", responseTarget: "340200"},
		{name: "different pending region", pendingTarget: "340200", responseTarget: "110101"},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(GBVersion11)
			if test.pendingTarget != "" {
				key := buildPendingQueryKey(gb10DeviceID, "Catalog", 68)
				api.pendingDeviceQuery.Store(key, &pendingQueryWait{
					wait: make(chan *DeviceQueryOutput, 1), targetID: test.pendingTarget,
				})
				defer api.pendingDeviceQuery.Delete(key)
			}
			body := []byte(`<Response><CmdType>Catalog</CmdType><SN>68</SN><DeviceID>` + test.responseTarget + `</DeviceID><SumNum>0</SumNum></Response>`)
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "catalog-unmatched-administrative-target", body, api.sipMessageCatalog)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("unmatched administrative Catalog response = %s", response)
			}
		})
	}
}

func TestCatalog20AcceptsStandardScalableInfoAnd11RejectsIt(t *testing.T) {
	body := `<Response><CmdType>Catalog</CmdType><SN>12</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Info><DownloadSpeed>1/2/4</DownloadSpeed><SVCSpaceSupportMode>3</SVCSpaceSupportMode><SVCTimeSupportMode>2</SVCTimeSupportMode></Info></Item></DeviceList></Response>`
	for _, test := range []struct {
		version GBProtocolVersion
		wantOK  bool
	}{
		{version: GBVersion11},
		{version: GBVersion20, wantOK: true},
	} {
		t.Run(string(test.version), func(t *testing.T) {
			api, _ := newVersionGateAPI(test.version)
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "catalog-scalable-"+string(test.version), []byte(body), api.sipMessageCatalog)
			if test.wantOK {
				assertFlowOK(t, response)
			} else if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("cross-version Catalog response = %s", response)
			}
		})
	}
}

func TestCatalogInfoRejectsDuplicateAndOutOfOrderFields(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion11)
	for _, test := range []struct {
		name string
		info string
	}{
		{name: "duplicate", info: `<PTZType>1</PTZType><PTZType>2</PTZType>`},
		{name: "out of order", info: `<RoomType>1</RoomType><PTZType>1</PTZType>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `<Response><CmdType>Catalog</CmdType><SN>14</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Info>` + test.info + `</Info></Item></DeviceList></Response>`
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "catalog-info-"+test.name, []byte(body), api.sipMessageCatalog)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid Catalog Info response = %s", response)
			}
		})
	}
}

func TestCatalogHandlerPreservesTailVendorExtensions(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion11)
	body := `<Response><CmdType>Catalog</CmdType><SN>13</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Info><PTZType>1</PTZType><VendorCapability><Enabled>true</Enabled></VendorCapability></Info><VendorExtension>fixture-value</VendorExtension></Item></DeviceList><Info>vendor-response</Info></Response>`
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "catalog-vendor-tail", []byte(body), api.sipMessageCatalog)
	assertFlowOK(t, response)
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

func TestCatalogAcceptsDeclaredTotalAbovePerChunkLimit(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion11)
	listNum := 1
	msg := MessageDeviceListResponse{
		XMLName: xml.Name{Local: "Response"}, CmdType: "Catalog", SN: 10, DeviceID: gb10DeviceID,
		SumNum: gbMultiResponseMaxItems + 1, HasSumNum: true, HasList: true, ListNum: &listNum,
		Item: []Channels{{ChannelID: gb10ChannelID, Status: "ON"}},
	}
	if err := api.validateCatalogEnvelope(&sip.Context{DeviceID: gb10DeviceID}, msg, false); err != nil {
		t.Fatalf("Catalog declared total above the per-chunk limit was rejected: %v", err)
	}
}

func TestPendingCatalogChunkKeepsIndependentCumulativeSafetyLimit(t *testing.T) {
	seen := map[string]struct{}{gb10ChannelID: {}}
	items, keys, err := pendingCatalogChunkItems(gbMultiResponseMaxCollectedItems, seen, []Channels{{ChannelID: "34020000001320000003"}})
	if !errors.Is(err, errMultiResponseItemLimit) || len(items) != 0 || len(keys) != 0 {
		t.Fatalf("pending Catalog cumulative safety result = items:%v keys:%v err:%v", items, keys, err)
	}
	if len(seen) != 1 {
		t.Fatalf("rejected pending Catalog chunk mutated seen set: %v", seen)
	}
}

func TestCatalogItemAdministrativeIDVersionBoundary(t *testing.T) {
	listNum := 1
	for _, test := range []struct {
		version GBProtocolVersion
		wantOK  bool
	}{
		{version: GBVersion10},
		{version: GBVersion11, wantOK: true},
		{version: GBVersion20, wantOK: true},
		{version: GBVersion30, wantOK: true},
	} {
		t.Run(string(test.version), func(t *testing.T) {
			api, _ := newVersionGateAPI(test.version)
			msg := MessageDeviceListResponse{
				XMLName: xml.Name{Local: "Response"}, CmdType: "Catalog", SN: 11, DeviceID: gb10DeviceID,
				SumNum: 1, HasSumNum: true, HasList: true, ListNum: &listNum,
				Item: []Channels{{ChannelID: "340200", Name: "合肥市", hasName: true}},
			}
			err := api.validateCatalogEnvelope(&sip.Context{DeviceID: gb10DeviceID}, msg, false)
			if (err == nil) != test.wantOK {
				t.Fatalf("protocol %s administrative Catalog item validation error = %v, want success = %v", test.version, err, test.wantOK)
			}
		})
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
			item.Name = "Camera"
			item.Manufacturer = "Vendor"
			item.Model = "Model"
			item.CivilCode = "340200"
			item.Address = "Address"
			item.ParentID = gb10DeviceID
			item.hasParental = true
			item.hasRegisterWay = true
			item.hasSecrecy = true
			item.hasLongitude = true
			item.hasLatitude = true
			item.Info = CatalogItemInfo{PTZType: 1, GrassrootsCode: "000000", PointType: 2, hasPointType: true}
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
	for _, value := range []string{"0010100", "0010142", "9999900"} {
		if err := validateCatalogItemValues(Channels{Info: CatalogItemInfo{CapturePositionType: value, GrassrootsCode: "340200"}}, GBVersion30); err != nil {
			t.Fatalf("2022 rejected valid position code %q: %v", value, err)
		}
	}
	for _, value := range []string{"0010600", "0300100", "7777700"} {
		if err := validateCatalogItemValues(Channels{Info: CatalogItemInfo{CapturePositionType: value}}, GBVersion30); err == nil {
			t.Fatalf("2022 accepted CapturePositionType %q outside Annex O table O.1", value)
		}
	}
}

func TestValidCapturePositionTypeAnnexOTable(t *testing.T) {
	// 期望值逐项抄录自 GB/T 28181-2022 表 O.1，避免测试依赖生产代码中的 switch。
	middleCodes := strings.Fields(`
		00101 00102 00103 00104 00105 00199
		00201 00202 00203 00204 00297 00298 00299
		00301 00302 00303 00304 00305 00306 00307 00308 00399
		00401 00402 00499
		00501 00502 00503 00599
		00601 00602 00603 00604 00605 00699
		00701 00799
		00801 00802 00803 00804 00805 00806 00807 00808 00809 00896 00897 00898 00899
		00901 01001
		01101 01102 01199
		01201 01202 01203 01204 01205 01206 01207 01299
		01301 01399
		01401 01402 01403 01499
		01501 01502 01503 01504 01505 01599
		01601 01602 01603 01699
		01701 01702 01703 01799
		01801 01802 01803 01899
		01901 01902 01903 01904 01905 01999
		02001 02002 02099
		02101 02102 02103 02199
		02201 02299 02301 02399 02401 02499
		02501 02502 02599 02601 02699 02701 02799 02801 02899
		03101 03102 03199 03201 03202 03203 03299
		99899 99999
	`)
	wantCodes := make(map[string]struct{}, len(middleCodes))
	for _, code := range middleCodes {
		if _, exists := wantCodes[code]; exists {
			t.Fatalf("duplicate Annex O middle code %q in test fixture", code)
		}
		wantCodes[code] = struct{}{}
		for suffix := 0; suffix <= 99; suffix++ {
			value := fmt.Sprintf("%s%02d", code, suffix)
			if !validCapturePositionType(value) {
				t.Fatalf("validCapturePositionType(%q) = false", value)
			}
		}
	}

	for prefix := 0; prefix <= 99999; prefix++ {
		code := fmt.Sprintf("%05d", prefix)
		_, want := wantCodes[code]
		if got := validCapturePositionType(code + "00"); got != want {
			t.Fatalf("validCapturePositionType(%q) = %t, want %t", code+"00", got, want)
		}
	}

	for _, value := range []string{"000000", "00000000", "001010A", " 0010100x ", "0000000"} {
		if validCapturePositionType(value) {
			t.Fatalf("validCapturePositionType(%q) = true", value)
		}
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

func TestCatalogTypedOptionalFieldsDistinguishMissingFromExplicitZero(t *testing.T) {
	decodeItem := func(t *testing.T, body string) Channels {
		t.Helper()
		var item Channels
		if err := sip.XMLDecode([]byte(body), &item); err != nil {
			t.Fatalf("decode Catalog item: %v", err)
		}
		return item
	}

	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20} {
		for _, field := range []string{
			`<RegisterWay>0</RegisterWay>`,
			`<ErrCode>0</ErrCode>`,
			`<EndTime/>`,
		} {
			item := decodeItem(t, `<Item>`+field+`</Item>`)
			if err := validateCatalogItemValues(item, version); err == nil {
				t.Fatalf("protocol %s accepted explicitly invalid Catalog field %s", version, field)
			}
		}
	}

	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20} {
		for _, field := range []string{
			`<PTZType/>`,
			`<PositionType>0</PositionType>`,
			`<RoomType>0</RoomType>`,
			`<UseType>0</UseType>`,
			`<SupplyLightType>0</SupplyLightType>`,
			`<DirectionType>0</DirectionType>`,
			`<BusinessGroupID/>`,
		} {
			item := decodeItem(t, `<Item><Info>`+field+`</Info></Item>`)
			if err := validateCatalogItemValues(item, version); err == nil {
				t.Fatalf("protocol %s accepted explicitly invalid Catalog Info field %s", version, field)
			}
		}
	}

	valid30 := `<Item><DeviceID>11010100002000000001</DeviceID><Name>Platform</Name>` +
		`<Manufacturer>Vendor</Manufacturer><Model>Model</Model><CivilCode>110101</CivilCode>` +
		`<Address>Address</Address><RegisterWay>1</RegisterWay><Secrecy>0</Secrecy><Status>ON</Status>` +
		`<Info><GrassrootsCode>000000</GrassrootsCode></Info></Item>`
	if err := validateCatalogItemValues(decodeItem(t, valid30), GBVersion30); err != nil {
		t.Fatalf("valid protocol 3.0 Catalog baseline rejected: %v", err)
	}
	for _, field := range []string{
		`<RoomType>0</RoomType>`,
		`<SupplyLightType>0</SupplyLightType>`,
		`<DirectionType>0</DirectionType>`,
		`<MobileDeviceType>0</MobileDeviceType>`,
		`<HorizontalFieldAngle>0</HorizontalFieldAngle>`,
		`<VerticalFieldAngle>0</VerticalFieldAngle>`,
		`<PointType>0</PointType>`,
		`<InstallTime/>`,
	} {
		itemBody := strings.Replace(valid30, `<GrassrootsCode>000000</GrassrootsCode>`, `<GrassrootsCode>000000</GrassrootsCode>`+field, 1)
		if err := validateCatalogItemValues(decodeItem(t, itemBody), GBVersion30); err == nil {
			t.Fatalf("protocol 3.0 accepted explicitly invalid Catalog Info field %s", field)
		}
	}
	emptyBusinessGroup := strings.Replace(valid30, `<Info>`, `<BusinessGroupID/><Info>`, 1)
	if err := validateCatalogItemValues(decodeItem(t, emptyBusinessGroup), GBVersion30); err == nil {
		t.Fatal("protocol 3.0 accepted empty typed BusinessGroupID")
	}

	validZeroMode := strings.Replace(valid30, `<GrassrootsCode>000000</GrassrootsCode>`, `<GrassrootsCode>000000</GrassrootsCode><SVCSpaceSupportMode>0</SVCSpaceSupportMode>`, 1)
	if err := validateCatalogItemValues(decodeItem(t, validZeroMode), GBVersion30); err != nil {
		t.Fatalf("protocol 3.0 rejected explicitly valid SVC zero mode: %v", err)
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
	body := `<Notify><CmdType>Catalog</CmdType><SN>65</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>2</SumNum><DeviceList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Event>OFF</Event></Item></DeviceList></Notify>`
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
