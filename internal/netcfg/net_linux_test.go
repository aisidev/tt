package netcfg

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aisidev/tt/internal/runcmd"
)

func TestBridgeSetupPlanNeverWritesRealForwarding(t *testing.T) {
	n := New()
	var commands []string
	var writes []string
	n.execute = func(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) (runcmd.Result, error) {
		command := strings.Join(cmd.Args, " ")
		commands = append(commands, command)
		switch command {
		case "ip link show tt0":
			return runcmd.Result{ExitCode: 1}, nil
		case "ip -j -d link show dev tt0":
			return runcmd.Result{Stdout: []byte(`[{"linkinfo":{"info_kind":"bridge"}}]`)}, nil
		default:
			return runcmd.Result{}, nil
		}
	}
	n.writeFile = func(path string, data []byte, mode os.FileMode) error {
		writes = append(writes, path+"="+string(data))
		return nil
	}
	if err := n.SetupBridge(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"ip link show tt0", "ip link add tt0 type bridge", "ip -j -d link show dev tt0", "ip addr replace 10.10.0.1/16 dev tt0", "ip link set tt0 up"}
	if !reflect.DeepEqual(commands, want) || !reflect.DeepEqual(writes, []string{"/proc/sys/net/ipv4/ip_forward=1"}) {
		t.Fatal(commands, writes)
	}
}
func TestCleanupOnlyMatchesExactGuestAddressAndExistingObjects(t *testing.T) {
	n := New()
	var rules []string
	n.execute = func(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) (runcmd.Result, error) {
		switch strings.Join(cmd.Args, " ") {
		case "nft -j list ruleset":
			return runcmd.Result{Stdout: []byte(`{"nftables":[{"chain":{"family":"ip","table":"tt-nat","name":"prerouting"}}]}`)}, nil
		case "nft -a list chain ip tt-nat prerouting":
			return runcmd.Result{Stdout: []byte("tcp dport 20000 dnat to 10.10.0.2:22 # handle 4\ntcp dport 20001 dnat to 10.10.0.20:22 # handle 5\n")}, nil
		case "nft -f -":
			data, err := io.ReadAll(cmd.Stdin)
			rules = append(rules, string(data))
			return runcmd.Result{}, err
		default:
			return runcmd.Result{}, fmt.Errorf("unexpected command")
		}
	}
	if err := n.RemovePortForwards(context.Background(), "10.10.0.2"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rules, []string{"delete rule ip tt-nat prerouting handle 4\n"}) {
		t.Fatal(rules)
	}
	n.execute = func(context.Context, *exec.Cmd, time.Duration) (runcmd.Result, error) {
		return runcmd.Result{Stdout: []byte(`{"nftables":[]}`)}, nil
	}
	if err := n.RemovePortForwards(context.Background(), "10.10.0.2"); err != nil {
		t.Fatal(err)
	}
}
func TestNATSetupPreservesPreroutingAndDenylist(t *testing.T) {
	rules := NATRules()
	if strings.Contains(rules, "flush chain ip tt-nat prerouting") || strings.Contains(rules, "flush set") {
		t.Fatal("restart discards mappings")
	}
	n := New()
	n.execute = func(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) (runcmd.Result, error) {
		data, err := io.ReadAll(cmd.Stdin)
		if string(data) != rules+"\n" || timeout != 10*time.Second || !reflect.DeepEqual(cmd.Args, []string{"nft", "-f", "-"}) {
			t.Fatal("NAT invocation differs")
		}
		return runcmd.Result{}, err
	}
	if err := n.SetupNAT(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNftRulesAreUnlinkedBeforeTheToolStarts(t *testing.T) {
	n := New()
	n.execute = func(_ context.Context, cmd *exec.Cmd, timeout time.Duration) (runcmd.Result, error) {
		file, ok := cmd.Stdin.(*os.File)
		if !ok {
			t.Fatal("nft stdin is not the prepared descriptor")
		}
		if _, err := os.Stat(file.Name()); !os.IsNotExist(err) {
			t.Fatal("firewall scratch file remains named", err)
		}
		data, err := io.ReadAll(file)
		if err != nil || string(data) != "fixture rules\n" || timeout != 10*time.Second {
			t.Fatal(string(data), timeout, err)
		}
		return runcmd.Result{}, nil
	}
	if err := n.nft(context.Background(), "fixture rules"); err != nil {
		t.Fatal(err)
	}
}

func TestUnparseableNATHandleFailsClosedInsteadOfLeakingTheRule(t *testing.T) {
	n := New()
	deleted := 0
	n.execute = func(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) (runcmd.Result, error) {
		command := strings.Join(cmd.Args, " ")
		var data []byte
		if cmd.Stdin != nil {
			data, _ = io.ReadAll(cmd.Stdin)
		}
		switch {
		case command == "nft -j list ruleset":
			return runcmd.Result{Stdout: []byte(`{"nftables":[{"chain":{"family":"ip","table":"tt-nat","name":"prerouting"}}]}`)}, nil
		case command == "nft -a list chain ip tt-nat prerouting":
			return runcmd.Result{Stdout: []byte("table ip tt-nat {\n chain prerouting {\n  meta l4proto tcp ip daddr 10.0.0.5 tcp dport 20000 dnat to 10.10.0.77:22 # handle corrupt?\n}\n")}, nil
		case len(data) > 0 && strings.Contains(string(data), "delete rule"):
			deleted++
			return runcmd.Result{}, nil
		}
		return runcmd.Result{}, nil
	}
	err := n.RemovePortForwards(context.Background(), "10.10.0.77")
	if err == nil || !strings.Contains(err.Error(), "cannot parse NAT handle") {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatal("a rule was deleted despite the unparseable sibling handle")
	}
}
