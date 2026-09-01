package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

func remoteTestClient(t *testing.T, handler http.Handler) *cli.RemoteClient {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: ts.URL, Token: "iap_test"})
}

func remotePrinter(format cli.OutputFormat) (*cli.Printer, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &cli.Printer{W: buf, Format: format}, buf
}

func remoteWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- config resolution ---

func TestResolveRemoteConfig_EnvMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no stored config
	t.Setenv("ITERION_REMOTE_URL", "https://cloud.example/")
	t.Setenv("ITERION_REMOTE_TOKEN", "iap_env")
	t.Setenv("ITERION_REMOTE_TEAM", "team-env")

	cfg, err := cli.ResolveRemoteConfig()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.BaseURL != "https://cloud.example" {
		t.Errorf("BaseURL = %q, want trailing slash trimmed", cfg.BaseURL)
	}
	if cfg.Token != "iap_env" {
		t.Errorf("Token = %q", cfg.Token)
	}
	if cfg.TeamID != "team-env" {
		t.Errorf("TeamID = %q", cfg.TeamID)
	}
}

func TestResolveRemoteConfig_EnvURLIgnoresStoredToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := cli.SaveRemoteConfig(cli.RemoteConfig{BaseURL: "https://stored.example", Token: "iap_stored"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_REMOTE_URL", "https://other.example")
	t.Setenv("ITERION_REMOTE_TOKEN", "")
	t.Setenv("ITERION_TOKEN", "")

	cfg, err := cli.ResolveRemoteConfig()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The stored token must never be sent to a different host.
	if cfg.Token != "" {
		t.Errorf("Token = %q, want empty in env mode without env token", cfg.Token)
	}
	if cfg.BaseURL != "https://other.example" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
}

func TestResolveRemoteConfig_NotConfigured(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_REMOTE_URL", "")
	_, err := cli.ResolveRemoteConfig()
	if err == nil || !strings.Contains(err.Error(), "ITERION_REMOTE_URL") {
		t.Fatalf("want explicit no-remote error naming the env var, got: %v", err)
	}
}

// --- Call / APIError ---

func TestRemoteClientCall_DecodeAndAuth(t *testing.T) {
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer iap_test" {
			t.Errorf("Authorization = %q", got)
		}
		if r.URL.Path != "/api/thing" || r.Method != "GET" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"value":42}`)
	}))
	var out struct {
		Value int `json:"value"`
	}
	raw, err := c.Call(context.Background(), "GET", "/api/thing", nil, &out)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out.Value != 42 {
		t.Errorf("decoded value = %d", out.Value)
	}
	if !strings.Contains(string(raw), `"value":42`) {
		t.Errorf("raw = %s", raw)
	}
}

func TestRemoteClientCall_APIError(t *testing.T) {
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden: not a member", http.StatusForbidden)
	}))
	_, err := c.Call(context.Background(), "POST", "/api/thing", map[string]string{"a": "b"}, nil)
	var apiErr *cli.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.Status != 403 || apiErr.Method != "POST" || apiErr.Path != "/api/thing" {
		t.Errorf("APIError = %+v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "HTTP 403 POST /api/thing") || !strings.Contains(apiErr.Error(), "not a member") {
		t.Errorf("Error() = %q", apiErr.Error())
	}
}

func TestRemoteClientUpload_Multipart(t *testing.T) {
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		if string(b) != "payload" || hdr.Filename != "x.botz" {
			t.Errorf("file = %q name %q", b, hdr.Filename)
		}
		if r.FormValue("extra") != "v" {
			t.Errorf("extra = %q", r.FormValue("extra"))
		}
		fmt.Fprint(w, `{"upload_id":"u1"}`)
	}))
	var out struct {
		UploadID string `json:"upload_id"`
	}
	if _, err := c.Upload(context.Background(), "/api/runs/uploads", "file", "x.botz",
		strings.NewReader("payload"), map[string]string{"extra": "v"}, &out); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if out.UploadID != "u1" {
		t.Errorf("upload id = %q", out.UploadID)
	}
}

// --- team resolution ---

func TestResolveTeam_Chain(t *testing.T) {
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"active_team_id":"team-me"}`)
	}))

	// Flag wins without any HTTP call needed.
	got, err := c.ResolveTeam(context.Background(), "team-flag")
	if err != nil || got != "team-flag" {
		t.Fatalf("flag: %q %v", got, err)
	}

	// Falls through to /api/auth/me.
	got, err = c.ResolveTeam(context.Background(), "")
	if err != nil || got != "team-me" {
		t.Fatalf("me fallback: %q %v", got, err)
	}
}

