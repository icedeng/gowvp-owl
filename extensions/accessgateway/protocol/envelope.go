package protocol

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// payloadType 是消息名、方向分类与 Go 类型的唯一字典。
func payloadType(name string) (string, any, string) {
	switch name {
	case "gateway.info":
		return "request", &GatewayInfoRequest{}, "gateway.info.result"
	case "gateway.info.result":
		return "response", &GatewayInfo{}, ""
	case "media.inspect":
		return "request", &MediaInspect{}, "media.inspect.result"
	case "media.inspect.result":
		return "response", &MediaInspection{}, ""
	case "alarm.pull":
		return "request", &AlarmPull{}, "alarm.pull.result"
	case "alarm.pull.result":
		return "response", &AlarmBatch{}, ""
	case "alarm.ack":
		return "request", &AlarmAck{}, "alarm.ack.result"
	case "alarm.ack.result":
		return "response", &AlarmAckResult{}, ""
	case "sip.resume.list":
		return "request", &ResumeList{}, "sip.resume.list.result"
	case "sip.resume.list.result":
		return "response", &ResumeFlows{}, ""
	case "hello":
		return "request", &Hello{}, "hello.ack"
	case "hello.ack":
		return "response", &HelloAck{}, ""
	case "heartbeat":
		return "request", &Heartbeat{}, "heartbeat.ack"
	case "heartbeat.ack":
		return "response", &HeartbeatAck{}, ""
	case "sip.flow.open":
		return "request", &FlowOpen{}, "sip.flow.open.result"
	case "sip.flow.open.result":
		return "response", &FlowOpenResult{}, ""
	case "sip.flow.close":
		return "request", &FlowClose{}, "sip.flow.close.result"
	case "sip.flow.close.result":
		return "response", &FlowCloseResult{}, ""
	case "sip.binding.update":
		return "request", &BindingUpdate{}, "sip.binding.update.result"
	case "sip.binding.update.result":
		return "response", &BindingResult{}, ""
	case "sip.packet":
		return "event", &SIPPacket{}, ""
	case "sip.flow.closed":
		return "event", &FlowClosed{}, ""
	case "media.open":
		return "request", &MediaOpen{}, "media.open.result"
	case "media.open.result":
		return "response", &MediaOpenResult{}, ""
	case "media.connect":
		return "request", &MediaConnect{}, "media.connect.result"
	case "media.connect.result":
		return "response", &MediaConnectResult{}, ""
	case "media.close":
		return "request", &MediaClose{}, "media.close.result"
	case "media.close.result":
		return "response", &MediaCloseResult{}, ""
	case "media.ready":
		return "event", &MediaReady{}, ""
	case "media.failed":
		return "event", &MediaFailed{}, ""
	case "attachment.issue":
		return "request", &AttachmentIssue{}, "attachment.issue.result"
	case "attachment.issue.result":
		return "response", &AttachmentResult{}, ""
	case "resources.list":
		return "request", &ResourcesList{}, "resources.list.result"
	case "resources.list.result":
		return "response", &ResourcesResult{}, ""
	case "resources.close":
		return "request", &ResourceClose{}, "resources.close.result"
	case "resources.close.result":
		return "response", &ResourceCloseResult{}, ""
	case "config.prepare":
		return "request", &ConfigPrepare{}, "config.prepare.result"
	case "config.prepare.result":
		return "response", &ConfigPrepared{}, ""
	case "config.apply":
		return "request", &ConfigApply{}, "config.result"
	case "config.result":
		return "response", &ConfigResult{}, ""
	case "gateway.drain":
		return "request", &Drain{}, "gateway.drain.result"
	case "gateway.drain.result":
		return "response", &DrainResult{}, ""
	case "gateway.resume":
		return "request", &Resume{}, "gateway.resume.result"
	case "gateway.resume.result":
		return "response", &ResumeResult{}, ""
	case "transfer.open":
		return "request", &TransferOpen{}, "transfer.open.result"
	case "transfer.open.result":
		return "response", &TransferOpened{}, ""
	case "transfer.connect":
		return "request", &TransferConnect{}, "transfer.connect.result"
	case "transfer.connect.result":
		return "response", &TransferConnected{}, ""
	case "transfer.close":
		return "request", &TransferClose{}, "transfer.close.result"
	case "transfer.close.result":
		return "response", &TransferClosed{}, ""
	case "transfer.finish":
		return "request", &TransferFinish{}, "transfer.finish.result"
	case "transfer.finish.result":
		return "response", &TransferFinished{}, ""
	case "transfer.query":
		return "request", &TransferQuery{}, "transfer.query.result"
	case "transfer.query.result":
		return "response", &TransferQueryResult{}, ""
	case "transfer.ready":
		return "event", &TransferReady{}, ""
	case "transfer.progress":
		return "event", &TransferProgress{}, ""
	case "transfer.sent":
		return "event", &TransferSent{}, ""
	case "transfer.completed":
		return "event", &TransferCompleted{}, ""
	case "transfer.failed":
		return "event", &TransferFailed{}, ""
	case "status.snapshot":
		return "event", &StatusSnapshot{}, ""
	case "metrics.snapshot":
		return "event", &MetricsSnapshot{}, ""
	}
	return "", nil, ""
}
func ResponseType(request string) string { _, _, name := payloadType(request); return name }

