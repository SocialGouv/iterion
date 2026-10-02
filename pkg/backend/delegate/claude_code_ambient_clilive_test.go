//go:build clilive

// Real-CLI proof of the ambient-context translation (ADR-119) — gated by the
// `clilive` build tag because it needs the `claude` CLI on PATH and a route
// to api.anthropic.com. It costs nothing: the CLI authenticates with a
// deliberately invalid key, assembles its context, writes the session
// transcript, and only then gets a 401. Run with:
//
//	devbox run -- go test -tags clilive -run TestCLILive -v ./pkg/backend/delegate/
//
// The test feeds the CLI the scopes and the flag settings object iterion's
// own functions produce (claudeAmbient, claudeFlagSettings), then reads which
// planted markers reached the transcript. The argv the SDK assembles around
// them is covered without the CLI by TestClaudeSpawnsCarryTheAmbientPolicy.
package delegate

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	"github.com/SocialGouv/iterion/pkg/git"
)

func TestCLILiveClaudeAmbientPolicies(t *testing.T) {
	cli, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude CLI not on PATH")
	}
	unsetSettingSourcesEnv(t)

	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(tmp, "home")
	repo := filepath.Join(home, "lab", "repo")
	plant := map[string]string{
		filepath.Join(home, ".claude", "CLAUDE.md"):                         "MARKER-OPERATOR-MEMORY-Q1",
		filepath.Join(home, ".claude", "rules", "op.md"):                    "MARKER-OPERATOR-RULE-Q2",
		filepath.Join(home, "lab", "CLAUDE.md"):                             "MARKER-ANCESTOR-Q3",
		filepath.Join(repo, "CLAUDE.md"):                                    "MARKER-REPO-MEMORY-Q4",
		filepath.Join(repo, ".claude", "rules", "repo.md"):                  "MARKER-REPO-RULE-Q5",
		filepath.Join(repo, ".claude", "skills", "probe-skill", "SKILL.md"): "---\nname: probe-skill\ndescription: MARKER-REPO-SKILL-Q6\n---\nbody\n",
	}
	for path, content := range plant {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", git.NoAutoMaintenance("init", "-q", repo)...).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	markers := []string{"MARKER-OPERATOR-MEMORY-Q1", "MARKER-OPERATOR-RULE-Q2", "MARKER-ANCESTOR-Q3", "MARKER-REPO-MEMORY-Q4", "MARKER-REPO-RULE-Q5", "MARKER-REPO-SKILL-Q6"}
	operator, workspace, skill := markers[:3], markers[3:5], markers[5]
	cases := []struct {
		policy        ambient.Policy
		wantOperator  bool
		wantWorkspace bool
	}{
		{ambient.All, true, true},
		{ambient.Workspace, false, true},
		{ambient.Operator, true, false},
		{ambient.None, false, false},
	}
	for _, c := range cases {
		t.Run(c.policy.String(), func(t *testing.T) {
			// Each policy starts a new session: newestTranscript reads its
			// transcript, so nothing is ever deleted between cases.
			amb := claudeAmbient(Task{WorkDir: repo, AmbientContext: c.policy})
			raw, err := claudeFlagSettings(map[string]string{}, nil, amb.excludes)
			if err != nil {
				t.Fatal(err)
			}
			var settings map[string]any
			if err := json.Unmarshal(raw, &settings); err != nil {
				t.Fatal(err)
			}
			settings["apiKeyHelper"] = "echo sk-ant-invalid-clilive"
			flagSettings, _ := json.Marshal(settings)
			sources := make([]string, len(amb.sources))
			for i, s := range amb.sources {
				sources[i] = string(s)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, cli, "-p", "hi",
				"--setting-sources", strings.Join(sources, ","),
				"--settings", string(flagSettings))
			cmd.Dir = repo
			cmd.Env = cliliveEnv(home)
			_ = cmd.Run() // a 401 (or the timeout) is the expected end

			transcript := newestTranscript(t, filepath.Join(home, ".claude", "projects"))
			for _, m := range operator {
				if got := strings.Contains(transcript, m); got != c.wantOperator {
					t.Errorf("%s reached the session = %v, want %v", m, got, c.wantOperator)
				}
			}
			for _, m := range workspace {
				if got := strings.Contains(transcript, m); got != c.wantWorkspace {
					t.Errorf("%s reached the session = %v, want %v", m, got, c.wantWorkspace)
				}
			}
			if !strings.Contains(transcript, skill) {
				t.Errorf("%s (a project skill) is missing: the project scope must load under every policy", skill)
			}
		})
	}
}

// cliliveEnv is the CLI's environment: the fake home, and none of the
// operator's credentials, so the invalid key from apiKeyHelper is what the
// CLI uses.
func cliliveEnv(home string) []string {
	env := []string{"HOME=" + home}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case name == "HOME", name == "CLAUDE_CONFIG_DIR", strings.HasPrefix(name, "ANTHROPIC_"), strings.HasPrefix(name, "CLAUDE_CODE_"):
			continue
		}
		env = append(env, kv)
	}
	return env
}

// newestTranscript returns the content of the most recent session transcript.
func newestTranscript(t *testing.T, root string) string {
	t.Helper()
	var newest string
	var newestMod time.Time
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(newestMod) {
			newest, newestMod = path, info.ModTime()
		}
		return nil
	})
	if newest == "" {
		t.Fatalf("no session transcript under %s: the CLI did not start a session", root)
	}
	b, err := os.ReadFile(newest)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
