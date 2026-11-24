package model

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// P3: a claw node whose PRIMARY route cannot produce structured output
// (malformed tool JSON on the forced turn and on the unforced re-ask) and
// whose declared fallbacks: route answers validly. Which route serves?
func TestR2_ClawParseFailureAndTheFallbackChain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	primary := newMockClient(
		toolUseEvents("tu_1", "structured_output", `{"verdict":tru`, 50, 20),
		toolUseEvents("tu_2", "structured_output", `{"verdict":tru`, 50, 20),
		toolUseEvents("tu_3", "structured_output", `{"verdict":tru`, 50, 20),
		toolUseEvents("tu_4", "structured_output", `{"verdict":tru`, 50, 20),
		toolUseEvents("tu_5", "structured_output", `{"verdict":tru`, 50, 20),
		toolUseEvents("tu_6", "structured_output", `{"verdict":tru`, 50, 20),
	)
	rescue := newMockClient(
		toolUseEvents("ok_1", "structured_output", `{"verdict":true,"reason":"rescued"}`, 30, 10),
	)
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return primary, nil })
	reg.Register("alt", func(string) (api.APIClient, error) { return rescue, nil })
	exec := newTestClawExecutor(reg, verdictWorkflow(), WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
	node := verdictJudge("claw")
	node.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: "claw", Model: "alt/alt-model", On: []string{"usage_window", "unavailable"}}}
	out, err := exec.Execute(WithRunID(context.Background(), "run-chain"), node, map[string]any{})
	if err != nil || out["_served_by"] != "rescue" || len(rescue.getCalls()) != 1 {
		t.Errorf("rescue never serves: out=%v err=%v calls=%d", out, err, len(rescue.getCalls()))
	}
	t.Logf("out=%v", out)
	t.Logf("err=%v", err)
	t.Logf("primary provider calls=%d  rescue provider calls=%d", len(primary.getCalls()), len(rescue.getCalls()))
}
