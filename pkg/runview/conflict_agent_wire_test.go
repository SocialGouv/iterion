package runview

import (
	"context"
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
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The merge-conflict resolver sends its model's wire id whole, through the
// real claw client: the routing prefix comes off once, in the model
// package, so a model id's own slashes ("meta-llama/…") reach the provider.
// Red when the resolver strips the prefix itself before handing the spec on.
func TestResolveConflictsWithAgent_SendsTheWireIDWhole(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("OPENAI_API_KEY", "test-key")

	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	repoDir := filepath.Join(dir, "repo")
	logger := iterlog.Nop()
	st, err := store.New(storeDir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runGit := func(args ...string) {
		t.Helper()
		gittest.Run(t, repoDir, args...)
	}
	writeRepo := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("write file.txt: %v", err)
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "t@t.t")
	runGit("config", "user.name", "t")
	runGit("config", "commit.gpgsign", "false")
	writeRepo("alpha\nbravo\ncharlie\n")
	runGit("add", "file.txt")
	runGit("commit", "-qm", "base")
	baseSHA := strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))
	runGit("checkout", "-qb", "iterion/run/test-wire")
	writeRepo("alpha\nBRAVO-INCOMING\ncharlie\n")
	runGit("commit", "-qam", "feat")
	storageSHA := strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))
	runGit("checkout", "-q", "main")
	writeRepo("alpha\nbravo-main\ncharlie\n")
	runGit("commit", "-qam", "main-change")

	ctx := context.Background()
	runID := "run-conflict-wire"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.Worktree = true
	r.RepoRoot = repoDir
	r.WorkDir = repoDir
	r.BaseCommit = baseSHA
	r.FinalCommit = storageSHA
	r.FinalBranch = "iterion/run/test-wire"
	r.Status = store.RunStatusFinished
	r.MergeStrategy = store.MergeStrategySquash
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	svc, err := NewService(storeDir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.PerformMergeCtx(ctx, runID, MergeRequest{}); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("fixture: the merge did not conflict (err=%v)", err)
	}

	// The answer holds no resolution, so the resolver fails after the call;
	// the request it sent is what this test reads.
	_, _ = svc.resolveAllConflictsWithAgent(ctx, runID, "openai/meta-llama/Llama-3.3-70B")

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the resolver sent no request")
	}
	for _, m := range seen {
		if m != "meta-llama/Llama-3.3-70B" {
			t.Errorf("wire model %q, want meta-llama/Llama-3.3-70B", m)
		}
	}
}
