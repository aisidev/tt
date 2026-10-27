package agent

import (
	"context"
	"net/netip"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aisidev/tt/model"
)

func TestSSHIngressDeletesOnlyExactOwnedMapping(t *testing.T) {
	value, err := model.DecodeJSONValue([]byte(`{"nftables":[
 {"rule":{"family":"ip","table":"tt-ssh","chain":"prerouting","handle":1,"comment":"ttstack:ssh:mine"}},
 {"rule":{"family":"ip","table":"tt-ssh","chain":"prerouting","handle":2,"comment":"ttstack:ssh:other"}},
 {"rule":{"family":"ip","table":"other","chain":"prerouting","handle":3,"comment":"ttstack:ssh:mine"}},
 {"rule":{"family":"ip","table":"tt-ssh","chain":"prerouting","handle":4.0,"comment":"ttstack:ssh:mine"}}
 ]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := removeIngressRules(value.(map[string]any), "mine"); got != "delete rule ip tt-ssh prerouting handle 1\n" {
		t.Fatal(got)
	}
	if got := removeIngressRules(nil, "mine"); got != "" {
		t.Fatal(got)
	}
}
func TestSSHIngressValidatesNamespaceAndPlatform(t *testing.T) {
	dir := t.TempDir()
	target := netip.MustParseAddr("192.0.2.2")
	s := SshIngress{PublicAddress: netip.MustParseAddr("192.0.2.1"), Namespace: &dir, Target: &target}
	err := s.Validate()
	if runtime.GOOS == "linux" {
		if err != nil {
			t.Fatal(err)
		}
	} else {
		if err == nil {
			t.Fatal("non-Linux namespace accepted")
		}
		if s.Remove(context.Background(), "guest") == nil {
			t.Fatal("non-Linux mutation accepted")
		}
	}
	s.Namespace = nil
	s.Target = nil
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(context.Background(), "guest"); err != nil {
		t.Fatal(err)
	}
	s.Target = &target
	if err := s.Validate(); err == nil {
		t.Fatal("unpaired target accepted")
	}
	absent := filepath.Join(dir, "absent")
	s.Namespace = &absent
	if err := s.Validate(); err == nil {
		t.Fatal("absent namespace accepted")
	}
}
