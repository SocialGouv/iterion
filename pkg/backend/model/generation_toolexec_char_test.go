package model

// Characterization tests for executeToolsDirect and its pure companions
// (isContextWindowError, forceCompactToTokens, maybeCompactPause) ahead of
// the generation_toolexec.go decomposition (backlog B2). They pin CURRENT
// behavior — including quirks — so a later extract-function refactor that
// changes behavior fails here. The permission-gate paths already have direct
// coverage in permission_gate_test.go; these tests cover the dispatch loop,
// callback contract, hook payloads, ask_user propagation, and secret
// materialization.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

// charToolResultText extracts the text payload of a tool_result content block.
func charToolResultText(t *testing.T, b api.ContentBlock) string {
	t.Helper()
	if b.Type != "tool_result" {
		t.Fatalf("block type = %q, want tool_result", b.Type)
	}
	if len(b.Content) != 1 || b.Content[0].Type != "text" {
		t.Fatalf("tool_result content = %+v, want a single text block", b.Content)
	}
	return b.Content[0].Text
}

// TestExecuteToolsDirect_ResultShapesAndOrder pins the core dispatch loop: a
// failing tool yields an isError "tool error: ..." result and the loop
// CONTINUES to later tools; results come back in input order with matching
// ToolUseIDs; onToolStarted fires before each executed tool with the raw
// input; onToolCall fires after each with output/error.
func TestExecuteToolsDirect_ResultShapesAndOrder(t *testing.T) {
	var order []string
	toolMap := map[string]*GenerationTool{
		"ok_a": {Name: "ok_a", Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			order = append(order, "exec:ok_a")
			return "alpha-out", nil
		}},
		"boom": {Name: "boom", Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			order = append(order, "exec:boom")
			return "", fmt.Errorf("kaput")
		}},
		"ok_b": {Name: "ok_b", Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			order = append(order, "exec:ok_b")
			return "beta-out", nil
		}},
	}
	uses := []toolUseBlock{
		{ID: "tu_1", Name: "ok_a", PartialJSON: `{"n":1}`},
		{ID: "tu_2", Name: "boom", PartialJSON: `{"n":2}`},
		{ID: "tu_3", Name: "ok_b", PartialJSON: `{"n":3}`},
	}

	var started, completed []ToolCallInfo
	results, err := executeToolsDirect(context.Background(), uses, toolMap,
		func(i ToolCallInfo) { started = append(started, i) },
		func(i ToolCallInfo) { completed = append(completed, i) },
		nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}

	// Order + IDs + shapes.
	if results[0].ToolUseID != "tu_1" || results[0].IsError {
		t.Errorf("results[0] = %+v", results[0])
	}
	if got := charToolResultText(t, results[0]); got != "alpha-out" {
		t.Errorf("results[0] text = %q", got)
	}
	if results[1].ToolUseID != "tu_2" || !results[1].IsError {
		t.Errorf("results[1] = %+v, want isError for the failing tool", results[1])
	}
	if got := charToolResultText(t, results[1]); got != "tool error: kaput" {
		t.Errorf("results[1] text = %q, want %q", got, "tool error: kaput")
	}
	if results[2].ToolUseID != "tu_3" || results[2].IsError {
		t.Errorf("results[2] = %+v — loop must continue past a tool failure", results[2])
	}

	// Execution order preserved.
	if got := strings.Join(order, ","); got != "exec:ok_a,exec:boom,exec:ok_b" {
		t.Errorf("execution order = %q", got)
	}

	// onToolStarted: one per executed tool, with ToolUseID + raw Input.
	if len(started) != 3 {
		t.Fatalf("onToolStarted fired %d times, want 3", len(started))
	}
	if started[0].ToolUseID != "tu_1" || string(started[0].Input) != `{"n":1}` {
		t.Errorf("started[0] = %+v", started[0])
	}
	if started[1].InputSize != len(`{"n":2}`) {
		t.Errorf("started[1].InputSize = %d", started[1].InputSize)
	}

	// onToolCall: after each execution; failing tool carries the error, the
	// others carry the output.
	if len(completed) != 3 {
		t.Fatalf("onToolCall fired %d times, want 3", len(completed))
	}
	if completed[0].Output != "alpha-out" || completed[0].Error != nil {
		t.Errorf("completed[0] = %+v", completed[0])
	}
	if completed[1].Error == nil || completed[1].Error.Error() != "kaput" {
		t.Errorf("completed[1].Error = %v", completed[1].Error)
	}
	if completed[2].ToolUseID != "tu_3" {
		t.Errorf("completed[2].ToolUseID = %q", completed[2].ToolUseID)
	}
}

