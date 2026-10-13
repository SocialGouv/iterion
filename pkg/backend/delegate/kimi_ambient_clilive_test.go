//go:build clilive

// Real-CLI proof of kimi's ambient-context translation (ADR-119) — gated by
// the `clilive` build tag because it needs Moonshot's `kimi` CLI (kimi-code
// 2.1) on PATH. It costs nothing and leaves the machine: kimi's env-model
// variables point it at an in-process server that records the request, whose
// system prompt carries the instruction files kimi loaded, and answers 401.
// Run with:
//
//	devbox run -- go test -tags clilive -run TestCLILiveKimi -v ./pkg/backend/delegate/
//
// Each case goes through the backend's own Execute, so what is proven is the
// spawn iterion makes, overlay home included.
package delegate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	"github.com/SocialGouv/iterion/pkg/git"
)

func TestCLILiveKimiAmbientPolicies(t *testing.T) {
	cli, err := exec.LookPath("kimi")
	if err != nil {
		t.Skip("kimi CLI not on PATH")
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	userHome := filepath.Join(tmp, "home")
	kimiHome := filepath.Join(tmp, "kimi-home")
	repo := filepath.Join(tmp, "lab", "repo")
	workDir := filepath.Join(repo, "sub")
	plant := map[string]string{
		filepath.Join(kimiHome, "AGENTS.md"):           "MARKER-KIMI-HOME-K1",
		filepath.Join(tmp, "lab", "AGENTS.md"):         "MARKER-KIMI-ABOVE-ROOT-K3",
		filepath.Join(repo, "AGENTS.md"):               "MARKER-KIMI-REPO-K4",
		filepath.Join(repo, ".kimi-code", "AGENTS.md"): "MARKER-KIMI-REPO-DOTDIR-K5",
		filepath.Join(workDir, "agents.md"):            "MARKER-KIMI-SUB-K6",
		filepath.Join(repo, "CLAUDE.md"):               "MARKER-KIMI-CLAUDE-K7",
		filepath.Join(workDir, "deeper", "AGENTS.md"):  "MARKER-KIMI-BELOW-CWD-K8",
	}
	for path, content := range plant {
		mustWriteFile(t, path, content+"\n")
	}
	if err := os.MkdirAll(userHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", git.NoAutoMaintenance("init", "-q", repo)...).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key (clilive)","type":"invalid_request_error","code":"invalid_api_key"}}`)
	}))
	defer server.Close()
	cliliveKimiEnv(t, userHome, kimiHome, server.URL+"/v1")

	// request runs one node through Execute and returns what reached the model.
	request := func(t *testing.T, policy ambient.Policy, source string) string {
		t.Helper()
		mu.Lock()
		bodies = nil
		mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		task := Task{NodeID: "probe", UserPrompt: "hi", WorkDir: workDir, AmbientContext: policy, AmbientContextSource: source}
		_, _ = NewKimiBackend(testLogger(), cli).Execute(ctx, task) // the 401 ends the run
		mu.Lock()
		defer mu.Unlock()
		if len(bodies) == 0 {
			t.Fatal("kimi sent no request: it never assembled its context")
		}
		return strings.Join(bodies, "\n")
	}
	assertMarkers := func(t *testing.T, got string, want map[string]bool) {
		t.Helper()
		for marker, wanted := range want {
			if reached := strings.Contains(got, marker); reached != wanted {
				t.Errorf("%s reached the model = %v, want %v", marker, reached, wanted)
			}
		}
	}
	workspace := map[string]bool{"MARKER-KIMI-REPO-K4": true, "MARKER-KIMI-REPO-DOTDIR-K5": true, "MARKER-KIMI-SUB-K6": true}
	never := map[string]bool{"MARKER-KIMI-ABOVE-ROOT-K3": false, "MARKER-KIMI-CLAUDE-K7": false, "MARKER-KIMI-BELOW-CWD-K8": false}

	t.Run("all is kimi's own loading", func(t *testing.T) {
		got := request(t, ambient.All, "node")
		assertMarkers(t, got, map[string]bool{"MARKER-KIMI-HOME-K1": true})
		assertMarkers(t, got, workspace)
		assertMarkers(t, got, never)
	})
	t.Run("workspace withholds the kimi home's AGENTS.md", func(t *testing.T) {
		got := request(t, ambient.Workspace, "node")
		assertMarkers(t, got, map[string]bool{"MARKER-KIMI-HOME-K1": false})
		assertMarkers(t, got, workspace)
		assertMarkers(t, got, never)
	})
	// The measurements behind the refusals: what no overlay of the kimi home
	// can withhold.
	t.Run("the repository's files load whatever home kimi is served", func(t *testing.T) {
		// The workspace run above already used the overlay; this one points
		// kimi at an empty home of its own.
		empty := filepath.Join(tmp, "empty-kimi-home")
		if err := os.MkdirAll(empty, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KIMI_CODE_HOME", empty)
		assertMarkers(t, request(t, ambient.All, "node"), workspace)
	})
	t.Run("the user home's .agents/AGENTS.md loads whatever home kimi is served", func(t *testing.T) {
		mustWriteFile(t, filepath.Join(userHome, ".agents", "AGENTS.md"), "MARKER-KIMI-GENERIC-K2\n")
		defer os.Remove(filepath.Join(userHome, ".agents", "AGENTS.md"))
		got := request(t, ambient.Workspace, "default") // the default runs, warned
		assertMarkers(t, got, map[string]bool{"MARKER-KIMI-GENERIC-K2": true, "MARKER-KIMI-HOME-K1": false})
		if _, err := NewKimiBackend(testLogger(), cli).Execute(context.Background(), Task{
			NodeID: "probe", UserPrompt: "hi", WorkDir: workDir, AmbientContext: ambient.Workspace, AmbientContextSource: "node",
		}); err == nil || !strings.Contains(err.Error(), "cannot serve ambient_context: workspace (from node)") {
			t.Errorf("a declared workspace must be refused while that file exists, got %v", err)
		}
	})
}

func TestCLILiveKimiStopsAtANestedWorktreesRoot(t *testing.T) {
	cli, err := exec.LookPath("kimi")
	if err != nil {
		t.Skip("kimi CLI not on PATH")
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(tmp, "main")
	mustWriteFile(t, filepath.Join(main, "AGENTS.md"), "MARKER-KIMI-MAIN-CHECKOUT-W1\n")
	for _, args := range [][]string{
		{"init", "-q", main},
		{"-C", main, "-c", "user.email=clilive@example.invalid", "-c", "user.name=clilive", "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", main, "worktree", "add", "-q", filepath.Join(main, ".claude", "worktrees", "wt")},
	} {
		if out, err := exec.Command("git", git.NoAutoMaintenance(args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	wt := filepath.Join(main, ".claude", "worktrees", "wt")
	mustWriteFile(t, filepath.Join(wt, "AGENTS.md"), "MARKER-KIMI-WORKTREE-W2\n")

	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	cliliveKimiEnv(t, filepath.Join(tmp, "home"), filepath.Join(tmp, "kimi-home"), server.URL+"/v1")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, _ = NewKimiBackend(testLogger(), cli).Execute(ctx, Task{NodeID: "probe", UserPrompt: "hi", WorkDir: wt, AmbientContext: ambient.Workspace})
	mu.Lock()
	got := strings.Join(bodies, "\n")
	mu.Unlock()
	if got == "" {
		t.Fatal("kimi sent no request")
	}
	if !strings.Contains(got, "MARKER-KIMI-WORKTREE-W2") {
		t.Error("the worktree's own AGENTS.md did not reach the model")
	}
	if strings.Contains(got, "MARKER-KIMI-MAIN-CHECKOUT-W1") {
		t.Error("kimi walked past the worktree's root into its main checkout")
	}
}

// cliliveKimiEnv gives kimi a fake user home and kimi home, points its
// env-model at baseURL with a deliberately invalid key, and blanks the
// operator's own kimi and Moonshot variables so none of them decides.
func cliliveKimiEnv(t *testing.T, userHome, kimiHome, baseURL string) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "KIMI_") || strings.HasPrefix(name, "MOONSHOT_") {
			t.Setenv(name, "")
		}
	}
	for name, value := range map[string]string{
		"HOME":                     userHome,
		"KIMI_CODE_HOME":           kimiHome,
		"KIMI_MODEL_NAME":          "clilive-probe",
		"KIMI_MODEL_API_KEY":       "sk-invalid-clilive",
		"KIMI_MODEL_PROVIDER_TYPE": "openai",
		"KIMI_MODEL_BASE_URL":      baseURL,
		"KIMI_DISABLE_TELEMETRY":   "1",
		"KIMI_CODE_NO_AUTO_UPDATE": "1",
		"KIMI_CLI_NO_AUTO_UPDATE":  "1",
	} {
		t.Setenv(name, value)
	}
}
