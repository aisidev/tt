package model

// StopOptions overrides one explicit stop. Omitted fields retain engine defaults.
// Force skips grace; GracefulOnly refuses to kill when grace is exhausted.
type StopOptions struct {
	GraceSeconds *uint32 `json:"grace_seconds,omitempty"`
	Force        bool    `json:"force,omitempty"`
	GracefulOnly bool    `json:"graceful_only,omitempty"`
}

// ValidStopOptions checks policy combinations before any engine effect.
func ValidStopOptions(options StopOptions) bool {
	return !(options.Force && options.GracefulOnly) && (options.GraceSeconds == nil || (*options.GraceSeconds > 0 && *options.GraceSeconds <= 300))
}
