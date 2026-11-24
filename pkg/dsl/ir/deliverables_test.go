package ir

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// deliverablesHead is the declaration preamble every fixture below shares.
const deliverablesHead = `schema empty:
  ok: bool

prompt sys:
  System.

prompt usr:
  Produce the report.

`

// nodeKindFixtures is one minimal document per node kind that could host
// `deliverables:`, with %s where the property line goes. The sweep writes
// the line into each and asks the REGISTRY whether that kind accepts it: a
// kind that does must reach NodeDeliverables, a kind that does not must be
// refused by name at parse time.
var nodeKindFixtures = map[string]string{
	"agent": deliverablesHead + `agent worker:
  model: "m"
  input: empty
  output: empty
  system: sys
  user: usr
%s

workflow demo:
  entry: worker
  worker -> done
`,
	"judge": deliverablesHead + `judge gate:
  model: "m"
  input: empty
  output: empty
  system: sys
  user: usr
%s

workflow demo:
  entry: gate
  gate -> done
`,
	"tool": deliverablesHead + `tool export:
  command: "make report"
%s

workflow demo:
  entry: export
  export -> done
`,
	"human": deliverablesHead + `human approve:
  instructions: usr
%s

workflow demo:
  entry: approve
  approve -> done
`,
	"compute": deliverablesHead + `compute total:
  expr:
    sum: "1 + 1"
%s

workflow demo:
  entry: total
  total -> done
`,
}

// nodeIDOfKind names the node each fixture declares.
var nodeIDOfKind = map[string]string{
	"agent": "worker", "judge": "gate", "tool": "export",
	"human": "approve", "compute": "total",
}

// kindsAcceptingDeliverables reads the registry rather than a memory of it.
func kindsAcceptingDeliverables() map[string]bool {
	accepting := map[string]bool{}
	for _, k := range spec.Kinds {
		if k.Role != spec.Node {
			continue
		}
		for _, p := range k.Properties {
			if p.Name == "deliverables" {
				accepting[k.Name] = true
			}
		}
	}
	return accepting
}

// TestTheDeliverablesSurfaceIsExactlyTheKindsThatRunInAWorkspace pins the
// surface itself: agent, judge and tool run something in a workspace and can
// produce a file there; a human node's file comes from the operator's upload
// and a compute node writes nothing. A kind added to (or dropped from) the
// registry's surface without the IR wiring and a fixture reddens here.
func TestTheDeliverablesSurfaceIsExactlyTheKindsThatRunInAWorkspace(t *testing.T) {
	want := map[string]bool{"agent": true, "judge": true, "tool": true}
	got := kindsAcceptingDeliverables()
	for kind := range want {
		if !got[kind] {
			t.Errorf("the registry's %s kind no longer accepts deliverables: — wire it, or move it to the refused side of this guard", kind)
		}
	}
	for kind := range got {
		if !want[kind] {
			t.Errorf("the registry's %s kind now accepts deliverables: — add its IR field, its NodeDeliverables arm and its line here, or the declaration is dropped in silence", kind)
		}
		if _, ok := nodeKindFixtures[kind]; !ok {
			t.Errorf("no fixture declares a %s node, so this sweep proves nothing about it", kind)
		}
	}
}

// TestEveryKindThatAcceptsDeliverablesReachesTheIR compiles one node per
// accepting kind and reads the declaration back through the ONE chokepoint.
// A property wired through the parser and the AST but forgotten in the
// compiler is silent — the collector simply never sees it — and this is
// where that silence reddens.
func TestEveryKindThatAcceptsDeliverablesReachesTheIR(t *testing.T) {
	for kind := range kindsAcceptingDeliverables() {
		t.Run(kind, func(t *testing.T) {
			src := strings.Replace(nodeKindFixtures[kind], "%s",
				`  deliverables: ["report.md", "out/annex.html"]`, 1)
			w := mustCompile(t, src)
			n := w.Nodes[nodeIDOfKind[kind]]
			if n == nil {
				t.Fatalf("%s node %q missing from the compiled workflow", kind, nodeIDOfKind[kind])
			}
			got := NodeDeliverables(n)
			if strings.Join(got, ",") != "report.md,out/annex.html" {
				t.Fatalf("NodeDeliverables(%s) = %v, want [report.md out/annex.html]", kind, got)
			}
		})
	}
}

