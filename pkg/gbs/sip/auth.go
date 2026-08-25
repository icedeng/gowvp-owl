package sip

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
)

// Authorization 表示 SIP Digest 挑战或认证参数。
type Authorization struct {
	realm     string
	nonce     string
	algorithm string
	username  string
	password  string
	uri       string
	response  string
	method    string
	qop       string
	nc        string
	cnonce    string
	other     map[string]string
	Data      map[string]string
}

// AuthFromValue AuthFromValue
func AuthFromValue(value string) *Authorization {
	auth := &Authorization{
		algorithm: "MD5",
		other:     make(map[string]string),
		Data:      make(map[string]string),
	}

	for key, value := range parseDigestParams(value) {
		switch key {
		case "realm":
			auth.realm = value
		case "algorithm":
			auth.algorithm = value
		case "nonce":
			auth.nonce = value
		case "username":
			auth.username = value
		case "uri":
			auth.uri = value
		case "response":
			auth.response = value
		case "qop":
			for v := range strings.SplitSeq(value, ",") {
				v = strings.ToLower(strings.TrimSpace(v))
				if v == "auth" {
					auth.qop = "auth"
					break
				}
			}
			if auth.qop == "" && strings.EqualFold(strings.TrimSpace(value), "auth-int") {
				auth.qop = "auth-int"
			}
		case "nc":
			auth.nc = value
		case "cnonce":
			auth.cnonce = value
		default:
			auth.other[key] = value
		}
		auth.Data[key] = value
	}
	return auth
}

func parseDigestParams(value string) map[string]string {
	value = strings.TrimSpace(value)
	if colon := strings.IndexByte(value, ':'); colon >= 0 {
		name := strings.ToLower(strings.TrimSpace(value[:colon]))
		if name == "authorization" || name == "proxy-authorization" || name == "www-authenticate" || name == "proxy-authenticate" {
			value = strings.TrimSpace(value[colon+1:])
		}
	}
	if index := strings.IndexByte(value, ' '); index >= 0 && strings.EqualFold(strings.TrimSpace(value[:index]), "Digest") {
		value = strings.TrimSpace(value[index+1:])
	}
	out := make(map[string]string)
	for len(value) > 0 {
		value = strings.TrimLeft(value, " \t,")
		if value == "" {
			break
		}
		eq := strings.IndexByte(value, '=')
		if eq <= 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(value[:eq]))
		value = strings.TrimSpace(value[eq+1:])
		if key == "" || value == "" {
			break
		}
		var parsed string
		if value[0] == '"' {
			value = value[1:]
			var builder strings.Builder
			escaped := false
			consumed := 0
			for consumed < len(value) {
				ch := value[consumed]
				consumed++
				if escaped {
					builder.WriteByte(ch)
					escaped = false
					continue
				}
				if ch == '\\' {
					escaped = true
					continue
				}
				if ch == '"' {
					break
				}
				builder.WriteByte(ch)
			}
			parsed = builder.String()
			value = value[consumed:]
		} else {
			end := strings.IndexByte(value, ',')
			if end < 0 {
				parsed = strings.TrimSpace(value)
				value = ""
			} else {
				parsed = strings.TrimSpace(value[:end])
				value = value[end+1:]
			}
		}
		out[key] = parsed
	}
	return out
}

// Get Get
func (auth *Authorization) Get(key string) string {
	return auth.Data[strings.ToLower(strings.TrimSpace(key))]
}

// Algorithm 返回挑战声明的摘要算法，缺省为 MD5。
func (auth *Authorization) Algorithm() string {
	if auth == nil || strings.TrimSpace(auth.algorithm) == "" {
		return "MD5"
	}
	return strings.TrimSpace(auth.algorithm)
}

// QOP 返回实际选择的 qop；当前仅实现 auth。
func (auth *Authorization) QOP() string {
	if auth == nil {
		return ""
	}
	return auth.qop
}

// SetUsername SetUsername
func (auth *Authorization) SetUsername(username string) *Authorization {
	auth.username = username
	auth.setData("username", username)

	return auth
}

// SetURI SetURI
func (auth *Authorization) SetURI(uri string) *Authorization {
	auth.uri = uri
	auth.setData("uri", uri)

	return auth
}

// SetMethod SetMethod
func (auth *Authorization) SetMethod(method string) *Authorization {
	auth.method = method

	return auth
}

