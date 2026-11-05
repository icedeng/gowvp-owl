package openapi

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	op "github.com/gowvp/owl/internal/core/openplatform"
)

var uriAttribute = regexp.MustCompile(`URI="([^"]+)"`)
var mediaClient = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 15 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (a *API) media(c *gin.Context) {
	kind, id := c.Param("kind"), c.Param("id")
	if !a.S.Limit("media-ip:"+a.IP(c), 100, 200) {
		respond(c, nil, op.Fail(429, "RATE_LIMITED", "媒体请求过于频繁"))
		return
	}
	raw := c.Query("ticket")
	if raw == "" {
		raw = strings.TrimPrefix(c.GetHeader("Authorization"), "Owl-Media ")
	}
	t, p, e := a.S.CheckTicket(raw, kind, id)
	if e != nil {
		a.S.Record(op.Audit{RequestID: c.GetString("open_request_id"), IP: a.IP(c), Route: c.FullPath(), Action: "media.denied", Result: op.AsError(e).Code, Status: op.AsError(e).Status})
		respond(c, nil, e)
		return
	}
	if !a.S.Limit("media:"+p.App.ID, 200, 400) {
		respond(c, nil, op.Fail(429, "RATE_LIMITED", "媒体请求超过配额"))
		return
	}
	release, e := a.S.AcquireConnection(p, kind)
	if e != nil {
		respond(c, nil, e)
		return
	}
	defer release()
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	stop := make(chan struct{})
	watcherDone := make(chan struct{})
	// 返回前等待撤权监视退出，避免异步访问已被 Gin 回收的 Context/Writer。
	defer func() { close(stop); <-watcherDone }()
	var writer http.ResponseWriter = c.Writer
	for {
		unwrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		writer = unwrapper.Unwrap()
	}
	controller := http.NewResponseController(writer)
	// 短期票据控制新连接入场；已建立连接按应用/会话当前状态撤销。
	go func() {
		defer close(watcherDone)
		defer func() {
			if ctx.Err() != nil {
				_ = controller.SetWriteDeadline(time.Now())
			}
		}()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if a.S.CheckActive(t) != nil {
					cancel()
					return
				}
			}
		}
	}()
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Time{})
	started := time.Now()
	defer func() {
		a.S.Record(op.Audit{RequestID: c.GetString("open_request_id"), AppID: t.AppID, CredentialID: t.CredentialID, Action: "media." + kind, ResourceID: id, Route: c.FullPath(), Status: c.Writer.Status(), DurationMS: time.Since(started).Milliseconds(), Result: "completed"})
	}()
	if kind != "live" && kind != "playback" {
		if c.Param("path") != "/content" {
			respond(c, nil, op.ErrNotFound)
			return
		}
		if strings.Contains(c.GetHeader("Range"), ",") {
			respond(c, nil, op.Fail(400, "INVALID_RANGE", "仅支持单段Range"))
			return
		}
		file, e := a.S.File(t)
		if e != nil {
			if op.AsError(e).Code == "SNAPSHOT_PENDING" {
				c.Header("Retry-After", "1")
				respond(c, nil, e)
			} else {
				respond(c, nil, op.ErrNotFound)
			}
			return
		}
		f, e := os.Open(file)
		if e != nil {
			respond(c, nil, op.ErrNotFound)
			return
		}
		defer f.Close()
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() {
			respond(c, nil, op.ErrNotFound)
			return
		}
		go func() {
			select {
			case <-ctx.Done():
				_ = f.Close()
			case <-stop:
			}
		}()
		if kind == "download" {
			c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"recording-%s.mp4\"", id))
		}
		http.ServeContent(c.Writer, c.Request.WithContext(ctx), info.Name(), info.ModTime(), &pacedFile{File: f, ctx: ctx, s: a.S, app: t.AppID})
		return
	}
	var session struct {
		Source   op.Source `gorm:"serializer:json;type:text"`
		Protocol string
	}
	table := "open_media_sessions"
	if kind == "playback" {
		table = "open_playback_sessions"
	}
	if e = a.S.DB.Table(table).First(&session, "id = ?", id).Error; e != nil {
		respond(c, nil, e)
		return
	}
	rel := strings.TrimPrefix(c.Param("path"), "/")
	if !op.Has([]string{".m3u8", ".ts", ".m4s", ".mp4", ".aac", ".key", ".flv"}, path.Ext(rel)) {
		respond(c, nil, op.ErrNotFound)
		return
	}
	if rel == "" || path.Clean(rel) != rel || strings.HasPrefix(rel, "/") || strings.ContainsAny(rel, "\\%\r\n") || strings.HasPrefix(rel, "../") {
		respond(c, nil, op.ErrNotFound)
		return
	}
	source := session.Source
	if source.BaseURL == "" || source.App == "" || source.Stream == "" {
		respond(c, nil, op.ErrUnavailable)
		return
	}
	upstream := source.BaseURL + "/" + url.PathEscape(source.App) + "/" + url.PathEscape(source.Stream) + "/" + rel
	if session.Protocol == "flv" {
		if rel != "stream.live.flv" {
			respond(c, nil, op.ErrNotFound)
			return
		}
		upstream = source.BaseURL + "/" + url.PathEscape(source.App) + "/" + url.PathEscape(source.Stream) + ".live.flv"
	} else if session.Protocol != "hls" {
		respond(c, nil, op.ErrNotFound)
		return
	}
	if source.Query != "" {
		upstream += "?" + source.Query
	}
	req, e := http.NewRequestWithContext(ctx, "GET", upstream, nil)
	if e != nil {
		respond(c, nil, op.ErrUnavailable)
		return
	}
	if v := c.GetHeader("Range"); v != "" {
		if strings.Contains(v, ",") {
			respond(c, nil, op.Fail(400, "INVALID_RANGE", "仅支持单段Range"))
			return
		}
		req.Header.Set("Range", v)
	}
	res, e := mediaClient.Do(req)
	if e != nil {
		respond(c, nil, op.ErrUnavailable)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 200 && res.StatusCode != 206 {
		respond(c, nil, op.Fail(503, "MEDIA_NOT_READY", "媒体尚未就绪或已中断"))
		return
	}
	if strings.HasSuffix(rel, ".m3u8") {
		b, e := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
		if e != nil || len(b) > 1<<20 {
			respond(c, nil, op.ErrUnavailable)
			return
		}
		out, e := rewriteHLS(b, rel, func(resource string) (string, error) {
			u, e := url.Parse(resource)
			if e != nil || u.IsAbs() || u.Host != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(resource, "\\%\r\n") {
				return "", op.ErrNotFound
			}
			base := &url.URL{Path: "/" + rel}
			resolved := base.ResolveReference(u)
			r := strings.TrimPrefix(resolved.Path, "/")
			if strings.HasPrefix(resource, "/") || strings.HasPrefix(r, "../") || r == "" {
				return "", op.ErrNotFound
			}
			return a.S.Config.PublicBaseURL + "/openmedia/v1/" + kind + "/" + id + "/" + r + "?ticket=" + url.QueryEscape(raw), nil
		})
		if e != nil {
			respond(c, nil, op.ErrUnavailable)
			return
		}
		c.Data(200, "application/vnd.apple.mpegurl", out)
		return
	}
	for _, header := range []string{"Content-Type", "Content-Range", "Accept-Ranges"} {
		if value := res.Header.Get(header); value != "" {
			c.Header(header, value)
		}
	}
	c.Status(res.StatusCode)
	buf := make([]byte, 32*1024)
	for {
		n, e := res.Body.Read(buf)
		if n > 0 {
			if e = a.S.WaitBytes(ctx, t.AppID, n); e != nil {
				return
			}
			if _, e = c.Writer.Write(buf[:n]); e != nil {
				return
			}
			c.Writer.Flush()
		}
		if e != nil {
			return
		}
	}
}
func rewriteHLS(b []byte, rel string, rewrite func(string) (string, error)) ([]byte, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(b), []byte("#EXTM3U")) {
		return nil, op.ErrUnavailable
	}
	scanner := bufio.NewScanner(bytes.NewReader(b))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var out strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			var rewriteErr error
			line = uriAttribute.ReplaceAllStringFunc(line, func(match string) string {
				u := uriAttribute.FindStringSubmatch(match)[1]
				v, e := rewrite(u)
				if e != nil {
					rewriteErr = e
					return match
				}
				return `URI="` + v + `"`
			})
			if rewriteErr != nil {
				return nil, rewriteErr
			}
		} else if strings.TrimSpace(line) != "" {
			var e error
			line, e = rewrite(strings.TrimSpace(line))
			if e != nil {
				return nil, e
			}
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return []byte(out.String()), scanner.Err()
}

type pacedFile struct {
	*os.File
	ctx context.Context
	s   *op.Service
	app string
}

func (p *pacedFile) Read(b []byte) (int, error) {
	if len(b) > 32*1024 {
		b = b[:32*1024]
	}
	if e := p.ctx.Err(); e != nil {
		return 0, e
	}
	n, e := p.File.Read(b)
	if n > 0 {
		if err := p.s.WaitBytes(p.ctx, p.app, n); err != nil {
			return 0, err
		}
	}
	return n, e
}
