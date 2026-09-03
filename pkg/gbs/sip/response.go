package sip

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/gofrs/uuid"
)

// Response Response
type Response struct {
	message
	statusCode       int
	reason           string
	dialogCSeqMu     sync.Mutex
	dialogCSeq       uint32
	dialogCSeqLoaded bool
}

// NewResponseFromRequest NewResponseFromRequest
func NewResponseFromRequest(
	resID MessageID,
	req *Request,
	statusCode int,
	reason string,
	body []byte,
) *Response {
	res := NewResponse(
		resID,
		req.SipVersion(),
		statusCode,
		reason,
		[]Header{},
		[]byte{},
	)

	CopyHeaders("Record-Route", req, res)
	CopyHeaders("Via", req, res)
	CopyHeaders("From", req, res)
	to, ok := req.To()
	if ok && to != nil {
		responseTo, _ := to.Clone().(*ToHeader)
		if responseTo.Params == nil {
			responseTo.Params = NewParams()
		}
		if _, ok := responseTo.Params.Get("tag"); !ok {
			responseTo.Params.Add("tag", String{Str: RandString(32)})
		}
		res.AppendHeader(responseTo)
	}
	CopyHeaders("CSeq", req, res)
	CopyHeaders("Call-ID", req, res)

	if statusCode == 100 {
		CopyHeaders("Timestamp", req, res)
	}

	res.SetSource(req.Destination())
	res.SetDestination(req.Source())

	res.SetBody(body, true)

	res.AppendHeader(&GenericHeader{
		HeaderName: "User-Agent",
		Contents:   "GoWVP/1.0",
	})

	return res
}

// NewResponse NewResponse
func NewResponse(
	messID MessageID,
	sipVersion string,
	statusCode int,
	reason string,
	hdrs []Header,
	body []byte,
) *Response {
	return newResponse(messID, sipVersion, statusCode, reason, hdrs, body, true)
}

// newResponse 与 newRequest 一样，为解析器保留原始头集合；公开构造的响应
// 始终携带唯一且准确的 Content-Length。
func newResponse(
	messID MessageID,
	sipVersion string,
	statusCode int,
	reason string,
	hdrs []Header,
	body []byte,
	setContentLength bool,
) *Response {
	res := new(Response)
	if messID == "" {
		res.messID = MessageID(uuid.Must(uuid.NewV4()).String())
	} else {
		res.messID = messID
	}
	res.startLine = res.StartLine
	res.SetSipVersion(sipVersion)
	res.headers = newHeaders(hdrs)
	res.SetStatusCode(statusCode)
	res.SetReason(reason)
	res.SetBody(body, setContentLength)

	return res
}

// Reason Reason
func (res *Response) Reason() string {
	return res.reason
}

// SetReason SetReason
func (res *Response) SetReason(reason string) {
	res.reason = reason
}

// SetStatusCode SetStatusCode
func (res *Response) SetStatusCode(code int) {
	res.statusCode = code
}

// StatusCode StatusCode
func (res *Response) StatusCode() int {
	return res.statusCode
}

// StartLine returns Response Status Line - RFC 2361 7.2.
func (res *Response) StartLine() string {
	var buffer bytes.Buffer

	// Every SIP response starts with a Status Line - RFC 2361 7.2.
	buffer.WriteString(
		fmt.Sprintf(
			"%s %d %s",
			res.SipVersion(),
			res.StatusCode(),
			res.Reason(),
		),
	)

	return buffer.String()
}

// Clone Clone
func (res *Response) Clone() Message {
	if res == nil {
		return (*Response)(nil)
	}
	clone := NewResponse(
		"",
		res.SipVersion(),
		res.StatusCode(),
		res.Reason(),
		res.headers.CloneHeaders(),
		res.Body(),
	)
	clone.SetSource(res.Source())
	clone.SetDestination(res.Destination())
	clone.SetConnection(res.conn)
	return clone
}

// IsAck IsAck
func (res *Response) IsAck() bool {
	if cseq, ok := res.CSeq(); ok {
		return cseq.MethodName == MethodACK
	}
	return false
}

// IsCancel IsCancel
func (res *Response) IsCancel() bool {
	if cseq, ok := res.CSeq(); ok {
		return cseq.MethodName == MethodCancel
	}
	return false
}
