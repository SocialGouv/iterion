package botscaffold

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// templateForShape is the gallery entry carrying shape.
func templateForShape(t *testing.T, shape string) Template {
	t.Helper()
	for _, tpl := range Templates() {
		if tpl.Spec.Shape == shape {
			return tpl
		}
	}
	t.Fatalf("no gallery template carries the shape %q", shape)
	return Template{}
}

// multilineValues names, per shape, the references whose value is
// MULTI-LINE at run time — a plan, a log, verifier output, a list of
// answers. Each must be a value slot: alone on its line, under a heading
// or inside a fence. Written inline (`Plan: {{outputs.plan.plan}}`) the
// value's first line would sit in the sentence and the rest run into
// whatever follows.
var multilineValues = map[string][]string{
	"campaign-loop":       {"input.previous_verify"},
	"plan-gate-implement": {"input.feedback", "outputs.plan.plan", "outputs.plan.risks", "outputs.approve.feedback"},
	"scheduled-digest":    {"outputs.collect.log"},
	"async-questions":     {"outputs.draft.draft", "outputs.gate.answers"},
}

// TestGalleryPayloadSlotsSurviveTheBlankLineDrop: the lexer drops the blank
// lines of a prompt body, so a multi-line `{{…}}` value set off by blank
// lines reaches the reader glued to the line that follows it — a plan run
// into the sentence under it, a log into the instruction after it. In every
// gallery shape a value slot (a line that is nothing but a reference) sits
// under a heading or inside a fence, the two markdown boundaries that
// survive the drop; and every reference multilineValues names IS such a
// slot, never an inline mention. Measured on the PARSED body: the text the
// model gets.
func TestGalleryPayloadSlotsSurviveTheBlankLineDrop(t *testing.T) {
	slotRe := regexp.MustCompile(`^\{\{\s*([^}\s]+)\s*\}\}$`)
	refRe := regexp.MustCompile(`\{\{\s*([^}\s]+)\s*\}\}`)
	boundary := func(line string) bool {
		line = strings.TrimSpace(line)
		// Under `dsl: 2` — every template — a blank line survives the
		// lexer and separates the slot from what follows.
		return line == "" || line == "```" || strings.HasPrefix(line, "#")
	}
	seen := 0
	for _, shape := range Shapes() {
		spec := templateForShape(t, shape).Spec
		spec.Slug = "slot"
		spec.Model, spec.Backend = "anthropic/claude-opus-4-8", "claude_code"
		dir := filepath.Join(t.TempDir(), spec.Slug)
		if _, err := Scaffold(dir, spec); err != nil {
			t.Fatalf("%s: Scaffold: %v", shape, err)
		}
		src, err := os.ReadFile(filepath.Join(dir, "main.bot"))
		if err != nil {
			t.Fatal(err)
		}
		pr := parser.Parse("main.bot", string(src))
		if len(pr.Diagnostics) != 0 {
			t.Fatalf("%s: parse: %v", shape, pr.Diagnostics)
		}
		mustBeSlots := map[string]bool{}
		for _, ref := range multilineValues[shape] {
			mustBeSlots[ref] = true
		}
		slotted := map[string]bool{}
		for _, p := range pr.File.Prompts {
			lines := strings.Split(p.Body, "\n")
			for i, raw := range lines {
				line := strings.TrimSpace(raw)
				m := slotRe.FindStringSubmatch(line)
				if m == nil {
					// An inline mention of a multi-line value is the defect.
					for _, im := range refRe.FindAllStringSubmatch(line, -1) {
						if mustBeSlots[im[1]] {
							t.Errorf("%s: prompt %s: %s is a multi-line value mentioned inline in %q; give it a line of its own under a heading or in a fence", shape, p.Name, im[1], line)
						}
					}
					continue
				}
				seen++
				slotted[m[1]] = true
				if i == 0 || !boundary(lines[i-1]) {
					prev := "<start>"
					if i > 0 {
						prev = lines[i-1]
					}
					t.Errorf("%s: prompt %s: the value slot %s follows %q, want a heading, a fence or a blank line", shape, p.Name, line, prev)
				}
				if i+1 < len(lines) && !boundary(lines[i+1]) {
					t.Errorf("%s: prompt %s: the value slot %s is followed by %q, want a heading, a fence, a blank line or the end of the body", shape, p.Name, line, lines[i+1])
				}
			}
		}
		for ref := range mustBeSlots {
			if !slotted[ref] {
				t.Errorf("%s: the multi-line value %s appears in no prompt of main.bot as a slot; update multilineValues if it moved", shape, ref)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no value slot found in any gallery prompt; the rule guards nothing")
	}
}
