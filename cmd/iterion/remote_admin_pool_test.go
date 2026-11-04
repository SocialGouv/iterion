package main

import (
	"encoding/json"
	"github.com/spf13/cobra"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type poolRequest struct{ method, path, query, body string }

func runPoolAdmin(t *testing.T, args []string, status int) ([]poolRequest, string, error) {
	t.Helper()
	var mu sync.Mutex
	var requests []poolRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		requests = append(requests, poolRequest{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(b)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"marker":"pool-response"}`))
	}))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_REMOTE_URL", srv.URL)
	t.Setenv("ITERION_REMOTE_TOKEN", "test-pool-token")
	t.Setenv("ITERION_REMOTE_TEAM", "test-team")
	previous := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = previous })
	// Fresh command instances avoid Cobra's Changed/value state leaking between
	// invocations. The DLQ uses the existing command's real handler and args.
	root := &cobra.Command{Use: "admin", SilenceUsage: true, SilenceErrors: true}
	dlq := &cobra.Command{Use: remoteAdminDLQCmd.Use, Args: remoteAdminDLQCmd.Args, RunE: remoteAdminDLQCmd.RunE}
	previousPool := remoteAdminDLQPool
	remoteAdminDLQPool = ""
	t.Cleanup(func() { remoteAdminDLQPool = previousPool })
	dlq.Flags().StringVar(&remoteAdminDLQPool, "pool", "", "pool")
	root.AddCommand(newRemoteAdminRunnerPoolsCmd(), newRemoteAdminTeamsCmd(), dlq)
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	var err error
	out := captureStdoutAllowingError(t, func() error { err = root.Execute(); return err })
	mu.Lock()
	defer mu.Unlock()
	return append([]poolRequest(nil), requests...), out, err
}

func TestPoolAdminCLITransport(t *testing.T) {
	data := `{"pools":[{"name":"alpha","state":"active"}]}`
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want poolRequest
	}{
		{"list", []string{"runner-pools", "list"}, poolRequest{"GET", "/api/admin/runner-pools", "", ""}},
		{"set", []string{"runner-pools", "set", "--data", data}, poolRequest{"PUT", "/api/admin/runner-pools", "", data}},
		{"file", []string{"runner-pools", "set", "--data", "@" + path}, poolRequest{"PUT", "/api/admin/runner-pools", "", data}},
		{"map", []string{"teams", "runner-pool", "team/a ?", "--pool", "alpha"}, poolRequest{"PUT", "/api/admin/teams/team%2Fa%20%3F/runner-pool", "", `{"runner_pool":"alpha"}`}},
		{"clear", []string{"teams", "runner-pool", "t1", "--clear"}, poolRequest{"PUT", "/api/admin/teams/t1/runner-pool", "", `{"runner_pool":""}`}},
		{"forced", []string{"teams", "runner-pool", "t1", "--pool", "beta", "--force"}, poolRequest{"PUT", "/api/admin/teams/t1/runner-pool", "force=1", `{"runner_pool":"beta"}`}},
		{"none", []string{"teams", "llm-fallback", "t1", "--policy", "none"}, poolRequest{"PUT", "/api/admin/teams/t1/llm-fallback", "", `{"llm_fallback":"none"}`}},
		{"platform", []string{"teams", "llm-fallback", "t1", "--policy", "platform"}, poolRequest{"PUT", "/api/admin/teams/t1/llm-fallback", "", `{"llm_fallback":"platform"}`}},
		{"fallback-clear", []string{"teams", "llm-fallback", "t1", "--clear"}, poolRequest{"PUT", "/api/admin/teams/t1/llm-fallback", "", `{"llm_fallback":""}`}},
		{"shared-dlq", []string{"dlq"}, poolRequest{"GET", "/api/admin/dlq", "", ""}},
		{"pool-dlq", []string{"dlq", "--pool", "alpha"}, poolRequest{"GET", "/api/admin/dlq", "pool=alpha", ""}},
		{"show", []string{"dlq", "show", "18446744073709551615", "--pool", "alpha"}, poolRequest{"GET", "/api/admin/dlq/18446744073709551615", "pool=alpha", ""}},
		{"replay", []string{"dlq", "replay", "3", "--pool", "alpha&force=1"}, poolRequest{"POST", "/api/admin/dlq/3/replay", "pool=alpha%26force%3D1", ""}},
		{"delete", []string{"dlq", "delete", "7", "--pool", "alpha"}, poolRequest{"DELETE", "/api/admin/dlq/7", "pool=alpha", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, out, err := runPoolAdmin(t, tc.args, 200)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, []poolRequest{tc.want}) {
				t.Fatalf("transport=%+v want%+v", got, tc.want)
			}
			var answer map[string]string
			if err := json.Unmarshal([]byte(out), &answer); err != nil || answer["marker"] != "pool-response" {
				t.Fatalf("Printer output=%q err=%v", out, err)
			}
		})
	}
}

func TestPoolAdminCLIRefusesBeforeHTTP(t *testing.T) {
	for _, args := range [][]string{
		{"runner-pools", "set"}, {"runner-pools", "set", "--data", ""}, {"runner-pools", "set", "--data", "@/this-file-does-not-exist-codex3nov"},
		{"teams", "runner-pool", "t1"}, {"teams", "runner-pool", "t1", "--pool", "alpha", "--clear"}, {"teams", "runner-pool", "t1", "--clear=false"}, {"teams", "runner-pool", "t1", "--pool", ""}, {"teams", "runner-pool", "t1", "--pool", "bad_name"}, {"teams", "runner-pool", "..", "--pool", "alpha"},
		{"teams", "llm-fallback", "t1"}, {"teams", "llm-fallback", "t1", "--policy", "none", "--clear"}, {"teams", "llm-fallback", "t1", "--clear=false"}, {"teams", "llm-fallback", "t1", "--policy", ""}, {"teams", "llm-fallback", "t1", "--policy", "shared"},
		{"dlq", "show"}, {"dlq", "show", "0"}, {"dlq", "show", "-1"}, {"dlq", "show", "18446744073709551616"}, {"dlq", "show", "../teams"}, {"dlq", "replay", "3?force=1"}, {"dlq", "delete", "3/4"}, {"dlq", "--pool", ""},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			got, _, err := runPoolAdmin(t, args, 200)
			if err == nil || len(got) != 0 {
				t.Fatalf("invalid invocation sent HTTP=%+v err=%v", got, err)
			}
		})
	}
}

func TestPoolAdminCLIServerRefusalAndRegistration(t *testing.T) {
	got, _, err := runPoolAdmin(t, []string{"teams", "runner-pool", "t1", "--pool", "alpha"}, 409)
	if len(got) != 1 || err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("server refusal=%v requests=%v", err, got)
	}
	for _, path := range [][]string{{"remote", "admin", "runner-pools", "list"}, {"remote", "admin", "runner-pools", "set"}, {"remote", "admin", "teams", "runner-pool"}, {"remote", "admin", "teams", "llm-fallback"}, {"remote", "admin", "dlq"}} {
		cmd, rest, err := rootCmd.Find(path)
		if err != nil || len(rest) != 0 || cmd.RunE == nil {
			t.Fatalf("unreachable %v: %v %v", path, rest, err)
		}
	}
}
