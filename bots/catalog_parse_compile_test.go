package bots

import (
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// TestCatalogBotsParseAndCompileClean is the CI floor that every shipped
// catalog workflow (bundles under bots/ + loose .bot files under examples/)
// must PARSE and COMPILE without a single error-severity diagnostic.
//
// This is the guard that was missing when bots/review-pr/main.bot shipped with
// `agent emit:` — a node named after the reserved `emit` node-type keyword
// (ADR-051), which the parser rejects with E002. That bot passed CI because the
// two tests that DO load every catalog bot both bail on a parse/compile failure
// ("another test's job") without ever failing:
//   - bundle_consistency_test.go: `if pr.File == nil { continue }`
//   - catalog_typing_test.go: only inspects four typing codes
//
// So a non-parseable bot sailed through CI and only surfaced in prod as a 502
// on the review-pr webhook (`launch failed: parse error … expected agent
// name`). This test IS that job: a catalog bot that a fresh `iterion validate`
// would reject must fail here, at build time, not at launch time.
//
// It asserts exactly what the launch path requires: parser.Parse must produce
// a File with no error diagnostics, and ir.Compile must produce a Workflow with
// no error diagnostics. Warnings are out of scope (the typing/consistency tests
// own those).
// teamBotFiles lists every workflow of the catalog's bundles, not only each
// bot's main.bot: a sibling workflow (extend.bot, reanchor.bot,
// sync-harness.bot) launches by the same path and must pass the same gates.
//
// It reads the bundle ROOT of catalogRoots, so the bundles this hands to the
// guards that read them alone and the bundles catalogWorkflowFiles walks can
// never be spelled apart — and the root's floor is asserted here too. Measured
// with a second spelling of the same glob: narrowing this one alone left FOUR
// of its seven readers green over zero bundle workflow files — the
// author-quoted-ref guard (an injectable template shape), the typing
// regressions and the two universality guards — while the other three
// reddened only on an emptiness check of their own.
func teamBotFiles(t *testing.T) []string { return catalogRootFiles(t, bundleRoot) }

// exampleFragments are the .bot files under examples/ that are not
// workflows: declaration catalogues meant to be pasted into one
// (examples_cursors_test.go compiles the cursor catalogue inside a workflow).
// Discovery routes them anyway, so each is named in routableNonWorkflows too —
// and dropping it from either list reddens the other.
var exampleFragments = map[string]bool{"../examples/cursors/cursors.bot": true}

// catalogRoot is one glob of the shipped corpus, with the floor under that
// root's OWN discovery and what the root holds.
type catalogRoot struct {
	glob  string
	least int
	what  string
}

// bundleRoot is the catalogue's bundles. Named, because teamBotFiles hands the
// same corpus to the guards that read the bundles alone: one root, one
// spelling, so the two enumerators of this file cannot drift apart.
var bundleRoot = catalogRoot{"*/*.bot", 35, "the catalogue bundles under bots/, every entrypoint of each — not only main.bot"}

// bundleEntryRoot and exampleEntryRoot are the two trees' ENTRY workflows,
// for the guards whose verdict binds ONE file to the bundle that ships it —
// a manifest cross-check, a chat surface. The manifest names the entry
// workflow, not every sibling, so widening those guards to the whole corpus
// would judge a sibling against its neighbour's contract. They go through
// catalogRootFiles like any root, so a rename that stops the main.bot
// spelling matching reddens naming the root, instead of leaving the guard
// green over nothing.
var (
	bundleEntryRoot  = catalogRoot{"*/main.bot", 30, "every bundle's entry workflow — the file its manifest binds to"}
	exampleEntryRoot = catalogRoot{"../examples/*/main.bot", 5, "every example's entry workflow"}
)

// teamEntryFiles lists the bundles' entry workflows — teamBotFiles' root,
// narrowed to the file a manifest binds to.
func teamEntryFiles(t *testing.T) []string { return catalogRootFiles(t, bundleEntryRoot) }

// exampleEntryFiles lists the examples' entry workflows.
func exampleEntryFiles(t *testing.T) []string { return catalogRootFiles(t, exampleEntryRoot) }

// catalogRoots is the one definition of "every shipped workflow" this package
// has: catalogWorkflowFiles walks the whole table, teamBotFiles reads
// bundleRoot alone, and TestEveryRoutableBotIsAShippedWorkflow holds the table
// against the discovery production actually routes. Every guard in this
// package enumerates the corpus through this table or through
// catalogRootFiles on a root of its own — none globs examples/ privately.
//
// The floor is PER ROOT because a total cannot see a root: the standalone
// examples are 7 of the 70 files the walk keeps, so a layout rename that
// stopped matching `../examples/*.bot` leaves 63 — a count every total floor
// downstream reads as healthy, each sitting below today's so a bot may be
// retired — while those seven shipped workflows drop out of most guards at
// once, which is the failure a guard cannot report about itself. Measured with
// that root narrowed to nothing AND its floor dropped to 0: fourteen of the
// sixteen guards over the corpus stay green; the two that redden are the
// `bounded` census and the frozen class list, which names the five prompts
// that left (149 against 154) — neither is a floor on the discovery itself.
// Each floor is read against what the root CONTRIBUTES to the corpus (42, 7,
// 18, 1, 2 — the declaration fragments already dropped) and sits under
// today's count, so a workflow may be retired without touching this table
// while a wholesale loss of its root cannot pass; the dispatcher fallback is
// an exact path, so its floor of 1 says the file must be there.
// catalogRootFiles is where they redden.
var catalogRoots = []catalogRoot{
	bundleRoot,
	{"../examples/*.bot", 5, "the standalone examples"},
	{"../examples/*/*.bot", 15, "the examples that ship a directory, subbot children included"},
	{dispatchFallbackPath, 1, "the dispatcher's `default` assignee, copied into the //go:embed tree at build — a parse regression there breaks `iterion dispatch` in every binary"},
	{"../scripts/adhoc/*.bot", 1, "the operator scripts under scripts/adhoc/"},
}

// dispatchFallbackPath is the dispatcher's zero-config fallback. Shipped like
// a bundle — compiled into every binary and run against a raw issue body —
// it is one exact path rather than a glob, and shippedWorkflowPrefix keys its
// nodes off this same spelling, so the root and the key cannot drift.
const dispatchFallbackPath = "../pkg/cli/templates/dispatch_bots_default.bot"

// catalogRootFiles lists what ONE root contributes to the shipped corpus — its
// glob's matches, the declaration fragments dropped — and fails its caller
// when that is fewer files than the root's floor, naming the root that broke.
//
// It is the one place a root's files are produced, so no enumerator can hand
// them on without the floor: most walks of the catalogue hold no floor of
// their own, and a guard reading a root that matches nothing — a layout
// rename, or a pattern filepath.Glob rejects, which yields no match and no
// error — reports the same green as one reading it whole. A `-run` that
// selects a single guard reddens here too.
//
// The floor reads the KEPT files, not the raw match count: four more
// declaration fragments would otherwise shrink the corpus every guard walks
// while the floor still read the files the glob matched.
func catalogRootFiles(t *testing.T, root catalogRoot) []string {
	t.Helper()
	paths, err := filepath.Glob(root.glob)
	if err != nil {
		t.Fatalf("glob %q (%s): %v", root.glob, root.what, err)
	}
	kept := make([]string, 0, len(paths))
	for _, p := range paths {
		if !exampleFragments[p] {
			kept = append(kept, p)
		}
	}
	if len(kept) < root.least {
		t.Fatalf("root %q (%s) contributes %d workflow file(s), want at least %d: the discovery broke, and every guard built on this root is now passing over a corpus that lost it",
			root.glob, root.what, len(kept), root.least)
	}
	return kept
}

// catalogWorkflowFiles lists every shipped workflow — what every root of
// catalogRoots contributes — sorted, so the order a guard reports in does not
// depend on the order the globs ran.
func catalogWorkflowFiles(t *testing.T) []string {
	t.Helper()
	var targets []string
	for _, root := range catalogRoots {
		targets = append(targets, catalogRootFiles(t, root)...)
	}
	sort.Strings(targets)
	return targets
}

// fixtureNotShippedReason is why a .bot under a testdata/ directory is
// routable here yet owes none of the catalogue's contracts: it is an input to
// this repository's own Go tests. The engine reaches it because discovery
// walks a root at unbounded depth, which only ever happens in a workspace that
// holds iterion's test tree — the released binaries ship no testdata/.
const fixtureNotShippedReason = "a fixture of this repository's Go tests, not a workflow it ships"

// routableNonWorkflows are the .bot files botregistry's discovery turns into
// routable entries that this repository does not SHIP as workflows, each with
// the reason it owes none of the catalogue's contracts. Keyed by the
// repository-relative file, per file rather than per directory: a new fixture
// reddens TestEveryRoutableBotIsAShippedWorkflow until its reason is written
// down here, which is the whole point of naming them.
//
// Every one of these holds acting prompts the class does not reach (18 under
// bots/testdata/ today, all on the full native claude_code toolset). Making
// them unroutable is a change to discovery, and papering them is prose in a
// parser fixture: both are decisions about the engine's walk, not about this
// corpus, so what this table buys is that the next one cannot arrive in
// silence.
var routableNonWorkflows = map[string]string{
	"bots/testdata/input_mapping/action_params.bot":  fixtureNotShippedReason,
	"bots/testdata/input_mapping/agent_prompt.bot":   fixtureNotShippedReason,
	"bots/testdata/input_mapping/compute_expr.bot":   fixtureNotShippedReason,
	"bots/testdata/input_mapping/human_prompt.bot":   fixtureNotShippedReason,
	"bots/testdata/input_mapping/judge_prompt.bot":   fixtureNotShippedReason,
	"bots/testdata/input_mapping/llm_router.bot":     fixtureNotShippedReason,
	"bots/testdata/input_mapping/mapped.bot":         fixtureNotShippedReason,
	"bots/testdata/input_mapping/review_url.bot":     fixtureNotShippedReason,
	"bots/testdata/input_mapping/tool_command.bot":   fixtureNotShippedReason,
	"bots/testdata/input_mapping/tool_script.bot":    fixtureNotShippedReason,
	"bots/testdata/prose_input_refs/human_entry.bot": fixtureNotShippedReason,
	"bots/testdata/prose_input_refs/surfaces.bot":    fixtureNotShippedReason,
	"examples/cursors/cursors.bot":                   "a declaration catalogue meant to be pasted into a workflow (exampleFragments), which discovery routes anyway because it is a .bot file",
}

// repoRelativeBotFile spells a path this package globbed, or one a discovered
// entry reports, as the same repository-relative slash path — the only form in
// which the corpus and the discovery can be compared at all. The walk runs
// from bots/, so a glob yields `golden-master/main.bot` and
// `../examples/x.bot` where discovery yields `../bots/golden-master` and
// `../examples/x.bot`.
func repoRelativeBotFile(p string) string { return filepath.ToSlash(filepath.Join("bots", p)) }

// routedWorkflowFile is the workflow file one discovered source routes to: a
// bundle's `main.bot` — discovery collapses the directory into one entry and
// skips its siblings, so the bundle's name resolves to its main — or the loose
// .bot file itself.
//
// The resolution is botregistry.Entry.MainFile(), the oracle the DISPATCHER
// uses to turn a routed name into a concrete workflow path, rather than a
// second stat-and-join of its own: the day MainFile() honours a manifest
// entrypoint, this guard follows it instead of comparing the wrong file and
// reporting green. It also accepts a synthetic Entry, which is how a path the
// walk reached without producing one is resolved.
func routedWorkflowFile(e botregistry.Entry) string {
	return repoRelativeBotFile(e.MainFile())
}

// discoveredBotSources is the one place this package asks the registry what
// this workspace routes — the resolver `iterion run`, the zero-config
// dispatcher, the studio and the server all go through it — so the guards
// below cannot read two different discoveries, and neither can read one that
// returned nothing: the floor is here, under both of them. A registry that
// stopped walking would leave every comparison built on it vacuously green,
// and that is the one failure a guard cannot report about itself. 74 sources
// reach it today; the floor sits under that so a bot may be retired.
func discoveredBotSources(t *testing.T) ([]botregistry.Entry, []botregistry.DiscoveryError) {
	t.Helper()
	entries, diags := shippedBotSources(t, "..")
	if len(entries) < 60 {
		t.Fatalf("discovery over %v returned %d bot(s), want at least 60: the registry stopped reading this workspace, so every guard built on it is now comparing nothing", botregistry.DefaultPaths(".."), len(entries))
	}
	return entries, diags
}

// shippedBotSources is the discovery over root, less the sources the
// repository does not SHIP.
//
// The roots stay botregistry.DefaultPaths — the discovery's own, so a root
// added there is covered here without a second spelling — and one of them is
// `.botz`, the install cache `iterion bots install` and `iterion bundle pack`
// write into (pkg/botinstall, pkg/botdeps), gitignored (`*.botz`) and never
// committed. A guard that attributes it to "this repository ships" reddens on
// every developer who has ever packed a bundle, over the state
// docs/bot-runs/whats-next.md records as benign and already handled by
// precedence (`bots/` > `.botz/`, pkg/botregistry's
// TestList_DedupesSameBotAcrossRoots).
//
// What the repository ships is GIT's answer, not a hand-written root list: a
// source git IGNORES is out, and a source git does not track YET is IN — that
// is a .bot being authored, and it owes every gate a committed one owes.
func shippedBotSources(t *testing.T, root string) ([]botregistry.Entry, []botregistry.DiscoveryError) {
	t.Helper()
	roots := botregistry.DefaultPaths(root)
	entries, diags, err := botregistry.ListWithDiagnostics(botregistry.ListOptions{Paths: roots})
	if err != nil {
		t.Fatalf("bot discovery over %v: %v", roots, err)
	}
	paths := make([]string, 0, len(entries)+len(diags))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	for _, d := range diags {
		paths = append(paths, d.Path)
	}
	ignored := gitIgnoredPaths(t, root, paths)
	keptEntries := make([]botregistry.Entry, 0, len(entries))
	for _, e := range entries {
		if !ignored[e.Path] {
			keptEntries = append(keptEntries, e)
		}
	}
	keptDiags := make([]botregistry.DiscoveryError, 0, len(diags))
	for _, d := range diags {
		if !ignored[d.Path] {
			keptDiags = append(keptDiags, d)
		}
	}
	return keptEntries, keptDiags
}

// gitIgnoredPaths reports which of paths — bot sources under root, as
// discovery spells them — this repository ignores, keyed by the path as given.
//
// git is asked rather than re-modelled: a rule added to .gitignore moves this
// filter with it, where a hand-written list of ignored roots would not.
// `check-ignore` consults the INDEX, so a TRACKED file that matches an ignore
// rule answers "not ignored" — which is the answer a guard over what the
// repository ships wants. Exit 1 is "none of them"; any other failure is a
// broken oracle and fails the caller, because both of the quiet alternatives
// are wrong (covering the install cache reddens on a benign state, skipping
// everything leaves every guard built on it green over nothing).
func gitIgnoredPaths(t *testing.T, root string, paths []string) map[string]bool {
	t.Helper()
	if len(paths) == 0 {
		return nil
	}
	rels := make([]string, 0, len(paths))
	byRel := map[string]string{}
	for _, p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatalf("spell %q relative to %q: %v", p, root, err)
		}
		rel = filepath.ToSlash(rel)
		rels = append(rels, rel)
		byRel[rel] = p
	}
	cmd := exec.Command("git", gitlib.NoAutoMaintenance("-C", root, "check-ignore", "-z", "--stdin")...)
	cmd.Stdin = strings.NewReader(strings.Join(rels, "\x00") + "\x00")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			t.Fatalf("git check-ignore under %s: %v (%s)", root, err, strings.TrimSpace(stderr.String()))
		}
	}
	ignored := map[string]bool{}
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" {
			continue
		}
		p, ok := byRel[rel]
		if !ok {
			t.Fatalf("git check-ignore answered about %q, which was not asked about: %q", rel, out)
		}
		ignored[p] = true
	}
	return ignored
}

