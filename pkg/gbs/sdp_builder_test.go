package gbs

import (
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
				URI:         "34020000001320000002:0",
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
				URI:              "34020000001320000002:0",
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

func TestBuildGBSDPValidation(t *testing.T) {
	base := gbSDPInput{
		Version:     GBVersion10,
		SessionName: historyModePlayback,
		ChannelID:   "34020000001320000002",
		URI:         "34020000001320000002:0",
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

func TestBuildGBInviteSubject(t *testing.T) {
	got := buildGBInviteSubject("34020000001320000002", "0100000001", "34020000002000000001")
	want := "34020000001320000002:0100000001,34020000002000000001:0"
	if got != want {
		t.Fatalf("buildGBInviteSubject() = %q, want %q", got, want)
	}
}

func normalizeSDPLineEndings(value string) string {
	return strings.ReplaceAll(value, "\r\n", "\n")
}
