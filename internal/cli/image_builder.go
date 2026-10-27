package cli

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aisidev/tt/internal/runcmd"
)

type imageRecipe struct{ name, engine, description string }

var recipes = []imageRecipe{
	{"alpine", "docker", "Alpine Linux 3.21 container base (needs a long-running workload)"},
	{"debian", "docker", "Debian 13 Trixie slim container base (needs a long-running workload)"},
	{"ubuntu", "docker", "Ubuntu 24.04 container base (needs a long-running workload)"},
	{"rockylinux", "docker", "Rocky Linux 9 minimal container base (needs a long-running workload)"},
	{"nginx", "docker", "Nginx web server (Alpine-based)"},
	{"redis", "docker", "Redis 7 (Alpine-based)"},
	{"postgres", "docker", "PostgreSQL 17 (requires workload-specific initialization settings)"},
	{"fc-alpine", "firecracker", "Alpine boot/network check (kernel + 128 MiB rootfs; no SSH)"},
	{"alpine-cloud", "qemu", "Alpine Linux 3.21.7 NoCloud image (qcow2)"},
	{"debian-cloud", "qemu", "Debian 13 generic cloud image (qcow2, daily/latest)"},
	{"ubuntu-cloud", "qemu", "Ubuntu 24.04 cloud image (qcow2, current)"},
	{"freebsd-base", "jail", "FreeBSD base archive (derived from host release)"},
}

const fcKernelURL = "https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/kernels/vmlinux.bin"
const fcKernelPin = "ea5e7d5cf494a8c4ba043259812fc018b44880d70bcbbfc4d57d2760631b1cd6"
const alpineRootURL = "https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/x86_64/alpine-minirootfs-3.21.3-x86_64.tar.gz"
const alpineRootPin = "1a694899e406ce55d32334c47ac0b2efb6c06d7e878102d1840892ad44cd5239"
const alpineCloudPin = "5b22a46e9aa6bbacf585c055e87362c8be1993e53c121bdaf74203ac3490c70bdbbf714df4276eef36184ea8c11fd7cd3b28c9ccc74f6a9a82c430d441fa2f95"

type imageBuilder struct {
	app      *application
	platform string
	execute  func(context.Context, *exec.Cmd, time.Duration) (runcmd.Result, error)
	verified func(context.Context, string, string, *string) error
	download func(context.Context, string, string) error
}

