package context

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// These tests pin the memory loader's behaviour when no scope option is set.
// They were written against the loader as it stood before scoped memory
// existed and must stay green unchanged: a host that never sets a scope field
// gets today's output, byte for byte.

func TestMemoryUnscopedZeroOptionsIsPinned(t *testing.T) {
	l := newMemoryLayout(t)

	got, _ := LoadMemory(l.workDir, MemoryOptions{})

	// The user file, then the workDir file: no ancestors (WalkUp off), no
	// rules, and nothing the layout holds above the workDir.
	want := "## User global (~/.claude/CLAUDE.md)\n\n" + mkUser +
		sectionSeparator +
		"## Project root (CLAUDE.md)\n\n" + mkSub
	if got != want {
		t.Errorf("zero options changed:\n got  %q\n want %q", got, want)
	}
}

func TestMemoryUnscopedWalkUpIsPinned(t *testing.T) {
	l := newMemoryLayout(t)

	outer := "## Ancestor (" + filepath.Join(l.outer, "CLAUDE.md") + ")\n\n" + mkOuter
	root := "## Ancestor (" + filepath.Join(l.root, "CLAUDE.md") + ")\n\n" + mkRoot
	want := []string{
		"## User global (~/.claude/CLAUDE.md)\n\n" + mkUser,
		outer,
		root,
		"## Project root (CLAUDE.md)\n\n" + mkSub,
	}

	for _, tc := range []struct {
		name string
		opts MemoryOptions
	}{
		{"WalkUp", MemoryOptions{WalkUp: true}},
		{"WalkUp+Imports", MemoryOptions{WalkUp: true, Imports: true}},
		{"DefaultMemoryOptions", DefaultMemoryOptions()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := LoadMemory(l.workDir, tc.opts)
			assertSections(t, got, l.tmp, want...)
		})
	}
}

// Ancestors contribute their CLAUDE.md only; a rules directory is never read;
// the workDir's own .claude/CLAUDE.md has always been loaded.
func TestMemoryUnscopedIgnoresRulesAndAncestorDotClaude(t *testing.T) {
	l := newMemoryLayout(t).withExtras(t)

	got, _ := LoadMemory(l.workDir, MemoryOptions{WalkUp: true, Imports: true})

	assertMarkers(t, got, mkUser, mkOuter, mkRoot, mkSub, mkSubCfg)
	assertSections(t, got, l.tmp,
		"## User global (~/.claude/CLAUDE.md)\n\n"+mkUser,
		"## Ancestor ("+filepath.Join(l.outer, "CLAUDE.md")+")\n\n"+mkOuter,
		"## Ancestor ("+filepath.Join(l.root, "CLAUDE.md")+")\n\n"+mkRoot,
		"## Project root (CLAUDE.md)\n\n"+mkSub,
		"## Project config (.claude/CLAUDE.md)\n\n"+mkSubCfg,
	)
}

func TestMemoryUnscopedCandidateMtimesArePinned(t *testing.T) {
	l := newMemoryLayout(t).withExtras(t)

	got := MemoryCandidateMtimes(l.workDir, MemoryOptions{WalkUp: true})

	var paths []string
	for p := range got {
		// Real ancestors of the temp directory are not the fixture's.
		if strings.HasPrefix(p, l.tmp+string(filepath.Separator)) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	want := []string{
		filepath.Join(l.home, ".claude", "CLAUDE.md"),
		filepath.Join(l.outer, "CLAUDE.md"),
		filepath.Join(l.root, "CLAUDE.md"),
		filepath.Join(l.workDir, ".claude", "CLAUDE.md"),
		filepath.Join(l.workDir, "CLAUDE.md"),
	}
	sort.Strings(want)
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Errorf("candidate files changed:\n got  %q\n want %q", paths, want)
	}
}

// No home directory, no user scope: an unset HOME neither fails nor invents one.
func TestMemoryUnscopedWithoutAHomeDirectory(t *testing.T) {
	l := newMemoryLayout(t)
	t.Setenv("HOME", "")

	got, _ := LoadMemory(l.workDir, MemoryOptions{WalkUp: true})

	assertSections(t, got, l.tmp,
		"## Ancestor ("+filepath.Join(l.outer, "CLAUDE.md")+")\n\n"+mkOuter,
		"## Ancestor ("+filepath.Join(l.root, "CLAUDE.md")+")\n\n"+mkRoot,
		"## Project root (CLAUDE.md)\n\n"+mkSub,
	)
}

func TestMemoryUnscopedEmptyWhenNothingExists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()

	got, mtimes := LoadMemory(workDir, MemoryOptions{Imports: true})
	if got != "" {
		t.Errorf("no memory file anywhere must load as the empty string, got %q", got)
	}
	for p := range mtimes {
		if strings.HasPrefix(p, workDir) {
			t.Errorf("mtime recorded for a file that does not exist: %s", p)
		}
	}
}
