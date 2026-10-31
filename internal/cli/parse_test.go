package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCLIHelpTreeMatchesPinnedRust(t *testing.T) {
	t.Setenv("TT_API_KEY", "")
	for path := range commandSpecs {
		t.Run(path, func(t *testing.T) {
			args := append(strings.Fields(path), "--help")
			var stdout, stderr bytes.Buffer
			if code := Run(args, &stdout, &stderr); code != 0 || stderr.Len() != 0 || stdout.String() != helpText(path) || stdout.Len() == 0 {
				t.Fatal(code, stdout.String(), stderr.String())
			}
		})
	}
}
func TestCLIRequiredValuesAndNumericBounds(t *testing.T) {
	for _, args := range [][]string{{}, {"env"}, {"env", "create", "name"}, {"env", "create", "name", "-i", "image", "--cpu", "4294967296"}, {"env", "create", "name", "-i", "image", "-p", "65536"}, {"env", "resize", "vm", "--cpu", "1", "--mem", "128"}, {"env", "backup", "restore", "vm"}, {"env", "backup", "create", "vm", "--operation-id", "operation"}, {"status", "--unknown"}, {"status", "extra"}, {"env", "create", "name", "-i", "image", "--cpu", "1", "--cpu", "2"}} {
		if _, err := parse(args); err == nil {
			t.Fatal(args)
		}
	}
	p, err := parse([]string{"-s127.0.0.1:9200", "env", "create", "name", "-iimage", "--image=other", "-p22", "--port=80", "--cpu=+1", "-kfixture-key"})
	if err != nil || p.server == nil || *p.server != "127.0.0.1:9200" || p.key == nil || *p.key != "fixture-key" || len(p.values["image"]) != 2 || p.number("cpu") != 1 || p.number("dup") != 1 || p.value("engine") != "qemu" {
		t.Fatal(p, err)
	}
	p, err = parse([]string{"help", "env", "backup", "restore"})
	if err != nil || !p.help || p.path != "env backup restore" {
		t.Fatal(p, err)
	}
}
func TestExpiryLabelsPreserveDeletionSemantics(t *testing.T) {
	for _, tc := range []struct {
		expires, now uint64
		want         string
	}{{0, 100, "never (retained until explicit deletion)"}, {100, 100, "expired; deletion pending"}, {99, 100, "expired; deletion pending"}, {3761, 100, "in 1h 1m 1s (deletes disks)"}} {
		if got := expiryLabel(tc.expires, tc.now); got != tc.want {
			t.Fatal(got)
		}
	}
}
func TestGuestConfigAndSSHFileInputs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "guest.json")
	for _, content := range []string{"null", `{"a":123}`, `{"a":"\ud800"}`, strings.Repeat(" ", 512*1024+1)} {
		writeFixture(t, path, content)
		if _, err := readGuestConfig(path); err == nil {
			t.Fatal("accepted invalid guest config")
		}
	}
	writeFixture(t, path, `{"config":"one\ntwo"}`)
	got, err := readGuestConfig(path)
	if err != nil || got["config"] != "one\ntwo" {
		t.Fatal(got, err)
	}
	keyPath := filepath.Join(dir, "fixture.pub")
	writeFixture(t, keyPath, "ssh-ed25519 fixture-key\n")
	keys, err := readSSHKeys([]string{keyPath, "ssh-ed25519 inline-key"})
	if err != nil || len(keys) != 2 || keys[0] != "ssh-ed25519 fixture-key" || keys[1] != "ssh-ed25519 inline-key" {
		t.Fatal(keys, err)
	}
}
func TestDeploymentTemplatesMatchPinnedRust(t *testing.T) {
	data, err := os.ReadFile("testdata/templates-rust.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Revision string
		Cases    []struct {
			Service              string
			Authenticated        bool
			Unit, Openrc, Remote string
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Revision != "1ad83a9feed5fac820d74b5e7ef752e50a552740" || len(fixture.Cases) != 4 {
		t.Fatal("oracle provenance")
	}
	for _, tc := range fixture.Cases {
		command := "/opt/tt/bin/" + tc.Service + " --listen 0.0.0.0:9100"
		path := "/opt/tt/etc/" + tc.Service + ".env"
		var envPath *string
		var env *envFile
		if tc.Authenticated {
			envPath = &path
			env = &envFile{path, "TT_API_KEY=fixture-key\n"}
		}
		if got := systemdUnit(tc.Service, command, tc.Service == "tt-agent", envPath); got != tc.Unit {
			t.Fatal("systemd template drift")
		}
		if got := openRCInit(tc.Service, command); got != tc.Openrc {
			t.Fatal("OpenRC template drift")
		}
		// Compatibility §7 deliberately propagates unsuccessful activation checks.
		// Keep the original oracle bytes; apply only that documented output delta.
		expectedRemote := tc.Remote
		for _, check := range []string{"sudo systemctl is-active " + tc.Service, "sudo rc-service " + tc.Service + " status", "pgrep -x '" + tc.Service + "'"} {
			expectedRemote = strings.Replace(expectedRemote, check+" && echo", check+"\n    echo", 1)
		}
		if got := remoteSetupScript(tc.Service, command, "fixture", "/home/fixture", "/opt/tt", "/tmp/ttstack-deploy.fixture", []string{tc.Service}, []string{"/srv/images"}, "testhost", env); got != expectedRemote {
			t.Fatal("remote template drift")
		}
	}
}

func TestEndOfOptionsCannotSelectOrExecuteASubcommand(t *testing.T) {
	t.Setenv("TT_API_KEY", "fixture-key")
	var requests atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	defer peer.Close()
	for _, args := range [][]string{{"--", "status"}, {"env", "--", "list"}, {"host", "--", "remove", "fixture"}, {"env", "backup", "--", "delete", "fixture"}} {
		var stdout, stderr bytes.Buffer
		app := newApplication(&stdout, &stderr)
		config := filepath.Join(t.TempDir(), "config")
		app.configPath = func() (string, error) { return config, nil }
		if code := app.run(append([]string{"--server", peer.URL}, args...)); code != 2 || !strings.Contains(stderr.String(), "unexpected argument") || stdout.Len() != 0 {
			t.Fatal(args, code, stdout.String(), stderr.String())
		}
	}
	if requests.Load() != 0 {
		t.Fatal("rejected input contacted the controller")
	}
	p, err := parse([]string{"env", "delete", "--", "fixture"})
	if err != nil || p.path != "env delete" || p.argument != "fixture" {
		t.Fatal(p, err)
	}
}
func TestLoneDashIsAnOptionValue(t *testing.T) {
	p, err := parse([]string{"env", "create", "fixture", "--image", "base", "--owner", "-"})
	if err != nil || p.value("owner") != "-" {
		t.Fatal(p, err)
	}
	if _, err := parse([]string{"env", "create", "fixture", "--image", "base", "--owner", "-x"}); err == nil {
		t.Fatal("option-like value accepted without equals")
	}
}
func TestMissingRequirementsAreCompleteAndDeterministic(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		missing []string
	}{{[]string{"env", "create"}, []string{"--image <IMAGE>", "<NAME>"}}, {[]string{"env", "resize", "vm"}, []string{"--cpu <CPU>", "--mem <MEM>", "--disk <DISK>"}}, {[]string{"env", "backup", "create", "--operation-id", "operation"}, []string{"--expected-revision <EXPECTED_REVISION>", "<VM_ID>"}}} {
		previous := ""
		for range 10 {
			_, err := parse(tc.args)
			if err == nil {
				t.Fatal(tc.args)
			}
			for _, label := range tc.missing {
				if !strings.Contains(err.Error(), label) {
					t.Fatal(label, err)
				}
			}
			if previous != "" && previous != err.Error() {
				t.Fatal("nondeterministic missing arguments")
			}
			previous = err.Error()
		}
	}
}
func TestMissingSubcommandPrintsItsParentHelp(t *testing.T) {
	for path := range commandChildren {
		var stdout, stderr bytes.Buffer
		if code := Run(strings.Fields(path), &stdout, &stderr); code != 2 || stdout.Len() != 0 || stderr.String() != helpText(path) {
			t.Fatal(path, code, stdout.String(), stderr.String())
		}
	}
}

func TestHelpForHelpSubcommandsMatchesRust(t *testing.T) {
	for parent := range commandChildren {
		var stdout, stderr bytes.Buffer
		args := append(strings.Fields(parent), "help", "help")
		path := strings.TrimSpace(parent + " help")
		if code := Run(args, &stdout, &stderr); code != 0 || stderr.Len() != 0 || stdout.String() != helpText(path) || stdout.Len() == 0 {
			t.Fatal(parent, code, stdout.String(), stderr.String())
		}
	}
}

func TestImageCreateRejectsEngineFilterOutsideAllRecipes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := Run([]string{"image", "create", "alpine-cloud", "--engine", "docker"}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "--engine only applies") {
		t.Fatal(code, stderr.String())
	}
}
