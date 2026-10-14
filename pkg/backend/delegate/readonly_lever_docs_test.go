package delegate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// readonlyLeverSources maps each backend a `.bot` can name onto the GLOB of
// the files that would honour `readonly:` for it. The table is the question
// "who reads the flag?" asked of source rather than of memory: a backend
// wired for it answers in its own files, and nothing else in the tree has to
// be kept in step by hand.
//
// A glob and not one file per backend: every one of these delegates is spread
// over siblings (`claw_ambient.go`, `codex_web_search.go`, `pi_rpc.go`,
// `cliagent_ambient.go`), so a lever wired in a sibling — the likeliest shape
// for the follow-up that wires `claw` — would leave a single-file canary
// green while the four enumerations below go stale.
var readonlyLeverSources = map[string]string{
	"claude_code": "claude_code*.go",
	"codex":       "codex*.go",
	"pi":          "pi*.go",
	"claw":        filepath.Join("..", "model", "claw*.go"),
	"opencode":    "opencode*.go",
	"cliagent":    "cliagent*.go",
}

// backendsReadingReadonly splits readonlyLeverSources into the backends whose
// delegate reads `Task.Readonly` and the ones that never look at it.
//
// Read from the AST, not from a grep: a mention inside a comment or a string
// is what a doc page is being checked against here, so counting one as a
// reader would make this guard agree with a stale page.
//
// `_test.go` files are not parsed: the question is what the delegate DOES, and
// a test naming the field proves nothing about the argv a run gets.
func backendsReadingReadonly(t *testing.T) (reads, ignores []string) {
	t.Helper()
	for backend, pattern := range readonlyLeverSources {
		paths, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("%s (%s): %v", backend, pattern, err)
		}
		found := false
		parsed := 0
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("%s (%s): %v — this guard's table names the files each backend would honour the flag in, so a rename is a finding, not a skip", backend, path, err)
			}
			parsed++
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && sel.Sel != nil && sel.Sel.Name == "Readonly" {
					found = true
				}
				return !found
			})
			if found {
				break
			}
		}
		if parsed == 0 {
			t.Fatalf("%s (%s): no source file matched — a backend whose files moved is a finding, not a skip: this guard would otherwise report it as ignoring the flag", backend, pattern)
		}
		if found {
			reads = append(reads, backend)
		} else {
			ignores = append(ignores, backend)
		}
	}
	sort.Strings(reads)
	sort.Strings(ignores)
	return reads, ignores
}

// TestTheReadonlyLeverIsHonouredByThreeBackends pins the fact every statement
// of the flag depends on. `readonly:` is honoured with the lever each delegate
// has, and three have none — so a page that describes the flag as a property
// of the NODE rather than of the pair (node, backend) promises a bound that
// does not exist there.
//
// Load-bearing as a canary: wiring `claw` (or typed-refusing it) is the
// follow-up this change leaves open, and the day it lands, the four statements
// below have to move in the same commit.
func TestTheReadonlyLeverIsHonouredByThreeBackends(t *testing.T) {
	reads, ignores := backendsReadingReadonly(t)
	if strings.Join(reads, ",") != "claude_code,codex,pi" {
		t.Errorf("the backends reading `readonly:` are now %v (ignoring it: %v), not [claude_code codex pi] — four statements enumerate them and are now wrong: docs/dsl.md, docs/backends.md, docs/delegation.md and the Task.Readonly godoc. The non-mutating admission in pkg/runtime/workspace_safety.go is the flag's OTHER meaning and is backend-independent: it does not move with this list",
			reads, ignores)
	}
}

