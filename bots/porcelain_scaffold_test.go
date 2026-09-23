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
	"github.com/SocialGouv/iterion/pkg/treenoise"
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
	// The engine provisions ITERION_TREE_NOISE on every tool process it
	// spawns (host and sandbox); this bare `sh -c` stands in for that
	// spawn and carries the same environment.
	provisioned := exec.Command("sh", "-c", cmd)
	provisioned.Env = append(os.Environ(), treenoise.TreeNoiseEnvVar+"="+treenoise.EnvValue())
	raw, err := provisioned.Output()
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
			// The engine rewrites a tracked .claude/settings.json to inject plugin
			// hooks: that modification reaches the diff branch of the check and is
			// not the run's change either (the gates' whole-tree rule).
			if err := os.Remove(filepath.Join(ws, "stray.py")); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, ws, "add", "-f", filepath.Join(".claude", "settings.json"))
			gittest.Run(t, ws, "commit", "-q", "-m", "track the settings")
			if err := os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte("{\"hooks\": {}}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runScaffoldJSON(t, expand(ws), &res)
			if !res.ScopeOK || len(res.OutOfScope) != 0 {
				t.Fatalf("a tracked settings.json the engine rewrote must stay in scope, got %+v", res)
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
	// The whole .claude/ tree is the engine's, tracked or not: it rewrites a
	// tracked settings.json to inject plugin hooks, and a reader that voided
	// every decline on that write would stop every run on a repository that
	// commits the file. The destructive steps are what spare that tree.
	if err := os.Remove(filepath.Join(ws, "half.py")); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, ws, "add", "-f", filepath.Join(".claude", "settings.json"))
	gittest.Run(t, ws, "commit", "-q", "-m", "track the settings")
	head = strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte("{\"hooks\": {}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runScaffoldJSON(t, expand(), &res)
	if !res.Honoured {
		t.Fatalf("the engine's rewrite of a tracked settings.json must not void the decline, got %+v", res)
	}
}

// sec-audit-source's prepare_branch stashes the operator's work in flight
// before it branches; the scaffold is neither work nor stashed, and a stash
// that saved nothing must not be recorded — the later pop would take
// somebody else's stash.
func TestPrepareBranchRecordsOnlyItsOwnStash(t *testing.T) {
	for _, bin := range []string{"python3", "git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	tpl := toolCommand(t, "sec-audit-source/main.bot", "prepare_branch")
	expand := func(ws string) string {
		s := strings.ReplaceAll(tpl, "{{vars.workspace_dir}}", ws)
		return strings.ReplaceAll(s, "{{run.id}}", "01a0-test-run")
	}
	type prepared struct {
		Prepared bool   `json:"prepared"`
		Stashed  bool   `json:"stashed"`
		StashRef string `json:"stash_ref"`
		Note     string `json:"note"`
	}

	// A foreign stash sits on top; the only dirt is the scaffold (and a
	// modified tracked file under it): nothing of the operator's to stash.
	ws := scaffoldRepo(t)
	if err := os.WriteFile(filepath.Join(ws, "wip.txt"), []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, ws, "stash", "push", "--include-untracked", "-m", "somebody else's", "--", "wip.txt")
	foreign := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "stash@{0}"))
	gittest.Run(t, ws, "add", "-f", filepath.Join(".claude", "settings.json"))
	gittest.Run(t, ws, "commit", "-q", "-m", "track the settings")
	if err := os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte("{\"hooks\": {}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var res prepared
	runScaffoldJSON(t, expand(ws), &res)
	if !res.Prepared || res.Stashed || res.StashRef != "" {
		t.Fatalf("with nothing of the operator's to stash, no stash may be recorded (the foreign one is %s), got %+v", foreign[:8], res)
	}
	if _, err := os.Stat(filepath.Join(ws, ".claude", "skills", "bot.md")); err != nil {
		t.Fatalf("the scaffold must stay in place: %v", err)
	}

	// The operator's own edit is stashed, and the record is that stash.
	ws = scaffoldRepo(t)
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("baseline\ntheirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runScaffoldJSON(t, expand(ws), &res)
	top := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "stash@{0}"))
	if !res.Prepared || !res.Stashed || res.StashRef != top {
		t.Fatalf("the operator's edit must be stashed and recorded as the top stash %s, got %+v", top[:8], res)
	}
	if _, err := os.Stat(filepath.Join(ws, ".claude", "skills", "bot.md")); err != nil {
		t.Fatalf("the scaffold must stay in place: %v", err)
	}
}

