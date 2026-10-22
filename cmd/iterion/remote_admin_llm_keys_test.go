package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// adminKeysRecorder runs the root command against a recording server and
// returns the requests it received.
func adminKeysRecorder(t *testing.T, args ...string) ([]string, error) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(b)))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"k1","repos":[]}`))
	}))
	defer srv.Close()
	t.Setenv("ITERION_REMOTE_URL", srv.URL)
	t.Setenv("ITERION_REMOTE_TOKEN", "test-token-not-a-secret")
	t.Setenv("ITERION_REMOTE_TEAM", "team-a")
	t.Setenv("ADMIN_KEYS_TEST_VALUE", "sk-litellm-test-value-1234567890")
	t.Cleanup(func() { resetAdminLLMKeyFlags(t) })
	rootCmd.SetArgs(args)
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	err := rootCmd.Execute()
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), got...), err
}

// Cobra keeps flag values between executions in one process (and a slice
// flag's Set APPENDS); each case starts from the defaults.
func resetAdminLLMKeyFlags(t *testing.T) {
	t.Helper()
	remoteLLMProvider, remoteLLMName, remoteLLMBaseURL, remoteLLMKeyData = "", "", "", ""
	remoteLLMFromEnv, remoteLLMFromFile = "", ""
	remoteLLMDefault = false
	remoteLLMRepos = nil
	remoteLLMKeyData = ""
	for _, name := range []string{"provider", "name", "default", "base-url", "repos", "data", "from-env", "from-file"} {
		if f := remoteAdminLLMKeysCmd.Flags().Lookup(name); f != nil {
			f.Changed = false
		}
	}
	remoteAPIKeysRotateBaseURL, remoteSecretFromEnv, remoteSecretFromFile = "", "", ""
	for _, name := range []string{"base-url", "from-env", "from-file", "scope", "team", "org"} {
		if f := remoteAPIKeysRotateCmd.Flags().Lookup(name); f != nil {
			f.Changed = false
		}
	}
}

// The create flags share the command with rotate, update and delete, which
// never read them. A flag accepted and dropped would leave the operator
// believing the key was restricted or repointed; it is refused before any
// request leaves.
func TestRemoteAdminLLMKeys_CreateOnlyFlagsAreRefusedOnOtherActions(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"rotate --repos", []string{"remote", "admin", "llm", "api-keys", "rotate", "k1", "--repos", "owner/only-this", "--from-env", "ADMIN_KEYS_TEST_VALUE"}},
		{"update --repos", []string{"remote", "admin", "llm", "api-keys", "update", "k1", "--repos", "owner/only-this", "--data", `{"name":"x"}`}},
		{"update --base-url", []string{"remote", "admin", "llm", "api-keys", "update", "k1", "--base-url", "https://gw2.example.test", "--data", `{"name":"x"}`}},
		{"delete --name", []string{"remote", "admin", "llm", "api-keys", "delete", "k1", "--name", "x"}},
		{"list --repos", []string{"remote", "admin", "llm", "api-keys", "--repos", "owner/only-this"}},
		{"rotate --data", []string{"remote", "admin", "llm", "api-keys", "rotate", "k1", "--from-env", "ADMIN_KEYS_TEST_VALUE", "--data", `{"repos":["owner/only-this"]}`}},
		{"update --from-env", []string{"remote", "admin", "llm", "api-keys", "update", "k1", "--from-env", "ADMIN_KEYS_TEST_VALUE", "--data", `{"name":"x"}`}},
		{"delete --from-file", []string{"remote", "admin", "llm", "api-keys", "delete", "k1", "--from-file", "/dev/null"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetAdminLLMKeyFlags(t)
			got, err := adminKeysRecorder(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), "applies to") || !strings.Contains(err.Error(), "only") {
				t.Fatalf("err = %v, want a refusal naming the create-only flag", err)
			}
			if len(got) != 0 {
				t.Fatalf("a request left anyway: %v", got)
			}
		})
	}
	// A gateway key moves only with its secret re-sent, so the move rides
	// rotate — on the platform twin and on the team one — and the secret
	// never travels in a JSON blob typed on the command line.
	for name, args := range map[string][]string{
		"admin rotate": {"remote", "admin", "llm", "api-keys", "rotate", "k1", "--base-url", "https://gw2.example.test", "--from-env", "ADMIN_KEYS_TEST_VALUE"},
		"team rotate":  {"remote", "api-keys", "rotate", "k1", "--base-url", "https://gw2.example.test", "--from-env", "ADMIN_KEYS_TEST_VALUE"},
	} {
		resetAdminLLMKeyFlags(t)
		got, err := adminKeysRecorder(t, args...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var patch string
		for _, g := range got {
			if strings.HasPrefix(g, "PATCH ") {
				patch = g
			}
		}
		if patch == "" || !strings.Contains(patch, `"base_url":"https://gw2.example.test"`) || !strings.Contains(patch, `"secret":"sk-litellm-test-value-1234567890"`) {
			t.Fatalf("%s sent %v, want one PATCH carrying the secret and base_url together", name, got)
		}
	}
	// list takes no positional: an id after it would be dropped.
	resetAdminLLMKeyFlags(t)
	if got, err := adminKeysRecorder(t, "remote", "admin", "llm", "api-keys", "list", "k1"); err == nil || len(got) != 0 {
		t.Fatalf("list k1: err=%v sent=%v, want a usage error and no request", err, got)
	}
	// The flags still do their job on create.
	resetAdminLLMKeyFlags(t)
	got, err := adminKeysRecorder(t, "remote", "admin", "llm", "api-keys", "create", "--provider", "openai_compatible", "--name", "pliage", "--base-url", "https://gw.example.test", "--repos", "owner/repo", "--from-env", "ADMIN_KEYS_TEST_VALUE")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], "POST /api/admin/llm/api-keys ") || !strings.Contains(got[0], `"base_url":"https://gw.example.test"`) || !strings.Contains(got[0], `"repos":["owner/repo"]`) {
		t.Fatalf("create sent %v, want one POST carrying base_url and repos", got)
	}
}
