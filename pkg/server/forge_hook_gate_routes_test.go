package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// refusingGitLab is the GitLab of the case this feature exists for: an
// instance whose outbound allowlist does not list the deployment's host, so
// it answers the hook write 422 "Invalid url given". The hook create and the
// hook update are armed separately — the refusal reaches the operator from
// either call.
type refusingGitLab struct {
	refuseCreate bool
	refuseUpdate bool
	hookID       int
}

func (m *refusingGitLab) server() *httptest.Server {
	const invalidURL = `{"message":"Invalid url given"}`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "username": "botuser"})
	})
	mux.HandleFunc("/api/v4/projects/group%2Fapi/hooks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{}) // the GetHook probe: none yet
		case http.MethodPost:
			if m.refuseCreate {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(invalidURL))
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.hookID++
			body["id"] = m.hookID
			_ = json.NewEncoder(w).Encode(body)
		}
	})
	mux.HandleFunc("/api/v4/projects/group%2Fapi/hooks/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && m.refuseUpdate {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(invalidURL))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": m.hookID, "url": "https://unchanged.example/hook"})
	})
	return httptest.NewServer(mux)
}

// seedForgeConn registers a PAT connection on t1 pointed at the mock forge,
// with the given hook-base pin ("" = follow the deployment's public URL).
func seedForgeConn(t *testing.T, s *Server, connID, baseURL, webhookBase string) {
	t.Helper()
	sealed, err := forge.SealPAT(s.sealer, connID, "glpat-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: connID, TenantID: "t1", Provider: forge.ProviderGitLab, Kind: forge.KindPAT,
		ForgeBaseURL: baseURL, AccountLogin: "botuser", Status: forge.StatusActive,
		WebhookBaseURL: webhookBase, SealedPayload: sealed,
	}); err != nil {
		t.Fatal(err)
	}
}

// gateBotLookup resolves the bots these tests enable: one posts a commit
// status (so it gates merges), one does not, and one cannot be resolved at
// all — the three cases the posture surfaces must tell apart.
func gateBotLookup(_ context.Context, _, botID string) (*bundle.ForgeRequirements, error) {
	switch botID {
	case "gate-bot":
		return &bundle.ForgeRequirements{
			Events:      []string{bundle.ForgeEventPullRequest},
			TokenScopes: map[string]string{"pull_requests": "write", bundle.ForgeScopeStatuses: "write"},
			Secret:      "forge_token",
		}, nil
	case "plain-bot":
		return &bundle.ForgeRequirements{
			Events:      []string{bundle.ForgeEventPullRequestComment},
			TokenScopes: map[string]string{"pull_requests": "write"},
			Secret:      "forge_token",
		}, nil
	default:
		return nil, fmt.Errorf("bot %q not found", botID)
	}
}

func enableRepoBots(t *testing.T, s *Server, connID string, bots ...string) *errorRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"connection_id": connID, "repo": "group/api", "bot_ids": bots})
	if err != nil {
		t.Fatal(err)
	}
	w := newErrorRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(superAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", string(body), "t1"))
	return w
}

// decodeErrorBody reads the {error, detail} shape the studio switches on.
func decodeErrorBody(t *testing.T, w *errorRecorder) (string, string) {
	t.Helper()
	var body struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the typed error shape: %v (%s)", err, w.Body.String())
	}
	return body.Error, body.Detail
}

