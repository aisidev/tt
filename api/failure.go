package api

import "fmt"

// BoundaryError preserves a classified refusal across service boundaries.
// Message is diagnostic data and must never drive retry or authorization.
type BoundaryError struct {
	Class   string `json:"class"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *BoundaryError) Error() string { return fmt.Sprintf("%s/%s: %s", e.Class, e.Code, e.Message) }
func (e *BoundaryError) Status() int {
	switch e.Class {
	case "denied":
		return 403
	case "terminal":
		if e.Code == "not_found" {
			return 404
		}
		return 400
	case "state":
		return 409
	case "rate_limited":
		return 429
	default:
		return 503
	}
}
