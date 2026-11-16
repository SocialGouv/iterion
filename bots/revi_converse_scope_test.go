package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// converse_scope is the run's entry node: the diff facts the answer grounds
// in, computed BEFORE the model's first turn. These tests run its script as
// the engine renders it (vars as JSON literals, python3 -I) on fixture git
// repos, and hold it against what review-pr's diff_precheck computes on the
// SAME repos — the two must never disagree about the revisions, the base
// freshness, the anomalies or the sentinel, because a converse answer
// reading one revision while a review reads another is exactly the drift
// the shared facts exist to stop.

// scopeOut is the converse_scope_output shape the script emits.
type scopeOut struct {
	Block            string `json:"block"`
	ReviewedSHA      string `json:"reviewed_sha"`
	BaseSHA          string `json:"base_sha"`
	BaseIsCurrent    bool   `json:"base_is_current"`
	WorkspaceAnomaly string `json:"workspace_anomaly"`
	ChangedFiles     int    `json:"changed_files"`
}

// scopeAnchor is the thread root's diff position as the webhook lanes set
// the thread_anchor_* vars (all strings; empty when the forge gave
// nothing).
type scopeAnchor struct {
	path, oldPath, line, oldLine, side, commit string
}

func (a scopeAnchor) vars() map[string]any {
	return map[string]any{
		"thread_anchor_path":     a.path,
		"thread_anchor_old_path": a.oldPath,
		"thread_anchor_line":     a.line,
		"thread_anchor_old_line": a.oldLine,
		"thread_anchor_side":     a.side,
		"thread_anchor_commit":   a.commit,
	}
}

// renderReviConverseScope renders converse_scope's script with the engine's
// renderer: the run's vars, no input (the node reads vars only).
func renderReviConverseScope(t *testing.T, vars map[string]any) string {
	t.Helper()
	wf := compileBotFile(t, filepath.Join(botsDir(t), "revi-converse", "main.bot"))
	tn, ok := wf.Nodes["converse_scope"].(*ir.ToolNode)
	if !ok || tn.Script == "" || tn.Language != "py" {
		t.Fatalf("converse_scope is %T (language %q), want a python script tool node", wf.Nodes["converse_scope"], tn.Language)
	}
	all := map[string]any{
		"workspace_dir": "", "base_ref": "main",
		"thread_anchor_path": "", "thread_anchor_old_path": "", "thread_anchor_line": "",
		"thread_anchor_old_line": "", "thread_anchor_side": "", "thread_anchor_commit": "",
	}
	for k, v := range vars {
		all[k] = v
	}
	var unresolved []string
	script, err := model.RenderScript(tn.Script, tn.Language, tn.ScriptRefs, nil, all, &model.TemplateData{}, "run-scope", func(ref string) {
		unresolved = append(unresolved, ref)
	})
	if err != nil {
		t.Fatalf("converse_scope does not render: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("converse_scope left %v unresolved — the case does not drive the vars it names", unresolved)
	}
	return script
}

// runScope executes a rendered scope script the way the engine does
// (python3 -I) and parses the scope output.
func runScope(t *testing.T, script, ws string) scopeOut {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "converse_scope.py")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-I", file)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(), "TMPDIR="+t.TempDir(),
		"GIT_TERMINAL_PROMPT=0",
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("converse_scope failed: %v (out %q, stderr %q)", err, out, stderr.String())
	}
	var res scopeOut
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output is not converse_scope_output JSON: %v (%q, stderr %q)", err, out, stderr.String())
	}
	return res
}

// scopeFixture is a git checkout on a branch whose change touches a file in
// two distant hunks (so "the anchor's hunk" and "the other hunk" are
// distinguishable), plus a second file, against main.
type scopeFixture struct {
	ws        string
	head      string
	mergeBase string // merge-base of feat and main
}

