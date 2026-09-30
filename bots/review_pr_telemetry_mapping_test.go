package bots

import (
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
func TestReviewPRPublishEdgeMapsTheServingTelemetry(t *testing.T) {
	u := unit.LoadDir("review-pr/main.bot")
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
		want[f.Name] = "{{outputs." + node + "." + key + "}}"
	}
	if len(want) < 20 {
		t.Fatalf("only %d ai_* fields in publish_input — the schema changed shape and this derivation no longer reads it", len(want))
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
