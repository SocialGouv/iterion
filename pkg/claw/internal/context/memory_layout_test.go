package context

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// One distinct marker per fixture file. They are bracketed so that none is a
// substring of another and a plain strings.Index finds exactly one file.
const (
	mkUser      = "[[M-USER]]"
	mkUserRule  = "[[M-USER-RULE]]"
	mkOuter     = "[[M-OUTER]]"
	mkRoot      = "[[M-ROOT]]"
	mkRule      = "[[M-RULE]]"
	mkCond      = "[[M-COND]]"
	mkSub       = "[[M-SUB]]"
	mkOuterCfg  = "[[M-OUTER-CFG]]"
	mkOuterRule = "[[M-OUTER-RULE]]"
	mkRootCfg   = "[[M-ROOT-CFG]]"
	mkSubCfg    = "[[M-SUB-CFG]]"
	mkSubRule   = "[[M-SUB-RULE]]"
)

var allMarkers = []string{
	mkUser, mkUserRule, mkOuter, mkRoot, mkRule, mkCond, mkSub,
	mkOuterCfg, mkOuterRule, mkRootCfg, mkSubCfg, mkSubRule,
}

// memoryLayout is the on-disk fixture shared by the memory tests. Everything
// lives under tmp (a t.TempDir()), and HOME points at tmp/home so no real
// user file can leak in:
//
//	tmp/home/.claude/CLAUDE.md                    mkUser      (user scope)
//	tmp/home/.claude/rules/u.md                   mkUserRule  (user rule)
//	tmp/home/lab/CLAUDE.md                        mkOuter     (outer ancestor)
//	tmp/home/lab/repo/CLAUDE.md                   mkRoot      (the root)
//	tmp/home/lab/repo/.claude/rules/r.md          mkRule
//	tmp/home/lab/repo/.claude/rules/cond.md       mkCond      (paths: frontmatter)
//	tmp/home/lab/repo/sub/CLAUDE.md               mkSub       (workDir)
type memoryLayout struct {
	tmp     string
	home    string
	outer   string // tmp/home/lab, an ancestor strictly above the root
	root    string // tmp/home/lab/repo, the workspace boundary
	workDir string // tmp/home/lab/repo/sub
}

func newMemoryLayout(t *testing.T) memoryLayout {
	t.Helper()
	tmp := t.TempDir()
	l := memoryLayout{tmp: tmp, home: filepath.Join(tmp, "home")}
	l.outer = filepath.Join(l.home, "lab")
	l.root = filepath.Join(l.outer, "repo")
	l.workDir = filepath.Join(l.root, "sub")
	t.Setenv("HOME", l.home)

	writeFile(t, filepath.Join(l.home, ".claude", "CLAUDE.md"), mkUser)
	writeFile(t, filepath.Join(l.home, ".claude", "rules", "u.md"), mkUserRule)
	writeFile(t, filepath.Join(l.outer, "CLAUDE.md"), mkOuter)
	writeFile(t, filepath.Join(l.root, "CLAUDE.md"), mkRoot)
	writeFile(t, filepath.Join(l.root, ".claude", "rules", "r.md"), mkRule)
	writeFile(t, filepath.Join(l.root, ".claude", "rules", "cond.md"),
		"---\npaths:\n  - \"src/**/*.go\"\n---\n"+mkCond)
	writeFile(t, filepath.Join(l.workDir, "CLAUDE.md"), mkSub)
	return l
}

// withExtras adds the files that exercise `.claude/CLAUDE.md` and rules at
// every directory level:
//
//	tmp/home/lab/.claude/CLAUDE.md                mkOuterCfg
//	tmp/home/lab/.claude/rules/or.md              mkOuterRule (with frontmatter)
//	tmp/home/lab/repo/.claude/CLAUDE.md           mkRootCfg
//	tmp/home/lab/repo/sub/.claude/CLAUDE.md       mkSubCfg
//	tmp/home/lab/repo/sub/.claude/rules/sr.md     mkSubRule   (with frontmatter)
func (l memoryLayout) withExtras(t *testing.T) memoryLayout {
	t.Helper()
	writeFile(t, filepath.Join(l.outer, ".claude", "CLAUDE.md"), mkOuterCfg)
	writeFile(t, filepath.Join(l.outer, ".claude", "rules", "or.md"),
		"---\ndescription: ancestor rule frontmatter\n---\n"+mkOuterRule)
	writeFile(t, filepath.Join(l.root, ".claude", "CLAUDE.md"), mkRootCfg)
	writeFile(t, filepath.Join(l.workDir, ".claude", "CLAUDE.md"), mkSubCfg)
	writeFile(t, filepath.Join(l.workDir, ".claude", "rules", "sr.md"),
		"---\ndescription: project rule frontmatter\n---\n"+mkSubRule)
	return l
}

// markersIn returns every fixture marker found in out, in order of appearance,
// once per occurrence — so a file loaded twice shows up twice.
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
	got := markersIn(out)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("markers loaded:\n got  %v\n want %v\noutput:\n%s", got, want, out)
	}
}

// sectionSeparator joins the per-file sections LoadMemory emits.
const sectionSeparator = "\n\n---\n\n"

// fixtureSections splits out into its per-file sections, minus the
// "Ancestor (<path>)" sections whose path lies outside dir: the real
// ancestors of the machine's temp directory are none of the fixture's
// business, and a stray /tmp/CLAUDE.md must not redden a pinned output.
func fixtureSections(out, dir string) []string {
	if out == "" {
		return nil
	}
	var got []string
	for _, section := range strings.Split(out, sectionSeparator) {
		if rest, ok := strings.CutPrefix(section, "## Ancestor ("); ok {
			if path, _, _ := strings.Cut(rest, ")\n"); !strings.HasPrefix(path, dir+string(filepath.Separator)) {
				continue
			}
		}
		got = append(got, section)
	}
	return got
}

// assertSections pins the exact sections, headings and bodies, in order.
func assertSections(t *testing.T, out, dir string, want ...string) {
	t.Helper()
	got := fixtureSections(out, dir)
	if strings.Join(got, sectionSeparator) != strings.Join(want, sectionSeparator) {
		t.Errorf("sections:\n got  %q\n want %q", got, want)
	}
}
