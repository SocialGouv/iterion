package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gateStore is the native filesystem store with the probes the run-origin
// gate tests read: a count of run loads, a one-shot hook run right after the
// Nth load returns, and a count of merge-reconciliation writes. Every read is
// forwarded unchanged with the caller's context, so the binding policy under
// test stays in the native store.
type gateStore struct {
	*store.FilesystemRunStore
	loads       atomic.Int32
	mergeWrites atomic.Int32
	mu          sync.Mutex
	afterLoadAt int32
	afterLoad   func() error
	hookErr     error
}

func (s *gateStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	n := s.loads.Add(1)
	r, err := s.FilesystemRunStore.LoadRun(ctx, id)
	s.mu.Lock()
	h := s.afterLoad
	fire := h != nil && n == s.afterLoadAt
	if fire {
		s.afterLoad = nil
	}
	s.mu.Unlock()
	if fire {
		if herr := h(); herr != nil {
			s.mu.Lock()
			s.hookErr = errors.Join(s.hookErr, herr)
			s.mu.Unlock()
		}
	}
	return r, err
}

func (s *gateStore) UpdateRunMergeIf(ctx context.Context, id string, upd store.RunMergeUpdate, expectedFrom []store.MergeStatus) (bool, error) {
	s.mergeWrites.Add(1)
	return s.FilesystemRunStore.UpdateRunMergeIf(ctx, id, upd, expectedFrom)
}

// afterLoadN arms h to run once, right after the nth run load from now
// returns (its result already read).
func (s *gateStore) afterLoadN(n int32, h func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.afterLoadAt = s.loads.Load() + n
	s.afterLoad = h
}

func (s *gateStore) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hookErr
}

// gateFixture serves the PRODUCTION chain (Server.Handler) over a real
// runview.Service on the gate store.
type gateFixture struct {
	srv     *Server
	hs      *httptest.Server
	native  *store.FilesystemRunStore
	store   *gateStore
	streams *testOwnedRunStreams
}

func newGateFixture(t *testing.T, disableAuth bool) *gateFixture {
	t.Helper()
	workDir := t.TempDir()
	storeDir := filepath.Join(workDir, ".iterion")
	srv := New(Config{WorkDir: workDir, StoreDir: storeDir, SkipProjectRegistration: true, DisableAuth: disableAuth}, iterlog.New(iterlog.LevelError, os.Stderr))
	if srv.runs == nil {
		t.Fatal("run console service not wired")
	}
	native, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &gateStore{FilesystemRunStore: native}
	srv.runs.Stop(t.Context())
	svc := newTestRunviewService(t, storeDir, runview.WithRecoveryPassive(), runview.WithStore(wrapped))
	streams := &testOwnedRunStreams{Source: svc.StreamSource()}
	runview.WithStreamSource(streams)(svc)
	srv.runs = svc
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		hs.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		if err := streams.Close(); err != nil {
			t.Errorf("join run streams: %v", err)
		}
	})
	return &gateFixture{srv: srv, hs: hs, native: native, store: wrapped, streams: streams}
}

func birthOf(t *testing.T, st store.RunStore, id string) string {
	t.Helper()
	r, err := st.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatalf("load %s: %v", id, err)
	}
	return r.BirthID
}

// clearBirth rewrites a run's run.json without its birth: a historical run.
func clearBirth(t *testing.T, storeDir, id string) {
	t.Helper()
	path := filepath.Join(storeDir, "runs", id, "run.json")
	raw, err := os.ReadFile(path)
	mustWS(t, err)
	var doc map[string]json.RawMessage
	mustWS(t, json.Unmarshal(raw, &doc))
	delete(doc, "birth_id")
	raw, err = json.Marshal(doc)
	mustWS(t, err)
	mustWS(t, os.WriteFile(path, raw, 0o600))
}

type gateReply struct {
	status int
	body   string
	code   string
	runID  string
}

