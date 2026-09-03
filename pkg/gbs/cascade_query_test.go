package gbs

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/gowvp/owl/internal/core/recording/store/recordingdb"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

const (
	testCascadeChannelID = "34020000001320000011"
	testExposedChannelID = "34020000001320000911"
)

func testSharedCascadePlatform(t *testing.T) cascadePlatform {
	t.Helper()
	platform, err := normalizeCascadePlatform(conf.SIPUpstream{
		Name: "provincial", Enabled: true,
		ServerID: gb10PlatformID, Domain: "remote.example", Host: "192.0.2.30", Port: 5060,
		LocalID: gb10DeviceID, LocalDomain: "local.example", LocalHost: "192.0.2.20",
		Version: "1.1", SharedChannels: []string{testCascadeChannelID},
		ChannelIDMap: map[string]string{testCascadeChannelID: testExposedChannelID},
	}, conf.SIP{ID: gb10DeviceID, Domain: "local.example", Host: "192.0.2.20", Port: 5060}, "")
	if err != nil {
		t.Fatal(err)
	}
	return platform
}

func TestBuildCascadeCatalogItemsUsesMappingAndVersionProfile(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	channel := &ipc.Channel{
		ChannelID: testCascadeChannelID, Name: "Front Gate", PTZType: 3, IsOnline: true,
		Ext: ipc.DeviceExt{Manufacturer: "Vendor", Model: "IPC", GBCatalog: &ipc.GBCatalogExt{
			CivilCode: "3402000000", Address: "Building A", Longitude: 120.1, Latitude: 30.2,
			PositionType: 1, RoomType: 2, Resolution: "1920x1080",
		}},
	}

	legacy := buildCascadeCatalogItems([]*ipc.Channel{channel}, platform, GBVersion10)
	if len(legacy) != 1 || legacy[0].DeviceID != testExposedChannelID || legacy[0].ParentID != gb10DeviceID || legacy[0].Status != "ON" {
		t.Fatalf("legacy catalog item = %+v", legacy)
	}
	if legacy[0].Info != nil {
		t.Fatalf("2011 catalog unexpectedly contains 2014 Info: %+v", legacy[0].Info)
	}

	supplement := buildCascadeCatalogItems([]*ipc.Channel{channel}, platform, GBVersion11)
	if len(supplement) != 1 || supplement[0].Info == nil || supplement[0].Info.PTZType != "3" || supplement[0].Info.Resolution != "1920x1080" {
		t.Fatalf("2014 catalog item = %+v", supplement)
	}

	body, err := sip.XMLEncode(cascadeCatalogResponse{
		CmdType: "Catalog", SN: 7, DeviceID: gb10DeviceID, SumNum: 1,
		DeviceList: &cascadeCatalogDeviceList{Num: 1, Items: supplement},
	})
	if err != nil {
		t.Fatal(err)
	}
	xmlText := string(body)
	for _, expected := range []string{`<Response>`, `<SumNum>1</SumNum>`, `<DeviceList Num="1">`, `<DeviceID>` + testExposedChannelID + `</DeviceID>`, `<Owner></Owner>`, `<SafetyWay>0</SafetyWay>`, `<CertNum></CertNum>`, `<Certifiable>0</Certifiable>`, `<ErrCode>0</ErrCode>`, `<EndTime></EndTime>`, `<Info>`} {
		if !strings.Contains(xmlText, expected) {
			t.Fatalf("catalog XML missing %q: %s", expected, xmlText)
		}
	}
	for _, forbidden := range []string{"<PositionType>0</PositionType>", "<RoomType>0</RoomType>", "<UseType>0</UseType>", "<SupplyLightType>0</SupplyLightType>", "<DirectionType>0</DirectionType>"} {
		if strings.Contains(xmlText, forbidden) {
			t.Fatalf("2014 catalog XML contains unset Info field %q: %s", forbidden, xmlText)
		}
	}

	channel.Ext.GBCatalog.PTZTypeList = "1/2"
	channel.Ext.GBCatalog.BusinessGroupID = "34020000002150000001"
	channel.Ext.GBCatalog.SecurityLevelCode = "B"
	channel.Ext.GBCatalog.PhotoelectricImagingType = "1/9"
	channel.Ext.GBCatalog.StreamNumberList = "0/2"
	channel.Ext.GBCatalog.FunctionType = "01/99"
	channel.Ext.GBCatalog.RecordSaveDays = 30
	modern := buildCascadeCatalogItems([]*ipc.Channel{channel}, platform, GBVersion30)
	if len(modern) != 1 || modern[0].Info == nil || modern[0].Info.PTZType != "1/2" || modern[0].SecurityLevelCode != "B" || modern[0].BusinessGroupID != "34020000002150000001" || modern[0].Info.StreamNumberList != "0/2" || modern[0].Info.FunctionType != "01/99" || modern[0].Info.RecordSaveDays == nil || *modern[0].Info.RecordSaveDays != 30 {
		t.Fatalf("2022 catalog item = %+v", modern)
	}
	modernBody, err := sip.XMLEncode(cascadeCatalogResponse{
		CmdType: "Catalog", SN: 8, DeviceID: gb10DeviceID, SumNum: len(modern),
		DeviceList: &cascadeCatalogDeviceList{Num: len(modern), Items: modern},
	})
	if err != nil {
		t.Fatal(err)
	}
	modernXML := string(modernBody)
	for _, expected := range []string{"<SecurityLevelCode>B</SecurityLevelCode>", "<BusinessGroupID>34020000002150000001</BusinessGroupID>", "<PTZType>1/2</PTZType>", "<StreamNumberList>0/2</StreamNumberList>", "<RecordSaveDays>30</RecordSaveDays>"} {
		if !strings.Contains(modernXML, expected) {
			t.Fatalf("2022 catalog XML missing %q: %s", expected, modernXML)
		}
	}
	if strings.Contains(modernXML, "<Info><BusinessGroupID>") {
		t.Fatalf("2022 catalog kept BusinessGroupID in Info: %s", modernXML)
	}
	for _, removed := range []string{"<Owner>", "<PositionType>", "<UseType>", "<SafetyWay>", "<CertNum>", "<Certifiable>", "<ErrCode>", "<EndTime>"} {
		if strings.Contains(modernXML, removed) {
			t.Fatalf("2022 catalog contains removed field %q: %s", removed, modernXML)
		}
	}

	channel.Ext.GBCatalog.PointType = 1
	channel.Ext.GBCatalog.GrassrootsCode = "000000"
	channel.Ext.GBCatalog.InstallTime = "2026-08-28T10:00:00+08:00"
	channel.Ext.GBCatalog.ContactInfo = ""
	channel.Ext.GBCatalog.RecordSaveDays = 0
	classOne := buildCascadeCatalogItems([]*ipc.Channel{channel}, platform, GBVersion30)
	if len(classOne) != 1 || classOne[0].Info == nil || classOne[0].Info.ContactInfo == nil ||
		*classOne[0].Info.ContactInfo != "" || classOne[0].Info.RecordSaveDays == nil ||
		*classOne[0].Info.RecordSaveDays != 0 {
		t.Fatalf("2022 class I Catalog required zero values = %+v", classOne)
	}
	classOneBody, err := sip.XMLEncode(cascadeCatalogResponse{
		CmdType: "Catalog", SN: 9, DeviceID: gb10DeviceID, SumNum: len(classOne),
		DeviceList: &cascadeCatalogDeviceList{Num: len(classOne), Items: classOne},
	})
	if err != nil {
		t.Fatal(err)
	}
	classOneXML := string(classOneBody)
	for _, expected := range []string{"<ContactInfo></ContactInfo>", "<RecordSaveDays>0</RecordSaveDays>"} {
		if !strings.Contains(classOneXML, expected) {
			t.Fatalf("2022 class I Catalog XML missing %q: %s", expected, classOneXML)
		}
	}
}

func TestCascadeCatalog2022UsesAnnexJItemProfiles(t *testing.T) {
	const (
		systemID  = "11010100002000000001"
		groupID   = "11010100002150000001"
		virtualID = "11010100002160000001"
		deviceID  = "11010100001320000009"
	)
	items := []cascadeCatalogItem{
		{protocolVersion: GBVersion30, DeviceID: "110101", Name: "District"},
		{
			protocolVersion: GBVersion30, DeviceID: systemID, Name: "Platform",
			Manufacturer: "Platform Vendor", Model: "Platform Model", CivilCode: "110101",
			Address: "Platform Address", RegisterWay: 1, Status: "ON",
		},
		{
			protocolVersion: GBVersion30, DeviceID: groupID, Name: "Business Group",
			CivilCode: "110101", ParentID: systemID,
		},
		{
			protocolVersion: GBVersion30, DeviceID: virtualID, Name: "Virtual Organization",
			BusinessGroupID: groupID,
		},
		{
			protocolVersion: GBVersion30, DeviceID: deviceID, Name: "IPC",
			Manufacturer: "Device Vendor", Model: "Device Model", CivilCode: "110101",
			Address: "Device Address", ParentID: systemID, RegisterWay: 1, Status: "ON",
		},
	}
	if err := validateCascadeCatalogItemsForVersion(items, GBVersion30); err != nil {
		t.Fatal(err)
	}
	body, err := sip.XMLEncode(cascadeCatalogDeviceList{Num: len(items), Items: items})
	if err != nil {
		t.Fatal(err)
	}
	xmlText := string(body)
	itemXML := func(deviceID string) string {
		t.Helper()
		marker := "<DeviceID>" + deviceID + "</DeviceID>"
		markerIndex := strings.Index(xmlText, marker)
		if markerIndex < 0 {
			t.Fatalf("Catalog XML missing %s: %s", deviceID, xmlText)
		}
		start := strings.LastIndex(xmlText[:markerIndex], "<Item>")
		endOffset := strings.Index(xmlText[markerIndex:], "</Item>")
		if start < 0 || endOffset < 0 {
			t.Fatalf("Catalog item %s boundaries not found: %s", deviceID, xmlText)
		}
		return xmlText[start : markerIndex+endOffset+len("</Item>")]
	}
	for _, forbidden := range []string{"<Manufacturer>", "<CivilCode>", "<Status>", "<Info>"} {
		if strings.Contains(itemXML("110101"), forbidden) {
			t.Errorf("administrative item contains %s: %s", forbidden, itemXML("110101"))
		}
	}
	for _, forbidden := range []string{"<Parental>", "<Longitude>", "<Latitude>", "<Info>"} {
		if strings.Contains(itemXML(systemID), forbidden) {
			t.Errorf("system item contains %s: %s", forbidden, itemXML(systemID))
		}
	}
	if strings.Contains(itemXML(groupID), "<Status>") || strings.Contains(itemXML(virtualID), "<CivilCode>") {
		t.Fatalf("directory item profile mismatch: %s / %s", itemXML(groupID), itemXML(virtualID))
	}
	for _, expected := range []string{"<Manufacturer>Device Vendor</Manufacturer>", "<Address>Device Address</Address>", "<ParentID>" + systemID + "</ParentID>", "<Status>ON</Status>"} {
		if !strings.Contains(itemXML(deviceID), expected) {
			t.Errorf("device item missing %s: %s", expected, itemXML(deviceID))
		}
	}
	for _, forbidden := range []string{"<Longitude>", "<Latitude>", "<Info>"} {
		if strings.Contains(itemXML(deviceID), forbidden) {
			t.Errorf("device without known camera metadata contains %s: %s", forbidden, itemXML(deviceID))
		}
	}
}

func TestCascadeCatalog2022DerivesCivilCodeAndRejectsPartialInfo(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	platform.localDomain = "local.example"
	channel := &ipc.Channel{
		ChannelID: testCascadeChannelID, Name: "Camera", IsOnline: true,
		Ext: ipc.DeviceExt{Manufacturer: "Vendor", Model: "IPC", GBCatalog: &ipc.GBCatalogExt{
			Address: "Building A",
		}},
	}
	items := buildCascadeCatalogItems([]*ipc.Channel{channel}, platform, GBVersion30)
	if len(items) != 1 || items[0].CivilCode != testExposedChannelID[:8] {
		t.Fatalf("derived CivilCode items = %+v", items)
	}
	if err := validateCascadeCatalogItemsForVersion(items, GBVersion30); err != nil {
		t.Fatal(err)
	}

	items[0].Info = &cascadeCatalogInfo{Resolution: "1920x1080"}
	if err := validateCascadeCatalogItemsForVersion(items, GBVersion30); err == nil || !strings.Contains(err.Error(), "GrassrootsCode") {
		t.Fatalf("partial 2022 camera Info validation error = %v", err)
	}
	items[0].Info = nil
	items[0].CivilCode = "local.example"
	if err := validateCascadeCatalogItemsForVersion(items, GBVersion30); err == nil || !strings.Contains(err.Error(), "CivilCode") {
		t.Fatalf("invalid 2022 CivilCode validation error = %v", err)
	}
}

