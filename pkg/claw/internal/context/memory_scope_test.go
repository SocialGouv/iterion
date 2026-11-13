package context

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The four scope shapes a host builds out of the options. The layout has HOME
// (tmp/home) and the outer ancestor (tmp/home/lab) above the root, so every
// scope has a file of its own to appear or not.
func scopeShapes(l memoryLayout) (workspace, operator, all MemoryOptions) {
	workspace = MemoryOptions{WalkUp: true, SkipUser: true, SkipOuter: true, Root: l.root}
	operator = MemoryOptions{WalkUp: true, SkipWorkspace: true, Root: l.root}
	all = MemoryOptions{WalkUp: true, Root: l.root}
	return workspace, operator, all
}

func withLayout(o MemoryOptions) MemoryOptions {
	o.ClaudeCodeLayout = true
	return o
}

func TestMemoryScopeMatrix(t *testing.T) {
	l := newMemoryLayout(t)
	workspace, operator, all := scopeShapes(l)

	// Root moved up the tree: it is inclusive, so the directory it names joins
	// the workspace along with everything between it and workDir.
	rootAtHome := workspace
	rootAtHome.Root = l.home
	rootAtLab := workspace
	rootAtLab.Root = l.outer

	cases := []struct {
		name string
		opts MemoryOptions
		want []string // fixture markers, in load order
	}{
		// What is read today, with no scope option set.
		{"zero options", MemoryOptions{}, []string{mkUser, mkSub}},
		{"default options", DefaultMemoryOptions(), []string{mkUser, mkOuter, mkRoot, mkSub}},
		{"Root set but nothing skipped changes nothing", all, []string{mkUser, mkOuter, mkRoot, mkSub}},

		// The workspace policy: the repository's files and nothing above it.
		{"workspace", workspace, []string{mkRoot, mkSub}},
		{"workspace + layout", withLayout(workspace), []string{mkRoot, mkRule, mkSub}},

		// The operator policy: what the host brings, not the repository.
		{"operator", operator, []string{mkUser, mkOuter}},
		{"operator + layout", withLayout(operator), []string{mkUser, mkUserRule, mkOuter}},

		// Everything, layout off then on.
		{"all, layout off", all, []string{mkUser, mkOuter, mkRoot, mkSub}},
		{"all + layout", withLayout(all), []string{mkUser, mkUserRule, mkOuter, mkRoot, mkRule, mkSub}},

		// Root unset: every ancestor is outer.
		{"layout, Root unset", MemoryOptions{WalkUp: true, ClaudeCodeLayout: true},
			[]string{mkUser, mkUserRule, mkOuter, mkRoot, mkRule, mkSub}},
		{"SkipOuter, Root unset, drops every ancestor", MemoryOptions{WalkUp: true, SkipOuter: true},
			[]string{mkUser, mkSub}},
		{"workspace policy without a Root keeps only workDir",
			MemoryOptions{WalkUp: true, SkipUser: true, SkipOuter: true, ClaudeCodeLayout: true},
			[]string{mkSub}},
		{"SkipWorkspace, Root unset: the repository counts as outer",
			MemoryOptions{WalkUp: true, SkipWorkspace: true, ClaudeCodeLayout: true},
			[]string{mkUser, mkUserRule, mkOuter, mkRoot, mkRule}},

		// Ancestors need WalkUp: without it only the user scope and workDir.
		{"layout without WalkUp", MemoryOptions{ClaudeCodeLayout: true, Root: l.root},
			[]string{mkUser, mkUserRule, mkSub}},
		{"SkipOuter without WalkUp is a no-op", MemoryOptions{SkipOuter: true, Root: l.root},
			[]string{mkUser, mkSub}},

		// SkipUser on its own.
		{"SkipUser, layout off", MemoryOptions{WalkUp: true, SkipUser: true, Root: l.root},
			[]string{mkOuter, mkRoot, mkSub}},

		// Root is inclusive, and moves the boundary.
		{"Root at HOME includes HOME's own .claude", withLayout(rootAtHome),
			[]string{mkUser, mkUserRule, mkOuter, mkRoot, mkRule, mkSub}},
		{"Root at the outer ancestor includes it", rootAtLab, []string{mkOuter, mkRoot, mkSub}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := LoadMemory(l.workDir, tc.opts)
			assertMarkers(t, got, tc.want...)
		})
	}
}

