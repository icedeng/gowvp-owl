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
	case historyModePlayback, historyModeDownload:
		if strings.TrimSpace(in.URI) == "" {
			return nil, fmt.Errorf("%s SDP URI is required", in.SessionName)
		}
		if in.StartAt.IsZero() || in.EndAt.IsZero() || !in.EndAt.After(in.StartAt) {
			return nil, fmt.Errorf("invalid %s SDP time range", in.SessionName)
		}
	default:
		return nil, fmt.Errorf("unsupported SDP session name: %s", in.SessionName)
	}
	if err := validateGBMediaDescription(in.MediaDescription); err != nil {
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

	formats := []string{"96", "97", "98"}
	h265Enabled := version.Capabilities().H265 && !in.H265Disabled
	if h265Enabled {
		formats = append(formats, "99")
	}
	video := sdp.Media{
		Description: sdp.MediaDescription{
			Type:     "video",
			Port:     in.Port,
			Formats:  formats,
			Protocol: protocol,
		},
	}
	video.AddAttribute("recvonly")
	if in.DownloadSpeed > 0 {
		video.AddAttribute("downloadspeed", strconv.Itoa(in.DownloadSpeed))
	}
	if !in.DirectTCP {
		switch in.StreamMode {
		case 1:
			video.AddAttribute("setup", "passive")
			video.AddAttribute("connection", "new")
		case 2:
			video.AddAttribute("setup", "active")
			video.AddAttribute("connection", "new")
		}
	}
	video.AddAttribute("rtpmap", "96", "PS/90000")
	video.AddAttribute("rtpmap", "97", "MPEG4/90000")
	video.AddAttribute("rtpmap", "98", "H264/90000")
	if h265Enabled {
		video.AddAttribute("rtpmap", "99", "H265/90000")
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
		Medias: []sdp.Media{video},
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
	} else if in.SessionName == historyModePlay && !in.DirectTCP {
		// 2022 新增 AAC。未指定媒体描述时不锁定内部音频编码，由设备在 G.711/SVAC/AAC 中选择；
		// 旧版本继续声明 G.711 A-law，保持既有设备兼容。
		if version.Capabilities().AAC && !in.AACDisabled {
			body = append(body, "f=v/////a///\r\n"...)
		} else {
			body = append(body, "f=v/////a/1/8/1\r\n"...)
		}
	}
	return body, nil
}

func buildGBInviteSubject(sourceID, sourceSequence, receiverID string) string {
	return fmt.Sprintf("%s:%s,%s:0", sourceID, sourceSequence, receiverID)
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
	video, audio := value, ""
	if index := strings.Index(value, "a/"); index >= 0 {
		video, audio = value[:index], value[index:]
	}
	if video != "" && (!strings.HasPrefix(video, "v/") || strings.Count(video, "/") != 5) {
		return fmt.Errorf("invalid SDP video description: %s", value)
	}
	if audio != "" && (!strings.HasPrefix(audio, "a/") || strings.Count(audio, "/") != 3) {
		return fmt.Errorf("invalid SDP audio description: %s", value)
	}
	if video == "" && audio == "" {
		return fmt.Errorf("invalid SDP media description: %s", value)
	}
	return nil
}
