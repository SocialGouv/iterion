package bots

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/skillshare"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// briefMarker is the name a bundle's pages use for the always-loaded durable
// brief. It is the one token every promise of the class carries, which is why
// the ticket's own verify step greps for it.
const briefMarker = "CONTEXT_BRIEF"

// clawMemoryTools are the three tools a `memory:` block installs on a claw
// run (model.installWorkspaceMemory, from the node's MemorySpec). They exist
// nowhere else — no `memory:` block, no tools — which is what makes a page
// naming them a claim about the bundle that carries it.
var clawMemoryTools = []string{"memory_read", "memory_write", "memory_list"}

// briefPromiseExemptions are the pages whose mention of the brief is not a
// promise the bundle makes, each with the reason it survives and, where the
// reason is a statement in the page itself, the words that must still be
// there. An exemption whose marker is gone is NOT an exemption: the page is
// read like any other.
//
// Keyed by path relative to the repository root.
var briefPromiseExemptions = map[string]struct {
	reason string
	marker string
}{
	"bots/whats-next/skills/session-continuity.md": {
		reason: "the shared mechanism page (internal/skillshare family, source whats-next, variant evolve): it documents the claw write contract for the bundles that carry a `memory:` block, and its own note refuses the write for THIS bundle",
		marker: "do not keep a `CONTEXT_BRIEF.md` here, and do not promise one",
	},
}

// TestABundleSellsADurableBriefOnlyWhereItCanWriteOne is the agreement #2773
// asks for, as a guard rather than a grep: a bundle may sell a durable
// cross-session brief exactly when one of its own nodes can write one.
//
// Both halves are MEASURED, neither restated:
//
//   - the capability comes from the engine's own seam — EffectiveToolNames
//     over every route the node may take, which is the list the runtime puts
//     on delegate.Task.AllowedTools after its appends — plus the permission
//     rules that decide what RUNS. On the CLI backends the declaration is what
//     bounds availability (claudeNativeDisallowedTools withholds every native
//     tool the list omits), and `memory_*` plus autoload are claw-only, so a
//     claude_code node that names neither a shell nor a writer and carries no
//     `memory:` block has no path to the tree at all.
//   - the promise comes from the bundle's own pages, attributed to the bundle
//     it is ABOUT: a generated catalogue card describes another bot, so a card
//     for `evolve` is checked against bots/evolve, not against its host.
//
// Mutating either half reddens. Put a promise back in any page and that page's
// own line is named; widen a node's `tools:` with a writer and the capability
// half flips, which is the signal to revisit the pages rather than the guard.
func TestABundleSellsADurableBriefOnlyWhereItCanWriteOne(t *testing.T) {
	t.Parallel()

	canWrite := map[string]string{}
	for _, sw := range shippedWorkflows(t) {
		slug := bundleSlug(sw.prefix)
		if slug == "" {
			continue
		}
		if why := durableMemoryWritePath(sw); why != "" {
			canWrite[slug] = why
		} else {
			canWrite[slug] = ""
		}
	}
	// A guard whose capability half saw no bundle would accept every promise.
	if len(canWrite) < 20 {
		t.Fatalf("the walk resolved %d bundles of a catalogue that ships well over 20 — the capability half is judging nothing", len(canWrite))
	}
	if _, ok := canWrite["whats-next"]; !ok {
		t.Fatal("bots/whats-next did not resolve — it is the bundle this guard exists for")
	}
	if canWrite["evolve"] == "" {
		t.Error("bots/evolve resolved as unable to write durable memory, but it ships a `memory:` block on a claw node — the capability half reads the wrong thing, and every verdict below is suspect")
	}

	promises := briefPromises(t)
	// Both of the honest shapes must be in the corpus, or the verdict below
	// proves nothing: a bundle that may promise (evolve) and a page exempted
	// by its own note (whats-next's mechanism page).
	if len(promises) == 0 {
		t.Fatal("no mention of the durable brief anywhere under bots/ — this guard lost its subject")
	}

	var unbacked, unattributed []string
	sawPermitted, sawExempt := 0, 0
	for _, p := range promises {
		if ex, ok := briefPromiseExemptions[p.file]; ok && exemptionHolds(t, p.file, ex.marker) {
			sawExempt++
			continue
		}
		why, known := canWrite[p.about]
		if !known {
			unattributed = append(unattributed, fmt.Sprintf("%s:%d names bundle %q, which is not a shipped bundle: %s", p.file, p.line, p.about, p.text))
			continue
		}
		if why == "" {
			unbacked = append(unbacked, fmt.Sprintf("%s:%d sells %s for bundle %q, which has no write path: %s", p.file, p.line, briefMarker, p.about, p.text))
			continue
		}
		sawPermitted++
	}

	if sawExempt == 0 {
		t.Errorf("no exempted page was read — %s claims to be exempt by its own note, so either the note or the page moved, and the exemption table is stale",
			"bots/whats-next/skills/session-continuity.md")
	}
	if sawPermitted == 0 {
		t.Error("no promise was backed by a real write path — the permitting arm of this guard never ran, so it cannot be trusted to permit evolve's card")
	}
	if len(unattributed) > 0 {
		sort.Strings(unattributed)
		t.Errorf("%d mention(s) of the durable brief name a bundle this walk cannot resolve:\n  %s", len(unattributed), strings.Join(unattributed, "\n  "))
	}
	if len(unbacked) > 0 {
		sort.Strings(unbacked)
		t.Errorf("%d page(s) sell a durable %s a bundle cannot write (iterion#2773) — drop the promise, or give the node a path to the tree (`backend: \"claw\"` with a `memory:` block, or a `tools:` list that names a writer):\n  %s",
			len(unbacked), briefMarker, strings.Join(unbacked, "\n  "))
	}
	t.Logf("%d bundles, %d brief mention(s): %d permitted, %d exempt", len(canWrite), len(promises), sawPermitted, sawExempt)
}