func TestResolveTeam_ConfigDefault(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("must not call the API when config carries a team")
	}))
	defer ts.Close()
	c := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: ts.URL, Token: "iap_test", TeamID: "team-cfg"})
	got, err := c.ResolveTeam(context.Background(), "")
	if err != nil || got != "team-cfg" {
		t.Fatalf("config default: %q %v", got, err)
	}
}

// --- runs ---

func TestRemoteRunsList_Table(t *testing.T) {
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "running" || r.URL.Query().Get("limit") != "5" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"runs":[{"id":"r1","name":"demo","workflow_name":"wf","status":"running","created_at":"2026-07-10T10:00:00Z"}]}`)
	}))
	p, buf := remotePrinter(cli.OutputHuman)
	if err := cli.RemoteRunsList(context.Background(), c, p, cli.RemoteRunsListOptions{Status: "running", Limit: 5}); err != nil {
		t.Fatalf("list: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "r1") || !strings.Contains(out, "demo") || !strings.Contains(out, "running") {
		t.Errorf("table output missing fields:\n%s", out)
	}
}

func TestRemoteRunsLaunch_SendsSourceAndVars(t *testing.T) {
	dir := t.TempDir()
	botPath := dir + "/wf.bot"
	remoteWriteFile(t, botPath, "workflow demo:\n")
	var got map[string]any
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/runs" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		fmt.Fprint(w, `{"run_id":"r9","status":"running"}`)
	}))
	p, buf := remotePrinter(cli.OutputHuman)
	err := cli.RemoteRunsLaunch(context.Background(), c, p, cli.RemoteRunsLaunchOptions{
		FilePath: botPath,
		Vars:     map[string]string{"k": "v"},
		Backend:  "claw",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if got["source"] != "workflow demo:\n" {
		t.Errorf("source = %v", got["source"])
	}
	if got["backend"] != "claw" {
		t.Errorf("backend = %v", got["backend"])
	}
	vars, _ := got["vars"].(map[string]any)
	if vars["k"] != "v" {
		t.Errorf("vars = %v", got["vars"])
	}
	if !strings.Contains(buf.String(), "r9") {
		t.Errorf("output = %s", buf.String())
	}
}

func TestRemoteRunsFollow_CursorAndTerminal(t *testing.T) {
	page := 0
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			from := r.URL.Query().Get("from")
			page++
			switch page {
			case 1:
				if from != "0" {
					t.Errorf("page1 from = %s", from)
				}
				fmt.Fprint(w, `{"events":[{"seq":1,"type":"run_started","timestamp":"2026-07-10T10:00:00Z"},{"seq":2,"type":"node_started","timestamp":"2026-07-10T10:00:01Z"}]}`)
			case 2:
				if from != "3" {
					t.Errorf("page2 from = %s (cursor must advance past seq 2)", from)
				}
				fmt.Fprint(w, `{"events":[]}`)
			default:
				fmt.Fprint(w, `{"events":[]}`)
			}
		default:
			fmt.Fprint(w, `{"run":{"id":"r1","status":"finished"}}`)
		}
	}))
	p, buf := remotePrinter(cli.OutputHuman)
	if err := cli.RemoteRunsFollow(context.Background(), c, p, "r1", time.Millisecond); err != nil {
		t.Fatalf("follow: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "run_started") || !strings.Contains(out, "finished") {
		t.Errorf("follow output:\n%s", out)
	}
}

func TestRemoteRunsFollow_ToleratesLaunchRace(t *testing.T) {
	// The POST /api/runs response can precede the store's first write:
	// the run 404s briefly. Follow must ride it out, not error.
	calls := 0
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			http.Error(w, `{"error":"run not found"}`, http.StatusNotFound)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/events") {
			fmt.Fprint(w, `{"events":[]}`)
			return
		}
		fmt.Fprint(w, `{"run":{"id":"r1","status":"finished"}}`)
	}))
	p, _ := remotePrinter(cli.OutputHuman)
	if err := cli.RemoteRunsFollow(context.Background(), c, p, "r1", time.Millisecond); err != nil {
		t.Fatalf("follow must tolerate the launch race: %v", err)
	}
}

func TestRemoteRunsFollow_FailureIsError(t *testing.T) {
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			fmt.Fprint(w, `{"events":[]}`)
			return
		}
		fmt.Fprint(w, `{"run":{"id":"r1","status":"failed"}}`)
	}))
	p, _ := remotePrinter(cli.OutputHuman)
	err := cli.RemoteRunsFollow(context.Background(), c, p, "r1", time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("want terminal-failure error, got %v", err)
	}
}

// --- tokens ---

func TestRemoteTokensCreate_PrintsPlaintextOnce(t *testing.T) {
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["name"] != "ci" || req["team_id"] != "team-1" {
			t.Errorf("req = %v", req)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"pat":{"id":"p1","name":"ci"},"token":"iap_plain"}`)
	}))
	p, buf := remotePrinter(cli.OutputHuman)
	if err := cli.RemoteTokensCreate(context.Background(), c, p, "ci", "team-1", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(buf.String(), "iap_plain") {
		t.Errorf("plaintext missing:\n%s", buf.String())
	}
}

