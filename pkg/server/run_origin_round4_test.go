package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
)

// crossStoreFixture: run r in the primary store (birth B, marked PRIMARY,
// "primary bytes" logged) and run r in a foreign store of the iterion home
// (birth A, marked FOREIGN) — one ID, two runs.
func crossStoreFixture(t *testing.T, fx *gateFixture) (xdir, foreign, primary string) {
	t.Helper()
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "PRIMARY", store.LegacyValueFormat, store.RunStatusFinished, 1))
	writeRunLog(t, fx.srv.cfg.StoreDir, "r", "primary bytes\n")
	xs, xdir := newCrossStore(t)
	mustWS(t, seedWSOriginRun(ctx, xs, "r", "FOREIGN", store.LegacyValueFormat, store.RunStatusFinished, 1))
	return xdir, birthOf(t, xs, "r"), birthOf(t, fx.native, "r")
}

// X5-1: a `?store=` naming the server's own store — the desktop's run links
// name the run's store even when the daemon serves it — is a request for the
// primary run. Named directly or through a symlink, every run route answers it
// as the bare request: the expected birth compared (a stale one refused 409
// like the bare request's, never 400 store_unsupported), the cancel a primary
// request (no cross_store_readonly), a GET the same bytes, and the run socket
// serves the run.
func TestRunOriginGateOwnStoreParamIsThePrimary(t *testing.T) {
	fx := newGateFixture(t, true)
	ctx := t.Context()
	t.Setenv("ITERION_HOME", fx.srv.cfg.StoreDir)
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "PRIMARY", store.LegacyValueFormat, store.RunStatusFinished, 1))
	writeRunLog(t, fx.srv.cfg.StoreDir, "r", "primary bytes\n")
	current := birthOf(t, fx.native, "r")
	link := filepath.Join(t.TempDir(), "store-link")
	mustWS(t, os.Symlink(fx.srv.cfg.StoreDir, link))
	for _, dir := range []string{fx.srv.cfg.StoreDir, link} {
		q := "?store=" + url.QueryEscape(dir)
		for _, rt := range classRoutes(fx.srv, "r") {
			method, path := rt[0], rt[1]
			bare := gateDo(t, fx.hs.URL, method, path, staleBirth, "header")
			named := gateDo(t, fx.hs.URL, method, path+q, staleBirth, "header")
			if named.status != bare.status || named.code != bare.code {
				t.Fatalf("%s %s%s with a stale birth: %d %s %.120s, the bare request %d %s", method, path, q, named.status, named.code, named.body, bare.status, bare.code)
			}
		}
		for _, c := range []struct{ method, path string }{
			{http.MethodGet, "/api/runs/r"},
			{http.MethodGet, "/api/runs/r/log"},
			{http.MethodGet, "/api/runs/r/artifacts"},
			{http.MethodPost, "/api/runs/r/cancel"},
			{http.MethodGet, "/api/runs/r/watching"},
		} {
			for _, birth := range []string{"", current} {
				bare := gateDo(t, fx.hs.URL, c.method, c.path, birth, "header")
				named := gateDo(t, fx.hs.URL, c.method, c.path+q, birth, "header")
				if named.status != bare.status || named.code != bare.code || c.method == http.MethodGet && named.body != bare.body {
					t.Fatalf("%s %s%s (birth %q): %d %.160s, the bare request %d %.160s", c.method, c.path, q, birth, named.status, named.body, bare.status, bare.body)
				}
			}
		}
		ws := dialGate(t, fx.hs, "/api/ws/runs/r"+q+"&expected_birth="+current)
		writeJSONMessage(t, ws, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s"})
		if snap := snapshotOf(t, readEnvelope(t, ws)); snap.Run.WorkflowName != "PRIMARY" || snap.Run.BirthID != current {
			t.Fatalf("the run socket named %s: %q birth %q", q, snap.Run.WorkflowName, snap.Run.BirthID)
		}
	}
}

