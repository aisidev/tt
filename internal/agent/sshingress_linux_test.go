package agent

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aisidev/tt/model"
)

func TestSSHIngressAppliesOneNamespaceTransactionWithoutHostFirewallAccess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("TT_TEST_INGRESS", dir)
	script := `#!/bin/sh
set -eu
printf '%s\n' "$@" >> "$TT_TEST_INGRESS/args"
if [ "$4" = -j ]; then
 if [ "$6" = tables ]; then echo '{"nftables":[{"table":{"name":"tt-ssh","family":"ip"}}]}'
 else echo '{"nftables":[{"rule":{"family":"ip","table":"tt-ssh","chain":"prerouting","handle":42,"comment":"ttstack:ssh:mine"}}]}'
 fi
elif [ "$4" = -f ]; then /bin/cat "$5" > "$TT_TEST_INGRESS/transaction"
else exit 99
fi
`
	if err := os.WriteFile(filepath.Join(dir, "nsenter"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	target := netip.MustParseAddr("192.0.2.2")
	s := SshIngress{PublicAddress: netip.MustParseAddr("192.0.2.1"), Namespace: &dir, Target: &target}
	vm := vmFixture("mine")
	vm.SSH = &model.SshInfo{}
	vm.PortMap = model.Map[uint16, uint16]{22: 20022, 80: 20080}
	if err := s.Apply(context.Background(), &vm); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "transaction"))
	if err != nil {
		t.Fatal(err)
	}
	want := "delete rule ip tt-ssh prerouting handle 42\nadd rule ip tt-ssh prerouting ip daddr 192.0.2.1 tcp dport 20022 dnat to 192.0.2.2:20022 comment \"ttstack:ssh:mine\"\n"
	if string(data) != want {
		t.Fatal(string(data))
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.HasPrefix(string(args), "--net="+dir+"\n--\nnft\n-j\nlist\ntables\n") {
		t.Fatal(string(args))
	}
	if err := s.Remove(context.Background(), "mine"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "transaction"))
	if string(data) != "delete rule ip tt-ssh prerouting handle 42\n" {
		t.Fatal(string(data))
	}
}