// --- teams switch ---

func TestRemoteTeamsSwitch_MintsPersistsRevokes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ITERION_REMOTE_URL", "")

	oldToken := "iap_old"
	oldFP := secrets.FingerprintSHA256(oldToken)
	var revoked string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/me/tokens", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["team_id"] != "team-b" {
			t.Errorf("mint team_id = %v", req["team_id"])
		}
		w.WriteHeader(http.StatusCreated)
		// org_id: the server states the team's org; the CLI follows it.
		fmt.Fprint(w, `{"pat":{"id":"p-new"},"token":"iap_new","org_id":"o1"}`)
	})
	mux.HandleFunc("GET /api/me/tokens", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tokens":[{"id":"p-old","name":"cli","fingerprint":%q},{"id":"p-new","name":"cli","fingerprint":"other"}]}`, oldFP)
	})
	mux.HandleFunc("DELETE /api/me/tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		revoked = r.PathValue("id")
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	if err := cli.SaveRemoteConfig(cli.RemoteConfig{BaseURL: ts.URL, Token: oldToken}); err != nil {
		t.Fatal(err)
	}
	c := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: ts.URL, Token: oldToken})
	p, _ := remotePrinter(cli.OutputHuman)
	if err := cli.RemoteTeamsSwitch(context.Background(), c, p, "team-b", "cli"); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if revoked != "p-old" {
		t.Errorf("revoked = %q, want p-old (fingerprint match)", revoked)
	}
	cfg, err := cli.LoadRemoteConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "iap_new" || cfg.TeamID != "team-b" {
		t.Errorf("persisted cfg = %+v", cfg)
	}
	if cfg.OrgID != "o1" {
		t.Errorf("org scope did not follow the team into its org: OrgID = %q, want o1", cfg.OrgID)
	}
}

