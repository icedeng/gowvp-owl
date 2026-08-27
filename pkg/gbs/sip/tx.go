package sip

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const (
	transactionIdleTimeout       = 20 * time.Second
	serverTransactionIdleTimeout = 32 * time.Second
)

type transacionts struct {
	txs    map[string]*Transaction
	rwm    *sync.RWMutex
	closed bool
}

func (txs *transacionts) newTX(key string, conn Connection) *Transaction {
	txs.rwm.Lock()
	if existing := txs.txs[key]; existing != nil {
		txs.rwm.Unlock()
		if conn != nil {
			existing.setConnection(conn)
		}
		return existing
	}
	tx := newTransaction(key, conn, txs)
	txs.txs[key] = tx
	txs.rwm.Unlock()
	return tx
}

func (txs *transacionts) newServerTX(key string, conn Connection) *Transaction {
	txs.rwm.Lock()
	if existing := txs.txs[key]; existing != nil {
		txs.rwm.Unlock()
		return existing
	}
	tx := newTransaction(key, conn, txs)
	txs.txs[key] = tx
	txs.rwm.Unlock()
	return tx
}

func (txs *transacionts) newTXIfOpen(key string, conn Connection) *Transaction {
	if txs == nil {
		return nil
	}
	txs.rwm.Lock()
	defer txs.rwm.Unlock()
	if txs.closed {
		return nil
	}
	if existing := txs.txs[key]; existing != nil {
		if conn != nil {
			existing.setConnection(conn)
		}
		return existing
	}
	tx := newTransaction(key, conn, txs)
	txs.txs[key] = tx
	return tx
}

func (txs *transacionts) getTX(key string) *Transaction {
	txs.rwm.RLock()
	tx, ok := txs.txs[key]
	if !ok {
		tx = nil
	}
	txs.rwm.RUnlock()
	return tx
}

func (txs *transacionts) rmTX(tx *Transaction) {
	if txs == nil || tx == nil {
		return
	}
	txs.rwm.Lock()
	if txs.txs[tx.key] == tx {
		delete(txs.txs, tx.key)
	}
	txs.rwm.Unlock()
}

func (txs *transacionts) close() {
	if txs == nil {
		return
	}
	txs.rwm.Lock()
	txs.closed = true
	items := make([]*Transaction, 0, len(txs.txs))
	for _, tx := range txs.txs {
		items = append(items, tx)
	}
	txs.rwm.Unlock()
	for _, tx := range items {
		tx.Close()
	}
	for _, tx := range items {
		if tx != nil && tx.watchDone != nil {
			<-tx.watchDone
		}
	}
}

// Transaction Transaction
type Transaction struct {
	connMu            sync.RWMutex
	conn              Connection
	securityMu        sync.RWMutex
	security          MessageSecurity
	requestMu         sync.RWMutex
	request           *Request
	bindingMu         sync.RWMutex
	binding           *responseTransactionBinding
	serverMu          sync.RWMutex
	serverRequest     bool
	serverResponse    []byte
	serverDestination net.Addr
	key               string
	resp              chan *Response
	active            chan int
	done              chan struct{}
	owner             *transacionts
	closeOnce         sync.Once
	watchDone         chan struct{}
}

type responseTransactionBinding struct {
	protocolName    string
	protocolVersion string
	transport       string
	sentBy          string
	branch          string
	remoteEndpoint  string
}

func (tx *Transaction) setConnection(conn Connection) {
	if tx == nil || conn == nil {
		return
	}
	tx.connMu.Lock()
	tx.conn = conn
	tx.connMu.Unlock()
}

func (tx *Transaction) connection() Connection {
	if tx == nil {
		return nil
	}
	tx.connMu.RLock()
	conn := tx.conn
	tx.connMu.RUnlock()
	return conn
}

func (tx *Transaction) beginServerRequest() bool {
	if tx == nil {
		return false
	}
	tx.serverMu.Lock()
	first := !tx.serverRequest
	tx.serverRequest = true
	tx.serverMu.Unlock()
	tx.markActive(1)
	return first
}

func (tx *Transaction) idleTimeout() time.Duration {
	if tx == nil {
		return transactionIdleTimeout
	}
	tx.serverMu.RLock()
	serverRequest := tx.serverRequest
	tx.serverMu.RUnlock()
	if serverRequest {
		// RFC 3261 的服务端事务定时器 H/J 为 64*T1；默认 T1=500ms。
		return serverTransactionIdleTimeout
	}
	return transactionIdleTimeout
}