// TestEveryStatementOfTheReadonlyLeverNamesTheBackendsThatIgnoreIt is the
// class contract of those four statements: each names EVERY backend that does
// not read the flag, in the sentence that says so.
//
// The class was corrected twice from a one-off grep, and each pass left a
// sibling behind — the last one a prescriptive recipe whose example node is
// `backend: "claw"` + `readonly: true`, i.e. the page most likely to be copied
// stating the strongest form of the refuted claim. The names come from the AST
// above, so dropping a backend from a page cannot be hidden by also dropping
// it from this table.
//
// What it cannot do: judge the rest of the sentence. It proves the
// enumeration is complete and sits beside the statement, not that the prose
// around it is well written.
func TestEveryStatementOfTheReadonlyLeverNamesTheBackendsThatIgnoreIt(t *testing.T) {
	_, ignores := backendsReadingReadonly(t)
	if len(ignores) == 0 {
		t.Fatal("every backend now reads `readonly:`, so this guard has nothing to check: fold it into the one above")
	}
	// A site states the lever and owns the enumeration. `required` is the
	// clause the names must sit in front of — the godoc says "field" where a
	// page says "flag", and matching one phrase everywhere would force a
	// wording on four surfaces to satisfy a test.
	for _, site := range []struct{ path, required string }{
		{filepath.Join("..", "..", "..", "docs", "dsl.md"), "do not read the flag at all"},
		{filepath.Join("..", "..", "..", "docs", "backends.md"), "do not read the flag at all"},
		{filepath.Join("..", "..", "..", "docs", "delegation.md"), "do not read the flag at all"},
		{"delegate.go", "do not read the field at all"},
	} {
		raw, err := os.ReadFile(site.path)
		if err != nil {
			t.Fatalf("%s: %v", site.path, err)
		}
		// Collapsed to one line: every one of these statements wraps, and a
		// per-line probe would miss a name sitting one wrap away.
		prose := strings.Join(strings.Fields(string(raw)), " ")
		at := strings.Index(prose, site.required)
		if at < 0 {
			t.Errorf("%s no longer states which backends ignore `readonly:` (%q is gone): a statement of the flag that does not name them describes it as a property of the node, and on %v there is no bound at all",
				site.path, site.required, ignores)
			continue
		}
		// The names must introduce the clause, not merely appear on the page:
		// every page here mentions `claw` many times for other reasons.
		window := prose[max(0, at-200):at]
		for _, backend := range ignores {
			if !strings.Contains(window, backend) {
				t.Errorf("%s says %q but does not name %q in front of it (%q): the enumeration is incomplete, which is how three of these statements shipped naming one backend of %v",
					site.path, site.required, backend, window, ignores)
			}
		}
	}
}

// refutedLeverClaims are the sentences #1816 made false. `readonly:` IS read
// on claude_code — it withholds the file-writing tools a node did not declare
// plus the worktree-moving pair — so a passage saying this backend ignores the
// flag, or naming codex and pi as the only backends that honour it, now tells
// an author the opposite of what the delegate does. An author who reads one
// declares `tools:` to buy a bound the flag already gives, or believes a
// readonly claude_code node is bounded by nothing but its prompt.
//
// Needles rather than a predicate, and the reason is measured: the claim has
// no single shape ("nothing reads it", "never reads it", "by pi and codex
// only", "NOT a tool gate: only codex … and pi …"), and a window predicate
// ("names codex and pi, not claude_code, near the flag") answered with two
// false alarms on paragraphs that are correct, while missing the three
// passages that DO name claude_code to say it reads nothing. Each needle was
// measured against the whole tree: inside the scope below, one hit — the
// sentence it is named for; outside it, only the two engine comments the scope
// note names. A new wording escapes the list — the FACT is
// TestTheReadonlyLeverIsHonouredByThreeBackends above, which reads the source;
// this guard only keeps the sentences that shipped from coming back.
var refutedLeverClaims = []string{
	"claude_code never reads",
	"claude_code nothing reads",
	"claude_code nothing does",
	"claude_code does not read",
	"claude_code doesn't read",
	"claude_code ignores readonly",
	"codex and pi delegates only",
	"by pi and codex only",
	"not a tool gate: only",
	// The wordings the first three sweeps left behind, each on a `##`
	// comment sitting ON a `readonly: true` + claude_code node: the flag as
	// codex/pi's alone, and its opposite — a node that "cannot mutate" on an
	// undeclared surface that keeps Bash.
	"enforced on codex/pi only",
	"readonly so it cannot mutate",
	"cannot mutate the workspace",
}

