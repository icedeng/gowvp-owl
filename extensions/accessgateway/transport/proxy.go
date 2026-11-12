package transport

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

// ConnectProxy 只从服务器私有配置读取，管理 API 只能保存引用，不能提交地址/密码。
type ConnectProxy struct {
	URL            string   `json:"url"`
	Username       string   `json:"username"`
	Password       string   `json:"password"`
	AllowedTargets []string `json:"allowed_targets"`
}

var proxyRefPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func LoadProxy(dir, ref, target string) (*ConnectProxy, error) {
	if !proxyRefPattern.MatchString(ref) {
		return nil, fmt.Errorf("无效代理引用")
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("代理目录必须为私有目录")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("代理配置目录不可用")
	}
	defer root.Close()
	info, err := root.Lstat(ref + ".json")
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("代理配置必须为私有普通文件")
	}
	// OpenRoot 防止越界，显式拒绝代理引用文件软链。
	file, err := root.Open(ref + ".json")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("代理配置读取期间发生变化")
	}
	data, err := io.ReadAll(io.LimitReader(file, 16<<10+1))
	if err != nil || len(data) > 16<<10 {
		return nil, fmt.Errorf("代理配置超出限制")
	}
	var p ConnectProxy
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err = d.Decode(&p); err != nil {
		return nil, fmt.Errorf("代理配置无效")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("代理配置包含额外内容")
	}
	u, err := url.Parse(p.URL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Hostname() == "" || u.Port() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("代理只允许明确端口的 HTTP CONNECT 地址")
	}
	port, portErr := strconv.Atoi(u.Port())
	if portErr != nil || port < 1 || port > 65535 || strings.Contains(p.Username, ":") {
		return nil, fmt.Errorf("代理端口或用户名无效")
	}
	if strings.ContainsAny(p.Username+p.Password, "\r\n") || len(p.Username)+len(p.Password) > 2048 {
		return nil, fmt.Errorf("代理凭据无效")
	}
	allowed := false
	for _, v := range p.AllowedTargets {
		if v == target {
			allowed = true
		}
	}
	if !allowed {
		return nil, protocol.Failure(protocol.Forbidden, "代理策略未授权此网关地址")
	}
	return &p, nil
}
func ProxyDirectory(credentials string) string { return filepath.Join(credentials, "proxies") }

// DialContext 将公网目标固定为校验过的 IP，再通过代理 CONNECT；TLS 仍校验原网关主机名。
func (p *ConnectProxy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("网关 DNS 解析失败")
	}
	for _, ip := range ips {
		if !protocol.ValidPublicIP(ip.Unmap().String()) {
			return nil, protocol.Failure(protocol.Forbidden, "代理目标包含非公网地址")
		}
	}
	u, _ := url.Parse(p.URL)
	conn, err := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("连接受控代理失败")
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	if d, has := ctx.Deadline(); has && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	target := net.JoinHostPort(ips[0].Unmap().String(), port)
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
	if p.Username != "" || p.Password != "" {
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(p.Username+":"+p.Password)))
	}
	if err = req.Write(conn); err != nil {
		return nil, fmt.Errorf("发送代理 CONNECT 失败")
	}
	reader := bufio.NewReader(io.LimitReader(conn, 16<<10))
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, fmt.Errorf("代理 CONNECT 响应无效")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("代理 CONNECT 被拒绝: HTTP %d", resp.StatusCode)
	}
	if reader.Buffered() != 0 {
		return nil, fmt.Errorf("代理返回了非预期隧道数据")
	}
	_ = conn.SetDeadline(time.Time{})
	ok = true
	return conn, nil
}
