package sip

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand"
	"net"
	"net/http"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// Error Error
type Error struct {
	err    error
	params []any
}

func (err *Error) Error() string {
	if err == nil {
		return "<nil>"
	}
	str := fmt.Sprint(err.params...)
	if err.err != nil {
		str += fmt.Sprintf(" err:%s", err.err.Error())
	}
	return str
}

// NewError NewError
func NewError(err error, params ...any) error {
	return &Error{err, params}
}

// JSONEncode JSONEncode
func JSONEncode(data any) []byte {
	d, err := json.Marshal(data)
	if err != nil {
		slog.Error("JSONEncode error:", "err", err)
	}
	return d
}

// JSONDecode JSONDecode
func JSONDecode(data []byte, obj any) error {
	return json.Unmarshal(data, obj)
}

func RandInt(min, max int) int {
	if max < min {
		return 0
	}
	max++
	max -= min
	mathrand.Seed(time.Now().UnixNano())
	r := mathrand.Int()
	return r%max + min
}

const (
	letterBytes = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

// RandString https://github.com/kpbird/golang_random_string
func RandString(n int) string {
	output := make([]byte, n)
	// We will take n bytes, one byte for each character of output.
	randomness := make([]byte, n)
	// read all random
	_, err := cryptorand.Read(randomness)
	if err != nil {
		panic(err)
	}
	l := len(letterBytes)
	// fill output
	for pos := range output {
		// get random item
		random := randomness[pos]
		// random % 64
		randomPos := random % uint8(l)
		// put into output
		output[pos] = letterBytes[randomPos]
	}

	return string(output)
}

func timeoutClient() *http.Client {
	connectTimeout := time.Duration(20 * time.Second)
	readWriteTimeout := time.Duration(30 * time.Second)
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         timeoutDialer(connectTimeout, readWriteTimeout),
			MaxIdleConnsPerHost: 200,
			DisableKeepAlives:   true,
		},
	}
}

func timeoutDialer(cTimeout time.Duration,
	rwTimeout time.Duration,
) func(ctx context.Context, net, addr string) (c net.Conn, err error) {
	return func(ctx context.Context, netw, addr string) (net.Conn, error) {
		conn, err := net.DialTimeout(netw, addr, cTimeout)
		if err != nil {
			return nil, err
		}
		conn.SetDeadline(time.Now().Add(rwTimeout))
		return conn, nil
	}
}

// PostRequest PostRequest
func PostRequest(url string, bodyType string, body io.Reader) ([]byte, error) {
	client := timeoutClient()
	resp, err := client.Post(url, bodyType, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respbody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return respbody, nil
}

// PostJSONRequest PostJSONRequest
func PostJSONRequest(url string, data any) ([]byte, error) {
	bytesData, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return PostRequest(url, "application/json;charset=UTF-8", bytes.NewReader(bytesData))
}

// GetRequest GetRequest
func GetRequest(url string) ([]byte, error) {
	client := timeoutClient()
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respbody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return respbody, nil
}

// XMLDecode 解码 xml
func XMLDecode(data []byte, v any) error {
	destination := reflect.ValueOf(v)
	if !destination.IsValid() || destination.Kind() != reflect.Pointer || destination.IsNil() {
		return fmt.Errorf("XML decode destination must be a non-nil pointer")
	}
	decode := func(input []byte) (reflect.Value, error) {
		temporary := reflect.New(destination.Elem().Type())
		if err := xmlDecode(input, temporary.Interface()); err != nil {
			return reflect.Value{}, err
		}
		return temporary.Elem(), nil
	}

	decoded, err := decode(data)
	if err == nil {
		destination.Elem().Set(decoded)
		return nil
	}
	value := string(data)
	value = strings.Replace(value, `<?xml version="1.0"?>`, `<?xml version="1.0" encoding="GB2312"?>`, 1)
	value = strings.Replace(value, `UTF-8`, `GB2312`, 1)
	decoded, err = decode([]byte(value))
	if err != nil {
		return err
	}
	destination.Elem().Set(decoded)
	return nil
}

func xmlDecode(data []byte, v any) error {
	decoder := NewGBXMLDecoder(data)
	return decoder.Decode(v)
}

// NewGBXMLDecoder 创建兼容国标信令字符集的 XML 解码器。
// 对实际 UTF-8 字节保留历史上的宽容处理；非 UTF-8 字节统一按 GB18030
// （兼容 GB2312/GBK）解码，并修复设备常见的缺失或错误 UTF-8 声明。
func NewGBXMLDecoder(data []byte) *xml.Decoder {
	sourceUTF8 := utf8.Valid(data)
	input := data
	if !sourceUTF8 {
		input = xmlDocumentWithEncoding(data, "GB18030")
	}
	decoder := xml.NewDecoder(bytes.NewReader(input))
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		if sourceUTF8 {
			return input, nil
		}
		return simplifiedchinese.GB18030.NewDecoder().Reader(input), nil
	}
	return decoder
}