func newScopeFixture(t *testing.T) scopeFixture {
	t.Helper()
	ws := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		return gittest.Run(t, ws, args...)
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write("app.go", strings.Join([]string{
		"package app",
		"",
		"func A() int { return 1 }",
		"",
		"func pad01() int { return 0 }",
		"func pad02() int { return 0 }",
		"func pad03() int { return 0 }",
		"func pad04() int { return 0 }",
		"func pad05() int { return 0 }",
		"func pad06() int { return 0 }",
		"func pad07() int { return 0 }",
		"func pad08() int { return 0 }",
		"func pad09() int { return 0 }",
		"func pad10() int { return 0 }",
		"func pad11() int { return 0 }",
		"func pad12() int { return 0 }",
		"",
		"func B() int { return 1 }",
		"",
		"func pad21() int { return 0 }",
		"func pad22() int { return 0 }",
		"func pad23() int { return 0 }",
		"func pad24() int { return 0 }",
		"func pad25() int { return 0 }",
		"func pad26() int { return 0 }",
		"func pad27() int { return 0 }",
		"func pad28() int { return 0 }",
		"func pad29() int { return 0 }",
		"",
	}, "\n"))
	write("other.go", "package app\n\nfunc C() int { return 1 }\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	mergeBase := git("rev-parse", "HEAD")
	git("checkout", "-q", "-b", "feat")
	body := strings.Join([]string{
		"package app",
		"",
		"func A() int { return 2 }",
		"",
		"func pad01() int { return 0 }",
		"func pad02() int { return 0 }",
		"func pad03() int { return 0 }",
		"func pad04() int { return 0 }",
		"func pad05() int { return 0 }",
		"func pad06() int { return 0 }",
		"func pad07() int { return 0 }",
		"func pad08() int { return 0 }",
		"func pad09() int { return 0 }",
		"func pad10() int { return 0 }",
		"func pad11() int { return 0 }",
		"func pad12() int { return 0 }",
		"",
		"func B() int { return 2 }",
		"",
		"func pad21() int { return 0 }",
		"func pad22() int { return 0 }",
		"func pad23() int { return 0 }",
		"func pad24() int { return 0 }",
		"func pad25() int { return 0 }",
		"func pad26() int { return 0 }",
		"func pad27() int { return 0 }",
		"func pad28() int { return 0 }",
		"func pad29() int { return 0 }",
		"",
	}, "\n")
	write("app.go", body)
	write("other.go", "package app\n\nfunc C() int { return 2 }\n")
	git("commit", "-q", "-am", "change A and B and C")
	return scopeFixture{ws: ws, head: git("rev-parse", "HEAD"), mergeBase: mergeBase}
}

// runFixtureScope renders and runs converse_scope over the fixture with the
// given anchor.
func runFixtureScope(t *testing.T, f scopeFixture, anchor scopeAnchor) scopeOut {
	t.Helper()
	vars := map[string]any{"workspace_dir": f.ws, "base_ref": "main"}
	for k, v := range anchor.vars() {
		vars[k] = v
	}
	return runScope(t, renderReviConverseScope(t, vars), f.ws)
}

// TestReviConverseScopeFactsMatchDiffPrecheck is the parity half: converse
// answers and reviews review the SAME revisions, whatever the webhook
// launched. Both scripts run on the same fixture repos; every fact the two
// share must agree — reviewed_sha, base_sha, base_is_current,
// workspace_anomaly, changed_files, and the -1 sentinel on the unresolved
// path.
func TestReviConverseScopeFactsMatchDiffPrecheck(t *testing.T) {
	requirePython(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	precheck := func(t *testing.T, ws, base string) map[string]any {
		t.Helper()
		wf := compileBotFile(t, filepath.Join(botsDir(t), "review-pr", "main.bot"))
		raw, ok := wf.Nodes["diff_precheck"]
		if !ok {
			t.Fatal("review-pr has no diff_precheck node")
		}
		tn, ok := raw.(*ir.ToolNode)
		if !ok {
			t.Fatalf("diff_precheck is %T, want *ir.ToolNode", raw)
		}
		cmd := tn.Command
		for ref, val := range map[string]string{
			"{{vars.workspace_dir}}": ws,
			"{{vars.base_ref}}":      base,
		} {
			if !strings.Contains(cmd, ref) {
				t.Fatalf("diff_precheck no longer references %s — the test wires nothing", ref)
			}
			cmd = strings.ReplaceAll(cmd, ref, "'"+strings.ReplaceAll(val, "'", `'\''`)+"'")
		}
		out, err := exec.Command("sh", "-c", cmd).Output()
		if err != nil {
			t.Fatalf("diff_precheck failed: %v (out %q)", err, out)
		}
		var res map[string]any
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("diff_precheck output is not JSON: %v (%q)", err, out)
		}
		return res
	}
	sameFacts := func(t *testing.T, where string, f scopeFixture, base string, pre map[string]any, got scopeOut) {
		t.Helper()
		for _, pair := range [][2]string{
			{"reviewed_sha", got.ReviewedSHA},
			{"base_sha", got.BaseSHA},
			{"workspace_anomaly", got.WorkspaceAnomaly},
		} {
			if pre[pair[0]] != pair[1] {
				t.Errorf("%s: %s = %v (diff_precheck) vs %q (converse_scope) — the two readers disagree", where, pair[0], pre[pair[0]], pair[1])
			}
		}
		if pre["base_is_current"] != got.BaseIsCurrent {
			t.Errorf("%s: base_is_current = %v vs %v", where, pre["base_is_current"], got.BaseIsCurrent)
		}
		if int(pre["changed_files"].(float64)) != got.ChangedFiles {
			t.Errorf("%s: changed_files = %v vs %d", where, pre["changed_files"], got.ChangedFiles)
		}
		_ = f
		_ = base
	}

	t.Run("a branched checkout against its merge-base", func(t *testing.T) {
		f := newScopeFixture(t)
		got := runFixtureScope(t, f, scopeAnchor{})
		pre := precheck(t, f.ws, "main")
		sameFacts(t, "branched", f, "main", pre, got)
		if got.ChangedFiles != 2 || got.BaseIsCurrent {
			t.Errorf("changed_files = %d, base_is_current = %v; want 2 files, no remote to refresh from", got.ChangedFiles, got.BaseIsCurrent)
		}
	})

	t.Run("an unresolvable base: the sentinel, not zero", func(t *testing.T) {
		f := newScopeFixture(t)
		got := runScope(t, renderReviConverseScope(t, map[string]any{"workspace_dir": f.ws, "base_ref": "no-such-branch"}), f.ws)
		pre := precheck(t, f.ws, "no-such-branch")
		if got.ChangedFiles != -1 || pre["changed_files"].(float64) != -1 {
			t.Fatalf("changed_files = %d / %v, want the -1 sentinel on BOTH readers — never zero, which would read as an empty change", got.ChangedFiles, pre["changed_files"])
		}
		if got.BaseSHA != pre["base_sha"].(string) {
			t.Errorf("base_sha %q vs %v — the base TRIED must agree", got.BaseSHA, pre["base_sha"])
		}
		if !strings.Contains(got.Block, "UNAVAILABLE") {
			t.Errorf("the block must say the scope is unavailable: %.200q", got.Block)
		}
	})

	t.Run("a locally modified tracked file is the anomaly on both sides", func(t *testing.T) {
		f := newScopeFixture(t)
		if err := os.WriteFile(filepath.Join(f.ws, "other.go"), []byte("package app\n\nfunc C() int { return 3 }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := runFixtureScope(t, f, scopeAnchor{})
		pre := precheck(t, f.ws, "main")
		sameFacts(t, "anomaly", f, "main", pre, got)
		if !strings.Contains(got.WorkspaceAnomaly, "other.go") {
			t.Errorf("workspace_anomaly %q does not name the modified tracked file", got.WorkspaceAnomaly)
		}
		if !strings.Contains(got.Block, "other.go") || !strings.Contains(got.Block, "workspace anomaly") {
			t.Errorf("the block must name the anomaly: %.400q", got.Block)
		}
	})

	t.Run("a fetched base refresh agrees", func(t *testing.T) {
		origin := t.TempDir()
		gittest.Run(t, origin, "init", "-q", "--bare", "-b", "main")
		f := newScopeFixture(t)
		gittest.Run(t, f.ws, "remote", "add", "origin", origin)
		gittest.Run(t, f.ws, "push", "-q", "origin", "main")
		// Move the remote main so the fetch actually refreshes something:
		// a commit on main the checkout has not seen.
		clone := t.TempDir()
		gittest.Run(t, clone, "clone", "-q", origin, ".")
		if err := os.WriteFile(filepath.Join(clone, "later.txt"), []byte("later\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, clone, "add", "-A")
		gittest.Run(t, clone, "commit", "-q", "-m", "later")
		gittest.Run(t, clone, "push", "-q", "origin", "main")

		got := runFixtureScope(t, f, scopeAnchor{})
		pre := precheck(t, f.ws, "main")
		sameFacts(t, "fetched", f, "main", pre, got)
		if !got.BaseIsCurrent {
			t.Errorf("base_is_current = false after a successful fetch of a moved origin main")
		}
	})
}

// TestReviConverseScopeFocusFollowsTheThreadAnchor holds the focus half:
// the anchor isolates its hunk when it is provably fresh, and every way it
// degrades says so in the block — the pre-image hint is never read as a
// current line, a stale anchor is named stale, a removed line shows the
// file's whole diff.
func TestReviConverseScopeFocusFollowsTheThreadAnchor(t *testing.T) {
	requirePython(t)
	f := newScopeFixture(t)
	// A's change is the FIRST hunk of app.go; B's the second. The full diff
	// carries both; a focused read carries one.
	const hunkA = "func A() int { return 2 }"
	const hunkB = "func B() int { return 2 }"
	lineOfA := 3    // A() is line 3 in the new file
	lineOfB := 18   // B() is line 18 in the new file

	t.Run("fresh anchor isolates its hunk", func(t *testing.T) {
		got := runFixtureScope(t, f, scopeAnchor{path: "app.go", line: scopeItoa(lineOfA), side: "new", commit: f.head})
		if !strings.Contains(got.Block, hunkA) {
			t.Errorf("the focus must carry the anchored hunk (%q): %.600q", hunkA, got.Block)
		}
		if strings.Contains(got.Block, hunkB) || strings.Contains(got.Block, "func C() int { return 2 }") {
			t.Errorf("the focus must NOT carry the other hunks: %.600q", got.Block)
		}
		if !strings.Contains(got.Block, "the thread's anchor") {
			t.Errorf("the block must say the focus IS the anchor: %.200q", got.Block)
		}
	})

	t.Run("a fresh anchor on the second hunk isolates that one", func(t *testing.T) {
		got := runFixtureScope(t, f, scopeAnchor{path: "app.go", line: scopeItoa(lineOfB), side: "new", commit: f.head})
		if !strings.Contains(got.Block, hunkB) || strings.Contains(got.Block, hunkA) {
			t.Errorf("the focus must follow the anchor's line: %.600q", got.Block)
		}
	})

	t.Run("a deleted line (old side) shows the file's whole diff and says so", func(t *testing.T) {
		got := runFixtureScope(t, f, scopeAnchor{path: "app.go", line: scopeItoa(lineOfA), oldLine: scopeItoa(lineOfA), side: "old", commit: f.head})
		if !strings.Contains(got.Block, hunkA) || !strings.Contains(got.Block, hunkB) {
			t.Errorf("a removed line anchors no single hunk — the file's whole diff must show: %.600q", got.Block)
		}
		if !strings.Contains(got.Block, "a line the change removed") {
			t.Errorf("the block must say why it degraded: %.300q", got.Block)
		}
	})

	t.Run("an outdated comment (no current line): the original is never read as one", func(t *testing.T) {
		// GitHub's shape for an outdated comment: line null, original_line
		// kept, side still named, and the anchor commit behind the head.
		got := runFixtureScope(t, f, scopeAnchor{path: "app.go", line: "", oldLine: scopeItoa(lineOfA), side: "new", commit: f.head})
		if strings.Contains(got.Block, "around line") {
			t.Errorf("the original line must NOT isolate a hunk: %.400q", got.Block)
		}
		if !strings.Contains(got.Block, hunkA) || !strings.Contains(got.Block, hunkB) {
			t.Errorf("the file's whole diff must show: %.600q", got.Block)
		}
		if !strings.Contains(got.Block, "never read as a current one") {
			t.Errorf("the block must say the original line was not read as a current one: %.300q", got.Block)
		}
	})

	t.Run("a stale anchor (its commit is not the head) says so", func(t *testing.T) {
		parent := gittest.Run(t, f.ws, "rev-parse", f.head+"^")
		got := runFixtureScope(t, f, scopeAnchor{path: "app.go", line: scopeItoa(lineOfA), side: "new", commit: parent})
		if strings.Contains(got.Block, "the thread's anchor: its commit is the reviewed head") {
			t.Errorf("a stale anchor must not read as fresh: %.400q", got.Block)
		}
		if !strings.Contains(got.Block, hunkA) || !strings.Contains(got.Block, hunkB) {
			t.Errorf("a stale anchor shows the file's whole diff: %.600q", got.Block)
		}
		if !strings.Contains(got.Block, "may be outdated") {
			t.Errorf("the block must name the staleness: %.300q", got.Block)
		}
	})

	t.Run("a rename is named, the new path carries the focus", func(t *testing.T) {
		// GitLab's position carries old_path ≠ new_path on a renamed file.
		got := runFixtureScope(t, f, scopeAnchor{path: "app.go", oldPath: "was_app.go", line: scopeItoa(lineOfA), side: "new", commit: f.head})
		if !strings.Contains(got.Block, hunkA) {
			t.Errorf("the new path's hunk must carry the focus: %.600q", got.Block)
		}
		if !strings.Contains(got.Block, "renamed from was_app.go") {
			t.Errorf("the block must name the rename: %.300q", got.Block)
		}
	})

	t.Run("no anchor: the whole change, said as such", func(t *testing.T) {
		got := runFixtureScope(t, f, scopeAnchor{})
		if !strings.Contains(got.Block, hunkA) || !strings.Contains(got.Block, "func C() int { return 2 }") {
			t.Errorf("no anchor means the whole change: %.600q", got.Block)
		}
		if !strings.Contains(got.Block, "no thread anchor") {
			t.Errorf("the block must say the thread anchored nothing: %.300q", got.Block)
		}
	})

	t.Run("the block names both revisions and the base freshness", func(t *testing.T) {
		got := runFixtureScope(t, f, scopeAnchor{})
		for _, want := range []string{f.head, f.mergeBase, "base is current: NO"} {
			if !strings.Contains(got.Block, want) {
				t.Errorf("the block must name %q (this checkout has no origin, so the refresh fails and the block says it): %.400q", want, got.Block)
			}
		}
	})
}

// TestReviConverseScopeCapsTheBlock holds the cap: a scope block never
// exceeds its bound, and a cut is visible — an agent reading a truncated
// block must know it is reading one.
func TestReviConverseScopeCapsTheBlock(t *testing.T) {
	requirePython(t)
	f := newScopeFixture(t)
	// A change past the cap: one file whose diff alone dwarfs 48000
	// characters.
	var body strings.Builder
	body.WriteString("line\n")
	for i := 0; i < 6000; i++ {
		body.WriteString("changed line to bloat the diff past every honest scope: ")
		body.WriteString(strings.Repeat("x", 60))
		body.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(f.ws, "huge.txt"), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, f.ws, "add", "-A")
	gittest.Run(t, f.ws, "commit", "-q", "-m", "huge")
	head := gittest.Run(t, f.ws, "rev-parse", "HEAD")

	got := runFixtureScope(t, f, scopeAnchor{path: "huge.txt", line: "2", side: "new", commit: head})
	const cap = 48000
	if len(got.Block) > cap+200 {
		t.Errorf("the block is %d characters, want capped at ~%d", len(got.Block), cap)
	}
	if !strings.Contains(got.Block, "scope capped at 48000 characters") {
		t.Errorf("a cut block must say so: tail %q", tail(got.Block, 200))
	}
	if got.ChangedFiles < 2 {
		t.Errorf("changed_files = %d, want the real count (the cap cuts the block, not the facts)", got.ChangedFiles)
	}
}

// TestReviConverseScopeAnchorsArriveFromTheWebhookVars pins the var
// contract: the anchor the lanes set is the one the scope reads — spelling
// drift between the server and the bot breaks the focus silently, so the
// test reads BOTH sides from the same names.
func TestReviConverseScopeAnchorsArriveFromTheWebhookVars(t *testing.T) {
	requirePython(t)
	// The exact six names the GitLab and GitHub/Forgejo lanes set.
	want := []string{
		"thread_anchor_path", "thread_anchor_old_path", "thread_anchor_line",
		"thread_anchor_old_line", "thread_anchor_side", "thread_anchor_commit",
	}
	src, err := botUnitSource(filepath.Join(botsDir(t), "revi-converse", "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range want {
		if !strings.Contains(string(src), "{{vars."+name+"}}") {
			t.Errorf("converse_scope's script never reads vars.%s — the lanes set it, nobody reads it", name)
		}
		if !strings.Contains(string(src), name+": string = \"\"") {
			t.Errorf("main.bot does not declare %s as a defaulted string var — a launch carrying it would ride nowhere", name)
		}
	}
}

func scopeItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