// bundleSlug turns a shippedWorkflow key into the bundle directory name, and
// returns "" for a workflow that is not a bundle (an example, the dispatcher
// fallback, an adhoc script) — those have no manifest and sell nothing.
func bundleSlug(prefix string) string {
	if strings.Contains(prefix, ":") || !strings.HasSuffix(prefix, "/") {
		return ""
	}
	return strings.TrimSuffix(prefix, "/")
}

// durableMemoryWritePath answers whether any LLM node of one bundle holds a
// path to the workspace memory tree, and names it. The empty string means no
// node does.
func durableMemoryWritePath(sw shippedWorkflow) string {
	exec := model.NewProgramExecutor(sw.wf)
	ids := make([]string, 0, len(sw.wf.Nodes))
	for id := range sw.wf.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		llm, ok := sw.wf.Nodes[id].(ir.LLMNode)
		if !ok {
			continue
		}
		routes := append([]string{exec.EffectiveBackendName(sw.wf.Nodes[id])}, exec.EffectiveFallbackBackends(sw.wf.Nodes[id])...)
		// `memory:` and autoload are claw-only (C047, ir.validate_nodes), so
		// the block is a path exactly on a claw route.
		if llm.GetMemory() != nil && slices.Contains(routes, delegate.BackendClaw) {
			return fmt.Sprintf("node %q carries a `memory:` block on a claw route", id)
		}
		// An UNDECLARED list is not an empty one: on the CLI backends it means
		// "no restriction", so the native roster keeps Bash, Write and Edit.
		// Asked of toolcatalog.ToolsDeclared, the same predicate buildTask
		// stamps on the task — `tools: []` IS a declaration, and reading it as
		// "no declaration" is the defect that let a node asking for no tools
		// keep the whole roster.
		if !toolcatalog.ToolsDeclared(llm.GetTools()) && !onlyClawRoutes(routes) {
			return fmt.Sprintf("node %q declares no `tools:` list, so its CLI route keeps the native write surface", id)
		}
		// Taken over the pessimistic reading of the node's effort: an incoming
		// edge may raise it to ultracode, which appends `agent` on claw.
		for _, writer := range writersAmong(exec.EffectiveToolNames(sw.wf.Nodes[id], true)) {
			if deniedByPermissionRules(llm, sw.wf, writer) {
				continue
			}
			return fmt.Sprintf("node %q holds %q", id, writer)
		}
	}
	return ""
}

