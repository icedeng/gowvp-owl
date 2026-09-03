package gbs

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
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
			name: "2011 seek", version: GBVersion10, input: ControlHistoryInput{Action: "seek", SeekAt: start.Add(125 * time.Second).Unix()},
			expected: []string{"PLAY MANSRTSP/1.0", "CSeq: 4", "Range: smpte=00:02:05-"}, rejected: []string{"Range: npt="},
		},
		{
			name: "2022 seek", version: GBVersion30, input: ControlHistoryInput{Action: "seek", SeekAt: start.Add(125 * time.Second).Unix()},
			expected: []string{"PLAY RTSP/1.0", "CSeq: 5", "Range: npt=125-"}, rejected: []string{"clock="},
		},
		{
			name: "2022 teardown", version: GBVersion30, input: ControlHistoryInput{Action: "teardown"},
			expected: []string{"TEARDOWN RTSP/1.0", "CSeq: 6"},
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

func TestBuildHistoryControlCmdSupportsVersionSpecificPositionedSpeed(t *testing.T) {
	start := time.Unix(1711929600, 0)
	stream := &Streams{S: start, E: start.Add(time.Hour)}
	body, err := buildHistoryControlCmdForVersion(stream, &ControlHistoryInput{
		Action: "speed",
		Scale:  -2,
		SeekAt: start.Add(10 * time.Minute).Unix(),
	}, GBVersion30)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"PLAY RTSP/1.0", "Scale: -2.00", "Range: npt=600-"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("2022 reverse command missing %q: %s", expected, body)
		}
	}

	legacyBody, err := buildHistoryControlCmdForVersion(&Streams{S: start, E: start.Add(time.Hour)}, &ControlHistoryInput{
		Action: "speed",
		Scale:  2,
		SeekAt: start.Add(10 * time.Minute).Unix(),
	}, GBVersion10)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"PLAY MANSRTSP/1.0", "Scale: 2.00", "Range: npt=600-"} {
		if !strings.Contains(legacyBody, expected) {
			t.Fatalf("2011 positioned speed command missing %q: %s", expected, legacyBody)
		}
	}
	if strings.Contains(legacyBody, "Range: smpte=") {
		t.Fatalf("2011 speed command used random-seek SMPTE range: %s", legacyBody)
	}

	for _, test := range []struct {
		name    string
		version GBProtocolVersion
		scale   float64
	}{
		{name: "2014 speed from position", version: GBVersion11, scale: 2},
		{name: "2016 reverse from position", version: GBVersion20, scale: -2},
		{name: "2022 forward from position", version: GBVersion30, scale: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := buildHistoryControlCmdForVersion(&Streams{S: start, E: start.Add(time.Hour)}, &ControlHistoryInput{
				Action: "speed", Scale: test.scale, SeekAt: start.Add(time.Minute).Unix(),
			}, test.version)
			if err == nil || !strings.Contains(err.Error(), "only valid") {
				t.Fatalf("speed seek_at error = %v", err)
			}
		})
	}
}

func TestInvalidHistoryControlDoesNotConsumeCSeq(t *testing.T) {
	start := time.Unix(1711929600, 0)
	tests := []ControlHistoryInput{
		{Action: "unknown"},
		{Action: "speed", Scale: 0},
		{Action: "seek", SeekAt: start.Add(-time.Second).Unix()},
		{Action: "speed", Scale: -2, SeekAt: start.Add(2 * time.Hour).Unix()},
	}
	for index, input := range tests {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			stream := &Streams{S: start, E: start.Add(time.Hour), CseqNo: 40}
			if _, err := buildHistoryControlCmdForVersion(stream, &input, GBVersion30); err == nil {
				t.Fatal("invalid history control was accepted")
			}
			if stream.CseqNo != 40 {
				t.Fatalf("invalid history control consumed CSeq: got %d, want 40", stream.CseqNo)
			}
		})
	}
}

