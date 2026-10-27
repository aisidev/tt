package netcfg

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisidev/tt/internal/runcmd"
)

func TestTAPRecoveryRetainsForeignDevice(t *testing.T) {
	n := New()
	n.runDir = t.TempDir()
	if err := os.WriteFile(n.tapRecord("vm1"), []byte("tap7"), 0600); err != nil {
		t.Fatal(err)
	}
	n.execute = func(ctx context.Context, cmd *exec.Cmd, d time.Duration) (runcmd.Result, error) {
		switch strings.Join(cmd.Args, " ") {
		case "ifconfig -l":
			return runcmd.Result{Stdout: []byte("lo0 tap7")}, nil
		case "ifconfig tap7":
			return runcmd.Result{Stdout: []byte("description: foreign")}, nil
		default:
			return runcmd.Result{}, fmt.Errorf("unexpected destructive command: %v", cmd.Args)
		}
	}
	if err := n.DestroyTap(context.Background(), "vm1"); err == nil || !strings.Contains(err.Error(), "ownership changed") {
		t.Fatal(err)
	}
	if _, err := os.Stat(n.tapRecord("vm1")); err != nil {
		t.Fatal("record removed", err)
	}
}
func TestTAPRenameRetainsCharacterDeviceRecord(t *testing.T) {
	n := New()
	n.runDir = t.TempDir()
	tap := TapName("vm1")
	var commands []string
	n.execute = func(ctx context.Context, cmd *exec.Cmd, d time.Duration) (runcmd.Result, error) {
		command := strings.Join(cmd.Args, " ")
		commands = append(commands, command)
		switch command {
		case "ifconfig -l":
			return runcmd.Result{Stdout: []byte("lo0")}, nil
		case "ifconfig tap create":
			return runcmd.Result{Stdout: []byte("tap7\n")}, nil
		case "ifconfig tt0":
			return runcmd.Result{Stdout: []byte("tt0: bridge")}, nil
		default:
			return runcmd.Result{}, nil
		}
	}
	if err := n.CreateTap(context.Background(), "vm1", "10.10.0.2"); err != nil {
		t.Fatal(err)
	}
	device, err := n.BhyveTapDevice(context.Background(), "vm1")
	if err != nil || device != "tap7" {
		t.Fatal(device, err)
	}
	if !strings.Contains(strings.Join(commands, "\n"), "ifconfig tap7 name "+tap) {
		t.Fatal(commands)
	}
	if _, err := os.Stat(filepath.Join(n.runDir, "bhyve-vm1.tap")); err != nil {
		t.Fatal(err)
	}
}