func DecodeEnvelope(data []byte) (Envelope, any, error) {
	var env Envelope
	if len(data) > MaxControlBytes {
		return env, nil, fmt.Errorf("control message too large")
	}
	if err := DecodeStrict(data, &env); err != nil {
		return env, nil, err
	}
	kind, payload, _ := payloadType(env.Type)
	if payload == nil || env.Kind != kind || env.Version != Version || !ValidID(env.RequestID) || !ValidGatewayID(env.GatewayID) || len(env.PlatformID) < 1 || len(env.PlatformID) > 64 {
		return env, nil, fmt.Errorf("invalid envelope")
	}
	if _, err := env.OwnerEpoch.Int64(); err != nil {
		return env, nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return env, nil, err
	}
	if env.Kind != "response" {
		if _, ok := fields["error"]; ok {
			return env, nil, fmt.Errorf("request/event cannot carry error")
		}
		if _, ok := fields["reply_to"]; ok {
			return env, nil, fmt.Errorf("request/event cannot carry reply_to")
		}
	}
	if env.Kind == "response" {
		if _, ok := fields["error"]; !ok {
			return env, nil, fmt.Errorf("response requires error field")
		}

		if !ValidID(env.ReplyTo) {
			return env, nil, fmt.Errorf("missing reply_to")
		}
	} else if env.ReplyTo != "" || env.Error != nil {
		return env, nil, fmt.Errorf("unexpected response fields")
	}
	if env.Type == "hello" || env.Type == "hello.ack" && env.Error != nil {
		if env.Generation != nil || env.ConnectionEpoch != nil {
			return env, nil, fmt.Errorf("hello must have null epochs")
		}
	} else if env.Generation == nil || !ValidID(*env.Generation) || env.ConnectionEpoch == nil {
		return env, nil, fmt.Errorf("missing epochs")
	}
	if env.ConnectionEpoch != nil {
		n, err := env.ConnectionEpoch.Int64()
		if err != nil || n == 0 {
			return env, nil, fmt.Errorf("invalid connection_epoch")
		}
	}
	if env.Error != nil {
		if env.Kind != "response" || env.Error.Code == "" || env.Error.RetryAfterMS < 0 {
			return env, nil, fmt.Errorf("invalid error")
		}
		var empty map[string]any
		if err := DecodeStrict(env.Payload, &empty); err != nil || len(empty) != 0 {
			return env, nil, fmt.Errorf("error payload must be empty object")
		}
		return env, nil, nil
	}
	if err := DecodeStrict(env.Payload, payload); err != nil {
		return env, nil, err
	}
	if err := Validate(payload); err != nil {
		return env, nil, err
	}
	return env, payload, nil
}

// Payload 按共享类型编码。整个信封的身份由已经认证的连接补全。
func Payload(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func DecodeSIP(p SIPPacket) ([]byte, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(p.RawBase64)
	if err != nil || len(b) == 0 || len(b) > (1<<20)+(64<<10) {
		return nil, fmt.Errorf("invalid SIP bytes")
	}
	return b, nil
}

// MarshalJSON 仅响应携带 error（成功为 null），避免请求凭空出现响应字段。
func (e Envelope) MarshalJSON() ([]byte, error) {
	type plain Envelope
	data, err := json.Marshal(plain(e))
	if err != nil {
		return nil, err
	}
	if e.Kind != "response" {
		return data, nil
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	fields["error"], err = json.Marshal(e.Error)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}
