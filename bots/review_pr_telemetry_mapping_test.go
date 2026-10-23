package bots

import (
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// telemetryKeyOf maps the suffix of an `ai_<node>_<suffix>` publish field to
// the engine-written output key it carries.
var telemetryKeyOf = map[string]string{
	"model":    "_model",
	"backend":  "_backend",
	"tokens":   "_tokens",
	"fallback": "_fallback_used",
	"wire":     "_session_fingerprint",
}

// The published run table and the merge-gate note read engine telemetry the
// pr_gate -> publish_review edge maps in. Nothing else checks those refs:
// `_`-prefixed output keys are never validated, and the run-details tests
// substitute {{input.*}} themselves, so a typo in `_session_fingerprint` would
// title a claude id served by z.ai by its model, a typo in `_fallback_used`
// would drop the fallback gate note, and one in `_model` would drop the family
// of a claw row that carries no fingerprint. Every `ai_*` field publish_input
// declares is checked, derived from the schema rather than listed here, so a
// field added later is covered the day it lands.
//
// A field may take either of two routes, and the engine key is checked on
// whichever it takes:
//
//   - straight from its node, `{{outputs.<node>.<key>}}` — the shape of a
//     slot only one node ever fills (each reviewer tier);
//   - through pr_gate's passthrough, `{{outputs.pr_gate.<field>}}` — the
//     shape of a slot SEVERAL nodes can fill (the merge twins, one per
//     family): then every edge into pr_gate owes the field, mapped from ITS
//     source's engine key, or the run table describes a merge step that did
//     not run on every run the other twin served.
func TestReviewPRPublishEdgeMapsTheServingTelemetry(t *testing.T) {
	u := unittest.Whole(unit.LoadDir("review-pr/main.bot"))
	if u.Merged == nil {
		t.Fatalf("review-pr: the bundle did not parse: %v", u.Diagnostics)
	}
	cr := ir.Compile(u.Merged)
	if cr.Workflow == nil {
		t.Fatalf("review-pr: the bundle did not compile: %v", cr.Diagnostics)
	}
	schema := cr.Workflow.Schemas["publish_input"]
	if schema == nil {
		t.Fatal("review-pr: no publish_input schema — the publish step is typed some other way now, and this test proves nothing")
	}

	// The engine key each ai_* field carries, and the direct mapping it takes
	// unless pr_gate carries it through.
	keyOf := map[string]string{}
	want := map[string]string{}
	for _, f := range schema.Fields {
		if !strings.HasPrefix(f.Name, "ai_") {
			continue
		}
		if f.Name == "ai_run_tokens" {
			want[f.Name] = "{{run.tokens}}"
			continue
		}
		rest := strings.TrimPrefix(f.Name, "ai_")
		cut := strings.LastIndex(rest, "_")
		if cut <= 0 {
			t.Fatalf("publish_input.%s does not read as ai_<node>_<telemetry>", f.Name)
		}
		node, suffix := rest[:cut], rest[cut+1:]
		key, known := telemetryKeyOf[suffix]
		if !known {
			t.Fatalf("publish_input.%s: telemetry suffix %q has no engine output key here — extend telemetryKeyOf", f.Name, suffix)
		}
		if _, exists := cr.Workflow.Nodes[node]; !exists {
			t.Errorf("publish_input.%s names node %q, which the workflow does not have", f.Name, node)
		}
		keyOf[f.Name] = key
		want[f.Name] = "{{outputs." + node + "." + key + "}}"
	}
	if len(want) < 20 {
		t.Fatalf("only %d ai_* fields in publish_input — the schema changed shape and this derivation no longer reads it", len(want))
	}

	// A field pr_gate carries through is owed by EVERY edge into pr_gate,
	// mapped from that edge's own source: the gate fires from one twin per
	// run, and the field must name the one that ran.
	carried := map[string]bool{}
	gateSources := 0
	for _, e := range cr.Workflow.Edges {
		if e.To != "pr_gate" {
			continue
		}
		gateSources++
	}
	for field, key := range keyOf {
		through := "{{outputs.pr_gate." + field + "}}"
		for _, e := range cr.Workflow.Edges {
			if e.From != "pr_gate" || e.To != "publish_review" {
				continue
			}
			for _, m := range e.With {
				if m.Key == field && strings.TrimSpace(m.Raw) == through {
					carried[field] = true
				}
			}
		}
		if !carried[field] {
			continue
		}
		want[field] = through
		owed := 0
		for _, e := range cr.Workflow.Edges {
			if e.To != "pr_gate" {
				continue
			}
			from := "{{outputs." + e.From + "." + key + "}}"
			for _, m := range e.With {
				if m.Key != field {
					continue
				}
				owed++
				if strings.TrimSpace(m.Raw) != from {
					t.Errorf("%s -> pr_gate maps %s to %q, want %q — a field pr_gate carries through must name the node that produced it", e.From, field, strings.TrimSpace(m.Raw), from)
				}
			}
		}
		if owed != gateSources {
			t.Errorf("%d of %d edge(s) into pr_gate map %s — the ones that do not leave the published run table describing a step that did not run", owed, gateSources, field)
		}
	}

	seen := 0
	for _, e := range cr.Workflow.Edges {
		if e.From != "pr_gate" || e.To != "publish_review" {
			continue
		}
		seen++
		got := map[string]string{}
		for _, m := range e.With {
			got[m.Key] = strings.TrimSpace(m.Raw)
		}
		keys := make([]string, 0, len(want))
		for key := range want {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if got[key] != want[key] {
				t.Errorf("pr_gate -> publish_review maps %s to %q, want %q", key, got[key], want[key])
			}
		}
	}
	if seen == 0 {
		t.Fatal("no pr_gate -> publish_review edge: the publish step is wired some other way now, and this test proves nothing")
	}
}