// onlyClawRoutes reports whether every route of a node is claw — the one
// backend where an empty `tools:` list means "no tool loop at all" instead of
// "no restriction".
func onlyClawRoutes(routes []string) bool {
	for _, r := range routes {
		if r != delegate.BackendClaw {
			return false
		}
	}
	return len(routes) > 0
}

// writersAmong folds a held tool list onto the shared spelling table and
// returns the writers it names.
func writersAmong(held []string) []string {
	var out []string
	// The shared set, never a second list: toolcatalog owns which concepts
	// write, and DurableWriteCanonicals owns this question's reading of it
	// (the shell and claw's own memory_write included).
	writers := toolcatalog.DurableWriteCanonicals()
	for _, name := range held {
		if slices.Contains(writers, toolcatalog.CanonicalToolName(name)) {
			out = append(out, name)
		}
	}
	return out
}

// deniedByPermissionRules reports whether the runtime permission gate removes
// one tool the declaration granted. The gate bounds what RUNS, independently
// of what exists, so a bundle whose policy denies `Write` has no write path
// even where its list names one.
//
// An unset mode is off (ir.Workflow.Permission), and an off gate makes the
// rule lists inert — read as "not denied" rather than guessed.
func deniedByPermissionRules(llm ir.LLMNode, wf *ir.Workflow, tool string) bool {
	mode := llm.GetPermission()
	if mode == "" {
		mode = wf.Permission
	}
	if mode == "" || mode == "off" {
		return false
	}
	for _, rule := range ir.EffectivePermissionRules(llm.GetPermissionDeny(), wf.PermissionDeny) {
		// A rule is `Tool` or `Tool(pattern)`; a scoped rule bounds a subset
		// of the calls, so only the bare form removes the tool outright.
		if strings.Contains(rule, "(") {
			continue
		}
		if toolcatalog.CanonicalToolName(strings.TrimSpace(rule)) == toolcatalog.CanonicalToolName(tool) {
			return true
		}
	}
	return false
}

// briefPromise is one mention of the durable brief: where it is written, and
// which bundle it is ABOUT.
type briefPromise struct {
	file  string // repo-relative
	line  int
	about string // bundle slug the mention describes
	text  string
}

// briefPromises walks every bundle's shipped pages and returns each mention of
// the durable brief, attributed to the bundle it describes.
//
// Attribution matters because the generated catalogue is a page about OTHER
// bots: inside `skills/iterion-bot-catalog.md` a mention belongs to the bundle
// whose card encloses it, so Evoly's claw memory claim is checked against
// bots/evolve wherever the card is copied.
func briefPromises(t *testing.T) []briefPromise {
	t.Helper()
	bundles, err := filepath.Glob("*/manifest.yaml")
	if err != nil {
		t.Fatalf("glob the bundles: %v", err)
	}
	if len(bundles) < 20 {
		t.Fatalf("globbed %d bundle manifests of a catalogue that ships well over 20 — the promise half is judging nothing", len(bundles))
	}
	var out []briefPromise
	for _, manifest := range bundles {
		dir := filepath.Dir(manifest)
		var pages []string
		// Every page the bundle ships, sub-bot `.bot` files included: a promise
		// in a child workflow is the bundle's promise too.
		for _, pattern := range []string{"manifest.yaml", "*.bot", "*.md", "skills/*.md"} {
			matches, err := filepath.Glob(filepath.Join(dir, pattern))
			if err != nil {
				t.Fatalf("glob %s/%s: %v", dir, pattern, err)
			}
			pages = append(pages, matches...)
		}
		sort.Strings(pages)
		for _, page := range pages {
			body, err := os.ReadFile(page)
			if err != nil {
				t.Fatalf("read %s: %v", page, err)
			}
			card := ""
			for i, line := range strings.Split(string(body), "\n") {
				if slug := catalogCardSlug(line); slug != "" {
					card = slug
				}
				if !strings.Contains(line, briefMarker) {
					continue
				}
				// A manifest `##` comment is the bundle's changelog, which no
				// model ever reads: it records a claim, it does not make one.
				if strings.HasSuffix(page, "manifest.yaml") && strings.HasPrefix(strings.TrimSpace(line), "##") {
					continue
				}
				about := dir
				if filepath.Base(page) == "iterion-bot-catalog.md" && card != "" {
					about = card
				}
				out = append(out, briefPromise{
					file:  "bots/" + filepath.ToSlash(page),
					line:  i + 1,
					about: about,
					text:  strings.TrimSpace(line),
				})
			}
		}
	}
	return out
}

