package botscaffold

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// renderedVarRefs scans a RENDERED bundle — main.bot and the annexes that
// read the Spec's vars, not a child .bot, which declares its own — for
// {{vars.<name>}} references, sorted.
func renderedVarRefs(t *testing.T, spec Spec) []string {
	t.Helper()
	mainBot, annexes, err := renderShape(spec)
	if err != nil {
		t.Fatalf("renderShape(%s): %v", spec.Shape, err)
	}
	seen := map[string]bool{}
	scan := func(src string) {
		for _, m := range varRefRe.FindAllStringSubmatch(src, -1) {
			seen[m[1]] = true
		}
	}
	scan(mainBot)
	for _, rel := range sortedAnnexes(annexes) {
		if strings.HasSuffix(rel, ".bot") {
			continue
		}
		scan(string(annexes[rel]))
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestVarRefScanReadsBothForms: a var is referenced as `{{vars.x}}` in a
// prompt or a command and as the bare `vars.x` in an `expr:` or a quoted
// `when` — the scan reads both, so a var a shape uses only in an
// expression is refused by name at the form (400) rather than met as
// C033 behind the compile guard (422).
func TestVarRefScanReadsBothForms(t *testing.T) {
	src := "  configured: \"!!vars.verify_command && vars.max_passes >= 1\"\n  command: `sh -c {{vars.cmd}}`\n  x: \"{{ vars.spaced }}\" outputs.check.vars.not_a_var"
	seen := map[string]bool{}
	for _, m := range varRefRe.FindAllStringSubmatch(src, -1) {
		seen[m[1]] = true
	}
	for _, want := range []string{"verify_command", "max_passes", "cmd", "spaced"} {
		if !seen[want] {
			t.Errorf("the scan missed %q in %q", want, src)
		}
	}
	if seen["not_a_var"] {
		t.Errorf("the scan took `outputs.check.vars.not_a_var` for a var reference")
	}
}

// TestShapeVarRefsAreUnconditional: shapeVarRefs reads the RAW templates,
// so a {{vars.x}} inside a template conditional would be demanded of every
// Spec, including one whose rendering never emits it. The check stays
// honest only while the shapes keep their var references unconditional:
// for each shape, the refs of a bare rendering (no vars, no dials, no
// text) and of the gallery template's own pre-filled Spec both equal the
// raw scan. A shape that gains a conditional reference turns this red and
// its author decides — a Spec field the check branches on too, or the
// reference moved out of the branch.
func TestShapeVarRefsAreUnconditional(t *testing.T) {
	var shapes int
	for _, tpl := range Templates() {
		if tpl.Spec.Shape == "" {
			continue
		}
		shapes++
		raw := shapeVarRefs(tpl.Spec.Shape)
		for name, spec := range map[string]Spec{
			"bare":     {Slug: "bare", Shape: tpl.Spec.Shape},
			"template": tpl.Spec,
		} {
			if got := renderedVarRefs(t, spec); !reflect.DeepEqual(got, raw) {
				t.Errorf("%s: the %s rendering references %v, the raw scan %v — a var reference is conditional", tpl.Spec.Shape, name, got, raw)
			}
		}
	}
	if shapes != len(Shapes()) {
		t.Errorf("%d shape templates for %d shapes; every shape has a gallery entry", shapes, len(Shapes()))
	}
}
