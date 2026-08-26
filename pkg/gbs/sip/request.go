package sip

import (
	"bytes"
	"fmt"
	"net"
	"strings"

	"github.com/gofrs/uuid"
)

// Request Request
type Request struct {
	message
	method    string
	recipient *URI
}

// NewRequest NewRequest
func NewRequest(
	messID MessageID,
	method string,
	recipient *URI,
	sipVersion string,
	hdrs []Header,
	body []byte,
) *Request {
	req := new(Request)
	if messID == "" {
		req.messID = MessageID(uuid.Must(uuid.NewV4()).String())
	} else {
		req.messID = messID
	}
	req.SetSipVersion(sipVersion)
	req.startLine = req.StartLine
	req.headers = newHeaders(hdrs)

	req.SetMethod(method)
	req.SetRecipient(recipient)

	if len(body) != 0 {
		req.SetBody(body, true)
	}
	return req
}

// NewRequestFromResponse NewRequestFromResponse
func NewRequestFromResponse(method string, resp *Response) *Request {
	request, _ := NewRequestFromResponseChecked(method, resp)
	return request
}

// NewRequestFromResponseChecked 根据响应构造对话内请求，并校验 RFC 3261 必需头。
// 设备返回畸形 2xx 时应把错误交给业务层处理，不能因缺失 Via/CSeq/To 等字段触发 panic。
func NewRequestFromResponseChecked(method string, resp *Response) (*Request, error) {
	if resp == nil {
		return nil, fmt.Errorf("cannot build %s request from nil response", method)
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		return nil, fmt.Errorf("cannot build request without method")
	}
	if method == MethodCancel {
		return nil, fmt.Errorf("cannot build CANCEL from a response; use the original INVITE transaction")
	}
	var remoteTarget *URI
	if contact, ok := resp.Contact(); ok && contact != nil && contact.Address != nil {
		remoteTarget = contact.Address.Clone()
	} else if to, ok := resp.To(); ok && to != nil && to.Address != nil {
		// 部分老设备的 2xx 响应缺少 Contact，退化为对话 To URI，避免 ACK/BYE 构造崩溃。
		remoteTarget = to.Address.Clone()
	}
	if remoteTarget == nil || strings.TrimSpace(remoteTarget.Host()) == "" {
		return nil, fmt.Errorf("cannot build %s request: response has no Contact or To target", method)
	}
	if from, ok := resp.From(); !ok || from == nil || from.Address == nil {
		return nil, fmt.Errorf("cannot build %s request: response is missing From", method)
	}
	if to, ok := resp.To(); !ok || to == nil || to.Address == nil {
		return nil, fmt.Errorf("cannot build %s request: response is missing To", method)
	}
	if callID, ok := resp.CallID(); !ok || callID == nil || strings.TrimSpace(string(*callID)) == "" {
		return nil, fmt.Errorf("cannot build %s request: response is missing Call-ID", method)
	}
	responseCSeq, ok := resp.CSeq()
	if !ok || responseCSeq == nil {
		return nil, fmt.Errorf("cannot build %s request: response is missing CSeq", method)
	}
	responseVia, ok := resp.ViaHop()
	if !ok || responseVia == nil || strings.TrimSpace(responseVia.Host) == "" {
		return nil, fmt.Errorf("cannot build %s request: response is missing Via", method)
	}
	if method == MethodACK && !strings.EqualFold(strings.TrimSpace(responseCSeq.MethodName), MethodInvite) {
		return nil, fmt.Errorf("cannot build ACK for non-INVITE response")
	}
	if method == MethodACK && (resp.StatusCode() < 200 || resp.StatusCode() >= 300) {
		return nil, fmt.Errorf("cannot build non-2xx ACK without original INVITE transaction")
	}
	routeSet, err := dialogRouteSet(resp)
	if err != nil {
		return nil, fmt.Errorf("cannot build %s request: %w", method, err)
	}
	recipient := remoteTarget.Clone()
	routes := routeSet
	if len(routeSet) > 0 && !sipURIHasParam(routeSet[0], "lr") {
		// RFC 3261 12.2.1.1 严格路由：首个 route 成为 Request-URI，远端 target 追加到 Route 尾部。
		recipient = routeSet[0].Clone()
		routes = append(cloneURISlice(routeSet[1:]), remoteTarget.Clone())
	}
	ackRequest := NewRequest(
		resp.MessageID(),
		method,
		recipient,
		resp.SipVersion(),
		[]Header{},
		[]byte{},
	)

	ackRequest.AppendHeader(ViaHeader{newDialogViaHop(responseVia)})
	if len(routes) > 0 {
		ackRequest.AppendHeader(&RouteHeader{Addresses: routes})
	}

	CopyHeaders("From", resp, ackRequest)
	CopyHeaders("To", resp, ackRequest)
	CopyHeaders("Call-ID", resp, ackRequest)
	cseq := *responseCSeq
	cseq.MethodName = method

	// https://www.rfc-editor.org/rfc/rfc3261.html#section-12.2.1.1
	// The Call-ID of the request MUST be set to the Call-ID of the dialog.
	// Requests within a dialog MUST contain strictly monotonically
	// increasing and contiguous CSeq sequence numbers (increasing-by-one)
	// in each direction (excepting ACK and CANCEL of course, whose numbers
	// equal the requests being acknowledged or cancelled).  Therefore, if
	// the local sequence number is not empty, the value of the local
	// sequence number MUST be incremented by one, and this value MUST be
	// placed into the CSeq header field.
	if method != MethodACK {
		cseq.SeqNo, err = resp.nextDialogCSeq(responseCSeq.SeqNo)
		if err != nil {
			return nil, fmt.Errorf("cannot build %s request: %w", method, err)
		}
	}
	ackRequest.AppendHeader(&cseq)
	ackRequest.SetSource(resp.Destination())
	ackRequest.SetDestination(resp.Source())
	ackRequest.SetConnection(resp.GetConnection())
	ackRequest.SetBody(nil, true)
	return ackRequest, nil
}

