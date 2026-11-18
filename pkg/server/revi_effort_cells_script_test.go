package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// The operator script that applies the Revi gateway effort cells
// (scripts/ops/revi-effort-cells.py) and the block it applies, from this
// package.
const (
	reviEffortScript  = "../../scripts/ops/revi-effort-cells.py"
	reviEffortFixture = "cloudpublisher/testdata/revi_gateway_routing.json"
)

// reviEffortForwarder stands in for `iterion remote api METHOD PATH [--data
// @file]`: it forwards to $ITERION_FORWARD_URL — this package's real
// handlers — and answers as cli.RemoteAPIPrint does: the body on stdout, exit
// 1 on a non-2xx status.
const reviEffortForwarder = `#!/usr/bin/env python3
import json, os, sys, urllib.request, urllib.error
args = sys.argv[1:]
assert args[:2] == ["remote", "api"], args
method, path = args[2].upper(), args[3]
data = None
if "--data" in args:
    data = open(args[args.index("--data") + 1][1:], "rb").read()
req = urllib.request.Request(os.environ["ITERION_FORWARD_URL"] + path, data=data, method=method)
if data is not None:
    req.add_header("Content-Type", "application/json")
try:
    resp = urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req)
    code, body = resp.status, resp.read()
except urllib.error.HTTPError as e:
    code, body = e.code, e.read()
if body:
    sys.stdout.write(body.decode())
if code // 100 != 2:
    print("Error: %s %s: HTTP %d" % (method, path, code), file=sys.stderr)
    sys.exit(1)
`

// reviEffortInstance is a server whose real admin listings and repo-bots
// handlers the script reaches, with a seam on each request: the test can
// count the script's calls, answer a PATCH in the handler's stead, rewrite a
// PATCH body before the handler stores it, or run a write of its own just
// before a GET is served.
type reviEffortInstance struct {
	t   *testing.T
	s   *Server
	bin string

	mu      sync.Mutex
	patches int
	gets    map[string]int                                   // team id -> repo-bots listings served
	beforeG map[string]func(n int)                           // team id -> run before its n-th listing
	answer  map[string]func(w http.ResponseWriter)           // integration id -> answers its PATCH
	rewrite map[string]func(llmroute.Policy) llmroute.Policy // integration id -> rewrites the PATCHed block
}

func newReviEffortInstance(t *testing.T) *reviEffortInstance {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	in := &reviEffortInstance{t: t, s: newForgeTestServer(t), gets: map[string]int{}, beforeG: map[string]func(int){},
		answer: map[string]func(http.ResponseWriter){}, rewrite: map[string]func(llmroute.Policy) llmroute.Policy{}}
	in.bin = filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(in.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in.bin, "iterion"), []byte(reviEffortForwarder), 0o755); err != nil {
		t.Fatal(err)
	}
	super := func(h http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := auth.Identity{UserID: "admin", IsSuperAdmin: true, Credential: auth.CredentialAccessToken}
			h(w, r.WithContext(testMintVerdictContext(auth.WithIdentity(r.Context(), id))))
		})
	}
	mux := http.NewServeMux()
	mux.Handle("GET /api/admin/orgs", super(in.s.handleAdminListOrgs))
	mux.Handle("GET /api/admin/orgs/{id}/teams", super(in.s.handleAdminOrgTeams))
	mux.Handle("GET /api/teams/{id}/forge/repo-bots", super(func(w http.ResponseWriter, r *http.Request) {
		team := r.PathValue("id")
		in.mu.Lock()
		in.gets[team]++
		n, before := in.gets[team], in.beforeG[team]
		in.mu.Unlock()
		if before != nil {
			before(n)
		}
		in.s.handleListForgeRepoBots(w, r)
	}))
	mux.Handle("PATCH /api/teams/{id}/forge/repo-bots/{integration_id}", super(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("integration_id")
		in.mu.Lock()
		in.patches++
		answer, rewrite := in.answer[id], in.rewrite[id]
		in.mu.Unlock()
		if answer != nil {
			answer(w)
			return
		}
		if rewrite != nil {
			var req struct {
				Routing llmroute.Policy `json:"routing"`
			}
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &req); err != nil {
				t.Errorf("decode the script's PATCH: %v", err)
			}
			r.Body = io.NopCloser(strings.NewReader(`{"routing":` + string(mustJSON(t, rewrite(req.Routing))) + `}`))
		}
		in.s.handleUpdateForgeRepoBots(w, r)
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("ITERION_FORWARD_URL", srv.URL)
	return in
}

