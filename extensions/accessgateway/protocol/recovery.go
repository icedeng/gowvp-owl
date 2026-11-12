package protocol

// AlarmRecord 仅保存报警文字和原始时间，不携带媒体与旧 SIP 事务。
type AlarmRecord struct {
	ID              string  `json:"id"`
	SubjectID       string  `json:"subject_id"`
	BindingRevision Decimal `json:"binding_revision"`
	DeviceCode      string  `json:"device_code"`
	ChannelCode     string  `json:"channel_code"`
	ReceivedAt      string  `json:"received_at"`
	Historical      bool    `json:"historical"`
	AlarmTime       string  `json:"alarm_time"`
	Priority        string  `json:"priority"`
	Method          string  `json:"method"`
	Type            string  `json:"type"`
	EventType       *int    `json:"event_type"`
	Description     string  `json:"description"`
	Longitude       string  `json:"longitude"`
	Latitude        string  `json:"latitude"`
}
type AlarmPull struct {
	After string `json:"after"`
}
type AlarmBatch struct {
	StoreError bool          `json:"store_error"`
	Items      []AlarmRecord `json:"items"`
	Next       string        `json:"next"`
}
type AlarmAck struct {
	IDs []string `json:"ids"`
}
type AlarmAckResult struct {
	Removed int `json:"removed"`
}
type ResumeList struct {
	After string `json:"after"`
}
type ResumeFlow struct {
	SubjectID       string  `json:"subject_id"`
	BindingRevision Decimal `json:"binding_revision"`
	Route           Route   `json:"route"`
	ExpiresAt       string  `json:"expires_at"`
	LastSeenAt      string  `json:"last_seen_at"`
}
type ResumeFlows struct {
	Items []ResumeFlow `json:"items"`
	Next  string       `json:"next"`
}

type MediaInspect struct {
	SessionID string `json:"session_id"`
}
type MediaInspection struct {
	FirstPacketMS *int64 `json:"first_packet_ms"`
}
