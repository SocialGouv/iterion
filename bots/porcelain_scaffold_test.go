package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// scaffoldRepo is a real repository with one committed baseline file and
// iterion's scaffold laid untracked under .claude/, the way a run workspace
// looks on a repository that does not ignore it (#1364).
func scaffoldRepo(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	gittest.Run(t, ws, "init", "-q")
	gittest.Run(t, ws, "config", "user.email", "t@example.invalid")
	gittest.Run(t, ws, "config", "user.name", "t")
	if err := os.MkdirAll(filepath.Join(ws, "docs", "adr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("baseline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, ws, "add", "README.md")
	gittest.Run(t, ws, "commit", "-q", "-m", "baseline")
	writeScaffold(t, ws)
	return ws
}

func runScaffoldJSON(t *testing.T, cmd string, out any) {
	t.Helper()
	raw, err := exec.Command("sh", "-c", cmd).Output()
	if err != nil {
		t.Fatalf("command failed: %v (out %q)", err, raw)
	}
	if uerr := json.Unmarshal(raw, out); uerr != nil {
		t.Fatalf("output is not JSON: %v (out %q)", uerr, raw)
	}
}

// The scope checks of adr-cartograph and docs-refresh read every untracked
// path as a write of the run; the scaffold alone must not fail them, a real
// stray file still must.
func TestScopeChecksLeaveTheScaffoldOut(t *testing.T) {
	for _, bin := range []string{"python3", "git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	type scope struct {
		ScopeOK    bool     `json:"scope_ok"`
		OutOfScope []string `json:"out_of_scope"`
	}
	for _, c := range []struct{ rel, node string }{
		{"adr-cartograph/main.bot", "scope_check"},
		{"docs-refresh/main.bot", "scope_check"},
	} {
		c := c
		t.Run(c.rel, func(t *testing.T) {
			tpl := toolCommand(t, c.rel, c.node)
			expand := func(ws string) string {
				s := strings.ReplaceAll(tpl, "{{vars.workspace_dir}}", ws)
				s = strings.ReplaceAll(s, "{{vars.adr_dir}}", "docs/adr")
				s = strings.ReplaceAll(s, "{{vars.audit_cache_path}}", "")
				return s
			}
			ws := scaffoldRepo(t)
			var res scope
			runScaffoldJSON(t, expand(ws), &res)
			if !res.ScopeOK || len(res.OutOfScope) != 0 {
				t.Fatalf("the scaffold alone must be in scope, got %+v", res)
			}
			if err := os.WriteFile(filepath.Join(ws, "stray.py"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runScaffoldJSON(t, expand(ws), &res)
			if res.ScopeOK || len(res.OutOfScope) != 1 || res.OutOfScope[0] != "stray.py" {
				t.Fatalf("a stray file must still be out of scope, and only it, got %+v", res)
			}
		})
	}
}

// branch-improve-loop's decline probe honours a refusal only on a tree the
// run left untouched; the scaffold is not the run's touch.
func TestDeclineProbeLeavesTheScaffoldOut(t *testing.T) {
	for _, bin := range []string{"python3", "git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	tpl := toolCommand(t, "branch-improve-loop/main.bot", "decline_probe")
	ws := scaffoldRepo(t)
	head := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))
	expand := func() string {
		s := strings.ReplaceAll(tpl, "{{vars.workspace_dir}}", ws)
		s = strings.ReplaceAll(s, "{{input.entry_head}}", head)
		s = strings.ReplaceAll(s, "{{input.decline_reason}}", "'nothing to do'")
		return s
	}
	var res struct {
		Honoured bool   `json:"honoured"`
		Reason   string `json:"reason"`
	}
	runScaffoldJSON(t, expand(), &res)
	if !res.Honoured {
		t.Fatalf("a decline on a tree that carries only the scaffold must be honoured, got %+v", res)
	}
	if err := os.WriteFile(filepath.Join(ws, "half.py"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runScaffoldJSON(t, expand(), &res)
	if res.Honoured || !strings.Contains(res.Reason, "half.py") || strings.Contains(res.Reason, ".claude") {
		t.Fatalf("a real leftover must void the decline and be the one named, got %+v", res)
	}
}

// Every porcelain read in the catalogue is classified: it applies the
// scaffold rule (the node's text carries the helper or the pathspec), reads
// tracked files only, or is named below with the reason it is unaffected.
// A new reader that does none of these fails here until it is classified.
func TestPorcelainReadersAreClassified(t *testing.T) {
	unaffected := map[string]string{
		"modernize/main.bot:lot_verify": "scoped to a lot's own paths by pathspec",
	}
	header := regexp.MustCompile(`^(tool|script|agent|compute|human|subbot|judge|router) ([a-z_]+):`)
	prose := regexp.MustCompile(`^\s*(#|//|""")|test -z on|probe = |1\. .git status|Run .git status|Split BEFORE|and .git diff HEAD`)
	// A USE of the rule, never its definition: `def is_scaffold(line):` left
	// in a script whose read stopped calling it must not pass.
	rule := regexp.MustCompile(`(?:not|or|and) is_scaffold\(|\.startswith\(['"]\.claude/['"]\)|exclude,top\)\.claude`)
	var files []string
	if err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".bot") {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reads := 0
	for _, rel := range files {
		src, err := os.ReadFile(rel)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(src), "\n")
		// The text of each node, by name, to look the rule up in.
		nodeOf := make([]string, len(lines))
		bodies := map[string]*strings.Builder{}
		node := ""
		inHelper := false // the helper's own definition is not a use of it
		for i, line := range lines {
			if m := header.FindStringSubmatch(line); m != nil {
				node = m[2]
				bodies[node] = &strings.Builder{}
			}
			nodeOf[i] = node
			if strings.Contains(line, "def is_scaffold(") {
				inHelper = true
			}
			if inHelper {
				if strings.Contains(line, "return path.startswith('.claude/')") {
					inHelper = false
				}
				continue
			}
			if b, ok := bodies[node]; ok {
				b.WriteString(line)
				b.WriteByte('\n')
			}
		}
		for i, line := range lines {
			if !strings.Contains(line, "--porcelain") || prose.MatchString(line) {
				continue
			}
			reads++
			if strings.Contains(line, "--untracked-files=no") {
				continue // tracked files only: the untracked scaffold is invisible to it
			}
			if rule.MatchString(line) {
				continue // the read itself carries the pathspec (an instruction, a shell command)
			}
			key := filepath.ToSlash(rel) + ":" + nodeOf[i]
			if _, ok := unaffected[key]; ok {
				continue
			}
			body := ""
			if b, ok := bodies[nodeOf[i]]; ok {
				body = b.String()
			}
			if !rule.MatchString(body) {
				t.Errorf("%s:%d (node %s) reads porcelain without the scaffold rule: %s", rel, i+1, nodeOf[i], strings.TrimSpace(line))
			}
		}
	}
	if reads < 30 {
		t.Errorf("only %d porcelain reads found — the walk no longer covers the catalogue", reads)
	}
}