// XMLEncode XML编码器
func XMLEncode(data any) ([]byte, error) {
	b, err := xml.Marshal(data)
	if err != nil {
		slog.Error("MarshalIndent", "err", err)
		return nil, err
	}
	return EncodeGBXMLDocument(b)
}

// EncodeGBXMLDocument 把 UTF-8 XML 文档规范化为实际 GBK 字节，并声明 GB2312。
// GBK 覆盖 GB2312 字符集，保持项目既有的国标设备兼容策略。
func EncodeGBXMLDocument(data []byte) ([]byte, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("GB XML encoder requires UTF-8 input")
	}
	return Utf8ToGbk(xmlDocumentWithEncoding(data, "GB2312"))
}

func xmlDocumentWithEncoding(data []byte, charset string) []byte {
	const declarationPrefix = "<?xml"
	if !bytes.HasPrefix(data, []byte(declarationPrefix)) {
		prefix := []byte("<?xml version=\"1.0\" encoding=\"" + charset + "\"?>\n")
		return append(prefix, data...)
	}
	declarationEnd := bytes.Index(data, []byte("?>"))
	if declarationEnd < 0 {
		return data
	}
	declaration := data[:declarationEnd]
	lowerDeclaration := strings.ToLower(string(declaration))
	encodingIndex := strings.Index(lowerDeclaration, "encoding")
	if encodingIndex < 0 {
		result := make([]byte, 0, len(data)+len(charset)+12)
		result = append(result, data[:declarationEnd]...)
		result = append(result, []byte(" encoding=\"")...)
		result = append(result, charset...)
		result = append(result, '"')
		result = append(result, data[declarationEnd:]...)
		return result
	}
	valueStart := encodingIndex + len("encoding")
	for valueStart < len(declaration) && strings.ContainsRune(abnfWs, rune(declaration[valueStart])) {
		valueStart++
	}
	if valueStart >= len(declaration) || declaration[valueStart] != '=' {
		return data
	}
	valueStart++
	for valueStart < len(declaration) && strings.ContainsRune(abnfWs, rune(declaration[valueStart])) {
		valueStart++
	}
	if valueStart >= len(declaration) || declaration[valueStart] != '\'' && declaration[valueStart] != '"' {
		return data
	}
	quote := declaration[valueStart]
	valueStart++
	valueEnd := bytes.IndexByte(declaration[valueStart:], quote)
	if valueEnd < 0 {
		return data
	}
	valueEnd += valueStart
	result := make([]byte, 0, len(data)-valueEnd+valueStart+len(charset))
	result = append(result, data[:valueStart]...)
	result = append(result, charset...)
	result = append(result, data[valueEnd:]...)
	return result
}

// Max Max
func Max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ResolveSelfIP ResolveSelfIP
func ResolveSelfIP() (net.IP, error) {
	return resolveSelfIPForFamily(0)
}

func resolveSelfIPForFamily(family int) (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	addrs := make([]net.Addr, 0)
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		ifaceAddrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		addrs = append(addrs, ifaceAddrs...)
	}
	return selectSelfIP(addrs, family)
}

func selectSelfIP(addrs []net.Addr, family int) (net.IP, error) {
	var ipv6 net.IP
	for _, addr := range addrs {
		var ip net.IP
		switch value := addr.(type) {
		case *net.IPNet:
			ip = value.IP
		case *net.IPAddr:
			ip = value.IP
		}
		if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
			continue
		}
		if ip4 := ip.To4(); ip4 != nil {
			if family == 0 || family == 4 {
				return ip4, nil
			}
			continue
		}
		if ip.To16() != nil && family != 4 && ipv6 == nil {
			ipv6 = ip
		}
	}
	if ipv6 != nil {
		return ipv6, nil
	}
	return nil, errors.New("server not connected to a usable network")
}

// GBK 转 UTF-8
func GbkToUtf8(s []byte) ([]byte, error) {
	reader := transform.NewReader(bytes.NewReader(s), simplifiedchinese.GBK.NewDecoder())
	d, e := io.ReadAll(reader)
	if e != nil {
		return nil, e
	}
	return d, nil
}

// UTF-8 转 GBK
func Utf8ToGbk(s []byte) ([]byte, error) {
	reader := transform.NewReader(bytes.NewReader(s), simplifiedchinese.GBK.NewEncoder())
	d, e := io.ReadAll(reader)
	if e != nil {
		return nil, e
	}
	return d, nil
}
