package gbs

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestBuildDirectTCPDownloadSDP11(t *testing.T) {
	body, err := buildGBSDP(gbSDPInput{
		Version:     GBVersion11,
		SessionName: historyModeDownload,
		ChannelID:   gb10ChannelID,
		URI:         gb10ChannelID + ":3",
		IP:          "192.0.2.20",
		Port:        9,
		StartAt:     time.Unix(1711929600, 0),
		EndAt:       time.Unix(1711933200, 0),
		SSRC:        "1100000001",
		DirectTCP:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		"s=Download\r\n",
		"u=" + gb10ChannelID + ":3\r\n",
		"m=video 9 tcp 96 97 98\r\n",
		"a=recvonly\r\n",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("direct TCP SDP missing %q:\n%s", required, text)
		}
	}
	if strings.Contains(text, "TCP/RTP/AVP") || strings.Contains(text, "a=setup:") {
		t.Fatalf("direct TCP SDP was mixed with RTP over TCP:\n%s", text)
	}
}

func TestBuildDirectTCPDownloadSDPVersionIsolation(t *testing.T) {
	base := gbSDPInput{
		SessionName: historyModeDownload,
		ChannelID:   gb10ChannelID,
		URI:         gb10ChannelID + ":3",
		IP:          "192.0.2.20",
		Port:        9,
		StartAt:     time.Unix(1711929600, 0),
		EndAt:       time.Unix(1711933200, 0),
		SSRC:        "1100000001",
		DirectTCP:   true,
	}
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion20, GBVersion30} {
		base.Version = version
		if _, err := buildGBSDP(base); err == nil || !strings.Contains(err.Error(), "直接 TCP") {
			t.Fatalf("version %s direct TCP error = %v", version, err)
		}
	}
	base.Version = GBVersion11
	base.StreamMode = 1
	if _, err := buildGBSDP(base); err == nil || !strings.Contains(err.Error(), "must not reuse RTP") {
		t.Fatalf("direct TCP StreamMode error = %v", err)
	}
}

func TestParseDirectTCPDownloadSDP(t *testing.T) {
	body := []byte("v=0\r\n" +
		"o=34020000001320000001 0 0 IN IP4 192.0.2.10\r\n" +
		"s=Embedded Net DVR\r\n" +
		"c=IN IP4 192.0.2.10\r\n" +
		"t=0 0\r\n" +
		"m=video 9412 tcp 96\r\n" +
		"a=sendonly\r\n" +
		"a=rtpmap:96 PS/90000\r\n" +
		"a=filesize:12345\r\n" +
		"y=1100000000\r\n")
	offer, err := parseDirectTCPDownloadSDP(body)
	if err != nil {
		t.Fatal(err)
	}
	if offer.Address != "192.0.2.10:9412" || !offer.FileSizeKnown || offer.FileSize != 12345 || offer.SSRC != "1100000000" {
		t.Fatalf("parsed offer = %+v", offer)
	}
	duplicated := append(append([]byte(nil), body...), "y=1100000001\r\n"...)
	if _, err := parseDirectTCPDownloadSDP(duplicated); err == nil || !strings.Contains(err.Error(), "multiple y fields") {
		t.Fatalf("duplicate SSRC error = %v", err)
	}
	duplicated = append(append([]byte(nil), body...), "a=fileszie:12346\r\n"...)
	if _, err := parseDirectTCPDownloadSDP(duplicated); err == nil || !strings.Contains(err.Error(), "multiple filesize") {
		t.Fatalf("duplicate filesize error = %v", err)
	}
	duplicated = append(append([]byte(nil), body...), "m=video 9413 tcp 96\r\na=sendonly\r\n"...)
	if _, err := parseDirectTCPDownloadSDP(duplicated); err == nil || !strings.Contains(err.Error(), "exactly one video") {
		t.Fatalf("duplicate video media error = %v", err)
	}
	conflictingMapping := append(append([]byte(nil), body...), "a=rtpmap:96 H264/90000\r\n"...)
	if _, err := parseDirectTCPDownloadSDP(conflictingMapping); err == nil || !strings.Contains(err.Error(), "conflicting SDP rtpmap") {
		t.Fatalf("conflicting payload mapping error = %v", err)
	}
	wrongMapping := []byte(strings.Replace(string(body), "a=rtpmap:96 PS/90000", "a=rtpmap:96 H264/90000", 1))
	if _, err := parseDirectTCPDownloadSDP(wrongMapping); err == nil || !strings.Contains(err.Error(), "must use PS/90000") {
		t.Fatalf("wrong payload mapping error = %v", err)
	}
	wrongPayload := []byte(strings.Replace(string(body), "m=video 9412 tcp 96", "m=video 9412 tcp 98", 1))
	if _, err := parseDirectTCPDownloadSDP(wrongPayload); err == nil || !strings.Contains(err.Error(), "does not offer PS payload 96") {
		t.Fatalf("wrong payload error = %v", err)
	}
}