// NewRequestFromServerDialogChecked 根据入向请求及其 2xx 响应构造 UAS 侧对话内请求。
// 路由集按初始请求的 Record-Route 顺序建立，调用方可将生成的对话头复制到
// 已按当前连接创建的请求上，避免复用已经失效的 TCP/TLS 连接。
func NewRequestFromServerDialogChecked(method string, inbound *Request, response *Response, cseq uint32) (*Request, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		return nil, fmt.Errorf("cannot build server dialog request without method")
	}
	if inbound == nil || response == nil {
		return nil, fmt.Errorf("cannot build %s server dialog request without request and response", method)
	}
	if response.StatusCode() < 200 || response.StatusCode() >= 300 {
		return nil, fmt.Errorf("cannot build %s server dialog request from non-2xx response", method)
	}
	if cseq == 0 {
		return nil, fmt.Errorf("cannot build %s server dialog request with zero CSeq", method)
	}

	remote, remoteOK := inbound.From()
	local, localOK := response.To()
	responseRemote, responseRemoteOK := response.From()
	requestCallID, requestCallIDOK := inbound.CallID()
	responseCallID, responseCallIDOK := response.CallID()
	requestCSeq, requestCSeqOK := inbound.CSeq()
	responseCSeq, responseCSeqOK := response.CSeq()
	if !remoteOK || remote == nil || remote.Address == nil ||
		!localOK || local == nil || local.Address == nil ||
		!responseRemoteOK || responseRemote == nil || responseRemote.Address == nil ||
		!requestCallIDOK || requestCallID == nil || !responseCallIDOK || responseCallID == nil ||
		!requestCSeqOK || requestCSeq == nil || !responseCSeqOK || responseCSeq == nil {
		return nil, fmt.Errorf("cannot build %s server dialog request: dialog headers are incomplete", method)
	}
	if strings.TrimSpace(string(*requestCallID)) == "" || string(*requestCallID) != string(*responseCallID) {
		return nil, fmt.Errorf("cannot build %s server dialog request: Call-ID does not match", method)
	}
	if requestCSeq.SeqNo == 0 || requestCSeq.SeqNo != responseCSeq.SeqNo ||
		!strings.EqualFold(strings.TrimSpace(requestCSeq.MethodName), strings.TrimSpace(responseCSeq.MethodName)) {
		return nil, fmt.Errorf("cannot build %s server dialog request: CSeq does not match", method)
	}
	remoteTag := dialogHeaderParam(remote.Params, "tag")
	responseRemoteTag := dialogHeaderParam(responseRemote.Params, "tag")
	localTag := dialogHeaderParam(local.Params, "tag")
	if remoteTag == "" || responseRemoteTag == "" || localTag == "" || remoteTag != responseRemoteTag {
		return nil, fmt.Errorf("cannot build %s server dialog request: dialog tags are invalid", method)
	}

	var remoteTarget *URI
	if contact, ok := inbound.Contact(); ok && contact != nil && contact.Address != nil {
		remoteTarget = contact.Address.Clone()
	} else {
		remoteTarget = remote.Address.Clone()
	}
	if remoteTarget == nil || strings.TrimSpace(remoteTarget.Host()) == "" {
		return nil, fmt.Errorf("cannot build %s server dialog request: remote target is invalid", method)
	}
	routeSet, err := serverDialogRouteSet(inbound)
	if err != nil {
		return nil, fmt.Errorf("cannot build %s server dialog request: %w", method, err)
	}
	recipient := remoteTarget.Clone()
	routes := routeSet
	if len(routeSet) > 0 && !sipURIHasParam(routeSet[0], "lr") {
		// RFC 3261 12.2.1.1 严格路由：首个 route 成为 Request-URI，远端 target 追加到 Route 尾部。
		recipient = routeSet[0].Clone()
		routes = append(cloneURISlice(routeSet[1:]), remoteTarget.Clone())
	}

	request := NewRequest("", method, recipient, response.SipVersion(), nil, nil)
	if len(routes) > 0 {
		request.AppendHeader(&RouteHeader{Addresses: routes})
	}
	request.AppendHeader(&FromHeader{
		DisplayName: local.DisplayName,
		Address:     local.Address.Clone(),
		Params:      cloneDialogParams(local.Params),
	})
	request.AppendHeader(&ToHeader{
		DisplayName: responseRemote.DisplayName,
		Address:     responseRemote.Address.Clone(),
		Params:      cloneDialogParams(responseRemote.Params),
	})
	callID := *responseCallID
	request.AppendHeader(&callID)
	request.AppendHeader(&CSeq{SeqNo: cseq, MethodName: method})
	request.SetSource(inbound.Destination())
	request.SetDestination(inbound.Source())
	request.SetBody(nil, true)
	return request, nil
}