// leverProseScope is where author-facing text about the flag lives: the
// catalog bots (prompts, `##` node comments, READMEs, skills, the bundle
// tests' own comments), the docs tree, and the scaffold templates every new
// bot is born from.
//
// Out of scope, deliberately: docs/adr and docs/bot-runs record the state at
// their date, and a superseding note is their mechanism, not an edit. Two
// engine comments outside this scope state the same refuted sentence
// (pkg/runtime/workspace_safety.go, pkg/runtime/tool_surface_test.go); neither
// is an assertion — the workspace-safety read of the flag is backend-blind on
// purpose — so they cost a reader, not a run.
//
// Each root carries its OWN floor, because the walk's total cannot witness
// the loss of a root: `bots` alone is 459 files today, so an aggregate floor
// stayed green over a scope that had dropped `docs` and the scaffold
// templates — the two roots the class kept slipping through.
var leverProseScope = []struct {
	root string
	min  int
}{
	{"bots", 400},
	{"docs", 150},
	{filepath.Join("pkg", "botscaffold", "templates"), 15},
}

// TestNoAuthorFacingPageSaysClaudeCodeIgnoresTheReadonlyFlag is the class
// contract of the refuted statement. The class was corrected three times from
// a one-off grep — docs first, then the skills, then the prompts — and each
// pass left siblings behind: a bot README, a `##` comment on the very node the
// flag now bounds, and the scaffold template every new review bot copies.
func TestNoAuthorFacingPageSaysClaudeCodeIgnoresTheReadonlyFlag(t *testing.T) {
	reads, _ := backendsReadingReadonly(t)
	if !slices.Contains(reads, "claude_code") {
		t.Fatal("claude_code no longer reads `readonly:`, so these sentences would be true again: retire this guard in the same change")
	}
	var offenders []string
	var roots []string
	for _, top := range leverProseScope {
		roots = append(roots, top.root)
		files := 0
		root := filepath.Join("..", "..", "..", top.root)
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if base := filepath.Base(path); base == "adr" || base == "bot-runs" || base == "changelog" {
					return fs.SkipDir
				}
				return nil
			}
			switch filepath.Ext(path) {
			case ".md", ".bot", ".tmpl", ".go":
			default:
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files++
			// Collapsed the way a reader reads it: the claim wraps across
			// lines, and `##`, backticks and bold markers sit between its
			// words.
			prose := strings.ToLower(string(raw))
			for _, strip := range []string{"`", "*", "#"} {
				prose = strings.ReplaceAll(prose, strip, " ")
			}
			prose = strings.Join(strings.Fields(prose), " ")
			for _, claim := range refutedLeverClaims {
				if strings.Contains(prose, claim) {
					offenders = append(offenders, path+": "+claim)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", root, err)
		}
		// A FLOOR per root: this guard proves nothing over a root it stopped
		// reading, and the roots differ by an order of magnitude, so one
		// total cannot witness the loss of the smallest.
		if files < top.min {
			t.Fatalf("the walk read %d file(s) of %s, want at least %d: that root broke, and this guard is now green over an unexamined tree", files, top.root, top.min)
		}
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d author-facing passage(s) say claude_code does not read `readonly:`, which it does (%v read it): state what the flag removes there — the file-writing tools a node did not declare, plus the worktree-moving pair — or drop the sentence:\n  %s",
			len(offenders), reads, strings.Join(offenders, "\n  "))
	}
	// The roots themselves, pinned: a per-root floor witnesses a root that
	// SHRANK, never one that was dropped — the remaining roots clear their own
	// floors, which is how a scope of `bots` alone stayed green.
	for _, want := range []string{"bots", "docs", filepath.Join("pkg", "botscaffold", "templates")} {
		if !slices.Contains(roots, want) {
			t.Fatalf("the scope walked %v and %q is not among them: author-facing text about the flag lives in all three — the catalog bots, the docs tree, and the scaffold templates every new bot is born from — and the last two are the roots this class kept slipping through", roots, want)
		}
	}
}