// gateDo sends one request with the expectation in the header (how=header),
// the query (how=query) or nowhere (birth "").
func gateDo(t *testing.T, base, method, path, birth, how string) gateReply {
	t.Helper()
	target := base + path
	if birth != "" && how == "query" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		target += sep + expectedBirthParam + "=" + url.QueryEscape(birth)
	}
	req, err := http.NewRequest(method, target, nil)
	mustWS(t, err)
	if birth != "" && how == "header" {
		req.Header.Set(expectedBirthHeader, birth)
	}
	resp, err := http.DefaultClient.Do(req)
	mustWS(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := gateReply{status: resp.StatusCode, body: string(raw)}
	var env struct {
		Code  string `json:"error_code"`
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(raw, &env)
	out.code, out.runID = env.Code, env.RunID
	return out
}

func wantOriginLost(t *testing.T, label string, got gateReply, runID string) {
	t.Helper()
	if got.status != http.StatusConflict || got.code != wsErrRunOriginLost || got.runID != runID {
		t.Fatalf("%s: %d %s, want 409 %s naming %s", label, got.status, got.body, wsErrRunOriginLost, runID)
	}
}

// classRoutes lists every registered run-wildcard route as (method, path) for
// the given run ID, derived from the mux (routeIsRunWildcard), never from the
// production table.
func classRoutes(srv *Server, runID string) [][2]string {
	var out [][2]string
	for _, route := range srv.mux.Routes() {
		if !routeIsRunWildcard(route.Pattern) {
			continue
		}
		method := route.Method
		if method == "" {
			method = http.MethodGet
		}
		path := strings.ReplaceAll(samplePath(route.Pattern), "run-1", runID)
		path = strings.ReplaceAll(path, "{runID}", runID)
		out = append(out, [2]string{method, path})
	}
	return out
}

// T1: the gate stands in front of EVERY run-wildcard route (a stub replaces
// the handlers) and a probe registered under another wildcard spelling. A
// stale expectation in the header or the query is refused 409 and the stub
// never runs; no expectation passes with zero store reads; the current birth
// passes; `historical` passes only for a run with no birth. Routes outside the
// class — a fixed sibling, another resource's {id} — are never checked.
func TestRunOriginGateCoversEveryRunRoute(t *testing.T) {
	fx := newGateFixture(t, true)
	ctx := t.Context()
	fx.srv.mux.HandleFunc("GET /api/runs/{runID}/rva-probe", func(http.ResponseWriter, *http.Request) {})
	fx.srv.mux.HandleFunc("GET /api/teams/{id}/rva-probe", func(http.ResponseWriter, *http.Request) {})
	mustWS(t, seedWSOriginRun(ctx, fx.native, "run-1", "A", store.LegacyValueFormat, store.RunStatusFinished, 0))
	stale := birthOf(t, fx.native, "run-1")
	mustWS(t, replaceWSOriginRun(ctx, fx.native, "run-1", "B", store.LegacyValueFormat, store.RunStatusFinished, 0))
	current := birthOf(t, fx.native, "run-1")
	if stale == "" || current == "" || stale == current {
		t.Fatalf("fixture births %q → %q", stale, current)
	}
	mustWS(t, seedWSOriginRun(ctx, fx.native, "hist", "H", store.LegacyValueFormat, store.RunStatusFinished, 0))
	clearBirth(t, fx.srv.cfg.StoreDir, "hist")

	var called atomic.Int32
	var inClass atomic.Bool
	inClass.Store(true)
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		if inClass.Load() && (r.Header.Get(expectedBirthHeader) != "" || r.URL.Query().Has(expectedBirthParam)) {
			t.Errorf("%s %s reached its handler still carrying the expectation", r.Method, r.URL)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	hs := httptest.NewServer(fx.srv.authMiddleware(fx.srv.runOriginGate(stub)))
	t.Cleanup(hs.Close)

	routes := classRoutes(fx.srv, "run-1")
	if len(routes) < 70 {
		t.Fatalf("only %d run-wildcard routes found", len(routes))
	}
	probe := false
	for _, rt := range routes {
		method, path := rt[0], rt[1]
		probe = probe || strings.HasSuffix(path, "/rva-probe")
		label := method + " " + path
		for _, how := range []string{"header", "query"} {
			before := called.Load()
			wantOriginLost(t, label+" stale "+how, gateDo(t, hs.URL, method, path, stale, how), "run-1")
			if called.Load() != before {
				t.Fatalf("%s: a stale %s expectation reached the handler", label, how)
			}
		}
		before, loads := called.Load(), fx.store.loads.Load()
		if got := gateDo(t, hs.URL, method, path, "", ""); got.status != http.StatusNoContent || called.Load() != before+1 {
			t.Fatalf("%s without expectation: %d %s", label, got.status, got.body)
		}
		if n := fx.store.loads.Load() - loads; n != 0 {
			t.Fatalf("%s without expectation read the store %d times", label, n)
		}
		if got := gateDo(t, hs.URL, method, path, current, "header"); got.status != http.StatusNoContent {
			t.Fatalf("%s current birth: %d %s", label, got.status, got.body)
		}
		wantOriginLost(t, label+" historical on a born run", gateDo(t, hs.URL, method, path, expectedBirthHistorical, "query"), "run-1")
		histPath := strings.Replace(path, "/run-1", "/hist", 1)
		if got := gateDo(t, hs.URL, method, histPath, expectedBirthHistorical, "query"); got.status != http.StatusNoContent {
			t.Fatalf("%s historical: %d %s", method+" "+histPath, got.status, got.body)
		}
		wantOriginLost(t, label+" birth on a historical run", gateDo(t, hs.URL, method, histPath, current, "header"), "hist")
	}
	if !probe {
		t.Fatal("the {runID} probe route is not in the class")
	}
	inClass.Store(false)
	for _, path := range []string{"/api/runs/global-active", "/api/teams/run-1/rva-probe"} {
		before, loads := called.Load(), fx.store.loads.Load()
		if got := gateDo(t, hs.URL, http.MethodGet, path, stale, "header"); got.status != http.StatusNoContent || called.Load() != before+1 {
			t.Fatalf("%s is outside the class but answered %d %s", path, got.status, got.body)
		}
		if fx.store.loads.Load() != loads {
			t.Fatalf("%s is outside the class but read the store", path)
		}
	}
}

// The wire contract: a malformed or conflicting expectation is a 400, never
// a silent pass, and the CORS preflight lets the header through.
func TestRunOriginGateRefusesAMalformedExpectation(t *testing.T) {
	fx := newGateFixture(t, true)
	mustWS(t, seedWSOriginRun(t.Context(), fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 0))
	birth := birthOf(t, fx.native, "r")
	for _, tc := range []struct {
		name   string
		header []string
		query  string
	}{
		{"not a birth", []string{"A-B"}, ""},
		{"empty header", []string{""}, ""},
		{"twice", []string{birth, birth}, ""},
		{"header and query disagree", []string{birth}, "?expected_birth=" + expectedBirthHistorical},
		{"query twice", nil, "?expected_birth=" + birth + "&expected_birth=" + birth},
	} {
		req, _ := http.NewRequest(http.MethodGet, fx.hs.URL+"/api/runs/r"+tc.query, nil)
		for _, h := range tc.header {
			req.Header.Add(expectedBirthHeader, h)
		}
		resp, err := http.DefaultClient.Do(req)
		mustWS(t, err)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), errCodeInvalidExpectedBirth) {
			t.Fatalf("%s: %d %s, want 400 %s", tc.name, resp.StatusCode, raw, errCodeInvalidExpectedBirth)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, fx.hs.URL+"/api/runs/r?expected_birth="+birth, nil)
	req.Header.Set(expectedBirthHeader, birth)
	resp, err := http.DefaultClient.Do(req)
	mustWS(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agreeing header and query: %d", resp.StatusCode)
	}
	pre, _ := http.NewRequest(http.MethodOptions, fx.hs.URL+"/api/runs/r", nil)
	pre.Header.Set("Origin", fx.hs.URL)
	pre.Header.Set("Access-Control-Request-Headers", expectedBirthHeader)
	resp, err = http.DefaultClient.Do(pre)
	mustWS(t, err)
	resp.Body.Close()
	if allow := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(allow, expectedBirthHeader) {
		t.Fatalf("preflight allows %q, want %s", allow, expectedBirthHeader)
	}
}

// T2: the production chain on the DisableAuth path the local studio serves.
// run-1 is born A, then deleted, pruned and recreated B. Every class route
// with A's birth is refused 409 — the mutations included, and nothing runs:
// the run is still B, unchanged. The snapshot with B's birth answers B.
func TestRunOriginGateProductionChainDisableAuth(t *testing.T) {
	fx := newGateFixture(t, true)
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, fx.native, "run-1", "A", store.LegacyValueFormat, store.RunStatusFinished, 1))
	stale := birthOf(t, fx.native, "run-1")
	mustWS(t, replaceWSOriginRun(ctx, fx.native, "run-1", "B", store.LegacyValueFormat, store.RunStatusFinished, 1))
	current := birthOf(t, fx.native, "run-1")
	routes := classRoutes(fx.srv, "run-1")
	if len(routes) < 70 {
		t.Fatalf("only %d run-wildcard routes found", len(routes))
	}
	for _, rt := range routes {
		wantOriginLost(t, rt[0]+" "+rt[1], gateDo(t, fx.hs.URL, rt[0], rt[1], stale, "header"), "run-1")
	}
	after, err := fx.native.LoadRun(ctx, "run-1")
	mustWS(t, err)
	if after.BirthID != current || after.WorkflowName != "B" || after.Status != store.RunStatusFinished {
		t.Fatalf("a refused request changed the run: %+v", after)
	}
	got := gateDo(t, fx.hs.URL, http.MethodGet, "/api/runs/run-1", current, "header")
	var snap runview.RunSnapshot
	if got.status != http.StatusOK || json.Unmarshal([]byte(got.body), &snap) != nil || snap.Run.BirthID != current || snap.Run.WorkflowName != "B" {
		t.Fatalf("current birth: %d %s", got.status, got.body)
	}
}

