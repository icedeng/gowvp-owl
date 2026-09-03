package sip

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// RFC 3261 的客户端事务定时器 B/F 为 64*T1；默认 T1=500ms。
	transactionIdleTimeout       = 32 * time.Second
	serverTransactionIdleTimeout = 32 * time.Second
	// 最长保留一个初始事务窗口和一个最终响应完成窗口；活动/重传不得无限续期。
	transactionMaxLifetime  = transactionIdleTimeout + serverTransactionIdleTimeout
	transactionWriteTimeout = 5 * time.Second
	// GB/T 28181 的单设备呼叫通常不发生大规模分叉；限制异常 To-tag 洪泛造成的状态与 goroutine 增长。
	maxInvite2xxDialogsPerTransaction = 32
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
	connMu                     sync.RWMutex
	conn                       Connection
	ownedConnMu                sync.Mutex
	ownedConn                  Connection
	ownedWritePending          bool
	ownedFinalPending          bool
	securityMu                 sync.RWMutex
	security                   MessageSecurity
	requestMu                  sync.RWMutex
	request                    *Request
	inviteACKMu                sync.RWMutex
	inviteACKs                 map[string]*cachedInvite2xxACK
	inviteSelectedDialog       string
	invite2xxDelivered         bool
	invitePending2xx           map[string]*Response
	inviteCleanupScheduled     map[string]struct{}
	invite2xxDialogs           map[string]struct{}
	inviteDialogLimitLogged    atomic.Bool
	bindingMu                  sync.RWMutex
	binding                    *responseTransactionBinding
	serverWriteMu              sync.Mutex
	serverMu                   sync.RWMutex
	serverRequest              bool
	serverResponse             []byte
	serverResponseFinal        bool
	serverFinalAttempted       bool
	serverHandlerActive        bool
	serverRetryPending         bool
	serverDestination          net.Addr
	key                        string
	resp                       chan *Response
	active                     chan int
	done                       chan struct{}
	owner                      *transacionts
	closeOnce                  sync.Once
	watchDone                  chan struct{}
	closeOnFinal               atomic.Bool
	detachedCancel             atomic.Bool
	inviteNon2xxFinalDelivered atomic.Bool
	inviteCleanupMu            sync.Mutex
	inviteCleanupBYE           map[string]struct{}
	backgroundMu               sync.Mutex
	backgroundClosed           bool
	backgroundWG               sync.WaitGroup
}

type cachedInvite2xxACK struct {
	dialogKey   string
	payload     []byte
	destination net.Addr
}

type contextWriteToConnection interface {
	writeToContext(context.Context, []byte, net.Addr) (int, error)
}

type responseTransactionBinding struct {
	protocolName    string
	protocolVersion string
	transport       string
	sentBy          string
	branch          string
	fromTag         string
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

func (tx *Transaction) ownConnection(conn Connection) {
	if tx == nil || conn == nil {
		return
	}
	tx.ownedConnMu.Lock()
	previous := tx.ownedConn
	tx.ownedConn = conn
	tx.ownedWritePending = true
	tx.ownedFinalPending = false
	tx.ownedConnMu.Unlock()
	if previous != nil && previous != conn {
		_ = previous.Close()
	}
}

func (tx *Transaction) finishOwnedConnectionWrite() {
	if tx == nil {
		return
	}
	tx.ownedConnMu.Lock()
	tx.ownedWritePending = false
	var conn Connection
	if tx.ownedFinalPending {
		conn = tx.ownedConn
		tx.ownedConn = nil
		tx.ownedFinalPending = false
	}
	tx.ownedConnMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (tx *Transaction) releaseOwnedConnectionAfterFinalResponse() {
	if tx == nil {
		return
	}
	tx.ownedConnMu.Lock()
	if tx.ownedWritePending {
		tx.ownedFinalPending = true
		tx.ownedConnMu.Unlock()
		return
	}
	conn := tx.ownedConn
	tx.ownedConn = nil
	tx.ownedFinalPending = false
	tx.ownedConnMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (tx *Transaction) releaseOwnedConnection() {
	if tx == nil {
		return
	}
	tx.ownedConnMu.Lock()
	conn := tx.ownedConn
	tx.ownedConn = nil
	tx.ownedWritePending = false
	tx.ownedFinalPending = false
	tx.ownedConnMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func transactionWriteContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), transactionWriteTimeout)
}

func writeConnectionContext(ctx context.Context, conn Connection, payload []byte, destination net.Addr) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if writer, ok := conn.(contextWriteToConnection); ok {
		return writer.writeToContext(ctx, payload, destination)
	}
	return conn.WriteTo(payload, destination)
}

