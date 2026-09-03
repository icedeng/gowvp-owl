package gbs

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestCascadeFourVersionProtocolMatrix(t *testing.T) {
	channel := &ipc.Channel{
		ChannelID: testCascadeChannelID, Name: "   ", PTZType: 3, IsOnline: true,
		Ext: ipc.DeviceExt{Manufacturer: " Vendor ", Model: " IPC ", GBCatalog: &ipc.GBCatalogExt{Resolution: "1920x1080"}},
	}
	const start, end = int64(1711929600), int64(1711933200)
	tests := []struct {
		name               string
		version            GBProtocolVersion
		catalogInfo        bool
		rtpOverTCP         bool
		directTCPDownload  bool
		downloadSpeed      bool
		mobilePosition     bool
		initialCatalog     bool
		voiceBroadcast     bool
		voicePayload       int
		voiceMapping       string
		historyRTSPVersion string
		catalogEvent       string
	}{
		{"2011", GBVersion10, false, false, false, false, false, false, false, 0, "", "MANSRTSP/1.0", "presence"},
		{"2014", GBVersion11, true, false, true, true, false, true, true, 96, "PS/90000", "RTSP/1.0", "Catalog;id=1894"},
		{"2016", GBVersion20, true, true, false, true, true, true, true, 8, "PCMA/8000", "RTSP/1.0", "Catalog;id=1894"},
		{"2022", GBVersion30, true, true, false, true, true, true, true, 8, "PCMA/8000", "RTSP/1.0", "Catalog;id=1894"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			platform := testSharedCascadePlatform(t)
			platform.version = test.version
			worker := newCascadeWorker(nil, platform)
			register := worker.newRegisterRequest(3600, nil)
			if headers := register.GetHeaders("X-GB-Ver"); len(headers) != 1 || !strings.Contains(headers[0].String(), string(test.version)) {
				t.Fatalf("REGISTER X-GB-Ver = %v", headers)
			}
			response := sip.NewResponseFromRequest("", register, http.StatusOK, "OK", nil)
			remoteVersion := sip.XGBVer(test.version)
			response.AppendHeader(&remoteVersion)
			if got, err := negotiateCascadeVersion(test.version, response); err != nil || got != test.version {
				t.Fatalf("negotiated version = %s, %v", got, err)
			}

			items := buildCascadeCatalogItems([]*ipc.Channel{channel}, platform, test.version)
			if len(items) != 1 || (items[0].Info != nil) != test.catalogInfo {
				t.Fatalf("Catalog profile = %+v", items)
			}
			if items[0].DeviceID != testExposedChannelID || items[0].ParentID != gb10DeviceID {
				t.Fatalf("Catalog mapping = %+v", items[0])
			}
			if items[0].Name != "   " || items[0].Manufacturer != " Vendor " || items[0].Model != " IPC " {
				t.Fatalf("Catalog ordinary string whitespace was changed: %+v", items[0])
			}
			alarmXML, err := xml.Marshal(cascadeAlarmStatusForVersion(test.version, 0))
			if err != nil {
				t.Fatal(err)
			}
			wantAlarmCount := `Num="0"`
			if test.version == GBVersion20 {
				wantAlarmCount = `num="0"`
			}
			if !strings.Contains(string(alarmXML), wantAlarmCount) {
				t.Fatalf("DeviceStatus Alarmstatus profile = %s, want %s", alarmXML, wantAlarmCount)
			}
			deviceInfo := cascadeDeviceInfoResponse{
				DeviceName: cascadeDeviceInfoName(test.version, "   "), Result: "OK", Channel: 1,
			}
			applyCascadeDeviceInfoCompatibility(&deviceInfo, test.version, "IPC", 1, 2)
			deviceInfoXML, err := xml.Marshal(deviceInfo)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(deviceInfoXML), "<DeviceName>") != test.version.AtLeast(GBVersion11) {
				t.Fatalf("DeviceInfo DeviceName profile = %s", deviceInfoXML)
			}
			if test.version.AtLeast(GBVersion11) && !strings.Contains(string(deviceInfoXML), "<DeviceName>   </DeviceName>") {
				t.Fatalf("DeviceInfo ordinary string whitespace was changed: %s", deviceInfoXML)
			}
			deviceInfoText := string(deviceInfoXML)
			resultIndex := strings.Index(deviceInfoText, "<Result>")
			if test.version.AtLeast(GBVersion11) && strings.Index(deviceInfoText, "<DeviceName>") > resultIndex {
				t.Fatalf("DeviceInfo DeviceName must precede Result: %s", deviceInfoXML)
			}
			maxCameraIndex, maxAlarmIndex, channelIndex := strings.Index(deviceInfoText, "<MaxCamera>"), strings.Index(deviceInfoText, "<MaxAlarm>"), strings.Index(deviceInfoText, "<Channel>")
			if test.version == GBVersion30 {
				if strings.Contains(deviceInfoText, "<DeviceType>") || maxCameraIndex >= 0 || maxAlarmIndex >= 0 {
					t.Fatalf("2022 DeviceInfo contains removed compatibility fields: %s", deviceInfoXML)
				}
				if channelIndex < resultIndex {
					t.Fatalf("2022 DeviceInfo Channel must follow Result: %s", deviceInfoXML)
				}
			} else if maxCameraIndex < resultIndex || maxAlarmIndex < maxCameraIndex || channelIndex < maxAlarmIndex {
				t.Fatalf("DeviceInfo compatibility field order = %s", deviceInfoXML)
			}

			if _, err := parseCascadeVideoOffer(cascadeOfferSDP("RTP/AVP", "192.0.2.30", ""), test.version, platform); err != nil {
				t.Fatalf("UDP Play rejected: %v", err)
			}
			_, tcpErr := parseCascadeVideoOffer(cascadeOfferSDP("TCP/RTP/AVP", "192.0.2.30", "passive"), test.version, platform)
			if (tcpErr == nil) != test.rtpOverTCP {
				t.Fatalf("TCP Play support = %v, err %v", tcpErr == nil, tcpErr)
			}
			if _, err := parseCascadeVideoOffer(cascadeHistoryOfferSDP(historyModeDownload, "RTP/AVP", "192.0.2.30", "", start, end, 0), test.version, platform); err != nil {
				t.Fatalf("RTP Download rejected: %v", err)
			}
			_, speedErr := parseCascadeVideoOffer(cascadeHistoryOfferSDP(historyModeDownload, "RTP/AVP", "192.0.2.30", "", start, end, 4), test.version, platform)
			if (speedErr == nil) != test.downloadSpeed {
				t.Fatalf("Download speed support = %v, err %v", speedErr == nil, speedErr)
			}

			capabilities := test.version.Capabilities()
			if capabilities.RTPOverTCP != test.rtpOverTCP || capabilities.DirectTCPDownload != test.directTCPDownload || capabilities.DownloadSpeed != test.downloadSpeed || capabilities.MobilePosition != test.mobilePosition {
				t.Fatalf("capabilities = %+v", capabilities)
			}
			if got := historyControlProtocolVersion(test.version); got != test.historyRTSPVersion {
				t.Fatalf("history control version = %s", got)
			}
			if got := buildSubscriptionEventValueForVersion(test.version, "Catalog", "1894"); got != test.catalogEvent {
				t.Fatalf("Catalog Event = %s", got)
			}
			if got := buildSubscriptionEventValueForVersion(test.version, "Alarm", gb10DeviceID); got != "presence" {
				t.Fatalf("Alarm Event = %s", got)
			}
			if got := shouldSendCascadeInitialCatalogNotify(worker, "Catalog"); got != test.initialCatalog {
				t.Fatalf("initial Catalog NOTIFY = %v, want %v", got, test.initialCatalog)
			}
			if shouldSendCascadeInitialCatalogNotify(worker, "Alarm") {
				t.Fatal("Alarm subscription must not send initial Catalog NOTIFY")
			}
			defaultExpires, err := parseSubscribeExpiresForProfile("", "Catalog", worker)
			if err != nil {
				t.Fatal(err)
			}
			wantExpires := defaultSubscribeExpires
			if test.initialCatalog {
				wantExpires = defaultCascadeCatalogSubscribeExpires
			}
			if defaultExpires != wantExpires {
				t.Fatalf("Catalog default Expires = %d, want %d", defaultExpires, wantExpires)
			}
			if outgoingExpires := defaultOutgoingSubscribeExpires(test.version, "Catalog"); outgoingExpires != wantExpires {
				t.Fatalf("outgoing Catalog default Expires = %d, want %d", outgoingExpires, wantExpires)
			}
			if outgoingExpires := defaultOutgoingSubscribeExpires(test.version, "Alarm"); outgoingExpires != defaultSubscribeExpires {
				t.Fatalf("outgoing Alarm default Expires = %d, want %d", outgoingExpires, defaultSubscribeExpires)
			}
			payload, mapping, voiceAllowed := cascadeBroadcastProfile(test.version)
			if voiceAllowed != test.voiceBroadcast || payload != test.voicePayload || mapping != test.voiceMapping {
				t.Fatalf("Broadcast profile = %d, %q, %v", payload, mapping, voiceAllowed)
			}
			voiceSDP, voiceErr := buildCascadeVoiceReceiveSDP(gb10DeviceID, &sms.MediaServer{SDPIP: "192.0.2.20"}, test.version, 30000, "0100000001")
			if (voiceErr == nil) != test.voiceBroadcast {
				t.Fatalf("Broadcast SDP support = %v, err %v", voiceErr == nil, voiceErr)
			}
			if voiceErr == nil && (!strings.Contains(string(voiceSDP), "m=audio 30000 RTP/AVP "+strconv.Itoa(test.voicePayload)) || !strings.Contains(string(voiceSDP), "a=rtpmap:"+strconv.Itoa(test.voicePayload)+" "+test.voiceMapping)) {
				t.Fatalf("Broadcast SDP profile = %s", voiceSDP)
			}
			if test.voiceBroadcast {
				if _, err := buildCascadeVoiceReceiveSDP(gb10DeviceID, &sms.MediaServer{SDPIP: "192.0.2.20"}, test.version, 30000, "1100000001"); err == nil || !strings.Contains(err.Error(), "realtime SSRC") {
					t.Fatalf("Broadcast history SSRC error = %v, want realtime SSRC rejection", err)
				}
			}

			stream := &Streams{S: time.Unix(start, 0), E: time.Unix(end, 0)}
			pause, err := buildHistoryControlCmdForVersion(stream, &ControlHistoryInput{Action: "pause"}, test.version)
			if err != nil || !strings.Contains(pause, test.historyRTSPVersion) {
				t.Fatalf("Pause control = %q, err %v", pause, err)
			}
			if test.version.AtLeast(GBVersion11) != strings.Contains(pause, "PauseTime: now") {
				t.Fatalf("PauseTime profile = %q", pause)
			}
		})
	}
}

