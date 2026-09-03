package gbs

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	sdp "github.com/panjjo/gosdp"
)

type directTCPDownloadOffer struct {
	Address       string
	IP            net.IP
	Port          int
	FileSize      int64
	FileSizeKnown bool
	SSRC          string
}

// parseDirectTCPDownloadSDP 解析附录 O 中媒体发送方 200 OK 的 TCP 服务地址和文件大小。
func parseDirectTCPDownloadSDP(body []byte) (directTCPDownloadOffer, error) {
	message, err := sdp.Decode(body)
	if err != nil {
		return directTCPDownloadOffer{}, fmt.Errorf("decode direct TCP download SDP: %w", err)
	}
	medias := sdpMediasByType(message, "video")
	if len(medias) == 0 {
		return directTCPDownloadOffer{}, fmt.Errorf("invalid direct TCP download SDP: SDP does not contain a video tcp media description")
	}
	if len(medias) > 1 {
		return directTCPDownloadOffer{}, fmt.Errorf("direct TCP download SDP must contain exactly one video media description")
	}
	media := medias[0]
	if !strings.EqualFold(media.Description.Protocol, "tcp") {
		return directTCPDownloadOffer{}, fmt.Errorf("invalid direct TCP download SDP: SDP does not contain a video tcp media description")
	}
	if !sdpMediaOffersPayload(media, 96) {
		return directTCPDownloadOffer{}, fmt.Errorf("direct TCP download SDP does not offer PS payload 96")
	}
	mapping, err := sdpPayloadFormat(media, 96)
	if err != nil {
		return directTCPDownloadOffer{}, fmt.Errorf("invalid direct TCP download SDP: %w", err)
	}
	if mapping != "" && !strings.EqualFold(mapping, "PS/90000") {
		return directTCPDownloadOffer{}, fmt.Errorf("direct TCP download SDP payload 96 must use PS/90000")
	}
	direction, err := effectiveSDPDirection(message, media)
	if err != nil {
		return directTCPDownloadOffer{}, fmt.Errorf("invalid direct TCP download SDP direction: %w", err)
	}
	if direction == "inactive" || direction == "recvonly" {
		return directTCPDownloadOffer{}, fmt.Errorf("direct TCP download SDP does not send media")
	}
	if media.Description.Port <= 0 || media.Description.Port > 65535 {
		return directTCPDownloadOffer{}, fmt.Errorf("invalid direct TCP download SDP port: %d", media.Description.Port)
	}
	ip := media.Connection.IP
	networkType, addressType := media.Connection.NetworkType, media.Connection.AddressType
	if ip == nil {
		ip = message.Connection.IP
		networkType, addressType = message.Connection.NetworkType, message.Connection.AddressType
	}
	if err := validateSDPConnectionAddress(networkType, addressType, ip); err != nil {
		return directTCPDownloadOffer{}, fmt.Errorf("invalid direct TCP download SDP: SDP does not contain a valid IP connection address")
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		ip = ipv4
	} else {
		ip = ip.To16()
	}
	// 兼容部分设备沿用标准示例 ACK 中的 fileszie 拼写错误，但单值语义不允许两种拼写并存。
	fileSizeValues := append(sdpAttributeValues(message.Attributes, "filesize", "fileszie"), sdpAttributeValues(media.Attributes, "filesize", "fileszie")...)
	if len(fileSizeValues) > 1 {
		return directTCPDownloadOffer{}, fmt.Errorf("direct TCP download SDP must not contain multiple filesize attributes")
	}
	value := ""
	if len(fileSizeValues) == 1 {
		value = fileSizeValues[0]
	}
	ssrcValues := directTCPSDPLineValues(body, "y")
	if len(ssrcValues) > 1 {
		return directTCPDownloadOffer{}, fmt.Errorf("direct TCP download SDP must not contain multiple y fields")
	}
	ssrc := ""
	if len(ssrcValues) == 1 {
		ssrc = ssrcValues[0]
	}
	offer := directTCPDownloadOffer{
		IP:      ip,
		Port:    media.Description.Port,
		SSRC:    ssrc,
		Address: net.JoinHostPort(ip.String(), strconv.Itoa(media.Description.Port)),
	}
	if value != "" {
		size, err := strconv.ParseInt(value, 10, 64)
		if err != nil || size < 0 {
			return directTCPDownloadOffer{}, fmt.Errorf("invalid direct TCP download SDP filesize: %q", value)
		}
		offer.FileSize = size
		offer.FileSizeKnown = true
	}
	return offer, nil
}