func serverDialogRouteSet(inbound *Request) ([]*URI, error) {
	var routeSet []*URI
	for _, header := range inbound.GetHeaders("Record-Route") {
		recordRoute, ok := header.(*RecordRouteHeader)
		if !ok || recordRoute == nil {
			return nil, fmt.Errorf("invalid Record-Route header")
		}
		for _, uri := range recordRoute.Addresses {
			if uri == nil || strings.TrimSpace(uri.Host()) == "" {
				return nil, fmt.Errorf("invalid Record-Route target")
			}
			routeSet = append(routeSet, uri.Clone())
		}
	}
	return routeSet, nil
}

func dialogHeaderParam(params Params, name string) string {
	if params == nil {
		return ""
	}
	for _, key := range params.Keys() {
		if !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		value, ok := params.Get(key)
		if !ok || value == nil {
			return ""
		}
		return strings.TrimSpace(value.String())
	}
	return ""
}

func cloneDialogParams(params Params) Params {
	if params == nil {
		return NewParams()
	}
	return params.Clone()
}

func dialogRouteSet(resp *Response) ([]*URI, error) {
	var routeSet []*URI
	for _, header := range resp.GetHeaders("Record-Route") {
		recordRoute, ok := header.(*RecordRouteHeader)
		if !ok || recordRoute == nil {
			return nil, fmt.Errorf("invalid Record-Route header")
		}
		for _, uri := range recordRoute.Addresses {
			if uri == nil || strings.TrimSpace(uri.Host()) == "" {
				return nil, fmt.Errorf("invalid Record-Route target")
			}
			routeSet = append(routeSet, uri.Clone())
		}
	}
	// RFC 3261 12.1.2：UAC 从响应建立的路由集必须按 Record-Route 逆序排列。
	for left, right := 0, len(routeSet)-1; left < right; left, right = left+1, right-1 {
		routeSet[left], routeSet[right] = routeSet[right], routeSet[left]
	}
	return routeSet, nil
}

