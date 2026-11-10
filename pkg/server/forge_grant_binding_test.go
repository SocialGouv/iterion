package server

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The pull-request binding of a publish grant (#3130). A repo-bound schedule's
// grant carries the workflow dispatch and nothing else: every surface that
// acts on a pull request — the review, the merge-gate commit status, the
// pull-request read, the delivery preflight, the server's gate repair —
// refuses it typed, before any forge call, while the dispatch it was minted
// for still goes through and a pull-request launch's grant keeps every power
// it had. Each witness mints its grant through the production mint
// (scheduledDispatchGrant, injectForgePublishVars) from the shipped bots'
// manifests, and drives the production handler through the full mux.

// recordingPublishForge stands behind the publish surface's two forge seams —
// the review client and the gate client — and records every call, so "before
// any forge call" covers them as well as the fake GitHub, which records every
// request the real clients make.
type recordingPublishForge struct {
	mu      sync.Mutex
	headSHA string
	calls   []string
}

func (f *recordingPublishForge) note(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *recordingPublishForge) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *recordingPublishForge) CreatePullReview(_ context.Context, repo string, number int, _ forge.NewReview) (forge.ReviewResult, error) {
	f.note("review " + repo + "#" + strconv.Itoa(number))
	return forge.ReviewResult{URL: "https://forge.example/" + repo + "/pull/" + strconv.Itoa(number) + "#review-1", Verified: true}, nil
}

func (f *recordingPublishForge) GetPullRequest(_ context.Context, repo string, number int) (forge.PullRef, error) {
	f.note("read " + repo + "#" + strconv.Itoa(number))
	return forge.PullRef{Number: number, HeadSHA: f.headSHA, HeadRepoFullName: repo, State: "open", SourceBranch: "feat/x", TargetBranch: "main"}, nil
}

func (f *recordingPublishForge) SetCommitStatus(_ context.Context, _, sha string, st forge.CommitStatus) error {
	f.note("status " + st.Context + "=" + string(st.State) + "@" + sha)
	return nil
}

func (f *recordingPublishForge) ListCommitStatuses(context.Context, string, string) ([]forge.CommitStatus, error) {
	f.note("list statuses")
	return nil, nil
}

const bindingHeadSHA = "cafe1234cafe1234cafe1234cafe1234cafe1234"

// bindingWorld is the dispatch world with the publish surface's forge seams
// recording. backend "valkey" swaps the grant registry for the Valkey store
// BEFORE anything is minted, so the binding crosses the cloud twin's
// serialization.
func bindingWorld(t *testing.T, backend string) (*dispatchWorld, *recordingPublishForge) {
	t.Helper()
	w := newDispatchWorld(t)
	if backend == "valkey" {
		_, rdb := newTestRedis(t)
		w.s.forgePublishTokens = newValkeyForgePublishTokenStore(rdb, nil)
	}
	rec := &recordingPublishForge{headSHA: bindingHeadSHA}
	w.s.forgeReviewClientFor = func(context.Context, forge.Connection) (forge.ReviewClient, error) { return rec, nil }
	w.s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return rec, nil }
	// The reviewer bookkeeping after a publish is best-effort and detached;
	// it is not what these witnesses observe.
	w.s.forgeReviewerAssignerFor = func(context.Context, forge.Connection) forge.ReviewerAssigner { return nil }
	w.s.forgeReviewRequestWithdrawerFor = func(context.Context, forge.Connection) forge.ReviewRequestWithdrawer { return nil }
	return w, rec
}

// prURL is a pull request of the world's repository on the fake forge's host.
func (w *dispatchWorld) prURL() string { return w.f.srv.URL + "/acme/widgets/pull/7" }

// serve sends one request through the server's FULL handler, bearing token as
// a run does, and decodes the JSON answer.
func (w *dispatchWorld) serve(t *testing.T, method, target, token, body string) (int, map[string]any, string) {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("X-Iterion-Run", token)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	w.s.Handler().ServeHTTP(rec, r)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

// assertNothingReachedTheForge is the "refused BEFORE any forge call" oracle
// over both forges: the fake GitHub and the recording seams.
func assertNothingReachedTheForge(t *testing.T, w *dispatchWorld, rec *recordingPublishForge) {
	t.Helper()
	w.assertForgeUntouched(t)
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("the publish surface's forge was called %v — the refusal must come before any forge call", calls)
	}
}

