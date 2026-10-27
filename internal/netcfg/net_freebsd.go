package netcfg

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aisidev/tt/internal/runcmd"
)

func (n *Network) BridgeExists(ctx context.Context) (bool, error) {
	out, err := n.output(ctx, runcmd.DefaultTimeout, "ifconfig", BridgeName)
	return out.Success(), err
}
func (n *Network) SetupBridge(ctx context.Context) error {
	exists, err := n.BridgeExists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := n.run(ctx, "ifconfig", "bridge", "create", "name", BridgeName); err != nil {
			return err
		}
	} else {
		// Never address a foreign interface that happens to carry our name.
		info, err := n.run(ctx, "ifconfig", BridgeName)
		if err != nil {
			return err
		}
		if !strings.Contains(info, "groups: bridge") {
			return fmt.Errorf("%s exists but is not a bridge", BridgeName)
		}
	}
	for _, args := range [][]string{{"ifconfig", BridgeName, "inet", BridgeCIDR}, {"ifconfig", BridgeName, "up"}, {"sysctl", "net.inet.ip.forwarding=1"}} {
		if _, err := n.run(ctx, args[0], args[1:]...); err != nil {
			return err
		}
	}
	return nil
}
func (n *Network) tapRecord(id string) string { return filepath.Join(n.runDir, "bhyve-"+id+".tap") }
func (n *Network) BhyveTapDevice(ctx context.Context, id string) (string, error) {
	data, err := os.ReadFile(n.tapRecord(id))
	if err != nil {
		return "", fmt.Errorf("read bhyve TAP device: %w", err)
	}
	name := string(data)
	digits, ok := strings.CutPrefix(name, "tap")
	valid := ok && digits != ""
	for _, c := range []byte(digits) {
		if c < '0' || c > '9' {
			valid = false
		}
	}
	if !valid {
		return "", fmt.Errorf("invalid bhyve TAP device record")
	}
	return name, nil
}
func containsWord(text, word string) bool {
	for _, w := range strings.Fields(text) {
		if w == word {
			return true
		}
	}
	return false
}
func (n *Network) TapExists(ctx context.Context, id string) (bool, error) {
	out, err := n.output(ctx, runcmd.DefaultTimeout, "ifconfig", "-l")
	if err != nil {
		return false, fmt.Errorf("list interfaces: %w", err)
	}
	if !out.Success() {
		return false, fmt.Errorf("cannot inspect host interfaces")
	}
	return containsWord(string(out.Stdout), TapName(id)), nil
}
func (n *Network) CreateTap(ctx context.Context, id, ip string) error {
	tap := TapName(id)
	interfaces, err := n.run(ctx, "ifconfig", "-l")
	if err != nil {
		return err
	}
	if !containsWord(interfaces, tap) {
		device := ""
		if _, err := os.Stat(n.tapRecord(id)); err == nil {
			saved, err := n.BhyveTapDevice(ctx, id)
			if err != nil {
				return err
			}
			if containsWord(interfaces, saved) {
				info, err := n.run(ctx, "ifconfig", saved)
				if err != nil {
					return err
				}
				if !strings.Contains(info, "description: ttstack:"+id) {
					return fmt.Errorf("TAP device ownership changed; retain resources")
				}
				device = saved
			}
		}
		if device == "" {
			created, err := n.run(ctx, "ifconfig", "tap", "create")
			if err != nil {
				return err
			}
			device = strings.TrimSpace(created)
			if _, err := n.run(ctx, "ifconfig", device, "description", "ttstack:"+id); err != nil {
				return err
			}
			if err := os.MkdirAll(n.runDir, 0777); err != nil {
				return err
			}
			if err := n.writeFile(n.tapRecord(id), []byte(device), 0666); err != nil {
				if _, cleanup := n.run(ctx, "ifconfig", device, "destroy"); cleanup != nil {
					return cleanup
				}
				return err
			}
		}
		if _, err := n.run(ctx, "ifconfig", device, "name", tap); err != nil {
			return err
		}
	}
	if _, err := n.BhyveTapDevice(ctx, id); err != nil {
		return err
	}
	bridge, err := n.run(ctx, "ifconfig", BridgeName)
	if err != nil {
		return err
	}
	member := false
	for _, line := range strings.Split(bridge, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "member: "+tap+" ") {
			member = true
		}
	}
	if !member {
		if _, err := n.run(ctx, "ifconfig", BridgeName, "addm", tap); err != nil {
			return err
		}
	}
	_, err = n.run(ctx, "ifconfig", tap, "up")
	return err
}
func (n *Network) DestroyTap(ctx context.Context, id string) error {
	tap := TapName(id)
	interfaces, err := n.run(ctx, "ifconfig", "-l")
	if err != nil {
		return err
	}
	if containsWord(interfaces, tap) {
		info, err := n.run(ctx, "ifconfig", tap)
		if err != nil {
			return err
		}
		if !strings.Contains(info, "description: ttstack:"+id) {
			return fmt.Errorf("TAP device ownership changed; retain resources")
		}
		if _, err := n.run(ctx, "ifconfig", tap, "destroy"); err != nil {
			return err
		}
	} else if _, err := os.Stat(n.tapRecord(id)); err == nil {
		saved, err := n.BhyveTapDevice(ctx, id)
		if err != nil {
			return err
		}
		if containsWord(interfaces, saved) {
			info, err := n.run(ctx, "ifconfig", saved)
			if err != nil {
				return err
			}
			if !strings.Contains(info, "description: ttstack:"+id) {
				return fmt.Errorf("TAP device ownership changed; retain resources")
			}
			if _, err := n.run(ctx, "ifconfig", saved, "destroy"); err != nil {
				return err
			}
		}
	}
	err = os.Remove(n.tapRecord(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func (n *Network) SetupNAT(ctx context.Context) error {
	info, err := n.run(ctx, "pfctl", "-s", "info")
	if err != nil {
		return err
	}
	nat, err := n.run(ctx, "pfctl", "-sn")
	if err != nil {
		return err
	}
	filter, err := n.run(ctx, "pfctl", "-sr")
	if err != nil {
		return err
	}
	if !PFHooksReady(info, nat, filter) {
		return fmt.Errorf("enable PF and configure rdr-anchor \"ttstack/*\" and anchor \"ttstack/*\" without quick before starting guests; provision outbound NAT separately")
	}
	return nil
}
func (n *Network) loadAnchor(ctx context.Context, anchor, rule string) error {
	// Rules travel over stdin from an open, unlinked scratch file rather than a
	// named path in a shared temporary directory.
	file, err := runcmd.AnonymousFile("", "tt-pf-")
	if err != nil {
		return fmt.Errorf("PF rule file: %w", err)
	}
	defer file.Close()
	if _, err := file.WriteString(rule); err != nil {
		return fmt.Errorf("write PF rule: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind PF rule: %w", err)
	}
	cmd := exec.Command("pfctl", "-a", anchor, "-f", "-")
	cmd.Stdin = file
	out, err := n.execute(ctx, cmd, runcmd.DefaultTimeout)
	if err != nil {
		return fmt.Errorf("load PF anchor: %w", err)
	}
	if !out.Success() {
		return fmt.Errorf("load PF anchor: %s", out.Stderr)
	}
	return nil
}
func (n *Network) AddPortForward(ctx context.Context, host uint16, ip string, guest uint16) error {
	anchor, rule, err := PFForwardRule(host, ip, guest)
	if err != nil {
		return err
	}
	return n.loadAnchor(ctx, anchor, rule)
}
func (n *Network) RemovePortForwards(ctx context.Context, ip string) error {
	key, err := addressKey(ip)
	if err != nil {
		return err
	}
	prefix := "forward-" + key + "-"
	anchors, err := n.run(ctx, "pfctl", "-a", "ttstack", "-s", "Anchors")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(anchors, "\n") {
		if name, ok := ownedForwardAnchor(line, prefix); ok {
			for _, kind := range []string{"nat", "rules"} {
				if _, err := n.run(ctx, "pfctl", "-a", "ttstack/"+name, "-F", kind); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (n *Network) DenyOutgoing(ctx context.Context, ip string) error {
	key, err := addressKey(ip)
	if err != nil {
		return err
	}
	return n.loadAnchor(ctx, "ttstack/deny-"+key, fmt.Sprintf("pass in quick inet from %s to self keep state (if-bound)\nblock return in quick inet from %s to any\n", ip, ip))
}
func (n *Network) AllowOutgoing(ctx context.Context, ip string) error {
	key, err := addressKey(ip)
	if err != nil {
		return err
	}
	for _, kind := range []string{"nat", "rules"} {
		if _, err := n.run(ctx, "pfctl", "-a", "ttstack/deny-"+key, "-F", kind); err != nil {
			return err
		}
	}
	return nil
}
func (n *Network) PrepareJailedTap(ctx context.Context, id string, uid uint32) error {
	return fmt.Errorf("Firecracker is not supported on this platform")
}
func (n *Network) Isolate(ctx context.Context, id, ip string) error {
	return fmt.Errorf("guest isolation is not supported on this platform")
}
func (n *Network) RemoveIsolation(ctx context.Context, id string) error {
	return fmt.Errorf("guest isolation is not supported on this platform")
}
