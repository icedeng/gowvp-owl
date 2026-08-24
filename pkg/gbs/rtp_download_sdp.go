package gbs

import (
	"fmt"
	"strconv"
	"strings"

	sdp "github.com/panjjo/gosdp"
)

// parseRTPDownloadFileSize 读取 2014 附录 F 定义的 a=filesize。
func parseRTPDownloadFileSize(body []byte) (int64, bool, error) {
	message, err := sdp.Decode(body)
	if err != nil {
		return 0, false, fmt.Errorf("decode RTP download SDP: %w", err)
	}
	value := strings.TrimSpace(message.Attribute("filesize"))
	for i := range message.Medias {
		media := &message.Medias[i]
		if !strings.EqualFold(media.Description.Type, "video") {
			continue
		}
		if candidate := strings.TrimSpace(media.Attribute("filesize")); candidate != "" {
			value = candidate
			break
		}
		if candidate := strings.TrimSpace(media.Attribute("fileszie")); candidate != "" {
			value = candidate
			break
		}
	}
	if value == "" {
		value = directTCPAttributeValue(body, "filesize", "fileszie")
	}
	if value == "" {
		return 0, false, nil
	}
	size, err := strconv.ParseInt(value, 10, 64)
	if err != nil || size < 0 {
		return 0, false, fmt.Errorf("invalid RTP download SDP filesize: %q", value)
	}
	return size, true, nil
}
