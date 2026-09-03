package gbs

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	cascadeRoutePathHeader     = "X-RoutePath"
	cascadePreferredPathHeader = "X-PreferredPath"
)

type cascadeDownstreamInviteError struct {
	err      error
	response *sip.Response
}

func (e *cascadeDownstreamInviteError) Error() string {
	if e == nil || e.err == nil {
		return "cascade downstream INVITE failed"
	}
	return e.err.Error()
}

func (e *cascadeDownstreamInviteError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func wrapCascadeDownstreamInviteError(err error, response *sip.Response) error {
	if err == nil || response == nil {
		return err
	}
	return &cascadeDownstreamInviteError{err: err, response: response}
}

func cascadeDownstreamInviteResponse(err error) sip.Message {
	var downstreamErr *cascadeDownstreamInviteError
	if errors.As(err, &downstreamErr) {
		return downstreamErr.response
	}
	return nil
}

func validateCascadePlatformIdentifier(value string) error {
	if !isGBDeviceIdentifier(value) {
		return fmt.Errorf("platform id must be 20 ASCII digits")
	}
	if value[10:13] != "200" {
		return fmt.Errorf("platform id type code must be 200")
	}
	return nil
}

func parseCascadePlatformPath(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, "-")
	seen := make(map[string]struct{}, len(parts))
	for index, part := range parts {
		part = strings.TrimSpace(part)
		if err := validateCascadePlatformIdentifier(part); err != nil {
			return nil, fmt.Errorf("invalid cascade path item %d: %w", index+1, err)
		}
		if _, exists := seen[part]; exists {
			return nil, fmt.Errorf("cascade path contains duplicate platform id: %s", part)
		}
		seen[part] = struct{}{}
		parts[index] = part
	}
	return parts, nil
}

func singleSIPHeaderValue(message sip.Message, name string) (string, error) {
	if message == nil {
		return "", nil
	}
	headers := message.GetHeaders(name)
	if len(headers) == 0 {
		return "", nil
	}
	if len(headers) != 1 {
		return "", fmt.Errorf("multiple %s headers", name)
	}
	value := headers[0].String()
	if _, after, ok := strings.Cut(value, ":"); ok {
		value = after
	}
	return strings.TrimSpace(value), nil
}

// consumeCascadePreferredPath 校验当前平台是指定路径的下一跳，并返回转发给下级平台的剩余路径。
func consumeCascadePreferredPath(request *sip.Request, worker *cascadeWorker) (string, error) {
	value, err := singleSIPHeaderValue(request, cascadePreferredPathHeader)
	if err != nil || value == "" {
		return "", err
	}
	if worker == nil || worker.protocolVersion() != GBVersion30 {
		return "", fmt.Errorf("X-PreferredPath requires protocol 3.0")
	}
	path, err := parseCascadePlatformPath(value)
	if err != nil {
		return "", err
	}
	localID := strings.TrimSpace(worker.platform.localID)
	if len(path) == 0 || path[0] != localID {
		return "", fmt.Errorf("preferred cascade path does not target local platform %s", localID)
	}
	return strings.Join(path[1:], "-"), nil
}

// buildCascadeRoutePath 将下级响应携带的实际路径前置本平台编码，并校验指定路径确实被采用。
func buildCascadeRoutePath(localID string, downstream sip.Message, expectedDownstream string) (string, error) {
	localID = strings.TrimSpace(localID)
	if err := validateCascadePlatformIdentifier(localID); err != nil {
		return "", fmt.Errorf("invalid local cascade platform id: %w", err)
	}
	raw, err := singleSIPHeaderValue(downstream, cascadeRoutePathHeader)
	if err != nil {
		return "", err
	}
	downstreamPath, err := parseCascadePlatformPath(raw)
	if err != nil {
		return "", err
	}
	if expectedDownstream = strings.TrimSpace(expectedDownstream); expectedDownstream != "" {
		expectedPath, parseErr := parseCascadePlatformPath(expectedDownstream)
		if parseErr != nil {
			return "", parseErr
		}
		if raw == "" || strings.Join(downstreamPath, "-") != strings.Join(expectedPath, "-") {
			return "", fmt.Errorf("downstream did not confirm preferred cascade path %s", expectedDownstream)
		}
	}
	for _, platformID := range downstreamPath {
		if platformID == localID {
			return "", fmt.Errorf("cascade route loop detected at platform %s", localID)
		}
	}
	return strings.Join(append([]string{localID}, downstreamPath...), "-"), nil
}

func appendCascadeRoutePath(response *sip.Response, worker *cascadeWorker, downstream sip.Message, expectedDownstream string) error {
	if response == nil || worker == nil || worker.protocolVersion() != GBVersion30 {
		return nil
	}
	if err := validateCascadePlatformIdentifier(strings.TrimSpace(worker.platform.localID)); err != nil {
		downstreamPath, headerErr := singleSIPHeaderValue(downstream, cascadeRoutePathHeader)
		if headerErr != nil {
			return headerErr
		}
		// X-RoutePath 是可选扩展。兼容历史上使用非 200 类型本地编码的配置；
		// 只有实际参与指定/返回路径时才要求标准平台编码。
		if strings.TrimSpace(expectedDownstream) == "" && downstreamPath == "" {
			return nil
		}
		return fmt.Errorf("invalid local cascade platform id: %w", err)
	}
	path, err := buildCascadeRoutePath(worker.platform.localID, downstream, expectedDownstream)
	if err != nil {
		return err
	}
	response.AppendHeader(&sip.GenericHeader{HeaderName: cascadeRoutePathHeader, Contents: path})
	return nil
}

// appendCascadeFailureRoutePath 保留失败响应已走过的实际路径。下级未返回路径时，
// 只能确认指定路径中的第一跳已经收到请求，因此不能把尚未经过的完整路径写入响应。
func appendCascadeFailureRoutePath(response *sip.Response, worker *cascadeWorker, downstream sip.Message, expectedDownstream string) error {
	if response == nil || worker == nil || worker.protocolVersion() != GBVersion30 {
		return nil
	}
	raw, err := singleSIPHeaderValue(downstream, cascadeRoutePathHeader)
	if err != nil {
		return err
	}
	if raw == "" && strings.TrimSpace(expectedDownstream) != "" {
		expected, parseErr := parseCascadePlatformPath(expectedDownstream)
		if parseErr != nil {
			return parseErr
		}
		if len(expected) > 0 {
			fallback := sip.NewResponse("", sip.DefaultSipVersion, http.StatusBadGateway, "Bad Gateway", nil, nil)
			fallback.AppendHeader(&sip.GenericHeader{HeaderName: cascadeRoutePathHeader, Contents: expected[0]})
			downstream = fallback
		}
	}
	return appendCascadeRoutePath(response, worker, downstream, "")
}

func respondCascadeInviteStatus(ctx *sip.Context, worker *cascadeWorker, status int, reason string) {
	respondCascadeInviteStatusWithRoute(ctx, worker, status, reason, nil, "")
}

func respondCascadeInviteStatusWithRoute(ctx *sip.Context, worker *cascadeWorker, status int, reason string, downstream sip.Message, expectedDownstream string) {
	if ctx == nil || ctx.Request == nil || ctx.Tx == nil {
		return
	}
	response := sip.NewResponseFromRequest("", ctx.Request, status, reason, nil)
	if err := appendCascadeFailureRoutePath(response, worker, downstream, expectedDownstream); err != nil && status < http.StatusInternalServerError {
		response = sip.NewResponseFromRequest("", ctx.Request, http.StatusInternalServerError, err.Error(), nil)
	}
	_ = ctx.Tx.Respond(response)
}