// TestExecuteToolsDirect_UnknownToolResultAndCallbackQuirk pins the unknown
// tool path: an isError "unknown tool: X" result, no execution, no
// onToolStarted — and the quirk that the OnToolCall info for an unknown tool
// carries NO ToolUseID (all other error paths stamp it).
func TestExecuteToolsDirect_UnknownToolResultAndCallbackQuirk(t *testing.T) {
	var startedCount int
	var completed []ToolCallInfo
	results, err := executeToolsDirect(context.Background(),
		[]toolUseBlock{{ID: "tu_9", Name: "nope", PartialJSON: `{}`}},
		map[string]*GenerationTool{},
		func(ToolCallInfo) { startedCount++ },
		func(i ToolCallInfo) { completed = append(completed, i) },
		nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].ToolUseID != "tu_9" || !results[0].IsError {
		t.Errorf("results[0] = %+v", results[0])
	}
	if got := charToolResultText(t, results[0]); got != "unknown tool: nope" {
		t.Errorf("result text = %q", got)
	}
	if startedCount != 0 {
		t.Errorf("onToolStarted fired %d times for an unknown tool, want 0", startedCount)
	}
	if len(completed) != 1 {
		t.Fatalf("onToolCall fired %d times, want 1", len(completed))
	}
	if completed[0].Error == nil || completed[0].Error.Error() != "unknown tool: nope" {
		t.Errorf("completed[0].Error = %v", completed[0].Error)
	}
	// Quirk: the unknown-tool callback does NOT stamp ToolUseID.
	if completed[0].ToolUseID != "" {
		t.Errorf("completed[0].ToolUseID = %q, want empty (current unknown-tool quirk)", completed[0].ToolUseID)
	}
}

// TestExecuteToolsDirect_MalformedInputShortCircuitsBeforeHooks pins the
// JSON pre-validation: malformed PartialJSON produces an isError result and
// skips BOTH the lifecycle hooks and execution — the PreToolUse hook never
// observes the call.
func TestExecuteToolsDirect_MalformedInputShortCircuitsBeforeHooks(t *testing.T) {
	r := hooks.NewRunner()
	var preCount int
	r.Register(hooks.PreToolUse, func(_ context.Context, _ hooks.Context) (hooks.Decision, error) {
		preCount++
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})

	var executed bool
	toolMap := map[string]*GenerationTool{
		"Bash": {Name: "Bash", Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			executed = true
			return "ran", nil
		}},
	}
	var completed []ToolCallInfo
	results, err := executeToolsDirect(context.Background(),
		[]toolUseBlock{{ID: "tu_1", Name: "Bash", PartialJSON: `{"cmd":`}},
		toolMap, nil,
		func(i ToolCallInfo) { completed = append(completed, i) },
		r, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if executed {
		t.Error("tool executed despite malformed input")
	}
	if preCount != 0 {
		t.Errorf("PreToolUse fired %d times, want 0 (validation precedes hooks)", preCount)
	}
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("results = %+v, want a single isError", results)
	}
	if got := charToolResultText(t, results[0]); !strings.Contains(got, "malformed tool input") {
		t.Errorf("result text = %q", got)
	}
	if len(completed) != 1 || completed[0].ToolUseID != "tu_1" {
		t.Fatalf("onToolCall = %+v, want one info with ToolUseID stamped", completed)
	}
}

