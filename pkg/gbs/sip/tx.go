package sip

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const transactionIdleTimeout = 20 * time.Second

type transacionts struct {
	txs map[string]*Transaction
	rwm *sync.RWMutex
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
	txs.rwm.RLock()
	items := make([]*Transaction, 0, len(txs.txs))
	for _, tx := range txs.txs {
		items = append(items, tx)
	}
	txs.rwm.RUnlock()
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
	connMu     sync.RWMutex
	conn       Connection
	securityMu sync.RWMutex
	security   MessageSecurity
	key        string
	resp       chan *Response
	active     chan int
	done       chan struct{}
	owner      *transacionts
	closeOnce  sync.Once
	watchDone  chan struct{}
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
	timer := time.NewTimer(transactionIdleTimeout)
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
			timer.Reset(transactionIdleTimeout)
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
			if res.StatusCode() == http.StatusContinue || res.statusCode == http.StatusSwitchingProtocols {
				// Trying and Dialog Establishement 等待下一个返回
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
	if security := tx.messageSecurity(); security != nil {
		if err := security.Sign(req); err != nil {
			return NewError(err, "sign SIP request failed")
		}
	}
	str := req.String()
	s := unsafe.Slice(unsafe.StringData(str), len(str))
	logTraffic("out", conn.Network(), conn.LocalAddr(), req.dest, s)
	// logrus.Traceln("send request,to:", req.dest.String(), "txkey:", tx.key, "message: \n", req.String())
	_, err := conn.WriteTo(s, req.dest)
	return err
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
