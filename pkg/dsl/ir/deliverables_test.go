package ir

import (
	"strings"
	"testing"

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
