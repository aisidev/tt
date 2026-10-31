package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisidev/tt/internal/runcmd"
)

func TestDeploymentAuthorityInputsRefuseInvalidOrRestoredLocationsBeforeRemoteWork(t *testing.T) {
	for _, part := range []string{
		"[controller]\nhost='node'\nauthority_file='/home/ttstack/ctl/control.json'",
		"[[agents]]\nhost='node'\nauthority_file='/home/ttstack/data/control.json'",
		"[[agents]]\nhost='node'\nauthority_file='/srv/images/control.json'\nimage_dir='/srv/images'",
		"[[agents]]\nhost='node'\nauthority_file='/tmp/control;command'",
	} {
		path := filepath.Join(t.TempDir(), "deploy.toml")
		writeFixture(t, path, part)
		d := newDeployer(quietApplication())
		calls := 0
		d.execute = func(context.Context, *exec.Cmd, time.Duration) (runcmd.Result, error) {
			calls++
			return runcmd.Result{}, nil
		}
		if e := d.distributed(context.Background(), path); e == nil || calls != 0 {
			t.Fatal("invalid authority reached remote work", e, calls)
		}
	}
}
func TestMissingSelectedAuthorityStopsBeforeAnyUploadOrServiceSetup(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	for _, name := range []string{"tt", "tt-ctl", "tt-agent"} {
		writeFixture(t, filepath.Join(release, name), "fixture")
	}
	path := filepath.Join(dir, "deploy.toml")
	writeFixture(t, path, fmt.Sprintf("[general]\nrelease_dir=%q\napi_key='private-fixture-key'\n[controller]\nhost='node'\nauthority_file='/srv/control/controller.json'\n[[agents]]\nhost='other-node'\nauthority_file='/srv/control/agent.json'\n", release))
	d := newDeployer(quietApplication())
	observed := false
	d.execute = func(_ context.Context, cmd *exec.Cmd, _ time.Duration) (runcmd.Result, error) {
		if cmd.Args[0] != "ssh" || cmd.Stdin == nil {
			t.Fatal("authority preflight uploaded or installed a role", cmd.Args)
		}
		body, e := io.ReadAll(cmd.Stdin)
		if e != nil {
			t.Fatal(e)
		}
		if strings.HasPrefix(string(body), "sudo test -f /srv/control/controller.json") {
			observed = true
			return runcmd.Result{ExitCode: 1}, nil
		}
		t.Fatal("unexpected effect before selected authority verification")
		return runcmd.Result{}, nil
	}
	if e := d.distributed(context.Background(), path); e == nil || !observed {
		t.Fatal("missing input did not stop deployment", e)
	}
}
