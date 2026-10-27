package sshboot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aisidev/tt/model"
)

func TestEngineOptionsPreserveJailKeysWithoutLinuxBootstrap(t *testing.T) {
	keys := []string{"ssh-ed25519 AAAA test"}
	for _, e := range []model.Engine{model.Qemu, model.Firecracker} {
		options, err := ResolveOptions(e, nil, keys)
		if err != nil || options == nil || *options != model.DefaultSshOptions() {
			t.Fatal(options, err)
		}
		if _, err := ResolveOptions(e, options, nil); err == nil {
			t.Fatal("keys required")
		}
	}
	if options, err := ResolveOptions(model.Jail, nil, keys); err != nil || options != nil {
		t.Fatal(options, err)
	}
	for _, o := range []model.SshOptions{{User: "user"}, {User: "root", Sudo: true}} {
		if _, err := ResolveOptions(model.Jail, &o, keys); err == nil {
			t.Fatal(o)
		}
	}
	for _, e := range []model.Engine{model.Docker, model.Bhyve} {
		if _, err := ResolveOptions(e, nil, keys); err == nil {
			t.Fatal(e)
		}
		if o, err := ResolveOptions(e, nil, nil); o != nil || err != nil {
			t.Fatal(o, err)
		}
	}
}
func TestInitialScriptQuotesKeysAndPreservesCompletedBootstrap(t *testing.T) {
	script := Script(model.SshOptions{User: "user", Sudo: true}, []string{"ssh-ed25519 AAAA $(touch unsafe)' comment"}, "private\n", "public\n")
	if err := exec.Command("sh", "-n", "-c", script).Run(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh-initialized"), []byte("1"), 0600); err != nil {
		t.Fatal(err)
	}
	script = strings.ReplaceAll(script, "/var/lib/ttstack", dir)
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "unsafe")); !os.IsNotExist(err) {
		t.Fatal("key content executed", err)
	}
}
func TestGeneratesUniqueHostIdentityWithoutReturningItInMetadata(t *testing.T) {
	if os.Getenv("TT_TEST_INTEGRATION") != "1" {
		t.Skip("opt-in real SSH tool")
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Fatal(err)
	}
	script, public, err := Bootstrap(context.Background(), model.DefaultSshOptions(), []string{"ssh-ed25519 AAAA test"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(public, "ssh-ed25519 ") || strings.Contains(public, "PRIVATE") || !strings.Contains(script, "OPENSSH PRIVATE KEY") {
		t.Fatal("host key metadata invalid")
	}
	_, other, err := Bootstrap(context.Background(), model.DefaultSshOptions(), nil)
	if err != nil || other == public {
		t.Fatal("host identity reused", err)
	}
}

func TestPreparedRootVersionUsesRustASCIIWhitespace(t *testing.T) {
	directory := t.TempDir()
	program := filepath.Join(directory, "debugfs")
	payload := filepath.Join(directory, "version")
	if err := os.WriteFile(program, []byte("#!/bin/sh\ncat \"$TT_TEST_VERSION_FILE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TT_TEST_VERSION_FILE", payload)
	for _, tc := range []struct {
		value string
		ok    bool
	}{{"1", true}, {" \t\n\r\f1\f \t\n", true}, {"1\v", false}, {"\v1", false}, {"1\u00a0", false}, {"2", false}} {
		if err := os.WriteFile(payload, []byte(tc.value), 0600); err != nil {
			t.Fatal(err)
		}
		if err := PreparedRootfs(context.Background(), "fixture.ext4"); (err == nil) != tc.ok {
			t.Fatalf("%q: %v", tc.value, err)
		}
	}
}