// TestEveryRoutableBotIsAShippedWorkflow closes the corpus against the
// DISCOVERY production routes, rather than against a second glob of its own.
//
// catalogRoots is a table of globs that reach one directory level;
// botregistry.discoverBots walks botregistry.DefaultPaths — [bots examples
// .botz] — with filepath.WalkDir at UNBOUNDED depth, pruning only a bot's
// `lib/` fragments and the inside of a bundle. The two disagree:
// `examples/a/b/c.bot` is routable and matches no root, and 12 files under
// bots/testdata/ are routable entries the globs never see. Where they
// disagree, an acting prompt ships with no paragraph and no guard able to
// redden, which is why this guard asks the registry itself — the resolver
// `iterion run`, the zero-config dispatcher, the studio and the server all go
// through — which bots this workspace routes, and requires each one to be in
// the corpus or named above.
//
// It does not re-model that walk: a change to its depth, its pruning or to
// DefaultPaths' roots moves this guard with it, and anything the two
// enumerations do not already agree on falls on the failing side. What the
// walk REACHED is the subject, not what it returned — the entries plus the
// sources it dropped for a name already claimed, each resolved to its
// workflow file by the dispatcher's own oracle.
//
// `Enabled` is not consulted: discovery returns a disabled bot too (the studio
// lists it to flip back on), so the obligation does not depend on the flag.
func TestEveryRoutableBotIsAShippedWorkflow(t *testing.T) {
	entries, diags := discoveredBotSources(t)
	// Every source the walk REACHED, with how it is routed — not only the
	// ones that became entries. A .bot whose name another source already
	// claimed produces no Entry (botregistry records it as a shadowed
	// diagnostic instead), so a check reading the entries alone passes over
	// the very same acting prompt under the basename `main.bot` that it
	// reddens under any unclaimed one — and `main.bot` is the commonest
	// .bot filename there is. A diagnostic of any other kind is a source
	// whose contents are unknown, which this guard cannot clear at all.
	reached := map[string]string{}
	for _, e := range entries {
		reached[routedWorkflowFile(e)] = fmt.Sprintf("routed as bot %q", e.Name)
	}
	for _, d := range diags {
		if d.Kind == botregistry.DiscoveryErrorShadowed {
			reached[routedWorkflowFile(botregistry.Entry{Path: d.Path})] = "reached by discovery, then shadowed: " + d.Error
			continue
		}
		t.Errorf("discovery skipped %s (%s): %s — this guard cannot read what it holds", d.Path, d.Kind, d.Error)
	}
	corpus := map[string]bool{}
	for _, p := range catalogWorkflowFiles(t) {
		corpus[repoRelativeBotFile(p)] = true
	}
	files := make([]string, 0, len(reached))
	for file := range reached {
		files = append(files, file)
	}
	sort.Strings(files)
	var orphans []string
	seen := map[string]bool{}
	for _, file := range files {
		switch {
		case corpus[file]:
		case routableNonWorkflows[file] != "":
			seen[file] = true
		default:
			orphans = append(orphans, fmt.Sprintf("%s (%s)", file, reached[file]))
		}
	}
	var stale []string
	for file, why := range routableNonWorkflows {
		if !seen[file] {
			stale = append(stale, fmt.Sprintf("%s (%s) is not a routable bot outside the corpus: drain its entry", file, why))
		}
	}
	sort.Strings(orphans)
	sort.Strings(stale)
	if len(orphans) > 0 {
		t.Errorf("%d bot(s) this workspace ROUTES are outside the shipped corpus, so every guard built on catalogWorkflowFiles passes over them — add the root to catalogRoots, or name the file in routableNonWorkflows with the reason it ships no workflow:\n  %s",
			len(orphans), strings.Join(orphans, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("%d stale entr(y/ies) in routableNonWorkflows:\n  %s", len(stale), strings.Join(stale, "\n  "))
	}
	t.Logf("%d routable bot(s), %d source(s) reached, %d outside the corpus and named", len(entries), len(reached), len(seen))
}

// TestNoShippedBotNameIsShadowed holds this workspace to ONE source per
// routable name.
//
// Two .bot files that answer to the same name are not a tie: the first the
// walk reaches routes, and the other is reachable by no name at all — not by
// `iterion run <name>`, not by a ticket's assignee, not in the studio gallery.
// botregistry REFUSES to create such a collision (EnsureNameFree, "a duplicate
// name there is what makes `iterion run <name>` and dispatcher routing
// ambiguous"), so one arriving as a new file is the same defect arriving the
// other way round — and the four this repository shipped were all `main.bot`
// or `sample.bot`, the two filenames an author reaches for first.
//
// The escape hatch is the name itself: a bundle's manifest `name:`, a loose
// file's `## ---` frontmatter `name:` (examples/catalog-card.bot shows one).
func TestNoShippedBotNameIsShadowed(t *testing.T) {
	_, diags := discoveredBotSources(t)
	var shadowed []string
	for _, d := range diags {
		if d.Kind == botregistry.DiscoveryErrorShadowed {
			shadowed = append(shadowed, fmt.Sprintf("%s — %s", repoRelativeBotFile(d.Path), d.Error))
		}
	}
	sort.Strings(shadowed)
	if len(shadowed) > 0 {
		t.Errorf("%d .bot file(s) this repository ships answer to a name another source already claimed, so nothing can route to them — give each one a distinct name:\n  %s",
			len(shadowed), strings.Join(shadowed, "\n  "))
	}
}

// TestShippedBotSourcesSkipWhatGitIgnores holds the two guards above to the
// sources this repository SHIPS, against a workspace in the state a developer
// reaches by running `iterion bots install` or `iterion bundle pack` once.
//
// `.botz/<name>/` is where both land a bundle, `*.botz` is gitignored, and a
// packed copy of a bots/ bundle answers to the SAME name — so without the
// filter the install cache arrives as an orphan outside the corpus AND as a
// shadowed source, and the remedy the message prints ("give it a distinct
// name") cannot be followed: the path is keyed by the bundle's own name by
// design. The precedence that makes the state benign is pkg/botregistry's own
// (TestList_DedupesSameBotAcrossRoots).
//
// It runs over a workspace of its own rather than the repository, because what
// it must prove is that an IGNORED source leaves and an untracked-but-shipped
// one stays — a distinction the repository's own tree (where everything under
// bots/ is committed) cannot show. It is the same function the guards call.
func TestShippedBotSourcesSkipWhatGitIgnores(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const stub = "agent x:\n  model: \"test\"\n"
	write(".gitignore", "*.botz\n")
	// Committed by nobody yet, and ignored by nobody: a bot being authored.
	write("bots/local-only/manifest.yaml", "name: local-only\ndescription: authored here.\n")
	write("bots/local-only/main.bot", stub)
	// The install cache: an installed dependency, plus a packed copy of the
	// bundle above — which loses the name to it.
	write(".botz/installed-dep/manifest.yaml", "name: installed-dep\ndescription: from a dependency.\n")
	write(".botz/installed-dep/main.bot", stub)
	write(".botz/local-only/manifest.yaml", "name: local-only\ndescription: a packed copy.\n")
	write(".botz/local-only/main.bot", stub)
	// Committed under an ignored path: the predicate is what git ANSWERS, not
	// where the file sits, and `check-ignore` consults the index — so a bot a
	// repository chose to track owes the guards whatever its path matches.
	write(".botz/tracked-anyway/manifest.yaml", "name: tracked-anyway\ndescription: committed on purpose.\n")
	write(".botz/tracked-anyway/main.bot", stub)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", gitlib.NoAutoMaintenance(append([]string{"-C", root}, args...)...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v (%s)", args, root, err, out)
		}
	}
	git("init", "-q")
	git("add", "-f", ".botz/tracked-anyway")

	// The premise, asserted rather than assumed: discovery really does reach
	// all three, and really does shadow the packed copy.
	raw, rawDiags, err := botregistry.ListWithDiagnostics(botregistry.ListOptions{Paths: botregistry.DefaultPaths(root)})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 3 || len(rawDiags) != 1 || rawDiags[0].Kind != botregistry.DiscoveryErrorShadowed {
		t.Fatalf("discovery = %d entr(y/ies) + %#v, want 3 entries and one shadowed diagnostic", len(raw), rawDiags)
	}

	entries, diags := shippedBotSources(t, root)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "local-only" || names[1] != "tracked-anyway" {
		t.Errorf("shipped entries = %v, want the untracked bot under bots/ and the one committed under .botz: the gitignored install cache is not source this repository ships, and git's answer is the predicate", names)
	}
	if len(diags) != 0 {
		t.Errorf("shipped diagnostics = %#v, want none: a packed copy under .botz losing its name to the bundle it was packed from is the precedence pkg/botregistry applies on purpose", diags)
	}
}

