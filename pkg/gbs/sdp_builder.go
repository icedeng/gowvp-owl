package gbs

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	sdp "github.com/panjjo/gosdp"
)

const historyModePlay = "Play"

// gbSDPInput 描述平台向设备发起媒体会话时需要生成的 SDP 字段。
type gbSDPInput struct {
	Version          GBProtocolVersion
	SessionName      string
	ChannelID        string
	URI              string
	IP               string
	Port             int
	StreamMode       int8
	StartAt          time.Time
	EndAt            time.Time
	SSRC             string
	MediaDescription string
	DownloadSpeed    int
	DirectTCP        bool
	AudioOnly        bool
	H265Disabled     bool
	AACDisabled      bool
}

// buildGBSDP 按设备的协议档案生成直播、回放或下载 SDP。
func buildGBSDP(in gbSDPInput) ([]byte, error) {
	version, ok := ParseGBProtocolVersion(string(in.Version))
	if !ok {
		return nil, fmt.Errorf("invalid GB protocol version: %s", in.Version)
	}
	if strings.TrimSpace(in.ChannelID) == "" {
		return nil, fmt.Errorf("SDP channel ID is required")
	}
	address, err := parseSDPAddress(in.IP)
	if err != nil {
		return nil, err
	}
	if in.Port <= 0 || in.Port > 65535 {
		return nil, fmt.Errorf("invalid SDP media port: %d", in.Port)
	}
	if !validGBSSRC(in.SSRC) {
		return nil, fmt.Errorf("invalid GB/T 28181 SSRC: %s", in.SSRC)
	}

	protocol := "RTP/AVP"
	if in.DirectTCP {
		if in.SessionName != historyModeDownload || !version.Capabilities().DirectTCPDownload {
			return nil, fmt.Errorf("直接 TCP 文件下载不受当前协议档案 %s 支持", version.StandardName())
		}
		if in.StreamMode != 0 {
			return nil, fmt.Errorf("direct TCP download must not reuse RTP stream mode: %d", in.StreamMode)
		}
		protocol = "tcp"
	} else {
		switch in.StreamMode {
		case 0:
		case 1, 2:
			if !version.Capabilities().RTPOverTCP {
				return nil, fmt.Errorf("RTP over TCP 不受当前协议档案 %s 支持", version.StandardName())
			}
			protocol = "TCP/RTP/AVP"
		default:
			return nil, fmt.Errorf("invalid stream mode: %d", in.StreamMode)
		}
	}

	switch in.SessionName {
	case historyModePlay:
		if in.SSRC[0] != '0' {
			return nil, fmt.Errorf("Play SDP requires realtime SSRC starting with 0: %s", in.SSRC)
		}
	case historyModePlayback, historyModeDownload:
		if in.SSRC[0] != '1' {
			return nil, fmt.Errorf("%s SDP requires history SSRC starting with 1: %s", in.SessionName, in.SSRC)
		}
		if strings.TrimSpace(in.URI) == "" {
			return nil, fmt.Errorf("%s SDP URI is required", in.SessionName)
		}
		if in.StartAt.IsZero() || in.EndAt.IsZero() || !in.EndAt.After(in.StartAt) {
			return nil, fmt.Errorf("invalid %s SDP time range", in.SessionName)
		}
	default:
		return nil, fmt.Errorf("unsupported SDP session name: %s", in.SessionName)
	}
	if in.AudioOnly {
		if in.SessionName != historyModePlay {
			return nil, fmt.Errorf("audio-only SDP is only valid for Play")
		}
		if !version.AtLeast(GBVersion20) {
			return nil, fmt.Errorf("标准双流程语音对讲不受当前协议档案 %s 支持", version.StandardName())
		}
		if in.MediaDescription != "" {
			return nil, fmt.Errorf("audio-only Play SDP uses the standard PCMA media description")
		}
	}
	if err := validateGBMediaDescriptionForVersion(in.MediaDescription, version); err != nil {
		return nil, err
	}
	if in.DownloadSpeed < 0 {
		return nil, fmt.Errorf("download speed must not be negative")
	}
	if in.DownloadSpeed > 0 && in.SessionName != historyModeDownload {
		return nil, fmt.Errorf("download speed is only valid for Download SDP")
	}
	if in.DownloadSpeed > 0 && !version.Capabilities().DownloadSpeed {
		return nil, fmt.Errorf("download speed is not supported by protocol profile %s", version.StandardName())
	}

	h265Enabled := version.Capabilities().H265 && !in.H265Disabled
	formats := gbVideoPayloadFormats(version, in.H265Disabled)
	mediaType := "video"
	if in.AudioOnly {
		mediaType = "audio"
		formats = []string{"8"}
	}
	media := sdp.Media{
		Description: sdp.MediaDescription{
			Type:     mediaType,
			Port:     in.Port,
			Formats:  formats,
			Protocol: protocol,
		},
	}
	media.AddAttribute("recvonly")
	if in.DownloadSpeed > 0 {
		media.AddAttribute("downloadspeed", strconv.Itoa(in.DownloadSpeed))
	}
	if !in.DirectTCP {
		switch in.StreamMode {
		case 1:
			media.AddAttribute("setup", "passive")
			media.AddAttribute("connection", "new")
		case 2:
			media.AddAttribute("setup", "active")
			media.AddAttribute("connection", "new")
		}
	}
	if in.AudioOnly {
		media.AddAttribute("rtpmap", "8", "PCMA/8000")
	} else {
		media.AddAttribute("rtpmap", "96", "PS/90000")
		media.AddAttribute("rtpmap", "97", "MPEG4/90000")
		media.AddAttribute("rtpmap", "98", "H264/90000")
		if h265Enabled {
			media.AddAttribute("rtpmap", "100", "H265/90000")
		}
	}

	message := &sdp.Message{
		Origin: sdp.Origin{
			Username:    in.ChannelID,
			NetworkType: "IN",
			AddressType: address.Type,
			Address:     address.Canonical,
		},
		Name: in.SessionName,
		Connection: sdp.ConnectionData{
			NetworkType: "IN",
			AddressType: address.Type,
			IP:          address.IP,
		},
		Timing: []sdp.Timing{{Start: in.StartAt, End: in.EndAt}},
		Medias: []sdp.Media{media},
		SSRC:   in.SSRC,
	}
	if in.SessionName != historyModePlay {
		message.URI = strings.TrimSpace(in.URI)
	}

	body := message.Append(nil).AppendTo(nil)
	if in.MediaDescription != "" {
		body = append(body, "f="...)
		body = append(body, in.MediaDescription...)
		body = append(body, '\r', '\n')
	} else if in.AudioOnly {
		body = append(body, "f=v/////a/1/8/1\r\n"...)
	} else if in.SessionName == historyModePlay && !in.DirectTCP {
		// 各版本均允许设备在 G.711/G.723.1/G.729/SVAC 中选择音频编码，G.722.1 为扩展能力；
		// 2022 另增加 AAC。空参数仍保持 f 字段完整，避免把合规设备错误锁定到 G.711。
		// 仅在显式关闭 2022 AAC 能力时保留既有 G.711 兼容回退。
		if version == GBVersion30 && in.AACDisabled {
			body = append(body, "f=v/////a/1/8/1\r\n"...)
		} else {
			body = append(body, "f=v/////a///\r\n"...)
		}
	}
	return body, nil
}

