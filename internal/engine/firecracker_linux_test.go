package engine

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aisidev/tt/internal/netcfg"
	"github.com/aisidev/tt/model"
	"golang.org/x/sys/unix"
)

func TestConfigurationAttachesOneReadOnlyDiskForConfigOrSSH(t *testing.T) {
	for _, config := range []bool{false, true} {
		for _, ssh := range []bool{false, true} {
			vm := testVM()
			vm.Engine = model.Firecracker
			if config {
				digest := "caller-config"
				vm.Options.GuestConfigDigest = &digest
			}
			if ssh {
				vm.Options.SSH = &model.SshOptions{}
			}
			drives := firecrackerConfiguration(vm, "02:54:0a:0a:01:02")["drives"].([]map[string]any)
			want := 1
			if config || ssh {
				want = 2
			}
			if len(drives) != want {
				t.Fatal(drives)
			}
			if want == 2 && (drives[1]["path_on_host"] != "/guest-config.ext4" || drives[1]["is_read_only"] != true) {
				t.Fatal(drives)
			}
		}
	}
}
func TestFileRootKeepsThePersistentInode(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "rootfs.ext4")
	target := filepath.Join(dir, "jailed-root")
	put(t, source, "before")
	if err := stageMember(source, target, true); err != nil {
		t.Fatal(err)
	}
	put(t, target, "after")
	got, _ := os.ReadFile(source)
	if string(got) != "after" {
		t.Fatal(string(got))
	}
	a, _ := os.Stat(source)
	b, _ := os.Stat(target)
	if !os.SameFile(a, b) {
		t.Fatal("root copied")
	}
}
func TestRejectsSymlinkedRegularMembersAndCharacterDevices(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "link")
	target := filepath.Join(dir, "jailed")
	regular := filepath.Join(dir, "file")
	put(t, regular, "kernel")
	for _, path := range []string{"/dev/null", regular} {
		if err := os.Symlink(path, source); err != nil {
			t.Fatal(err)
		}
		for _, writable := range []bool{true, false} {
			if err := stageMember(source, target, writable); err == nil {
				t.Fatalf("accepted %s writable=%v", path, writable)
			}
		}
		if err := os.Remove(source); err != nil {
			t.Fatal(err)
		}
	}
	if exists(target) {
		t.Fatal("invalid member staged")
	}
}
func TestSandboxPreparationRecordsOwnershipAndRetainsDisk(t *testing.T) {
	// Use a short root even when the service TMPDIR is a long data-disk path.
	// The real Unix socket limit still applies to the production runtime.
	dir, err := os.MkdirTemp("/tmp", "ttfc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	image := filepath.Join(dir, "image")
	if err := os.Mkdir(image, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"vmlinux", "rootfs.ext4", "guest-config.ext4"} {
		put(t, filepath.Join(image, name), name)
	}
	for _, name := range []string{"firecracker", "jailer"} {
		fakeTool(t, dir, name, "exit 88\n")
	}
	t.Setenv("PATH", dir)
	f := newFirecracker()
	f.runDir = dir
	f.cgroupRoot = dir
	put(t, filepath.Join(dir, "cgroup.controllers"), "cpu memory pids")
	vm := testVM()
	vm.Engine = model.Firecracker
	vm.Options.SSH = &model.SshOptions{}
	var owned []string
	f.chown = func(path string, uid, gid int) error {
		if uid != 100258 || gid != uid {
			t.Fatalf("ownership %d:%d", uid, gid)
		}
		owned = append(owned, path)
		return nil
	}
	tapCalls := 0
	f.prepareTap = func(_ context.Context, id string, uid uint32) error {
		tapCalls++
		if id != vm.ID || uid != 100258 {
			t.Fatal(id, uid)
		}
		return nil
	}
	s, cmd, err := f.prepareSandbox(context.Background(), vm, image)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 4 || tapCalls != 1 {
		t.Fatal(owned, tapCalls)
	}
	loaded, err := f.loadSandbox(vm)
	if err != nil || !reflect.DeepEqual(loaded, s) {
		t.Fatal(loaded, err)
	}
	id := netcfg.TapName(vm.ID)
	want := []string{filepath.Join(dir, "jailer"), "--id", id, "--exec-file", filepath.Join(dir, "firecracker"), "--uid", "100258", "--gid", "100258", "--chroot-base-dir", filepath.Join(dir, ".jailer"), "--cgroup-version", "2", "--parent-cgroup", "ttstack", "--cgroup", "cpu.max=200000 100000", "--cgroup", "memory.max=402653184", "--cgroup", "memory.swap.max=0", "--cgroup", "pids.max=68", "--", "--api-sock", "/fc-" + id + ".sock", "--config-file", "/config.json"}
	golden := readGolden(t)
	normalized := append([]string(nil), cmd.Args...)
	for i := range normalized {
		normalized[i] = strings.ReplaceAll(normalized[i], filepath.Join(dir, ".jailer"), "/images/.jailer")
		normalized[i] = strings.ReplaceAll(normalized[i], dir, "/tools")
	}
	if !reflect.DeepEqual(normalized, golden.Jailer) {
		t.Fatalf("jailer Rust golden: %q != %q", normalized, golden.Jailer)
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("got %q\nwant %q", cmd.Args, want)
	}
	for _, name := range []string{"vmlinux", "rootfs.ext4", "guest-config.ext4"} {
		a, _ := os.Stat(filepath.Join(image, name))
		b, err := os.Stat(filepath.Join(s.Root, name))
		if err != nil || !os.SameFile(a, b) {
			t.Fatal(name, err)
		}
		want := os.FileMode(0400)
		if name == "rootfs.ext4" {
			want = 0600
		}
		if b.Mode().Perm() != want {
			t.Fatal(name, b.Mode())
		}
	}
	// No real jailer, cgroup, TAP or ownership change is invoked by this test.
	if err := s.cleanup(); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(image, "rootfs.ext4")) || exists(s.Root) {
		t.Fatal("cleanup removed retained disk or left sandbox")
	}
}
func TestSandboxMetadataIsStrictAboutRequiredFields(t *testing.T) {
	for _, text := range []string{`{}`, `{"root":null,"marker":"m","cgroup":"c"}`, `{"root":"a","root":"b","marker":"m","cgroup":"c"}`} {
		var s sandbox
		if json.Unmarshal([]byte(text), &s) == nil {
			t.Fatal(text)
		}
	}
	var s sandbox
	if err := json.Unmarshal([]byte(`["/tmp/root","/socket","/tmp/cgroup"]`), &s); err != nil {
		t.Fatal(err)
	}
}
func TestFirecrackerUnixAPIStateResumeAndFailures(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ttapi-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	vm := testVM()
	f := newFirecracker()
	f.runDir = dir
	f.processes = processes{root: t.TempDir()}
	socket := f.legacySocket(vm)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 10)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		requests <- r.Method + " " + r.URL.Path + " " + string(data)
		switch r.URL.Path {
		case "/":
			io.WriteString(w, `{"state":"Running"}`)
		case "/vm":
			w.WriteHeader(204)
		case "/redirect":
			w.Header().Set("Location", "/unexpected")
			w.WriteHeader(302)
		default:
			http.Error(w, "bad", 400)
		}
	})}
	go server.Serve(listener)
	defer server.Close()
	fakeProcess(t, f.processes, 24, "firecracker\x00"+socket+"\x00")
	state, err := f.State(context.Background(), vm)
	if err != nil || state != model.VmRunning {
		t.Fatal(state, err)
	}
	if got := <-requests; got != "GET / " {
		t.Fatal(got)
	}
	if err := f.Start(context.Background(), vm); err != nil {
		t.Fatal(err)
	}
	if got := <-requests; got != `PATCH /vm {"state":"Resumed"}` {
		t.Fatal(got)
	}
	if _, err := firecrackerRequest(context.Background(), socket, "GET", "/bad", nil); err == nil || !strings.HasPrefix(err.Error(), "Firecracker API GET /bad failed:") {
		t.Fatal(err)
	}
	<-requests
	if _, err := firecrackerRequest(context.Background(), socket, "GET", "/redirect", nil); err != nil {
		t.Fatal(err)
	}
	if got := <-requests; got != "GET /redirect " {
		t.Fatal(got)
	}
	select {
	case got := <-requests:
		t.Fatal("followed redirect", got)
	default:
	}
	if err := f.PrepareDiskRestore(context.Background(), vm); err == nil || err.Error() != "Firecracker is still running" {
		t.Fatal(err)
	}
}

func TestBlockRootRecreatesNodeInsteadOfLinkingDevice(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "rootfs.ext4")
	target := filepath.Join(dir, "jailed-root")
	if err := os.Symlink("/dev/null", source); err != nil {
		t.Fatal(err)
	}
	stat := func(path string, s *unix.Stat_t) error {
		if path != source {
			t.Fatal(path)
		}
		s.Mode = unix.S_IFBLK | 0600
		s.Rdev = 0x1234
		return nil
	}
	calls := 0
	mknod := func(path string, mode uint32, device int) error {
		calls++
		if path != target || mode != unix.S_IFBLK|0600 || device != 0x1234 {
			t.Fatal(path, mode, device)
		}
		return nil
	}
	if err := stageMemberWithDevice(source, target, true, stat, mknod); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	if err := stageMemberWithDevice(source, target, false, stat, mknod); err == nil {
		t.Fatal("read-only block member accepted")
	}
	if calls != 1 {
		t.Fatal("unexpected device creation")
	}
}