func TestCascadeCatalogBuildsAndMergesAnnexHPlatformTopology(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	platform.localID = testCascadePathB
	platform.version = GBVersion30
	cameraM := "34020000001320000077"
	exposedCameraM := "34020000001320000977"
	cameraWithoutParent := "34020000001320000079"
	exposedCameraWithoutParent := "34020000001320000979"
	platform.sharedChannels = []string{cameraM, cameraWithoutParent}
	platform.channelIDMap = map[string]string{cameraM: exposedCameraM, cameraWithoutParent: exposedCameraWithoutParent}
	platform.exposedChannelMap = map[string]string{exposedCameraM: cameraM, exposedCameraWithoutParent: cameraWithoutParent}
	directD := "34020000002000000005"

	createDevice := func(id, internalID string, online bool) {
		t.Helper()
		if err := adapter.Store().Device().Create(t.Context(), &ipc.Device{
			ID: internalID, DeviceID: id, Type: ipc.TypeGB28181, IsOnline: online,
		}); err != nil {
			t.Fatal(err)
		}
	}
	createChannel := func(internalID, deviceInternalID, deviceID, channelID, parentID string, online bool) {
		t.Helper()
		if err := adapter.Store().Channel().Create(t.Context(), &ipc.Channel{
			ID: internalID, DID: deviceInternalID, DeviceID: deviceID, ChannelID: channelID,
			Name: channelID, Type: ipc.TypeGB28181, IsOnline: online,
			Ext: ipc.DeviceExt{GBCatalog: &ipc.GBCatalogExt{
				Kind: classifyGBCatalogItem(channelID), ParentID: parentID,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	createDevice(testCascadePathC, "GB_path_c", true)
	createDevice(directD, "GB_path_d", true)
	createChannel("GBC_path_c_root", "GB_path_c", testCascadePathC, testCascadePathC, "", true)
	createChannel("GBC_path_c_e", "GB_path_c", testCascadePathC, testCascadePathE, testCascadePathC, true)
	createChannel("GBC_path_c_m", "GB_path_c", testCascadePathC, cameraM, testCascadePathE, true)
	createChannel("GBC_path_c_n", "GB_path_c", testCascadePathC, cameraWithoutParent, "", true)
	createChannel("GBC_path_d_root", "GB_path_d", directD, directD, "", true)
	createChannel("GBC_path_d_e", "GB_path_d", directD, testCascadePathE, directD, true)
	createChannel("GBC_path_d_m", "GB_path_d", directD, cameraM, testCascadePathE, false)

	api := &GB28181API{core: adapter}
	channels, err := api.loadCascadeCatalogChannels(t.Context(), platform, GBVersion30)
	if err != nil {
		t.Fatal(err)
	}
	items := buildCascadeCatalogItems(channels, platform, GBVersion30)
	byID := make(map[string]cascadeCatalogItem, len(items))
	for _, item := range items {
		byID[item.DeviceID] = item
	}
	if len(items) != 6 {
		t.Fatalf("Annex H Catalog item count = %d, items = %+v", len(items), items)
	}
	if root := byID[testCascadePathB]; root.DeviceID == "" || root.ParentID != "" {
		t.Fatalf("local platform item = %+v", root)
	}
	rootBody, err := sip.XMLEncode(cascadeCatalogDeviceList{Num: 1, Items: []cascadeCatalogItem{byID[testCascadePathB]}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rootBody), "<ParentID>") {
		t.Fatalf("local platform item contains ParentID: %s", rootBody)
	}
	if item := byID[testCascadePathC]; item.ParentID != testCascadePathB {
		t.Fatalf("platform C ParentID = %q", item.ParentID)
	}
	if item := byID[directD]; item.ParentID != testCascadePathB {
		t.Fatalf("platform D ParentID = %q", item.ParentID)
	}
	if item := byID[testCascadePathE]; item.ParentID != testCascadePathC+"/"+directD {
		t.Fatalf("platform E ParentID = %q", item.ParentID)
	}
	if item := byID[exposedCameraM]; item.ParentID != testCascadePathE || item.Status != "ON" {
		t.Fatalf("merged camera item = %+v", item)
	}
	if item := byID[exposedCameraWithoutParent]; item.ParentID != testCascadePathC {
		t.Fatalf("camera fallback ParentID = %+v", item)
	}
}

func TestCascadeCatalogSynthesizesConfirmedMissingPlatformNodes(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	cameraID := "34020000001320000080"
	exposedCameraID := "34020000001320000980"
	platform := testSharedCascadePlatform(t)
	platform.localID = testCascadePathB
	platform.version = GBVersion30
	platform.sharedChannels = []string{cameraID}
	platform.channelIDMap = map[string]string{cameraID: exposedCameraID}
	platform.exposedChannelMap = map[string]string{exposedCameraID: cameraID}
	device := &ipc.Device{ID: "GB_missing_path_c", DeviceID: testCascadePathC, Type: ipc.TypeGB28181, IsOnline: true}
	channel := &ipc.Channel{
		ID: "GBC_missing_path_camera", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: cameraID, Name: "Camera M", Type: ipc.TypeGB28181, IsOnline: true,
		Ext: ipc.DeviceExt{GBCatalog: &ipc.GBCatalogExt{Kind: GBCatalogItemDevice, ParentID: testCascadePathE}},
	}
	if err := adapter.Store().Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Store().Channel().Create(t.Context(), channel); err != nil {
		t.Fatal(err)
	}

	api := &GB28181API{core: adapter}
	channels, err := api.loadCascadeCatalogChannels(t.Context(), platform, GBVersion30)
	if err != nil {
		t.Fatal(err)
	}
	items := buildCascadeCatalogItems(channels, platform, GBVersion30)
	byID := make(map[string]cascadeCatalogItem, len(items))
	for _, item := range items {
		byID[item.DeviceID] = item
	}
	if len(items) != 4 || byID[testCascadePathC].ParentID != testCascadePathB || byID[testCascadePathE].ParentID != testCascadePathC || byID[exposedCameraID].ParentID != testCascadePathE {
		t.Fatalf("synthesized Annex H topology = %+v", items)
	}
}

func TestCascadeInitialCatalogNotifyDoesNotFabricateOfflineSyntheticDirectories(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	cameraID := "34020000001320000080"
	exposedCameraID := "34020000001320000980"
	platform := testSharedCascadePlatform(t)
	platform.localID = testCascadePathB
	platform.version = GBVersion30
	platform.sharedChannels = []string{cameraID}
	platform.channelIDMap = map[string]string{cameraID: exposedCameraID}
	platform.exposedChannelMap = map[string]string{exposedCameraID: cameraID}
	device := &ipc.Device{ID: "GB_offline_synthetic_path", DeviceID: testCascadePathC, Type: ipc.TypeGB28181, IsOnline: true}
	channel := &ipc.Channel{
		ID: "GBC_offline_synthetic_camera", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: cameraID, Name: "Offline Camera", Type: ipc.TypeGB28181, IsOnline: false,
		Ext: ipc.DeviceExt{GBCatalog: &ipc.GBCatalogExt{Kind: GBCatalogItemDevice, ParentID: testCascadePathE}},
	}
	if err := adapter.Store().Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Store().Channel().Create(t.Context(), channel); err != nil {
		t.Fatal(err)
	}

	api := &GB28181API{core: adapter}
	channels, err := api.loadCascadeCatalogChannels(t.Context(), platform, GBVersion30)
	if err != nil {
		t.Fatal(err)
	}
	items := buildCascadeCatalogItems(channels, platform, GBVersion30)
	byID := make(map[string]cascadeCatalogItem, len(items))
	for _, item := range items {
		byID[item.DeviceID] = item
	}
	for _, directoryID := range []string{testCascadePathC, testCascadePathE} {
		if item := byID[directoryID]; item.Status != "ON" {
			t.Errorf("synthetic directory %s status = %q, want ON", directoryID, item.Status)
		}
	}
	if item := byID[exposedCameraID]; item.Status != "OFF" {
		t.Fatalf("offline shared camera = %+v", item)
	}

	initial := prepareCascadeCatalogNotifyItems(items, true)
	if len(initial) != 1 || initial[0].DeviceID != exposedCameraID || initial[0].Event != "OFF" {
		t.Fatalf("initial Catalog items = %+v", initial)
	}
}

func TestMergeCascadeCatalogChannelsKeepsPersistedDirectoryOffline(t *testing.T) {
	persisted := &ipc.Channel{
		ID: "GBC_persisted_offline_directory", DeviceID: testCascadePathC,
		ChannelID: testCascadePathE, Name: "Offline Directory", Type: ipc.TypeGB28181, IsOnline: false,
		Ext: ipc.DeviceExt{GBCatalog: &ipc.GBCatalogExt{
			Kind: GBCatalogItemSystem, ParentID: testCascadePathC,
		}},
	}
	synthetic := newSyntheticCascadeCatalogChannel(testCascadePathE, testCascadePathC, testCascadePathC, "", "")
	merged := mergeCascadeCatalogChannels([]*ipc.Channel{persisted, synthetic}, testCascadePathB)
	if item := merged[testCascadePathE]; item == nil || item.IsOnline {
		t.Fatalf("persisted offline directory was overwritten by synthetic relation: %+v", item)
	}
}

func TestCascadeCatalogBuildsAnnexJDirectoryGraph(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	const (
		systemID       = "11010100002000000001"
		groupID        = "11010100002150000001"
		virtualID      = "11010100002160000001"
		childVirtualID = "11010100002160000002"
		parentDeviceID = "11010100001320000008"
		cameraAID      = "11010100001320000009"
		cameraBID      = "11010100001320000010"
		exposedAID     = "11010100001320000909"
		exposedBID     = "11010100001320000910"
	)
	platform := testSharedCascadePlatform(t)
	platform.localID = testCascadePathB
	platform.version = GBVersion30
	platform.sharedChannels = []string{cameraAID, cameraBID}
	platform.channelIDMap = map[string]string{cameraAID: exposedAID, cameraBID: exposedBID}
	platform.exposedChannelMap = map[string]string{exposedAID: cameraAID, exposedBID: cameraBID}

	if err := adapter.Store().Device().Create(t.Context(), &ipc.Device{
		ID: "GB_annex_j_system", DeviceID: systemID, Type: ipc.TypeGB28181, IsOnline: true,
	}); err != nil {
		t.Fatal(err)
	}
	createNode := func(internalID, channelID, name, parentID, businessGroupID, civilCode string) {
		t.Helper()
		if err := adapter.Store().Channel().Create(t.Context(), &ipc.Channel{
			ID: internalID, DID: "GB_annex_j_system", DeviceID: systemID, ChannelID: channelID,
			Name: name, Type: ipc.TypeGB28181, IsOnline: true,
			Ext: ipc.DeviceExt{GBCatalog: &ipc.GBCatalogExt{
				Kind: classifyGBCatalogItem(channelID), ParentID: parentID,
				BusinessGroupID: businessGroupID, CivilCode: civilCode,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	createNode("GBC_annex_j_province", "11", "北京市", "", "", "11")
	createNode("GBC_annex_j_group", groupID, "业务分组", systemID, "", "110101")
	createNode("GBC_annex_j_virtual", virtualID, "虚拟组织一", "", groupID, "110101")
	createNode("GBC_annex_j_child_virtual", childVirtualID, "虚拟组织二", virtualID, groupID, "110101")
	createNode("GBC_annex_j_parent_device", parentDeviceID, "父设备", systemID, "", "11010101")
	createNode("GBC_annex_j_camera_a", cameraAID, "摄像机 A", virtualID+"/"+parentDeviceID, groupID, "11010101")
	createNode("GBC_annex_j_camera_b", cameraBID, "摄像机 B", childVirtualID, "", "11010101")

	api := &GB28181API{core: adapter}
	channels, err := api.loadCascadeCatalogChannels(t.Context(), platform, GBVersion30)
	if err != nil {
		t.Fatal(err)
	}
	items := buildCascadeCatalogItems(channels, platform, GBVersion30)
	byID := make(map[string]cascadeCatalogItem, len(items))
	for _, item := range items {
		if _, duplicate := byID[item.DeviceID]; duplicate {
			t.Fatalf("duplicate Annex J item %s: %+v", item.DeviceID, items)
		}
		byID[item.DeviceID] = item
	}
	if len(items) != 12 {
		t.Fatalf("Annex J Catalog item count = %d, items = %+v", len(items), items)
	}
	for id, parentID := range map[string]string{
		"11": systemID, "1101": "11", "110101": "1101", "11010101": "110101",
		systemID: testCascadePathB, groupID: systemID, childVirtualID: virtualID,
		parentDeviceID: systemID, exposedAID: parentDeviceID + "/" + virtualID, exposedBID: childVirtualID,
	} {
		if got := byID[id].ParentID; got != parentID {
			t.Errorf("Annex J item %s ParentID = %q, want %q", id, got, parentID)
		}
	}
	if item := byID[virtualID]; item.ParentID != "" || item.BusinessGroupID != groupID || item.Name != "虚拟组织一" {
		t.Errorf("top virtual organization = %+v", item)
	}
	if byID["11"].Name != "北京市" || byID[groupID].Name != "业务分组" {
		t.Errorf("stored directory metadata was not preserved: %+v / %+v", byID["11"], byID[groupID])
	}

	assertScope := func(target string, want ...string) {
		t.Helper()
		filtered := filterCascadeCatalogNotifyItems(items, target, platform.localID)
		got := make(map[string]struct{}, len(filtered))
		for _, item := range filtered {
			got[item.DeviceID] = struct{}{}
		}
		if len(got) != len(want) {
			t.Fatalf("Catalog scope %s = %v, want %v", target, got, want)
		}
		for _, id := range want {
			if _, exists := got[id]; !exists {
				t.Fatalf("Catalog scope %s missing %s: %v", target, id, got)
			}
		}
	}
	assertScope("110101", "110101", "11010101", parentDeviceID, exposedAID, exposedBID)
	assertScope(groupID, groupID, virtualID, childVirtualID, exposedAID, exposedBID)
	assertScope(virtualID, virtualID, childVirtualID, exposedAID, exposedBID)
	assertScope(parentDeviceID, parentDeviceID, exposedAID)
	assertScope(systemID, systemID, "11", "1101", "110101", "11010101", groupID, virtualID, childVirtualID, parentDeviceID, exposedAID, exposedBID)

	for _, target := range []string{"110101", groupID, virtualID, parentDeviceID} {
		visible, visibilityErr := api.cascadeCatalogTargetVisible(t.Context(), platform, GBVersion30, target)
		if visibilityErr != nil || !visible {
			t.Errorf("visible Catalog target %s = %v, %v", target, visible, visibilityErr)
		}
	}
	visible, err := api.cascadeCatalogTargetVisible(t.Context(), platform, GBVersion30, "11010100002160000099")
	if err != nil || visible {
		t.Fatalf("unshared Catalog target visible = %v, %v", visible, err)
	}

	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20} {
		t.Run(version.StandardYear(), func(t *testing.T) {
			legacyChannels, loadErr := api.loadCascadeCatalogChannels(t.Context(), platform, version)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			legacyItems := buildCascadeCatalogItems(legacyChannels, platform, version)
			legacyByID := make(map[string]cascadeCatalogItem, len(legacyItems))
			for _, item := range legacyItems {
				legacyByID[item.DeviceID] = item
			}
			if len(legacyByID) != 11 {
				t.Fatalf("%s directory item count = %d, items = %+v", version.StandardName(), len(legacyByID), legacyItems)
			}
			for id, parentID := range map[string]string{
				"11": systemID, "1101": "11", "110101": "1101", "11010101": "110101",
				systemID: testCascadePathB, groupID: systemID, childVirtualID: virtualID,
				parentDeviceID: systemID, exposedAID: parentDeviceID + "/" + virtualID, exposedBID: childVirtualID,
			} {
				if got := legacyByID[id].ParentID; got != parentID {
					t.Errorf("%s item %s ParentID = %q, want %q", version.StandardName(), id, got, parentID)
				}
			}
			if item := legacyByID[virtualID]; item.ParentID != "" || item.Info == nil || item.Info.BusinessGroupID != groupID {
				t.Errorf("%s virtual organization = %+v", version.StandardName(), item)
			}
			assertLegacyScope := func(target string, want ...string) {
				t.Helper()
				filtered := filterCascadeCatalogNotifyItems(legacyItems, target, platform.localID)
				got := make(map[string]struct{}, len(filtered))
				for _, item := range filtered {
					got[item.DeviceID] = struct{}{}
				}
				if len(got) != len(want) {
					t.Fatalf("%s Catalog scope %s = %v, want %v", version.StandardName(), target, got, want)
				}
				for _, id := range want {
					if _, exists := got[id]; !exists {
						t.Fatalf("%s Catalog scope %s missing %s: %v", version.StandardName(), target, id, got)
					}
				}
			}
			assertLegacyScope("110101", "110101", "11010101", parentDeviceID, exposedAID, exposedBID)
			assertLegacyScope(groupID, groupID, virtualID, childVirtualID, exposedAID, exposedBID)
			assertLegacyScope(virtualID, virtualID, childVirtualID, exposedAID, exposedBID)
			for _, target := range []string{"110101", systemID, groupID, virtualID, parentDeviceID} {
				targetVisible, targetErr := api.cascadeCatalogTargetVisible(t.Context(), platform, version, target)
				if targetErr != nil || !targetVisible {
					t.Errorf("%s visible Catalog target %s = %v, %v", version.StandardName(), target, targetVisible, targetErr)
				}
			}
			targetVisible, targetErr := api.cascadeCatalogTargetVisible(t.Context(), platform, version, "11010100002160000099")
			if targetErr != nil || targetVisible {
				t.Fatalf("%s unshared Catalog target visible = %v, %v", version.StandardName(), targetVisible, targetErr)
			}
		})
	}
}

func TestCascadeCatalogLegacyRequiredFieldsStayPresent(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	channel := &ipc.Channel{ChannelID: testCascadeChannelID, Name: "Camera", IsOnline: true}
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20} {
		items := buildCascadeCatalogItems([]*ipc.Channel{channel}, platform, version)
		body, err := sip.XMLEncode(cascadeCatalogResponse{
			CmdType: "Catalog", SN: 9, DeviceID: gb10DeviceID, SumNum: 1,
			DeviceList: &cascadeCatalogDeviceList{Num: 1, Items: items},
		})
		if err != nil {
			t.Fatal(err)
		}
		xmlText := string(body)
		for _, expected := range []string{"<Owner></Owner>", "<SafetyWay>0</SafetyWay>", "<CertNum></CertNum>", "<Certifiable>0</Certifiable>", "<ErrCode>0</ErrCode>", "<EndTime></EndTime>"} {
			if !strings.Contains(xmlText, expected) {
				t.Errorf("version %s Catalog missing required field %q: %s", version, expected, xmlText)
			}
		}
	}
}

func TestBuildCascadeCatalogItemsSafelyCarries2022ExtraInfo(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	channel := &ipc.Channel{
		DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID, Name: "Door", IsOnline: true,
		Ext: ipc.DeviceExt{GBCatalog: &ipc.GBCatalogExt{
			RawXML: `<Item><DeviceID>` + testCascadeChannelID + `</DeviceID><ExtraInfo>{"type":"doorType","DeviceID":"` + testCascadeChannelID + `","ParentID":"` + gb10PlatformID + `"}</ExtraInfo></Item>`,
		}},
	}
	legacy := buildCascadeCatalogItems([]*ipc.Channel{channel}, platform, GBVersion20)
	if len(legacy) != 1 || len(legacy[0].ExtraInfo) != 0 {
		t.Fatalf("2.0 Catalog exposed 3.0 ExtraInfo: %+v", legacy)
	}
	modern := buildCascadeCatalogItems([]*ipc.Channel{channel}, platform, GBVersion30)
	if len(modern) != 1 || len(modern[0].ExtraInfo) != 1 {
		t.Fatalf("3.0 Catalog ExtraInfo = %+v", modern)
	}
	extra := modern[0].ExtraInfo[0]
	if !strings.Contains(extra, testExposedChannelID) || !strings.Contains(extra, gb10DeviceID) || strings.Contains(extra, testCascadeChannelID) || strings.Contains(extra, gb10PlatformID) {
		t.Fatalf("3.0 Catalog ExtraInfo mapping = %s", extra)
	}

	channel.Ext.GBCatalog.RawXML = `<Item><ExtraInfo>{"type":"doorType","DoorID":"34020000001320000099"}</ExtraInfo></Item>`
	modern = buildCascadeCatalogItems([]*ipc.Channel{channel}, platform, GBVersion30)
	if len(modern) != 1 || len(modern[0].ExtraInfo) != 0 {
		t.Fatalf("unshared A.4 object ID was exposed: %+v", modern)
	}
}

func TestBuildCascadeCatalogItemsPreserves2022ExtraInfoWhitespace(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	channel := &ipc.Channel{
		DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID, Name: "Door", IsOnline: true,
		Ext: ipc.DeviceExt{GBCatalog: &ipc.GBCatalogExt{
			RawXML: `<Item><ExtraInfo>  keep  </ExtraInfo><ExtraInfo>   </ExtraInfo><ExtraInfo></ExtraInfo><ExtraInfo> x </ExtraInfo></Item>`,
		}},
	}

	modern := buildCascadeCatalogItems([]*ipc.Channel{channel}, platform, GBVersion30)
	if len(modern) != 1 {
		t.Fatalf("3.0 Catalog item count = %d", len(modern))
	}
	want := []string{"  keep  ", "   ", "", " x "}
	if !reflect.DeepEqual(modern[0].ExtraInfo, want) {
		t.Fatalf("3.0 Catalog ExtraInfo whitespace changed: got %#v, want %#v", modern[0].ExtraInfo, want)
	}
	body, err := sip.XMLEncode(cascadeCatalogResponse{
		CmdType: "Catalog", SN: 17, DeviceID: gb10DeviceID, SumNum: 1,
		DeviceList: &cascadeCatalogDeviceList{Num: 1, Items: modern},
	})
	if err != nil {
		t.Fatalf("encode 3.0 Catalog ExtraInfo: %v", err)
	}
	if strings.Count(string(body), "<ExtraInfo>") != len(want) || !strings.Contains(string(body), "<ExtraInfo>  keep  </ExtraInfo>") || !strings.Contains(string(body), "<ExtraInfo></ExtraInfo>") {
		t.Fatalf("3.0 Catalog ExtraInfo wire values changed: %s", body)
	}
}

func TestPrepareCascadeInitialCatalogNotifyIncludesOnlyOffline(t *testing.T) {
	items := []cascadeCatalogItem{
		{DeviceID: "34020000001320000003", Status: "ON"},
		{DeviceID: "34020000001320000004", Status: "OFF"},
	}
	initial := prepareCascadeCatalogNotifyItems(items, true)
	if len(initial) != 1 || initial[0].DeviceID != "34020000001320000004" || initial[0].Event != "OFF" {
		t.Fatalf("initial Catalog items = %+v", initial)
	}
	body, err := encodeCascadeCatalogNotify(GBVersion11, cascadeCatalogNotify{
		CmdType: "Catalog", SN: 1, DeviceID: gb10DeviceID, Status: "OK", SumNum: len(initial),
		DeviceList: cascadeCatalogDeviceList{Num: len(initial), Items: initial},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, expected := range []string{"<Status>OK</Status>", "<SumNum>1</SumNum>", "<Event>OFF</Event>"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("initial Catalog NOTIFY missing %q: %s", expected, text)
		}
	}
	if strings.Contains(text, "34020000001320000003") {
		t.Fatalf("online channel leaked into initial Catalog NOTIFY: %s", text)
	}

	changes := prepareCascadeCatalogNotifyItems(items, false)
	if len(changes) != 2 || changes[0].Event != "UPDATE" || changes[1].Event != "UPDATE" {
		t.Fatalf("Catalog change items = %+v", changes)
	}
}

func TestCascadeCatalogNotifyUsesVersionSpecificRootAndTarget(t *testing.T) {
	item := cascadeCatalogItem{DeviceID: testExposedChannelID, Status: "ON", Event: "ADD"}
	notify := cascadeCatalogNotify{
		CmdType: "Catalog", SN: 9, DeviceID: testExposedChannelID, Status: "OK", SumNum: 1,
		DeviceList: cascadeCatalogDeviceList{Num: 1, Items: []cascadeCatalogItem{item}},
	}
	legacy, err := encodeCascadeCatalogNotify(GBVersion10, notify)
	if err != nil {
		t.Fatal(err)
	}
	legacyText := string(legacy)
	if !strings.Contains(legacyText, "<Response>") || strings.Contains(legacyText, "<Notify>") {
		t.Fatalf("2011 Catalog NOTIFY body root:\n%s", legacy)
	}
	if strings.Contains(legacyText, "<Status>OK</Status>") || strings.Contains(legacyText, "<Event>") {
		t.Fatalf("2011 Catalog NOTIFY emitted later-version fields:\n%s", legacy)
	}
	if notify.Status != "OK" || notify.DeviceList.Items[0].Event != "ADD" {
		t.Fatalf("2011 Catalog encoding mutated input: %+v", notify)
	}
	modern, err := encodeCascadeCatalogNotify(GBVersion11, notify)
	if err != nil {
		t.Fatal(err)
	}
	modernText := string(modern)
	if !strings.Contains(modernText, "<Notify>") || strings.Contains(modernText, "<Response>") ||
		!strings.Contains(modernText, "<Status>OK</Status>") || !strings.Contains(modernText, "<Event>ADD</Event>") {
		t.Fatalf("2014 Catalog NOTIFY body root:\n%s", modern)
	}

	items := []cascadeCatalogItem{
		{DeviceID: testExposedChannelID},
		{DeviceID: "34020000001320009999"},
	}
	filtered := filterCascadeCatalogNotifyItems(items, testExposedChannelID, gb10DeviceID)
	if len(filtered) != 1 || filtered[0].DeviceID != testExposedChannelID {
		t.Fatalf("partial Catalog notify items = %+v", filtered)
	}
}

func TestPrepareCascadeCatalogNotifyItemsUses2011FullSnapshot(t *testing.T) {
	current := []cascadeCatalogItem{
		{DeviceID: "34020000001320000003", Status: "ON", Event: "UPDATE"},
		{DeviceID: "34020000001320000004", Status: "OFF", Event: "UPDATE"},
	}
	items, changed := prepareCascadeCatalogNotifyItemsForVersion(GBVersion10, nil, current, true)
	if !changed || len(items) != 2 || items[0].Event != "" || items[1].Event != "" {
		t.Fatalf("2011 initial Catalog snapshot = %+v, changed=%v", items, changed)
	}
	if current[0].Event != "UPDATE" || current[1].Event != "UPDATE" {
		t.Fatalf("2011 Catalog preparation mutated input: %+v", current)
	}

	previous := catalogSnapshot(current)
	if items, changed = prepareCascadeCatalogNotifyItemsForVersion(GBVersion10, previous, current, false); changed || items != nil {
		t.Fatalf("unchanged 2011 Catalog = %+v, changed=%v", items, changed)
	}

	updated := []cascadeCatalogItem{{DeviceID: current[0].DeviceID, Status: "OFF"}}
	items, changed = prepareCascadeCatalogNotifyItemsForVersion(GBVersion10, previous, updated, false)
	if !changed || len(items) != 1 || items[0].DeviceID != updated[0].DeviceID || items[0].Event != "" {
		t.Fatalf("changed 2011 Catalog snapshot = %+v, changed=%v", items, changed)
	}

	items, changed = prepareCascadeCatalogNotifyItemsForVersion(GBVersion11, nil, current, true)
	if !changed || len(items) != 1 || items[0].DeviceID != current[1].DeviceID || items[0].Event != "OFF" {
		t.Fatalf("2014 initial Catalog delta = %+v, changed=%v", items, changed)
	}
}

func TestCascadeCatalogNotifyFiltersDirectorySubscriptionScopes(t *testing.T) {
	groupID := "34020000002150000001"
	virtualID := "34020000002160000001"
	systemID := "34020000002000000002"
	items := []cascadeCatalogItem{
		{DeviceID: "650102"},
		{DeviceID: systemID},
		{DeviceID: groupID, ParentID: systemID},
		{DeviceID: virtualID, BusinessGroupID: groupID},
		{DeviceID: "34020000001320000001", CivilCode: "65010211", ParentID: systemID},
		{DeviceID: "34020000001320000002", CivilCode: "65010212", BusinessGroupID: groupID},
		{DeviceID: "34020000001320000003", CivilCode: "34020000", ParentID: groupID + "/" + virtualID},
		{DeviceID: "34020000001320000004", CivilCode: "33010000"},
		{DeviceID: "34020000002000000003", CivilCode: "65010200", ParentID: systemID},
		{DeviceID: "34020000001320000005", CivilCode: "65010213", ParentID: "34020000002000000003"},
	}
	tests := []struct {
		name   string
		target string
		want   []string
	}{
		{name: "administrative", target: "650102", want: []string{items[0].DeviceID, items[4].DeviceID, items[5].DeviceID, items[9].DeviceID}},
		{name: "system subtree", target: systemID, want: []string{items[1].DeviceID, items[2].DeviceID, items[3].DeviceID, items[4].DeviceID, items[5].DeviceID, items[6].DeviceID, items[8].DeviceID, items[9].DeviceID}},
		{name: "business group", target: groupID, want: []string{items[2].DeviceID, items[3].DeviceID, items[5].DeviceID, items[6].DeviceID}},
		{name: "virtual organization", target: virtualID, want: []string{items[3].DeviceID, items[6].DeviceID}},
		{name: "device subtree", target: items[8].DeviceID, want: []string{items[8].DeviceID, items[9].DeviceID}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filtered := filterCascadeCatalogNotifyItems(items, test.target, gb10PlatformID)
			got := make([]string, len(filtered))
			for index := range filtered {
				got[index] = filtered[index].DeviceID
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("filtered Catalog items = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDiffCascadeCatalogNotifyItems(t *testing.T) {
	previous := catalogSnapshot([]cascadeCatalogItem{
		{DeviceID: "34020000001320000001", Name: "camera-1", Status: "ON"},
		{DeviceID: "34020000001320000002", Name: "camera-2", Status: "OFF"},
	})
	current := []cascadeCatalogItem{
		{DeviceID: "34020000001320000001", Name: "camera-1", Status: "OFF"},
		{DeviceID: "34020000001320000003", Name: "camera-3", Status: "ON"},
	}
	changes := diffCascadeCatalogNotifyItems(previous, current)
	if len(changes) != 3 {
		t.Fatalf("Catalog delta = %+v", changes)
	}
	events := make(map[string]string, len(changes))
	for _, item := range changes {
		events[item.DeviceID] = item.Event
	}
	if events["34020000001320000001"] != "OFF" || events["34020000001320000002"] != "DEL" || events["34020000001320000003"] != "ADD" {
		t.Fatalf("Catalog delta events = %+v", events)
	}
	if unchanged := diffCascadeCatalogNotifyItems(catalogSnapshot(current), current); len(unchanged) != 0 {
		t.Fatalf("unchanged Catalog produced delta = %+v", unchanged)
	}

	updated := append([]cascadeCatalogItem(nil), current...)
	updated[0].Name = "renamed"
	changes = diffCascadeCatalogNotifyItems(catalogSnapshot(current), updated)
	if len(changes) != 1 || changes[0].Event != "UPDATE" || changes[0].DeviceID != updated[0].DeviceID {
		t.Fatalf("Catalog metadata delta = %+v", changes)
	}
}

func TestCascadeCatalogNotifyChunkCountsMatch(t *testing.T) {
	items := make([]cascadeCatalogItem, 20)
	for index := range items {
		items[index].DeviceID = fmt.Sprintf("3402000000132%07d", index)
	}
	notify := newCascadeCatalogNotify("34020000002000000001", 8, "", len(items), items)
	if notify.SumNum != len(items) || notify.DeviceList.Num != len(items) {
		t.Fatalf("Catalog NOTIFY chunk counts = SumNum %d, Num %d", notify.SumNum, notify.DeviceList.Num)
	}
}

func TestCascadeCatalogNotifyDeltaLifecycle(t *testing.T) {
	adapter, device, _ := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	extraChannels := make([]*ipc.Channel, 0, 20)
	for index := 0; index < 20; index++ {
		localID := fmt.Sprintf("3402000000132%07d", index+100)
		exposedID := fmt.Sprintf("3402000000139%07d", index+100)
		channel := &ipc.Channel{
			ID: fmt.Sprintf("GBC_catalog_%02d", index), DID: device.ID, DeviceID: device.DeviceID,
			ChannelID: localID, Name: fmt.Sprintf("Camera %02d", index), Type: ipc.TypeGB28181,
		}
		if err := adapter.Store().Channel().Create(t.Context(), channel); err != nil {
			t.Fatal(err)
		}
		extraChannels = append(extraChannels, channel)
		platform.sharedChannels = append(platform.sharedChannels, localID)
		platform.channelIDMap[localID] = exposedID
		platform.exposedChannelMap[exposedID] = localID
	}

	worker := newCascadeWorker(nil, platform)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	api := &GB28181API{core: adapter}
	visibleChannels, err := api.loadCascadeChannels(t.Context(), worker.platform)
	if err != nil {
		t.Fatal(err)
	}
	visibleItems := buildCascadeCatalogItems(visibleChannels, worker.platform, worker.protocolVersion())
	previousItems := append([]cascadeCatalogItem(nil), visibleItems...)
	for index := range previousItems {
		if previousItems[index].Status == "ON" {
			previousItems[index].Status = "OFF"
		} else {
			previousItems[index].Status = "ON"
		}
	}
	sub := newCascadeCatalogTestSubscription(t, worker)
	sub.CatalogSnapshot = catalogSnapshot(previousItems)

	requests := make([]*sip.Request, 0, 4)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	if err := api.sendCascadeCatalogNotify(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("Catalog delta chunks = %d, want 2", len(requests))
	}
	for index, request := range requests {
		body := string(request.Body())
		wantChunkCount := 20
		if index == 1 {
			wantChunkCount = 1
		}
		for _, expected := range []string{
			"<SumNum>21</SumNum>",
			fmt.Sprintf(`<DeviceList Num="%d">`, wantChunkCount),
		} {
			if !strings.Contains(body, expected) {
				t.Fatalf("Catalog delta chunk %d missing %q: %s", index, expected, body)
			}
		}
	}
	if events := strings.Count(string(requests[0].Body()), "<Event>") + strings.Count(string(requests[1].Body()), "<Event>"); events != 21 {
		t.Fatalf("Catalog delta event count = %d, want 21", events)
	}
	if err := api.sendCascadeCatalogNotify(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("unchanged Catalog sent %d extra chunks", len(requests)-2)
	}

	deleted := extraChannels[0]
	deletedExposedID := worker.platform.channelIDMap[deleted.ChannelID]
	if err := adapter.Store().Channel().Delete(t.Context(), deleted, orm.Where("id = ?", deleted.ID)); err != nil {
		t.Fatal(err)
	}
	if err := api.sendCascadeCatalogNotify(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 {
		t.Fatalf("Catalog delete chunks = %d, want 1", len(requests)-2)
	}
	deleteBody := string(requests[2].Body())
	for _, expected := range []string{"<Event>DEL</Event>", "<DeviceID>" + deletedExposedID + "</DeviceID>", "<SumNum>1</SumNum>", `<DeviceList Num="1">`} {
		if !strings.Contains(deleteBody, expected) {
			t.Fatalf("Catalog delete missing %q: %s", expected, deleteBody)
		}
	}

	addedLocalID := "34020000001320000888"
	addedExposedID := "34020000001320009888"
	added := &ipc.Channel{
		ID: "GBC_catalog_added", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: addedLocalID, Name: "Added camera", Type: ipc.TypeGB28181, IsOnline: true,
	}
	if err := adapter.Store().Channel().Create(t.Context(), added); err != nil {
		t.Fatal(err)
	}
	worker.platform.sharedChannels = append(worker.platform.sharedChannels, addedLocalID)
	worker.platform.channelIDMap[addedLocalID] = addedExposedID
	worker.platform.exposedChannelMap[addedExposedID] = addedLocalID
	worker.exchange = func(context.Context, *sip.Request) (*sip.Response, error) {
		return nil, errors.New("upstream unavailable")
	}
	if err := api.sendCascadeCatalogNotify(t.Context(), sub); err == nil {
		t.Fatal("failed Catalog NOTIFY unexpectedly succeeded")
	}
	sub.mu.Lock()
	_, committedAfterFailure := sub.CatalogSnapshot[addedExposedID]
	sub.mu.Unlock()
	if committedAfterFailure {
		t.Fatal("failed Catalog NOTIFY committed its snapshot")
	}
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	if err := api.sendCascadeCatalogNotify(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 4 || !strings.Contains(string(requests[3].Body()), "<Event>ADD</Event>") || !strings.Contains(string(requests[3].Body()), "<DeviceID>"+addedExposedID+"</DeviceID>") {
		t.Fatalf("Catalog retry did not resend ADD: %+v", requests)
	}
}

func TestCascadeCatalogNotifyRetriesOnlyFailedChunk(t *testing.T) {
	adapter, device, _ := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	for index := 0; index < 20; index++ {
		localID := fmt.Sprintf("3402000000132%07d", index+200)
		exposedID := fmt.Sprintf("3402000000139%07d", index+200)
		channel := &ipc.Channel{
			ID: fmt.Sprintf("GBC_catalog_retry_%02d", index), DID: device.ID, DeviceID: device.DeviceID,
			ChannelID: localID, Name: fmt.Sprintf("Retry camera %02d", index), Type: ipc.TypeGB28181,
		}
		if err := adapter.Store().Channel().Create(t.Context(), channel); err != nil {
			t.Fatal(err)
		}
		platform.sharedChannels = append(platform.sharedChannels, localID)
		platform.channelIDMap[localID] = exposedID
		platform.exposedChannelMap[exposedID] = localID
	}

	worker := newCascadeWorker(nil, platform)
	t.Cleanup(worker.cancel)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	api := &GB28181API{
		core:                 adapter,
		eventNotifyRetryWait: func(int, *sip.Response) time.Duration { return 0 },
	}
	visibleChannels, err := api.loadCascadeChannels(t.Context(), worker.platform)
	if err != nil {
		t.Fatal(err)
	}
	visibleItems := buildCascadeCatalogItems(visibleChannels, worker.platform, worker.protocolVersion())
	previousItems := append([]cascadeCatalogItem(nil), visibleItems...)
	for index := range previousItems {
		if previousItems[index].Status == "ON" {
			previousItems[index].Status = "OFF"
		} else {
			previousItems[index].Status = "ON"
		}
	}
	sub := newCascadeCatalogTestSubscription(t, worker)
	sub.Key = "catalog-retry-failed-chunk"
	sub.CatalogSnapshot = catalogSnapshot(previousItems)
	api.eventSubscribers.Store(sub.Key, sub)

	bodies := make([]string, 0, 3)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		bodies = append(bodies, string(request.Body()))
		if len(bodies) == 2 {
			response := sip.NewResponseFromRequest("", request, http.StatusServiceUnavailable, "Service Unavailable", nil)
			response.AppendHeader(&sip.GenericHeader{HeaderName: "Retry-After", Contents: "0"})
			return response, nil
		}
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}

	if err = api.sendCascadeCatalogNotify(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 {
		t.Fatalf("Catalog NOTIFY requests = %d, want first chunk once and second chunk twice", len(bodies))
	}
	if bodies[0] == bodies[1] {
		t.Fatal("Catalog retry resent the already acknowledged first chunk")
	}
	if bodies[1] != bodies[2] {
		t.Fatal("Catalog retry did not preserve the failed second chunk payload")
	}
	if !strings.Contains(bodies[0], `<DeviceList Num="20">`) || !strings.Contains(bodies[1], `<DeviceList Num="1">`) {
		t.Fatalf("Catalog retry chunk sizes are invalid: first=%q second=%q", bodies[0], bodies[1])
	}
	sub.mu.Lock()
	actualSnapshot := sub.CatalogSnapshot
	sub.mu.Unlock()
	if !reflect.DeepEqual(actualSnapshot, catalogSnapshot(visibleItems)) {
		t.Fatal("successful Catalog chunk retry did not commit the complete snapshot")
	}
	if err = api.sendCascadeCatalogNotify(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 {
		t.Fatalf("committed Catalog snapshot sent %d duplicate requests", len(bodies)-3)
	}
}

func TestCascadeCatalogNotifyRetryExhaustionDetachesSubscriptionVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			adapter, _, _ := newCascadeMediaCore(t)
			worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
			t.Cleanup(worker.cancel)
			worker.mu.Lock()
			worker.effective = version
			worker.mu.Unlock()
			api := &GB28181API{
				core:                 adapter,
				eventNotifyRetryWait: func(int, *sip.Response) time.Duration { return 0 },
			}
			visibleChannels, err := api.loadCascadeChannels(t.Context(), worker.platform)
			if err != nil {
				t.Fatal(err)
			}
			visibleItems := buildCascadeCatalogItems(visibleChannels, worker.platform, worker.protocolVersion())
			previousItems := append([]cascadeCatalogItem(nil), visibleItems...)
			for index := range previousItems {
				previousItems[index].Status = "OFF"
			}
			sub := newCascadeCatalogTestSubscription(t, worker)
			sub.Key = "catalog-retry-exhaustion-" + string(version)
			sub.GBVersion = string(version)
			previousSnapshot := catalogSnapshot(previousItems)
			sub.CatalogSnapshot = previousSnapshot
			api.eventSubscribers.Store(sub.Key, sub)

			calls := 0
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				calls++
				return sip.NewResponseFromRequest("", request, http.StatusServiceUnavailable, "Service Unavailable", nil), nil
			}
			if err = api.sendCascadeCatalogNotify(t.Context(), sub); err == nil {
				t.Fatal("exhausted Catalog NOTIFY unexpectedly succeeded")
			}
			if calls != eventNotifyMaxAttempts {
				t.Fatalf("Catalog retry calls = %d, want %d", calls, eventNotifyMaxAttempts)
			}
			if _, exists := api.eventSubscribers.Load(sub.Key); exists {
				t.Fatal("exhausted Catalog NOTIFY retained the failed subscription")
			}
			sub.mu.Lock()
			actualSnapshot := sub.CatalogSnapshot
			sub.mu.Unlock()
			if !reflect.DeepEqual(actualSnapshot, previousSnapshot) {
				t.Fatal("exhausted Catalog NOTIFY committed a new snapshot")
			}
		})
	}
}

func TestCascadeCatalogNotifyRetryStopsAfterRenewalVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			adapter, _, _ := newCascadeMediaCore(t)
			worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
			t.Cleanup(worker.cancel)
			worker.mu.Lock()
			worker.effective = version
			worker.mu.Unlock()
			firstAttempt := make(chan struct{})
			api := &GB28181API{
				core:                 adapter,
				eventNotifyRetryWait: func(int, *sip.Response) time.Duration { return 50 * time.Millisecond },
			}
			visibleChannels, err := api.loadCascadeChannels(t.Context(), worker.platform)
			if err != nil {
				t.Fatal(err)
			}
			visibleItems := buildCascadeCatalogItems(visibleChannels, worker.platform, worker.protocolVersion())
			previousItems := append([]cascadeCatalogItem(nil), visibleItems...)
			for index := range previousItems {
				previousItems[index].Status = "OFF"
			}
			sub := newCascadeCatalogTestSubscription(t, worker)
			sub.Key = "catalog-retry-renewal-" + string(version)
			sub.GBVersion = string(version)
			sub.RemoteCSeq = 1
			previousSnapshot := catalogSnapshot(previousItems)
			sub.CatalogSnapshot = previousSnapshot
			api.eventSubscribers.Store(sub.Key, sub)

			calls := 0
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				calls++
				if calls == 1 {
					close(firstAttempt)
				}
				return sip.NewResponseFromRequest("", request, http.StatusServiceUnavailable, "Service Unavailable", nil), nil
			}
			done := make(chan error, 1)
			go func() { done <- api.sendCascadeCatalogNotify(t.Context(), sub) }()
			select {
			case <-firstAttempt:
			case <-time.After(time.Second):
				t.Fatal("first Catalog NOTIFY attempt did not start")
			}
			sub.mu.Lock()
			sub.RemoteCSeq = 2
			sub.ExpiresAt = time.Now().Add(time.Hour)
			sub.mu.Unlock()
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("stale Catalog retry did not stop")
			}
			if calls != 1 {
				t.Fatalf("old Catalog subscription generation retry calls = %d, want 1", calls)
			}
			if current, exists := api.eventSubscribers.Load(sub.Key); !exists || current != sub {
				t.Fatal("old Catalog retry removed the renewed subscription")
			}
			sub.mu.Lock()
			actualSnapshot := sub.CatalogSnapshot
			sub.mu.Unlock()
			if !reflect.DeepEqual(actualSnapshot, previousSnapshot) {
				t.Fatal("old Catalog retry committed its snapshot after renewal")
			}
		})
	}
}

func TestCascadeCatalogNotifySerializesConcurrentSnapshotUpdates(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	api := &GB28181API{core: adapter}
	sub := newCascadeCatalogTestSubscription(t, worker)
	sub.CatalogSnapshot = catalogSnapshot([]cascadeCatalogItem{{DeviceID: testExposedChannelID, Status: "OFF"}})

	firstRequest := make(chan struct{})
	releaseFirst := make(chan struct{})
	var requestMu sync.Mutex
	requestCount := 0
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requestMu.Lock()
		requestCount++
		current := requestCount
		requestMu.Unlock()
		if current == 1 {
			close(firstRequest)
			<-releaseFirst
		}
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}

	errs := make(chan error, 2)
	go func() { errs <- api.sendCascadeCatalogNotify(t.Context(), sub) }()
	<-firstRequest
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		errs <- api.sendCascadeCatalogNotify(t.Context(), sub)
	}()
	<-secondStarted
	close(releaseFirst)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	requestMu.Lock()
	defer requestMu.Unlock()
	if requestCount != 1 {
		t.Fatalf("concurrent Catalog updates sent %d duplicate NOTIFY requests", requestCount)
	}
}

func TestCascadeCatalogNotifyDoesNotCommitSnapshotAfterRenewalFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			adapter, _, _ := newCascadeMediaCore(t)
			worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
			t.Cleanup(worker.cancel)
			worker.mu.Lock()
			worker.effective = version
			worker.mu.Unlock()
			api := &GB28181API{core: adapter}
			visibleChannels, err := api.loadCascadeChannels(t.Context(), worker.platform)
			if err != nil {
				t.Fatal(err)
			}
			visibleItems := buildCascadeCatalogItems(visibleChannels, worker.platform, worker.protocolVersion())
			previousItems := append([]cascadeCatalogItem(nil), visibleItems...)
			for index := range previousItems {
				if previousItems[index].Status == "ON" {
					previousItems[index].Status = "OFF"
				} else {
					previousItems[index].Status = "ON"
				}
			}
			sub := newCascadeCatalogTestSubscription(t, worker)
			sub.Key = "catalog-renewal-" + string(version)
			sub.GBVersion = string(version)
			sub.RemoteCSeq = 1
			sub.CatalogSnapshot = catalogSnapshot(previousItems)
			api.eventSubscribers.Store(sub.Key, sub)

			started := make(chan struct{})
			release := make(chan struct{})
			worker.exchange = func(ctx context.Context, request *sip.Request) (*sip.Response, error) {
				close(started)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-release:
					return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
				}
			}
			done := make(chan error, 1)
			go func() { done <- api.sendCascadeCatalogNotify(t.Context(), sub) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("Catalog NOTIFY did not start")
			}

			unlock, err := api.lockEventSubscriptionOperation(t.Context(), sub.Key)
			if err != nil {
				t.Fatal(err)
			}
			sub.mu.Lock()
			sub.RemoteCSeq = 2
			sub.mu.Unlock()
			unlock()
			close(release)
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Catalog NOTIFY did not finish")
			}
			sub.mu.Lock()
			actualSnapshot := sub.CatalogSnapshot
			sub.mu.Unlock()
			if !reflect.DeepEqual(actualSnapshot, catalogSnapshot(previousItems)) {
				t.Fatal("old Catalog NOTIFY committed its snapshot after subscription renewal")
			}
		})
	}
}

