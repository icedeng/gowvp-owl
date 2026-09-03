package sip

import (
	"bytes"
	"fmt"
	"net"
	"strings"
)

// MessageID MessageID
type MessageID string

// RequestMethod This is syntactic sugar around the string type, so make sure to use
// the Equals method rather than built-in equality, or you'll fall foul of case differences.
// If you're defining your own Method, uppercase is preferred but not compulsory.
// type RequestMethod string

// It's nicer to avoid using raw strings to represent methods, so the following standard
// method names are defined here as constants for convenience.
const (
	MethodInvite    = "INVITE"
	MethodACK       = "ACK"
	MethodCancel    = "CANCEL"
	MethodBYE       = "BYE"
	MethodRegister  = "REGISTER"
	MethodOptions   = "OPTIONS"
	MethodSubscribe = "SUBSCRIBE"
	MethodNotify    = "NOTIFY"
	// REFER    = "REFER"
	MethodInfo    = "INFO"
	MethodMessage = "MESSAGE"
)

// Message introduces common SIP message RFC 3261 - 7.
type Message interface {
	MessageID() MessageID

	Clone() Message
	// Start line returns message start line.
	StartLine() string
	// String returns string representation of SIP message in RFC 3261 form.
	String() string
	// SipVersion returns SIP protocol version.
	SipVersion() string
	// SetSipVersion sets SIP protocol version.
	SetSipVersion(version string)

	// Headers returns all message headers.
	Headers() []Header
	// GetHeaders returns slice of headers of the given type.
	GetHeaders(name string) []Header
	// AppendHeader appends header to message.
	AppendHeader(header Header)
	// PrependHeader prepends header to message.
	RemoveHeader(name string)

	// Body returns message body.
	Body() []byte
	// SetBody sets message body.
	SetBody(body []byte, setContentLength bool)

	/* Helper getters for common headers */
	// CallID returns 'Call-ID' header.
	CallID() (*CallID, bool)
	// Via returns the top 'Via' header field.
	Via() (ViaHeader, bool)
	// ViaHop returns the first segment of the top 'Via' header.
	ViaHop() (*ViaHop, bool)
	// From returns 'From' header field.
	From() (*FromHeader, bool)
	// To returns 'To' header field.
	To() (*ToHeader, bool)
	// CSeq returns 'CSeq' header field.
	CSeq() (*CSeq, bool)
	ContentLength() (*ContentLength, bool)
	ContentType() (*ContentType, bool)
	Contact() (*ContactHeader, bool)

	Transport() string
	Source() net.Addr
	SetSource(src net.Addr)
	Destination() net.Addr
	SetDestination(dest net.Addr)
	SetConnection(Connection)
	GetConnection() Connection

	IsCancel() bool
	IsAck() bool
}

type message struct {
	// message headers
	*headers
	messID       MessageID
	sipVersion   string
	body         []byte
	source, dest net.Addr
	startLine    func() string

	conn Connection `json:"-"`
}

// MessageID MessageID
func (msg *message) MessageID() MessageID {
	return msg.messID
}

// StartLine StartLine
func (msg *message) StartLine() string {
	return msg.startLine()
}

func (msg *message) String() string {
	var buffer bytes.Buffer

	// write message start line
	buffer.WriteString(msg.StartLine() + "\r\n")
	// Write the headers.
	buffer.WriteString(msg.headers.String())
	// message body
	buffer.WriteString("\r\n")
	buffer.Write(msg.Body())

	return buffer.String()
}

// SipVersion SipVersion
func (msg *message) SipVersion() string {
	return msg.sipVersion
}

// SetSipVersion SetSipVersion
func (msg *message) SetSipVersion(version string) {
	msg.sipVersion = version
}

// Body Body
func (msg *message) Body() []byte {
	return msg.body
}

