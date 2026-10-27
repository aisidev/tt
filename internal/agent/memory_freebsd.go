package agent

import (
	"context"
	"strconv"
	"strings"

	"github.com/aisidev/tt/internal/runcmd"
)

func totalMemory(ctx context.Context) (uint32, bool) {
	out, err := runcmd.Run(ctx, "sysctl", "-n", "hw.physmem")
	if err != nil {
		return 0, false
	}
	bytes, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(string(out.Stdout)), "+"), 10, 64)
	return uint32(bytes / (1024 * 1024)), err == nil
}
