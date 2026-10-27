// Wire structures transcribed from the pinned Rust backup.rs.
package model

type BackupRequest struct {
	OperationID      string       `json:"operation_id"`
	ExpectedRevision string       `json:"expected_revision"`
	Action           BackupAction `json:"action"`
	Generation       *string      `json:"generation"`
}

func (v *BackupRequest) UnmarshalJSON(data []byte) error {
	type plain BackupRequest
	var next plain
	if err := DecodeRecord(data, &next, []string{"operation_id", "expected_revision", "action"}, true, "generation"); err != nil {
		return err
	}
	*v = BackupRequest(next)
	return nil
}

type BackupGeneration struct {
	ID             string        `json:"id"`
	Backend        BackupBackend `json:"backend"`
	RootBytes      uint64        `json:"root_bytes"`
	CreatedAt      uint64        `json:"created_at"`
	Identity       string        `json:"identity"`
	Dependencies   string        `json:"dependencies"`
	RestoreStaging bool          `json:"restore_staging"`
}

func (v *BackupGeneration) UnmarshalJSON(data []byte) error {
	type plain BackupGeneration
	var next plain
	if err := DecodeRecord(data, &next, []string{"id", "backend", "root_bytes", "created_at", "identity", "dependencies", "restore_staging"}, false); err != nil {
		return err
	}
	*v = BackupGeneration(next)
	return nil
}

type BackupPending struct {
	Request      BackupRequest    `json:"request"`
	Artifact     BackupGeneration `json:"artifact"`
	DiskIdentity string           `json:"disk_identity"`
	Error        *string          `json:"error"`
}

func (v *BackupPending) UnmarshalJSON(data []byte) error {
	type plain BackupPending
	var next plain
	if err := DecodeRecord(data, &next, []string{"request", "artifact", "disk_identity"}, false, "error"); err != nil {
		return err
	}
	*v = BackupPending(next)
	return nil
}

type BackupReceipt struct {
	Request     BackupRequest `json:"request"`
	CompletedAt uint64        `json:"completed_at"`
	Error       *string       `json:"error"`
}

func (v *BackupReceipt) UnmarshalJSON(data []byte) error {
	type plain BackupReceipt
	var next plain
	if err := DecodeRecord(data, &next, []string{"request", "completed_at"}, false, "error"); err != nil {
		return err
	}
	*v = BackupReceipt(next)
	return nil
}

type BackupState struct {
	Revision                  string                 `json:"revision"`
	Sequence                  uint64                 `json:"sequence"`
	Current                   *BackupGeneration      `json:"current"`
	Pending                   *BackupPending         `json:"pending"`
	Retired                   List[BackupGeneration] `json:"retired"`
	LastResult                *BackupReceipt         `json:"last_result"`
	CleanupError              *string                `json:"cleanup_error"`
	CleanupRetryAt            uint64                 `json:"cleanup_retry_at"`
	CleanupFailures           uint32                 `json:"cleanup_failures"`
	Forwarding                *BackupRequest         `json:"forwarding"`
	ForwardingReservedMiB     uint32                 `json:"forwarding_reserved_mib"`
	ForwardingSettledSequence *uint64                `json:"forwarding_settled_sequence"`
}

func (v *BackupState) UnmarshalJSON(data []byte) error {
	type plain BackupState
	var next plain
	if err := DecodeRecord(data, &next, []string{"revision", "sequence", "retired", "cleanup_retry_at", "cleanup_failures", "forwarding_reserved_mib"}, false, "current", "pending", "last_result", "cleanup_error", "forwarding"); err != nil {
		return err
	}
	*v = BackupState(next)
	return nil
}