// Every read of the working tree's dirt in the catalogue — `git status
// --porcelain`, `git ls-files --others` — is classified AT THE READ: the read
// line or the statements that consume it (the next sixteen lines) use the
// scaffold rule, the read lists tracked files only, or the read is named
// below with the reason the scaffold cannot reach what it decides. A node
// that carries one filtered read does not clear its other reads; a new
// reader fails here until it is classified.
func TestPorcelainReadersAreClassified(t *testing.T) {
	unaffected := map[string]string{
		"modernize/main.bot:lot_verify:code, log, _ = run(\"git -c core.quotePath=false status --porcelain -- %s\"":                                           "scoped to the lot's own paths by pathspec",
		"assessment/main.bot:commit:dirty = git(\"status\", \"--porcelain\", \"--\", *present)":                                                               "scoped by pathspec to the assessment's own outputs, which the scaffold never sits under",
		"golden-master/extend.bot:extend_base:dirty = subprocess.run([\"git\", \"-C\", ws, \"status\", \"--porcelain\", \"-z\"],":                             "filtered where the -z tokens are parsed, below the window",
		"branch-improve-loop/main.bot:delivery_probe:changed += git('ls-files', '--others', '--exclude-standard', '-z')":                                      "asks only whether a .github/workflows/ path changed",
		"golden-master/main.bot:oracle_run:code, out = run(\"git --no-optional-locks status --porcelain\", ws, timeout=120)":                                  "a tree fingerprint, only ever compared with itself",
		"golden-master/sync-harness.bot:sync_harness:code, out = run(\"git --no-optional-locks status --porcelain\", ws, timeout=120)":                        "a tree fingerprint, only ever compared with itself",
		"golden-master/main.bot:oracle_run:p = subprocess.run([\"git\", \"-C\", ws, \"--no-optional-locks\", \"status\", \"--porcelain\", \"-z\"],":           "a path set compared before and after a mutant: the scaffold cancels",
		"golden-master/sync-harness.bot:sync_harness:p = subprocess.run([\"git\", \"-C\", ws, \"--no-optional-locks\", \"status\", \"--porcelain\", \"-z\"],": "a path set compared before and after a mutant: the scaffold cancels",
		"golden-master/main.bot:oracle_run:\"gm-applied\" in sub(\"git\", \"status\", \"--porcelain\").stdout],":                                              "looks for one marker path",
		"secured-renovacy/main.bot:prepare_commit:const untracked = execSync(`git -C \"${workspaceDir}\" ls-files --others --exclude-standard`, EXEC_OPTS);":  "filtered where the file list is built, twenty lines below (`/^\\.claude\\//`)",
		"golden-master/sync-harness.bot:sync_harness:\"gm-applied\" in sub(\"git\", \"status\", \"--porcelain\").stdout],":                                    "looks for one marker path",
		"campaign/main.bot:plan_landed:st = git(\"status\", \"--porcelain\", \"--\", plan_rel)":                                                               "scoped by pathspec to the contract FILE; the scaffold is a .claude/ tree and the engine's script a workspace-root file",
		"campaign/main.bot:net_landed:st = git(\"status\", \"--porcelain\", \"--\", oracle_rel)":                                                              "scoped by pathspec to the oracle directory; the scaffold is a .claude/ tree and the engine's script a workspace-root file",
		"campaign/main.bot:docs_landed:st = git(\"status\", \"--porcelain\", \"--\", docs_rel)":                                                               "scoped by pathspec to the documentation directory; the scaffold is a .claude/ tree and the engine's script a workspace-root file",
	}
	header := regexp.MustCompile(`^(tool|script|agent|compute|human|subbot|judge|router) ([a-z_]+):`)
	read := regexp.MustCompile(`--porcelain|ls-files.{0,12}--others`)
	prose := regexp.MustCompile(`^\s*(#|//|""")|test -z on|probe = |1\. .git status|Run .git status|Split BEFORE|and .git diff HEAD|The earlier .git status`)
	// A USE of the rule — never the helper's definition, which sits above
	// every read and would clear them all.
	rule := regexp.MustCompile(`(?:not|or|and) is_scaffold\(|\.startswith\(['"]\.claude/['"]\)|exclude,top\)\.claude|\^\\\.claude\\/|is_noise\(|\{\{run\.tree_noise\}\}`)
	const window = 16
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
	reads, named := 0, 0
	for _, rel := range files {
		src, err := os.ReadFile(rel)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(src), "\n")
		node := ""
		for i, line := range lines {
			if m := header.FindStringSubmatch(line); m != nil {
				node = m[2]
			}
			if !read.MatchString(line) || prose.MatchString(line) {
				continue
			}
			reads++
			if strings.Contains(line, "--untracked-files=no") {
				continue // tracked files only: the untracked scaffold is invisible to it
			}
			if strings.Contains(line, "ITERION_TREE_NOISE") {
				continue // the read itself carries the exclusion (the env value word-splits at the read)
			}
			key := filepath.ToSlash(rel) + ":" + node + ":" + strings.TrimSpace(line)
			nodeEnd := len(lines)
			for j := i + 1; j < len(lines); j++ {
				if header.MatchString(lines[j]) {
					nodeEnd = j
					break
				}
			}
			if reason, ok := unaffected[key]; ok {
				named++
				// A reader named as "filtered … below" is held to it: the rule
				// must appear between the read and the end of its node.
				if strings.HasPrefix(reason, "filtered") && !rule.MatchString(strings.Join(lines[i:nodeEnd], "\n")) {
					t.Errorf("%s:%d (node %s) is named as filtered below the read, but no use of the rule follows it in the node", rel, i+1, node)
				}
				continue
			}
			end := i + window
			if end > nodeEnd {
				end = nodeEnd
			}
			if !rule.MatchString(strings.Join(lines[i:end], "\n")) {
				t.Errorf("%s:%d (node %s) reads the tree's dirt without the scaffold rule at the read: %s", rel, i+1, node, strings.TrimSpace(line))
			}
		}
	}
	if reads < 30 || named != len(unaffected) {
		t.Errorf("%d reads found, %d of %d named readers matched — the walk or the names no longer cover the catalogue", reads, named, len(unaffected))
	}
}
