package context

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func relPaths(rules []ruleFile) []string {
	got := make([]string, len(rules))
	for i, r := range rules {
		got[i] = r.rel
	}
	return got
}

func TestDiscoverRulesIsRecursiveAndSortedByRelativePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rules")
	for _, f := range []string{
		"b.md", "a.md", "a/b.md", "a/z.md", "Z.md", ".hidden.md",
		"sub/deeper/d.md",
		"dir.md/inner.md", // a directory whose name ends in .md is entered, not listed
		"notes.txt",       // not markdown
		"a/readme.MD",     // the suffix is matched as Claude Code matches it: case-sensitively
	} {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(f)), "x")
	}

	got := relPaths(discoverRules(dir, ""))

	// Plain string order of the relative paths: "a.md" sorts before "a/b.md"
	// ('.' < '/'), which is NOT the order a directory walk produces (it
	// enters "a/" before it reaches "a.md").
	want := []string{".hidden.md", "Z.md", "a.md", "a/b.md", "a/z.md", "b.md", "dir.md/inner.md", "sub/deeper/d.md"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("rules:\n got  %q\n want %q", got, want)
	}
}

// discoverWithin runs discoverRules, failing the test rather than hanging it
// when the walk does not end.
func discoverWithin(t *testing.T, dir string, limit time.Duration) []ruleFile {
	t.Helper()
	done := make(chan []ruleFile, 1)
	go func() { done <- discoverRules(dir, "") }()
	select {
	case rules := <-done:
		return rules
	case <-time.After(limit):
		t.Fatalf("discoverRules(%s) did not return within %s: a symlink cycle is not cut", dir, limit)
		return nil
	}
}

func TestDiscoverRulesMissingOrEmptyDirectory(t *testing.T) {
	if got := discoverRules(filepath.Join(t.TempDir(), "nowhere"), ""); len(got) != 0 {
		t.Errorf("missing directory listed %v", got)
	}
	empty := filepath.Join(t.TempDir(), "rules")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := discoverRules(empty, ""); len(got) != 0 {
		t.Errorf("empty directory listed %v", got)
	}
	// A regular file where the directory should be.
	file := filepath.Join(t.TempDir(), "rules")
	writeFile(t, file, "not a directory")
	if got := discoverRules(file, ""); len(got) != 0 {
		t.Errorf("a file listed as a rules directory: %v", got)
	}
}