// workDir is the root itself: it has no inner ancestor, and everything above
// it is outer.
func TestMemoryScopeWorkDirIsRoot(t *testing.T) {
	l := newMemoryLayout(t)

	workspace := MemoryOptions{WalkUp: true, SkipUser: true, SkipOuter: true, ClaudeCodeLayout: true, Root: l.root}
	got, _ := LoadMemory(l.root, workspace)
	assertMarkers(t, got, mkRoot, mkRule)
	assertSections(t, got, l.tmp,
		"## Project root (CLAUDE.md)\n\n"+mkRoot,
		"## Project rule (.claude/rules/r.md)\n\n"+mkRule,
	)

	operator := MemoryOptions{WalkUp: true, SkipWorkspace: true, Root: l.root}
	got, _ = LoadMemory(l.root, operator)
	assertMarkers(t, got, mkUser, mkOuter)
}

// A Root that does not contain workDir bounds nothing, so it is treated as
// unset: every ancestor is outer. It must not be read as "everything is
// inside" (which would let the workspace policy load the whole chain) nor
// matched by prefix.
func TestMemoryScopeRootThatDoesNotContainWorkDirIsUnset(t *testing.T) {
	l := newMemoryLayout(t)

	// /…/lab/rep is a textual prefix of /…/lab/repo; /…/lab/other is a
	// sibling; /…/lab/repo/sub/deeper is below workDir. All exist, so the
	// identity comparison runs against real directories.
	prefixSibling := filepath.Join(l.outer, "rep")
	sibling := filepath.Join(l.outer, "other")
	below := filepath.Join(l.workDir, "deeper")
	for _, dir := range []string{prefixSibling, sibling, below} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for name, root := range map[string]string{
		"textual prefix of an ancestor": prefixSibling,
		"sibling":                       sibling,
		"below workDir":                 below,
		"does not exist":                filepath.Join(l.tmp, "nowhere"),
	} {
		t.Run(name, func(t *testing.T) {
			workspace := MemoryOptions{WalkUp: true, SkipUser: true, SkipOuter: true, ClaudeCodeLayout: true, Root: root}
			got, _ := LoadMemory(l.workDir, workspace)
			// Unset Root + SkipOuter: no ancestor at all, only workDir.
			assertMarkers(t, got, mkSub)

			operator := MemoryOptions{WalkUp: true, SkipWorkspace: true, ClaudeCodeLayout: true, Root: root}
			got, _ = LoadMemory(l.workDir, operator)
			// Unset Root + SkipWorkspace: the whole chain is outer.
			assertMarkers(t, got, mkUser, mkUserRule, mkOuter, mkRoot, mkRule)
		})
	}
}

// A relative Root resolves against the working directory, like workDir.
func TestMemoryScopeRelativeRootResolvesAgainstCwd(t *testing.T) {
	l := newMemoryLayout(t)
	t.Chdir(l.outer)

	workspace := MemoryOptions{WalkUp: true, SkipUser: true, SkipOuter: true, ClaudeCodeLayout: true, Root: "repo"}
	got, _ := LoadMemory(l.workDir, workspace)
	assertMarkers(t, got, mkRoot, mkRule, mkSub)
}

// workDir may be relative: it resolves against the working directory, so "."
// is the directory the process is in, with its ancestors.
func TestMemoryScopeRelativeWorkDir(t *testing.T) {
	l := newMemoryLayout(t)

	for _, tc := range []struct{ name, cwd, workDir string }{
		{"dot", l.workDir, "."},
		{"a name below the working directory", l.root, "sub"},
		{"a parent reference", l.outer, "repo/../repo/sub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.cwd)
			got, _ := LoadMemory(tc.workDir, MemoryOptions{WalkUp: true})
			assertMarkers(t, got, mkUser, mkOuter, mkRoot, mkSub)
		})
	}
}

