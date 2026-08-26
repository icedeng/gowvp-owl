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
		ChannelID: testCascadeChannelID, Name: "Front Gate", PTZType: 3, IsOnline: true,
		Ext: ipc.DeviceExt{GBCatalog: &ipc.GBCatalogExt{Resolution: "1920x1080"}},
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
		{"2014", GBVersion11, true, false, true, true, false, true, true, 96, "PS/90000", "RTSP/1.0", "Catalog;id=" + gb10DeviceID},
		{"2016", GBVersion20, true, true, false, true, true, true, true, 8, "PCMA/8000", "RTSP/1.0", "Catalog;id=" + gb10DeviceID},
		{"2022", GBVersion30, true, true, false, true, true, true, true, 8, "PCMA/8000", "RTSP/1.0", "Catalog;id=" + gb10DeviceID},
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
			if got := negotiateCascadeVersion(test.version, response); got != test.version {
				t.Fatalf("negotiated version = %s", got)
			}

			items := buildCascadeCatalogItems([]*ipc.Channel{channel}, platform, test.version)
			if len(items) != 1 || (items[0].Info != nil) != test.catalogInfo {
				t.Fatalf("Catalog profile = %+v", items)
			}
			if items[0].DeviceID != testExposedChannelID || items[0].ParentID != gb10DeviceID {
				t.Fatalf("Catalog mapping = %+v", items[0])
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
			if got := buildSubscriptionEventValueForVersion(test.version, "Catalog", gb10DeviceID); got != test.catalogEvent {
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
	if got := negotiateCascadeVersion(GBVersion11, response); got != GBVersion11 {
		t.Fatalf("configured 1.1 was upgraded to %s", got)
	}
}