// TestForgeEnable_HookCreateRefused_UnpinnedNamesThePinRemedy: the forge
// refuses the ADDRESS, and the generic mapping would answer "provisioning
// failed: … HTTP 422" — a status whose remedy (pin a base this forge accepts)
// appears nowhere. The typed answer is what the studio turns into that step.
func TestForgeEnable_HookCreateRefused_UnpinnedNamesThePinRemedy(t *testing.T) {
	gl := &refusingGitLab{refuseCreate: true}
	srv := gl.server()
	defer srv.Close()

	s := newForgeTestServer(t)
	s.forgeOrchestrator.Bots = gateBotLookup
	seedForgeConn(t, s, "c1", srv.URL, "")

	w := enableRepoBots(t, s, "c1", "plain-bot")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code=%d body=%s, want 422", w.Code, w.Body.String())
	}
	code, detail := decodeErrorBody(t, w)
	if code != "hook_url_refused" {
		t.Fatalf("error=%q, want hook_url_refused (the studio switches on it): %s", code, w.Body.String())
	}
	for _, want := range []string{
		"gitlab refused to register the webhook at https://iterion.example.com/api/webhooks/gitlab/",
		// The forge's own reason, read by the real GitLab client off the 422
		// body — what tells this refusal from a hook-limit or field one.
		`(HTTP 422: "Invalid url given")`,
		"The usual cause is a forge that only delivers to allowlisted hosts",
		"pin a webhook base URL it accepts on this connection",
		`Integrations → Repositories → the connection's "Webhook delivery" row`,
		"iterion remote forge connections webhook-base c1 --url <base>",
		"then retry the enable (on an org that requires approval, approve the pending request again).",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail is missing %q:\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "This connection pins") {
		t.Errorf("the unpinned remedy must not talk about a pin that does not exist:\n%s", detail)
	}
}

// TestForgeEnable_HookCreateRefused_PinnedChecksTheHost: once a base IS
// pinned, "pin a base" is no longer the fix — the pinned host itself is what
// the forge does not accept.
func TestForgeEnable_HookCreateRefused_PinnedChecksTheHost(t *testing.T) {
	gl := &refusingGitLab{refuseCreate: true}
	srv := gl.server()
	defer srv.Close()

	s := newForgeTestServer(t)
	s.forgeOrchestrator.Bots = gateBotLookup
	seedForgeConn(t, s, "c1", srv.URL, "https://iterion.pinned.example/")

	w := enableRepoBots(t, s, "c1", "plain-bot")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code=%d body=%s, want 422", w.Code, w.Body.String())
	}
	code, detail := decodeErrorBody(t, w)
	if code != "hook_url_refused" {
		t.Fatalf("error=%q, want hook_url_refused: %s", code, w.Body.String())
	}
	for _, want := range []string{
		"gitlab refused to register the webhook at https://iterion.pinned.example/api/webhooks/gitlab/",
		"This connection pins https://iterion.pinned.example: check that the forge accepts that host",
		"GitLab: Admin area → Settings → Network → Outbound requests",
		"or change the pin.",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail is missing %q:\n%s", want, detail)
		}
	}
}