// TestExecuteToolsDirect_HookBlockDefaultReason pins the refusal wording
// when a PreToolUse hook blocks WITHOUT a reason: "tool refused: blocked by
// lifecycle hook". onToolStarted must not fire for a blocked call.
func TestExecuteToolsDirect_HookBlockDefaultReason(t *testing.T) {
	r := hooks.NewRunner()
	r.Register(hooks.PreToolUse, func(_ context.Context, _ hooks.Context) (hooks.Decision, error) {
		return hooks.Decision{Action: hooks.ActionBlock}, nil // no Reason
	})
	var executed bool
	var startedCount int
	toolMap := map[string]*GenerationTool{
		"Bash": {Name: "Bash", Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			executed = true
			return "ran", nil
		}},
	}
	results, err := executeToolsDirect(context.Background(),
		[]toolUseBlock{{ID: "tu_1", Name: "Bash", PartialJSON: `{}`}},
		toolMap,
		func(ToolCallInfo) { startedCount++ },
		nil, r, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if executed {
		t.Error("blocked tool executed")
	}
	if startedCount != 0 {
		t.Errorf("onToolStarted fired %d times for a blocked call, want 0", startedCount)
	}
	if got := charToolResultText(t, results[0]); got != "tool refused: blocked by lifecycle hook" {
		t.Errorf("refusal text = %q", got)
	}
	if !results[0].IsError {
		t.Error("refusal must be an isError tool_result")
	}
}

// TestExecuteToolsDirect_WrappedAskUserUnwrappedAndAborts pins the ask_user
// suspension contract: a WRAPPED *delegate.ErrAskUser returned by Execute is
// unwrapped (the wrapper text is dropped from the returned error), stamped
// with the pending tool_use ID, and aborts the loop — earlier results are
// returned, later tools never run, PostToolUseFailure does NOT fire for the
// suspension while PostToolUse fired for the earlier success.
func TestExecuteToolsDirect_WrappedAskUserUnwrappedAndAborts(t *testing.T) {
	inner := &delegate.ErrAskUser{Question: "prod or staging?"}
	var laterExecuted bool
	toolMap := map[string]*GenerationTool{
		"first": {Name: "first", Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "first-out", nil
		}},
		"asker": {Name: "asker", Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "", fmt.Errorf("tool wrapper context: %w", inner)
		}},
		"later": {Name: "later", Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			laterExecuted = true
			return "later-out", nil
		}},
	}
	r := hooks.NewRunner()
	var post, postFail int
	r.Register(hooks.PostToolUse, func(_ context.Context, _ hooks.Context) (hooks.Decision, error) {
		post++
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})
	r.Register(hooks.PostToolUseFailure, func(_ context.Context, _ hooks.Context) (hooks.Decision, error) {
		postFail++
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})

	uses := []toolUseBlock{
		{ID: "tu_1", Name: "first", PartialJSON: `{}`},
		{ID: "tu_2", Name: "asker", PartialJSON: `{}`},
		{ID: "tu_3", Name: "later", PartialJSON: `{}`},
	}
	results, err := executeToolsDirect(context.Background(), uses, toolMap, nil, nil, r, nil, nil)

	var askErr *delegate.ErrAskUser
	if !errors.As(err, &askErr) {
		t.Fatalf("expected *delegate.ErrAskUser, got %v", err)
	}
	if askErr != inner {
		t.Errorf("returned error is not the unwrapped inner ErrAskUser: %v", err)
	}
	if strings.Contains(err.Error(), "tool wrapper context") {
		t.Errorf("wrapper text must be dropped (inner error returned raw): %q", err.Error())
	}
	if askErr.PendingToolUseID != "tu_2" {
		t.Errorf("PendingToolUseID = %q, want tu_2", askErr.PendingToolUseID)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want only the pre-suspension result", len(results))
	}
	if results[0].ToolUseID != "tu_1" {
		t.Errorf("results[0].ToolUseID = %q", results[0].ToolUseID)
	}
	if laterExecuted {
		t.Error("tools after the suspension must not execute")
	}
	if post != 1 {
		t.Errorf("PostToolUse fired %d times, want 1 (the earlier success)", post)
	}
	if postFail != 0 {
		t.Errorf("PostToolUseFailure fired %d times, want 0 (suspension is not a failure)", postFail)
	}
}

