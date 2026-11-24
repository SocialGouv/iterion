package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// agentModelGate is an OpenAI-compatible endpoint whose answer waits for the
// test: while it waits, the agent resolver is inside its model call — the
// conflict read, the repository's hold not taken. Its answer resolves
// file.txt with content.
type agentModelGate struct {
	inCall chan struct{}
	answer chan struct{}
	let    func()
}

func newAgentModelGate(t *testing.T, content string) *agentModelGate {
	t.Helper()
	g := &agentModelGate{inCall: make(chan struct{}), answer: make(chan struct{})}
	var entered sync.Once
	g.let = sync.OnceFunc(func() { close(g.answer) })
	payload, err := json.Marshal(map[string]any{"files": []map[string]string{{"path": "file.txt", "content": content}}})
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := json.Marshal(map[string]any{"id": "c1", "choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": string(payload)}, "finish_reason": "stop"}}})
	if err != nil {
		t.Fatal(err)
	}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		entered.Do(func() { close(g.inCall) })
		<-g.answer
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+string(chunk)+"\n\ndata: [DONE]\n\n")
	}))
	// Cleanups run last-in first-out: the held answer is let go before the
	// endpoint's Close waits for it.
	t.Cleanup(model.Close)
	t.Cleanup(g.let)
	t.Setenv("OPENAI_BASE_URL", model.URL)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "0")
	return g
}

// agentAnswer is the resolve-with-agent route's answer.
type agentAnswer struct {
	code int
	body string
	err  error
}