// The same directory spelled through a symlink is the same directory: a
// workDir reached by one spelling and a Root by another must still bound the
// walk, or the workspace policy would silently drop the repository's own
// ancestors (the host got Root from `git rev-parse --show-toplevel`, which
// resolves symlinks, and workDir from a configured path, which does not).
func TestMemoryScopeRootMatchesBySymlinkedSpelling(t *testing.T) {
	l := newMemoryLayout(t)

	// tmp/link -> tmp/home/lab/repo
	link := filepath.Join(l.tmp, "link")
	if err := os.Symlink(l.root, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	workspace := MemoryOptions{WalkUp: true, SkipUser: true, SkipOuter: true, ClaudeCodeLayout: true}

	t.Run("workDir through the symlink, Root real", func(t *testing.T) {
		workspace := workspace
		workspace.Root = l.root
		got, _ := LoadMemory(filepath.Join(link, "sub"), workspace)
		assertMarkers(t, got, mkRoot, mkRule, mkSub)
	})
	t.Run("workDir real, Root through the symlink", func(t *testing.T) {
		workspace := workspace
		workspace.Root = link
		got, _ := LoadMemory(l.workDir, workspace)
		assertMarkers(t, got, mkRoot, mkRule, mkSub)
	})
	t.Run("workDir is the root, spelled differently", func(t *testing.T) {
		workspace := workspace
		workspace.Root = l.root
		got, _ := LoadMemory(link, workspace)
		assertMarkers(t, got, mkRoot, mkRule)
	})
}

// One file, one load. HOME is an ancestor of workDir, so ~/.claude/CLAUDE.md
// is both the user file and the ancestor's .claude/CLAUDE.md: the first,
// the user scope, wins.
func TestMemoryScopeDedupsTheFileReachedAsUserAndAsAncestor(t *testing.T) {
	l := newMemoryLayout(t)

	got, _ := LoadMemory(l.workDir, withLayout(MemoryOptions{WalkUp: true, Root: l.root}))

	for _, marker := range []string{mkUser, mkUserRule} {
		if n := strings.Count(got, marker); n != 1 {
			t.Errorf("%s loaded %d times, want 1:\n%s", marker, n, got)
		}
	}
	assertSections(t, got, l.tmp,
		"## User global (~/.claude/CLAUDE.md)\n\n"+mkUser,
		"## User rule (~/.claude/rules/u.md)\n\n"+mkUserRule,
		"## Ancestor ("+filepath.Join(l.outer, "CLAUDE.md")+")\n\n"+mkOuter,
		"## Ancestor ("+filepath.Join(l.root, "CLAUDE.md")+")\n\n"+mkRoot,
		"## Ancestor rule ("+filepath.Join(l.root, ".claude", "rules", "r.md")+")\n\n"+mkRule,
		"## Project root (CLAUDE.md)\n\n"+mkSub,
	)
}

// SkipUser removes the user scope, not the file: HOME is an ancestor of
// workDir, so ~/.claude is also an outer ancestor's .claude, and with the
// ancestors on it loads there under its ancestor label (Claude Code
// behaves the same way). SkipOuter is what keeps it out.
func TestMemorySkipUserRemovesTheScopeNotTheFile(t *testing.T) {
	l := newMemoryLayout(t)

	skipUser := withLayout(MemoryOptions{WalkUp: true, SkipUser: true, Root: l.root})
	got, _ := LoadMemory(l.workDir, skipUser)
	assertMarkers(t, got, mkUser, mkUserRule, mkOuter, mkRoot, mkRule, mkSub)
	assertSections(t, got, l.tmp,
		"## Ancestor ("+filepath.Join(l.home, ".claude", "CLAUDE.md")+")\n\n"+mkUser,
		"## Ancestor rule ("+filepath.Join(l.home, ".claude", "rules", "u.md")+")\n\n"+mkUserRule,
		"## Ancestor ("+filepath.Join(l.outer, "CLAUDE.md")+")\n\n"+mkOuter,
		"## Ancestor ("+filepath.Join(l.root, "CLAUDE.md")+")\n\n"+mkRoot,
		"## Ancestor rule ("+filepath.Join(l.root, ".claude", "rules", "r.md")+")\n\n"+mkRule,
		"## Project root (CLAUDE.md)\n\n"+mkSub,
	)

	skipUser.SkipOuter = true
	got, _ = LoadMemory(l.workDir, skipUser)
	assertMarkers(t, got, mkRoot, mkRule, mkSub)
}

// Labels, order and the per-directory sequence on a tree with a
// .claude/CLAUDE.md and rules at every level.
func TestMemoryScopeLabelsAndOrder(t *testing.T) {
	l := newMemoryLayout(t).withExtras(t)

	got, _ := LoadMemory(l.workDir, withLayout(MemoryOptions{WalkUp: true, Root: l.root}))

	assertMarkers(t, got,
		mkUser, mkUserRule,
		mkOuter, mkOuterCfg, mkOuterRule,
		mkRoot, mkRootCfg, mkRule,
		mkSub, mkSubCfg, mkSubRule,
	)
	assertSections(t, got, l.tmp,
		"## User global (~/.claude/CLAUDE.md)\n\n"+mkUser,
		"## User rule (~/.claude/rules/u.md)\n\n"+mkUserRule,
		"## Ancestor ("+filepath.Join(l.outer, "CLAUDE.md")+")\n\n"+mkOuter,
		"## Ancestor ("+filepath.Join(l.outer, ".claude", "CLAUDE.md")+")\n\n"+mkOuterCfg,
		"## Ancestor rule ("+filepath.Join(l.outer, ".claude", "rules", "or.md")+")\n\n"+mkOuterRule,
		"## Ancestor ("+filepath.Join(l.root, "CLAUDE.md")+")\n\n"+mkRoot,
		"## Ancestor ("+filepath.Join(l.root, ".claude", "CLAUDE.md")+")\n\n"+mkRootCfg,
		"## Ancestor rule ("+filepath.Join(l.root, ".claude", "rules", "r.md")+")\n\n"+mkRule,
		"## Project root (CLAUDE.md)\n\n"+mkSub,
		"## Project config (.claude/CLAUDE.md)\n\n"+mkSubCfg,
		"## Project rule (.claude/rules/sr.md)\n\n"+mkSubRule,
	)

	// Outer ancestors load before inner ones, and both before workDir, even
	// when only part of the chain is wanted.
	workspace, operator, _ := scopeShapes(l)
	got, _ = LoadMemory(l.workDir, withLayout(operator))
	assertMarkers(t, got, mkUser, mkUserRule, mkOuter, mkOuterCfg, mkOuterRule)
	got, _ = LoadMemory(l.workDir, withLayout(workspace))
	assertMarkers(t, got, mkRoot, mkRootCfg, mkRule, mkSub, mkSubCfg, mkSubRule)
}

// With the scope fields zero, ancestors' .claude/CLAUDE.md and every rule
// stay unread no matter what else is on.
func TestMemoryScopeLayoutIsWhatLoadsRules(t *testing.T) {
	l := newMemoryLayout(t).withExtras(t)
	_, _, all := scopeShapes(l)

	off, _ := LoadMemory(l.workDir, all)
	assertMarkers(t, off, mkUser, mkOuter, mkRoot, mkSub, mkSubCfg)

	on, _ := LoadMemory(l.workDir, withLayout(all))
	if len(markersIn(on)) <= len(markersIn(off)) {
		t.Errorf("layout on loaded no more than layout off:\noff: %v\non:  %v", markersIn(off), markersIn(on))
	}
}

func TestMemoryScopeMtimesFollowTheScope(t *testing.T) {
	l := newMemoryLayout(t)
	workspace, _, _ := scopeShapes(l)
	workspace = withLayout(workspace)

	rule := filepath.Join(l.root, ".claude", "rules", "r.md")
	cond := filepath.Join(l.root, ".claude", "rules", "cond.md")
	user := filepath.Join(l.home, ".claude", "CLAUDE.md")
	outer := filepath.Join(l.outer, "CLAUDE.md")

	_, loaded := LoadMemory(l.workDir, workspace)
	for _, want := range []string{rule, cond, filepath.Join(l.root, "CLAUDE.md"), filepath.Join(l.workDir, "CLAUDE.md")} {
		if _, ok := loaded[want]; !ok {
			t.Errorf("%s was read but has no mtime entry: %v", want, loaded)
		}
	}
	for _, skipped := range []string{user, outer} {
		if _, ok := loaded[skipped]; ok {
			t.Errorf("%s is out of scope but was read: %v", skipped, loaded)
		}
		if _, ok := MemoryCandidateMtimes(l.workDir, workspace)[skipped]; ok {
			t.Errorf("%s is out of scope but is a discovery candidate", skipped)
		}
	}
	for _, want := range []string{rule, cond} {
		if _, ok := MemoryCandidateMtimes(l.workDir, workspace)[want]; !ok {
			t.Errorf("%s is in scope but not a discovery candidate", want)
		}
	}
}

// ---------------------------------------------------------------------------
// The assembler cache is valid for one (workDir, memory options), and tells
// scopes apart.

func TestAssemblerBuildsOnTheSameDirWithDifferentScopes(t *testing.T) {
	l := newMemoryLayout(t)
	workspace, operator, all := scopeShapes(l)

	build := func(m MemoryOptions) string {
		opts := DefaultAssembleOptions()
		opts.Environment, opts.GitStatus, opts.AutoMemory = false, false, false
		opts.Memory = m
		return NewAssemblerWithOptions(l.workDir, opts).Assemble()
	}

	assertMarkers(t, build(workspace), mkRoot, mkSub)
	assertMarkers(t, build(operator), mkUser, mkOuter)
	assertMarkers(t, build(all), mkUser, mkOuter, mkRoot, mkSub)
	assertMarkers(t, build(workspace), mkRoot, mkSub) // and back
}

// optionFlip is a pair of option sets that render differently on the layout,
// differing in one option.
type optionFlip struct {
	name     string
	from, to MemoryOptions
}

// optionFlips has one row per MemoryOptions field that decides what is loaded.
func optionFlips(l memoryLayout) []optionFlip {
	base := MemoryOptions{WalkUp: true, Root: l.root, ClaudeCodeLayout: true}
	with := func(o MemoryOptions, set func(*MemoryOptions)) MemoryOptions { set(&o); return o }
	skipOuter := with(base, func(o *MemoryOptions) { o.SkipOuter = true })
	return []optionFlip{
		{"SkipUser", base, with(base, func(o *MemoryOptions) { o.SkipUser = true })},
		{"SkipWorkspace", base, with(base, func(o *MemoryOptions) { o.SkipWorkspace = true })},
		{"SkipOuter", base, skipOuter},
		{"ClaudeCodeLayout", base, with(base, func(o *MemoryOptions) { o.ClaudeCodeLayout = false })},
		{"Root", skipOuter, with(skipOuter, func(o *MemoryOptions) { o.Root = l.outer })},
		{"WalkUp", base, with(base, func(o *MemoryOptions) { o.WalkUp = false })},
		{"Imports", with(base, func(o *MemoryOptions) { o.Imports = true }), base},
		{"MaxBytes", base, with(base, func(o *MemoryOptions) { o.MaxBytes = 120 })},
	}
}

// fixtureWithImport makes workDir's CLAUDE.md import a file, so Imports and
// MaxBytes have something to change.
func fixtureWithImport(t *testing.T, l memoryLayout) {
	t.Helper()
	writeFile(t, filepath.Join(l.workDir, "imp.md"), "IMPORTED-BODY")
	writeFile(t, filepath.Join(l.workDir, "CLAUDE.md"), mkSub+" @./imp.md")
}

// One assembler, its options changed in place: what it serves must be what a
// fresh assembler with the new options builds. The assembler's options are
// private and never reassigned in production; this holds the cache to its own
// key rather than to that convention.
func TestAssemblerCacheKeyCoversEveryMemoryOption(t *testing.T) {
	l := newMemoryLayout(t)
	fixtureWithImport(t, l)

	fresh := func(m MemoryOptions) string {
		return NewAssemblerWithOptions(l.workDir, AssembleOptions{ProjectInstructions: true, Memory: m}).Assemble()
	}

	for _, tc := range optionFlips(l) {
		t.Run(tc.name, func(t *testing.T) {
			wantFrom, wantTo := fresh(tc.from), fresh(tc.to)
			if wantFrom == wantTo {
				t.Fatalf("the two option sets render the same, so the case proves nothing:\n%s", wantFrom)
			}

			a := NewAssemblerWithOptions(l.workDir, AssembleOptions{ProjectInstructions: true, Memory: tc.from})
			if got := a.Assemble(); got != wantFrom {
				t.Fatalf("first build:\n got  %q\n want %q", got, wantFrom)
			}
			a.opts.Memory = tc.to
			if got := a.Assemble(); got != wantTo {
				t.Errorf("after changing %s the cache served the old scope:\n got  %q\n want %q", tc.name, got, wantTo)
			}
			a.opts.Memory = tc.from
			if got := a.Assemble(); got != wantFrom {
				t.Errorf("changing %s back did not return to the first scope:\n got  %q\n want %q", tc.name, got, wantFrom)
			}
		})
	}
}

// The same flips with the file-state check held still. Two scopes can read
// the same files and the discovery-candidate comparison then passes on its
// own; most flips above also move the candidates, so they would be caught
// without the key. Here the candidates are made to agree, and only the key can
// tell the scopes apart.
func TestAssemblerCacheKeyAloneSeparatesEveryMemoryOption(t *testing.T) {
	l := newMemoryLayout(t)
	fixtureWithImport(t, l)

	fresh := func(m MemoryOptions) string {
		return NewAssemblerWithOptions(l.workDir, AssembleOptions{ProjectInstructions: true, Memory: m}).Assemble()
	}

	for _, tc := range optionFlips(l) {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAssemblerWithOptions(l.workDir, AssembleOptions{ProjectInstructions: true, Memory: tc.from})
			a.Assemble()

			a.opts.Memory = tc.to
			a.memCandidates = MemoryCandidateMtimes(l.workDir, tc.to) // what the file-state check would see
			if got, want := a.Assemble(), fresh(tc.to); got != want {
				t.Errorf("changing %s with the file state unchanged served the old scope:\n got  %q\n want %q", tc.name, got, want)
			}
		})
	}
}