// TestAKindThatDoesNotAcceptDeliverablesRefusesItByName holds the other
// half: the refusal is TYPED (a parse error naming the property), never a
// value the compiler drops without a word.
func TestAKindThatDoesNotAcceptDeliverablesRefusesItByName(t *testing.T) {
	accepting := kindsAcceptingDeliverables()
	for kind, fixture := range nodeKindFixtures {
		if accepting[kind] {
			continue
		}
		t.Run(kind, func(t *testing.T) {
			src := strings.Replace(fixture, "%s", `  deliverables: ["report.md"]`, 1)
			pr := parsertest.Parse("x.bot", src)
			var refused bool
			for _, d := range pr.Diagnostics {
				if d.Severity == parser.SeverityError && strings.Contains(d.Error(), "deliverables") {
					refused = true
				}
			}
			if !refused {
				t.Fatalf("a %s node accepted deliverables: without a word — diagnostics: %+v", kind, pr.Diagnostics)
			}
		})
	}
}

// TestADeliverableTheRunCouldNeverCollectIsRefused — C317 on every shape the
// collector cannot resolve inside the node's workspace.
func TestADeliverableTheRunCouldNeverCollectIsRefused(t *testing.T) {
	cases := map[string]string{
		"absolute":    `["/tmp/report.md"]`,
		"escape":      `["../report.md"]`,
		"deep escape": `["out/../../report.md"]`,
		"directory":   `["out/"]`,
		"empty":       `[""]`,
		"dot":         `["."]`,
		// A template excuses nothing a rendering cannot change: no value of
		// vars.x makes `/tmp/…` relative, puts a trailing slash back, or
		// turns a leading `../` into a path inside the workspace.
		"templated absolute":  `["/tmp/{{vars.x}}.md"]`,
		"templated escape":    `["../{{vars.x}}.md"]`,
		"templated directory": `["{{vars.dir}}/"]`,
	}
	for name, list := range cases {
		t.Run(name, func(t *testing.T) {
			src := strings.Replace(nodeKindFixtures["tool"], "%s", "  deliverables: "+list, 1)
			r := compileText(t, src)
			if !hasDiagAtSeverity(r, string(DiagDeliverableNotWorkspacePath), SeverityError) {
				t.Fatalf("expected C317 as an error, got: %+v", r.Diagnostics)
			}
		})
	}
}

// TestADeliverableTheRunCanCollectIsAccepted is the control: a relative path
// in a sub-directory, and a templated entry whose shape is a RUN-time
// question, both pass — a guard that refuses them would refuse the verb.
func TestADeliverableTheRunCanCollectIsAccepted(t *testing.T) {
	for _, list := range []string{
		`["report.md"]`,
		`["out/annex.html"]`,
		`["./report.md"]`,
		`["{{vars.slug}}/report.md"]`,
		`["${REPORT_DIR}/report.md"]`,
	} {
		t.Run(list, func(t *testing.T) {
			src := strings.Replace(nodeKindFixtures["tool"], "%s", "  deliverables: "+list, 1)
			r := compileText(t, src)
			if hasDiag(r.Diagnostics, DiagDeliverableNotWorkspacePath) {
				t.Fatalf("C317 refused %s, which the collector can resolve: %+v", list, r.Diagnostics)
			}
		})
	}
}

