package model

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestOutputViolationsShareLegacyRulesAndDoNotReadText(t *testing.T) {
	t.Parallel()
	schema := &ir.Schema{Fields: []*ir.SchemaField{
		{Name: "absent", Type: ir.FieldTypeString}, {Name: "nil", Type: ir.FieldTypeBool},
		{Name: "number", Type: ir.FieldTypeInt}, {Name: "choice", Type: ir.FieldTypeString, EnumValues: []string{"allowed"}},
		{Name: "file", Type: ir.FieldTypeFile}, {Name: "list", Type: ir.FieldTypeStringArray},
	}}
	output := map[string]any{"nil": nil, "number": 1.5, "choice": "missing required field", "file": map[string]any{}, "list": []any{false}}
	got := CollectOutputViolations(output, schema, nil)
	var kinds []OutputViolationKind
	for _, v := range got {
		kinds = append(kinds, v.Kind)
	}
	want := []OutputViolationKind{OutputMissing, OutputNull, OutputWrongType, OutputEnum, OutputInvalidFile, OutputWrongType}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("violation kinds %v", kinds)
	}
	legacy := `missing required field "absent"; field "nil" is null; field "number": expected integer, got float 1.5; field "choice": value "missing required field" not in enum [allowed]; field "file": file descriptor has no path; field "list"[0]: expected string, got bool`
	if err := ValidateOutput(output, schema); err == nil || err.Error() != legacy || !isMissingFieldError(err) {
		t.Fatalf("legacy diagnostic changed: %v", err)
	}
	output["absent"] = "present"
	err := ValidateOutput(output, schema)
	if isMissingFieldError(err) || !OutputCorrectionEligible(CollectOutputViolations(output, schema, nil), false) {
		t.Fatalf("typed violations lost eligibility or fabricated missing field: %v", err)
	}
	validSchema := &ir.Schema{Fields: []*ir.SchemaField{{Name: "text", Type: ir.FieldTypeString}}}
	if OutputCorrectionEligible(CollectOutputViolations(map[string]any{"text": "missing required field"}, validSchema, nil), false) {
		t.Fatal("valid diagnostic-looking text fabricated eligibility")
	}
	if !OutputCorrectionEligible(nil, true) {
		t.Fatal("parse fallback lost eligibility")
	}
}

type correctionAmbiguous bool

func (e correctionAmbiguous) Error() string         { return "remote effect" }
func (e correctionAmbiguous) AmbiguousEffect() bool { return bool(e) }
func TestOutputCorrectionControlGraph(t *testing.T) {
	t.Parallel()
	causes := []error{correctionAmbiguous(true), &delegate.ErrAuthFailed{}, &delegate.ErrRateLimited{Kind: delegate.RateLimitKindUsageWindow}, &delegate.ErrRateLimited{SelfImposed: true}, &ErrNodeDeadline{NodeID: "n"}, context.Canceled}
	for _, cause := range causes {
		for _, wrap := range []func(error) error{func(e error) error { return e }, func(e error) error {
			return errors.Join(correctionAmbiguous(false), &delegate.ErrRateLimited{Kind: delegate.RateLimitKindTransient}, e)
		}, func(e error) error {
			return &redactedNodeError{msg: "scrubbed", err: errors.Join(correctionAmbiguous(false), e)}
		}} {
			if err := wrap(cause); !correctionControlCause(err) {
				t.Fatalf("lost control cause %T through %T", cause, err)
			}
		}
	}
	if correctionControlCause(errors.New("authentication failed: missing required field")) || correctionControlCause(correctionAmbiguous(false)) {
		t.Fatal("diagnostic text/false marker became control")
	}
	if !errtrack.IsAmbiguousEffect(&redactedNodeError{msg: "redacted", err: errors.Join(correctionAmbiguous(false), correctionAmbiguous(true))}) {
		t.Fatal("opaque join lost ambiguity")
	}
}

func TestOutputCorrectionSnapshotCopiesTaskDataAndSchema(t *testing.T) {
	t.Parallel()
	task := delegate.Task{UserPrompt: "branch A", ExtraEnv: []string{"A=one"}, OutputSchema: json.RawMessage(`{"type":"object"}`), UserContent: []delegate.ContentBlock{{Type: "text", Text: "original"}}, ToolDefs: []delegate.ToolDef{{Name: "tool", InputSchema: json.RawMessage(`{"old":true}`)}}}
	copied, err := copyCorrectionTask(task)
	if err != nil {
		t.Fatal(err)
	}
	task.ExtraEnv[0] = "A=other"
	task.OutputSchema[0] = '!'
	task.UserContent[0].Text = "other"
	task.ToolDefs[0].InputSchema[0] = '!'
	if copied.ExtraEnv[0] != "A=one" || copied.OutputSchema[0] != '{' || copied.UserContent[0].Text != "original" || copied.ToolDefs[0].InputSchema[0] != '{' {
		t.Fatalf("task snapshot aliases input: %+v", copied)
	}
	cycle := map[string]any{"value": "original"}
	cycle["self"] = cycle
	out := copyCorrectionOutput(cycle)
	cycle["value"] = "changed"
	if out["value"] != "original" || out["self"].(map[string]any)["value"] != "original" {
		t.Fatal("output snapshot aliases source")
	}
}