// TestForgeEnable_RefusedReprovisionKeepsTheLiveWebhookSecret: the second
// enable on an already-integrated repo mints a new iwh_ and replaces the
// stored config BEFORE the forge accepts anything. When the forge refuses the
// hook update, the hook it still delivers to carries the previous secret —
// so the stored config must be the previous one too, or the repo answers 401
// to every delivery of a webhook nobody touched.
func TestForgeEnable_RefusedReprovisionKeepsTheLiveWebhookSecret(t *testing.T) {
	gl := &refusingGitLab{}
	srv := gl.server()
	defer srv.Close()

	s := newForgeTestServer(t)
	s.forgeOrchestrator.Bots = gateBotLookup
	seedForgeConn(t, s, "c1", srv.URL, "")
	bg := context.Background()

	w := enableRepoBots(t, s, "c1", "plain-bot")
	if w.Code != http.StatusOK {
		t.Fatalf("first enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	before, err := s.webhookConfigs.Get(bg, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}

	gl.refuseUpdate = true
	w = enableRepoBots(t, s, "c1", "gate-bot")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("refused re-provision: code=%d body=%s, want 422", w.Code, w.Body.String())
	}
	if code, _ := decodeErrorBody(t, w); code != "hook_url_refused" {
		t.Fatalf("error=%q, want hook_url_refused: %s", code, w.Body.String())
	}
	after, err := s.webhookConfigs.Get(bg, res.WebhookID)
	if err != nil {
		t.Fatalf("the refused re-provision left no webhook config: %v", err)
	}
	if after.TokenHash != before.TokenHash {
		t.Errorf("the stored webhook token hash changed on a refused re-provision — the live hook still signs with the previous secret, so every delivery would now be rejected")
	}
	if !sameStrings(after.BotIDs, before.BotIDs) {
		t.Errorf("stored bot set not restored: %v → %v", before.BotIDs, after.BotIDs)
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestForgePreview_GatesMergesAndHookBase: the enable dialog announces the
// merge-gate posture and the host the hook will be registered under, both
// read from the server's own derivations — a studio-side guess would promise
// what the provision does not do.
func TestForgePreview_GatesMergesAndHookBase(t *testing.T) {
	for _, tc := range []struct {
		name        string
		webhookBase string
		bots        string
		wantGates   bool
		wantBase    string
	}{
		{"unpinned, gating bot", "", "gate-bot", true, "https://iterion.example.com"},
		{"unpinned, no gating bot", "", "plain-bot", false, "https://iterion.example.com"},
		{"pinned base, trailing slash trimmed", "https://iterion.pinned.example/", "gate-bot,plain-bot", true, "https://iterion.pinned.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newForgeTestServer(t)
			s.forgeOrchestrator.Bots = gateBotLookup
			seedForgeConn(t, s, "c1", "https://gitlab.example.com", tc.webhookBase)

			w := newErrorRecorder()
			s.handlePreviewForgeEnable(w, forgeReq(superAdminCtx(), "GET",
				"/api/teams/t1/forge/repo-bots/preview?connection_id=c1&repo=group/api&bots="+tc.bots, "", "t1"))
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			var p forgeEnablePreview
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if len(p.Conflicts) != 0 {
				t.Fatalf("unexpected conflicts: %v", p.Conflicts)
			}
			if p.GatesMerges != tc.wantGates {
				t.Errorf("gates_merges = %v, want %v for %s", p.GatesMerges, tc.wantGates, tc.bots)
			}
			if p.HookBaseURL != tc.wantBase {
				t.Errorf("hook_base_url = %q, want %q — the operator reads the host the forge will be given", p.HookBaseURL, tc.wantBase)
			}
		})
	}
}

// TestListTeamForgeRepos_GatesMerges: each row carries the posture of its own
// bots, and a row binding a bot that does not resolve carries NO posture —
// "nothing gates here" is a different fact from "we could not read a bot",
// and the listing must not serve the second as the first. The unresolvable
// bot is warned once per request, not once per row.
func TestListTeamForgeRepos_GatesMerges(t *testing.T) {
	s := newForgeTestServer(t)
	s.forgeOrchestrator.Bots = gateBotLookup
	logs := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelWarn, logs)
	bg := context.Background()

	if err := s.forgeConnections.Create(bg, forge.Connection{
		ID: "c1", TenantID: "t1", Provider: forge.ProviderGitLab, Kind: forge.KindPAT, Status: forge.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	mkInt := func(id, repo string, bots []string) {
		t.Helper()
		if err := s.forgeIntegrations.Create(bg, forge.RepoIntegration{
			ID: id, TenantID: "t1", ConnectionID: "c1", Provider: forge.ProviderGitLab,
			RepoFullName: repo, BotIDs: bots,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mkInt("i1", "group/gated", []string{"plain-bot", "gate-bot"})
	mkInt("i2", "group/advisory", []string{"plain-bot"})
	mkInt("i3", "group/ghost-a", []string{"plain-bot", "ghost-bot"})
	mkInt("i4", "group/ghost-b", []string{"ghost-bot"})
	// A gating bot settles the OR whatever its sibling resolves to.
	mkInt("i5", "group/gated-ghost", []string{"gate-bot", "ghost-bot"})

	w := newErrorRecorder()
	s.handleListTeamForgeRepos(w, forgeReq(superAdminCtx(), "GET", "/api/teams/t1/forge/repos", "", "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	// Read the raw objects: an OMITTED field and a false one are different
	// answers, and a decode into the Go struct cannot tell them apart.
	var raw struct {
		Repos []map[string]any `json:"repos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	rows := map[string]map[string]any{}
	for _, row := range raw.Repos {
		name, _ := row["repo_full_name"].(string)
		rows[name] = row
	}
	if len(rows) != 5 {
		t.Fatalf("want 5 rows, got %d: %+v", len(rows), raw.Repos)
	}
	if got, ok := rows["group/gated-ghost"]["gates_merges"]; !ok || got != true {
		t.Errorf("group/gated-ghost gates_merges = %v (present=%v), want true — a bound bot gates, so the unreadable sibling cannot make the posture unknown", got, ok)
	}
	if got, ok := rows["group/gated"]["gates_merges"]; !ok || got != true {
		t.Errorf("group/gated gates_merges = %v (present=%v), want true — a bound bot posts a commit status", got, ok)
	}
	if got, ok := rows["group/advisory"]["gates_merges"]; !ok || got != false {
		t.Errorf("group/advisory gates_merges = %v (present=%v), want false", got, ok)
	}
	for _, repo := range []string{"group/ghost-a", "group/ghost-b"} {
		if got, ok := rows[repo]["gates_merges"]; ok {
			t.Errorf("%s reports gates_merges = %v although one of its bots does not resolve — the row must omit the field", repo, got)
		}
	}
	warned := 0
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, `bot "ghost-bot" does not resolve`) {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("the unresolvable bot was warned %d time(s), want exactly 1 per request (two rows bind it):\n%s", warned, logs.String())
	}
}

// TestWriteForgeProvisionError_RestoreFailureOutranksTheHookFailure: a
// re-provision whose restore failed leaves the repo with a stored config the
// live hook's secret no longer matches. Whatever hook failure it is joined
// with — a refused address or a forge outage — the answer names the restore
// failure and its remedy, never the hook's story alone.
//
// A refused ADDRESS joined with it keeps its own story too: "re-run the same
// request" is a request that cannot succeed while the forge refuses the URL,
// so the answer carries the machine code and the one gesture that changes the
// outcome, after the restore failure and not instead of it.
func TestWriteForgeProvisionError_RestoreFailureOutranksTheHookFailure(t *testing.T) {
	s := newForgeTestServer(t)
	restore := &forge.ConfigRestoreError{WebhookID: "wh-1", Err: errors.New("webhook store unavailable")}
	for _, tc := range []struct {
		name string
		hook error
		// wantCode is the machine code the body must carry, empty when the
		// answer is the plain message shape.
		wantCode string
		wantText []string
	}{{
		name: "refused address",
		hook: &forge.HookRefusedError{
			Provider: forge.ProviderGitLab, ConnectionID: "c1",
			URL:    "https://iterion.example.com/api/webhooks/gitlab/wh-1",
			Detail: "Invalid url given",
			Err:    forge.StatusErr("gitlab", "update hook", http.StatusUnprocessableEntity),
		},
		wantCode: "hook_url_refused",
		wantText: []string{
			"gitlab refused to register the webhook at https://iterion.example.com/api/webhooks/gitlab/wh-1",
			"Invalid url given",
			"pin a webhook base URL",
			"iterion remote forge connections webhook-base c1 --url <base>",
		},
	}, {
		name: "forge outage",
		hook: forge.StatusErr("gitlab", "update hook", http.StatusBadGateway),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newErrorRecorder()
			s.writeForgeProvisionError(w, errors.Join(tc.hook, restore))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("code=%d body=%s, want 500: the repo has stopped receiving deliveries", w.Code, w.Body.String())
			}
			// Read the decoded fields: the shapes differ (a coded refusal
			// carries `detail`, a plain one puts the message in `error`), and
			// the raw JSON escapes the gesture's `<base>` placeholder.
			var cause struct {
				Error string `json:"error"`
				Code  string `json:"error_code"`
			}
			if err := json.Unmarshal([]byte(w.Diagnostic(t)), &cause); err != nil {
				t.Fatal(err)
			}
			code, body := cause.Code, cause.Error
			var public map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &public)
			if public["error_code"] != tc.wantCode {
				t.Fatalf("lost stable code: %s", w.Body.String())
			}
			for _, want := range append([]string{"restore webhook config wh-1", "webhook store unavailable", "re-run the same request"}, tc.wantText...) {
				if !strings.Contains(body, want) {
					t.Errorf("the answer is missing %q: %s", want, body)
				}
			}
			if tc.wantCode != "" && code != tc.wantCode {
				t.Errorf("error = %q, want %q — the code is what the studio turns into the remedy step: %s",
					code, tc.wantCode, body)
			}
			if tc.wantCode != "" {
				// The restore failure leads: it is the fact the operator has
				// to act on first, and a remedy read before it reads as the
				// whole story.
				restoreAt := strings.Index(body, "restore webhook config wh-1")
				if remedyAt := strings.Index(body, "pin a webhook base URL"); restoreAt < 0 || remedyAt < restoreAt {
					t.Errorf("the restore failure does not lead the answer (restore at %d, the refusal's remedy at %d): %s",
						restoreAt, remedyAt, body)
				}
			}
		})
	}
}

// TestForgeEnable_HookRefusedWithoutPublicURLNamesTheDeploymentGap: with no
// public URL the hook address iterion offers is relative, and a forge refuses
// it whatever its allowlist says. The remedy is the deployment setting (or a
// pin) — sending the operator to the forge's allowlist would be the wrong door.
func TestForgeEnable_HookRefusedWithoutPublicURLNamesTheDeploymentGap(t *testing.T) {
	gl := &refusingGitLab{refuseCreate: true}
	srv := gl.server()
	defer srv.Close()

	s := newForgeTestServer(t)
	s.forgeOrchestrator.Bots = gateBotLookup
	s.forgeOrchestrator.PublicURL = ""
	seedForgeConn(t, s, "c1", srv.URL, "")

	w := enableRepoBots(t, s, "c1", "plain-bot")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code=%d body=%s, want 422", w.Code, w.Body.String())
	}
	code, detail := decodeErrorBody(t, w)
	if code != "hook_url_refused" {
		t.Fatalf("error=%q, want hook_url_refused: %s", code, w.Body.String())
	}
	for _, want := range []string{
		"This deployment has no public URL configured (auth.publicUrl / ITERION_PUBLIC_URL)",
		"set it, or pin a base URL on this connection",
		"iterion remote forge connections webhook-base c1 --url <base>",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail is missing %q:\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "allowlisted hosts") {
		t.Errorf("a relative address is not an allowlist problem:\n%s", detail)
	}

	// The preview says the same thing before the write: no base at all.
	pw := newErrorRecorder()
	s.handlePreviewForgeEnable(pw, forgeReq(superAdminCtx(), "GET",
		"/api/teams/t1/forge/repo-bots/preview?connection_id=c1&repo=group/api&bots=plain-bot", "", "t1"))
	var pv struct {
		HookBaseURL *string `json:"hook_base_url"`
	}
	if err := json.Unmarshal(pw.Body.Bytes(), &pv); err != nil || pw.Code != http.StatusOK {
		t.Fatalf("preview code=%d err=%v body=%s", pw.Code, err, pw.Body.String())
	}
	if pv.HookBaseURL == nil || *pv.HookBaseURL != "" {
		t.Errorf("preview hook_base_url = %v, want present and empty — the studio warns on it", pv.HookBaseURL)
	}
}

// TestListTeamForgeRepos_GatesMergesMemoSpansOneWindow: the repo listing is
// polled every 30 s by every open studio tab, and resolving a bot is a store
// round-trip. One resolution per (team, bot) per window, across requests; a
// window later the bot is read again, so a changed declaration is picked up.
func TestListTeamForgeRepos_GatesMergesMemoSpansOneWindow(t *testing.T) {
	s := newForgeTestServer(t)
	var mu sync.Mutex
	resolved := map[string]int{}
	s.forgeOrchestrator.Bots = func(ctx context.Context, team, bot string) (*bundle.ForgeRequirements, error) {
		mu.Lock()
		resolved[bot]++
		mu.Unlock()
		return gateBotLookup(ctx, team, bot)
	}
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s.forgeGatesMemo.clock = func() time.Time { return now }
	bg := context.Background()
	if err := s.forgeConnections.Create(bg, forge.Connection{
		ID: "c1", TenantID: "t1", Provider: forge.ProviderGitLab, Kind: forge.KindPAT, Status: forge.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	for i, bots := range [][]string{{"gate-bot"}, {"plain-bot", "gate-bot"}} {
		if err := s.forgeIntegrations.Create(bg, forge.RepoIntegration{
			ID: fmt.Sprintf("i%d", i), TenantID: "t1", ConnectionID: "c1", Provider: forge.ProviderGitLab,
			RepoFullName: fmt.Sprintf("group/r%d", i), BotIDs: bots,
		}); err != nil {
			t.Fatal(err)
		}
	}
	list := func() {
		t.Helper()
		w := newErrorRecorder()
		s.handleListTeamForgeRepos(w, forgeReq(superAdminCtx(), "GET", "/api/teams/t1/forge/repos", "", "t1"))
		if w.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
	}
	count := func(bot string) int {
		mu.Lock()
		defer mu.Unlock()
		return resolved[bot]
	}

	list()
	list()
	if got := count("gate-bot"); got != 1 {
		t.Errorf("gate-bot resolved %d times over two polls in one window, want 1", got)
	}
	now = now.Add(gatesMergesTTL + time.Second)
	list()
	if got := count("gate-bot"); got != 2 {
		t.Errorf("gate-bot resolved %d times after the window elapsed, want 2 — an expired answer must be read again", got)
	}
}

// TestHookRefusedDetail_NonDialableAddress: the refusal's remedy branches on
// what the address IS, not on whether it happens to contain "://". A relative
// address means no public URL at all; an address with a host but no scheme
// means the public URL is set and unusable — one gesture apart, and the wrong
// sentence sends an operator to set a value they already set.
func TestHookRefusedDetail_NonDialableAddress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		url      string
		want     []string
		unwanted []string
	}{{
		name: "relative — no public URL at all",
		url:  "/api/webhooks/gitlab/wh-1",
		want: []string{
			"This deployment has no public URL configured (auth.publicUrl / ITERION_PUBLIC_URL)",
			"so the webhook address is relative: set it, or pin a base URL on this connection",
			"iterion remote forge connections webhook-base c1 --url <base>",
			hookRefusedRetry,
		},
		unwanted: []string{"allowlisted hosts", "is not an absolute http(s) URL"},
	}, {
		name: "a host without a scheme — the public URL is set and unusable",
		url:  "iterion.fabrique.social.gouv.fr/api/webhooks/gitlab/wh-1",
		want: []string{
			"The webhook address iterion.fabrique.social.gouv.fr/api/webhooks/gitlab/wh-1 is not an absolute http(s) URL",
			"must be scheme+host, so set it to https://…",
			"or pin a base URL on this connection",
			hookRefusedRetry,
		},
		unwanted: []string{"has no public URL configured", "allowlisted hosts"},
	}, {
		name: "a dialable address — the forge's allowlist is the cause to hedge at",
		url:  "https://iterion.example.com/api/webhooks/gitlab/wh-1",
		want: []string{"The usual cause is a forge that only delivers to allowlisted hosts"},
		unwanted: []string{
			"has no public URL configured",
			"is not an absolute http(s) URL",
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			detail := hookRefusedDetail(&forge.HookRefusedError{
				Provider: forge.ProviderGitLab, ConnectionID: "c1", URL: tc.url,
				Err: forge.StatusErr("gitlab", "create hook", http.StatusUnprocessableEntity),
			})
			for _, want := range tc.want {
				if !strings.Contains(detail, want) {
					t.Errorf("detail is missing %q:\n%s", want, detail)
				}
			}
			for _, bad := range tc.unwanted {
				if strings.Contains(detail, bad) {
					t.Errorf("detail must not say %q:\n%s", bad, detail)
				}
			}
		})
	}
}

// TestForgeEnable_SchemeLessPublicURLNamesTheScheme drives the same branch
// from the real enable: config.Validate only checks the scheme and host of
// auth.publicUrl when canonical redirects are on (off by default), so a
// deployment started with ITERION_PUBLIC_URL=host-only hands that host to the
// forge verbatim — and the forge refuses it.
func TestForgeEnable_SchemeLessPublicURLNamesTheScheme(t *testing.T) {
	gl := &refusingGitLab{refuseCreate: true}
	srv := gl.server()
	defer srv.Close()

	s := newForgeTestServer(t)
	s.forgeOrchestrator.Bots = gateBotLookup
	s.forgeOrchestrator.PublicURL = "iterion.fabrique.social.gouv.fr"
	seedForgeConn(t, s, "c1", srv.URL, "")

	w := enableRepoBots(t, s, "c1", "plain-bot")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code=%d body=%s, want 422", w.Code, w.Body.String())
	}
	code, detail := decodeErrorBody(t, w)
	if code != "hook_url_refused" {
		t.Fatalf("error=%q, want hook_url_refused: %s", code, w.Body.String())
	}
	for _, want := range []string{
		"The webhook address iterion.fabrique.social.gouv.fr/api/webhooks/gitlab/",
		"is not an absolute http(s) URL",
		"must be scheme+host, so set it to https://…",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail is missing %q:\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "has no public URL configured") {
		t.Errorf("the deployment HAS a public URL (%q); the detail must not tell the operator to set it:\n%s",
			s.forgeOrchestrator.PublicURL, detail)
	}
}

// echoingGitLab answers the hook create 422 with a body built from the
// request it was sent — the shape a WAF, a validating gateway or a debug
// proxy in front of a self-hosted forge answers with — and records the
// webhook secret it was handed.
type echoingGitLab struct {
	mu   sync.Mutex
	body string
	seen string
}

func (m *echoingGitLab) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "username": "botuser"})
	})
	mux.HandleFunc("/api/v4/projects/group%2Fapi/hooks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		var sent map[string]any
		_ = json.NewDecoder(r.Body).Decode(&sent)
		tok, _ := sent["token"].(string)
		m.mu.Lock()
		m.seen = tok
		body := m.body
		m.mu.Unlock()
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(strings.ReplaceAll(body, "$TOKEN", tok)))
	})
	return httptest.NewServer(mux)
}

// TestForgeEnable_RefusalEchoingTheSecretLeaksItNowhere: the hook write POSTs
// the freshly minted iwh_ token to an operator-supplied forge, behind
// whatever proxy that operator runs. Whatever that endpoint answers on a
// non-2xx reaches two places iterion controls — the 422 detail the studio
// renders, and the Warn line that ships to the deployment's log sink — so a
// forge that quotes the request it refused must not publish the secret there.
func TestForgeEnable_RefusalEchoingTheSecretLeaksItNowhere(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"the WAF echoes the request", `{"message":"blocked request body: {\"url\":\"https://iterion.example.com/api/webhooks/gitlab/x\",\"token\":\"$TOKEN\"}"}`},
		{"a field error names the token", `{"message":{"token":["$TOKEN is invalid"],"url":["is blocked: Requests to the local network are not allowed"]}}`},
		{"a proxy page quotes the body", `<html><body><h1>422</h1><p>rule outbound_requests_allowlist rejected body url=... token=$TOKEN</p></body></html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gl := &echoingGitLab{body: tc.body}
			srv := gl.server()
			defer srv.Close()

			s := newForgeTestServer(t)
			s.forgeOrchestrator.Bots = gateBotLookup
			logs := &lockedBuffer{}
			s.logger = iterlog.New(iterlog.LevelWarn, logs)
			seedForgeConn(t, s, "c1", srv.URL, "")

			w := enableRepoBots(t, s, "c1", "plain-bot")
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("code=%d body=%s, want 422", w.Code, w.Body.String())
			}
			_, detail := decodeErrorBody(t, w)
			gl.mu.Lock()
			tok := gl.seen
			gl.mu.Unlock()
			if !strings.HasPrefix(tok, "iwh_") {
				t.Fatalf("premise: the forge was handed %q, not a webhook secret", tok)
			}
			for where, text := range map[string]string{"the 422 detail": detail, "the Warn line": logs.String()} {
				if strings.Contains(text, tok) {
					t.Errorf("%s carries the webhook secret the forge was handed:\n%s", where, text)
				}
			}
			// The forge's own words are the operator's only clue about which
			// setting to fix, so the scrub must not take the message with it.
			if !strings.Contains(detail, "iwh_…") {
				t.Errorf("the detail carries no marker of what was removed:\n%s", detail)
			}
			if !strings.Contains(logs.String(), "the webhook address was refused") {
				t.Errorf("the refusal was not logged at all:\n%s", logs.String())
			}
		})
	}
}

// TestListTeamForgeRepos_MemoStoresOnlyFactsAboutTheBot: the TTL memo spares
// a store round-trip per (team, bot) per window, but a failure is not a fact
// about the bot — a cancelled request context or a store blip would otherwise
// be served to the next request, healthy, for the rest of the window, and the
// repo page hides the Merge gate card for a bot that resolves.
func TestListTeamForgeRepos_MemoStoresOnlyFactsAboutTheBot(t *testing.T) {
	s := newForgeTestServer(t)
	logs := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelWarn, logs)
	var mu sync.Mutex
	calls := 0
	s.forgeOrchestrator.Bots = func(ctx context.Context, team, bot string) (*bundle.ForgeRequirements, error) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			// What the bot-source read answers once the poll's client is
			// gone: the request context is dead and nothing resolves.
			return nil, fmt.Errorf("bot %q: %w", bot, context.Canceled)
		}
		return gateBotLookup(ctx, team, bot)
	}
	// The clock stands still, so any caching at all is in its window.
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s.forgeGatesMemo.clock = func() time.Time { return now }
	bg := context.Background()
	if err := s.forgeConnections.Create(bg, forge.Connection{
		ID: "c1", TenantID: "t1", Provider: forge.ProviderGitLab, Kind: forge.KindPAT, Status: forge.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.forgeIntegrations.Create(bg, forge.RepoIntegration{
		ID: "i1", TenantID: "t1", ConnectionID: "c1", Provider: forge.ProviderGitLab,
		RepoFullName: "group/gated", BotIDs: []string{"gate-bot"},
	}); err != nil {
		t.Fatal(err)
	}
	row := func() (any, bool) {
		t.Helper()
		w := newErrorRecorder()
		s.handleListTeamForgeRepos(w, forgeReq(superAdminCtx(), "GET", "/api/teams/t1/forge/repos", "", "t1"))
		if w.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		var raw struct {
			Repos []map[string]any `json:"repos"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if len(raw.Repos) != 1 {
			t.Fatalf("want 1 row, got %d", len(raw.Repos))
		}
		got, ok := raw.Repos[0]["gates_merges"]
		return got, ok
	}

	if got, ok := row(); ok {
		t.Errorf("the aborted poll reports gates_merges = %v although the bot did not resolve", got)
	}
	if got, ok := row(); !ok || got != true {
		t.Errorf("gates_merges = %v (present=%v) on a healthy poll, want true — the failure of the previous "+
			"request was memoised, so the repo page hides the Merge gate card for a bot that gates", got, ok)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 2 {
		t.Errorf("the bot was resolved %d time(s) over the two polls, want 2 (the failure must not be cached)", n)
	}
	if warned := strings.Count(logs.String(), `bot "gate-bot" does not resolve`); warned != 1 {
		t.Errorf("the unresolvable bot was warned %d time(s), want exactly 1 (one failing poll):\n%s", warned, logs.String())
	}
	// A resolution IS cached: the window is what the polling cadence pays for.
	if _, _ = row(); true {
		mu.Lock()
		n = calls
		mu.Unlock()
		if n != 2 {
			t.Errorf("a third poll in the same window resolved the bot again (%d calls): a successful resolution is memoised", n)
		}
	}
}