// assertBindingRefusal checks the typed answer every pull-request endpoint
// gives a grant bound to no pull request.
func assertBindingRefusal(t *testing.T, what string, code int, out map[string]any, raw string) {
	t.Helper()
	if code != http.StatusForbidden || out["error_code"] != grantNoPullRequestReason {
		t.Fatalf("%s with a schedule grant = %d %s, want 403 error_code %s", what, code, raw, grantNoPullRequestReason)
	}
}

// A schedule grant posting a review — with inline comments, the shape a
// reviewer bot sends — is refused typed, and neither forge hears of it.
// Both registries: the binding must survive the Valkey record too.
//
// Mutations witnessed: the pullRequestGrant call in handleForgePublishReview
// replaced by the raw lookup → the review reaches the recording forge → red
// (and the source scan below reddens); NoPullRequest dropped from
// scheduledDispatchGrant's grant → red; the json tag of NoPullRequest set to
// "-" → the valkey arm reads the grant back unbound → red.
func TestScheduleGrant_AReviewIsRefusedBeforeAnyForgeCall(t *testing.T) {
	for _, backend := range []string{"memory", "valkey"} {
		t.Run(backend, func(t *testing.T) {
			w, rec := bindingWorld(t, backend)
			token, _ := w.scheduleGrant(t, "workflow-kick", "run-kick-review", store.RunTrustDefault)
			body, _ := json.Marshal(map[string]any{
				"pr_url":  w.prURL(),
				"summary": "a review the schedule grant must not post",
				"comments": []map[string]any{
					{"path": "main.go", "line": 3, "body": "finding"},
				},
			})
			code, out, raw := w.serve(t, http.MethodPost, "/api/v1/forge/publish-review", token, string(body))
			assertBindingRefusal(t, "publish-review", code, out, raw)
			assertNothingReachedTheForge(t, w, rec)
		})
	}
}

// The merge-gate commit status: a schedule grant asking for a SUCCESS on the
// required check — the forgery the binding exists to prevent — is refused
// typed, and no status is posted on any head.
//
// Mutation witnessed: the pullRequestGrant call in handleForgePublishReview
// replaced by the raw lookup → a success status lands on the head → red.
func TestScheduleGrant_TheMergeGateStatusIsRefusedBeforeAnyForgeCall(t *testing.T) {
	w, rec := bindingWorld(t, "memory")
	token, _ := w.scheduleGrant(t, "workflow-kick", "run-kick-gate", store.RunTrustDefault)
	body, _ := json.Marshal(map[string]any{
		"pr_url":  w.prURL(),
		"summary": "gate",
		"gate":    map[string]any{"enabled": true, "context": "revi/review", "blocking_count": 0, "total_findings": 0, "audited_sha": bindingHeadSHA},
	})
	code, out, raw := w.serve(t, http.MethodPost, "/api/v1/forge/publish-review", token, string(body))
	assertBindingRefusal(t, "a merge-gate status", code, out, raw)
	assertNothingReachedTheForge(t, w, rec)
}

// The read half of the grant is a pull-request surface too.
//
// Mutation witnessed: the pullRequestGrant call in handleForgePullRequest
// replaced by the raw lookup → the read reaches the forge and answers 200 →
// red.
func TestScheduleGrant_APullRequestReadIsRefused(t *testing.T) {
	w, rec := bindingWorld(t, "memory")
	token, _ := w.scheduleGrant(t, "workflow-kick", "run-kick-read", store.RunTrustDefault)
	code, out, raw := w.serve(t, http.MethodGet, "/api/v1/forge/pull-request?pr_url="+url.QueryEscape(w.prURL()), token, "")
	assertBindingRefusal(t, "a pull-request read", code, out, raw)
	assertNothingReachedTheForge(t, w, rec)
}

