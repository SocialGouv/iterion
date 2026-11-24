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
	"github.com/SocialGouv/iterion/pkg/bundlelint"
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
//
// EMPTY, and that is the verdict: its one row was the shared mechanism page
// in a bundle that cannot write the tree, and rooting the family on the
// member that CAN (internal/skillshare/families.go, iterion#2832) left that
// bundle's copy with no brief to mention. Every row is audited by
// reportUnearnedExemptions, so a row that stops being read is a finding
// rather than a silent pardon — the list can only shrink without a
// review.
var briefPromiseExemptions = map[string]struct {
	reason string
	marker string
}{}

// TestABundleSellsADurableBriefOnlyWhereItCanWriteOne is the agreement #2773
// asks for, as a guard rather than a grep: a bundle may sell a durable
// cross-session brief exactly when one of its own nodes can write one.
//
// Both halves are MEASURED, neither restated:
//
//   - the capability is the LINT's own answer, asked of the same predicate
//     C274 asks per node (bundlelint.NodeDurableWriter): the routes in the
//     walk's posture, the engine's effective-tool seam, the per-backend
//     readings of `readonly:` and of the `tools:` list, and the gate asked
//     per route. This guard used to re-derive each arm, and every
//     re-derivation was the lint's last defect one reader behind —
//     dispatch's route posture against the walk's, and the gate asked over
//     the union of the routes' spellings (iterion#2881), the unread
//     `readonly:` flag and the list pi/kimi/grok/opencode never receive
//     (iterion#2882). One predicate, two readers, no drift by construction.
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
	// The honest shape must be in the corpus, or the verdict below proves
	// nothing: a bundle that MAY promise (evolve, which carries the family's
	// source page since iterion#2832). The exempted shape is gone with the
	// table — the rooting removed the one page that needed a pardon.
	if len(promises) == 0 {
		t.Fatal("no mention of the durable brief anywhere under bots/ — this guard lost its subject")
	}

	var unbacked, unattributed []string
	sawPermitted := 0
	usedBriefExemptions := map[string]bool{}
	for _, p := range promises {
		if ex, ok := briefPromiseExemptions[p.file]; ok && exemptionHolds(t, p.file, ex.marker) {
			usedBriefExemptions[p.file] = true
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

	reportUnearnedExemptions(t, "briefPromiseExemptions", briefExemptionReasons(), usedBriefExemptions)
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
	t.Logf("%d bundles, %d brief mention(s): %d permitted, %d exempt (of %d exemptions declared)",
		len(canWrite), len(promises), sawPermitted, len(usedBriefExemptions), len(briefPromiseExemptions))
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
//
// The question is asked ONCE, of the predicate the lint asks
// (bundlelint.NodeDurableWriter — C274's capability half): a `memory:` block
// whose write tool a run gets and the gate leaves, or a durable writer the
// node holds on one of its routes whose call THAT ROUTE's gate leaves. Every
// arm the engine decides — the routes a `when:` gate walks, the appends a
// route decides, what `readonly:` withholds on a backend, whether the
// `tools:` list reaches it, the gate's answer per route — is read there and
// nowhere here, so this guard cannot drift from the lint by construction.
func durableMemoryWritePath(sw shippedWorkflow) string {
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
		if writer := bundlelint.NodeDurableWriter(llm, sw.wf); writer != "" {
			return fmt.Sprintf("node %q holds a durable write path through %q", id, writer)
		}
	}
	return ""
}

// TestTheCapabilityHalfAsksTheGateAboutAMemoryBlock holds the shared
// predicate's `memory:` arm to examples the CORPUS cannot provide: no
// shipped bundle combines a `memory:` block with a `permission:` gate, and
// bots/evolve — the one bundle with blocks — declares `bash` besides them,
// so the walk would reach the same verdict with the arm removed. Without
// this, the gate question in the `memory:` arm is asserted over nothing.
//
// Both directions, over a workflow built here: a gated block whose allow list
// names no writer is NOT a path, and the same block under an allow row that
// names the store tool is.
func TestTheCapabilityHalfAsksTheGateAboutAMemoryBlock(t *testing.T) {
	t.Parallel()

	writers := writersAmong(clawMemoryTools)
	if len(writers) == 0 {
		t.Fatal("no member of clawMemoryTools is a durable writer in the shared vocabulary — the arm this test is about credits nothing, and both cases below would pass over a no-op")
	}
	build := func(mode string, allow []string) shippedWorkflow {
		node := &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "nexie"},
			LLMFields: ir.LLMFields{Backend: delegate.BackendClaw},
			// No writer of its own: the block is the only path, so the
			// verdict is the arm's and not the writer loop's.
			Tools:  []string{"read_file", "glob", "grep"},
			Memory: &ir.Memory{Enabled: true, Scope: "vision", Read: true, Write: true, Visibility: "bot"},
		}
		return shippedWorkflow{prefix: "probe/", wf: &ir.Workflow{
			Name: "probe", Nodes: map[string]ir.Node{"nexie": node},
			Permission: mode, PermissionAllow: allow,
		}}
	}
	if why := durableMemoryWritePath(build("", nil)); why == "" {
		t.Error("a `memory:` block on a claw node with no gate resolved as no write path — the capability half reads the wrong thing")
	}
	if why := durableMemoryWritePath(build("deny", []string{"Read(**)", "Glob"})); why != "" {
		t.Errorf("a `memory:` block under `permission: deny` with a writer-free allow list was credited as a write path: %s", why)
	}
	if why := durableMemoryWritePath(build("deny", writers)); why == "" {
		t.Errorf("a `memory:` block whose allow list names %v was read as no write path", writers)
	}
}