func TestOutputCorrectionRoundHasOneCandidateProtocol(t *testing.T) {
	t.Parallel()
	results := []delegate.Result{
		{Output: map[string]any{"verdict": false}, SessionID: "s", CostIsSessionTotal: true, Tokens: 20, CostUSD: 8, UnreportedCalls: 1},
		{Output: map[string]any{"verdict": true}, SessionID: "s", CostIsSessionTotal: true, Tokens: 30, CostUSD: 12},
		{Output: map[string]any{"verdict": true, "reason": "yes"}, SessionID: "s", CostIsSessionTotal: true, Tokens: 40, CostUSD: 13},
	}
	calls := 0
	backend := persistentRouteBackend(func(ctx context.Context, task delegate.Task) (delegate.Result, error) {
		result := results[calls]
		calls++
		if task.SessionID != "s" || strings.Contains(task.UserPrompt, reaskPrompt) {
			t.Fatalf("round did not resume own session: %+v", task)
		}
		return result, nil
	})
	exec := NewClawExecutor(NewRegistry(), verdictWorkflow(), WithRetryPolicy(RetryPolicy{MaxAttempts: 1}))
	first := delegate.Result{Output: map[string]any{"verdict": true}, SessionID: "s", CostIsSessionTotal: true, Tokens: 100, CostUSD: 10, UnreportedCalls: 2}
	task := delegate.Task{NodeID: "judge", UserPrompt: reaskPrompt, Model: "test/test-model"}
	f, _ := extractBackendFields(verdictJudge(delegate.BackendClaudeCode))
	h, err := exec.newCorrectionHandle(context.Background(), nil, f, OutputCorrectionOwnership{}, delegate.BackendClaudeCode, backend, &task, first, verdictWorkflow().Schemas["verdict_schema"], chainOutcome{FellThrough: true, ServedBy: "backup", SessionDegraded: true}, time.Time{}, 0, &correctionLifetime{})
	if err != nil {
		t.Fatal(err)
	}
	for i, wantCost := range []float64{0, 2, 1} {
		round := exec.runSchemaCorrectionRound(context.Background(), h, ValidateOutput(h.aggregate.Output, h.schema))
		if !round.answered || round.err != nil {
			t.Fatalf("structured candidate %d treated as NoAnswer: %+v", i, round)
		}
		if round.usage.CostUSD != wantCost || round.usage.Tokens != results[i].Tokens {
			t.Fatalf("marginal round %d=%+v", i, round.usage)
		}
		if round.result.Output["_fallback_used"] != true || round.result.Output["_session_degraded"] != true || round.result.Output["_served_by"] != "backup" {
			t.Fatalf("route provenance lost: %+v", round.result.Output)
		}
	}
	accounting := h.Accounting()
	if accounting.Total.Tokens != 190 || accounting.Total.CostUSD != 13 || accounting.OtherCostUSD != 0 || len(accounting.Sessions) != 1 || accounting.Sessions[0].CostUSD != 13 {
		t.Fatalf("accounting declaration: %+v", accounting)
	}
	accounting.Sessions[0].CostUSD = 999
	if h.Accounting().Sessions[0].CostUSD != 13 {
		t.Fatal("accounting snapshot aliases handle")
	}
	if h.aggregate.Tokens != 190 || h.aggregate.CostUSD != 13 || h.aggregate.UnreportedCalls != 3 || h.raw.Tokens != 40 || h.raw.CostUSD != 13 {
		t.Fatalf("raw and bill mixed: raw=%+v total=%+v", h.raw, h.aggregate)
	}
}

func TestOutputCorrectionWithoutHandleNeverCallsBackend(t *testing.T) {
	t.Parallel()
	e := NewClawExecutor(NewRegistry(), verdictWorkflow())
	_, usage, err := e.CorrectOutputWithUsage(context.Background(), verdictJudge("claw"), nil, errors.New("missing"))
	if !errors.Is(err, ErrOutputCorrectionIneligible) || usage != (store.OutputCorrectionUsage{}) {
		t.Fatalf("missing handle: usage=%+v err=%v", usage, err)
	}
}
