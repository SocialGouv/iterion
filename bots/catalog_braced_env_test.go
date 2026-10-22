package bots

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// The catalogue floor for the C188 class (ADR-130): no shipped workflow holds
// a shell-bound tool body that assigns a name and THEN reads it back in a
// braced form the engine expands (`${NAME}` or `${NAME:-default}`). On such a
// body the engine answers the read from its own environment before any shell
// sets the value, so the assignment is dead — the run-id guard of
// sec-audit-source refused every production deep-scan pass that way for five
// days while the harness stayed green (#1473 repaired that bot, the C188
// warning in pkg/dsl/ir generalises the check, and this floor keeps the
// catalogue at zero findings so a new hit is a decision to convert, not a
// silent ship).
//
// The verdict is THE LINT'S, not a lookalike: every shipped workflow is loaded
// the way a launch loads it (`unit.LoadDir`, imports merged) and compiled, and
// the finding reported is the compiler's own C188 diagnostic — the same walk
// the sibling tree-noise floor runs over the compiled IR
// (tree_noise_channel_test.go). Nothing here reads a body a second time: a
// floor carrying its own writer predicate goes silent on shapes the lint
// fires on (an `export -n COUNT=1`, a `read -rp "prompt" NAME`) and reddens
// on shapes it does not (`--env FOO=${FOO}`, where nothing is assigned), and
// a gate that disagrees with the check it defends teaches authors to ignore
// both. The sec-audit-source bot is stricter on purpose — it forbids EVERY
// braced form, assigned or not — and keeps its own per-bot test
// (sec_audit_shell_safe_test.go).
//
// Mutation that reddens it: restore any of the five converted reads
// (secured-renovacy's `${COUNT}`/`${FAMILY}`, adr-cartograph's
// `${PORT}`/`${ID_SHORT}`) — the floor reopens naming the file, the tool node,
// the body line and the name.
func TestCatalogHasNoBracedReadOfItsOwnAssignment(t *testing.T) {
	targets := catalogWorkflowFiles(t)
	if len(targets) == 0 {
		t.Fatal("no catalog workflows found — discovery glob likely broke")
	}
	judged := map[string]bool{} // bundle-relative tool node id → compiled
	bodies, declined := 0, 0
	for _, path := range targets {
		u := unit.LoadDir(path)
		if u.Merged == nil {
			// The parse/compile floor reports the parse failure itself; say
			// it here too, because an unreadable file is a file this floor
			// did not judge.
			t.Errorf("%s: no merged file — this floor could not judge it (see TestCatalogBotsParseAndCompileClean)", path)
			continue
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			t.Errorf("%s: compile produced no workflow — this floor could not judge it (see TestCatalogBotsParseAndCompileClean)", path)
			continue
		}
		for id, n := range cr.Workflow.Nodes {
			tn, ok := n.(*ir.ToolNode)
			if !ok {
				continue
			}
			judged[filepath.Dir(path)+"/"+id] = true
			j, d := shellBoundBodies(tn)
			bodies += j
			declined += d
		}
		// The compiler walks its nodes from a map, so the findings of one
		// file arrive in no fixed order: sorted, a failure reads the same
		// twice.
		var found []string
		for _, d := range cr.Diagnostics {
			if d.Code != ir.DiagBracedEnvDefeatsAssignment {
				continue
			}
			// The body line travels in the message (the AST keeps no
			// per-property span); the node's own declaration position is
			// what names the FILE a fragment's body really lives in.
			at := path
			if d.File != "" && d.Line > 0 {
				at = fmt.Sprintf("%s (node declared at %s:%d)", path, d.File, d.Line)
			}
			found = append(found, fmt.Sprintf("%s: %s", at, d.Message))
		}
		sort.Strings(found)
		for _, f := range found {
			t.Errorf("%s\n\tC188 over the catalogue must stay at zero: apply the fix the diagnostic above names, or record the baseline here with the reason", f)
		}
	}
	// A floor over a corpus the walk stopped reading passes everything: the
	// shell-bound bodies the walk reached, and the fragments' tool nodes it
	// reached through the mains that import them. The quota is on the REACH
	// (judged plus declining) because a bundle that declares
	// `env_expansion: none` widely is still a corpus this walk read, while
	// the judged count alone is what the lint's verdict above covers — a
	// declining body is one nobody judged, and the two are reported apart so
	// neither number can be read as the other.
	if bodies+declined < 120 {
		t.Fatalf("the walk reached %d shell-bound tool bodies over %d workflows (%d judged by the lint, %d declining the expansion) — a read this thin is a walk that stopped reading, and this floor is now guarding nothing (the corpus carries 157 judged today)", bodies+declined, len(targets), bodies, declined)
	}
	for _, p := range fragmentFiles(t, "*/lib/*.bot", 5) {
		bundle := filepath.Dir(filepath.Dir(p))
		for _, name := range fragmentToolNodeNames(t, p) {
			if !judged[bundle+"/"+name] {
				t.Errorf("%s declares the tool node %q that no compiled workflow of %s carries — a fragment no main imports is a body this floor never judges", p, name, bundle)
			}
		}
	}
}