// SetBody sets message body, calculates it length and add 'Content-Length' header.
func (msg *message) SetBody(body []byte, setContentLength bool) {
	msg.body = body
	if setContentLength {
		length := ContentLength(len(body))
		canonical := msg.GetHeaders("Content-Length")
		compact := msg.GetHeaders("l")
		if len(canonical) == 1 && len(compact) == 0 {
			// 保留唯一标准头的原始位置，保证事务重传和持久化指纹字节稳定。
			canonical[0] = &length
			return
		}
		// 缺失、重复或与 compact form `l` 冲突时重建唯一标准头，
		// 避免程序化报文形成歧义帧。
		msg.RemoveHeader("Content-Length")
		msg.RemoveHeader("l")
		msg.AppendHeader(&length)
	}
}

// prepareOutboundContentLength 在最终签名前收敛程序化报文的长度头。
// 签名完成后只能调用 validateOutboundContentLength，避免改写已签名报文。
func prepareOutboundContentLength(msg Message) error {
	if msg == nil {
		return fmt.Errorf("SIP message is nil")
	}
	msg.SetBody(msg.Body(), true)
	return validateOutboundContentLength(msg)
}

func validateOutboundContentLength(msg Message) error {
	if msg == nil {
		return fmt.Errorf("SIP message is nil")
	}
	if compact := msg.GetHeaders("l"); len(compact) != 0 {
		return fmt.Errorf("SIP message must not contain compact Content-Length header")
	}
	headers := msg.GetHeaders("Content-Length")
	if len(headers) != 1 {
		return fmt.Errorf("SIP message must contain exactly one Content-Length header")
	}
	length, ok := headers[0].(*ContentLength)
	if !ok || length == nil {
		return fmt.Errorf("SIP message Content-Length header is invalid")
	}
	if uint64(*length) != uint64(len(msg.Body())) {
		return fmt.Errorf("SIP message Content-Length %d does not match body length %d", *length, len(msg.Body()))
	}
	return nil
}

// serializeOutboundMessage 在网络边界一次性校验并构造线缆报文。
// Header 是公开扩展点，错误实现不得把服务进程带入 panic，也不得注入额外头行。
func serializeOutboundMessage(msg Message) (payload []byte, err error) {
	if msg == nil {
		return nil, fmt.Errorf("SIP message is nil")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			payload = nil
			err = fmt.Errorf("serialize SIP message: %v", recovered)
		}
	}()

	startLine := msg.StartLine()
	if strings.TrimSpace(startLine) == "" || strings.ContainsAny(startLine, "\r\n") {
		return nil, fmt.Errorf("SIP message start line is invalid")
	}
	headerLines, err := outboundHeaderLines(msg)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	buffer.WriteString(startLine)
	buffer.WriteString("\r\n")
	for _, line := range headerLines {
		buffer.WriteString(line)
		buffer.WriteString("\r\n")
	}
	buffer.WriteString("\r\n")
	buffer.Write(msg.Body())
	return buffer.Bytes(), nil
}

