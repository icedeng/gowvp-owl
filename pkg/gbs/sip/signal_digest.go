package sip

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const (
	SignalDigestEncodingBase64 = "base64"
	SignalDigestEncodingHex    = "hex"
	defaultSignalDigestWindow  = 10 * time.Minute
	signalDigestDateLayout     = "2006-01-02T15:04:05"
)

var signalDigestBeijingLocation = time.FixedZone("CST", 8*60*60)

// MessageSecurity 在 SIP 消息最终写出及收到响应时执行签名和验签。
type MessageSecurity interface {
	Sign(Message) error
	Verify(Message) error
}

// SignalDigestOptions 对应 GB/T 28181 的 Date + Note 信令摘要参数。
type SignalDigestOptions struct {
	Seed            string
	Algorithm       string
	Encoding        string
	Window          time.Duration
	Required        bool
	AcceptLegacyHex bool
	Now             func() time.Time
}

// SignalDigestSecurity 实现 GB/T 28181-2011/2016 附录 H 及 2022 版 8.3 的信令摘要。
type SignalDigestSecurity struct {
	seed            string
	algorithm       string
	encoding        string
	window          time.Duration
	required        bool
	acceptLegacyHex bool
	now             func() time.Time
}

func NewSignalDigestSecurity(options SignalDigestOptions) (*SignalDigestSecurity, error) {
	seed := strings.TrimSpace(options.Seed)
	if seed == "" {
		return nil, fmt.Errorf("signal Digest seed is required")
	}
	algorithm := canonicalDigestAlgorithm(options.Algorithm)
	if _, err := digestHashFactory(algorithm); err != nil {
		return nil, err
	}
	encoding := strings.ToLower(strings.TrimSpace(options.Encoding))
	if encoding == "" {
		encoding = SignalDigestEncodingBase64
	}
	if encoding != SignalDigestEncodingBase64 && encoding != SignalDigestEncodingHex {
		return nil, fmt.Errorf("unsupported signal Digest encoding %q", options.Encoding)
	}
	window := options.Window
	if window <= 0 {
		window = defaultSignalDigestWindow
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &SignalDigestSecurity{
		seed:            seed,
		algorithm:       algorithm,
		encoding:        encoding,
		window:          window,
		required:        options.Required,
		acceptLegacyHex: options.AcceptLegacyHex,
		now:             now,
	}, nil
}

func canonicalDigestAlgorithm(value string) string {
	switch strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(value), "_", "-")) {
	case "", "MD5":
		return "MD5"
	case "SHA-1", "SHA1":
		return "SHA-1"
	case "SHA-256", "SHA256":
		return "SHA-256"
	default:
		return strings.TrimSpace(value)
	}
}

func (security *SignalDigestSecurity) Sign(message Message) error {
	if security == nil || signalDigestExempt(message) {
		return nil
	}
	message.RemoveHeader("Date")
	message.RemoveHeader("Note")
	date := security.now().In(signalDigestBeijingLocation).Format(signalDigestDateLayout)
	message.AppendHeader(&GenericHeader{HeaderName: "Date", Contents: date})
	digest, err := security.calculate(message, date)
	if err != nil {
		return err
	}
	nonce := security.encode(digest)
	message.AppendHeader(&GenericHeader{
		HeaderName: "Note",
		Contents:   fmt.Sprintf(`Digest nonce="%s",algorithm=%s`, nonce, security.algorithm),
	})
	return nil
}