func TestDirectTCPDownloadSDPIPv6(t *testing.T) {
	body, err := buildGBSDP(gbSDPInput{
		Version: GBVersion11, SessionName: historyModeDownload,
		ChannelID: gb10ChannelID, URI: gb10ChannelID + ":3",
		IP: "2001:db8::20", Port: 9,
		StartAt: time.Unix(1711929600, 0), EndAt: time.Unix(1711933200, 0),
		SSRC: "1100000001", DirectTCP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if text := string(body); !strings.Contains(text, "o="+gb10ChannelID+" 0 0 IN IP6 2001:db8::20\r\n") || !strings.Contains(text, "c=IN IP6 2001:DB8::20\r\n") {
		t.Fatalf("IPv6 direct TCP request SDP mismatch:\n%s", text)
	}

	response := []byte("v=0\r\no=x 0 0 IN IP6 2001:db8::30\r\ns=x\r\nc=IN IP6 2001:db8::30\r\nt=0 0\r\nm=video 9000 tcp 96\r\na=filesize:7\r\n")
	offer, err := parseDirectTCPDownloadSDP(response)
	if err != nil {
		t.Fatal(err)
	}
	if offer.Address != "[2001:db8::30]:9000" || !offer.IP.Equal(net.ParseIP("2001:db8::30")) {
		t.Fatalf("IPv6 direct TCP offer = %+v", offer)
	}
}

func TestParseDirectTCPDownloadSDPCompatibilityAndValidation(t *testing.T) {
	typo := []byte("v=0\r\no=x 0 0 IN IP4 192.0.2.10\r\ns=x\r\nc=IN IP4 192.0.2.10\r\nt=0 0\r\nm=video 9000 tcp 96\r\na=fileszie:7\r\n")
	offer, err := parseDirectTCPDownloadSDP(typo)
	if err != nil || !offer.FileSizeKnown || offer.FileSize != 7 {
		t.Fatalf("fileszie compatibility offer = %+v, err = %v", offer, err)
	}
	unknown := []byte("v=0\r\no=x 0 0 IN IP4 192.0.2.10\r\ns=x\r\nc=IN IP4 192.0.2.10\r\nt=0 0\r\nm=video 9000 tcp 96\r\n")
	offer, err = parseDirectTCPDownloadSDP(unknown)
	if err != nil || offer.FileSizeKnown {
		t.Fatalf("unknown-size offer = %+v, err = %v", offer, err)
	}
	rtp := []byte("v=0\r\no=x 0 0 IN IP4 192.0.2.10\r\ns=x\r\nc=IN IP4 192.0.2.10\r\nt=0 0\r\nm=video 9000 TCP/RTP/AVP 96\r\n")
	if _, err = parseDirectTCPDownloadSDP(rtp); err == nil {
		t.Fatal("RTP over TCP SDP must not be accepted as direct TCP")
	}
	for _, direction := range []string{"recvonly", "inactive"} {
		t.Run(direction, func(t *testing.T) {
			body := []byte("v=0\r\no=x 0 0 IN IP4 192.0.2.10\r\ns=x\r\nc=IN IP4 192.0.2.10\r\nt=0 0\r\nm=video 9000 tcp 96\r\na=" + direction + "\r\n")
			if _, err := parseDirectTCPDownloadSDP(body); err == nil || !strings.Contains(err.Error(), "does not send media") {
				t.Fatalf("%s direction error = %v", direction, err)
			}
		})
	}
	sendrecv := []byte("v=0\r\no=x 0 0 IN IP4 192.0.2.10\r\ns=x\r\nc=IN IP4 192.0.2.10\r\nt=0 0\r\nm=video 9000 tcp 96\r\na=sendrecv\r\n")
	if _, err = parseDirectTCPDownloadSDP(sendrecv); err != nil {
		t.Fatalf("sendrecv compatibility error = %v", err)
	}
	sessionRecvonly := []byte(strings.Replace(string(unknown), "t=0 0\r\n", "t=0 0\r\na=recvonly\r\n", 1))
	if _, err = parseDirectTCPDownloadSDP(sessionRecvonly); err == nil || !strings.Contains(err.Error(), "does not send media") {
		t.Fatalf("session recvonly direction error = %v", err)
	}
	mediaOverride := []byte(strings.Replace(string(sendrecv), "t=0 0\r\n", "t=0 0\r\na=inactive\r\n", 1))
	if _, err = parseDirectTCPDownloadSDP(mediaOverride); err != nil {
		t.Fatalf("media direction override error = %v", err)
	}
	conflicting := append(append([]byte(nil), sendrecv...), "a=recvonly\r\n"...)
	if _, err = parseDirectTCPDownloadSDP(conflicting); err == nil || !strings.Contains(err.Error(), "multiple direction") {
		t.Fatalf("conflicting directions error = %v", err)
	}
}
