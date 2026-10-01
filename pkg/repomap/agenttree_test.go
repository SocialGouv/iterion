package repomap_test

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/docsguard"
	"github.com/SocialGouv/iterion/internal/mdcode"
)

// The agent instruction tree is paid on every turn of every session:
// AGENTS.md is the one file every harness loads (Codex and pi natively,
// Claude Code through CLAUDE.md's import), and pi re-injects both roots on
// every call. "The root stays short" held only while someone watched it, so
// the budgets below make it a gate.
//
// Raising a budget is an edit of this table, read in review: that is the
// escape hatch, and it is meant to be greppable. The first matching pattern
// wins, so an index rule sits above any broader rule for the same directory.
var agentTreeBudgets = []agentTreeBudget{
	{"AGENTS.md", 6144},
	{"CLAUDE.md", 1536},
	{"docs/agents/**/README.md", 2560},
}

type agentTreeBudget struct {
	pattern string
	max     int
}

// The import line Claude Code needs: it reads AGENTS.md natively only when
// no CLAUDE.md exists, so the root reaches a Claude session through this
// line or not at all.
const agentsImport = "@AGENTS.md"

// budgetMatches reads a pattern as either a path.Match pattern or
// "<dir>/**/<base glob>", which matches the glob at any depth under dir.
func budgetMatches(pattern, rel string) bool {
	if dir, base, ok := strings.Cut(pattern, "/**/"); ok {
		m, _ := path.Match(base, path.Base(rel))
		return strings.HasPrefix(rel, dir+"/") && m
	}
	m, _ := path.Match(pattern, rel)
	return m
}

// firstBudget is the budget that governs rel: the first pattern matching it.
func firstBudget(budgets []agentTreeBudget, rel string) (agentTreeBudget, bool) {
	for _, b := range budgets {
		if budgetMatches(b.pattern, rel) {
			return b, true
		}
	}
	return agentTreeBudget{}, false
}

