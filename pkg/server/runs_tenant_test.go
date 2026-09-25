package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// tenantGuardStore mimics the mongo store's tenant filter at LoadRun —
// the store method runview.Service.LoadRunCtx calls, so the method every
// handler's tenant gate reaches: a run is only visible to its owning
// tenant. The rest is a real filesystem store, which knows no tenant, as
// the blob behind the mongo store does not.
type tenantGuardStore struct {
	*store.FilesystemRunStore
	runTenant string
}

func (g *tenantGuardStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if tid, ok := store.TenantFromContext(ctx); ok && tid != g.runTenant {
		return nil, errors.New("store: run file not found")
	}
	return g.FilesystemRunStore.LoadRun(ctx, id)
}

// tenantGuardedServer serves one run, run-1, that exists for real and
// belongs to tenant-A, with a log, a tool blob and an artifact file of its
// own: a handler that skipped its gate would serve them. Before any handler
// is exercised, the guard is probed from the service the handlers call:
// tenant-A must see run-1 and tenant-B must not. A guard nobody reaches
// would otherwise leave every "another tenant gets 404" assertion green for
// the wrong reason.
func tenantGuardedServer(t *testing.T, opts ...runview.ServiceOption) (*Server, *store.FilesystemRunStore) {
	t.Helper()
	srv, _ := newTestServer(t)
	orig := srv.runs
	t.Cleanup(func() { srv.runs = orig }) // restore so Shutdown drains the real service

	realStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if _, err := realStore.CreateRun(ctx, "run-1", "wf", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := realStore.AppendRunLog(ctx, "run-1", 0, []byte("tenant-A log\n")); err != nil {
		t.Fatalf("append log: %v", err)
	}
	if _, err := realStore.WriteToolBlob(ctx, "run-1", "tu-1", "input", []byte("tenant-A tool input")); err != nil {
		t.Fatalf("write tool blob: %v", err)
	}
	filesDir, err := realStore.EnsureRunFilesDir(ctx, "run-1")
	if err != nil {
		t.Fatalf("run files dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(filesDir, "report.md"), []byte("# tenant-A report\n"), 0o600); err != nil {
		t.Fatalf("write artifact file: %v", err)
	}
	opts = append([]runview.ServiceOption{runview.WithStore(&tenantGuardStore{FilesystemRunStore: realStore, runTenant: "tenant-A"})}, opts...)
	srv.runs = newTestRunviewService(t, srv.cfg.StoreDir, opts...)

	if _, err := srv.runs.LoadRunCtx(store.WithTenant(ctx, "tenant-A"), "run-1"); err != nil {
		t.Fatalf("probe: the owning tenant does not see run-1: %v", err)
	}
	if _, err := srv.runs.LoadRunCtx(store.WithTenant(ctx, "tenant-B"), "run-1"); err == nil {
		t.Fatalf("probe: another tenant sees run-1 — the guard is not on the path the handlers take")
	}
	return srv, realStore
}

// A caller from another tenant must not be able to cancel/pause/read a
// run by guessing its id: each handler pre-checks ownership via
// LoadRunCtx (which carries the request's tenant), so cross-tenant
// requests get 404 before any mutation or filesystem read. Each read is
// first made by the owning tenant, which must be served: the data is
// there, so another tenant's 404 is the gate's. cancel's 404 here also
// comes from a second load, after the runtime was asked to cancel; its
// gate is proven by that effect in
// TestAnotherTenantsCancelNeverReachesTheRunnerPool.
func TestRunEndpointsRejectCrossTenant(t *testing.T) {
	srv, _ := tenantGuardedServer(t)

	cases := []struct {
		name      string
		method    string
		path      string
		call      func(http.ResponseWriter, *http.Request)
		pathVals  map[string]string
		ownerRead bool
	}{
		{"cancel", http.MethodPost, "/api/runs/run-1/cancel", srv.handleCancelRun, map[string]string{"id": "run-1"}, false},
		{"pause", http.MethodPost, "/api/runs/run-1/pause", srv.handlePauseRun, map[string]string{"id": "run-1"}, false},
		{"log", http.MethodGet, "/api/runs/run-1/log", srv.handleGetRunLog, map[string]string{"id": "run-1"}, true},
		{"toolblob", http.MethodGet, "/api/runs/run-1/tools/tu-1/input", srv.handleGetToolBlob, map[string]string{"id": "run-1", "toolUseID": "tu-1", "kind": "input"}, true},
		{"artifact-files-list", http.MethodGet, "/api/runs/run-1/artifact-files", srv.handleListArtifactFiles, map[string]string{"id": "run-1"}, true},
		{"artifact-file-get", http.MethodGet, "/api/runs/run-1/artifact-files/report.md", srv.handleGetArtifactFile, map[string]string{"id": "run-1", "path": "report.md"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			as := func(tenant string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(c.method, c.path, nil)
				for k, v := range c.pathVals {
					req.SetPathValue(k, v)
				}
				req = req.WithContext(store.WithTenant(req.Context(), tenant))
				rec := httptest.NewRecorder()
				c.call(rec, req)
				return rec
			}
			if c.ownerRead {
				if rec := as("tenant-A"); rec.Code != http.StatusOK {
					t.Fatalf("%s by the owning tenant: got status %d, want 200 (body %s)", c.name, rec.Code, rec.Body.String())
				}
			}
			if rec := as("tenant-B"); rec.Code != http.StatusNotFound {
				t.Fatalf("%s cross-tenant: got status %d, want 404 (body %s)", c.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// cancelSpyPublisher stands in for the cloud runner pool: it records
// every cancel it is asked to deliver. Only CancelRun is implemented.
type cancelSpyPublisher struct {
	runview.LaunchPublisher
	mu        sync.Mutex
	cancelled []string
}

func (p *cancelSpyPublisher) CancelRun(_ context.Context, runID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancelled = append(p.cancelled, runID)
	return nil
}

func (p *cancelSpyPublisher) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.cancelled...)
}

// In cloud mode a cancel reaches the runner pool through the launch
// publisher, which knows no tenant: the handler's gate is all that keeps
// another tenant's cancel from being delivered. The owning tenant's
// cancel is delivered first — the witness that the path reaches the pool.
func TestAnotherTenantsCancelNeverReachesTheRunnerPool(t *testing.T) {
	spy := &cancelSpyPublisher{}
	srv, _ := tenantGuardedServer(t, runview.WithLaunchPublisher(spy))

	cancelAs := func(tenant string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/runs/run-1/cancel", nil)
		req.SetPathValue("id", "run-1")
		req = req.WithContext(store.WithTenant(req.Context(), tenant))
		rec := httptest.NewRecorder()
		srv.handleCancelRun(rec, req)
		return rec
	}
	if rec := cancelAs("tenant-A"); rec.Code != http.StatusAccepted {
		t.Fatalf("owning tenant: got status %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if got := spy.calls(); len(got) != 1 || got[0] != "run-1" {
		t.Fatalf("owning tenant: runner pool asked to cancel %v, want [run-1]", got)
	}
	if rec := cancelAs("tenant-B"); rec.Code != http.StatusNotFound {
		t.Fatalf("another tenant: got status %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if got := spy.calls(); len(got) != 1 {
		t.Fatalf("another tenant's cancel reached the runner pool: cancels %v", got)
	}
}

// The run WebSocket accepts a "cancel" command that reaches the runner
// pool like the HTTP cancel, so its upgrade gate is the only thing between
// another tenant and that pool. The owning tenant's cancel is delivered
// first — the witness that the socket reaches the pool.
func TestAnotherTenantsWebSocketCancelNeverReachesTheRunnerPool(t *testing.T) {
	spy := &cancelSpyPublisher{}
	srv, _ := tenantGuardedServer(t, runview.WithLaunchPublisher(spy))
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("id", "run-1")
		r = r.WithContext(store.WithTenant(r.Context(), r.URL.Query().Get("tenant")))
		srv.handleRunWebSocket(w, r)
	}))
	t.Cleanup(hs.Close)
	dial := func(tenant string) (*websocket.Conn, *http.Response, error) {
		return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http")+"/api/ws/runs/run-1?tenant="+tenant, nil)
	}

	owner, _, err := dial("tenant-A")
	if err != nil {
		t.Fatalf("owning tenant: dial: %v", err)
	}
	if err := owner.WriteJSON(map[string]any{"type": "cancel", "ack_id": "a1"}); err != nil {
		t.Fatalf("owning tenant: send cancel: %v", err)
	}
	_ = owner.SetReadDeadline(time.Now().Add(5 * time.Second))
	for len(spy.calls()) == 0 {
		if _, _, err := owner.ReadMessage(); err != nil {
			t.Fatalf("owning tenant: no cancel reached the runner pool before the socket ended: %v", err)
		}
	}
	_ = owner.Close()
	if got := spy.calls(); len(got) != 1 || got[0] != "run-1" {
		t.Fatalf("owning tenant: runner pool asked to cancel %v, want [run-1]", got)
	}

	other, resp, err := dial("tenant-B")
	if err == nil {
		_ = other.WriteJSON(map[string]any{"type": "cancel", "ack_id": "b1"})
		_ = other.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, _ = other.ReadMessage()
		_ = other.Close()
		t.Fatalf("another tenant: the socket was upgraded; runner pool cancels %v", spy.calls())
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("another tenant: dial refused with status %d, want 404 (%v)", code, err)
	}
	if got := spy.calls(); len(got) != 1 {
		t.Fatalf("another tenant's cancel reached the runner pool: cancels %v", got)
	}
}

// The versions of a node's artifact are listed, in cloud mode, from the
// blob store, which knows no tenant; the list is gated on the run first.
func TestTheArtifactVersionsOfAnotherTenantsRunAreNotListed(t *testing.T) {
	srv, realStore := tenantGuardedServer(t)
	if err := realStore.WriteArtifact(context.Background(), &store.Artifact{RunID: "run-1", NodeID: "review", Version: 0, Data: map[string]any{"verdict": "tenant-A only"}}); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	list := func(tenant string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/runs/run-1/artifacts/review", nil)
		req.SetPathValue("id", "run-1")
		req.SetPathValue("node", "review")
		req = req.WithContext(store.WithTenant(req.Context(), tenant))
		rec := httptest.NewRecorder()
		srv.handleListArtifacts(rec, req)
		return rec
	}
	if rec := list("tenant-A"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":0`) {
		t.Fatalf("owning tenant: got status %d, want 200 listing version 0 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := list("tenant-B"); rec.Code != http.StatusNotFound {
		t.Fatalf("another tenant: got status %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// An artifact version is read by (run, node, version) from a store that
// knows no tenant — the blob, in cloud mode — so the handler gates on the
// run first, as its siblings do. The artifact is written for real: the
// owning tenant reads it, and only then does another tenant's 404 prove
// the gate rather than a missing file.
func TestAnArtifactVersionOfAnotherTenantsRunIsNotServed(t *testing.T) {
	srv, realStore := tenantGuardedServer(t)
	if err := realStore.WriteArtifact(context.Background(), &store.Artifact{RunID: "run-1", NodeID: "review", Version: 0, Data: map[string]any{"verdict": "tenant-A only"}}); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	get := func(tenant string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/runs/run-1/artifacts/review/0", nil)
		req.SetPathValue("id", "run-1")
		req.SetPathValue("node", "review")
		req.SetPathValue("version", "0")
		req = req.WithContext(store.WithTenant(req.Context(), tenant))
		rec := httptest.NewRecorder()
		srv.handleGetArtifact(rec, req)
		return rec
	}
	if rec := get("tenant-A"); rec.Code != http.StatusOK {
		t.Fatalf("owning tenant: got status %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := get("tenant-B"); rec.Code != http.StatusNotFound {
		t.Fatalf("another tenant: got status %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// The cross-store proxy (?store=) reads a filesystem store that knows no
// tenant, so a cloud instance refuses it. The same request on a local
// daemon is served — the witness that the store holds the run, so the
// cloud 400 proves the mode guard rather than a missing run.
func TestTheCrossStoreProxyIsRefusedOnACloudInstance(t *testing.T) {
	srv, _ := newTestServer(t)
	foreign, dir := newCrossStore(t)
	if _, err := foreign.CreateRun(context.Background(), "run-x", "wf", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/runs/run-x?store="+url.QueryEscape(dir), nil)
		req.SetPathValue("id", "run-x")
		req = req.WithContext(store.WithTenant(req.Context(), "tenant-B"))
		rec := httptest.NewRecorder()
		srv.handleGetRun(rec, req)
		return rec
	}
	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("local daemon: got status %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	srv.cfg.Mode = "cloud"
	if rec := get(); rec.Code != http.StatusBadRequest {
		t.Fatalf("cloud instance: got status %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}
