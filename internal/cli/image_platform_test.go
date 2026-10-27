package cli

import (
	"context"
	"fmt"
	"github.com/aisidev/tt/internal/runcmd"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFreeBSDRecipeUsesExactReleaseAndOfficialArchitecturePath(t *testing.T) {
	got, err := freeBSDBaseURL("15.1-RELEASE-p3\n")
	if err != nil || got != "https://download.freebsd.org/releases/amd64/amd64/15.1-RELEASE/base.txz" {
		t.Fatal(got, err)
	}
	for _, value := range []string{"", "15.0-STABLE", "15.1/../../-RELEASE", "15-RELEASE"} {
		if _, err := freeBSDBaseURL(value); err == nil {
			t.Fatal(value)
		}
	}
}
func TestJailArchiveFailureIsNotPublishedAndCanBeRetried(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "freebsd-base")
	b := newImageBuilder(quietApplication())
	b.platform = "freebsd"
	complete := false
	b.execute = func(_ context.Context, cmd *exec.Cmd, _ time.Duration) (runcmd.Result, error) {
		if cmd.Args[0] != "tar" || cmd.Args[len(cmd.Args)-1] != "--no-fflags" {
			t.Fatal(cmd.Args)
		}
		if complete {
			for _, name := range []string{"bin/sh", "etc/rc", "etc/master.passwd"} {
				writeFixture(t, filepath.Join(cmd.Args[4], name), "fixture")
			}
		}
		return runcmd.Result{}, nil
	}
	if err := b.installJailArchive(context.Background(), "fixture.txz", target); err == nil {
		t.Fatal("incomplete archive published")
	}
	if fileExists(target) {
		t.Fatal("target created")
	}
	complete = true
	if err := b.installJailArchive(context.Background(), "fixture.txz", target); err != nil {
		t.Fatal(err)
	}
	if !jailImageComplete(target) {
		t.Fatal("missing completion marker")
	}
	assertFile(t, filepath.Join(target, "etc/rc.conf"), "sendmail_enable=\"NONE\"\nsyslogd_flags=\"-ss\"\nsshd_enable=\"YES\"\n")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal(entries)
	}
	b.platform = "linux"
	if err := b.createJail(context.Background(), "freebsd-base", dir); err == nil {
		t.Fatal("wrong OS accepted")
	}
}
func TestFirecrackerAlwaysUnmountsAndRetainsUnknownMountOutcome(t *testing.T) {
	for _, mode := range []string{"success", "populate-failure", "unmount-failure"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			b := newImageBuilder(quietApplication())
			mounted, unmounted := false, false
			mount, rootfs := "", ""
			b.verified = func(_ context.Context, url, path string, pin *string) error {
				if url == fcKernelURL {
					if pin == nil || *pin != fcKernelPin {
						t.Fatal("kernel pin")
					}
				} else {
					if !mounted || pin == nil || *pin != alpineRootPin {
						t.Fatal("root download sequence/pin")
					}
					if mode == "populate-failure" {
						return fmt.Errorf("download failed")
					}
				}
				return os.WriteFile(path, []byte("fixture"), 0600)
			}
			b.execute = func(_ context.Context, cmd *exec.Cmd, timeout time.Duration) (runcmd.Result, error) {
				if timeout <= 0 {
					t.Fatal("timeout")
				}
				switch cmd.Args[0] {
				case "dd":
					if !strings.HasPrefix(cmd.Args[2], "of=") || cmd.Args[4] != "count=128" {
						t.Fatal(cmd.Args)
					}
					rootfs = strings.TrimPrefix(cmd.Args[2], "of=")
					writeFixture(t, rootfs, "fake rootfs")
				case "mkfs.ext4":
					if cmd.Args[2] != rootfs {
						t.Fatal(cmd.Args)
					}
				case "mount":
					mounted = true
					mount = cmd.Args[4]
					if cmd.Args[3] != rootfs {
						t.Fatal(cmd.Args)
					}
				case "tar":
					writeFixture(t, filepath.Join(mount, "etc/placeholder"), "")
				case "chmod":
				case "umount":
					unmounted = true
					if cmd.Args[1] != mount {
						t.Fatal(cmd.Args)
					}
					if mode == "unmount-failure" {
						return runcmd.Result{ExitCode: 1}, nil
					}
				default:
					t.Fatal(cmd.Args)
				}
				return runcmd.Result{}, nil
			}
			err := b.createFirecracker(context.Background(), "fc-alpine", dir)
			// This is a fake mount: remove only the temporary directory recorded by the fake.
			if mount != "" {
				defer os.RemoveAll(mount)
			}
			if !mounted || !unmounted || (err == nil) != (mode == "success") {
				t.Fatal(mounted, unmounted, err)
			}
			if mode == "unmount-failure" {
				if !fileExists(rootfs) || !fileExists(mount) || !strings.Contains(err.Error(), rootfs) || !strings.Contains(err.Error(), mount) {
					t.Fatal("unknown mount outcome discarded", err)
				}
			} else if fileExists(rootfs) || fileExists(mount) {
				t.Fatal("staging leaked")
			}
			if fileExists(filepath.Join(dir, "fc-alpine/rootfs.ext4")) != (mode == "success") {
				t.Fatal("publication state")
			}
		})
	}
}
