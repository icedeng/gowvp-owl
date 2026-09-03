package gbs

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
	sdp "github.com/panjjo/gosdp"
)

type activeRTPEndpoint struct {
	IP   net.IP
	Port int
}

type rtpMediaConnector interface {
	ConnectRTPServer(*sms.MediaServer, zlm.ConnectRTPServerRequest) (*zlm.ConnectRTPServerResponse, error)
}

type rtpMediaConnectorContext interface {
	ConnectRTPServerContext(context.Context, *sms.MediaServer, zlm.ConnectRTPServerRequest) (*zlm.ConnectRTPServerResponse, error)
}

const (
	activeRTPReconnects2022        = 3
	activeRTPConnectRetryDelay2022 = time.Second
	inviteACKWriteTimeout          = time.Second
)

func parseActiveRTPAnswer(body []byte, mediaType string) (*activeRTPEndpoint, error) {
	return validateRTPAnswer(body, mediaType, 2, "")
}

func validateRTPAnswer(body []byte, mediaType string, streamMode int8, expectedSSRC string, offeredFormats ...string) (*activeRTPEndpoint, error) {
	message, err := sdp.Decode(body)
	if err != nil {
		return nil, fmt.Errorf("decode RTP SDP answer: %w", err)
	}
	expectedProtocol := "RTP/AVP"
	expectedSetup := ""
	switch streamMode {
	case 0:
	case 1:
		expectedProtocol = "TCP/RTP/AVP"
		expectedSetup = "active"
	case 2:
		expectedProtocol = "TCP/RTP/AVP"
		expectedSetup = "passive"
	default:
		return nil, fmt.Errorf("invalid RTP stream mode: %d", streamMode)
	}
	medias := sdpMediasByType(message, mediaType)
	if len(medias) == 0 {
		return nil, fmt.Errorf("RTP SDP answer has no %s media", mediaType)
	}
	if len(medias) > 1 {
		return nil, fmt.Errorf("RTP SDP answer must contain exactly one %s media description", mediaType)
	}
	media := medias[0]
	if !strings.EqualFold(strings.TrimSpace(media.Description.Protocol), expectedProtocol) {
		return nil, fmt.Errorf("RTP answer requires %s %s media", expectedProtocol, mediaType)
	}
	if media.Description.Port <= 0 || media.Description.Port > 65535 {
		return nil, fmt.Errorf("invalid RTP %s port", mediaType)
	}
	for _, format := range media.Description.Formats {
		payload, parseErr := strconv.Atoi(strings.TrimSpace(format))
		if parseErr != nil || payload < 0 || payload > 127 {
			continue
		}
		if _, mappingErr := sdpPayloadFormat(media, payload); mappingErr != nil {
			return nil, fmt.Errorf("invalid RTP %s answer: %w", mediaType, mappingErr)
		}
	}
	if len(offeredFormats) > 0 {
		offered := make(map[string]struct{}, len(offeredFormats))
		for _, format := range offeredFormats {
			format = strings.TrimSpace(format)
			if format != "" {
				offered[format] = struct{}{}
			}
		}
		if len(media.Description.Formats) == 0 {
			return nil, fmt.Errorf("RTP %s answer has no payload format", mediaType)
		}
		for _, format := range media.Description.Formats {
			format = strings.TrimSpace(format)
			if _, ok := offered[format]; !ok {
				return nil, fmt.Errorf("RTP %s answer payload %q was not offered", mediaType, format)
			}
		}
	}
	setupValues, err := effectiveSDPAttributeValues(message, media, "setup")
	if err != nil {
		return nil, fmt.Errorf("TCP RTP answer requires exactly one setup:%s attribute: %w", expectedSetup, err)
	}
	connectionValues, err := effectiveSDPAttributeValues(message, media, "connection")
	if err != nil {
		return nil, fmt.Errorf("TCP RTP answer only supports a single connection:new attribute: %w", err)
	}
	if expectedSetup == "" {
		if len(setupValues) != 0 || len(connectionValues) != 0 {
			return nil, fmt.Errorf("UDP RTP answer must not contain TCP setup/connection attributes")
		}
	} else {
		if len(setupValues) != 1 || !strings.EqualFold(setupValues[0], expectedSetup) {
			return nil, fmt.Errorf("TCP RTP answer requires exactly one setup:%s attribute", expectedSetup)
		}
		if len(connectionValues) > 1 || len(connectionValues) == 1 && !strings.EqualFold(connectionValues[0], "new") {
			return nil, fmt.Errorf("TCP RTP answer only supports a single connection:new attribute")
		}
	}
	direction, err := effectiveSDPDirection(message, media)
	if err != nil {
		return nil, fmt.Errorf("invalid RTP %s direction: %w", mediaType, err)
	}
	if direction == "inactive" || direction == "recvonly" {
		return nil, fmt.Errorf("RTP %s answer does not send media", mediaType)
	}

	remoteIP := media.Connection.IP
	networkType, addressType := media.Connection.NetworkType, media.Connection.AddressType
	if remoteIP == nil {
		remoteIP = message.Connection.IP
		networkType, addressType = message.Connection.NetworkType, message.Connection.AddressType
	}
	if err := validateSDPConnectionAddress(networkType, addressType, remoteIP); err != nil {
		return nil, fmt.Errorf("invalid RTP address: %w", err)
	}
	if remoteIP == nil || remoteIP.IsUnspecified() || remoteIP.IsMulticast() {
		return nil, fmt.Errorf("invalid RTP address: %v", remoteIP)
	}
	ssrcValues := directTCPSDPLineValues(body, "y")
	if len(ssrcValues) > 1 {
		return nil, fmt.Errorf("RTP answer must not contain multiple y fields")
	}
	ssrc := ""
	if len(ssrcValues) == 1 {
		ssrc = ssrcValues[0]
	}
	if !validGBSSRC(ssrc) {
		return nil, fmt.Errorf("RTP answer has invalid SSRC %q", ssrc)
	}
	if expectedSSRC = strings.TrimSpace(expectedSSRC); expectedSSRC != "" && ssrc != expectedSSRC {
		return nil, fmt.Errorf("RTP answer SSRC %s does not match offer %s", ssrc, expectedSSRC)
	}
	return &activeRTPEndpoint{IP: remoteIP, Port: media.Description.Port}, nil
}

