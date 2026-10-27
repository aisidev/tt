package agent

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

type SshIngress struct {
	PublicAddress netip.Addr  `json:"public_address"`
	Namespace     *string     `json:"namespace"`
	Target        *netip.Addr `json:"target"`
}

func (s SshIngress) Validate() error {
	if runtime.GOOS != "linux" && s.Namespace != nil {
		return fmt.Errorf("outer SSH network namespaces require Linux")
	}
	if (s.Namespace != nil) != (s.Target != nil) {
		return fmt.Errorf("SSH ingress requires both namespace and target address")
	}
	if s.Namespace != nil {
		_, err := os.Stat(*s.Namespace)
		if !filepath.IsAbs(*s.Namespace) || err != nil {
			return fmt.Errorf("SSH ingress namespace must be an existing absolute namespace path")
		}
	}
	return nil
}
func (s SshIngress) command(args ...string) *exec.Cmd {
	if s.Namespace != nil {
		return exec.Command("nsenter", append([]string{"--net=" + *s.Namespace, "--", "nft"}, args...)...)
	}
	return exec.Command("nft", args...)
}
func (s SshIngress) rules(ctx context.Context) (map[string]any, error) {
	out, err := runcmd.Output(ctx, s.command("-j", "list", "tables"), runcmd.DefaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("inspect SSH ingress tables: %w", err)
	}
	if !out.Success() {
		return nil, fmt.Errorf("cannot inspect SSH ingress tables")
	}
	value, err := model.DecodeJSONValue(out.Stdout)
	if err != nil {
		return nil, fmt.Errorf("decode SSH ingress tables: %w", err)
	}
	top, _ := value.(map[string]any)
	items, _ := top["nftables"].([]any)
	exists := false
	for _, item := range items {
		entry, _ := item.(map[string]any)
		table, _ := entry["table"].(map[string]any)
		exists = exists || (table["name"] == "tt-ssh" && table["family"] == "ip")
	}
	if !exists {
		return nil, nil
	}
	out, err = runcmd.Output(ctx, s.command("-j", "list", "table", "ip", "tt-ssh"), runcmd.DefaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("inspect SSH ingress rules: %w", err)
	}
	if !out.Success() {
		return nil, fmt.Errorf("cannot inspect SSH ingress rules")
	}
	value, err = model.DecodeJSONValue(out.Stdout)
	if err != nil {
		return nil, fmt.Errorf("decode SSH ingress rules: %w", err)
	}
	// A present table with an unexpected inventory shape is still represented as
	// present, as in serde_json::Value. Only exact owned rules can be removed.
	rules, _ := value.(map[string]any)
	if rules == nil {
		rules = map[string]any{}
	}
	return rules, nil
}
func (s SshIngress) write(ctx context.Context, script string) error {
	if script == "" {
		return nil
	}
	// The ruleset travels over stdin from an open, unlinked scratch file; an
	// interruption leaves neither a named file nor a partial command line.
	file, err := runcmd.AnonymousFile("", "tt-ssh-ingress-")
	if err != nil {
		return fmt.Errorf("SSH ingress transaction: %w", err)
	}
	defer file.Close()
	if _, err := file.WriteString(script); err != nil {
		return fmt.Errorf("write SSH ingress transaction: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind SSH ingress transaction: %w", err)
	}
	cmd := s.command("-f", "-")
	cmd.Stdin = file
	out, err := runcmd.Output(ctx, cmd, runcmd.DefaultTimeout)
	if err != nil {
		return fmt.Errorf("apply SSH ingress: %w", err)
	}
	if !out.Success() {
		return fmt.Errorf("cannot apply SSH ingress transaction")
	}
	return nil
}
func (s SshIngress) Apply(ctx context.Context, vm *model.Vm) error {
	if runtime.GOOS != "linux" {
		return s.Validate()
	}
	if s.Namespace == nil || vm.SSH == nil {
		return nil
	}
	port, ok := vm.PortMap[22]
	if !ok {
		return fmt.Errorf("SSH guest port is not mapped")
	}
	rules, err := s.rules(ctx)
	if err != nil {
		return err
	}
	script := ""
	if rules == nil {
		script = "add table ip tt-ssh\nadd chain ip tt-ssh prerouting { type nat hook prerouting priority -105; policy accept; }\n"
	}
	script += removeIngressRules(rules, vm.ID)
	comment, _ := model.QuoteJSONString("ttstack:ssh:" + vm.ID)
	script += fmt.Sprintf("add rule ip tt-ssh prerouting ip daddr %s tcp dport %d dnat to %s:%d comment %s\n", s.PublicAddress, port, s.Target, port, comment)
	return s.write(ctx, script)
}
func (s SshIngress) Remove(ctx context.Context, id string) error {
	if runtime.GOOS != "linux" {
		return s.Validate()
	}
	if s.Namespace == nil {
		return nil
	}
	rules, err := s.rules(ctx)
	if err != nil {
		return err
	}
	return s.write(ctx, removeIngressRules(rules, id))
}
func removeIngressRules(rules map[string]any, id string) string {
	script := ""
	items, _ := rules["nftables"].([]any)
	for _, item := range items {
		entry, _ := item.(map[string]any)
		rule, _ := entry["rule"].(map[string]any)
		if rule["family"] != "ip" || rule["table"] != "tt-ssh" || rule["chain"] != "prerouting" || rule["comment"] != "ttstack:ssh:"+id {
			continue
		}
		// JSON values retain their number lexemes, so floats and negative handles
		// cannot be silently coerced into a rule selected for deletion.
		value, ok := rule["handle"].(interface{ String() string })
		if !ok {
			continue
		}
		handle, err := strconv.ParseUint(value.String(), 10, 64)
		if err != nil {
			continue
		}
		script += fmt.Sprintf("delete rule ip tt-ssh prerouting handle %d\n", handle)
	}
	return script
}