func (security *SignalDigestSecurity) Verify(message Message) error {
	if security == nil || signalDigestExempt(message) {
		return nil
	}
	date, datePresent, err := singleGenericHeaderValue(message, "Date")
	if err != nil {
		return err
	}
	note, notePresent, err := singleGenericHeaderValue(message, "Note")
	if err != nil {
		return err
	}
	if !datePresent && !notePresent && !security.required {
		return nil
	}
	if !datePresent || !notePresent {
		return fmt.Errorf("signal Digest requires both Date and Note headers")
	}
	dateTime, err := parseSignalDigestDate(date)
	if err != nil {
		return err
	}
	delta := security.now().Sub(dateTime)
	if delta < 0 {
		delta = -delta
	}
	if delta > security.window {
		return fmt.Errorf("signal Digest Date is outside the allowed window")
	}
	auth := AuthFromValue(note)
	if !strings.EqualFold(canonicalDigestAlgorithm(auth.Algorithm()), security.algorithm) {
		return fmt.Errorf("signal Digest algorithm mismatch: %s", auth.Algorithm())
	}
	provided := strings.TrimSpace(auth.Get("nonce"))
	if provided == "" {
		return fmt.Errorf("signal Digest nonce is missing")
	}
	expected, err := security.calculate(message, date)
	if err != nil {
		return err
	}
	if security.matches(provided, expected) {
		return nil
	}
	return fmt.Errorf("signal Digest verification failed")
}

func (security *SignalDigestSecurity) calculate(message Message, date string) ([]byte, error) {
	from, ok, err := singleGenericHeaderValue(message, "From")
	if err != nil || !ok {
		return nil, fmt.Errorf("signal Digest requires one From header")
	}
	to, ok, err := singleGenericHeaderValue(message, "To")
	if err != nil || !ok {
		return nil, fmt.Errorf("signal Digest requires one To header")
	}
	callID, ok, err := singleGenericHeaderValue(message, "Call-ID")
	if err != nil || !ok {
		return nil, fmt.Errorf("signal Digest requires one Call-ID header")
	}
	newHash, err := digestHashFactory(security.algorithm)
	if err != nil {
		return nil, err
	}
	hasher := newHash()
	_, _ = hasher.Write([]byte(from))
	_, _ = hasher.Write([]byte(to))
	_, _ = hasher.Write([]byte(callID))
	_, _ = hasher.Write([]byte(date))
	_, _ = hasher.Write([]byte(security.seed))
	_, _ = hasher.Write(message.Body())
	return hasher.Sum(nil), nil
}

func (security *SignalDigestSecurity) encode(value []byte) string {
	if security.encoding == SignalDigestEncodingHex {
		return hex.EncodeToString(value)
	}
	return base64.StdEncoding.EncodeToString(value)
}

func (security *SignalDigestSecurity) matches(provided string, expected []byte) bool {
	candidates := []string{security.encode(expected)}
	if security.acceptLegacyHex && security.encoding != SignalDigestEncodingHex {
		candidates = append(candidates, hex.EncodeToString(expected))
	}
	for _, candidate := range candidates {
		if len(provided) == len(candidate) && subtle.ConstantTimeCompare([]byte(provided), []byte(candidate)) == 1 {
			return true
		}
	}
	return false
}

func signalDigestExempt(message Message) bool {
	if message == nil {
		return true
	}
	if request, ok := message.(*Request); ok {
		return strings.EqualFold(request.Method(), MethodRegister)
	}
	if cseq, ok := message.CSeq(); ok && cseq != nil {
		return strings.EqualFold(cseq.MethodName, MethodRegister)
	}
	return false
}

func singleGenericHeaderValue(message Message, name string) (string, bool, error) {
	if message == nil {
		return "", false, fmt.Errorf("nil SIP message")
	}
	headers := message.GetHeaders(name)
	if len(headers) == 0 {
		return "", false, nil
	}
	if len(headers) != 1 || headers[0] == nil {
		return "", false, fmt.Errorf("signal Digest requires exactly one %s header", name)
	}
	value := headers[0].String()
	if _, after, ok := strings.Cut(value, ":"); ok {
		value = after
	}
	return strings.TrimSpace(value), true, nil
}

func parseSignalDigestDate(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{
		signalDigestDateLayout,
		"2006-01-02T15:04:05.000",
		"2006010215:04:05",
		"2006010215:04",
		time.RFC1123,
		time.RFC1123Z,
	} {
		if parsed, err := time.ParseInLocation(layout, value, signalDigestBeijingLocation); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid signal Digest Date %q", value)
}
