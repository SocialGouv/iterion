package model

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// capturingBackend records every Task it is asked to Execute so a test can
// assert on the prompt content the executor sends per attempt.
type capturingBackend struct {
	mu      sync.Mutex
	tasks   []delegate.Task
	results []delegate.Result
}

func (b *capturingBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	idx := len(b.tasks)
	b.tasks = append(b.tasks, task)
	if idx < len(b.results) {
		return b.results[idx], nil
	}
	return delegate.Result{}, nil
}

// TestValidateAndRetry_InjectsSchemaFeedback proves that when structured
// output fails validation with a retry-eligible error (missing required
// field), the executor's SECOND backend call receives a UserPrompt augmented
// with the schema-validation feedback marker — the model is told what failed
// instead of blindly re-running the identical prompt.
func TestValidateAndRetry_InjectsSchemaFeedback(t *testing.T) {
	backend := &capturingBackend{
		results: []delegate.Result{
			// 1st call: valid JSON but missing the required "answer" field.
			{Output: map[string]any{"other": "x"}, BackendName: "test_backend"},
			// 2nd call: corrected, schema-valid output.
			{Output: map[string]any{"answer": "blue"}, BackendName: "test_backend"},
		},
	}

	reg := delegate.NewRegistry()
	reg.Register("test_backend", backend)
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{
			"out_schema": {
				Name:   "out_schema",
				Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}},
			},
		},
	}
	exec := NewClawExecutor(NewRegistry(), wf,
		WithBackendRegistry(reg),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond}),
	)

	node := &ir.AgentNode{
		BaseNode:     ir.BaseNode{ID: "answerer"},
		LLMFields:    ir.LLMFields{Backend: "test_backend"},
		SchemaFields: ir.SchemaFields{OutputSchema: "out_schema"},
	}

	output, err := exec.executeBackend(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := output["answer"]; got != "blue" {
		t.Fatalf("expected corrected answer=blue, got %v", output)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.tasks) != 2 {
		t.Fatalf("expected exactly 2 backend calls (attempt + feedback retry), got %d", len(backend.tasks))
	}
	if strings.Contains(backend.tasks[0].UserPrompt, schemaRetryFeedbackMarker) {
		t.Errorf("first attempt must NOT carry the retry feedback marker: %q", backend.tasks[0].UserPrompt)
	}
	if !strings.Contains(backend.tasks[1].UserPrompt, schemaRetryFeedbackMarker) {
		t.Errorf("retry attempt UserPrompt missing feedback marker %q: %q", schemaRetryFeedbackMarker, backend.tasks[1].UserPrompt)
	}
	if !strings.Contains(backend.tasks[1].UserPrompt, "missing required field") {
		t.Errorf("retry feedback should name the validation error, got: %q", backend.tasks[1].UserPrompt)
	}
}

