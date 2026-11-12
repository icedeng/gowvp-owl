package protocol

// 通过独立能力查询版本，避免改变旧版本严格 probe 响应结构。
var GatewayBuild = "development"

type GatewayInfoRequest struct{}
type GatewayInfo struct {
	Build           string  `json:"build"`
	ProtocolVersion int     `json:"protocol_version"`
	AlarmPending    int     `json:"alarm_pending"`
	AlarmBytes      Decimal `json:"alarm_bytes"`
	AlarmStoreError bool    `json:"alarm_store_error"`
}