// startAgentResolve posts resolve-with-agent for runID and waits until the
// resolver is inside its model call.
func startAgentResolve(t *testing.T, hsURL, runID string, model *agentModelGate) <-chan agentAnswer {
	t.Helper()
	done := make(chan agentAnswer, 1)
	go func() {
		resp, err := http.Post(hsURL+"/api/runs/"+runID+"/merge/conflicts/resolve-with-agent", "application/json", strings.NewReader(`{"model":"openai/gpt-x"}`))
		if err != nil {
			done <- agentAnswer{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		done <- agentAnswer{code: resp.StatusCode, body: string(body), err: err}
	}()
	select {
	case <-model.inCall:
	case got := <-done:
		t.Fatalf("the agent resolve answered %d %s (%v) before its model call", got.code, got.body, got.err)
	}
	return done
}

// The agent resolver reads the conflict before its model call and stages the
// model's answer under the repository's hold. Under the hold it reads the
// run's status and the conflict again: a file resolved by hand, edited, or a
// merge aborted while the model answered is never overwritten — the route
// answers 409 and stages nothing. One scenario per check: the manual
// resolution leaves the run conflicted and its file merged (the unmerged
// set), the edit leaves the file unmerged with other bytes (the bytes the
// model saw), and another run's identical conflict leaves the file unmerged
// with the same bytes under a run that is no longer conflicted (the status).
// The abort fails the status and the unmerged set at once.
func TestAgentConflictResolutionStagesOnlyTheConflictItWasShown(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const agent = "alpha\nAGENT\ncharlie\n"
	for _, tc := range []struct {
		name string
		// meanwhile runs while the model answers.
		meanwhile func(t *testing.T, srv *Server, hsURL, repoDir string)
		code      int
		mention   string
		// after checks the repository and the run once the agent answered.
		after func(t *testing.T, srv *Server, repoDir string)
	}{
		{
			name:      "nothing moved",
			meanwhile: func(*testing.T, *Server, string, string) {},
			code:      http.StatusOK,
			after: func(t *testing.T, srv *Server, repoDir string) {
				requireFileAndIndex(t, repoDir, agent, agent)
			},
		},
		{
			name: "a manual resolution",
			meanwhile: func(t *testing.T, _ *Server, hsURL, _ string) {
				resp := postJSON(t, hsURL+"/api/runs/run-agent-race/merge/conflicts/resolve", map[string]any{"path": "file.txt", "content": "alpha\nOPERATOR\ncharlie\n"})
				if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
					t.Fatalf("the manual resolution answered %d %s", resp.StatusCode, body)
				}
			},
			code:    http.StatusConflict,
			mention: "file.txt is no longer conflicted",
			after: func(t *testing.T, srv *Server, repoDir string) {
				requireFileAndIndex(t, repoDir, "alpha\nOPERATOR\ncharlie\n", "alpha\nOPERATOR\ncharlie\n")
				if got := loadRun(t, srv, "run-agent-race").MergeStatus; got != store.MergeStatusConflicted {
					t.Fatalf("merge_status=%q; the manual resolution leaves it conflicted until the finalize", got)
				}
			},
		},
		{
			name: "an abort",
			meanwhile: func(t *testing.T, _ *Server, hsURL, _ string) {
				resp := postJSON(t, hsURL+"/api/runs/run-agent-race/merge/conflicts/abort", nil)
				if body := readBody(t, resp); resp.StatusCode != http.StatusNoContent {
					t.Fatalf("the abort answered %d %s", resp.StatusCode, body)
				}
			},
			code:    http.StatusConflict,
			mention: "is no longer conflicted",
			after: func(t *testing.T, srv *Server, repoDir string) {
				requireFileAndIndex(t, repoDir, "alpha\nMAIN\ncharlie\n", "alpha\nMAIN\ncharlie\n")
				if status := gittest.Run(t, repoDir, "status", "--porcelain"); status != "" {
					t.Fatalf("the aborted checkout is dirty: %q", status)
				}
				if got := loadRun(t, srv, "run-agent-race").MergeStatus; got != store.MergeStatusFailed {
					t.Fatalf("merge_status=%q; want failed after the abort", got)
				}
			},
		},
		{
			name: "an edit of the conflicted file",
			meanwhile: func(t *testing.T, _ *Server, _, repoDir string) {
				if err := os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("alpha\nOPERATOR-EDITING\ncharlie\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			code:    http.StatusConflict,
			mention: "file.txt changed since the agent read it",
			after: func(t *testing.T, srv *Server, repoDir string) {
				if got, err := os.ReadFile(filepath.Join(repoDir, "file.txt")); err != nil || string(got) != "alpha\nOPERATOR-EDITING\ncharlie\n" {
					t.Fatalf("file.txt = %q, %v; the operator's edit must stand", got, err)
				}
				if unmerged := gittest.Run(t, repoDir, "ls-files", "--unmerged", "--", "file.txt"); unmerged == "" {
					t.Fatal("file.txt was staged over the operator's edit")
				}
			},
		},
		{
			name: "another run's identical conflict",
			meanwhile: func(t *testing.T, srv *Server, hsURL, repoDir string) {
				resp := postJSON(t, hsURL+"/api/runs/run-agent-race/merge/conflicts/abort", nil)
				if body := readBody(t, resp); resp.StatusCode != http.StatusNoContent {
					t.Fatalf("the abort answered %d %s", resp.StatusCode, body)
				}
				seedConflictRun(t, srv, repoDir, "run-agent-race-other")
				resp = postJSON(t, hsURL+"/api/runs/run-agent-race-other/merge", map[string]any{})
				if body := readBody(t, resp); resp.StatusCode != http.StatusConflict {
					t.Fatalf("the other run's merge answered %d %s; want its conflict", resp.StatusCode, body)
				}
			},
			code:    http.StatusConflict,
			mention: "is no longer conflicted (merge_status=",
			after: func(t *testing.T, srv *Server, repoDir string) {
				if unmerged := gittest.Run(t, repoDir, "ls-files", "--unmerged", "--", "file.txt"); unmerged == "" {
					t.Fatal("the agent of an aborted run staged its answer into another run's conflict")
				}
				if got := loadRun(t, srv, "run-agent-race-other").MergeStatus; got != store.MergeStatusConflicted {
					t.Fatalf("the other run's merge_status=%q; want conflicted", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := newAgentModelGate(t, agent)
			srv, hs := newTestServer(t)
			repoDir := setupConflictingRepo(t)
			seedConflictRun(t, srv, repoDir, "run-agent-race")
			resp := postJSON(t, hs.URL+"/api/runs/run-agent-race/merge", map[string]any{})
			if body := readBody(t, resp); resp.StatusCode != http.StatusConflict {
				t.Fatalf("the run's merge answered %d %s; want its conflict", resp.StatusCode, body)
			}

			done := startAgentResolve(t, hs.URL, "run-agent-race", model)
			tc.meanwhile(t, srv, hs.URL, repoDir)
			model.let()
			got := <-done
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.code != tc.code || !strings.Contains(got.body, tc.mention) {
				t.Fatalf("the agent resolve answered %d %s; want %d naming %q", got.code, got.body, tc.code, tc.mention)
			}
			tc.after(t, srv, repoDir)
		})
	}
}

// requireFileAndIndex checks file.txt's bytes in repoDir's worktree and in
// its index.
func requireFileAndIndex(t *testing.T, repoDir, onDisk, staged string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(repoDir, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != onDisk {
		t.Fatalf("file.txt on disk = %q; want %q", got, onDisk)
	}
	if index := gittest.Run(t, repoDir, "show", ":file.txt"); index != strings.TrimSuffix(staged, "\n") {
		t.Fatalf("file.txt staged = %q; want %q", index, staged)
	}
}
