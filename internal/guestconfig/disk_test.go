package guestconfig

import (
	"context"
	"github.com/aisidev/tt/model"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestConfigDiskContainsExactFilesAndHasPrivateHostPermissions(t *testing.T) {
	if os.Getenv("TT_TEST_INTEGRATION") != "1" {
		t.Skip("opt-in real ext4 tools; enabled on CI/authorized integration host")
	}
	for _, tool := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	files := model.GuestConfig{"app.json": "{\"token\":\"test-secret\"}\n"}
	if err := writeDisk(context.Background(), dir, files); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(dir, ConfigDisk)
	info, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	data, err := exec.Command("debugfs", "-R", "cat app.json", disk).Output()
	if err != nil || string(data) != files["app.json"] {
		t.Fatalf("%q %v", data, err)
	}
	if err := writeDisk(context.Background(), dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(disk); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestFailedBuildKeepsPreviousConfigDisk(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, ConfigDisk)
	if err := os.WriteFile(destination, []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "mkfs.ext4"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	if err := writeDisk(context.Background(), dir, model.GuestConfig{"app": "new"}); err == nil {
		t.Fatal("failed formatter accepted")
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "previous" {
		t.Fatal("previous image lost", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("staging files left behind", err)
	}
}