// TestExecuteToolsDirect_HookPayloads pins what the lifecycle hooks observe:
// PreToolUse gets the PARSED input map, PostToolUse the tool's output,
// PostToolUseFailure the execution error.
func TestExecuteToolsDirect_HookPayloads(t *testing.T) {
	r := hooks.NewRunner()
	var preInputs []map[string]any
	var postResult string
	var failErr error
	r.Register(hooks.PreToolUse, func(_ context.Context, h hooks.Context) (hooks.Decision, error) {
		preInputs = append(preInputs, h.ToolInput)
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})
	r.Register(hooks.PostToolUse, func(_ context.Context, h hooks.Context) (hooks.Decision, error) {
		postResult = h.ToolResult
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})
	r.Register(hooks.PostToolUseFailure, func(_ context.Context, h hooks.Context) (hooks.Decision, error) {
		failErr = h.ToolError
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})

	toolMap := map[string]*GenerationTool{
		"ok": {Name: "ok", Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "ok-output", nil
		}},
		"bad": {Name: "bad", Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "", fmt.Errorf("boom")
		}},
	}
	uses := []toolUseBlock{
		{ID: "tu_1", Name: "ok", PartialJSON: `{"path":"a.txt"}`},
		{ID: "tu_2", Name: "bad", PartialJSON: `{"path":"b.txt"}`},
	}
	if _, err := executeToolsDirect(context.Background(), uses, toolMap, nil, nil, r, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(preInputs) != 2 {
		t.Fatalf("PreToolUse fired %d times, want 2", len(preInputs))
	}
	if preInputs[0]["path"] != "a.txt" || preInputs[1]["path"] != "b.txt" {
		t.Errorf("PreToolUse inputs = %v", preInputs)
	}
	if postResult != "ok-output" {
		t.Errorf("PostToolUse ToolResult = %q", postResult)
	}
	if failErr == nil || failErr.Error() != "boom" {
		t.Errorf("PostToolUseFailure ToolError = %v", failErr)
	}
}

