package gbs

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

// OutgoingSubscriptionState 是平台向设备或下级平台建立的事件订阅运行态。
// 仅暴露运维所需字段，不返回 SIP 对话、认证信息或内部索引键。
type OutgoingSubscriptionState struct {
	DeviceID          string    `json:"device_id"`
	TargetID          string    `json:"target_id"`
	Event             string    `json:"event"`
	Status            string    `json:"status"`
	Expires           int       `json:"expires"`
	ExpiresAt         time.Time `json:"expires_at"`
	RefreshAt         time.Time `json:"refresh_at,omitempty"`
	Persisted         bool      `json:"persisted,omitempty"`
	UpdatedAt         time.Time `json:"updated_at,omitempty"`
	NextAttemptAt     time.Time `json:"next_attempt_at,omitempty"`
	LastError         string    `json:"last_error,omitempty"`
	RetryBlocked      bool      `json:"retry_blocked,omitempty"`
	TerminationReason string    `json:"termination_reason,omitempty"`

	Refreshing    bool `json:"refreshing"`
	CancelPending bool `json:"cancel_pending"`

	NotifyCSeq      uint32    `json:"notify_cseq,omitempty"`
	NotifyExpiresAt time.Time `json:"notify_expires_at,omitempty"`

	StartAlarmPriority string `json:"start_alarm_priority,omitempty"`
	EndAlarmPriority   string `json:"end_alarm_priority,omitempty"`
	AlarmMethod        string `json:"alarm_method,omitempty"`
	AlarmType          string `json:"alarm_type,omitempty"`
	StartAlarmTime     string `json:"start_alarm_time,omitempty"`
	EndAlarmTime       string `json:"end_alarm_time,omitempty"`
	StartTime          string `json:"start_time,omitempty"`
	EndTime            string `json:"end_time,omitempty"`
	Interval           int    `json:"interval,omitempty"`
}

// OutgoingSubscriptionStates 返回指定设备当前仍在运行态索引中的出向订阅。
// 尚未收到成功响应的临时对话不会对外暴露，避免把请求中状态误报为有效订阅。
func (g *GB28181API) OutgoingSubscriptionStates(ctx context.Context, deviceID string) ([]OutgoingSubscriptionState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return nil, fmt.Errorf("device id is required")
	}
	if g == nil {
		return []OutgoingSubscriptionState{}, nil
	}

	now := time.Now()
	states := make([]OutgoingSubscriptionState, 0)
	stateIndexes := make(map[string]int)
	var rangeErr error
	g.outgoingSubscriptions.Range(func(rawKey, value any) bool {
		if err := ctx.Err(); err != nil {
			rangeErr = err
			return false
		}
		dialog, ok := value.(*outgoingSubscriptionDialog)
		if !ok || dialog == nil {
			return true
		}

		dialog.mu.Lock()
		if dialog.response == nil || strings.TrimSpace(dialog.deviceID) != deviceID {
			dialog.mu.Unlock()
			return true
		}
		body := append([]byte(nil), dialog.requestBody...)
		state := OutgoingSubscriptionState{
			DeviceID:      strings.TrimSpace(dialog.deviceID),
			TargetID:      strings.TrimSpace(dialog.targetID),
			Expires:       dialog.expires,
			ExpiresAt:     dialog.expiresAt,
			RefreshAt:     dialog.refreshAt,
			Refreshing:    dialog.refreshing,
			CancelPending: dialog.cancelPending.Load(),
		}
		dialog.mu.Unlock()

		notify := dialog.snapshotNotifyDialog()
		state.NotifyCSeq = notify.cseq
		state.NotifyExpiresAt = notify.expiresAt
		var request subscribeEventRequest
		if len(body) > 0 && sip.XMLDecode(body, &request) == nil {
			state.Event = outgoingSubscriptionEventName(request.CmdType)
			state.StartAlarmPriority = request.StartAlarmPriority
			state.EndAlarmPriority = request.EndAlarmPriority
			state.AlarmMethod = request.AlarmMethod
			state.AlarmType = request.AlarmType
			state.StartAlarmTime = request.StartAlarmTime
			state.EndAlarmTime = request.EndAlarmTime
			state.StartTime = request.StartTime
			state.EndTime = request.EndTime
			if request.Interval != nil {
				state.Interval = *request.Interval
			}
		}
		if state.Event == "" {
			state.Event = outgoingSubscriptionEventName(notify.cmdType)
		}
		switch {
		case state.CancelPending:
			state.Status = "terminating"
		case !state.ExpiresAt.IsZero() && !now.Before(state.ExpiresAt):
			state.Status = "expired"
		case state.Refreshing:
			state.Status = "refreshing"
		default:
			state.Status = "active"
		}
		states = append(states, state)
		if key, ok := rawKey.(string); ok {
			stateIndexes[key] = len(states) - 1
		}
		return true
	})
	if rangeErr != nil {
		return nil, rangeErr
	}
	intentRecords, err := g.listManualSubscriptionIntentRecords(ctx, deviceID, maxManualSubscriptionIntentStates)
	if err != nil {
		return nil, err
	}
	for _, record := range intentRecords {
		if index, exists := stateIndexes[record.Key]; exists {
			states[index].Persisted = true
			continue
		}
		states = append(states, manualSubscriptionIntentStateToOutgoing(record))
	}
	sort.Slice(states, func(i, j int) bool {
		if states[i].TargetID != states[j].TargetID {
			return states[i].TargetID < states[j].TargetID
		}
		if states[i].Event != states[j].Event {
			return states[i].Event < states[j].Event
		}
		return states[i].ExpiresAt.Before(states[j].ExpiresAt)
	})
	return states, nil
}

func outgoingSubscriptionEventName(cmdType string) string {
	switch strings.ToLower(strings.TrimSpace(cmdType)) {
	case "alarm":
		return "alarm"
	case "catalog":
		return "catalog"
	case "mobileposition", "mobile_position", "device_position":
		return "mobile_position"
	case "ptzposition", "ptz_position":
		return "ptz_position"
	default:
		return strings.ToLower(strings.TrimSpace(cmdType))
	}
}
