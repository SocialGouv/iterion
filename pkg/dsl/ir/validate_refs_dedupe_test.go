package ir

import (
	"fmt"
	"strings"
	"testing"
)

// #2779: a prompt read by N nodes had each of its R references validated
// once PER READER — R × N diagnostics, each naming its reader in the
// message — so a valid bot of a shared prompt and many readers answered
// megabytes of validate JSON and could OOM the route. The validation is
// per-reader (the verdict reads the reader's schema and its place in the
// graph), but the FINDING is shared when the verdict is: one diagnostic per
// reference and distinct verdict, the readers named in it.
func TestASharedPromptsReferencesDrawOneDiagnosticPerVerdict(t *testing.T) {
	const refs, readers = 100, 100
	var b strings.Builder
	b.WriteString("prompt p:\n")
	for i := 0; i < refs; i++ {
		fmt.Fprintf(&b, "  {{vars.v%d}}\n", i)
	}
	b.WriteString("\n")
	for i := 0; i < readers; i++ {
		fmt.Fprintf(&b, "agent a%d:\n  model: \"m\"\n  system: p\n\n", i)
	}
	b.WriteString("workflow w:\n  entry: a0\n")
	for i := 0; i+1 < readers; i++ {
		fmt.Fprintf(&b, "  a%d -> a%d\n", i, i+1)
	}
	fmt.Fprintf(&b, "  a%d -> done\n", readers-1)
	cr := compileText(t, b.String())
	if n := countByCode(cr, string(DiagUndeclaredVar)); n != refs {
		t.Fatalf("%d C033 for %d references read by %d nodes: want one per reference with its readers named, not one per reader (%d)", n, refs, readers, refs*readers)
	}
	d := diagFor(cr, DiagUndeclaredVar)
	if d == nil {
		t.Fatal("no C033 at all")
	}
	// The finding is attributed to its readers by name — the first ones in
	// full, the rest counted.
	if !strings.Contains(d.Message, `prompt "p"`) || !strings.Contains(d.Message, `"a0"`) || !strings.Contains(d.Message, `"a1"`) {
		t.Errorf("the merged finding does not name its readers: %.240s", d.Message)
	}
	if !strings.Contains(d.Message, "and 68 more") {
		t.Errorf("100 readers: the first are named, the rest counted: %.120s…", d.Message)
	}
	// The studio locates the finding on a reader: the first one's id.
	if d.NodeID != "a0" {
		t.Errorf("NodeID = %q, want the first reader", d.NodeID)
	}
}

// A prompt read by ONE node is said as ever: the reader named in the
// singular, no merge in play.
func TestASingleReadersFindingIsSaidAsEver(t *testing.T) {
	src := "prompt p:\n  {{vars.v}}\n\nagent a:\n  model: \"m\"\n  system: p\n\nworkflow w:\n  entry: a\n  a -> done\n"
	cr := compileText(t, src)
	d := diagFor(cr, DiagUndeclaredVar)
	if d == nil {
		t.Fatalf("no C033: %+v", cr.Diagnostics)
	}
	want := `prompt "p" (node "a"): reference {{vars.v}} targets undeclared variable "v"`
	if d.Message != want {
		t.Errorf("message = %q, want %q", d.Message, want)
	}
}

// The verdict is the reader's: the SAME reference validates clean against
// one reader's schema and refuses against another's — the refusal names the
// reader it refuses, never a reader it does not.
func TestAMergedFindingNamesOnlyTheReadersItFails(t *testing.T) {
	src := "schema s:\n  x: string\n\nschema t:\n  y: string\n\nprompt p:\n  {{input.x}}\n\nagent ok1:\n  model: \"m\"\n  input: s\n  system: p\n\nagent bad1:\n  model: \"m\"\n  input: t\n  system: p\n\nagent bad2:\n  model: \"m\"\n  input: t\n  system: p\n\nworkflow w:\n  entry: ok1\n  ok1 -> bad1\n  bad1 -> bad2\n  bad2 -> done\n"
	cr := compileText(t, src)
	if n := countByCode(cr, string(DiagInputFieldNotInSchema)); n != 1 {
		t.Fatalf("%d C034, want one — the two readers it fails, merged", n)
	}
	d := diagFor(cr, DiagInputFieldNotInSchema)
	if !strings.Contains(d.Message, `"bad1"`) || !strings.Contains(d.Message, `"bad2"`) {
		t.Errorf("the finding does not name both failing readers: %s", d.Message)
	}
	if strings.Contains(d.Message, `"ok1"`) {
		t.Errorf("the finding names a reader it does not fail: %s", d.Message)
	}
}

// The merged finding names its readers quoted where the sentence quotes a
// name — the location, a %q — and bare where it does not: an edge list's
// `src -> dst`.
func TestAMergedFindingNamesItsReadersBareInAnEdgeList(t *testing.T) {
	src := "schema s:\n  x: string\n\nprompt p:\n  {{input.x}}\n\nagent a:\n  model: \"m\"\nagent z1:\n  model: \"m\"\n  input: s\n  system: p\n\nagent z2:\n  model: \"m\"\n  input: s\n  system: p\n\nworkflow w:\n  entry: a\n  a -> z1\n  a -> z2\n  z1 -> done\n  z2 -> done\n"
	cr := compileText(t, src)
	if n := countByCode(cr, string(DiagInputFieldNeverMapped)); n != 1 {
		t.Fatalf("%d C308, want one — the two readers it fails, merged\ndiagnostics: %+v", n, cr.Diagnostics)
	}
	d := diagFor(cr, DiagInputFieldNeverMapped)
	if !strings.Contains(d.Message, "(node \"z1\", \"z2\")") {
		t.Errorf("the location names the readers quoted: %s", d.Message)
	}
	if !strings.Contains(d.Message, "incoming: `a -> z1, z2`") {
		t.Errorf("the edge list names the readers bare: %s", d.Message)
	}
	if strings.Contains(d.Message, `a -> "z1"`) {
		t.Errorf("the edge list quotes a name: %s", d.Message)
	}
}

// The amplification measured in #2779: the diagnostics a valid shared-prompt
// bot draws are linear in its references, its readers changing the names a
// finding attributes, not its count — and the whole answer stays small.
func TestASharedPromptsCostIsLinear(t *testing.T) {
	const refs, readers = 200, 300
	var b strings.Builder
	b.WriteString("prompt p:\n")
	for i := 0; i < refs; i++ {
		fmt.Fprintf(&b, "  {{vars.v%d}}\n", i)
	}
	b.WriteString("\n")
	for i := 0; i < readers; i++ {
		fmt.Fprintf(&b, "agent a%d:\n  model: \"m\"\n  system: p\n\n", i)
	}
	b.WriteString("workflow w:\n  entry: a0\n")
	for i := 0; i+1 < readers; i++ {
		fmt.Fprintf(&b, "  a%d -> a%d\n", i, i+1)
	}
	fmt.Fprintf(&b, "  a%d -> done\n", readers-1)
	cr := compileText(t, b.String())
	total := 0
	for _, d := range cr.Diagnostics {
		total += len(d.Message) + len(d.Hint)
	}
	// 200 C033 (one per reference), each under the text bound: under 2 MiB
	// in all, where R × N diagnostics were 60 000.
	if len(cr.Diagnostics) > 4*refs || total > 2<<20 {
		t.Errorf("%d diagnostics, %d bytes in all for %d refs × %d readers: want O(refs)", len(cr.Diagnostics), total, refs, readers)
	}
}
