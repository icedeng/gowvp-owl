package gbs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
)

func activeTCPAnswer(mediaType, address, setup, direction string) []byte {
	return []byte("v=0\r\n" +
		"o=device 0 0 IN IP4 " + address + "\r\n" +
		"s=Play\r\n" +
		"c=IN IP4 " + address + "\r\n" +
		"t=0 0\r\n" +
		"m=" + mediaType + " 9000 TCP/RTP/AVP 96\r\n" +
		"a=setup:" + setup + "\r\n" +
		"a=connection:new\r\n" +
		"a=" + direction + "\r\n" +
		"y=0100000001\r\n")
}

func TestParseActiveRTPAnswer(t *testing.T) {
	endpoint, err := parseActiveRTPAnswer(activeTCPAnswer("video", "192.0.2.20", "passive", "sendonly"), "video")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.IP.String() != "192.0.2.20" || endpoint.Port != 9000 {
		t.Fatalf("endpoint = %+v", endpoint)
	}

	tests := []struct {
		name string
		body []byte
		want string
	}{
		{name: "wrong setup", body: activeTCPAnswer("video", "192.0.2.20", "active", "sendonly"), want: "setup:passive"},
		{name: "does not send", body: activeTCPAnswer("video", "192.0.2.20", "passive", "recvonly"), want: "does not send"},
		{name: "unspecified address", body: activeTCPAnswer("video", "0.0.0.0", "passive", "sendonly"), want: "invalid RTP address"},
		{name: "missing media", body: activeTCPAnswer("audio", "192.0.2.20", "passive", "sendonly"), want: "no video media"},
		{name: "duplicate SSRC", body: append(activeTCPAnswer("video", "192.0.2.20", "passive", "sendonly"), "y=0100000002\r\n"...), want: "multiple y fields"},
		{name: "duplicate media", body: append(activeTCPAnswer("video", "192.0.2.20", "passive", "sendonly"), "m=video 9001 TCP/RTP/AVP 96\r\na=setup:passive\r\n"...), want: "exactly one video media"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseActiveRTPAnswer(test.body, "video")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateRTPAnswerRejectsUnofferedPayload(t *testing.T) {
	body := activeTCPAnswer("audio", "192.0.2.20", "passive", "sendonly")
	if _, err := validateRTPAnswer(body, "audio", 2, "0100000001", "8"); err == nil || !strings.Contains(err.Error(), "not offered") {
		t.Fatalf("unoffered audio payload error = %v", err)
	}
	body = []byte(strings.Replace(string(body), "TCP/RTP/AVP 96", "TCP/RTP/AVP 8", 1))
	if _, err := validateRTPAnswer(body, "audio", 2, "0100000001", "8"); err != nil {
		t.Fatalf("offered audio payload rejected: %v", err)
	}
	conflicting := []byte(strings.Replace(string(body), "a=sendonly\r\n", "a=sendonly\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:8 PCMU/8000\r\n", 1))
	if _, err := validateRTPAnswer(conflicting, "audio", 2, "0100000001", "8"); err == nil || !strings.Contains(err.Error(), "conflicting SDP rtpmap") {
		t.Fatalf("conflicting audio payload mapping error = %v", err)
	}
}

func TestBuildVoiceSDPRejectsInvalidStreamMode(t *testing.T) {
	if _, err := buildVoiceSDP(gb10ChannelID, "192.0.2.10", 9000, 3, "0100000001"); err == nil || !strings.Contains(err.Error(), "invalid RTP stream mode") {
		t.Fatalf("error = %v", err)
	}
}

type activeConnectorMediaService struct {
	*fakeRTPMediaService
	request zlm.ConnectRTPServerRequest
	err     error
}

type retrySenderMediaService struct {
	*activeConnectorMediaService
	startFailures     int
	startInvalidPorts int
	startCalls        int
}

func (f *retrySenderMediaService) StartSendRTPContext(ctx context.Context, _ *sms.MediaServer, request zlm.StartSendRTPRequest) (*zlm.StartSendRTPResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.startCalls++
	f.started = request
	if f.startFailures > 0 {
		f.startFailures--
		return nil, errors.New("connect failed")
	}
	if f.startInvalidPorts > 0 {
		f.startInvalidPorts--
		return &zlm.StartSendRTPResponse{}, nil
	}
	return &zlm.StartSendRTPResponse{LocalPort: f.startPort}, nil
}

func (f *activeConnectorMediaService) ConnectRTPServer(_ *sms.MediaServer, request zlm.ConnectRTPServerRequest) (*zlm.ConnectRTPServerResponse, error) {
	f.request = request
	if f.err != nil {
		return nil, f.err
	}
	return &zlm.ConnectRTPServerResponse{}, nil
}

func TestAckAndConnectActiveRTP(t *testing.T) {
	connection := newFlowConnection()
	invite := newFlowRequest(t, connection, sip.MethodInvite, "active-tcp-dialog", nil)
	response := sip.NewResponseFromRequest("", invite, 200, "OK", activeTCPAnswer("video", "192.0.2.21", "passive", "sendonly"))
	response.AppendHeader(&sip.ContentTypeSDP)
	media := &activeConnectorMediaService{fakeRTPMediaService: &fakeRTPMediaService{}}
	api := &GB28181API{sms: media}
	if err := api.ackAndConnectActiveRTP(t.Context(), sip.NewTransaction("active-tcp-dialog", connection), response,
		&sms.MediaServer{Type: sms.ProtocolZLMediaKit}, "stream-1", 2, "video", "0100000001", GBVersion20); err != nil {
		t.Fatal(err)
	}
	if media.request.StreamID != "stream-1" || media.request.DstURL != "192.0.2.21" || media.request.DstPort != 9000 {
		t.Fatalf("connect request = %+v", media.request)
	}
	select {
	case payload := <-connection.writes:
		if !strings.HasPrefix(string(payload), "ACK ") {
			t.Fatalf("request = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("ACK was not sent")
	}

	media.err = errors.New("connect failed")
	response = sip.NewResponseFromRequest("", invite, 200, "OK", activeTCPAnswer("video", "192.0.2.21", "passive", "sendonly"))
	response.AppendHeader(&sip.ContentTypeSDP)
	err := api.ackAndConnectActiveRTP(t.Context(), sip.NewTransaction("active-tcp-failed", connection), response,
		&sms.MediaServer{Type: sms.ProtocolZLMediaKit}, "stream-1", 2, "video", "0100000001", GBVersion20)
	if err == nil || !strings.Contains(err.Error(), "connect failed") {
		t.Fatalf("error = %v", err)
	}
	select {
	case payload := <-connection.writes:
		if !strings.HasPrefix(string(payload), "ACK ") {
			t.Fatalf("request = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("ACK was not sent before connect failure")
	}
}

func TestInviteACKUsesIndependentBoundedContext(t *testing.T) {
	connection := newFlowConnection()
	invite := newFlowRequest(t, connection, sip.MethodInvite, "cancelled-active-tcp-dialog", nil)
	response := sip.NewResponseFromRequest("", invite, 200, "OK", activeTCPAnswer("video", "192.0.2.21", "passive", "sendonly"))
	response.AppendHeader(&sip.ContentTypeSDP)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	api := &GB28181API{sms: &activeConnectorMediaService{fakeRTPMediaService: &fakeRTPMediaService{}}}
	err := api.ackAndConnectActiveRTP(ctx, sip.NewTransaction("cancelled-active-tcp-dialog", connection), response,
		&sms.MediaServer{Type: sms.ProtocolZLMediaKit}, "stream-1", 2, "video", "0100000001", GBVersion20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("active RTP connect error = %v; want %v", err, context.Canceled)
	}
	select {
	case payload := <-connection.writes:
		if !strings.HasPrefix(string(payload), "ACK ") {
			t.Fatalf("request = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("ACK was not sent after caller cancellation")
	}
}

func TestActiveRTPConnectPolicyByVersion(t *testing.T) {
	tests := []struct {
		name         string
		version      GBProtocolVersion
		mediaType    string
		wantAttempts int
		wantDelay    time.Duration
	}{
		{name: "2016 ZLM", version: GBVersion20, mediaType: sms.ProtocolZLMediaKit, wantAttempts: 1},
		{name: "2022 ZLM", version: GBVersion30, mediaType: sms.ProtocolZLMediaKit, wantAttempts: 4, wantDelay: time.Second},
		{name: "2022 unsupported media server", version: GBVersion30, mediaType: sms.ProtocolLalmax, wantAttempts: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attempts, delay := activeRTPConnectPolicy(test.version, &sms.MediaServer{Type: test.mediaType})
			if attempts != test.wantAttempts || delay != test.wantDelay {
				t.Fatalf("policy = (%d, %s), want (%d, %s)", attempts, delay, test.wantAttempts, test.wantDelay)
			}
		})
	}
}

func TestConfirmedInviteRollbackRetainsDialogWhenSIPServerUnavailable(t *testing.T) {
	api := &GB28181API{}
	response := sip.NewResponse("", sip.DefaultSipVersion, 200, "OK", nil, nil)
	err := api.sendInviteResponseBYE(t.Context(), &Channel{}, response)
	if err == nil || !strings.Contains(err.Error(), "SIP server is unavailable") {
		t.Fatalf("confirmed INVITE rollback error = %v", err)
	}
	stream := &Streams{}
	api.rememberMediaDialogCleanupResult(stream, response, err)
	stream.cleanupMu.Lock()
	defer stream.cleanupMu.Unlock()
	if stream.Resp != response || stream.dialogStopped {
		t.Fatalf("confirmed INVITE rollback ownership = response:%p stopped:%v", stream.Resp, stream.dialogStopped)
	}
}

func TestConnectActiveRTPServerRetriesAndHonorsCancellation(t *testing.T) {
	t.Run("three reconnects after initial failure", func(t *testing.T) {
		attempts := 0
		response, err := connectActiveRTPServerWithRetry(t.Context(), 4, time.Millisecond, func(context.Context) (*zlm.ConnectRTPServerResponse, error) {
			attempts++
			if attempts < 4 {
				return nil, errors.New("connect failed")
			}
			return &zlm.ConnectRTPServerResponse{Code: 0, Msg: "success"}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if attempts != 4 || response == nil || response.Code != 0 {
			t.Fatalf("attempts = %d, response = %+v", attempts, response)
		}
	})

	t.Run("empty success response is retried", func(t *testing.T) {
		attempts := 0
		response, err := connectActiveRTPServerWithRetry(t.Context(), 4, time.Millisecond, func(context.Context) (*zlm.ConnectRTPServerResponse, error) {
			attempts++
			if attempts < 4 {
				return nil, nil
			}
			return &zlm.ConnectRTPServerResponse{Code: 0, Msg: "success"}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if attempts != 4 || response == nil || response.Code != 0 {
			t.Fatalf("attempts = %d, response = %+v", attempts, response)
		}
	})

	t.Run("caller cancellation stops retry delay", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		attempts := 0
		_, err := connectActiveRTPServerWithRetry(ctx, 4, time.Second, func(context.Context) (*zlm.ConnectRTPServerResponse, error) {
			attempts++
			cancel()
			return nil, errors.New("connect failed")
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want %v", err, context.Canceled)
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1", attempts)
		}
	})
}

func TestStartCascadeRTPApplies2022ClientReconnectPolicy(t *testing.T) {
	newService := func(failures int) *retrySenderMediaService {
		return &retrySenderMediaService{
			activeConnectorMediaService: &activeConnectorMediaService{
				fakeRTPMediaService: &fakeRTPMediaService{startPort: 30000},
			},
			startFailures: failures,
		}
	}
	mediaServer := &sms.MediaServer{Type: sms.ProtocolZLMediaKit}
	offer := &cascadeVideoOffer{IsUDP: false}
	request := zlm.StartSendRTPRequest{Stream: "stream-1", DstURL: "192.0.2.30", DstPort: 9000}

	service2022 := newService(1)
	api := &GB28181API{sms: service2022}
	startedAt := time.Now()
	response, err := api.startCascadeRTP(t.Context(), GBVersion30, mediaServer, offer, request)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(startedAt); elapsed < time.Second {
		t.Fatalf("2022 reconnect interval = %s, want at least 1s", elapsed)
	}
	if service2022.startCalls != 2 || response == nil || response.LocalPort != 30000 || service2022.started != request {
		t.Fatalf("2022 start calls = %d, response = %+v, request = %+v", service2022.startCalls, response, service2022.started)
	}

	invalidResponse2022 := newService(0)
	invalidResponse2022.startInvalidPorts = 1
	api.sms = invalidResponse2022
	response, err = api.startCascadeRTP(t.Context(), GBVersion30, mediaServer, offer, request)
	if err != nil {
		t.Fatal(err)
	}
	if invalidResponse2022.startCalls != 2 || response == nil || response.LocalPort != 30000 {
		t.Fatalf("2022 invalid response calls = %d, response = %+v", invalidResponse2022.startCalls, response)
	}

	service2016 := newService(1)
	api.sms = service2016
	if _, err := api.startCascadeRTP(t.Context(), GBVersion20, mediaServer, offer, request); err == nil {
		t.Fatal("2016 active TCP send unexpectedly retried to success")
	}
	if service2016.startCalls != 1 {
		t.Fatalf("2016 start calls = %d, want 1", service2016.startCalls)
	}
}

func TestAckAndConnectRejectsInvalidSDPContentTypeAfterACK(t *testing.T) {
	for _, test := range []struct {
		name        string
		contentType []sip.Header
		body        []byte
	}{
		{name: "missing", body: activeTCPAnswer("video", "192.0.2.21", "passive", "sendonly")},
		{name: "wrong", contentType: []sip.Header{func() sip.Header { value := sip.ContentTypeXML; return &value }()}, body: activeTCPAnswer("video", "192.0.2.21", "passive", "sendonly")},
		{name: "duplicate", contentType: []sip.Header{&sip.ContentTypeSDP, &sip.ContentTypeSDP}, body: activeTCPAnswer("video", "192.0.2.21", "passive", "sendonly")},
		{name: "empty body", contentType: []sip.Header{&sip.ContentTypeSDP}},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := newFlowConnection()
			invite := newFlowRequest(t, connection, sip.MethodInvite, "invalid-sdp-"+test.name, nil)
			response := sip.NewResponseFromRequest("", invite, 200, "OK", test.body)
			for _, header := range test.contentType {
				response.AppendHeader(header)
			}
			api := &GB28181API{sms: &activeConnectorMediaService{fakeRTPMediaService: &fakeRTPMediaService{}}}
			err := api.ackAndConnectActiveRTP(t.Context(), sip.NewTransaction("invalid-sdp-"+test.name, connection), response,
				&sms.MediaServer{Type: sms.ProtocolZLMediaKit}, "stream-1", 0, "video", "0100000001", GBVersion20)
			if err == nil || !strings.Contains(err.Error(), "INVITE response") {
				t.Fatalf("invalid SDP error = %v", err)
			}
			select {
			case payload := <-connection.writes:
				if !strings.HasPrefix(string(payload), "ACK ") {
					t.Fatalf("request = %s", payload)
				}
			case <-time.After(time.Second):
				t.Fatal("ACK was not sent before rejecting invalid SDP response")
			}
		})
	}
}

func TestValidateRTPAnswerByTransport(t *testing.T) {
	answer := func(protocol, setup, direction, ssrc string) []byte {
		setupLines := ""
		if setup != "" {
			setupLines = "a=setup:" + setup + "\r\na=connection:new\r\n"
		}
		ssrcLine := ""
		if ssrc != "" {
			ssrcLine = "y=" + ssrc + "\r\n"
		}
		return []byte("v=0\r\n" +
			"o=device 0 0 IN IP4 192.0.2.21\r\n" +
			"s=Play\r\n" +
			"c=IN IP4 192.0.2.21\r\n" +
			"t=0 0\r\n" +
			"m=video 9000 " + protocol + " 96\r\n" +
			setupLines + "a=" + direction + "\r\n" + ssrcLine)
	}
	withSessionDirection := func(body []byte, direction string, keepMediaDirection bool) []byte {
		value := strings.Replace(string(body), "t=0 0\r\n", "t=0 0\r\na="+direction+"\r\n", 1)
		if !keepMediaDirection {
			value = strings.Replace(value, "a=sendonly\r\n", "", 1)
		}
		return []byte(value)
	}
	moveAttributeToSession := func(body []byte, attribute string) []byte {
		value := strings.Replace(string(body), "a="+attribute+"\r\n", "", 1)
		value = strings.Replace(value, "t=0 0\r\n", "t=0 0\r\na="+attribute+"\r\n", 1)
		return []byte(value)
	}
	mediaSetupOverridesSession := []byte(strings.Replace(
		string(answer("TCP/RTP/AVP", "active", "sendonly", "0100000001")),
		"t=0 0\r\n", "t=0 0\r\na=setup:passive\r\n", 1,
	))

	for _, test := range []struct {
		name       string
		body       []byte
		streamMode int8
		want       string
	}{
		{name: "UDP", body: answer("RTP/AVP", "", "sendonly", "0100000001"), streamMode: 0},
		{name: "passive TCP receiver", body: answer("TCP/RTP/AVP", "active", "sendonly", "0100000001"), streamMode: 1},
		{name: "session TCP setup", body: moveAttributeToSession(answer("TCP/RTP/AVP", "active", "sendonly", "0100000001"), "setup:active"), streamMode: 1},
		{name: "session TCP connection", body: moveAttributeToSession(answer("TCP/RTP/AVP", "active", "sendonly", "0100000001"), "connection:new"), streamMode: 1},
		{name: "media setup overrides session", body: mediaSetupOverridesSession, streamMode: 1},
		{name: "UDP wrong protocol", body: answer("TCP/RTP/AVP", "active", "sendonly", "0100000001"), streamMode: 0, want: "requires RTP/AVP"},
		{name: "UDP with TCP attribute", body: append(answer("RTP/AVP", "", "sendonly", "0100000001"), "a=setup:active\r\n"...), streamMode: 0, want: "must not contain TCP"},
		{name: "passive TCP wrong setup", body: answer("TCP/RTP/AVP", "passive", "sendonly", "0100000001"), streamMode: 1, want: "setup:active"},
		{name: "passive TCP duplicate setup", body: append(answer("TCP/RTP/AVP", "active", "sendonly", "0100000001"), "a=setup:active\r\n"...), streamMode: 1, want: "exactly one setup"},
		{name: "passive TCP duplicate connection", body: append(answer("TCP/RTP/AVP", "active", "sendonly", "0100000001"), "a=connection:new\r\n"...), streamMode: 1, want: "single connection:new"},
		{name: "wrong direction", body: answer("RTP/AVP", "", "recvonly", "0100000001"), streamMode: 0, want: "does not send media"},
		{name: "session direction", body: withSessionDirection(answer("RTP/AVP", "", "sendonly", "0100000001"), "recvonly", false), streamMode: 0, want: "does not send media"},
		{name: "media direction overrides session", body: withSessionDirection(answer("RTP/AVP", "", "sendonly", "0100000001"), "recvonly", true), streamMode: 0},
		{name: "conflicting media directions", body: append(answer("RTP/AVP", "", "sendonly", "0100000001"), "a=recvonly\r\n"...), streamMode: 0, want: "multiple direction"},
		{name: "valued direction", body: []byte(strings.Replace(string(answer("RTP/AVP", "", "sendonly", "0100000001")), "a=sendonly\r\n", "a=sendonly:invalid\r\n", 1)), streamMode: 0, want: "must not have a value"},
		{name: "missing SSRC", body: answer("RTP/AVP", "", "sendonly", ""), streamMode: 0, want: "invalid SSRC"},
		{name: "mismatched SSRC", body: answer("RTP/AVP", "", "sendonly", "0100000002"), streamMode: 0, want: "does not match offer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateRTPAnswer(test.body, "video", test.streamMode, "0100000001")
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
