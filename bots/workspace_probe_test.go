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

// gitHermetic runs one git command in dir under the hermetic env, failing
// the test on a non-zero exit.
func gitHermetic(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v (%s)", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
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
		gitHermetic(t, env, dir, args...)
	}
	return dir
}

// commitFile adds one file to dir and commits it.
func commitFile(t *testing.T, env []string, dir, name, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitHermetic(t, env, dir, "add", "--", name)
	gitHermetic(t, env, dir, "commit", "-q", "-m", msg)
}

// runnerStyleClone reproduces the workspace a cloud PR run gets from
// pkg/runner/loop_gitws.go: `git clone --no-tags` of the repository (only
// its default branch `main` becomes a local branch), then `git fetch origin
// <head>` + `git checkout -B <head> FETCH_HEAD` for the PR head. The PR's
// base `develop` — the branch base_ref is stamped from — exists in that
// checkout ONLY as refs/remotes/origin/develop. Returns the workspace.
func runnerStyleClone(t *testing.T, env []string) string {
	t.Helper()
	upstream := initRepo(t, env)
	gitHermetic(t, env, upstream, "checkout", "-q", "-b", "develop")
	commitFile(t, env, upstream, "base.txt", "base work")
	gitHermetic(t, env, upstream, "checkout", "-q", "-b", "feature")
	commitFile(t, env, upstream, "feature.txt", "feature work")
	gitHermetic(t, env, upstream, "checkout", "-q", "main")

	bare := filepath.Join(t.TempDir(), "remote.git")
	gitHermetic(t, env, ".", "clone", "-q", "--bare", upstream, bare)

	ws := filepath.Join(t.TempDir(), "ws")
	gitHermetic(t, env, ".", "clone", "-q", "--no-tags", bare, ws)
	gitHermetic(t, env, ws, "fetch", "--no-tags", "-q", "origin", "feature")
	gitHermetic(t, env, ws, "checkout", "-q", "-B", "feature", "FETCH_HEAD")

	// The fixture must have the runner's shape, or the case proves nothing.
	if out, err := exec.Command("git", "-C", ws, "rev-parse", "--verify", "-q", "develop^{commit}").CombinedOutput(); err == nil {
		t.Fatalf("fixture drift: `develop` resolves as a LOCAL ref in the runner-style clone (%s)", out)
	}
	gitHermetic(t, env, ws, "rev-parse", "--verify", "-q", "refs/remotes/origin/develop^{commit}")
	return ws
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
				t.Run("repository whose base_ref exists nowhere names both refs tried", func(t *testing.T) {
					v := run(t, initRepo(t, env), "no-such-base")
					refused(t, v, "no-such-base")
					if !strings.Contains(v.Reason, "refs/remotes/origin/no-such-base") {
						t.Errorf("reason %q does not name the remote-tracking ref the probe also tried", v.Reason)
					}
				})
				// A cloud PR run: the runner's clone carries the PR's base
				// only as a remote-tracking ref (see runnerStyleClone). The
				// probe must resolve it there — never refuse a valid run, and
				// never fetch to find out.
				t.Run("base that exists only as origin/<base> in the runner's clone passes", func(t *testing.T) {
					v := run(t, runnerStyleClone(t, env), "develop")
					if !v.OK {
						t.Fatalf("a valid cloud PR workspace was refused: %+v", v)
					}
					if !strings.Contains(v.Reason, "refs/remotes/origin/develop") {
						t.Errorf("reason %q does not name the ref the base resolved to", v.Reason)
					}
				})
			}
		})
	}
}

// TestPlanScopeProbeResolvesARemoteOnlyBase executes branch-improve-loop's
// REAL plan_scope_probe command — the deterministic diff-footprint pre-flight
// of the plan phase — on the runner-style clone: the branch diff must be
// measured against the base even when that base exists only as
// refs/remotes/origin/<base>, the same resolution the entry probe applies. A
// bare `git merge-base develop HEAD` fails there, and a probe that quietly
// diffs against the bare name reports an EMPTY footprint (large=false) for a
// diff it never saw.
func TestPlanScopeProbeResolvesARemoteOnlyBase(t *testing.T) {
	for _, bin := range []string{"python3", "git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	env := hermeticGitEnv(t)
	body := toolCommand(t, "branch-improve-loop/main.bot", "plan_scope_probe")
	for ref, val := range map[string]string{
		"{{vars.workspace_dir}}":         runnerStyleClone(t, env),
		"{{vars.base_ref}}":              "develop",
		"{{vars.plan_large_diff_lines}}": "1500",
	} {
		if !strings.Contains(body, ref) {
			t.Fatalf("%s is no longer referenced by plan_scope_probe — the test wires nothing", ref)
		}
		body = strings.ReplaceAll(body, ref, shellQuote(val))
	}
	c := exec.Command("sh", "-c", body)
	c.Env = env
	out, err := c.Output()
	if err != nil {
		t.Fatalf("plan_scope_probe failed: %v (out %q)", err, out)
	}
	var res struct {
		DiffStat string `json:"diff_stat"`
		Large    bool   `json:"large"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output is not plan_scope_state JSON: %v (%q)", err, out)
	}
	if !strings.Contains(res.DiffStat, "feature.txt") {
		t.Errorf("diff_stat = %q, want the branch's own change (feature.txt) measured against the remote-only base", res.DiffStat)
	}
	if strings.Contains(res.DiffStat, "base.txt") {
		t.Errorf("diff_stat = %q, includes base.txt — the footprint was measured against the wrong base", res.DiffStat)
	}
	if res.Large {
		t.Errorf("large = true for a one-file branch")
	}
}
