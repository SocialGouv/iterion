package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestMergeConflict_FullChain drives the entire conflict-resolver
// HTTP surface against the real server: trigger conflict → fetch
// conflict snapshot → resolve a file → finalize.
func TestMergeConflict_FullChain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	srv, hs := newTestServer(t)

	repoDir := setupConflictingRepo(t)
	runID := "run-merge-conflict-http"
	seedConflictRun(t, srv, repoDir, runID)

	// 1. Trigger the merge → expect 409 (guard rejected) with the
	// conflict persisted server-side. The HTTP layer returns the
	// conflict message verbatim so the studio can show it as a toast.
	resp := postJSON(t, hs.URL+"/api/runs/"+runID+"/merge", map[string]any{
		"merge_strategy": "squash",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("/merge status=%d, want 409 (conflict)", resp.StatusCode)
	}
	resp.Body.Close()

	// 2. Run is now MergeStatusConflicted server-side.
	loaded := loadRun(t, srv, runID)
	if loaded.MergeStatus != store.MergeStatusConflicted {
		t.Fatalf("run.MergeStatus=%q, want conflicted", loaded.MergeStatus)
	}

	// 3. GET /merge/conflicts surfaces the parsed conflicts.
	getResp := httpGet(t, hs.URL+"/api/runs/"+runID+"/merge/conflicts")
	if getResp.StatusCode != 200 {
		t.Fatalf("GET /merge/conflicts status=%d", getResp.StatusCode)
	}
	var conflicts runview.MergeConflictsResponse
	decodeJSON(t, getResp, &conflicts)
	if len(conflicts.Files) != 1 {
		t.Fatalf("conflicts.files=%d, want 1", len(conflicts.Files))
	}
	if conflicts.Files[0].Path != "file.txt" {
		t.Errorf("file path=%q, want file.txt", conflicts.Files[0].Path)
	}
	if conflicts.PendingMergeInto != "main" {
		t.Errorf("pending_merge_into=%q, want main", conflicts.PendingMergeInto)
	}

	// 4. Out-of-set path is rejected with 409.
	rej := postJSON(t, hs.URL+"/api/runs/"+runID+"/merge/conflicts/resolve",
		map[string]any{"path": "../escape", "content": "x"},
	)
	if rej.StatusCode != http.StatusConflict {
		t.Errorf("out-of-set path status=%d, want 409", rej.StatusCode)
	}
	rej.Body.Close()

	// 5. Real resolve succeeds + returns fresh empty snapshot.
	resolved := "alpha\nresolved\ncharlie\n"
	okResp := postJSON(t, hs.URL+"/api/runs/"+runID+"/merge/conflicts/resolve",
		map[string]any{"path": "file.txt", "content": resolved},
	)
	if okResp.StatusCode != 200 {
		t.Fatalf("resolve status=%d", okResp.StatusCode)
	}
	var afterResolve runview.MergeConflictsResponse
	decodeJSON(t, okResp, &afterResolve)
	if len(afterResolve.Files) != 0 {
		t.Errorf("post-resolve files=%d, want 0", len(afterResolve.Files))
	}

	// 6. Finalize commits the squash and flips status to merged.
	finResp := postJSON(t, hs.URL+"/api/runs/"+runID+"/merge/conflicts/finalize",
		map[string]any{},
	)
	if finResp.StatusCode != 200 {
		t.Fatalf("finalize status=%d", finResp.StatusCode)
	}
	var fin mergeRunResponse
	decodeJSON(t, finResp, &fin)
	if fin.MergeStatus != store.MergeStatusMerged {
		t.Errorf("merge_status=%q, want merged", fin.MergeStatus)
	}
	if fin.MergedCommit == "" {
		t.Error("merged_commit should be set")
	}

	// 7. Worktree reflects the resolved content.
	content, err := os.ReadFile(filepath.Join(repoDir, "file.txt"))
	if err != nil {
		t.Fatalf("read file.txt: %v", err)
	}
	if string(content) != resolved {
		t.Errorf("file.txt content mismatch: got %q want %q", string(content), resolved)
	}
}