// TestExecuteBackendKeepsSpendOnInvalidStructuredOutput: a node whose output
// never satisfies its schema still burned everything the generations cost —
// and a schema failure is the one that can bill TWICE, because a
// retry-eligible error buys a second call. validateAndRetry preserves the
// figure through all of its error exits; executeBackend used to drop it one
// line later, so the engine (the only caller that books) saw nothing of a
// node that could have run for an hour.
func TestExecuteBackendKeepsSpendOnInvalidStructuredOutput(t *testing.T) {
	newExec := func(backend delegate.Backend) *ClawExecutor {
		reg := delegate.NewRegistry()
		reg.Register("test_backend", backend)
		wf := &ir.Workflow{
			Prompts: map[string]*ir.Prompt{},
			Schemas: map[string]*ir.Schema{
				"out_schema": {
					Name:   "out_schema",
					Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}},
				},
			},
		}
		return NewClawExecutor(NewRegistry(), wf,
			WithBackendRegistry(reg),
			WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}),
		)
	}
	node := func() *ir.AgentNode {
		return &ir.AgentNode{
			BaseNode:     ir.BaseNode{ID: "answerer"},
			LLMFields:    ir.LLMFields{Backend: "test_backend"},
			SchemaFields: ir.SchemaFields{OutputSchema: "out_schema"},
		}
	}

	t.Run("a non-retryable shape books the one call it made", func(t *testing.T) {
		// A type mismatch is the model returning the WRONG shape; no retry
		// is bought for it, so the bill is exactly the first generation's.
		exec := newExec(&capturingBackend{results: []delegate.Result{{
			Output:      map[string]any{"answer": 42, "_cost_usd": 1.50},
			Tokens:      1_000,
			BackendName: "test_backend",
		}}})
		output, err := exec.executeBackend(context.Background(), node(), map[string]any{})
		if err == nil {
			t.Fatal("precondition: a type-mismatched output must fail validation")
		}
		if output == nil {
			t.Fatal("the failed node's spend never left the executor: nil output")
		}
		if got := output["_tokens"]; got != 1_000 {
			t.Errorf("_tokens = %v, want 1000", got)
		}
		if got := output["_cost_usd"]; got != 1.50 {
			t.Errorf("_cost_usd = %v, want 1.50", got)
		}
	})

	t.Run("a failed schema retry books both generations", func(t *testing.T) {
		// Missing-required-field IS retry-eligible: the executor buys a
		// second generation, and when that one is invalid too the node owes
		// the sum. Booking only one of the two halves the bill.
		exec := newExec(&capturingBackend{results: []delegate.Result{
			{Output: map[string]any{"other": "x", "_cost_usd": 1.50}, Tokens: 1_000, BackendName: "test_backend"},
			{Output: map[string]any{"other": "y", "_cost_usd": 0.90}, Tokens: 700, BackendName: "test_backend"},
		}})
		output, err := exec.executeBackend(context.Background(), node(), map[string]any{})
		if err == nil {
			t.Fatal("precondition: an output still missing the field must fail")
		}
		if output == nil {
			t.Fatal("the failed node's spend never left the executor: nil output")
		}
		if got := output["_tokens"]; got != 1_700 {
			t.Errorf("_tokens = %v, want 1700 (first attempt + retry)", got)
		}
	})

	t.Run("an abandoned retry is billed too", func(t *testing.T) {
		// The retry came back parse-fallback, so the executor abandons it
		// and surfaces the FIRST attempt's error. It was still a second
		// generation the provider charged for: the exit accumulates nothing
		// of its own, so without the fold the node reports a bill it did
		// not run up. No "text" on either output, so the last-resort claw
		// extraction bails before any provider lookup.
		exec := newExec(&capturingBackend{results: []delegate.Result{
			{Output: map[string]any{"other": "x", "_cost_usd": 1.50}, Tokens: 1_000, BackendName: "test_backend"},
			{Output: map[string]any{"other": "y", "_cost_usd": 0.90}, Tokens: 700, ParseFallback: true, BackendName: "test_backend"},
		}})
		output, err := exec.executeBackend(context.Background(), node(), map[string]any{})
		if err == nil {
			t.Fatal("precondition: an abandoned parse-fallback retry must fail the node")
		}
		if output == nil {
			t.Fatal("the failed node's spend never left the executor: nil output")
		}
		if got := output["_tokens"]; got != 1_700 {
			t.Errorf("_tokens = %v, want 1700 (first attempt + abandoned retry)", got)
		}
		if got := output["_cost_usd"]; got != 2.40 {
			t.Errorf("_cost_usd = %v, want 2.40 (first attempt + abandoned retry)", got)
		}
	})
}

