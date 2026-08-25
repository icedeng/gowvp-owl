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
	var recipient *URI
	if contact, ok := resp.Contact(); ok && contact != nil && contact.Address != nil {
		recipient = contact.Address.Clone()
	} else if to, ok := resp.To(); ok && to != nil && to.Address != nil {
		// 部分老设备的 2xx 响应缺少 Contact，退化为对话 To URI，避免 ACK/BYE 构造崩溃。
		recipient = to.Address.Clone()
	}
	if recipient == nil {
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
	if viaHop, ok := resp.ViaHop(); !ok || viaHop == nil || strings.TrimSpace(viaHop.Host) == "" {
		return nil, fmt.Errorf("cannot build %s request: response is missing Via", method)
	}
	ackRequest := NewRequest(
		resp.MessageID(),
		method,
		recipient,
		resp.SipVersion(),
		[]Header{},
		[]byte{},
	)

	CopyHeaders("Via", resp, ackRequest)
	viaHop, ok := ackRequest.ViaHop()
	if !ok || viaHop == nil {
		return nil, fmt.Errorf("cannot build %s request: failed to copy Via", method)
	}
	if viaHop.Params == nil {
		viaHop.Params = NewParams()
	}
	// update branch, 2xx ACK is separate Tx
	viaHop.Params.Add("branch", String{Str: GenerateBranch()})

	if len(resp.GetHeaders("Route")) > 0 {
		CopyHeaders("Route", resp, ackRequest)
	} else {
		uris := make([]*URI, 0)
		for _, h := range resp.GetHeaders("Record-Route") {
			recordRoute, ok := h.(*RecordRouteHeader)
			if !ok || recordRoute == nil {
				continue
			}
			for _, u := range recordRoute.Addresses {
				if u != nil {
					uris = append(uris, u.Clone())
				}
			}
		}
		// RFC 3261 12.1.2：UAC 从响应建立的路由集必须按 Record-Route 逆序排列。
		for left, right := 0, len(uris)-1; left < right; left, right = left+1, right-1 {
			uris[left], uris[right] = uris[right], uris[left]
		}
		if len(uris) > 0 {
			ackRequest.AppendHeader(&RouteHeader{Addresses: uris})
		}
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
	if !(method == MethodACK || method == MethodCancel) {
		cseq.SeqNo++
	}
	ackRequest.AppendHeader(&cseq)
	ackRequest.SetSource(resp.Destination())
	ackRequest.SetDestination(resp.Source())
	return ackRequest, nil
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
	return NewRequest(
		"",
		req.Method(),
		req.Recipient().Clone(),
		req.SipVersion(),
		req.headers.CloneHeaders(),
		req.Body(),
	)
}
