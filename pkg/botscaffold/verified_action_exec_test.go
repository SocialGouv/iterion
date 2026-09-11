package botscaffold

import (
	"bytes"
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

// shellInRepo runs one shipped command text through `bash -c` in repo the
// way a tool node runs it (executor_tool.go's toolNodeCommand), under the
// tests' scrubbed git environment — so the operator's own gitconfig (a
// tag.gpgSign, a hooksPath) cannot decide the verdict — and with git's
// auto-maintenance refused the way gittest refuses it, since the recipe's
// `git tag` is a writing command.
func shellInRepo(repo, command string) (stdout, stderr string, err error) {
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = repo
	cmd.Env = append(gittest.Env(),
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=maintenance.auto", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=gc.auto", "GIT_CONFIG_VALUE_1=0",
	)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

// TestVerifiedActionPostconditionAssertsTheGoal executes the
// verified-action shape's recipe and postcondition in a real repository.
// The postcondition is the truth at every rung, the idempotent skip and
// the self-repair included, so it must assert the GOAL — an ANNOTATED tag
// on HEAD — and none of the weaker states a rung could otherwise pass off
// as success: a tag that merely exists, one an earlier run left on an
// earlier commit, or a lightweight `git tag <name>` a repair could reach
// for. The {{vars.tag}} reference is substituted as the runtime
// substitutes it, as one single-quoted word.
func TestVerifiedActionPostconditionAssertsTheGoal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shape's commands are POSIX shell")
	}
	for _, bin := range []string{"bash", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	tpl, ok := TemplateByID("verified-action")
	if !ok {
		t.Fatal("no verified-action template")
	}
	spec := tpl.Spec
	spec.Slug = "tagger"
	_, w, _ := scaffoldAndCompile(t, spec)
	action := nodesOf[*ir.ToolNode](w)[0]
	const tag = "v1.2.3"
	subst := func(s string) string {
		t.Helper()
		out := strings.ReplaceAll(s, "{{vars.tag}}", "'"+tag+"'")
		if out == s {
			t.Fatalf("%q no longer reads {{vars.tag}}", s)
		}
		return out
	}
	recipe, post := subst(action.Command), subst(action.Postcondition)

	repo := t.TempDir()
	gittest.Run(t, repo, "init", "-q")
	commit := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, repo, "add", name)
		gittest.Run(t, repo, "commit", "-q", "-m", name)
	}
	unmet := func(state string) {
		t.Helper()
		if out, _, err := shellInRepo(repo, post); err == nil {
			t.Fatalf("%s: the postcondition reported the goal met (stdout %q)", state, out)
		}
	}
	commit("one")

	// Before the recipe the goal is unmet: the skip rung must not fire.
	unmet("no tag")
	// A LIGHTWEIGHT tag on HEAD is not the goal either (`git tag <name>`,
	// the plausible repair of "tag already exists", makes one).
	gittest.Run(t, repo, "tag", tag)
	unmet("lightweight tag on HEAD")
	gittest.Run(t, repo, "tag", "-d", tag)
	// The recipe, then the postcondition: met, and its stdout is the
	// node's output — the JSON the `tagged` schema wants, nothing else.
	if _, stderr, err := shellInRepo(repo, recipe); err != nil {
		t.Fatalf("recipe %q: %v\n%s", recipe, err, stderr)
	}
	out, _, err := shellInRepo(repo, post)
	if err != nil {
		t.Fatalf("postcondition unmet right after the recipe: %v", err)
	}
	var got struct {
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Tag != tag {
		t.Fatalf("postcondition stdout = %q, want the JSON {\"tag\":%q}: %v", out, tag, err)
	}
	// A later commit leaves the tag EXISTING but stale: the goal is unmet
	// again, and a postcondition that only checked existence would let the
	// skip rung report an earlier run's tag as this run's success.
	commit("two")
	unmet("stale annotated tag")
}
