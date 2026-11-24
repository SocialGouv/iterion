package runtime

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	mkUser     = "[[M-USER]]"
	mkUserRule = "[[M-USER-RULE]]"
	mkOuter    = "[[M-OUTER]]"
	mkRoot     = "[[M-ROOT]]"
	mkRule     = "[[M-RULE]]"
	mkCond     = "[[M-COND]]"
	mkSub      = "[[M-SUB]]"
)

var allMarkers = []string{mkUser, mkUserRule, mkOuter, mkRoot, mkRule, mkCond, mkSub}

// scopeTree is the on-disk layout the scope is exercised on; HOME points at
// tmp/home so no real user file leaks in:
//
//	tmp/home/.claude/CLAUDE.md                    mkUser
//	tmp/home/.claude/rules/u.md                   mkUserRule
//	tmp/home/lab/CLAUDE.md                        mkOuter  (outer ancestor)
//	tmp/home/lab/repo/CLAUDE.md                   mkRoot   (the root)
//	tmp/home/lab/repo/.claude/rules/r.md          mkRule
//	tmp/home/lab/repo/.claude/rules/cond.md       mkCond   (paths: frontmatter)
//	tmp/home/lab/repo/sub/CLAUDE.md               mkSub    (workDir)
type scopeTree struct {
	tmp, home, outer, root, workDir string
}

func newScopeTree(t *testing.T) scopeTree {
	t.Helper()
	tmp := t.TempDir()
	tr := scopeTree{tmp: tmp, home: filepath.Join(tmp, "home")}
	tr.outer = filepath.Join(tr.home, "lab")
	tr.root = filepath.Join(tr.outer, "repo")
	tr.workDir = filepath.Join(tr.root, "sub")
	t.Setenv("HOME", tr.home)
	t.Setenv("CLAW_MEMORY_DIR", "")

	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(tr.home, ".claude", "CLAUDE.md"), mkUser)
	write(filepath.Join(tr.home, ".claude", "rules", "u.md"), mkUserRule)
	write(filepath.Join(tr.outer, "CLAUDE.md"), mkOuter)
	write(filepath.Join(tr.root, "CLAUDE.md"), mkRoot)
	write(filepath.Join(tr.root, ".claude", "rules", "r.md"), mkRule)
	write(filepath.Join(tr.root, ".claude", "rules", "cond.md"), "---\npaths:\n  - \"src/**/*.go\"\n---\n"+mkCond)
	write(filepath.Join(tr.workDir, "CLAUDE.md"), mkSub)
	return tr
}

// markersIn lists the fixture markers in out, in order of appearance.
func markersIn(out string) []string {
	type hit struct {
		at     int
		marker string
	}
	var hits []hit
	for _, m := range allMarkers {
		for from := 0; ; {
			i := strings.Index(out[from:], m)
			if i < 0 {
				break
			}
			hits = append(hits, hit{from + i, m})
			from += i + len(m)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].at < hits[j].at })
	got := make([]string, len(hits))
	for i, h := range hits {
		got[i] = h.marker
	}
	return got
}

func assertMarkers(t *testing.T, out string, want ...string) {
	t.Helper()
	if got := markersIn(out); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("markers loaded:\n got  %v\n want %v\noutput:\n%s", got, want, out)
	}
}

// projectInstructionsOnly is the host's usual starting point: no section but
// the CLAUDE.md one, with the ancestor walk on.
func projectInstructionsOnly() PromptConfig {
	cfg := MinimalPromptConfig()
	cfg.ProjectInstructions = true
	cfg.MemoryWalkUp = true
	return cfg
}

