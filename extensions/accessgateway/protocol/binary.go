package protocol

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
)

const (
	UDPRTP           byte = 0x01
	UDPRTCP          byte = 0x02
	TCPBytes         byte = 0x03
	FileBytes        byte = 0x04
	EOF              byte = 0x05
	BinaryHeaderSize      = 12
)

type BinaryMessage struct {
	Kind     byte
	Sequence uint64
	Payload  []byte
}

func EncodeBinary(m BinaryMessage) ([]byte, error) {
	if e := validateBinary(m); e != nil {
		return nil, e
	}
	b := make([]byte, BinaryHeaderSize+len(m.Payload))
	b[0] = Version
	b[1] = m.Kind
	binary.BigEndian.PutUint64(b[4:12], m.Sequence)
	copy(b[12:], m.Payload)
	return b, nil
}
func DecodeBinary(b []byte) (BinaryMessage, error) {
	if len(b) < BinaryHeaderSize || len(b) > MaxBinaryBytes || b[0] != Version || b[2] != 0 || b[3] != 0 {
		return BinaryMessage{}, invalid("binary header")
	}
	m := BinaryMessage{Kind: b[1], Sequence: binary.BigEndian.Uint64(b[4:12]), Payload: b[12:]}
	return m, validateBinary(m)
}
func validateBinary(m BinaryMessage) error {
	if m.Sequence == 0 {
		return invalid("binary sequence")
	}
	switch m.Kind {
	case UDPRTP:
		if len(m.Payload) > 65507 {
			return invalid("RTP size")
		}
		return ValidateRTP(m.Payload, nil)
	case UDPRTCP:
		if len(m.Payload) > 65507 {
			return invalid("RTCP size")
		}
		return ValidateRTCP(m.Payload)
	case TCPBytes, FileBytes:
		if len(m.Payload) < 1 || len(m.Payload) > 65536 {
			return invalid("stream chunk")
		}
	case EOF:
		if len(m.Payload) != 0 {
			return invalid("EOF payload")
		}
	default:
		return invalid("binary kind")
	}
	return nil
}

// BinaryState 每条 WSS 的每个方向独立持有，EOF 后拒绝继续传送。
type BinaryState struct {
	Sequence  uint64
	Ended     bool
	Transfer  bool
	WireMode  string
	Direction string
	ToA       bool
}

func (s *BinaryState) Accept(m BinaryMessage) error {
	if s.Ended || s.Sequence == math.MaxUint64 || m.Sequence != s.Sequence+1 {
		return invalid("out of order or ended binary stream")
	}
	if e := validateBinary(m); e != nil {
		return e
	}
	if s.Direction == "device_to_a" && !s.ToA || s.Direction == "a_to_device" && s.ToA {
		// 收流也需要 RTCP 回程；TCP 复用方向在完整帧验证处区分。
		if m.Kind != UDPRTCP && !(m.Kind == TCPBytes && s.WireMode != "udp") && m.Kind != EOF {
			return invalid("binary direction")
		}
	}
	if s.Transfer {
		if m.Kind != FileBytes && m.Kind != EOF {
			return invalid("transfer kind")
		}
	} else if s.WireMode == "udp" {
		if m.Kind != UDPRTP && m.Kind != UDPRTCP {
			return invalid("UDP kind")
		}
	} else if m.Kind != TCPBytes && m.Kind != EOF {
		return invalid("TCP kind")
	}
	s.Sequence = m.Sequence
	s.Ended = m.Kind == EOF
	return nil
}
func ValidateRTP(b []byte, expectedSSRC *string) error {
	if len(b) < 12 || b[0]>>6 != 2 {
		return invalid("RTP header")
	}
	length := 12 + int(b[0]&15)*4
	if length > len(b) {
		return invalid("RTP CSRC")
	}
	if b[0]&0x10 != 0 {
		if length+4 > len(b) {
			return invalid("RTP extension")
		}
		length += 4 + int(binary.BigEndian.Uint16(b[length+2:length+4]))*4
		if length > len(b) {
			return invalid("RTP extension length")
		}
	}
	if b[0]&0x20 != 0 {
		padding := int(b[len(b)-1])
		if padding == 0 || padding > len(b)-length {
			return invalid("RTP padding")
		}
	}
	if expectedSSRC != nil {
		n, e := strconv.ParseUint(*expectedSSRC, 10, 32)
		if e != nil || uint32(n) != binary.BigEndian.Uint32(b[8:12]) {
			return invalid("RTP SSRC")
		}
	}
	return nil
}
func ValidateRTCP(b []byte) error {
	if len(b) < 4 || len(b) > 65507 || len(b)%4 != 0 {
		return invalid("RTCP size")
	}
	for len(b) > 0 {
		if len(b) < 4 || b[0]>>6 != 2 || b[1] < 192 || b[1] > 223 {
			return invalid("RTCP header")
		}
		n := (int(binary.BigEndian.Uint16(b[2:4])) + 1) * 4
		if n > len(b) {
			return invalid("RTCP length")
		}
		if b[0]&0x20 != 0 {
			if n != len(b) || int(b[n-1]) == 0 || int(b[n-1]) > n-4 {
				return invalid("RTCP padding")
			}
		}
		b = b[n:]
	}
	return nil
}

// RTPFramer 在 TCP read/WSS 消息任意分片时校验 RFC4571，不改变原字节。
// 校验完一帧才允许将设备连接认作该租约的可信对端。
type RTPFramer struct {
	pending   []byte
	SSRC      *string
	AllowRTP  bool
	AllowRTCP bool
}

func (f *RTPFramer) Feed(data []byte) ([]byte, error) {
	if len(data) == 0 || len(data) > 65536 {
		return nil, invalid("TCP fragment")
	}
	f.pending = append(f.pending, data...)
	out := []byte{}
	for len(f.pending) >= 2 {
		size := int(binary.BigEndian.Uint16(f.pending[:2]))
		if size == 0 {
			return nil, invalid("empty RFC4571 frame")
		}
		if len(f.pending) < size+2 {
			break
		}
		frame := f.pending[2 : size+2]
		var err error
		if len(frame) >= 2 && frame[1] >= 192 && frame[1] <= 223 {
			if !f.AllowRTCP {
				return nil, invalid("unexpected RTCP")
			}
			err = ValidateRTCP(frame)
		} else {
			if !f.AllowRTP {
				return nil, invalid("unexpected RTP")
			}
			err = ValidateRTP(frame, f.SSRC)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, f.pending[:size+2]...)
		f.pending = f.pending[size+2:]
	}
	if len(f.pending) > 65537 {
		return nil, fmt.Errorf("RFC4571 buffer limit")
	}
	return out, nil
}
func (f *RTPFramer) Finish() error {
	if len(f.pending) != 0 {
		return invalid("truncated RFC4571 frame")
	}
	return nil
}
