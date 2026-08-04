package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// mapping compiles a `with` template the way the IR compiler does
// (compileWithMappings), so these tests exercise the exact DataMapping
// shape the engine sees at run time.
func mapping(t *testing.T, raw string) *ir.DataMapping {
	t.Helper()
	refs, err := ir.ParseRefs(raw)
	if err != nil {
		t.Fatalf("ParseRefs(%q): %v", raw, err)
	}
	return &ir.DataMapping{Key: "field", Refs: refs, Raw: raw}
}

// A template made of exactly one reference must hand the value over
// untouched — mappings carry typed payloads into typed fields, so
// stringifying here would break every object/bool/number binding.
func TestResolveMapping_WholeReferencePreservesType(t *testing.T) {
	e := &Engine{}
	sc := resolveScope{outputs: map[string]map[string]any{
		"seed": {
			"obj":  map[string]any{"a": 1},
			"list": []any{"x", "y"},
			"flag": true,
			"n":    int64(42),
			"text": "AAA",
		},
	}}

	for _, tc := range []struct {
		raw  string
		want any
	}{
		{"{{outputs.seed.text}}", "AAA"},
		{"{{outputs.seed.flag}}", true},
		{"{{outputs.seed.n}}", int64(42)},
	} {
		if got := e.resolveMapping(mapping(t, tc.raw), sc); got != tc.want {
			t.Errorf("resolveMapping(%q) = %#v, want %#v", tc.raw, got, tc.want)
		}
	}

	if got := e.resolveMapping(mapping(t, "{{outputs.seed.obj}}"), sc); got == nil {
		t.Error("resolveMapping of a whole-object reference returned nil")
	} else if _, ok := got.(map[string]any); !ok {
		t.Errorf("resolveMapping of a whole-object reference = %T, want map[string]any", got)
	}
}

// The regression this file exists for: a reference wrapped in literal
// text used to collapse to the bare value, dropping everything around
// it. app-concept's runtime canary caught it, but every composite
// mapping in every bot was silently corrupted the same way — scratch
// paths, Git branch names, error messages.
func TestResolveMapping_InterpolatesSurroundingLiterals(t *testing.T) {
	e := &Engine{}
	sc := resolveScope{
		vars: map[string]any{"scratch_dir": "/tmp/iterion-scratch"},
		outputs: map[string]map[string]any{
			"seed":     {"token": "mapping-v1"},
			"dispatch": {"ticket": map[string]any{"id": "T-7"}},
		},
	}

	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "single reference between literals",
			raw:  "app-concept/{{outputs.seed.token}}/resolved",
			want: "app-concept/mapping-v1/resolved",
		},
		{
			name: "reference with a trailing literal only",
			raw:  "{{outputs.seed.token}}/topics",
			want: "mapping-v1/topics",
		},
		{
			name: "several references and literals",
			raw:  "{{vars.scratch_dir}}/x/{{outputs.seed.token}}/topics",
			want: "/tmp/iterion-scratch/x/mapping-v1/topics",
		},
		{
			name: "adjacent references",
			raw:  "{{vars.scratch_dir}}{{outputs.seed.token}}",
			want: "/tmp/iterion-scratchmapping-v1",
		},
		{
			name: "same reference twice",
			raw:  "{{outputs.seed.token}}/{{outputs.seed.token}}",
			want: "mapping-v1/mapping-v1",
		},
		{
			name: "drilled path inside a literal",
			raw:  "iterion/squad/t/{{outputs.dispatch.ticket.id}}",
			want: "iterion/squad/t/T-7",
		},
		{
			name: "inner whitespace is part of the reference",
			raw:  "a/{{ outputs.seed.token }}/b",
			want: "a/mapping-v1/b",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.resolveMapping(mapping(t, tc.raw), sc); got != tc.want {
				t.Errorf("resolveMapping(%q) = %#v, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// A template with no reference at all is a literal and must survive
// verbatim.
func TestResolveMapping_LiteralPassesThrough(t *testing.T) {
	e := &Engine{}
	if got := e.resolveMapping(mapping(t, "just/a/path"), resolveScope{}); got != "just/a/path" {
		t.Errorf("resolveMapping of a literal = %#v, want %q", got, "just/a/path")
	}
}

// An unresolvable reference renders as empty rather than leaking the
// `{{…}}` template into the target field, where it would later read as
// a plausible literal path or branch name.
func TestResolveMapping_MissingReferenceRendersEmpty(t *testing.T) {
	e := &Engine{}
	sc := resolveScope{outputs: map[string]map[string]any{}}
	if got := e.resolveMapping(mapping(t, "a/{{outputs.absent.field}}/b"), sc); got != "a//b" {
		t.Errorf("resolveMapping with a missing ref = %#v, want %q", got, "a//b")
	}
}

// Structured values spliced into a larger template render as compact
// JSON, matching what the prompt renderer does.
func TestResolveMapping_StructuredValueRendersAsJSON(t *testing.T) {
	e := &Engine{}
	sc := resolveScope{outputs: map[string]map[string]any{
		"seed": {"list": []any{"x", "y"}},
	}}
	if got := e.resolveMapping(mapping(t, "items=[{{outputs.seed.list}}]"), sc); got != `items=[["x","y"]]` {
		t.Errorf("resolveMapping of a structured value = %#v, want %q", got, `items=[["x","y"]]`)
	}
}
