package runview

import (
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The fold is a no-op when no level set overrides: the launch's rows come
// back as the launch typed them, and a nil snapshot folds the same.
func TestEffectiveModelOverrides_NilPolicyIsIdentity(t *testing.T) {
	rows := []ModelOverrideEntry{{Selector: "judge", Backend: "claw"}}
	if got := EffectiveModelOverrides(rows, nil); !reflect.DeepEqual(got, rows) {
		t.Fatalf("nil policy changed the rows: %+v", got)
	}
	if got := EffectiveModelOverrides(rows, &store.RunLLMRoutePolicy{}); !reflect.DeepEqual(got, rows) {
		t.Fatalf("empty policy changed the rows: %+v", got)
	}
}

// The policy's cells sit UNDER the launch's rows: a cell both pin goes to
// the launch's row (the later rule wins the specificity tie), and a cell
// only the policy pins survives the fold.
func TestEffectiveModelOverrides_PolicyCellsSitUnderTheLaunchRows(t *testing.T) {
	pol := &store.RunLLMRoutePolicy{
		Overrides: []store.RunLLMRouteOverride{
			{Selector: "judge", Backend: "claw"},
			{Selector: "reviewer_gpt", Model: "openai/gpt-5.5"},
		},
	}
	launch := []ModelOverrideEntry{{Selector: "judge", Backend: "codex"}}
	got := EffectiveModelOverrides(launch, pol)
	var judgeBackend string
	var gptModel string
	for _, e := range got {
		switch e.Selector {
		case "judge":
			judgeBackend = e.Backend
		case "reviewer_gpt":
			gptModel = e.Model
		}
	}
	if judgeBackend != "codex" {
		t.Errorf("judge backend = %q, want the launch's codex (the launch row wins the shared cell)", judgeBackend)
	}
	if gptModel != "openai/gpt-5.5" {
		t.Errorf("reviewer_gpt model = %q, want the policy's cell to survive (cells are independent)", gptModel)
	}
}

// The O2 case at the fold: the launcher's canonical gesture records "*" —
// the LEAST specific selector — and an exact policy id would otherwise
// outrank it on score alone. A launch row pinning a field on "*" shadows
// the policy's cells on that field outright — EVERY routing field, not
// just backend — while a "*" row on another field leaves a cell alone
// (the shadow is per field, not per row).
func TestEffectiveModelOverrides_LaunchStarShadowsThePolicyCell(t *testing.T) {
	cells := []struct {
		field string
		cell  store.RunLLMRouteOverride
		star  ModelOverrideEntry
	}{
		{"backend", store.RunLLMRouteOverride{Selector: "reviewer_claude", Backend: "claw"}, ModelOverrideEntry{Selector: "*", Backend: "claude_code"}},
		{"model", store.RunLLMRouteOverride{Selector: "reviewer_claude", Model: "openai/gpt-5.5"}, ModelOverrideEntry{Selector: "*", Model: "anthropic/claude-opus-4-8"}},
		{"provider", store.RunLLMRouteOverride{Selector: "reviewer_claude", Provider: "anthropic"}, ModelOverrideEntry{Selector: "*", Provider: "anthropic"}},
		{"effort", store.RunLLMRouteOverride{Selector: "reviewer_claude", Effort: "low"}, ModelOverrideEntry{Selector: "*", Effort: "low"}},
	}
	for _, tc := range cells {
		t.Run(tc.field, func(t *testing.T) {
			pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{tc.cell}}
			cell := ModelOverrideEntry{
				Selector: tc.cell.Selector,
				Backend:  tc.cell.Backend,
				Model:    tc.cell.Model,
				Provider: tc.cell.Provider,
				Effort:   tc.cell.Effort,
			}
			// The same-field shadow: the exact-id policy cell is dropped
			// outright, the launch's "*" row is all that remains.
			got := EffectiveModelOverrides([]ModelOverrideEntry{tc.star}, pol)
			if !reflect.DeepEqual(got, []ModelOverrideEntry{tc.star}) {
				t.Errorf("a * row on %s left the policy cell alive: folded = %+v, want the launch row alone", tc.field, got)
			}
			// Cross-field isolation: a "*" row on ANY OTHER field leaves
			// the cell intact, its own pin included.
			for _, other := range cells {
				if other.field == tc.field {
					continue
				}
				got := EffectiveModelOverrides([]ModelOverrideEntry{other.star}, pol)
				if !reflect.DeepEqual(got, []ModelOverrideEntry{cell, other.star}) {
					t.Errorf("a * row on %s dropped the %s cell: folded = %+v, want the cell plus the row", other.field, tc.field, got)
				}
			}
		})
	}
}

// The resolved snapshot's NORMAL shape is a multi-field cell: Resolve merges
// a selector's spoken cells into ONE Override. A "*" row pinning one field
// must shadow exactly that field and leave the cell's other pins standing —
// dropping the whole cell because one field is shadowed silently deletes
// pins the launch never spoke over.
func TestEffectiveModelOverrides_LaunchStarShadowsOneFieldOfAComposedCell(t *testing.T) {
	composed := store.RunLLMRouteOverride{
		Selector: "work",
		Backend:  "claw",
		Model:    "anthropic/claude-opus-4-8",
		Provider: "anthropic",
		Effort:   "low",
	}
	star := ModelOverrideEntry{Selector: "*", Backend: "claude_code"}
	got := EffectiveModelOverrides([]ModelOverrideEntry{star}, &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{composed}})
	want := []ModelOverrideEntry{
		{Selector: "work", Model: "anthropic/claude-opus-4-8", Provider: "anthropic", Effort: "low"},
		star,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("a * row on backend folded = %+v, want the cell's other pins (model, provider, effort) standing beside the star row: %+v", got, want)
	}
}

// The four wire twins of the launch's override rows must stay field-identical
// (the ONE fold's doc leans on it: pkg/backend/model/override_fold.go), and
// the snapshot's override cells must stay the wire twin of llmroute.Override.
func TestOverrideTwinsStayFieldIdentical(t *testing.T) {
	twinFields := func(t *testing.T, v any) []string {
		t.Helper()
		typ := reflect.TypeOf(v)
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		out := make([]string, 0, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			out = append(out, typ.Field(i).Name)
		}
		return out
	}
	// The four launch-row twins: Selector + the four routing fields.
	entry := twinFields(t, ModelOverrideEntry{})
	for name, fields := range map[string][]string{
		"model.OverrideEntry":    twinFields(t, model.OverrideEntry{}),
		"store.RunModelOverride": twinFields(t, store.RunModelOverride{}),
		"queue.ModelOverride":    twinFields(t, queue.ModelOverride{}),
	} {
		if !reflect.DeepEqual(fields, entry) {
			t.Errorf("%s fields = %v, want field-identity with runview.ModelOverrideEntry %v", name, fields, entry)
		}
	}
	// The snapshot's cell twin must carry llmroute.Override's five fields.
	cell := twinFields(t, store.RunLLMRouteOverride{})
	policyCell := twinFields(t, llmroute.Override{})
	if !reflect.DeepEqual(cell, policyCell) {
		t.Errorf("store.RunLLMRouteOverride fields = %v, want llmroute.Override's %v", cell, policyCell)
	}
}