// TestTheCapabilityHalfAsksTheGatePerRoute is the twin's own witness for
// iterion#2881: a writer HELD on one route and REFUSED there by the gate is
// no write path, whatever another route's spelling of the same concept
// survives. The shape is the ticket's: a claude_code node whose list names
// no writer, the auto-memory grant waiting on the claw fallback, and a gate
// whose allow glob covers the claude_code spelling (`Write*`) and not the
// claw one (`write_file`). Asked over the UNION of the routes' spellings the
// gate leaves `Write` and the bundle is credited a path no run can take;
// asked per route, the claw route's own spelling is refused and the verdict
// is none.
func TestTheCapabilityHalfAsksTheGatePerRoute(t *testing.T) {
	t.Parallel()

	node := &ir.AgentNode{
		BaseNode:          ir.BaseNode{ID: "nexie"},
		LLMFields:         ir.LLMFields{Backend: "claude_code"},
		Tools:             []string{"read_file"},
		AutoMemory:        "on",
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionNone},
		Fallbacks:         []ir.Fallback{{Name: "alt", Backend: delegate.BackendClaw, Model: "provider/model-a"}},
	}
	wf := &ir.Workflow{
		Name: "probe", Nodes: map[string]ir.Node{"nexie": node},
		Permission: "deny", PermissionAllow: []string{"Write*"},
	}
	sw := shippedWorkflow{prefix: "probe/", wf: wf}

	// The precondition, measured rather than assumed: the claw route holds
	// the granted writer and the gate refuses THAT route's spelling of it —
	// or the verdict below proves nothing.
	held := bundlelint.NodeDurableWriter(node, &ir.Workflow{Name: "probe", Nodes: map[string]ir.Node{"nexie": node}})
	if held == "" {
		t.Fatal("without the gate the bundle holds no durable writer — the auto-memory grant this case exercises is gone, and the refusal below would prove nothing")
	}
	if bundlelint.PermissionGateLeavesOnRoute(node, wf, delegate.BackendClaw, held) {
		t.Fatalf("the gate leaves %q on the claw route — the refusal this case is about is not exercised", held)
	}

	if why := durableMemoryWritePath(sw); why != "" {
		t.Errorf("a writer refused by the gate on the route that holds it was credited through another route's surviving spelling: %s", why)
	}
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
//
// EMPTY, and that is the point: its one row pardoned the session-continuity
// family for being rooted in bots/whats-next, which cannot serve the tools its
// source page teaches. Rooting the family on bots/evolve
// (internal/skillshare/families.go, iterion#2832) removed the exemption
// instead of encoding it, which is what let the same rule be carried into
// `iterion validate` (C274) — a universal lint has nowhere to keep a pardon
// for one bundle. reportUnearnedExemptions audits what survives.
var memoryRootExemptions = map[string]struct {
	reason string
	marker string
}{}

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
	checked := 0
	usedRootExemptions := map[string]bool{}
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
			usedRootExemptions[f.Name] = true
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
	reportUnearnedExemptions(t, "memoryRootExemptions", rootExemptionReasons(), usedRootExemptions)
	if len(unrooted) > 0 {
		sort.Strings(unrooted)
		t.Errorf("%d shared memory skill(s) rooted where they cannot be served (iterion#2773):\n  %s", len(unrooted), strings.Join(unrooted, "\n  "))
	}
	t.Logf("%d family source(s) teaching the memory tools, %d exempt (of %d exemptions declared)",
		checked, len(usedRootExemptions), len(memoryRootExemptions))
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