func (tx *Transaction) beginServerRequest() bool {
	if tx == nil {
		return false
	}
	tx.serverMu.Lock()
	first := !tx.serverRequest
	tx.serverRequest = true
	if first {
		tx.serverFinalAttempted = false
	}
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

func (tx *Transaction) cacheServerResponse(payload []byte, destination net.Addr, final bool) {
	if tx == nil {
		return
	}
	tx.serverMu.Lock()
	cached := false
	if tx.serverRequest && !tx.serverResponseFinal {
		tx.serverResponse = append(tx.serverResponse[:0], payload...)
		tx.serverDestination = destination
		tx.serverResponseFinal = final
		if final {
			tx.serverRetryPending = false
		}
		cached = true
	}
	tx.serverMu.Unlock()
	if cached {
		tx.markActive(1)
	}
}

func (tx *Transaction) allowServerRequestRetry() {
	if tx == nil {
		return
	}
	tx.serverMu.Lock()
	// 临时响应不提交事务终态；后续最终响应失败时必须清除临时缓存并允许业务重入。
	// 已成功写出的最终响应继续由事务层稳定重放，避免重复业务副作用。
	if tx.serverRequest && !tx.serverResponseFinal {
		if tx.serverHandlerActive {
			// 当前 handler 可能仍在响应失败后执行清理或尝试其他终态；在它真正退出前，
			// 同事务重传不能启动并发业务代次，也不能重置本代次的最终响应尝试标记。
			tx.serverRetryPending = true
		} else {
			tx.releaseServerRequestLocked()
		}
	}
	tx.serverMu.Unlock()
}

func (tx *Transaction) releaseServerRequestLocked() {
	tx.serverRequest = false
	tx.serverResponse = nil
	tx.serverDestination = nil
	tx.serverResponseFinal = false
	tx.serverRetryPending = false
}

func (tx *Transaction) beginServerHandler() {
	if tx == nil {
		return
	}
	tx.serverMu.Lock()
	if tx.serverRequest && !tx.serverResponseFinal {
		tx.serverHandlerActive = true
	}
	tx.serverMu.Unlock()
}

func (tx *Transaction) completeServerHandler() {
	if tx == nil {
		return
	}
	tx.serverMu.Lock()
	tx.serverHandlerActive = false
	if tx.serverRetryPending && !tx.serverResponseFinal {
		tx.releaseServerRequestLocked()
	}
	tx.serverMu.Unlock()
}

func (tx *Transaction) hasServerFinalResponse() bool {
	if tx == nil {
		return false
	}
	tx.serverMu.RLock()
	cached := tx.serverResponseFinal
	tx.serverMu.RUnlock()
	return cached
}

func (tx *Transaction) markServerFinalAttempted() {
	if tx == nil {
		return
	}
	tx.serverMu.Lock()
	tx.serverFinalAttempted = true
	tx.serverMu.Unlock()
}

func (tx *Transaction) hasServerFinalAttempt() bool {
	if tx == nil {
		return false
	}
	tx.serverMu.RLock()
	attempted := tx.serverFinalAttempted
	tx.serverMu.RUnlock()
	return attempted
}

func (tx *Transaction) replayServerResponseForRequest(request *Request) bool {
	if tx == nil || request == nil {
		return false
	}
	tx.serverMu.RLock()
	payload := append([]byte(nil), tx.serverResponse...)
	cachedDestination := tx.serverDestination
	tx.serverMu.RUnlock()
	if len(payload) == 0 {
		return false
	}
	original := tx.connection()
	incoming := request.GetConnection()
	if incoming == nil {
		return false
	}
	destination := cachedDestination
	if incoming != original {
		incomingSource := request.Source()
		if incomingSource == nil {
			incomingSource = incoming.RemoteAddr()
		}
		if !sameServerRetransmissionPeer(original, incoming, cachedDestination, incomingSource) {
			return false
		}
		// TCP/TLS 断线重连后，响应必须写向承载本次合法重传的新流连接。
		destination = incomingSource
	}
	logTraffic("out", incoming.Network(), incoming.LocalAddr(), destination, payload)
	ctx, cancel := transactionWriteContext()
	defer cancel()
	if _, err := writeConnectionContext(ctx, incoming, payload, destination); err != nil {
		return false
	}
	tx.markActive(1)
	return true
}

// rebindServerRequestForRequest transfers an in-flight stream transaction to a
// reconnecting peer only after the transport and source host have been proven
// to match the original connection.  This keeps the response for a handler
// that is still running on the new TCP/TLS stream without allowing an
// unrelated connection to replace transaction ownership.
func (tx *Transaction) rebindServerRequestForRequest(request *Request) bool {
	if tx == nil || request == nil {
		return false
	}
	incoming := request.GetConnection()
	if incoming == nil {
		return false
	}
	original := tx.connection()
	if incoming == original {
		return true
	}
	incomingSource := request.Source()
	if incomingSource == nil {
		incomingSource = incoming.RemoteAddr()
	}
	tx.serverMu.RLock()
	eligible := tx.serverRequest && !tx.serverResponseFinal && tx.serverHandlerActive
	originalDestination := tx.serverDestination
	tx.serverMu.RUnlock()
	if !eligible || !sameServerRetransmissionPeer(original, incoming, originalDestination, incomingSource) {
		return false
	}
	tx.setConnection(incoming)
	return true
}

func sameServerRetransmissionPeer(original, incoming Connection, originalDestination, incomingSource net.Addr) bool {
	if original == nil || incoming == nil {
		return false
	}
	originalTransport := SignalingTransport(original)
	if originalTransport != SignalingTransport(incoming) || (originalTransport != "TCP" && originalTransport != "TLS") {
		return false
	}
	if originalDestination == nil {
		originalDestination = original.RemoteAddr()
	}
	return networkAddressHost(originalDestination) != "" && networkAddressHost(originalDestination) == networkAddressHost(incomingSource)
}

func networkAddressHost(address net.Addr) string {
	if address == nil {
		return ""
	}
	raw := strings.TrimSpace(address.String())
	host, _, err := net.SplitHostPort(raw)
	if err != nil {
		host = raw
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return strings.ToLower(host)
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

func (tx *Transaction) rememberRequest(request *Request) error {
	if tx == nil || request == nil {
		return nil
	}
	clone, err := cloneSIPRequest(request)
	if err != nil {
		return err
	}
	tx.requestMu.Lock()
	if tx.request == nil {
		tx.request = clone
	}
	tx.requestMu.Unlock()
	return nil
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
	clone, err := cloneSIPRequest(request)
	if err != nil {
		return nil
	}
	return clone
}

func invite2xxDialogKey(message Message) string {
	if message == nil {
		return ""
	}
	callID, callIDOK := message.CallID()
	cseq, cseqOK := message.CSeq()
	to, toOK := message.To()
	if !callIDOK || callID == nil || !cseqOK || cseq == nil || !toOK || to == nil || to.Params == nil {
		return ""
	}
	tag := dialogHeaderParam(to.Params, "tag")
	if strings.TrimSpace(string(*callID)) == "" || tag == "" {
		return ""
	}
	return string(*callID) + "\x00" + strconv.FormatUint(uint64(cseq.SeqNo), 10) + "\x00" + tag
}

func (tx *Transaction) rememberInvite2xxACK(request *Request, payload []byte) {
	if tx == nil || request == nil || !strings.EqualFold(strings.TrimSpace(request.Method()), MethodACK) {
		return
	}
	invite := tx.originalRequest()
	if invite == nil {
		return
	}
	inviteCSeq, inviteCSeqOK := invite.CSeq()
	ackCSeq, ackCSeqOK := request.CSeq()
	inviteCallID, inviteCallIDOK := invite.CallID()
	ackCallID, ackCallIDOK := request.CallID()
	inviteVia, inviteViaOK := invite.ViaHop()
	ackVia, ackViaOK := request.ViaHop()
	if !inviteCSeqOK || inviteCSeq == nil || !ackCSeqOK || ackCSeq == nil ||
		!inviteCallIDOK || inviteCallID == nil || !ackCallIDOK || ackCallID == nil ||
		!inviteViaOK || inviteVia == nil || !ackViaOK || ackVia == nil ||
		!strings.EqualFold(strings.TrimSpace(invite.Method()), MethodInvite) ||
		!strings.EqualFold(strings.TrimSpace(ackCSeq.MethodName), MethodACK) ||
		inviteCSeq.SeqNo != ackCSeq.SeqNo || string(*inviteCallID) != string(*ackCallID) ||
		sipViaBranchValue(inviteVia) == "" || sipViaBranchValue(inviteVia) == sipViaBranchValue(ackVia) {
		return
	}
	dialogKey := invite2xxDialogKey(request)
	if dialogKey == "" {
		return
	}
	tx.inviteACKMu.Lock()
	if !tx.admitInvite2xxDialogLocked(dialogKey) {
		tx.inviteACKMu.Unlock()
		tx.logInvite2xxDialogLimit()
		return
	}
	if tx.inviteACKs == nil {
		tx.inviteACKs = make(map[string]*cachedInvite2xxACK)
	}
	tx.inviteACKs[dialogKey] = &cachedInvite2xxACK{
		dialogKey: dialogKey, payload: append([]byte(nil), payload...), destination: request.Destination(),
	}
	pending := make([]*Response, 0, len(tx.invitePending2xx))
	if tx.inviteSelectedDialog == "" {
		tx.inviteSelectedDialog = dialogKey
		for pendingDialog, response := range tx.invitePending2xx {
			if pendingDialog != dialogKey && response != nil {
				pending = append(pending, response)
			}
		}
		tx.invitePending2xx = nil
	}
	tx.inviteACKMu.Unlock()
	for _, response := range pending {
		tx.scheduleInvite2xxCleanup(response)
	}
}

func (tx *Transaction) retransmitInvite2xxACK(response *Response) bool {
	if tx == nil || response == nil || response.StatusCode() < 200 || response.StatusCode() >= 300 {
		return false
	}
	cseq, ok := response.CSeq()
	if !ok || cseq == nil || !strings.EqualFold(strings.TrimSpace(cseq.MethodName), MethodInvite) {
		return false
	}
	dialogKey := invite2xxDialogKey(response)
	tx.inviteACKMu.RLock()
	cached := tx.inviteACKs[dialogKey]
	if cached == nil {
		tx.inviteACKMu.RUnlock()
		return false
	}
	payload := append([]byte(nil), cached.payload...)
	destination := cached.destination
	tx.inviteACKMu.RUnlock()
	conn := tx.connection()
	if conn == nil {
		slog.Warn("cannot retransmit 2xx INVITE ACK without connection", "tx_key", tx.key)
		return true
	}
	logTraffic("out", conn.Network(), conn.LocalAddr(), destination, payload)
	ctx, cancel := transactionWriteContext()
	defer cancel()
	if _, err := writeConnectionContext(ctx, conn, payload, destination); err != nil {
		slog.Warn("retransmit 2xx INVITE ACK failed", "tx_key", tx.key, "err", err)
	}
	return true
}

func (tx *Transaction) handleForkedInvite2xx(response *Response) bool {
	if tx == nil || response == nil || response.StatusCode() < 200 || response.StatusCode() >= 300 {
		return false
	}
	cseq, ok := response.CSeq()
	if !ok || cseq == nil || !strings.EqualFold(strings.TrimSpace(cseq.MethodName), MethodInvite) {
		return false
	}
	dialogKey := invite2xxDialogKey(response)
	if dialogKey == "" {
		return false
	}

	tx.inviteACKMu.Lock()
	if !tx.admitInvite2xxDialogLocked(dialogKey) {
		tx.inviteACKMu.Unlock()
		tx.logInvite2xxDialogLimit()
		return true
	}
	selectedDialog := tx.inviteSelectedDialog
	if selectedDialog != "" {
		tx.inviteACKMu.Unlock()
		if selectedDialog == dialogKey {
			return false
		}
		tx.scheduleInvite2xxCleanup(response)
		return true
	}
	if !tx.invite2xxDelivered {
		tx.invite2xxDelivered = true
		tx.inviteACKMu.Unlock()
		return false
	}
	if tx.invitePending2xx == nil {
		tx.invitePending2xx = make(map[string]*Response)
	}
	if _, exists := tx.invitePending2xx[dialogKey]; !exists {
		clone, err := cloneOutboundResponse(response)
		if err == nil {
			tx.invitePending2xx[dialogKey] = clone
		}
	}
	tx.inviteACKMu.Unlock()
	return true
}

func (tx *Transaction) admitInvite2xxDialogLocked(dialogKey string) bool {
	if tx == nil || dialogKey == "" {
		return false
	}
	if tx.invite2xxDialogs == nil {
		tx.invite2xxDialogs = make(map[string]struct{})
	}
	if _, exists := tx.invite2xxDialogs[dialogKey]; exists {
		return true
	}
	if len(tx.invite2xxDialogs) >= maxInvite2xxDialogsPerTransaction {
		return false
	}
	tx.invite2xxDialogs[dialogKey] = struct{}{}
	return true
}

func (tx *Transaction) admitInvite2xxDialog(dialogKey string) bool {
	if tx == nil {
		return false
	}
	tx.inviteACKMu.Lock()
	accepted := tx.admitInvite2xxDialogLocked(dialogKey)
	tx.inviteACKMu.Unlock()
	if !accepted {
		tx.logInvite2xxDialogLimit()
	}
	return accepted
}

func (tx *Transaction) logInvite2xxDialogLimit() {
	if tx != nil && tx.inviteDialogLimitLogged.CompareAndSwap(false, true) {
		slog.Warn("discard INVITE 2xx after dialog tracking limit", "tx_key", tx.key, "limit", maxInvite2xxDialogsPerTransaction)
	}
}

func (tx *Transaction) scheduleInvite2xxCleanup(response *Response) {
	if tx == nil || response == nil {
		return
	}
	dialogKey := invite2xxDialogKey(response)
	if dialogKey == "" {
		return
	}
	clone, err := cloneOutboundResponse(response)
	if err != nil {
		return
	}
	tx.inviteACKMu.Lock()
	if tx.inviteCleanupScheduled == nil {
		tx.inviteCleanupScheduled = make(map[string]struct{})
	}
	if _, exists := tx.inviteCleanupScheduled[dialogKey]; exists {
		tx.inviteACKMu.Unlock()
		return
	}
	tx.inviteCleanupScheduled[dialogKey] = struct{}{}
	tx.inviteACKMu.Unlock()

	if !tx.startBackgroundTask(func() {
		tx.terminateInvite2xxDialog(clone)
		tx.inviteACKMu.Lock()
		delete(tx.inviteCleanupScheduled, dialogKey)
		tx.inviteACKMu.Unlock()
	}) {
		tx.inviteACKMu.Lock()
		delete(tx.inviteCleanupScheduled, dialogKey)
		tx.inviteACKMu.Unlock()
	}
}

func (tx *Transaction) startBackgroundTask(task func()) bool {
	if tx == nil || task == nil {
		return false
	}
	tx.backgroundMu.Lock()
	if tx.backgroundClosed {
		tx.backgroundMu.Unlock()
		return false
	}
	tx.backgroundWG.Add(1)
	tx.backgroundMu.Unlock()
	go func() {
		defer tx.backgroundWG.Done()
		task()
	}()
	return true
}

// CancelInvite 发送与原 INVITE 客户端事务绑定的 CANCEL，并返回独立 CANCEL 事务。
func (tx *Transaction) CancelInvite() (*Transaction, error) {
	ctx, cancel := transactionWriteContext()
	defer cancel()
	return tx.CancelInviteContext(ctx)
}

// CancelInviteContext 发送与原 INVITE 客户端事务绑定的 CANCEL，并允许调用方限制网络写入时间。
func (tx *Transaction) CancelInviteContext(ctx context.Context) (*Transaction, error) {
	return tx.cancelInviteContext(ctx, false)
}

// CancelInviteDetached 发送无需业务层消费响应的 CANCEL。
// 独立 CANCEL 事务收到最终响应后会立即从事务表回收。
func (tx *Transaction) CancelInviteDetached() (bool, error) {
	ctx, cancel := transactionWriteContext()
	defer cancel()
	return tx.CancelInviteDetachedContext(ctx)
}

// CancelInviteDetachedContext 发送无需业务层消费响应的 CANCEL，并保留调用方写入期限。
func (tx *Transaction) CancelInviteDetachedContext(ctx context.Context) (bool, error) {
	cancelTX, err := tx.cancelInviteContext(ctx, true)
	return cancelTX != nil, err
}

func (tx *Transaction) cancelInviteContext(ctx context.Context, closeOnFinal bool) (*Transaction, error) {
	invite := tx.originalRequest()
	if invite == nil || !strings.EqualFold(strings.TrimSpace(invite.Method()), MethodInvite) {
		return nil, nil
	}
	cancel, err := NewCancelRequestFromInviteChecked(invite)
	if err != nil {
		return nil, err
	}
	if closeOnFinal {
		// 即使 CANCEL 写失败，调用方也已结束等待；迟到的 INVITE 2xx 仍必须 ACK 并用 BYE 收敛。
		tx.detachedCancel.Store(true)
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
	if closeOnFinal {
		cancelTX.closeOnFinal.Store(true)
	}
	cancelTX.SetMessageSecurity(tx.messageSecurity())
	if err := cancelTX.RequestContext(ctx, cancel); err != nil {
		cancelTX.Close()
		return nil, err
	}
	return cancelTX, nil
}

func (tx *Transaction) handleDetachedInvite2xx(response *Response) bool {
	if tx == nil || response == nil || !tx.detachedCancel.Load() || response.StatusCode() < 200 || response.StatusCode() >= 300 {
		return false
	}
	cseq, ok := response.CSeq()
	if !ok || cseq == nil || !strings.EqualFold(strings.TrimSpace(cseq.MethodName), MethodInvite) {
		return false
	}
	dialogKey := invite2xxDialogKey(response)
	if dialogKey == "" {
		return false
	}
	if !tx.admitInvite2xxDialog(dialogKey) {
		return true
	}

	return tx.terminateInvite2xxDialog(response)
}

func (tx *Transaction) terminateInvite2xxDialog(response *Response) bool {
	if tx == nil || response == nil {
		return false
	}
	tx.inviteCleanupMu.Lock()
	defer tx.inviteCleanupMu.Unlock()
	if !tx.retransmitInvite2xxACK(response) {
		ack, err := NewRequestFromResponseChecked(MethodACK, response)
		if err != nil {
			slog.Warn("cannot acknowledge extra INVITE 2xx dialog", "tx_key", tx.key, "err", err)
			return true
		}
		ackCtx, cancel := transactionWriteContext()
		err = tx.RequestContext(ackCtx, ack)
		cancel()
		if err != nil {
			slog.Warn("send extra INVITE 2xx ACK failed", "tx_key", tx.key, "err", err)
			return true
		}
	}

	dialogKey := invite2xxDialogKey(response)
	if dialogKey != "" {
		if _, sent := tx.inviteCleanupBYE[dialogKey]; sent {
			return true
		}
	}
	bye, err := NewRequestFromResponseChecked(MethodBYE, response)
	if err != nil {
		slog.Warn("cannot terminate extra INVITE 2xx dialog", "tx_key", tx.key, "err", err)
		return true
	}
	connection := tx.connection()
	if connection == nil {
		connection = bye.GetConnection()
	}
	var byeTX *Transaction
	if tx.owner != nil {
		byeTX = tx.owner.newTXIfOpen(getTXKey(bye), connection)
		if byeTX == nil {
			slog.Warn("cannot terminate extra INVITE 2xx after transaction store closed", "tx_key", tx.key)
			return true
		}
	} else {
		byeTX = NewTransaction(getTXKey(bye), connection)
	}
	byeTX.closeOnFinal.Store(true)
	byeTX.SetMessageSecurity(tx.messageSecurity())
	byeCtx, cancel := transactionWriteContext()
	err = byeTX.RequestContext(byeCtx, bye)
	cancel()
	if err != nil {
		byeTX.Close()
		slog.Warn("send extra INVITE 2xx cleanup BYE failed", "tx_key", tx.key, "err", err)
		return true
	}
	if dialogKey != "" {
		if tx.inviteCleanupBYE == nil {
			tx.inviteCleanupBYE = make(map[string]struct{})
		}
		tx.inviteCleanupBYE[dialogKey] = struct{}{}
	}
	return true
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
	fromTag := ""
	if from, fromOK := request.From(); fromOK && from != nil {
		fromTag = dialogHeaderParam(from.Params, "tag")
	}
	binding := &responseTransactionBinding{
		protocolName: strings.TrimSpace(via.ProtocolName), protocolVersion: strings.TrimSpace(via.ProtocolVersion),
		transport: strings.TrimSpace(via.Transport), sentBy: strings.TrimSpace(via.SentBy()),
		branch: branch, fromTag: fromTag, remoteEndpoint: sipEndpointKey(request.Destination()),
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
	fromTag := ""
	if from, fromOK := response.From(); fromOK && from != nil {
		fromTag = dialogHeaderParam(from.Params, "tag")
	}
	_, branch, branchCount := sipViaParam(via, "branch")
	return strings.EqualFold(binding.protocolName, strings.TrimSpace(via.ProtocolName)) &&
		strings.EqualFold(binding.protocolVersion, strings.TrimSpace(via.ProtocolVersion)) &&
		strings.EqualFold(binding.transport, strings.TrimSpace(via.Transport)) &&
		strings.EqualFold(binding.sentBy, strings.TrimSpace(via.SentBy())) &&
		(binding.fromTag == "" || binding.fromTag == fromTag) &&
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
	tx.watchLoop(tx.idleTimeout, transactionMaxLifetime)
}

func (tx *Transaction) watchLoop(idleTimeout func() time.Duration, maxLifetime time.Duration) {
	timer := time.NewTimer(idleTimeout())
	defer timer.Stop()
	var lifetimeTimer *time.Timer
	var lifetime <-chan time.Time
	if maxLifetime > 0 {
		lifetimeTimer = time.NewTimer(maxLifetime)
		lifetime = lifetimeTimer.C
		defer lifetimeTimer.Stop()
	}
	for {
		// 到达绝对期限后优先关闭，避免持续可读的 active 通道随机延迟回收。
		select {
		case <-lifetime:
			tx.Close()
			return
		default:
		}
		select {
		case <-tx.done:
			return
		case <-lifetime:
			tx.Close()
			return
		case <-tx.active:
			// logrus.Traceln("active tx", tx.Key(), time.Now().Format("2006-01-02 15:04:05"))
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idleTimeout())
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
		tx.backgroundMu.Lock()
		tx.backgroundClosed = true
		tx.backgroundMu.Unlock()
		if tx.done != nil {
			close(tx.done)
		}
		tx.backgroundWG.Wait()
		tx.releaseOwnedConnection()
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
		if err := verifyInboundMessage(security, msg); err != nil {
			slog.Warn("discard SIP response with invalid signal Digest", "tx_key", tx.key, "err", err)
			return
		}
	}
	if msg.StatusCode() >= 100 && msg.StatusCode() < 200 {
		// 公开等待接口只返回最终响应；临时响应仅维持事务活跃，不能占满最终响应队列。
		tx.markActive(1)
		return
	}
	if tx.handleDetachedInvite2xx(msg) {
		tx.markActive(1)
		return
	}
	if tx.handleForkedInvite2xx(msg) {
		tx.markActive(1)
		return
	}
	if tx.retransmitInvite2xxACK(msg) {
		tx.markActive(1)
		return
	}
	inviteNon2xxFinal := false
	if msg.StatusCode() >= 300 && msg.StatusCode() <= 699 {
		if invite := tx.originalRequest(); invite != nil && strings.EqualFold(strings.TrimSpace(invite.Method()), MethodInvite) {
			inviteNon2xxFinal = true
			ack, err := NewAckRequestForNon2xxResponseChecked(invite, msg)
			if err != nil {
				slog.Warn("cannot acknowledge non-2xx INVITE response", "tx_key", tx.key, "err", err)
			} else {
				ackCtx, cancel := transactionWriteContext()
				err = tx.RequestContext(ackCtx, ack)
				cancel()
				if err != nil {
					slog.Warn("send non-2xx INVITE ACK failed", "tx_key", tx.key, "err", err)
				}
			}
		}
	}
	if tx.detachedCancel.Load() {
		tx.markActive(1)
		return
	}
	if inviteNon2xxFinal && !tx.inviteNon2xxFinalDelivered.CompareAndSwap(false, true) {
		tx.markActive(1)
		return
	}
	if tx.closeOnFinal.Load() {
		tx.markActive(1)
		if msg.StatusCode() >= 200 {
			tx.Close()
		}
		return
	}
	// logrus.Traceln("receiveResponse tx", tx.Key(), time.Now().Format("2006-01-02 15:04:05"))
	select {
	case <-tx.done:
		return
	case tx.resp <- msg:
	}
	tx.markActive(1)
	if msg.StatusCode() >= 200 {
		tx.releaseOwnedConnectionAfterFinalResponse()
	}
}

// Respond Respond
func (tx *Transaction) Respond(res *Response) error {
	ctx, cancel := transactionWriteContext()
	defer cancel()
	return tx.RespondContext(ctx, res)
}

// RespondContext 写出 SIP 响应，并允许 context 中止流式连接的写入及写锁等待。
func (tx *Transaction) RespondContext(ctx context.Context, res *Response) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// 同一服务端事务的临时/最终响应必须按线缆顺序串行；最终响应一旦提交，
	// 后续 handler 或异步误调用不能再写出矛盾的第二个响应。
	tx.serverWriteMu.Lock()
	defer tx.serverWriteMu.Unlock()
	if tx.hasServerFinalResponse() {
		return NewError(nil, "SIP server transaction final response is already committed")
	}
	committed := false
	defer func() {
		// 服务端事务只有在响应真实写出并缓存后才能阻止业务重入；
		// context、连接、签名、克隆及网络写入的任何失败都必须允许原请求重传。
		if !committed {
			tx.allowServerRequestRetry()
		}
	}()
	if res == nil {
		return NewError(nil, "SIP response is nil")
	}
	if res.StatusCode() >= 200 {
		// 记录业务已经尝试提交最终响应。即使后续 context、签名或网络写出失败，
		// handler 返回时也不能再补发语义矛盾的 500；设备重传应重新进入业务处理。
		tx.markServerFinalAttempted()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := serializeOutboundMessage(res); err != nil {
		return err
	}
	// 缓存响应可能被多个重传事务复用；发送前克隆，避免签名过程改写共享对象。
	outbound, err := cloneOutboundResponse(res)
	if err != nil {
		return err
	}
	if err := prepareOutboundContentLength(outbound); err != nil {
		return err
	}
	if _, err := serializeOutboundMessage(outbound); err != nil {
		return err
	}
	// logrus.Traceln("send response,to:", outbound.dest.String(), "txkey:", tx.key, "message: \n", outbound.String())
	conn := tx.connection()
	if conn == nil {
		return NewError(nil, "transaction connection is unavailable")
	}
	if security := tx.messageSecurity(); security != nil {
		if err := signOutboundMessage(security, outbound); err != nil {
			return NewError(err, "sign SIP response failed")
		}
	}
	if err := validateOutboundContentLength(outbound); err != nil {
		return err
	}
	if err := validateOutboundResponseCSeq(outbound); err != nil {
		return err
	}
	payload, err := serializeOutboundMessage(outbound)
	if err != nil {
		return err
	}
	logTraffic("out", conn.Network(), conn.LocalAddr(), outbound.dest, payload)
	_, err = writeConnectionContext(ctx, conn, payload, outbound.dest)
	if err != nil {
		return err
	}
	// 仅缓存已确认写出的响应。写失败时设备重传必须重新进入 handler，
	// 让依赖 RespondString 成功结果的业务状态有机会完成提交。
	tx.cacheServerResponse(payload, outbound.dest, outbound.StatusCode() >= 200)
	committed = true
	return nil
}

// Request Request
func (tx *Transaction) Request(req *Request) error {
	ctx, cancel := transactionWriteContext()
	defer cancel()
	return tx.RequestContext(ctx, req)
}

type preparedTransactionRequest struct {
	request *Request
	payload []byte
	conn    Connection
}

// prepareRequest 完成请求写出前所有不会触碰网络的校验、签名、序列化和事务快照。
func (tx *Transaction) prepareRequest(req *Request) (*preparedTransactionRequest, error) {
	if req == nil {
		return nil, NewError(nil, "SIP request is nil")
	}
	if err := prepareOutboundContentLength(req); err != nil {
		return nil, err
	}
	if err := validateOutboundRequestHeaders(req); err != nil {
		return nil, err
	}
	if _, err := serializeOutboundMessage(req); err != nil {
		return nil, err
	}
	conn := tx.connection()
	if conn == nil {
		return nil, NewError(nil, "transaction connection is unavailable")
	}
	tx.bindResponse(req)
	if security := tx.messageSecurity(); security != nil {
		if err := signOutboundMessage(security, req); err != nil {
			return nil, NewError(err, "sign SIP request failed")
		}
	}
	if err := validateOutboundContentLength(req); err != nil {
		return nil, err
	}
	if err := validateOutboundRequestHeaders(req); err != nil {
		return nil, err
	}
	payload, err := serializeOutboundMessage(req)
	if err != nil {
		return nil, err
	}
	// 必须在写出前保存事务快照；TCP 对端可能在 WriteTo 返回前就回送最终响应。
	if err := tx.rememberRequest(req); err != nil {
		return nil, err
	}
	return &preparedTransactionRequest{request: req, payload: payload, conn: conn}, nil
}

func (tx *Transaction) writePreparedRequestContext(ctx context.Context, prepared *preparedTransactionRequest) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if prepared == nil || prepared.request == nil || prepared.conn == nil {
		return NewError(nil, "prepared SIP request is unavailable")
	}
	req := prepared.request
	logTraffic("out", prepared.conn.Network(), prepared.conn.LocalAddr(), req.dest, prepared.payload)
	_, err := writeConnectionContext(ctx, prepared.conn, prepared.payload, req.dest)
	if err == nil {
		tx.rememberInvite2xxACK(req, prepared.payload)
	}
	return err
}

// RequestContext 写出 SIP 请求，并在底层连接支持时用 context 中止阻塞的流式写入。
func (tx *Transaction) RequestContext(ctx context.Context, req *Request) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	prepared, err := tx.prepareRequest(req)
	if err != nil {
		return err
	}
	return tx.writePreparedRequestContext(ctx, prepared)
}

func cloneOutboundResponse(response *Response) (cloned *Response, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			cloned = nil
			err = fmt.Errorf("clone SIP response: %v", recovered)
		}
	}()
	cloned, _ = response.Clone().(*Response)
	if cloned == nil {
		return nil, NewError(nil, "clone SIP response failed")
	}
	return cloned, nil
}

func cloneSIPRequest(request *Request) (cloned *Request, err error) {
	if request == nil {
		return nil, NewError(nil, "clone SIP request failed")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			cloned = nil
			err = fmt.Errorf("clone SIP request: %v", recovered)
		}
	}()
	cloned, _ = request.Clone().(*Request)
	if cloned == nil {
		return nil, NewError(nil, "clone SIP request failed")
	}
	return cloned, nil
}

func signOutboundMessage(security MessageSecurity, message Message) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("SIP message signer panic: %v", recovered)
		}
	}()
	return security.Sign(message)
}

func verifyInboundMessage(security MessageSecurity, message Message) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("SIP message verifier panic: %v", recovered)
		}
	}()
	return security.Verify(message)
}

func getServerTXKey(msg *Request) string {
	base := getTXBaseKey(msg)
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
	key = getTXBaseKey(msg)
	if cseq, exists := msg.CSeq(); !exists || cseq == nil {
		return key
	}
	via, ok := msg.ViaHop()
	if !ok || via == nil {
		return key
	}
	branch := sipViaBranchValue(via)
	if branch == "" {
		return key
	}
	return transactionKeyPart(key) +
		transactionKeyPart(strings.ToUpper(strings.TrimSpace(via.Transport))) +
		transactionKeyPart(strings.ToLower(strings.TrimSpace(via.SentBy()))) +
		transactionKeyPart(branch)
}

func getTXBaseKey(msg Message) (key string) {
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
