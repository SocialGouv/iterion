package runtime

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

const settledEachEvidenceBot = `dsl: 2
schema batch:
  items: json
schema identity:
  id: string
schema decision:
  ok: bool
schema result:
  value: string
prompt p:
  Test.
agent entry:
  model: "test-model"
  output: batch
  system: p
  user: p
agent head:
  model: "test-model"
  input: identity
  output: decision
  system: p
  user: p
agent yes:
  model: "test-model"
  output: result
  system: p
  user: p
agent no:
  model: "test-model"
  output: result
  system: p
  user: p
agent collect:
  model: "test-model"
  output: result
  system: p
  user: p
  await: best_effort
router dispatch:
  mode: fan_out_each
  over: "{{outputs.entry.items}}"
  as: item
workflow settled_each_evidence:
  entry: entry
  worktree: none
  sandbox: none
  entry -> dispatch
  dispatch -> head with { id: "{{outputs.dispatch.item.id}}" }
  head -> yes when ok
  head -> no else
  yes -> collect with { common: "agreed", disputed: "yes" }
  no -> collect with { common: "agreed", disputed: "no", only_unknown: "present" }
  collect -> done
`

// The second item dies at head, before routing. The first item's choice
// must neither erase that uncertainty nor hide the disagreement it carries.
// Successful incoming mappings still override the floor. With no undecided
// item, the rejected route must remain absent (#484).
func TestSettledFloorEachEvidence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		earlyFail  bool
		yesFails   bool
		wantChoice string
	}{
		{"mixed_failures_keep_disagreement", true, true, ""},
		{"live_mapping_wins", true, false, "yes"},
		{"all_chose_yes", false, true, "yes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compiled := compileBot(t, settledEachEvidenceBot)
			for _, d := range compiled.Diagnostics {
				if d.Severity == ir.SeverityError {
					t.Fatalf("compile: %v", d)
				}
			}
			wf := compiled.Workflow
			exec := newStubExecutor()
			var heads, noCalls atomic.Int32
			exec.on("entry", func(map[string]any) (map[string]any, error) {
				return map[string]any{"items": []any{item("A"), item("B")}}, nil
			})
			exec.on("head", func(input map[string]any) (map[string]any, error) {
				heads.Add(1)
				if tc.earlyFail && input["id"] == "B" {
					return nil, errors.New("B died before routing")
				}
				return map[string]any{"ok": true}, nil
			})
			exec.on("yes", func(map[string]any) (map[string]any, error) {
				if tc.yesFails {
					return nil, errors.New("yes failed")
				}
				return map[string]any{"value": "success"}, nil
			})
			exec.on("no", func(map[string]any) (map[string]any, error) {
				noCalls.Add(1)
				return map[string]any{"value": "unexpected"}, nil
			})
			var inputs []map[string]any
			exec.on("collect", func(input map[string]any) (map[string]any, error) {
				inputs = append(inputs, input)
				if len(inputs) == 1 {
					return nil, errors.New("park collector for a fresh-engine resume")
				}
				return map[string]any{"value": "finished"}, nil
			})
			s := tmpStore(t)
			ctx := context.Background()
			runID := "settled-each-" + tc.name
			if err := New(wf, s, exec).Run(ctx, runID, nil); err == nil {
				t.Fatal("expected collector failure")
			}
			if err := New(wf, s, exec).Resume(ctx, runID, nil); err != nil {
				t.Fatalf("fresh-engine resume: %v", err)
			}
			if len(inputs) != 2 || heads.Load() != 2 || noCalls.Load() != 0 {
				t.Fatalf("collector visits=%d, head calls=%d, rejected route calls=%d", len(inputs), heads.Load(), noCalls.Load())
			}
			for visit, input := range inputs {
				if input["common"] != "agreed" {
					t.Errorf("visit %d lost the common mapping: %v", visit, input)
				}
				choice, present := input["disputed"]
				if tc.wantChoice == "" && present || tc.wantChoice != "" && choice != tc.wantChoice {
					t.Errorf("visit %d disputed=%v (present=%t), want %q (empty means absent)", visit, choice, present, tc.wantChoice)
				}
				_, unknownPresent := input["only_unknown"]
				if unknownPresent != tc.earlyFail {
					t.Errorf("visit %d unknown route present=%t, want %t: %v", visit, unknownPresent, tc.earlyFail, input)
				}
			}
			run, err := s.LoadRun(ctx, runID)
			if err != nil || run.Status != store.RunStatusFinished {
				t.Fatalf("run=%+v, err=%v", run, err)
			}
		})
	}
}

func TestSettledTemplateEvidenceUsesEachStart(t *testing.T) {
	wf := &ir.Workflow{Edges: []*ir.Edge{
		{From: "dispatch", To: "new_head"},
		{From: "old_head", To: "chosen", Condition: "ok"},
		{From: "old_head", To: "rejected", IsElse: true},
		{From: "chosen", To: "collect"},
		{From: "rejected", To: "collect"},
		{From: "new_head", To: "collect"},
	}}
	resumed := &branchResult{
		startNodeID: "old_head",
		outputs:     map[string]map[string]any{"old_head": {"ok": true}},
		selectedIncoming: map[string][]store.IncomingEdge{
			"chosen": {incomingFromEdge(wf.Edges[1])},
		},
	}
	// The first two items resumed at the old cursor. The third
	// never started, so its own declaration remains its only provenance.
	results := []*branchResult{resumed, resumed, {err: errors.New("never started")}}
	want := []store.IncomingEdge{incomingFromEdge(wf.Edges[3]), incomingFromEdge(wf.Edges[5])}
	for range 2 {
		got := settledTemplateEdgesInto(wf, wf.Edges[0], "collect", results)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("floor = %+v, want %+v", got, want)
		}
		slices.Reverse(results)
	}
}