func gbVideoPayloadFormats(version GBProtocolVersion, h265Disabled bool) []string {
	formats := []string{"96", "97", "98"}
	if version.Capabilities().H265 && !h265Disabled {
		formats = append(formats, "100")
	}
	return formats
}

func buildGBInviteSubject(sourceID, sourceSequence, receiverID string) string {
	// 附录 L 要求接收方序列号在同一接收者端同时不重复；发送方序列号本身就是当前媒体会话的唯一标识。
	return fmt.Sprintf("%s:%s,%s:%s", sourceID, sourceSequence, receiverID, sourceSequence)
}

func validGBSSRC(value string) bool {
	if len(value) != 10 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

// validateGBMediaDescription 避免发送缺少分隔项的 f 字段。
func validateGBMediaDescription(value string) error {
	if value == "" {
		return nil
	}
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n \t") {
		return fmt.Errorf("invalid SDP media description: %s", value)
	}
	if !strings.HasPrefix(value, "v/") || strings.Count(value, "a/") != 1 {
		return fmt.Errorf("invalid SDP media description: %s", value)
	}
	index := strings.Index(value, "a/")
	video, audio := value[:index], value[index:]
	if strings.Count(video, "/") != 5 {
		return fmt.Errorf("invalid SDP video description: %s", value)
	}
	if strings.Count(audio, "/") != 3 {
		return fmt.Errorf("invalid SDP audio description: %s", value)
	}
	return nil
}

