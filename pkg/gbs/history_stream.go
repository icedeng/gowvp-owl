package gbs

import "strings"

// HistoryStreamID 将平台发起的历史收流与直播、下载分别隔离。
// 级联自定义收流标识继续由 HistoryInput.streamID 指定。
func HistoryStreamID(mode, channelID string) string {
	return "history-" + strings.ToLower(mode) + "-" + channelID
}
