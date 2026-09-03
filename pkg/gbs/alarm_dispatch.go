package gbs

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

type pendingAlarmDispatchKey struct {
	worker   *cascadeWorker
	sn       int
	deviceID string
}

type pendingAlarmDispatch struct {
	result          chan alarmDispatchOutcome
	cancelOperation context.CancelCauseFunc
	completeOnce    sync.Once
}

type alarmDispatchOutcome struct {
	response alarmBusinessResponse
	err      error
}

func newPendingAlarmDispatch(ctx context.Context) (*pendingAlarmDispatch, context.Context) {
	operationCtx, cancel := context.WithCancelCause(ctx)
	return &pendingAlarmDispatch{
		result:          make(chan alarmDispatchOutcome, 1),
		cancelOperation: cancel,
	}, operationCtx
}

func (p *pendingAlarmDispatch) complete(response alarmBusinessResponse) {
	if p == nil || p.result == nil {
		return
	}
	p.completeOnce.Do(func() {
		p.result <- alarmDispatchOutcome{response: response}
	})
}

func (p *pendingAlarmDispatch) cancel(cause error) {
	if p == nil {
		return
	}
	if cause == nil {
		cause = context.Canceled
	}
	if p.cancelOperation != nil {
		p.cancelOperation(cause)
	}
	if p.result == nil {
		return
	}
	p.completeOnce.Do(func() {
		p.result <- alarmDispatchOutcome{err: cause}
	})
}

type pendingLocalAlarmDispatchKey struct {
	receiverID string
	sn         int
	deviceID   string
}

type pendingLocalAlarmDispatch struct {
	wait      chan alarmBusinessResponse
	operation *pendingDeviceOperation
}

type localAlarmDispatchTarget struct {
	config conf.SIPAlarmReceiver
	device *Device
}

// dispatchAlarmToLocalTargets 将报警分发给本平台已注册且显式授权的接警 SIP 客户端。
func (g *GB28181API) dispatchAlarmToLocalTargets(sourceDeviceID string, body []byte, expected ...inboundRegistrationBinding) {
	if g == nil || len(body) == 0 {
		return
	}
	var envelope struct {
		DeviceID string `xml:"DeviceID"`
	}
	if err := sip.XMLDecode(body, &envelope); err != nil {
		return
	}
	alarmDeviceID := strings.TrimSpace(envelope.DeviceID)
	expectedBinding := append([]inboundRegistrationBinding(nil), expected...)
	for _, target := range g.localAlarmDispatchTargets(alarmDeviceID) {
		target := target
		payload := append([]byte(nil), body...)
		if !g.startLifecycleTask(context.Background(), func(taskCtx context.Context) {
			unlockSource, err := g.lockInboundDeviceStateCommit(sourceDeviceID, expectedBinding...)
			if err != nil {
				return
			}
			// 报警已在源请求确认后成为独立事件；这里只校验其注册代次仍有效。
			// 目标侧 SIP/业务应答等待不能继续占用源设备锁，否则多个接警目标会被串行化。
			unlockSource()
			if err := g.dispatchAlarmToLocalTarget(taskCtx, target, sourceDeviceID, alarmDeviceID, payload); err != nil &&
				!errors.Is(err, context.Canceled) && !errors.Is(err, ErrServiceStopped) &&
				!errors.Is(err, ErrDeviceOffline) && !errors.Is(err, ErrDeviceNotExist) {
				slog.Warn("dispatch Alarm to registered receiver failed", "receiver", target.config.Name,
					"receiver_id", target.config.DeviceID, "err", err)
			}
		}) {
			return
		}
	}
}

