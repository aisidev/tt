package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aisidev/tt/internal/netcfg"
	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

type Qemu struct {
	controlParent string
	gracefulOnly  bool
	runDir        string
	processes     processes
	// stopGrace bounds the graceful powerdown wait; 0 = force kill
	// immediately. Defaults to 10 s (stopGraceSeconds override applies).
	stopGrace time.Duration
}

const qemuDefaultStopGrace = 10 * time.Second

func newQemu() *Qemu {
	grace := qemuDefaultStopGrace
	if stopGraceSeconds >= 0 {
		grace = time.Duration(stopGraceSeconds) * time.Second
	}
	return &Qemu{runDir: model.RunDir, processes: hostProcesses(), stopGrace: grace}
}
func (q *Qemu) Name() string                { return "qemu" }
func (q *Qemu) pidPath(vm *model.Vm) string { return filepath.Join(q.runDir, "qemu-"+vm.ID+".pid") }
func (q *Qemu) monitorPath(vm *model.Vm) string {
	return filepath.Join(q.runDir, "qemu-"+vm.ID+".sock")
}
func (q *Qemu) seedPath(vm *model.Vm) string { return filepath.Join(q.runDir, "seed-"+vm.ID+".iso") }
func (q *Qemu) existingPID(vm *model.Vm) (uint32, error) {
	return q.processes.recover(q.pidPath(vm), []string{q.pidPath(vm)})
}
func (q *Qemu) command(vm *model.Vm, disk, format string) (*exec.Cmd, error) {
	mac, err := netcfg.MAC(vm.IP)
	if err != nil {
		return nil, fmt.Errorf("invalid guest IP: %w", err)
	}
	args := []string{"-enable-kvm", "-daemonize", "-name", vm.ID, "-m", fmt.Sprintf("%dM", vm.Mem), "-smp", strconv.FormatUint(uint64(vm.CPU), 10), "-drive", fmt.Sprintf("file=%s,format=%s,if=virtio", disk, format), "-netdev", fmt.Sprintf("tap,id=net0,ifname=%s,script=no,downscript=no", netcfg.TapName(vm.ID)), "-device", "virtio-net-pci,netdev=net0,mac=" + mac, "-pidfile", q.pidPath(vm), "-monitor", "unix:" + q.monitorPath(vm) + ",server,nowait", "-vnc", "none"}
	for index, disk := range vm.Options.DataVolumes {
		if disk.Format != "raw" {
			return nil, fmt.Errorf("data volume must use raw format")
		}
		device := fmt.Sprintf("tt-data-%d", index)
		args = append(args, "-drive", fmt.Sprintf("file=%s,format=raw,if=none,id=%s", disk.Path, device), "-device", fmt.Sprintf("virtio-blk-pci,drive=%s,serial=data-%d", device, index))
	}
	if seed := q.seedPath(vm); exists(seed) {
		args = append(args, "-drive", "file="+seed+",format=raw,if=virtio,readonly=on")
	}
	return exec.Command("qemu-system-x86_64", args...), nil
}
func qemuNetworkConfig(vm *model.Vm) (string, error) {
	mac, err := netcfg.MAC(vm.IP)
	if err != nil {
		return "", fmt.Errorf("invalid guest IP: %w", err)
	}
	return fmt.Sprintf("version: 1\nconfig:\n  - type: physical\n    name: eth0\n    mac_address: \"%s\"\n    subnets:\n      - type: static\n        address: %s/16\n        gateway: 10.10.0.1\n        dns_nameservers: [8.8.8.8, 1.1.1.1]\n", mac, vm.IP), nil
}
func (q *Qemu) userData(vm *model.Vm, keys []string) (string, error) {
	if vm.Options.SSH != nil {
		data, err := os.ReadFile(filepath.Join(q.runDir, "ssh-"+vm.ID+".sh"))
		if err != nil {
			return "", fmt.Errorf("read initial SSH seed: %w", err)
		}
		content, err := model.QuoteJSONString(string(data))
		if err != nil {
			return "", fmt.Errorf("encode initial SSH seed: %w", err)
		}
		return "#cloud-config\nssh_pwauth: false\nwrite_files:\n  - path: /var/lib/ttstack-initial-ssh.sh\n    permissions: '0600'\n    content: " + content + "\nruncmd:\n  - sh /var/lib/ttstack-initial-ssh.sh && (systemctl restart sshd 2>/dev/null || service sshd restart 2>/dev/null || rc-service sshd restart 2>/dev/null)\n", nil
	}
	text := "#cloud-config\ndisable_root: false\nssh_pwauth: false\n"
	if len(keys) > 0 {
		text += "users:\n  - name: root\n    lock_passwd: false\n    hashed_passwd: '*'\n    ssh_authorized_keys:\n"
		for _, key := range keys {
			quoted, err := model.QuoteJSONString(key)
			if err != nil {
				return "", fmt.Errorf("quote SSH key: %w", err)
			}
			text += "      - " + quoted + "\n"
		}
	}
	text += "runcmd:\n  - sed -i 's/^#*PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config\n  - sed -i 's/^#*PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config\n  - systemctl restart sshd 2>/dev/null || service sshd restart 2>/dev/null || rc-service sshd restart 2>/dev/null || true\n"
	return text, nil
}
func (q *Qemu) generateSeedISO(ctx context.Context, vm *model.Vm, keys []string) error {
	dir := filepath.Join(q.runDir, "seed-"+vm.ID)
	if err := os.MkdirAll(dir, 0777); err != nil {
		return fmt.Errorf("create seed dir: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("private SSH seed directory: %w", err)
	}
	meta := filepath.Join(dir, "meta-data")
	network := filepath.Join(dir, "network-config")
	user := filepath.Join(dir, "user-data")
	iso := filepath.Join(dir, "seed.iso")
	if err := os.WriteFile(meta, []byte(fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", vm.ID, vm.ID)), 0666); err != nil {
		return fmt.Errorf("write meta-data: %w", err)
	}
	netText, err := qemuNetworkConfig(vm)
	if err != nil {
		return err
	}
	if err := os.WriteFile(network, []byte(netText), 0666); err != nil {
		return fmt.Errorf("write network-config: %w", err)
	}
	text, err := q.userData(vm, keys)
	if err != nil {
		return err
	}
	if err := os.WriteFile(user, []byte(text), 0666); err != nil {
		return fmt.Errorf("write user-data: %w", err)
	}
	name := "mkisofs"
	args := []string{"-o", iso, "-V", "cidata", "-J", "-R", "-quiet", meta, user, network}
	if exists("/usr/bin/genisoimage") {
		name = "genisoimage"
		args = []string{"-output", iso, "-volid", "cidata", "-joliet", "-rock", "-quiet", meta, user, network}
	}
	out, err := runcmd.Run(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("generate seed ISO: %w", err)
	}
	if !out.Success() {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("seed ISO creation failed: %s", out.Stderr)
	}
	if err := os.Chmod(iso, 0600); err != nil {
		return fmt.Errorf("private seed ISO: %w", err)
	}
	if err := os.Rename(iso, q.seedPath(vm)); err != nil {
		return fmt.Errorf("publish seed ISO: %w", err)
	}
	_ = os.RemoveAll(dir)
	return nil
}
func (q *Qemu) monitor(ctx context.Context, vm *model.Vm, command string) (string, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", q.monitorPath(vm))
	if err != nil {
		return "", fmt.Errorf("QEMU monitor: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	prompt := func() (string, error) {
		reply := []byte{}
		buf := make([]byte, 1024)
		for len(reply) < 65536 {
			if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				return "", err
			}
			n, err := conn.Read(buf)
			if n > 0 {
				reply = append(reply, buf[:n]...)
				if strings.Contains(string(reply), "(qemu)") {
					return string(reply), nil
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					return "", fmt.Errorf("QEMU monitor closed")
				}
				return "", fmt.Errorf("read QEMU monitor: %w", err)
			}
		}
		return "", fmt.Errorf("QEMU monitor response too large")
	}
	if _, err := prompt(); err != nil {
		return "", err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return "", err
	}
	if _, err := fmt.Fprintln(conn, command); err != nil {
		return "", fmt.Errorf("write QEMU monitor: %w", err)
	}
	return prompt()
}
func (q *Qemu) Create(ctx context.Context, vm *model.Vm, image, format string, keys []string) error {
	if err := os.MkdirAll(q.runDir, 0777); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}
	pid, err := q.existingPID(vm)
	if err != nil {
		return err
	}
	if pid != 0 {
		return fmt.Errorf("QEMU is still running; refusing duplicate launch")
	}
	if err := q.generateSeedISO(ctx, vm, keys); err != nil {
		return err
	}
	_ = os.Remove(q.monitorPath(vm))
	_ = os.Remove(q.pidPath(vm))
	cmd, err := q.command(vm, image, format)
	if err != nil {
		return err
	}
	if q.controlParent != "" {
		domain, err := bindQemuDomain(cmd, q.controlParent, netcfg.TapName(vm.ID))
		if err != nil {
			return err
		}
		defer domain.Close()
	}
	out, err := runcmd.Output(ctx, cmd, runcmd.DefaultTimeout)
	if err != nil {
		return fmt.Errorf("spawn qemu: %w", err)
	}
	if !out.Success() {
		return fmt.Errorf("qemu launch failed: %s", out.Stderr)
	}
	return nil
}
func (q *Qemu) Start(ctx context.Context, vm *model.Vm) error {
	_, err := q.monitor(ctx, vm, "cont")
	return err
}
func (q *Qemu) Stop(ctx context.Context, vm *model.Vm) error {
	pid, err := q.existingPID(vm)
	if err != nil {
		return err
	}
	if pid == 0 {
		return nil
	}
	if q.stopGrace > 0 {
		_, _ = q.monitor(ctx, vm, "system_powerdown")
		deadline := time.Now().Add(q.stopGrace)
		for time.Now().Before(deadline) {
			match, err := q.processes.matches(pid, q.pidPath(vm))
			if err != nil {
				return err
			}
			if !match {
				return nil
			}
			if err := sleepContext(ctx, 100*time.Millisecond); err != nil {
				return err
			}
		}
	}
	if q.gracefulOnly {
		return gracefulTimeout()
	}
	return q.processes.terminate(ctx, pid, q.pidPath(vm))
}
func (q *Qemu) Destroy(ctx context.Context, vm *model.Vm) error {
	pid, err := q.existingPID(vm)
	if err != nil {
		return err
	}
	if pid != 0 {
		if err := q.processes.terminate(ctx, pid, q.pidPath(vm)); err != nil {
			return err
		}
	}
	for _, path := range []string{q.pidPath(vm), q.monitorPath(vm), q.seedPath(vm)} {
		if err := removeFile(path); err != nil {
			return fmt.Errorf("remove QEMU runtime file: %w", err)
		}
	}
	return nil
}
func (q *Qemu) State(ctx context.Context, vm *model.Vm) (model.VmState, error) {
	pid, err := q.existingPID(vm)
	if err != nil {
		return "", err
	}
	if pid == 0 {
		return model.VmStopped, nil
	}
	match, err := q.processes.matches(pid, q.pidPath(vm))
	if err != nil {
		return "", err
	}
	if !match {
		return model.VmStopped, nil
	}
	status, err := q.monitor(ctx, vm, "info status")
	if err != nil {
		return "", err
	}
	if strings.Contains(status, "paused") {
		return model.VmPaused, nil
	}
	if strings.Contains(status, "running") {
		return model.VmRunning, nil
	}
	return "", fmt.Errorf("unexpected QEMU status: %s", status)
}

func (q *Qemu) withStopPolicy(options model.StopOptions) Engine {
	clone := *q
	clone.stopGrace = policyGrace(q.stopGrace, options)
	clone.gracefulOnly = options.GracefulOnly
	return &clone
}