// agentTreeFiles lists the two roots and every page under docs/agents,
// slash-separated and relative to root. Symlinks are skipped here — the
// budgets and the index rule must judge real files — and refused on their
// own by TestTheAgentTreeHasNoSymlinks: a link into or out of the tree would
// make "indexed" mean something no reader agrees with.
func agentTreeFiles(t *testing.T, root string) []string {
	t.Helper()
	files := []string{"AGENTS.md", "CLAUDE.md"}
	err := filepath.WalkDir(filepath.Join(root, "docs", "agents"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func symlinkViolations(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(filepath.Join(root, "docs", "agents"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, fmt.Sprintf("%s is a symlink: the agent tree is read without following links, make it a real file or directory", filepath.ToSlash(rel)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// pageLinks returns the repository pages one markdown file links to.
// Inherited blind spot, shared with the repository's link checker:
// docsguard reports an UNUSED reference definition as a link, so a page
// indexed only by one still counts as indexed.
// slash-separated and relative to root. It reads links the way the
// repository's link checker does (internal/docsguard: code, HTML comments
// and front matter skipped; inline, reference and HTML links read), and a
// link to a directory means that directory's README.md, as GitHub renders it.
func pageLinks(t *testing.T, root, rel string) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, l := range docsguard.Parse(rel, body).Links {
		target := strings.TrimSpace(l.Target)
		if target == "" || docsguard.IsExternal(target) {
			continue
		}
		p, _, _ := strings.Cut(target, "#")
		p, _, _ = strings.Cut(p, "?")
		if decoded, err := url.PathUnescape(p); err == nil {
			p = decoded
		}
		if p == "" || strings.HasPrefix(p, "/") {
			continue
		}
		resolved := path.Clean(path.Join(path.Dir(rel), p))
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(resolved))); strings.HasSuffix(p, "/") || (err == nil && info.IsDir()) {
			resolved = path.Join(resolved, "README.md")
		}
		out[resolved] = true
	}
	return out
}

func budgetViolations(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, rel := range agentTreeFiles(t, root) {
		b, ok := firstBudget(agentTreeBudgets, rel)
		if !ok {
			continue
		}
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > int64(b.max) {
			out = append(out, fmt.Sprintf("%s is %d bytes, over its %d-byte budget (%s): move the content into a page of the tree and leave one line behind — raising the budget is an edit of agentTreeBudgets, made in review",
				rel, info.Size(), b.max, b.pattern))
		}
	}
	return out
}

// htmlCommentRe matches an HTML comment, which Claude Code strips before it
// injects a CLAUDE.md: an import written inside one is never expanded.
var htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?(?:-->|\z)`)

func importViolations(t *testing.T, root string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if errors.Is(err, fs.ErrNotExist) {
		return []string{"CLAUDE.md is missing: Claude Code would then read AGENTS.md natively, but the brand check and the Claude-only notes need the file — restore it with its `" + agentsImport + "` line"}
	}
	if err != nil {
		t.Fatal(err)
	}
	// Fences win over comments, as internal/docsguard and CommonMark read a
	// page: masking the code first keeps an unterminated `<!--` inside a
	// fenced example from swallowing a real import written after the fence.
	uncommented := htmlCommentRe.ReplaceAllStringFunc(mdcode.MaskDocument(string(body)), func(c string) string {
		return strings.Map(func(r rune) rune {
			if r == '\n' {
				return r
			}
			return ' '
		}, c)
	})
	for _, line := range strings.Split(mdcode.MaskDocument(uncommented), "\n") {
		if strings.TrimSpace(line) == agentsImport {
			return nil
		}
	}
	return []string{"CLAUDE.md has no bare `" + agentsImport + "` line outside code and comments: Claude Code reads AGENTS.md only when no CLAUDE.md exists, so every Claude session would lose the root"}
}

// indexOf names the file that must index rel: a page is indexed by the
// README.md of its own directory, a directory's README.md by its parent's,
// and the tree's top README.md by AGENTS.md.
func indexOf(rel string) string {
	switch {
	case rel == "docs/agents/README.md":
		return "AGENTS.md"
	case path.Base(rel) == "README.md":
		return path.Join(path.Dir(path.Dir(rel)), "README.md")
	default:
		return path.Join(path.Dir(rel), "README.md")
	}
}

// unindexedPages returns every docs/agents page its index does not link.
// Reachable is not enough: a page found only through one sentence of
// another page is not on the index an agent reads to choose what to open.
func unindexedPages(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	links := map[string]map[string]bool{}
	for _, rel := range agentTreeFiles(t, root) {
		if !strings.HasPrefix(rel, "docs/agents/") {
			continue
		}
		index := indexOf(rel)
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(index))); err != nil {
			out = append(out, fmt.Sprintf("%s has no index: create %s and give it one line for the page", rel, index))
			continue
		}
		if links[index] == nil {
			links[index] = pageLinks(t, root, index)
		}
		if !links[index][rel] {
			out = append(out, fmt.Sprintf("%s is not indexed: %s must link it, one line saying when to read it", rel, index))
		}
	}
	sort.Strings(out)
	return out
}

// shadowedBudgets returns every budget that is the first match of no file:
// a pattern that matches nothing, or only files an earlier pattern already
// governs, guards nothing — and looks exactly like one that works.
func shadowedBudgets(budgets []agentTreeBudget, files []string) []string {
	governs := map[string]bool{}
	for _, rel := range files {
		if b, ok := firstBudget(budgets, rel); ok {
			governs[b.pattern] = true
		}
	}
	var out []string
	for _, b := range budgets {
		if !governs[b.pattern] {
			out = append(out, fmt.Sprintf("budget %q governs no file of the agent tree: no file matches it first", b.pattern))
		}
	}
	return out
}

func TestTheAgentRootsAndIndexesHoldTheirByteBudgets(t *testing.T) {
	for _, msg := range budgetViolations(t, repoRoot) {
		t.Error(msg)
	}
}

func TestClaudeMdImportsTheAgentsRoot(t *testing.T) {
	for _, msg := range importViolations(t, repoRoot) {
		t.Error(msg)
	}
}

func TestEveryAgentTreePageIsIndexedByItsDirectoryReadme(t *testing.T) {
	for _, msg := range unindexedPages(t, repoRoot) {
		t.Error(msg)
	}
}

func TestTheAgentTreeHasNoSymlinks(t *testing.T) {
	for _, msg := range symlinkViolations(t, repoRoot) {
		t.Error(msg)
	}
}

func TestEveryAgentTreeBudgetIsTheFirstMatchOfSomeFile(t *testing.T) {
	for _, msg := range shadowedBudgets(agentTreeBudgets, agentTreeFiles(t, repoRoot)) {
		t.Error(msg)
	}
}

// A guard that cannot redden is not a guard. Each case breaks a healthy
// miniature tree in one way and must be reported for exactly that reason;
// the healthy tree — written with every link form the tree's reader must
// accept — must report nothing, or the gate would redden a correct change.
func TestTheAgentTreeGuardBites(t *testing.T) {
	healthy := map[string]string{
		"AGENTS.md":                   "# Root\n\n[the tree](docs/agents/README.md)\n",
		"CLAUDE.md":                   "# Claude\n\n" + agentsImport + "\n",
		"docs/agents/README.md":       "# Tree\n\n| [a](a.md#top) | [sub](sub/) | [sub2](sub2) | [b][b-ref] |\n\n[b-ref]: ./b.md\n",
		"docs/agents/a.md":            "# A\n\nBack to [the tree](README.md).\n",
		"docs/agents/b.md":            "# B\n",
		"docs/agents/sub/README.md":   "# Sub\n\n- <a href=\"leaf%20one.md\">leaf</a>\n",
		"docs/agents/sub/leaf one.md": "# Leaf\n",
		"docs/agents/sub2/README.md":  "# Sub two\n\n- [leaf](two.md)\n",
		"docs/agents/sub2/two.md":     "# Two\n",
	}
	tree := func(t *testing.T, change map[string]string, drop ...string) string {
		t.Helper()
		root := t.TempDir()
		for rel, body := range healthy {
			if b, ok := change[rel]; ok {
				body = b
			}
			if !contains(drop, rel) {
				writeTreeFile(t, root, rel, body)
			}
		}
		for rel, body := range change {
			if _, ok := healthy[rel]; !ok {
				writeTreeFile(t, root, rel, body)
			}
		}
		return root
	}

	root := tree(t, nil)
	for _, check := range []func(*testing.T, string) []string{budgetViolations, importViolations, unindexedPages} {
		if v := check(t, root); len(v) > 0 {
			t.Fatalf("the healthy tree is reported: %v", v)
		}
	}

	for _, tc := range []struct {
		name   string
		change map[string]string
		drop   []string
		check  func(*testing.T, string) []string
		want   string
	}{
		{"a page nothing links", map[string]string{"docs/agents/orphan.md": "# Orphan\n"}, nil, unindexedPages, "docs/agents/orphan.md is not indexed"},
		// The root links the page directly and a sibling links it too: it is
		// reachable twice over, and still missing from the index.
		{"a page reachable but not on its index", map[string]string{
			"AGENTS.md":        "# Root\n\n[the tree](docs/agents/README.md), [c](docs/agents/c.md)\n",
			"docs/agents/a.md": "# A\n\nSee [c](c.md).\n",
			"docs/agents/c.md": "# C\n",
		}, nil, unindexedPages, "docs/agents/c.md is not indexed"},
		{"an index line written inside code", map[string]string{"docs/agents/README.md": "# Tree\n\n| [a](a.md) | [sub](sub/) | [b](b.md) | `[c](c.md)` |\n", "docs/agents/c.md": "# C\n"}, nil, unindexedPages, "docs/agents/c.md is not indexed"},
		{"an index line commented out", map[string]string{"docs/agents/README.md": "# Tree\n\n| [a](a.md) | [sub](sub/) | [b](b.md) |\n<!-- [c](c.md) -->\n", "docs/agents/c.md": "# C\n"}, nil, unindexedPages, "docs/agents/c.md is not indexed"},
		{"a sub-tree its parent index forgets", map[string]string{"docs/agents/README.md": "# Tree\n\n| [a](a.md) | [b](b.md) |\n"}, nil, unindexedPages, "docs/agents/sub/README.md is not indexed"},
		{"a sub-tree with no index of its own", nil, []string{"docs/agents/sub/README.md"}, unindexedPages, "docs/agents/sub/leaf one.md has no index"},
		{"a root over its budget", map[string]string{"AGENTS.md": "# Root\n\n[the tree](docs/agents/README.md)\n" + strings.Repeat("x", 6144)}, nil, budgetViolations, "AGENTS.md is"},
		{"a nested index over its budget", map[string]string{"docs/agents/sub/README.md": "# Sub\n\n- [leaf](leaf%20one.md)\n" + strings.Repeat("x", 2560)}, nil, budgetViolations, "docs/agents/sub/README.md is"},
		{"CLAUDE.md without the import", map[string]string{"CLAUDE.md": "# Claude\n\nRead AGENTS.md.\n"}, nil, importViolations, "no bare"},
		{"the import fenced as code", map[string]string{"CLAUDE.md": "# Claude\n\n```\n" + agentsImport + "\n```\n"}, nil, importViolations, "no bare"},
		{"the import commented out", map[string]string{"CLAUDE.md": "# Claude\n\n<!--\n" + agentsImport + "\n-->\n"}, nil, importViolations, "no bare"},
		{"the import after an unterminated comment", map[string]string{"CLAUDE.md": "# Claude\n\n<!-- a note nobody closed\n\n" + agentsImport + "\n"}, nil, importViolations, "no bare"},
		{"CLAUDE.md missing", nil, []string{"CLAUDE.md"}, importViolations, "CLAUDE.md is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.check(t, tree(t, tc.change, tc.drop...))
			if !strings.Contains(strings.Join(got, "\n"), tc.want) {
				t.Fatalf("not reported: want a violation naming %q, got %v", tc.want, got)
			}
		})
	}

	t.Run("the import after a fenced example with an unterminated comment", func(t *testing.T) {
		root := tree(t, map[string]string{"CLAUDE.md": "# Claude\n\n```sh\n<!-- an example nobody closed\n```\n\n" + agentsImport + "\n"})
		if v := importViolations(t, root); len(v) > 0 {
			t.Fatalf("a fenced example must not swallow the import after it: %v", v)
		}
	})

	t.Run("a symlinked page", func(t *testing.T) {
		root := tree(t, nil)
		if err := os.Symlink(filepath.Join(root, "docs", "agents", "a.md"), filepath.Join(root, "docs", "agents", "linked.md")); err != nil {
			t.Fatal(err)
		}
		got := symlinkViolations(t, root)
		if !strings.Contains(strings.Join(got, "\n"), "docs/agents/linked.md is a symlink") {
			t.Fatalf("not reported: want a symlink violation, got %v", got)
		}
	})

	t.Run("a file exactly at its budget, then one byte over", func(t *testing.T) {
		at := tree(t, map[string]string{"CLAUDE.md": strings.Repeat("x", 1536)})
		if v := budgetViolations(t, at); len(v) > 0 {
			t.Fatalf("a file exactly at its budget is reported: %v", v)
		}
		over := tree(t, map[string]string{"CLAUDE.md": strings.Repeat("x", 1537)})
		if v := budgetViolations(t, over); !strings.Contains(strings.Join(v, "\n"), "CLAUDE.md is 1537 bytes") {
			t.Fatalf("not reported: one byte over the budget, got %v", v)
		}
	})

	t.Run("a budget shadowed by an earlier pattern", func(t *testing.T) {
		appended := append(append([]agentTreeBudget{}, agentTreeBudgets...), agentTreeBudget{"docs/agents/README.md", 8192})
		got := shadowedBudgets(appended, agentTreeFiles(t, tree(t, nil)))
		if !strings.Contains(strings.Join(got, "\n"), `"docs/agents/README.md" governs no file`) {
			t.Fatalf("not reported: an appended pattern that never matches first, got %v", got)
		}
	})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func writeTreeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
