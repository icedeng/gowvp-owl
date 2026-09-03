package gbs

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestGBProtocolFixtureManifest(t *testing.T) {
	required := []string{
		"register-initial.sip",
		"register-auth.sip",
		"register-401.sip",
		"register-200.sip",
		"keepalive.xml",
		"catalog-response.xml",
		"record-info-response.xml",
		"alarm-notify.xml",
		"invite.sdp",
		"media-status-notify.xml",
	}
	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		version := version
		t.Run(version, func(t *testing.T) {
			for _, name := range required {
				path := filepath.Join("testdata", "gb28181", version, name)
				if info, err := os.Stat(path); err != nil {
					t.Errorf("required fixture %s: %v", path, err)
				} else if info.Size() == 0 {
					t.Errorf("required fixture %s is empty", path)
				}
			}
		})
	}
}

func TestGBProtocolVersionFixtureMatrix(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	defer func() { config = previousConfig }()

	tests := []struct {
		name               string
		directory          string
		version            GBProtocolVersion
		wantFileSize       string
		wantRecordLocation string
		wantStreamNumber   int
		wantAlarmMethod    string
		wantAlarmType      string
		wantEventType      int
		wantExtraInfo      bool
		wantCatalogInfo    bool
		wantProtocol       string
	}{
		{name: "2011", directory: "1.0", version: GBVersion10, wantAlarmMethod: "2", wantProtocol: "RTP/AVP"},
		{name: "2014", directory: "1.1", version: GBVersion11, wantAlarmMethod: "2", wantCatalogInfo: true, wantProtocol: "RTP/AVP"},
		{name: "2016", directory: "2.0", version: GBVersion20, wantFileSize: "1048576", wantAlarmMethod: "2", wantAlarmType: "1", wantCatalogInfo: true, wantProtocol: "TCP/RTP/AVP"},
		{name: "2022", directory: "3.0", version: GBVersion30, wantFileSize: "2097152", wantRecordLocation: gb10DeviceID, wantStreamNumber: 2, wantAlarmMethod: "5", wantAlarmType: "6", wantEventType: 2, wantExtraInfo: true, wantCatalogInfo: true, wantProtocol: "TCP/RTP/AVP"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			core, _, _ := newCascadeMediaCore(t)
			conn := newFlowConnection()
			platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
			sipServer := sip.NewServer(platform)
			defer sipServer.Close()

			memory := newFlowMemory(gb10DeviceID)
			memory.persistent.Ext.GBEffectiveVersion = string(test.version)
			memory.persistent.Ext.GBDeclaredVersion = string(test.version)
			memory.persistent.Ext.GBVersionSource = "fixture"
			memory.runtime.setGBVersion(test.version)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			api := &GB28181API{
				cfg:              &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"},
				catalogResponses: newMultiResponseCollector(func(item Channels) string { return item.ChannelID }),
				core:             core,
				recordResponses: newMultiResponseCollector(func(item RecordItem) string {
					return item.DeviceID + item.FilePath + item.StartTime + item.EndTime
				}),
				streams: &conc.Map[string, *Streams]{},
			}
			api.svr = &Server{
				Server:       sipServer,
				gb:           api,
				fromAddress:  *platform,
				memoryStorer: memory,
			}

			memory.persistent.IsOnline = false
			memory.runtime.UpdateRuntime(func(device *Device) { device.IsOnline = false })
			response := runFlowHandler(t, conn, api, sip.MethodMessage, "fixture-keepalive-"+test.directory, readGBVersionFixture(t, test.directory, "keepalive.xml"), api.sipMessageKeepalive)
			assertFlowOK(t, response)
			if !memory.persistent.IsOnline || memory.persistent.KeepaliveAt.Time.IsZero() || memory.persistent.Ext.GBEffectiveVersion != string(test.version) {
				t.Fatalf("Keepalive state = online:%v at:%v version:%q", memory.persistent.IsOnline, memory.persistent.KeepaliveAt.Time, memory.persistent.Ext.GBEffectiveVersion)
			}

			catalogBody := readGBVersionFixture(t, test.directory, "catalog-response.xml")
			var catalogEnvelope struct {
				SN       int    `xml:"SN"`
				DeviceID string `xml:"DeviceID"`
			}
			if err := sip.XMLDecode(catalogBody, &catalogEnvelope); err != nil {
				t.Fatalf("decode Catalog fixture: %v", err)
			}
			catalogKey := buildMultiResponseKey(catalogEnvelope.DeviceID, "Catalog", catalogEnvelope.SN)
			if entry := api.catalogResponses.Start(catalogKey); entry == nil {
				t.Fatal("start Catalog collector")
			}
			response = runFlowHandler(t, conn, api, sip.MethodMessage, "fixture-catalog-"+test.directory, catalogBody, api.sipMessageCatalog)
			assertFlowOK(t, response)
			catalogResult := api.catalogResponses.Wait(context.Background(), catalogKey)
			if !catalogResult.Complete || len(catalogResult.Items) != 1 {
				t.Fatalf("Catalog result = %+v", catalogResult)
			}
			catalogItem := catalogResult.Items[0]
			if catalogItem.ChannelID != gb10ChannelID || (catalogItem.Info.XMLName.Local != "") != test.wantCatalogInfo {
				t.Fatalf("Catalog item = %+v", catalogItem)
			}

			offer, err := parseCascadeVideoOffer(
				readGBVersionFixture(t, test.directory, "invite.sdp"),
				test.version,
				cascadePlatform{remote: &net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060}},
			)
			if err != nil {
				t.Fatalf("parse INVITE SDP fixture: %v", err)
			}
			if offer.Version != test.version || offer.Protocol != test.wantProtocol || offer.IsUDP != (test.wantProtocol == "RTP/AVP") || offer.SSRC != "0100000001" || offer.Payload != 96 || offer.Port != 30000 {
				t.Fatalf("INVITE SDP offer = %+v", offer)
			}

			recordBody := readGBVersionFixture(t, test.directory, "record-info-response.xml")
			var recordEnvelope struct {
				SN       int    `xml:"SN"`
				DeviceID string `xml:"DeviceID"`
			}
			if err := sip.XMLDecode(recordBody, &recordEnvelope); err != nil {
				t.Fatalf("decode RecordInfo fixture: %v", err)
			}
			recordKey := buildMultiResponseKey(recordEnvelope.DeviceID, "RecordInfo", recordEnvelope.SN)
			generation := api.startRecordResponseExtra(recordKey)
			if entry := api.recordResponses.Start(recordKey); entry == nil {
				t.Fatal("start RecordInfo collector")
			}
			response = runFlowHandler(t, conn, api, sip.MethodMessage, "fixture-record-"+test.directory, recordBody, api.sipMessageRecordInfo)
			assertFlowOK(t, response)
			recordResult := api.recordResponses.Wait(context.Background(), recordKey)
			if !recordResult.Complete || len(recordResult.Items) != 1 {
				t.Fatalf("RecordInfo result = %+v", recordResult)
			}
			item := recordResult.Items[0]
			if item.FileSize != test.wantFileSize || item.HasFileSize != (test.wantFileSize != "") {
				t.Fatalf("RecordInfo FileSize = %q present:%v", item.FileSize, item.HasFileSize)
			}
			if item.RecordLocation != test.wantRecordLocation || item.HasRecordLocation != (test.wantRecordLocation != "") {
				t.Fatalf("RecordInfo RecordLocation = %q present:%v", item.RecordLocation, item.HasRecordLocation)
			}
			if test.wantStreamNumber == 0 {
				if item.StreamNumber != nil {
					t.Fatalf("RecordInfo StreamNumber = %d, want absent", *item.StreamNumber)
				}
			} else if item.StreamNumber == nil || *item.StreamNumber != test.wantStreamNumber {
				t.Fatalf("RecordInfo StreamNumber = %v, want %d", item.StreamNumber, test.wantStreamNumber)
			}
			metadata := api.takeRecordResponseMetadata(recordKey, generation)
			if got := len(metadata.ExtraInfo) > 0; got != test.wantExtraInfo {
				t.Fatalf("RecordInfo ExtraInfo = %+v", metadata.ExtraInfo)
			}
			if len(metadata.ResponseXML) != 1 {
				t.Fatalf("RecordInfo raw response count = %d", len(metadata.ResponseXML))
			}

			alarmEvents := make(chan *AlarmEvent, 1)
			api.SetAlarmHandler(func(_ context.Context, event *AlarmEvent) { alarmEvents <- event })
			response = runFlowHandler(t, conn, api, sip.MethodMessage, "fixture-alarm-"+test.directory, readGBVersionFixture(t, test.directory, "alarm-notify.xml"), api.sipMessageAlarm)
			assertFlowOK(t, response)
			select {
			case event := <-alarmEvents:
				if event.ChannelID != gb10ChannelID || event.AlarmMethod != test.wantAlarmMethod || event.AlarmType != test.wantAlarmType {
					t.Fatalf("Alarm event = %+v", event)
				}
				if test.wantEventType == 0 {
					if event.EventType != nil {
						t.Fatalf("Alarm EventType = %d, want absent", *event.EventType)
					}
				} else if event.EventType == nil || *event.EventType != test.wantEventType {
					t.Fatalf("Alarm EventType = %v, want %d", event.EventType, test.wantEventType)
				}
			case <-time.After(time.Second):
				t.Fatal("Alarm event timeout")
			}

			callID := "fixture-media-" + strings.ReplaceAll(test.directory, ".", "-")
			streamKey := historyKey(historyModeDownload, gb10DeviceID, gb10ChannelID)
			stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: callID}
			api.streams.Store(streamKey, stream)
			response = runFlowHandler(t, conn, api, sip.MethodMessage, callID, readGBVersionFixture(t, test.directory, "media-status-notify.xml"), api.sipMessageMediaStatus)
			assertFlowOK(t, response)
			if _, ok := api.streams.Load(streamKey); ok || !stream.Stop || stream.EndReason != "media_status" {
				t.Fatalf("MediaStatus stream = %+v", stream)
			}
		})
	}
}

func readGBVersionFixture(t *testing.T, version, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "gb28181", version, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