// T3: on the authenticated chain the gate runs AFTER scopeRunByID: a caller
// who cannot see the run gets the 404 a missing run answers, whatever it
// expects (no existence oracle); a caller who can see it gets the 409.
func TestRunOriginGateRunsAfterTheRunScope(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	stale := "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	if birthOf(t, srv.runs.RunStore(), "run-1") == stale {
		t.Fatal("fixture birth collides with the stale one")
	}
	get := func(userID, teamID string) (int, string) {
		t.Helper()
		token, _, err := srv.signer.IssueAccess(caller(userID, teamID))
		mustWS(t, err)
		req, _ := http.NewRequest(http.MethodGet, hs.URL+"/api/runs/run-1", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(expectedBirthHeader, stale)
		resp, err := http.DefaultClient.Do(req)
		mustWS(t, err)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	if code, body := get("u-member-b", "tenant-B"); code != http.StatusNotFound {
		t.Fatalf("invisible run with a stale expectation: %d %s, want 404", code, body)
	}
	if code, body := get("u-both", "tenant-B"); code != http.StatusConflict || !strings.Contains(body, wsErrRunOriginLost) {
		t.Fatalf("visible run with a stale expectation: %d %s, want 409", code, body)
	}
}

// T9: ITERION_RUN_BIRTH_CHECK=0 admits every expectation unread; the check is
// on by default.
func TestRunOriginGateEscapeHatch(t *testing.T) {
	fx := newGateFixture(t, true)
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 0))
	stale := birthOf(t, fx.native, "r")
	mustWS(t, replaceWSOriginRun(ctx, fx.native, "r", "B", store.LegacyValueFormat, store.RunStatusFinished, 0))
	wantOriginLost(t, "default", gateDo(t, fx.hs.URL, http.MethodGet, "/api/runs/r", stale, "header"), "r")
	t.Setenv(runBirthCheckEnv, "0")
	if got := gateDo(t, fx.hs.URL, http.MethodGet, "/api/runs/r", stale, "header"); got.status != http.StatusOK || !strings.Contains(got.body, `"workflow_name":"B"`) {
		t.Fatalf("check off: %d %s, want the run served", got.status, got.body)
	}
}