func TestCascadeVersionNegotiationNeverUpgradesConfiguredProfile(t *testing.T) {
	request := sip.NewRequest("", sip.MethodRegister, &sip.URI{}, sip.DefaultSipVersion, nil, nil)
	response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	remote := sip.XGBVer(GBVersion30)
	response.AppendHeader(&remote)
	if got, err := negotiateCascadeVersion(GBVersion11, response); err != nil || got != GBVersion11 {
		t.Fatalf("configured 1.1 was upgraded to %s, %v", got, err)
	}
}

func TestCascadeVersionNegotiationValidatesResponseHeader(t *testing.T) {
	request := sip.NewRequest("", sip.MethodRegister, &sip.URI{}, sip.DefaultSipVersion, nil, nil)
	tests := []struct {
		name    string
		headers []string
		want    GBProtocolVersion
		wantErr string
	}{
		{name: "missing legacy header", want: GBVersion30},
		{name: "known downgrade", headers: []string{"1.1"}, want: GBVersion11},
		{name: "unknown extension is conservative", headers: []string{"4.0"}, want: GBVersion10},
		{name: "malformed", headers: []string{"2011"}, wantErr: "invalid X-GB-Ver"},
		{name: "empty", headers: []string{""}, wantErr: "invalid X-GB-Ver"},
		{name: "duplicate", headers: []string{"2.0", "3.0"}, wantErr: "multiple X-GB-Ver"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
			for _, value := range test.headers {
				response.AppendHeader(&sip.GenericHeader{HeaderName: "X-GB-Ver", Contents: value})
			}
			got, err := negotiateCascadeVersion(GBVersion30, response)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("negotiation error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("negotiated version = %s, %v; want %s", got, err, test.want)
			}
		})
	}
}
