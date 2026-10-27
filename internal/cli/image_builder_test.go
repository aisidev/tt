package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aisidev/tt/internal/runcmd"
)

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("%s: got %q, want %q: %v", path, got, want, err)
	}
}
func quietApplication() *application { return newApplication(io.Discard, io.Discard) }

func TestFailedDownloadKeepsPreviousImageAndSuccessPublishesAtomically(t *testing.T) {
	for _, mode := range []string{"truncated", "http-error", "oversized", "complete"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "truncated":
					w.Header().Set("Content-Length", "100")
					fmt.Fprint(w, "partial")
				case "http-error":
					http.Error(w, "failed", http.StatusServiceUnavailable)
				case "oversized":
					w.Header().Set("Content-Length", "8589934593")
				default:
					fmt.Fprint(w, "complete")
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			target := filepath.Join(dir, "image")
			writeFixture(t, target, "previous")
			err := downloadFile(context.Background(), server.URL, target)
			if (err == nil) != (mode == "complete") {
				t.Fatal(err)
			}
			want := "previous"
			if mode == "complete" {
				want = "complete"
			}
			assertFile(t, target, want)
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatal(entries, err)
			}
		})
	}
}
func TestChecksumManifestsSelectTheExactFilename(t *testing.T) {
	expected := strings.Repeat("a", 64)
	manifest := strings.Repeat("b", 64) + " other.img\n" + strings.ToUpper(expected) + " *wanted.img\n"
	got, err := expectedDigest(manifest, "wanted.img")
	if err != nil || got != expected {
		t.Fatal(got, err)
	}
	for _, name := range []string{"missing.img", "wanted", "./wanted.img"} {
		if _, err := expectedDigest(manifest, name); err == nil {
			t.Fatal(name)
		}
	}
	path := filepath.Join(t.TempDir(), "image")
	writeFixture(t, path, "abc")
	got, err = fileDigest(path, false)
	if err != nil || got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal(got, err)
	}
}
func TestChecksumMismatchNeverReplacesAnExistingImage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "abc") }))
	defer server.Close()
	dir := t.TempDir()
	target := filepath.Join(dir, "image")
	writeFixture(t, target, "previous")
	b := newImageBuilder(quietApplication())
	pin := strings.Repeat("0", 64)
	if err := b.downloadVerified(context.Background(), server.URL, target, &pin); err == nil {
		t.Fatal("accepted mismatch")
	}
	assertFile(t, target, "previous")
	pin = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if err := b.downloadVerified(context.Background(), server.URL, target, &pin); err != nil {
		t.Fatal(err)
	}
	assertFile(t, target, "abc")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal(entries)
	}
}
func TestVerifiedRecipesSelectManifestAlgorithm(t *testing.T) {
	for _, name := range []string{"debian-cloud", "ubuntu-cloud"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "image")
			b := newImageBuilder(quietApplication())
			urls := []string{}
			payload := filepath.Join(dir, "payload")
			writeFixture(t, payload, "fixture")
			digest, err := fileDigest(payload, name == "debian-cloud")
			if err != nil {
				t.Fatal(err)
			}
			b.download = func(_ context.Context, url, path string) error {
				urls = append(urls, url)
				content := "fixture"
				if len(urls) == 2 {
					content = digest + " *" + filepath.Base(qemuCloudURL(name)) + "\n"
				}
				return os.WriteFile(path, []byte(content), 0600)
			}
			if err := b.downloadVerified(context.Background(), qemuCloudURL(name), target, nil); err != nil {
				t.Fatal(err)
			}
			sums := "SHA256SUMS"
			if name == "debian-cloud" {
				sums = "SHA512SUMS"
			}
			if len(urls) != 2 || !strings.HasSuffix(urls[1], "/"+sums) {
				t.Fatal(urls)
			}
			assertFile(t, target, "fixture")
		})
	}
}
func TestQemuRecipeConversionAndFailedPublication(t *testing.T) {
	for _, mode := range []string{"qcow2", "raw", "invalid", "conversion-failure", "checksum-failure"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			b := newImageBuilder(quietApplication())
			converted := false
			b.verified = func(_ context.Context, url, path string, pin *string) error {
				if url != qemuCloudURL("alpine-cloud") || pin == nil || *pin != alpineCloudPin {
					t.Fatal("recipe drift")
				}
				if mode == "checksum-failure" {
					return fmt.Errorf("checksum mismatch")
				}
				return os.WriteFile(path, []byte("downloaded"), 0600)
			}
			b.execute = func(_ context.Context, cmd *exec.Cmd, timeout time.Duration) (runcmd.Result, error) {
				wantTimeout := time.Duration(0)
				if cmd.Args[1] == "convert" {
					wantTimeout = 300 * time.Second
				}
				if timeout != wantTimeout {
					t.Fatal("recipe timeout drift", timeout, wantTimeout)
				}
				if cmd.Args[0] != "qemu-img" {
					t.Fatal(cmd.Args)
				}
				if cmd.Args[1] == "info" {
					format := mode
					if mode == "conversion-failure" {
						format = "raw"
					}
					return runcmd.Result{Stdout: []byte(fmt.Sprintf(`{"format":%q,"virtual-size":1024}`, format))}, nil
				}
				if cmd.Args[1] != "convert" || !reflect.DeepEqual(cmd.Args[2:6], []string{"-f", "raw", "-O", "qcow2"}) {
					t.Fatal(cmd.Args)
				}
				converted = true
				if mode == "conversion-failure" {
					return runcmd.Result{ExitCode: 1}, nil
				}
				return runcmd.Result{}, os.WriteFile(cmd.Args[7], []byte("converted"), 0600)
			}
			err := b.createQemu(context.Background(), "alpine-cloud", dir)
			success := mode == "qcow2" || mode == "raw"
			if (err == nil) != success {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "alpine-cloud")
			if success {
				want := "downloaded"
				if converted {
					want = "converted"
				}
				assertFile(t, target, want)
			} else if fileExists(target) {
				t.Fatal("failed image published")
			}
			entries, _ := os.ReadDir(dir)
			want := 0
			if success {
				want = 1
			}
			if len(entries) != want {
				t.Fatal(entries)
			}
		})
	}
}
func TestDockerRecipeUsesSelectedRuntimeAndAlias(t *testing.T) {
	b := newImageBuilder(quietApplication())
	calls := [][]string{}
	b.execute = func(_ context.Context, cmd *exec.Cmd, timeout time.Duration) (runcmd.Result, error) {
		wantTimeout := time.Duration(0)
		if cmd.Args[1] == "tag" {
			wantTimeout = 300 * time.Second
		}
		if timeout != wantTimeout {
			t.Fatal("container timeout drift", timeout, wantTimeout)
		}
		calls = append(calls, cmd.Args)
		if cmd.Args[0] == "docker" {
			return runcmd.Result{ExitCode: 1}, nil
		}
		return runcmd.Result{}, nil
	}
	if err := b.createDocker(context.Background(), "debian"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"docker", "version"}, {"podman", "version"}, {"podman", "pull", "debian:trixie-slim"}, {"podman", "tag", "debian:trixie-slim", "debian"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatal(calls)
	}
}
func TestUnboundedCLICommandsDoNotTreatZeroAsImmediateDeadline(t *testing.T) {
	ctx := context.Background()
	b := newImageBuilder(quietApplication())
	d := newDeployer(quietApplication())
	for _, run := range []func() (runcmd.Result, error){func() (runcmd.Result, error) { return b.output(ctx, "/bin/sh", 0, "-c", "printf ready") }, func() (runcmd.Result, error) { return d.output(ctx, "/bin/sh", "-c", "printf ready") }} {
		result, err := run()
		if err != nil || !result.Success() || string(result.Stdout) != "ready" {
			t.Fatal(result, err)
		}
	}
}

func TestDownloadsPreserveCurlRedirectLimit(t *testing.T) {
	for _, limit := range []int{50, 51} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var current int
				fmt.Sscanf(r.URL.Path, "/%d", &current)
				if current < limit {
					http.Redirect(w, r, fmt.Sprintf("/%d", current+1), http.StatusFound)
					return
				}
				fmt.Fprint(w, "complete")
			}))
			defer server.Close()
			target := filepath.Join(t.TempDir(), "image")
			writeFixture(t, target, "previous")
			err := downloadFile(context.Background(), server.URL+"/0", target)
			if (err == nil) != (limit == 50) {
				t.Fatal(limit, err)
			}
			want := "previous"
			if limit == 50 {
				want = "complete"
			}
			assertFile(t, target, want)
		})
	}
}