// TestValidateAndRetry_BooksTheClawRecoveryCall: the last-resort structured
// -output recovery is a THIRD billed generation — a claw call to a provider
// of its own, on top of the delegation and its retry — and its usage exists
// nowhere but on the value it returns. Both of its outcomes were dropping it:
// the answer it produced was priced at the delegation alone, and the exit
// where it gave up AFTER the model answered reported nothing at all, which is
// exactly the figure GenerateObjectDirect's partial return was added to keep.
//
// Asserted on `_tokens`, deterministic whatever the host's price table says
// (`_cost_usd` for the recovery model is not).
func TestValidateAndRetry_BooksTheClawRecoveryCall(t *testing.T) {
	// The detector picks the recovery's provider off the environment, and
	// the claw registry serves that spec from a scripted client.
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	schema := &ir.Schema{
		Name:   "out_schema",
		Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}},
	}
	schemaJSON, err := SchemaToJSON(schema)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}

	cases := []struct {
		name      string
		recovery  []api.StreamEvent
		wantError bool
	}{{
		// The recovery ANSWERS: the node is rescued, and owes all three.
		name:     "a recovery that answered",
		recovery: toolUseEvents("tu_1", "structured_output", `{"answer":"blue"}`, 200, 50),
	}, {
		// The recovery is billed and still fails — the model narrated
		// instead of calling the synthetic tool. The node fails, owing the
		// same three.
		name:      "a recovery billed for an answer it could not use",
		recovery:  textEvents("I would rather narrate.", 200, 50),
		wantError: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clawReg := NewRegistry()
			clawReg.Register("anthropic", func(string) (api.APIClient, error) {
				return newMockClient(tc.recovery), nil
			})
			// The retry comes back parse-fallback with text, which is what
			// sends the executor into the claw recovery.
			delegateReg := delegate.NewRegistry()
			delegateReg.Register("test_backend", &capturingBackend{results: []delegate.Result{{
				Output:        map[string]any{"text": "the answer is blue", "_cost_usd": 0.50},
				Tokens:        500,
				ParseFallback: true,
				BackendName:   "test_backend",
			}}})
			exec := NewClawExecutor(clawReg,
				&ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{"out_schema": schema}},
				WithBackendRegistry(delegateReg),
				WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}),
			)
			backend, err := delegateReg.Resolve("test_backend")
			if err != nil {
				t.Fatalf("backend: %v", err)
			}

			// OutputSchema is what lets the recovery run at all.
			task := &delegate.Task{OutputSchema: schemaJSON}
			first := delegate.Result{
				Output:        map[string]any{"text": "the answer is blue", "_cost_usd": 1.00},
				Tokens:        1_000,
				ParseFallback: true,
				BackendName:   "test_backend",
			}

			out, err := exec.validateAndRetry(context.Background(),
				backendFields{id: "answerer", outputSchema: "out_schema"},
				"test_backend", backend, task, first, schema)
			if tc.wantError != (err != nil) {
				t.Fatalf("precondition: wantError=%v, got err=%v", tc.wantError, err)
			}
			// 1000 + 500 + the recovery's own 250: no session anywhere here,
			// so every figure is disjoint and they all add.
			if got := out.Output["_tokens"]; got != 1_750 {
				t.Errorf("_tokens = %v, want 1750 (delegation + retry + the recovery call)", got)
			}
			// The two delegations alone; the recovery adds whatever its
			// model is priced at, and may add nothing on an unpriced one.
			if got, _ := out.Output["_cost_usd"].(float64); got < 1.50 {
				t.Errorf("_cost_usd = %v, want >= 1.50 (both delegations)", out.Output["_cost_usd"])
			}
		})
	}
}