func TestPFOnlyReplacesOwnedChildAnchors(t *testing.T) {
	n := New()
	var flushed []string
	var loaded []string
	n.execute = func(ctx context.Context, cmd *exec.Cmd, d time.Duration) (runcmd.Result, error) {
		command := strings.Join(cmd.Args, " ")
		switch command {
		case "pfctl -s info":
			return runcmd.Result{Stdout: []byte("Status: Enabled")}, nil
		case "pfctl -sn":
			return runcmd.Result{Stdout: []byte(`rdr-anchor "ttstack/*" all`)}, nil
		case "pfctl -sr":
			return runcmd.Result{Stdout: []byte(`anchor "ttstack/*" all`)}, nil
		case "pfctl -a ttstack -s Anchors":
			return runcmd.Result{Stdout: []byte(" ttstack/forward-10-10-0-2-20000\nforward-10-10-0-20-20001\nforeign/forward-10-10-0-2-20000\n")}, nil
		}
		if len(cmd.Args) == 5 && cmd.Args[1] == "-a" && cmd.Args[3] == "-f" {
			if !strings.HasPrefix(cmd.Args[2], "ttstack/") {
				t.Fatal("root anchor modified")
			}
			if cmd.Args[4] != "-" {
				t.Fatal("named PF rule file leaked back in")
			}
			data, err := io.ReadAll(cmd.Stdin)
			loaded = append(loaded, cmd.Args[2]+"\n"+string(data))
			return runcmd.Result{}, err
		}
		if len(cmd.Args) == 5 && cmd.Args[3] == "-F" {
			flushed = append(flushed, command)
			return runcmd.Result{}, nil
		}
		return runcmd.Result{}, fmt.Errorf("unexpected PF command: %s", command)
	}
	ctx := context.Background()
	if err := n.SetupNAT(ctx); err != nil {
		t.Fatal(err)
	}
	if err := n.AddPortForward(ctx, 20000, "10.10.0.2", 22); err != nil {
		t.Fatal(err)
	}
	if err := n.DenyOutgoing(ctx, "10.10.0.2"); err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 || !strings.Contains(loaded[0], "rdr inet proto tcp from any to self port 20000 -> 10.10.0.2 port 22\n") || !strings.Contains(loaded[1], "block return in quick inet from 10.10.0.2 to any\n") {
		t.Fatal(loaded)
	}
	if err := n.RemovePortForwards(ctx, "10.10.0.2"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(flushed, "\n") != "pfctl -a ttstack/forward-10-10-0-2-20000 -F nat\npfctl -a ttstack/forward-10-10-0-2-20000 -F rules" {
		t.Fatal(flushed)
	}
}

func TestSetupBridgeRejectsForeignInterfaceCarryingOurName(t *testing.T) {
	n := New()
	n.runDir = t.TempDir()
	n.execute = func(ctx context.Context, cmd *exec.Cmd, d time.Duration) (runcmd.Result, error) {
		switch strings.Join(cmd.Args, " ") {
		case "ifconfig tt0":
			return runcmd.Result{Stdout: []byte("tt0: flags=8843<UP> metric 0 mtu 1500\n\tgroups: mesh wlan\n")}, nil
		default:
			return runcmd.Result{}, fmt.Errorf("unexpected configuration command: %v", cmd.Args)
		}
	}
	if err := n.SetupBridge(context.Background()); err == nil || !strings.Contains(err.Error(), "tt0 exists but is not a bridge") {
		t.Fatal(err)
	}
}
func TestSetupBridgeAcceptsExistingManagedBridge(t *testing.T) {
	n := New()
	n.runDir = t.TempDir()
	configured := []string{}
	n.execute = func(ctx context.Context, cmd *exec.Cmd, d time.Duration) (runcmd.Result, error) {
		command := strings.Join(cmd.Args, " ")
		switch command {
		case "ifconfig tt0":
			return runcmd.Result{Stdout: []byte("tt0: flags=8843<UP> metric 0 mtu 1500\n\tgroups: bridge\n")}, nil
		default:
			configured = append(configured, command)
			return runcmd.Result{}, nil
		}
	}
	if err := n.SetupBridge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(configured) != 3 || configured[0] != "ifconfig tt0 inet 10.10.0.1/16" || configured[2] != "sysctl net.inet.ip.forwarding=1" {
		t.Fatal(configured)
	}
}
func TestDestroyNamedTapRequiresOurOwnershipProof(t *testing.T) {
	tap := TapName("vm-named")
	n := New()
	n.runDir = t.TempDir()
	foreign := true
	destroyed := false
	if err := os.WriteFile(n.tapRecord("vm-named"), []byte("tap7"), 0600); err != nil {
		t.Fatal(err)
	}
	n.execute = func(ctx context.Context, cmd *exec.Cmd, d time.Duration) (runcmd.Result, error) {
		switch strings.Join(cmd.Args, " ") {
		case "ifconfig -l":
			return runcmd.Result{Stdout: []byte("lo0 " + tap)}, nil
		case "ifconfig " + tap:
			description := "description: something-else"
			if !foreign {
				description = "description: ttstack:vm-named"
			}
			return runcmd.Result{Stdout: []byte(description + "\n")}, nil
		case "ifconfig " + tap + " destroy":
			destroyed = true
			return runcmd.Result{}, nil
		default:
			return runcmd.Result{}, fmt.Errorf("unexpected command: %v", cmd.Args)
		}
	}
	if err := n.DestroyTap(context.Background(), "vm-named"); err == nil || !strings.Contains(err.Error(), "ownership changed") {
		t.Fatal(err)
	}
	if destroyed {
		t.Fatal("foreign device destroyed")
	}
	foreign = false
	if err := n.DestroyTap(context.Background(), "vm-named"); err != nil {
		t.Fatal(err)
	}
	if !destroyed {
		t.Fatal("owned device retained")
	}
}
