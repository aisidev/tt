// Package sshboot implements one-time SSH provisioning and bounded readiness probes.
package sshboot

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

const SeedFile = "ttstack-ssh.sh"

//go:embed bootstrap.sh
var bootstrap string

func SeedPath(id string) string { return filepath.Join(model.RunDir, "ssh-"+id+".sh") }
func ResolveOptions(e model.Engine, requested *model.SshOptions, keys []string) (*model.SshOptions, error) {
	if requested == nil && len(keys) == 0 {
		return nil, nil
	}
	options := model.DefaultSshOptions()
	if requested != nil {
		options = *requested
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("SSH bootstrap requires a public key")
	}
	if capability.SshBootstrap.Supported(e, model.File) {
		return &options, nil
	}
	if capability.RootKeys.Supported(e, model.File) {
		if options == model.DefaultSshOptions() {
			return nil, nil
		}
		//lint:ignore ST1005 Preserve the pinned compatibility diagnostic.
		return nil, fmt.Errorf("Jail supports only root SSH keys without SSH bootstrap options")
	}
	return nil, fmt.Errorf("SSH bootstrap is not supported by %s", e)
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func Script(options model.SshOptions, keys []string, private, public string) string {
	sudo := "no"
	if options.Sudo {
		sudo = "yes"
	}
	return fmt.Sprintf("#!/bin/sh\nset -eu\naccount=%s\nsudo_access=%s\ninitial_keys=%s\nhost_private=%s\nhost_public=%s\n%s", quote(options.User), sudo, quote(strings.Join(keys, "\n")+"\n"), quote(private), quote(public), bootstrap)
}
func Bootstrap(ctx context.Context, options model.SshOptions, keys []string) (string, string, error) {
	if err := options.Validate(); err != nil {
		return "", "", err
	}
	staging, err := os.MkdirTemp("", "tt-ssh-*")
	if err != nil {
		return "", "", fmt.Errorf("SSH host key staging: %w", err)
	}
	defer os.RemoveAll(staging)
	key := filepath.Join(staging, "host")
	output, err := runcmd.Run(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "ttstack-vm", "-f", key)
	if err != nil {
		return "", "", fmt.Errorf("generate VM host identity: %w", err)
	}
	if !output.Success() {
		return "", "", fmt.Errorf("cannot generate VM host identity")
	}
	private, err := os.ReadFile(key)
	if err != nil {
		return "", "", fmt.Errorf("read host identity: %w", err)
	}
	public, err := os.ReadFile(key + ".pub")
	if err != nil {
		return "", "", fmt.Errorf("read public host identity: %w", err)
	}
	return Script(options, keys, string(private), string(public)), strings.TrimSpace(string(public)), nil
}
func Ready(ctx context.Context, ip string) bool {
	if net.ParseIP(ip) == nil {
		return false
	}
	d := net.Dialer{Timeout: 200 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, "22"))
	if err != nil {
		return false
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		return false
	}
	var prefix [4]byte
	_, err = io.ReadFull(conn, prefix[:])
	return err == nil && string(prefix[:]) == "SSH-"
}
func InitialIdentityReady(ctx context.Context, ip, public string) bool {
	if net.ParseIP(ip) == nil {
		return false
	}
	expected := strings.Fields(public)
	if len(expected) < 2 {
		return false
	}
	expected = expected[:2]
	output, err := runcmd.Output(ctx, exec.Command("ssh-keyscan", "-T", "1", "-t", "ed25519", ip), 2*time.Second)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(output.Stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && slices.Equal(fields[1:3], expected) {
			return true
		}
	}
	return false
}
func PreparedRootfs(ctx context.Context, path string) error {
	output, err := runcmd.Run(ctx, "debugfs", "-R", "cat /etc/ttstack/ssh-bootstrap-version", path)
	if err != nil {
		return fmt.Errorf("inspect SSH image support: %w", err)
	}
	if !output.Success() || strings.Trim(string(output.Stdout), " \t\n\r\x0c") != "1" {
		//lint:ignore ST1005 Preserve the pinned compatibility diagnostic.
		return fmt.Errorf("Firecracker image requires TTstack SSH bootstrap version 1")
	}
	return nil
}