// TestValidateAndRetry_AbandonedRetryFoldsASharedSession pins the arithmetic
// of the exit the abandoned retry takes on the backend it exists for.
//
// `retryTask := *task` keeps the node's SessionID, so on `session:
// inherit/persist` both attempts run in ONE session — and claude_code's
// figure is that session's cumulative TOTAL (CostIsSessionTotal), already
// containing the first attempt's spend. Summing there reports ~2x the real
// cost, and an over-count is the direction that kills runs which still had
// budget. Tokens still SUM: every backend reports its own turn.
//
// Driven through validateAndRetry directly because sharesSession reads the
// TASK, and giving executeBackend a non-empty SessionID would mean plumbing
// the session-continuity store for an accounting assertion.
func TestValidateAndRetry_AbandonedRetryFoldsASharedSession(t *testing.T) {
	// The retry comes back parse-fallback, so it is ABANDONED: the exit
	// under test surfaces the first attempt's validation error and owes the
	// bill for both generations. $2.40 is the session total AFTER the retry,
	// i.e. it already contains the first attempt's $1.50.
	backend := &capturingBackend{results: []delegate.Result{{
		Output:             map[string]any{"other": "y", "_cost_usd": 2.40},
		Tokens:             700,
		ParseFallback:      true,
		CostIsSessionTotal: true,
		BackendName:        "test_backend",
	}}}
	reg := delegate.NewRegistry()
	reg.Register("test_backend", backend)
	schema := &ir.Schema{
		Name:   "out_schema",
		Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}},
	}
	exec := NewClawExecutor(NewRegistry(),
		&ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{"out_schema": schema}},
		WithBackendRegistry(reg),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}),
	)

	// No OutputSchema on the task, so the last-resort claw extraction bails
	// before any provider lookup and the abandoned exit is the one taken.
	task := &delegate.Task{SessionID: "s1"}
	first := delegate.Result{
		Output:             map[string]any{"other": "x", "_cost_usd": 1.50},
		Tokens:             1_000,
		CostIsSessionTotal: true,
		BackendName:        "test_backend",
	}

	out, err := exec.validateAndRetry(context.Background(),
		backendFields{id: "answerer", outputSchema: "out_schema"},
		"test_backend", backend, task, first, schema)
	if err == nil {
		t.Fatal("precondition: an abandoned parse-fallback retry must fail the node")
	}
	if got, _ := out.Output["_cost_usd"].(float64); got != 2.40 {
		t.Errorf("_cost_usd = %v, want 2.40 — one shared session's total, not 1.50+2.40 billed twice", out.Output["_cost_usd"])
	}
	if got := out.Output["_tokens"]; got != 1_700 {
		t.Errorf("_tokens = %v, want 1700 — each attempt reports its own turn, so tokens sum", got)
	}
}

// The schema-validation retry is a retry IN PLACE, and its accounting has
// to survive like one: the first attempt produced unusable output but was
// billed for a whole agentic turn.
//
// The assertion is on the OUTPUT MAP on purpose — that is what enforcement
// reads (runtime.extractUsage takes `_tokens` / `_cost_usd` from it, never
// the Result struct). The hand-rolled accumulation this replaced summed
// the struct fields only, so the first attempt's tokens never reached
// max_tokens and its cost reached nothing at all, while the comment above
// it claimed the opposite.
func TestValidateAndRetry_KeepsTheFirstAttemptsSpend(t *testing.T) {
	backend := &capturingBackend{
		results: []delegate.Result{
			// A full turn, billed, then rejected for a missing field.
			{
				Output:      map[string]any{"other": "x", "_tokens": 9000, "_cost_usd": 0.90},
				Tokens:      9000,
				BackendName: "test_backend",
			},
			// The correction is cheap — and taking only its figure is how
			// a 9300-token node reported 300.
			{
				Output:      map[string]any{"answer": "blue", "_tokens": 300, "_cost_usd": 0.03},
				Tokens:      300,
				BackendName: "test_backend",
			},
		},
	}

	reg := delegate.NewRegistry()
	reg.Register("test_backend", backend)
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{
			"out_schema": {
				Name:   "out_schema",
				Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}},
			},
		},
	}
	exec := NewClawExecutor(NewRegistry(), wf,
		WithBackendRegistry(reg),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond}),
	)
	node := &ir.AgentNode{
		BaseNode:     ir.BaseNode{ID: "answerer"},
		LLMFields:    ir.LLMFields{Backend: "test_backend"},
		SchemaFields: ir.SchemaFields{OutputSchema: "out_schema"},
	}

	output, err := exec.executeBackend(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := output["_tokens"].(int); got != 9300 {
		t.Errorf("_tokens = %v, want 9300 — the rejected attempt's 9000 was billed too", output["_tokens"])
	}
	// Two calls, two disjoint sessions: the node carries no session id, so
	// neither report contains the other and the costs add.
	if got, _ := output["_cost_usd"].(float64); got < 0.92 {
		t.Errorf("_cost_usd = %v, want ~0.93 — the rejected attempt's $0.90 was dropped", output["_cost_usd"])
	}
}