// TestMergeConflict_AbortRestores drives the abort path: conflict +
// /abort flips status back to failed and restores the worktree.
func TestMergeConflict_AbortRestores(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	srv, hs := newTestServer(t)
	repoDir := setupConflictingRepo(t)
	runID := "run-merge-conflict-abort"
	seedConflictRun(t, srv, repoDir, runID)

	resp := postJSON(t, hs.URL+"/api/runs/"+runID+"/merge", map[string]any{})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("/merge status=%d, want 409", resp.StatusCode)
	}
	resp.Body.Close()

	abortResp := postJSON(t, hs.URL+"/api/runs/"+runID+"/merge/conflicts/abort", nil)
	if abortResp.StatusCode != http.StatusNoContent {
		t.Fatalf("abort status=%d, want 204", abortResp.StatusCode)
	}
	abortResp.Body.Close()

	loaded := loadRun(t, srv, runID)
	if loaded.MergeStatus != store.MergeStatusFailed {
		t.Errorf("post-abort merge_status=%q, want failed", loaded.MergeStatus)
	}
}

// TestMergeConflict_AgentResolverUnavailable verifies the agent
// endpoint surfaces a clear error when no LLM credential is reachable
// (the typical CI scenario).
func TestMergeConflict_AgentResolverUnavailable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// Strip every credential env var so the detector returns "".
	for _, k := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
		"OPENAI_API_KEY", "OPENAI_BASE_URL",
		"ZAI_API_KEY", "AWS_REGION", "GOOGLE_CLOUD_PROJECT",
		"AZURE_OPENAI_API_KEY",
	} {
		t.Setenv(k, "")
	}
	// Point the OAuth lookup at an empty dir so codex/claude detection
	// also returns false.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "0")

	srv, hs := newTestServer(t)
	repoDir := setupConflictingRepo(t)
	runID := "run-agent-unavailable"
	seedConflictRun(t, srv, repoDir, runID)

	resp := postJSON(t, hs.URL+"/api/runs/"+runID+"/merge", map[string]any{})
	resp.Body.Close()

	agentResp := postJSON(t, hs.URL+"/api/runs/"+runID+"/merge/conflicts/resolve-with-agent",
		map[string]any{},
	)
	if agentResp.StatusCode == 200 {
		t.Fatalf("expected non-200 when no creds; got 200")
	}
	body := readBody(t, agentResp)
	if !strings.Contains(body, "credential") && !strings.Contains(body, "no LLM") {
		t.Errorf("error body should mention the missing credential; got %q", body)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func setupConflictingRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"LC_ALL=C",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\noutput: %s", args, err, string(out))
		}
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "t@t.t")
	runGit("config", "user.name", "t")
	runGit("config", "commit.gpgsign", "false")
	write("file.txt", "alpha\nbravo\ncharlie\n")
	runGit("add", "file.txt")
	runGit("commit", "-qm", "base")

	runGit("checkout", "-qb", "iterion/run/test")
	write("file.txt", "alpha\nINCOMING\ncharlie\n")
	runGit("commit", "-qam", "feat")

	runGit("checkout", "-q", "main")
	write("file.txt", "alpha\nMAIN\ncharlie\n")
	runGit("commit", "-qam", "main-change")
	return dir
}

func seedConflictRun(t *testing.T, srv *Server, repoDir, runID string) {
	t.Helper()
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := st.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.Worktree = true
	r.RepoRoot = repoDir
	r.WorkDir = repoDir
	r.Status = store.RunStatusFinished
	r.FinalBranch = "iterion/run/test"
	// FinalCommit is the storage branch tip; resolve via git.
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "iterion/run/test").Output()
	if err != nil {
		t.Fatalf("rev-parse storage: %v", err)
	}
	r.FinalCommit = strings.TrimSpace(string(out))
	baseOut, err := exec.Command("git", "-C", repoDir, "merge-base", "main", "iterion/run/test").Output()
	if err != nil {
		t.Fatalf("merge-base: %v", err)
	}
	r.BaseCommit = strings.TrimSpace(string(baseOut))
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("SaveRun seed: %v", err)
	}
}

func loadRun(t *testing.T, srv *Server, runID string) *store.Run {
	t.Helper()
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	r, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	return r
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// requireSafeOrigin only rejects when Origin is set to a
	// non-loopback value; an unset Origin (server-to-server call)
	// passes through. Leave it unset.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func httpGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var b bytes.Buffer
	if _, err := b.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b.String()
}