// TestTwoDeliverablesCannotShareOneDownloadName — C318. Two paths from
// different directories are served under one base name, and the run store
// refuses the second rather than overwrite the first: the collision is
// refused where renaming is free.
func TestTwoDeliverablesCannotShareOneDownloadName(t *testing.T) {
	src := strings.Replace(nodeKindFixtures["tool"], "%s",
		`  deliverables: ["out/report.md", "tmp/report.md"]`, 1)
	r := compileText(t, src)
	if !hasDiagAtSeverity(r, string(DiagDeliverableNameCollision), SeverityError) {
		t.Fatalf("expected C318 as an error, got: %+v", r.Diagnostics)
	}
	var named bool
	for _, d := range r.Diagnostics {
		if d.Code == DiagDeliverableNameCollision && strings.Contains(d.Message, "report.md") {
			named = true
		}
	}
	if !named {
		t.Fatalf("C318 does not name the colliding handle: %+v", r.Diagnostics)
	}
}

// TestTwoDeliverablesWithTheirOwnNamesAreAccepted is C318's control: same
// directory, different names, and two nodes that each declare report.md —
// the handle is per node, so that is no collision.
func TestTwoDeliverablesWithTheirOwnNamesAreAccepted(t *testing.T) {
	src := deliverablesHead + `tool export:
  command: "make report"
  deliverables: ["out/report.md", "out/annex.html"]

tool export_again:
  command: "make report"
  deliverables: ["report.md"]

workflow demo:
  entry: export
  export -> export_again
  export_again -> done
`
	r := compileText(t, src)
	if hasDiag(r.Diagnostics, DiagDeliverableNameCollision) {
		t.Fatalf("C318 fired on distinct names: %+v", r.Diagnostics)
	}
}

// TestDeliverableNameIsTheBaseName pins the handle a declared deliverable is
// served under — extension included, as G119 settled and as the download
// route addresses it (`.../deliverables/report.md`).
func TestDeliverableNameIsTheBaseName(t *testing.T) {
	cases := map[string]string{
		"report.md":          "report.md",
		"out/report.md":      "report.md",
		"./out/../report.md": "report.md",
		" report.md ":        "report.md",
		"out/":               "",
		"":                   "",
		".":                  "",
	}
	for declared, want := range cases {
		if got := DeliverableName(declared); got != want {
			t.Errorf("DeliverableName(%q) = %q, want %q", declared, got, want)
		}
	}
}

// TestRefusedDeliverablesAreSaidOnceWithTheirReasons — C317 once per node,
// however many entries it refuses: the first sixteen by excerpt with the
// reason each is refused for, the rest by count. A diagnostic per entry grew
// with the list (#2779): a 2.6 MB bot of refused entries ran the validator
// out of memory.
func TestRefusedDeliverablesAreSaidOnceWithTheirReasons(t *testing.T) {
	entries := []string{`"/tmp/report.md"`, `"../report.md"`, `"out/"`, `""`}
	for i := 0; i < 16; i++ {
		entries = append(entries, `"/tmp/`+strings.Repeat("x", i+1)+`.md"`)
	}
	src := strings.Replace(nodeKindFixtures["tool"], "%s", "  deliverables: ["+strings.Join(entries, ", ")+"]", 1)
	r := compileText(t, src)
	if got := countByCode(r, string(DiagDeliverableNotWorkspacePath)); got != 1 {
		t.Fatalf("C317 count = %d for 20 refused entries, want 1 (once per node)\n%+v", got, r.Diagnostics)
	}
	msg := messageOf(r, DiagDeliverableNotWorkspacePath)
	for _, want := range []string{`"/tmp/report.md" (absolute)`, `"../report.md" (climbs out of the workspace)`, `"out/" (a directory)`, `"" (empty)`, "and 4 more"} {
		if !strings.Contains(msg, want) {
			t.Errorf("C317 does not say %s:\n%s", want, msg)
		}
	}
}

