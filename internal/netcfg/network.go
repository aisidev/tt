package netcfg

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

// Network's defaults use fixed Rust-compatible paths. Internal seams let tests
// exercise command plans without changing the real host's forwarding or devices.
type Network struct {
	runDir    string
	execute   func(context.Context, *exec.Cmd, time.Duration) (runcmd.Result, error)
	writeFile func(string, []byte, os.FileMode) error
}

func New() *Network {
	return &Network{runDir: model.RunDir, execute: runcmd.Output, writeFile: os.WriteFile}
}
func (n *Network) output(ctx context.Context, timeout time.Duration, name string, args ...string) (runcmd.Result, error) {
	return n.execute(ctx, exec.Command(name, args...), timeout)
}
func (n *Network) run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := n.output(ctx, runcmd.DefaultTimeout, name, args...)
	label := strings.Join(append([]string{name}, args...), " ")
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	if !out.Success() {
		return "", fmt.Errorf("%s: %s", label, out.Stderr)
	}
	if runtime.GOOS == "freebsd" && !utf8.Valid(out.Stdout) {
		return "", fmt.Errorf("invalid command output")
	}
	return strings.ToValidUTF8(string(out.Stdout), "\ufffd"), nil
}
