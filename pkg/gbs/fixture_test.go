package gbs

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestGBVersionXMLAndSDPFixtures(t *testing.T) {
	versions := []string{"1.0", "1.1", "2.0", "3.0"}
	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			dir := filepath.Join("testdata", "gb28181", version)

			keepaliveBody, err := os.ReadFile(filepath.Join(dir, "keepalive.xml"))
			if err != nil {
				t.Fatal(err)
			}
			var keepalive MessageNotify
			if err := sip.XMLDecode(keepaliveBody, &keepalive); err != nil {
				t.Fatalf("decode keepalive: %v", err)
			}
			if keepalive.CmdType != "Keepalive" || keepalive.DeviceID == "" {
				t.Fatalf("invalid keepalive fixture: %+v", keepalive)
			}

			catalogBody, err := os.ReadFile(filepath.Join(dir, "catalog-response.xml"))
			if err != nil {
				t.Fatal(err)
			}
			var catalog MessageDeviceListResponse
			if err := sip.XMLDecode(catalogBody, &catalog); err != nil {
				t.Fatalf("decode catalog: %v", err)
			}
			if catalog.SumNum != 1 || len(catalog.Item) != 1 {
				t.Fatalf("invalid catalog fixture: sum=%d items=%d", catalog.SumNum, len(catalog.Item))
			}

			sdpBody, err := os.ReadFile(filepath.Join(dir, "invite.sdp"))
			if err != nil {
				t.Fatal(err)
			}
			assertSDPFixtureFraming(t, sdpBody)
			sdpText := string(sdpBody)
			for _, required := range []string{"v=0", "s=Play", "a=rtpmap:96 PS/90000", "y=0100000001"} {
				if !strings.Contains(sdpText, required) {
					t.Fatalf("SDP fixture missing %q", required)
				}
			}
			wantTCP := version == "2.0" || version == "3.0"
			if gotTCP := strings.Contains(sdpText, "TCP/RTP/AVP"); gotTCP != wantTCP {
				t.Fatalf("TCP SDP = %v; want %v", gotTCP, wantTCP)
			}

			sdpFiles, err := filepath.Glob(filepath.Join(dir, "*.sdp"))
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range sdpFiles {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				assertSDPFixtureFraming(t, data)
			}
		})
	}
}

func TestKeepalive11FaultDeviceList(t *testing.T) {
	body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>8</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>ERROR</Status><Info><DeviceID>34020000001320000002</DeviceID><DeviceID>34020000001320000003</DeviceID></Info></Notify>`)
	var keepalive MessageNotify
	if err := sip.XMLDecode(body, &keepalive); err != nil {
		t.Fatal(err)
	}
	got := normalizeGBIDList(append(keepalive.Info.DeviceIDs, keepalive.Info.DeviceIDs[0]))
	if len(got) != 2 || got[0] != "34020000001320000002" || got[1] != "34020000001320000003" {
		t.Fatalf("fault device IDs = %v", got)
	}
}

func assertSDPFixtureFraming(t *testing.T, data []byte) {
	t.Helper()
	if len(data) == 0 {
		t.Fatal("empty SDP fixture")
	}
	for i, b := range data {
		if b == '\n' && (i == 0 || data[i-1] != '\r') {
			t.Fatalf("SDP fixture contains bare LF at byte %d", i)
		}
	}
	if !bytes.HasSuffix(data, []byte("\r\n")) {
		t.Fatal("SDP fixture must end with CRLF")
	}
}