// T10: birth_id is on the wire — the run's birth, and "" (present) for a
// historical run — on the snapshot, the run list and the children rows.
func TestRunBirthIsExposedOnSnapshotsAndRows(t *testing.T) {
	fx := newGateFixture(t, true)
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, fx.native, "born", "A", store.LegacyValueFormat, store.RunStatusFinished, 0))
	mustWS(t, seedWSOriginRun(ctx, fx.native, "hist", "H", store.LegacyValueFormat, store.RunStatusFinished, 0))
	clearBirth(t, fx.srv.cfg.StoreDir, "hist")
	born := birthOf(t, fx.native, "born")
	if _, err := fx.native.CreateChildRun(ctx, "kid", "K", "born", nil); err != nil {
		t.Fatal(err)
	}
	kid := birthOf(t, fx.native, "kid")
	if born == "" || kid == "" {
		t.Fatal("fixture runs have no birth")
	}
	for id, want := range map[string]string{"born": `"birth_id":"` + born + `"`, "hist": `"birth_id":""`} {
		got := gateDo(t, fx.hs.URL, http.MethodGet, "/api/runs/"+id, "", "")
		if got.status != http.StatusOK || !strings.Contains(got.body, want) {
			t.Fatalf("snapshot of %s: %d, want %s in %s", id, got.status, want, got.body)
		}
	}
	list := gateDo(t, fx.hs.URL, http.MethodGet, "/api/runs", "", "")
	for _, want := range []string{`"id":"born","birth_id":"` + born + `"`, `"id":"hist","birth_id":""`} {
		if !strings.Contains(list.body, want) {
			t.Fatalf("run list lacks %s: %s", want, list.body)
		}
	}
	if children := gateDo(t, fx.hs.URL, http.MethodGet, "/api/runs/born/children", "", ""); !strings.Contains(children.body, `"id":"kid","birth_id":"`+kid+`"`) {
		t.Fatalf("children rows lack the birth: %s", children.body)
	}
}

