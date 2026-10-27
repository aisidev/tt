package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/model"
)

type recordedRequest struct {
	Method         string  `json:"method"`
	Path           string  `json:"path"`
	Body           any     `json:"body"`
	IdempotencyKey *string `json:"idempotency_key"`
	IfMatch        *string `json:"if_match"`
}

// dropExperimentalLabel removes the Rust "(experimental)" platform suffix while
// keeping the %-23s column width, so the rest of the captured row stays exact.
func dropExperimentalLabel(text string) string {
	const marker = " (experimental)"
	return strings.ReplaceAll(text, marker, strings.Repeat(" ", len(marker)))
}

// These outputs and request sequences were captured from the pinned native Rust
// binary. Only loopback HTTP is used; no command can reach a deployment host.
// The capabilities table and freebsd-base recipe text omit the Rust experimental
// label, as documented in compatibility §7.
func TestRustCLIOutputAndRequests(t *testing.T) {
	data, err := os.ReadFile("testdata/output-rust.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Revision string
		Host     model.Host
		VM       model.Vm
		Env      model.Env
		Matrix   model.Matrix
		Backup   api.BackupView
		Status   api.FleetStatus
		Cases    []struct {
			Args           []string
			Empty          bool
			Stdout, Stderr string
			Requests       []recordedRequest
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Revision != "1ad83a9feed5fac820d74b5e7ef752e50a552740" {
		t.Fatal("unexpected oracle revision")
	}
	t.Setenv("TT_API_KEY", "fixture-key")
	for index, tc := range fixture.Cases {
		t.Run(fmt.Sprintf("%02d_%s", index, strings.Join(tc.Args, "_")), func(t *testing.T) {
			requests := make(chan recordedRequest, 32)
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body any
				decode := json.NewDecoder(r.Body)
				decode.UseNumber()
				if err := decode.Decode(&body); err != nil && err != io.EOF {
					t.Error(err)
				}
				header := func(name string) *string {
					if value := r.Header.Get(name); value != "" {
						return &value
					}
					return nil
				}
				requests <- recordedRequest{r.Method, r.URL.RequestURI(), body, header("Idempotency-Key"), header("If-Match")}
				var response any
				status := 200
				switch path := r.URL.Path; {
				case path == "/api/status":
					response = fixture.Status
				case path == "/api/capabilities":
					response = fixture.Matrix
				case path == "/api/hosts":
					if r.Method == "GET" {
						hosts := model.List[model.Host]{}
						if !tc.Empty {
							hosts = append(hosts, fixture.Host)
						}
						response = hosts
					} else {
						response = fixture.Host
					}
				case strings.HasSuffix(path, "/detach"):
					response = model.List[model.Vm]{fixture.VM}
				case strings.HasPrefix(path, "/api/hosts/"):
					if r.Method != "DELETE" {
						response = fixture.Host
					}
				case path == "/api/envs":
					if r.Method == "GET" {
						envs := model.List[model.Env]{}
						if !tc.Empty {
							envs = append(envs, fixture.Env)
						}
						response = envs
					} else {
						env, vm := fixture.Env, fixture.VM
						env.ID, env.Owner, env.State = body.(map[string]any)["id"].(string), body.(map[string]any)["owner"].(string), model.EnvActive
						vm.State = model.VmRunning
						response = api.EnvDetail{Env: env, VMs: model.List[model.Vm]{vm}, Warnings: model.List[string]{}}
						status = 202
					}
				case strings.HasPrefix(path, "/api/envs/"):
					if r.Method == "GET" {
						response = api.EnvDetail{Env: fixture.Env, VMs: model.List[model.Vm]{fixture.VM}, Warnings: model.List[string]{}}
					}
				case strings.HasSuffix(path, "/resources"):
					vm := fixture.VM
					request, _ := json.Marshal(body)
					var resources model.VmResources
					if err := json.Unmarshal(request, &resources); err != nil {
						t.Error(err)
					}
					vm.CPU, vm.Mem, vm.Disk, vm.Options.RequestedDisk = resources.CPU, resources.Mem, resources.Disk, resources.Disk
					response = vm
				case strings.Contains(path, "/backup"):
					response = fixture.Backup
				case path == "/api/images":
					images := model.List[api.ImageInfo]{}
					if !tc.Empty {
						images = append(images, api.ImageInfo{Name: "image-fixture", HostID: fixture.Host.ID})
					}
					response = images
				default:
					t.Errorf("unexpected request: %s", path)
				}
				if response == nil {
					api.WriteJSON(w, status, api.Resp[model.Unit]{OK: true})
				} else {
					api.WriteJSON(w, status, api.Success(response))
				}
			}))
			defer peer.Close()
			var stdout, stderr bytes.Buffer
			app := newApplication(&stdout, &stderr)
			privateConfig := filepath.Join(t.TempDir(), "config")
			app.configPath = func() (string, error) { return privateConfig, nil }
			if code := app.run(append([]string{"--server", peer.URL}, tc.Args...)); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			gotOut, wantOut := stdout.String(), tc.Stdout
			if reflect.DeepEqual(tc.Args, []string{"capabilities"}) {
				const note = "FreeBSD bhyve and Jail are validated only on FreeBSD 15.x.\n"
				if !strings.HasSuffix(gotOut, note) {
					t.Errorf("missing FreeBSD 15.x validation note:\n%s", gotOut)
				}
				gotOut = strings.TrimSuffix(gotOut, note)
				wantOut = dropExperimentalLabel(wantOut)
			}
			if reflect.DeepEqual(tc.Args, []string{"image", "recipes"}) {
				wantOut = strings.ReplaceAll(wantOut, "Experimental FreeBSD base archive (derived from host release)", "FreeBSD base archive (derived from host release)")
			}
			if gotOut != wantOut {
				t.Errorf("stdout differs\ngot:\n%s\nwant:\n%s", gotOut, wantOut)
			}
			if stderr.String() != tc.Stderr {
				t.Errorf("stderr differs\ngot:\n%s\nwant:\n%s", stderr.String(), tc.Stderr)
			}
			got := []recordedRequest{}
			for len(requests) > 0 {
				got = append(got, <-requests)
			}
			if !reflect.DeepEqual(got, tc.Requests) {
				t.Errorf("requests differ\ngot: %#v\nwant: %#v", got, tc.Requests)
			}
		})
	}
}