func (tx *Transaction) cacheServerResponse(payload []byte, destination net.Addr) {
	if tx == nil {
		return
	}
	tx.serverMu.Lock()
	cached := false
	if tx.serverRequest {
		tx.serverResponse = append(tx.serverResponse[:0], payload...)
		tx.serverDestination = destination
		cached = true
	}
	tx.serverMu.Unlock()
	if cached {
		tx.markActive(1)
	}
}

func (tx *Transaction) replayServerResponse() bool {
	if tx == nil {
		return false
	}
	tx.serverMu.RLock()
	payload := append([]byte(nil), tx.serverResponse...)
	destination := tx.serverDestination
	tx.serverMu.RUnlock()
	if len(payload) == 0 {
		return false
	}
	conn := tx.connection()
	if conn == nil {
		return false
	}
	logTraffic("out", conn.Network(), conn.LocalAddr(), destination, payload)
	if _, err := conn.WriteTo(payload, destination); err != nil {
		return false
	}
	tx.markActive(1)
	return true
}

// SetMessageSecurity 为事务的出站消息签名，并校验该事务收到的响应。
func (tx *Transaction) SetMessageSecurity(security MessageSecurity) {
	if tx == nil {
		return
	}
	tx.securityMu.Lock()
	tx.security = security
	tx.securityMu.Unlock()
}

func (tx *Transaction) messageSecurity() MessageSecurity {
	if tx == nil {
		return nil
	}
	tx.securityMu.RLock()
	security := tx.security
	tx.securityMu.RUnlock()
	return security
}

func (tx *Transaction) rememberRequest(request *Request) {
	if tx == nil || request == nil {
		return
	}
	clone, _ := request.Clone().(*Request)
	if clone == nil {
		return
	}
	tx.requestMu.Lock()
	if tx.request == nil {
		tx.request = clone
	}
	tx.requestMu.Unlock()
}

func (tx *Transaction) originalRequest() *Request {
	if tx == nil {
		return nil
	}
	tx.requestMu.RLock()
	request := tx.request
	tx.requestMu.RUnlock()
	if request == nil {
		return nil
	}
	clone, _ := request.Clone().(*Request)
	return clone
}

// CancelInvite 发送与原 INVITE 客户端事务绑定的 CANCEL，并返回独立 CANCEL 事务。
func (tx *Transaction) CancelInvite() (*Transaction, error) {
	invite := tx.originalRequest()
	if invite == nil || !strings.EqualFold(strings.TrimSpace(invite.Method()), MethodInvite) {
		return nil, nil
	}
	cancel, err := NewCancelRequestFromInviteChecked(invite)
	if err != nil {
		return nil, err
	}
	connection := tx.connection()
	if connection == nil {
		connection = cancel.GetConnection()
	}
	var cancelTX *Transaction
	if tx.owner != nil {
		cancelTX = tx.owner.newTXIfOpen(getTXKey(cancel), connection)
		if cancelTX == nil {
			return nil, fmt.Errorf("transaction store is closed")
		}
	} else {
		cancelTX = NewTransaction(getTXKey(cancel), connection)
	}
	cancelTX.SetMessageSecurity(tx.messageSecurity())
	if err := cancelTX.Request(cancel); err != nil {
		cancelTX.Close()
		return nil, err
	}
	return cancelTX, nil
}

func (tx *Transaction) bindResponse(request *Request) {
	if tx == nil || request == nil {
		return
	}
	via, ok := request.ViaHop()
	if !ok || via == nil {
		return
	}
	_, branch, branchCount := sipViaParam(via, "branch")
	if branchCount != 1 || branch == "" {
		return
	}
	binding := &responseTransactionBinding{
		protocolName: strings.TrimSpace(via.ProtocolName), protocolVersion: strings.TrimSpace(via.ProtocolVersion),
		transport: strings.TrimSpace(via.Transport), sentBy: strings.TrimSpace(via.SentBy()),
		branch: branch, remoteEndpoint: sipEndpointKey(request.Destination()),
	}
	tx.bindingMu.Lock()
	if tx.binding == nil {
		tx.binding = binding
	}
	tx.bindingMu.Unlock()
}