func newCascadeCatalogTestSubscription(t *testing.T, worker *cascadeWorker) *eventSubscription {
	t.Helper()
	remoteURI, err := sip.ParseSipURI("sip:" + gb10PlatformID + "@remote.example")
	if err != nil {
		t.Fatal(err)
	}
	localURI, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@local.example")
	if err != nil {
		t.Fatal(err)
	}
	remoteContactURI, err := sip.ParseSipURI("sip:" + gb10PlatformID + "@192.0.2.30:5060")
	if err != nil {
		t.Fatal(err)
	}
	callID := sip.CallID("cascade-catalog-" + t.Name())
	subscribe := sip.NewRequest("", sip.MethodSubscribe, &localURI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().
			SetFrom(&sip.Address{URI: &remoteURI, Params: sip.NewParams().Add("tag", sip.String{Str: "remote-tag"})}).
			SetTo(&sip.Address{URI: &localURI, Params: sip.NewParams()}).
			SetContact(&sip.Address{URI: &remoteContactURI, Params: sip.NewParams()}).
			SetMethod(sip.MethodSubscribe).
			SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).
			Build(), nil)
	return &eventSubscription{
		CmdType: "Catalog", DeviceID: gb10DeviceID, Event: "Catalog;id=" + gb10DeviceID,
		ExpiresAt: time.Now().Add(time.Hour), To: &sip.Address{URI: &remoteContactURI, Params: sip.NewParams()},
		GBVersion: string(GBVersion11), DialogRequest: subscribe,
		Response: sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil),
		Contact:  worker.contactAddress(), Cascade: worker,
	}
}

func TestCascadeSendMessageUsesRegisteredPeerAddressing(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	var request *sip.Request
	worker.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		request = in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}
	if err := sendCascadeXML(t.Context(), worker, cascadeDeviceStatusResponse{
		CmdType: "DeviceStatus", SN: 8, DeviceID: gb10DeviceID, Result: "OK",
	}); err != nil {
		t.Fatal(err)
	}
	if request == nil || request.Method() != sip.MethodMessage || request.Recipient().String() != "sip:"+gb10PlatformID+"@remote.example" {
		t.Fatalf("cascade MESSAGE request = %#v", request)
	}
	from, _ := request.From()
	to, _ := request.To()
	if from == nil || from.Address.String() != "sip:"+gb10DeviceID+"@local.example" || to == nil || to.Address.String() != "sip:"+gb10PlatformID+"@remote.example" {
		t.Fatalf("cascade MESSAGE From/To = %v / %v", from, to)
	}
	if headers := request.GetHeaders("X-GB-Ver"); len(headers) != 1 || !strings.Contains(headers[0].String(), "1.1") {
		t.Fatalf("cascade MESSAGE X-GB-Ver = %v", headers)
	}
}

func TestCascadeUnsupportedQueryDoesNotEmitInvalidBusinessResponse(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	var request *sip.Request
	worker.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		request = in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{}
	api.respondCascadeQuery(worker, cascadeQueryEnvelope{CmdType: "SDCardStatus", SN: 9, DeviceID: gb10DeviceID})
	if request != nil {
		t.Fatalf("unsupported cascade query emitted a non-standard business response: %s", request.Body())
	}
}

func TestCascadeExtendedQueryVersionMatrix(t *testing.T) {
	tests := []struct {
		name    string
		cmdType string
		version GBProtocolVersion
		allowed bool
	}{
		{name: "2011 preset", cmdType: "PresetQuery", version: GBVersion10},
		{name: "2011 alarm", cmdType: "Alarm", version: GBVersion10, allowed: true},
		{name: "2014 alarm", cmdType: "Alarm", version: GBVersion11, allowed: true},
		{name: "2016 alarm", cmdType: "Alarm", version: GBVersion20, allowed: true},
		{name: "2022 alarm", cmdType: "Alarm", version: GBVersion30, allowed: true},
		{name: "2014 preset", cmdType: "PresetQuery", version: GBVersion11, allowed: true},
		{name: "2011 config", cmdType: "ConfigDownload", version: GBVersion10},
		{name: "2014 config", cmdType: "ConfigDownload", version: GBVersion11, allowed: true},
		{name: "2014 home position", cmdType: "HomePositionQuery", version: GBVersion11},
		{name: "2016 home position", cmdType: "HomePositionQuery", version: GBVersion20},
		{name: "2022 home position", cmdType: "HomePositionQuery", version: GBVersion30, allowed: true},
		{name: "2014 mobile position", cmdType: "MobilePosition", version: GBVersion11},
		{name: "2016 mobile position", cmdType: "MobilePosition", version: GBVersion20, allowed: true},
		{name: "2016 precise PTZ", cmdType: "PTZPosition", version: GBVersion20},
		{name: "2022 precise PTZ", cmdType: "PTZPosition", version: GBVersion30, allowed: true},
		{name: "2016 SD card", cmdType: "SDCardStatus", version: GBVersion20},
		{name: "2022 SD card", cmdType: "SDCardStatus", version: GBVersion30, allowed: true},
		{name: "2016 cruise track list", cmdType: "CruiseTrackListQuery", version: GBVersion20},
		{name: "2022 cruise track list", cmdType: "CruiseTrackListQuery", version: GBVersion30, allowed: true},
		{name: "2016 cruise track", cmdType: "CruiseTrackQuery", version: GBVersion20},
		{name: "2022 cruise track", cmdType: "CruiseTrackQuery", version: GBVersion30, allowed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := cascadeExtendedQueryAction(tt.cmdType, tt.version)
			if got != tt.allowed {
				t.Fatalf("cascadeExtendedQueryAction(%q, %q) allowed = %v, want %v", tt.cmdType, tt.version, got, tt.allowed)
			}
		})
	}
}

func TestCascadeConfigDownloadTypeVersionMatrix(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		version GBProtocolVersion
		want    string
		allowed bool
	}{
		{name: "2011 base", value: "BasicParam", version: GBVersion10},
		{name: "2014 base", value: "BasicParam/VideoParamOpt", version: GBVersion11, want: "BasicParam/VideoParamOpt", allowed: true},
		{name: "2016 removed 2014 audio", value: "AudioParamConfig", version: GBVersion20},
		{name: "2016 base", value: "BasicParam/SVACEncodeConfig", version: GBVersion20, want: "BasicParam/SVACEncodeConfig", allowed: true},
		{name: "2022 removed 2014 video", value: "VideoParamConfig", version: GBVersion30},
		{name: "2016 extension", value: "VideoRecordPlan", version: GBVersion20},
		{name: "2022 extension", value: "video_record_plan/snapshot", version: GBVersion30, want: "VideoRecordPlan/SnapShotConfig", allowed: true},
		{name: "unknown", value: "VendorConfig", version: GBVersion30},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, allowed := cascadeConfigDownloadType(test.value, test.version)
			if got != test.want || allowed != test.allowed {
				t.Fatalf("cascadeConfigDownloadType(%q, %s) = %q, %v; want %q, %v", test.value, test.version, got, allowed, test.want, test.allowed)
			}
		})
	}
}

func TestCascadeConfigDownloadForwardsAllResponses(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()

	var downstream *DeviceQueryInput
	first := `<Response><CmdType>ConfigDownload</CmdType><SN>4321</SN><DeviceID>` + channel.ChannelID + `</DeviceID><Result>OK</Result><BasicParam><Name>IPC</Name><DeviceID>` + channel.ChannelID + `</DeviceID><SIPServerID>` + gb10PlatformID + `</SIPServerID><SIPServerIP>192.0.2.20</SIPServerIP><SIPServerPort>5060</SIPServerPort><DomainName>3402000000</DomainName><Expiration>3600</Expiration><Password>secret</Password><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam></Response>`
	second := `<Response><CmdType>ConfigDownload</CmdType><SN>4321</SN><DeviceID>` + channel.ChannelID + `</DeviceID><Result>OK</Result><VideoParamOpt><VideoFormatOpt>H.264/H.265</VideoFormatOpt><ResolutionOpt>720P/1080P</ResolutionOpt><FrameRateOpt>25/30</FrameRateOpt><BitRateTypeOpt>0/1</BitRateTypeOpt><VideoBitRateOpt>1024/2048</VideoBitRateOpt><DownloadSpeedOpt>1/2</DownloadSpeedOpt></VideoParamOpt></Response>`
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		copyInput := *input
		downstream = &copyInput
		return &DeviceQueryOutput{
			SN: 4321, CmdType: "ConfigDownload", DeviceID: channel.ChannelID, Result: "OK", XML: second,
			responseXML: []string{first, second},
		}, nil
	}
	var requests []*sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}

	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "ConfigDownload", SN: 97, DeviceID: testExposedChannelID,
		ConfigType: "basic_param/video_param_opt",
	})
	if downstream == nil || downstream.Action != deviceQueryActionConfigDownload || downstream.ConfigType != "BasicParam/VideoParamOpt" || downstream.TargetID != channel.ChannelID {
		t.Fatalf("downstream ConfigDownload = %+v", downstream)
	}
	if len(requests) != 2 {
		t.Fatalf("ConfigDownload cascade responses = %d, want 2", len(requests))
	}
	for index, request := range requests {
		body := string(request.Body())
		for _, expected := range []string{"<CmdType>ConfigDownload</CmdType>", "<SN>97</SN>", "<DeviceID>" + testExposedChannelID + "</DeviceID>"} {
			if !strings.Contains(body, expected) {
				t.Fatalf("ConfigDownload response %d missing %q: %s", index, expected, body)
			}
		}
		if strings.Contains(body, channel.ChannelID) || strings.Contains(body, "<SN>4321</SN>") {
			t.Fatalf("ConfigDownload response %d leaked downstream identity: %s", index, body)
		}
	}
	if !strings.Contains(string(requests[0].Body()), "<BasicParam>") || !strings.Contains(string(requests[1].Body()), "<VideoParamOpt>") {
		t.Fatalf("ConfigDownload response order/content = %s / %s", requests[0].Body(), requests[1].Body())
	}
}

func TestCascadeExtendedQueryUsesDownstreamSNAndRewritesResponse(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()

	var downstream *DeviceQueryInput
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		copyInput := *input
		downstream = &copyInput
		return &DeviceQueryOutput{
			SN: 4321, CmdType: "PresetQuery", DeviceID: channel.ChannelID, Result: "OK",
			XML: `<?xml version="1.0" encoding="UTF-8"?><Response><CmdType>PresetQuery</CmdType><SN>4321</SN><DeviceID>` + channel.ChannelID + `</DeviceID><PresetList Num="1"><Item><PresetID>1</PresetID><PresetName>Gate</PresetName></Item></PresetList></Response>`,
		}, nil
	}
	var request *sip.Request
	worker.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		request = in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}

	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "PresetQuery", SN: 97, DeviceID: testExposedChannelID,
	})
	if downstream == nil {
		t.Fatal("extended cascade query was not forwarded downstream")
	}
	if downstream.DeviceID != channel.DeviceID || downstream.TargetID != channel.ChannelID || downstream.Action != deviceQueryActionPresetQuery {
		t.Fatalf("downstream extended query = %+v", downstream)
	}
	if request == nil {
		t.Fatal("extended cascade query did not receive a response")
	}
	body := string(request.Body())
	for _, expected := range []string{
		"<CmdType>PersetQuery</CmdType>", "<SN>97</SN>",
		"<DeviceID>" + testExposedChannelID + "</DeviceID>",
		"<PresetID>1</PresetID>", "<PresetName>Gate</PresetName>",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("rewritten cascade response missing %q: %s", expected, body)
		}
	}
	for _, forbidden := range []string{"<SN>4321</SN>", channel.ChannelID} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("rewritten cascade response leaked %q: %s", forbidden, body)
		}
	}
}

func TestCascadeForwardedQueryRespectsDisabledDeviceCapabilitiesBeforeDownstreamCall(t *testing.T) {
	tests := []struct {
		name       string
		version    GBProtocolVersion
		disabled   string
		query      cascadeQueryEnvelope
		wantRoutes int
	}{
		{
			name: "preset query", version: GBVersion11, disabled: "preset_query",
			query: cascadeQueryEnvelope{CmdType: "PresetQuery", SN: 201, DeviceID: testExposedChannelID},
		},
		{
			name: "config query", version: GBVersion30, disabled: "config_query",
			query: cascadeQueryEnvelope{CmdType: "ConfigDownload", SN: 202, DeviceID: testExposedChannelID, ConfigType: "BasicParam"},
		},
		{
			name: "snapshot query", version: GBVersion30, disabled: "snapshot",
			query: cascadeQueryEnvelope{CmdType: "ConfigDownload", SN: 203, DeviceID: testExposedChannelID, ConfigType: "SnapShotConfig"},
		},
		{
			name: "mobile position", version: GBVersion20, disabled: "mobile_position",
			query: cascadeQueryEnvelope{CmdType: "MobilePosition", SN: 204, DeviceID: testExposedChannelID, Interval: 5},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, device, _ := newCascadeMediaCore(t)
			memory := newFlowMemory(device.DeviceID)
			memory.runtime.setGBProfile(test.version, []string{test.disabled})
			server := &Server{memoryStorer: memory}
			api := &GB28181API{core: adapter, svr: server}
			server.gb = api
			worker := newCascadeWorker(server, testSharedCascadePlatform(t))
			worker.effective = test.version
			t.Cleanup(worker.cancel)

			calls := 0
			api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
				calls++
				return &DeviceQueryOutput{CmdType: test.query.CmdType, DeviceID: input.TargetID}, nil
			}
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}

			api.respondCascadeQuery(worker, test.query)

			if calls != 0 {
				t.Fatalf("disabled %s reached downstream query %d times", test.disabled, calls)
			}
			if got := syncMapLen(&api.cascadeMobilePositionQueries); got != test.wantRoutes {
				t.Fatalf("disabled %s retained %d MobilePosition routes, want %d", test.disabled, got, test.wantRoutes)
			}
		})
	}
}

func TestCascadePresetQueryErrorUsesUpstreamVersionSpelling(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	var request *sip.Request
	worker.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		request = in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}
	if err := sendCascadeQueryError(t.Context(), worker, cascadeQueryEnvelope{
		CmdType: "PresetQuery", SN: 98, DeviceID: testExposedChannelID,
	}); err != nil {
		t.Fatal(err)
	}
	if request == nil || !strings.Contains(string(request.Body()), "<CmdType>PersetQuery</CmdType>") {
		t.Fatalf("2014 PresetQuery error response = %v", request)
	}
	body := string(request.Body())
	if !strings.Contains(body, `<PresetList Num="0"></PresetList>`) || strings.Contains(body, "<Result>") || strings.Contains(body, "<SumNum>") {
		t.Fatalf("2014 PresetQuery error response is not schema-compatible: %s", body)
	}
}

func TestCascadeQueryErrorResponsesMatchCommandSchemas(t *testing.T) {
	tests := []struct {
		name      string
		version   GBProtocolVersion
		query     cascadeQueryEnvelope
		required  []string
		forbidden []string
	}{
		{
			name: "2011 Catalog", version: GBVersion10,
			query:    cascadeQueryEnvelope{CmdType: "Catalog", SN: 1, DeviceID: gb10DeviceID},
			required: []string{"<SumNum>0</SumNum>", `<DeviceList Num="0"></DeviceList>`}, forbidden: []string{"<Result>"},
		},
		{
			name: "2014 Catalog", version: GBVersion11,
			query:    cascadeQueryEnvelope{CmdType: "Catalog", SN: 2, DeviceID: gb10DeviceID},
			required: []string{"<SumNum>0</SumNum>"}, forbidden: []string{"<Result>", "<DeviceList"},
		},
		{
			name: "2016 Catalog", version: GBVersion20,
			query:    cascadeQueryEnvelope{CmdType: "Catalog", SN: 3, DeviceID: gb10DeviceID},
			required: []string{"<SumNum>0</SumNum>"}, forbidden: []string{"<Result>", "<DeviceList"},
		},
		{
			name: "2022 Catalog", version: GBVersion30,
			query:    cascadeQueryEnvelope{CmdType: "Catalog", SN: 4, DeviceID: gb10DeviceID},
			required: []string{"<SumNum>0</SumNum>"}, forbidden: []string{"<Result>", "<DeviceList"},
		},
		{
			name: "DeviceInfo", version: GBVersion10,
			query:    cascadeQueryEnvelope{CmdType: "DeviceInfo", SN: 3, DeviceID: gb10DeviceID},
			required: []string{"<Result>ERROR</Result>"},
		},
		{
			name: "DeviceStatus", version: GBVersion20,
			query:    cascadeQueryEnvelope{CmdType: "DeviceStatus", SN: 4, DeviceID: gb10DeviceID},
			required: []string{"<Result>ERROR</Result>", "<Online>OFFLINE</Online>", "<Status>ERROR</Status>"},
		},
		{
			name: "2011 RecordInfo", version: GBVersion10,
			query:    cascadeQueryEnvelope{CmdType: "RecordInfo", SN: 5, DeviceID: gb10DeviceID},
			required: []string{"<Name>" + gb10DeviceID + "</Name>", "<SumNum>0</SumNum>", `<RecordList Num="0"></RecordList>`}, forbidden: []string{"<Result>"},
		},
		{
			name: "RecordInfo", version: GBVersion30,
			query:    cascadeQueryEnvelope{CmdType: "RecordInfo", SN: 5, DeviceID: gb10DeviceID},
			required: []string{"<Name>" + gb10DeviceID + "</Name>", "<SumNum>0</SumNum>"}, forbidden: []string{"<Result>", "<RecordList"},
		},
		{
			name: "2022 PresetQuery", version: GBVersion30,
			query:    cascadeQueryEnvelope{CmdType: "PresetQuery", SN: 6, DeviceID: gb10DeviceID},
			required: []string{"<SumNum>0</SumNum>", `<PresetList Num="0"></PresetList>`}, forbidden: []string{"<Result>"},
		},
		{
			name: "HomePositionQuery", version: GBVersion30,
			query:     cascadeQueryEnvelope{CmdType: "HomePositionQuery", SN: 7, DeviceID: gb10DeviceID},
			forbidden: []string{"<Result>", "<SumNum>"},
		},
		{
			name: "CruiseTrackListQuery", version: GBVersion30,
			query:    cascadeQueryEnvelope{CmdType: "CruiseTrackListQuery", SN: 8, DeviceID: gb10DeviceID},
			required: []string{"<SumNum>0</SumNum>"}, forbidden: []string{"<Result>"},
		},
		{
			name: "CruiseTrackQuery", version: GBVersion30,
			query:    cascadeQueryEnvelope{CmdType: "CruiseTrackQuery", SN: 9, DeviceID: gb10DeviceID, Number: intPointer(1)},
			required: []string{"<Number>1</Number>", "<SumNum>0</SumNum>"}, forbidden: []string{"<Result>"},
		},
		{
			name: "PTZPosition", version: GBVersion30,
			query:     cascadeQueryEnvelope{CmdType: "PTZPosition", SN: 10, DeviceID: gb10DeviceID},
			forbidden: []string{"<Result>", "<SumNum>"},
		},
		{
			name: "SDCardStatus", version: GBVersion30,
			query:    cascadeQueryEnvelope{CmdType: "SDCardStatus", SN: 11, DeviceID: gb10DeviceID},
			required: []string{"<SumNum>0</SumNum>"}, forbidden: []string{"<Result>"},
		},
		{
			name: "ConfigDownload", version: GBVersion11,
			query:    cascadeQueryEnvelope{CmdType: "ConfigDownload", SN: 12, DeviceID: gb10DeviceID},
			required: []string{"<Result>ERROR</Result>"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
			worker.mu.Lock()
			worker.effective = test.version
			worker.mu.Unlock()
			var request *sip.Request
			worker.exchange = func(_ context.Context, input *sip.Request) (*sip.Response, error) {
				request = input
				return sip.NewResponseFromRequest("", input, http.StatusOK, "OK", nil), nil
			}
			if err := sendCascadeQueryError(t.Context(), worker, test.query); err != nil {
				t.Fatal(err)
			}
			if request == nil {
				t.Fatal("query failure did not emit a schema-compatible response")
			}
			body := string(request.Body())
			for _, value := range append([]string{
				"<CmdType>" + gbQueryCmdTypeForVersion(test.query.CmdType, test.version) + "</CmdType>",
				"<DeviceID>" + test.query.DeviceID + "</DeviceID>",
			}, test.required...) {
				if !strings.Contains(body, value) {
					t.Fatalf("response missing %q: %s", value, body)
				}
			}
			for _, value := range test.forbidden {
				if strings.Contains(body, value) {
					t.Fatalf("response contains forbidden %q: %s", value, body)
				}
			}
		})
	}
}

func TestCascadeMobilePositionFailureDoesNotInventResponseSchema(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion20
	worker.mu.Unlock()
	sent := false
	worker.exchange = func(_ context.Context, input *sip.Request) (*sip.Response, error) {
		sent = true
		return sip.NewResponseFromRequest("", input, http.StatusOK, "OK", nil), nil
	}
	err := sendCascadeQueryError(t.Context(), worker, cascadeQueryEnvelope{CmdType: "MobilePosition", SN: 13, DeviceID: gb10DeviceID})
	if err == nil || sent {
		t.Fatalf("MobilePosition failure response = sent %v, err %v", sent, err)
	}
}