// X5-1: births are compared in the server's own store only. A request naming
// a foreign store with an expectation — in the header or the query, the
// foreign run's birth or the primary run's — is refused 400 store_unsupported
// on every run route before any handler, and the run socket is accepted then
// closed with that reason: never compared with the primary run of the ID.
func TestRunOriginGateForeignStoreRefusesAnExpectation(t *testing.T) {
	fx := newGateFixture(t, true)
	xdir, foreign, primary := crossStoreFixture(t, fx)
	q := "?store=" + url.QueryEscape(xdir)
	for _, rt := range classRoutes(fx.srv, "r") {
		for _, birth := range []string{foreign, primary} {
			for _, how := range []string{"header", "query"} {
				if got := gateDo(t, fx.hs.URL, rt[0], rt[1]+q, birth, how); got.status != http.StatusBadRequest || got.code != errCodeStoreUnsupported {
					t.Fatalf("%s %s%s, birth %s in the %s: %d %.160s, want 400 %s", rt[0], rt[1], q, birth, how, got.status, got.body, errCodeStoreUnsupported)
				}
			}
		}
	}
	c := dialGate(t, fx.hs, "/api/ws/runs/r"+q+"&expected_birth="+foreign)
	if ce, texts := awaitWSClose(t, c, 10*time.Second); len(texts) != 0 || !strings.HasPrefix(ce.Text, errCodeStoreUnsupported+": ") {
		t.Fatalf("the run socket: frames %v close %q, want the %s close alone", texts, ce.Text, errCodeStoreUnsupported)
	}
}

// X5-1: a request naming a foreign store without an expectation is outside
// the guarantee and answered as the gate-less server answers it: no run route
// refuses it for its store; the snapshot reads the foreign run (the
// cross-store proxy) while the log still reads the primary run (the
// pre-existing class of a panel reading the primary store in a foreign view),
// a write is refused cross_store_readonly; and on the authenticated chain the
// run is scoped against the primary store, where a foreign-only run is not
// found.
func TestRunOriginGateForeignStoreWithoutExpectationIsUngated(t *testing.T) {
	fx := newGateFixture(t, true)
	xdir, _, _ := crossStoreFixture(t, fx)
	q := "?store=" + url.QueryEscape(xdir)
	for _, rt := range classRoutes(fx.srv, "r") {
		if got := gateDo(t, fx.hs.URL, rt[0], rt[1]+q, "", ""); got.code == errCodeStoreUnsupported {
			t.Fatalf("%s %s%s without an expectation: %d %.160s", rt[0], rt[1], q, got.status, got.body)
		}
	}
	if got := gateDo(t, fx.hs.URL, http.MethodGet, "/api/runs/r"+q, "", ""); got.status != http.StatusOK || !strings.Contains(got.body, `"workflow_name":"FOREIGN"`) {
		t.Fatalf("the foreign snapshot: %d %.200s", got.status, got.body)
	}
	if got := gateDo(t, fx.hs.URL, http.MethodGet, "/api/runs/r/log"+q, "", ""); got.status != http.StatusOK || got.body != "primary bytes\n" {
		t.Fatalf("the log of a foreign view: %d %q, want the primary run's", got.status, got.body)
	}
	if got := gateDo(t, fx.hs.URL, http.MethodPost, "/api/runs/r/cancel"+q, "", ""); got.status != http.StatusConflict || !strings.Contains(got.body, "cross_store_readonly") {
		t.Fatalf("a cancel in a foreign view: %d %.200s, want 409 cross_store_readonly", got.status, got.body)
	}

	srv, _ := newRunScopeServer(t)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	xs, xdir2 := newCrossStore(t)
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, xs, "fx", "FOREIGN", store.LegacyValueFormat, store.RunStatusFinished, 1))
	run, err := xs.LoadRun(ctx, "fx")
	mustWS(t, err)
	run.TenantID = "tenant-A"
	mustWS(t, xs.SaveRun(ctx, run))
	token, _, err := srv.signer.IssueAccess(caller("u-both", "tenant-B"))
	mustWS(t, err)
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/api/runs/fx?store="+url.QueryEscape(xdir2), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	mustWS(t, err)
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a foreign-only run on the authenticated chain: %d %.200s, want 404 (scoped against the primary store)", resp.StatusCode, raw)
	}
}

