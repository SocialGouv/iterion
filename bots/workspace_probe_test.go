package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shellQuote wraps a value so `sh -c` passes it through verbatim, the way
// the engine's shellEscapeValue renders a {{vars.X}} ref into a command.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// hermeticGitEnv keeps the test's git calls off the operator's own config
// (a global commit.gpgsign or a required identity would otherwise leak in).
func hermeticGitEnv(t *testing.T) []string {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+empty,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=iterion-test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=iterion-test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
}

// initRepo creates a git repository with one commit on branch `main`.
func initRepo(t *testing.T, env []string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"symbolic-ref", "HEAD", "refs/heads/main"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	return dir
}

// TestWorkspaceProbeRefusesTypedBeforeAnyLLM executes each repo-requiring
// campaign bot's REAL workspace_probe command — the precondition ahead of
// the first LLM node — against the two silent shapes it exists to refuse:
//
//  1. workspace_dir absent, or present but not a git repository;
//  2. a repository in which the bot's base ref is not reachable from HEAD
//     (only for a bot that declares base_ref — its diff-anchored mission
//     would otherwise plan and review against a range that does not exist).
//
// Both must answer ok=false with the typed code WORKSPACE_NOT_A_REPO, on a
// ZERO exit status: the verdict is the node's output (routed to `fail` by
// the graph), and a non-zero exit would replace it with the engine's
// generic tool failure. A real repository with a reachable base passes.
func TestWorkspaceProbeRefusesTypedBeforeAnyLLM(t *testing.T) {
	for _, bin := range []string{"python3", "git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	env := hermeticGitEnv(t)

	type verdict struct {
		OK     bool   `json:"ok"`
		Code   string `json:"code"`
		Reason string `json:"reason"`
	}
	for _, bot := range repoRequiringCampaignBots {
		t.Run(bot, func(t *testing.T) {
			body := toolCommand(t, bot+"/main.bot", "workspace_probe")
			_, hasBase := compilePlanPhaseBot(t, bot).Vars["base_ref"]

			run := func(t *testing.T, ws, base string) verdict {
				t.Helper()
				cmd := body
				if !strings.Contains(cmd, "{{vars.workspace_dir}}") {
					t.Fatal("workspace_probe no longer reads {{vars.workspace_dir}} — the test wires nothing")
				}
				cmd = strings.ReplaceAll(cmd, "{{vars.workspace_dir}}", shellQuote(ws))
				cmd = strings.ReplaceAll(cmd, "{{vars.base_ref}}", shellQuote(base))
				c := exec.Command("sh", "-c", cmd)
				c.Env = env
				var stderr strings.Builder
				c.Stderr = &stderr
				out, err := c.Output()
				if err != nil {
					t.Fatalf("workspace_probe exited non-zero (%v) — the typed verdict must ride the node OUTPUT, never a tool failure; stdout %q stderr %q", err, out, stderr.String())
				}
				var v verdict
				if err := json.Unmarshal(out, &v); err != nil {
					t.Fatalf("output is not workspace_probe_state JSON: %v (%q)", err, out)
				}
				if !v.OK && !strings.Contains(stderr.String(), "WORKSPACE_NOT_A_REPO") {
					t.Errorf("a refusal must also name its code on stderr (the tool log), got %q", stderr.String())
				}
				return v
			}
			refused := func(t *testing.T, v verdict, mention string) {
				t.Helper()
				if v.OK {
					t.Fatalf("ok=true, want a refusal (reason %q)", v.Reason)
				}
				if v.Code != "WORKSPACE_NOT_A_REPO" {
					t.Errorf("code = %q, want WORKSPACE_NOT_A_REPO", v.Code)
				}
				if !strings.Contains(v.Reason, mention) {
					t.Errorf("reason %q does not name %q — the operator cannot see what was refused", v.Reason, mention)
				}
			}

			t.Run("absent workspace_dir", func(t *testing.T) {
				absent := filepath.Join(t.TempDir(), "nowhere")
				refused(t, run(t, absent, "main"), absent)
			})
			t.Run("directory that is not a repository", func(t *testing.T) {
				plain := t.TempDir()
				refused(t, run(t, plain, "main"), plain)
			})
			t.Run("repository with a reachable base passes", func(t *testing.T) {
				v := run(t, initRepo(t, env), "main")
				if !v.OK || v.Code != "" {
					t.Fatalf("a real repository was refused: %+v", v)
				}
			})
			if hasBase {
				t.Run("repository whose base_ref is unreachable", func(t *testing.T) {
					refused(t, run(t, initRepo(t, env), "no-such-base"), "no-such-base")
				})
			}
		})
	}
}
