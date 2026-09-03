package gbs

import (
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
	// "github.com/panjjo/gosip/db"
)

// MessageNotify 心跳包xml结构
type MessageNotify struct {
	XMLName  xml.Name
	CmdType  string `xml:"CmdType"`
	SN       int    `xml:"SN"`
	DeviceID string `xml:"DeviceID"`
	Status   string `xml:"Status"`
	Info     struct {
		DeviceIDs []string `xml:"DeviceID"`
	} `xml:"Info"`
}

func (g *GB28181API) sipMessageKeepalive(ctx *sip.Context) {
	var msg MessageNotify
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		ctx.Log.Error("Message Unmarshal xml err", "err", err)
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	if err := validateKeepaliveStructure(ctx.Request.Body()); err != nil {
		ctx.String(400, err.Error())
		return
	}
	msg.CmdType = strings.TrimSpace(msg.CmdType)
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	msg.Status = strings.ToUpper(strings.TrimSpace(msg.Status))
	if err := validateKeepaliveEnvelope(ctx, msg); err != nil {
		ctx.String(400, err.Error())
		return
	}

	// 与 REGISTER、离线扫描串行，防止旧离线清理在本次心跳提交后删除新运行态。
	unlockActivity := g.lockRegisterOperation(ctx.DeviceID)
	if binding, ok := admittedInboundRegistrationBinding(ctx); ok &&
		!g.inboundRegistrationBindingMatchesLocked(ctx.DeviceID, binding) {
		unlockActivity()
		ctx.String(200, "OK")
		return
	}
	// 程序重启后内存丢失，收到 keepalive 时补上；首次补载后还需主动加载 Catalog。
	alreadyLoaded, err := g.ensureKnownKeepaliveDevice(ctx.DeviceID)
	if err != nil {
		unlockActivity()
		if errors.Is(err, errInboundDeviceNotRegistered) || orm.IsErrRecordNotFound(err) {
			ctx.String(403, "unregistered GB28181 device")
			return
		}
		ctx.Log.Error("validate Keepalive device", "err", err)
		ctx.String(503, "GB28181 device store unavailable")
		return
	}
	// Keepalive 的 Status 表示设备是否正常工作；收到合法心跳本身即证明发送方在线。
	// OFF 是既有厂商扩展，继续保留其显式离线语义以保持向后兼容。
	isOnline := msg.Status != "OFF"
	// 9.6 状态信息报送：先构造候选状态并确认设备；只有 SIP 200 实际写出后才提交，
	// 避免响应写失败后的同事务重传重复消费同一次心跳。
	status := &DeviceStatusData{
		CmdType:        "DeviceStatus",
		SN:             msg.SN,
		DeviceID:       ctx.DeviceID,
		Status:         msg.Status,
		FaultDeviceIDs: normalizeGBIDList(msg.Info.DeviceIDs),
	}
	if isOnline {
		status.Online = "ONLINE"
	} else {
		status.Online = "OFFLINE"
	}
	runtimeDevice := &Device{
		conn:    ctx.Request.GetConnection(),
		source:  ctx.Source,
		to:      ctx.To,
		Address: ctx.Source.String(),
	}
	if err := g.svr.prepareDeviceMemory(g.serviceContext(), ctx.DeviceID, runtimeDevice); err != nil {
		unlockActivity()
		ctx.Log.Error("restore Keepalive device channels", "err", err)
		ctx.String(503, "GB28181 device store unavailable")
		return
	}
	if err := ctx.RespondString(200, "OK"); err != nil {
		unlockActivity()
		ctx.Log.Error("respond Keepalive", "err", err, "sn", msg.SN)
		return
	}
	g.svr.memoryStorer.LoadOrStore(ctx.DeviceID, runtimeDevice)
	g.storeQueryStateForOwnerLocked(ctx.DeviceID, ctx.DeviceID, "DeviceStatus", status)

	// 本次状态报送晚于待补偿的 DeviceStatus；即使本次落库失败，也不能再重放旧在线状态。
	var registrationState deviceRuntimeState
	if current, ok := g.svr.memoryStorer.Load(ctx.DeviceID); ok && current != nil {
		registrationState = current.runtimeSnapshot()
		current.UpdateRuntime(clearPendingDeviceStatusLocked)
	}

	observedAt := time.Now()
	observation := keepaliveObservation{
		online:          isOnline,
		observedAt:      observedAt,
		address:         ctx.Source.String(),
		transport:       requestSignalingTransport(ctx),
		declaredVersion: ctx.XGBVer,
	}
	updateConnection := func(d *Device) {
		d.conn = ctx.Request.GetConnection()
		d.source = ctx.Source
		d.to = ctx.To
	}
	changeErr := g.persistKeepaliveObservation(ctx.DeviceID, observation, updateConnection)
	if changeErr != nil {
		if current, ok := g.svr.memoryStorer.Load(ctx.DeviceID); ok && current != nil {
			current.UpdateRuntime(func(device *Device) {
				if device.registrationClosed || device.offlinePersistencePending ||
					device.Expires != registrationState.Expires || !device.LastRegisterAt.Equal(registrationState.LastRegisterAt) {
					return
				}
				applyKeepaliveObservationLocked(device, observation)
				updateConnection(device)
				device.keepalivePersistencePending = true
				device.pendingKeepaliveOnline = observation.online
				device.pendingKeepaliveAt = observation.observedAt
				device.pendingKeepaliveAddress = observation.address
				device.pendingKeepaliveTransport = observation.transport
				device.pendingKeepaliveVersion = observation.declaredVersion
			})
		}
	}
	if changeErr != nil {
		ctx.Log.Error("keepalive", "err", changeErr)
	}
	if isOnline {
		g.deviceOfflineTombstones.Delete(ctx.DeviceID)
	}
	if history := g.core.DeviceHistory(); history != nil {
		if err := history.Record(g.serviceContext(), ctx.DeviceID, ipc.DeviceHistoryHeartbeat, ctx.Source.String(), msg.Status, time.Now()); err != nil {
			ctx.Log.Error("持久化设备心跳历史失败", "err", err)
		}
	}
	// 历史必须与设备删除使用同一活动锁串行；否则删除提交后可能再次插入孤立历史。
	unlockActivity()

	if body, err := sip.XMLEncode(struct {
		XMLName xml.Name `xml:"Notify"`
		*DeviceStatusData
	}{
		XMLName:          xml.Name{Local: "Notify"},
		DeviceStatusData: status,
	}); err == nil {
		g.publishEventNotify("DeviceStatus", ctx.DeviceID, body)
	}

	// QueryCatalog 会检查在线状态，因此必须放在 Change(IsOnline=true) 之后。
	if !alreadyLoaded {
		slog.Info("keepalive 触发 Catalog 补载", "device_id", ctx.DeviceID)
		if err := g.QueryCatalogContext(g.serviceContext(), ctx.DeviceID); err != nil {
			ctx.Log.Warn("keepalive 目录补载失败", "err", err)
			if g.scheduleCatalogRefreshAfterFailure(ctx.DeviceID, err) {
				ctx.Log.Info("已安排 Keepalive Catalog 补查")
			}
		}
	}
}