// R4-B1: a live log buffer is served only to a request bound to the run it
// was created for. A predecessor's buffer a subscription still holds is not
// served to a request expecting the successor — on GET /log nor on a socket
// — which gets the successor's persisted log; the current run's own buffer
// is served. (The opposite direction — a successor's buffer appearing after a
// request admitted its predecessor — is TestRunOriginGateLiveLogBufferRefusesASuccessor.)
func TestRunOriginGateLogBufferServedOnlyToItsRun(t *testing.T) {
	fx := newGateFixture(t, true)
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 0))
	predecessor := birthOf(t, fx.native, "r")
	mustWS(t, replaceWSOriginRun(ctx, fx.native, "r", "B", store.LegacyValueFormat, store.RunStatusFinished, 0))
	writeRunLog(t, fx.srv.cfg.StoreDir, "r", "successor persisted\n")
	current := birthOf(t, fx.native, "r")
	remove := fx.srv.runs.InstallLogBufferForTest("r", predecessor, []byte("predecessor buffer\n"))
	t.Cleanup(remove)

	got := gateDo(t, fx.hs.URL, http.MethodGet, "/api/runs/r/log", current, "header")
	if got.status != http.StatusOK || strings.Contains(got.body, "predecessor") || !strings.Contains(got.body, "successor persisted") {
		t.Fatalf("GET /log expecting the successor: %d %q, want its persisted log", got.status, got.body)
	}
	c := dialGate(t, fx.hs, "/api/ws/runs/r?expected_birth="+current)
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l"})
	env := readEnvelopeWithin(t, c, 10*time.Second, wsTypeLogChunk, wsTypeLogTerminated)
	if env.Type != wsTypeLogChunk {
		t.Fatalf("socket logs expecting the successor: %s before any log", env.Type)
	}
	first := decodeLogChunk(t, env)
	if first.Offset != 0 || first.Text == "" || !strings.HasPrefix("successor persisted\n", first.Text) {
		t.Fatalf("socket logs expecting the successor: first chunk at %d %q, want its persisted log", first.Offset, first.Text)
	}
	if text := collectLogText(t, c, first.Text, "successor persisted\n", 10*time.Second); text != "successor persisted\n" {
		t.Fatalf("socket logs expecting the successor: %q", text)
	}

	remove()
	own := fx.srv.runs.InstallLogBufferForTest("r", current, []byte("successor live\n"))
	t.Cleanup(own)
	if got := gateDo(t, fx.hs.URL, http.MethodGet, "/api/runs/r/log", current, "header"); got.status != http.StatusOK || got.body != "successor live\n" {
		t.Fatalf("GET /log on the run's own live buffer: %d %q", got.status, got.body)
	}
}

// mongoLikeGuard answers a run another team owns the way the Mongo store
// does: its tenant filter finds nothing — store.ErrRunNotFound.
type mongoLikeGuard struct {
	*store.FilesystemRunStore
	runTenant string
}

func (g *mongoLikeGuard) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if tid, ok := store.TenantFromContext(ctx); ok && tid != g.runTenant {
		return nil, fmt.Errorf("store/mongo: run %s: %w", id, store.ErrRunNotFound)
	}
	return g.FilesystemRunStore.LoadRun(ctx, id)
}

