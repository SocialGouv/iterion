package botscaffold

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestReviewFanoutScopeGateCountsTheScope executes the review-fanout
// shape's scope gate in a real repository: a clean tree is an EMPTY scope
// (the typed refusal, never an approve), pending work — a modified tracked
// file and an untracked one — is counted without staging anything, and a
// base ref counts the committed work since it. The {{vars.base}} reference
// is substituted as the runtime substitutes it, as one single-quoted word;
// the tree-noise exclusion arrives through the environment, as it does
// for every tool process the engine spawns.
func TestReviewFanoutScopeGateCountsTheScope(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shape's commands are POSIX shell")
	}
	for _, bin := range []string{"bash", "git", "xargs"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	tpl, ok := TemplateByID("review-fanout")
	if !ok {
		t.Fatal("no review-fanout template")
	}
	spec := tpl.Spec
	spec.Slug = "lenses"
	_, w, _ := scaffoldAndCompile(t, spec)
	var scope *ir.ToolNode
	for _, n := range nodesOf[*ir.ToolNode](w) {
		if n.ID == "scope" {
			scope = n
		}
	}
	if scope == nil {
		t.Fatal("no scope tool")
	}

	repo := t.TempDir()
	type scopeOut struct {
		Files int  `json:"files"`
		Empty bool `json:"empty"`
	}
	// The exclusion travels on the executable channel only: the shell the
	// gate runs in carries ITERION_TREE_NOISE (shellInRepo, as the engine
	// does), and a template that regressed to the prompt rendering would
	// render one inert argument at run time — named here, not masked by a
	// substitution the runtime never produces.
	if !strings.Contains(scope.Command, "$ITERION_TREE_NOISE") {
		t.Fatalf("the scope gate does not read $ITERION_TREE_NOISE: %q", scope.Command)
	}
	if strings.Contains(scope.Command, "{{run.tree_noise}}") {
		t.Fatalf("the scope gate embeds the prompt rendering {{run.tree_noise}} in a tool command, where it shell-escapes to one inert argument: %q", scope.Command)
	}
	gate := func(state, base string) scopeOut {
		t.Helper()
		command := strings.ReplaceAll(scope.Command, "{{vars.base}}", "'"+base+"'")
		if command == scope.Command {
			t.Fatalf("%q no longer reads {{vars.base}}", scope.Command)
		}
		out, stderr, err := shellInRepo(repo, command)
		if err != nil {
			t.Fatalf("%s: scope gate failed: %v\n%s", state, err, stderr)
		}
		var got scopeOut
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("%s: scope stdout = %q, want the JSON the schema wants: %v", state, out, err)
		}
		return got
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	gittest.Run(t, repo, "init", "-q")
	write("tracked.txt", "one\n")
	gittest.Run(t, repo, "add", "tracked.txt")
	gittest.Run(t, repo, "commit", "-q", "-m", "one")

	if got := gate("clean tree", ""); !got.Empty || got.Files != 0 {
		t.Errorf("clean tree: %+v, want empty", got)
	}
	// The engine's mirror alone, on a repository that does not ignore
	// .claude/: still an empty scope — the typed refusal stays reachable,
	// and the mirror never reaches a reviewer as pending work.
	if err := os.MkdirAll(filepath.Join(repo, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(".claude", "skills", "mirrored.md"), "iterion wrote this\n")
	if got := gate("mirror only", ""); !got.Empty || got.Files != 0 {
		t.Errorf("the engine's mirror alone: %+v, want empty (the mirror is tree noise, not pending work)", got)
	}
	// A tracked devbox.lock the run's own tooling rewrote (#1459): tree
	// noise on the TRACKED side, which only the `git diff` half of the gate
	// sees — the untracked mirror above never reaches it. Still an empty
	// scope, with and without a base.
	write("devbox.lock", "plugin_version: 0.0.4\n")
	gittest.Run(t, repo, "add", "devbox.lock")
	gittest.Run(t, repo, "commit", "-q", "-m", "the lock")
	write("devbox.lock", "plugin_version: 0.0.5\n")
	if got := gate("rewritten tracked lock", ""); !got.Empty || got.Files != 0 {
		t.Errorf("a rewritten tracked devbox.lock: %+v, want empty (the drift is tree noise, not pending work)", got)
	}
	if got := gate("rewritten tracked lock, base HEAD", "HEAD"); !got.Empty || got.Files != 0 {
		t.Errorf("a rewritten tracked devbox.lock against base HEAD: %+v, want empty", got)
	}
	// Pending work: a modified tracked file and an untracked file, counted
	// with nothing staged (the index stays as it was). The drifted lock
	// beside them still does not count.
	write("tracked.txt", "two\n")
	write("new.txt", "hello\n")
	if got := gate("pending work", ""); got.Empty || got.Files != 2 {
		t.Errorf("pending work: %+v, want 2 files (the mirror beside them does not count)", got)
	}
	if staged := gittest.Run(t, repo, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("the scope gate staged something: %q", staged)
	}
	// Committed work against a base ref — the shape's scope under
	// `worktree: auto`, where the pending work is not in the checkout.
	gittest.Run(t, repo, "add", "tracked.txt", "new.txt")
	gittest.Run(t, repo, "commit", "-q", "-m", "two")
	if got := gate("clean tree, no base", ""); !got.Empty {
		t.Errorf("clean tree after the commit: %+v, want empty", got)
	}
	if got := gate("base ref", "HEAD~1"); got.Empty || got.Files != 2 {
		t.Errorf("base ref: %+v, want the 2 committed files", got)
	}
	// An unresolvable base must FAIL the gate: swallowing git's error into
	// {"files":0,"empty":true} would take the typed refusal on a lie ("no
	// changed file in the scope" when the truth is "no such base").
	broken := strings.ReplaceAll(scope.Command, "{{vars.base}}", "'origin/nope'")
	if out, stderr, err := shellInRepo(repo, broken); err == nil {
		t.Errorf("an unresolvable base ref must fail the gate, got a verdict %q (stderr %q)", out, stderr)
	}
}