// The delivery preflight answers about a push onto a pull request.
//
// Mutation witnessed: the pullRequestGrant call in
// handleForgeDeliveryPreflight replaced by the raw lookup → the preflight
// reads on (and answers another status) → red on the code.
func TestScheduleGrant_ADeliveryPreflightIsRefused(t *testing.T) {
	w, rec := bindingWorld(t, "memory")
	token, _ := w.scheduleGrant(t, "workflow-kick", "run-kick-preflight", store.RunTrustDefault)
	body, _ := json.Marshal(map[string]string{"pr_url": w.prURL(), "credential_sha256": strings.Repeat("ab", 32)})
	code, out, raw := w.serve(t, http.MethodPost, "/api/v1/forge/delivery-preflight", token, string(body))
	assertBindingRefusal(t, "a delivery preflight", code, out, raw)
	assertNothingReachedTheForge(t, w, rec)
}

// The server's own gate repair: a scheduled run whose vars name a pull
// request and the repository's gate context (a schedule row's vars are the
// tenant's to write) dies, and the reconciler would paint the required check
// on that head with the run's grant. A grant bound to no pull request speaks
// for no head: nothing is read, nothing is posted.
//
// Mutation witnessed: the pullRequestGrant call in reconcileGateForRunID
// replaced by the raw lookup → the reconciler reads the pull request and
// posts its synthetic failure → red.
func TestScheduleGrant_TheGateRepairPostsNothingWithIt(t *testing.T) {
	w, rec := bindingWorld(t, "memory")
	vars, minted, err := w.s.scheduledDispatchGrant(context.Background(), cloudsched.ScheduledBot{
		ID: "sch-gate-repair", TenantID: "team1", BotID: "workflow-kick", RepoIntegrationID: "integ1",
	}, w.manifestOf(t, "workflow-kick"), map[string]string{
		"workflow": "renovate.yml", "pr_url": w.prURL(), "gate_context": "revi/review", "head_sha": bindingHeadSHA,
	})
	if err != nil || minted == "" {
		t.Fatalf("scheduledDispatchGrant = (%q, %v), want a grant — the fixture must hold one to be refused", minted, err)
	}
	w.holdRun(t, "run-kick-dead", vars, store.RunTrustDefault)
	if err := w.s.reconcileGateForRun(context.Background(), terminalEvent("run-kick-dead")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertNothingReachedTheForge(t, w, rec)
}

// A pull-request launch that pins a schedule grant would publish with a grant
// every pull-request surface refuses: the launch is refused instead, typed,
// and the grant is not marked shared.
//
// Mutation witnessed: the pullRequestGrant check in injectForgePublishVars'
// pin branch removed → the launch is accepted on the schedule grant → red.
func TestScheduleGrant_APullRequestLaunchCannotPinIt(t *testing.T) {
	w, _ := bindingWorld(t, "memory")
	token, _ := w.scheduleGrant(t, "workflow-kick", "run-kick-pinned", store.RunTrustDefault)
	_, minted, err := w.s.injectForgePublishVars(context.Background(), "team1", "conn1", "review-pr",
		map[string]string{"pr_url": w.prURL(), forgePublishVarToken: token}, nil, store.RunTrustDefault)
	if !errors.Is(err, errForgePublishGrantUnavailable) || !errors.Is(err, errGrantNoPullRequest) || minted != "" {
		t.Fatalf("a pull-request launch pinning a schedule grant = (%q, %v), want it refused with %v and %v", minted, err, errForgePublishGrantUnavailable, errGrantNoPullRequest)
	}
	if g, ok := w.s.forgePublishTokens.lookup(token); !ok || g.Shared {
		t.Fatalf("the schedule grant after the refused pin = (%+v, %v), want it live and not shared", g, ok)
	}
}

// The same schedule grant, refused everywhere above, still dispatches its
// workflow: the binding narrows what the grant reaches, it does not void it.
//
// Mutation witnessed: handleForgeDispatchWorkflow resolving its grant through
// pullRequestGrant → the dispatch is refused → red.
func TestScheduleGrant_StillDispatchesItsWorkflow(t *testing.T) {
	w, rec := bindingWorld(t, "memory")
	token, _ := w.scheduleGrant(t, "workflow-kick", "run-kick-dispatch", store.RunTrustDefault)
	code, out, raw := w.serve(t, http.MethodPost, "/api/v1/forge/publish-review", token,
		`{"pr_url":"`+w.prURL()+`","summary":"refused first"}`)
	assertBindingRefusal(t, "publish-review", code, out, raw)

	dcode, dout, draw := w.dispatch(t, token, "run-kick-dispatch", "acme/widgets", "renovate.yml")
	if dcode != http.StatusOK || !dout.Dispatched || dout.Ref != "main" {
		t.Fatalf("dispatch with the schedule grant = %d %s, want 200 dispatched on main", dcode, draw)
	}
	if _, dispatches, _ := w.f.snapshot(); len(dispatches) != 1 || dispatches[0] != (dispatchRecord{Repo: "acme/widgets", Workflow: "renovate.yml", Ref: "main"}) {
		t.Fatalf("forge dispatches = %+v, want exactly renovate.yml on acme/widgets@main", dispatches)
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("the publish surface's forge was called %v by a dispatch", calls)
	}
}

// A pull-request launch's grant is unchanged: minted by the production
// pull-request mint for the shipped reviewer, it posts its review and its
// merge-gate status, and reads its pull request.
//
// Mutation witnessed: pullRequestGrant refusing every grant (the binding
// check inverted) → the review is refused → red.
func TestPullRequestGrant_KeepsItsReviewStatusAndRead(t *testing.T) {
	w, rec := bindingWorld(t, "memory")
	vars, minted, err := w.s.injectForgePublishVars(context.Background(), "team1", "conn1", "review-pr",
		map[string]string{"pr_url": w.prURL()}, nil, store.RunTrustDefault)
	if err != nil || minted == "" || vars[forgePublishVarToken] != string(minted) {
		t.Fatalf("PR grant for review-pr = (%q, %v), want one minted", minted, err)
	}
	if g, _ := w.s.forgePublishTokens.lookup(string(minted)); g.NoPullRequest {
		t.Fatalf("a pull-request launch's grant is bound to no pull request: %+v", g)
	}
	body, _ := json.Marshal(map[string]any{
		"pr_url":  w.prURL(),
		"summary": "review",
		"gate":    map[string]any{"enabled": true, "context": "revi/review", "blocking_count": 0, "total_findings": 0, "audited_sha": bindingHeadSHA},
	})
	code, out, raw := w.serve(t, http.MethodPost, "/api/v1/forge/publish-review", string(minted), string(body))
	if code != http.StatusOK || out["published"] != true || out["gate_posted"] != true {
		t.Fatalf("publish-review with a PR grant = %d %s, want 200 published with its gate posted", code, raw)
	}
	calls := rec.snapshot()
	wantStatus := "status revi/review=success@" + bindingHeadSHA
	if !containsCall(calls, "review acme/widgets#7") || !containsCall(calls, wantStatus) {
		t.Fatalf("forge calls = %v, want the review and %q", calls, wantStatus)
	}
	code, out, raw = w.serve(t, http.MethodGet, "/api/v1/forge/pull-request?pr_url="+url.QueryEscape(w.prURL()), string(minted), "")
	if code != http.StatusOK || out["open"] != true || out["head_sha"] != bindingHeadSHA {
		t.Fatalf("pull-request read with a PR grant = %d %s, want 200 open on %s", code, raw, bindingHeadSHA)
	}
}

func containsCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

// The server's other pull-request readers of a run's grant refuse it too: the
// auto-fix lane and the PR notices resolve through the same binding. Grants
// here are registered by hand with the record the schedule mint writes (its
// shape is pinned by the witnesses above).
//
// Mutation witnessed: the pullRequestGrant call in gateNoticeTarget replaced
// by the raw lookup → the DLQ notice is commented on the pull request → red.
func TestScheduleGrant_ThePullRequestNoticesSayNothingWithIt(t *testing.T) {
	s, runID, gc, c := dlqGateFixture(t, nil)
	registerPublishToken(t, s, "tok-gate", ForgePublishGrant{
		TeamID: "team1", ConnectionID: "conn1", Repo: "o/r", Bot: "workflow-kick",
		Capabilities: []string{publishCapDispatchWorkflow}, NoPullRequest: true,
	})
	run, err := s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	s.noticeGateDLQParked(context.Background(), run)
	if len(c.bodies) != 0 || gc.setCalls != 0 {
		t.Fatalf("a schedule grant commented %v and posted %d status(es) on a pull request", c.bodies, gc.setCalls)
	}
}

// TestEveryPullRequestSurfaceResolvesItsGrantThroughTheBinding pins the
// chokepoint: in the package's production code the grant registry's raw
// lookup is called only by pullRequestGrant itself and by the sites listed
// below, each for its stated reason, and the binding field is read nowhere
// else. A pull-request surface added later that reads the registry directly
// reddens here, before it can act on a grant bound to no pull request.
//
// Mutation witnessed: any of the pullRequestGrant call sites (publish-review,
// pull-request read, delivery preflight, reconciler, auto-fix, notice target)
// replaced by the raw lookup → red, naming the function.
func TestEveryPullRequestSurfaceResolvesItsGrantThroughTheBinding(t *testing.T) {
	rawLookupAllowed := map[string]string{
		"pullRequestGrant":            "the binding itself",
		"handleForgeDispatchWorkflow": "the one surface a grant bound to no pull request serves — gated on its dispatch capability",
		"injectForgePublishVars":      "the tenant door of a pinned token; the binding is asked of pullRequestGrant right after it",
		"withoutCardGrant":            "the tenant door of a board card's token, which is dropped whatever its binding",
		"relaunchStillRunning":        "reads Deferred, which only a pull-request surface writes",
	}
	// The binding is DECIDED in pullRequestGrant alone; the Valkey store
	// keeps it in the key namespace, which is storage, not a decision.
	fieldReadAllowed := map[string]string{
		"pullRequestGrant":        "the binding itself",
		"forgePublishRegisterKey": "the Valkey key namespace an unbound grant is written under, which older builds do not read",
		"bindStoredGrant":         "a record read from that namespace is unbound whatever its body says",
	}
	mustBind := []string{
		"handleForgePublishReview", "handleForgePullRequest", "handleForgeDeliveryPreflight",
		"reconcileGateForRunID", "autofixForRunID", "gateNoticeTarget", "injectForgePublishVars",
	}

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var offenders, fieldReads []string
	seen := map[string]bool{}
	binds := map[string]bool{}
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		files++
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			owner := fn.Name.Name
			seen[owner] = true
			ast.Inspect(fn, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if sel.Sel.Name == "pullRequestGrant" {
						binds[owner] = true
					}
					if inner, ok := sel.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "lookup" && inner.Sel.Name == "forgePublishTokens" {
						if _, ok := rawLookupAllowed[owner]; !ok {
							offenders = append(offenders, name+":"+strconv.Itoa(fset.Position(x.Pos()).Line)+" ("+owner+")")
						}
					}
				case *ast.SelectorExpr:
					if _, ok := fieldReadAllowed[owner]; x.Sel.Name == "NoPullRequest" && !ok {
						fieldReads = append(fieldReads, name+":"+strconv.Itoa(fset.Position(x.Pos()).Line)+" ("+owner+")")
					}
				}
				return true
			})
		}
	}
	if files < 100 {
		t.Fatalf("scanned %d files — the scan is not reading the package", files)
	}
	var stale []string
	for fn := range rawLookupAllowed {
		if !seen[fn] {
			stale = append(stale, fn)
		}
	}
	for fn := range fieldReadAllowed {
		if !seen[fn] {
			stale = append(stale, fn)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("the allowlist names functions that no longer exist: %v — re-point it at the sites that read the registry now", stale)
	}
	if len(offenders) > 0 {
		t.Fatalf("the grant registry is read raw outside the binding — a pull-request surface must resolve its grant through pullRequestGrant, or be listed here with its reason:\n  %s", strings.Join(offenders, "\n  "))
	}
	if len(fieldReads) > 0 {
		t.Fatalf("NoPullRequest is read outside pullRequestGrant — the binding is decided in one place:\n  %s", strings.Join(fieldReads, "\n  "))
	}
	for _, fn := range mustBind {
		if !binds[fn] {
			t.Errorf("%s acts on a pull request with a grant but does not resolve it through pullRequestGrant", fn)
		}
	}
}