// catalogCardSlug reads the bundle slug out of a generated catalogue card
// heading (`### `+"`slug`"+` — Display Name`), and returns "" for any other
// line. The backticked slug is the shape botregistry renders, and the heading
// level is the card's own.
func catalogCardSlug(line string) string {
	rest, ok := strings.CutPrefix(line, "### `")
	if !ok {
		return ""
	}
	end := strings.Index(rest, "`")
	if end <= 0 {
		return ""
	}
	name := rest[:end]
	// A comparison card (`### `+"`a`"+` vs `+"`b`"+``) describes a pair, not a
	// bundle's own claims. renderCatalogCard writes the slug, then " — " and
	// the display name only when the manifest carries one (catalog.go:112-115),
	// so a per-bot card ends after the slug or continues with the em dash —
	// and nothing else.
	if tail := strings.TrimSpace(rest[end+1:]); tail != "" && !strings.HasPrefix(tail, "—") {
		return ""
	}
	return name
}

// exemptionHolds reports whether an exempted page still carries the statement
// its exemption rests on. Whitespace is collapsed first: the note is prose in
// a blockquote, so a re-wrap must not break the guard, while deleting the
// sentence must.
func exemptionHolds(t *testing.T, page, marker string) bool {
	t.Helper()
	if marker == "" {
		return true
	}
	body, err := os.ReadFile(filepath.Join("..", page))
	if err != nil {
		t.Fatalf("read the exempted page %s: %v", page, err)
	}
	collapsed := strings.Join(strings.Fields(strings.ReplaceAll(string(body), ">", " ")), " ")
	return strings.Contains(collapsed, strings.Join(strings.Fields(marker), " "))
}

// memoryRootExemptions are the shared-skill family SOURCES whose page names
// claw's memory tools in a bundle that cannot serve them, each with its reason
// and the words the page must still carry for the exemption to hold.
var memoryRootExemptions = map[string]struct {
	reason string
	marker string
}{
	"session-continuity.md": {
		reason: "the family's other member is bots/evolve, whose claw node really does carry a `memory:` block: the page documents the write contract FOR that member, and its own note refuses the write for the source bundle",
		marker: "This bundle cannot write the tree.",
	},
}

