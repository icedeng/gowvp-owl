package gbs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildGBSDP10Golden(t *testing.T) {
	tests := []struct {
		name   string
		input  gbSDPInput
		golden string
	}{
		{
			name: "play",
			input: gbSDPInput{
				Version:     GBVersion10,
				SessionName: historyModePlay,
				ChannelID:   "34020000001320000002",
				IP:          "192.0.2.20",
				Port:        30000,
				SSRC:        "0100000001",
			},
			golden: "play-request.sdp",
		},
		{
			name: "playback",
			input: gbSDPInput{
				Version:     GBVersion10,
				SessionName: historyModePlayback,
				ChannelID:   "34020000001320000002",
				URI:         "34020000001320000002:3",
				IP:          "192.0.2.20",
				Port:        30000,
				StartAt:     time.Unix(1711929600, 0),
				EndAt:       time.Unix(1711933200, 0),
				SSRC:        "1100000001",
			},
			golden: "playback-request.sdp",
		},
		{
			name: "download",
			input: gbSDPInput{
				Version:          GBVersion10,
				SessionName:      historyModeDownload,
				ChannelID:        "34020000001320000002",
				URI:              "34020000001320000002:3",
				IP:               "192.0.2.20",
				Port:             30000,
				StartAt:          time.Unix(1711929600, 0),
				EndAt:            time.Unix(1711933200, 0),
				SSRC:             "1100000001",
				MediaDescription: "v/2/6/25/1/4096a/1/8/1",
			},
			golden: "download-request.sdp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildGBSDP(tt.input)
			if err != nil {
				t.Fatalf("buildGBSDP() error = %v", err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.0", tt.golden))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if normalizeSDPLineEndings(string(got)) != normalizeSDPLineEndings(string(want)) {
				t.Fatalf("SDP mismatch\ngot:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestBuildGBSDPVersionTransport(t *testing.T) {
	base := gbSDPInput{
		Version:     GBVersion10,
		SessionName: historyModePlay,
		ChannelID:   "34020000001320000002",
		IP:          "192.0.2.20",
		Port:        30000,
		SSRC:        "0100000001",
	}

	base.StreamMode = 1
	if _, err := buildGBSDP(base); err == nil || !strings.Contains(err.Error(), "RTP over TCP") {
		t.Fatalf("1.0 TCP build error = %v, want RTP over TCP rejection", err)
	}

	base.Version = GBVersion20
	got, err := buildGBSDP(base)
	if err != nil {
		t.Fatalf("2.0 TCP build error = %v", err)
	}
	if !strings.Contains(string(got), "m=video 30000 TCP/RTP/AVP 96 97 98\r\n") ||
		!strings.Contains(string(got), "a=setup:passive\r\n") ||
		!strings.Contains(string(got), "a=connection:new\r\n") {
		t.Fatalf("2.0 TCP SDP missing transport attributes:\n%s", got)
	}
}

func TestBuildGBSDPStandardTalkUpstreamUsesAudioMedia(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion20, GBVersion30} {
		body, err := buildGBSDP(gbSDPInput{
			Version: version, SessionName: historyModePlay,
			ChannelID: gb10ChannelID, IP: "192.0.2.20", Port: 30000,
			SSRC: "0100000001", AudioOnly: true,
		})
		if err != nil {
			t.Fatalf("%s audio Play SDP error = %v", version, err)
		}
		text := string(body)
		for _, want := range []string{
			"s=Play\r\n",
			"m=audio 30000 RTP/AVP 8\r\n",
			"a=recvonly\r\n",
			"a=rtpmap:8 PCMA/8000\r\n",
			"f=v/////a/1/8/1\r\n",
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s audio Play SDP missing %q:\n%s", version, want, body)
			}
		}
		if strings.Contains(text, "m=video") {
			t.Fatalf("%s audio Play SDP contains video media:\n%s", version, body)
		}
	}

	if _, err := buildGBSDP(gbSDPInput{
		Version: GBVersion11, SessionName: historyModePlay,
		ChannelID: gb10ChannelID, IP: "192.0.2.20", Port: 30000,
		SSRC: "0100000001", AudioOnly: true,
	}); err == nil || !strings.Contains(err.Error(), "语音对讲") {
		t.Fatalf("1.1 audio Play SDP error = %v, want voice intercom rejection", err)
	}
}

func TestBuildGBSDP11DownloadSpeed(t *testing.T) {
	in := gbSDPInput{
		Version: GBVersion11, SessionName: historyModeDownload,
		ChannelID: gb10ChannelID, URI: gb10ChannelID + ":3",
		IP: "192.0.2.20", Port: 30000,
		StartAt: time.Unix(1711929600, 0), EndAt: time.Unix(1711933200, 0),
		SSRC: "1100000001", DownloadSpeed: 4,
	}
	body, err := buildGBSDP(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "a=downloadspeed:4\r\n") {
		t.Fatalf("Download SDP missing speed attribute:\n%s", body)
	}
	in.Version = GBVersion10
	if _, err := buildGBSDP(in); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("1.0 Download speed error = %v", err)
	}
	in.Version = GBVersion11
	in.SessionName = historyModePlay
	in.URI = ""
	in.StartAt = time.Time{}
	in.EndAt = time.Time{}
	in.SSRC = "0100000001"
	if _, err := buildGBSDP(in); err == nil || !strings.Contains(err.Error(), "only valid for Download") {
		t.Fatalf("Play download speed validation error = %v", err)
	}
}

func TestBuildGBSDPValidation(t *testing.T) {
	base := gbSDPInput{
		Version:     GBVersion10,
		SessionName: historyModePlayback,
		ChannelID:   "34020000001320000002",
		URI:         "34020000001320000002:3",
		IP:          "192.0.2.20",
		Port:        30000,
		StartAt:     time.Unix(1711929600, 0),
		EndAt:       time.Unix(1711933200, 0),
		SSRC:        "1100000001",
	}

	tests := []struct {
		name   string
		mutate func(*gbSDPInput)
	}{
		{"missing uri", func(in *gbSDPInput) { in.URI = "" }},
		{"invalid range", func(in *gbSDPInput) { in.EndAt = in.StartAt }},
		{"invalid ip", func(in *gbSDPInput) { in.IP = "not-an-ip" }},
		{"invalid f", func(in *gbSDPInput) { in.MediaDescription = "v/2" }},
		{"f missing audio segment", func(in *gbSDPInput) { in.MediaDescription = "v/2/6/25/1/4096" }},
		{"f missing video segment", func(in *gbSDPInput) { in.MediaDescription = "a/1/8/1" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base
			tt.mutate(&in)
			if _, err := buildGBSDP(in); err == nil {
				t.Fatal("buildGBSDP() error = nil, want validation error")
			}
		})
	}
}

func TestBuildGBSDPValidatesSSRCStreamType(t *testing.T) {
	base := gbSDPInput{
		Version:     GBVersion10,
		SessionName: historyModePlayback,
		ChannelID:   "34020000001320000002",
		URI:         "34020000001320000002:3",
		IP:          "192.0.2.20",
		Port:        30000,
		StartAt:     time.Unix(1711929600, 0),
		EndAt:       time.Unix(1711933200, 0),
		SSRC:        "1100000001",
	}
	tests := []struct {
		name        string
		sessionName string
		ssrc        string
		wantError   string
	}{
		{"play with history SSRC", historyModePlay, "1100000001", "realtime SSRC"},
		{"playback with realtime SSRC", historyModePlayback, "0100000001", "history SSRC"},
		{"download with realtime SSRC", historyModeDownload, "0100000001", "history SSRC"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := base
			in.SessionName = test.sessionName
			in.SSRC = test.ssrc
			if _, err := buildGBSDP(in); err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("buildGBSDP() error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestHistoryURIRecordType(t *testing.T) {
	defaultURI, err := historyURI(testCascadeChannelID, nil)
	if err != nil || defaultURI != testCascadeChannelID+":3" {
		t.Fatalf("default history URI = %q, err = %v", defaultURI, err)
	}
	for recordType := 0; recordType <= 3; recordType++ {
		value := recordType
		uri, buildErr := historyURI(testCascadeChannelID, &value)
		if buildErr != nil || uri != fmt.Sprintf("%s:%d", testCascadeChannelID, recordType) {
			t.Fatalf("record type %d URI = %q, err = %v", recordType, uri, buildErr)
		}
	}
	for _, recordType := range []int{-1, 4} {
		value := recordType
		if _, buildErr := historyURI(testCascadeChannelID, &value); buildErr == nil {
			t.Fatalf("invalid record type %d accepted", recordType)
		}
	}
}

func TestBuildGBSDPValidatesMediaDescriptionByVersion(t *testing.T) {
	base := gbSDPInput{
		Version: GBVersion10, SessionName: historyModePlay,
		ChannelID: gb10DeviceID, IP: "192.0.2.20", Port: 30000, SSRC: "0100000001",
	}

	base.MediaDescription = "v/5/6/25/1/4096a///"
	if _, err := buildGBSDP(base); err == nil || !strings.Contains(err.Error(), "video codec") {
		t.Fatalf("2011 H.265 media description error = %v", err)
	}
	base.Version = GBVersion30
	if _, err := buildGBSDP(base); err != nil {
		t.Fatalf("2022 H.265 media description rejected: %v", err)
	}

	base.Version = GBVersion20
	base.MediaDescription = "v/////a/6/4/1"
	if _, err := buildGBSDP(base); err == nil || !strings.Contains(err.Error(), "audio codec") {
		t.Fatalf("2016 AAC media description error = %v", err)
	}
	base.Version = GBVersion30
	if _, err := buildGBSDP(base); err != nil {
		t.Fatalf("2022 AAC media description rejected: %v", err)
	}
	for _, resolution := range []string{"1920x1080", "1920X1080", "1920×1080"} {
		base.MediaDescription = "v/5/" + resolution + "/25/1/4096a///"
		if _, err := buildGBSDP(base); err != nil {
			t.Fatalf("2022 custom resolution %q rejected: %v", resolution, err)
		}
	}
	base.Version = GBVersion20
	base.MediaDescription = "v/2/1920x1080/25/1/4096a///"
	if _, err := buildGBSDP(base); err == nil || !strings.Contains(err.Error(), "video resolution") {
		t.Fatalf("2016 custom resolution error = %v", err)
	}
	base.Version = GBVersion30

	for _, value := range []string{
		"v/2/7/25/1/4096a///",
		"v/2/1920x0/25/1/4096a///",
		"v/2/1920xx1080/25/1/4096a///",
		"v/2/6/100/1/4096a///",
		"v/2/6/25/3/4096a///",
		"v/2/6/25/1/100001a///",
		"v/////a/1/7/1",
	} {
		base.MediaDescription = value
		if _, err := buildGBSDP(base); err == nil {
			t.Fatalf("invalid media description %q was accepted", value)
		}
	}
}

func TestBuildGBSDPCanDisableH265ForDeviceFirmware(t *testing.T) {
	in := gbSDPInput{
		Version: GBVersion30, SessionName: historyModePlay,
		ChannelID: gb10DeviceID, IP: "192.0.2.20", Port: 30000, SSRC: "0100000001",
		H265Disabled: true,
	}
	body, err := buildGBSDP(in)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "H265/90000") || strings.Contains(text, " 100") {
		t.Fatalf("disabled H.265 remained in SDP:\n%s", text)
	}
}

func TestBuildGBSDP30UsesStandardH265Payload(t *testing.T) {
	in := gbSDPInput{
		Version: GBVersion30, SessionName: historyModePlay,
		ChannelID: gb10DeviceID, IP: "192.0.2.20", Port: 30000, SSRC: "0100000001",
	}
	body, err := buildGBSDP(in)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "m=video 30000 RTP/AVP 96 97 98 100\r\n") ||
		!strings.Contains(text, "a=rtpmap:100 H265/90000\r\n") {
		t.Fatalf("3.0 SDP missing H.265 payload 100:\n%s", text)
	}
	if strings.Contains(text, "a=rtpmap:99 H265/90000\r\n") {
		t.Fatalf("3.0 SDP reused the SVAC payload for H.265:\n%s", text)
	}
}

func TestBuildGBSDPDoesNotLockStandardAudioCodecs(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		in := gbSDPInput{
			Version: version, SessionName: historyModePlay,
			ChannelID: gb10DeviceID, IP: "192.0.2.20", Port: 30000, SSRC: "0100000001",
		}
		body, err := buildGBSDP(in)
		if err != nil {
			t.Fatalf("%s: %v", version, err)
		}
		if !strings.Contains(string(body), "f=v/////a///\r\n") {
			t.Fatalf("%s SDP locked a standard audio codec:\n%s", version, body)
		}
	}

	in := gbSDPInput{
		Version: GBVersion30, SessionName: historyModePlay,
		ChannelID: gb10DeviceID, IP: "192.0.2.20", Port: 30000, SSRC: "0100000001", AACDisabled: true,
	}
	body, err := buildGBSDP(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "f=v/////a/1/8/1\r\n") {
		t.Fatalf("AAC-disabled SDP did not fall back to G.711:\n%s", body)
	}
}

func TestBuildGBInviteSubject(t *testing.T) {
	tests := []struct {
		name     string
		sequence string
	}{
		{name: "live", sequence: "0100000001"},
		{name: "history", sequence: "1100000002"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := buildGBInviteSubject("34020000001320000002", test.sequence, "34020000002000000001")
			want := "34020000001320000002:" + test.sequence + ",34020000002000000001:" + test.sequence
			if got != want {
				t.Fatalf("buildGBInviteSubject() = %q, want %q", got, want)
			}
		})
	}
}

func normalizeSDPLineEndings(value string) string {
	return strings.ReplaceAll(value, "\r\n", "\n")
}