// The case no mtime check can see: the same files exist under both scopes,
// only their labels differ (HOME is an ancestor, so ~/.claude/CLAUDE.md is the
// user's under one scope and an ancestor's under the other). Only the key
// tells these apart.
func TestAssemblerCacheKeySeparatesScopesThatLoadTheSameFiles(t *testing.T) {
	l := newMemoryLayout(t)
	asUser := withLayout(MemoryOptions{WalkUp: true, Root: l.root})
	asAncestor := asUser
	asAncestor.SkipUser = true

	// Same candidate files, so the discovery-candidate mtimes are identical.
	a, b := MemoryCandidateMtimes(l.workDir, asUser), MemoryCandidateMtimes(l.workDir, asAncestor)
	if !mtimesEqual(a, b) {
		t.Fatalf("the scenario needs identical candidates, got:\n%v\n%v", a, b)
	}

	asm := NewAssemblerWithOptions(l.workDir, AssembleOptions{ProjectInstructions: true, Memory: asUser})
	if got := asm.Assemble(); !strings.Contains(got, "## User global (~/.claude/CLAUDE.md)") {
		t.Fatalf("user scope should label the file as the user's:\n%s", got)
	}
	asm.opts.Memory = asAncestor
	got := asm.Assemble()
	if strings.Contains(got, "## User global") {
		t.Errorf("served the user-scope label under SkipUser:\n%s", got)
	}
	if want := "## Ancestor (" + filepath.Join(l.home, ".claude", "CLAUDE.md") + ")"; !strings.Contains(got, want) {
		t.Errorf("missing %q under SkipUser:\n%s", want, got)
	}
}