func TestCascadeCruiseTrackQueryForwardsNumberAndRewritesResponse(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()

	var downstream *DeviceQueryInput
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		copyInput := *input
		downstream = &copyInput
		return &DeviceQueryOutput{
			SN: 4322, CmdType: "CruiseTrackQuery", DeviceID: channel.ChannelID,
			XML: `<Response><CmdType>CruiseTrackQuery</CmdType><SN>4322</SN><DeviceID>` + channel.ChannelID + `</DeviceID><Number>1</Number><Name>夜间</Name><SumNum>1</SumNum><CruisePointList Num="1"><CruisePoint><PresetIndex>7</PresetIndex><StayTime>20</StayTime><Speed>8</Speed></CruisePoint></CruisePointList></Response>`,
		}, nil
	}
	var request *sip.Request
	worker.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		request = in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}

	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "CruiseTrackQuery", SN: 98, DeviceID: testExposedChannelID, Number: intPointer(1),
	})
	if downstream == nil || downstream.Action != deviceQueryActionCruiseTrack || downstream.Number != 1 || downstream.TargetID != channel.ChannelID {
		t.Fatalf("downstream cruise query = %+v", downstream)
	}
	if request == nil {
		t.Fatal("cruise track cascade query did not receive a response")
	}
	body := string(request.Body())
	for _, expected := range []string{"<CmdType>CruiseTrackQuery</CmdType>", "<SN>98</SN>", "<DeviceID>" + testExposedChannelID + "</DeviceID>", "<Number>1</Number>", "<PresetIndex>7</PresetIndex>"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("rewritten cruise response missing %q: %s", expected, body)
		}
	}
	if strings.Contains(body, channel.ChannelID) || strings.Contains(body, "<SN>4322</SN>") {
		t.Fatalf("rewritten cruise response leaked downstream identity: %s", body)
	}
}

func TestRewriteCascadeQueryResponseRejectsUnknownAppendixA4ID(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	body := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>6</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><Info><doorType><DoorID>34020000001320000099</DoorID></doorType></Info></Response>`)
	if _, err := rewriteCascadeQueryResponse(body, cascadeQueryEnvelope{CmdType: "DeviceStatus", SN: 81, DeviceID: testExposedChannelID}, platform, GBVersion30, channel); err == nil {
		t.Fatal("query response with unshared A.4 ID was forwarded")
	}
}

func TestRewriteCascadeDeviceStatusConvertsLegacyInfoFor2022(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	body := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>6</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Info>legacy</Info></Response>`)
	rewritten, err := rewriteCascadeQueryResponse(
		body, cascadeQueryEnvelope{CmdType: "DeviceStatus", SN: 81, DeviceID: testExposedChannelID},
		platform, GBVersion30, channel,
	)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rewritten)
	if !strings.Contains(text, `<ExtraInfo>legacy</ExtraInfo>`) || strings.Contains(text, `<Info>legacy</Info>`) {
		t.Fatalf("legacy DeviceStatus Info was not converted for 2022: %s", text)
	}
	if err := validateGenericQueryPayload(GBVersion30, "DeviceStatus", rewritten); err != nil {
		t.Fatalf("converted DeviceStatus response is invalid for 2022: %v", err)
	}
}

func TestRewriteCascadeDeviceInfoDropsRemovedCompatibilityFieldsFor2022(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	body := []byte(`<Response><CmdType>DeviceInfo</CmdType><SN>6</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><DeviceName>legacy camera</DeviceName><Result>OK</Result><DeviceType>IPC</DeviceType>` +
		`<Manufacturer>vendor</Manufacturer><Model>model</Model><Firmware>1.0</Firmware>` +
		`<MaxCamera>1</MaxCamera><MaxAlarm>2</MaxAlarm><Channel>1</Channel><Info>legacy</Info></Response>`)
	rewritten, err := rewriteCascadeQueryResponse(
		body, cascadeQueryEnvelope{CmdType: "DeviceInfo", SN: 81, DeviceID: testExposedChannelID},
		platform, GBVersion30, channel,
	)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rewritten)
	for _, removed := range []string{"<DeviceType>", "<MaxCamera>", "<MaxAlarm>"} {
		if strings.Contains(text, removed) {
			t.Fatalf("2022 DeviceInfo retained removed compatibility field %s: %s", removed, text)
		}
	}
	for _, expected := range []string{
		"<DeviceName>legacy camera</DeviceName>", "<Manufacturer>vendor</Manufacturer>",
		"<Model>model</Model>", "<Firmware>1.0</Firmware>", "<Channel>1</Channel>",
		"<ExtraInfo>legacy</ExtraInfo>",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("2022 DeviceInfo lost compatible field %s: %s", expected, text)
		}
	}
	if err := validateCascadeRewrittenQueryResponse(rewritten, "DeviceInfo", GBVersion30); err != nil {
		t.Fatalf("converted DeviceInfo response is invalid for 2022: %v", err)
	}
}

func TestRewriteCascadePresetQueryNormalizesLegacyResponseFor2022(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	body := []byte(`<Response><CmdType>PresetQuery</CmdType><SN>6</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><Result>OK</Result><PresetList Num="1"><Item><PresetID>1</PresetID>` +
		`<PresetName>入口</PresetName></Item></PresetList></Response>`)
	rewritten, err := rewriteCascadeQueryResponse(
		body, cascadeQueryEnvelope{CmdType: "PresetQuery", SN: 81, DeviceID: testExposedChannelID},
		platform, GBVersion30, channel,
	)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rewritten)
	if strings.Contains(text, "<Result>") || !strings.Contains(text, "<SumNum>1</SumNum>") {
		t.Fatalf("2022 PresetQuery did not normalize legacy envelope: %s", text)
	}
	if err := validateCascadeRewrittenQueryResponse(rewritten, "PresetQuery", GBVersion30); err != nil {
		t.Fatalf("converted PresetQuery response is invalid for 2022: %v", err)
	}
}

func TestRewriteCascadeConfigDownloadCanonicalizesSnapshotAlias(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>6</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><Result>OK</Result><SnapShotConfig><SnapNum>1</SnapNum><Interval>1</Interval><UploadURL>https://example.invalid/upload</UploadURL><SessionID>snapshot-session-0000000000000006</SessionID></SnapShotConfig></Response>`)
	rewritten, err := rewriteCascadeQueryResponse(
		body, cascadeQueryEnvelope{CmdType: "ConfigDownload", SN: 81, DeviceID: testExposedChannelID},
		platform, GBVersion30, channel,
	)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rewritten)
	if !strings.Contains(text, `<SnapShot>`) || strings.Contains(text, `<SnapShotConfig>`) {
		t.Fatalf("snapshot alias was not canonicalized: %s", text)
	}
	var response ConfigDownloadResponse
	if err := sip.XMLDecode(rewritten, &response); err != nil || response.SnapShot == nil || response.SnapShotConfig != nil {
		t.Fatalf("canonical ConfigDownload response = %+v, err = %v", response, err)
	}
}

func TestRewriteCascadeConfigDownloadConvertsBasicParamByTargetVersion(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	full2014 := `<BasicParam><Name>IPC</Name><DeviceID>` + testCascadeChannelID + `</DeviceID><SIPServerID>34020000002000000099</SIPServerID><SIPServerIP>192.0.2.10</SIPServerIP><SIPServerPort>5060</SIPServerPort><DomainName>3402000000</DomainName><Expiration>3600</Expiration><Password>secret</Password><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`
	full2016 := `<BasicParam><Name>IPC</Name><Expiration>3600</Expiration><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount><PositionCapability>2</PositionCapability><Longitude>116.397</Longitude><Latitude>39.908</Latitude></BasicParam>`
	tests := []struct {
		name      string
		version   GBProtocolVersion
		section   string
		required  []string
		forbidden []string
	}{
		{
			name: "2014 response to 2016 upstream", version: GBVersion20, section: full2014,
			required:  []string{"<Name>IPC</Name>", "<Expiration>3600</Expiration>", "<HeartBeatInterval>60</HeartBeatInterval>", "<HeartBeatCount>3</HeartBeatCount>"},
			forbidden: []string{"<DeviceID>" + testCascadeChannelID + "</DeviceID>", "<SIPServerID>", "<SIPServerIP>", "<SIPServerPort>", "<DomainName>", "<Password>"},
		},
		{
			name: "2016 response to 2022 upstream", version: GBVersion30, section: full2016,
			required:  []string{"<Name>IPC</Name>", "<Expiration>3600</Expiration>", "<HeartBeatInterval>60</HeartBeatInterval>", "<HeartBeatCount>3</HeartBeatCount>"},
			forbidden: []string{"<PositionCapability>", "<Longitude>", "<Latitude>"},
		},
		{
			name: "2014 response to 2014 upstream rewrites identities", version: GBVersion11, section: full2014,
			required: []string{
				"<BasicParam><Name>IPC</Name><DeviceID>" + testExposedChannelID + "</DeviceID>",
				"<SIPServerID>" + platform.localID + "</SIPServerID>",
			},
			forbidden: []string{"34020000002000000099", "<DeviceID>" + testCascadeChannelID + "</DeviceID>"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>6</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><Result>OK</Result>` + test.section + `</Response>`)
			rewritten, err := rewriteCascadeQueryResponse(
				body, cascadeQueryEnvelope{CmdType: "ConfigDownload", SN: 81, DeviceID: testExposedChannelID},
				platform, test.version, channel,
			)
			if err != nil {
				t.Fatal(err)
			}
			text := string(rewritten)
			for _, expected := range test.required {
				if !strings.Contains(text, expected) {
					t.Fatalf("rewritten ConfigDownload missing %q: %s", expected, text)
				}
			}
			for _, forbidden := range test.forbidden {
				if strings.Contains(text, forbidden) {
					t.Fatalf("rewritten ConfigDownload contains %q: %s", forbidden, text)
				}
			}
			if err := validateConfigDownloadResponseForVersion(rewritten, test.version); err != nil {
				t.Fatalf("rewritten ConfigDownload is invalid: %v", err)
			}
		})
	}
}

func TestRewriteCascadeConfigDownloadRejectsLossyVersionConversions(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	tests := []struct {
		name    string
		version GBProtocolVersion
		section string
	}{
		{
			name: "2022 partial BasicParam to 2016", version: GBVersion20,
			section: `<BasicParam><Name>IPC</Name></BasicParam>`,
		},
		{
			name: "2022 configuration to 2016", version: GBVersion20,
			section: `<VideoRecordPlan><RecordEnable>0</RecordEnable><RecordScheduleSumNum>0</RecordScheduleSumNum><StreamNumber>0</StreamNumber></VideoRecordPlan>`,
		},
		{
			name: "2014 VideoParamOpt to 2016", version: GBVersion20,
			section: `<VideoParamOpt><VideoFormatOpt>1/2</VideoFormatOpt><ResolutionOpt>1/2</ResolutionOpt><FrameRateOpt>25</FrameRateOpt><BitRateTypeOpt>1</BitRateTypeOpt><VideoBitRateOpt>2048</VideoBitRateOpt><DownloadSpeedOpt>1/2</DownloadSpeedOpt></VideoParamOpt>`,
		},
		{
			name: "ConfigDownload to 2011", version: GBVersion10,
			section: `<BasicParam><Name>IPC</Name></BasicParam>`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>6</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><Result>OK</Result>` + test.section + `</Response>`)
			if _, err := rewriteCascadeQueryResponse(
				body, cascadeQueryEnvelope{CmdType: "ConfigDownload", SN: 81, DeviceID: testExposedChannelID},
				platform, test.version, channel,
			); err == nil {
				t.Fatal("lossy ConfigDownload conversion was forwarded")
			}
		})
	}
}

func TestRewriteCascadeConfigDownloadConvertsVersionedExtensions(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	base := `<Response><CmdType>ConfigDownload</CmdType><SN>6</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><Result>OK</Result><BasicParam><Name>IPC</Name><Expiration>3600</Expiration><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`
	tests := []struct {
		name      string
		version   GBProtocolVersion
		extension string
		required  string
		forbidden string
	}{
		{name: "legacy Info to 2022 ExtraInfo", version: GBVersion30, extension: `<Info>legacy</Info>`, required: `<ExtraInfo>legacy</ExtraInfo>`, forbidden: `<Info>legacy</Info>`},
		{name: "misspelled 2022 extension is canonicalized", version: GBVersion30, extension: `<ExtralInfo>legacy</ExtralInfo>`, required: `<ExtraInfo>legacy</ExtraInfo>`, forbidden: `<ExtralInfo>`},
		{name: "structured Appendix A4 is removed for 2016", version: GBVersion20, extension: `<Info><doorType><DoorID>` + testCascadeChannelID + `</DoorID></doorType></Info>`, forbidden: `<doorType>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rewritten, err := rewriteCascadeQueryResponse(
				[]byte(base+test.extension+`</Response>`), cascadeQueryEnvelope{CmdType: "ConfigDownload", SN: 81, DeviceID: testExposedChannelID},
				platform, test.version, channel,
			)
			if err != nil {
				t.Fatal(err)
			}
			text := string(rewritten)
			if test.required != "" && !strings.Contains(text, test.required) {
				t.Fatalf("rewritten ConfigDownload missing %q: %s", test.required, text)
			}
			if test.forbidden != "" && strings.Contains(text, test.forbidden) {
				t.Fatalf("rewritten ConfigDownload contains %q: %s", test.forbidden, text)
			}
		})
	}
}

func TestRewriteCascadeDeviceInfoConvertsVersionedFields(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	tests := []struct {
		name      string
		version   GBProtocolVersion
		body      string
		required  []string
		forbidden []string
	}{
		{
			name: "2022 response to 2011 upstream", version: GBVersion10,
			body: `<Response><CmdType>DeviceInfo</CmdType><SN>6</SN><DeviceID>` + testCascadeChannelID +
				`</DeviceID><DeviceName>入口相机</DeviceName><Result>OK</Result><Channel>1</Channel>` +
				`<ExtraInfo>{"DeviceID":"` + testCascadeChannelID + `"}</ExtraInfo></Response>`,
			required: []string{
				`<DeviceID>` + testExposedChannelID + `</DeviceID>`,
				`<Info>{&#34;DeviceID&#34;:&#34;` + testExposedChannelID + `&#34;}</Info>`,
			},
			forbidden: []string{"<DeviceName>", "<ExtraInfo>"},
		},
		{
			name: "2011 response to 2022 upstream", version: GBVersion30,
			body: `<Response><CmdType>DeviceInfo</CmdType><SN>6</SN><DeviceID>` + testCascadeChannelID +
				`</DeviceID><Result>OK</Result><Channel>1</Channel><Info>legacy</Info></Response>`,
			required:  []string{"<ExtraInfo>legacy</ExtraInfo>"},
			forbidden: []string{"<Info>legacy</Info>"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rewritten, err := rewriteCascadeQueryResponse(
				[]byte(test.body), cascadeQueryEnvelope{CmdType: "DeviceInfo", SN: 81, DeviceID: testExposedChannelID},
				platform, test.version, channel,
			)
			if err != nil {
				t.Fatal(err)
			}
			text := string(rewritten)
			for _, expected := range test.required {
				if !strings.Contains(text, expected) {
					t.Fatalf("rewritten DeviceInfo missing %q: %s", expected, text)
				}
			}
			for _, forbidden := range test.forbidden {
				if strings.Contains(text, forbidden) {
					t.Fatalf("rewritten DeviceInfo contains %q: %s", forbidden, text)
				}
			}
		})
	}
}

func TestRewriteCascadeDeviceStatusConvertsVersionedFields(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	tests := []struct {
		name      string
		version   GBProtocolVersion
		body      string
		required  []string
		forbidden []string
	}{
		{
			name: "2022 response to 2016 upstream", version: GBVersion20,
			body: `<Response><CmdType>DeviceStatus</CmdType><SN>6</SN><DeviceID>` + testCascadeChannelID +
				`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status>` +
				`<Alarmstatus Num="1"><Item><DeviceID>` + testCascadeChannelID + `</DeviceID><DutyStatus>ONDUTY</DutyStatus></Item></Alarmstatus>` +
				`<Info><doorType><DoorID>` + testCascadeChannelID + `</DoorID></doorType></Info><ExtraInfo>legacy</ExtraInfo></Response>`,
			required: []string{
				`<Alarmstatus num="1">`, `<DeviceID>` + testExposedChannelID + `</DeviceID>`, `<Info>legacy</Info>`,
			},
			forbidden: []string{"<ExtraInfo>", "<doorType>"},
		},
		{
			name: "2011 response to 2022 upstream", version: GBVersion30,
			body: `<Response><CmdType>DeviceStatus</CmdType><SN>6</SN><DeviceID>` + testCascadeChannelID +
				`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status>` +
				`<Alarmstatus Num="1"><Item><DeviceID>` + testCascadeChannelID + `</DeviceID><Status>ONDUTY</Status></Item></Alarmstatus>` +
				`<Info>legacy</Info></Response>`,
			required:  []string{`<Alarmstatus Num="1">`, `<DutyStatus>ONDUTY</DutyStatus>`, `<ExtraInfo>legacy</ExtraInfo>`},
			forbidden: []string{"<Info>legacy</Info>", "<Status>ONDUTY</Status>"},
		},
		{
			name: "2022 response to 2014 upstream", version: GBVersion11,
			body: `<Response><CmdType>DeviceStatus</CmdType><SN>6</SN><DeviceID>` + testCascadeChannelID +
				`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status>` +
				`<Alarmstatus Num="1"><Item><DeviceID>` + testCascadeChannelID + `</DeviceID><DutyStatus>ONDUTY</DutyStatus></Item></Alarmstatus></Response>`,
			required:  []string{`<Alarmstatus Num="1">`, `<StatusDutyStatus>ONDUTY</StatusDutyStatus>`},
			forbidden: []string{"<DutyStatus>ONDUTY</DutyStatus>", "<Status>ONDUTY</Status>"},
		},
		{
			name: "2014 response to 2011 upstream", version: GBVersion10,
			body: `<Response><CmdType>DeviceStatus</CmdType><SN>6</SN><DeviceID>` + testCascadeChannelID +
				`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status>` +
				`<Alarmstatus Num="1"><Item><DeviceID>` + testCascadeChannelID + `</DeviceID><StatusDutyStatus>ONDUTY</StatusDutyStatus></Item></Alarmstatus></Response>`,
			required:  []string{`<Alarmstatus Num="1">`, `<DutyStatus>ONDUTY</DutyStatus>`},
			forbidden: []string{"<StatusDutyStatus>ONDUTY</StatusDutyStatus>", "<Status>ONDUTY</Status>"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rewritten, err := rewriteCascadeQueryResponse(
				[]byte(test.body), cascadeQueryEnvelope{CmdType: "DeviceStatus", SN: 81, DeviceID: testExposedChannelID},
				platform, test.version, channel,
			)
			if err != nil {
				t.Fatal(err)
			}
			text := string(rewritten)
			for _, expected := range test.required {
				if !strings.Contains(text, expected) {
					t.Fatalf("rewritten DeviceStatus missing %q: %s", expected, text)
				}
			}
			for _, forbidden := range test.forbidden {
				if strings.Contains(text, forbidden) {
					t.Fatalf("rewritten DeviceStatus contains %q: %s", forbidden, text)
				}
			}
		})
	}
}