func sipURIHasParam(uri *URI, name string) bool {
	if uri == nil || uri.FUriParams == nil {
		return false
	}
	for _, key := range uri.FUriParams.Keys() {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return true
		}
	}
	return false
}

func newDialogViaHop(responseVia *ViaHop) *ViaHop {
	via := responseVia.Clone()
	via.Params = NewParams()
	if responseVia.Params != nil {
		for _, key := range responseVia.Params.Keys() {
			switch {
			case strings.EqualFold(strings.TrimSpace(key), "branch"),
				strings.EqualFold(strings.TrimSpace(key), "received"),
				strings.EqualFold(strings.TrimSpace(key), "rport"):
				continue
			}
			value, _ := responseVia.Params.Get(key)
			via.Params.Add(key, value)
		}
	}
	via.Params.Add("branch", String{Str: GenerateBranch()})
	via.Params.Add("rport", nil)
	return via
}

// NewCancelRequestFromInviteChecked 按 RFC 3261 9.1 从原始 INVITE 客户端事务构造 CANCEL。
func NewCancelRequestFromInviteChecked(invite *Request) (*Request, error) {
	if invite == nil || !strings.EqualFold(strings.TrimSpace(invite.Method()), MethodInvite) {
		return nil, fmt.Errorf("cannot build CANCEL without original INVITE")
	}
	if invite.Recipient() == nil || strings.TrimSpace(invite.Recipient().Host()) == "" {
		return nil, fmt.Errorf("cannot build CANCEL: INVITE target is invalid")
	}
	via, ok := invite.ViaHop()
	if !ok || via == nil {
		return nil, fmt.Errorf("cannot build CANCEL: INVITE Via is missing")
	}
	if _, branch, count := sipViaParam(via, "branch"); count != 1 || branch == "" {
		return nil, fmt.Errorf("cannot build CANCEL: INVITE Via branch is invalid")
	}
	from, fromOK := invite.From()
	to, toOK := invite.To()
	callID, callIDOK := invite.CallID()
	cseq, cseqOK := invite.CSeq()
	if !fromOK || from == nil || from.Address == nil || !toOK || to == nil || to.Address == nil ||
		!callIDOK || callID == nil || strings.TrimSpace(string(*callID)) == "" || !cseqOK || cseq == nil {
		return nil, fmt.Errorf("cannot build CANCEL: INVITE transaction headers are incomplete")
	}
	cancel := NewRequest("", MethodCancel, invite.Recipient().Clone(), invite.SipVersion(), nil, nil)
	cancel.AppendHeader(ViaHeader{via.Clone()})
	CopyHeaders("Route", invite, cancel)
	CopyHeaders("From", invite, cancel)
	CopyHeaders("To", invite, cancel)
	CopyHeaders("Call-ID", invite, cancel)
	CopyHeaders("Max-Forwards", invite, cancel)
	cancel.AppendHeader(&CSeq{SeqNo: cseq.SeqNo, MethodName: MethodCancel})
	cancel.SetSource(invite.Source())
	cancel.SetDestination(invite.Destination())
	cancel.SetConnection(invite.GetConnection())
	cancel.SetBody(nil, true)
	return cancel, nil
}

