package model

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"strings"
	"testing"
	"time"
)

func TestOutputCorrectionNeverExtractsPastAControlCause(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "scripted-test-key")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	for _, tc := range []struct {
		name    string
		cause   error
		extract bool
	}{
		{"ordinary", errors.New("backend could not structure answer"), true},
		{"request timeout while node live", context.DeadlineExceeded, true},
		{"ambiguous", errors.Join(correctionAmbiguous(false), correctionAmbiguous(true)), false},
		{"redacted ambiguity", &redactedNodeError{msg: "redacted", err: errors.Join(correctionAmbiguous(false), correctionAmbiguous(true))}, false},
		{"auth", &delegate.ErrAuthFailed{Provider: "test"}, false},
		{"window second sibling", errors.Join(&delegate.ErrRateLimited{}, &delegate.ErrRateLimited{Kind: delegate.RateLimitKindUsageWindow}), false},
		{"node deadline", &ErrNodeDeadline{NodeID: "judge"}, false},
		{"cancel and ambiguity", errors.Join(context.Canceled, correctionAmbiguous(true)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMockClient(toolUseEvents("extract", "structured_output", `{"verdict":true,"reason":"extracted"}`, 15, 5))
			e := NewClawExecutor(NewRegistry(), verdictWorkflow(), WithRetryPolicy(RetryPolicy{MaxAttempts: 1}))
			spec := e.detectorSuggestedModel()
			if spec == "" {
				t.Fatal("scripted detector fixture missing route")
			}
			e.registry.Register(strings.SplitN(spec, "/", 2)[0], func(string) (api.APIClient, error) { return mock, nil })
			backend := persistentRouteBackend(func(context.Context, delegate.Task) (delegate.Result, error) {
				return delegate.Result{Output: map[string]any{"text": "the finished answer approves it"}, Tokens: 7, CostUSD: 2}, tc.cause
			})
			node := verdictJudge(delegate.BackendClaudeCode)
			f, _ := extractBackendFields(node)
			schema := e.schemas["verdict_schema"]
			wire, err := SchemaToJSON(schema)
			if err != nil {
				t.Fatal(err)
			}
			task := delegate.Task{Model: "test/test-model", NodeID: "judge", OutputSchema: wire}
			first := delegate.Result{Output: map[string]any{"text": "the finished answer approves it"}, ParseFallback: true, Tokens: 10, CostUSD: 3}
			h, err := e.newCorrectionHandle(context.Background(), nil, f, OutputCorrectionOwnership{}, delegate.BackendClaudeCode, backend, &task, first, schema, chainOutcome{}, time.Time{}, 0, &correctionLifetime{})
			if err != nil {
				t.Fatal(err)
			}
			round := e.runSchemaCorrectionRound(context.Background(), h, ValidateOutput(first.Output, schema))
			if tc.extract {
				if !round.answered || round.err != nil || len(mock.getCalls()) != 0 || round.usage.Tokens != 7 || !h.HasPendingExtraction() {
					t.Fatalf("reask crossed extraction reservation: %+v", round)
				}
				round = e.runSchemaCorrectionRound(context.Background(), h, ValidateOutput(h.aggregate.Output, schema))
				if !round.answered || round.err != nil || len(mock.getCalls()) != 1 || round.usage.Tokens != 20 || round.result.Tokens != 37 {
					t.Fatalf("positive extraction control: %+v calls=%d", round, len(mock.getCalls()))
				}
			} else {
				if round.answered || round.err == nil || len(mock.getCalls()) != 0 || round.usage.Tokens != 7 || round.usage.CostUSD != 2 {
					t.Fatalf("control cause crossed extraction: %+v calls=%d", round, len(mock.getCalls()))
				}
				if !errors.Is(round.err, tc.cause) {
					t.Fatalf("control cause erased: %v", round.err)
				}
			}
		})
	}
}