// A rule file is a candidate: adding, editing and un-conditioning one is seen
// without a new assembler.
func TestAssemblerCacheSeesRuleChanges(t *testing.T) {
	l := newMemoryLayout(t)
	_, _, all := scopeShapes(l)
	a := NewAssemblerWithOptions(l.workDir, AssembleOptions{ProjectInstructions: true, Memory: withLayout(all)})

	if got := a.Assemble(); !strings.Contains(got, mkRule) || strings.Contains(got, mkCond) {
		t.Fatalf("initial build:\n%s", got)
	}

	added := filepath.Join(l.root, ".claude", "rules", "new.md")
	writeFile(t, added, "ADDED-RULE")
	if got := a.Assemble(); !strings.Contains(got, "ADDED-RULE") {
		t.Errorf("a new rule file was not picked up:\n%s", got)
	}

	writeFile(t, added, "EDITED-RULE")
	bumpMtime(t, added)
	if got := a.Assemble(); !strings.Contains(got, "EDITED-RULE") || strings.Contains(got, "ADDED-RULE") {
		t.Errorf("an edited rule file was not re-read:\n%s", got)
	}

	// Dropping `paths` turns a conditional rule into a loaded one.
	cond := filepath.Join(l.root, ".claude", "rules", "cond.md")
	writeFile(t, cond, "---\ndescription: now global\n---\n"+mkCond)
	bumpMtime(t, cond)
	if got := a.Assemble(); !strings.Contains(got, mkCond) {
		t.Errorf("a rule that lost its paths key stayed skipped:\n%s", got)
	}

	if err := os.Remove(added); err != nil {
		t.Fatal(err)
	}
	if got := a.Assemble(); strings.Contains(got, "EDITED-RULE") {
		t.Errorf("a deleted rule file is still served:\n%s", got)
	}
}