func sdpMediasByType(message *sdp.Message, mediaType string) []*sdp.Media {
	medias := make([]*sdp.Media, 0, 1)
	if message == nil {
		return medias
	}
	for index := range message.Medias {
		media := &message.Medias[index]
		if strings.EqualFold(strings.TrimSpace(media.Description.Type), mediaType) {
			medias = append(medias, media)
		}
	}
	return medias
}

func sdpMediaOffersPayload(media *sdp.Media, payload int) bool {
	if media == nil {
		return false
	}
	want := strconv.Itoa(payload)
	for _, format := range media.Description.Formats {
		if strings.TrimSpace(format) == want {
			return true
		}
	}
	return false
}

// sdpPayloadFormat 按完整负载编号读取 rtpmap，避免第三方库的前缀匹配把 80 误作 8。
// 同一负载重复声明相同映射保持厂商兼容，声明冲突映射则拒绝歧义 SDP。
func sdpPayloadFormat(media *sdp.Media, payload int) (string, error) {
	if media == nil {
		return "", nil
	}
	mapping := ""
	for _, value := range media.Attributes.Values("rtpmap") {
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		mappedPayload, err := strconv.Atoi(fields[0])
		if err != nil || mappedPayload != payload {
			continue
		}
		if len(fields) != 2 {
			return "", fmt.Errorf("invalid SDP rtpmap for payload %d", payload)
		}
		candidate := strings.TrimSpace(fields[1])
		if mapping != "" && !strings.EqualFold(mapping, candidate) {
			return "", fmt.Errorf("conflicting SDP rtpmap for payload %d", payload)
		}
		mapping = candidate
	}
	return mapping, nil
}

func sdpAttributeValues(attributes sdp.Attributes, names ...string) []string {
	values := make([]string, 0, 1)
	for _, attribute := range attributes {
		for _, name := range names {
			if strings.EqualFold(strings.TrimSpace(attribute.Key), name) {
				values = append(values, strings.TrimSpace(attribute.Value))
				break
			}
		}
	}
	return values
}

func effectiveSDPAttributeValues(message *sdp.Message, media *sdp.Media, names ...string) ([]string, error) {
	sessionValues := []string(nil)
	if message != nil {
		sessionValues = sdpAttributeValues(message.Attributes, names...)
	}
	mediaValues := []string(nil)
	if media != nil {
		mediaValues = sdpAttributeValues(media.Attributes, names...)
	}
	if len(sessionValues) > 1 || len(mediaValues) > 1 {
		return nil, fmt.Errorf("multiple %s attributes", names[0])
	}
	if len(mediaValues) != 0 {
		return mediaValues, nil
	}
	return sessionValues, nil
}

func effectiveSDPDirection(message *sdp.Message, media *sdp.Media) (string, error) {
	directions := []string{"sendrecv", "sendonly", "recvonly", "inactive"}
	sessionDirections, err := sdpDirectionValues(message, nil, directions)
	if err != nil {
		return "", err
	}
	mediaDirections, err := sdpDirectionValues(nil, media, directions)
	if err != nil {
		return "", err
	}
	if len(sessionDirections) > 1 || len(mediaDirections) > 1 {
		return "", fmt.Errorf("multiple direction attributes")
	}
	if len(mediaDirections) == 1 {
		return mediaDirections[0], nil
	}
	if len(sessionDirections) == 1 {
		return sessionDirections[0], nil
	}
	return "sendrecv", nil
}

func sdpDirectionValues(message *sdp.Message, media *sdp.Media, directions []string) ([]string, error) {
	attributes := sdp.Attributes(nil)
	if media != nil {
		attributes = media.Attributes
	} else if message != nil {
		attributes = message.Attributes
	}
	values := make([]string, 0, 1)
	for _, attribute := range attributes {
		for _, direction := range directions {
			if strings.EqualFold(strings.TrimSpace(attribute.Key), direction) {
				if strings.TrimSpace(attribute.Value) != "" {
					return nil, fmt.Errorf("direction attribute %s must not have a value", direction)
				}
				values = append(values, direction)
				break
			}
		}
	}
	return values, nil
}

func directTCPSDPLineValue(body []byte, field string) string {
	values := directTCPSDPLineValues(body, field)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func directTCPSDPLineValues(body []byte, field string) []string {
	prefix := field + "="
	values := make([]string, 0, 1)
	for line := range strings.SplitSeq(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= len(prefix) && strings.EqualFold(line[:len(prefix)], prefix) {
			values = append(values, strings.TrimSpace(line[len(prefix):]))
		}
	}
	return values
}