func TestRewriteCascadeDeviceStatusPreservesAppendixA4StatusName(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	body := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>6</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status>` +
		`<Info><doorType><DoorID>` + testCascadeChannelID + `</DoorID><Status>OPEN</Status></doorType></Info></Response>`)
	rewritten, err := rewriteCascadeQueryResponse(
		body, cascadeQueryEnvelope{CmdType: "DeviceStatus", SN: 81, DeviceID: testExposedChannelID},
		platform, GBVersion30, channel,
	)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rewritten)
	if !strings.Contains(text, `<Status>OPEN</Status>`) || strings.Contains(text, `<DutyStatus>OPEN</DutyStatus>`) {
		t.Fatalf("Appendix A.4 Status was rewritten as alarm duty status: %s", text)
	}
}

func TestRewriteCascadeExtraInfoMapsNumericIdentifiersWithoutPrecisionLoss(t *testing.T) {
	platform := cascadePlatform{
		localID:           gb10PlatformID,
		channelIDMap:      map[string]string{testCascadeChannelID: testExposedChannelID},
		exposedChannelMap: map[string]string{testExposedChannelID: testCascadeChannelID},
	}
	const prefix = "  "
	const suffix = "\t "
	rewritten, err := rewriteCascadeOpaqueIdentifiers(
		prefix+`[{"type":"doorType","DeviceID":`+testCascadeChannelID+`,"Sequence":100,"Zero":0}]`+suffix,
		"ExtraInfo", platform, testCascadeChannelID, testExposedChannelID,
	)
	if err != nil {
		t.Fatalf("rewrite numeric ExtraInfo: %v", err)
	}
	if !strings.Contains(rewritten, `"DeviceID":`+testExposedChannelID) ||
		!strings.Contains(rewritten, `"Sequence":100`) || !strings.Contains(rewritten, `"Zero":0`) ||
		!strings.HasPrefix(rewritten, prefix) || !strings.HasSuffix(rewritten, suffix) {
		t.Fatalf("numeric ExtraInfo mapping = %s", rewritten)
	}

	const opaque = "  vendor payload  "
	if unchanged, err := rewriteCascadeOpaqueIdentifiers(opaque, "ExtraInfo", platform, testCascadeChannelID, testExposedChannelID); err != nil || unchanged != opaque {
		t.Fatalf("opaque ExtraInfo = %q, %v", unchanged, err)
	}

	if _, err := rewriteCascadeOpaqueIdentifiers(
		`{"type":"doorType","DeviceID":34020000001320000099}`,
		"ExtraInfo", platform, testCascadeChannelID, testExposedChannelID,
	); err == nil {
		t.Fatal("unknown numeric A.4 identifier was not rejected")
	}
}

func TestCascadeAlarmQueryForwardsFiltersAndRewritesResponseAcrossVersions(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardYear(), func(t *testing.T) {
			adapter, _, channel := newCascadeMediaCore(t)
			server := &Server{}
			api := &GB28181API{core: adapter, svr: server}
			server.gb = api
			worker := newCascadeWorker(server, testSharedCascadePlatform(t))
			worker.mu.Lock()
			worker.effective = version
			worker.mu.Unlock()

			query := cascadeQueryEnvelope{
				CmdType: "Alarm", SN: 108, DeviceID: testExposedChannelID,
				StartAlarmPriority: "1", EndAlarmPriority: "4", AlarmMethod: "25",
				StartAlarmTime: "2026-08-26T08:00:00", EndAlarmTime: "2026-08-26T09:00:00",
			}
			if version.AtLeast(GBVersion20) {
				query.AlarmType = "2"
			}
			var downstream *DeviceQueryInput
			api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
				copyInput := *input
				downstream = &copyInput
				return &DeviceQueryOutput{
					SN: 4321, CmdType: "Alarm", DeviceID: channel.ChannelID, Result: "OK",
					XML: `<Response><CmdType>Alarm</CmdType><SN>4321</SN><DeviceID>` + channel.ChannelID + `</DeviceID><Result>OK</Result></Response>`,
				}, nil
			}
			var response *sip.Request
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				response = request
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}

			api.respondCascadeQuery(worker, query)
			if downstream == nil || downstream.DeviceID != channel.DeviceID || downstream.TargetID != channel.ChannelID ||
				downstream.Action != deviceQueryActionAlarm || downstream.StartAlarmPriority != query.StartAlarmPriority ||
				downstream.EndAlarmPriority != query.EndAlarmPriority || downstream.AlarmMethod != query.AlarmMethod ||
				downstream.AlarmType != query.AlarmType || downstream.StartAlarmTime != query.StartAlarmTime ||
				downstream.EndAlarmTime != query.EndAlarmTime {
				t.Fatalf("downstream Alarm query = %+v", downstream)
			}
			if response == nil {
				t.Fatal("Alarm query did not send an upstream business response")
			}
			body := string(response.Body())
			for _, expected := range []string{
				"<CmdType>Alarm</CmdType>", "<SN>108</SN>",
				"<DeviceID>" + testExposedChannelID + "</DeviceID>", "<Result>OK</Result>",
			} {
				if !strings.Contains(body, expected) {
					t.Fatalf("rewritten %s Alarm response missing %q: %s", version.StandardName(), expected, body)
				}
			}
			if strings.Contains(body, channel.ChannelID) || strings.Contains(body, "<SN>4321</SN>") {
				t.Fatalf("rewritten %s Alarm response leaked downstream identity: %s", version.StandardName(), body)
			}
		})
	}
}

func TestCascadeAlarmQueryFailureReturnsBusinessError(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()
	api.cascadeDeviceQuery = func(context.Context, *DeviceQueryInput) (*DeviceQueryOutput, error) {
		return nil, errors.New("downstream timeout")
	}
	var response *sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		response = request
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}

	api.respondCascadeQuery(worker, cascadeQueryEnvelope{CmdType: "Alarm", SN: 109, DeviceID: testExposedChannelID})
	if response == nil {
		t.Fatal("failed Alarm query did not send an upstream business response")
	}
	body := string(response.Body())
	for _, expected := range []string{
		"<CmdType>Alarm</CmdType>", "<SN>109</SN>",
		"<DeviceID>" + testExposedChannelID + "</DeviceID>", "<Result>ERROR</Result>",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("Alarm query error response missing %q: %s", expected, body)
		}
	}
}

func TestCascadeRecordHookRespectsDownstreamDeviceVersionBeforeCall(t *testing.T) {
	adapter, device, channel := newCascadeMediaCore(t)
	memory := newFlowMemory(device.DeviceID)
	memory.runtime.setGBProfile(GBVersion10, nil)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	t.Cleanup(worker.cancel)

	streamNumber := 0
	query := cascadeQueryEnvelope{
		CmdType: "RecordInfo", SN: 206, DeviceID: testExposedChannelID,
		StartTime: "2026-08-26T08:00:00", EndTime: "2026-08-26T09:00:00", StreamNumber: &streamNumber,
	}
	startAt, endAt, err := cascadeRecordQueryTimes(query)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	api.cascadeRecordResult = func(context.Context, *RecordQueryInput) (recordQueryResult, error) {
		calls++
		return recordQueryResult{Items: []RecordItem{{DeviceID: channel.ChannelID}}}, nil
	}

	items, extra, err := api.queryCascadeFrontendRecordItems(t.Context(), worker, []*ipc.Channel{channel}, query, startAt, endAt)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("2022 RecordInfo filters reached 2011 downstream hook %d times", calls)
	}
	if len(items) != 0 || len(extra) != 0 {
		t.Fatalf("rejected downstream RecordInfo returned items=%v extra=%v", items, extra)
	}
}

func TestCascade2022DeviceStatusCarriesSafeAppendixA4Objects(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		if input.Action != deviceQueryActionDeviceStatus || input.DeviceID != channel.DeviceID || input.TargetID != channel.ChannelID {
			t.Fatalf("downstream DeviceStatus input = %+v", input)
		}
		return &DeviceQueryOutput{
			SN: 700, CmdType: "DeviceStatus", DeviceID: channel.ChannelID, Result: "OK",
			XML: `<Response><CmdType>DeviceStatus</CmdType><SN>700</SN><DeviceID>` + channel.ChannelID + `</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status><Info><doorType><DeviceID>` + channel.ChannelID + `</DeviceID><ParentID>` + gb10PlatformID + `</ParentID><DoorID>` + channel.ChannelID + `</DoorID></doorType><ExtraInfo>{"type":"doorType","DeviceID":"` + channel.ChannelID + `","ParentID":"` + gb10PlatformID + `"}</ExtraInfo></Info></Response>`,
		}, nil
	}
	var response *sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		response = request
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	if err := api.respondCascadeDeviceStatus(t.Context(), worker, cascadeQueryEnvelope{CmdType: "DeviceStatus", SN: 82, DeviceID: testExposedChannelID}); err != nil {
		t.Fatal(err)
	}
	if response == nil {
		t.Fatal("2022 DeviceStatus did not receive a response")
	}
	text := string(response.Body())
	for _, expected := range []string{"<SN>82</SN>", "<DeviceID>" + testExposedChannelID + "</DeviceID>", "<DoorID>" + testExposedChannelID + "</DoorID>", "<ParentID>" + gb10DeviceID + "</ParentID>", "doorType"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("2022 DeviceStatus missing %q: %s", expected, text)
		}
	}
	if strings.Contains(text, channel.ChannelID) || strings.Contains(text, gb10PlatformID) || strings.Contains(text, "<SN>700</SN>") {
		t.Fatalf("2022 DeviceStatus leaked downstream identifiers: %s", text)
	}
}

func TestCascadeExtendedQueryFailureReturnsBusinessError(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()
	api.cascadeDeviceQuery = func(context.Context, *DeviceQueryInput) (*DeviceQueryOutput, error) {
		return nil, errors.New("downstream timeout")
	}
	var request *sip.Request
	worker.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		request = in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}

	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "SDCardStatus", SN: 98, DeviceID: testExposedChannelID,
	})
	if request == nil {
		t.Fatal("failed downstream query did not receive a business response")
	}
	body := string(request.Body())
	for _, expected := range []string{
		"<CmdType>SDCardStatus</CmdType>", "<SN>98</SN>",
		"<DeviceID>" + testExposedChannelID + "</DeviceID>", "<SumNum>0</SumNum>",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("cascade error response missing %q: %s", expected, body)
		}
	}
	if strings.Contains(body, "<Result>") {
		t.Fatalf("SDCardStatus schema does not define Result: %s", body)
	}
}

func TestCascadeExtendedQueryResponsesAreIsolatedPerUpstream(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		return &DeviceQueryOutput{
			SN: 500, CmdType: "PresetQuery", DeviceID: input.TargetID, Result: "OK",
			XML: `<Response><CmdType>PresetQuery</CmdType><SN>500</SN><DeviceID>` + channel.ChannelID + `</DeviceID><Result>OK</Result><PresetList Num="0"></PresetList></Response>`,
		}, nil
	}

	platformA := testSharedCascadePlatform(t)
	platformB := platformA
	platformB.name = "municipal"
	platformB.serverID = "44010000002000000001"
	platformB.channelIDMap = map[string]string{testCascadeChannelID: "44010000001320000911"}
	platformB.exposedChannelMap = map[string]string{"44010000001320000911": testCascadeChannelID}
	workerA := newCascadeWorker(server, platformA)
	workerB := newCascadeWorker(server, platformB)
	workerA.mu.Lock()
	workerA.effective = GBVersion11
	workerA.mu.Unlock()
	workerB.mu.Lock()
	workerB.effective = GBVersion11
	workerB.mu.Unlock()

	var requestA, requestB *sip.Request
	workerA.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		requestA = in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}
	workerB.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		requestB = in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		api.respondCascadeQuery(workerA, cascadeQueryEnvelope{CmdType: "PresetQuery", SN: 101, DeviceID: testExposedChannelID})
	}()
	go func() {
		defer wg.Done()
		api.respondCascadeQuery(workerB, cascadeQueryEnvelope{CmdType: "PresetQuery", SN: 101, DeviceID: "44010000001320000911"})
	}()
	wg.Wait()

	if requestA == nil || requestB == nil {
		t.Fatalf("isolated responses = %v / %v", requestA, requestB)
	}
	bodyA := string(requestA.Body())
	bodyB := string(requestB.Body())
	if !strings.Contains(bodyA, "<DeviceID>"+testExposedChannelID+"</DeviceID>") || strings.Contains(bodyA, "44010000001320000911") {
		t.Fatalf("upstream A received mixed response: %s", bodyA)
	}
	if !strings.Contains(bodyB, "<DeviceID>44010000001320000911</DeviceID>") || strings.Contains(bodyB, testExposedChannelID) {
		t.Fatalf("upstream B received mixed response: %s", bodyB)
	}
}

func TestCascadeRecordInfoQueriesSharedChannelAndMapsResponse(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	if err := adapter.Store().Channel().Update(
		t.Context(),
		channel,
		func(item *ipc.Channel) error {
			item.Name = "   "
			return nil
		},
		orm.Where("id = ?", channel.ID),
	); err != nil {
		t.Fatal(err)
	}
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	platform := testSharedCascadePlatform(t)
	platform.version = GBVersion30
	worker := newCascadeWorker(server, platform)
	requests := make([]*sip.Request, 0, 2)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	var downstream *RecordQueryInput
	api.cascadeQueryRecords = func(_ context.Context, input *RecordQueryInput) ([]RecordItem, error) {
		downstream = input
		items := make([]RecordItem, 21)
		for index := range items {
			streamNumber := 2
			recorderID := channel.DeviceID
			filePath := strconv.Itoa(index + 1)
			address := ""
			if index == 0 {
				recorderID = " recorder<&> "
				filePath = " file<&> "
				address = " address<&> "
			}
			itemName := "Front Gate"
			if index == 0 {
				itemName = " record<&> "
			}
			items[index] = RecordItem{
				DeviceID: channel.ChannelID, Name: itemName, FilePath: filePath, Address: address,
				StartTime: "2024-04-01T00:00:00", EndTime: "2024-04-01T00:10:00",
				Secrecy: 0, Type: "time", RecorderID: recorderID,
				FileSize: "1024", RecordLocation: channel.DeviceID, StreamNumber: &streamNumber,
			}
		}
		return items, nil
	}
	query := cascadeQueryEnvelope{
		CmdType: "RecordInfo", SN: 88, DeviceID: testExposedChannelID,
		StartTime: "2024-04-01T00:00:00", EndTime: "2024-04-01T01:00:00",
		FilePath: " /record/<front>&gate.ps ", Address: " front&gate ", Type: "ALL", RecorderID: " recorder-main ",
		StreamNumber: intPointer(2), AlarmMethod: "5", AlarmType: "13", IndistinctQuery: intPointer(1),
	}
	if err := api.respondCascadeRecordInfo(t.Context(), worker, query); err != nil {
		t.Fatal(err)
	}
	if downstream == nil || downstream.DeviceID != channel.DeviceID || downstream.ChannelID != channel.ChannelID || downstream.End <= downstream.Start ||
		downstream.FilePath != " /record/<front>&gate.ps " || downstream.Address != " front&gate " || downstream.Type != "all" || downstream.RecorderID != " recorder-main " ||
		downstream.IndistinctQuery == nil || *downstream.IndistinctQuery != 1 || downstream.StreamNumber == nil || *downstream.StreamNumber != 2 ||
		downstream.AlarmMethod != "5" || downstream.AlarmType != "13" {
		t.Fatalf("downstream RecordInfo query = %+v", downstream)
	}
	if len(requests) != 2 {
		t.Fatalf("RecordInfo response chunks = %d, want 2", len(requests))
	}
	for index, request := range requests {
		body := string(request.Body())
		for _, expected := range []string{
			"<CmdType>RecordInfo</CmdType>", "<SN>88</SN>", "<DeviceID>" + testExposedChannelID + "</DeviceID>", "<Name>   </Name>", "<SumNum>21</SumNum>",
		} {
			if !strings.Contains(body, expected) {
				t.Fatalf("RecordInfo chunk %d missing %q: %s", index, expected, body)
			}
		}
		wantNum := `Num="20"`
		if index == 1 {
			wantNum = `Num="1"`
		}
		if !strings.Contains(body, wantNum) {
			t.Fatalf("RecordInfo chunk %d item count: %s", index, body)
		}
		if strings.Contains(body, "<DeviceID>"+channel.ChannelID+"</DeviceID>") || strings.Contains(body, "<RecorderID>"+channel.DeviceID+"</RecorderID>") {
			t.Fatalf("RecordInfo chunk %d leaked local IDs: %s", index, body)
		}
		for _, expected := range []string{"<RecorderID>" + testExposedChannelID + "</RecorderID>", "<RecordLocation>" + testExposedChannelID + "</RecordLocation>", "<StreamNumber>2</StreamNumber>"} {
			if !strings.Contains(body, expected) {
				t.Fatalf("RecordInfo chunk %d missing mapped 2022 field %q: %s", index, expected, body)
			}
		}
		if strings.Contains(body, channel.DeviceID) {
			t.Fatalf("RecordInfo chunk %d leaked local storage ID: %s", index, body)
		}
	}
	if body := string(requests[0].Body()); !strings.Contains(body, "<RecorderID> recorder&lt;&amp;&gt; </RecorderID>") ||
		!strings.Contains(body, "<Name> record&lt;&amp;&gt; </Name>") ||
		!strings.Contains(body, "<FilePath> file&lt;&amp;&gt; </FilePath>") ||
		!strings.Contains(body, "<Address> address&lt;&amp;&gt; </Address>") {
		t.Fatalf("RecordInfo response changed opaque strings: %s", body)
	}
}

func TestCascadeRecordRecorderIDMapsOnlyKnownDeviceIdentifiers(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "opaque string", value: "recorder-main", want: "recorder-main"},
		{name: "opaque string whitespace", value: " recorder-main ", want: " recorder-main "},
		{name: "whitespace wrapped digits remain opaque", value: " 44010000002000000001 ", want: " 44010000002000000001 "},
		{name: "known mapped channel", value: testCascadeChannelID, want: testExposedChannelID},
		{name: "local device", value: gb10DeviceID, want: testExposedChannelID},
		{name: "unknown device identifier", value: "44010000002000000001"},
		{name: "empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := cascadeRecordRecorderID(platform, test.value, testCascadeChannelID, gb10DeviceID, testExposedChannelID)
			if got != test.want {
				t.Fatalf("cascadeRecordRecorderID(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}

func TestCascadeCenterRecordFiltersPreserveOpaqueStrings(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	tests := []struct {
		name  string
		query cascadeQueryEnvelope
		want  bool
	}{
		{name: "empty filters", query: cascadeQueryEnvelope{}, want: true},
		{name: "exact local recorder", query: cascadeQueryEnvelope{RecorderID: platform.localID}, want: true},
		{name: "padded local recorder", query: cascadeQueryEnvelope{RecorderID: " " + platform.localID + " "}},
		{name: "whitespace address", query: cascadeQueryEnvelope{Address: " "}},
		{name: "structured blank alarm filters", query: cascadeQueryEnvelope{AlarmMethod: " ", AlarmType: "\t"}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cascadeCenterRecordFiltersMatch(platform, test.query); got != test.want {
				t.Fatalf("cascadeCenterRecordFiltersMatch(%+v) = %t, want %t", test.query, got, test.want)
			}
		})
	}
}

func newCascadeRecordingStore(t *testing.T) recording.RecordingStorer {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s-recording-%s?mode=memory&cache=shared", t.Name(), sip.RandString(12))))
	if err != nil {
		t.Fatal(err)
	}
	return recordingdb.NewDB(db).AutoMigrate(true).Recording()
}

func TestCascadeRecordInfo2022DefaultsToSharedCenterRecordings(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	store := newCascadeRecordingStore(t)
	start := time.Date(2026, 8, 27, 8, 0, 0, 0, sip.GBTimeLocation())
	for _, item := range []*recording.Recording{
		{CID: channel.ID, Path: "shared.mp4", StartedAt: orm.Time{Time: start}, EndedAt: orm.Time{Time: start.Add(10 * time.Minute)}, Size: 2048},
		{CID: channel.ID, Path: " shared.mp4 ", StartedAt: orm.Time{Time: start}, EndedAt: orm.Time{Time: start.Add(10 * time.Minute)}, Size: 3072},
		{CID: channel.ID, Path: "deleted.mp4", StartedAt: orm.Time{Time: start}, EndedAt: orm.Time{Time: start.Add(10 * time.Minute)}, DeleteFlag: true},
		{CID: "private-channel", Path: "private.mp4", StartedAt: orm.Time{Time: start}, EndedAt: orm.Time{Time: start.Add(10 * time.Minute)}},
	} {
		if err := store.Create(t.Context(), item); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server, recordingStore: store}
	server.gb = api
	platform := testSharedCascadePlatform(t)
	platform.version = GBVersion30
	worker := newCascadeWorker(server, platform)
	var body string
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		body = string(request.Body())
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	frontendCalled := false
	api.cascadeQueryRecords = func(context.Context, *RecordQueryInput) ([]RecordItem, error) {
		frontendCalled = true
		return nil, nil
	}
	err := api.respondCascadeRecordInfo(t.Context(), worker, cascadeQueryEnvelope{
		CmdType: "RecordInfo", SN: 191, DeviceID: platform.localID,
		StartTime: "2026-08-27T07:59:00", EndTime: "2026-08-27T08:30:00", FilePath: " shared.mp4 ", Type: "time",
	})
	if err != nil {
		t.Fatal(err)
	}
	if frontendCalled {
		t.Fatal("2022 default center query reached frontend")
	}
	for _, expected := range []string{
		"<SumNum>1</SumNum>", "<DeviceID>" + testExposedChannelID + "</DeviceID>", "<FilePath> shared.mp4 </FilePath>",
		"<RecorderID>" + platform.localID + "</RecorderID>", "<RecordLocation>" + platform.localID + "</RecordLocation>", "<FileSize>3072</FileSize>",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("center RecordInfo missing %q: %s", expected, body)
		}
	}
	for _, forbidden := range []string{"deleted.mp4", "private.mp4", channel.ChannelID} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("center RecordInfo leaked %q: %s", forbidden, body)
		}
	}
}

func TestCascadeRecordInfoIndistinctQueryMergesAndDeduplicates(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	store := newCascadeRecordingStore(t)
	start := time.Date(2026, 8, 27, 8, 0, 0, 0, sip.GBTimeLocation())
	if err := store.Create(t.Context(), &recording.Recording{
		CID: channel.ID, Path: "same.mp4", StartedAt: orm.Time{Time: start}, EndedAt: orm.Time{Time: start.Add(10 * time.Minute)}, Size: 1024,
	}); err != nil {
		t.Fatal(err)
	}
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server, recordingStore: store}
	server.gb = api
	platform := testSharedCascadePlatform(t)
	platform.version = GBVersion30
	worker := newCascadeWorker(server, platform)
	var body string
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		body = string(request.Body())
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api.cascadeQueryRecords = func(_ context.Context, input *RecordQueryInput) ([]RecordItem, error) {
		if input.ChannelID != channel.ChannelID {
			t.Fatalf("frontend channel = %s", input.ChannelID)
		}
		return []RecordItem{
			{DeviceID: channel.ChannelID, Name: "duplicate", FilePath: "same.mp4", StartTime: "2026-08-27T08:00:00", EndTime: "2026-08-27T08:10:00", Secrecy: 0, Type: "time", RecordLocation: channel.DeviceID},
			{DeviceID: channel.ChannelID, Name: "frontend", FilePath: "front.mp4", StartTime: "2026-08-27T08:10:00", EndTime: "2026-08-27T08:20:00", Secrecy: 0, Type: "time", RecordLocation: channel.DeviceID},
		}, nil
	}
	err := api.respondCascadeRecordInfo(t.Context(), worker, cascadeQueryEnvelope{
		CmdType: "RecordInfo", SN: 192, DeviceID: testExposedChannelID,
		StartTime: "2026-08-27T07:59:00", EndTime: "2026-08-27T08:30:00", Type: "all", IndistinctQuery: intPointer(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "<SumNum>2</SumNum>") || strings.Count(body, "<FilePath>same.mp4</FilePath>") != 1 ||
		!strings.Contains(body, "<FilePath>front.mp4</FilePath>") {
		t.Fatalf("merged RecordInfo = %s", body)
	}
}

func TestCascadeRecordInfoRejectsMissingLocationFor2022IndistinctQuery(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	platform.version = GBVersion30
	worker := newCascadeWorker(nil, platform)
	worker.effective = GBVersion30
	var body string
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		body = string(request.Body())
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{core: adapter}
	api.cascadeQueryRecords = func(_ context.Context, input *RecordQueryInput) ([]RecordItem, error) {
		if input.IndistinctQuery == nil || *input.IndistinctQuery != 1 {
			t.Fatalf("downstream IndistinctQuery = %v", input.IndistinctQuery)
		}
		return []RecordItem{{
			DeviceID: channel.ChannelID, Name: "record", FilePath: "missing-location.mp4",
			StartTime: "2026-08-27T08:00:00", EndTime: "2026-08-27T08:10:00", Secrecy: 0, Type: "time",
		}}, nil
	}
	if err := api.respondCascadeRecordInfo(t.Context(), worker, cascadeQueryEnvelope{
		CmdType: "RecordInfo", SN: 194, DeviceID: testExposedChannelID,
		StartTime: "2026-08-27T07:59:00", EndTime: "2026-08-27T08:30:00", Type: "all", IndistinctQuery: intPointer(1),
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "<SumNum>0</SumNum>") || strings.Contains(body, "<RecordList") || strings.Contains(body, "missing-location.mp4") {
		t.Fatalf("invalid fuzzy RecordInfo result was forwarded: %s", body)
	}
}

func TestCascadeRecordInfoForwardsIndistinctQueryForSupportedVersions(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			adapter, _, _ := newCascadeMediaCore(t)
			platform := testSharedCascadePlatform(t)
			platform.version = version
			worker := newCascadeWorker(nil, platform)
			worker.effective = version
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}
			api := &GB28181API{core: adapter}
			var downstream *RecordQueryInput
			api.cascadeQueryRecords = func(_ context.Context, input *RecordQueryInput) ([]RecordItem, error) {
				downstream = input
				return nil, nil
			}
			if err := api.respondCascadeRecordInfo(t.Context(), worker, cascadeQueryEnvelope{
				CmdType: "RecordInfo", SN: 195, DeviceID: testExposedChannelID,
				StartTime: "2026-08-27T07:59:00", EndTime: "2026-08-27T08:30:00", Type: "all", IndistinctQuery: intPointer(1),
			}); err != nil {
				t.Fatal(err)
			}
			if downstream == nil || downstream.IndistinctQuery == nil || *downstream.IndistinctQuery != 1 {
				t.Fatalf("version %s downstream RecordInfo query = %+v", version, downstream)
			}
		})
	}
}

func TestCascadeLegacyRecordInfoOmitsMissingTimesDownstream(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11} {
		t.Run(string(version), func(t *testing.T) {
			adapter, _, _ := newCascadeMediaCore(t)
			server := &Server{}
			api := &GB28181API{core: adapter, svr: server}
			server.gb = api
			platform := testSharedCascadePlatform(t)
			platform.version = version
			worker := newCascadeWorker(server, platform)
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}
			var downstream *RecordQueryInput
			api.cascadeQueryRecords = func(_ context.Context, input *RecordQueryInput) ([]RecordItem, error) {
				downstream = input
				return nil, nil
			}
			query := cascadeQueryEnvelope{CmdType: "RecordInfo", SN: 193, DeviceID: testExposedChannelID, Type: "time"}
			if version.AtLeast(GBVersion11) {
				query.recordQueryLocationID = testExposedChannelID
			}
			if err := api.respondCascadeRecordInfo(t.Context(), worker, query); err != nil {
				t.Fatal(err)
			}
			if downstream == nil || !downstream.OmitStartTime || !downstream.OmitEndTime {
				t.Fatalf("legacy downstream RecordInfo = %+v", downstream)
			}
		})
	}
}

func TestCascadeRecordInfoQuerySourceSelection(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	tests := []struct {
		name     string
		version  GBProtocolVersion
		query    cascadeQueryEnvelope
		center   bool
		frontend bool
	}{
		{name: "2022 center by To", version: GBVersion30, query: cascadeQueryEnvelope{DeviceID: testExposedChannelID, recordQueryLocationID: platform.localID}, center: true},
		{name: "2022 frontend by To", version: GBVersion30, query: cascadeQueryEnvelope{DeviceID: testExposedChannelID, recordQueryLocationID: testExposedChannelID}, frontend: true},
		{name: "2014 center by To", version: GBVersion11, query: cascadeQueryEnvelope{DeviceID: testExposedChannelID, recordQueryLocationID: platform.localID}, center: true},
		{name: "2014 indistinct", version: GBVersion11, query: cascadeQueryEnvelope{DeviceID: testExposedChannelID, IndistinctQuery: intPointer(1)}, center: true, frontend: true},
		{name: "2016 frontend by To", version: GBVersion20, query: cascadeQueryEnvelope{DeviceID: testExposedChannelID, recordQueryLocationID: testExposedChannelID}, frontend: true},
		{name: "2022 indistinct", version: GBVersion30, query: cascadeQueryEnvelope{DeviceID: testExposedChannelID, IndistinctQuery: intPointer(1)}, center: true, frontend: true},
		{name: "2011 system center", version: GBVersion10, query: cascadeQueryEnvelope{DeviceID: platform.localID}, center: true},
		{name: "2011 channel frontend", version: GBVersion10, query: cascadeQueryEnvelope{DeviceID: testExposedChannelID}, frontend: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			center, frontend := cascadeRecordQuerySources(test.version, platform, test.query)
			if center != test.center || frontend != test.frontend {
				t.Fatalf("sources = center:%v frontend:%v", center, frontend)
			}
		})
	}
}

func TestCascadeRecordInfoMapsAppendixA4ExtraInfo(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	platform.version = GBVersion30
	platform.localID = gb10PlatformID
	worker := newCascadeWorker(nil, platform)
	var body string
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		body = string(request.Body())
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	values, err := rewriteCascadeRecordExtraInfo([]string{
		`{"type":"doorType","DeviceID":"` + channel.ChannelID + `","ParentID":"` + channel.DeviceID + `"}`,
	}, GBVersion30, platform, channel.ChannelID, channel.DeviceID, testExposedChannelID)
	if err != nil {
		t.Fatal(err)
	}
	api := &GB28181API{core: adapter}
	if err := api.sendCascadeRecordItems(t.Context(), worker, cascadeQueryEnvelope{
		CmdType: "RecordInfo", SN: 189, DeviceID: testExposedChannelID,
	}, nil, "camera", values); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "<ExtraInfo>") || !strings.Contains(body, testExposedChannelID) || !strings.Contains(body, platform.localID) ||
		strings.Contains(body, channel.ChannelID) || strings.Contains(body, channel.DeviceID) {
		t.Fatalf("cascade RecordInfo ExtraInfo = %s", body)
	}
	worker.effective = GBVersion20
	if err := api.sendCascadeRecordItems(t.Context(), worker, cascadeQueryEnvelope{
		CmdType: "RecordInfo", SN: 190, DeviceID: testExposedChannelID,
	}, nil, "camera", values); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "<ExtraInfo>") {
		t.Fatalf("downgraded cascade RecordInfo exposed 3.0 ExtraInfo: %s", body)
	}

	if values, err := rewriteCascadeRecordExtraInfo([]string{`{"type":"doorType"}`}, GBVersion20, platform, channel.ChannelID, channel.DeviceID, testExposedChannelID); err != nil || len(values) != 0 {
		t.Fatalf("legacy RecordInfo ExtraInfo = %+v, %v", values, err)
	}
	if _, err := rewriteCascadeRecordExtraInfo([]string{
		`{"type":"doorType","DeviceID":"34020000001320000099"}`,
	}, GBVersion30, platform, channel.ChannelID, channel.DeviceID, testExposedChannelID); err == nil {
		t.Fatal("unknown RecordInfo ExtraInfo identifier was accepted")
	}
}

func TestRewriteCascadeRecordExtraInfoPreserves2022StringWhitespace(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	platform.version = GBVersion30

	values, err := rewriteCascadeRecordExtraInfo([]string{
		"  keep  ",
		"   ",
		"x",
		" x ",
		strings.Repeat("界", 1024),
	}, GBVersion30, platform, testCascadeChannelID, gb10DeviceID, testExposedChannelID)
	if err != nil {
		t.Fatalf("2022 RecordInfo ExtraInfo with valid whitespace rejected: %v", err)
	}
	want := []string{
		"  keep  ",
		"   ",
		"x",
		" x ",
		strings.Repeat("界", 1024),
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("2022 RecordInfo ExtraInfo whitespace changed: got %#v, want %#v", values, want)
	}

	if _, err := rewriteCascadeRecordExtraInfo([]string{strings.Repeat("界", 1025)}, GBVersion30, platform, testCascadeChannelID, gb10DeviceID, testExposedChannelID); err == nil {
		t.Fatal("oversized 2022 RecordInfo ExtraInfo accepted")
	}
}

func TestCascadeRecordInfoRewriteFailureReturnsBusinessError(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	platform := testSharedCascadePlatform(t)
	platform.version = GBVersion30
	worker := newCascadeWorker(server, platform)
	requests := make([]*sip.Request, 0, 1)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api.cascadeRecordResult = func(_ context.Context, _ *RecordQueryInput) (recordQueryResult, error) {
		return recordQueryResult{ExtraInfo: []string{
			`{"type":"doorType","DeviceID":"34020000001320000099"}`,
		}}, nil
	}

	err := api.respondCascadeRecordInfo(t.Context(), worker, cascadeQueryEnvelope{
		CmdType: "RecordInfo", SN: 190, DeviceID: testExposedChannelID,
		StartTime: "2024-04-01T00:00:00", EndTime: "2024-04-01T01:00:00", Type: "ALL", IndistinctQuery: intPointer(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("RecordInfo rewrite failure responses = %d, want 1", len(requests))
	}
	body := string(requests[0].Body())
	if !strings.Contains(body, "<Name>"+testExposedChannelID+"</Name>") || !strings.Contains(body, "<SumNum>0</SumNum>") ||
		strings.Contains(body, "<RecordList") || !strings.Contains(body, "<DeviceID>"+testExposedChannelID+"</DeviceID>") ||
		strings.Contains(body, "<Result>") {
		t.Fatalf("RecordInfo rewrite failure response = %s", body)
	}
}

func TestCascadeRecordInfoPreservesTypeForAllVersions(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			adapter, _, _ := newCascadeMediaCore(t)
			server := &Server{}
			api := &GB28181API{core: adapter, svr: server}
			server.gb = api
			platform := testSharedCascadePlatform(t)
			platform.version = version
			worker := newCascadeWorker(server, platform)
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}
			var downstream *RecordQueryInput
			api.cascadeQueryRecords = func(_ context.Context, input *RecordQueryInput) ([]RecordItem, error) {
				downstream = input
				return nil, nil
			}
			query := cascadeQueryEnvelope{
				CmdType: "RecordInfo", SN: 188, DeviceID: testExposedChannelID, Type: "ALL",
				StartTime: "2024-04-01T00:00:00", EndTime: "2024-04-01T01:00:00",
			}
			if version.AtLeast(GBVersion11) {
				query.recordQueryLocationID = testExposedChannelID
			}
			if version == GBVersion30 {
				query.IndistinctQuery = intPointer(1)
			}
			err := api.respondCascadeRecordInfo(t.Context(), worker, query)
			if err != nil {
				t.Fatal(err)
			}
			if downstream == nil || downstream.Type != "all" {
				t.Fatalf("version %s downstream RecordInfo query = %+v", version, downstream)
			}
		})
	}
}

func TestCascadeRecordInfoRejects2022FiltersFromLegacyUpstream(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	platform := testSharedCascadePlatform(t)
	platform.version = GBVersion20
	worker := newCascadeWorker(server, platform)
	var response *sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		response = request
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	called := false
	api.cascadeQueryRecords = func(_ context.Context, _ *RecordQueryInput) ([]RecordItem, error) {
		called = true
		return nil, nil
	}
	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "RecordInfo", SN: 89, DeviceID: testExposedChannelID,
		StartTime: "2024-04-01T00:00:00", EndTime: "2024-04-01T01:00:00", StreamNumber: intPointer(0),
	})
	if called {
		t.Fatal("legacy upstream RecordInfo filter reached downstream query")
	}
	if response != nil {
		t.Fatalf("legacy upstream RecordInfo filter emitted a non-standard business response: %s", response.Body())
	}
}

func TestCascadeRecordInfoFiltersFieldsByUpstreamVersion(t *testing.T) {
	streamNumber := 2
	items := []RecordItem{{
		DeviceID: testExposedChannelID, Name: "record", Secrecy: 0, Type: "all",
		FileSize: "1024", RecordLocation: testExposedChannelID, StreamNumber: &streamNumber,
	}}
	tests := []struct {
		version      GBProtocolVersion
		wantFileSize bool
		want2022     bool
		wantAll      bool
	}{
		{version: GBVersion10, wantAll: true},
		{version: GBVersion11},
		{version: GBVersion20, wantFileSize: true},
		{version: GBVersion30, wantFileSize: true, want2022: true},
	}
	for _, test := range tests {
		t.Run(string(test.version), func(t *testing.T) {
			filtered := recordItemsForVersion(items, test.version)
			body, err := xml.Marshal(filtered[0])
			if err != nil {
				t.Fatal(err)
			}
			text := string(body)
			if strings.Contains(text, "<FileSize>") != test.wantFileSize ||
				strings.Contains(text, "<RecordLocation>") != test.want2022 ||
				strings.Contains(text, "<StreamNumber>") != test.want2022 ||
				strings.Contains(text, "<Type>all</Type>") != test.wantAll {
				t.Fatalf("version %s RecordInfo item = %s", test.version, text)
			}
			if items[0].FileSize != "1024" || items[0].RecordLocation == "" || items[0].StreamNumber == nil || items[0].Type != "all" {
				t.Fatalf("source RecordInfo item was mutated: %+v", items[0])
			}
		})
	}
}

func TestCascadeEmptyCatalogAndRecordInfoUseVersionedLists(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	var bodies []string
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		bodies = append(bodies, string(request.Body()))
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{core: adapter}
	worker.effective = GBVersion10
	if err := api.respondCascadeCatalog(t.Context(), worker, cascadeQueryEnvelope{
		CmdType: "Catalog", SN: 89, DeviceID: "34020000002110000009",
	}); err != nil {
		t.Fatal(err)
	}
	worker.effective = GBVersion11
	if err := api.respondCascadeCatalog(t.Context(), worker, cascadeQueryEnvelope{
		CmdType: "Catalog", SN: 90, DeviceID: "34020000002110000009",
	}); err != nil {
		t.Fatal(err)
	}
	if err := api.sendCascadeRecordItems(t.Context(), worker, cascadeQueryEnvelope{
		CmdType: "RecordInfo", SN: 91, DeviceID: testExposedChannelID,
	}, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 {
		t.Fatalf("empty cascade responses = %d, want 3", len(bodies))
	}
	if !strings.Contains(bodies[0], "<SumNum>0</SumNum>") || !strings.Contains(bodies[0], `<DeviceList Num="0"></DeviceList>`) {
		t.Fatalf("empty 1.0 Catalog response is not standard-compliant: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], "<SumNum>0</SumNum>") || strings.Contains(bodies[1], "<DeviceList") {
		t.Fatalf("empty 1.1 Catalog response is not standard-compliant: %s", bodies[1])
	}
	if !strings.Contains(bodies[2], "<SumNum>0</SumNum>") || strings.Contains(bodies[2], "<RecordList") ||
		!strings.Contains(bodies[2], "<Name>"+testExposedChannelID+"</Name>") {
		t.Fatalf("empty RecordInfo response is not standard-compliant: %s", bodies[2])
	}
}

func TestCascadeEmptyCatalogUsesVersionedListRules(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
			worker.effective = version
			var body string
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				body = string(request.Body())
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}
			api := &GB28181API{core: adapter}
			if err := api.respondCascadeCatalog(t.Context(), worker, cascadeQueryEnvelope{
				CmdType: "Catalog", SN: 93, DeviceID: "34020000002110000009",
			}); err != nil {
				t.Fatal(err)
			}
			wantList := version == GBVersion10
			if !strings.Contains(body, "<SumNum>0</SumNum>") || strings.Contains(body, "<DeviceList") != wantList {
				t.Fatalf("version %s empty Catalog response = %s", version, body)
			}
		})
	}
}

func TestCascadeEmptyRecordInfoUsesVersionedListRules(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
			worker.effective = version
			var body string
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				body = string(request.Body())
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}
			api := &GB28181API{}
			if err := api.sendCascadeRecordItems(t.Context(), worker, cascadeQueryEnvelope{
				CmdType: "RecordInfo", SN: 92, DeviceID: testExposedChannelID,
			}, nil, "camera", nil); err != nil {
				t.Fatal(err)
			}
			wantList := version == GBVersion10
			if !strings.Contains(body, "<SumNum>0</SumNum>") || strings.Contains(body, "<RecordList") != wantList {
				t.Fatalf("version %s empty RecordInfo response = %s", version, body)
			}
		})
	}
}

func TestCascadeRecordInfoChunksWaitForPreviousSIPFinalResponse(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.effective = GBVersion11
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseFirst)
		}
	}()

	var requestMu sync.Mutex
	requests := make([]*sip.Request, 0, 2)
	worker.exchange = func(ctx context.Context, request *sip.Request) (*sip.Response, error) {
		requestMu.Lock()
		requests = append(requests, request)
		current := len(requests)
		requestMu.Unlock()
		switch current {
		case 1:
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case 2:
			close(secondStarted)
		}
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}

	items := make([]RecordItem, cascadeCatalogChunkSize+1)
	for index := range items {
		items[index] = RecordItem{
			DeviceID:  testExposedChannelID,
			Name:      fmt.Sprintf("record-%d", index),
			FilePath:  fmt.Sprintf("/record/%d.ps", index),
			StartTime: "2026-08-30T10:00:00",
			EndTime:   "2026-08-30T10:01:00",
			Secrecy:   0,
			Type:      "time",
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- (&GB28181API{}).sendCascadeRecordItems(t.Context(), worker, cascadeQueryEnvelope{
			CmdType: "RecordInfo", SN: 314, DeviceID: testExposedChannelID,
		}, items, "camera", nil)
	}()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first RecordInfo chunk was not sent")
	}
	select {
	case <-secondStarted:
		close(releaseFirst)
		released = true
		<-done
		t.Fatal("second RecordInfo chunk was sent before the first SIP final response")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	released = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("RecordInfo chunk sending did not resume after the first SIP final response")
	}

	requestMu.Lock()
	defer requestMu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("RecordInfo chunks = %d, want 2", len(requests))
	}
	for _, request := range requests {
		body := string(request.Body())
		if !strings.Contains(body, "<SN>314</SN>") || !strings.Contains(body, "<SumNum>21</SumNum>") {
			t.Fatalf("RecordInfo chunk lost request correlation: %s", body)
		}
	}
}

func TestCascadeQueryTargetAllowsSupportedSharedQueries(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	for _, cmdType := range []string{
		"Catalog", "DeviceInfo", "DeviceStatus", "RecordInfo", "PresetQuery", "HomePositionQuery",
		"CruiseTrackListQuery", "CruiseTrackQuery", "MobilePosition", "PTZPosition", "SDCardStatus", "ConfigDownload",
	} {
		if !cascadeQueryTargetAllowed(platform, cmdType, testExposedChannelID) {
			t.Errorf("shared %s query was rejected", cmdType)
		}
	}
	if cascadeQueryTargetAllowed(platform, "RecordInfo", "34020000001320000099") {
		t.Fatal("unshared RecordInfo query was accepted")
	}
	for _, cmdType := range []string{"Catalog", "DeviceInfo", "DeviceStatus", "RecordInfo"} {
		if !cascadeQueryTargetAllowed(platform, cmdType, platform.localID) {
			t.Errorf("local %s query was rejected", cmdType)
		}
	}
	for _, cmdType := range []string{
		"PresetQuery", "HomePositionQuery", "CruiseTrackListQuery", "CruiseTrackQuery",
		"MobilePosition", "PTZPosition", "SDCardStatus", "ConfigDownload",
	} {
		if cascadeQueryTargetAllowed(platform, cmdType, platform.localID) {
			t.Errorf("local %s query was accepted without a local implementation", cmdType)
		}
	}
	if !cascadeQueryTargetAllowed(platform, "MobilePosition", platform.localID, GBVersion30) {
		t.Fatal("2022 local system MobilePosition query was rejected")
	}
	if cascadeQueryTargetAllowed(platform, "MobilePosition", platform.localID, GBVersion20) {
		t.Fatal("2016 local system MobilePosition query was accepted")
	}
}

func newCascadeSystemMobilePositionTest(t *testing.T) (*GB28181API, *cascadeWorker, *ipc.Channel, *ipc.Channel, string, string) {
	t.Helper()
	adapter, device, _ := newCascadeMediaCore(t)
	mobileByInfo := &ipc.Channel{
		ID: "GBC_mobile_info", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000021", Name: "Mobile Info", Type: ipc.TypeGB28181, IsOnline: true,
		Ext: ipc.DeviceExt{GBCatalog: &ipc.GBCatalogExt{MobileDeviceType: 1}},
	}
	mobileByCode := &ipc.Channel{
		ID: "GBC_mobile_code", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001380000022", Name: "Mobile Code", Type: ipc.TypeGB28181, IsOnline: true,
	}
	stationary := &ipc.Channel{
		ID: "GBC_stationary", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000023", Name: "Stationary", Type: ipc.TypeGB28181, IsOnline: true,
	}
	offlineMobile := &ipc.Channel{
		ID: "GBC_mobile_offline", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001380000024", Name: "Offline Mobile", Type: ipc.TypeGB28181, IsOnline: false,
	}
	for _, channel := range []*ipc.Channel{mobileByInfo, mobileByCode, stationary, offlineMobile} {
		if err := adapter.Store().Channel().Create(t.Context(), channel); err != nil {
			t.Fatal(err)
		}
	}
	exposedInfo := "34020000001320000921"
	exposedCode := "34020000001380000922"
	platform := testSharedCascadePlatform(t)
	platform.version = GBVersion30
	platform.sharedChannels = []string{mobileByInfo.ChannelID, mobileByCode.ChannelID, stationary.ChannelID, offlineMobile.ChannelID}
	platform.channelIDMap = map[string]string{
		mobileByInfo.ChannelID: exposedInfo, mobileByCode.ChannelID: exposedCode,
		stationary.ChannelID: "34020000001320000923", offlineMobile.ChannelID: "34020000001380000924",
	}
	platform.exposedChannelMap = make(map[string]string, len(platform.channelIDMap))
	for localID, exposedID := range platform.channelIDMap {
		platform.exposedChannelMap[exposedID] = localID
	}
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, platform)
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()
	t.Cleanup(worker.cancel)
	return api, worker, mobileByInfo, mobileByCode, exposedInfo, exposedCode
}

func TestCascadeSystemMobilePositionQueryFansOutOnlyToSharedOnlineMobileChannels(t *testing.T) {
	api, worker, mobileByInfo, mobileByCode, _, _ := newCascadeSystemMobilePositionTest(t)
	var mu sync.Mutex
	var targets []string
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		mu.Lock()
		targets = append(targets, input.TargetID)
		mu.Unlock()
		return &DeviceQueryOutput{CmdType: "MobilePosition", DeviceID: input.TargetID}, nil
	}
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		t.Fatalf("system MobilePosition query emitted unexpected business response: %s", request.Body())
		return nil, nil
	}

	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "MobilePosition", SN: 41, DeviceID: worker.platform.localID, Interval: 5,
	})
	mu.Lock()
	sort.Strings(targets)
	got := append([]string(nil), targets...)
	mu.Unlock()
	want := []string{mobileByInfo.ChannelID, mobileByCode.ChannelID}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("system MobilePosition fan-out targets = %v, want %v", got, want)
	}
	if count := syncMapLen(&api.cascadeMobilePositionQueries); count != 1 {
		t.Fatalf("system MobilePosition routes = %d, want 1", count)
	}
}

func TestCascadeSystemMobilePositionQueryRespectsDisabledDeviceCapabilityBeforeRouteRegistration(t *testing.T) {
	api, worker, _, _, _, _ := newCascadeSystemMobilePositionTest(t)
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBProfile(GBVersion30, []string{"mobile_position"})
	api.svr.memoryStorer = memory

	var calls atomic.Int32
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		calls.Add(1)
		return &DeviceQueryOutput{CmdType: "MobilePosition", DeviceID: input.TargetID}, nil
	}
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}

	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "MobilePosition", SN: 205, DeviceID: worker.platform.localID, Interval: 5,
	})

	if got := calls.Load(); got != 0 {
		t.Fatalf("disabled mobile_position reached %d downstream system query calls", got)
	}
	if got := syncMapLen(&api.cascadeMobilePositionQueries); got != 0 {
		t.Fatalf("disabled system MobilePosition registered %d routes", got)
	}
}

func TestCascadeSystemMobilePositionDoesNotGuessAmbiguous2016Target(t *testing.T) {
	api, worker, mobileByInfo, _, _, _ := newCascadeSystemMobilePositionTest(t)
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		return &DeviceQueryOutput{CmdType: "MobilePosition", DeviceID: input.TargetID}, nil
	}
	var requests []*sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "MobilePosition", SN: 40, DeviceID: worker.platform.localID, Interval: 5,
	})
	body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>80</SN><Time>2026-08-28T10:00:00</Time><Longitude>120.5</Longitude><Latitude>30.2</Latitude></Notify>`)
	api.forwardCascadeMobilePositionQueryNotify(mobileByInfo.DeviceID, body)
	if len(requests) != 0 {
		t.Fatalf("ambiguous 2016 MobilePosition was attributed to a channel: %s", requests[0].Body())
	}
}