func (tx *Transaction) acceptsResponse(response *Response) bool {
	if tx == nil || response == nil {
		return false
	}
	tx.bindingMu.RLock()
	binding := tx.binding
	tx.bindingMu.RUnlock()
	// 兼容仅用于内部测试或入向请求应答的未绑定事务；正式出向请求均在发送时绑定。
	if binding == nil {
		return true
	}
	via, ok := response.ViaHop()
	if !ok || via == nil {
		return false
	}
	_, branch, branchCount := sipViaParam(via, "branch")
	return strings.EqualFold(binding.protocolName, strings.TrimSpace(via.ProtocolName)) &&
		strings.EqualFold(binding.protocolVersion, strings.TrimSpace(via.ProtocolVersion)) &&
		strings.EqualFold(binding.transport, strings.TrimSpace(via.Transport)) &&
		strings.EqualFold(binding.sentBy, strings.TrimSpace(via.SentBy())) &&
		branchCount == 1 && binding.branch != "" && binding.branch == branch &&
		(binding.remoteEndpoint == "" || binding.remoteEndpoint == sipEndpointKey(response.Source()))
}

func sipViaBranchValue(via *ViaHop) string {
	_, branch, count := sipViaParam(via, "branch")
	if count != 1 {
		return ""
	}
	return branch
}

func sipViaParam(via *ViaHop, name string) (key, value string, count int) {
	if via == nil || via.Params == nil {
		return "", "", 0
	}
	for _, candidate := range via.Params.Keys() {
		if !strings.EqualFold(strings.TrimSpace(candidate), name) {
			continue
		}
		count++
		if count == 1 {
			key = candidate
			if parameter, ok := via.Params.Get(candidate); ok && parameter != nil {
				value = strings.TrimSpace(parameter.String())
			}
		}
	}
	return key, value, count
}

func sipEndpointKey(address net.Addr) string {
	if address == nil {
		return ""
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(address.String()))
	if err != nil {
		return strings.ToLower(strings.TrimSpace(address.String()))
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else {
		host = strings.ToLower(host)
	}
	return net.JoinHostPort(host, port)
}

// NewTransaction NewTransaction
func NewTransaction(key string, conn Connection) *Transaction {
	return newTransaction(key, conn, nil)
}

func newTransaction(key string, conn Connection, owner *transacionts) *Transaction {
	// logrus.Traceln("new tx", key, time.Now().Format("2006-01-02 15:04:05"))
	tx := &Transaction{
		conn: conn, key: key, resp: make(chan *Response, 10), active: make(chan int, 1),
		done: make(chan struct{}), owner: owner,
		watchDone: make(chan struct{}),
	}
	go tx.watch()
	return tx
}

// Key Key
func (tx *Transaction) Key() string {
	return tx.key
}

func (tx *Transaction) watch() {
	defer close(tx.watchDone)
	timer := time.NewTimer(tx.idleTimeout())
	defer timer.Stop()
	for {
		select {
		case <-tx.done:
			return
		case <-tx.active:
			// logrus.Traceln("active tx", tx.Key(), time.Now().Format("2006-01-02 15:04:05"))
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(tx.idleTimeout())
		case <-timer.C:
			tx.Close()
			// logrus.Traceln("watch closed tx", tx.key, time.Now().Format("2006-01-02 15:04:05"))
			return
		}
	}
}

// GetResponse GetResponse
func (tx *Transaction) GetResponse() *Response {
	response, _ := tx.GetResponseContext(context.Background())
	return response
}

// GetResponseContext 等待最终响应，并允许调用方在超时或取消时立即退出。
// 1xx 临时响应会被消费，直到收到最终响应、事务关闭或 context 结束。
func (tx *Transaction) GetResponseContext(ctx context.Context) (*Response, error) {
	if tx == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tx.done:
			return nil, nil
		case res, ok := <-tx.resp:
			if !ok || res == nil {
				return nil, nil
			}
			tx.markActive(2)
			// logrus.Traceln("response tx", tx.key, time.Now().Format("2006-01-02 15:04:05"))
			if res.StatusCode() >= 100 && res.StatusCode() < 200 {
				// 所有 1xx 均为临时响应，等待最终响应。
				continue
			}
			return res, nil
		}
	}
}

func (tx *Transaction) markActive(value int) {
	if tx == nil || tx.active == nil {
		return
	}
	select {
	case <-tx.done:
		return
	default:
	}
	select {
	case <-tx.done:
	case tx.active <- value:
	default:
	}
}

// Close Close
func (tx *Transaction) Close() {
	if tx == nil {
		return
	}
	tx.closeOnce.Do(func() {
		// logrus.Traceln("closed tx", tx.key, time.Now().Format("2006-01-02 15:04:05"))
		if tx.owner != nil {
			tx.owner.rmTX(tx)
		}
		if tx.done != nil {
			close(tx.done)
		}
	})
}