// TestEverySharedMemorySkillIsRootedWhereItCanBeServed closes the root cause
// #2773 reported, one layer under the promise: a shared skill whose SOURCE
// page teaches claw's `memory_*` tools lives in a bundle that cannot serve
// them, so every reader of that bundle reads a contract its own node cannot
// honour.
//
// The source is where a family's text is authored and `task skills:sync`
// copies mirrors FROM, so a family rooted in a bundle with no memory tree is
// the defect; a VARIANT in such a bundle is not, since a variant diverges on
// purpose. The capability is read from the declaration that installs the
// tools — a `memory:` block on a claw route, which is the only thing
// model.installWorkspaceMemory runs on — not from the page.
//
// An exemption is allowed, and it must be EARNED by a statement in the page
// itself: the marker is asserted here, so deleting the scoping note reddens
// the file it was protecting.
func TestEverySharedMemorySkillIsRootedWhereItCanBeServed(t *testing.T) {
	t.Parallel()

	served := map[string]string{}
	for _, sw := range shippedWorkflows(t) {
		if slug := bundleSlug(sw.prefix); slug != "" {
			served[slug] = clawMemoryToolPath(sw)
		}
	}
	if served["evolve"] == "" {
		t.Error("bots/evolve resolved as unable to serve the memory tools, but it ships a `memory:` block on a claw node — the capability half reads the wrong thing")
	}

	families := skillshare.Families()
	if len(families) < 15 {
		t.Fatalf("the declaration holds %d families of the 20 this tree carries — the subject moved and every verdict below is vacuous", len(families))
	}
	var unrooted []string
	checked, exempt := 0, 0
	for _, f := range families {
		page := f.Path(f.Source) // repo-relative
		body, err := os.ReadFile(filepath.Join("..", page))
		if err != nil {
			t.Fatalf("read the family source %s: %v", page, err)
		}
		if !namesAClawMemoryTool(string(body)) {
			continue
		}
		checked++
		if why, known := served[f.Source]; !known {
			t.Errorf("family %s is rooted in %q, which this walk cannot resolve to a shipped bundle", f.Name, f.Source)
			continue
		} else if why != "" {
			continue
		}
		ex, ok := memoryRootExemptions[f.Name]
		if ok && strings.Contains(string(body), ex.marker) {
			exempt++
			continue
		}
		fix := "re-root the family on a member that carries a `memory:` block on a claw node, or scope the page with a note saying this bundle cannot reach the tree"
		if ok {
			fix = "the page no longer carries the note its exemption rests on (" + ex.marker + ") — restore it, or " + fix
		}
		unrooted = append(unrooted, fmt.Sprintf("%s teaches claw's memory tools and its source bundle %q has no `memory:` block on a claw node: %s", page, f.Source, fix))
	}

	if checked == 0 {
		t.Fatal("no family source names claw's memory tools — this guard lost its subject; it must see session-continuity.md")
	}
	if exempt == 0 {
		t.Errorf("no exemption was earned, yet %d family source(s) were checked: either every source can now serve the tools (retire the table) or a marker moved", checked)
	}
	if len(unrooted) > 0 {
		sort.Strings(unrooted)
		t.Errorf("%d shared memory skill(s) rooted where they cannot be served (iterion#2773):\n  %s", len(unrooted), strings.Join(unrooted, "\n  "))
	}
	t.Logf("%d family source(s) teaching the memory tools, %d exempt", checked, exempt)
}

// namesAClawMemoryTool reports whether a page names one of the three tools a
// `memory:` block installs.
func namesAClawMemoryTool(body string) bool {
	for _, tool := range clawMemoryTools {
		if strings.Contains(body, tool) {
			return true
		}
	}
	return false
}

// clawMemoryToolPath names the node that makes claw's `memory_*` tools exist
// for one bundle, or returns "" when none does.
//
// The tools come from the node's `memory:` spec on a claw run
// (model.installWorkspaceMemory) — `memory:` on any other backend is
// diagnosed and ignored (pkg/dsl/ir, "memory_read/memory_write/memory_list
// and autoload are claw-only"), so the block alone is not enough and neither
// is the route.
func clawMemoryToolPath(sw shippedWorkflow) string {
	exec := model.NewProgramExecutor(sw.wf)
	ids := make([]string, 0, len(sw.wf.Nodes))
	for id := range sw.wf.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		llm, ok := sw.wf.Nodes[id].(ir.LLMNode)
		if !ok || llm.GetMemory() == nil {
			continue
		}
		routes := append([]string{exec.EffectiveBackendName(sw.wf.Nodes[id])}, exec.EffectiveFallbackBackends(sw.wf.Nodes[id])...)
		if slices.Contains(routes, delegate.BackendClaw) {
			return fmt.Sprintf("node %q carries a `memory:` block on a claw route", id)
		}
	}
	return ""
}