func newImageBuilder(a *application) *imageBuilder {
	b := &imageBuilder{app: a, platform: runtime.GOOS, execute: toolOutput}
	b.download = downloadFile
	b.verified = b.downloadVerified
	return b
}
func (a *application) listRecipes() {
	a.out("%-16s %-14s DESCRIPTION", "NAME", "ENGINE")
	for _, recipe := range recipes {
		a.out("%-16s %-14s %s", recipe.name, recipe.engine, recipe.description)
	}
}
func (b *imageBuilder) output(ctx context.Context, name string, timeout time.Duration, args ...string) (runcmd.Result, error) {
	return b.execute(ctx, exec.Command(name, args...), timeout)
}
func (b *imageBuilder) run(ctx context.Context, name string, args ...string) error {
	out, err := b.output(ctx, name, 300*time.Second, args...)
	if err != nil {
		return fmt.Errorf("run %s: %w", name, err)
	}
	if !out.Success() {
		return fmt.Errorf("%s failed: %s", name, out.Stderr)
	}
	return nil
}
func (b *imageBuilder) create(ctx context.Context, name, directory string) error {
	for _, recipe := range recipes {
		if recipe.name != name {
			continue
		}
		switch recipe.engine {
		case "docker":
			return b.createDocker(ctx, name)
		case "firecracker":
			return b.createFirecracker(ctx, name, directory)
		case "qemu":
			return b.createQemu(ctx, name, directory)
		case "jail":
			return b.createJail(ctx, name, directory)
		default:
			return fmt.Errorf("unsupported engine: %s", recipe.engine)
		}
	}
	return fmt.Errorf("unknown image recipe '%s' (run 'tt image recipes' to list)", name)
}
func (b *imageBuilder) createAll(ctx context.Context, directory string, engine *string) error {
	matching := []imageRecipe{}
	for _, recipe := range recipes {
		if engine != nil {
			if recipe.engine != *engine {
				continue
			}
		} else if (recipe.engine == "jail" && b.platform != "freebsd") || (recipe.engine == "firecracker" && b.platform == "freebsd") {
			continue
		}
		matching = append(matching, recipe)
	}
	if engine != nil && len(matching) == 0 {
		return fmt.Errorf("no recipes for engine '%s'", *engine)
	}
	failures := []string{}
	for _, recipe := range matching {
		b.app.out("\n--- %s: %s ---", recipe.name, recipe.description)
		if err := b.create(ctx, recipe.name, directory); err != nil {
			failures = append(failures, recipe.name+": "+err.Error())
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("image creation failed: %s", strings.Join(failures, "; "))
	}
	return nil
}
func dockerTag(name string) string {
	tags := map[string]string{"alpine": "alpine:3.21", "debian": "debian:trixie-slim", "ubuntu": "ubuntu:24.04", "rockylinux": "rockylinux:9-minimal", "nginx": "nginx:alpine", "redis": "redis:7-alpine", "postgres": "postgres:17-alpine"}
	if tag, ok := tags[name]; ok {
		return tag
	}
	return name
}
func (b *imageBuilder) detectRuntime(ctx context.Context) (string, error) {
	for _, name := range []string{"docker", "podman"} {
		out, err := b.output(ctx, name, 0, "version")
		if err == nil && out.Success() {
			return name, nil
		}
	}
	return "", fmt.Errorf("neither docker nor podman found")
}
func (b *imageBuilder) createDocker(ctx context.Context, name string) error {
	runtime, err := b.detectRuntime(ctx)
	if err != nil {
		return err
	}
	tag := dockerTag(name)
	b.app.out("[image] pulling %s via %s...", tag, runtime)
	out, err := b.output(ctx, runtime, 0, "pull", tag)
	if err != nil {
		return fmt.Errorf("pull failed: %w", err)
	}
	if !out.Success() {
		return fmt.Errorf("%s pull %s failed: %s", runtime, tag, out.Stderr)
	}
	if tag != name {
		if err := b.run(ctx, runtime, "tag", tag, name); err != nil {
			return err
		}
	}
	b.app.out("[image] %s ready (%s)", name, runtime)
	return nil
}
func qemuCloudURL(name string) string {
	switch name {
	case "alpine-cloud":
		return "https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/cloud/nocloud_alpine-3.21.7-x86_64-bios-cloudinit-r0.qcow2"
	case "debian-cloud":
		return "https://cloud.debian.org/images/cloud/trixie/daily/latest/debian-13-generic-amd64-daily.qcow2"
	case "ubuntu-cloud":
		return "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
	}
	return ""
}
func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }
func humanSize(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "?"
	}
	bytes := info.Size()
	if bytes >= 1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(bytes)/1024/1024)
	}
	if bytes >= 1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%d B", bytes)
}
func imageInfo(data []byte) (string, uint64, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return "", 0, err
	}
	var format string
	var size uint64
	_ = json.Unmarshal(fields["format"], &format)
	_ = json.Unmarshal(fields["virtual-size"], &size)
	return format, size, nil
}
func (b *imageBuilder) createQemu(ctx context.Context, name, directory string) error {
	target := filepath.Join(directory, name)
	if fileExists(target) {
		out, err := b.output(ctx, "qemu-img", 0, "info", "--output=json", target)
		if err != nil {
			return fmt.Errorf("inspect existing image: %w", err)
		}
		format, size, decodeErr := imageInfo(out.Stdout)
		if !out.Success() || decodeErr != nil || format != "qcow2" || size == 0 {
			return fmt.Errorf("existing image is not a valid qcow2 disk; inspect or remove it before retrying")
		}
		b.app.out("[image] %s already exists", name)
		return nil
	}
	url := qemuCloudURL(name)
	if url == "" {
		return fmt.Errorf("unknown QEMU image: %s", name)
	}
	b.app.out("[image] downloading %s cloud image...", name)
	staging, err := os.MkdirTemp(directory, ".tt-image-*")
	if err != nil {
		return fmt.Errorf("image staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	downloaded := filepath.Join(staging, "download")
	var pin *string
	if name == "alpine-cloud" {
		value := alpineCloudPin
		pin = &value
	}
	if err := b.verified(ctx, url, downloaded, pin); err != nil {
		return err
	}
	out, err := b.output(ctx, "qemu-img", 0, "info", "--output=json", downloaded)
	if err != nil {
		return fmt.Errorf("inspect downloaded image: %w", err)
	}
	if !out.Success() {
		return fmt.Errorf("invalid cloud image: %s", out.Stderr)
	}
	format, _, err := imageInfo(out.Stdout)
	if err != nil {
		return fmt.Errorf("cloud image format: %w", err)
	}
	switch format {
	case "qcow2":
		if err := os.Rename(downloaded, target); err != nil {
			return fmt.Errorf("publish image: %w", err)
		}
	case "raw":
		converted := filepath.Join(staging, "disk.qcow2")
		if err := b.run(ctx, "qemu-img", "convert", "-f", "raw", "-O", "qcow2", downloaded, converted); err != nil {
			return err
		}
		if err := os.Rename(converted, target); err != nil {
			return fmt.Errorf("publish converted image: %w", err)
		}
	default:
		return fmt.Errorf("unsupported cloud image format; expected raw or qcow2")
	}
	b.app.out("[image] %s ready: %s", name, humanSize(target))
	return nil
}
func fileDigest(path string, use512 bool) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open digest input: %w", err)
	}
	defer file.Close()
	var digest hash.Hash = sha256.New()
	if use512 {
		digest = sha512.New()
	}
	if _, err := io.CopyBuffer(digest, file, make([]byte, 65536)); err != nil {
		return "", fmt.Errorf("read digest input: %w", err)
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}
func expectedDigest(manifest, name string) (string, error) {
	for _, line := range strings.Split(manifest, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		digest, file := fields[0], strings.TrimLeft(fields[1], "*")
		if file != name || (len(digest) != 64 && len(digest) != 128) {
			continue
		}
		valid := true
		for _, c := range []byte(digest) {
			valid = valid && ((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'))
		}
		if valid {
			return strings.ToLower(digest), nil
		}
	}
	return "", fmt.Errorf("image is missing from upstream checksum manifest")
}
func (b *imageBuilder) downloadVerified(ctx context.Context, url, destination string, pin *string) error {
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".tt-verified-*")
	if err != nil {
		return fmt.Errorf("verified download staging: %w", err)
	}
	defer os.RemoveAll(staging)
	image := filepath.Join(staging, "image")
	if err := b.download(ctx, url, image); err != nil {
		return err
	}
	var expected string
	if pin != nil {
		expected = *pin
	} else {
		index := strings.LastIndexByte(url, '/')
		if index < 0 {
			return fmt.Errorf("invalid recipe URL")
		}
		base, name := url[:index], url[index+1:]
		sums := "SHA256SUMS"
		if strings.Contains(url, "cloud.debian.org/") {
			sums = "SHA512SUMS"
		}
		manifest := filepath.Join(staging, "checksums")
		if err := b.download(ctx, base+"/"+sums, manifest); err != nil {
			return err
		}
		content, err := os.ReadFile(manifest)
		if err != nil {
			return fmt.Errorf("read image checksums: %w", err)
		}
		if !utf8.Valid(content) {
			return fmt.Errorf("read image checksums: invalid UTF-8")
		}
		expected, err = expectedDigest(string(content), name)
		if err != nil {
			return err
		}
	}
	actual, err := fileDigest(image, len(expected) == 128)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("download checksum mismatch; image was not published")
	}
	bits := 256
	if len(expected) == 128 {
		bits = 512
	}
	b.app.out("[image] verified SHA-%d: %s", bits, actual)
	if err := os.Rename(image, destination); err != nil {
		return fmt.Errorf("publish verified download: %w", err)
	}
	return nil
}
func downloadFile(ctx context.Context, url, destination string) error {
	file, err := os.CreateTemp(filepath.Dir(destination), ".tt-download-*")
	if err != nil {
		return fmt.Errorf("download staging file: %w", err)
	}
	defer file.Close()
	defer os.Remove(file.Name())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.DialContext = (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 900 * time.Second, CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) > 50 {
			return fmt.Errorf("maximum (50) redirects followed")
		}
		return nil
	}}
	request, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download %s failed: %w", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return fmt.Errorf("download %s failed: HTTP %d", url, response.StatusCode)
	}
	const maximum = int64(8589934592)
	if response.ContentLength > maximum {
		return fmt.Errorf("download %s failed: maximum file size exceeded", url)
	}
	count, err := io.Copy(file, io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return fmt.Errorf("download %s failed: %w", url, err)
	}
	if count > maximum {
		return fmt.Errorf("download %s failed: maximum file size exceeded", url)
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), destination)
}
