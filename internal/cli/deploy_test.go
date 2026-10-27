package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aisidev/tt/internal/runcmd"
)

func TestParseDiskGiBAndMiB(t *testing.T) {
	for value, want := range map[string]uint32{"200G": 204800, "100g": 102400, "\"500G\"": 512000, "204800": 204800, "1024": 1024} {
		got, err := parseDisk(value)
		if err != nil || got != want {
			t.Fatal(value, got, err)
		}
	}
	for _, value := range []string{"oops", "0", "4294967295G", "-1", "1.5G"} {
		if _, err := parseDisk(value); err == nil {
			t.Fatal(value)
		}
	}
}
func TestParseConfigMinimalAndFull(t *testing.T) {
	cfg, err := parseDeployConfig("[[agents]]\nhost='10.0.0.2'\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Controller != nil || len(cfg.Agents) != 1 || cfg.Agents[0].SSHPort != 22 || cfg.Agents[0].Storage != "file" {
		t.Fatal(cfg)
	}
	cfg, err = parseDeployConfig(`[general]
prefix='/opt/tt'
user='myuser'
api_key='fixture-key'
[controller]
host='10.0.0.1'
[[agents]]
host='10.0.0.2'
storage='zvol'
host_id='node-a'
cpu_total=32
mem_total=65536
disk_total='1000G'
image_dir='tank/images'
runtime_dir='tank/runtime'
[[agents]]
host='10.0.0.3'
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDeployConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.General.Prefix != "/opt/tt" || cfg.Controller == nil || len(cfg.Agents) != 2 || *cfg.Agents[0].HostID != "node-a" || cfg.Agents[0].MemTotal != 65536 {
		t.Fatal(cfg)
	}
	for _, content := range []string{"[[agents]]\nHost='host'", "[[agents]]\nhost='host'\nssh_port=65536", "[[agents]]\nhost='host'\ndisk_total=100", "[[agents]]\nhost='host'\ncpu_total=-1", "[general]\nprefix=42"} {
		if _, err := parseDeployConfig(content); err == nil {
			t.Fatalf("accepted %s", content)
		}
	}
}
func TestDeploymentsReuseExistingRoleKeyAndPersistSharedKey(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "tt-agent.env"), "TT_API_KEY=existing-key\n")
	for range 2 {
		key, err := localAPIKey(dir)
		if err != nil || key != "existing-key" {
			t.Fatal(key, err)
		}
	}
	info, err := os.Stat(filepath.Join(dir, "api-key"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
}
func TestMismatchedRoleKeysAreNotSilentlyRotated(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "tt-agent.env"), "TT_API_KEY=agent-key\n")
	writeFixture(t, filepath.Join(dir, "tt-ctl.env"), "TT_API_KEY=ctl-key\n")
	if _, err := localAPIKey(dir); err == nil {
		t.Fatal("rotated mismatched keys")
	}
	if fileExists(filepath.Join(dir, "api-key")) {
		t.Fatal("shared key created")
	}
}
func TestDeploymentRejectsShellAndUnitInjectionBeforeRemoteWork(t *testing.T) {
	for _, extra := range []string{"image_dir='/tmp/image;touch /root/pwned'", "image_dir='/tmp/image%u'", "image_dir='/tmp/../root'", "ssh_user='-option'", "listen='host:9100'", "disk_total='4294967295G'", "container_runtime='invalid'"} {
		t.Run(extra, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "deploy.toml")
			writeFixture(t, path, "[[agents]]\nhost='127.0.0.1'\n"+extra+"\n")
			d := newDeployer(quietApplication())
			d.execute = func(context.Context, *exec.Cmd, time.Duration) (runcmd.Result, error) {
				t.Fatal("preflight performed remote work")
				return runcmd.Result{}, nil
			}
			if err := d.distributed(context.Background(), path); err == nil {
				t.Fatal("accepted invalid deployment")
			}
			if fileExists(path + ".api-key") {
				t.Fatal("preflight created key")
			}
		})
	}
}
func TestUpgradePreservesDocumentedMountGuards(t *testing.T) {
	previous := "[Unit]\nRequiresMountsFor=/srv/tt-run\n[Service]\nExecStartPre=/usr/bin/mountpoint -q /srv/tt-run\nExecStart=/old-agent\n"
	dropin := mountGuardDropin(previous)
	if dropin == nil || !strings.Contains(*dropin, "RequiresMountsFor=/srv/tt-run") || !strings.Contains(*dropin, "ExecStartPre=/usr/bin/mountpoint -q /srv/tt-run") || strings.Contains(*dropin, "/old-agent") {
		t.Fatal(dropin)
	}
	d := newDeployer(quietApplication())
	d.platform = "linux"
	d.root = t.TempDir()
	for _, path := range []string{"/run/systemd/system", "/etc/systemd/system"} {
		if err := os.MkdirAll(d.path(path), 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, d.path("/proc/self/status"), "Uid:\t0\t0\t0\t0\n")
	writeFixture(t, d.path("/etc/systemd/system/tt-agent.service"), previous)
	release := t.TempDir()
	for _, name := range []string{"tt", "tt-agent", "tt-ctl"} {
		writeFixture(t, filepath.Join(release, name), "fake "+name)
	}
	calls := [][]string{}
	d.execute = func(_ context.Context, cmd *exec.Cmd, timeout time.Duration) (runcmd.Result, error) {
		if timeout != 0 {
			t.Fatal("local helper unexpectedly bounded", timeout)
		}
		calls = append(calls, cmd.Args)
		return runcmd.Result{Stdout: []byte("active\n")}, nil
	}
	if err := d.local(context.Background(), "all", release); err != nil {
		t.Fatal(err)
	}
	assertFile(t, d.path("/etc/systemd/system/tt-agent.service.d/90-ttstack-mounts.conf"), *dropin)
	key, err := localAPIKey(d.path("/opt/ttstack/etc"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tt-agent", "tt-ctl"} {
		envPath := d.path("/opt/ttstack/etc/" + name + ".env")
		assertFile(t, envPath, "TT_API_KEY="+key+"\n")
		info, _ := os.Stat(envPath)
		if info.Mode().Perm() != 0600 {
			t.Fatal(info.Mode())
		}
		assertFile(t, d.path("/opt/ttstack/bin/"+name), "fake "+name)
		found := false
		for _, call := range calls {
			found = found || reflect.DeepEqual(call, []string{"systemctl", "restart", name})
		}
		if !found {
			t.Fatal("missing restart", name, calls)
		}
	}
}

func TestLocalMissingBinaryDiagnosticsUseGoBuildInstructions(t *testing.T) {
	for _, tc := range []struct {
		role, missing string
		present       []string
	}{
		{role: "agent", missing: "tt-agent"},
		{role: "ctl", missing: "tt-ctl"},
		{role: "ctl", missing: "tt", present: []string{"tt-ctl"}},
	} {
		t.Run(tc.role+"/"+tc.missing, func(t *testing.T) {
			d := newDeployer(quietApplication())
			d.platform = "linux"
			d.root = t.TempDir()
			if err := os.MkdirAll(d.path("/run/systemd/system"), 0755); err != nil {
				t.Fatal(err)
			}
			writeFixture(t, d.path("/proc/self/status"), "Uid:\t0\t0\t0\t0\n")
			release := t.TempDir()
			for _, name := range tc.present {
				writeFixture(t, filepath.Join(release, name), "fake "+name)
			}
			d.execute = func(context.Context, *exec.Cmd, time.Duration) (runcmd.Result, error) {
				return runcmd.Result{}, nil
			}
			err := d.local(context.Background(), tc.role, release)
			want := filepath.Join(release, tc.missing) + " not found (run 'make release')"
			if err == nil || err.Error() != want {
				t.Fatal(err, want)
			}
		})
	}
}
func TestRemoteScriptContainsSudoAndRefusesUnsupportedHosts(t *testing.T) {
	script := remoteSetupScript("tt-agent", "/opt/tt/bin/tt-agent --listen 0.0.0.0:9100", "ttstack", "/home/ttstack", "/opt/tt", "/tmp/deploy-1", []string{"tt-agent"}, []string{"/home/ttstack/images"}, "testhost", &envFile{"/opt/tt/etc/tt-agent.env", "TT_API_KEY=fixture-key\n"})
	for _, part := range []string{"sudo", "rc-service", "systemctl", "useradd", "EnvironmentFile=/opt/tt/etc/tt-agent.env", "TT_API_KEY=fixture-key", "KillMode=process", "TimeoutStopSec=360"} {
		if !strings.Contains(script, part) {
			t.Fatal(part)
		}
	}
	if strings.Contains(script, "--api-key") {
		t.Fatal("key in arguments")
	}
	cmd := exec.Command("/bin/sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	dir := t.TempDir()
	uname := filepath.Join(dir, "uname")
	writeFixture(t, uname, "#!/bin/sh\necho UnsupportedOS\n")
	if err := os.Chmod(uname, 0755); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("/bin/sh", "-s")
	cmd.Env = []string{"PATH=" + dir}
	cmd.Stdin = strings.NewReader(script)
	var out, diagnostic bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &diagnostic
	if err := cmd.Run(); err == nil || out.Len() != 0 || diagnostic.String() != "deployment requires a Linux host\n" {
		t.Fatal(err, out.String(), diagnostic.String())
	}
}
func TestDistributedDeploymentUsesStdinAndVerifiedUploads(t *testing.T) {
	for _, mode := range []string{"success", "checksum-failure", "setup-failure"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			release := filepath.Join(dir, "release")
			for _, name := range []string{"tt", "tt-agent", "tt-ctl"} {
				writeFixture(t, filepath.Join(release, name), "binary fixture")
			}
			path := filepath.Join(dir, "deploy.toml")
			writeFixture(t, path, fmt.Sprintf("[general]\nrelease_dir=%q\n[[agents]]\nhost='127.0.0.1'\n", release))
			d := newDeployer(quietApplication())
			imported, uploaded, verified, setup, cleaned := false, false, false, false, false
			d.execute = func(_ context.Context, cmd *exec.Cmd, timeout time.Duration) (runcmd.Result, error) {
				wantTimeout := time.Duration(0)
				if cmd.Args[0] == "ssh" {
					wantTimeout = 900 * time.Second
				}
				if timeout != wantTimeout {
					t.Fatal("deployment timeout drift", timeout, wantTimeout)
				}
				if strings.Contains(strings.Join(cmd.Args, " "), "fixture-key") {
					t.Fatal("secret in argv")
				}
				if cmd.Args[0] == "scp" {
					uploaded = true
					return runcmd.Result{}, nil
				}
				if cmd.Args[0] != "ssh" || cmd.Args[len(cmd.Args)-1] != "sh -s" || cmd.Stdin == nil {
					t.Fatal(cmd.Args)
				}
				payload, err := io.ReadAll(cmd.Stdin)
				if err != nil {
					t.Fatal(err)
				}
				script := string(payload)
				switch {
				case strings.HasPrefix(script, "sudo sh -c "):
					imported = true
					return runcmd.Result{Stdout: []byte("TT_API_KEY=fixture-key\n")}, nil
				case strings.HasPrefix(script, "umask"):
					return runcmd.Result{Stdout: []byte("/tmp/ttstack-deploy.fixture\n")}, nil
				case strings.Contains(script, "sha256sum -c"):
					verified = true
					if !uploaded {
						t.Fatal("verified before upload")
					}
					digest, _ := fileDigest(filepath.Join(release, "tt-agent"), false)
					if !strings.Contains(script, digest) {
						t.Fatal("wrong upload checksum")
					}
					if mode == "checksum-failure" {
						return runcmd.Result{ExitCode: 1}, nil
					}
				case strings.HasPrefix(script, "#!/bin/sh"):
					setup = true
					if !verified || !strings.Contains(script, "TT_API_KEY=fixture-key") {
						t.Fatal("unverified setup or lost key")
					}
					if mode == "setup-failure" {
						return runcmd.Result{ExitCode: 1, Stderr: []byte("TT_API_KEY=fixture-key")}, nil
					}
				case strings.HasPrefix(script, "rm -rf -- /tmp/ttstack-deploy."):
					cleaned = true
				default:
					t.Fatal("unexpected SSH script")
				}
				return runcmd.Result{}, nil
			}
			err := d.distributed(context.Background(), path)
			if (err == nil) != (mode == "success") {
				t.Fatal(err)
			}
			if err != nil && strings.Contains(err.Error(), "fixture-key") {
				t.Fatal("secret leaked")
			}
			if !imported || !uploaded || !verified || setup != (mode != "checksum-failure") || cleaned != setup {
				t.Fatal(imported, uploaded, verified, setup, cleaned)
			}
			assertFile(t, path+".api-key", "fixture-key\n")
			info, _ := os.Stat(path + ".api-key")
			if info.Mode().Perm() != 0600 {
				t.Fatal(info.Mode())
			}
		})
	}
}

func TestMalformedDeploymentHasOneParseContext(t *testing.T) {
	for _, content := range []string{"[general", "general=1", "agents=[1]"} {
		path := filepath.Join(t.TempDir(), "deploy.toml")
		writeFixture(t, path, content)
		err := newDeployer(quietApplication()).distributed(context.Background(), path)
		if err == nil || strings.Count(err.Error(), "parse deploy.toml:") != 1 {
			t.Fatal(content, err)
		}
	}
}

func TestRemoteKeyProbeRefusesDivergentRoleKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy.toml")
	writeFixture(t, path, "[controller]\nhost='10.0.0.1'\n")
	var probeScript string
	var stdout, stderr bytes.Buffer
	app := newApplication(&stdout, &stderr)
	d := newDeployer(app)
	d.execute = func(ctx context.Context, command *exec.Cmd, timeout time.Duration) (runcmd.Result, error) {
		if command.Stdin != nil {
			data, _ := io.ReadAll(command.Stdin)
			probeScript = string(data)
		}
		return runcmd.Result{ExitCode: 1, Stderr: []byte("existing agent/controller keys differ; put the intended shared key in deploy.toml general.api_key")}, nil
	}
	err := d.distributed(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "existing agent/controller keys differ") {
		t.Fatal(err)
	}
	if !strings.Contains(probeScript, "cmp -s") || !strings.Contains(probeScript, "tt-ctl.env") || !strings.Contains(probeScript, "tt-agent.env") {
		t.Fatal(probeScript)
	}
}
func TestRemoteDeployKeepsSuccessWhenStagingCleanupFails(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"tt-ctl", "tt"} {
		writeFixture(t, filepath.Join(dir, name), "binary-"+name)
	}
	path := filepath.Join(dir, "deploy.toml")
	writeFixture(t, path, "[general]\napi_key='AAAAAAAAAAAAAAAAAAAAAA'\nrelease_dir='"+dir+"'\n[controller]\nhost='10.0.0.1'\n")
	var stdout, stderr bytes.Buffer
	app := newApplication(&stdout, &stderr)
	d := newDeployer(app)
	d.execute = func(ctx context.Context, command *exec.Cmd, timeout time.Duration) (runcmd.Result, error) {
		if command.Args[0] == "scp" {
			return runcmd.Result{}, nil
		}
		data, _ := io.ReadAll(command.Stdin)
		script := string(data)
		switch {
		case strings.Contains(script, "mktemp"):
			return runcmd.Result{Stdout: []byte("/tmp/ttstack-deploy.test\n")}, nil
		case strings.HasPrefix(script, "rm -rf -- "):
			return runcmd.Result{ExitCode: 1, Stderr: []byte("simulated ssh hiccup")}, nil
		default:
			return runcmd.Result{}, nil
		}
	}
	if err := d.distributed(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "staging cleanup") {
		t.Fatal(stderr.String())
	}
}
