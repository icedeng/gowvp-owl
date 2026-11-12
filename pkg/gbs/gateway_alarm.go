package gbs

// GatewayTextAlarmsAllowed 保持强制 Date/Note 验签的原有信令路径；B 不持有信令密钥。
func (s *Server) GatewayTextAlarmsAllowed() bool {
	return s != nil && s.gb != nil && !s.gb.configSnapshot().SignalDigest.Required
}