func TestHistoryControlLocalFailureDoesNotConsumeProtocolOrDialogCSeq(t *testing.T) {
	fixture := newSignalDigestDialogFixture(t, "history-control-local-failure")
	fixture.channel.device.UpdateRuntime(func(device *Device) {
		device.conn = nil
	})
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "history-control-local-failure",
		Resp: fixture.response, CseqNo: 40,
	}
	fixture.api.streams.Store(historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID), stream)

	err := fixture.api.ControlHistory(t.Context(), &ControlHistoryInput{
		Channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID},
		Mode:    historyModePlayback,
		Action:  "pause",
	})
	if err == nil || !strings.Contains(err.Error(), "connection") {
		t.Fatalf("history control local error = %v", err)
	}
	if stream.CseqNo != 40 {
		t.Fatalf("history control local failure consumed MANSRTSP CSeq: got %d, want 40", stream.CseqNo)
	}
	next, err := sip.NewRequestFromResponseChecked(sip.MethodInfo, fixture.response)
	if err != nil {
		t.Fatal(err)
	}
	cseq, _ := next.CSeq()
	if cseq == nil || cseq.SeqNo != 2 {
		t.Fatalf("history control local failure consumed SIP dialog CSeq: %+v", cseq)
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

func TestDownloadSpeedRespectsAdvertisedChannelOptions(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion11)
	if err := api.requireHistoryDownloadSpeed(gb10DeviceID, 4, "1/2/4"); err != nil {
		t.Fatalf("advertised download speed rejected: %v", err)
	}
	if err := api.requireHistoryDownloadSpeed(gb10DeviceID, 8, "1/2/4"); err == nil {
		t.Fatal("unsupported advertised download speed was accepted")
	}
	if err := api.requireHistoryDownloadSpeed(gb10DeviceID, 4, "1/bad/4"); err == nil {
		t.Fatal("invalid advertised download speed list was accepted")
	}
	if err := api.requireHistoryDownloadSpeed(gb10DeviceID, 8, ""); err != nil {
		t.Fatalf("missing advertised options changed compatibility behavior: %v", err)
	}
	if err := api.requireHistoryDownloadSpeed(gb10DeviceID, -1); err == nil {
		t.Fatal("negative download speed was accepted")
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

func TestBuildHistoryControlCmdNormalizesRawCommand(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	stream := &Streams{CseqNo: 9}
	input := &ControlHistoryInput{
		Channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID},
		Cmd:     "PLAY MANSRTSP/1.0\r\nCSeq: 99\r\nScale: -2.0\r\nRange: npt=100-\r\n\r\n",
	}
	body, err := api.buildHistoryControlCmd(stream, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"PLAY RTSP/1.0", "CSeq: 10", "Scale: -2.0", "Range: npt=100-"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("normalized raw command missing %q: %s", expected, body)
		}
	}
	if strings.Contains(body, "CSeq: 99") || strings.Contains(body, "MANSRTSP/1.0") {
		t.Fatalf("raw command bypassed normalization: %s", body)
	}

	for _, invalid := range []string{
		"PLAY RTSP/1.0\r\nScale: 2\r\n\r\n",
		"PLAY RTSP/1.0\r\nCSeq: 1\r\nUnknown: value\r\n\r\n",
		"PLAY RTSP/1.0\r\nCSeq: 1\r\nRange: clock=20260401T000000Z-\r\n\r\n",
	} {
		input.Cmd = invalid
		if _, err := api.buildHistoryControlCmd(stream, input); err == nil {
			t.Fatalf("invalid raw history command accepted: %q", invalid)
		}
	}
}

func TestValidMANSRTSPRange(t *testing.T) {
	for _, value := range []string{
		"npt=now-",
		"npt=0-",
		"npt=100.5-200",
		"npt=600-120",
		"smpte=00:01:02-",
		"smpte=00:10:00-00:02:00",
		"smpte=01:02:03:29-02:03:04:00",
		"smpte=01:02:03:15.25-",
	} {
		if !validMANSRTSPRange(value) {
			t.Errorf("valid Range rejected: %s", value)
		}
	}
	for _, value := range []string{
		"clock=20260401T000000Z-",
		"npt=-1-",
		"npt=garbage-",
		"npt=now-100",
		"smpte=00:99:00-",
		"smpte=00:00:00:30-",
		"smpte=00:00:01.5-",
		"smpte=000:00:00-",
		"smpte=00:00:00:00.001-",
	} {
		if validMANSRTSPRange(value) {
			t.Errorf("invalid Range accepted: %s", value)
		}
	}
	stream := &Streams{S: time.Unix(1711929600, 0), E: time.Unix(1711933200, 0)}
	for _, test := range []struct {
		name    string
		version GBProtocolVersion
		body    string
		valid   bool
	}{
		{name: "2011 scale range", version: GBVersion10, body: "PLAY MANSRTSP/1.0\r\nCSeq: 1\r\nScale: -2\r\nRange: npt=100-\r\n\r\n", valid: true},
		{name: "2011 descending reverse range", version: GBVersion10, body: "PLAY MANSRTSP/1.0\r\nCSeq: 1\r\nScale: -2\r\nRange: npt=600-120\r\n\r\n", valid: false},
		{name: "2014 positive scale range", version: GBVersion11, body: "PLAY RTSP/1.0\r\nCSeq: 1\r\nScale: 2\r\nRange: npt=100-\r\n\r\n", valid: false},
		{name: "2022 reverse scale range", version: GBVersion30, body: "PLAY RTSP/1.0\r\nCSeq: 1\r\nScale: -2\r\nRange: npt=100-\r\n\r\n", valid: true},
		{name: "2022 bounded reverse range", version: GBVersion30, body: "PLAY RTSP/1.0\r\nCSeq: 1\r\nScale: -2\r\nRange: npt=600-120\r\n\r\n", valid: true},
		{name: "2022 reverse range wrong direction", version: GBVersion30, body: "PLAY RTSP/1.0\r\nCSeq: 1\r\nScale: -2\r\nRange: npt=120-600\r\n\r\n", valid: false},
		{name: "forward descending range", version: GBVersion30, body: "PLAY RTSP/1.0\r\nCSeq: 1\r\nRange: npt=600-120\r\n\r\n", valid: false},
		{name: "range outside session", version: GBVersion30, body: "PLAY RTSP/1.0\r\nCSeq: 1\r\nRange: npt=3601-\r\n\r\n", valid: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := parseCascadeMANSRTSP([]byte(test.body))
			if err != nil {
				t.Fatal(err)
			}
			err = validateHistoryControlCommand(request, test.version, stream)
			if (err == nil) != test.valid {
				t.Fatalf("validation error = %v, want valid %v", err, test.valid)
			}
		})
	}
}

