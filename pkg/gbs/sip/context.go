package sip

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"strings"
)

const abortIndex int8 = math.MaxInt8 >> 1

type HandlerFunc func(*Context)

type Context struct {
	Request  *Request
	Tx       *Transaction
	handlers []HandlerFunc
	index    int8

	cache map[string]any

	DeviceID string
	Host     string
	Port     string

	Source net.Addr
	To     *Address
	From   *Address

	Log *slog.Logger

	svr *Server
	// XGBVer 保存附录 I 定义的有效版本号（1.0/1.1/2.0/3.0）。
	XGBVer string
	// XGBVerRaw 保留设备发送的原始版本值，便于诊断未知扩展版本。
	XGBVerRaw string
}

func newContext(req *Request, tx *Transaction) *Context {
	c := Context{
		Request: req,
		Tx:      tx,
		cache:   make(map[string]any),
		Log:     slog.Default(),
		index:   -1,
	}
	if err := c.parserRequest(); err != nil {
		slog.Error("parserRequest", "err", err)
	}
	return &c
}

func (c *Context) parserRequest() error {
	req := c.Request
	header, ok := req.From()
	if !ok {
		return fmt.Errorf("req from is nil")
	}

	if header.Address == nil {
		return fmt.Errorf("header address is nil")
	}
	user := header.Address.User()
	if user == nil {
		return fmt.Errorf("address user is nil")
	}
	c.DeviceID = user.String()
	c.Host = header.Address.Host()
	via, ok := req.ViaHop()
	if !ok {
		return fmt.Errorf("via is nil")
	}
	c.Host = via.Host
	c.Port = via.Port.String()

	c.Source = req.Source()
	c.To = NewAddressFromFromHeader(header)
	if c.To == nil {
		slog.Error(">>>>>>>> to is nil", "header", header)
	}

	c.XGBVerRaw = c.GetHeader("X-GB-Ver")
	switch c.XGBVerRaw {
	case "1.0", "1.1", "2.0", "3.0":
		c.XGBVer = c.XGBVerRaw
	}

	c.Log = slog.Default().With("deviceID", c.DeviceID, "host", c.Host)
	return nil
}

func (c *Context) Next() {
	c.index++
	for c.index < int8(len(c.handlers)) {
		if fn := c.handlers[c.index]; fn != nil {
			fn(c)
		}
		c.index++
	}
}

func (c *Context) GetHeader(key string) string {
	headers := c.Request.GetHeaders(key)
	if len(headers) > 0 {
		header := headers[0]
		splits := strings.SplitN(header.String(), ":", 2)
		if len(splits) == 2 {
			return strings.TrimSpace(splits[1])
		}
	}
	return ""
}

func (c *Context) Abort() {
	c.index = abortIndex
}

func (c *Context) AbortString(status int, msg string) {
	c.Abort()
	c.String(status, msg)
}

func (c *Context) String(status int, msg string) {
	_ = c.Tx.Respond(NewResponseFromRequest("", c.Request, status, msg, nil))
}

func (c *Context) Set(k string, v any) {
	c.cache[k] = v
}

func (c *Context) Get(k string) (any, bool) {
	v, ok := c.cache[k]
	return v, ok
}

func (c *Context) GetMustString(k string) string {
	if v, ok := c.cache[k]; ok {
		return v.(string)
	}
	return ""
}

func (c *Context) GetMustInt(k string) int {
	if v, ok := c.cache[k]; ok {
		return v.(int)
	}
	return 0
}

func (c *Context) SendRequest(method string, body []byte) (*Transaction, error) {
	hb := NewHeaderBuilder().SetTo(c.To).SetFrom(c.From).AddVia(&ViaHop{
		Params: NewParams().Add("branch", String{Str: GenerateBranch()}),
	}).SetContentType(&ContentTypeXML).SetMethod(method)
	if c.XGBVer != "" {
		hb.SetXGBVerValue(c.XGBVer)
	}

	req := NewRequest("", method, c.To.URI, DefaultSipVersion, hb.Build(), body)
	req.SetDestination(c.Source)
	req.SetConnection(c.Request.conn)
	return c.svr.Request(req)
}