func validateGBMediaDescriptionForVersion(value string, version GBProtocolVersion) error {
	if err := validateGBMediaDescription(value); err != nil || value == "" {
		return err
	}
	version, ok := ParseGBProtocolVersion(string(version))
	if !ok {
		return fmt.Errorf("invalid GB protocol version: %s", version)
	}

	index := strings.Index(value, "a/")
	videoFields := strings.Split(strings.TrimPrefix(value[:index], "v/"), "/")
	audioFields := strings.Split(strings.TrimPrefix(value[index:], "a/"), "/")
	videoCodecMax := 4
	audioCodecMax, audioBitrateMax, audioSampleRateMax := 4, 8, 4
	if version == GBVersion30 {
		videoCodecMax = 5
		audioCodecMax, audioBitrateMax, audioSampleRateMax = 6, 30, 17
	}

	if _, _, err := parseOptionalSDPDecimal("video codec", videoFields[0], 1, videoCodecMax); err != nil {
		return err
	}
	if err := validateGBVideoResolution(videoFields[1], version); err != nil {
		return err
	}
	if _, _, err := parseOptionalSDPDecimal("video frame rate", videoFields[2], 0, 99); err != nil {
		return err
	}
	if _, _, err := parseOptionalSDPDecimal("video bitrate type", videoFields[3], 1, 2); err != nil {
		return err
	}
	if _, _, err := parseOptionalSDPDecimal("video bitrate", videoFields[4], 0, 100000); err != nil {
		return err
	}

	audioCodec, hasAudioCodec, err := parseOptionalSDPDecimal("audio codec", audioFields[0], 1, audioCodecMax)
	if err != nil {
		return err
	}
	audioBitrate, hasAudioBitrate, err := parseOptionalSDPDecimal("audio bitrate", audioFields[1], 1, audioBitrateMax)
	if err != nil {
		return err
	}
	audioSampleRate, hasAudioSampleRate, err := parseOptionalSDPDecimal("audio sample rate", audioFields[2], 1, audioSampleRateMax)
	if err != nil {
		return err
	}
	if hasAudioCodec && hasAudioBitrate && !gbAudioBitrateAllowed(version, audioCodec, audioBitrate) {
		return fmt.Errorf("invalid SDP audio bitrate %d for codec %d in protocol %s", audioBitrate, audioCodec, version)
	}
	if hasAudioCodec && hasAudioSampleRate && !gbAudioSampleRateAllowed(version, audioCodec, audioSampleRate) {
		return fmt.Errorf("invalid SDP audio sample rate %d for codec %d in protocol %s", audioSampleRate, audioCodec, version)
	}
	return nil
}

func validateGBVideoResolution(value string, version GBProtocolVersion) error {
	if _, _, err := parseOptionalSDPDecimal("video resolution", value, 1, 6); err == nil {
		return nil
	}
	if version == GBVersion30 && validGBVideoResolutionSize(value) {
		return nil
	}
	return fmt.Errorf("invalid SDP video resolution: %s", value)
}

func validGBVideoResolutionSize(value string) bool {
	for _, separator := range []string{"x", "X", "×"} {
		width, height, found := strings.Cut(value, separator)
		if found && validGBVideoResolutionDimension(width) && validGBVideoResolutionDimension(height) {
			return true
		}
	}
	return false
}

func validGBVideoResolutionDimension(value string) bool {
	if value == "" {
		return false
	}
	nonZero := false
	for index := range len(value) {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
		nonZero = nonZero || value[index] != '0'
	}
	return nonZero
}

func parseOptionalSDPDecimal(name, value string, minimum, maximum int) (int, bool, error) {
	if value == "" {
		return 0, false, nil
	}
	for index := range len(value) {
		if value[index] < '0' || value[index] > '9' {
			return 0, false, fmt.Errorf("invalid SDP %s: %s", name, value)
		}
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, false, fmt.Errorf("invalid SDP %s: %s", name, value)
	}
	return parsed, true, nil
}

func gbAudioBitrateAllowed(version GBProtocolVersion, codec, bitrate int) bool {
	switch codec {
	case 1:
		return bitrate == 8
	case 2:
		return bitrate == 1 || bitrate == 2
	case 3:
		return bitrate == 3
	case 4:
		return bitrate >= 4 && bitrate <= 7
	case 5:
		return version == GBVersion30 && (bitrate == 5 || bitrate == 9 || bitrate >= 20 && bitrate <= 30)
	case 6:
		return version == GBVersion30 && bitrate >= 4 && bitrate <= 19
	default:
		return false
	}
}

func gbAudioSampleRateAllowed(version GBProtocolVersion, codec, sampleRate int) bool {
	switch codec {
	case 1, 2, 3:
		return sampleRate == 1
	case 4:
		return sampleRate >= 2 && sampleRate <= 4
	case 5:
		return version == GBVersion30 && (sampleRate == 3 || sampleRate == 4 || sampleRate == 9 || sampleRate == 11 || sampleRate >= 15 && sampleRate <= 17)
	case 6:
		return version == GBVersion30 && (sampleRate == 1 || sampleRate >= 3 && sampleRate <= 14)
	default:
		return false
	}
}