func TestValidateHistoryControlCommandUsesPrevious2022Scale(t *testing.T) {
	stream := &Streams{S: time.Unix(1711929600, 0), E: time.Unix(1711933200, 0)}
	parse := func(body string) *cascadeMANSRTSPRequest {
		t.Helper()
		request, err := parseCascadeMANSRTSP([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return request
	}

	descending := parse("PLAY RTSP/1.0\r\nCSeq: 1\r\nRange: npt=600-120\r\n\r\n")
	if err := validateHistoryControlCommand(descending, GBVersion30, stream); err == nil {
		t.Fatal("2022 descending Range without a previous negative Scale was accepted")
	}

	reverse := parse("PLAY RTSP/1.0\r\nCSeq: 2\r\nScale: -2\r\n\r\n")
	stream.historyState.commitResult(reverse, nil, nil)
	if err := validateHistoryControlCommand(descending, GBVersion30, stream); err != nil {
		t.Fatalf("2022 Range-only reverse playback did not inherit the previous Scale: %v", err)
	}

	ascending := parse("PLAY RTSP/1.0\r\nCSeq: 3\r\nRange: npt=120-600\r\n\r\n")
	if err := validateHistoryControlCommand(ascending, GBVersion30, stream); err == nil {
		t.Fatal("2022 ascending Range ignored the previous negative Scale")
	}

	forward := parse("PLAY RTSP/1.0\r\nCSeq: 4\r\nScale: 2\r\n\r\n")
	stream.historyState.commitResult(forward, nil, errors.New("downstream rejected control"))
	if err := validateHistoryControlCommand(descending, GBVersion30, stream); err != nil {
		t.Fatalf("failed business response changed the previous negative Scale: %v", err)
	}
	stream.historyState.commitResult(forward, nil, nil)
	if err := validateHistoryControlCommand(ascending, GBVersion30, stream); err != nil {
		t.Fatalf("2022 Range-only forward playback did not inherit the restored positive Scale: %v", err)
	}

	stream.historyState.commitResult(reverse, &historyControlResponse{hasScale: true, scale: 1}, nil)
	if err := validateHistoryControlCommand(descending, GBVersion30, stream); err == nil {
		t.Fatal("2022 response Scale did not override the requested reverse Scale")
	}
	other := &Streams{S: stream.S, E: stream.E}
	if err := validateHistoryControlCommand(descending, GBVersion30, other); err == nil {
		t.Fatal("2022 history Scale leaked into another media session")
	}
}

func TestParseHistoryControlSIPResponse(t *testing.T) {
	response := func(body string, contentType string) *sip.Response {
		result := sip.NewResponse("", sip.DefaultSipVersion, http.StatusOK, "OK", nil, []byte(body))
		if contentType != "" {
			result.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: contentType})
		}
		return result
	}

	business, err := parseHistoryControlSIPResponse(response(
		"RTSP/1.0 200 OK\r\nCSeq: 7\r\nRange: npt=100-\r\nRTP-Info: seq=18139;rtptime=3119600838\r\nScale: -2.0\r\n\r\n",
		"Application/MANSRTSP; charset=UTF-8",
	), "RTSP/1.0", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !business.hasScale || business.scale != -2 {
		t.Fatalf("parsed history response Scale = %v (present %v), want -2", business.scale, business.hasScale)
	}
	rewritten := string(business.body(99, "MANSRTSP/1.0"))
	for _, expected := range []string{
		"MANSRTSP/1.0 200 OK", "CSeq: 99", "Range: npt=100-",
		"RTP-Info: seq=18139;rtptime=3119600838", "Scale: -2.0",
	} {
		if !strings.Contains(rewritten, expected) {
			t.Fatalf("rewritten history response missing %q: %s", expected, rewritten)
		}
	}

	if business, err := parseHistoryControlSIPResponse(response("", ""), "RTSP/1.0", 7); err != nil || business != nil {
		t.Fatalf("empty compatibility response = %+v, %v", business, err)
	}

	tests := []struct {
		name         string
		body         string
		want         string
		wantBusiness bool
	}{
		{name: "business failure", body: "RTSP/1.0 500 Server Error\r\nCSeq: 7\r\n\r\n", want: "500", wantBusiness: true},
		{name: "CSeq mismatch", body: "RTSP/1.0 200 OK\r\nCSeq: 8\r\n\r\n", want: "does not match"},
		{name: "version mismatch", body: "MANSRTSP/1.0 200 OK\r\nCSeq: 7\r\n\r\n", want: "does not match"},
		{name: "invalid RTP-Info", body: "RTSP/1.0 200 OK\r\nCSeq: 7\r\nRTP-Info: seq=1;seq=2\r\n\r\n", want: "RTP-Info"},
		{name: "invalid Range", body: "RTSP/1.0 200 OK\r\nCSeq: 7\r\nRange: clock=now-\r\n\r\n", want: "Range"},
		{name: "now with end", body: "RTSP/1.0 200 OK\r\nCSeq: 7\r\nRange: npt=now-100\r\n\r\n", want: "Range"},
		{name: "SMPTE subframes without frames", body: "RTSP/1.0 200 OK\r\nCSeq: 7\r\nRange: smpte=00:00:01.5-\r\n\r\n", want: "Range"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			business, err := parseHistoryControlSIPResponse(response(test.body, "Application/MANSRTSP"), "RTSP/1.0", 7)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parse error = %v, want %q", err, test.want)
			}
			if (business != nil) != test.wantBusiness {
				t.Fatalf("parsed business response = %+v, want present %v", business, test.wantBusiness)
			}
		})
	}

	for _, rtpInfo := range []string{
		"seq=18139",
		"rtptime=3119600838",
		"url=rtsp://example.com/archive",
	} {
		body := "RTSP/1.0 200 OK\r\nCSeq: 7\r\nRTP-Info: " + rtpInfo + "\r\n\r\n"
		if _, err := parseHistoryControlSIPResponse(response(body, "Application/MANSRTSP"), "RTSP/1.0", 7); err != nil {
			t.Fatalf("optional RTP-Info %q rejected: %v", rtpInfo, err)
		}
	}

	_, err = parseHistoryControlSIPResponse(response(
		"RTSP/1.0 200 OK\r\nCSeq: 7\r\n\r\n", "application/sdp",
	), "RTSP/1.0", 7)
	if err == nil || !strings.Contains(err.Error(), "Content-Type") {
		t.Fatalf("invalid response Content-Type error = %v", err)
	}
	_, err = parseHistoryControlSIPResponse(response(
		"RTSP/1.0 200 OK\r\nCSeq: 7\r\n\r\n", "Application/MANSRTSP; charset",
	), "RTSP/1.0", 7)
	if err == nil || !strings.Contains(err.Error(), "Content-Type") {
		t.Fatalf("malformed response Content-Type error = %v", err)
	}
	_, err = parseHistoryControlSIPResponse(response(
		"RTSP/1.0 200 OK\r\nCSeq: 7\r\n\r\n", ""), "RTSP/1.0", 7)
	if err == nil || !strings.Contains(err.Error(), "exactly one Content-Type") {
		t.Fatalf("missing response Content-Type error = %v", err)
	}
	duplicate := response("RTSP/1.0 200 OK\r\nCSeq: 7\r\n\r\n", "Application/MANSRTSP")
	duplicate.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
	if _, err := parseHistoryControlSIPResponse(duplicate, "RTSP/1.0", 7); err == nil || !strings.Contains(err.Error(), "exactly one Content-Type") {
		t.Fatalf("duplicate response Content-Type error = %v", err)
	}
}

func TestHistoryTeardownCleansLocalSession(t *testing.T) {
	key := historyKey(historyModePlayback, gb10DeviceID, testCascadeChannelID)
	stream := &Streams{T: 1, StreamID: "history-teardown"}
	api := &GB28181API{streams: &conc.Map[string, *Streams]{}}
	api.streams.Store(key, stream)
	api.completeHistoryTeardown(key, stream)
	if _, ok := api.streams.Load(key); ok {
		t.Fatal("TEARDOWN left local history session registered")
	}
	api.completeHistoryTeardown(key, stream)
}