// TestEveryShippedWorkflowRootIsReached is the named gate for the discovery
// itself, read even when no other guard runs.
//
// It also holds the two properties the corpus is handed on: SORTED, so what a
// guard reports does not depend on the order the globs ran, and each file
// ONCE, because two roots that overlap would walk a workflow twice — a member
// counted twice against a floor, and a line written twice into the frozen
// class.
func TestEveryShippedWorkflowRootIsReached(t *testing.T) {
	paths := catalogWorkflowFiles(t)
	if !sort.StringsAreSorted(paths) {
		t.Errorf("catalogWorkflowFiles is not sorted, so every guard built on it reports in glob order: %v", paths)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			t.Errorf("%s is reached by two roots of catalogRoots, so every guard walks it twice", p)
		}
		seen[p] = true
	}
	t.Logf("the %d root(s) of the shipped corpus reach %d workflow file(s)", len(catalogRoots), len(paths))
}

func TestCatalogBotsParseAndCompileClean(t *testing.T) {
	targets := catalogWorkflowFiles(t)
	if len(targets) == 0 {
		t.Fatal("no catalog workflows found — discovery glob likely broke")
	}

	checked := 0
	for _, path := range targets {
		// The bot's unit: its main and the fragments the imports reach.
		u := unittest.Whole(unit.LoadDir(path))
		parseFailed := false
		for _, d := range u.Diagnostics {
			if d.Severity == parser.SeverityError {
				parseFailed = true
				t.Errorf("%s: parse error: %s", path, d.Error())
			}
		}
		if u.Merged == nil {
			t.Errorf("%s: parser produced no File (unrecoverable parse failure)", path)
			continue
		}
		if parseFailed {
			continue // don't cascade compile errors off a broken parse
		}

		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			t.Errorf("%s: compile produced no Workflow", path)
		}
		for _, d := range cr.Diagnostics {
			if d.Severity == ir.SeverityError {
				t.Errorf("%s: compile error: %s", path, d.Error())
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no catalog workflows parsed+compiled — discovery glob likely broke")
	}
}