type keepaliveObservation struct {
	online          bool
	observedAt      time.Time
	address         string
	transport       string
	declaredVersion string
}

// applyKeepaliveObservationLocked 仅在持有设备状态写锁或运行态提交回调中调用。
func applyKeepaliveObservationLocked(device *Device, observation keepaliveObservation) {
	if device == nil {
		return
	}
	device.IsOnline = observation.online
	device.LastKeepaliveAt = observation.observedAt
	device.Address = observation.address
	device.offlinePersistencePending = false
	device.registrationClosed = false
	clearPendingDeviceStatusLocked(device)
	clearPendingKeepaliveLocked(device)
}

func (g *GB28181API) persistKeepaliveObservation(
	deviceID string,
	observation keepaliveObservation,
	updateConnection func(*Device),
) error {
	effectiveVersion := GBVersion10
	var disabledCapabilities []string
	return g.svr.changeMemory(g.serviceContext(), deviceID, func(device *ipc.Device) error {
		device.KeepaliveAt = orm.Time{Time: observation.observedAt}
		device.IsOnline = observation.online
		device.Address = observation.address
		device.Transport = observation.transport
		setPersistedRegistrationClosed(device, false)
		effectiveVersion = applyGBProtocolVersion(&device.Ext, observation.declaredVersion)
		disabledCapabilities = append([]string(nil), device.Ext.GBDisabledCapabilities...)
		return nil
	}, func(device *Device) {
		applyKeepaliveObservationLocked(device, observation)
		if updateConnection != nil {
			updateConnection(device)
		}
		device.setGBProfile(effectiveVersion, disabledCapabilities)
	})
}

func samePendingKeepalive(current, expected deviceRuntimeState) bool {
	return current.KeepalivePersistencePending && expected.KeepalivePersistencePending &&
		current.PendingKeepaliveOnline == expected.PendingKeepaliveOnline &&
		current.PendingKeepaliveAt.Equal(expected.PendingKeepaliveAt) &&
		current.PendingKeepaliveAddress == expected.PendingKeepaliveAddress &&
		current.PendingKeepaliveTransport == expected.PendingKeepaliveTransport &&
		current.PendingKeepaliveVersion == expected.PendingKeepaliveVersion &&
		current.Expires == expected.Expires && current.LastRegisterAt.Equal(expected.LastRegisterAt)
}

