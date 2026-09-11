package botscaffold

import (
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestPerTicketSubbotsListToolFeedsTheFanOut executes the per-ticket
// shape's only shell — the placeholder ticket source — and holds its JSON
// to the CONTRACT the graph reads, not to a literal: the field the
// `fan_out_each` router iterates (`over:`) is a non-empty array, and every
// key the subbot's `with` mapping takes from the current item (`as:`) is
// present on every item. A rename on one side only turns this red.
func TestPerTicketSubbotsListToolFeedsTheFanOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shape's commands are POSIX shell")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	tpl, ok := TemplateByID("per-ticket-subbots")
	if !ok {
		t.Fatal("no per-ticket-subbots template")
	}
	spec := tpl.Spec
	spec.Slug = "tickets"
	_, w, _ := scaffoldAndCompile(t, spec)
	var list *ir.ToolNode
	for _, n := range nodesOf[*ir.ToolNode](w) {
		if n.ID == "list_tickets" {
			list = n
		}
	}
	var router *ir.RouterNode
	for _, r := range nodesOf[*ir.RouterNode](w) {
		if r.RouterMode == ir.RouterFanOutEach {
			router = r
		}
	}
	subs := nodesOf[*ir.SubbotNode](w)
	if list == nil || router == nil || len(subs) != 1 {
		t.Fatalf("want the list tool, the fan_out_each router and one subbot, got list=%v router=%v subbots=%d", list != nil, router != nil, len(subs))
	}
	if strings.Contains(list.Command, "{{") {
		t.Fatalf("the placeholder source reads a ref (%q); this test runs it verbatim", list.Command)
	}
	// The router iterates one field of the tool's output.
	if len(router.OverRefs) != 1 || router.OverRefs[0].Kind != ir.RefOutputs || len(router.OverRefs[0].Path) != 2 || router.OverRefs[0].Path[0] != list.ID {
		t.Fatalf("router over = %q, want one {{outputs.%s.<field>}} ref, got refs %+v", router.Over, list.ID, router.OverRefs)
	}
	field := router.OverRefs[0].Path[1]
	// The child's inputs are keys of the current item.
	var keys []string
	for _, m := range subs[0].With {
		for _, ref := range m.Refs {
			if ref.Kind != ir.RefOutputs || len(ref.Path) != 3 || ref.Path[0] != router.ID || ref.Path[1] != router.ItemBinding {
				t.Fatalf("with %s = %q, want {{outputs.%s.%s.<key>}}, got %+v", m.Key, m.Raw, router.ID, router.ItemBinding, ref)
			}
			keys = append(keys, ref.Path[2])
		}
	}
	if len(keys) == 0 {
		t.Fatal("the subbot's with-mapping reads nothing from the item")
	}

	out, stderr, err := shellInRepo(t.TempDir(), list.Command)
	if err != nil {
		t.Fatalf("list_tickets failed: %v\n%s", err, stderr)
	}
	var got map[string][]map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout = %q, want a JSON object of arrays: %v", out, err)
	}
	items, ok := got[field]
	if !ok || len(items) == 0 {
		t.Fatalf("stdout %q carries no non-empty %q array, which the router iterates", out, field)
	}
	for i, item := range items {
		for _, key := range keys {
			if v, ok := item[key]; !ok || v == "" {
				t.Errorf("item %d %v lacks %q, which the child's with-mapping reads", i, item, key)
			}
		}
	}
}