// Response Response
func (tx *Transaction) receiveResponse(msg *Response) {
	defer func() {
		if r := recover(); r != nil {
			// logrus.Errorln("send to closed channel, txkey:", tx.key, "message: \n", msg.String())
		}
	}()
	if security := tx.messageSecurity(); security != nil {
		if err := security.Verify(msg); err != nil {
			slog.Warn("discard SIP response with invalid signal Digest", "tx_key", tx.key, "err", err)
			return
		}
	}
	if msg.StatusCode() >= 300 && msg.StatusCode() <= 699 {
		if invite := tx.originalRequest(); invite != nil && strings.EqualFold(strings.TrimSpace(invite.Method()), MethodInvite) {
			ack, err := NewAckRequestForNon2xxResponseChecked(invite, msg)
			if err != nil {
				slog.Warn("cannot acknowledge non-2xx INVITE response", "tx_key", tx.key, "err", err)
			} else if err := tx.Request(ack); err != nil {
				slog.Warn("send non-2xx INVITE ACK failed", "tx_key", tx.key, "err", err)
			}
		}
	}
	// logrus.Traceln("receiveResponse tx", tx.Key(), time.Now().Format("2006-01-02 15:04:05"))
	select {
	case <-tx.done:
		return
	case tx.resp <- msg:
	}
	tx.markActive(1)
}

// Respond Respond
func (tx *Transaction) Respond(res *Response) error {
	if res == nil {
		return NewError(nil, "SIP response is nil")
	}
	// 缓存响应可能被多个重传事务复用；发送前克隆，避免签名过程改写共享对象。
	outbound, _ := res.Clone().(*Response)
	if outbound == nil {
		return NewError(nil, "clone SIP response failed")
	}
	// logrus.Traceln("send response,to:", outbound.dest.String(), "txkey:", tx.key, "message: \n", outbound.String())
	conn := tx.connection()
	if conn == nil {
		return NewError(nil, "transaction connection is unavailable")
	}
	if security := tx.messageSecurity(); security != nil {
		if err := security.Sign(outbound); err != nil {
			return NewError(err, "sign SIP response failed")
		}
	}
	payload := []byte(outbound.String())
	tx.cacheServerResponse(payload, outbound.dest)
	logTraffic("out", conn.Network(), conn.LocalAddr(), outbound.dest, payload)
	_, err := conn.WriteTo(payload, outbound.dest)
	return err
}

// Request Request
func (tx *Transaction) Request(req *Request) error {
	conn := tx.connection()
	if conn == nil {
		return NewError(nil, "transaction connection is unavailable")
	}
	tx.bindResponse(req)
	if security := tx.messageSecurity(); security != nil {
		if err := security.Sign(req); err != nil {
			return NewError(err, "sign SIP request failed")
		}
	}
	str := req.String()
	s := unsafe.Slice(unsafe.StringData(str), len(str))
	// 必须在写出前保存事务快照；TCP 对端可能在 WriteTo 返回前就回送最终响应。
	tx.rememberRequest(req)
	logTraffic("out", conn.Network(), conn.LocalAddr(), req.dest, s)
	// logrus.Traceln("send request,to:", req.dest.String(), "txkey:", tx.key, "message: \n", req.String())
	_, err := conn.WriteTo(s, req.dest)
	return err
}

func getServerTXKey(msg *Request) string {
	base := getTXKey(msg)
	via, ok := msg.ViaHop()
	if !ok || via == nil {
		return "server:" + transactionKeyPart(base)
	}
	return "server:" + transactionKeyPart(base) +
		transactionKeyPart(strings.ToUpper(strings.TrimSpace(via.Transport))) +
		transactionKeyPart(strings.ToLower(strings.TrimSpace(via.SentBy()))) +
		transactionKeyPart(sipViaBranchValue(via))
}

func transactionKeyPart(value string) string {
	return strconv.Itoa(len(value)) + ":" + value
}

func getTXKey(msg Message) (key string) {
	callid, ok := msg.CallID()
	if ok {
		key = callid.String()
		if cseq, exists := msg.CSeq(); exists && cseq != nil {
			key = strconv.Itoa(len(key)) + ":" + key + ":" + strconv.FormatUint(uint64(cseq.SeqNo), 10) + ":" + strings.ToUpper(strings.TrimSpace(cseq.MethodName))
		}
		return key
	}
	return RandString(10)
}
