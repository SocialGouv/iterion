//go:build clilive

// Real-CLI proof of codex's ambient-context translation (ADR-119) — gated by
// the `clilive` build tag because it needs the `codex` CLI on PATH and a
// route to api.openai.com. It costs nothing: codex authenticates with a
// deliberately invalid key, writes its session rollout with the instructions
// it loaded, and only then fails. Run with:
//
//	devbox run -- go test -tags clilive -run TestCLILive -v ./pkg/backend/delegate/
package delegate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	"github.com/SocialGouv/iterion/pkg/git"
)

func TestCLILiveCodexAmbientPolicies(t *testing.T) {
	cli, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex CLI not on PATH")
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(tmp, "codex-home")
	repo := filepath.Join(tmp, "repo")
	mustWriteFile(t, filepath.Join(codexHome, "AGENTS.md"), "MARKER-CODEX-OPERATOR-K1\n")
	mustWriteFile(t, filepath.Join(repo, "AGENTS.md"), "MARKER-CODEX-REPO-K2\n")
	if out, err := exec.Command("git", git.NoAutoMaintenance("init", "-q", repo)...).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	for _, c := range []struct {
		policy                 ambient.Policy
		wantOperator, wantRepo bool
	}{
		{ambient.All, true, true},
		{ambient.Workspace, false, true},
		{ambient.Operator, true, false},
		{ambient.None, false, false},
	} {
		t.Run(c.policy.String(), func(t *testing.T) {
			task := Task{WorkDir: repo, AmbientContext: c.policy}
			home, release := codexHome, func() error { return nil }
			if !c.policy.IncludesOperator() {
				if home, release, err = codexHomeWithoutInstructions(codexHome); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"exec", "--skip-git-repo-check"}
			cfg := codexConfig(task, codexWebSearchModeDisabled)
			keys := make([]string, 0, len(cfg))
			for k := range cfg {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				args = append(args, "-c", k+"="+cfg[k])
			}
			args = append(args, "hi")

			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, cli, args...)
			cmd.Dir = repo
			cmd.Env = append(cliliveCodexEnv(), "CODEX_HOME="+home, "OPENAI_API_KEY=sk-invalid-clilive")
			_ = cmd.Run() // the invalid key ends the run
			if err := release(); err != nil {
				t.Fatalf("release the overlay home: %v", err)
			}

			rollout := newestTranscript(t, filepath.Join(codexHome, "sessions"))
			if got := strings.Contains(rollout, "MARKER-CODEX-OPERATOR-K1"); got != c.wantOperator {
				t.Errorf("the operator's AGENTS.md reached the session = %v, want %v", got, c.wantOperator)
			}
			if got := strings.Contains(rollout, "MARKER-CODEX-REPO-K2"); got != c.wantRepo {
				t.Errorf("the repository's AGENTS.md reached the session = %v, want %v", got, c.wantRepo)
			}
		})
	}
}

// cliliveCodexEnv drops the operator's codex and OpenAI credentials, so the
// invalid key is what codex uses.
func cliliveCodexEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "CODEX_HOME" || strings.HasPrefix(name, "OPENAI_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}