func TestBuildSystemContextMemoryScope(t *testing.T) {
	tr := newScopeTree(t)

	with := func(set func(*PromptConfig)) PromptConfig {
		cfg := projectInstructionsOnly()
		set(&cfg)
		return cfg
	}
	workspace := func(c *PromptConfig) {
		c.MemorySkipUser, c.MemorySkipOuter, c.MemoryRoot = true, true, tr.root
	}
	operator := func(c *PromptConfig) {
		c.MemorySkipWorkspace, c.MemoryRoot = true, tr.root
	}
	layout := func(c *PromptConfig) { c.MemoryClaudeCodeLayout = true }

	cases := []struct {
		name string
		cfg  PromptConfig
		want []string
	}{
		{"scope unset is today's behaviour", with(func(*PromptConfig) {}), []string{mkUser, mkOuter, mkRoot, mkSub}},
		{"the default prompt config is unscoped", DefaultPromptConfig(), []string{mkUser, mkOuter, mkRoot, mkSub}},
		{"workspace", with(workspace), []string{mkRoot, mkSub}},
		{"workspace + layout", with(func(c *PromptConfig) { workspace(c); layout(c) }), []string{mkRoot, mkRule, mkSub}},
		{"operator", with(operator), []string{mkUser, mkOuter}},
		{"operator + layout", with(func(c *PromptConfig) { operator(c); layout(c) }), []string{mkUser, mkUserRule, mkOuter}},
		{"all + layout", with(func(c *PromptConfig) { c.MemoryRoot = tr.root; layout(c) }),
			[]string{mkUser, mkUserRule, mkOuter, mkRoot, mkRule, mkSub}},
		{"layout, no Root", with(layout), []string{mkUser, mkUserRule, mkOuter, mkRoot, mkRule, mkSub}},
		{"workspace without a Root keeps only workDir",
			with(func(c *PromptConfig) { c.MemorySkipUser, c.MemorySkipOuter = true, true; layout(c) }), []string{mkSub}},
		{"a Root that does not contain workDir is unset",
			with(func(c *PromptConfig) { workspace(c); layout(c); c.MemoryRoot = filepath.Join(tr.tmp, "elsewhere") }),
			[]string{mkSub}},
		{"no walk-up: only the user scope and workDir",
			with(func(c *PromptConfig) { layout(c); c.MemoryWalkUp = false }), []string{mkUser, mkUserRule, mkSub}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildSystemContext(tr.workDir, tc.cfg)
			assertMarkers(t, got, tc.want...)
			if len(tc.want) > 0 && !strings.Contains(got, "# Project Instructions (CLAUDE.md)\n\n") {
				t.Errorf("the memory is not rendered as the project-instructions section:\n%s", got)
			}
		})
	}
}

// Two builds on the same directory with different scopes never share a
// result, in either order.
func TestBuildSystemContextScopesDoNotLeakAcrossCalls(t *testing.T) {
	tr := newScopeTree(t)

	workspace := projectInstructionsOnly()
	workspace.MemorySkipUser, workspace.MemorySkipOuter, workspace.MemoryRoot = true, true, tr.root
	operator := projectInstructionsOnly()
	operator.MemorySkipWorkspace, operator.MemoryRoot = true, tr.root

	for i := 0; i < 2; i++ {
		assertMarkers(t, BuildSystemContext(tr.workDir, workspace), mkRoot, mkSub)
		assertMarkers(t, BuildSystemContext(tr.workDir, operator), mkUser, mkOuter)
	}
}

// The scope fields are values rather than sections, so the CLI-style
// resolver neither knows nor lists them, and what it resolves leaves them
// unset.
func TestResolvePromptSectionsKnowsNoMemoryScope(t *testing.T) {
	for _, name := range []string{"memory-skip-user", "memory-root", "memory-skip-workspace", "memory-skip-outer", "memory-claude-code-layout"} {
		if _, err := ResolvePromptSections(false, []string{name}, nil); err == nil {
			t.Errorf("%q resolved as a section", name)
		}
		if _, err := ResolvePromptSections(false, nil, []string{name}); err == nil {
			t.Errorf("%q resolved as a disabled section", name)
		}
	}

	cfg, err := ResolvePromptSections(false, []string{"project-instructions", "memory-walk-up"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MemorySkipUser || cfg.MemoryRoot != "" || cfg.MemorySkipWorkspace || cfg.MemorySkipOuter || cfg.MemoryClaudeCodeLayout {
		t.Errorf("a resolved config must leave the scope unset: %+v", cfg)
	}
}
