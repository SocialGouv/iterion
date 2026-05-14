package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// newTestServer wires a Server with a temp WorkDir + StoreDir so the
// run endpoints have a real backing store. The returned *httptest.Server
// must be closed by the caller via t.Cleanup.
func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	workDir := t.TempDir()
	storeDir := filepath.Join(workDir, ".iterion")

	logger := iterlog.New(iterlog.LevelError, os.Stderr) // quiet
	srv := New(Config{
		WorkDir:  workDir,
		StoreDir: storeDir,
	}, logger)
	if srv.runs == nil {
		t.Fatalf("expected run console service to be wired")
	}
	hs := httptest.NewServer(srv.mux)
	t.Cleanup(func() {
		hs.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv, hs
}

// seedRun writes a synthetic run + a few events into the store so the
// read endpoints have something to return. We open a fresh store
// handle pointing at the same directory rather than borrowing one
// from the Service — the Service's surface is intentionally narrow.
func seedRun(t *testing.T, srv *Server, runID, workflowName string, status store.RunStatus) {
	t.Helper()
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	if _, err := st.CreateRun(context.Background(), runID, workflowName, map[string]interface{}{"k": "v"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.UpdateRunStatus(context.Background(), runID, status, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	for i, evt := range []store.Event{
		{Type: store.EventRunStarted, RunID: runID},
		{Type: store.EventNodeStarted, RunID: runID, NodeID: "analyze", Data: map[string]interface{}{"kind": "agent"}},
		{Type: store.EventNodeFinished, RunID: runID, NodeID: "analyze"},
	} {
		if _, err := st.AppendEvent(context.Background(), runID, evt); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}
}

func decodeJSON(t *testing.T, resp *http.Response, v interface{}) {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode body %q: %v", string(b), err)
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestListRuns_EmptyStore(t *testing.T) {
	_, hs := newTestServer(t)
	resp, err := http.Get(hs.URL + "/api/runs")
	if err != nil {
		t.Fatalf("GET /api/runs: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Runs []runview.RunSummary `json:"runs"`
	}
	decodeJSON(t, resp, &out)
	if len(out.Runs) != 0 {
		t.Errorf("Runs = %d, want 0", len(out.Runs))
	}
}

func TestListRuns_WithSeedAndFilter(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-1", "wf_alpha", store.RunStatusFinished)
	seedRun(t, srv, "run-2", "wf_beta", store.RunStatusFailed)
	seedRun(t, srv, "run-3", "wf_alpha", store.RunStatusRunning)

	t.Run("no filter", func(t *testing.T) {
		resp, err := http.Get(hs.URL + "/api/runs")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		var out struct {
			Runs []runview.RunSummary `json:"runs"`
		}
		decodeJSON(t, resp, &out)
		if len(out.Runs) != 3 {
			t.Errorf("len = %d, want 3", len(out.Runs))
		}
	})

	t.Run("filter by status", func(t *testing.T) {
		resp, err := http.Get(hs.URL + "/api/runs?status=finished")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		var out struct {
			Runs []runview.RunSummary `json:"runs"`
		}
		decodeJSON(t, resp, &out)
		if len(out.Runs) != 1 || out.Runs[0].ID != "run-1" {
			t.Errorf("Runs = %+v, want only run-1", out.Runs)
		}
	})

	t.Run("filter by workflow", func(t *testing.T) {
		resp, err := http.Get(hs.URL + "/api/runs?workflow=wf_alpha")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		var out struct {
			Runs []runview.RunSummary `json:"runs"`
		}
		decodeJSON(t, resp, &out)
		if len(out.Runs) != 2 {
			t.Errorf("len = %d, want 2", len(out.Runs))
		}
	})

	t.Run("limit", func(t *testing.T) {
		resp, err := http.Get(hs.URL + "/api/runs?limit=2")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		var out struct {
			Runs []runview.RunSummary `json:"runs"`
		}
		decodeJSON(t, resp, &out)
		if len(out.Runs) != 2 {
			t.Errorf("len = %d, want 2", len(out.Runs))
		}
	})
}

func TestGetRun_SnapshotShape(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-1", "wf_alpha", store.RunStatusFinished)

	resp, err := http.Get(hs.URL + "/api/runs/run-1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var snap runview.RunSnapshot
	decodeJSON(t, resp, &snap)

	if snap.Run.ID != "run-1" {
		t.Errorf("Run.ID = %q, want run-1", snap.Run.ID)
	}
	if len(snap.Executions) != 1 {
		t.Fatalf("Executions = %d, want 1", len(snap.Executions))
	}
	if snap.Executions[0].IRNodeID != "analyze" {
		t.Errorf("IRNodeID = %q, want analyze", snap.Executions[0].IRNodeID)
	}
	if snap.Executions[0].Status != runview.ExecStatusFinished {
		t.Errorf("Status = %q, want finished", snap.Executions[0].Status)
	}
	if snap.LastSeq < 2 {
		t.Errorf("LastSeq = %d, want >= 2", snap.LastSeq)
	}
}

func TestGetRun_NotFound(t *testing.T) {
	_, hs := newTestServer(t)
	resp, err := http.Get(hs.URL + "/api/runs/missing")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGetEvents_FromTo(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-1", "wf", store.RunStatusFinished)

	t.Run("all", func(t *testing.T) {
		resp, err := http.Get(hs.URL + "/api/runs/run-1/events")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		var out struct {
			Events []*store.Event `json:"events"`
		}
		decodeJSON(t, resp, &out)
		if len(out.Events) != 3 {
			t.Errorf("Events = %d, want 3", len(out.Events))
		}
	})

	t.Run("from=1", func(t *testing.T) {
		resp, err := http.Get(hs.URL + "/api/runs/run-1/events?from=1")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		var out struct {
			Events []*store.Event `json:"events"`
		}
		decodeJSON(t, resp, &out)
		if len(out.Events) != 2 {
			t.Errorf("Events = %d, want 2", len(out.Events))
		}
	})

	t.Run("to=2", func(t *testing.T) {
		resp, err := http.Get(hs.URL + "/api/runs/run-1/events?to=2")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		var out struct {
			Events []*store.Event `json:"events"`
		}
		decodeJSON(t, resp, &out)
		if len(out.Events) != 2 {
			t.Errorf("Events = %d, want 2", len(out.Events))
		}
	})
}

func TestCancelInactive_ReportsCurrentStatus(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-1", "wf", store.RunStatusFinished)

	resp, err := http.Post(hs.URL+"/api/runs/run-1/cancel", "application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}
	var out cancelRunResponse
	decodeJSON(t, resp, &out)
	if out.Status != string(store.RunStatusFinished) {
		t.Errorf("Status = %q, want finished", out.Status)
	}
}

func TestCancel_Missing404(t *testing.T) {
	_, hs := newTestServer(t)
	resp, err := http.Post(hs.URL+"/api/runs/ghost/cancel", "application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestLaunch_RejectsMissingFilePath(t *testing.T) {
	_, hs := newTestServer(t)
	resp, err := http.Post(hs.URL+"/api/runs", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestLaunch_RejectsPathOutsideWorkDir(t *testing.T) {
	_, hs := newTestServer(t)
	body := bytes.NewReader([]byte(`{"file_path":"/etc/passwd"}`))
	resp, err := http.Post(hs.URL+"/api/runs", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestResolveWorkflowPath_InlineCacheFallback covers the resume case
// where the persisted FilePath points at the server's own inline-source
// cache (outside the current WorkDir, because the operator switched
// projects between launch and resume). safePath would reject the path;
// resolveCachedInlineSource trusts files it wrote into its own cache.
func TestResolveWorkflowPath_InlineCacheFallback(t *testing.T) {
	// WorkDir and StoreDir live in separate temp trees on purpose: this
	// reproduces the production failure mode where the current project's
	// WorkDir is *not* an ancestor of the inline-source cache.
	workDir := t.TempDir()
	storeDir := filepath.Join(t.TempDir(), ".iterion")
	logger := iterlog.New(iterlog.LevelError, os.Stderr)
	srv := New(Config{WorkDir: workDir, StoreDir: storeDir}, logger)

	cacheRoot := srv.inlineSourceCacheDir()
	if cacheRoot == "" {
		t.Fatalf("inlineSourceCacheDir returned empty")
	}
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		t.Fatalf("mkdir cache root: %v", err)
	}
	cached := filepath.Join(cacheRoot, "resume.iter")
	if err := os.WriteFile(cached, []byte("workflow x:\n"), 0o644); err != nil {
		t.Fatalf("write cached source: %v", err)
	}

	// safePath SHOULD reject this path (it escapes WorkDir). The fallback
	// must rescue it because we wrote the file ourselves.
	if _, err := srv.safePath(cached); err == nil {
		t.Fatalf("test setup invalid: safePath unexpectedly accepted cached path %q", cached)
	}
	got, err := srv.resolveWorkflowPath(cached, "")
	if err != nil {
		t.Fatalf("resolveWorkflowPath(%q, \"\") = err %v, want success via inline-cache fallback", cached, err)
	}
	if got != cached {
		t.Errorf("resolved = %q, want %q", got, cached)
	}

	// An absolute path that escapes both WorkDir AND the inline cache
	// stays rejected — the fallback must not become a host-FS opener.
	outside := filepath.Join(t.TempDir(), "evil.iter")
	if err := os.WriteFile(outside, []byte("workflow x:\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if _, err := srv.resolveWorkflowPath(outside, ""); err == nil {
		t.Errorf("resolveWorkflowPath(%q, \"\") expected reject for path outside workdir+cache", outside)
	}
}

// TestMaterializeInlineSource_NoOverwriteAcrossContent ensures that two
// different source bodies for the same basename do NOT overwrite each
// other's cache file. Before the content-hashed filename, run A's
// persisted FilePath would silently start pointing at run B's bytes
// after a fresh launch of the same recipe, so resume of A failed the
// workflow-hash check unless the caller passed --force or re-supplied
// source.
func TestMaterializeInlineSource_NoOverwriteAcrossContent(t *testing.T) {
	workDir := t.TempDir()
	storeDir := filepath.Join(t.TempDir(), ".iterion")
	logger := iterlog.New(iterlog.LevelError, os.Stderr)
	srv := New(Config{WorkDir: workDir, StoreDir: storeDir}, logger)

	srcA := "workflow a:\n  entry: x\n"
	srcB := "workflow b:\n  entry: y\n"

	pathA, okA := srv.materializeInlineSource("/some/where/recipe.iter", srcA)
	if !okA {
		t.Fatalf("materialize A failed")
	}
	pathB, okB := srv.materializeInlineSource("/some/where/recipe.iter", srcB)
	if !okB {
		t.Fatalf("materialize B failed")
	}
	if pathA == pathB {
		t.Fatalf("different content must produce different cache paths: %q", pathA)
	}
	gotA, err := os.ReadFile(pathA)
	if err != nil || string(gotA) != srcA {
		t.Fatalf("cache file A content mismatch: %v %q", err, gotA)
	}
	gotB, err := os.ReadFile(pathB)
	if err != nil || string(gotB) != srcB {
		t.Fatalf("cache file B content mismatch: %v %q", err, gotB)
	}

	// Same content re-materialized → idempotent (same path).
	pathA2, okA2 := srv.materializeInlineSource("/some/where/recipe.iter", srcA)
	if !okA2 || pathA2 != pathA {
		t.Fatalf("idempotent re-materialize: got (%q, %v), want %q", pathA2, okA2, pathA)
	}
}