// TestExecuteToolsDirect_MaterializeOnlyAffectsExecution pins Layer 1 of the
// secret protection: the materialize func rewrites the input the tool
// EXECUTES with, while both callbacks (started + completed) keep observing
// the placeholder form.
func TestExecuteToolsDirect_MaterializeOnlyAffectsExecution(t *testing.T) {
	const placeholderJSON = `{"token":"__ITERION_SECRET_apikey__"}`
	var execInput string
	toolMap := map[string]*GenerationTool{
		"Fetch": {Name: "Fetch", Execute: func(_ context.Context, in json.RawMessage) (string, error) {
			execInput = string(in)
			return "fetched", nil
		}},
	}
	materialize := func(s string) string {
		return strings.ReplaceAll(s, "__ITERION_SECRET_apikey__", "real-secret-value")
	}

	var started, completed ToolCallInfo
	_, err := executeToolsDirect(context.Background(),
		[]toolUseBlock{{ID: "tu_1", Name: "Fetch", PartialJSON: placeholderJSON}},
		toolMap,
		func(i ToolCallInfo) { started = i },
		func(i ToolCallInfo) { completed = i },
		nil, materialize, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(execInput, "real-secret-value") {
		t.Errorf("Execute input = %q, want the materialized secret", execInput)
	}
	if strings.Contains(string(started.Input), "real-secret-value") {
		t.Errorf("onToolStarted leaked the real secret: %s", started.Input)
	}
	if !strings.Contains(string(started.Input), "__ITERION_SECRET_apikey__") {
		t.Errorf("onToolStarted must carry the placeholder form: %s", started.Input)
	}
	if started.InputSize != len(placeholderJSON) || completed.InputSize != len(placeholderJSON) {
		t.Errorf("InputSize (started=%d, completed=%d), want placeholder length %d",
			started.InputSize, completed.InputSize, len(placeholderJSON))
	}
}

// TestExecuteToolsDirect_PermissionAskCarriesMarkerAndQuestion extends the
// permission-gate coverage: the Ask suspension must carry a non-empty
// operator-facing question AND the structured permission marker the runtime
// parses to compute the grant on resume.
func TestExecuteToolsDirect_PermissionAskCarriesMarkerAndQuestion(t *testing.T) {
	pol, err := permission.NewPolicy(permission.ModeAsk, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	toolMap := map[string]*GenerationTool{
		"Bash": {Name: "Bash", Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "ran", nil
		}},
	}
	_, gErr := executeToolsDirect(context.Background(),
		[]toolUseBlock{{ID: "tu_1", Name: "Bash", PartialJSON: `{"command":"curl http://x"}`}},
		toolMap, nil, nil, nil, nil, pol)
	var askErr *delegate.ErrAskUser
	if !errors.As(gErr, &askErr) {
		t.Fatalf("expected *delegate.ErrAskUser, got %v", gErr)
	}
	if askErr.Question == "" {
		t.Error("ask suspension must carry an operator-facing question")
	}
	if askErr.PermissionMarker == nil {
		t.Error("ask suspension must carry the structured permission marker")
	}
}

// TestIsContextWindowError_MarkerTable exhaustively pins the classification
// of backend context-window rejections: every marker matches (any casing,
// embedded anywhere, wrapped errors), and — critically — Go context
// lifecycle errors ("context canceled" / "context deadline exceeded") do NOT
// classify as window overflows (they must not trigger compaction retries).
func TestIsContextWindowError_MarkerTable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},

		// One positive per marker, exact form.
		{"context_length_exceeded", errors.New("context_length_exceeded"), true},
		{"maximum context length", errors.New("maximum context length is 272000 tokens"), true},
		{"context window", errors.New("your input exceeds the context window"), true},
		{"context length", errors.New("this model's context length is smaller"), true},
		{"too many tokens", errors.New("too many tokens: 1200000 > 272000"), true},
		{"prompt is too long", errors.New("prompt is too long"), true},
		{"input is too long", errors.New("input is too long for requested model"), true},
		{"request is too large", errors.New("request is too large"), true},

		// Case-insensitive matching (ContainsAnyFold).
		{"uppercase marker", errors.New("CONTEXT_LENGTH_EXCEEDED"), true},
		{"mixed case marker", errors.New("Maximum Context Length reached"), true},
		{"title case marker", errors.New("Prompt Is Too Long for this model"), true},

		// Wrapped/nested error text still matches (classification reads Error()).
		{"wrapped marker", fmt.Errorf("claw backend: %w", errors.New("openai stream error: context_length_exceeded")), true},

		// Negatives — especially the Go ctx lifecycle errors that CONTAIN
		// the word "context" but are not window overflows.
		{"context canceled", context.Canceled, false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"rate limit", errors.New("rate limit exceeded"), false},
		{"unauthorized", errors.New("401 unauthorized"), false},
		{"connection reset", errors.New("connection reset by peer"), false},
		{"empty message", errors.New(""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isContextWindowError(tt.err); got != tt.want {
				t.Errorf("isContextWindowError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// charMessagesOfSize builds n alternating user/assistant text messages of
// roughly chars characters each.
func charMessagesOfSize(n, chars int) []api.Message {
	body := strings.Repeat("x", chars)
	msgs := make([]api.Message, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, api.Message{Role: role, Content: []api.ContentBlock{{Type: "text", Text: body}}})
	}
	return msgs
}

// TestForceCompactToTokens pins the reactive force-compactor: a transcript
// over the target shrinks (preserving the recent window verbatim), a
// transcript already under the target is returned unchanged with ok=false.
func TestForceCompactToTokens(t *testing.T) {
	t.Run("shrinks oversized transcript", func(t *testing.T) {
		msgs := charMessagesOfSize(12, 2000) // ~24k chars ≈ ~6k estimated tokens
		out, ok := forceCompactToTokens(msgs, 500, 0)
		if !ok {
			t.Fatal("expected compaction to fire for an oversized transcript")
		}
		if len(out) >= len(msgs) {
			t.Fatalf("compacted len = %d, want < %d", len(out), len(msgs))
		}
		// Default preserveRecent (4): the last 4 messages survive verbatim.
		for i := 0; i < 4; i++ {
			want := msgs[len(msgs)-4+i]
			got := out[len(out)-4+i]
			if got.Role != want.Role || got.Content[0].Text != want.Content[0].Text {
				t.Errorf("preserved-recent[%d] altered: role=%q", i, got.Role)
			}
		}
	})

	t.Run("small transcript untouched", func(t *testing.T) {
		msgs := charMessagesOfSize(3, 10)
		out, ok := forceCompactToTokens(msgs, 1_000_000, 0)
		if ok {
			t.Fatal("expected no compaction for a small transcript")
		}
		if len(out) != len(msgs) {
			t.Errorf("output len = %d, want %d (unchanged)", len(out), len(msgs))
		}
	})

	t.Run("cannot shrink below preserve window", func(t *testing.T) {
		// 4 messages == default preserveRecent: nothing to summarise away.
		msgs := charMessagesOfSize(4, 2000)
		out, ok := forceCompactToTokens(msgs, 10, 0)
		if ok {
			t.Fatalf("expected ok=false when the transcript cannot shrink, got compacted len %d", len(out))
		}
	})
}