// The server's canViewTeam is the membership authority: a 403 from the
// mint endpoint is the rejection, propagated with the teams-list hint. No
// client-side pre-check — a stale duplicate of the server's rule is what
// once made this command refuse every team.
func TestRemoteTeamsSwitch_RejectsNonMember(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_REMOTE_URL", "")
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":"not a member of team \"team-x\""}`)
	}))
	p, _ := remotePrinter(cli.OutputHuman)
	if err := cli.RemoteTeamsSwitch(context.Background(), c, p, "  ", "cli"); err == nil ||
		!strings.Contains(err.Error(), "team id required") {
		t.Fatalf("blank team id must be rejected before any mint, got %v", err)
	}
	err := cli.RemoteTeamsSwitch(context.Background(), c, p, "team-x", "cli")
	if err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("want the server's membership rejection, got %v", err)
	}
	if cfg, cfgErr := cli.LoadRemoteConfig(); cfgErr == nil && cfg.TeamID == "team-x" {
		t.Fatal("a rejected switch must not persist the target team")
	}
}

// --- helpers ---

func TestReadDataArg(t *testing.T) {
	dir := t.TempDir()
	remoteWriteFile(t, dir+"/d.json", `{"a":1}`)
	b, err := cli.ReadDataArg("@" + dir + "/d.json")
	if err != nil || string(b) != `{"a":1}` {
		t.Fatalf("file: %q %v", b, err)
	}
	b, err = cli.ReadDataArg(`{"x":2}`)
	if err != nil || string(b) != `{"x":2}` {
		t.Fatalf("literal: %q %v", b, err)
	}
	b, err = cli.ReadDataArg("")
	if err != nil || b != nil {
		t.Fatalf("empty: %q %v", b, err)
	}
}

func TestParseAttachFlags(t *testing.T) {
	m, err := cli.ParseAttachFlags([]string{"spec=./a.md", "logo=./b.png"})
	if err != nil {
		t.Fatal(err)
	}
	if m["spec"] != "./a.md" || m["logo"] != "./b.png" {
		t.Errorf("m = %v", m)
	}
	if _, err := cli.ParseAttachFlags([]string{"noequals"}); err == nil {
		t.Error("want error on malformed pair")
	}
}

// --- pool policy (operator) ---

// The operator half of the pool had no command: standing one up meant a
// raw `remote api PUT` with a hand-written audience. RemotePoolPolicy
// sends ONLY what the caller set — a partial update must not restate (or
// silently drop) the rest of the policy.
func TestRemotePoolPolicy_SendsOnlyWhatWasSet(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"org1","name":"devthejo","enabled":true}`))
	}))
	p, _ := remotePrinter(cli.OutputJSON)

	enabled := false
	if err := cli.RemotePoolPolicy(context.Background(), c, p, "team-1", cli.PoolPolicy{Enabled: &enabled}); err != nil {
		t.Fatalf("RemotePoolPolicy: %v", err)
	}
	if gotMethod != "PUT" || gotPath != "/api/teams/team-1/pool" {
		t.Fatalf("request = %s %s, want PUT /api/teams/team-1/pool", gotMethod, gotPath)
	}
	if len(gotBody) != 1 {
		t.Fatalf("body = %v, want only the enabled field", gotBody)
	}
	if gotBody["enabled"] != false {
		t.Fatalf("enabled = %v, want false", gotBody["enabled"])
	}
	if _, ok := gotBody["audience"]; ok {
		t.Fatal("a pause must not restate the audience — that is how a forgotten --all-teams survives")
	}
}

// An audience travels WHOLE: it is a set, so every dial the caller chose
// is present (and the ones they did not are absent, not carried over).
func TestRemotePoolPolicy_AudienceTravelsWhole(t *testing.T) {
	var gotBody map[string]any
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"org1"}`))
	}))
	p, _ := remotePrinter(cli.OutputJSON)

	name := "devthejo"
	pol := cli.PoolPolicy{Name: &name}
	pol.Audience = &struct {
		Teams        []string `json:"teams,omitempty"`
		Orgs         []string `json:"orgs,omitempty"`
		Contributors bool     `json:"contributors,omitempty"`
		AllTeams     bool     `json:"all_teams,omitempty"`
	}{AllTeams: true}
	if err := cli.RemotePoolPolicy(context.Background(), c, p, "team-1", pol); err != nil {
		t.Fatalf("RemotePoolPolicy: %v", err)
	}
	aud, ok := gotBody["audience"].(map[string]any)
	if !ok {
		t.Fatalf("audience missing from %v", gotBody)
	}
	if aud["all_teams"] != true {
		t.Fatalf("all_teams = %v, want true", aud["all_teams"])
	}
	if gotBody["name"] != "devthejo" {
		t.Fatalf("name = %v, want devthejo", gotBody["name"])
	}
	if _, ok := gotBody["enabled"]; ok {
		t.Fatal("enabled was not set by the caller and must not be sent")
	}
}