// TestAListOfRefusedDeliverablesDrawsOneDiagnostic: the count of a list is
// what the read budget does not bound — a hundred thousand refused entries,
// and as many names each served twice, draw one C317 and one C318, and the
// messages stay under the diagnostic bound.
func TestAListOfRefusedDeliverablesDrawsOneDiagnostic(t *testing.T) {
	var b strings.Builder
	b.WriteString(`  deliverables: [`)
	for i := 0; i < 100000; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, `"/", "a/%d.md", "b/%d.md"`, i, i)
	}
	b.WriteString("]")
	src := strings.Replace(nodeKindFixtures["tool"], "%s", b.String(), 1)
	r := compileText(t, src)
	for _, code := range []DiagCode{DiagDeliverableNotWorkspacePath, DiagDeliverableNameCollision} {
		if got := countByCode(r, string(code)); got != 1 {
			t.Fatalf("%s count = %d for a hundred thousand entries, want 1", code, got)
		}
		if msg := messageOf(r, code); len(msg) > expr.MaxDiagnosticText {
			t.Fatalf("%s message is %d bytes, past the bound", code, len(msg))
		}
	}
	// Sixteen quoted entries in all — eight names of two — then the count.
	if msg := messageOf(r, DiagDeliverableNameCollision); !strings.Contains(msg, "and 99992 more names") || !strings.Contains(msg, "declares 200000 deliverables served under 100000 shared names:") {
		t.Fatalf("C318 does not count the names past its quoted entries, or its totals:\n%s", msg)
	}
}

// TestACollisionListStaysUnderTheBoundWithItsTotals: C318's quoted entries
// are bounded across the whole message — not per name — so a few names with
// many long entries still leave the counts and the remedy inside the
// diagnostic bound rather than cut off by it.
func TestACollisionListStaysUnderTheBoundWithItsTotals(t *testing.T) {
	entries := make([]string, 0, 64)
	for n := 0; n < 4; n++ {
		for i := 0; i < 16; i++ {
			entries = append(entries, fmt.Sprintf(`"%s/%d/n%d.md"`, strings.Repeat("directory-with-a-long-name/", 3), i, n))
		}
	}
	src := strings.Replace(nodeKindFixtures["tool"], "%s", "  deliverables: ["+strings.Join(entries, ", ")+"]", 1)
	msg := messageOf(compileText(t, src), DiagDeliverableNameCollision)
	if len(msg) > expr.MaxDiagnosticText/2 {
		t.Fatalf("C318 is %d bytes for 64 entries under 4 names", len(msg))
	}
	for _, want := range []string{"declares 64 deliverables served under 4 shared names:", "and 3 more names", "; rename all but one of the files under each name"} {
		if !strings.Contains(msg, want) {
			t.Errorf("C318 lost %q to the bound:\n%s", want, msg)
		}
	}
	// One name, two entries: the plain shape reads as before.
	src = strings.Replace(nodeKindFixtures["tool"], "%s", `  deliverables: ["out/report.md", "tmp/report.md"]`, 1)
	if msg := messageOf(compileText(t, src), DiagDeliverableNameCollision); !strings.Contains(msg, `declares 2 deliverables served under one shared name: "out/report.md" and "tmp/report.md" served as "report.md"`) {
		t.Fatalf("C318 plain shape:\n%s", msg)
	}
}

// TestACleanNodeBesideACollidingOneDrawsNothing: the per-node state of the
// two rules starts afresh at each node — a clean node declared after a
// colliding one draws neither C317 nor C318.
func TestACleanNodeBesideACollidingOneDrawsNothing(t *testing.T) {
	src := deliverablesHead + `tool export:
  command: "make report"
  deliverables: ["/tmp/x.md", "out/report.md", "tmp/report.md"]

tool export_again:
  command: "make report"
  deliverables: ["annex.html"]

workflow demo:
  entry: export
  export -> export_again
  export_again -> done
`
	r := compileText(t, src)
	for _, d := range r.Diagnostics {
		if (d.Code == DiagDeliverableNameCollision || d.Code == DiagDeliverableNotWorkspacePath) && d.NodeID != "export" {
			t.Fatalf("%s on the clean node %q: %s", d.Code, d.NodeID, d.Message)
		}
	}
	if countByCode(r, string(DiagDeliverableNameCollision)) != 1 || countByCode(r, string(DiagDeliverableNotWorkspacePath)) != 1 {
		t.Fatalf("want one C317 and one C318 on the colliding node, got %+v", r.Diagnostics)
	}
}

