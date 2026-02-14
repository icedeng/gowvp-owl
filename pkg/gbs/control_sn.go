package gbs

// nextControlSN 生成设备控制统一序列号。
//
// 说明：
// 1. PTZ 与 DeviceControl 共用 DeviceControl 响应通道。
// 2. 使用单调递增 SN 避免并发随机碰撞引起的串包。
func (g *GB28181API) nextControlSN() int {
	sn := g.controlSN.Add(1)
	if sn == 0 {
		// 溢出后回到 1，避免出现 0（部分设备把 0 视为无效 SN）。
		g.controlSN.Store(1)
		sn = 1
	}
	return int(sn)
}

// nextQuerySN 生成设备查询统一序列号，避免随机碰撞。
func (g *GB28181API) nextQuerySN() int {
	sn := g.querySN.Add(1)
	if sn == 0 {
		g.querySN.Store(1)
		sn = 1
	}
	return int(sn)
}
