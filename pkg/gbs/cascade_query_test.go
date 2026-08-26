package gbs

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
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
	if len(supplement) != 1 || supplement[0].Info == nil || supplement[0].Info.PTZType != 3 || supplement[0].Info.Resolution != "1920x1080" {
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
	for _, expected := range []string{`<Response>`, `<SumNum>1</SumNum>`, `<DeviceList Num="1">`, `<DeviceID>` + testExposedChannelID + `</DeviceID>`, `<Info>`} {
		if !strings.Contains(xmlText, expected) {
			t.Fatalf("catalog XML missing %q: %s", expected, xmlText)
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
	item := cascadeCatalogItem{DeviceID: testExposedChannelID, Status: "ON"}
	notify := cascadeCatalogNotify{
		CmdType: "Catalog", SN: 9, DeviceID: testExposedChannelID, SumNum: 1,
		DeviceList: cascadeCatalogDeviceList{Num: 1, Items: []cascadeCatalogItem{item}},
	}
	legacy, err := encodeCascadeCatalogNotify(GBVersion10, notify)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(legacy), "<Response>") || strings.Contains(string(legacy), "<Notify>") {
		t.Fatalf("2011 Catalog NOTIFY body root:\n%s", legacy)
	}
	modern, err := encodeCascadeCatalogNotify(GBVersion11, notify)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(modern), "<Notify>") || strings.Contains(string(modern), "<Response>") {
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

func TestCascadeUnsupportedQueryReturnsExplicitError(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	var request *sip.Request
	worker.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		request = in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{}
	api.respondCascadeQuery(worker, cascadeQueryEnvelope{CmdType: "SDCardStatus", SN: 9, DeviceID: gb10DeviceID})
	if request == nil {
		t.Fatal("unsupported cascade query did not receive a business response")
	}
	body := string(request.Body())
	for _, expected := range []string{"<CmdType>SDCardStatus</CmdType>", "<SN>9</SN>", "<DeviceID>" + gb10DeviceID + "</DeviceID>", "<Result>ERROR</Result>"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("unsupported cascade response missing %q: %s", expected, body)
		}
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
		{name: "2014 preset", cmdType: "PresetQuery", version: GBVersion11, allowed: true},
		{name: "2011 config", cmdType: "ConfigDownload", version: GBVersion10},
		{name: "2014 config", cmdType: "ConfigDownload", version: GBVersion11, allowed: true},
		{name: "2014 home position", cmdType: "HomePositionQuery", version: GBVersion11},
		{name: "2016 home position", cmdType: "HomePositionQuery", version: GBVersion20, allowed: true},
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
		{name: "2016 base", value: "AudioParamConfig", version: GBVersion20, want: "AudioParamConfig", allowed: true},
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
	first := `<Response><CmdType>ConfigDownload</CmdType><SN>4321</SN><DeviceID>` + channel.ChannelID + `</DeviceID><Result>OK</Result><BasicParam><Name>IPC</Name></BasicParam></Response>`
	second := `<Response><CmdType>ConfigDownload</CmdType><SN>4321</SN><DeviceID>` + channel.ChannelID + `</DeviceID><Result>OK</Result><VideoParamOpt><VideoFormatOpt>2/5</VideoFormatOpt></VideoParamOpt></Response>`
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
			XML: `<?xml version="1.0" encoding="UTF-8"?><Response><CmdType>PresetQuery</CmdType><SN>4321</SN><DeviceID>` + channel.ChannelID + `</DeviceID><Result>OK</Result><ParentID>` + channel.DeviceID + `</ParentID><PresetList Num="1"><Item><DeviceID>` + channel.ChannelID + `</DeviceID><ParentID>` + channel.DeviceID + `</ParentID><PresetID>1</PresetID><VendorField>retained</VendorField></Item></PresetList></Response>`,
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
		"<CmdType>PresetQuery</CmdType>", "<SN>97</SN>",
		"<DeviceID>" + testExposedChannelID + "</DeviceID>",
		"<ParentID>" + gb10DeviceID + "</ParentID>", "<VendorField>retained</VendorField>",
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
	if _, err := rewriteCascadeQueryResponse(body, cascadeQueryEnvelope{CmdType: "DeviceStatus", SN: 81, DeviceID: testExposedChannelID}, platform, channel); err == nil {
		t.Fatal("query response with unshared A.4 ID was forwarded")
	}
}

func TestRewriteCascadeExtraInfoMapsNumericIdentifiersWithoutPrecisionLoss(t *testing.T) {
	platform := cascadePlatform{
		localID:           gb10PlatformID,
		channelIDMap:      map[string]string{testCascadeChannelID: testExposedChannelID},
		exposedChannelMap: map[string]string{testExposedChannelID: testCascadeChannelID},
	}
	rewritten, err := rewriteCascadeOpaqueIdentifiers(
		`[{"type":"doorType","DeviceID":`+testCascadeChannelID+`,"Sequence":100,"Zero":0}]`,
		"ExtraInfo", platform, testCascadeChannelID, testExposedChannelID,
	)
	if err != nil {
		t.Fatalf("rewrite numeric ExtraInfo: %v", err)
	}
	if !strings.Contains(rewritten, `"DeviceID":`+testExposedChannelID) ||
		!strings.Contains(rewritten, `"Sequence":100`) || !strings.Contains(rewritten, `"Zero":0`) {
		t.Fatalf("numeric ExtraInfo mapping = %s", rewritten)
	}

	if _, err := rewriteCascadeOpaqueIdentifiers(
		`{"type":"doorType","DeviceID":34020000001320000099}`,
		"ExtraInfo", platform, testCascadeChannelID, testExposedChannelID,
	); err == nil {
		t.Fatal("unknown numeric A.4 identifier was not rejected")
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
		"<DeviceID>" + testExposedChannelID + "</DeviceID>", "<Result>ERROR</Result>",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("cascade error response missing %q: %s", expected, body)
		}
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
			items[index] = RecordItem{
				DeviceID: channel.ChannelID, Name: "Front Gate", FilePath: strconv.Itoa(index + 1),
				StartTime: "2024-04-01T00:00:00", EndTime: "2024-04-01T00:10:00",
				Secrecy: 0, Type: "time", RecorderID: channel.DeviceID,
				FileSize: "1024", RecordLocation: channel.DeviceID, StreamNumber: &streamNumber,
			}
		}
		return items, nil
	}
	query := cascadeQueryEnvelope{
		CmdType: "RecordInfo", SN: 88, DeviceID: testExposedChannelID,
		StartTime: "2024-04-01T00:00:00", EndTime: "2024-04-01T01:00:00",
		Type: "ALL", StreamNumber: intPointer(2), AlarmMethod: "5", AlarmType: "13",
	}
	if err := api.respondCascadeRecordInfo(t.Context(), worker, query); err != nil {
		t.Fatal(err)
	}
	if downstream == nil || downstream.DeviceID != channel.DeviceID || downstream.ChannelID != channel.ChannelID || downstream.End <= downstream.Start ||
		downstream.Type != "all" || downstream.StreamNumber == nil || *downstream.StreamNumber != 2 || downstream.AlarmMethod != "5" || downstream.AlarmType != "13" {
		t.Fatalf("downstream RecordInfo query = %+v", downstream)
	}
	if len(requests) != 2 {
		t.Fatalf("RecordInfo response chunks = %d, want 2", len(requests))
	}
	for index, request := range requests {
		body := string(request.Body())
		for _, expected := range []string{
			"<CmdType>RecordInfo</CmdType>", "<SN>88</SN>", "<DeviceID>" + testExposedChannelID + "</DeviceID>", "<SumNum>21</SumNum>",
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
			err := api.respondCascadeRecordInfo(t.Context(), worker, cascadeQueryEnvelope{
				CmdType: "RecordInfo", SN: 188, DeviceID: testExposedChannelID, Type: "ALL",
				StartTime: "2024-04-01T00:00:00", EndTime: "2024-04-01T01:00:00",
			})
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
	if response == nil || !strings.Contains(string(response.Body()), "<Result>ERROR</Result>") {
		t.Fatalf("legacy upstream RecordInfo filter response = %v", response)
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
	}, nil, ""); err != nil {
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
		{name: "missing cruise number", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "CruiseTrackQuery", SN: 1, DeviceID: testExposedChannelID}},
		{name: "invalid cruise number", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "CruiseTrackQuery", SN: 1, DeviceID: testExposedChannelID, Number: intPointer(2)}},
		{name: "negative mobile interval", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "MobilePosition", SN: 1, DeviceID: testExposedChannelID, Interval: -1}},
		{name: "missing config type", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "ConfigDownload", SN: 1, DeviceID: testExposedChannelID}},
		{name: "unknown config type", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "ConfigDownload", SN: 1, DeviceID: testExposedChannelID, ConfigType: "VendorConfig"}},
		{name: "missing record time", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "RecordInfo", SN: 1, DeviceID: testExposedChannelID}},
		{name: "reversed record time", query: cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "RecordInfo", SN: 1, DeviceID: testExposedChannelID, StartTime: validRecord.EndTime, EndTime: validRecord.StartTime}},
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
	validCruise := cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "CruiseTrackQuery", SN: 1, DeviceID: testExposedChannelID, Number: intPointer(0)}
	if err := validateCascadeQueryRequest(validCruise); err != nil {
		t.Fatalf("valid CruiseTrackQuery number 0 rejected: %v", err)
	}
	validConfig := cascadeQueryEnvelope{XMLName: xml.Name{Local: "Query"}, CmdType: "ConfigDownload", SN: 1, DeviceID: testExposedChannelID, ConfigType: "BasicParam/VideoParamOpt"}
	if err := validateCascadeQueryRequest(validConfig); err != nil {
		t.Fatalf("valid ConfigDownload rejected: %v", err)
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

func TestCascadeSharedChannelInfoAndStatusUseExposedID(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
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
		"<DeviceName>" + channel.Name + "</DeviceName>", "<DeviceType>IPC</DeviceType>",
	} {
		if !strings.Contains(info, expected) {
			t.Fatalf("shared DeviceInfo missing %q: %s", expected, info)
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
	var request *sip.Request
	worker.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		request = in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{}
	api.eventSubscribers.Store("alarm", sub)
	body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>7</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority></Notify>`)
	api.publishEventNotify("Alarm", gb10DeviceID, body)
	if request == nil {
		t.Fatal("cascade Alarm NOTIFY was not sent")
	}
	if !strings.Contains(string(request.Body()), "<DeviceID>"+testExposedChannelID+"</DeviceID>") || strings.Contains(string(request.Body()), testCascadeChannelID) {
		t.Fatalf("cascade Alarm NOTIFY body: %s", request.Body())
	}
	if headers := request.GetHeaders("Event"); len(headers) != 1 || !strings.Contains(headers[0].String(), "presence") {
		t.Fatalf("cascade Alarm NOTIFY Event = %v", headers)
	}
	request = nil
	secondLocalID := "34020000001320000012"
	secondExposedID := "34020000001320000912"
	worker.platform.channelIDMap[secondLocalID] = secondExposedID
	worker.platform.exposedChannelMap[secondExposedID] = secondLocalID
	otherShared := strings.ReplaceAll(string(body), testCascadeChannelID, secondLocalID)
	api.publishEventNotify("Alarm", gb10DeviceID, []byte(otherShared))
	if request != nil {
		t.Fatal("event for a different shared target was sent to targeted subscription")
	}
	request = nil
	unshared := strings.ReplaceAll(string(body), testCascadeChannelID, "34020000001320000099")
	api.publishEventNotify("Alarm", gb10DeviceID, []byte(unshared))
	if request != nil {
		t.Fatal("unshared Alarm event was sent to upstream")
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
	var request *sip.Request
	worker.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		request = in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{}
	api.eventSubscribers.Store("ptz-position", sub)
	body := []byte(`<Notify><CmdType>PTZPosition</CmdType><SN>10</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><Pan>12.5</Pan><Tilt>3.5</Tilt><Zoom>2</Zoom></Notify>`)
	api.publishEventNotify("PTZPosition", gb10DeviceID, body)
	if request == nil {
		t.Fatal("cascade PTZPosition NOTIFY was not sent")
	}
	text := string(request.Body())
	if !strings.Contains(text, "<DeviceID>"+testExposedChannelID+"</DeviceID>") || strings.Contains(text, testCascadeChannelID) || !strings.Contains(text, "<Pan>12.5</Pan>") {
		t.Fatalf("cascade PTZPosition NOTIFY body: %s", text)
	}
}

func TestCascadeSubscriptionTargetRules(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	tests := []struct {
		cmdType  string
		deviceID string
		allowed  bool
	}{
		{"Catalog", gb10DeviceID, true},
		{"Catalog", "*", true},
		{"Catalog", testExposedChannelID, true},
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