// NewAckRequestForNon2xxResponseChecked 按 RFC 3261 17.1.1.3 为非 2xx INVITE 最终响应构造事务内 ACK。
func NewAckRequestForNon2xxResponseChecked(invite *Request, response *Response) (*Request, error) {
	if invite == nil || response == nil || !strings.EqualFold(strings.TrimSpace(invite.Method()), MethodInvite) ||
		response.StatusCode() < 300 || response.StatusCode() > 699 {
		return nil, fmt.Errorf("cannot build non-2xx ACK without INVITE transaction and final response")
	}
	if invite.Recipient() == nil || strings.TrimSpace(invite.Recipient().Host()) == "" {
		return nil, fmt.Errorf("cannot build non-2xx ACK: INVITE target is invalid")
	}
	inviteVia, inviteViaOK := invite.ViaHop()
	responseVia, responseViaOK := response.ViaHop()
	inviteFrom, inviteFromOK := invite.From()
	responseTo, responseToOK := response.To()
	inviteCallID, inviteCallIDOK := invite.CallID()
	responseCallID, responseCallIDOK := response.CallID()
	inviteCSeq, inviteCSeqOK := invite.CSeq()
	responseCSeq, responseCSeqOK := response.CSeq()
	if !inviteViaOK || inviteVia == nil || !responseViaOK || responseVia == nil ||
		!inviteFromOK || inviteFrom == nil || inviteFrom.Address == nil ||
		!responseToOK || responseTo == nil || responseTo.Address == nil ||
		!inviteCallIDOK || inviteCallID == nil || !responseCallIDOK || responseCallID == nil ||
		!inviteCSeqOK || inviteCSeq == nil || !responseCSeqOK || responseCSeq == nil {
		return nil, fmt.Errorf("cannot build non-2xx ACK: transaction headers are incomplete")
	}
	if !strings.EqualFold(strings.TrimSpace(responseCSeq.MethodName), MethodInvite) || responseCSeq.SeqNo != inviteCSeq.SeqNo ||
		strings.TrimSpace(string(*inviteCallID)) == "" || string(*inviteCallID) != string(*responseCallID) ||
		sipViaBranchValue(inviteVia) == "" || sipViaBranchValue(inviteVia) != sipViaBranchValue(responseVia) {
		return nil, fmt.Errorf("cannot build non-2xx ACK: response does not match INVITE transaction")
	}
	ack := NewRequest("", MethodACK, invite.Recipient().Clone(), invite.SipVersion(), nil, nil)
	ack.AppendHeader(ViaHeader{inviteVia.Clone()})
	CopyHeaders("Route", invite, ack)
	CopyHeaders("From", invite, ack)
	ack.AppendHeader(responseTo.Clone())
	CopyHeaders("Call-ID", invite, ack)
	CopyHeaders("Max-Forwards", invite, ack)
	ack.AppendHeader(&CSeq{SeqNo: inviteCSeq.SeqNo, MethodName: MethodACK})
	ack.SetSource(invite.Source())
	ack.SetDestination(invite.Destination())
	ack.SetConnection(invite.GetConnection())
	ack.SetBody(nil, true)
	return ack, nil
}

// StartLine returns Request Line - RFC 2361 7.1.
func (req *Request) StartLine() string {
	var buffer bytes.Buffer

	// Every SIP request starts with a Request Line - RFC 2361 7.1.
	buffer.WriteString(
		fmt.Sprintf(
			"%s %s %s",
			req.method,
			req.Recipient(),
			req.SipVersion(),
		),
	)

	return buffer.String()
}

// Method Method
func (req *Request) Method() string {
	return req.method
}

// SetMethod SetMethod
func (req *Request) SetMethod(method string) {
	req.method = method
}

// Recipient Recipient
func (req *Request) Recipient() *URI {
	return req.recipient
}

// SetRecipient SetRecipient
func (req *Request) SetRecipient(recipient *URI) {
	req.recipient = recipient
}

// IsInvite IsInvite
func (req *Request) IsInvite() bool {
	return req.Method() == MethodInvite
}

// IsAck IsAck
func (req *Request) IsAck() bool {
	return req.Method() == MethodACK
}

// IsCancel IsCancel
func (req *Request) IsCancel() bool {
	return req.Method() == MethodCancel
}

// Source Source
func (req *Request) Source() net.Addr {
	return req.source
}

// SetSource SetSource
func (req *Request) SetSource(src net.Addr) {
	req.source = src
}

// Destination Destination
func (req *Request) Destination() net.Addr {
	return req.dest
}

// SetDestination SetDestination
func (req *Request) SetDestination(dest net.Addr) {
	req.dest = dest
}

func (req *Request) SetConnection(conn Connection) {
	req.conn = conn
}

func (req *Request) GetConnection() Connection {
	return req.conn
}

// Clone Clone
func (req *Request) Clone() Message {
	var recipient *URI
	if req.Recipient() != nil {
		recipient = req.Recipient().Clone()
	}
	clone := NewRequest(
		"",
		req.Method(),
		recipient,
		req.SipVersion(),
		req.headers.CloneHeaders(),
		req.Body(),
	)
	clone.SetSource(req.Source())
	clone.SetDestination(req.Destination())
	clone.SetConnection(req.GetConnection())
	return clone
}