// dialGate dials a socket path on the fixture with an optional query.
func dialGate(t *testing.T, hs *httptest.Server, path string) *websocket.Conn {
	t.Helper()
	c, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http")+path, nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", path, err, code)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// T4: sockets. A run socket dialed with a stale expectation is accepted, sent
// run_origin_lost and closed scope_lost — no snapshot frame, no
// subscription. With the current birth it serves the run. The shell and CDP
// sockets get the scope_lost close alone.
func TestRunOriginGateSockets(t *testing.T) {
	fx := newGateFixture(t, true)
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 1))
	stale := birthOf(t, fx.native, "r")
	mustWS(t, replaceWSOriginRun(ctx, fx.native, "r", "B", store.LegacyValueFormat, store.RunStatusFinished, 1))
	current := birthOf(t, fx.native, "r")

	old := dialGate(t, fx.hs, "/api/ws/runs/r?expected_birth="+stale)
	writeJSONMessage(t, old, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s"})
	expectOriginLostClose(t, old, wsErrRunOriginLost)
	fx.streams.mu.Lock()
	subscribed := len(fx.streams.events)
	fx.streams.mu.Unlock()
	if subscribed != 0 {
		t.Fatalf("a refused socket subscribed %d times", subscribed)
	}

	cur := dialGate(t, fx.hs, "/api/ws/runs/r?expected_birth="+current)
	writeJSONMessage(t, cur, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s"})
	if snap := snapshotOf(t, readEnvelope(t, cur)); snap.Run.WorkflowName != "B" || snap.Run.BirthID != current {
		t.Fatalf("current socket served %q birth %q", snap.Run.WorkflowName, snap.Run.BirthID)
	}

	for _, path := range []string{"/api/ws/runs/r/shell?expected_birth=" + stale, "/api/runs/r/browser/cdp?expected_birth=" + stale} {
		c := dialGate(t, fx.hs, path)
		ce, texts := awaitWSClose(t, c, 10*time.Second)
		if len(texts) != 0 || ce.Code != websocket.CloseNormalClosure || !strings.HasPrefix(ce.Text, wsCloseScopeLost+": ") {
			t.Fatalf("%s: frames %v close %d %q, want the scope_lost close alone", path, texts, ce.Code, ce.Text)
		}
	}
}

// T4b: the socket binds exactly the origin the gate compared. A replacement
// right after the gate's read — before the upgrade — leaves the socket bound
// to the run it expected: its subscription is refused, never served the
// successor a second read would have bound.
func TestRunOriginGateSocketBindsTheComparedOrigin(t *testing.T) {
	fx := newGateFixture(t, true)
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 1))
	expected := birthOf(t, fx.native, "r")
	fx.store.afterLoadN(1, func() error {
		return replaceWSOriginRun(context.Background(), fx.native, "r", "B", store.LegacyValueFormat, store.RunStatusFinished, 1)
	})
	c := dialGate(t, fx.hs, "/api/ws/runs/r?expected_birth="+expected)
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s"})
	expectOriginLostClose(t, c, "snapshot_failed")
	mustWS(t, fx.store.failure())
	if birthOf(t, fx.native, "r") == expected {
		t.Fatal("the replacement hook did not run")
	}
}

