package agent

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/model"
)

type httpCase struct {
	Label, Method, Path string
	Input               *string
	Headers             map[string]string
	Repeat              *struct {
		Text  string
		Count int
	}
	APIKey      *string `json:"api_key"`
	Status      int
	ContentType *string `json:"content_type"`
	Allow       *string
	Body        string
}

func httpCases(t *testing.T) []httpCase {
	t.Helper()
	data, err := os.ReadFile("testdata/http-rust.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Revision string
		Cases    []httpCase
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Revision != "1ad83a9feed5fac820d74b5e7ef752e50a552740" {
		t.Fatal("wrong HTTP oracle")
	}
	return fixture.Cases
}
func TestRustJSONExtractorFixtures(t *testing.T) {
	for _, tc := range httpCases(t) {
		if tc.APIKey != nil || tc.Method != "POST" || (tc.Path != "/api/vms" && !strings.HasSuffix(tc.Path, "/resources") && !strings.HasSuffix(tc.Path, "/backup/restore")) {
			continue
		}
		t.Run(tc.Label, func(t *testing.T) {
			input := ""
			if tc.Input != nil {
				input = *tc.Input
			}
			if tc.Repeat != nil {
				input = strings.Repeat(tc.Repeat.Text, tc.Repeat.Count)
			}
			req := httptest.NewRequest(tc.Method, tc.Path, strings.NewReader(input))
			for k, v := range tc.Headers {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			var ok bool
			switch {
			case strings.HasSuffix(tc.Path, "/resources"):
				_, ok = api.ReadJSON[model.VmResources](w, req)
			case strings.HasSuffix(tc.Path, "/backup/restore"):
				_, ok = api.ReadJSON[api.RestoreBody](w, req)
			default:
				_, ok = api.ReadJSON[api.CreateVmReq](w, req)
			}
			accepted := tc.ContentType != nil && *tc.ContentType == "application/json"
			if ok != accepted {
				t.Fatalf("accepted=%v want=%v: %s", ok, accepted, w.Body.String())
			}
			if !ok && (w.Code != tc.Status || w.Body.String() != tc.Body) {
				t.Fatalf("got %d %q\nwant %d %q", w.Code, w.Body.String(), tc.Status, tc.Body)
			}
		})
	}
}

func TestRustHTTPRouterAuthAndExtractorFixtures(t *testing.T) {
	for _, tc := range httpCases(t) {
		t.Run(tc.Label, func(t *testing.T) {
			runtime, _, _ := runtimeFixture(t)
			runtime.engines = nil
			runtime.probedCapabilities = nil
			runtime.backupUnsupported = stringPointer("backup unsupported: backend has not been qualified")
			var seq int
			var name, path string
			if err := runtime.db.conn.QueryRowContext(context.Background(), "PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
				t.Fatal(err)
			}
			server := newServer(runtime, path)
			input := ""
			if tc.Input != nil {
				input = *tc.Input
			}
			if tc.Repeat != nil {
				input = strings.Repeat(tc.Repeat.Text, tc.Repeat.Count)
			}
			request := httptest.NewRequest(tc.Method, tc.Path, strings.NewReader(input))
			for k, v := range tc.Headers {
				request.Header.Set(k, v)
			}
			response := httptest.NewRecorder()
			server.Handler(tc.APIKey).ServeHTTP(response, request)
			wantType := ""
			if tc.ContentType != nil {
				wantType = *tc.ContentType
			}
			wantAllow := ""
			if tc.Allow != nil {
				wantAllow = *tc.Allow
			}
			if response.Code != tc.Status || response.Header().Get("Content-Type") != wantType || response.Header().Get("Allow") != wantAllow || response.Body.String() != tc.Body {
				t.Fatalf("got %d type=%q allow=%q body=%q\nwant %d type=%q allow=%q body=%q", response.Code, response.Header().Get("Content-Type"), response.Header().Get("Allow"), response.Body.String(), tc.Status, wantType, wantAllow, tc.Body)
			}
		})
	}
}
