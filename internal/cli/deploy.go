package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aisidev/tt/internal/runcmd"
	"github.com/google/uuid"
)

type deployer struct {
	app            *application
	platform, root string
	execute        func(context.Context, *exec.Cmd, time.Duration) (runcmd.Result, error)
}

func newDeployer(a *application) *deployer {
	return &deployer{app: a, platform: runtime.GOOS, execute: toolOutput}
}
func (d *deployer) path(path string) string {
	if d.root == "" {
		return path
	}
	return filepath.Join(d.root, strings.TrimPrefix(path, "/"))
}
func (d *deployer) output(ctx context.Context, name string, args ...string) (runcmd.Result, error) {
	return d.execute(ctx, exec.Command(name, args...), 0)
}
func checkOutput(out runcmd.Result, action string) error {
	if !out.Success() {
		return fmt.Errorf("%s failed: %s", action, out.Stderr)
	}
	return nil
}
func generateAPIKey() string {
	return "tt-" + strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")
}
func parseDeploymentKey(content string) (string, error) {
	key := strings.TrimSpace(content)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "TT_API_KEY=") {
			key = strings.TrimPrefix(line, "TT_API_KEY=")
			break
		}
	}
	if key == "" || !asciiAllowed(key, "-_.") {
		return "", fmt.Errorf("deployment API key must contain only letters, digits, '-', '_' or '.'")
	}
	return key, nil
}
func saveSecret(path, key string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		return fmt.Errorf("create key directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create API key file: %w", err)
	}
	defer file.Close()
	if _, err := fmt.Fprintln(file, key); err != nil {
		return fmt.Errorf("save API key: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync API key: %w", err)
	}
	return nil
}
func readUTF8(path, label string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%s: invalid UTF-8", label)
	}
	return string(data), nil
}
func localAPIKey(etc string) (string, error) {
	shared := filepath.Join(etc, "api-key")
	if fileExists(shared) {
		content, err := readUTF8(shared, "read API key")
		if err != nil {
			return "", err
		}
		return parseDeploymentKey(content)
	}
	keys := map[string]bool{}
	for _, name := range []string{"tt-agent.env", "tt-ctl.env"} {
		content, err := readUTF8(filepath.Join(etc, name), "read existing service key")
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		key, err := parseDeploymentKey(content)
		if err != nil {
			return "", err
		}
		keys[key] = true
	}
	if len(keys) > 1 {
		return "", fmt.Errorf("existing agent/controller keys differ; put the intended shared key in %s/api-key and deploy all", etc)
	}
	key := ""
	for existing := range keys {
		key = existing
	}
	if key == "" {
		key = generateAPIKey()
	}
	if err := saveSecret(shared, key); err != nil {
		return "", err
	}
	return key, nil
}
func (d *deployer) ensureUser(ctx context.Context, user string) error {
	out, err := d.output(ctx, "id", user)
	if err != nil {
		return fmt.Errorf("check user: %w", err)
	}
	if out.Success() {
		d.app.out("[deploy] user '%s' exists", user)
		return nil
	}
	d.app.out("[deploy] creating user '%s'", user)
	out, err = d.output(ctx, "useradd", "-r", "-m", "-d", "/home/"+user, "-s", "/bin/sh", user)
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return checkOutput(out, "useradd")
}
func (d *deployer) ensureDirs(ctx context.Context, home, user string) error {
	for _, name := range []string{"images", "runtime", "data", "ctl", "run"} {
		if err := os.MkdirAll(d.path(home+"/"+name), 0777); err != nil {
			return fmt.Errorf("mkdir: %w", err)
		}
	}
	out, err := d.output(ctx, "chown", user+":"+user, home, home+"/ctl")
	if err != nil {
		return fmt.Errorf("chown controller directory: %w", err)
	}
	return checkOutput(out, "chown controller directory")
}
func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
func (d *deployer) installBinary(ctx context.Context, source, prefix string) error {
	directory := d.path(prefix + "/bin")
	if err := os.MkdirAll(directory, 0777); err != nil {
		return fmt.Errorf("mkdir bin: %w", err)
	}
	name := filepath.Base(source)
	destination := filepath.Join(directory, name)
	staging, err := os.CreateTemp(directory, ".tt-binary-*")
	if err != nil {
		return fmt.Errorf("binary staging: %w", err)
	}
	defer staging.Close()
	defer os.Remove(staging.Name())
	if err := copyFile(source, staging.Name()); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}
	if err := os.Chmod(staging.Name(), 0755); err != nil {
		return fmt.Errorf("binary permissions: %w", err)
	}
	if err := os.Rename(staging.Name(), destination); err != nil {
		return err
	}
	if _, err := d.output(ctx, "chmod", "755", prefix+"/bin/"+name); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	d.app.out("[deploy] installed %s/bin/%s", prefix, name)
	return nil
}
func (d *deployer) installSystemd(ctx context.Context, name, command string, root bool, environment *envFile) error {
	var envPath *string
	if environment != nil {
		path := d.path(environment.path)
		if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
			return fmt.Errorf("create env dir: %w", err)
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			return fmt.Errorf("open env file: %w", err)
		}
		if _, err = file.WriteString(environment.content); err != nil {
			file.Close()
			return fmt.Errorf("write env file: %w", err)
		}
		// The env file carries the API key; a crash before it is durable would
		// restart the freshly reloaded service without credentials.
		if err = file.Sync(); err != nil {
			file.Close()
			return fmt.Errorf("sync env file: %w", err)
		}
		if err = file.Close(); err != nil {
			return fmt.Errorf("close env file: %w", err)
		}
		_ = os.Chmod(path, 0600)
		envPath = &environment.path
	}
	unit := systemdUnit(name, command, root, envPath)
	path := d.path("/etc/systemd/system/" + name + ".service")
	if previous, err := readUTF8(path, "read previous unit"); err == nil {
		if guards := mountGuardDropin(previous); guards != nil {
			directory := path + ".d"
			if err := os.MkdirAll(directory, 0777); err != nil {
				return fmt.Errorf("mount guard directory: %w", err)
			}
			if err := os.WriteFile(filepath.Join(directory, "90-ttstack-mounts.conf"), []byte(*guards), 0666); err != nil {
				return fmt.Errorf("preserve mount guards: %w", err)
			}
		}
	}
	if err := os.WriteFile(path, []byte(unit), 0666); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	out, err := d.output(ctx, "systemctl", "daemon-reload")
	if err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}
	if err := checkOutput(out, "systemctl daemon-reload"); err != nil {
		return err
	}
	out, err = d.output(ctx, "systemctl", "enable", name)
	if err != nil {
		return fmt.Errorf("enable service: %w", err)
	}
	if err := checkOutput(out, "systemctl enable"); err != nil {
		return err
	}
	d.app.out("[deploy] unit %s installed", name)
	return nil
}
func (d *deployer) restartService(ctx context.Context, name string) error {
	active, err := d.output(ctx, "systemctl", "is-active", "--quiet", name)
	action := "start"
	if err == nil && active.Success() {
		action = "restart"
	}
	out, err := d.output(ctx, "systemctl", action, name)
	if err != nil {
		return fmt.Errorf("restart service: %w", err)
	}
	if err := checkOutput(out, "systemctl start/restart"); err != nil {
		return err
	}
	out, err = d.output(ctx, "systemctl", "is-active", name)
	if err != nil {
		return fmt.Errorf("check status: %w", err)
	}
	if !out.Success() {
		return fmt.Errorf("service %s is not active: %s", name, out.Stderr)
	}
	d.app.out("[deploy] %s is %s", name, strings.TrimSpace(string(out.Stdout)))
	return nil
}
func (d *deployer) local(ctx context.Context, role, release string) error {
	info, err := os.Stat(d.path("/run/systemd/system"))
	if d.platform != "linux" || err != nil || !info.IsDir() {
		return fmt.Errorf("local deploy requires Linux with systemd; use distributed deploy for Linux with OpenRC. FreeBSD hosts require manual setup and are validated only on FreeBSD 15.x")
	}
	uid := uint64(1000)
	if content, err := readUTF8(d.path("/proc/self/status"), "read status"); err == nil {
		for _, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(line, "Uid:") {
				fields := strings.Fields(line)
				if len(fields) > 1 {
					if value, err := strconv.ParseUint(fields[1], 10, 32); err == nil {
						uid = value
					}
				}
				break
			}
		}
	}
	if uid != 0 {
		return fmt.Errorf("local deploy requires root (run with sudo)")
	}
	prefix, user, home := "/opt/ttstack", "ttstack", "/home/ttstack"
	if err := d.ensureUser(ctx, user); err != nil {
		return err
	}
	if err := d.ensureDirs(ctx, home, user); err != nil {
		return err
	}
	key, err := localAPIKey(d.path(prefix + "/etc"))
	if err != nil {
		return err
	}
	content := "TT_API_KEY=" + key + "\n"
	if role == "agent" || role == "all" {
		binary := filepath.Join(release, "tt-agent")
		if !fileExists(binary) {
			return fmt.Errorf("%s not found (run 'make release')", binary)
		}
		if err := d.installBinary(ctx, binary, prefix); err != nil {
			return err
		}
		command := prefix + "/bin/tt-agent --listen 0.0.0.0:9100 --image-dir " + home + "/images --runtime-dir " + home + "/runtime --data-dir " + home + "/data --storage file"
		if err := d.installSystemd(ctx, "tt-agent", command, true, &envFile{prefix + "/etc/tt-agent.env", content}); err != nil {
			return err
		}
		if err := d.restartService(ctx, "tt-agent"); err != nil {
			return err
		}
	}
	if role == "ctl" || role == "all" {
		for _, name := range []string{"tt-ctl", "tt"} {
			binary := filepath.Join(release, name)
			if !fileExists(binary) {
				return fmt.Errorf("%s not found (run 'make release')", binary)
			}
			if err := d.installBinary(ctx, binary, prefix); err != nil {
				return err
			}
		}
		command := prefix + "/bin/tt-ctl --listen 0.0.0.0:9200 --data-dir " + home + "/ctl"
		if err := d.installSystemd(ctx, "tt-ctl", command, false, &envFile{prefix + "/etc/tt-ctl.env", content}); err != nil {
			return err
		}
		if err := d.restartService(ctx, "tt-ctl"); err != nil {
			return err
		}
		d.app.out("[deploy] API key stored in %s/etc/api-key (0600). Configure the CLI using TT_API_KEY.", prefix)
	}
	d.app.out("[deploy] local deploy complete")
	return nil
}