// T5: a replacement between the gate and the handler's reads. The bound
// snapshot, events and persisted-log reads refuse the successor with 409 —
// never its body.
func TestRunOriginGateBoundReadsRefuseASuccessor(t *testing.T) {
	for _, path := range []string{"/api/runs/r", "/api/runs/r/events", "/api/runs/r/log"} {
		t.Run("GET "+path, func(t *testing.T) {
			fx := newGateFixture(t, true)
			ctx := t.Context()
			mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 2))
			writeRunLog(t, fx.srv.cfg.StoreDir, "r", "original bytes\n")
			expected := birthOf(t, fx.native, "r")
			fx.store.afterLoadN(1, func() error {
				if err := replaceWSOriginRun(context.Background(), fx.native, "r", "B", store.LegacyValueFormat, store.RunStatusFinished, 2); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(fx.srv.cfg.StoreDir, "runs", "r", "run.log"), []byte("successor bytes\n"), 0o600)
			})
			got := gateDo(t, fx.hs.URL, http.MethodGet, path, expected, "header")
			mustWS(t, fx.store.failure())
			wantOriginLost(t, path, got, "r")
			if strings.Contains(got.body, "successor") || strings.Contains(got.body, `"B"`) {
				t.Fatalf("%s leaked the successor: %s", path, got.body)
			}
		})
	}
}

// T5 (live buffer): GET /log serves a live in-memory buffer keyed by run ID
// alone. A successor that stands up its buffer after the handler's bound load
// is not served — the buffer records the successor's birth — and the
// persisted read under the binding refuses it: 409, not the successor's log.
func TestRunOriginGateLiveLogBufferRefusesASuccessor(t *testing.T) {
	fx := newGateFixture(t, true)
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 0))
	expected := birthOf(t, fx.native, "r")
	var fresh *websocket.Conn
	fx.store.afterLoadN(2, func() error {
		if err := replaceWSOriginRun(context.Background(), fx.native, "r", "B", store.LegacyValueFormat, store.RunStatusRunning, 0); err != nil {
			return err
		}
		writeRunLog(t, fx.srv.cfg.StoreDir, "r", "successor bytes\n")
		c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(fx.hs.URL, "http")+"/api/ws/runs/r", nil)
		if err != nil {
			return err
		}
		fresh = c
		if err := c.WriteJSON(runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l"}); err != nil {
			return err
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if buf := fx.srv.runs.GetLogBuffer("r"); buf != nil {
				if _, data, _ := buf.Snapshot(0); strings.Contains(string(data), "successor") {
					return nil
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		return errors.New("the successor's live buffer never filled")
	})
	got := gateDo(t, fx.hs.URL, http.MethodGet, "/api/runs/r/log", expected, "header")
	// Read while the successor's socket still holds its buffer: closing it
	// releases the subscription, and the buffer with it.
	held := fx.srv.runs.GetLogBuffer("r") != nil
	if fresh != nil {
		_ = fresh.Close()
	}
	mustWS(t, fx.store.failure())
	if !held {
		t.Fatal("fixture: the successor has no live buffer")
	}
	wantOriginLost(t, "live log", got, "r")
	if strings.Contains(got.body, "successor") {
		t.Fatalf("the live buffer leaked the successor: %s", got.body)
	}
}

// T5b: a checked snapshot never writes. A run whose merge is pending and
// already landed out of band is reconciled by an unchecked snapshot (the
// control: the writer is reachable), never by a checked one — the Mongo
// conditional merge write has no birth predicate.
func TestRunOriginGateCheckedSnapshotNeverReconciles(t *testing.T) {
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
	birth := birthOf(t, fx.native, "m")
	if got := gateDo(t, fx.hs.URL, http.MethodGet, "/api/runs/m", birth, "header"); got.status != http.StatusOK {
		t.Fatalf("checked snapshot: %d %s", got.status, got.body)
	}
	if n := fx.store.mergeWrites.Load(); n != 0 {
		t.Fatalf("a checked snapshot called the merge writer %d times", n)
	}
	if got := gateDo(t, fx.hs.URL, http.MethodGet, "/api/runs/m", "", ""); got.status != http.StatusOK {
		t.Fatalf("unchecked snapshot: %d %s", got.status, got.body)
	}
	if n := fx.store.mergeWrites.Load(); n != 1 {
		t.Fatalf("the unchecked control called the merge writer %d times, want 1: the fixture does not reach it", n)
	}
}
