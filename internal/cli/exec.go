package cli

import (
	"context"
	"os/exec"
	"time"

	"github.com/aisidev/tt/internal/runcmd"
)

// A zero CLI timeout explicitly denotes a Rust call site without a time limit.
// Host-side engine operations continue to use runcmd's bounded entry points.
func toolOutput(ctx context.Context, command *exec.Cmd, timeout time.Duration) (runcmd.Result, error) {
	if timeout == 0 {
		return runcmd.OutputUnbounded(ctx, command)
	}
	return runcmd.Output(ctx, command, timeout)
}
