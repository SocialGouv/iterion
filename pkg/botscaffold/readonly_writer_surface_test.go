package botscaffold

import (
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// scaffoldedWorkflows compiles every workflow the scaffold WROTE, keyed by
// its bundle-relative path — not `main.bot` alone. A shape may ship an
// entrypoint of its own beside the main: `per-ticket-subbots` writes
// `worker.bot`, a workflow carrying its own agent node, which the launch
// compiles as a bare file (runview/subbot.go). A corpus predicate read off
// the main would pass over every node such a child declares.
//
// The set of files is Scaffold's own report of what it wrote (Result.Files),
// so a shape that grows a second entrypoint is judged the day it lands,
// without this helper naming any shape.
//
// A fragment under lib/ is left to the main that imports it, which is how
// Scaffold's own compile guard reads one (scaffold.go): unit.LoadDir follows
// the import, so a fragment's nodes ARE judged — once, through that main.
func scaffoldedWorkflows(t *testing.T, spec Spec) map[string]*ir.Workflow {
	t.Helper()
	dir := filepath.Join(t.TempDir(), spec.Slug)
	res, err := Scaffold(dir, spec)
	if err != nil {
		t.Fatalf("Scaffold(%s): %v", spec.Shape, err)
	}
	b, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatalf("OpenDir(%s): %v", spec.Shape, err)
	}
	out := map[string]*ir.Workflow{}
	for _, abs := range res.Files {
		rel, err := filepath.Rel(dir, abs)
		if err != nil {
			t.Fatalf("Rel(%s, %s): %v", dir, abs, err)
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasSuffix(rel, ".bot") || strings.HasPrefix(rel, unit.FragmentDir+"/") {
			continue
		}
		u := unittest.Whole(unit.LoadDir(abs))
		if len(u.Diagnostics) != 0 {
			t.Fatalf("%s: %s: parse: %v", spec.Shape, rel, u.Diagnostics)
		}
		if rel == "main.bot" {
			// Only the main is promoted to its bundle, so only its prompts
			// come from prompts/*.md; a child is compiled with nothing else
			// in scope, the way its launch compiles it.
			if err := runview.MergeBundlePrompts(u.Merged, b); err != nil {
				t.Fatalf("%s: MergeBundlePrompts: %v", spec.Shape, err)
			}
		}
		cr := ir.Compile(u.Merged)
		for _, d := range cr.Diagnostics {
			if d.Severity == ir.SeverityError && d.Code != ir.DiagMissingModelOrBackend {
				t.Errorf("%s: %s: compile: %s", spec.Shape, rel, d.Error())
			}
		}
		if cr.Workflow == nil {
			t.Fatalf("%s: %s: no workflow", spec.Shape, rel)
		}
		out[rel] = cr.Workflow
	}
	return out
}

// TestNoScaffoldedNodeDeclaresReadonlyWithAWritingTool is the gallery half of
// #1816's corpus rule: `readonly: true` and a `tools:` list naming a writer
// contradict each other, the flag wins wherever it is read — codex runs a
// read-only OS sandbox, pi replaces the tool list with an allowlist of
// readers, claude_code withholds the writing natives from both spawns — and
// on the three backends that ignore it the writer runs inside a workspace the
// engine admitted the node into as non-mutating. A node whose deliverable is
// a written file drops `readonly:`, and its fan-out admission goes with the
// flag.
//
// Asserted HERE and not in bots/: the gallery ships `.bot.tmpl` Go templates,
// which the catalogue walk cannot parse, and this is the root every
// `iterion new` copies from — the templates carry `readonly:` today and a
// shape that grew a `tools:` list around it would reach every new bot with
// nothing red. The bundle-side guard is
// bots.TestNoShippedWorkflowDeclaresReadonlyWithAWritingTool; the two share
// the predicate, not the corpus.
//
// Read over EVERY workflow each shape writes (scaffoldedWorkflows), not its
// main.bot alone: a child entrypoint declares nodes of its own, and the
// bundle-side guard learned the same lesson on bundleReadonlyFlags.
func TestNoScaffoldedNodeDeclaresReadonlyWithAWritingTool(t *testing.T) {
	// The fold this guard depends on: a writer spelled `write_file` or
	// `apply_patch` must classify like `write` and `edit`, or a template
	// naming one would read as declaring no writer at all.
	for spelling, want := range map[string]string{
		"write_file": "write", "apply_patch": "edit", "notebook_edit": "notebookedit",
	} {
		if got := toolcatalog.CanonicalToolName(spelling); got != want {
			t.Fatalf("toolcatalog.CanonicalToolName(%q) = %q, want %q: this guard classifies a declared writer through that fold, and a row that moved makes it read a writing template as read-only", spelling, got, want)
		}
	}

	var offenders []string
	nodes, readonlyNodes, writerNodes, files, children := 0, 0, 0, 0, 0
	for _, tpl := range Templates() {
		spec := tpl.Spec
		spec.Slug = "roparity-" + tpl.ID
		spec.DisplayName = "Readonly parity"
		workflows := scaffoldedWorkflows(t, spec)
		rels := make([]string, 0, len(workflows))
		for rel := range workflows {
			rels = append(rels, rel)
		}
		sort.Strings(rels)
		for _, rel := range rels {
			files++
			if rel != "main.bot" {
				children++
			}
			wf := workflows[rel]
			ids := make([]string, 0, len(wf.Nodes))
			for id := range wf.Nodes {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				llm, ok := wf.Nodes[id].(ir.LLMNode)
				if !ok {
					continue
				}
				nodes++
				var writers []string
				for _, name := range llm.GetTools() {
					if slices.Contains(toolcatalog.WritingCanonicals(), toolcatalog.CanonicalToolName(name)) {
						writers = append(writers, name)
					}
				}
				if len(writers) > 0 {
					writerNodes++
				}
				if !llm.GetLLMFields().Readonly {
					continue
				}
				readonlyNodes++
				if len(writers) == 0 {
					continue
				}
				offenders = append(offenders, fmt.Sprintf("%s %s %s (writers %v)", tpl.ID, rel, id, writers))
			}
		}
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d scaffolded node(s) declare `readonly: true` together with a writing tool, which no backend keeps — and this is the tree every new bot is born from, so the shape would ship to every `iterion new`; drop `readonly:` where the deliverable is a written file, or drop the writer:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	// FIVE floors, one per SELECTION, because the offender is the
	// INTERSECTION of two predicates over a walk: a total over every
	// scaffolded LLM node stays green while the flag read answers nothing, a
	// walk that lost the child entrypoints stays green over the nodes it no
	// longer reads, and a `tools:` read that resolved to nothing stays green
	// with a writing template in the gallery — measured, with `GetTools()`
	// returning nil and the offending shape planted: the other four floors
	// cleared and the offender went unreported. 16 files and 18 nodes are
	// rendered today (15 main.bot plus per-ticket-subbots' worker.bot),
	// carrying 3 `readonly:` nodes and 1 that declares a writer — each floor
	// sits under its count so a shape may be retired without touching these
	// lines, except the child and writer floors: ONE of each is all the
	// gallery ships, so those two are equalities in practice.
	if files < 13 {
		t.Fatalf("the walk compiled %d scaffolded workflow file(s), want at least 13: Scaffold's file report or the compile broke, and this guard is now green over an unexamined tree", files)
	}
	if children < 1 {
		t.Fatalf("the walk compiled %d scaffolded workflow file(s) other than main.bot, want at least 1 (per-ticket-subbots writes worker.bot): the walk is back to reading the mains alone, and every node a child entrypoint declares is unexamined", children)
	}
	if nodes < 12 {
		t.Fatalf("the walk reached %d scaffolded agent/judge node(s), want at least 12: the gallery render broke, and this guard is now green over an unexamined tree", nodes)
	}
	if readonlyNodes < 2 {
		t.Fatalf("the walk read `readonly: true` on %d scaffolded node(s), want at least 2: the flag reading broke, and this guard now intersects an empty set", readonlyNodes)
	}
	if writerNodes < 1 {
		t.Fatalf("the walk read a declared writer on %d scaffolded node(s), want at least 1 (`async-questions` declares write_file and file_edit on its `draft` node): the `tools:` read or the fold under it broke, and this guard now intersects an empty set", writerNodes)
	}
}
