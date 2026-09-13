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
// is substituted as the runtime substitutes it, as one single-quoted word.
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
	// Pending work: a modified tracked file and an untracked file, counted
	// with nothing staged (the index stays as it was).
	write("tracked.txt", "two\n")
	write("new.txt", "hello\n")
	if got := gate("pending work", ""); got.Empty || got.Files != 2 {
		t.Errorf("pending work: %+v, want 2 files", got)
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
}
