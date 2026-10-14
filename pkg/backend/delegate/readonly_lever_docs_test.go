package delegate

import (
	"fmt"
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
//
// The sites are NAMED because each owns the clause in its own wording, and a
// reference page is the one surface that may not be silent about the three.
// Author-facing text that enumerates the levers anywhere else is swept by
// TestEveryLeverEnumerationNamesTheBackendsThatIgnoreIt, which walks the
// whole prose scope: that is where a bot README and a `##` node comment kept
// naming one backend of three.
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
		{filepath.Join("..", "..", "..", "bots", "copilot", "skills", "iterion-bot-architecture.md"), "do not read the flag at all"},
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

// refutedLeverClaims are the sentences the delegates make false — two
// families, measured and listed below: what #1816 refuted about claude_code,
// and what pi's lever has never been. `readonly:` IS read
// on claude_code — it withholds the file-writing tools plus the
// worktree-moving pair, whatever the node's `tools:` list says — so a passage
// saying this backend ignores the flag, or naming codex and pi as the only
// backends that honour it, or promising that a declared writer survives it,
// now tells
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
	// The wordings of the opposite error: the flag honoured, but beaten by a
	// `tools:` list that names a writer — each refuted by the argv
	// (TestAReadonlyDeclarationThatNamesAWriterLosesItAndIsToldSo). Measured
	// inside the scope below, on the tree that shipped them: two docs pages
	// said "survives the flag", four `##` prompts plus the bundle guard's own
	// comment said "write_file is kept", and that same bundle guard said
	// "readonly: true is not enforced" — a bundle test comment, not a page.
	// The wording that shipped in this package ("a declared writing tool is
	// not deleted here") is deliberately NOT a needle: pkg/backend/delegate
	// is outside leverProseScope, so a needle for it could never go red from
	// the tree this guard walks, and a needle that cannot go red is a comment.
	"survives the flag",
	"readonly: true is not enforced",
	"write_file is kept",
	// The same rule in the THIRD PERSON — the form that states the RULE,
	// which is the form that outlived three sweeps: a `##` comment on a
	// `readonly: true` node went on describing the withholding as bounded by
	// the list, one scroll from a sibling comment the same sweep rewrote. The
	// SECOND-person prompts ("the file-writing tools you did not declare")
	// stay and are not needles: each sits on a node that declares no writer,
	// so the set it names IS the set withheld, and the corpus guard in bots/
	// (TestNoShippedWorkflowDeclaresReadonlyWithAWritingTool) is what keeps
	// that true. Measured on the tree that shipped them: 9, 7, 4 and 1 hit
	// inside the scope, 0 after the sweep. A needle shortened to "list does
	// not name" was REFUSED on measurement: it also hits docs/permissions.md
	// and docs/browser-security.md, where the phrase is about an allowlist.
	"tools a node did not declare",
	"tools this node did not declare",
	"tools the node did not declare",
	"tools this list does not name",
	// The SECOND refuted family: pi's lever read as a sandbox, or as a bound
	// on the process. piExtraArgsFor's whole readonly branch is
	// `--tools read,grep,find,ls` — a four-name allowlist of readers, with no
	// sandbox argument anywhere in that function — while codex really does
	// run the node in one (codexSandboxForTask → "read-only"). Calling the
	// one other list-narrowing backend a sandbox denies, for pi, the teaching
	// claude_code's own pages rest on (a tool list is not a sandbox: a write
	// path spelled outside the list survives, #1651), and it is the sentence
	// an author follows when the notice tells them where to move a node that
	// must not write. Measured inside the scope on the tree that shipped
	// them: 1, 1, 1, 1, 3, 1, 7, 0, 0 and 1 hit; 0 after the sweep. The
	// slash form is where the class hid: a grep for "codex and pi" misses "a
	// sandbox on codex/pi", and that wording sat on six byte-identical
	// `## plan` comments plus a `copilot` guarantees table.
	//
	// The last three are ONE sentence, and the terminator is what makes it
	// the refuted one: the joint attribution is false only when the clause
	// ENDS at pi, so the needle carries the `.`, `,` or `;` that ends it —
	// the shipped wording ended in `;` (the 1 hit; the two others are the
	// same sentence re-punctuated, 0 on that tree). Measured as a bare
	// prefix it refused a TRUE page instead: "a read-only OS sandbox on
	// codex and pi's own `--tools read,grep,find,ls` allowlist" says what
	// each lever is and contains the prefix whole, and a guard that reddens
	// on the correction it asks for teaches authors to delete the sentence.
	// Both directions are pinned by
	// TestTheRefutedNeedlesRefuseTheFalseFormAndNotTheTrueOne.
	//
	// What pi's lever IS may be said freely — "an OS sandbox on codex and a
	// four-reader tool list on pi" is the shape of the correction — these
	// needles only keep the two backends from sharing the word "sandbox".
	"pi enforce it as a sandbox",
	"pi delegates enforce it as a sandbox",
	"sandbox on the codex and pi",
	"codex and pi make it a sandbox",
	"codex and pi lock",
	"pi enforce the flag against the whole process",
	"sandbox on codex/pi",
	"sandbox on codex and pi.",
	"sandbox on codex and pi,",
	"sandbox on codex and pi;",
}

