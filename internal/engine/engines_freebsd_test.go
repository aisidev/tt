package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aisidev/tt/model"
	"golang.org/x/sys/unix"
)

func TestNativePSInventoryHasSeparateColumns(t *testing.T) {
	listing, err := processListing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	own := strconv.Itoa(os.Getpid())
	found := false
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == own {
			found = true
		}
	}
	if !found {
		t.Fatal("own PID absent")
	}
	if _, err := findBhyveProcess("ttstack-nonexistent-test-vm", listing); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", ",stat=,comm=,args=\n123\n"} {
		if _, err := findBhyveProcess("guest", bad); err == nil {
			t.Fatal(bad)
		}
	}
}
func TestBhyveIdentityUsesExactLiveProcessTitlesInsteadOfStalePIDs(t *testing.T) {
	listing := "10 S sleep sleep guest\n11 Z bhyve bhyve: guest (bhyve)\n12 S bhyve bhyve: guest-other (bhyve)\n13 ICJ bhyve bhyve: guest (bhyve)\n"
	for _, tc := range []struct {
		id, listing string
		want        int
	}{{"guest", listing, 13}, {"absent", listing, 0}, {"guest", "14 S bhyve bhyve -c 1 guest", 14}} {
		pid, err := findBhyveProcess(tc.id, tc.listing)
		if err != nil || pid != tc.want {
			t.Fatal(pid, err)
		}
	}
	if _, err := findBhyveProcess("guest", listing+"15 S bhyve bhyve: guest (bhyve)\n"); err == nil {
		t.Fatal("duplicate accepted")
	}
}
func TestBhyveOwnershipGatesSignalsAndDeviceRemoval(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("TT_ENGINE_TEST_DIR", dir)
	fakeTool(t, dir, "ps", `/bin/cat "$TT_ENGINE_TEST_DIR/listing"`)
	fakeTool(t, dir, "bhyvectl", `printf '%s\n' "$@" > "$TT_ENGINE_TEST_DIR/destroy-args"`)
	b := newBhyve()
	b.runDir = dir
	b.deviceDir = dir
	vm := testVM()
	vm.Engine = model.Bhyve
	put(t, filepath.Join(dir, "listing"), "99 S bhyve bhyve: guest (bhyve)\n")
	signals := 0
	b.signal = func(pid int, sig unix.Signal) error {
		signals++
		if pid != 99 || sig != unix.SIGTERM {
			t.Fatal(pid, sig)
		}
		put(t, filepath.Join(dir, "listing"), "1 S init /sbin/init\n")
		return nil
	}
	if err := b.Stop(context.Background(), vm); err == nil {
		t.Fatal("missing metadata accepted")
	}
	if signals != 0 {
		t.Fatal("signalled unowned process")
	}
	put(t, b.artifact(vm, "disk"), "/retained/disk")
	put(t, b.device(vm), "fake-vmm-device")
	if err := b.Stop(context.Background(), vm); err != nil {
		t.Fatal(err)
	}
	if signals != 1 {
		t.Fatal(signals)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "destroy-args"))
	if string(args) != "--destroy\n--vm\nguest\n" {
		t.Fatal(string(args))
	}
	if !exists(b.artifact(vm, "disk")) {
		t.Fatal("stop removed retained metadata")
	}
	if err := b.Start(context.Background(), vm); err == nil || !strings.Contains(err.Error(), "cold creation") {
		t.Fatal(err)
	}
}
func TestBhyveFailedLaunchRetainsDiskMetadata(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("TT_ENGINE_TEST_DIR", dir)
	fakeTool(t, dir, "ps", "echo '1 S init /sbin/init'\n")
	fakeTool(t, dir, "bhyveload", `printf '%s\n' "$@" > "$TT_ENGINE_TEST_DIR/load-args"`)
	fakeTool(t, dir, "bhyve", `printf '%s\n' "$@" > "$TT_ENGINE_TEST_DIR/bhyve-args"; exit 7`)
	b := newBhyve()
	b.runDir = dir
	b.deviceDir = filepath.Join(dir, "devices")
	b.tapDevice = func(context.Context, string) (string, error) { return "tap42", nil }
	disk := filepath.Join(dir, "disk")
	put(t, disk, "retained")
	vm := testVM()
	vm.Engine = model.Bhyve
	err := b.Create(context.Background(), vm, disk, "raw", nil)
	if err == nil || !strings.Contains(err.Error(), "exited during startup") {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(b.artifact(vm, "disk"))
	if string(data) != disk {
		t.Fatal(string(data))
	}
	args, _ := os.ReadFile(filepath.Join(dir, "bhyve-args"))
	want := strings.Join([]string{"-A", "-H", "-P", "-c", "2", "-m", "256M", "-s", "0:0,hostbridge", "-s", "3:0,virtio-blk," + disk, "-s", "4:0,virtio-net,tap42", "-s", "31,lpc", "-l", "com1,stdio", "guest", ""}, "\n")
	if string(args) != want {
		t.Fatal(string(args))
	}
	if err := b.Destroy(context.Background(), vm); err != nil {
		t.Fatal(err)
	}
	if !exists(disk) || exists(b.artifact(vm, "disk")) {
		t.Fatal("wrong cleanup")
	}
}
func TestJailColdLifecycleUsesOwnedRootAndDevfsOnly(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(filepath.Join(root, "etc/ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(root, "etc/ssh/sshd_config"), "# original\n")
	t.Setenv("PATH", dir)
	t.Setenv("TT_ENGINE_TEST_DIR", dir)
	t.Setenv("TT_ENGINE_TEST_ROOT", root)
	fakeTool(t, dir, "jls", `if [ -e "$TT_ENGINE_TEST_DIR/alive" ]; then printf '{"jail-information":{"jail":[{"name":"parent.tt-guest","path":"%s"}]}}' "$TT_ENGINE_TEST_ROOT"; else echo '{"jail-information":{"jail":[]}}'; fi`)
	fakeTool(t, dir, "jail", `printf '%s\n' "$@" >> "$TT_ENGINE_TEST_DIR/jail-args"
case "$3" in -c) : > "$TT_ENGINE_TEST_DIR/alive" ;; -r) /bin/rm "$TT_ENGINE_TEST_DIR/alive" ;; *) exit 2 ;; esac`)
	fakeTool(t, dir, "mount", `printf 'devfs %s/dev devfs rw 0 0\n' "$TT_ENGINE_TEST_ROOT"`)
	fakeTool(t, dir, "umount", `printf '%s\n' "$@" > "$TT_ENGINE_TEST_DIR/umount-args"`)
	j := newJail()
	j.runDir = dir
	vm := testVM()
	vm.Engine = model.Jail
	ctx := context.Background()
	if err := j.Create(ctx, vm, root, "directory", []string{"ssh-ed25519 fixture"}); err != nil {
		t.Fatal(err)
	}
	state, err := j.State(ctx, vm)
	if err != nil || state != model.VmRunning {
		t.Fatal(state, err)
	}
	key := filepath.Join(root, "root/.ssh/authorized_keys")
	info, err := os.Stat(key)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	config, err := os.ReadFile(j.configPath(vm))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), fmt.Sprintf("path = %q;", root)) || !strings.Contains(string(config), "ip4.addr = 10.10.1.2;") {
		t.Fatal(string(config))
	}
	put(t, j.rootPath(vm), "/different")
	if err := j.Stop(ctx, vm); err == nil {
		t.Fatal("unowned jail stopped")
	}
	if !exists(filepath.Join(dir, "alive")) {
		t.Fatal("wrong jail removed")
	}
	put(t, j.rootPath(vm), root)
	if err := j.Stop(ctx, vm); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "umount-args"))
	if string(args) != root+"/dev\n" {
		t.Fatal(string(args))
	}
	if err := j.Start(ctx, vm); err == nil || !strings.Contains(err.Error(), "cold creation") {
		t.Fatal(err)
	}
	if err := j.Create(ctx, vm, root, "directory", nil); err != nil {
		t.Fatal(err)
	}
	if err := j.Destroy(ctx, vm); err != nil {
		t.Fatal(err)
	}
	if !exists(key) || exists(j.configPath(vm)) || exists(j.rootPath(vm)) {
		t.Fatal("wrong retained root cleanup")
	}
}
func TestJailQueryAndMountErrorsRetainResources(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	j := newJail()
	j.runDir = dir
	vm := testVM()
	fakeTool(t, dir, "jls", `echo '{"jail-information":{}}'`)
	if _, err := j.State(context.Background(), vm); err == nil {
		t.Fatal("missing inventory accepted")
	}
	fakeTool(t, dir, "jls", `echo '{"jail-information":{"jail":[]}}'`)
	put(t, j.rootPath(vm), "/owned")
	fakeTool(t, dir, "mount", `echo 'disk /owned/dev ufs rw 0 0'`)
	fakeTool(t, dir, "umount", `exit 99`)
	if err := j.Destroy(context.Background(), vm); err == nil || !strings.Contains(err.Error(), "unexpected filesystem") {
		t.Fatal(err)
	}
	if !exists(j.rootPath(vm)) {
		t.Fatal("unknown mount removed metadata")
	}
}