func (g *GB28181API) localAlarmDispatchTargets(alarmDeviceID string) []localAlarmDispatchTarget {
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil || strings.TrimSpace(alarmDeviceID) == "" {
		return nil
	}
	cfg := g.configSnapshot()
	if cfg == nil {
		return nil
	}
	alarmDeviceID = strings.TrimSpace(alarmDeviceID)
	targets := make([]localAlarmDispatchTarget, 0, len(cfg.AlarmReceivers))
	seen := make(map[string]struct{}, len(cfg.AlarmReceivers))
	for _, receiver := range cfg.AlarmReceivers {
		if !receiver.Enabled || !alarmReceiverAllowsSource(receiver, alarmDeviceID) {
			continue
		}
		receiverID := strings.TrimSpace(receiver.DeviceID)
		if _, exists := seen[receiverID]; exists {
			continue
		}
		device, ok := g.svr.memoryStorer.Load(receiverID)
		if !ok || device == nil || !device.IsOnlineNow() {
			continue
		}
		receiver.Name = strings.TrimSpace(receiver.Name)
		receiver.DeviceID = receiverID
		targets = append(targets, localAlarmDispatchTarget{config: receiver, device: device})
		seen[receiverID] = struct{}{}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].config.Name == targets[j].config.Name {
			return targets[i].config.DeviceID < targets[j].config.DeviceID
		}
		return targets[i].config.Name < targets[j].config.Name
	})
	return targets
}

func alarmReceiverAllowsSource(receiver conf.SIPAlarmReceiver, alarmDeviceID string) bool {
	alarmDeviceID = strings.TrimSpace(alarmDeviceID)
	for _, sourceID := range receiver.SourceIDs {
		if strings.TrimSpace(sourceID) == alarmDeviceID {
			return true
		}
	}
	return false
}

func (g *GB28181API) dispatchAlarmToLocalTarget(ctx context.Context, target localAlarmDispatchTarget, sourceDeviceID, alarmDeviceID string, body []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if target.device == nil || !target.device.IsOnlineNow() {
		return ErrDeviceOffline
	}
	receiverID := strings.TrimSpace(target.config.DeviceID)
	if receiverID == "" {
		return ErrDeviceNotExist
	}
	// 接警终端不能与当前报警上报者相同，避免误配置导致报警反射回源设备。
	if receiverID == strings.TrimSpace(sourceDeviceID) {
		return nil
	}
	version := g.getDeviceGBProtocolVersion(receiverID)
	rewritten, err := rewriteCascadeAlarmInfoForVersion(body, version)
	if err != nil {
		return err
	}
	// 在登记 pending 和分配 SN 前完成版本转换后的结构校验，避免异常报警报文
	// 消耗序号或留下无法匹配的等待状态。
	if err = validateAlarmStructure(rewritten, version); err != nil {
		return fmt.Errorf("validate registered receiver Alarm payload: %w", err)
	}

	operation, releaseOperation := g.trackPendingDeviceRequest(ctx, receiverID, alarmDeviceID)
	defer releaseOperation()
	requestCtx := operation.Context(ctx)
	var key pendingLocalAlarmDispatchKey
	var pending *pendingLocalAlarmDispatch
	for {
		key = pendingLocalAlarmDispatchKey{receiverID: receiverID, sn: g.nextQuerySN(), deviceID: alarmDeviceID}
		pending = &pendingLocalAlarmDispatch{wait: make(chan alarmBusinessResponse, 1), operation: operation}
		if _, loaded := g.pendingLocalAlarmDispatch.LoadOrStore(key, pending); !loaded {
			break
		}
	}
	defer g.pendingLocalAlarmDispatch.CompareAndDelete(key, pending)

	rewritten, err = rewriteAlarmDispatchSN(rewritten, key.sn)
	if err != nil {
		return err
	}
	tx, err := g.svr.wrapRequestContext(requestCtx, target.device, sip.MethodMessage, &sip.ContentTypeXML, rewritten)
	if err != nil {
		return operation.ErrorOr(err)
	}
	if _, err = sipResponseContext(requestCtx, tx); err != nil {
		return operation.ErrorOr(err)
	}

	timer := time.NewTimer(defaultCascadeRequestTimeout)
	defer timer.Stop()
	select {
	case response := <-pending.wait:
		if !strings.EqualFold(strings.TrimSpace(response.Result), "OK") {
			return fmt.Errorf("registered receiver Alarm rejected: %s", strings.TrimSpace(response.Result))
		}
		return nil
	case <-operation.Done():
		return operation.Cause()
	case <-g.serviceDone():
		return ErrServiceStopped
	case <-timer.C:
		return fmt.Errorf("wait registered receiver Alarm response timeout")
	}
}