func TestCascadeSystemMobilePositionInfersUnique2016Target(t *testing.T) {
	api, worker, mobileByInfo, _, exposedInfo, _ := newCascadeSystemMobilePositionTest(t)
	worker.platform.sharedChannels = []string{mobileByInfo.ChannelID}
	worker.platform.channelIDMap = map[string]string{mobileByInfo.ChannelID: exposedInfo}
	worker.platform.exposedChannelMap = map[string]string{exposedInfo: mobileByInfo.ChannelID}
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		return &DeviceQueryOutput{CmdType: "MobilePosition", DeviceID: input.TargetID}, nil
	}
	var requests []*sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "MobilePosition", SN: 40, DeviceID: worker.platform.localID, Interval: 5,
	})
	body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>80</SN><Time>2026-08-28T10:00:00</Time><Longitude>120.5</Longitude><Latitude>30.2</Latitude><Direction>360</Direction></Notify>`)
	api.forwardCascadeMobilePositionQueryNotify(mobileByInfo.DeviceID, body)
	if len(requests) != 1 || !strings.Contains(string(requests[0].Body()), "<DeviceID>"+exposedInfo+"</DeviceID>") ||
		!strings.Contains(string(requests[0].Body()), "<Direction>0</Direction>") {
		t.Fatalf("unique 2016 MobilePosition target was not aggregated: %v", requests)
	}
}

func TestCascadeSystemMobilePositionQueryAggregatesLatestSnapshot(t *testing.T) {
	api, worker, mobileByInfo, mobileByCode, exposedInfo, exposedCode := newCascadeSystemMobilePositionTest(t)
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		return &DeviceQueryOutput{CmdType: "MobilePosition", DeviceID: input.TargetID}, nil
	}
	var requests []*sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "MobilePosition", SN: 42, DeviceID: worker.platform.localID, Interval: 5,
	})

	batch := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>81</SN><DeviceID>` + mobileByCode.DeviceID + `</DeviceID><Time>2026-08-28T10:00:01</Time><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + mobileByCode.ChannelID + `</DeviceID><CaptureTime>2026-08-28T10:00:00</CaptureTime><Longitude>121.2</Longitude><Latitude>31.2</Latitude></Item></DeviceList></Notify>`)
	api.forwardCascadeMobilePositionQueryNotify(mobileByCode.DeviceID, batch)
	secondBatch := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>82</SN><DeviceID>` + mobileByInfo.DeviceID + `</DeviceID><Time>2026-08-28T10:00:02</Time><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + mobileByInfo.ChannelID + `</DeviceID><CaptureTime>2026-08-28T10:00:02</CaptureTime><Longitude>120.5</Longitude><Latitude>30.2</Latitude></Item></DeviceList></Notify>`)
	api.forwardCascadeMobilePositionQueryNotify(mobileByInfo.DeviceID, secondBatch)
	if len(requests) != 2 {
		t.Fatalf("system MobilePosition notify requests = %d, want 2", len(requests))
	}
	body := string(requests[len(requests)-1].Body())
	for _, expected := range []string{
		"<DeviceID>" + worker.platform.localID + "</DeviceID>", "<SumNum>2</SumNum>", `<DeviceList Num="2">`,
		"<DeviceID>" + exposedInfo + "</DeviceID>", "<DeviceID>" + exposedCode + "</DeviceID>", exposedCode,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("system MobilePosition snapshot missing %q: %s", expected, body)
		}
	}
	if strings.Index(body, exposedInfo) > strings.Index(body, exposedCode) {
		t.Fatalf("system MobilePosition snapshot is not stably sorted: %s", body)
	}
	for _, hidden := range []string{mobileByInfo.ChannelID, mobileByCode.ChannelID} {
		if strings.Contains(body, hidden) {
			t.Fatalf("system MobilePosition snapshot leaked local id %q: %s", hidden, body)
		}
	}

	updated := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>83</SN><DeviceID>` + mobileByInfo.ChannelID + `</DeviceID><Time>2026-08-28T10:00:03</Time><Longitude>120.6</Longitude><Latitude>30.3</Latitude></Notify>`)
	api.forwardCascadeMobilePositionQueryNotify(mobileByInfo.DeviceID, updated)
	body = string(requests[len(requests)-1].Body())
	if !strings.Contains(body, "<SumNum>2</SumNum>") || strings.Count(body, "<Item>") != 2 || !strings.Contains(body, "<Longitude>120.6</Longitude>") {
		t.Fatalf("system MobilePosition snapshot was not deduplicated to latest positions: %s", body)
	}
}

func TestCascadeSystemMobilePositionQueryForwardsZeroSnapshot(t *testing.T) {
	api, worker, mobileByInfo, _, _, _ := newCascadeSystemMobilePositionTest(t)
	worker.platform.sharedChannels = []string{mobileByInfo.ChannelID}
	worker.platform.channelIDMap = map[string]string{mobileByInfo.ChannelID: "34020000001320000921"}
	worker.platform.exposedChannelMap = map[string]string{"34020000001320000921": mobileByInfo.ChannelID}
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		return &DeviceQueryOutput{CmdType: "MobilePosition", DeviceID: input.TargetID}, nil
	}
	var requests []*sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "MobilePosition", SN: 48, DeviceID: worker.platform.localID, Interval: 5,
	})
	zero := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>89</SN><DeviceID>` + mobileByInfo.ChannelID +
		`</DeviceID><Time>2026-08-28T10:00:09</Time><SumNum>0</SumNum></Notify>`)
	api.forwardCascadeMobilePositionQueryNotify(mobileByInfo.DeviceID, zero)
	if len(requests) != 1 {
		t.Fatalf("zero-result system MobilePosition requests = %d, want 1", len(requests))
	}
	body := string(requests[0].Body())
	for _, expected := range []string{
		"<DeviceID>" + worker.platform.localID + "</DeviceID>",
		"<SumNum>0</SumNum>",
		`<DeviceList Num="0"></DeviceList>`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("zero-result system MobilePosition missing %q: %s", expected, body)
		}
	}
}

func TestCascadeSystemMobilePositionQueryIgnoresOlderCaptureTime(t *testing.T) {
	api, worker, mobileByInfo, _, exposedInfo, _ := newCascadeSystemMobilePositionTest(t)
	worker.platform.sharedChannels = []string{mobileByInfo.ChannelID}
	worker.platform.channelIDMap = map[string]string{mobileByInfo.ChannelID: exposedInfo}
	worker.platform.exposedChannelMap = map[string]string{exposedInfo: mobileByInfo.ChannelID}
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		return &DeviceQueryOutput{CmdType: "MobilePosition", DeviceID: input.TargetID}, nil
	}
	var requests []*sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "MobilePosition", SN: 49, DeviceID: worker.platform.localID, Interval: 5,
	})
	newer := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>90</SN><DeviceID>` + mobileByInfo.ChannelID +
		`</DeviceID><Time>2026-08-28T10:00:11</Time><Longitude>120.6</Longitude><Latitude>30.3</Latitude></Notify>`)
	older := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>91</SN><DeviceID>` + mobileByInfo.ChannelID +
		`</DeviceID><Time>2026-08-28T10:00:10</Time><Longitude>120.1</Longitude><Latitude>30.1</Latitude></Notify>`)
	api.forwardCascadeMobilePositionQueryNotify(mobileByInfo.DeviceID, newer)
	api.forwardCascadeMobilePositionQueryNotify(mobileByInfo.DeviceID, older)
	if len(requests) != 1 {
		t.Fatalf("out-of-order system MobilePosition requests = %d, want 1", len(requests))
	}
	body := string(requests[0].Body())
	if !strings.Contains(body, "<Longitude>120.6</Longitude>") || strings.Contains(body, "<Longitude>120.1</Longitude>") {
		t.Fatalf("out-of-order system MobilePosition regressed snapshot: %s", body)
	}
}

func TestCascadeSystemMobilePositionQueryDropsFailedSources(t *testing.T) {
	api, worker, mobileByInfo, mobileByCode, exposedInfo, exposedCode := newCascadeSystemMobilePositionTest(t)
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		if input.TargetID == mobileByCode.ChannelID {
			return nil, errors.New("downstream rejected query")
		}
		return &DeviceQueryOutput{CmdType: "MobilePosition", DeviceID: input.TargetID}, nil
	}
	var requests []*sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "MobilePosition", SN: 43, DeviceID: worker.platform.localID, Interval: 5,
	})
	failed := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>84</SN><DeviceID>` + mobileByCode.ChannelID + `</DeviceID><Time>2026-08-28T10:00:04</Time><Longitude>121.2</Longitude><Latitude>31.2</Latitude></Notify>`)
	api.forwardCascadeMobilePositionQueryNotify(mobileByCode.DeviceID, failed)
	if len(requests) != 0 {
		t.Fatalf("failed MobilePosition source was forwarded: %s", requests[0].Body())
	}
	success := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>85</SN><DeviceID>` + mobileByInfo.ChannelID + `</DeviceID><Time>2026-08-28T10:00:05</Time><Longitude>120.5</Longitude><Latitude>30.2</Latitude></Notify>`)
	api.forwardCascadeMobilePositionQueryNotify(mobileByInfo.DeviceID, success)
	if len(requests) != 1 {
		t.Fatalf("successful MobilePosition source requests = %d, want 1", len(requests))
	}
	body := string(requests[0].Body())
	if !strings.Contains(body, exposedInfo) || strings.Contains(body, exposedCode) || !strings.Contains(body, "<SumNum>1</SumNum>") {
		t.Fatalf("partial MobilePosition snapshot = %s", body)
	}
}

func TestCascadeSystemMobilePositionQueryResetsSnapshot(t *testing.T) {
	api, worker, mobileByInfo, mobileByCode, exposedInfo, exposedCode := newCascadeSystemMobilePositionTest(t)
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		return &DeviceQueryOutput{CmdType: "MobilePosition", DeviceID: input.TargetID}, nil
	}
	var requests []*sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	query := cascadeQueryEnvelope{CmdType: "MobilePosition", SN: 44, DeviceID: worker.platform.localID, Interval: 5}
	api.respondCascadeQuery(worker, query)
	first := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>86</SN><DeviceID>` + mobileByInfo.ChannelID + `</DeviceID><Time>2026-08-28T10:00:06</Time><Longitude>120.5</Longitude><Latitude>30.2</Latitude></Notify>`)
	api.forwardCascadeMobilePositionQueryNotify(mobileByInfo.DeviceID, first)
	if len(requests) != 1 || !strings.Contains(string(requests[0].Body()), exposedInfo) {
		t.Fatalf("first system MobilePosition snapshot = %v", requests)
	}

	query.SN++
	api.respondCascadeQuery(worker, query)
	second := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>87</SN><DeviceID>` + mobileByCode.ChannelID + `</DeviceID><Time>2026-08-28T10:00:07</Time><Longitude>121.2</Longitude><Latitude>31.2</Latitude></Notify>`)
	api.forwardCascadeMobilePositionQueryNotify(mobileByCode.DeviceID, second)
	if len(requests) != 2 {
		t.Fatalf("reset system MobilePosition requests = %d, want 2", len(requests))
	}
	body := string(requests[1].Body())
	if !strings.Contains(body, exposedCode) || strings.Contains(body, exposedInfo) || !strings.Contains(body, "<SumNum>1</SumNum>") {
		t.Fatalf("reset system MobilePosition snapshot retained old state: %s", body)
	}
}

func TestCascadeSystemMobilePositionQueryIsolatesUpstreams(t *testing.T) {
	api, workerA, mobileByInfo, _, exposedInfo, _ := newCascadeSystemMobilePositionTest(t)
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		return &DeviceQueryOutput{CmdType: "MobilePosition", DeviceID: input.TargetID}, nil
	}
	platformB := workerA.platform
	platformB.name = "municipal"
	platformB.serverID = "44010000002000000001"
	exposedB := "44010000001320000921"
	platformB.channelIDMap = make(map[string]string, len(workerA.platform.channelIDMap))
	platformB.exposedChannelMap = make(map[string]string, len(workerA.platform.exposedChannelMap))
	for localID, exposedID := range workerA.platform.channelIDMap {
		if localID == mobileByInfo.ChannelID {
			exposedID = exposedB
		}
		platformB.channelIDMap[localID] = exposedID
		platformB.exposedChannelMap[exposedID] = localID
	}
	workerB := newCascadeWorker(workerA.server, platformB)
	workerB.mu.Lock()
	workerB.effective = GBVersion30
	workerB.mu.Unlock()
	t.Cleanup(workerB.cancel)
	var requestsA, requestsB []*sip.Request
	workerA.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requestsA = append(requestsA, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	workerB.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requestsB = append(requestsB, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	query := cascadeQueryEnvelope{CmdType: "MobilePosition", SN: 46, DeviceID: workerA.platform.localID, Interval: 5}
	api.respondCascadeQuery(workerA, query)
	api.respondCascadeQuery(workerB, query)
	body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>88</SN><DeviceID>` + mobileByInfo.ChannelID + `</DeviceID><Time>2026-08-28T10:00:08</Time><Longitude>120.5</Longitude><Latitude>30.2</Latitude></Notify>`)
	api.forwardCascadeMobilePositionQueryNotify(mobileByInfo.DeviceID, body)
	if len(requestsA) != 1 || len(requestsB) != 1 {
		t.Fatalf("isolated upstream MobilePosition requests = %d/%d, want 1/1", len(requestsA), len(requestsB))
	}
	bodyA := string(requestsA[0].Body())
	bodyB := string(requestsB[0].Body())
	if !strings.Contains(bodyA, exposedInfo) || strings.Contains(bodyA, exposedB) || !strings.Contains(bodyB, exposedB) || strings.Contains(bodyB, exposedInfo) {
		t.Fatalf("upstream MobilePosition mappings leaked: A=%s B=%s", bodyA, bodyB)
	}
}

