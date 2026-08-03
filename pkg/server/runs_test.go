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
	"strings"
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
		WorkDir:                 workDir,
		StoreDir:                storeDir,
		SkipProjectRegistration: true, // tests must not pollute ~/.config/Iterion/config.json
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
	if _, err := st.CreateRun(context.Background(), runID, workflowName, map[string]any{"k": "v"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.UpdateRunStatus(context.Background(), runID, status, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	for i, evt := range []store.Event{
		{Type: store.EventRunStarted, RunID: runID},
		{Type: store.EventNodeStarted, RunID: runID, NodeID: "analyze", Data: map[string]any{"kind": "agent"}},
		{Type: store.EventNodeFinished, RunID: runID, NodeID: "analyze"},
	} {
		if _, err := st.AppendEvent(context.Background(), runID, evt); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}
}

// decodeJSONResp reads + unmarshals the response body into v, failing
// the test on any error. Distinct from production's decodeJSON (which
// drives request decoding from a handler), hence the -Resp suffix.
func decodeJSONResp(t *testing.T, resp *http.Response, v any) {
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
	decodeJSONResp(t, resp, &out)
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
		decodeJSONResp(t, resp, &out)
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
		decodeJSONResp(t, resp, &out)
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
		decodeJSONResp(t, resp, &out)
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
		decodeJSONResp(t, resp, &out)
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
	decodeJSONResp(t, resp, &snap)

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
		decodeJSONResp(t, resp, &out)
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
		decodeJSONResp(t, resp, &out)
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
		decodeJSONResp(t, resp, &out)
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
	decodeJSONResp(t, resp, &out)
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

// TestStateChangingEndpoints_RejectCrossStore covers the symmetric
// counterpart to the WS handlers' cross_store_readonly rejection: any
// POST /api/runs/{id}/... with ?store= must return 409 before any
// other validation, regardless of payload validity. Without this the
// pre-fix UX surfaced a confusing 404 pointing at the LOCAL store
// path when the operator clicked Cancel on a foreign-store run from
// the cross-daemon banner.
func TestStateChangingEndpoints_RejectCrossStore(t *testing.T) {
	_, hs := newTestServer(t)
	cases := []struct {
		name string
		path string
	}{
		{"cancel", "/api/runs/any-id/cancel"},
		{"resume", "/api/runs/any-id/resume"},
		{"merge", "/api/runs/any-id/merge"},
		{"browser_attach", "/api/runs/any-id/browser/attach"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(
				hs.URL+tc.path+"?store=/some/foreign/path",
				"application/json",
				bytes.NewReader([]byte(`{}`)),
			)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			if resp.StatusCode != http.StatusConflict {
				t.Errorf("status = %d, want 409", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !bytes.Contains(body, []byte("cross_store_readonly")) {
				t.Errorf("body = %q, want it to contain cross_store_readonly", string(body))
			}
		})
	}
}

// TestLaunch_RejectsBadBudgetDuration pins the admission contract for
// launchRunRequest.Budget: a max_duration that time.ParseDuration
// rejects must 400 with an explicit "invalid budget" error, before any
// run is created.
func TestLaunch_RejectsBadBudgetDuration(t *testing.T) {
	srv, hs := newTestServer(t)
	botPath := filepath.Join(srv.cfg.WorkDir, "demo.bot")
	if err := os.WriteFile(botPath, []byte("workflow demo:\n  entry: done\n"), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}

	body, err := json.Marshal(map[string]any{
		"file_path": botPath,
		"budget":    map[string]any{"max_duration": "4 hours"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(hs.URL+"/api/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(b, []byte("invalid budget")) {
		t.Errorf("body = %q, want it to contain %q", string(b), "invalid budget")
	}
}

// TestListRuns_BotFilter covers the ?bot= query param: it matches the
// run's resolved bundle name case-insensitively, including the legacy
// basename(bundle_path) fallback, and excludes bundle-less runs.
func TestListRuns_BotFilter(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-docs", "wf_docs", store.RunStatusFinished)
	seedRun(t, srv, "run-feature", "wf_feature", store.RunStatusFinished)
	seedRun(t, srv, "run-plain", "wf_plain", store.RunStatusFinished)

	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	stamp := func(id, bundleName, bundlePath string) {
		t.Helper()
		r, err := st.LoadRun(context.Background(), id)
		if err != nil {
			t.Fatalf("LoadRun %s: %v", id, err)
		}
		r.BundleName = bundleName
		r.BundlePath = bundlePath
		if err := st.SaveRun(context.Background(), r); err != nil {
			t.Fatalf("SaveRun %s: %v", id, err)
		}
	}
	stamp("run-docs", "docs-refresh", "")
	stamp("run-feature", "", "/bots/feature-dev.botz")

	get := func(query string) []runview.RunSummary {
		t.Helper()
		resp, err := http.Get(hs.URL + "/api/runs" + query)
		if err != nil {
			t.Fatalf("GET %s: %v", query, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200", query, resp.StatusCode)
		}
		var out struct {
			Runs []runview.RunSummary `json:"runs"`
		}
		decodeJSONResp(t, resp, &out)
		return out.Runs
	}

	if runs := get("?bot=docs-refresh"); len(runs) != 1 || runs[0].ID != "run-docs" {
		t.Errorf("?bot=docs-refresh → %+v, want only run-docs", runs)
	}
	if runs := get("?bot=DOCS-Refresh"); len(runs) != 1 || runs[0].ID != "run-docs" {
		t.Errorf("?bot=DOCS-Refresh (case-insensitive) → %+v, want only run-docs", runs)
	}
	if runs := get("?bot=feature-dev"); len(runs) != 1 || runs[0].ID != "run-feature" {
		t.Errorf("?bot=feature-dev (path fallback) → %+v, want only run-feature", runs)
	}
	if runs := get("?bot=nonexistent"); len(runs) != 0 {
		t.Errorf("?bot=nonexistent → %+v, want empty", runs)
	}
	if runs := get(""); len(runs) != 3 {
		t.Errorf("unfiltered → %d runs, want 3", len(runs))
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
	srv := New(Config{WorkDir: workDir, StoreDir: storeDir, SkipProjectRegistration: true}, logger)

	cacheRoot := srv.inlineSourceCacheDir()
	if cacheRoot == "" {
		t.Fatalf("inlineSourceCacheDir returned empty")
	}
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		t.Fatalf("mkdir cache root: %v", err)
	}
	cached := filepath.Join(cacheRoot, "resume.bot")
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
	outside := filepath.Join(t.TempDir(), "evil.bot")
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
	srv := New(Config{WorkDir: workDir, StoreDir: storeDir, SkipProjectRegistration: true}, logger)

	srcA := "workflow a:\n  entry: x\n"
	srcB := "workflow b:\n  entry: y\n"

	pathA, okA := srv.materializeInlineSource("/some/where/recipe.bot", srcA)
	if !okA {
		t.Fatalf("materialize A failed")
	}
	pathB, okB := srv.materializeInlineSource("/some/where/recipe.bot", srcB)
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
	pathA2, okA2 := srv.materializeInlineSource("/some/where/recipe.bot", srcA)
	if !okA2 || pathA2 != pathA {
		t.Fatalf("idempotent re-materialize: got (%q, %v), want %q", pathA2, okA2, pathA)
	}
}

// TestArtifactFile_DispositionToggle pins the contract that the studio's
// Artifacts panel relies on: the route serves files inline by default
// (so previewable types render in the browser) and switches to
// `attachment` when `?download=1` is set (so the Download button
// triggers a save dialog regardless of the response's content type or
// the embedding WebView's handling of the HTML5 `download` attribute).
func TestArtifactFile_DispositionToggle(t *testing.T) {
	srv, hs := newTestServer(t)
	const runID = "art-run"
	seedRun(t, srv, runID, "wf", store.RunStatusFinished)

	dir := filepath.Join(srv.cfg.StoreDir, "runs", runID, "artifact_files")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir artifact_files: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	for _, tc := range []struct {
		name        string
		query       string
		wantPrefix  string // disposition before "; filename=..."
		wantBodyHas string
	}{
		{name: "default inline", query: "", wantPrefix: "inline; ", wantBodyHas: `"ok":true`},
		{name: "download flag attaches", query: "?download=1", wantPrefix: "attachment; ", wantBodyHas: `"ok":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(hs.URL + "/api/runs/" + runID + "/artifact-files/report.json" + tc.query)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			disp := resp.Header.Get("Content-Disposition")
			if !bytes.HasPrefix([]byte(disp), []byte(tc.wantPrefix)) {
				t.Errorf("Content-Disposition = %q, want prefix %q", disp, tc.wantPrefix)
			}
			if !bytes.Contains([]byte(disp), []byte(`filename="report.json"`)) {
				t.Errorf("Content-Disposition missing filename: %q", disp)
			}
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if !bytes.Contains(b, []byte(tc.wantBodyHas)) {
				t.Errorf("body = %q, want to contain %q", b, tc.wantBodyHas)
			}
		})
	}
}

// TestRewindHandler_RefusesEscapingSourcePath is Revi's Rc48b6f.
//
// The handler forwarded a client-supplied source_path straight into
// CompileWorkflow + os.ReadFile, bypassing the containment boundary every
// other workflow-path surface goes through. That is an arbitrary-file
// primitive for any authenticated caller, and the parse diagnostic
// returned on failure echoes file content back.
func TestRewindHandler_RefusesEscapingSourcePath(t *testing.T) {
	srv, hs := newTestServer(t)
	// A real, rewindable run with a checkpoint — otherwise Rewind fails at
	// LoadRun and never reaches the path, and the test passes with or
	// without the containment check (verified by mutation).
	seedRun(t, srv, "rewindable", "wf", store.RunStatusFailedResumable)
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	run, err := st.LoadRun(context.Background(), "rewindable")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	run.Checkpoint = &store.Checkpoint{
		NodeID:  "implement",
		Outputs: map[string]map[string]any{"implement": {"v": 1}},
	}
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save: %v", err)
	}

	for _, escaping := range []string{"/etc/passwd", "../../../../etc/hostname"} {
		body := strings.NewReader(`{"node_id":"implement","source_path":"` + escaping + `"}`)
		resp, err := http.Post(hs.URL+"/api/runs/rewindable/rewind", "application/json", body)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		payload, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			t.Errorf("source_path %q was accepted", escaping)
		}
		// The give-away of the old behaviour: the parser diagnostic quotes
		// the offending token from the file it managed to read.
		for _, leak := range []string{"root:", "unexpected token"} {
			if strings.Contains(string(payload), leak) {
				t.Errorf("response for %q leaked file content (%q): %s", escaping, leak, payload)
			}
		}
	}
}