// TestEveryRefusedShapeIsSaidWithItsReason: each shape C317 refuses is named
// by the word for it, the text trimmed first — a leading blank hides
// neither an absolute path nor a climb.
func TestEveryRefusedShapeIsSaidWithItsReason(t *testing.T) {
	for entry, want := range map[string]string{
		`"/tmp/report.md"`:  `"/tmp/report.md" (absolute)`,
		`" /tmp/report.md"`: `" /tmp/report.md" (absolute)`,
		`"../report.md"`:    `"../report.md" (climbs out of the workspace)`,
		`".."`:              `".." (climbs out of the workspace)`,
		`"out/"`:            `"out/" (a directory)`,
		`"."`:               `"." (a directory)`,
		`"out/.."`:          `"out/.." (a directory)`,
		`""`:                `"" (empty)`,
		`"  "`:              `"  " (empty)`,
	} {
		src := strings.Replace(nodeKindFixtures["tool"], "%s", "  deliverables: ["+entry+"]", 1)
		msg := messageOf(compileText(t, src), DiagDeliverableNotWorkspacePath)
		if !strings.Contains(msg, want) || !strings.Contains(msg, "declares a deliverable that is not a workspace-relative file path: ") {
			t.Errorf("C317 for %s: want %s in:\n%s", entry, want, msg)
		}
	}
}

// TestARepeatedDeliverableIsSaidAsARepeat: two entries spelling the same
// file collide on their name, and the remedy is to drop one, not to rename
// it — C318 says so instead of asking for a rename that cannot happen.
func TestARepeatedDeliverableIsSaidAsARepeat(t *testing.T) {
	src := strings.Replace(nodeKindFixtures["tool"], "%s",
		`  deliverables: ["report.md", "./report.md", "report.md"]`, 1)
	r := compileText(t, src)
	msg := messageOf(r, DiagDeliverableNameCollision)
	if !strings.Contains(msg, `the file "report.md" is listed 3 times`) || strings.Contains(msg, "served as") || !strings.HasSuffix(msg, "; drop the repeated entries") {
		t.Fatalf("C318 does not say the entry is repeated, with the remedy of a repeat:\n%s", msg)
	}
	// A file listed twice beside a real collision: both said, the remedy
	// names both gestures.
	src = strings.Replace(nodeKindFixtures["tool"], "%s",
		`  deliverables: ["r.md", "./r.md", "a/r.md"]`, 1)
	msg = messageOf(compileText(t, src), DiagDeliverableNameCollision)
	if !strings.Contains(msg, `"r.md" and "a/r.md" (1 repeated) served as "r.md"`) || !strings.HasSuffix(msg, "rename all but one of the files under each name, and drop the repeated entries") {
		t.Fatalf("C318 does not say the repeat beside the collision:\n%s", msg)
	}
	// A long repeated path is quoted by excerpt like every other entry.
	long := strings.Repeat("d/", 50000) + "r.md"
	src = strings.Replace(nodeKindFixtures["tool"], "%s",
		`  deliverables: ["`+long+`", "`+long+`"]`, 1)
	if msg = messageOf(compileText(t, src), DiagDeliverableNameCollision); len(msg) > 512 || !strings.Contains(msg, "is listed 2 times") {
		t.Fatalf("C318 quotes a repeated path whole (%d bytes):\n%.200s", len(msg), msg)
	}
}

// TestCollidingDeliverablesAreQuotedAroundTheirDifference: two long paths
// that share their first seventy runes read alike by plain excerpt; C318
// quotes them where they differ, so the author sees two paths.
func TestCollidingDeliverablesAreQuotedAroundTheirDifference(t *testing.T) {
	prefix := strings.Repeat("reports/2026-q3/security-and-compliance/", 2)
	src := strings.Replace(nodeKindFixtures["tool"], "%s",
		`  deliverables: ["`+prefix+`alpha/report.md", "`+prefix+`beta/report.md"]`, 1)
	r := compileText(t, src)
	msg := messageOf(r, DiagDeliverableNameCollision)
	for _, want := range []string{"alpha/report.md", "beta/report.md"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("C318 does not quote the paths where they differ (%s):\n%s", want, msg)
		}
	}
}