// SetPassword SetPassword
func (auth *Authorization) SetPassword(password string) *Authorization {
	auth.password = password

	return auth
}

// SetClientNonce 设置客户端 Digest qop=auth 所需的 nonce-count 和 cnonce。
// 无 qop 的旧设备/平台不需要调用，保持 RFC 2069 兼容计算方式。
func (auth *Authorization) SetClientNonce(nc, cnonce string) *Authorization {
	auth.nc = nc
	auth.cnonce = cnonce
	auth.setData("nc", nc)
	auth.setData("cnonce", cnonce)
	return auth
}

func (auth *Authorization) setData(key, value string) {
	if auth == nil {
		return
	}
	if auth.Data == nil {
		auth.Data = make(map[string]string)
	}
	auth.Data[key] = value
}

// CalcResponse CalcResponse
func (auth *Authorization) CalcResponse() string {
	response, _ := auth.CalcResponseChecked()
	return response
}

// CalcResponseChecked 计算响应并拒绝未实现的算法或 qop，避免报文声明算法与实际计算不一致。
func (auth *Authorization) CalcResponseChecked() (string, error) {
	if auth == nil {
		return "", fmt.Errorf("nil Digest authorization")
	}
	response, err := CalcResponseWithAlgorithm(
		auth.Algorithm(),
		auth.username,
		auth.realm,
		auth.password,
		auth.method,
		auth.uri,
		auth.nonce,
		auth.qop,
		auth.cnonce,
		auth.nc,
	)
	if err != nil {
		return "", err
	}
	auth.response = response
	auth.setData("response", response)
	return auth.response, nil
}

func (auth *Authorization) String() string {
	if auth == nil {
		return "<nil>"
	}

	str := fmt.Sprintf(
		`Digest realm="%s",algorithm=%s,nonce="%s",username="%s",uri="%s",response="%s"`,
		auth.realm,
		auth.algorithm,
		auth.nonce,
		auth.username,
		auth.uri,
		auth.response,
	)
	if auth.qop == "auth" {
		str += fmt.Sprintf(`,qop=%s,nc=%s,cnonce="%s"`, auth.qop, auth.nc, auth.cnonce)
	}
	if opaque := auth.Get("opaque"); opaque != "" {
		str += fmt.Sprintf(`,opaque="%s"`, opaque)
	}

	return str
}

// CalcResponse 保留历史 MD5 API。
func CalcResponse(username, realm, password, method, uri, nonce, qop, cnonce, nc string) string {
	response, _ := CalcResponseWithAlgorithm("MD5", username, realm, password, method, uri, nonce, qop, cnonce, nc)
	return response
}

// CalcResponseWithAlgorithm 按挑战声明的算法计算 RFC Digest response。
// GB/T 28181 常见低安全级别注册使用 MD5；同时兼容部分平台的 SHA-1/SHA-256 挑战。
func CalcResponseWithAlgorithm(algorithm, username, realm, password, method, uri, nonce, qop, cnonce, nc string) (string, error) {
	newHash, err := digestHashFactory(algorithm)
	if err != nil {
		return "", err
	}
	qop = strings.ToLower(strings.TrimSpace(qop))
	if qop != "" && qop != "auth" {
		return "", fmt.Errorf("unsupported Digest qop %q", qop)
	}
	if qop == "auth" && (strings.TrimSpace(nc) == "" || strings.TrimSpace(cnonce) == "") {
		return "", fmt.Errorf("Digest qop=auth requires nc and cnonce")
	}
	digest := func(value string) string {
		encoder := newHash()
		_, _ = encoder.Write([]byte(value))
		return hex.EncodeToString(encoder.Sum(nil))
	}
	a1 := digest(username + ":" + realm + ":" + password)
	a2 := digest(method + ":" + uri)
	payload := a1 + ":" + nonce + ":"
	if qop != "" {
		payload += nc + ":" + cnonce + ":" + qop + ":"
	}
	return digest(payload + a2), nil
}

func digestHashFactory(algorithm string) (func() hash.Hash, error) {
	switch strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(algorithm), "_", "-")) {
	case "", "MD5":
		return md5.New, nil
	case "SHA-1", "SHA1":
		return sha1.New, nil
	case "SHA-256", "SHA256":
		return sha256.New, nil
	default:
		return nil, fmt.Errorf("unsupported Digest algorithm %q", algorithm)
	}
}