// reportUnearnedExemptions names every declared exemption the walk did not
// read. A row nobody earns is not a harmless leftover: it is a pardon the
// tree no longer needs, and leaving it would let the next real promise hide
// behind it. The rule is the ratchet both tables carry — the list can only
// SHRINK without a review — and it is why an EMPTY table is the healthy
// state rather than a disabled guard: with no rows, there is nothing to earn
// and nothing pardoned.
//
// Each table's reasons are passed in so the message can say what the row was
// kept FOR, which is the half of an exemption a reader needs in order to
// retire it.
func reportUnearnedExemptions(t *testing.T, table string, reasons map[string]string, used map[string]bool) {
	t.Helper()
	stale := unearnedExemptions(reasons, used)
	if len(stale) == 0 {
		return
	}
	t.Errorf("%d row(s) of %s were not earned by this walk — the page moved, its marker moved, or the exemption is no longer needed; delete the row (the list may only shrink) or restore what it rests on:\n  %s",
		len(stale), table, strings.Join(stale, "\n  "))
}

// unearnedExemptions is the auditor's predicate, sorted, separated from the
// reporting so it can be held to examples itself: an auditor nobody ever saw
// name a row is an assertion about an empty map, which both tables now carry.
func unearnedExemptions(reasons map[string]string, used map[string]bool) []string {
	var stale []string
	for key, reason := range reasons {
		if used[key] {
			continue
		}
		stale = append(stale, fmt.Sprintf("%s (kept for: %s)", key, reason))
	}
	sort.Strings(stale)
	return stale
}

// TestTheExemptionAuditorNamesOnlyUnearnedRows holds the auditor to examples,
// because the two tables it guards are EMPTY: with no rows, the walks above
// exercise it over nothing, and a broken auditor would read as a clean tree.
// The day a row returns, this is what says the audit still works.
func TestTheExemptionAuditorNamesOnlyUnearnedRows(t *testing.T) {
	t.Parallel()

	if got := unearnedExemptions(map[string]string{}, map[string]bool{}); len(got) != 0 {
		t.Errorf("an empty table reported %v — an empty exemption list pardons nothing and must audit clean", got)
	}
	reasons := map[string]string{
		"a.md": "the family source, refused by its own note",
		"b.md": "a generated card about another bundle",
	}
	if got := unearnedExemptions(reasons, map[string]bool{"a.md": true, "b.md": true}); len(got) != 0 {
		t.Errorf("rows the walk earned were reported stale: %v", got)
	}
	got := unearnedExemptions(reasons, map[string]bool{"b.md": true})
	if len(got) != 1 || !strings.Contains(got[0], "a.md") || !strings.Contains(got[0], "refused by its own note") {
		t.Errorf("the unearned row is not named with its reason: %v", got)
	}
	// Sorted: two stale rows must read the same on every run.
	if got := unearnedExemptions(reasons, nil); len(got) != 2 || !strings.HasPrefix(got[0], "a.md") {
		t.Errorf("the stale rows are not in a stable order: %v", got)
	}
}

// briefExemptionReasons is briefPromiseExemptions keyed for the auditor.
func briefExemptionReasons() map[string]string {
	out := make(map[string]string, len(briefPromiseExemptions))
	for key, ex := range briefPromiseExemptions {
		out[key] = ex.reason
	}
	return out
}

// rootExemptionReasons is memoryRootExemptions keyed for the auditor.
func rootExemptionReasons() map[string]string {
	out := make(map[string]string, len(memoryRootExemptions))
	for key, ex := range memoryRootExemptions {
		out[key] = ex.reason
	}
	return out
}
