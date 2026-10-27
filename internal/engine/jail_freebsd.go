package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aisidev/tt/internal/netcfg"
	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

type Jail struct{ runDir string }

func newJail() *Jail                           { return &Jail{runDir: model.RunDir} }
func (j *Jail) Name() string                   { return "jail" }
func jailName(vm *model.Vm) string             { return "tt-" + vm.ID }
func (j *Jail) configPath(vm *model.Vm) string { return filepath.Join(j.runDir, "jail-"+vm.ID+".conf") }
func (j *Jail) rootPath(vm *model.Vm) string   { return filepath.Join(j.runDir, "jail-"+vm.ID+".root") }
func (j *Jail) query(ctx context.Context, vm *model.Vm) (*string, error) {
	out, err := runcmd.Run(ctx, "jls", "--libxo", "json", "-dn", "name", "path")
	if err != nil {
		return nil, fmt.Errorf("query jails: %w", err)
	}
	if !out.Success() {
		return nil, fmt.Errorf("jls failed: %s", out.Stderr)
	}
	var value any
	if err := json.Unmarshal(out.Stdout, &value); err != nil {
		return nil, fmt.Errorf("parse jls: %w", err)
	}
	top, _ := value.(map[string]any)
	inventory, _ := top["jail-information"].(map[string]any)
	jails, ok := inventory["jail"].([]any)
	if !ok {
		return nil, fmt.Errorf("missing jail inventory")
	}
	for _, entry := range jails {
		record, _ := entry.(map[string]any)
		name, _ := record["name"].(string)
		parts := strings.Split(name, ".")
		if parts[len(parts)-1] != jailName(vm) {
			continue
		}
		root, ok := record["path"].(string)
		if !ok {
			return nil, fmt.Errorf("missing jail path")
		}
		expected, err := os.ReadFile(j.rootPath(vm))
		if err != nil {
			return nil, fmt.Errorf("read owned jail root: %w", err)
		}
		if !utf8.Valid(expected) {
			return nil, fmt.Errorf("read owned jail root: invalid UTF-8")
		}
		if root != string(expected) {
			return nil, fmt.Errorf("jail name belongs to a different root")
		}
		return &root, nil
	}
	return nil, nil
}
func (j *Jail) unmountDevfs(ctx context.Context, vm *model.Vm) error {
	root, err := os.ReadFile(j.rootPath(vm))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !utf8.Valid(root) {
		return fmt.Errorf("invalid jail root encoding")
	}
	dev := string(root) + "/dev"
	out, err := runcmd.Run(ctx, "mount", "-p")
	if err != nil {
		return fmt.Errorf("list mounts: %w", err)
	}
	if !out.Success() {
		return fmt.Errorf("cannot inspect jail mounts")
	}
	for _, line := range strings.Split(string(out.Stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != dev {
			continue
		}
		if len(fields) < 3 || fields[2] != "devfs" {
			return fmt.Errorf("unexpected filesystem on jail dev directory")
		}
		result, err := runcmd.Run(ctx, "umount", dev)
		if err != nil {
			return fmt.Errorf("unmount jail devfs: %w", err)
		}
		if !result.Success() {
			return fmt.Errorf("cannot unmount jail devfs: %s", result.Stderr)
		}
	}
	return nil
}
func jailConfiguration(vm *model.Vm, root string) (string, error) {
	quoted, err := model.QuoteJSONString(root)
	if err != nil {
		return "", err
	}
	ip, err := netcfg.FleetIPv4(vm.IP)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s {\n path = %s;\n host.hostname = \"ttstack\";\n ip4.addr = %s;\n interface = \"tt0\";\n persist;\n mount.devfs;\n exec.start = \"/bin/sh /etc/rc\";\n exec.stop = \"/bin/sh /etc/rc.shutdown\";\n exec.timeout = 30;\n stop.timeout = 10;\n}\n", jailName(vm), quoted, ip), nil
}
func (j *Jail) Create(ctx context.Context, vm *model.Vm, image, format string, keys []string) error {
	root, err := canonicalPath(image)
	if err != nil {
		return fmt.Errorf("canonicalize jail root: %w", err)
	}
	if !utf8.ValidString(root) {
		return fmt.Errorf("invalid jail root path")
	}
	if strings.IndexFunc(root, unicode.IsSpace) >= 0 {
		return fmt.Errorf("jail root path must not contain whitespace")
	}
	existing, err := j.query(ctx, vm)
	if err != nil {
		return err
	}
	if existing != nil {
		return fmt.Errorf("jail already exists; reconcile before retrying")
	}
	if len(keys) > 0 {
		ssh := filepath.Join(root, "root/.ssh")
		if err := os.MkdirAll(ssh, 0777); err != nil {
			return fmt.Errorf("create jail .ssh: %w", err)
		}
		path := filepath.Join(ssh, "authorized_keys")
		if err := os.WriteFile(path, []byte(strings.Join(keys, "\n")+"\n"), 0666); err != nil {
			return fmt.Errorf("write jail keys: %w", err)
		}
		if err := os.Chmod(ssh, 0700); err != nil {
			return err
		}
		if err := os.Chmod(path, 0600); err != nil {
			return err
		}
		config := filepath.Join(root, "etc/ssh/sshd_config")
		old, err := os.ReadFile(config)
		if err != nil {
			return fmt.Errorf("read jail sshd config: %w", err)
		}
		if !utf8.Valid(old) {
			return fmt.Errorf("read jail sshd config: invalid UTF-8")
		}
		// Only the complete leading stanza proves all three global settings
		// are installed. Comments and Match blocks must not suppress them.
		const managedSSH = "PermitRootLogin prohibit-password\nPasswordAuthentication no\nKbdInteractiveAuthentication no\n"
		if !strings.HasPrefix(string(old), managedSSH) {
			if err := os.WriteFile(config, append([]byte(managedSSH), old...), 0666); err != nil {
				return fmt.Errorf("configure jail sshd: %w", err)
			}
		}
	}
	if err := os.MkdirAll(j.runDir, 0777); err != nil {
		return err
	}
	if err := os.WriteFile(j.rootPath(vm), []byte(root), 0666); err != nil {
		return fmt.Errorf("record jail root: %w", err)
	}
	config, err := jailConfiguration(vm, root)
	if err != nil {
		return err
	}
	if err := os.WriteFile(j.configPath(vm), []byte(config), 0666); err != nil {
		return fmt.Errorf("write jail configuration: %w", err)
	}
	out, err := runcmd.Run(ctx, "jail", "-f", j.configPath(vm), "-c", jailName(vm))
	if err != nil {
		return fmt.Errorf("create jail: %w", err)
	}
	if !out.Success() {
		return fmt.Errorf("jail create failed: %s", out.Stderr)
	}
	running, err := j.query(ctx, vm)
	if err != nil {
		return err
	}
	if running == nil {
		return fmt.Errorf("jail exited during startup")
	}
	return nil
}
func (j *Jail) Start(ctx context.Context, vm *model.Vm) error {
	return fmt.Errorf("stopped jails require cold creation from their retained root")
}
func (j *Jail) Stop(ctx context.Context, vm *model.Vm) error {
	root, err := j.query(ctx, vm)
	if err != nil {
		return err
	}
	if root != nil {
		out, err := runcmd.Run(ctx, "jail", "-f", j.configPath(vm), "-r", jailName(vm))
		if err != nil {
			return fmt.Errorf("stop jail: %w", err)
		}
		if !out.Success() {
			return fmt.Errorf("jail stop failed: %s", out.Stderr)
		}
		for i := 0; i < 100; i++ {
			root, err := j.query(ctx, vm)
			if err != nil {
				return err
			}
			if root == nil {
				break
			}
			if err := sleepContext(ctx, 50*time.Millisecond); err != nil {
				return err
			}
		}
		root, err = j.query(ctx, vm)
		if err != nil {
			return err
		}
		if root != nil {
			return fmt.Errorf("jail is still running or dying")
		}
	}
	return j.unmountDevfs(ctx, vm)
}
func (j *Jail) Destroy(ctx context.Context, vm *model.Vm) error {
	if err := j.Stop(ctx, vm); err != nil {
		return err
	}
	for _, path := range []string{j.configPath(vm), j.rootPath(vm)} {
		if err := removeFile(path); err != nil {
			return err
		}
	}
	return nil
}
func (j *Jail) State(ctx context.Context, vm *model.Vm) (model.VmState, error) {
	root, err := j.query(ctx, vm)
	if err != nil {
		return "", err
	}
	if root == nil {
		return model.VmStopped, nil
	}
	return model.VmRunning, nil
}