func TestOutputCorrectionExtractionBillDoesNotBecomeSessionBaseline(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "scripted-test-key")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	e := NewClawExecutor(NewRegistry(), verdictWorkflow(), WithRetryPolicy(RetryPolicy{MaxAttempts: 1}))
	mock := newMockClient(toolUseEvents("extract", "structured_output", `{"verdict":true,"reason":"extracted"}`, 15, 5))
	spec := e.detectorSuggestedModel()
	if spec == "" {
		t.Fatal("missing scripted route")
	}
	e.registry.Register(strings.SplitN(spec, "/", 2)[0], func(string) (api.APIClient, error) { return mock, nil })
	calls := 0
	backend := persistentRouteBackend(func(context.Context, delegate.Task) (delegate.Result, error) {
		calls++
		if calls == 1 {
			return delegate.Result{Output: map[string]any{"text": "finished"}, ParseFallback: true, SessionID: "s", CostIsSessionTotal: true, Tokens: 30, CostUSD: 12}, nil
		}
		return delegate.Result{Output: map[string]any{"verdict": true, "reason": "next"}, SessionID: "s", CostIsSessionTotal: true, Tokens: 7, CostUSD: 13}, nil
	})
	schema := e.schemas["verdict_schema"]
	wire, _ := SchemaToJSON(schema)
	task := delegate.Task{Model: "test/test-model", OutputSchema: wire}
	f, _ := extractBackendFields(verdictJudge(delegate.BackendClaudeCode))
	first := delegate.Result{Output: map[string]any{"verdict": true}, SessionID: "s", Tokens: 100, CostUSD: 10, CostIsSessionTotal: true}
	h, err := e.newCorrectionHandle(context.Background(), nil, f, OutputCorrectionOwnership{}, delegate.BackendClaudeCode, backend, &task, first, schema, chainOutcome{}, time.Time{}, 0, &correctionLifetime{})
	if err != nil {
		t.Fatal(err)
	}
	round := e.runSchemaCorrectionRound(context.Background(), h, ValidateOutput(first.Output, schema))
	if !round.answered || round.usage.Tokens != 30 || round.usage.CostUSD != 2 || len(mock.getCalls()) != 0 || !h.HasPendingExtraction() {
		t.Fatalf("reask round: %+v", round)
	}
	round = e.runSchemaCorrectionRound(context.Background(), h, ValidateOutput(h.aggregate.Output, schema))
	if !round.answered || round.usage.Tokens != 20 || calls != 1 || h.HasPendingExtraction() {
		t.Fatalf("extraction round: %+v", round)
	}
	priorCost := h.aggregate.CostUSD
	next := e.runSchemaCorrectionRound(context.Background(), h, errors.New("next feedback"))
	if !next.answered || next.usage.Tokens != 7 || next.usage.CostUSD != 1 || h.aggregate.Tokens != 157 || h.aggregate.CostUSD != priorCost+1 {
		t.Fatalf("extraction bill swallowed/reused: next=%+v aggregate=%+v", next, h.aggregate)
	}
}

func TestOutputCorrectionTaskPermissionIsFrozen(t *testing.T) {
	policy, err := permission.NewPolicy(permission.ModeDeny, []string{"Read"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	task := delegate.Task{Permission: policy}
	frozen, err := copyCorrectionTask(task)
	if err != nil {
		t.Fatal(err)
	}
	policy.AddAllowRule("Write")
	if decision, _ := frozen.Permission.Evaluate("Write", map[string]any{"file_path": "x"}); decision != permission.Deny {
		t.Fatal("serving permission changed after snapshot")
	}
	again, err := copyCorrectionTask(frozen)
	if err != nil {
		t.Fatal(err)
	}
	again.Permission.AddAllowRule("Write")
	if decision, _ := frozen.Permission.Evaluate("Write", map[string]any{"file_path": "x"}); decision != permission.Deny {
		t.Fatal("round mutated frozen permission")
	}
}