func outboundHeaderLines(msg Message) ([]string, error) {
	headers := msg.Headers()
	lines := make([]string, 0, len(headers))
	for index, header := range headers {
		if isNilInterfaceValue(header) {
			return nil, fmt.Errorf("SIP header %d is nil", index)
		}
		name := strings.TrimSpace(header.Name())
		if !isSIPToken(name) {
			return nil, fmt.Errorf("SIP header %d name is invalid", index)
		}
		if err := validateOutboundHeaderValue(name, header); err != nil {
			return nil, err
		}
		line := header.String()
		if line == "" || strings.ContainsAny(line, "\r\n") {
			return nil, fmt.Errorf("SIP %s header line is invalid", name)
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 || !strings.EqualFold(strings.TrimSpace(line[:colon]), name) {
			return nil, fmt.Errorf("SIP %s header line does not match its name", name)
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// isSIPToken 按 RFC 3261 token 语法约束请求方法、CSeq 方法和 Header 名。
// 逐字节白名单同时拒绝非 ASCII、空白、控制字符及 URI/Header 分隔符。
func isSIPToken(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("-.!%*_+`'~", rune(character)) {
			continue
		}
		return false
	}
	return true
}

func validateOutboundHeaderValue(name string, header Header) error {
	switch value := header.(type) {
	case ViaHeader:
		if len(value) == 0 {
			return fmt.Errorf("SIP %s header is empty", name)
		}
		for _, hop := range value {
			if hop == nil || strings.TrimSpace(hop.ProtocolName) == "" ||
				strings.TrimSpace(hop.ProtocolVersion) == "" || strings.TrimSpace(hop.Transport) == "" ||
				strings.TrimSpace(hop.Host) == "" || isNilInterfaceValue(hop.Params) {
				return fmt.Errorf("SIP %s header contains an invalid hop", name)
			}
		}
	case *ViaHeader:
		// ViaHeader 的规范动态类型是值类型；指针类型会使 Via()/ViaHop() 无法识别。
		return fmt.Errorf("SIP %s header has invalid type %T", name, header)
	case *FromHeader:
		if value == nil || isNilInterfaceValue(value.Params) {
			return fmt.Errorf("SIP %s header is invalid", name)
		}
		if err := validateOutboundURI(name, value.Address); err != nil {
			return err
		}
	case *ToHeader:
		if value == nil || value.Params != nil && isNilInterfaceValue(value.Params) {
			return fmt.Errorf("SIP %s header is invalid", name)
		}
		if err := validateOutboundURI(name, value.Address); err != nil {
			return err
		}
	case *ContactHeader:
		if value == nil || value.Params != nil && isNilInterfaceValue(value.Params) {
			return fmt.Errorf("SIP %s header is invalid", name)
		}
		if err := validateOutboundURI(name, value.Address); err != nil {
			return err
		}
	case *RouteHeader:
		if value == nil || len(value.Addresses) == 0 {
			return fmt.Errorf("SIP %s header is empty", name)
		}
		for _, uri := range value.Addresses {
			if err := validateOutboundURI(name, uri); err != nil {
				return err
			}
		}
	case *RecordRouteHeader:
		if value == nil || len(value.Addresses) == 0 {
			return fmt.Errorf("SIP %s header is empty", name)
		}
		for _, uri := range value.Addresses {
			if err := validateOutboundURI(name, uri); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateOutboundURI(owner string, uri *URI) error {
	if uri == nil || strings.TrimSpace(uri.Host()) == "" ||
		uri.FUriParams != nil && isNilInterfaceValue(uri.FUriParams) ||
		uri.FHeaders != nil && isNilInterfaceValue(uri.FHeaders) {
		return fmt.Errorf("SIP %s URI is invalid", owner)
	}
	return nil
}

// Transport  Transport
func (msg *message) Transport() string {
	if viaHop, ok := msg.ViaHop(); ok {
		return viaHop.Transport
	}
	return DefaultProtocol
}

// Source Source
func (msg *message) Source() net.Addr {
	return msg.source
}

// SetSource SetSource
func (msg *message) SetSource(src net.Addr) {
	msg.source = src
}

func (msg *message) SetConnection(conn Connection) {
	msg.conn = conn
}

func (msg *message) GetConnection() Connection {
	return msg.conn
}

// Destination Destination
func (msg *message) Destination() net.Addr {
	return msg.dest
}

// SetDestination SetDestination
func (msg *message) SetDestination(dest net.Addr) {
	msg.dest = dest
}

// URI  A SIP or SIPS URI, including all params and URI header params.
// noinspection GoNameStartsWithPackageName
type URI struct {
	// True if and only if the URI is a SIPS URI.
	FIsEncrypted bool

	// The user part of the URI: the 'joe' in sip:joe@bloggs.com
	// This is a pointer, so that URIs without a user part can have 'nil'.
	FUser MaybeString

	// The password field of the URI. This is represented in the URI as joe:hunter2@bloggs.com.
	// Note that if a URI has a password field, it *must* have a user field as well.
	// This is a pointer, so that URIs without a password field can have 'nil'.
	// Note that RFC 3261 strongly recommends against the use of password fields in SIP URIs,
	// as they are fundamentally insecure.
	FPassword MaybeString

	// The host part of the URI. This can be a domain, or a string representation of an IP address.
	FHost string

	// The port part of the URI. This is optional, and so is represented here as a pointer type.
	FPort *Port

	// Any parameters associated with the URI.
	// These are used to provide information about requests that may be constructed from the URI.
	// (For more details, see RFC 3261 section 19.1.1).
	// These appear as a semicolon-separated list of key=value pairs following the host[:port] part.
	FUriParams Params

	// Any headers to be included on requests constructed from this URI.
	// These appear as a '&'-separated list at the end of the URI, introduced by '?'.
	// Although the values of the map are MaybeStrings, they will never be NoString in practice as the parser
	// guarantees to not return blank values for header elements in SIP URIs.
	// You should not set the values of headers to NoString.
	FHeaders Params
}

// User User
func (uri *URI) User() MaybeString {
	return uri.FUser
}

// Host Host
func (uri *URI) Host() string {
	return uri.FHost
}

// SetHost SetHost
func (uri *URI) SetHost(host string) {
	uri.FHost = host
}

// Generates the string representation of a SipUri struct.
func (uri *URI) String() string {
	var buffer bytes.Buffer

	// Compulsory protocol identifier.
	if uri.FIsEncrypted {
		buffer.WriteString("sips")
		buffer.WriteString(":")
	} else {
		buffer.WriteString("sip")
		buffer.WriteString(":")
	}

	// Optional userinfo part.
	if user, ok := uri.FUser.(String); ok && user.String() != "" {
		buffer.WriteString(uri.FUser.String())
		if pass, ok := uri.FPassword.(String); ok && pass.String() != "" {
			buffer.WriteString(":")
			buffer.WriteString(pass.String())
		}
		buffer.WriteString("@")
	}

	// Compulsory hostname. RFC 3261 要求 IPv6 地址在 SIP URI 中使用方括号。
	buffer.WriteString(formatSIPHost(uri.FHost))

	// Optional port number.
	if uri.FPort != nil {
		buffer.WriteString(fmt.Sprintf(":%d", *uri.FPort))
	}

	if (uri.FUriParams != nil) && uri.FUriParams.Length() > 0 {
		buffer.WriteString(";")
		buffer.WriteString(uri.FUriParams.ToString(';'))
	}

	if (uri.FHeaders != nil) && uri.FHeaders.Length() > 0 {
		buffer.WriteString("?")
		buffer.WriteString(uri.FHeaders.ToString('&'))
	}

	return buffer.String()
}

func formatSIPHost(host string) string {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		return host
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// Clone the Sip URI.
func (uri *URI) Clone() *URI {
	var newURI *URI
	if uri == nil {
		return newURI
	}

	newURI = &URI{
		FIsEncrypted: uri.FIsEncrypted,
		FUser:        uri.FUser,
		FPassword:    uri.FPassword,
		FHost:        uri.FHost,
		FUriParams:   cloneWithNil(uri.FUriParams),
		FHeaders:     cloneWithNil(uri.FHeaders),
	}
	if uri.FPort != nil {
		newURI.FPort = uri.FPort.Clone()
	}
	return newURI
}

// Equals Determine if the SIP URI is equal to the specified URI according to the rules laid down in RFC 3261 s. 19.1.4.
func (uri *URI) Equals(val any) bool {
	otherPtr, ok := val.(*URI)
	if !ok {
		return false
	}

	if uri == otherPtr {
		return true
	}
	if uri == nil && otherPtr != nil || uri != nil && otherPtr == nil {
		return false
	}

	if uri.FIsEncrypted != otherPtr.FIsEncrypted ||
		!sipURIComponentEqual(uri.FUser, otherPtr.FUser, true) ||
		!sipURIComponentEqual(uri.FPassword, otherPtr.FPassword, true) ||
		!strings.EqualFold(uri.FHost, otherPtr.FHost) ||
		!Uint16PtrEq((*uint16)(uri.FPort), (*uint16)(otherPtr.FPort)) {
		return false
	}
	if !sipURIParamsEqual(uri.FUriParams, otherPtr.FUriParams) {
		return false
	}
	return sipURIHeadersEqual(uri.FHeaders, otherPtr.FHeaders)
}

func sipURIParamsEqual(left, right Params) bool {
	leftParams, ok := normalizedSIPURIParams(left)
	if !ok {
		return false
	}
	rightParams, ok := normalizedSIPURIParams(right)
	if !ok {
		return false
	}
	for name, leftValue := range leftParams {
		if rightValue, exists := rightParams[name]; exists &&
			!sipURIComponentEqual(leftValue, rightValue, false) {
			return false
		}
	}
	// 这些参数会改变 URI 的标准路由或请求语义，不能按扩展参数规则忽略单边值。
	for _, name := range []string{"user", "ttl", "method", "maddr"} {
		_, leftExists := leftParams[name]
		_, rightExists := rightParams[name]
		if leftExists != rightExists {
			return false
		}
	}
	return true
}

func sipURIHeadersEqual(left, right Params) bool {
	leftHeaders, ok := normalizedSIPURIParams(left)
	if !ok {
		return false
	}
	rightHeaders, ok := normalizedSIPURIParams(right)
	if !ok || len(leftHeaders) != len(rightHeaders) {
		return false
	}
	for name, leftValue := range leftHeaders {
		rightValue, exists := rightHeaders[name]
		if !exists || !sipURIComponentEqual(leftValue, rightValue, true) {
			return false
		}
	}
	return true
}

func normalizedSIPURIParams(params Params) (map[string]MaybeString, bool) {
	normalized := make(map[string]MaybeString)
	if params == nil {
		return normalized, true
	}
	for name, value := range params.Items() {
		name = strings.ToLower(normalizeSIPURIPercentEncoding(name))
		if _, duplicate := normalized[name]; duplicate {
			return nil, false
		}
		normalized[name] = value
	}
	return normalized, true
}

func sipURIComponentEqual(left, right MaybeString, caseSensitive bool) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue := normalizeSIPURIPercentEncoding(left.String())
	rightValue := normalizeSIPURIPercentEncoding(right.String())
	if caseSensitive {
		return leftValue == rightValue
	}
	return strings.EqualFold(leftValue, rightValue)
}

func normalizeSIPURIPercentEncoding(value string) string {
	var normalized strings.Builder
	normalized.Grow(len(value))
	for index := 0; index < len(value); index++ {
		if value[index] != '%' || index+2 >= len(value) {
			normalized.WriteByte(value[index])
			continue
		}
		high, highOK := fromHex(value[index+1])
		low, lowOK := fromHex(value[index+2])
		if !highOK || !lowOK {
			normalized.WriteByte(value[index])
			continue
		}
		decoded := high<<4 | low
		if isRFC2396Unreserved(decoded) {
			normalized.WriteByte(decoded)
		} else {
			normalized.WriteByte('%')
			normalized.WriteByte(toUpperHex(high))
			normalized.WriteByte(toUpperHex(low))
		}
		index += 2
	}
	return normalized.String()
}

func fromHex(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func toUpperHex(value byte) byte {
	if value < 10 {
		return '0' + value
	}
	return 'A' + value - 10
}

func isRFC2396Unreserved(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' ||
		strings.ContainsRune("-_.!~*'()", rune(value))
}

func cloneWithNil(params Params) Params {
	if params == nil {
		return NewParams()
	}
	return params.Clone()
}
