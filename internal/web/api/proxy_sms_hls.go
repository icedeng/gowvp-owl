package api

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/gowvp/owl/internal/core/sms"
)

const smsPlaylistLimit = 1 << 20

var smsHLSURI = regexp.MustCompile(`\bURI="([^"]*)"`)

// resourceURL 将同节点、同流的引用留在鉴权代理内，禁止向外部地址泄露播放凭据。
func (g smsPlayGrant) resourceURL(raw string, base *url.URL, driver string) (string, error) {
	ref, err := url.Parse(raw)
	if err != nil || raw == "" || ref.User != nil || ref.Fragment != "" {
		return "", fmt.Errorf("无效的媒体资源引用")
	}
	u := base.ResolveReference(ref)
	if u.Scheme != base.Scheme || u.Host != base.Host {
		return "", fmt.Errorf("媒体资源引用不属于授权节点")
	}
	q, err := smsPlayQuery(u.RawQuery)
	if err != nil {
		return "", err
	}
	q.Set("token", g.token)
	u.RawQuery = q.Encode()
	if err := g.authorize(http.MethodGet, u.Path, u.RawQuery, driver); err != nil {
		return "", err
	}
	return "/proxy/sms/" + sms.MediaServerProxyRoute(g.mediaServerID) + u.EscapedPath() + "?" + u.RawQuery, nil
}

func (g smsPlayGrant) rewriteResponse(r *http.Response, target *url.URL, upstreamPath, driver string) error {
	// 所有资源均需逐次鉴权，避免共享缓存返回其他持链接者的鉴权响应。
	r.Header.Set("Cache-Control", "private, no-store")
	base := *target
	base.Path = upstreamPath
	if r.StatusCode >= 300 && r.StatusCode < 400 && r.Header.Get("Location") != "" {
		location, err := g.resourceURL(r.Header.Get("Location"), &base, driver)
		if err != nil {
			return err
		}
		r.Header.Set("Location", location)
	}
	if !strings.HasSuffix(upstreamPath, ".m3u8") || r.StatusCode < 200 || r.StatusCode >= 300 {
		return nil
	}
	if r.Request.Method == http.MethodHead {
		r.Header.Del("Content-Length")
		r.ContentLength = -1
		return nil
	}
	if r.StatusCode != http.StatusOK {
		return fmt.Errorf("媒体播放清单响应不完整")
	}
	defer r.Body.Close()
	var reader io.Reader = r.Body
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return err
		}
		defer gz.Close()
		reader = gz
	default:
		return fmt.Errorf("不支持的媒体播放清单编码")
	}
	body, err := io.ReadAll(io.LimitReader(reader, smsPlaylistLimit+1))
	if err != nil {
		return err
	}
	if len(body) > smsPlaylistLimit || !bytes.HasPrefix(bytes.TrimSpace(body), []byte("#EXTM3U")) {
		return fmt.Errorf("无效或过大的媒体播放清单")
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), smsPlaylistLimit+1)
	var out strings.Builder
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			var rewriteErr error
			line = smsHLSURI.ReplaceAllStringFunc(line, func(match string) string {
				resource, err := g.resourceURL(smsHLSURI.FindStringSubmatch(match)[1], &base, driver)
				if err != nil {
					rewriteErr = err
					return match
				}
				return `URI="` + resource + `"`
			})
			if rewriteErr != nil {
				return rewriteErr
			}
		} else if line != "" {
			line, err = g.resourceURL(line, &base, driver)
			if err != nil {
				return err
			}
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	result := out.String()
	r.Body = io.NopCloser(strings.NewReader(result))
	r.ContentLength = int64(len(result))
	r.Header.Set("Content-Length", strconv.Itoa(len(result)))
	r.Header.Del("Content-Encoding")
	r.Header.Del("ETag")
	r.Header.Del("Last-Modified")
	r.Header.Del("Accept-Ranges")
	return nil
}