// R4-A1: a run the context resolver cannot find is not lost — a
// tenant-scoped store answers ErrRunNotFound for a run of another team the
// sender sees elsewhere. Only a birth mismatch or a deletion is.
func TestAssistantContextNotFoundIsNoLoss(t *testing.T) {
	srv, guard := newRunScopeServer(t)
	srv.runs = newTestRunviewService(t, srv.cfg.StoreDir, runview.WithStore(&mongoLikeGuard{FilesystemRunStore: guard.FilesystemRunStore, runTenant: "tenant-A"}))
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	birth := birthOf(t, guard.FilesystemRunStore, "run-1")
	token, _, err := srv.signer.IssueAccess(caller("u-both", "tenant-B"))
	mustWS(t, err)
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/assistant/context/resolve", bytes.NewBufferString(`{"references":["run/run-1@`+birth+`"]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	mustWS(t, err)
	defer resp.Body.Close()
	var out assistantContextResolveResponse
	mustWS(t, json.NewDecoder(resp.Body).Decode(&out))
	if len(out.References) != 1 || out.References[0].Reason != "not_found" {
		t.Fatalf("a run of another team the caller can see: %+v, want not_found, never %s", out.References, wsErrRunOriginLost)
	}
}

// R4-A2: the mission reattach compares the assistant's birth before it
// answers: a stale birth is 409, a malformed one 400, the current one
// reattaches.
func TestAssistantMissionReattachChecksTheBirth(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "mission-target", "target", store.RunStatusFailedResumable)
	seedAssistantWatchChat(t, srv, "mission-assistant", store.RunStatusPausedWaitingHuman)
	birth := birthOf(t, srv.runs.RunStore(), "mission-assistant")
	now := time.Now().UTC()
	mustWS(t, srv.assistantWatches.CreateWatch(context.Background(), runwatch.Watch{ID: "mission-watch", OwnerID: "local", TargetRunID: "mission-target", AssistantRunID: "mission-assistant", Mode: runwatch.ModePropose, State: runwatch.WatchActive, CreatedAt: now, UpdatedAt: now}))
	post := func(b string) gateReply {
		return gatePost(t, hs.URL+"/api/runs/mission-target/assistant-missions", `{"invocation_key":"goal:b","watch_id":"mission-watch","assistant_run_birth":"`+b+`","ttl_seconds":600}`)
	}
	if got := post(birth); got.status != http.StatusOK {
		t.Fatalf("create: %d %s", got.status, got.body)
	}
	wantOriginLost(t, "reattach with a stale birth", post(staleBirth), "mission-assistant")
	if got := post("not-a-birth!"); got.status != http.StatusBadRequest || !strings.Contains(got.body, errCodeInvalidExpectedBirth) {
		t.Fatalf("reattach with a malformed birth: %d %s", got.status, got.body)
	}
	if got := post(birth); got.status != http.StatusOK {
		t.Fatalf("reattach with the current birth: %d %s", got.status, got.body)
	}
	// Once the assistant run is deleted, its birth is lost on a reattach; a
	// malformed birth is still refused before anything is read for it, never
	// answered as a loss.
	mustWS(t, srv.runs.RunStore().DeleteRun(t.Context(), "mission-assistant"))
	wantOriginLost(t, "reattach after the assistant run was deleted", post(birth), "mission-assistant")
	if got := post("not-a-birth!"); got.status != http.StatusBadRequest || !strings.Contains(got.body, errCodeInvalidExpectedBirth) {
		t.Fatalf("a malformed birth for a deleted assistant run: %d %s, want 400 %s", got.status, got.body, errCodeInvalidExpectedBirth)
	}
}

// R4-A3: only a request carrying an expected birth skips the out-of-band
// merge reconciliation; a run socket's snapshot, bound to its origin but
// expecting nothing, still reconciles (TestRunOriginGateCheckedSnapshotNeverReconciles
// pins the skip).
func TestRunSocketSnapshotStillReconciles(t *testing.T) {
	fx := newGateFixture(t, true)
	ctx := t.Context()
	repo := initRepo(t)
	head := gittest.Run(t, repo, "rev-parse", "HEAD")
	r, err := fx.native.CreateRun(ctx, "m", "wf", nil)
	mustWS(t, err)
	r.RepoRoot, r.WorkDir, r.FinalCommit, r.FinalBranch = repo, repo, head, "main"
	r.Worktree = true
	r.MergeStatus = store.MergeStatusPending
	r.Status = store.RunStatusFinished
	mustWS(t, fx.native.SaveRun(ctx, r))
	c := dialGate(t, fx.hs, "/api/ws/runs/m")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s"})
	snapshotOf(t, readEnvelopeWithin(t, c, 5*time.Second))
	if n := fx.store.mergeWrites.Load(); n != 1 {
		t.Fatalf("a run socket's snapshot called the merge writer %d times, want 1", n)
	}
}

