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
	values := sdpAttributeValues(message.Attributes, "filesize", "fileszie")
	for i := range message.Medias {
		media := &message.Medias[i]
		if !strings.EqualFold(media.Description.Type, "video") {
			continue
		}
		values = append(values, sdpAttributeValues(media.Attributes, "filesize", "fileszie")...)
	}
	if len(values) > 1 {
		return 0, false, fmt.Errorf("RTP download SDP must not contain multiple filesize attributes")
	}
	value := ""
	if len(values) == 1 {
		value = values[0]
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