func TestCascadeSystemMobilePositionQueryAllFailuresRemoveRoute(t *testing.T) {
	api, worker, _, _, _, _ := newCascadeSystemMobilePositionTest(t)
	api.cascadeDeviceQuery = func(context.Context, *DeviceQueryInput) (*DeviceQueryOutput, error) {
		return nil, errors.New("downstream rejected query")
	}
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		t.Fatalf("failed system MobilePosition query emitted a response: %s", request.Body())
		return nil, nil
	}
	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "MobilePosition", SN: 47, DeviceID: worker.platform.localID, Interval: 5,
	})
	if count := syncMapLen(&api.cascadeMobilePositionQueries); count != 0 {
		t.Fatalf("failed system MobilePosition query retained %d routes", count)
	}
}

func TestRemoveCascadeMobilePositionQueriesOnlyRemovesStoppedWorker(t *testing.T) {
	api, workerA, mobileByInfo, _, _, _ := newCascadeSystemMobilePositionTest(t)
	platformB := workerA.platform
	platformB.name = "municipal-cleanup"
	platformB.serverID = "44010000002000000002"
	workerB := newCascadeWorker(workerA.server, platformB)
	t.Cleanup(workerB.cancel)
	routeA := api.storeCascadeSystemMobilePositionQuery(workerA, workerA.platform.localID, []*ipc.Channel{mobileByInfo})
	routeB := api.storeCascadeSystemMobilePositionQuery(workerB, workerB.platform.localID, []*ipc.Channel{mobileByInfo})
	if routeA == nil || routeB == nil || syncMapLen(&api.cascadeMobilePositionQueries) != 2 {
		t.Fatal("failed to create isolated MobilePosition routes")
	}
	api.removeCascadeMobilePositionQueries(workerA)
	if _, ok := api.cascadeMobilePositionQueries.Load(routeA.key); ok {
		t.Fatal("stopped worker MobilePosition route was retained")
	}
	if current, ok := api.cascadeMobilePositionQueries.Load(routeB.key); !ok || current != routeB {
		t.Fatal("active worker MobilePosition route was removed")
	}
}

func TestCascadeMobilePositionQueryForwardsSubsequentNotify(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion20
	worker.mu.Unlock()
	t.Cleanup(worker.cancel)

	var downstream *DeviceQueryInput
	api.cascadeDeviceQuery = func(_ context.Context, input *DeviceQueryInput) (*DeviceQueryOutput, error) {
		copyInput := *input
		downstream = &copyInput
		return &DeviceQueryOutput{
			SN: 77, CmdType: "MobilePosition", DeviceID: channel.ChannelID, Result: "OK",
		}, nil
	}
	var requests []*sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}

	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "MobilePosition", SN: 31, DeviceID: testExposedChannelID, Interval: 5,
	})
	if downstream == nil || downstream.Action != deviceQueryActionMobilePosition || downstream.Interval != 5 || downstream.TargetID != channel.ChannelID {
		t.Fatalf("downstream MobilePosition query = %+v", downstream)
	}
	if len(requests) != 0 {
		t.Fatalf("MobilePosition query emitted a business response: %s", requests[0].Body())
	}

	body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>78</SN><Time>2026-08-28T10:00:00</Time><Longitude>120.5</Longitude><Latitude>30.2</Latitude></Notify>`)
	api.forwardCascadeMobilePositionQueryNotify(channel.DeviceID, body)
	if len(requests) != 1 {
		t.Fatalf("MobilePosition notify requests = %d, want 1", len(requests))
	}
	text := string(requests[0].Body())
	if !strings.Contains(text, "<CmdType>MobilePosition</CmdType>") || strings.Contains(text, "<DeviceID>") || strings.Contains(text, channel.ChannelID) {
		t.Fatalf("rewritten MobilePosition notify = %s", text)
	}
}

func TestCascadeMobilePositionQueryConverts2016NotifyFor2022Upstream(t *testing.T) {
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()
	t.Cleanup(worker.cancel)
	api.storeCascadeMobilePositionQuery(worker, testExposedChannelID, gb10DeviceID, testCascadeChannelID)
	var requests []*sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>79</SN><Time>2026-08-28T10:00:00</Time><Longitude>120.5</Longitude><Latitude>30.2</Latitude><Direction>360</Direction></Notify>`)
	api.forwardCascadeMobilePositionQueryNotify(gb10DeviceID, body)
	if len(requests) != 1 {
		t.Fatalf("2022 MobilePosition query notify requests = %d, want 1", len(requests))
	}
	text := string(requests[0].Body())
	if !strings.Contains(text, "<SumNum>1</SumNum>") || !strings.Contains(text, "<DeviceID>"+testExposedChannelID+"</DeviceID>") ||
		!strings.Contains(text, "<CaptureTime>2026-08-28T10:00:00</CaptureTime>") || !strings.Contains(text, "<Direction>0</Direction>") {
		t.Fatalf("2016 to 2022 MobilePosition query conversion = %s", text)
	}
}

func TestCascadeMobilePositionQueryRejectsNotifyFromDifferentDevice(t *testing.T) {
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()
	t.Cleanup(worker.cancel)
	api.storeCascadeMobilePositionQuery(worker, testExposedChannelID, gb10DeviceID, testCascadeChannelID)
	var requests []*sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>79</SN><Time>2026-08-28T10:00:00</Time><Longitude>120.5</Longitude><Latitude>30.2</Latitude></Notify>`)

	api.forwardCascadeMobilePositionQueryNotify("34020000001320000009", body)
	if len(requests) != 0 {
		t.Fatalf("different device MobilePosition notify requests = %d, want 0", len(requests))
	}

	api.forwardCascadeMobilePositionQueryNotify(gb10DeviceID, body)
	if len(requests) != 1 {
		t.Fatalf("matching device MobilePosition notify requests = %d, want 1", len(requests))
	}
}

func TestCascadeMobilePositionQueryFailureRemovesNotifyRoute(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.effective = GBVersion20
	t.Cleanup(worker.cancel)
	api.cascadeDeviceQuery = func(context.Context, *DeviceQueryInput) (*DeviceQueryOutput, error) {
		return nil, errors.New("downstream rejected query")
	}
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}

	api.respondCascadeQuery(worker, cascadeQueryEnvelope{
		CmdType: "MobilePosition", SN: 32, DeviceID: testExposedChannelID, Interval: 5,
	})
	if count := syncMapLen(&api.cascadeMobilePositionQueries); count != 0 {
		t.Fatalf("failed MobilePosition query retained %d notify routes", count)
	}
}

func TestCascadeQueryRequestRejectsInvalidRequiredPayload(t *testing.T) {
	validRecord := cascadeQueryEnvelope{
		XMLName: xml.Name{Local: "Query"}, CmdType: "RecordInfo", SN: 1, DeviceID: testExposedChannelID,
		StartTime: "2026-08-26T08:00:00", EndTime: "2026-08-26T09:00:00",
	}
	tests := []struct {
		name  string
		query cascadeQueryEnvelope
	}{
		{name: "invalid device id", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "DeviceStatus", SN: 1, DeviceID: "device"}},
		{name: "invalid catalog directory id", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "Catalog", SN: 1, DeviceID: "11010"}},
		{name: "missing cruise number", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "CruiseTrackQuery", SN: 1, DeviceID: testExposedChannelID}},
		{name: "invalid cruise number", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "CruiseTrackQuery", SN: 1, DeviceID: testExposedChannelID, Number: intPointer(2)}},
		{name: "negative mobile interval", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "MobilePosition", SN: 1, DeviceID: testExposedChannelID, Interval: -1}},
		{name: "missing config type", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "ConfigDownload", SN: 1, DeviceID: testExposedChannelID}},
		{name: "unknown config type", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "ConfigDownload", SN: 1, DeviceID: testExposedChannelID, ConfigType: "VendorConfig"}},
		{name: "reversed record time", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "RecordInfo", SN: 1, DeviceID: testExposedChannelID, StartTime: validRecord.EndTime, EndTime: validRecord.StartTime}},
		{name: "invalid record secrecy", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "RecordInfo", SN: 1, DeviceID: testExposedChannelID, Secrecy: intPointer(2)}},
		{name: "invalid indistinct query", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "RecordInfo", SN: 1, DeviceID: testExposedChannelID, IndistinctQuery: intPointer(2)}},
		{name: "unsupported command", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "Unknown", SN: 1, DeviceID: testExposedChannelID}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateCascadeQueryRequest(test.query); err == nil {
				t.Fatal("invalid cascade query was accepted")
			}
		})
	}
	if err := validateCascadeQueryRequest(validRecord); err != nil {
		t.Fatalf("valid RecordInfo rejected: %v", err)
	}
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		recorderString := validRecord
		recorderString.RecorderID = "recorder-main"
		if err := validateCascadeQueryRequest(recorderString); err != nil {
			t.Fatalf("%s RecordInfo recorder string rejected: %v", version.StandardName(), err)
		}
		if err := validateCascadeQueryVersion(recorderString, version); err != nil {
			t.Fatalf("%s RecordInfo recorder string rejected by version gate: %v", version.StandardName(), err)
		}
	}
	missingRecordTime := cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "RecordInfo", SN: 1, DeviceID: testExposedChannelID}
	if err := validateCascadeQueryRequest(missingRecordTime); err != nil {
		t.Fatalf("legacy RecordInfo without time rejected: %v", err)
	}
	if err := validateCascadeQueryVersion(missingRecordTime, GBVersion30); err == nil {
		t.Fatal("2022 RecordInfo without time was accepted")
	}
	if err := validateCascadeQueryVersion(missingRecordTime, GBVersion20); err == nil {
		t.Fatal("2016 RecordInfo without time was accepted")
	}
	if err := validateCascadeQueryVersion(missingRecordTime, GBVersion11); err != nil {
		t.Fatalf("2014 RecordInfo without time rejected: %v", err)
	}
	legacyIndistinct := validRecord
	legacyIndistinct.IndistinctQuery = intPointer(1)
	if err := validateCascadeQueryVersion(legacyIndistinct, GBVersion10); err == nil {
		t.Fatal("2011 RecordInfo IndistinctQuery was accepted")
	}
	compatibilityAlias := validRecord
	compatibilityAlias.DistinctQuery = intPointer(1)
	if err := validateCascadeQueryVersion(compatibilityAlias, GBVersion30); err != nil {
		t.Fatalf("2022 compatibility DistinctQuery rejected: %v", err)
	}
	bothNames := compatibilityAlias
	bothNames.IndistinctQuery = intPointer(1)
	if err := validateCascadeQueryRequest(bothNames); err == nil {
		t.Fatal("RecordInfo with both IndistinctQuery and DistinctQuery was accepted")
	}
	validCruise := cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "CruiseTrackQuery", SN: 1, DeviceID: testExposedChannelID, Number: intPointer(0)}
	if err := validateCascadeQueryRequest(validCruise); err != nil {
		t.Fatalf("valid CruiseTrackQuery number 0 rejected: %v", err)
	}
	validConfig := cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "ConfigDownload", SN: 1, DeviceID: testExposedChannelID, ConfigType: "BasicParam/VideoParamOpt"}
	if err := validateCascadeQueryRequest(validConfig); err != nil {
		t.Fatalf("valid ConfigDownload rejected: %v", err)
	}
	validDirectoryCatalog := cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "Catalog", SN: 1, DeviceID: "110101"}
	if err := validateCascadeQueryRequest(validDirectoryCatalog); err != nil {
		t.Fatalf("valid administrative Catalog rejected: %v", err)
	}
	validAlarm := cascadeQueryEnvelope{
		XMLName: xml.Name{Local: "Query"}, CmdType: "Alarm", SN: 1, DeviceID: testExposedChannelID,
		StartAlarmPriority: "1", EndAlarmPriority: "4", AlarmMethod: "25",
		StartAlarmTime: "2026-08-26T08:00:00", EndAlarmTime: "2026-08-26T09:00:00",
	}
	if err := validateCascadeQueryRequest(validAlarm); err != nil {
		t.Fatalf("valid Alarm query rejected: %v", err)
	}
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11} {
		if err := validateCascadeQueryVersion(validAlarm, version); err != nil {
			t.Fatalf("%s Alarm query rejected: %v", version.StandardName(), err)
		}
	}
	for _, version := range []GBProtocolVersion{GBVersion20, GBVersion30} {
		withType := validAlarm
		withType.AlarmType = "2"
		if err := validateCascadeQueryVersion(withType, version); err != nil {
			t.Fatalf("%s Alarm query with AlarmType rejected: %v", version.StandardName(), err)
		}
	}
	legacyType := validAlarm
	legacyType.AlarmType = "2"
	if err := validateCascadeQueryVersion(legacyType, GBVersion11); err == nil {
		t.Fatal("2014 Alarm query with AlarmType was accepted")
	}
	reversedAlarm := validAlarm
	reversedAlarm.StartAlarmTime, reversedAlarm.EndAlarmTime = reversedAlarm.EndAlarmTime, reversedAlarm.StartAlarmTime
	if err := validateCascadeQueryVersion(reversedAlarm, GBVersion30); err == nil {
		t.Fatal("Alarm query with reversed time range was accepted")
	}
	for _, test := range []struct {
		version GBProtocolVersion
		wantOK  bool
	}{
		{version: GBVersion10},
		{version: GBVersion11, wantOK: true},
		{version: GBVersion20, wantOK: true},
		{version: GBVersion30, wantOK: true},
	} {
		err := validateCascadeQueryVersion(validDirectoryCatalog, test.version)
		if (err == nil) != test.wantOK {
			t.Fatalf("protocol %s administrative Catalog query error = %v, want success = %v", test.version, err, test.wantOK)
		}
	}
}

func TestCascadeMiddlewareRejectsMissingCruiseNumberBeforeForwarding(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()
	api := &GB28181API{}
	forwarded := false
	api.cascadeDeviceQuery = func(context.Context, *DeviceQueryInput) (*DeviceQueryOutput, error) {
		forwarded = true
		return nil, nil
	}
	connection := newFlowConnection()
	body := []byte(`<Query><CmdType>CruiseTrackQuery</CmdType><SN>8</SN><DeviceID>` + testExposedChannelID + `</DeviceID></Query>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "cascade-missing-cruise-number", body)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-missing-cruise-number", connection),
		DeviceID: platform.serverID, Source: connection.remote,
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 400") {
			t.Fatalf("missing CruiseTrackQuery Number response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("missing CruiseTrackQuery Number response timeout")
	}
	if forwarded {
		t.Fatal("invalid CruiseTrackQuery was forwarded downstream")
	}
}

func TestCascadeMiddlewareRejectsMalformedQueryBeforeSIPOK(t *testing.T) {
	base := `<Query><CmdType>DeviceStatus</CmdType><SN>8</SN><DeviceID>` + testExposedChannelID + `</DeviceID></Query>`
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown field", body: strings.Replace(base, "</Query>", "<Vendor>1</Vendor></Query>", 1)},
		{name: "duplicate sequence", body: strings.Replace(base, "</Query>", "<SN>8</SN></Query>", 1)},
		{name: "root attribute", body: strings.Replace(base, "<Query>", `<Query vendor="1">`, 1)},
		{name: "root namespace", body: strings.Replace(strings.Replace(base, "<Query>", `<gb:Query xmlns:gb="urn:vendor">`, 1), "</Query>", "</gb:Query>", 1)},
		{name: "simple field attribute", body: strings.Replace(base, "<DeviceID>", `<DeviceID vendor="1">`, 1)},
		{name: "simple field namespace", body: strings.Replace(base, "<DeviceID>", `<gb:DeviceID xmlns:gb="urn:vendor">`, 1)},
		{name: "simple field nesting", body: strings.Replace(base, testExposedChannelID, "<Value>"+testExposedChannelID+"</Value>", 1)},
		{name: "out of order", body: `<Query><CmdType>DeviceStatus</CmdType><DeviceID>` + testExposedChannelID + `</DeviceID><SN>8</SN></Query>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			platform := testSharedCascadePlatform(t)
			worker := newCascadeWorker(nil, platform)
			worker.mu.Lock()
			worker.effective = GBVersion30
			worker.mu.Unlock()
			api := &GB28181API{lifecycleClosed: true}
			forwarded := false
			api.cascadeDeviceQuery = func(context.Context, *DeviceQueryInput) (*DeviceQueryOutput, error) {
				forwarded = true
				return nil, nil
			}
			connection := newFlowConnection()
			request := newFlowRequest(t, connection, sip.MethodMessage, "cascade-malformed-query-"+test.name, []byte(test.body))
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction("cascade-malformed-query-"+test.name, connection),
				DeviceID: platform.serverID, Source: connection.remote,
			}
			ctx.Set(cascadeWorkerContextKey, worker)
			api.sipCascadeMessageMiddleware(ctx)
			select {
			case response := <-connection.writes:
				if !strings.Contains(string(response), "SIP/2.0 400") || strings.Contains(string(response), "SIP/2.0 200") {
					t.Fatalf("malformed query response = %s", response)
				}
			case <-time.After(time.Second):
				t.Fatal("malformed query response timeout")
			}
			if forwarded {
				t.Fatal("malformed query was forwarded downstream")
			}
		})
	}
}

func TestCascadeMiddlewareRejectsVersionMismatchBeforeSIP200(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		body    string
	}{
		{
			name: "2011 SD card query", version: GBVersion10,
			body: `<Query><CmdType>SDCardStatus</CmdType><SN>8</SN><DeviceID>` + testExposedChannelID + `</DeviceID></Query>`,
		},
		{
			name: "2014 mobile position query", version: GBVersion11,
			body: `<Query><CmdType>MobilePosition</CmdType><SN>8</SN><DeviceID>` + testExposedChannelID + `</DeviceID></Query>`,
		},
		{
			name: "2016 RecordInfo 2022 filter", version: GBVersion20,
			body: `<Query><CmdType>RecordInfo</CmdType><SN>8</SN><DeviceID>` + testExposedChannelID + `</DeviceID><StartTime>2026-08-26T08:00:00</StartTime><EndTime>2026-08-26T09:00:00</EndTime><StreamNumber>0</StreamNumber></Query>`,
		},
		{
			name: "2011 RecordInfo IndistinctQuery", version: GBVersion10,
			body: `<Query><CmdType>RecordInfo</CmdType><SN>8</SN><DeviceID>` + testExposedChannelID + `</DeviceID><IndistinctQuery>1</IndistinctQuery></Query>`,
		},
		{
			name: "2016 RecordInfo missing time", version: GBVersion20,
			body: `<Query><CmdType>RecordInfo</CmdType><SN>8</SN><DeviceID>` + testExposedChannelID + `</DeviceID></Query>`,
		},
		{
			name: "2022 RecordInfo missing time", version: GBVersion30,
			body: `<Query><CmdType>RecordInfo</CmdType><SN>8</SN><DeviceID>` + testExposedChannelID + `</DeviceID></Query>`,
		},
		{
			name: "2016 RecordInfo DistinctQuery alias", version: GBVersion20,
			body: `<Query><CmdType>RecordInfo</CmdType><SN>8</SN><DeviceID>` + testExposedChannelID + `</DeviceID><StartTime>2026-08-26T08:00:00</StartTime><EndTime>2026-08-26T09:00:00</EndTime><DistinctQuery>1</DistinctQuery></Query>`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			platform := testSharedCascadePlatform(t)
			worker := newCascadeWorker(nil, platform)
			worker.mu.Lock()
			worker.effective = test.version
			worker.mu.Unlock()
			api := &GB28181API{lifecycleClosed: true}
			forwarded := false
			api.cascadeDeviceQuery = func(context.Context, *DeviceQueryInput) (*DeviceQueryOutput, error) {
				forwarded = true
				return nil, nil
			}
			connection := newFlowConnection()
			request := newFlowRequest(t, connection, sip.MethodMessage, "cascade-version-mismatch-"+test.name, []byte(test.body))
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction("cascade-version-mismatch-"+test.name, connection),
				DeviceID: platform.serverID, Source: connection.remote,
			}
			ctx.Set(cascadeWorkerContextKey, worker)
			api.sipCascadeMessageMiddleware(ctx)
			select {
			case response := <-connection.writes:
				if !strings.Contains(string(response), "SIP/2.0 400") || strings.Contains(string(response), "SIP/2.0 200") {
					t.Fatalf("version-mismatched query response = %s", response)
				}
			case <-time.After(time.Second):
				t.Fatal("version-mismatched query response timeout")
			}
			if forwarded {
				t.Fatal("version-mismatched query was forwarded downstream")
			}
		})
	}
}

func TestCascadeMiddlewareAcceptsStandardRecordIndistinctQuery(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	api := &GB28181API{lifecycleClosed: true}
	connection := newFlowConnection()
	body := []byte(`<Query><CmdType>RecordInfo</CmdType><SN>8</SN><DeviceID>` + testExposedChannelID + `</DeviceID><IndistinctQuery>1</IndistinctQuery></Query>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "cascade-2014-indistinct-query", body)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-2014-indistinct-query", connection),
		DeviceID: platform.serverID, Source: connection.remote,
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200") {
			t.Fatalf("2014 IndistinctQuery response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("2014 IndistinctQuery response timeout")
	}
}

func TestCascadeMiddlewareRejectsBothRecordQueryFieldNamesBeforeSIP200(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()
	api := &GB28181API{lifecycleClosed: true}
	connection := newFlowConnection()
	body := []byte(`<Query><CmdType>RecordInfo</CmdType><SN>8</SN><DeviceID>` + testExposedChannelID + `</DeviceID><StartTime>2026-08-26T08:00:00</StartTime><EndTime>2026-08-26T09:00:00</EndTime><IndistinctQuery>1</IndistinctQuery><DistinctQuery>1</DistinctQuery></Query>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "cascade-record-query-both-names", body)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-record-query-both-names", connection),
		DeviceID: platform.serverID, Source: connection.remote,
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 400") || strings.Contains(string(response), "SIP/2.0 200") {
			t.Fatalf("RecordInfo with both field names response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("RecordInfo with both field names response timeout")
	}
}

func TestCascadeCatalogQuerySupportsSharedChannelTarget(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	var body string
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		body = string(request.Body())
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{core: adapter}
	if err := api.respondCascadeCatalog(t.Context(), worker, cascadeQueryEnvelope{
		CmdType: "Catalog", SN: 44, DeviceID: testExposedChannelID,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "<DeviceID>"+testExposedChannelID+"</DeviceID>") || !strings.Contains(body, "<SumNum>1</SumNum>") {
		t.Fatalf("shared channel Catalog response:\n%s", body)
	}
}

func TestCascadeCatalogQuerySupportsVisibleDirectoryTargets11And20(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	const groupID = "34020000002150000001"
	channel.Ext.GBCatalog = &ipc.GBCatalogExt{
		ParentID: groupID, BusinessGroupID: groupID, CivilCode: "340200",
	}
	if err := adapter.Store().Channel().EditGB28181Config(t.Context(), channel); err != nil {
		t.Fatal(err)
	}

	platform := testSharedCascadePlatform(t)
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20} {
		for _, test := range []struct {
			name     string
			targetID string
			status   string
		}{
			{name: "administrative", targetID: "340200", status: "SIP/2.0 200"},
			{name: "business-group", targetID: groupID, status: "SIP/2.0 200"},
			{name: "unshared", targetID: "34020000002160000099", status: "SIP/2.0 404"},
		} {
			t.Run(version.StandardYear()+"/"+test.name, func(t *testing.T) {
				worker := newCascadeWorker(nil, platform)
				worker.mu.Lock()
				worker.effective = version
				worker.mu.Unlock()
				api := &GB28181API{core: adapter, lifecycleClosed: true}
				connection := newFlowConnection()
				body := []byte(`<Query><CmdType>Catalog</CmdType><SN>45</SN><DeviceID>` + test.targetID + `</DeviceID></Query>`)
				request := newFlowRequest(t, connection, sip.MethodMessage, "cascade-directory-query-"+version.StandardYear()+"-"+test.name, body)
				ctx := &sip.Context{
					Request: request, Tx: sip.NewTransaction("cascade-directory-query-"+version.StandardYear()+"-"+test.name, connection),
					DeviceID: platform.serverID, Source: connection.remote,
				}
				ctx.Set(cascadeWorkerContextKey, worker)
				api.sipCascadeMessageMiddleware(ctx)
				response := <-flowResponse(t, connection)
				if !strings.Contains(response, test.status) {
					t.Fatalf("%s directory target %s response:\n%s", version.StandardName(), test.targetID, response)
				}
			})
		}
	}
}

func TestCascadeCatalogQueryFiltersSharedChannelsByCreatedAtAndKeepsAncestors(t *testing.T) {
	adapter, device, _ := newCascadeMediaCore(t)
	createdAt := time.Date(2026, 8, 20, 12, 0, 0, 0, sip.GBTimeLocation())
	matchingID := "34020000001320000081"
	oldID := "34020000001320000082"
	matchingExposedID := "34020000001320000981"
	oldExposedID := "34020000001320000982"
	for _, channel := range []*ipc.Channel{
		{
			ID: "GBC_catalog_time_match", DID: device.ID, DeviceID: device.DeviceID,
			ChannelID: matchingID, Name: "matching", Type: ipc.TypeGB28181, IsOnline: true,
			CreatedAt: orm.Time{Time: createdAt},
			Ext:       ipc.DeviceExt{GBCatalog: &ipc.GBCatalogExt{ParentID: "340200", CivilCode: "340200"}},
		},
		{
			ID: "GBC_catalog_time_old", DID: device.ID, DeviceID: device.DeviceID,
			ChannelID: oldID, Name: "old", Type: ipc.TypeGB28181, IsOnline: true,
			CreatedAt: orm.Time{Time: createdAt.Add(-24 * time.Hour)},
			Ext:       ipc.DeviceExt{GBCatalog: &ipc.GBCatalogExt{ParentID: "340200", CivilCode: "340200"}},
		},
	} {
		if err := adapter.Store().Channel().Create(t.Context(), channel); err != nil {
			t.Fatal(err)
		}
	}

	platform := testSharedCascadePlatform(t)
	platform.sharedChannels = []string{matchingID, oldID}
	platform.channelIDMap = map[string]string{matchingID: matchingExposedID, oldID: oldExposedID}
	platform.exposedChannelMap = map[string]string{matchingExposedID: matchingID, oldExposedID: oldID}
	api := &GB28181API{core: adapter}
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardYear(), func(t *testing.T) {
			channels, err := api.loadCascadeCatalogChannelsInRange(
				t.Context(), platform, version, createdAt, createdAt.Add(time.Hour),
			)
			if err != nil {
				t.Fatal(err)
			}
			items := buildCascadeCatalogItems(channels, platform, version)
			byID := make(map[string]cascadeCatalogItem, len(items))
			for _, item := range items {
				byID[item.DeviceID] = item
			}
			if _, ok := byID[matchingExposedID]; !ok {
				t.Fatalf("%s matching Catalog channel was filtered out: %+v", version.StandardName(), items)
			}
			if _, ok := byID[oldExposedID]; ok {
				t.Fatalf("%s old Catalog channel was retained: %+v", version.StandardName(), items)
			}
			if _, ok := byID["340200"]; !ok {
				t.Fatalf("%s matching channel ancestor was not retained: %+v", version.StandardName(), items)
			}
		})
	}
}

func TestFilterCascadeChannelsByCreatedAtUsesInclusiveBounds(t *testing.T) {
	start := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	channels := []*ipc.Channel{
		{ChannelID: "start", CreatedAt: orm.Time{Time: start}},
		{ChannelID: "end", CreatedAt: orm.Time{Time: start.Add(time.Hour)}},
		{ChannelID: "before", CreatedAt: orm.Time{Time: start.Add(-time.Nanosecond)}},
		{ChannelID: "after", CreatedAt: orm.Time{Time: start.Add(time.Hour + time.Nanosecond)}},
		{ChannelID: "zero"},
	}
	filtered := filterCascadeChannelsByCreatedAt(channels, start, start.Add(time.Hour))
	if len(filtered) != 2 || filtered[0].ChannelID != "start" || filtered[1].ChannelID != "end" {
		t.Fatalf("inclusive Catalog time filter = %+v", filtered)
	}
}

func TestSeedCascadeCatalogSnapshotAppliesSubscriptionTimeRange(t *testing.T) {
	adapter, device, _ := newCascadeMediaCore(t)
	createdAt := time.Date(2026, 8, 20, 12, 0, 0, 0, sip.GBTimeLocation())
	matchingID := "34020000001320000091"
	oldID := "34020000001320000092"
	matchingExposedID := "34020000001320000991"
	oldExposedID := "34020000001320000992"
	for _, channel := range []*ipc.Channel{
		{
			ID: "GBC_catalog_subscription_time_match", DID: device.ID, DeviceID: device.DeviceID,
			ChannelID: matchingID, Name: "matching", Type: ipc.TypeGB28181, IsOnline: true,
			CreatedAt: orm.Time{Time: createdAt},
		},
		{
			ID: "GBC_catalog_subscription_time_old", DID: device.ID, DeviceID: device.DeviceID,
			ChannelID: oldID, Name: "old", Type: ipc.TypeGB28181, IsOnline: true,
			CreatedAt: orm.Time{Time: createdAt.Add(-24 * time.Hour)},
		},
	} {
		if err := adapter.Store().Channel().Create(t.Context(), channel); err != nil {
			t.Fatal(err)
		}
	}

	platform := testSharedCascadePlatform(t)
	platform.version = GBVersion30
	platform.sharedChannels = []string{matchingID, oldID}
	platform.channelIDMap = map[string]string{matchingID: matchingExposedID, oldID: oldExposedID}
	platform.exposedChannelMap = map[string]string{matchingExposedID: matchingID, oldExposedID: oldID}
	worker := newCascadeWorker(nil, platform)
	t.Cleanup(worker.cancel)
	sub := &eventSubscription{
		Cascade:  worker,
		DeviceID: platform.localID,
		Filter: eventSubscriptionFilter{
			CatalogStartTime: sip.FormatGBTime(createdAt, "2006-01-02T15:04:05"),
			CatalogEndTime:   sip.FormatGBTime(createdAt.Add(time.Hour), "2006-01-02T15:04:05"),
		},
	}
	api := &GB28181API{core: adapter}
	if err := api.seedCascadeCatalogSnapshot(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	if _, ok := sub.CatalogSnapshot[matchingExposedID]; !ok {
		t.Fatalf("matching Catalog item missing from subscription snapshot: %+v", sub.CatalogSnapshot)
	}
	if _, ok := sub.CatalogSnapshot[oldExposedID]; ok {
		t.Fatalf("out-of-range Catalog item retained in subscription snapshot: %+v", sub.CatalogSnapshot)
	}
}

func TestCascadeMiddlewareRejectsInvalidCatalogTimeBeforeSIP200(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	api := &GB28181API{lifecycleClosed: true}
	connection := newFlowConnection()
	body := []byte(`<Query><CmdType>Catalog</CmdType><SN>8</SN><DeviceID>` + platform.localID + `</DeviceID><StartTime>invalid</StartTime></Query>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "cascade-catalog-invalid-time", body)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-catalog-invalid-time", connection),
		DeviceID: platform.serverID, Source: connection.remote,
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 400") || strings.Contains(string(response), "SIP/2.0 200") {
			t.Fatalf("invalid Catalog time response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("invalid Catalog time response timeout")
	}
}