func (g *GB28181API) ackAndConnectActiveRTP(ctx context.Context, tx *sip.Transaction, resp *sip.Response, mediaServer *sms.MediaServer, streamID string, streamMode int8, mediaType, expectedSSRC string, version GBProtocolVersion, offeredFormats ...string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ack, err := sip.NewRequestFromResponseChecked(sip.MethodACK, resp)
	if err != nil {
		return err
	}
	if err := requestInviteACKContext(ctx, tx, ack); err != nil {
		return err
	}
	if err := validateSIPContentType(resp, string(sip.ContentTypeSDP)); err != nil {
		return fmt.Errorf("INVITE response %w", err)
	}
	if strings.TrimSpace(string(resp.Body())) == "" {
		return fmt.Errorf("INVITE response SDP body is empty")
	}
	endpoint, err := validateRTPAnswer(resp.Body(), mediaType, streamMode, expectedSSRC, offeredFormats...)
	if err != nil {
		return err
	}
	if streamMode != 2 {
		return nil
	}
	connector, legacy := g.sms.(rtpMediaConnector)
	contextConnector, contextAware := g.sms.(rtpMediaConnectorContext)
	if !legacy && !contextAware {
		return fmt.Errorf("media service does not support active TCP RTP receiving")
	}
	request := zlm.ConnectRTPServerRequest{
		StreamID: strings.TrimSpace(streamID),
		DstURL:   endpoint.IP.String(),
		DstPort:  endpoint.Port,
	}
	attempts, delay := activeRTPConnectPolicy(version, mediaServer)
	_, err = connectActiveRTPServerWithRetry(ctx, attempts, delay, func(connectCtx context.Context) (*zlm.ConnectRTPServerResponse, error) {
		if contextAware {
			return contextConnector.ConnectRTPServerContext(connectCtx, mediaServer, request)
		}
		return connector.ConnectRTPServer(mediaServer, request)
	})
	if err != nil {
		return fmt.Errorf("connect active TCP RTP server: %w", err)
	}
	return nil
}

// requestInviteACKContext 让已经收到 2xx 的 INVITE 即使调用方刚好取消也能完成 ACK，
// 同时给 TCP/TLS 阻塞写设置独立上限，避免业务退出或停服被半开连接无限拖住。
func requestInviteACKContext(ctx context.Context, tx *sip.Transaction, ack *sip.Request) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inviteACKWriteTimeout)
	defer cancel()
	return tx.RequestContext(ackCtx, ack)
}

func activeRTPConnectPolicy(version GBProtocolVersion, mediaServer *sms.MediaServer) (attempts int, delay time.Duration) {
	if version.AtLeast(GBVersion30) && mediaServer != nil && mediaServer.Type == sms.ProtocolZLMediaKit {
		return activeRTPReconnects2022 + 1, activeRTPConnectRetryDelay2022
	}
	return 1, 0
}

func connectActiveRTPServerWithRetry(
	ctx context.Context,
	attempts int,
	delay time.Duration,
	connect func(context.Context) (*zlm.ConnectRTPServerResponse, error),
) (*zlm.ConnectRTPServerResponse, error) {
	return retryMediaOperationWithDelay(ctx, attempts, delay, func(operationCtx context.Context) (*zlm.ConnectRTPServerResponse, error) {
		response, err := connect(operationCtx)
		if err != nil {
			return nil, err
		}
		if response == nil {
			return nil, fmt.Errorf("media server returned an empty active TCP RTP response")
		}
		return response, nil
	})
}

func retryMediaOperationWithDelay[T any](
	ctx context.Context,
	attempts int,
	delay time.Duration,
	operation func(context.Context) (*T, error),
) (*T, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if attempts <= 0 || operation == nil {
		return nil, fmt.Errorf("invalid active TCP RTP retry policy")
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		response, err := operation(ctx)
		if err == nil {
			return response, nil
		}
		lastErr = err
		if attempt+1 == attempts {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func (g *GB28181API) sendInviteResponseBYE(ctx context.Context, ch *Channel, resp *sip.Response) error {
	if resp == nil {
		return nil
	}
	if g == nil || g.svr == nil {
		return fmt.Errorf("SIP server is unavailable")
	}
	if ch == nil {
		return fmt.Errorf("SIP dialog channel is unavailable")
	}
	tx, err := g.svr.requestFromResponseCleanupContext(ctx, ch, sip.MethodBYE, resp)
	if err == nil {
		g.consumeDialogResponseAsync(tx)
	}
	return err
}
