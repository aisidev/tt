package engine

import (
	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/model"
	"time"
)

type policyEngine interface {
	withStopPolicy(model.StopOptions) Engine
}

// ApplyStopPolicy returns an isolated engine view. It never mutates fleet-wide
// defaults or shares per-call policy with another VM.
func ApplyStopPolicy(e Engine, options model.StopOptions) (Engine, error) {
	if options.Force && options.GracefulOnly {
		return nil, &api.BoundaryError{Class: "terminal", Code: "invalid_stop_policy", Message: "force and graceful_only are mutually exclusive"}
	}
	if options.GraceSeconds != nil && (*options.GraceSeconds == 0 || *options.GraceSeconds > 300) {
		return nil, &api.BoundaryError{Class: "terminal", Code: "invalid_stop_policy", Message: "grace_seconds must be within 1..300"}
	}
	if options.GraceSeconds == nil && !options.Force && !options.GracefulOnly {
		return e, nil
	}
	if configurable, ok := e.(policyEngine); ok {
		return configurable.withStopPolicy(options), nil
	}
	return nil, &api.BoundaryError{Class: "terminal", Code: "unimplemented", Message: "engine does not support per-call stop policy"}
}
func policyGrace(current time.Duration, options model.StopOptions) time.Duration {
	if options.Force {
		return 0
	}
	if options.GraceSeconds != nil {
		return time.Duration(*options.GraceSeconds) * time.Second
	}
	return current
}
func gracefulTimeout() error {
	return &api.BoundaryError{Class: "state", Code: "graceful_timeout", Message: "guest did not stop gracefully; retained without force"}
}