// samePendingKeepaliveLocked 必须在持有 device.stateMu 写锁时调用。
func samePendingKeepaliveLocked(device *Device, expected deviceRuntimeState) bool {
	return device != nil && device.keepalivePersistencePending && expected.KeepalivePersistencePending &&
		device.pendingKeepaliveOnline == expected.PendingKeepaliveOnline &&
		device.pendingKeepaliveAt.Equal(expected.PendingKeepaliveAt) &&
		device.pendingKeepaliveAddress == expected.PendingKeepaliveAddress &&
		device.pendingKeepaliveTransport == expected.PendingKeepaliveTransport &&
		device.pendingKeepaliveVersion == expected.PendingKeepaliveVersion &&
		device.Expires == expected.Expires && device.LastRegisterAt.Equal(expected.LastRegisterAt)
}

func (g *GB28181API) retryPendingKeepalive(deviceID string, expected deviceRuntimeState) (bool, error) {
	unlockActivity := g.lockRegisterOperation(deviceID)
	defer unlockActivity()
	device, ok := g.svr.memoryStorer.Load(deviceID)
	if !ok || device == nil {
		return false, nil
	}
	current := device.runtimeSnapshot()
	if !samePendingKeepalive(current, expected) {
		return false, nil
	}
	if current.RegistrationClosed || current.OfflinePersistencePending ||
		registrationBindingExpired(current.LastRegisterAt, current.Expires, time.Now()) {
		device.UpdateRuntime(func(latest *Device) {
			if samePendingKeepaliveLocked(latest, current) {
				clearPendingKeepaliveLocked(latest)
			}
		})
		return false, nil
	}
	observation := keepaliveObservation{
		online:          current.PendingKeepaliveOnline,
		observedAt:      current.PendingKeepaliveAt,
		address:         current.PendingKeepaliveAddress,
		transport:       current.PendingKeepaliveTransport,
		declaredVersion: current.PendingKeepaliveVersion,
	}
	if err := g.persistKeepaliveObservation(deviceID, observation, nil); err != nil {
		return false, fmt.Errorf("retry Keepalive persistence for %s: %w", deviceID, err)
	}
	return true, nil
}

// ensureKnownKeepaliveDevice 仅允许已注册运行态或持久化已建档的设备通过心跳恢复。
// 校验必须先于 LoadOrStore 和状态写入，避免未知编码注入运行态并触发目录查询。
func (g *GB28181API) ensureKnownKeepaliveDevice(deviceID string) (bool, error) {
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return false, fmt.Errorf("GB28181 memory store unavailable")
	}
	if device, ok := g.svr.memoryStorer.Load(deviceID); ok && device != nil {
		if runtimeRegistrationBindingActive(device.runtimeSnapshot(), time.Now()) {
			return true, nil
		}
		return false, errInboundDeviceNotRegistered
	}
	store := g.core.Store()
	if store == nil {
		return false, fmt.Errorf("GB28181 device store unavailable")
	}
	deviceStore := store.Device()
	if deviceStore == nil {
		return false, fmt.Errorf("GB28181 device store unavailable")
	}
	var device ipc.Device
	if err := deviceStore.Get(g.serviceContext(), &device, orm.Where("device_id=?", deviceID)); err != nil {
		return false, fmt.Errorf("lookup GB28181 device %s: %w", deviceID, err)
	}
	if strings.TrimSpace(device.GetGB28181DeviceID()) != strings.TrimSpace(deviceID) {
		return false, fmt.Errorf("GB28181 device store returned mismatched device")
	}
	if !persistedRegistrationBindingActive(&device, time.Now()) {
		return false, errInboundDeviceNotRegistered
	}
	return false, nil
}

func validateKeepaliveEnvelope(ctx *sip.Context, msg MessageNotify) error {
	if ctx == nil || !isGBDeviceIdentifier(strings.TrimSpace(ctx.DeviceID)) {
		return fmt.Errorf("Keepalive requires authenticated GB28181 device")
	}
	if msg.XMLName.Local != "Notify" || !strings.EqualFold(msg.CmdType, "Keepalive") || msg.SN <= 0 {
		return fmt.Errorf("invalid Keepalive envelope")
	}
	if !isGBDeviceIdentifier(msg.DeviceID) || msg.DeviceID != strings.TrimSpace(ctx.DeviceID) {
		return fmt.Errorf("Keepalive device id mismatch")
	}
	// 空值及 ON/OFF 是已存在的厂商兼容；标准值为 OK/ERROR。
	if msg.Status != "" && !equalFoldAny(msg.Status, "OK", "ERROR", "ON", "OFF") {
		return fmt.Errorf("invalid Keepalive status")
	}
	for _, deviceID := range msg.Info.DeviceIDs {
		deviceID = strings.TrimSpace(deviceID)
		if deviceID != "" && !isGBDeviceIdentifier(deviceID) {
			return fmt.Errorf("invalid Keepalive fault device id")
		}
	}
	return nil
}

func normalizeGBIDList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
