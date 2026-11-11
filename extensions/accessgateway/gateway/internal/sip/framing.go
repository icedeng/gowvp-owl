// Package sip 仅处理设备侧消息边界与网络 flow，不执行设备鉴权和国标业务。
package sip

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const MaxHeaderBytes = 64 << 10
const MaxBodyBytes = 1 << 20
const MaxMessageBytes = MaxHeaderBytes + MaxBodyBytes

// ReadMessage 逐行限制头部，按唯一 Content-Length 读取正文，支持 TCP 粘包/拆包。
func ReadMessage(r *bufio.Reader) ([]byte, error) {
	var header bytes.Buffer
	length := -1
	for {
		line, err := r.ReadSlice('\n')
		if err != nil {
			return nil, err
		}
		if len(line) > 8192 || header.Len()+len(line) > MaxHeaderBytes {
			return nil, fmt.Errorf("SIP headers too large")
		}
		if !bytes.HasSuffix(line, []byte("\r\n")) {
			return nil, fmt.Errorf("SIP requires CRLF")
		}
		header.Write(line)
		if bytes.Equal(line, []byte("\r\n")) {
			break
		}
		name, value, ok := strings.Cut(string(line), ":")
		if ok && (strings.EqualFold(name, "Content-Length") || strings.EqualFold(name, "l")) {
			if length >= 0 {
				return nil, fmt.Errorf("duplicate SIP Content-Length")
			}
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil || length < 0 || length > MaxBodyBytes {
				return nil, fmt.Errorf("invalid SIP Content-Length")
			}
		}
	}
	if length < 0 {
		length = 0
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return append(header.Bytes(), body...), nil
}
func ValidateDatagram(data []byte) error {
	if len(data) > 65507 {
		return fmt.Errorf("SIP datagram too large")
	}
	r := bufio.NewReaderSize(bytes.NewReader(data), 8193)
	packet, e := ReadMessage(r)
	if e != nil {
		return e
	}
	if len(packet) != len(data) {
		return fmt.Errorf("SIP datagram trailing data")
	}
	return nil
}
func unavailable(data []byte) []byte {
	end := bytes.Index(data, []byte("\r\n\r\n"))
	if end < 0 {
		return nil
	}
	lines := strings.Split(string(data[:end]), "\r\n")
	if len(lines) < 2 || strings.HasPrefix(lines[0], "SIP/") || strings.HasPrefix(lines[0], "ACK ") {
		return nil
	}
	var out strings.Builder
	out.WriteString("SIP/2.0 503 Service Unavailable\r\n")
	required := map[string]bool{"from": false, "to": false, "call-id": false, "cseq": false, "via": false}
	for _, line := range lines[1:] {
		name, _, ok := strings.Cut(line, ":")
		key := strings.ToLower(name)
		if ok {
			if _, wanted := required[key]; wanted {
				required[key] = true
				out.WriteString(line)
				out.WriteString("\r\n")
			}
		}
	}
	for _, present := range required {
		if !present {
			return nil
		}
	}
	out.WriteString("Retry-After: 5\r\nContent-Length: 0\r\n\r\n")
	return []byte(out.String())
}