func TestJailSSHSettingsRequireCompleteLeadingStanza(t *testing.T) {
	const managed = "PermitRootLogin prohibit-password\nPasswordAuthentication no\nKbdInteractiveAuthentication no\n"
	for _, tc := range []struct{ name, original string }{
		{"comment", "#PermitRootLogin prohibit-password\nPermitRootLogin no\n"},
		{"partial", "PermitRootLogin prohibit-password\nPasswordAuthentication yes\nKbdInteractiveAuthentication yes\n"},
		{"match", "Match User guest\n" + managed},
		{"managed", managed + "# retained settings\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			root := filepath.Join(dir, "root")
			if err := os.MkdirAll(filepath.Join(root, "etc/ssh"), 0700); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(root, "etc/ssh/sshd_config")
			put(t, config, tc.original)
			t.Setenv("PATH", dir)
			t.Setenv("TT_ENGINE_TEST_DIR", dir)
			t.Setenv("TT_ENGINE_TEST_ROOT", root)
			fakeTool(t, dir, "jls", `if [ -e "$TT_ENGINE_TEST_DIR/alive" ]; then printf '{"jail-information":{"jail":[{"name":"tt-guest","path":"%s"}]}}' "$TT_ENGINE_TEST_ROOT"; else echo '{"jail-information":{"jail":[]}}'; fi`)
			fakeTool(t, dir, "jail", `case "$3" in -c) : > "$TT_ENGINE_TEST_DIR/alive" ;; -r) /bin/rm "$TT_ENGINE_TEST_DIR/alive" ;; *) exit 2 ;; esac`)
			fakeTool(t, dir, "mount", `exit 0`)
			j := newJail()
			j.runDir = dir
			vm := testVM()
			vm.Engine = model.Jail
			ctx := context.Background()
			var previous string
			for attempt := range 2 {
				if err := j.Create(ctx, vm, root, "directory", []string{"ssh-ed25519 fixture"}); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(config)
				if err != nil || !strings.HasPrefix(string(data), managed) {
					t.Fatal("missing global SSH settings", string(data), err)
				}
				if attempt > 0 && string(data) != previous {
					t.Fatal("cold recreation duplicated SSH settings")
				}
				if !strings.HasSuffix(string(data), tc.original) {
					t.Fatal("original SSH settings were discarded")
				}
				previous = string(data)
				if err := j.Stop(ctx, vm); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