// readonlyNoticePromise is the sentence that promises the operator is told
// which declaration the flag deleted. It is a promise only `claude_code`
// keeps: codex answers the flag with `--sandbox read-only`
// (codexSandboxForTask, which takes no logger) and pi with `--tools
// read,grep,find,ls` (piExtraArgsFor, which HAS a logger and does not call
// it) — each replaces the node's `tools:` declaration in silence.
const readonlyNoticePromise = "declaration is named on the node's log"

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

// collapseLeverProse reads a page the way a reader does: a claim about the
// flag wraps across lines, and `##`, backticks and bold markers sit between
// its words. Lowercased, so a needle is written in one case only.
//
// ONE function, read by the walk below and by the needle vocabulary test
// beside it: a test that collapsed its sentences its own way would measure a
// double of the guard, and a needle could then refuse a page the test calls
// clean.
func collapseLeverProse(raw string) string {
	prose := strings.ToLower(raw)
	for _, strip := range []string{"`", "*", "#"} {
		prose = strings.ReplaceAll(prose, strip, " ")
	}
	return strings.Join(strings.Fields(prose), " ")
}

// TestTheRefutedNeedlesRefuseTheFalseFormAndNotTheTrueOne pins BOTH
// directions of refutedLeverClaims on sentences, not on the tree: a needle
// that stopped matching the wording it is named for is a dead ratchet, and a
// needle wide enough to match a TRUE statement of the lever reddens the page
// an author is being sent to write. Both are failures the walk below cannot
// report about itself — it only ever sees the corrected tree.
//
// The sentences are collapsed with collapseLeverProse, the walk's own reader,
// so a match here is a match there.
func TestTheRefutedNeedlesRefuseTheFalseFormAndNotTheTrueOne(t *testing.T) {
	// The wordings that SHIPPED and the delegates refute, verbatim from the
	// tree that carried them (`git show 99d8fe689`): each must still be
	// caught, whichever needle catches it.
	for _, shipped := range []string{
		"`readonly: true` below is a sandbox on codex and pi; on claude_code it withholds the file-writing tools whatever this list says and leaves a shell",
		"`readonly` is a sandbox on codex/pi; on claude_code (bypassPermissions) it withholds the file-writing tools and the worktree-moving pair",
		"`readonly: true` | a sandbox on codex/pi; the file-writing tools withheld on claude_code |",
	} {
		prose := collapseLeverProse(shipped)
		if !slices.ContainsFunc(refutedLeverClaims, func(claim string) bool { return strings.Contains(prose, claim) }) {
			t.Errorf("no needle catches a wording that shipped and is refuted: %q — the ratchet is open again, and the sweep that removed it from the tree is the only thing keeping it out", shipped)
		}
	}
	// Statements that are TRUE of the delegates today, each clause read off
	// the source: pi's whole readonly branch is `--tools read,grep,find,ls`
	// (piExtraArgsFor), codex's is `read-only` (codexSandboxForTask),
	// claude_code withholds claudeReadonlyWithheld from both spawns whatever
	// the node declares, and `claw`, `opencode` and `cliagent` do not read
	// the flag (backendsReadingReadonly). No needle may touch one: these are
	// the sentences the guard asks authors for.
	for _, correct := range []string{
		"`readonly: true` is answered by a read-only OS sandbox on codex and pi's own `--tools read,grep,find,ls` allowlist; on `claude_code` it withholds the file-writing tools and the worktree-moving pair whatever the node's `tools:` list says. `claw`, `opencode` and `cliagent` do not read the flag at all.",
		"`readonly: true` makes an OS sandbox on codex and a four-reader tool list on pi; on `claude_code` it withholds the file-writing natives from both spawns and names the declaration it deleted on the node's log.",
		"the flag is enforced with the lever each backend has: codex an OS sandbox, pi a readers-only tool list, claude_code the withholding of the file-writing tools; `claw`, `opencode` and `cliagent` do not read the flag at all.",
	} {
		prose := collapseLeverProse(correct)
		for _, claim := range refutedLeverClaims {
			if strings.Contains(prose, claim) {
				t.Errorf("the needle %q matches a statement that is TRUE of the delegates: %q — the guard would redden the correction it asks for, so the needle has to carry what makes the refuted wording refuted", claim, correct)
			}
		}
	}
}

