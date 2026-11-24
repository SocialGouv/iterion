package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func resetRemoteForgeRepoBotsTestFlags() {
	for _, name := range []string{"data", "team", "connection-id", "bots"} {
		if flag := remoteForgeRepoBotsCmd.Flags().Lookup(name); flag != nil {
			_ = flag.Value.Set(flag.DefValue)
			flag.Changed = false
		}
	}
	jsonOutput = false
	if flag := rootCmd.PersistentFlags().Lookup("json"); flag != nil {
		flag.Changed = false
	}
	rootCmd.SetArgs(nil)
	rootCmd.SetOut(nil)
	rootCmd.SetErr(nil)
	rootCmd.SetIn(nil)
}

func runRemoteForgeRepoBots(t *testing.T, serverURL string, args ...string) error {
	t.Helper()
	resetRemoteForgeRepoBotsTestFlags()
	t.Cleanup(resetRemoteForgeRepoBotsTestFlags)
	t.Setenv("ITERION_REMOTE_URL", serverURL)
	// A SESSION credential: `repo-bots create|update` provisions, and the
	// provisioning mints the repo's webhook token, which the CLI refuses to
	// present a personal access token for before it reads an argument
	// (RefuseTeamTokenMintFromPAT). These cases are about the arguments, so
	// they hold a credential the command may actually run with; the refusal
	// itself has its own witness
	// (TestRemoteTeamTokenMints_EveryFirstHandCommandRefusesAStoredTokenBeforeSendingAnything).
	t.Setenv("ITERION_REMOTE_TOKEN", "eyJhbGciOiJIUzI1NiJ9.e30.sig")
	t.Setenv("ITERION_REMOTE_TEAM", "")
	t.Setenv("ITERION_REMOTE_ORG", "")
	rootCmd.SetArgs(append([]string{"--json", "remote", "forge", "repo-bots"}, args...))
	return rootCmd.Execute()
}

func TestRemoteForgeRepoBotsPreviewSendsConnectionAndBots(t *testing.T) {
	calls := 0
	var method, path string
	var query url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		method, path, query = r.Method, r.URL.Path, r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{"commands": []any{}})
	}))
	defer srv.Close()

	if err := runRemoteForgeRepoBots(t, srv.URL, "--team", "t1", "preview", "--connection-id", "conn /?", "--bots", " review-pr, ,revi-converse "); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || method != http.MethodGet || path != "/api/teams/t1/forge/repo-bots/preview" {
		t.Fatalf("calls=%d request=%s %s", calls, method, path)
	}
	if query.Get("connection_id") != "conn /?" || query.Get("bots") != "review-pr,revi-converse" {
		t.Fatalf("query = %v", query)
	}
	if _, ok := query["repo"]; ok {
		t.Fatalf("preview unexpectedly sent repo: %v", query)
	}
}

func TestRemoteForgeRepoBotsPreviewTrimsConnectionIDBeforeTheWire(t *testing.T) {
	var connectionID, rawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connectionID = r.URL.Query().Get("connection_id")
		rawQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"commands": []any{}})
	}))
	defer srv.Close()

	if err := runRemoteForgeRepoBots(t, srv.URL, "--team", "t1", "preview", "--connection-id", " c1 ", "--bots", "review-pr"); err != nil {
		t.Fatal(err)
	}
	if connectionID != "c1" {
		t.Fatalf("handler connection_id = %q, want c1", connectionID)
	}
	if strings.Contains(rawQuery, "%20c1%20") || strings.Contains(rawQuery, "+c1+") {
		t.Fatalf("outer spaces reached the wire: %s", rawQuery)
	}
	if !strings.Contains(rawQuery, "connection_id=c1") {
		t.Fatalf("trimmed connection_id absent from raw query: %s", rawQuery)
	}
}

func TestRemoteForgeRepoBotsUpdatePatchesDataWithoutBotIDs(t *testing.T) {
	calls := 0
	var method, path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		_ = json.NewEncoder(w).Encode(map[string]string{"integration_id": "i1"})
	}))
	defer srv.Close()

	data := `{"launch_vars":{"gate_context":"revi/review"},"auto_fix_on_gate_failure":true}`
	if err := runRemoteForgeRepoBots(t, srv.URL, "--team", "t1", "update", "i1", "--data", data); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || method != http.MethodPatch || path != "/api/teams/t1/forge/repo-bots/i1" {
		t.Fatalf("calls=%d request=%s %s", calls, method, path)
	}
	if body != data {
		t.Fatalf("body = %s, want passthrough %s", body, data)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["bot_ids"]; ok {
		t.Fatalf("update invented bot_ids: %s", body)
	}
}

func TestRemoteForgeRepoBotsValidatesBeforeNetwork(t *testing.T) {
	missingData := filepath.Join(t.TempDir(), "missing.json")
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "preview missing both", args: []string{"preview"}, wantErr: "--connection-id"},
		{name: "preview missing connection", args: []string{"preview", "--bots", "review-pr"}, wantErr: "--connection-id"},
		{name: "preview missing bots", args: []string{"preview", "--connection-id", "c1"}, wantErr: "--bots"},
		{name: "preview blank bots", args: []string{"preview", "--connection-id", "c1", "--bots", " "}, wantErr: "--bots"},
		{name: "preview comma-only bots", args: []string{"preview", "--connection-id", "c1", "--bots", ", ,"}, wantErr: "--bots"},
		{name: "list rejects preview flags", args: []string{"--connection-id", "c1"}, wantErr: "--connection-id"},
		{name: "create rejects preview flags", args: []string{"create", "--data", "{}", "--bots", "review-pr"}, wantErr: "--bots"},
		{name: "update rejects preview flags", args: []string{"update", "i1", "--data", "{}", "--connection-id", "c1"}, wantErr: "--connection-id"},
		{name: "delete rejects preview flags", args: []string{"delete", "i1", "--bots", "review-pr"}, wantErr: "--bots"},
		{name: "list rejects data", args: []string{"--data", "{}"}, wantErr: "--data"},
		{name: "preview rejects data", args: []string{"preview", "--connection-id", "c1", "--bots", "review-pr", "--data", "{}"}, wantErr: "--data"},
		{name: "delete rejects data", args: []string{"delete", "i1", "--data", "{}"}, wantErr: "--data"},
		{name: "create requires data", args: []string{"create"}, wantErr: "--data"},
		{name: "update requires data", args: []string{"update", "i1"}, wantErr: "--data"},
		{name: "update reads data before network", args: []string{"update", "i1", "--data", "@" + missingData}, wantErr: "--data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				_ = json.NewEncoder(w).Encode(map[string]any{})
			}))
			defer srv.Close()

			err := runRemoteForgeRepoBots(t, srv.URL, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to name %s", err, tc.wantErr)
			}
			if calls != 0 {
				t.Fatalf("invalid command made %d network call(s)", calls)
			}
		})
	}
}

func TestRemoteForgeRepoBotsHasNoRepoFlag(t *testing.T) {
	if flag := remoteForgeRepoBotsCmd.Flags().Lookup("repo"); flag != nil {
		t.Fatal("repo-bots must not grow a --repo flag")
	}
}