// Claude Code resolves symlinks in a rules directory (the documented way to
// share rules between projects) and survives a cycle.
func TestDiscoverRulesFollowsSymlinksAndSurvivesCycles(t *testing.T) {
	tmp := t.TempDir()
	rules := filepath.Join(tmp, "project", ".claude", "rules")
	shared := filepath.Join(tmp, "shared-rules")
	writeFile(t, filepath.Join(rules, "own.md"), "own")
	writeFile(t, filepath.Join(shared, "team.md"), "team")
	writeFile(t, filepath.Join(shared, "nested", "deep.md"), "deep")
	writeFile(t, filepath.Join(tmp, "elsewhere", "single.md"), "single")

	link := func(target, name string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(rules, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	link(shared, "shared")                                          // a directory symlink
	link(filepath.Join(tmp, "elsewhere", "single.md"), "linked.md") // a file symlink
	link(rules, "loop")                                             // back to the rules directory itself
	link(filepath.Join(tmp, "nowhere.md"), "dangling.md")           // a target that is gone
	// A cycle through a second directory: shared/back -> rules.
	if err := os.Symlink(rules, filepath.Join(shared, "back")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got := relPaths(discoverWithin(t, rules, 5*time.Second))

	// Each directory is entered once: through "loop" and "shared/back" the
	// rules directory is already visited, and "shared" is reached once.
	want := []string{"linked.md", "own.md", "shared/nested/deep.md", "shared/team.md"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("rules:\n got  %q\n want %q", got, want)
	}
	for _, r := range discoverRules(rules, "") {
		if r.path != filepath.Join(rules, filepath.FromSlash(r.rel)) {
			t.Errorf("rule %q is known as %q, want the path it was reached by", r.rel, r.path)
		}
	}
}

// A workspace's rules directory belongs to a repository, which must not be
// able to point the walk at the rest of the machine; the operator's own
// directories are not confined.
func TestDiscoverRulesConfinesSymlinks(t *testing.T) {
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	rules := filepath.Join(ws, ".claude", "rules")
	writeFile(t, filepath.Join(rules, "own.md"), "own")
	writeFile(t, filepath.Join(ws, "shared", "in.md"), "inside")
	writeFile(t, filepath.Join(tmp, "outside", "out.md"), "outside")
	writeFile(t, filepath.Join(tmp, "outside", "deeper", "d.md"), "outside, deeper")
	// "ws-evil" shares its name's prefix with the workspace "ws".
	writeFile(t, filepath.Join(tmp, "ws-evil", "evil.md"), "prefix sibling")

	link := func(target, name string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(rules, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	link(filepath.Join(ws, "shared"), "in-dir")                    // a directory inside the workspace
	link(filepath.Join(ws, "shared", "in.md"), "in-file.md")       // a file inside the workspace
	link("in-file.md", "hop.md")                                   // a link to a link, still inside
	link(filepath.Join(tmp, "outside"), "out-dir")                 // a directory outside
	link(filepath.Join(tmp, "outside", "out.md"), "out-file.md")   // a file outside
	link("out-file.md", "hop-out.md")                              // a link to a link that leaves
	link(filepath.Join(tmp, "ws-evil"), "evil-dir")                // outside, but its name starts with "ws"
	link(filepath.Join(tmp, "ws-evil", "evil.md"), "evil-file.md") // likewise

	confined := relPaths(discoverRules(rules, ws))
	wantConfined := []string{"hop.md", "in-dir/in.md", "in-file.md", "own.md"}
	if strings.Join(confined, "|") != strings.Join(wantConfined, "|") {
		t.Errorf("confined to the workspace:\n got  %q\n want %q", confined, wantConfined)
	}

	// The boundary is compared resolved: a symlinked spelling of it is the same.
	wsLink := filepath.Join(tmp, "ws-link")
	if err := os.Symlink(ws, wsLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := relPaths(discoverRules(rules, wsLink)); strings.Join(got, "|") != strings.Join(wantConfined, "|") {
		t.Errorf("confined to a symlinked spelling of the workspace:\n got  %q\n want %q", got, wantConfined)
	}

	free := relPaths(discoverRules(rules, ""))
	wantFree := []string{"evil-dir/evil.md", "evil-file.md", "hop-out.md", "hop.md", "in-dir/in.md", "in-file.md",
		"out-dir/deeper/d.md", "out-dir/out.md", "out-file.md", "own.md"}
	if strings.Join(free, "|") != strings.Join(wantFree, "|") {
		t.Errorf("unconfined:\n got  %q\n want %q", free, wantFree)
	}
}

// The rules directory itself, and the .claude above it, are symlinks too: a
// repository's `.claude/rules -> /` is the same attack as a link inside it.
func TestDiscoverRulesConfinesALinkedRulesDirectory(t *testing.T) {
	tmp := t.TempDir()
	outside := filepath.Join(tmp, "outside")
	writeFile(t, filepath.Join(outside, "rules", "out.md"), "outside")
	writeFile(t, filepath.Join(tmp, "inside", "in.md"), "inside")

	for name, build := range map[string]func(ws string) string{
		"rules is a link out": func(ws string) string {
			if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "rules"), filepath.Join(ws, ".claude", "rules")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return filepath.Join(ws, ".claude", "rules")
		},
		".claude is a link out": func(ws string) string {
			if err := os.MkdirAll(ws, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(ws, ".claude")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return filepath.Join(ws, ".claude", "rules")
		},
	} {
		t.Run(name, func(t *testing.T) {
			ws := filepath.Join(t.TempDir(), "ws")
			rules := build(ws)
			if got := relPaths(discoverRules(rules, ws)); len(got) != 0 {
				t.Errorf("a rules directory that leaves the workspace was walked: %q", got)
			}
			if got := relPaths(discoverRules(rules, "")); strings.Join(got, "|") != "out.md" {
				t.Errorf("unconfined, the linked directory should be walked: %q", got)
			}
		})
	}

	// A link that stays inside is fine.
	ws := filepath.Join(tmp, "ws-in")
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, "kept", "k.md"), "kept")
	if err := os.Symlink(filepath.Join(ws, "kept"), filepath.Join(ws, ".claude", "rules")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := relPaths(discoverRules(filepath.Join(ws, ".claude", "rules"), ws)); strings.Join(got, "|") != "k.md" {
		t.Errorf("a rules directory linked inside the workspace: %q", got)
	}

	// A confinement that cannot be resolved admits nothing.
	if got := discoverRules(filepath.Join(ws, ".claude", "rules"), filepath.Join(tmp, "nowhere")); len(got) != 0 {
		t.Errorf("an unresolvable boundary listed %v", got)
	}
}

// ---------------------------------------------------------------------------
// parseRule: frontmatter stripping and the conditional test

func TestParseRule(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		body        string
		conditional bool
	}{
		// No `paths` key: stripped, never conditional.
		{"no frontmatter", "BODY", "BODY", false},
		{"frontmatter without paths", "---\ndescription: d\nowner: me\n---\nBODY", "BODY", false},
		{"empty frontmatter", "---\n---\nBODY", "BODY", false},
		{"frontmatter closed at EOF", "---\ndescription: d\n---", "", false},

		// A `paths` key naming a pattern: conditional.
		{"block list", "---\npaths:\n  - \"src/**/*.go\"\n---\nBODY", "BODY", true},
		{"block list, flush items", "---\npaths:\n- src/**/*.go\n---\nBODY", "BODY", true},
		{"inline scalar", "---\npaths: src/**/*.ts\n---\nBODY", "BODY", true},
		{"inline quoted", "---\npaths: \"src/**/*.ts\"\n---\nBODY", "BODY", true},
		{"flow list", "---\npaths: [\"a/**\", \"b/**\"]\n---\nBODY", "BODY", true},
		{"comma separated", "---\npaths: a/**, b/**\n---\nBODY", "BODY", true},
		{"braces are one pattern", "---\npaths: src/**/*.{ts,tsx}\n---\nBODY", "BODY", true},
		{"quoted key", "---\n\"paths\": src/**\n---\nBODY", "BODY", true},
		{"other keys around it", "---\ndescription: d\npaths:\n  - lib/**\nowner: me\n---\nBODY", "BODY", true},
		{"trailing comment after the pattern", "---\npaths: src/** # TypeScript only\n---\nBODY", "BODY", true},
		{"a hash glued to the value is part of it", "---\npaths: **#x\n---\nBODY", "BODY", true},
		{"one scoped pattern among catch-alls", "---\npaths:\n  - \"**\"\n  - \"src/**\"\n---\nBODY", "BODY", true},

		// A `paths` key that scopes nothing: Claude Code applies the rule
		// everywhere, so it is loaded.
		{"empty value and no list", "---\npaths:\n---\nBODY", "BODY", false},
		{"empty flow list", "---\npaths: []\n---\nBODY", "BODY", false},
		{"catch-all", "---\npaths: \"**\"\n---\nBODY", "BODY", false},
		{"catch-all, block list", "---\npaths:\n  - \"**\"\n---\nBODY", "BODY", false},
		{"catch-all with a trailing glob", "---\npaths: \"**/**\"\n---\nBODY", "BODY", false},
		{"catch-all, comment after it", "---\npaths: \"**\" # everything\n---\nBODY", "BODY", false},
		{"only blanks", "---\npaths: \"\"\n---\nBODY", "BODY", false},
		{"comma-separated catch-alls", "---\npaths: \"**, **/**\"\n---\nBODY", "BODY", false},
		{"block list of catch-alls and an empty item", "---\npaths:\n  - \"**\"\n  - \"\"\n---\nBODY", "BODY", false},
		{"an empty paths does not take the next key's list", "---\npaths:\ntags:\n  - \"src/**\"\n---\nBODY", "BODY", false},

		// Not a top-level `paths` key.
		{"nested key", "---\nscope:\n  paths: src/**\n---\nBODY", "BODY", false},
		{"a key after a block scalar", "---\ndescription: |\n  intro\npaths: x\n---\nBODY", "BODY", true},
		{"indented line of a block scalar", "---\ndescription: |\n  paths: src/**\n---\nBODY", "BODY", false},
		{"different key", "---\npathsExtra: src/**\n---\nBODY", "BODY", false},
		{"key prefix", "---\nmypaths: src/**\n---\nBODY", "BODY", false},
		{"paths only in the body", "---\ndescription: d\n---\npaths: src/**\nBODY", "paths: src/**\nBODY", false},
		{"paths only in a body with no frontmatter", "paths: src/**\nBODY", "paths: src/**\nBODY", false},

		// Frontmatter boundaries.
		{"CRLF, conditional", "---\r\npaths:\r\n  - \"a/**\"\r\n---\r\nBODY", "BODY", true},
		{"CRLF, plain", "---\r\ndescription: d\r\n---\r\nBODY", "BODY", false},
		{"BOM before the opening line", "\ufeff---\npaths: a/**\n---\nBODY", "BODY", true},
		{"BOM, plain", "\ufeff---\ndescription: d\n---\nBODY", "BODY", false},
		{"trailing blanks on the delimiters", "---  \npaths: a/**\n---\t\nBODY", "BODY", true},
		{"unclosed frontmatter is not frontmatter", "---\npaths: a/**\nBODY", "---\npaths: a/**\nBODY", false},
		{"a leading blank line is not frontmatter", "\n---\npaths: a/**\n---\nBODY", "\n---\npaths: a/**\n---\nBODY", false},
		{"--- inside a value is not the closing line", "---\ndescription: a---b\npaths: x/**\n---\nBODY", "BODY", true},
		{"a horizontal rule in the body", "---\ndescription: d\n---\nBODY\n---\nMORE", "BODY\n---\nMORE", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, conditional := parseRule([]byte(tc.in))
			if conditional != tc.conditional {
				t.Errorf("conditional = %v, want %v", conditional, tc.conditional)
			}
			if body != tc.body {
				t.Errorf("body = %q, want %q", body, tc.body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Rules through LoadMemory

func ruleOpts(l memoryLayout) MemoryOptions {
	return MemoryOptions{WalkUp: true, SkipUser: true, SkipOuter: true, ClaudeCodeLayout: true, Root: l.root}
}

// What the loader confines is the workspace: from Root (workDir alone without
// one) down. The user scope and the outer ancestors are the operator's own.
func TestMemoryRuleSymlinksStayInTheWorkspace(t *testing.T) {
	l := newMemoryLayout(t)
	elsewhere := filepath.Join(l.tmp, "elsewhere")
	writeFile(t, filepath.Join(elsewhere, "leak.md"), "LEAKED-FROM-OUTSIDE")
	writeFile(t, filepath.Join(l.root, "shared", "team.md"), "TEAM-INSIDE-THE-ROOT")

	link := func(target, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	// The repository links one rule inside itself and one out of the machine;
	// the same two out-of-tree links sit in the operator's directories.
	link(filepath.Join(l.root, "shared", "team.md"), filepath.Join(l.workDir, ".claude", "rules", "team.md"))
	link(filepath.Join(elsewhere, "leak.md"), filepath.Join(l.workDir, ".claude", "rules", "leak.md"))
	link(filepath.Join(elsewhere, "leak.md"), filepath.Join(l.root, ".claude", "rules", "leak-root.md"))
	link(filepath.Join(elsewhere, "leak.md"), filepath.Join(l.home, ".claude", "rules", "operator.md"))
	link(filepath.Join(elsewhere, "leak.md"), filepath.Join(l.outer, ".claude", "rules", "operator-ancestor.md"))

	count := func(out string) int { return strings.Count(out, "LEAKED-FROM-OUTSIDE") }

	// workspace: a Root, so the link to team.md (inside the root) is kept, the
	// links out are not.
	workspace := MemoryOptions{WalkUp: true, SkipUser: true, SkipOuter: true, ClaudeCodeLayout: true, Root: l.root}
	got, _ := LoadMemory(l.workDir, workspace)
	if n := count(got); n != 0 {
		t.Errorf("a repository's rule symlinked out of the workspace was loaded %d time(s):\n%s", n, got)
	}
	if !strings.Contains(got, "TEAM-INSIDE-THE-ROOT") {
		t.Errorf("a rule symlinked to elsewhere inside the root was dropped:\n%s", got)
	}
	// It is not even listed: the walk refuses the link, so the file never
	// becomes a discovery candidate.
	if _, ok := MemoryCandidateMtimes(l.workDir, workspace)[filepath.Join(l.root, ".claude", "rules", "leak-root.md")]; ok {
		t.Error("a rule symlinked out of the workspace is a discovery candidate")
	}

	// No Root: the workspace is workDir alone, and team.md (inside the repo
	// but outside workDir) no longer qualifies.
	noRoot := workspace
	noRoot.Root = ""
	got, _ = LoadMemory(l.workDir, noRoot)
	if strings.Contains(got, "TEAM-INSIDE-THE-ROOT") || count(got) != 0 {
		t.Errorf("without a Root only workDir is the workspace:\n%s", got)
	}

	// The operator's directories: the user scope and the outer ancestors.
	operator := MemoryOptions{WalkUp: true, SkipWorkspace: true, ClaudeCodeLayout: true, Root: l.root}
	got, _ = LoadMemory(l.workDir, operator)
	if n := count(got); n != 2 {
		t.Errorf("the operator's rule symlinks should be followed (want 2 loads, got %d):\n%s", n, got)
	}
}

// The user scope is the operator's own: symlinks in ~/.claude/rules lead
// wherever they like (dotfiles are routinely linked in). HOME sits outside
// workDir's ancestry here, so the user scope is the only way to reach them.
func TestMemoryUserRuleSymlinksAreFollowed(t *testing.T) {
	l := newMemoryLayout(t)
	home := filepath.Join(l.tmp, "unrelated-home")
	t.Setenv("HOME", home)
	dotfiles := filepath.Join(l.tmp, "dotfiles")
	writeFile(t, filepath.Join(dotfiles, "file.md"), "USER-LINKED-FILE")
	writeFile(t, filepath.Join(dotfiles, "dir", "nested.md"), "USER-LINKED-DIR-FILE")

	rules := filepath.Join(home, ".claude", "rules")
	if err := os.MkdirAll(rules, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dotfiles, "file.md"), filepath.Join(rules, "file.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(dotfiles, "dir"), filepath.Join(rules, "dir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, _ := LoadMemory(l.workDir, MemoryOptions{ClaudeCodeLayout: true})
	for _, want := range []string{"USER-LINKED-FILE", "USER-LINKED-DIR-FILE"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s: a symlink in the user's rules was not followed:\n%s", want, got)
		}
	}
}

func TestMemoryRuleFrontmatterIsStrippedAndConditionalRulesAreLeftOut(t *testing.T) {
	l := newMemoryLayout(t)
	rules := filepath.Join(l.workDir, ".claude", "rules")
	writeFile(t, filepath.Join(rules, "plain.md"), "---\ndescription: FRONTMATTER-SECRET\n---\nPLAIN-BODY")
	writeFile(t, filepath.Join(rules, "scoped.md"), "---\npaths:\n  - \"src/**\"\n---\nSCOPED-BODY")
	writeFile(t, filepath.Join(rules, "catchall.md"), "---\npaths: \"**\"\n---\nCATCHALL-BODY")

	got, _ := LoadMemory(l.workDir, ruleOpts(l))

	for _, want := range []string{"PLAIN-BODY", "CATCHALL-BODY"} {
		if !strings.Contains(got, want) {
			t.Errorf("rule body %q missing:\n%s", want, got)
		}
	}
	for _, leaked := range []string{"FRONTMATTER-SECRET", "SCOPED-BODY", "description:", "paths:"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q must not reach the prompt:\n%s", leaked, got)
		}
	}
}

func TestMemoryRulesLoadInRelativePathOrder(t *testing.T) {
	l := newMemoryLayout(t)
	rules := filepath.Join(l.workDir, ".claude", "rules")
	for _, f := range []string{"b.md", "a/b.md", "a.md", "a/z.md"} {
		writeFile(t, filepath.Join(rules, filepath.FromSlash(f)), "RULE<"+f+">")
	}

	got, _ := LoadMemory(l.workDir, ruleOpts(l))

	var order []string
	for _, f := range []string{"a.md", "a/b.md", "a/z.md", "b.md"} {
		order = append(order, "RULE<"+f+">")
	}
	last := -1
	for _, marker := range order {
		i := strings.Index(got, marker)
		if i < 0 {
			t.Fatalf("%s missing:\n%s", marker, got)
		}
		if i < last {
			t.Errorf("%s loaded out of order (want %v):\n%s", marker, order, got)
		}
		last = i
	}
	if !strings.Contains(got, "## Project rule (.claude/rules/a/z.md)") {
		t.Errorf("nested rule not labelled by its relative path:\n%s", got)
	}
}

func TestMemoryRuleImportsExpandAndConditionalRulesImportNothing(t *testing.T) {
	l := newMemoryLayout(t)
	dot := filepath.Join(l.workDir, ".claude")
	writeFile(t, filepath.Join(dot, "shared.md"), "SHARED-IMPORT")
	writeFile(t, filepath.Join(dot, "hidden.md"), "HIDDEN-IMPORT")
	writeFile(t, filepath.Join(dot, "rules", "plain.md"), "see @../shared.md")
	writeFile(t, filepath.Join(dot, "rules", "scoped.md"), "---\npaths: src/**\n---\nsee @../hidden.md")

	opts := ruleOpts(l)
	opts.Imports = true
	got, mtimes := LoadMemory(l.workDir, opts)

	if !strings.Contains(got, "SHARED-IMPORT") {
		t.Errorf("an @import inside a rule was not expanded:\n%s", got)
	}
	if strings.Contains(got, "HIDDEN-IMPORT") {
		t.Errorf("a conditional rule's import reached the prompt:\n%s", got)
	}
	if _, ok := mtimes[filepath.Join(dot, "shared.md")]; !ok {
		t.Errorf("the rule's import is not tracked for cache revalidation: %v", mtimes)
	}

	opts.Imports = false
	if got, _ = LoadMemory(l.workDir, opts); strings.Contains(got, "SHARED-IMPORT") {
		t.Errorf("import expanded with Imports off:\n%s", got)
	}
}

// A conditional rule costs nothing: it is not part of the prompt, so it must
// not eat the byte budget the rules after it are loaded under.
func TestMemoryConditionalRuleDoesNotConsumeTheBudget(t *testing.T) {
	l := newMemoryLayout(t)
	rules := filepath.Join(l.workDir, ".claude", "rules")
	// Bigger than the whole budget: counted, it would leave nothing for the
	// rule after it.
	writeFile(t, filepath.Join(rules, "a-scoped.md"), "---\npaths: src/**\n---\n"+strings.Repeat("x", 4000))
	writeFile(t, filepath.Join(rules, "b-plain.md"), "PLAIN-AFTER-THE-SCOPED-ONE")

	opts := ruleOpts(l)
	opts.MaxBytes = 2000
	got, _ := LoadMemory(l.workDir, opts)

	if !strings.Contains(got, "PLAIN-AFTER-THE-SCOPED-ONE") {
		t.Errorf("a skipped conditional rule used up the budget:\n%s", got)
	}
}

func TestMemoryRulesShareTheByteCap(t *testing.T) {
	l := newMemoryLayout(t)
	rules := filepath.Join(l.workDir, ".claude", "rules")
	for _, f := range []string{"a.md", "b.md", "c.md"} {
		writeFile(t, filepath.Join(rules, f), "RULE-"+f+" "+strings.Repeat("y", 1000))
	}

	opts := ruleOpts(l)
	opts.MaxBytes = 1800
	got, _ := LoadMemory(l.workDir, opts)

	if !strings.Contains(got, "RULE-a.md") {
		t.Errorf("the first rule is missing:\n%s", got)
	}
	if !strings.Contains(got, "... (truncated)") {
		t.Errorf("the cap did not apply across rules:\n%s", got)
	}
	if strings.Contains(got, "RULE-c.md") {
		t.Errorf("a rule past the cap was loaded:\n%s", got)
	}
}

// The user rules come with the user scope and go with SkipUser; they follow
// the layout flag.
func TestMemoryUserRulesFollowTheUserScopeAndTheLayout(t *testing.T) {
	l := newMemoryLayout(t)
	rules := filepath.Join(l.home, ".claude", "rules")
	writeFile(t, filepath.Join(rules, "team", "style.md"), "USER-NESTED-RULE")
	writeFile(t, filepath.Join(rules, "framed.md"), "---\ndescription: USER-FRONTMATTER\n---\nUSER-FRAMED-BODY")
	writeFile(t, filepath.Join(rules, "scoped.md"), "---\npaths: src/**\n---\nUSER-SCOPED-BODY")

	got, _ := LoadMemory(l.workDir, MemoryOptions{ClaudeCodeLayout: true})
	assertMarkers(t, got, mkUser, mkUserRule, mkSub)
	if !strings.Contains(got, "USER-NESTED-RULE") || !strings.Contains(got, "## User rule (~/.claude/rules/team/style.md)") {
		t.Errorf("nested user rule missing or mislabelled:\n%s", got)
	}
	if !strings.Contains(got, "USER-FRAMED-BODY") || strings.Contains(got, "USER-FRONTMATTER") {
		t.Errorf("a user rule's frontmatter must be stripped and its body kept:\n%s", got)
	}
	if strings.Contains(got, "USER-SCOPED-BODY") {
		t.Errorf("a conditional user rule must be left out:\n%s", got)
	}

	got, _ = LoadMemory(l.workDir, MemoryOptions{ClaudeCodeLayout: true, SkipUser: true})
	assertMarkers(t, got, mkSub)
	if strings.Contains(got, "USER-NESTED-RULE") {
		t.Errorf("a user rule survived SkipUser:\n%s", got)
	}

	got, _ = LoadMemory(l.workDir, MemoryOptions{})
	assertMarkers(t, got, mkUser, mkSub)
	if strings.Contains(got, "USER-NESTED-RULE") {
		t.Errorf("a user rule loaded without the layout:\n%s", got)
	}
}
