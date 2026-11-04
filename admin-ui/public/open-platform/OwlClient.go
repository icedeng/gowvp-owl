// 服务端接入示例，无外部依赖；不在客户端内置SK。
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func digest(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func query(raw string) (string, error) {
	pairs := []string{}
	seen := map[string]bool{}
	encode := func(v string) string { return strings.ReplaceAll(url.QueryEscape(v), "+", "%20") }
	if raw == "" {
		return "", nil
	}
	for _, pair := range strings.Split(raw, "&") {
		k, v, _ := strings.Cut(pair, "=")
		k, e := url.PathUnescape(k)
		if e != nil || k == "" || !utf8.ValidString(k) || seen[k] {
			return "", fmt.Errorf("invalid query")
		}
		v, e = url.PathUnescape(v)
		if e != nil || !utf8.ValidString(v) {
			return "", fmt.Errorf("invalid query")
		}
		seen[k] = true
		pairs = append(pairs, encode(k)+"="+encode(v))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&"), nil
}
func canonical(audience, method, path, rawQuery, ct, ts, nonce, idem string, body []byte) (string, error) {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "%\\?#\r\n") {
		return "", fmt.Errorf("invalid path")
	}
	for _, s := range strings.Split(path, "/")[1:] {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("invalid path")
		}
	}
	q, e := query(rawQuery)
	if e != nil {
		return "", e
	}
	return strings.Join([]string{"OWL-HMAC-SHA256", audience, strings.ToUpper(method), path, q, ct, ts, nonce, idem, digest(body)}, "\n"), nil
}
func sign(sk, text string) (string, error) {
	key, e := base64.RawURLEncoding.DecodeString(sk)
	if e != nil || len(key) != 32 {
		return "", fmt.Errorf("invalid SK")
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(text))
	return hex.EncodeToString(m.Sum(nil)), nil
}
func signedRequest(base, ak, sk, audience, method, path, rawQuery, idem string, body []byte) (*http.Request, error) {
	if !strings.HasPrefix(base, "https://") && !(os.Getenv("OWL_ALLOW_HTTP") == "true" && strings.HasPrefix(base, "http://")) {
		return nil, fmt.Errorf("HTTP deployment requires OWL_ALLOW_HTTP=true")
	}
	ct := "application/json"
	if method == "GET" || method == "DELETE" {
		ct = ""
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	b := make([]byte, 24)
	if _, e := rand.Read(b); e != nil {
		return nil, e
	}
	nonce := base64.RawURLEncoding.EncodeToString(b)
	text, e := canonical(audience, method, path, rawQuery, ct, ts, nonce, idem, body)
	if e != nil {
		return nil, e
	}
	sig, e := sign(sk, text)
	if e != nil {
		return nil, e
	}
	target := strings.TrimRight(base, "/") + path
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req, e := http.NewRequest(method, target, bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "OWL-HMAC-SHA256 Credential="+ak+",Signature="+sig)
	req.Header.Set("X-Owl-Audience", audience)
	req.Header.Set("X-Owl-Timestamp", ts)
	req.Header.Set("X-Owl-Nonce", nonce)
	req.Header.Set("X-Owl-Content-SHA256", digest(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	return req, nil
}

// verifyCallback 只校验签名；接收方另须持久化nonce并按delivery_id去重和核对payload ID。
func verifyCallback(h http.Header, body []byte, sk string) bool {
	ts, e := strconv.ParseInt(h.Get("X-Owl-Timestamp"), 10, 64)
	if e != nil || ts < time.Now().Unix()-300 || ts > time.Now().Unix()+300 {
		return false
	}
	nonce, e := base64.RawURLEncoding.DecodeString(h.Get("X-Owl-Nonce"))
	if e != nil || len(nonce) < 16 || digest(body) != h.Get("X-Owl-Content-SHA256") {
		return false
	}
	text := strings.Join([]string{"OWL-CALLBACK-HMAC-SHA256", h.Get("X-Owl-Timestamp"), h.Get("X-Owl-Nonce"), h.Get("X-Owl-Event-ID"), h.Get("X-Owl-Delivery-ID"), digest(body)}, "\n")
	sig, e := sign(sk, text)
	return e == nil && hmac.Equal([]byte(h.Get("Authorization")), []byte("OWL-CALLBACK-HMAC-SHA256 Signature="+sig))
}
func main() {
	if len(os.Args) > 1 && os.Args[1] == "--vector" {
		key := make([]byte, 32)
		for i := range key {
			key[i] = byte(i)
		}
		text, _ := canonical("owl-test", "POST", "/openapi/v1/snapshot-tasks", "q=A+B%20C&empty=&label=%E5%91%8A%E8%AD%A6", "application/json", "1700000000", "AAECAwQFBgcICQoLDA0ODw", "capture-001", []byte(`{"channel_id":"c1"}`))
		sig, _ := sign(base64.RawURLEncoding.EncodeToString(key), text)
		fmt.Println(sig)
		return
	}
	req, e := signedRequest(os.Getenv("OWL_API_BASE"), os.Getenv("OWL_AK"), os.Getenv("OWL_SK"), os.Getenv("OWL_AUDIENCE"), "GET", "/openapi/v1/devices", "", "", nil)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, e := client.Do(req)
	if e != nil {
		fmt.Fprintln(os.Stderr, "请求失败")
		os.Exit(1)
	}
	defer resp.Body.Close()
	fmt.Println("HTTP", resp.StatusCode)
	io.Copy(os.Stdout, io.LimitReader(resp.Body, 1<<20))
}