// dispatchAlarmToCascadeTargets 按 9.4 将报警异步分发给显式开启且共享了报警通道的上级平台。
func (g *GB28181API) dispatchAlarmToCascadeTargets(sourceDeviceID string, body []byte, expected ...inboundRegistrationBinding) {
	if g == nil || g.svr == nil || g.svr.cascade == nil || len(body) == 0 {
		return
	}
	var envelope struct {
		DeviceID string `xml:"DeviceID"`
	}
	if err := sip.XMLDecode(body, &envelope); err != nil {
		return
	}
	workers := g.svr.cascade.alarmDispatchWorkers(strings.TrimSpace(envelope.DeviceID))
	expectedBinding := append([]inboundRegistrationBinding(nil), expected...)
	for _, worker := range workers {
		worker := worker
		payload := append([]byte(nil), body...)
		if !g.startCascadeLifecycleTask(context.Background(), worker, func(taskCtx context.Context) {
			unlockSource, err := g.lockInboundDeviceStateCommit(sourceDeviceID, expectedBinding...)
			if err != nil {
				return
			}
			// 各上级分发必须独立等待，不能用源设备注册锁把目标平台串行化。
			unlockSource()
			if err := g.dispatchAlarmToCascadeTarget(taskCtx, worker, sourceDeviceID, payload); err != nil &&
				!errors.Is(err, context.Canceled) && !errors.Is(err, ErrServiceStopped) {
				slog.Warn("dispatch Alarm to cascade platform failed", "upstream", worker.platform.name, "err", err)
			}
		}) {
			return
		}
	}
}

func (m *CascadeManager) alarmDispatchWorkers(localDeviceID string) []*cascadeWorker {
	if m == nil || strings.TrimSpace(localDeviceID) == "" {
		return nil
	}
	localDeviceID = strings.TrimSpace(localDeviceID)
	m.mu.RLock()
	workers := make([]*cascadeWorker, 0, len(m.items))
	now := time.Now()
	for _, worker := range m.items {
		if worker == nil || !worker.platform.alarmDispatchEnabled || worker.platform.channelIDMap[localDeviceID] == "" ||
			!worker.registrationActive(now) {
			continue
		}
		workers = append(workers, worker)
	}
	m.mu.RUnlock()
	sort.Slice(workers, func(i, j int) bool { return workers[i].platform.name < workers[j].platform.name })
	return workers
}

func (g *GB28181API) dispatchAlarmToCascadeTarget(ctx context.Context, worker *cascadeWorker, sourceDeviceID string, body []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if worker == nil || !worker.platform.alarmDispatchEnabled || !worker.registrationActive(time.Now()) {
		return fmt.Errorf("cascade Alarm target is unavailable")
	}
	version := worker.protocolVersion()
	converted, err := rewriteCascadeAlarmInfoForVersion(body, version)
	if err != nil {
		return err
	}
	rewritten, exposedDeviceID, err := rewriteCascadeEventBodyForDevice(worker.platform, converted, sourceDeviceID)
	if err != nil {
		return err
	}
	if exposedDeviceID == "" {
		return nil
	}
	// 先校验完整的目标侧报警结构，再登记 pending/分配 SN，避免转换失败
	// 产生不可关联的级联请求副作用。
	if err = validateAlarmStructure(rewritten, version); err != nil {
		return fmt.Errorf("validate cascade Alarm payload: %w", err)
	}

	pending, operationCtx := newPendingAlarmDispatch(ctx)
	defer pending.cancelOperation(nil)
	stopWorkerCancel := func() bool { return false }
	if operationCtx := worker.operationContext(); operationCtx != nil {
		stopWorkerCancel = context.AfterFunc(operationCtx, func() { pending.cancel(context.Canceled) })
	}
	defer stopWorkerCancel()

	var key pendingAlarmDispatchKey
	for {
		key = pendingAlarmDispatchKey{worker: worker, sn: g.nextQuerySN(), deviceID: exposedDeviceID}
		if _, loaded := g.pendingAlarmDispatch.LoadOrStore(key, pending); !loaded {
			break
		}
	}
	defer g.pendingAlarmDispatch.CompareAndDelete(key, pending)

	rewritten, err = rewriteAlarmDispatchSN(rewritten, key.sn)
	if err != nil {
		return err
	}
	if err = worker.sendMessage(operationCtx, rewritten); err != nil {
		if cause := context.Cause(operationCtx); cause != nil {
			return cause
		}
		return err
	}

	timer := time.NewTimer(defaultCascadeRequestTimeout)
	defer timer.Stop()
	select {
	case outcome := <-pending.result:
		if outcome.err != nil {
			return outcome.err
		}
		if !strings.EqualFold(strings.TrimSpace(outcome.response.Result), "OK") {
			return fmt.Errorf("cascade Alarm rejected: %s", strings.TrimSpace(outcome.response.Result))
		}
		return nil
	case <-operationCtx.Done():
		return context.Cause(operationCtx)
	case <-g.serviceDone():
		return ErrServiceStopped
	case <-timer.C:
		return fmt.Errorf("wait cascade Alarm response timeout")
	}
}

