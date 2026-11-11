package protocol

type TransferSpec struct {
	SubjectType    string   `json:"subject_type,omitempty"`
	TaskID         string   `json:"task_id"`
	SubjectID      string   `json:"subject_id"`
	Kind           string   `json:"kind"`
	Direction      string   `json:"direction"`
	MaxBytes       Decimal  `json:"max_bytes"`
	TTLMS          int      `json:"ttl_ms"`
	ContentType    string   `json:"content_type"`
	ExpectedSHA256 *string  `json:"expected_sha256"`
	WireMode       string   `json:"wire_mode,omitempty"`
	SourceIPs      []string `json:"source_ips,omitempty"`
	ContentLength  *Decimal `json:"content_length,omitempty"`
	SHA256         *string  `json:"sha256,omitempty"`
	RangeOffset    *Decimal `json:"range_offset,omitempty"`
	RangeLength    *Decimal `json:"range_length,omitempty"`
}
type TransferOpen struct {
	OperationID string       `json:"operation_id"`
	Spec        TransferSpec `json:"spec"`
}
type TransferOpened struct {
	TransferID  string    `json:"transfer_id"`
	PublicURL   *string   `json:"public_url"`
	PublicTCP   *Endpoint `json:"public_tcp"`
	ExpiresInMS int       `json:"expires_in_ms"`
	MaxBytes    Decimal   `json:"max_bytes"`
}
type TransferConnect struct {
	OperationID string   `json:"operation_id"`
	TransferID  string   `json:"transfer_id"`
	Target      Endpoint `json:"target"`
}
type TransferConnected struct {
	TransferID string `json:"transfer_id"`
	Connected  bool   `json:"connected"`
}
type TransferClose struct {
	OperationID string `json:"operation_id"`
	TransferID  string `json:"transfer_id"`
	Reason      string `json:"reason"`
}
type TransferClosed struct {
	TransferID     string `json:"transfer_id"`
	Closed         bool   `json:"closed"`
	CleanupPending bool   `json:"cleanup_pending"`
}
type TransferFinish struct {
	OperationID   string  `json:"operation_id"`
	TransferID    string  `json:"transfer_id"`
	ReceivedBytes Decimal `json:"received_bytes"`
	SHA256        string  `json:"sha256"`
}
type TransferFinished struct {
	TransferID string `json:"transfer_id"`
	Committed  bool   `json:"committed"`
}
type TransferQuery struct {
	TransferID string `json:"transfer_id"`
}
type TransferQueryResult struct {
	TransferID    string      `json:"transfer_id"`
	Ref           ResourceRef `json:"ref"`
	Phase         string      `json:"phase"`
	State         string      `json:"state"`
	BytesReceived Decimal     `json:"bytes_received"`
	BytesSent     Decimal     `json:"bytes_sent"`
	SHA256        *string     `json:"sha256"`
	Committed     bool        `json:"committed"`
}
type TransferReady struct {
	TransferID string `json:"transfer_id"`
}
type TransferProgress struct {
	TransferID string  `json:"transfer_id"`
	Bytes      Decimal `json:"bytes"`
	Phase      string  `json:"phase"`
}
type TransferSent struct {
	TransferID string  `json:"transfer_id"`
	Bytes      Decimal `json:"bytes"`
}
type TransferCompleted struct {
	TransferID string  `json:"transfer_id"`
	Committed  bool    `json:"committed"`
	Bytes      Decimal `json:"bytes"`
	SHA256     string  `json:"sha256"`
}
type TransferFailed struct {
	TransferID string `json:"transfer_id"`
	Error      Error  `json:"error"`
}
