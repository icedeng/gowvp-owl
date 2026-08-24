package gbs

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildHistoryControlCmdUsesVersionProfile(t *testing.T) {
	start := time.Unix(1711929600, 0)
	stream := &Streams{S: start, E: start.Add(time.Hour)}
	tests := []struct {
		name     string
		version  GBProtocolVersion
		input    ControlHistoryInput
		expected []string
		rejected []string
	}{
		{
			name: "2011 pause", version: GBVersion10, input: ControlHistoryInput{Action: "pause"},
			expected: []string{"PAUSE MANSRTSP/1.0", "CSeq: 1"}, rejected: []string{"PauseTime"},
		},
		{
			name: "2014 pause", version: GBVersion11, input: ControlHistoryInput{Action: "pause"},
			expected: []string{"PAUSE RTSP/1.0", "CSeq: 2", "PauseTime: now"},
		},
		{
			name: "2016 resume", version: GBVersion20, input: ControlHistoryInput{Action: "resume"},
			expected: []string{"PLAY RTSP/1.0", "CSeq: 3", "Range: npt=now-"}, rejected: []string{"Scale:"},
		},
		{
			name: "2022 seek", version: GBVersion30, input: ControlHistoryInput{Action: "seek", SeekAt: start.Add(125 * time.Second).Unix()},
			expected: []string{"PLAY RTSP/1.0", "CSeq: 4", "Range: npt=125-"}, rejected: []string{"clock="},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := buildHistoryControlCmdForVersion(stream, &test.input, test.version)
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range test.expected {
				if !strings.Contains(body, expected) {
					t.Fatalf("history control missing %q: %s", expected, body)
				}
			}
			for _, rejected := range test.rejected {
				if strings.Contains(body, rejected) {
					t.Fatalf("history control unexpectedly contains %q: %s", rejected, body)
				}
			}
		})
	}
}

func TestDownloadSpeedRespectsDeviceCapabilityOverride(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion11)
	if err := api.requireHistoryDownloadSpeed(gb10DeviceID, 4); err != nil {
		t.Fatalf("2014 download speed rejected: %v", err)
	}
	memory.device.setGBProfile(GBVersion11, []string{"download_speed"})
	if err := api.requireHistoryDownloadSpeed(gb10DeviceID, 4); err == nil {
		t.Fatal("disabled download_speed capability was ignored")
	}
	if err := api.requireHistoryDownloadSpeed(gb10DeviceID, 0); err != nil {
		t.Fatalf("default download speed rejected: %v", err)
	}
}

func TestBuildHistoryControlCmdRejectsSeekOutsideSession(t *testing.T) {
	start := time.Unix(1711929600, 0)
	stream := &Streams{S: start, E: start.Add(time.Hour)}
	_, err := buildHistoryControlCmdForVersion(stream, &ControlHistoryInput{Action: "seek", SeekAt: start.Add(-time.Second).Unix()}, GBVersion20)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("seek outside session error = %v", err)
	}
}

func TestBuildHistoryControlCmdAllocatesUniqueConcurrentCSeq(t *testing.T) {
	const controls = 128
	stream := &Streams{}
	results := make(chan uint32, controls)
	errors := make(chan error, controls)
	var wait sync.WaitGroup
	for range controls {
		wait.Add(1)
		go func() {
			defer wait.Done()
			body, err := buildHistoryControlCmdForVersion(stream, &ControlHistoryInput{Action: "pause"}, GBVersion20)
			if err != nil {
				errors <- err
				return
			}
			parsed, err := parseCascadeMANSRTSP([]byte(body))
			if err != nil {
				errors <- err
				return
			}
			results <- parsed.cseq
		}()
	}
	wait.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	seen := make(map[uint32]struct{}, controls)
	for cseq := range results {
		if _, duplicate := seen[cseq]; duplicate {
			t.Fatalf("duplicate concurrent CSeq: %d", cseq)
		}
		seen[cseq] = struct{}{}
	}
	if len(seen) != controls {
		t.Fatalf("allocated CSeq count = %d, want %d", len(seen), controls)
	}
}