func (g *GB28181API) removeCascadeAlarmDispatches(worker *cascadeWorker) {
	if g == nil || worker == nil {
		return
	}
	g.pendingAlarmDispatch.Range(func(key, value any) bool {
		dispatchKey, keyOK := key.(pendingAlarmDispatchKey)
		pending, pendingOK := value.(*pendingAlarmDispatch)
		if !keyOK || !pendingOK || pending == nil || dispatchKey.worker != worker {
			return true
		}
		if g.pendingAlarmDispatch.CompareAndDelete(key, pending) {
			pending.cancel(context.Canceled)
		}
		return true
	})
}

func rewriteAlarmDispatchSN(body []byte, sn int) ([]byte, error) {
	decoder := sip.NewGBXMLDecoder(body)
	var output bytes.Buffer
	encoder := xml.NewEncoder(&output)
	depth := 0
	rewritten := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode cascade Alarm SN: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if ok {
			depth++
			if depth == 2 && start.Name.Local == "SN" {
				if rewritten {
					return nil, fmt.Errorf("cascade Alarm contains duplicate SN")
				}
				var original string
				if err := decoder.DecodeElement(&original, &start); err != nil {
					return nil, fmt.Errorf("decode cascade Alarm SN value: %w", err)
				}
				if err := encoder.EncodeToken(start); err != nil {
					return nil, err
				}
				if err := encoder.EncodeToken(xml.CharData(strconv.AppendInt(nil, int64(sn), 10))); err != nil {
					return nil, err
				}
				if err := encoder.EncodeToken(start.End()); err != nil {
					return nil, err
				}
				depth--
				rewritten = true
				continue
			}
		} else if _, ok := token.(xml.EndElement); ok {
			depth--
		}
		if err := encoder.EncodeToken(token); err != nil {
			return nil, err
		}
	}
	if !rewritten {
		return nil, fmt.Errorf("cascade Alarm SN is missing")
	}
	if err := encoder.Flush(); err != nil {
		return nil, err
	}
	encoded, err := sip.EncodeGBXMLDocument(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("encode cascade Alarm SN as GB2312: %w", err)
	}
	return encoded, nil
}

// handleCascadeAlarmBusinessResponse 接收目标上级按 9.4 返回的独立 MESSAGE/Response。
func (g *GB28181API) handleCascadeAlarmBusinessResponse(ctx *sip.Context, worker *cascadeWorker) {
	if err := validateAlarmBusinessResponseStructure(ctx.Request.Body()); err != nil {
		ctx.AbortString(400, err.Error())
		return
	}
	var response alarmBusinessResponse
	if err := sip.XMLDecode(ctx.Request.Body(), &response); err != nil {
		ctx.AbortString(400, ErrXMLDecode.Error())
		return
	}
	response.CmdType = strings.TrimSpace(response.CmdType)
	response.DeviceID = strings.TrimSpace(response.DeviceID)
	response.Result = strings.TrimSpace(response.Result)
	if response.XMLName.Local != "Response" || !strings.EqualFold(response.CmdType, "Alarm") || response.SN <= 0 ||
		!validAlarmBusinessTargetID(response.DeviceID) || !isGBResultValue(response.Result) {
		ctx.AbortString(400, "invalid cascade Alarm response")
		return
	}
	key := pendingAlarmDispatchKey{worker: worker, sn: response.SN, deviceID: response.DeviceID}
	value, found := g.pendingAlarmDispatch.Load(key)
	if !found {
		targetMismatch := false
		g.pendingAlarmDispatch.Range(func(candidate, _ any) bool {
			candidateKey, ok := candidate.(pendingAlarmDispatchKey)
			if ok && candidateKey.worker == worker && candidateKey.sn == response.SN {
				targetMismatch = true
				return false
			}
			return true
		})
		if targetMismatch {
			ctx.AbortString(400, "cascade Alarm response target mismatch")
			return
		}
	}
	var pending *pendingAlarmDispatch
	if found {
		var pendingOK bool
		pending, pendingOK = value.(*pendingAlarmDispatch)
		if !pendingOK || pending == nil {
			ctx.AbortString(400, "invalid cascade Alarm pending state")
			return
		}
	}
	// 业务应答自身也需要先完成 SIP 事务，再唤醒分发等待方。
	if err := ctx.RespondString(200, "OK"); err != nil {
		ctx.Log.Error("respond cascade Alarm", "err", err, "sn", response.SN, "target_id", response.DeviceID)
		ctx.Abort()
		return
	}
	ctx.Abort()
	if pending != nil {
		pending.complete(response)
	}
}

