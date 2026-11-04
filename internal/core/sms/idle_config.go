package sms

import "fmt"

// SetStreamNoneReaderDelay 将超时下发到所有 ZLM 节点，并保留重连时使用的配置。
// 配置变更用于下一次无人观看计时，已创建的 ZLM 定时器按原时限完成。
func (n *NodeManager) SetStreamNoneReaderDelay(seconds int) error {
	if seconds < 1 || seconds > 86400 {
		return fmt.Errorf("无人观看自动断流时间须为 1–86400 秒")
	}
	n.connectionMu.Lock()
	defer n.connectionMu.Unlock()
	driver, ok := n.drivers[ProtocolZLMediaKit].(*ZLMDriver)
	if !ok {
		return fmt.Errorf("未配置 ZLM 驱动")
	}
	previous := driver.noneReaderDelay()
	var applied []*MediaServer
	var applyErr error
	n.cacheServers.Range(func(_ string, state *WarpMediaServer) bool {
		_, _, server := state.status()
		if server == nil || (server.Type != "" && server.Type != ProtocolZLMediaKit) {
			return true
		}
		if err := driver.setNoneReaderDelay(server, seconds); err != nil {
			applyErr = fmt.Errorf("更新媒体节点 %s 失败: %w", server.ID, err)
			return false
		}
		applied = append(applied, server)
		return true
	})
	if applyErr != nil {
		for _, server := range applied {
			if err := driver.setNoneReaderDelay(server, previous); err != nil {
				applyErr = fmt.Errorf("%v；回滚节点 %s 失败: %w", applyErr, server.ID, err)
			}
		}
		return applyErr
	}
	driver.noneReaderDelaySeconds.Store(int64(seconds))
	return nil
}