// walkLeverProse hands every author-facing file of leverProseScope to visit,
// with its prose collapsed by the caller's reader, and keeps the walk itself
// honest: each root carries its OWN floor, because the total cannot witness
// the loss of a root — `bots` alone is 453 files today, so an aggregate floor
// stayed green over a scope that had dropped `docs` and the scaffold
// templates. Returns the roots walked, for the caller that pins them.
//
// ONE walk, read by the three guards of this file: a second copy is how one
// of them keeps reading a root the others dropped, and the floors would then
// hold for one question only.
func walkLeverProse(t *testing.T, collapse func(string) string, visit func(path, prose string)) []string {
	t.Helper()
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
			visit(path, collapse(string(raw)))
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", root, err)
		}
		// A FLOOR per root: a guard proves nothing over a root it stopped
		// reading, and the roots differ by an order of magnitude, so one
		// total cannot witness the loss of the smallest.
		if files < top.min {
			t.Fatalf("the walk read %d file(s) of %s, want at least %d: that root broke, and the guard reading this walk is now green over an unexamined tree", files, top.root, top.min)
		}
	}
	return roots
}

// mentionsBackend reports whether prose names a backend as a WORD.
//
// Not strings.Contains: `pi` is a substring of `anthropic` and of `pinned`,
// and a window counted that way reads every page that mentions a provider as
// an enumeration of pi's lever — measured on docs/delegation.md, where the
// phrase below is a cross-reference to dsl.md and the only backend named is
// claude_code.
func mentionsBackend(prose, backend string) bool {
	word := func(c byte) bool {
		return c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
	}
	for at := 0; ; {
		i := strings.Index(prose[at:], backend)
		if i < 0 {
			return false
		}
		abs := at + i
		at = abs + 1
		if abs > 0 && word(prose[abs-1]) {
			continue
		}
		if end := abs + len(backend); end < len(prose) && word(prose[end]) {
			continue
		}
		return true
	}
}

// leverEnumerationMarkers are the phrases that OPEN a per-backend enumeration
// of the flag's lever. Both shipped, and both are what an author copies when
// they explain the flag on a page of their own: the enumeration that follows
// is the sentence that has to be complete.
//
// Measured inside leverProseScope: 6 passages carry one of these phrases, 4
// of which go on to name the levers — docs/dsl.md, bots/assessment/main.bot
// and the two review-pr surfaces. The other two name no lever and are not
// enumerations: docs/delegation.md defers to dsl.md, and
// bots/whats-next's quickref states the flag in one line without attributing
// a lever to anyone.
var leverEnumerationMarkers = []string{"lever each backend has", "lever it has"}