// seed provisions repo group/api for teamID with the block, through the
// enable handler.
func (in *reviEffortInstance) seed(glURL, teamID string, block llmroute.Policy) string {
	in.t.Helper()
	w := httptest.NewRecorder()
	in.s.handleConnectForge(w, forgeReq(superAdminCtx(), "POST", "/api/teams/"+teamID+"/forge/connections",
		`{"provider":"gitlab","mode":"pat","forge_base_url":"`+glURL+`","pat":"glpat-token"}`, teamID))
	if w.Code != http.StatusOK {
		in.t.Fatalf("connect %s: %d %s", teamID, w.Code, w.Body.String())
	}
	var conn forgeConnectResp
	if err := json.Unmarshal(w.Body.Bytes(), &conn); err != nil {
		in.t.Fatal(err)
	}
	body := `{"connection_id":"` + conn.Connection.ID + `","repo":"group/api","bot_ids":["review-pr"],` +
		`"launch_vars":{"gate_context":"revi/gate"},"hold_labels":["hold"],"overlap":"supersede",` +
		`"routing":` + string(mustJSON(in.t, block)) + `}`
	w = httptest.NewRecorder()
	in.s.handleEnableForgeRepoBots(w, forgeReq(superAdminCtx(), "POST", "/api/teams/"+teamID+"/forge/repo-bots", body, teamID))
	if w.Code != http.StatusOK {
		in.t.Fatalf("enable %s: %d %s", teamID, w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		in.t.Fatal(err)
	}
	return res.IntegrationID
}

func (in *reviEffortInstance) integ(id string) forge.RepoIntegration {
	in.t.Helper()
	ri, err := in.s.forgeIntegrations.Get(context.Background(), id)
	if err != nil {
		in.t.Fatal(err)
	}
	return ri
}

// write is another writer's PATCH of a whole block, through the handler.
func (in *reviEffortInstance) write(teamID, id string, block llmroute.Policy) {
	in.t.Helper()
	r := forgeReq(superAdminCtx(), "PATCH", "/api/teams/"+teamID+"/forge/repo-bots/"+id, `{"routing":`+string(mustJSON(in.t, block))+`}`, teamID)
	r.SetPathValue("integration_id", id)
	w := httptest.NewRecorder()
	in.s.handleUpdateForgeRepoBots(w, r)
	if w.Code != http.StatusOK {
		in.t.Fatalf("write %s: %d %s", id, w.Code, w.Body.String())
	}
}

// run executes the script on the repository's fixture; it answers its
// output, its exit code, and the PATCHes it sent.
func (in *reviEffortInstance) run(args ...string) (string, int, int) {
	in.t.Helper()
	return in.runWith(reviEffortFixture, args...)
}

// runWith is run on another fixture file.
func (in *reviEffortInstance) runWith(fixture string, args ...string) (string, int, int) {
	in.t.Helper()
	in.mu.Lock()
	before := in.patches
	in.mu.Unlock()
	cmd := exec.Command("python3", append([]string{"-I", reviEffortScript, "--fixture", fixture}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+in.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		in.t.Fatalf("run %v: %v", args, err)
	}
	in.mu.Lock()
	sent := in.patches - before
	in.mu.Unlock()
	in.t.Logf("$ revi-effort-cells %s -> exit %d, %d PATCH(es)\n%s", strings.Join(args, " "), code, sent, out)
	return string(out), code, sent
}

// reviEffortFixtureBlock is the fixture's block with its efforts stripped
// (the block production carried), and the efforts by selector.
func reviEffortFixtureBlock(t *testing.T) (llmroute.Policy, map[string]string) {
	t.Helper()
	raw, err := os.ReadFile(reviEffortFixture)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Routing llmroute.Policy `json:"routing"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	efforts := map[string]string{}
	for i, o := range f.Routing.Overrides {
		if o.Effort != "" {
			efforts[o.Selector] = o.Effort
		}
		f.Routing.Overrides[i].Effort = ""
	}
	return f.Routing, efforts
}

func cloneBlock(t *testing.T, p *llmroute.Policy) llmroute.Policy {
	t.Helper()
	var out llmroute.Policy
	if err := json.Unmarshal(mustJSON(t, p), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// writeLaunchVars REPLACES an integration's launch vars (the server refuses a
// PATCH that drops a pinned key without `launch_vars_replace`), the input
// migrate-ids reads to know which node the integration ran as its reviewer.
func (in *reviEffortInstance) writeLaunchVars(teamID, id string, vars map[string]string) {
	in.t.Helper()
	r := forgeReq(superAdminCtx(), "PATCH", "/api/teams/"+teamID+"/forge/repo-bots/"+id, `{"launch_vars":`+string(mustJSON(in.t, vars))+`,"launch_vars_replace":true}`, teamID)
	r.SetPathValue("integration_id", id)
	w := httptest.NewRecorder()
	in.s.handleUpdateForgeRepoBots(w, r)
	if w.Code != http.StatusOK {
		in.t.Fatalf("patch launch vars %s: %d %s", id, w.Code, w.Body.String())
	}
}

// extraCellSelector names a cell nobody ships: the script must preserve the
// cells it did not write, whatever they point at.
const extraCellSelector = "review-pr/reviewer#glm"

func withExtraCell(t *testing.T, p *llmroute.Policy, sel string) llmroute.Policy {
	out := cloneBlock(t, p)
	out.Overrides = append(out.Overrides, llmroute.Override{Selector: sel, Backend: "claw", Provider: "openai_compatible", Model: "openai_compatible/scaleway/deepseek-v4.1-flash"})
	return out
}

func blockEfforts(p *llmroute.Policy) map[string]string {
	out := map[string]string{}
	if p == nil {
		return out
	}
	for _, o := range p.Overrides {
		if o.Effort != "" {
			out[o.Selector] = o.Effort
		}
	}
	return out
}

func hasCell(p *llmroute.Policy, sel string) bool {
	if p == nil {
		return false
	}
	for _, o := range p.Overrides {
		if o.Selector == sel {
			return true
		}
	}
	return false
}

// withCell is p with fn applied to the cell of selector sel.
func withCell(t *testing.T, p *llmroute.Policy, sel string, fn func(*llmroute.Override)) llmroute.Policy {
	t.Helper()
	out := cloneBlock(t, p)
	for i := range out.Overrides {
		if out.Overrides[i].Selector == sel {
			fn(&out.Overrides[i])
			return out
		}
	}
	t.Fatalf("no cell %s", sel)
	return out
}

// reRouted moves a cell off the gateway route, its effort kept.
func reRouted(o *llmroute.Override) {
	o.Backend, o.Provider, o.Model = "claude_code", "anthropic", "claude-opus-5-5"
}

// updatedFixture writes the repository's fixture with one effort changed,
// the file a fixture update would leave, and answers its path.
func updatedFixture(t *testing.T, sel, effort string) string {
	t.Helper()
	raw, err := os.ReadFile(reviEffortFixture)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]any
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range f["routing"].(map[string]any)["overrides"].([]any) {
		if cell := o.(map[string]any); cell["selector"] == sel {
			cell["effort"], found = effort, true
		}
	}
	if !found {
		t.Fatalf("the fixture has no cell %s", sel)
	}
	path := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(path, mustJSON(t, f), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func sameEfforts(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestReviEffortCellsScriptAgainstTheRealHandlers drives the operator
// procedure of #3283 (backup, canary, apply, verify, restore) through this
// package's real admin listings and repo-bots handlers, two teams carrying
// the gateway block on the same repository name and a third whose reviewer
// cell routes elsewhere.
//
// Mutations that redden it (scripts/ops/revi-effort-cells.py): drop apply's
// moved-since-backup refusal, or its re-read before the PATCH; drop the
// read-back of apply or of restore; ignore the route fields when matching a
// cell; let restore write the backup block whole; stop the --all loop at the
// first failure; let --only take a repository name two teams share; compare
// the backup and the stored block with their efforts unmasked; let a second
// backup overwrite, or refuse, the first; write a new backup of a block that
// already carries the fixture's efforts; let restore drop a backed-up
// effort; drop apply's no-longer-a-target recheck; let restore skip, or
// reverse, a re-routed cell carrying the fixture's effort; let verify
// answer 0 with a target behind, or drop an integration that is no longer a
// target.
func TestReviEffortCellsScriptAgainstTheRealHandlers(t *testing.T) {
	gl := newMockGitLab()
	glSrv := gl.server()
	t.Cleanup(glSrv.Close)
	in := newReviEffortInstance(t)
	ctx := context.Background()
	now := time.Now()
	if _, err := in.s.authStore().CreateOrg(ctx, identity.Org{ID: "o1", Name: "SDPC", Slug: "sdpc", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, tm := range []identity.Team{{ID: "t1", OrgID: "o1", Name: "DomiFa", Slug: "domifa"}, {ID: "t2", OrgID: "o1", Name: "VAO", Slug: "vao"}, {ID: "t3", OrgID: "o1", Name: "Other", Slug: "other"}} {
		tm.CreatedAt, tm.UpdatedAt = now, now
		if _, err := in.s.authStore().CreateTeam(ctx, tm); err != nil {
			t.Fatal(err)
		}
	}
	prod, want := reviEffortFixtureBlock(t)
	i1 := in.seed(glSrv.URL, "t1", prod)
	i2 := in.seed(glSrv.URL, "t2", prod)
	// A reviewer cell that routes to claude_code: the gateway rule must not
	// claim it.
	elsewhere := cloneBlock(t, &prod)
	for i, o := range elsewhere.Overrides {
		if o.Selector == "review-pr/reviewer_alternate" {
			elsewhere.Overrides[i].Backend = "claude_code"
			elsewhere.Overrides[i].Provider = "anthropic"
			elsewhere.Overrides[i].Model = "claude-opus-5-5"
		}
	}
	i3 := in.seed(glSrv.URL, "t3", elsewhere)
	dir := t.TempDir()
	reset := func() {
		in.write("t1", i1, prod)
		in.write("t2", i2, prod)
		in.write("t3", i3, elsewhere)
	}

	t.Run("plan names the targets and skips the cell that routes elsewhere", func(t *testing.T) {
		out, code, sent := in.run("plan")
		if code != 0 || sent != 0 || !strings.Contains(out, "2 target(s)") || !strings.Contains(out, "SKIP partial") {
			t.Fatalf("plan exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
	})

	t.Run("apply refuses an integration with no backup", func(t *testing.T) {
		out, code, sent := in.run("apply", "--dir", filepath.Join(dir, "none"), "--only", i1, "--yes")
		if code == 0 || sent != 0 || !strings.Contains(out, "no backup") {
			t.Fatalf("apply with no backup exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
	})

	t.Run("a repository name two teams share is refused", func(t *testing.T) {
		b := filepath.Join(dir, "ambiguous")
		in.run("backup", "--dir", b)
		out, code, sent := in.run("apply", "--dir", b, "--only", "group/api", "--yes")
		if code == 0 || sent != 0 || !strings.Contains(out, "name one by its id") {
			t.Fatalf("--only group/api exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
	})

	t.Run("the canary gets the efforts and nothing else moves", func(t *testing.T) {
		reset()
		b := filepath.Join(dir, "canary")
		in.run("backup", "--dir", b)
		before := in.integ(i1)
		out, code, sent := in.run("apply", "--dir", b, "--only", i1, "--yes")
		after := in.integ(i1)
		if code != 0 || sent != 1 || !sameEfforts(blockEfforts(after.Routing), want) {
			t.Fatalf("apply exited %d after %d PATCH(es), efforts %v want %v:\n%s", code, sent, blockEfforts(after.Routing), want, out)
		}
		if !equalStringSets(before.BotIDs, after.BotIDs) || before.Overlap != after.Overlap || before.LaunchVars["gate_context"] != after.LaunchVars["gate_context"] ||
			strings.Join(before.HoldLabels, ",") != strings.Join(after.HoldLabels, ",") || before.WebhookID != after.WebhookID || before.HookID != after.HookID {
			t.Errorf("the routing PATCH moved another field: before %+v after %+v", before, after)
		}
		if len(after.Routing.Overrides) != len(before.Routing.Overrides) {
			t.Errorf("apply changed the cell count %d -> %d", len(before.Routing.Overrides), len(after.Routing.Overrides))
		}
		if e := blockEfforts(in.integ(i2).Routing); len(e) != 0 {
			t.Errorf("the canary touched the other team: %v", e)
		}
		if out, code, sent := in.run("apply", "--dir", b, "--only", i1, "--yes"); code != 0 || sent != 0 || !strings.Contains(out, "already applied") {
			t.Fatalf("a second apply exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
	})

	t.Run("a block moved since the backup is refused, not overwritten", func(t *testing.T) {
		reset()
		b := filepath.Join(dir, "moved")
		in.run("backup", "--dir", b)
		in.write("t2", i2, withExtraCell(t, &prod, "review-pr/reviewer#glm"))
		out, code, sent := in.run("apply", "--dir", b, "--only", i2, "--yes")
		if code == 0 || sent != 0 || !hasCell(in.integ(i2).Routing, "review-pr/reviewer#glm") {
			t.Fatalf("apply on a moved block exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
	})

	t.Run("a write landing after the listing is read before the PATCH", func(t *testing.T) {
		reset()
		b := filepath.Join(dir, "race")
		in.run("backup", "--dir", b)
		in.mu.Lock()
		base := in.gets["t2"]
		in.beforeG["t2"] = func(n int) {
			if n == base+2 { // the integration's own read, after discovery's
				in.write("t2", i2, withExtraCell(t, &prod, extraCellSelector))
			}
		}
		in.mu.Unlock()
		out, code, sent := in.run("apply", "--dir", b, "--only", i2, "--yes")
		in.mu.Lock()
		delete(in.beforeG, "t2")
		in.mu.Unlock()
		if code == 0 || sent != 0 || !hasCell(in.integ(i2).Routing, extraCellSelector) {
			t.Fatalf("apply after a concurrent write exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
	})

	t.Run("a block stored other than written is reported, apply and restore", func(t *testing.T) {
		reset()
		b := filepath.Join(dir, "mismatch")
		in.run("backup", "--dir", b)
		stripOne := func(p llmroute.Policy) llmroute.Policy {
			for i := range p.Overrides {
				if p.Overrides[i].Selector == "review-pr/converge" {
					p.Overrides[i].Effort = "high"
				}
			}
			return p
		}
		in.mu.Lock()
		in.rewrite[i1] = stripOne
		in.mu.Unlock()
		out, code, _ := in.run("apply", "--dir", b, "--only", i1, "--yes")
		if code == 0 || !strings.Contains(out, "read back is not the one written") {
			t.Fatalf("apply stored otherwise exited %d:\n%s", code, out)
		}
		in.mu.Lock()
		delete(in.rewrite, i1)
		in.mu.Unlock()
		in.write("t1", i1, prod)
		in.run("apply", "--dir", b, "--only", i1, "--yes")
		in.mu.Lock()
		in.rewrite[i1] = func(p llmroute.Policy) llmroute.Policy { return withExtraCell(t, &p, "review-pr/reviewer#glm") }
		in.mu.Unlock()
		out, code, _ = in.run("restore", "--dir", b, "--only", i1, "--yes")
		in.mu.Lock()
		delete(in.rewrite, i1)
		in.mu.Unlock()
		if code == 0 || !strings.Contains(out, "read back is not the one written") {
			t.Fatalf("restore stored otherwise exited %d:\n%s", code, out)
		}
	})

	t.Run("a parked change is not reported applied", func(t *testing.T) {
		reset()
		b := filepath.Join(dir, "parked")
		in.run("backup", "--dir", b)
		in.mu.Lock()
		in.answer[i2] = func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"pending_approval":true}`))
		}
		in.mu.Unlock()
		out, code, _ := in.run("apply", "--dir", b, "--only", i2, "--yes")
		in.mu.Lock()
		delete(in.answer, i2)
		in.mu.Unlock()
		if code == 0 || !strings.Contains(out, "PARKED") || len(blockEfforts(in.integ(i2).Routing)) != 0 {
			t.Fatalf("a parked apply exited %d:\n%s", code, out)
		}
	})

	t.Run("one failing integration does not stop the others", func(t *testing.T) {
		reset()
		b := filepath.Join(dir, "partial")
		in.run("backup", "--dir", b)
		in.mu.Lock()
		in.answer[i1] = func(w http.ResponseWriter) { http.Error(w, "injected", http.StatusBadGateway) }
		in.mu.Unlock()
		out, code, _ := in.run("apply", "--dir", b, "--all", "--yes")
		in.mu.Lock()
		delete(in.answer, i1)
		in.mu.Unlock()
		if code == 0 || !strings.Contains(out, "FAILED") || !sameEfforts(blockEfforts(in.integ(i2).Routing), want) {
			t.Fatalf("apply --all with one failure exited %d, the other carries %v:\n%s", code, blockEfforts(in.integ(i2).Routing), out)
		}
		if e := blockEfforts(in.integ(i3).Routing); len(e) != 0 {
			t.Errorf("the integration whose reviewer routes elsewhere was given efforts: %v", e)
		}
	})

	t.Run("restore reverses the efforts only, and refuses one someone else changed", func(t *testing.T) {
		reset()
		b := filepath.Join(dir, "restore")
		in.run("backup", "--dir", b)
		in.run("apply", "--dir", b, "--only", i1, "--yes")
		later := withExtraCell(t, in.integ(i1).Routing, "review-pr/reviewer#glm")
		in.write("t1", i1, later)
		in.write("t2", i2, withExtraCell(t, &prod, extraCellSelector))
		out, code, sent := in.run("restore", "--dir", b, "--all", "--yes")
		if code != 0 || sent != 1 {
			t.Fatalf("restore --all exited %d after %d PATCH(es), want one (the applied integration):\n%s", code, sent, out)
		}
		if e := blockEfforts(in.integ(i1).Routing); len(e) != 0 || !hasCell(in.integ(i1).Routing, "review-pr/reviewer#glm") {
			t.Errorf("after restore i1 carries efforts %v, later cell kept=%v", e, hasCell(in.integ(i1).Routing, "review-pr/reviewer#glm"))
		}
		if !hasCell(in.integ(i2).Routing, extraCellSelector) {
			t.Errorf("restore rewrote i2, which apply never touched, dropping its later cell")
		}
		in.run("apply", "--dir", b, "--only", i2, "--yes") // refused: moved since the backup
		in.write("t2", i2, prod)
		in.run("apply", "--dir", b, "--only", i2, "--yes")
		changed := cloneBlock(t, in.integ(i2).Routing)
		for i := range changed.Overrides {
			if changed.Overrides[i].Selector == "review-pr/reviewer_alternate" {
				changed.Overrides[i].Effort = "xhigh"
			}
		}
		in.write("t2", i2, changed)
		out, code, sent = in.run("restore", "--dir", b, "--only", i2, "--yes")
		if code == 0 || sent != 0 || blockEfforts(in.integ(i2).Routing)["review-pr/reviewer_alternate"] != "xhigh" {
			t.Fatalf("restore over an effort someone changed exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
	})
	t.Run("an effort carried before the backup comes back on restore", func(t *testing.T) {
		reset()
		seeded := withCell(t, &prod, "review-pr/reviewer_alternate", func(o *llmroute.Override) { o.Effort = "medium" })
		in.write("t1", i1, seeded)
		b := filepath.Join(dir, "seeded")
		if out, code, _ := in.run("backup", "--dir", b); code != 0 {
			t.Fatalf("backup of a block carrying a non-fixture effort exited %d:\n%s", code, out)
		}
		if out, code, _ := in.run("apply", "--dir", b, "--only", i1, "--yes"); code != 0 || !sameEfforts(blockEfforts(in.integ(i1).Routing), want) {
			t.Fatalf("apply exited %d, efforts %v:\n%s", code, blockEfforts(in.integ(i1).Routing), out)
		}
		out, code, sent := in.run("restore", "--dir", b, "--only", i1, "--yes")
		if got := blockEfforts(in.integ(i1).Routing); code != 0 || sent != 1 || !sameEfforts(got, map[string]string{"review-pr/reviewer_alternate": "medium"}) {
			t.Fatalf("restore exited %d after %d PATCH(es), efforts %v, want reviewer_alternate back to medium:\n%s", code, sent, got, out)
		}
	})

	t.Run("a second backup into the same dir keeps the first byte for byte", func(t *testing.T) {
		reset()
		b := filepath.Join(dir, "twice")
		in.run("backup", "--dir", b)
		path := filepath.Join(b, i1+".json")
		first, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		in.run("apply", "--dir", b, "--only", i1, "--yes")
		out, code, _ := in.run("backup", "--dir", b)
		if code != 0 || !strings.Contains(out, "kept     group/api ("+i1+")") {
			t.Fatalf("a backup over the applied block exited %d, want the first kept:\n%s", code, out)
		}
		if again, _ := os.ReadFile(path); string(again) != string(first) {
			t.Fatalf("the second backup rewrote %s:\n%s\nwas\n%s", path, again, first)
		}
		in.write("t1", i1, withExtraCell(t, in.integ(i1).Routing, "review-pr/reviewer#glm"))
		out, code, _ = in.run("backup", "--dir", b)
		if again, _ := os.ReadFile(path); code == 0 || !strings.Contains(out, "DIFFERENT backup") || string(again) != string(first) {
			t.Fatalf("a backup over a block that moved exited %d, file kept=%v:\n%s", code, string(again) == string(first), out)
		}
	})

	t.Run("a new backup of a block carrying the fixture's efforts is refused", func(t *testing.T) {
		reset()
		b := filepath.Join(dir, "first")
		in.run("backup", "--dir", b)
		in.run("apply", "--dir", b, "--only", i1, "--yes")
		fresh := filepath.Join(dir, "fresh")
		out, code, _ := in.run("backup", "--dir", fresh)
		if _, err := os.Stat(filepath.Join(fresh, i1+".json")); code == 0 || !strings.Contains(out, "already carries the fixture's effort") || err == nil {
			t.Fatalf("a new backup of the applied block exited %d, written=%v:\n%s", code, err == nil, out)
		}
	})

	t.Run("a fixture update applies and restores over the original backup", func(t *testing.T) {
		reset()
		b := filepath.Join(dir, "update")
		in.run("backup", "--dir", b)
		in.run("apply", "--dir", b, "--only", i1, "--yes")
		updated := updatedFixture(t, "review-pr/reviewer_alternate", "medium")
		out, code, sent := in.runWith(updated, "apply", "--dir", b, "--only", i1, "--yes")
		if code != 0 || sent != 1 || blockEfforts(in.integ(i1).Routing)["review-pr/reviewer_alternate"] != "medium" {
			t.Fatalf("the updated fixture's apply exited %d after %d PATCH(es), reviewer_alternate %q:\n%s", code, sent, blockEfforts(in.integ(i1).Routing)["review-pr/reviewer_alternate"], out)
		}
		out, code, _ = in.runWith(updated, "restore", "--dir", b, "--only", i1, "--yes")
		if e := blockEfforts(in.integ(i1).Routing); code != 0 || len(e) != 0 {
			t.Fatalf("restore after the update exited %d, efforts left %v:\n%s", code, e, out)
		}
	})

	t.Run("verify exits 1 before the apply and 0 after", func(t *testing.T) {
		reset()
		if out, code, sent := in.run("verify"); code != 1 || sent != 0 || !strings.Contains(out, "NOT APPLIED") {
			t.Fatalf("verify before the apply exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
		b := filepath.Join(dir, "verify")
		in.run("backup", "--dir", b)
		in.run("apply", "--dir", b, "--all", "--yes")
		if out, code, _ := in.run("verify"); code != 0 || strings.Count(out, "applied     group/api") != 2 {
			t.Fatalf("verify after the apply exited %d:\n%s", code, out)
		}
	})

	t.Run("apply rechecks the verdict on the block it re-reads", func(t *testing.T) {
		reset()
		b := filepath.Join(dir, "verdict")
		in.run("backup", "--dir", b)
		in.mu.Lock()
		base := in.gets["t2"]
		in.beforeG["t2"] = func(n int) {
			if n == base+2 { // the integration's own read, after discovery's
				in.write("t2", i2, withCell(t, &prod, "review-pr/reviewer_alternate", reRouted))
			}
		}
		in.mu.Unlock()
		out, code, sent := in.run("apply", "--dir", b, "--only", i2, "--yes")
		in.mu.Lock()
		delete(in.beforeG, "t2")
		in.mu.Unlock()
		if code == 0 || sent != 0 || !strings.Contains(out, "no longer a target: partial: no gateway cell for review-pr/reviewer_alternate") {
			t.Fatalf("apply on a block re-routed after the listing exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
	})

	t.Run("a cell re-routed since the apply is refused by restore and reported by verify", func(t *testing.T) {
		reset()
		b := filepath.Join(dir, "rerouted")
		in.run("backup", "--dir", b)
		in.run("apply", "--dir", b, "--only", i1, "--yes")
		in.write("t1", i1, withCell(t, in.integ(i1).Routing, "review-pr/reviewer_alternate", reRouted))
		out, code, sent := in.run("restore", "--dir", b, "--only", i1, "--yes")
		if code == 0 || sent != 0 || !strings.Contains(out, "review-pr/reviewer_alternate carries the fixture's effort 'high' but is not routed to the gateway") ||
			blockEfforts(in.integ(i1).Routing)["review-pr/reviewer_alternate"] != "high" {
			t.Fatalf("restore over a re-routed cell exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
		if out, code, _ := in.run("verify", "--only", i1); code != 1 || !strings.Contains(out, "RE-ROUTED   group/api ("+i1+")") {
			t.Fatalf("verify over a re-routed cell exited %d:\n%s", code, out)
		}
		every := cloneBlock(t, in.integ(i1).Routing)
		for i := range every.Overrides {
			if want[every.Overrides[i].Selector] != "" {
				reRouted(&every.Overrides[i])
			}
		}
		in.write("t1", i1, every)
		if out, code, _ := in.run("verify"); code != 1 || !strings.Contains(out, "RE-ROUTED   group/api ("+i1+"): none") {
			t.Fatalf("verify over a block whose every effort cell was re-routed exited %d:\n%s", code, out)
		}
	})

	// ADR-169 renamed review-pr's nodes to roles. A stored block still naming
	// reviewer_gpt/reviewer_claude/converge_gpt decides NOTHING: the launch
	// falls back on the bot's literals, and the effort cells #3283 applied
	// go inert with them — silently. migrate-ids rewrites the stored block,
	// backs it up first, and reads it back.
	t.Run("migrate-ids renames the retired node names and drops what no node takes", func(t *testing.T) {
		reset()
		// A block as a pre-rename integration carries it: the retired ids
		// only, with the claude merge cell and its GLM route beside the gpt
		// twin, a glance claude cell, and one cell a product added.
		const gw = "openai_compatible/scaleway/deepseek-v4.1-flash"
		retired := llmroute.Policy{Overrides: []llmroute.Override{
			{Selector: "review-pr/reviewer_claude", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "high"},
			{Selector: "review-pr/reviewer_claude_glance", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "low"},
			{Selector: "review-pr/reviewer_gpt", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "high"},
			{Selector: "review-pr/reviewer_gpt_glance", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "low"},
			{Selector: "review-pr/converge", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "low"},
			{Selector: "review-pr/converge_gpt", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "low"},
			{Selector: "review-pr/converge#glm", Backend: "claw", Provider: "openai_compatible", Model: gw},
			{Selector: "review-pr/reviewer_claude#glm", Backend: "claw", Provider: "openai_compatible", Model: gw},
			{Selector: "review-pr/supervisor:pacer", Model: gw},
			// evolve renamed its reviewers too, with the slots the OTHER way
			// round: its claude judge became `reviewer`, its claw one
			// `reviewer_alternate` — review-pr's table must not apply.
			{Selector: "evolve/review_claude", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "medium"},
			{Selector: "evolve/review_gpt", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "low"},
		}}
		in.write("t1", i1, retired)

		b := filepath.Join(dir, "migrate")
		out, code, sent := in.run("migrate-ids", "--prune", "--dir", b, "--all", "--dry-run", "--assume-family", "claude")
		if code != 0 || sent != 0 {
			t.Fatalf("a dry run exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
		// The seeded integration carries no mono_family: it ran the old
		// DEFAULT, mono-claude, where `reviewer_claude` was THE reviewer — so
		// its cell lands on `reviewer`, the node a single review runs.
		for _, want := range []string{"review-pr/reviewer_claude -> review-pr/reviewer", "review-pr/reviewer_gpt -> review-pr/reviewer_alternate", "review-pr/reviewer_claude_glance -> review-pr/reviewer_glance", "review-pr/converge#glm", "review-pr/converge_gpt"} {
			if !strings.Contains(out, want) {
				t.Errorf("the dry run does not say %q:\n%s", want, out)
			}
		}
		if _, err := os.Stat(filepath.Join(b, i1+".pre-migrate-ids.json")); err == nil {
			t.Error("a dry run wrote a backup — it must touch nothing, the backup dir included")
		}
		if !hasCell(in.integ(i1).Routing, "review-pr/reviewer_gpt") {
			t.Fatal("the dry run wrote")
		}

		out, code, sent = in.run("migrate-ids", "--prune", "--dir", b, "--only", i1, "--yes", "--assume-family", "claude")
		if code != 0 || sent != 1 || !strings.Contains(out, "migrated group/api ("+i1+")") {
			t.Fatalf("migrate-ids exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
		got := blockSelectors(in.integ(i1).Routing)
		// mono-claude: the pacer that watched reviewer_claude watched THE
		// reviewer, and `reviewer` is watched by pacer_gpt now.
		for _, want := range []string{"review-pr/reviewer_alternate", "review-pr/reviewer", "review-pr/reviewer_glance", "review-pr/converge", "review-pr/supervisor:pacer_gpt"} {
			if !slices.Contains(got, want) {
				t.Errorf("the migrated block does not carry %q: %v", want, got)
			}
		}
		if e := blockEfforts(in.integ(i1).Routing); e["evolve/reviewer"] != "medium" || e["evolve/reviewer_alternate"] != "low" {
			t.Errorf("evolve's cells were not renamed per evolve's own roles: efforts %v, want evolve/reviewer=medium (was review_claude) and evolve/reviewer_alternate=low (was review_gpt)", e)
		}
		for _, gone := range []string{"review-pr/reviewer_gpt", "review-pr/reviewer_claude", "review-pr/reviewer_gpt_glance", "review-pr/converge_gpt", "review-pr/converge#glm", "review-pr/reviewer_claude_glance", "evolve/review_claude", "evolve/review_gpt"} {
			if slices.Contains(got, gone) {
				t.Errorf("the migrated block still carries the retired %q: %v", gone, got)
			}
		}
		if n := len(got); n != len(slices.Compact(slices.Sorted(slices.Values(got)))) {
			t.Errorf("the migrated block names a selector twice: %v", got)
		}
		// The effort each cell carried survives the rename.
		if e := blockEfforts(in.integ(i1).Routing); e["review-pr/reviewer"] != "high" || e["review-pr/reviewer_glance"] != "low" || e["review-pr/converge"] != "low" {
			t.Errorf("the efforts moved with the rename: %v, want reviewer=high, reviewer_glance=low (the claude glance) and converge=low", e)
		}
		// And the backup holds the block as it was, retired names included.
		raw, err := os.ReadFile(filepath.Join(b, i1+".pre-migrate-ids.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "review-pr/reviewer_gpt") {
			t.Errorf("the backup does not hold the pre-migration block:\n%s", raw)
		}
		// A second run has nothing left to do.
		if out, code, sent := in.run("migrate-ids", "--prune", "--dir", b, "--only", i1, "--yes", "--assume-family", "claude"); code != 0 || sent != 0 || !strings.Contains(out, "already migrated") {
			t.Fatalf("a second migrate-ids exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
	})

	// No `mono_family` launch var is no evidence of claude: the old engine
	// resolved the family from credentials. Where the two readings migrate
	// the block differently, the run refuses to guess — no PATCH, a non-zero
	// exit — until --assume-family answers it.
	t.Run("migrate-ids refuses to guess the family of an integration that pinned none", func(t *testing.T) {
		reset()
		const gw = "openai_compatible/scaleway/deepseek-v4.1-flash"
		in.write("t1", i1, llmroute.Policy{Overrides: []llmroute.Override{
			{Selector: "review-pr/reviewer_gpt", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "high"},
		}})
		b := filepath.Join(dir, "ambiguous-family")
		out, code, sent := in.run("migrate-ids", "--prune", "--dir", b, "--only", i1, "--yes")
		if code == 0 || sent != 0 || !strings.Contains(out, "AMBIGUOUS") || !strings.Contains(out, "--assume-family") {
			t.Fatalf("migrate-ids on an integration with no mono_family exited %d after %d PATCH(es), want AMBIGUOUS and nothing written:\n%s", code, sent, out)
		}
		if out, code, _ := in.run("migrate-ids", "--prune", "--dir", b, "--only", i1, "--yes", "--assume-family", "gpt"); code != 0 {
			t.Fatalf("migrate-ids --assume-family gpt exited %d:\n%s", code, out)
		}
		if e := blockEfforts(in.integ(i1).Routing); e["review-pr/reviewer"] != "high" {
			t.Errorf("assumed gpt, the reviewer_gpt cell should land on reviewer: %v", e)
		}
	})

	// A glob cell covered every reviewer slot and no longer covers the
	// renamed primary: `reviewer_*` does not match `reviewer`. It cannot be
	// rewritten mechanically, so the run reports it and does not count the
	// integration as migrated — a dry run says so before anything is written.
	t.Run("migrate-ids reports a glob the rename stranded", func(t *testing.T) {
		reset()
		const gw = "openai_compatible/scaleway/deepseek-v4.1-flash"
		in.write("t1", i1, llmroute.Policy{Overrides: []llmroute.Override{
			{Selector: "review-pr/reviewer_*", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "high"},
		}})
		b := filepath.Join(dir, "glob")
		out, code, sent := in.run("migrate-ids", "--prune", "--dir", b, "--only", i1, "--dry-run")
		if code == 0 || sent != 0 || !strings.Contains(out, "GLOB") || !strings.Contains(out, "review-pr/reviewer_*") {
			t.Fatalf("a dry run over a stranded glob exited %d after %d PATCH(es), want a GLOB line and a non-zero exit:\n%s", code, sent, out)
		}
		out, code, sent = in.run("migrate-ids", "--prune", "--dir", b, "--only", i1, "--yes", "--assume-family", "claude")
		if code == 0 || sent != 0 || !strings.Contains(out, "rewrite it by hand") {
			t.Fatalf("a run over a stranded glob exited %d after %d PATCH(es), want it reported and nothing written:\n%s", code, sent, out)
		}
		if !hasCell(in.integ(i1).Routing, "review-pr/reviewer_*") {
			t.Error("the run rewrote the glob — it must leave it to the operator")
		}
	})

	// The collision rule must not depend on the stored ORDER: a block that
	// lists `converge_gpt` before the claude `converge` cell is the same
	// block, and must leave ONE `converge` — the renamed one, whose route
	// the surviving node takes.
	t.Run("migrate-ids resolves a converge collision by the topology the integration ran, whatever the stored order", func(t *testing.T) {
		const gw = "openai_compatible/scaleway/deepseek-v4.1-flash"
		for _, tc := range []struct {
			name       string
			launchVars map[string]string
			wantEffort string // of the surviving converge cell
		}{
			// The old default merged on the claude `converge`: its cell is
			// the live route, and the gpt twin's never ran.
			{"mono-claude (the old default)", map[string]string{"gate_context": "revi/gate"}, "high"},
			// mono-gpt merged on `converge_gpt`: the renamed cell wins.
			{"mono-gpt", map[string]string{"gate_context": "revi/gate", "mono_family": "gpt"}, "low"},
		} {
			for _, order := range [][]string{
				{"review-pr/converge", "review-pr/converge_gpt"},
				{"review-pr/converge_gpt", "review-pr/converge"},
			} {
				reset()
				in.writeLaunchVars("t1", i1, tc.launchVars)
				block := llmroute.Policy{}
				for _, sel := range order {
					effort := "low"
					if sel == "review-pr/converge" {
						effort = "high" // the claude merge's cell
					}
					block.Overrides = append(block.Overrides, llmroute.Override{Selector: sel, Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: effort})
				}
				in.write("t1", i1, block)
				b := filepath.Join(dir, "order-"+strings.ReplaceAll(tc.name, " ", "_")+"-"+strings.ReplaceAll(order[0], "/", "_"))
				if out, code, _ := in.run("migrate-ids", "--prune", "--dir", b, "--only", i1, "--yes", "--assume-family", "claude"); code != 0 {
					t.Fatalf("%s, order %v: migrate-ids exited %d:\n%s", tc.name, order, code, out)
				}
				got := blockSelectors(in.integ(i1).Routing)
				n := 0
				for _, sel := range got {
					if sel == "review-pr/converge" {
						n++
					}
				}
				if n != 1 {
					t.Errorf("%s, order %v: %d `review-pr/converge` cells, want exactly 1: %v", tc.name, order, n, got)
				}
				if e := blockEfforts(in.integ(i1).Routing)["review-pr/converge"]; e != tc.wantEffort {
					t.Errorf("%s, order %v: the surviving converge cell carries effort %q, want %q — the merge the integration actually ran", tc.name, order, e, tc.wantEffort)
				}
			}
		}
		in.writeLaunchVars("t1", i1, map[string]string{"gate_context": "revi/gate"})
	})

	// A mono-gpt integration ran `reviewer_gpt` as THE reviewer: its cell
	// lands on `reviewer`, and the claude node becomes the alternate.
	t.Run("migrate-ids maps a mono-gpt integration's reviewer onto the role it ran", func(t *testing.T) {
		reset()
		const gw = "openai_compatible/scaleway/deepseek-v4.1-flash"
		in.writeLaunchVars("t1", i1, map[string]string{"gate_context": "revi/gate", "mono_family": "gpt"})
		in.write("t1", i1, llmroute.Policy{Overrides: []llmroute.Override{
			{Selector: "review-pr/reviewer_gpt", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "xhigh"},
			{Selector: "review-pr/reviewer_claude", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "medium"},
		}})
		b := filepath.Join(dir, "mono-gpt")
		if out, code, _ := in.run("migrate-ids", "--prune", "--dir", b, "--only", i1, "--yes", "--assume-family", "claude"); code != 0 {
			t.Fatalf("migrate-ids exited %d:\n%s", code, out)
		}
		if e := blockEfforts(in.integ(i1).Routing); e["review-pr/reviewer"] != "xhigh" || e["review-pr/reviewer_alternate"] != "medium" {
			t.Errorf("a mono-gpt integration's cells: %v, want reviewer=xhigh (was reviewer_gpt) and reviewer_alternate=medium (was reviewer_claude)", e)
		}
		in.writeLaunchVars("t1", i1, map[string]string{"gate_context": "revi/gate"})
	})

	// The effort procedure's own backup (`<id>.json`) may hold an EARLIER
	// state of the block; migrate-ids writes its own file and refuses one
	// that is not this block, so the block a PATCH overwrites is always saved.
	t.Run("migrate-ids saves the block it overwrites under its own backup", func(t *testing.T) {
		reset()
		const gw = "openai_compatible/scaleway/deepseek-v4.1-flash"
		b := filepath.Join(dir, "own-backup")
		in.run("backup", "--dir", b) // the effort procedure's <id>.json, of the block before
		in.write("t1", i1, llmroute.Policy{Overrides: []llmroute.Override{
			{Selector: "review-pr/reviewer_claude", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "high"},
		}})
		if out, code, _ := in.run("migrate-ids", "--prune", "--dir", b, "--only", i1, "--yes", "--assume-family", "claude"); code != 0 {
			t.Fatalf("migrate-ids exited %d:\n%s", code, out)
		}
		raw, err := os.ReadFile(filepath.Join(b, i1+".pre-migrate-ids.json"))
		if err != nil || !strings.Contains(string(raw), "review-pr/reviewer_claude") {
			t.Fatalf("the block migrate-ids overwrote is not saved under its own backup (err=%v):\n%s", err, raw)
		}
		// A DIFFERENT pre-migration backup already there refuses.
		in.write("t1", i1, llmroute.Policy{Overrides: []llmroute.Override{
			{Selector: "review-pr/reviewer_gpt", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "low"},
		}})
		if out, code, sent := in.run("migrate-ids", "--prune", "--dir", b, "--only", i1, "--yes", "--assume-family", "claude"); code == 0 || sent != 0 || !strings.Contains(out, "changed since its pre-migration backup") {
			t.Fatalf("migrate-ids over a different pre-migration backup exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
	})

	// The catalogue serves ONE review-pr version to every integration: the
	// roll that ships the rename moves them all at once, and a block spelled
	// for either vocabulary is inert under the other. --stage writes the
	// role cells BESIDE the retired ones so both bundles find theirs while
	// the fleet moves; --prune, after the roll, leaves the roles alone.
	t.Run("migrate-ids stages both vocabularies before the roll and prunes after it", func(t *testing.T) {
		reset()
		const gw = "openai_compatible/scaleway/deepseek-v4.1-flash"
		in.writeLaunchVars("t1", i1, map[string]string{"gate_context": "revi/gate", "mono_family": "gpt"})
		defer in.writeLaunchVars("t1", i1, map[string]string{"gate_context": "revi/gate"})
		// A mono-gpt gateway integration as production stores it: the claude
		// merge cell beside the gpt twin, on different models here so the
		// one `converge` keeps is visible.
		retired := llmroute.Policy{Overrides: []llmroute.Override{
			{Selector: "review-pr/reviewer_gpt", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "high"},
			{Selector: "review-pr/reviewer_gpt_glance", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "low"},
			{Selector: "review-pr/reviewer_claude", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "medium"},
			{Selector: "review-pr/reviewer_claude_glance", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "low"},
			{Selector: "review-pr/converge", Backend: "claw", Provider: "openai_compatible", Model: "openai_compatible/scaleway/claude-merge", Effort: "xhigh"},
			{Selector: "review-pr/converge_gpt", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "low"},
			{Selector: "review-pr/converge#glm", Backend: "claw", Provider: "openai_compatible", Model: gw},
			{Selector: "review-pr/supervisor:pacer_gpt", Model: gw},
		}}
		in.write("t1", i1, retired)
		// An integration with no block at all has nothing to stage.
		in.write("t2", i2, llmroute.Policy{})
		b := filepath.Join(dir, "two-phase")

		out, code, sent := in.run("migrate-ids", "--stage", "--dir", b, "--all", "--dry-run")
		if code != 0 || sent != 0 {
			t.Fatalf("a staging dry run exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
		if !strings.Contains(out, "unchanged group/api ("+i2+"): no retired selector") {
			t.Errorf("an integration with no block is reported as work to do:\n%s", out)
		}
		// A block already in the role vocabulary, pacers included, is left
		// alone under either family reading — a migration must be a fixed
		// point of itself, or the second run swaps the pacers back.
		if !strings.Contains(out, "unchanged group/api ("+i3+"): no retired selector") {
			t.Errorf("a block already spelled with the roles is reported as work to do:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(b, i1+".pre-migrate-ids.json")); err == nil {
			t.Error("a staging dry run wrote a backup")
		}

		out, code, sent = in.run("migrate-ids", "--stage", "--dir", b, "--only", i1, "--yes")
		if code != 0 || sent != 1 || !strings.Contains(out, "staged") {
			t.Fatalf("--stage exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
		staged := in.integ(i1).Routing
		got := blockSelectors(staged)
		// Every selector the block carried is still there for the bundle
		// before the roll — the retired names, their routes, the pacer.
		for _, o := range retired.Overrides {
			if !slices.Contains(got, o.Selector) {
				t.Errorf("the staged block lost the retired %q, which the bundle before the roll still reads: %v", o.Selector, got)
			}
		}
		// And the bundle after it finds its roles, with the cells the
		// integration ran: reviewer_gpt was its reviewer.
		e := blockEfforts(staged)
		if e["review-pr/reviewer"] != "high" || e["review-pr/reviewer_glance"] != "low" || e["review-pr/reviewer_alternate"] != "medium" {
			t.Errorf("the staged role cells: %v, want reviewer=high (reviewer_gpt), reviewer_glance=low, reviewer_alternate=medium (reviewer_claude)", e)
		}
		// mono-gpt merged on the gpt twin: `converge` takes ITS cell.
		if e["review-pr/converge"] != "low" {
			t.Errorf("the staged converge carries effort %q, want low — the gpt twin's cell, the merge a mono-gpt integration runs after the roll", e["review-pr/converge"])
		}
		if n := len(got); n != len(slices.Compact(slices.Sorted(slices.Values(got)))) {
			t.Errorf("the staged block names a selector twice: %v", got)
		}

		// A second --stage has nothing to add.
		if out, code, sent := in.run("migrate-ids", "--stage", "--dir", b, "--only", i1, "--yes"); code != 0 || sent != 0 || !strings.Contains(out, "already staged") {
			t.Fatalf("a second --stage exited %d after %d PATCH(es):\n%s", code, sent, out)
		}

		// After the roll: --prune leaves the role view, computed from the
		// block as it was before staging.
		out, code, sent = in.run("migrate-ids", "--prune", "--dir", b, "--only", i1, "--yes")
		if code != 0 || sent != 1 {
			t.Fatalf("--prune exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
		got = blockSelectors(in.integ(i1).Routing)
		for _, gone := range []string{"review-pr/reviewer_gpt", "review-pr/reviewer_gpt_glance", "review-pr/reviewer_claude", "review-pr/reviewer_claude_glance", "review-pr/converge_gpt", "review-pr/converge#glm"} {
			if slices.Contains(got, gone) {
				t.Errorf("the pruned block still carries the retired %q: %v", gone, got)
			}
		}
		if e := blockEfforts(in.integ(i1).Routing); e["review-pr/reviewer"] != "high" || e["review-pr/converge"] != "low" {
			t.Errorf("the pruned role cells: %v, want reviewer=high and converge=low", e)
		}
		if !hasCell(in.integ(i1).Routing, "review-pr/supervisor:pacer_gpt") {
			t.Errorf("the pruned block lost the pacer cell: %v", got)
		}
		if out, code, sent := in.run("migrate-ids", "--prune", "--dir", b, "--only", i1, "--yes"); code != 0 || sent != 0 || !strings.Contains(out, "already migrated") {
			t.Fatalf("a second --prune exited %d after %d PATCH(es):\n%s", code, sent, out)
		}
	})

	// Between the phases the block is the operator's as much as the
	// script's: one changed after --stage is neither the origin nor its
	// staged form, and --prune refuses it rather than overwrite the change.
	// A block staged from ANOTHER dir is refused too: the dir given holds no
	// origin, and one derived from the staged block would lose what a pacer
	// swap set aside.
	t.Run("migrate-ids refuses to prune a block changed since the stage, or staged from another dir", func(t *testing.T) {
		reset()
		in.writeLaunchVars("t1", i1, map[string]string{"gate_context": "revi/gate"})
		const gw = "openai_compatible/scaleway/deepseek-v4.1-flash"
		in.write("t1", i1, llmroute.Policy{Overrides: []llmroute.Override{
			{Selector: "review-pr/reviewer_claude", Backend: "claw", Provider: "openai_compatible", Model: gw, Effort: "high"},
		}})
		b := filepath.Join(dir, "two-phase-moved")
		if out, code, _ := in.run("migrate-ids", "--stage", "--dir", b, "--only", i1, "--yes", "--assume-family", "claude"); code != 0 {
			t.Fatalf("--stage exited %d:\n%s", code, out)
		}
		if out, code, sent := in.run("migrate-ids", "--prune", "--dir", filepath.Join(dir, "two-phase-other"), "--only", i1, "--yes", "--assume-family", "claude"); code == 0 || sent != 0 || !strings.Contains(out, "staged with another --dir") {
			t.Fatalf("--prune with a dir that holds no stage exited %d after %d PATCH(es), want it refused:\n%s", code, sent, out)
		}
		moved := withCell(t, in.integ(i1).Routing, "review-pr/reviewer", func(o *llmroute.Override) { o.Effort = "max" })
		in.write("t1", i1, moved)
		if out, code, sent := in.run("migrate-ids", "--prune", "--dir", b, "--only", i1, "--yes", "--assume-family", "claude"); code == 0 || sent != 0 || !strings.Contains(out, "changed since its pre-migration backup") {
			t.Fatalf("--prune over a block changed since the stage exited %d after %d PATCH(es), want it refused:\n%s", code, sent, out)
		}
		if e := blockEfforts(in.integ(i1).Routing); e["review-pr/reviewer"] != "max" {
			t.Errorf("the refused prune overwrote the operator's change: %v", e)
		}
	})

	// Supervisor names say which ROUTE they watch, not which role: `pacer`
	// watches the claude-route node, `pacer_gpt` the gpt-route one, in both
	// vocabularies. A mono-claude integration's reviewer crosses that line
	// (reviewer_claude becomes `reviewer`, which `pacer_gpt` watches), so its
	// pacer cells swap — and while both bundles run, the stage keeps the
	// stored `pacer` cell, whose old meaning is the one that runs.
	t.Run("migrate-ids swaps a mono-claude integration's pacer cells with its reviewer", func(t *testing.T) {
		reset()
		in.writeLaunchVars("t1", i1, map[string]string{"gate_context": "revi/gate"})
		defer in.writeLaunchVars("t1", i1, map[string]string{"gate_context": "revi/gate"})
		block := llmroute.Policy{Overrides: []llmroute.Override{
			{Selector: "review-pr/reviewer_claude", Backend: "claw", Provider: "openai_compatible", Model: "openai_compatible/gw/claude-route", Effort: "high"},
			{Selector: "review-pr/supervisor:pacer", Model: "openai_compatible/gw/claude-pacer"},
			{Selector: "review-pr/supervisor:pacer_gpt", Model: "openai_compatible/gw/gpt-pacer"},
		}}
		in.write("t1", i1, block)
		models := func() map[string]string {
			out := map[string]string{}
			for _, o := range in.integ(i1).Routing.Overrides {
				out[o.Selector] = o.Model
			}
			return out
		}
		b := filepath.Join(dir, "pacer-swap")
		if out, code, _ := in.run("migrate-ids", "--stage", "--dir", b, "--only", i1, "--yes", "--assume-family", "claude"); code != 0 {
			t.Fatalf("--stage exited %d:\n%s", code, out)
		}
		if m := models(); m["review-pr/supervisor:pacer"] != "openai_compatible/gw/claude-pacer" || m["review-pr/supervisor:pacer_gpt"] != "openai_compatible/gw/claude-pacer" {
			t.Errorf("the staged pacers: %v, want pacer kept (it watches reviewer_claude until the roll) and pacer_gpt on the claude pacer's model (it watches `reviewer` after it)", m)
		}
		if out, code, _ := in.run("migrate-ids", "--prune", "--dir", b, "--only", i1, "--yes", "--assume-family", "claude"); code != 0 {
			t.Fatalf("--prune exited %d:\n%s", code, out)
		}
		if m := models(); m["review-pr/supervisor:pacer_gpt"] != "openai_compatible/gw/claude-pacer" || m["review-pr/supervisor:pacer"] != "openai_compatible/gw/gpt-pacer" {
			t.Errorf("the pruned pacers: %v, want pacer_gpt=claude-pacer (the reviewer's) and pacer=gpt-pacer (the alternate's)", m)
		}
		// A mono-gpt integration's reviewer stays on its route: no swap.
		in.writeLaunchVars("t1", i1, map[string]string{"gate_context": "revi/gate", "mono_family": "gpt"})
		in.write("t1", i1, block)
		if out, code, _ := in.run("migrate-ids", "--prune", "--dir", filepath.Join(dir, "pacer-noswap"), "--only", i1, "--yes"); code != 0 {
			t.Fatalf("--prune exited %d:\n%s", code, out)
		}
		if m := models(); m["review-pr/supervisor:pacer"] != "openai_compatible/gw/claude-pacer" || m["review-pr/supervisor:pacer_gpt"] != "openai_compatible/gw/gpt-pacer" {
			t.Errorf("a mono-gpt integration's pacers moved: %v", m)
		}
	})
}

// blockSelectors is a block's selectors in order.
func blockSelectors(p *llmroute.Policy) []string {
	out := []string{}
	for _, o := range (p).Overrides {
		out = append(out, o.Selector)
	}
	return out
}