func TestCascadeSharedChannelInfoAndStatusUseExposedID(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()
	requests := make([]*sip.Request, 0, 2)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	query := cascadeQueryEnvelope{SN: 90, DeviceID: testExposedChannelID}
	query.CmdType = "DeviceInfo"
	if err := api.respondCascadeDeviceInfo(t.Context(), worker, query); err != nil {
		t.Fatal(err)
	}
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	query.CmdType = "DeviceStatus"
	if err := api.respondCascadeDeviceStatus(t.Context(), worker, query); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("shared channel query responses = %d", len(requests))
	}
	info := string(requests[0].Body())
	for _, expected := range []string{
		"<CmdType>DeviceInfo</CmdType>", "<DeviceID>" + testExposedChannelID + "</DeviceID>",
		"<DeviceName>" + channel.Name + "</DeviceName>", "<Channel>1</Channel>",
	} {
		if !strings.Contains(info, expected) {
			t.Fatalf("shared DeviceInfo missing %q: %s", expected, info)
		}
	}
	for _, removed := range []string{"<DeviceType>", "<MaxCamera>", "<MaxAlarm>"} {
		if strings.Contains(info, removed) {
			t.Fatalf("2022 shared DeviceInfo contains removed field %q: %s", removed, info)
		}
	}
	status := string(requests[1].Body())
	for _, expected := range []string{
		"<CmdType>DeviceStatus</CmdType>", "<DeviceID>" + testExposedChannelID + "</DeviceID>",
		"<Online>ONLINE</Online>", "<Status>OK</Status>",
	} {
		if !strings.Contains(status, expected) {
			t.Fatalf("shared DeviceStatus missing %q: %s", expected, status)
		}
	}
	if strings.Contains(info, "<DeviceID>"+channel.ChannelID+"</DeviceID>") || strings.Contains(status, "<DeviceID>"+channel.ChannelID+"</DeviceID>") {
		t.Fatal("shared channel query leaked local channel ID")
	}
}

func TestCascadePlatformDeviceInfoStoreUnavailableReturnsBusinessError(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
			worker.mu.Lock()
			worker.effective = version
			worker.mu.Unlock()

			var sent *sip.Request
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				sent = request
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}
			api := &GB28181API{}
			query := cascadeQueryEnvelope{CmdType: "DeviceInfo", SN: 91, DeviceID: worker.platform.localID}
			if err := api.respondCascadeDeviceInfo(t.Context(), worker, query); err != nil {
				t.Fatal(err)
			}
			if sent == nil {
				t.Fatal("platform DeviceInfo storage failure did not send a business response")
			}
			body := string(sent.Body())
			for _, expected := range []string{
				"<Response>", "<CmdType>DeviceInfo</CmdType>", "<SN>91</SN>",
				"<DeviceID>" + worker.platform.localID + "</DeviceID>", "<Result>ERROR</Result>",
			} {
				if !strings.Contains(body, expected) {
					t.Fatalf("platform DeviceInfo error missing %q: %s", expected, body)
				}
			}
		})
	}
}

func TestCascadeManagerMatchesOnlyRegisteredSource(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	manager := NewCascadeManager(nil)
	manager.items[worker.platform.name] = worker
	source := &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 16060}
	if _, ok := manager.matchRegistered(gb10PlatformID, source); ok {
		t.Fatal("unregistered upstream unexpectedly matched")
	}
	worker.updateStatus(func(status *CascadePlatformStatus) { status.Registered = true })
	if got, ok := manager.matchRegistered(gb10PlatformID, source); !ok || got != worker {
		t.Fatalf("registered upstream match = %v, %v", got, ok)
	}
	wrongSource := &net.UDPAddr{IP: net.ParseIP("192.0.2.31"), Port: 5060}
	if _, ok := manager.matchRegistered(gb10PlatformID, wrongSource); ok {
		t.Fatal("wrong upstream source unexpectedly matched")
	}
}

func TestCascadeCatalogNotifyUsesSubscribeDialog(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	remoteURI, _ := sip.ParseSipURI("sip:" + gb10PlatformID + "@remote.example")
	localURI, _ := sip.ParseSipURI("sip:" + gb10DeviceID + "@local.example")
	remoteContactURI, _ := sip.ParseSipURI("sip:" + gb10PlatformID + "@192.0.2.30:5060")
	callID := sip.CallID("cascade-subscribe-dialog")
	subscribe := sip.NewRequest("", sip.MethodSubscribe, &localURI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().
			SetFrom(&sip.Address{URI: &remoteURI, Params: sip.NewParams().Add("tag", sip.String{Str: "remote-tag"})}).
			SetTo(&sip.Address{URI: &localURI, Params: sip.NewParams()}).
			SetContact(&sip.Address{URI: &remoteContactURI, Params: sip.NewParams()}).
			SetMethod(sip.MethodSubscribe).
			SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).
			Build(), nil)
	dialogResponse := sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil)
	sub := &eventSubscription{
		CmdType: "Catalog", DeviceID: gb10DeviceID, Event: "Catalog;id=" + gb10DeviceID,
		ExpiresAt: time.Now().Add(time.Hour), To: &sip.Address{URI: &remoteContactURI, Params: sip.NewParams()},
		GBVersion: "1.1", DialogRequest: subscribe, Response: dialogResponse,
		Contact: worker.contactAddress(), Cascade: worker,
	}
	requests := make([]*sip.Request, 0, 2)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	body, err := encodeCascadeCatalogNotify(GBVersion11, cascadeCatalogNotify{CmdType: "Catalog", SN: 9, DeviceID: gb10DeviceID})
	if err != nil {
		t.Fatal(err)
	}
	api := &GB28181API{}
	if err := api.sendEventNotify(sub, "Catalog", body); err != nil {
		t.Fatal(err)
	}
	if err := api.sendEventNotify(sub, "Catalog", body); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("NOTIFY request count = %d", len(requests))
	}
	for index, request := range requests {
		if request.Method() != sip.MethodNotify || request.Recipient().String() != remoteContactURI.String() {
			t.Fatalf("NOTIFY request = %s", request.StartLine())
		}
		from, _ := request.From()
		to, _ := request.To()
		if from == nil || from.Address.String() != localURI.String() || to == nil || to.Address.String() != remoteURI.String() {
			t.Fatalf("NOTIFY From/To = %v / %v", from, to)
		}
		if tag, ok := to.Params.Get("tag"); !ok || tag.String() != "remote-tag" {
			t.Fatalf("NOTIFY remote tag = %v", tag)
		}
		requestCallID, _ := request.CallID()
		cseq, _ := request.CSeq()
		if requestCallID == nil || *requestCallID != callID || cseq == nil || cseq.SeqNo != uint32(index+1) {
			t.Fatalf("NOTIFY dialog = call-id %v, cseq %v", requestCallID, cseq)
		}
		if headers := request.GetHeaders("Event"); len(headers) != 1 || !strings.Contains(headers[0].String(), gb10DeviceID) {
			t.Fatalf("NOTIFY Event = %v", headers)
		}
		if headers := request.GetHeaders("Subscription-State"); len(headers) != 1 || !strings.Contains(headers[0].String(), "active;expires=") {
			t.Fatalf("NOTIFY Subscription-State = %v", headers)
		}
		if got := request.GetHeaders("CSeq")[0].String(); !strings.Contains(got, strconv.Itoa(index+1)+" NOTIFY") {
			t.Fatalf("NOTIFY CSeq = %s", got)
		}
	}
}

func TestRewriteCascadeEventBodyFiltersAndMapsSharedChannel(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	body := []byte(`<?xml version="1.0"?><Notify><CmdType>Alarm</CmdType><SN>7</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><ParentID>lower-device</ParentID><AlarmPriority>1</AlarmPriority></Notify>`)
	rewritten, exposedID, err := rewriteCascadeEventBody(platform, body)
	if err != nil {
		t.Fatal(err)
	}
	if exposedID != testExposedChannelID {
		t.Fatal("shared cascade event was filtered")
	}
	text := string(rewritten)
	for _, expected := range []string{"<DeviceID>" + testExposedChannelID + "</DeviceID>", "<ParentID>" + gb10DeviceID + "</ParentID>"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("rewritten event missing %q: %s", expected, text)
		}
	}
	if strings.Contains(text, testCascadeChannelID) || strings.Contains(text, "lower-device") {
		t.Fatalf("rewritten event leaked local identifiers: %s", text)
	}
	unshared := strings.ReplaceAll(string(body), testCascadeChannelID, "34020000001320000099")
	if _, exposedID, err := rewriteCascadeEventBody(platform, []byte(unshared)); err != nil || exposedID != "" {
		t.Fatalf("unshared cascade event = exposed ID %q, err %v", exposedID, err)
	}
}

func TestRewriteCascadeEventBodyMapsNestedAppendixA4IDs(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>8</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><Info><alarmType><DeviceID>` + testCascadeChannelID + `</DeviceID><ParentID>` + gb10PlatformID + `</ParentID><DoorID>` + testCascadeChannelID + `</DoorID><SourceID>` + gb10PlatformID + `</SourceID><VendorField>retained</VendorField></alarmType><ExtraInfo>{"type":"doorEventType","DeviceID":"` + testCascadeChannelID + `","ParentID":"` + gb10PlatformID + `"}</ExtraInfo></Info></Notify>`)
	rewritten, exposedID, err := rewriteCascadeEventBodyForDevice(platform, body, gb10PlatformID)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rewritten)
	if exposedID != testExposedChannelID || !strings.Contains(text, "<VendorField>retained</VendorField>") || !strings.Contains(text, testExposedChannelID) || !strings.Contains(text, gb10DeviceID) {
		t.Fatalf("rewritten A.4 event = %s, exposed %s", text, exposedID)
	}
	if strings.Contains(text, testCascadeChannelID) || strings.Contains(text, gb10PlatformID) {
		t.Fatalf("rewritten A.4 event leaked local IDs: %s", text)
	}
}

func TestRewriteCascadeEventBodyRejectsUnknownNestedAppendixA4ID(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>9</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><Info><doorEventType><DoorID>34020000001320000099</DoorID></doorEventType></Info></Notify>`)
	if _, _, err := rewriteCascadeEventBody(platform, body); err == nil {
		t.Fatal("event with unshared nested A.4 ID was forwarded")
	}
}

func TestCascadeAlarmNotifyUsesMappedSharedChannel(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	remoteURI, _ := sip.ParseSipURI("sip:" + gb10PlatformID + "@remote.example")
	localURI, _ := sip.ParseSipURI("sip:" + gb10DeviceID + "@local.example")
	remoteContactURI, _ := sip.ParseSipURI("sip:" + gb10PlatformID + "@192.0.2.30:5060")
	callID := sip.CallID("cascade-alarm-subscribe")
	subscribe := sip.NewRequest("", sip.MethodSubscribe, &localURI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().
			SetFrom(&sip.Address{URI: &remoteURI, Params: sip.NewParams().Add("tag", sip.String{Str: "remote-tag"})}).
			SetTo(&sip.Address{URI: &localURI, Params: sip.NewParams()}).
			SetMethod(sip.MethodSubscribe).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	sub := &eventSubscription{
		CmdType: "Alarm", DeviceID: testExposedChannelID, Event: "presence", ExpiresAt: time.Now().Add(time.Hour),
		To: &sip.Address{URI: &remoteContactURI, Params: sip.NewParams()}, GBVersion: "1.1",
		DialogRequest: subscribe, Response: sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil),
		Contact: worker.contactAddress(), Cascade: worker,
	}
	requests := make(chan *sip.Request, 1)
	worker.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		requests <- in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{}
	t.Cleanup(func() {
		api.beginClose()
		api.lifecycleWG.Wait()
	})
	api.eventSubscribers.Store("alarm", sub)
	body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>7</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority></Notify>`)
	api.publishEventNotify("Alarm", gb10DeviceID, body)
	var request *sip.Request
	select {
	case request = <-requests:
	case <-time.After(time.Second):
		t.Fatal("cascade Alarm NOTIFY was not sent")
	}
	if !strings.Contains(string(request.Body()), "<DeviceID>"+testExposedChannelID+"</DeviceID>") || strings.Contains(string(request.Body()), testCascadeChannelID) {
		t.Fatalf("cascade Alarm NOTIFY body: %s", request.Body())
	}
	if headers := request.GetHeaders("Event"); len(headers) != 1 || !strings.Contains(headers[0].String(), "presence") {
		t.Fatalf("cascade Alarm NOTIFY Event = %v", headers)
	}
	secondLocalID := "34020000001320000012"
	secondExposedID := "34020000001320000912"
	worker.platform.channelIDMap[secondLocalID] = secondExposedID
	worker.platform.exposedChannelMap[secondExposedID] = secondLocalID
	otherShared := strings.ReplaceAll(string(body), testCascadeChannelID, secondLocalID)
	api.publishEventNotify("Alarm", gb10DeviceID, []byte(otherShared))
	select {
	case request = <-requests:
		t.Fatalf("event for a different shared target was sent to targeted subscription: %s", request)
	case <-time.After(100 * time.Millisecond):
	}
	unshared := strings.ReplaceAll(string(body), testCascadeChannelID, "34020000001320000099")
	api.publishEventNotify("Alarm", gb10DeviceID, []byte(unshared))
	select {
	case request = <-requests:
		t.Fatalf("unshared Alarm event was sent to upstream: %s", request)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCascadePTZPositionNotifyUsesMappedSharedChannel(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	remoteURI, _ := sip.ParseSipURI("sip:" + gb10PlatformID + "@remote.example")
	localURI, _ := sip.ParseSipURI("sip:" + gb10DeviceID + "@local.example")
	remoteContactURI, _ := sip.ParseSipURI("sip:" + gb10PlatformID + "@192.0.2.30:5060")
	callID := sip.CallID("cascade-ptz-position-subscribe")
	subscribe := sip.NewRequest("", sip.MethodSubscribe, &localURI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().
			SetFrom(&sip.Address{URI: &remoteURI, Params: sip.NewParams().Add("tag", sip.String{Str: "remote-tag"})}).
			SetTo(&sip.Address{URI: &localURI, Params: sip.NewParams()}).
			SetMethod(sip.MethodSubscribe).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	sub := &eventSubscription{
		CmdType: "PTZPosition", DeviceID: testExposedChannelID, Event: "presence", ExpiresAt: time.Now().Add(time.Hour),
		To: &sip.Address{URI: &remoteContactURI, Params: sip.NewParams()}, GBVersion: string(GBVersion30),
		DialogRequest: subscribe, Response: sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil),
		Contact: worker.contactAddress(), Cascade: worker,
	}
	requests := make(chan *sip.Request, 1)
	worker.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		requests <- in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{}
	t.Cleanup(func() {
		api.beginClose()
		api.lifecycleWG.Wait()
	})
	api.eventSubscribers.Store("ptz-position", sub)
	body := []byte(`<Notify><CmdType>PTZPosition</CmdType><SN>10</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><Pan>12.5</Pan><Tilt>3.5</Tilt><Zoom>2</Zoom></Notify>`)
	api.publishEventNotify("PTZPosition", gb10DeviceID, body)
	var request *sip.Request
	select {
	case request = <-requests:
	case <-time.After(time.Second):
		t.Fatal("cascade PTZPosition NOTIFY was not sent")
	}
	text := string(request.Body())
	if !strings.Contains(text, "<DeviceID>"+testExposedChannelID+"</DeviceID>") || strings.Contains(text, testCascadeChannelID) || !strings.Contains(text, "<Pan>12.5</Pan>") {
		t.Fatalf("cascade PTZPosition NOTIFY body: %s", text)
	}
}

func TestCascadeSubscriptionTargetRules(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	systemID := "34020000002000000002"
	groupID := "34020000002150000001"
	virtualID := "34020000002160000001"
	tests := []struct {
		cmdType  string
		deviceID string
		allowed  bool
	}{
		{"Catalog", gb10DeviceID, true},
		{"Catalog", "*", true},
		{"Catalog", testExposedChannelID, true},
		// 目录编码由 sipSubscribeEvent 使用存储支持的可见目录图继续校验，快速门禁不直接放行。
		{"Catalog", "340200", false},
		{"Catalog", systemID, false},
		{"Catalog", groupID, false},
		{"Catalog", virtualID, false},
		{"Catalog", "34020", false},
		{"Catalog", "34020000001320000099", false},
		{"Alarm", gb10DeviceID, true},
		{"Alarm", testExposedChannelID, true},
		{"MobilePosition", testExposedChannelID, true},
		{"PTZPosition", testExposedChannelID, true},
		{"Alarm", "34020000001320000099", false},
	}
	for _, test := range tests {
		if got := cascadeSubscriptionTargetAllowed(platform, test.cmdType, test.deviceID); got != test.allowed {
			t.Errorf("%s target %s allowed = %v, want %v", test.cmdType, test.deviceID, got, test.allowed)
		}
	}
}

func TestCascadePTZPositionSubscriptionRequires30(t *testing.T) {
	for _, test := range []struct {
		name    string
		version GBProtocolVersion
		expires string
		status  string
	}{
		{name: "2.0 rejects create", version: GBVersion20, expires: "60", status: "SIP/2.0 400"},
		{name: "3.0 accepts create", version: GBVersion30, expires: "60", status: "SIP/2.0 200"},
		{name: "2.0 rejects missing-dialog cancel", version: GBVersion20, expires: "0", status: "SIP/2.0 481"},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform := testSharedCascadePlatform(t)
			local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
			remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
			server := &Server{Server: sip.NewServer(local)}
			api := &GB28181API{svr: server}
			server.gb = api
			worker := newCascadeWorker(server, platform)
			worker.mu.Lock()
			worker.effective = test.version
			worker.mu.Unlock()

			connection := newFlowConnection()
			connection.remote = platform.remote
			callID := sip.CallID("cascade-ptz-subscribe-" + string(test.version))
			body, err := sip.XMLEncode(subscribeEventRequest{CmdType: "PTZPosition", SN: 19, DeviceID: testExposedChannelID})
			if err != nil {
				t.Fatal(err)
			}
			request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
					AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), body)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: test.expires})
			request.SetConnection(connection)
			request.SetSource(connection.remote)
			request.SetDestination(connection.local)
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction("cascade-ptz-subscribe", connection),
				DeviceID: platform.serverID, Source: connection.remote, To: remote, XGBVer: string(test.version),
			}
			ctx.Set(cascadeWorkerContextKey, worker)
			api.sipSubscribeEvent(ctx)
			select {
			case response := <-connection.writes:
				if !strings.Contains(string(response), test.status) {
					t.Fatalf("PTZPosition SUBSCRIBE response = %s", response)
				}
			case <-time.After(time.Second):
				t.Fatal("PTZPosition SUBSCRIBE response timeout")
			}
		})
	}
}

func TestCascadePTZPositionSubscriptionAllowsOlderVersionCancel(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	server := &Server{Server: sip.NewServer(local)}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, platform)
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()

	connection := newFlowConnection()
	connection.remote = platform.remote
	callID := sip.CallID("cascade-ptz-version-downgrade-cancel")
	from := remote.Clone()
	from.Params.Add("tag", sip.String{Str: "upstream-tag"})
	body, err := sip.XMLEncode(subscribeEventRequest{CmdType: "PTZPosition", SN: 20, DeviceID: testExposedChannelID})
	if err != nil {
		t.Fatal(err)
	}
	makeRequest := func(expires string, localTag string, cseq uint32) *sip.Request {
		request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
			sip.NewHeaderBuilder().SetFrom(from).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
				AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), body)
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: expires})
		request.SetConnection(connection)
		request.SetSource(connection.remote)
		request.SetDestination(connection.local)
		applyInboundSubscribeDialog(t, request, localTag, cseq)
		return request
	}
	invoke := func(request *sip.Request, version GBProtocolVersion, txID string) string {
		ctx := &sip.Context{
			Request: request, Tx: sip.NewTransaction(txID, connection), DeviceID: platform.serverID,
			Source: connection.remote, To: remote, XGBVer: string(version),
		}
		ctx.Set(cascadeWorkerContextKey, worker)
		api.sipSubscribeEvent(ctx)
		return <-flowResponse(t, connection)
	}

	assertFlowOK(t, invoke(makeRequest("60", "", 1), GBVersion30, "cascade-ptz-create"))
	var subscription *eventSubscription
	api.eventSubscribers.Range(func(_, value any) bool {
		subscription, _ = value.(*eventSubscription)
		return false
	})
	if subscription == nil || subscription.LocalTag == "" {
		t.Fatal("PTZPosition subscription dialog was not stored")
	}
	worker.mu.Lock()
	worker.effective = GBVersion20
	worker.mu.Unlock()
	assertFlowOK(t, invoke(makeRequest("0", subscription.LocalTag, 2), GBVersion20, "cascade-ptz-cancel"))
	if _, exists := api.eventSubscribers.Load(subscription.Key); exists {
		t.Fatal("older negotiated version did not cancel existing PTZPosition subscription")
	}
}

func TestCascadeAlarmSubscriptionDoesNotSendInitialCatalog(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	platform.sharedChannels = nil
	platform.channelIDMap = map[string]string{}
	platform.exposedChannelMap = map[string]string{}
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	server := &Server{Server: sip.NewServer(local)}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, platform)
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	notifies := make(chan *sip.Request, 1)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		notifies <- request
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}

	connection := newFlowConnection()
	connection.remote = platform.remote
	contact := mustFlowAddress(t, "sip:"+platform.serverID+"@192.0.2.30:5060")
	callID := sip.CallID("cascade-alarm-subscribe")
	body, err := sip.XMLEncode(subscribeEventRequest{CmdType: "Alarm", SN: 18, DeviceID: platform.localID})
	if err != nil {
		t.Fatal(err)
	}
	request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetContact(contact).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), body)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-alarm-subscribe", connection),
		DeviceID: platform.serverID, Source: connection.remote, To: remote, XGBVer: string(GBVersion11),
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipSubscribeEvent(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200 OK") {
			t.Fatalf("Alarm SUBSCRIBE response: %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("Alarm SUBSCRIBE response timeout")
	}
	select {
	case notify := <-notifies:
		t.Fatalf("Alarm subscription received unexpected initial NOTIFY: %s", notify.Body())
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNormalizeCascadePlatformValidatesSharedChannelMapping(t *testing.T) {
	base := conf.SIPUpstream{
		Name: "provincial", Enabled: true, ServerID: gb10PlatformID,
		Host: "192.0.2.30", LocalID: gb10DeviceID, LocalHost: "192.0.2.20",
		SharedChannels: []string{testCascadeChannelID},
	}
	local := conf.SIP{ID: gb10DeviceID, Domain: "3402000000", Host: "192.0.2.20", Port: 5060}

	invalid := base
	invalid.ChannelIDMap = map[string]string{"34020000001320000012": testExposedChannelID}
	if _, err := normalizeCascadePlatform(invalid, local, ""); err == nil || !strings.Contains(err.Error(), "source is not shared") {
		t.Fatalf("unshared mapping error = %v", err)
	}
	invalid = base
	invalid.ChannelIDMap = map[string]string{testCascadeChannelID: "bad-id"}
	if _, err := normalizeCascadePlatform(invalid, local, ""); err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("invalid target mapping error = %v", err)
	}
}
