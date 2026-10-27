package netcfg

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/aisidev/tt/internal/runcmd"
	"golang.org/x/sys/unix"
)

func (n *Network) SetupBridge(ctx context.Context) error {
	exists, err := n.BridgeExists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := n.run(ctx, "ip", "link", "add", BridgeName, "type", "bridge"); err != nil {
			return err
		}
	}
	out, err := n.output(ctx, runcmd.DefaultTimeout, "ip", "-j", "-d", "link", "show", "dev", BridgeName)
	if err != nil {
		return fmt.Errorf("inspect bridge: %w", err)
	}
	var links []struct {
		LinkInfo struct {
			Kind string `json:"info_kind"`
		} `json:"linkinfo"`
	}
	if err := json.Unmarshal(out.Stdout, &links); err != nil {
		return fmt.Errorf("bridge metadata: %w", err)
	}
	bridge := false
	for _, l := range links {
		bridge = bridge || l.LinkInfo.Kind == "bridge"
	}
	if !out.Success() || !bridge {
		return fmt.Errorf("tt0 exists but is not a bridge")
	}
	if _, err := n.run(ctx, "ip", "addr", "replace", BridgeCIDR, "dev", BridgeName); err != nil {
		return err
	}
	if _, err := n.run(ctx, "ip", "link", "set", BridgeName, "up"); err != nil {
		return err
	}
	if err := n.writeFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	return nil
}
func (n *Network) BridgeExists(ctx context.Context) (bool, error) {
	out, err := n.output(ctx, runcmd.DefaultTimeout, "ip", "link", "show", BridgeName)
	return out.Success(), err
}
func (n *Network) linkExists(ctx context.Context, name string) (bool, error) {
	out, err := n.output(ctx, 10*time.Second, "ip", "-j", "link", "show")
	if err != nil {
		return false, fmt.Errorf("list links: %w", err)
	}
	if !out.Success() {
		return false, fmt.Errorf("cannot list network interfaces")
	}
	var links []struct {
		Name string `json:"ifname"`
	}
	if err := json.Unmarshal(out.Stdout, &links); err != nil {
		return false, fmt.Errorf("parse interfaces: %w", err)
	}
	for _, link := range links {
		if link.Name == name {
			return true, nil
		}
	}
	return false, nil
}
func (n *Network) TapExists(ctx context.Context, id string) (bool, error) {
	return n.linkExists(ctx, TapName(id))
}
func (n *Network) CreateTap(ctx context.Context, id, ip string) error {
	return n.createTapOwned(ctx, id, 0)
}
func (n *Network) createTapOwned(ctx context.Context, id string, uid uint32) error {
	tap := TapName(id)
	exists, err := n.linkExists(ctx, tap)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := n.run(ctx, "ip", "tuntap", "add", "dev", tap, "mode", "tap", "user", strconv.FormatUint(uint64(uid), 10)); err != nil {
			return err
		}
	}
	if _, err := n.run(ctx, "ip", "link", "set", tap, "master", BridgeName); err != nil {
		return err
	}
	_, err = n.run(ctx, "ip", "link", "set", tap, "up")
	return err
}
func (n *Network) PrepareJailedTap(ctx context.Context, id string, uid uint32) error {
	exists, err := n.TapExists(ctx, id)
	if err != nil {
		return err
	}
	if !exists {
		return n.createTapOwned(ctx, id, uid)
	}
	tun, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open stopped TAP: %w", err)
	}
	defer tun.Close()
	request, err := unix.NewIfreq(TapName(id))
	if err != nil {
		return err
	}
	request.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(int(tun.Fd()), unix.TUNSETIFF, request); err != nil {
		return fmt.Errorf("open persistent TAP queue: %w", err)
	}
	if err := unix.IoctlSetInt(int(tun.Fd()), unix.TUNSETOWNER, int(uid)); err != nil {
		return fmt.Errorf("set TAP owner: %w", err)
	}
	return nil
}
func (n *Network) DestroyTap(ctx context.Context, id string) error {
	exists, err := n.TapExists(ctx, id)
	if err != nil {
		return err
	}
	if exists {
		_, err = n.run(ctx, "ip", "link", "del", TapName(id))
	}
	return err
}
func (n *Network) nft(ctx context.Context, rule string) error {
	file, err := runcmd.AnonymousFile("", "tt-nft-*")
	if err != nil {
		return fmt.Errorf("nft input: %w", err)
	}
	defer file.Close()
	if _, err := fmt.Fprintln(file, rule); err != nil {
		return fmt.Errorf("write nft input: %w", err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = file
	out, err := n.execute(ctx, cmd, 10*time.Second)
	if err != nil {
		return fmt.Errorf("nft: %w", err)
	}
	if !out.Success() {
		return fmt.Errorf("nft %s: %s", rule, out.Stderr)
	}
	return nil
}
func (n *Network) SetupNAT(ctx context.Context) error { return n.nft(ctx, NATRules()) }
func (n *Network) AddPortForward(ctx context.Context, host uint16, ip string, guest uint16) error {
	return n.nft(ctx, fmt.Sprintf("add rule ip %s prerouting fib daddr type local tcp dport %d dnat to %s:%d", NFTTable, host, ip, guest))
}
func (n *Network) objectExists(ctx context.Context, kind, name string) (bool, error) {
	out, err := n.output(ctx, 10*time.Second, "nft", "-j", "list", "ruleset")
	if err != nil {
		return false, fmt.Errorf("list firewall objects: %w", err)
	}
	if !out.Success() {
		return false, fmt.Errorf("cannot inspect firewall objects")
	}
	var listing struct {
		NFTables []map[string]struct{ Family, Table, Name string } `json:"nftables"`
	}
	if err := json.Unmarshal(out.Stdout, &listing); err != nil {
		return false, fmt.Errorf("firewall objects: %w", err)
	}
	for _, entry := range listing.NFTables {
		v := entry[kind]
		if v.Family == "ip" && v.Table == NFTTable && v.Name == name {
			return true, nil
		}
	}
	return false, nil
}
func (n *Network) RemovePortForwards(ctx context.Context, ip string) error {
	exists, err := n.objectExists(ctx, "chain", "prerouting")
	if err != nil || !exists {
		return err
	}
	out, err := n.output(ctx, runcmd.DefaultTimeout, "nft", "-a", "list", "chain", "ip", NFTTable, "prerouting")
	if err != nil {
		return err
	}
	if !out.Success() {
		return fmt.Errorf("cannot list NAT rules: %s", out.Stderr)
	}
	for _, line := range strings.Split(string(out.Stdout), "\n") {
		matched := false
		for _, part := range strings.Fields(line) {
			head, _, _ := strings.Cut(part, ":")
			matched = matched || head == ip
		}
		if !matched {
			continue
		}
		parts := strings.Split(line, "handle ")
		handle, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(parts[len(parts)-1]), "+"), 10, 64)
		if err != nil {
			// Fail closed: reporting success here would leave a live DNAT rule
			// pointing at an address a future guest may receive.
			return fmt.Errorf("cannot parse NAT handle for %s; forward retained", ip)
		}
		if err := n.nft(ctx, fmt.Sprintf("delete rule ip %s prerouting handle %d", NFTTable, handle)); err != nil {
			return err
		}
	}
	return nil
}
func (n *Network) isDenied(ctx context.Context, ip string) (bool, error) {
	out, err := n.output(ctx, 10*time.Second, "nft", "list", "set", "ip", NFTTable, "denylist")
	if err != nil {
		return false, fmt.Errorf("list denylist: %w", err)
	}
	if !out.Success() {
		return false, fmt.Errorf("cannot read outgoing firewall rules")
	}
	for _, part := range strings.FieldsFunc(string(out.Stdout), func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(",{}", r) }) {
		if part == ip {
			return true, nil
		}
	}
	return false, nil
}
func (n *Network) DenyOutgoing(ctx context.Context, ip string) error {
	denied, err := n.isDenied(ctx, ip)
	if err != nil {
		return err
	}
	if !denied {
		return n.nft(ctx, fmt.Sprintf("add element ip %s denylist { %s }", NFTTable, ip))
	}
	return nil
}
func (n *Network) AllowOutgoing(ctx context.Context, ip string) error {
	exists, err := n.objectExists(ctx, "set", "denylist")
	if err != nil || !exists {
		return err
	}
	denied, err := n.isDenied(ctx, ip)
	if err != nil {
		return err
	}
	if denied {
		return n.nft(ctx, fmt.Sprintf("delete element ip %s denylist { %s }", NFTTable, ip))
	}
	return nil
}
func (n *Network) Isolate(ctx context.Context, id, ip string) error {
	rules, err := IsolationRules(id, ip)
	if err != nil {
		return err
	}
	return n.nft(ctx, rules)
}
func (n *Network) RemoveIsolation(ctx context.Context, id string) error {
	out, err := n.output(ctx, runcmd.DefaultTimeout, "nft", "-j", "list", "tables")
	if err != nil {
		return fmt.Errorf("list isolation tables: %w", err)
	}
	if !out.Success() {
		return fmt.Errorf("cannot list isolation tables")
	}
	var listing struct {
		NFTables []struct{ Table struct{ Family, Name string } } `json:"nftables"`
	}
	if err := json.Unmarshal(out.Stdout, &listing); err != nil {
		return fmt.Errorf("isolation tables: %w", err)
	}
	table := isolationTable(id)
	for _, family := range []string{"bridge", "inet"} {
		for _, entry := range listing.NFTables {
			if entry.Table.Family == family && entry.Table.Name == table {
				if err := n.nft(ctx, fmt.Sprintf("delete table %s %s", family, table)); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}
func (n *Network) BhyveTapDevice(ctx context.Context, id string) (string, error) {
	return "", fmt.Errorf("bhyve is not supported on this platform")
}
