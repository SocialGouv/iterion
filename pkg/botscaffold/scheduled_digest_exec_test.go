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

// TestScheduledDigestCollectHandsTheLogOverInline executes the
// scheduled-digest shape's collect tool in a real repository: the commit
// log travels INSIDE the node's JSON output (no file the next node has to
// see), the count is the number of commits in the window, an empty window
// is an empty log with zero commits, nothing is written into the checkout
// — and a host without jq fails the node by name instead of handing the
// agent an unparseable output that would read as a green digest of nothing.
func TestScheduledDigestCollectHandsTheLogOverInline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shape's commands are POSIX shell")
	}
	bins := requireBins(t, "bash", "git", "jq", "grep")
	tpl, ok := TemplateByID("scheduled-digest")
	if !ok {
		t.Fatal("no scheduled-digest template")
	}
	spec := tpl.Spec
	spec.Slug = "digest"
	_, w, _ := scaffoldAndCompile(t, spec)
	var collect *ir.ToolNode
	for _, n := range nodesOf[*ir.ToolNode](w) {
		if n.ID == "collect" {
			collect = n
		}
	}
	if collect == nil {
		t.Fatal("no collect tool")
	}

	repo := t.TempDir()
	gittest.Run(t, repo, "init", "-q")
	for _, name := range []string{"first change", "second change"} {
		if err := os.WriteFile(filepath.Join(repo, strings.ReplaceAll(name, " ", "-")), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, repo, "add", ".")
		gittest.Run(t, repo, "commit", "-q", "-m", name)
	}
	command := func(window string) string {
		t.Helper()
		c := strings.ReplaceAll(collect.Command, "{{vars.window}}", "'"+window+"'")
		if c == collect.Command {
			t.Fatalf("%q no longer reads {{vars.window}}", collect.Command)
		}
		return c
	}
	type collected struct {
		Log     string `json:"log"`
		Commits int    `json:"commits"`
	}
	run := func(window string) collected {
		t.Helper()
		out, stderr, err := shellInRepo(repo, command(window))
		if err != nil {
			t.Fatalf("collect (%s): %v\n%s", window, err, stderr)
		}
		var got collected
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("collect (%s): stdout = %q, want the JSON the schema wants: %v", window, out, err)
		}
		return got
	}
	if got := run("24.hours"); got.Commits != 2 || !strings.Contains(got.Log, "first change") || !strings.Contains(got.Log, "second change") {
		t.Errorf("24.hours: %+v, want both commits in the log and commits=2", got)
	}
	// A window that starts after every commit (git's date parser reads a
	// year past 2038 as garbage, and garbage as the epoch: 2999 returns
	// everything).
	if got := run("2037-01-01"); got.Commits != 0 || got.Log != "" {
		t.Errorf("empty window: %+v, want an empty log and commits=0", got)
	}
	if dirty := gittest.Run(t, repo, "status", "--porcelain"); dirty != "" {
		t.Errorf("collect wrote into the checkout: %q", dirty)
	}

	// Without jq the node must FAIL, loudly and by name: a PATH that holds
	// git and grep but no jq.
	bin := t.TempDir()
	for _, name := range []string{"git", "grep"} {
		if err := os.Symlink(bins[name], filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bins["bash"], "-c", command("24.hours"))
	cmd.Dir = repo
	cmd.Env = append(gittest.Env(), "PATH="+bin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("without jq the collect tool succeeded with stdout %q; the agent would digest a placeholder", stdout.String())
	}
	if !strings.Contains(stderr.String(), "jq is required") {
		t.Errorf("without jq the refusal does not name jq: stderr %q", stderr.String())
	}
}