// leverEnumerationWindow is how far past the marker the enumeration is read.
// The longest shipped one (docs/dsl.md) runs ~300 characters from the phrase
// to the clause that names the three backends with no lever.
const leverEnumerationWindow = 700

// TestEveryLeverEnumerationNamesTheBackendsThatIgnoreIt is the class contract
// of the lever enumeration wherever an author meets it, which is the scope
// TestEveryStatementOfTheReadonlyLeverNamesTheBackendsThatIgnoreIt cannot
// reach: that guard owns five named reference surfaces, and the same
// incomplete enumeration shipped on a bot README, on a `##` comment sitting
// on the very node the flag bounds, and on a `##` comment beside a bundle's
// output schema — one naming a single backend of the three that have no
// lever at all, the two others naming none.
//
// A passage is an enumeration when it NAMES the levers: at least two of the
// backends that read the flag, as words, inside the window after the marker.
// A page that merely carries the phrase and defers (docs/delegation.md
// points at dsl.md) attributes nothing and is left alone — a guard that
// reddened there would push authors to delete the cross-reference.
//
// The names come from the AST (backendsReadingReadonly), both sides of it, so
// wiring `claw` moves this guard with the delegates in the same commit.
func TestEveryLeverEnumerationNamesTheBackendsThatIgnoreIt(t *testing.T) {
	reads, ignores := backendsReadingReadonly(t)
	if len(ignores) == 0 {
		t.Fatal("every backend now reads `readonly:`, so an enumeration has nothing to leave out: retire this guard in the same change")
	}
	var offenders []string
	enumerations := 0
	walkLeverProse(t, collapseLeverProse, func(path, prose string) {
		for _, marker := range leverEnumerationMarkers {
			for at := 0; ; {
				i := strings.Index(prose[at:], marker)
				if i < 0 {
					break
				}
				abs := at + i
				at = abs + len(marker)
				window := prose[abs:min(len(prose), abs+leverEnumerationWindow)]
				named := 0
				for _, backend := range reads {
					if mentionsBackend(window, backend) {
						named++
					}
				}
				if named < 2 {
					continue
				}
				enumerations++
				var missing []string
				for _, backend := range ignores {
					if !mentionsBackend(window, backend) {
						missing = append(missing, backend)
					}
				}
				if len(missing) > 0 {
					offenders = append(offenders, fmt.Sprintf("%s: %v not named in %q", path, missing, window))
				}
			}
		}
	})
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d passage(s) enumerate the lever each backend has and leave out a backend that has none: on %v the flag is read nowhere, so the node's `tools:` list is its only bound there, and a reader of these lines declares `readonly:` on one of them for a bound it will not get:\n  %s",
			len(offenders), ignores, strings.Join(offenders, "\n  "))
	}
	// A FLOOR on the recognition: this guard reads two phrases, and a reword
	// that kept the enumeration would leave it walking every page and judging
	// nothing. 4 passages enumerate the levers today, so one may be retired
	// or rewritten without touching this line.
	if enumerations < 3 {
		t.Fatalf("the walk found %d lever enumeration(s), want at least 3 of the 4 shipped (docs/dsl.md, bots/assessment, bots/review-pr README and lib/nodes.bot): the phrase was reworded, and this guard is now green over a class it no longer recognises", enumerations)
	}
}