// shellBoundBodies counts one tool node's shell-bound bodies, split by what
// the lint did with them: judged, or declining the expansion
// (`env_expansion: none`, ADR-130 — the lint skips such a node entirely, so
// its bodies are bodies nobody judged and counting them as judged would let
// the quota above vouch for a verdict that was never reached). The predicate
// is the IR's own, the one the lint and the engine ask, so the split cannot
// drift from the gate it mirrors.
func shellBoundBodies(tn *ir.ToolNode) (judged, declined int) {
	for _, b := range []struct {
		text  string
		shell bool
	}{
		{tn.Command, true},
		{tn.Script, tn.Language == "" || tn.Language == "sh" || tn.Language == "bash"},
		{tn.Postcondition, true},
	} {
		if b.text == "" || !b.shell {
			continue
		}
		if tn.ExpandsBracedEnv() {
			judged++
			continue
		}
		declined++
	}
	return judged, declined
}

// The census is the quota's witness, so it answers for the two halves apart:
// a declining node's bodies are reached and not judged. Red when
// shellBoundBodies counts a declining body as judged — which is what let the
// floor's "157 shell-bound tool bodies the lint judged" cover bodies the lint
// skipped.
func TestTheCensusSeparatesTheBodiesTheLintSkipped(t *testing.T) {
	node := &ir.ToolNode{
		Command:       "COUNT=1; echo \"${COUNT}\"",
		Script:        "RUN_ID=x; test -n \"${RUN_ID}\"",
		Language:      "sh",
		Postcondition: "test -n \"${RUN_ID:-}\"",
	}
	if judged, declined := shellBoundBodies(node); judged != 3 || declined != 0 {
		t.Errorf("an undeclared node: judged=%d declined=%d, want 3 and 0", judged, declined)
	}
	node.EnvExpansion = ir.EnvExpansionNone
	if judged, declined := shellBoundBodies(node); judged != 0 || declined != 3 {
		t.Errorf("a node declaring `env_expansion: none`: judged=%d declined=%d, want 0 and 3 — the lint judges none of its bodies", judged, declined)
	}
	// A non-shell script body is out of the lint's reach whatever the node
	// declares, so it is in neither count.
	node.EnvExpansion = ""
	node.Language = "js"
	if judged, declined := shellBoundBodies(node); judged != 2 || declined != 0 {
		t.Errorf("a `js` script body: judged=%d declined=%d, want 2 and 0", judged, declined)
	}
}

// fragmentFiles lists the imported fragments of the catalogue's bundles, with
// the floor that says the glob still matches them: a fragment holds tool
// bodies like any main, and is judged through the workflows that import it.
func fragmentFiles(t *testing.T, glob string, least int) []string {
	t.Helper()
	paths, err := filepath.Glob(glob)
	if err != nil {
		t.Fatalf("glob %q: %v", glob, err)
	}
	if len(paths) < least {
		t.Fatalf("glob %q matched %d file(s), want at least %d — the bundles' library fragments stopped matching, and their coverage is now unchecked", glob, len(paths), least)
	}
	return paths
}

// fragmentToolNodeNames parses one fragment alone and returns the tool nodes
// it declares — the names a compiled workflow of its bundle must carry.
func fragmentToolNodeNames(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	pr := parser.Parse(path, string(data))
	if pr.File == nil {
		t.Errorf("%s: parser produced no file — its tool nodes are unchecked here", path)
		return nil
	}
	names := make([]string, 0, len(pr.File.Tools))
	for _, td := range pr.File.Tools {
		names = append(names, td.Name)
	}
	return names
}