// handleLocalAlarmBusinessResponse 接收本域已注册接警终端按 9.4 返回的独立 MESSAGE/Response。
func (g *GB28181API) handleLocalAlarmBusinessResponse(ctx *sip.Context) {
	if ctx == nil || ctx.Request == nil || strings.TrimSpace(ctx.DeviceID) == "" {
		if ctx != nil {
			ctx.AbortString(403, "Alarm response requires authenticated receiver")
		}
		return
	}
	if err := validateAlarmBusinessResponseStructure(ctx.Request.Body()); err != nil {
		ctx.AbortString(400, err.Error())
		return
	}
	var response alarmBusinessResponse
	if err := sip.XMLDecode(ctx.Request.Body(), &response); err != nil {
		ctx.AbortString(400, ErrXMLDecode.Error())
		return
	}
	response.CmdType = strings.TrimSpace(response.CmdType)
	response.DeviceID = strings.TrimSpace(response.DeviceID)
	response.Result = strings.TrimSpace(response.Result)
	if response.XMLName.Local != "Response" || !strings.EqualFold(response.CmdType, "Alarm") || response.SN <= 0 ||
		!validAlarmBusinessTargetID(response.DeviceID) || !isGBResultValue(response.Result) {
		ctx.AbortString(400, "invalid registered receiver Alarm response")
		return
	}
	receiverID := strings.TrimSpace(ctx.DeviceID)
	key := pendingLocalAlarmDispatchKey{receiverID: receiverID, sn: response.SN, deviceID: response.DeviceID}
	value, found := g.pendingLocalAlarmDispatch.Load(key)
	if !found {
		targetMismatch := false
		g.pendingLocalAlarmDispatch.Range(func(candidate, _ any) bool {
			candidateKey, ok := candidate.(pendingLocalAlarmDispatchKey)
			if ok && candidateKey.receiverID == receiverID && candidateKey.sn == response.SN {
				targetMismatch = true
				return false
			}
			return true
		})
		if targetMismatch {
			ctx.AbortString(400, "registered receiver Alarm response target mismatch")
			return
		}
	}
	var pending *pendingLocalAlarmDispatch
	if found {
		var pendingOK bool
		pending, pendingOK = value.(*pendingLocalAlarmDispatch)
		if !pendingOK || pending == nil || pending.operation == nil {
			ctx.AbortString(400, "invalid registered receiver Alarm pending state")
			return
		}
	}
	if err := ctx.RespondString(200, "OK"); err != nil {
		ctx.Log.Error("respond registered receiver Alarm", "err", err, "sn", response.SN, "target_id", response.DeviceID)
		ctx.Abort()
		return
	}
	ctx.Abort()
	if pending == nil {
		return
	}
	unlockCommit, err := g.lockAdmittedInboundDeviceStateCommit(ctx)
	if err != nil {
		return
	}
	defer unlockCommit()
	pending.operation.Deliver(func() {
		select {
		case pending.wait <- response:
		default:
		}
	})
}

func validAlarmBusinessTargetID(value string) bool {
	value = strings.TrimSpace(value)
	return isGBDeviceIdentifier(value) || (len(value) == 10 && isNumericIdentifier(value))
}