// TestNoAuthorFacingPageMisstatesTheReadonlyLever is the class contract of
// the refuted statements (refutedLeverClaims): what claude_code does with the
// flag, and what pi's lever is. The class was corrected three times from a
// one-off grep — docs first, then the skills, then the prompts — and each
// pass left siblings behind: a bot README, a `##` comment on the very node the
// flag now bounds, and the scaffold template every new review bot copies.
func TestNoAuthorFacingPageMisstatesTheReadonlyLever(t *testing.T) {
	reads, _ := backendsReadingReadonly(t)
	if !slices.Contains(reads, "claude_code") {
		t.Fatal("claude_code no longer reads `readonly:`, so these sentences would be true again: retire this guard in the same change")
	}
	var offenders []string
	roots := walkLeverProse(t, collapseLeverProse, func(path, prose string) {
		for _, claim := range refutedLeverClaims {
			if strings.Contains(prose, claim) {
				offenders = append(offenders, path+": "+claim)
			}
		}
	})
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d author-facing passage(s) state a rule the delegates do not follow: claude_code reads `readonly:` (%v read it) and withholds the file-writing tools plus the worktree-moving pair whatever the node's tools: list says, and pi's lever is `--tools read,grep,find,ls` — a four-reader tool list, not a sandbox: only codex runs the node in one. State that, or drop the sentence:\n  %s",
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

// TestEveryPromiseOfTheReadonlyNoticeNamesTheBackendThatKeepsIt is the class
// contract of readonlyNoticePromise: a statement that the deleted declaration
// is named on the node's log says WHICH backend names it.
//
// Philosophy rule 1 is what makes the scope load-bearing rather than
// pedantic: "never silently replace an operator's explicit choice" is
// answered, on claude_code, by claudeReadonlyNotices. The other two backends
// that read the flag replace the whole declaration and say nothing, so a
// sentence that generalises the promise ("wherever it is read…") asserts a
// guarantee for exactly the two surfaces that do not offer it — and it is the
// reference page an author reaches for before choosing a backend.
//
// A class walk and not a site list: the sentence shipped on three surfaces at
// once (docs/dsl.md, docs/backends.md and this package's Task.Readonly
// godoc), and a page is written by copying the nearest paragraph.
func TestEveryPromiseOfTheReadonlyNoticeNamesTheBackendThatKeepsIt(t *testing.T) {
	reads, _ := backendsReadingReadonly(t)
	if !slices.Contains(reads, "claude_code") {
		t.Fatal("claude_code no longer reads `readonly:`, so no backend keeps this promise: delete the sentence wherever it is made, and this guard with it")
	}
	// `/` is stripped as well as the markdown markers: this statement wraps
	// across `//` comment lines in the godoc, and the marker sits between its
	// words.
	collapse := func(raw string) string {
		prose := strings.ToLower(raw)
		for _, strip := range []string{"`", "*", "#", "/"} {
			prose = strings.ReplaceAll(prose, strip, " ")
		}
		return strings.Join(strings.Fields(prose), " ")
	}
	var offenders []string
	promises := 0
	check := func(path, prose string) {
		for at := 0; ; {
			i := strings.Index(prose[at:], readonlyNoticePromise)
			if i < 0 {
				return
			}
			abs := at + i
			at = abs + len(readonlyNoticePromise)
			promises++
			// The backend must introduce the promise, not merely appear on
			// the page: every one of these surfaces names claude_code many
			// times for other reasons.
			if window := prose[max(0, abs-160):abs]; !strings.Contains(window, "claude_code") {
				offenders = append(offenders, fmt.Sprintf("%s: %q", path, window))
			}
		}
	}
	walkLeverProse(t, collapse, check)
	// The godoc the two pages were written from, read from this package
	// rather than through the scope above: it is the statement an author
	// reaches through the type, and it is outside leverProseScope.
	raw, err := os.ReadFile("delegate.go")
	if err != nil {
		t.Fatalf("delegate.go: %v", err)
	}
	check("delegate.go", collapse(string(raw)))
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d statement(s) promise the deleted declaration is named on the node's log without naming `claude_code` in front of it — codex (a read-only OS sandbox) and pi (a four-reader tool list) replace the declaration in silence, so an unscoped promise asserts the notice for the two surfaces that do not make it:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	// A FLOOR on the sweep: this guard reads one phrase, and a reword that
	// kept the promise would leave it reading every page and judging nothing.
	// 3 is the class shipped today — docs/dsl.md, docs/backends.md and the
	// Task.Readonly godoc.
	if promises < 3 {
		t.Fatalf("the walk found %d promise(s) of the readonly notice, want at least 3: the phrase was reworded, and this guard is now green over a class it no longer recognises", promises)
	}
}