// R5-3: an assistant watch created by a request the gate admitted on the
// target's expected birth reads the target's cursor without the merge
// reconciliation write, as a checked GET of its snapshot does; the unchecked
// control reaches the writer.
func TestAssistantWatchCheckedCreationNeverReconciles(t *testing.T) {
	fx := newGateFixture(t, true)
	ctx := t.Context()
	repo := initRepo(t)
	head := gittest.Run(t, repo, "rev-parse", "HEAD")
	r, err := fx.native.CreateRun(ctx, "m", "wf", nil)
	mustWS(t, err)
	r.RepoRoot, r.WorkDir, r.FinalCommit, r.FinalBranch = repo, repo, head, "main"
	r.Worktree = true
	r.MergeStatus = store.MergeStatusPending
	r.Status = store.RunStatusFinished
	mustWS(t, fx.native.SaveRun(ctx, r))
	seedAssistantWatchChat(t, fx.srv, "assistant", store.RunStatusPausedWaitingHuman)
	post := func(birth string) gateReply {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, fx.hs.URL+"/api/runs/m/assistant-watches", bytes.NewBufferString(`{"assistant_run_id":"assistant","mode":"diagnose"}`))
		req.Header.Set("Content-Type", "application/json")
		if birth != "" {
			req.Header.Set(expectedBirthHeader, birth)
		}
		resp, err := http.DefaultClient.Do(req)
		mustWS(t, err)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return gateReply{status: resp.StatusCode, body: string(raw)}
	}
	if got := post(birthOf(t, fx.native, "m")); got.status != http.StatusOK {
		t.Fatalf("checked creation: %d %s", got.status, got.body)
	}
	if n := fx.store.mergeWrites.Load(); n != 0 {
		t.Fatalf("a checked creation called the merge writer %d times", n)
	}
	if got := post(""); got.status != http.StatusOK {
		t.Fatalf("unchecked creation: %d %s", got.status, got.body)
	}
	if n := fx.store.mergeWrites.Load(); n != 1 {
		t.Fatalf("the unchecked control called the merge writer %d times, want 1: the fixture does not reach it", n)
	}
}

// R4-A5: a socket dialed with a malformed expectation is accepted, then
// closed naming invalid_expected_birth — a refused handshake would reach a
// browser as 1006 and be redialed.
func TestRunOriginGateMalformedSocketExpectationCloses(t *testing.T) {
	fx := newGateFixture(t, true)
	mustWS(t, seedWSOriginRun(t.Context(), fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 0))
	for _, path := range []string{"/api/ws/runs/r", "/api/ws/runs/r/shell", "/api/runs/r/browser/cdp"} {
		c, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(fx.hs.URL, "http")+path+"?expected_birth=not-a-birth!", nil)
		if err != nil {
			code := 0
			if resp != nil {
				code = resp.StatusCode
			}
			t.Fatalf("%s: handshake refused (%v, status %d), want an accepted then closed socket", path, err, code)
		}
		ce, texts := awaitWSClose(t, c, 10*time.Second)
		_ = c.Close()
		if len(texts) != 0 || !strings.HasPrefix(ce.Text, errCodeInvalidExpectedBirth+": ") {
			t.Fatalf("%s: frames %v close %q, want the %s close alone", path, texts, ce.Text, errCodeInvalidExpectedBirth)
		}
	}
}
